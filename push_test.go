package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// Fixtures for the push token checks. The signing key is generated per test run and
// served through a local JWKS endpoint, so verify exercises the real go-oidc path —
// signature, issuer, audience and expiry — rather than a stub that always says yes.

const (
	testIssuer   = "https://accounts.google.com"
	testAudience = "ollamail-gmail-push"
	testSA       = "gmail-push-invoker@example.iam.gserviceaccount.com"
)

type tokenClaims struct {
	Issuer   string `json:"iss"`
	Audience string `json:"aud"`
	Subject  string `json:"sub"`
	Email    string `json:"email"`
	Expiry   int64  `json:"exp"`
	IssuedAt int64  `json:"iat"`
}

// newTestSigning returns a mint function producing tokens signed by a key the returned
// JWKS URL publishes, plus a second mint function using an unpublished key to stand in
// for a forged signature.
func newTestSigning(t *testing.T) (certsURL string, mint func(tokenClaims) string, mintForeign func(tokenClaims) string) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	foreign, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate foreign key: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key: &key.PublicKey, KeyID: "test-key", Algorithm: "RS256", Use: "sig",
		}}})
	}))
	t.Cleanup(srv.Close)

	signWith := func(k *rsa.PrivateKey, kid string) func(tokenClaims) string {
		return func(c tokenClaims) string {
			t.Helper()
			signer, err := jose.NewSigner(
				jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: k, KeyID: kid}},
				(&jose.SignerOptions{}).WithType("JWT"),
			)
			if err != nil {
				t.Fatalf("new signer: %v", err)
			}
			payload, err := json.Marshal(c)
			if err != nil {
				t.Fatalf("marshal claims: %v", err)
			}
			obj, err := signer.Sign(payload)
			if err != nil {
				t.Fatalf("sign: %v", err)
			}
			tok, err := obj.CompactSerialize()
			if err != nil {
				t.Fatalf("serialize: %v", err)
			}
			return tok
		}
	}
	return srv.URL, signWith(key, "test-key"), signWith(foreign, "test-key")
}

// validClaims is a well-formed token body; tests mutate one field at a time from here so
// each case isolates exactly the check it is exercising.
func validClaims() tokenClaims {
	now := time.Now()
	return tokenClaims{
		Issuer:   testIssuer,
		Audience: testAudience,
		Subject:  "1234567890",
		Email:    testSA,
		IssuedAt: now.Add(-time.Minute).Unix(),
		Expiry:   now.Add(time.Hour).Unix(),
	}
}

func TestPushVerify(t *testing.T) {
	ctx := context.Background()
	certsURL, mint, mintForeign := newTestSigning(t)

	expired := validClaims()
	expired.Expiry = time.Now().Add(-time.Hour).Unix()
	expired.IssuedAt = time.Now().Add(-2 * time.Hour).Unix()

	wrongAud := validClaims()
	wrongAud.Audience = "some-other-audience"

	wrongIss := validClaims()
	wrongIss.Issuer = "https://accounts.example.com"

	wrongEmail := validClaims()
	wrongEmail.Email = "attacker@example.iam.gserviceaccount.com"

	tests := []struct {
		name    string
		authz   string // full Authorization header; "" means omit
		cfg     Config
		wantErr string // substring; "" means expect success
	}{
		{
			name:  "valid token",
			authz: "Bearer " + mint(validClaims()),
			cfg:   Config{PushAudience: testAudience, PushServiceAccount: testSA},
		},
		{
			name:  "valid token with no service account pinned skips the email check",
			authz: "Bearer " + mint(wrongEmail),
			cfg:   Config{PushAudience: testAudience},
		},
		{
			name:    "token minted for another audience",
			authz:   "Bearer " + mint(wrongAud),
			cfg:     Config{PushAudience: testAudience, PushServiceAccount: testSA},
			wantErr: "invalid token",
		},
		{
			name:    "expired token",
			authz:   "Bearer " + mint(expired),
			cfg:     Config{PushAudience: testAudience, PushServiceAccount: testSA},
			wantErr: "invalid token",
		},
		{
			name:    "signature from a key the JWKS does not publish",
			authz:   "Bearer " + mintForeign(validClaims()),
			cfg:     Config{PushAudience: testAudience, PushServiceAccount: testSA},
			wantErr: "invalid token",
		},
		{
			name:    "issuer other than Google",
			authz:   "Bearer " + mint(wrongIss),
			cfg:     Config{PushAudience: testAudience, PushServiceAccount: testSA},
			wantErr: "invalid token",
		},
		{
			name:    "valid Google token from an unexpected service account",
			authz:   "Bearer " + mint(wrongEmail),
			cfg:     Config{PushAudience: testAudience, PushServiceAccount: testSA},
			wantErr: "unexpected token issuer",
		},
		{
			name:    "missing Authorization header",
			cfg:     Config{PushAudience: testAudience, PushServiceAccount: testSA},
			wantErr: "missing bearer token",
		},
		{
			name:    "Authorization without the Bearer scheme",
			authz:   mint(validClaims()),
			cfg:     Config{PushAudience: testAudience, PushServiceAccount: testSA},
			wantErr: "missing bearer token",
		},
		{
			name:    "Bearer scheme with an empty token",
			authz:   "Bearer ",
			cfg:     Config{PushAudience: testAudience, PushServiceAccount: testSA},
			wantErr: "missing bearer token",
		},
		{
			name:    "garbage in place of a JWT",
			authz:   "Bearer not-a-jwt",
			cfg:     Config{PushAudience: testAudience, PushServiceAccount: testSA},
			wantErr: "invalid token",
		},
		{
			name:    "audience unconfigured fails closed",
			authz:   "Bearer " + mint(validClaims()),
			cfg:     Config{PushServiceAccount: testSA},
			wantErr: "not configured",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &pushHandler{cfg: &tt.cfg}
			// Mirrors runPush: the verifier exists only when an audience is configured.
			if tt.cfg.PushAudience != "" {
				h.verifier = newIDTokenVerifier(ctx, testIssuer, certsURL, tt.cfg.PushAudience)
			}

			r := httptest.NewRequest(http.MethodPost, "/", nil)
			if tt.authz != "" {
				r.Header.Set("Authorization", tt.authz)
			}

			err := h.verify(ctx, r)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("verify: want success, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("verify: want error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("verify: want error containing %q, got %v", tt.wantErr, err)
			}
		})
	}
}

// A nil verifier must reject even when an audience is set, so a construction path that
// forgets to build one can't leave the public endpoint unguarded.
func TestPushVerifyNilVerifierFailsClosed(t *testing.T) {
	h := &pushHandler{cfg: &Config{PushAudience: testAudience, PushServiceAccount: testSA}}
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.Header.Set("Authorization", "Bearer whatever")

	err := h.verify(context.Background(), r)
	if err == nil {
		t.Fatal("verify: want error for nil verifier, got nil")
	}
	if !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("verify: want 'not configured', got %v", err)
	}
}
