package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSecurityMiddleware(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := newSecurityMiddleware(inner)

	cases := []struct {
		name      string
		method    string
		path      string
		fetchSite string
		want      int
	}{
		{"get passes regardless of site", http.MethodGet, "/", "cross-site", http.StatusOK},
		{"same-origin post passes", http.MethodPost, "/api/prompts", "same-origin", http.StatusOK},
		{"user-initiated post passes", http.MethodPost, "/api/prompts", "none", http.StatusOK},
		{"non-browser post passes", http.MethodPost, "/api/prompts", "", http.StatusOK},
		{"cross-site post rejected", http.MethodPost, "/api/prompts", "cross-site", http.StatusForbidden},
		{"same-site post rejected", http.MethodPost, "/api/prompts", "same-site", http.StatusForbidden},
		{"cross-site delete rejected", http.MethodDelete, "/api/accounts/1", "cross-site", http.StatusForbidden},
		{"cross-site generate-stream GET rejected", http.MethodGet, "/api/prompts/generate-stream", "cross-site", http.StatusForbidden},
		{"same-origin generate-stream GET passes", http.MethodGet, "/api/prompts/generate-stream", "same-origin", http.StatusOK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(t.Context(), tc.method, tc.path, nil)
			if tc.fetchSite != "" {
				req.Header.Set("Sec-Fetch-Site", tc.fetchSite)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("got status %d, want %d", rec.Code, tc.want)
			}
		})
	}

	// Headers land on every response, including rejections.
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/x", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	for _, hdr := range []string{"Content-Security-Policy", "X-Content-Type-Options", "X-Frame-Options", "Referrer-Policy"} {
		if rec.Header().Get(hdr) == "" {
			t.Errorf("missing %s header", hdr)
		}
	}
}
