package main

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// static/vendor is gitignored and materialized by scripts/vendor.sh, so it is present in
// a deploy (sam build runs the Makefile) and in CI (the workflow vendors first) but not
// necessarily in a bare checkout. These tests therefore derive what they expect from the
// embedded FS rather than naming vendored files, so they assert the same invariants in
// both cases instead of failing on an environment difference.

// newStaticTestServer builds a server with only the routes registered, which is all the
// /static/ handler needs — it reads from the embedded FS, not from the store or LLM.
func newStaticTestServer(t *testing.T) *server {
	t.Helper()
	s := &server{mux: http.NewServeMux()}
	s.registerRoutes()
	return s
}

func getStatic(t *testing.T, s *server, path, acceptEncoding string) *http.Response {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
	if acceptEncoding != "" {
		r.Header.Set("Accept-Encoding", acceptEncoding)
	}
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, r)
	return w.Result()
}

// staticFile is an asset guaranteed to exist in every checkout, for the tests that just
// need some asset to exercise the handler with.
const staticFile = "app.js"

func TestAssetURLIsContentAddressed(t *testing.T) {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		t.Fatalf("sub: %v", err)
	}
	hashes := assetHashes()
	if len(hashes) == 0 {
		t.Fatal("no asset hashes built")
	}

	for name := range hashes {
		t.Run(name, func(t *testing.T) {
			data, err := fs.ReadFile(sub, name)
			if err != nil {
				t.Fatalf("read %s: %v", name, err)
			}
			sum := sha256.Sum256(data)
			want := "/static/" + hex.EncodeToString(sum[:])[:assetHashLen] + "/" + name
			if got := assetURL(name); got != want {
				t.Errorf("assetURL(%q) = %q, want %q", name, got, want)
			}
		})
	}
}

// The hash has to actually track content, or the immutable Cache-Control is a lie.
func TestAssetHashesDifferPerFile(t *testing.T) {
	hashes := assetHashes()
	if len(hashes) == 0 {
		t.Fatal("no asset hashes built")
	}
	seen := map[string]string{}
	for path, h := range hashes {
		if len(h) != assetHashLen {
			t.Errorf("%s: hash %q length %d, want %d", path, h, len(h), assetHashLen)
		}
		if prev, dup := seen[h]; dup {
			t.Errorf("%s and %s share hash %q — distinct content must not collide", prev, path, h)
		}
		seen[h] = path
		// .gz variants ride on their sibling's URL and must not get their own entry.
		if strings.HasSuffix(path, ".gz") {
			t.Errorf("%s: pre-compressed variant should not be hashed separately", path)
		}
	}
}

func TestAssetURLUnknownPathFallsBack(t *testing.T) {
	if got, want := assetURL("does-not-exist.js"), "/static/does-not-exist.js"; got != want {
		t.Errorf("assetURL = %q, want %q", got, want)
	}
}

func TestIsAssetHash(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{strings.Repeat("a", assetHashLen), true},
		{"0123456789ab", true},
		{"", false},
		{"vendor", false},
		{strings.Repeat("a", assetHashLen-1), false},
		{strings.Repeat("a", assetHashLen+1), false},
		{strings.Repeat("A", assetHashLen), false}, // uppercase is not our encoding
		{"0123456789ag", false},                    // 'g' is not hex
	} {
		if got := isAssetHash(tc.in); got != tc.want {
			t.Errorf("isAssetHash(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// A current hash is the only thing that earns the long Cache-Control.
func TestStaticHashedURLIsImmutable(t *testing.T) {
	s := newStaticTestServer(t)

	resp := getStatic(t, s, assetURL(staticFile), "")
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Cache-Control"); got != assetImmutableCacheControl {
		t.Errorf("Cache-Control = %q, want %q", got, assetImmutableCacheControl)
	}
	body, _ := io.ReadAll(resp.Body)
	want, _ := fs.ReadFile(staticFS, "static/"+staticFile)
	if string(body) != string(want) {
		t.Errorf("body length %d, want %d", len(body), len(want))
	}
}

func TestStaticUnhashedURLStaysUncached(t *testing.T) {
	s := newStaticTestServer(t)

	resp := getStatic(t, s, "/static/"+staticFile, "")
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}

// A page rendered just before a deploy still references the old hash. It must get the
// current bytes rather than a 404, and must not be told they're immutable.
func TestStaticStaleHashServesCurrentBytesUncached(t *testing.T) {
	s := newStaticTestServer(t)
	stale := "/static/" + strings.Repeat("0", assetHashLen) + "/" + staticFile

	resp := getStatic(t, s, stale, "")
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (stale hash must not 404)", resp.StatusCode)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store for a stale hash", got)
	}
	body, _ := io.ReadAll(resp.Body)
	want, _ := fs.ReadFile(staticFS, "static/"+staticFile)
	if string(body) != string(want) {
		t.Error("stale-hash request did not serve the current file")
	}
}

