package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"

	"github.com/sloccy/ollamail-aws/db"
	"github.com/sloccy/ollamail-aws/gmail"
	"github.com/sloccy/ollamail-aws/llm"
	"github.com/sloccy/ollamail-aws/processor"
)

// pushHandler processes Gmail Pub/Sub push notifications: verify the sender, resolve
// the account, and run one labeling pass for it. It reuses the same lookback+dedup
// path as the scheduled scan, so a missed or duplicated notification is harmless.
type pushHandler struct {
	store *db.Store
	llm   *llm.Client
	auth  *gmail.Auth
	cfg   *Config
	// verifier checks the Pub/Sub OIDC token. Nil when PushAudience is unset, which
	// verify treats as a misconfiguration and rejects; see the fail-closed note there.
	verifier *oidc.IDTokenVerifier
}

// Google's OIDC issuer and JWKS endpoint for the ID tokens Pub/Sub attaches to push
// requests. Pinned rather than discovered via oidc.NewProvider so startup doesn't
// depend on a network round trip. go-oidc special-cases Google's scheme-less
// "accounts.google.com" issuer variant internally, so both forms verify.
const (
	googleIssuer   = "https://accounts.google.com"
	googleCertsURL = "https://www.googleapis.com/oauth2/v3/certs"
)

// newIDTokenVerifier builds a verifier for issuer, fetching signing keys from certsURL
// and requiring audience. The returned verifier is meant to be built once and reused:
// its RemoteKeySet caches the JWKS and refetches only on an unseen key id, so building
// one per request would refetch Google's keys on every push.
//
// audience must be non-empty — go-oidc rejects an empty ClientID unless
// SkipClientIDCheck is set, so a misconfigured audience fails closed rather than
// silently accepting tokens minted for anyone else.
func newIDTokenVerifier(ctx context.Context, issuer, certsURL, audience string) *oidc.IDTokenVerifier {
	return oidc.NewVerifier(issuer, oidc.NewRemoteKeySet(ctx, certsURL), &oidc.Config{ClientID: audience})
}

// runPush serves the push webhook behind the Lambda Web Adapter (public Function URL).
// Auth is enforced in-app by validating the Google-signed OIDC token on each request.
func runPush(cfg Config) {
	store, llmClient, gmailAuth := buildDeps(cfg)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	h := &pushHandler{store: store, llm: llmClient, auth: gmailAuth, cfg: &cfg}
	// Built once here, not per request, so Google's JWKS is fetched and cached rather
	// than refetched on every notification. Left nil when unconfigured; verify rejects.
	if cfg.PushAudience != "" {
		h.verifier = newIDTokenVerifier(ctx, googleIssuer, googleCertsURL, cfg.PushAudience)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /", h.handle)
	// Health check for the adapter / manual probes.
	mux.HandleFunc("GET /", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	serveHTTP(ctx, mux)
}

// pubsubPush is the envelope Pub/Sub POSTs; message.data is base64 Gmail notification JSON.
type pubsubPush struct {
	Message struct {
		Data      string `json:"data"`
		MessageID string `json:"messageId"`
	} `json:"message"`
}

type gmailNotification struct {
	EmailAddress string `json:"emailAddress"`
	HistoryID    uint64 `json:"historyId"`
}

func (h *pushHandler) handle(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if err := h.verify(ctx, r); err != nil {
		slog.Warn("push auth rejected", "err", err)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var env pubsubPush
	if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
		http.Error(w, "bad envelope", http.StatusBadRequest)
		return
	}
	raw, err := base64.StdEncoding.DecodeString(env.Message.Data)
	if err != nil {
		http.Error(w, "bad message data", http.StatusBadRequest)
		return
	}
	var notif gmailNotification
	if err := json.Unmarshal(raw, &notif); err != nil || notif.EmailAddress == "" {
		// Malformed payload — ack so Pub/Sub stops redelivering it.
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Ack quickly, then process. Missed acks are safe: dedup prevents reprocessing.
	if err := h.process(ctx, notif.EmailAddress, notif.HistoryID); err != nil {
		slog.Error("push process", "email", notif.EmailAddress, "err", err)
		h.store.Log("ERROR", "Push processing failed for "+notif.EmailAddress+": "+err.Error())
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *pushHandler) process(ctx context.Context, email string, historyID uint64) error {
	id, err := h.store.GetAccountByEmail(ctx, email)
	if err != nil {
		return fmt.Errorf("lookup account: %w", err)
	}
	account, err := h.store.GetAccount(ctx, id)
	if err != nil {
		return fmt.Errorf("get account: %w", err)
	}
	if account.Active == 0 {
		return nil
	}
	// Skip stale/duplicate Pub/Sub redeliveries. Gmail history ids are monotonic and we
	// advance WatchHistoryID past every change we process, so a notification whose id we've
	// already passed carries nothing new — ack without the ListActivePrompts + history.list
	// round trips. Empty baseline is left to ProcessAccountHistory's full-scan reseed.
	if account.WatchHistoryID != "" {
		if stored, perr := strconv.ParseUint(account.WatchHistoryID, 10, 64); perr == nil && historyID <= stored {
			return nil
		}
	}
	prompts, err := h.store.ListActivePrompts(ctx)
	if err != nil {
		return fmt.Errorf("list prompts: %w", err)
	}
	procCfg := h.cfg.processConfig(true)
	start := time.Now()
	// History-driven: only new inbox messages since the stored history id are processed,
	// so our own label/read changes don't retrigger. Advance the stored id afterwards.
	newHist, perr := processor.ProcessAccountHistory(ctx, h.store, h.llm, h.auth, account, prompts, procCfg)
	if perr != nil {
		return perr
	}
	if newHist == "" {
		// Fell back to a full scan (no/expired baseline) — reseed from the notification id.
		newHist = strconv.FormatUint(historyID, 10)
	}
	if newHist != account.WatchHistoryID {
		_ = h.store.UpdateAccountWatch(ctx, db.UpdateAccountWatchParams{
			ID: account.ID, HistoryID: newHist, Expiration: account.WatchExpiration,
		})
	}
	slog.Info("push processed", "email", email, "elapsed", time.Since(start))
	return nil
}

// verify validates the Pub/Sub OIDC bearer token: Google-signed, non-expired, matching
// our configured audience, and issued by the expected push service account. This is the
// only thing guarding the public endpoint, so it fails closed on any misconfiguration.
func (h *pushHandler) verify(ctx context.Context, r *http.Request) error {
	if h.cfg.PushAudience == "" || h.verifier == nil {
		return errors.New("push audience not configured")
	}
	authz := r.Header.Get("Authorization")
	token, ok := strings.CutPrefix(authz, "Bearer ")
	if !ok || token == "" {
		return errors.New("missing bearer token")
	}
	// Verify checks the Google signature, issuer, audience and expiry; the service
	// account is a claim we narrow on ourselves afterwards.
	idTok, err := h.verifier.Verify(ctx, token)
	if err != nil {
		return fmt.Errorf("invalid token: %w", err)
	}
	if h.cfg.PushServiceAccount != "" {
		var claims struct {
			Email string `json:"email"`
		}
		if err := idTok.Claims(&claims); err != nil {
			return fmt.Errorf("invalid token claims: %w", err)
		}
		if claims.Email != h.cfg.PushServiceAccount {
			return fmt.Errorf("unexpected token issuer %q", claims.Email)
		}
	}
	return nil
}
