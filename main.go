package main

import (
	"context"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/sloccy/ollamail/db"
	"github.com/sloccy/ollamail/gmail"
	"github.com/sloccy/ollamail/llm"
	"github.com/sloccy/ollamail/poller"
	"github.com/sloccy/ollamail/processor"
)

func main() {
	cfg := loadConfig()

	// Logging
	level := slog.LevelInfo
	if cfg.DebugLogging {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	// Database
	dbPath := filepath.Join(cfg.DataDir, "labeler.db")
	store, err := db.Open(dbPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer func() { _ = store.Close() }()

	if err := store.Migrate(); err != nil {
		log.Fatalf("migrate db: %v", err) //nolint:gocritic // OS reclaims file handle on Fatalf
	}

	// Seed default poll_interval setting
	if err := store.SeedSetting("poll_interval", strconv.Itoa(cfg.PollInterval)); err != nil {
		log.Fatalf("seed settings: %v", err)
	}

	// Secret key for HMAC session signing
	secretKey, err := store.GetOrCreateSecretKey()
	if err != nil {
		log.Fatalf("secret key: %v", err)
	}

	// LLM client
	ollamaClient := llm.NewClient(cfg.OllamaHost, cfg.OllamaModel, cfg.OllamaNumCtx, time.Duration(cfg.OllamaTimeout)*time.Second)

	// Pull model in background
	go func() {
		if err := ollamaClient.EnsureModelPulled(store); err != nil {
			slog.Warn("model pull check failed", "err", err)
		}
	}()

	// Gmail auth
	gmailAuth := gmail.NewAuth(cfg.CredentialsFile)

	// Graceful shutdown context — created early so goroutines can observe it.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Seed the Troubleshooting debug table with the 3 most recent processed
	// emails when the table is empty. Delayed 10s to allow Gmail auth to be
	// ready before fetching.
	go func() {
		select {
		case <-time.After(10 * time.Second):
		case <-ctx.Done():
			return
		}
		if err := processor.BackfillLlmDebug(ctx, store, ollamaClient, gmailAuth,
			processor.ProcessConfig{BodyTruncation: cfg.EmailBodyTrunc}); err != nil {
			slog.Warn("llm debug backfill failed", "err", err)
		}
	}()

	// Poller
	p := poller.New(store, ollamaClient, gmailAuth, &poller.Config{
		LookbackHours:  cfg.GmailLookbackHours,
		MaxResults:     int64(cfg.GmailMaxResults),
		BodyTruncation: cfg.EmailBodyTrunc,
		LogRetention:   cfg.LogRetentionDays,
		DebugLogging:   cfg.DebugLogging,
	})
	p.Start()

	// HTTP server
	srv := newServer(ctx, store, ollamaClient, p, gmailAuth, &cfg, secretKey)
	httpSrv := &http.Server{
		Addr:              ":5000",
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		slog.Info("listening", "addr", httpSrv.Addr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http server: %v", err)
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	p.Stop()
}
