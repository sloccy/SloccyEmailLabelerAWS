package main

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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
	r := httptest.NewRequest(http.MethodGet, path, nil)
	if acceptEncoding != "" {
		r.Header.Set("Accept-Encoding", acceptEncoding)
	}
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, r)
	return w.Result()
}

func TestAssetURLIsContentAddressed(t *testing.T) {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		t.Fatalf("sub: %v", err)
	}

	for _, name := range []string{"app.js", "style.css", "vendor/htmx.min.js"} {
		t.Run(name, func(t *testing.T) {
			got := assetURL(name)

			data, err := fs.ReadFile(sub, name)
			if err != nil {
				t.Fatalf("read %s: %v", name, err)
			}
			sum := sha256.Sum256(data)
			want := "/static/" + hex.EncodeToString(sum[:])[:assetHashLen] + "/" + name
			if got != want {
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
	}
	// .gz variants ride on their sibling's URL and must not get their own entry.
	for path := range hashes {
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
	valid := strings.Repeat("a", assetHashLen)
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{valid, true},
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

	resp := getStatic(t, s, assetURL("app.js"), "")
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Cache-Control"); got != assetImmutableCacheControl {
		t.Errorf("Cache-Control = %q, want %q", got, assetImmutableCacheControl)
	}
	body, _ := io.ReadAll(resp.Body)
	want, _ := fs.ReadFile(staticFS, "static/app.js")
	if string(body) != string(want) {
		t.Errorf("body length %d, want %d", len(body), len(want))
	}
}

func TestStaticUnhashedURLStaysUncached(t *testing.T) {
	s := newStaticTestServer(t)

	resp := getStatic(t, s, "/static/app.js", "")
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
	stale := "/static/" + strings.Repeat("0", assetHashLen) + "/app.js"

	resp := getStatic(t, s, stale, "")
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (stale hash must not 404)", resp.StatusCode)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store for a stale hash", got)
	}
	body, _ := io.ReadAll(resp.Body)
	want, _ := fs.ReadFile(staticFS, "static/app.js")
	if string(body) != string(want) {
		t.Error("stale-hash request did not serve the current file")
	}
}

// The pre-gzipped sibling must still be picked, and still be cacheable, through a
// hashed URL — Vary is what keeps the edge from serving it to a client that can't
// decode it.
func TestStaticHashedURLServesGzipVariant(t *testing.T) {
	s := newStaticTestServer(t)

	resp := getStatic(t, s, assetURL("vendor/htmx.min.js"), "gzip")
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
	want, _ := fs.ReadFile(staticFS, "static/vendor/htmx.min.js")
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

// The whole scheme only pays off if the rendered page actually emits hashed URLs — an
// unhashed /static/ reference left in a template is a silently uncacheable asset.
func TestIndexRendersOnlyHashedAssetURLs(t *testing.T) {
	tmpl, err := loadTemplates()
	if err != nil {
		t.Fatalf("loadTemplates: %v", err)
	}
	var sb strings.Builder
	// index.html is rendered with nil data in registerRoutes.
	if err := tmpl.ExecuteTemplate(&sb, "index.html", nil); err != nil {
		t.Fatalf("ExecuteTemplate: %v", err)
	}
	html := sb.String()

	for name, hash := range assetHashes() {
		want := "/static/" + hash + "/" + name
		if !strings.Contains(html, want) {
			continue // not every asset has to be referenced by index.html
		}
		// If it is referenced, the unhashed form must not also appear.
		if strings.Contains(html, `"/static/`+name+`"`) {
			t.Errorf("%s appears both hashed and unhashed", name)
		}
	}

	// Any remaining /static/ occurrence must carry a hash segment.
	for rest := html; ; {
		i := strings.Index(rest, "/static/")
		if i < 0 {
			break
		}
		rest = rest[i+len("/static/"):]
		seg, _, _ := strings.Cut(rest, "/")
		if !isAssetHash(seg) {
			t.Errorf("unhashed static reference: /static/%s...", seg)
		}
	}
}

// Every asset the templates link must resolve, or a deploy ships a broken page.
func TestTemplateAssetsAllResolve(t *testing.T) {
	s := newStaticTestServer(t)

	for _, name := range []string{
		"favicon.png", "logo.webp", "style.css", "app.js",
		"vendor/bootstrap.min.css", "vendor/bootstrap.bundle.min.js", "vendor/htmx.min.js",
	} {
		t.Run(name, func(t *testing.T) {
			url := assetURL(name)
			if !strings.HasPrefix(url, "/static/") || url == "/static/"+name {
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
