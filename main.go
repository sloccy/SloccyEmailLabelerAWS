package main

import (
	"context"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
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
	store, err := db.Open()
	if err != nil {
		log.Fatalf("open db: %v", err)
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
	gmailAuth := gmail.NewAuth()
	return store, llmClient, gmailAuth, secretKey
}

func runWeb(cfg Config) {
	store, llmClient, gmailAuth, secretKey := buildDeps(cfg)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Scheduled scanning is handled by the ScanFunction (EventBridge, fixed daily 2 AM ET
	// cron — see ScanSchedule in template.yaml); the web UI's "Scan Now" runs an on-demand
	// pass in-process via scanOnce.
	srv := newServer(ctx, store, llmClient, gmailAuth, &cfg, secretKey)
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
