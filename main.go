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

	if cfg.Mode == "scan" {
		// EventBridge invokes this via the Lambda Runtime API; lambda.Start keeps the
		// process alive between scheduled invocations instead of exiting after one pass.
		lambda.Start(func(ctx context.Context) error {
			runScan(cfg)
			return nil
		})
		return
	}
	runWeb(cfg)
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
	if err := store.SeedSetting("poll_interval", strconv.Itoa(cfg.PollInterval)); err != nil {
		log.Fatalf("seed settings: %v", err)
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

	// No in-process poller in Lambda web mode — EventBridge triggers the scan function.
	srv := newServer(ctx, store, llmClient, nil, gmailAuth, &cfg, secretKey)

	// Enforce Cloudflare Access (Google SSO) when configured; otherwise pass-through
	// and rely on the AWS_IAM Function URL.
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
