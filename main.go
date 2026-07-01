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

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/sloccy/ollamail-aws/db"
	"github.com/sloccy/ollamail-aws/gmail"
	"github.com/sloccy/ollamail-aws/llm"
)

func main() {
	cfg := loadConfig()

	level := slog.LevelInfo
	if cfg.DebugLogging {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	switch cfg.Mode {
	case "scan":
		// EventBridge invokes this via the Lambda Runtime API; lambda.Start keeps the
		// process alive between scheduled invocations. Build deps once here (not per
		// invocation) so warm invokes skip the redundant client/config/seed work.
		store, llmClient, gmailAuth, _ := buildDeps(cfg)
		lambda.Start(func(ctx context.Context) error {
			scanOnce(ctx, store, llmClient, gmailAuth, &cfg)
			return nil
		})
	case "push":
		runPush(cfg)
	default:
		runWeb(cfg)
	}
}

func buildDeps(cfg Config) (*db.Store, *llm.Client, *gmail.Auth, []byte) {
	dbPath := filepath.Join(cfg.DataDir, "labeler.db")
	store, err := db.Open(dbPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	if err := store.Migrate(); err != nil {
		log.Fatalf("migrate db: %v", err)
	}
	if err := store.SeedSetting(llm.SettingClassifyModel, cfg.BedrockModel); err != nil {
		log.Fatalf("seed classify_model: %v", err)
	}
	if err := store.SeedSetting(llm.SettingImproveModel, cfg.BedrockModel); err != nil {
		log.Fatalf("seed improve_model: %v", err)
	}
	secretKey, err := store.GetOrCreateSecretKey()
	if err != nil {
		log.Fatalf("secret key: %v", err)
	}
	llmClient := llm.NewClient(store, cfg.BedrockModel)
	gmailAuth := gmail.NewAuth(cfg.CredentialsFile)
	return store, llmClient, gmailAuth, secretKey
}

func runWeb(cfg Config) {
	store, llmClient, gmailAuth, secretKey := buildDeps(cfg)
	defer func() { _ = store.Close() }()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sched, err := newScanScheduler(ctx, cfg)
	if err != nil {
		slog.Error("init scan scheduler", "err", err)
	}
	// A fresh `sam deploy` resets the schedule to the template baseline; re-apply the stored
	// interval so the user's choice survives deploys.
	if sched != nil {
		if v, gerr := store.GetSetting(ctx, llm.SettingScanInterval); gerr == nil {
			if n, cerr := strconv.Atoi(v); cerr == nil && n >= 1 {
				if uerr := sched.UpdateInterval(ctx, n); uerr != nil {
					slog.Error("resync scan schedule", "err", uerr)
				}
			}
		}
	}

	// Scheduled scanning is handled by the ScanFunction (EventBridge); the web UI's
	// "Scan Now" runs an on-demand pass in-process via scanOnce.
	srv := newServer(ctx, store, llmClient, gmailAuth, &cfg, secretKey, sched)
	handler := newCfAccessMiddleware(ctx, cfg.CfAccessTeamDomain, cfg.CfAccessAud)(srv)

	port := os.Getenv("AWS_LWA_PORT")
	if port == "" {
		port = "5000"
	}
	httpSrv := &http.Server{
		Addr:              ":" + port,
		Handler:           handler,
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
}
