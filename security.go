package main

import (
	"net/http"
)

// contentSecurityPolicy locks every fetch directive to the app's own origin — external
// scripts, styles, images, connections, and plugins are all blocked, as is framing and
// form submission elsewhere. 'unsafe-inline' stays on for script/style because the
// HTMX templates rely on inline onclick handlers and Bootstrap sets style attributes;
// html/template's contextual escaping is the primary injection defense, this is the
// backstop that cuts off external exfiltration and injected <script src> vectors.
// 'unsafe-eval' is also required: htmx compiles every hx-on handler with the Function()
// constructor, which browsers gate under 'unsafe-eval' regardless of 'unsafe-inline' —
// without it every hx-on attribute throws EvalError and silently no-ops (e.g. the
// Recategorize modal never opening). It adds negligible risk on top of 'unsafe-inline',
// which already lets injected markup run arbitrary script without needing eval.
const contentSecurityPolicy = "default-src 'self'; script-src 'self' 'unsafe-inline' 'unsafe-eval'; " +
	"style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; " +
	"object-src 'none'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'"

// newSecurityMiddleware sets browser security headers on every response and rejects
// cross-site requests to state-changing routes using the Sec-Fetch-Site header, which
// all modern browsers attach. This is the CSRF gate for the Cloudflare Access mode,
// where the CF_Authorization cookie is an ambient credential a cross-site page could
// otherwise ride. Requests without the header (curl, the SigV4 proxy's signed
// requests, non-browser clients) are allowed — in IAM mode signing is the auth, and a
// browser old enough to omit the header can't be driven cross-site any more easily
// than one that sends it.
//
// No Strict-Transport-Security: viewer-facing TLS is enforced at CloudFront
// (redirect-to-https) / Cloudflare, and HSTS emitted to the localhost SigV4 proxy
// could poison future http://localhost dev sessions.
func newSecurityMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")

		// GET /api/prompts/generate-stream drives Bedrock spend, so it gets the same
		// cross-site check as writes despite being a read.
		guarded := r.Method != http.MethodGet && r.Method != http.MethodHead ||
			r.URL.Path == "/api/prompts/generate-stream"
		if guarded {
			switch r.Header.Get("Sec-Fetch-Site") {
			case "", "same-origin", "none": // non-browser, same-origin, or user-initiated (typed URL)
			default: // cross-site or same-site (other subdomains get no ambient trust)
				http.Error(w, "cross-site request rejected", http.StatusForbidden)
				return
			}
		}

		next.ServeHTTP(w, r)
	})
}
