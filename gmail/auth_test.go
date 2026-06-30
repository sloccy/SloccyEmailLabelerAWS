package gmail

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// ============================================================
// TokenFromJSON
// ============================================================

func TestTokenFromJSON(t *testing.T) {
	t.Run("valid token", func(t *testing.T) {
		expiry := time.Now().Add(time.Hour).Truncate(time.Second)
		raw, _ := json.Marshal(map[string]any{
			"access_token":  "tok123",
			"refresh_token": "ref456",
			"token_type":    "Bearer",
			"expiry":        expiry,
		})
		tok, err := TokenFromJSON(string(raw))
		if err != nil {
			t.Fatalf("TokenFromJSON: %v", err)
		}
		if tok.AccessToken != "tok123" {
			t.Errorf("AccessToken = %q", tok.AccessToken)
		}
		if tok.RefreshToken != "ref456" {
			t.Errorf("RefreshToken = %q", tok.RefreshToken)
		}
	})

	t.Run("malformed JSON", func(t *testing.T) {
		_, err := TokenFromJSON(`{not valid json`)
		if err == nil {
			t.Fatal("expected error for malformed JSON")
		}
	})

	t.Run("empty JSON object still parses", func(t *testing.T) {
		tok, err := TokenFromJSON(`{}`)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if tok == nil {
			t.Fatal("expected non-nil token")
		}
	})

	t.Run("empty string", func(t *testing.T) {
		_, err := TokenFromJSON("")
		if err == nil {
			t.Fatal("expected error for empty string")
		}
	})
}

// ============================================================
// fetchEmail (via userinfoURL var seam)
// ============================================================

func TestFetchEmail(t *testing.T) {
	tests := []struct {
		name      string
		handler   http.HandlerFunc
		wantEmail string
		wantErr   bool
	}{
		{
			name: "returns email field",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"email":"user@example.com","id":"12345"}`)) //nolint:errcheck,gosec
			},
			wantEmail: "user@example.com",
		},
		{
			name: "email field missing — returns empty string",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"id":"12345"}`)) //nolint:errcheck,gosec
			},
			wantEmail: "",
		},
		{
			name: "non-JSON body returns decode error",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(`not json`)) //nolint:errcheck,gosec
			},
			wantErr: true,
		},
		{
			name: "server error status still returns decode error (body is HTML error page)",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
			},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			t.Cleanup(srv.Close)

			old := userinfoURL
			userinfoURL = srv.URL
			t.Cleanup(func() { userinfoURL = old })

			email, err := fetchEmail(t.Context(), srv.Client())
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if email != tc.wantEmail {
				t.Errorf("email = %q, want %q", email, tc.wantEmail)
			}
		})
	}
}
