package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"google.golang.org/api/idtoken"

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
}

// runPush serves the push webhook behind the Lambda Web Adapter (public Function URL).
// Auth is enforced in-app by validating the Google-signed OIDC token on each request.
func runPush(cfg Config) {
	store, llmClient, gmailAuth, _ := buildDeps(cfg)
	defer func() { _ = store.Close() }()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	h := &pushHandler{store: store, llm: llmClient, auth: gmailAuth, cfg: &cfg}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /", h.handle)
	// Health check for the adapter / manual probes.
	mux.HandleFunc("GET /", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	port := os.Getenv("AWS_LWA_PORT")
	if port == "" {
		port = "5000"
	}
	httpSrv := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		slog.Info("push listening", "addr", httpSrv.Addr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http server: %v", err)
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
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
	prompts, err := h.store.ListActivePrompts(ctx)
	if err != nil {
		return fmt.Errorf("list prompts: %w", err)
	}
	procCfg := processor.ProcessConfig{
		LookbackHours:  h.cfg.GmailLookbackHours,
		MaxResults:     int64(h.cfg.GmailMaxResults),
		BodyTruncation: h.cfg.EmailBodyTrunc,
		DebugLogging:   h.cfg.DebugLogging,
	}
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
	if h.cfg.PushAudience == "" {
		return errors.New("push audience not configured")
	}
	authz := r.Header.Get("Authorization")
	token, ok := strings.CutPrefix(authz, "Bearer ")
	if !ok || token == "" {
		return errors.New("missing bearer token")
	}
	payload, err := idtoken.Validate(ctx, token, h.cfg.PushAudience)
	if err != nil {
		return fmt.Errorf("invalid token: %w", err)
	}
	if h.cfg.PushServiceAccount != "" {
		email, _ := payload.Claims["email"].(string)
		if email != h.cfg.PushServiceAccount {
			return fmt.Errorf("unexpected token issuer %q", email)
		}
	}
	return nil
}
