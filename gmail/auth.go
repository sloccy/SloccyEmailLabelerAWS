package gmail

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

const (
	redirectURI = "http://localhost"
)

var scopes = []string{
	"https://www.googleapis.com/auth/gmail.modify",
	"https://www.googleapis.com/auth/userinfo.email",
	"openid",
}

// Auth handles OAuth2 for Gmail.
type Auth struct {
	credentialsFile string
	once            sync.Once
	cachedCfg       *oauth2.Config
	cachedErr       error
}

func NewAuth(credentialsFile string) *Auth {
	return &Auth{credentialsFile: credentialsFile}
}

func (a *Auth) loadConfig() (*oauth2.Config, error) {
	a.once.Do(func() {
		data, err := os.ReadFile(a.credentialsFile)
		if err != nil {
			a.cachedErr = fmt.Errorf("read credentials file: %w", err)
			return
		}
		cfg, err := google.ConfigFromJSON(data, scopes...)
		if err != nil {
			a.cachedErr = fmt.Errorf("parse credentials: %w", err)
			return
		}
		cfg.RedirectURL = redirectURI
		a.cachedCfg = cfg
	})
	return a.cachedCfg, a.cachedErr
}

// GetAuthURL returns the Google OAuth2 consent URL for the given state.
func (a *Auth) GetAuthURL(state string) (string, error) {
	cfg, err := a.loadConfig()
	if err != nil {
		return "", err
	}
	return cfg.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.ApprovalForce), nil
}

// ExchangeCode exchanges an auth code for credentials JSON and returns (email, credentialsJSON).
func (a *Auth) ExchangeCode(ctx context.Context, code string) (string, string, error) {
	cfg, err := a.loadConfig()
	if err != nil {
		return "", "", err
	}
	token, err := cfg.Exchange(ctx, code)
	if err != nil {
		return "", "", fmt.Errorf("exchange code: %w", err)
	}
	email, err := fetchEmail(ctx, cfg.Client(ctx, token))
	if err != nil {
		return "", "", err
	}
	credJSON, err := json.Marshal(token) //nolint:gosec // G117: token serialization is intentional
	if err != nil {
		return "", "", err
	}
	return email, string(credJSON), nil
}

var userinfoURL = "https://www.googleapis.com/oauth2/v2/userinfo"

func fetchEmail(ctx context.Context, client *http.Client) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, userinfoURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	var info struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return "", err
	}
	return info.Email, nil
}

// TokenFromJSON deserializes an oauth2.Token from JSON.
func TokenFromJSON(data string) (*oauth2.Token, error) {
	var token oauth2.Token
	if err := json.Unmarshal([]byte(data), &token); err != nil {
		return nil, err
	}
	return &token, nil
}

// ConfigFromFile returns the oauth2.Config from the credentials file.
func (a *Auth) ConfigFromFile() (*oauth2.Config, error) {
	return a.loadConfig()
}