// The pre-gzipped sibling must still be picked, and still be cacheable, through a hashed
// URL — Vary is what keeps the edge from serving it to a client that can't decode it.
// Only vendored assets are pre-gzipped, so this is skipped when they aren't materialized.
func TestStaticHashedURLServesGzipVariant(t *testing.T) {
	var name string
	for p := range assetHashes() {
		if _, err := fs.Stat(staticFS, "static/"+p+".gz"); err == nil {
			name = p
			break
		}
	}
	if name == "" {
		t.Skip("no pre-gzipped assets embedded (run scripts/vendor.sh)")
	}

	s := newStaticTestServer(t)
	resp := getStatic(t, s, assetURL(name), "gzip")
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if got := resp.Header.Get("Vary"); got != "Accept-Encoding" {
		t.Errorf("Vary = %q, want Accept-Encoding", got)
	}
	if got := resp.Header.Get("Cache-Control"); got != assetImmutableCacheControl {
		t.Errorf("Cache-Control = %q, want %q", got, assetImmutableCacheControl)
	}

	zr, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer func() { _ = zr.Close() }()
	got, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	want, _ := fs.ReadFile(staticFS, "static/"+name)
	if string(got) != string(want) {
		t.Error("decompressed gzip variant does not match the plain file")
	}
}

// A hash-shaped segment must not become a way to reach files outside the asset set.
func TestStaticHashedURLUnknownFileIsNotFound(t *testing.T) {
	s := newStaticTestServer(t)
	path := "/static/" + strings.Repeat("a", assetHashLen) + "/nope.js"

	resp := getStatic(t, s, path, "")
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// The scheme only pays off if templates actually go through {{asset}}. Checking the
// template source rather than rendered output makes this independent of whether the
// vendored files happen to be materialized: a literal "/static/… in a template is a
// missed helper call either way, and would silently ship an uncacheable asset.
func TestTemplatesReferenceAssetsThroughHelper(t *testing.T) {
	var bad []string
	err := fs.WalkDir(templateFS, "templates", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		b, readErr := templateFS.ReadFile(p)
		if readErr != nil {
			return readErr
		}
		for i, line := range strings.Split(string(b), "\n") {
			if strings.Contains(line, `"/static/`) {
				bad = append(bad, fmt.Sprintf("%s:%d", p, i+1))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk templates: %v", err)
	}
	if len(bad) > 0 {
		t.Errorf("templates must link assets via {{asset \"…\"}}, found literal /static/ at:\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// Every embedded asset must resolve through its hashed URL and be cacheable.
func TestEmbeddedAssetsAllResolveHashed(t *testing.T) {
	s := newStaticTestServer(t)
	hashes := assetHashes()
	if len(hashes) == 0 {
		t.Fatal("no asset hashes built")
	}

	for name := range hashes {
		t.Run(name, func(t *testing.T) {
			url := assetURL(name)
			if url == "/static/"+name {
				t.Fatalf("assetURL(%q) = %q, want a hashed URL", name, url)
			}
			resp := getStatic(t, s, url, "")
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("GET %s = %d, want 200", url, resp.StatusCode)
			}
			if got := resp.Header.Get("Cache-Control"); got != assetImmutableCacheControl {
				t.Errorf("Cache-Control = %q, want %q", got, assetImmutableCacheControl)
			}
		})
	}
}
