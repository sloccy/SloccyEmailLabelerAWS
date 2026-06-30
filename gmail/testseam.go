package gmail

import (
	"encoding/json"
	"net/http"
	"time"

	"golang.org/x/oauth2"
)

// SetBaseURLForTest replaces the Gmail API base URL and returns a restore function.
// Intended for use in tests only. Not safe for concurrent calls.
func SetBaseURLForTest(url string) func() {
	old := gmailBase
	gmailBase = url
	return func() { gmailBase = old }
}

// NewTestClient creates an unauthenticated Client whose HTTP calls go to gmailBase.
// Intended for use in tests only.
func NewTestClient() *Client {
	fakeToken := &oauth2.Token{
		AccessToken:  "test-access-token",
		RefreshToken: "test-refresh-token",
		TokenType:    "Bearer",
		Expiry:       time.Now().Add(time.Hour),
	}
	credJSON, _ := json.Marshal(fakeToken) //nolint:gosec,errchkjson
	cfg := &oauth2.Config{
		ClientID:     "test-client-id",
		ClientSecret: "test-client-secret",
		Endpoint: oauth2.Endpoint{ //nolint:gosec
			TokenURL: "http://localhost:0/token",
		},
	}
	svc, _ := NewService(testContext{}, string(credJSON), cfg, nil)
	if svc == nil {
		// Fallback: plain http.Client if NewService fails.
		return &Client{http: &http.Client{}}
	}
	return svc
}

// testContext is a minimal context.Context used by NewTestClient.
type testContext struct{}

func (testContext) Deadline() (deadline time.Time, ok bool) { return }
func (testContext) Done() <-chan struct{}                   { return nil }
func (testContext) Err() error                              { return nil }
func (testContext) Value(_ any) any                         { return nil }
