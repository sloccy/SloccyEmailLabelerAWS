package main

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"strings"
	"sync"
)

// Content-addressed URLs for the embedded static assets.
//
// Templates reference assets as {{asset "app.js"}}, which renders
// /static/<hash>/app.js. Because the hash is derived from the file's bytes, editing an
// asset changes its URL, and a URL therefore names one immutable body forever. That is
// what makes the long Cache-Control in registerRoutes safe, and in turn what lets the
// CloudFront /static/* behavior cache at the edge (see WebDistribution in template.yaml)
// instead of waking the Lambda for every stylesheet.
//
// The previous no-store approach existed because no cache-busting scheme did: an
// aggressive Cache-Control without one left returning users on a stale app.js for up to
// a day, which is what 26a650d had to undo.

// assetHashLen is how much of each SHA-256 lands in the URL. 12 hex chars is far more
// than enough to separate successive builds of one file, and keeps paths readable.
const assetHashLen = 12

// assetImmutableCacheControl is applied only to a request whose hash matches the file
// being served — a year, and immutable so browsers skip even revalidation.
const assetImmutableCacheControl = "public, max-age=31536000, immutable"

// assetHashes maps a static-relative path ("app.js", "vendor/htmx.min.js") to its hash
// segment. Computed on first use rather than at init so the three non-web Lambda modes,
// which never serve HTTP, don't pay for it at cold start.
var assetHashes = sync.OnceValue(buildAssetHashes)

func buildAssetHashes() map[string]string {
	m := map[string]string{}
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return m
	}
	_ = fs.WalkDir(sub, ".", func(p string, d fs.DirEntry, walkErr error) error {
		// .gz files are pre-compressed variants served under their sibling's URL
		// (registerRoutes picks them by Accept-Encoding), so they get no hash of their own.
		if walkErr != nil || d.IsDir() || strings.HasSuffix(p, ".gz") {
			return nil //nolint:nilerr // a missing or unreadable asset shouldn't stop the walk
		}
		data, readErr := fs.ReadFile(sub, p)
		if readErr != nil {
			return nil
		}
		sum := sha256.Sum256(data)
		m[p] = hex.EncodeToString(sum[:])[:assetHashLen]
		return nil
	})
	return m
}

// assetURL returns the content-addressed URL for a static file, for use as the "asset"
// template function. An unknown path falls back to the plain /static/ URL, which the
// handler still serves — uncached, since without a hash it can't promise immutability.
func assetURL(p string) string {
	p = strings.TrimPrefix(p, "/")
	if h, ok := assetHashes()[p]; ok {
		return "/static/" + h + "/" + p
	}
	return "/static/" + p
}

// isAssetHash reports whether seg looks like a hash segment. The handler uses this to
// recognise and strip an *outdated* hash too, so a page rendered just before a deploy
// still resolves its assets instead of 404ing on the tab the user left open.
func isAssetHash(seg string) bool {
	if len(seg) != assetHashLen {
		return false
	}
	for _, c := range seg {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
