package main

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
)

// newCfAccessMiddleware returns HTTP middleware that enforces a valid Cloudflare
// Access JWT (the Cf-Access-Jwt-Assertion header Cloudflare injects after a user
// authenticates, e.g. via Google).
//
// When teamDomain or aud is empty, verification is disabled and the middleware is a
// pass-through — in that mode the Function URL is expected to stay AWS_IAM-protected.
// When both are set (and the Function URL is public), every request must carry an
// Access JWT whose issuer matches the team domain and whose audience contains the AUD
// tag, otherwise it is rejected with 403. This prevents bypassing Cloudflare by hitting
// the raw Lambda Function URL directly.
func newCfAccessMiddleware(ctx context.Context, teamDomain, aud string) func(http.Handler) http.Handler {
	if teamDomain == "" || aud == "" {
		slog.Warn("Cloudflare Access verification disabled (CF_ACCESS_TEAM_DOMAIN/CF_ACCESS_AUD unset); Function URL must stay AWS_IAM-protected")
		return func(next http.Handler) http.Handler { return next }
	}

	teamDomain = strings.TrimRight(teamDomain, "/")
	certsURL := teamDomain + "/cdn-cgi/access/certs"
	keySet := oidc.NewRemoteKeySet(ctx, certsURL)
	verifier := oidc.NewVerifier(teamDomain, keySet, &oidc.Config{ClientID: aud})
	slog.Info("Cloudflare Access verification enabled", "issuer", teamDomain)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := r.Header.Get("Cf-Access-Jwt-Assertion")
			if token == "" {
				http.Error(w, "missing Cloudflare Access token", http.StatusForbidden)
				return
			}
			if _, err := verifier.Verify(r.Context(), token); err != nil {
				slog.Warn("Cloudflare Access token rejected", "err", err)
				http.Error(w, "invalid Cloudflare Access token", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
