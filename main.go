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
		store, llmClient, gmailAuth := buildDeps(cfg)
		lambda.Start(func(ctx context.Context) error {
			scanOnce(ctx, store, llmClient, gmailAuth, &cfg)
			return nil
		})
	case "push":
		runPush(cfg)
	case "improve":
		// Invoked async (Event) by WebFunction's dispatchImprove — see improve.go and
		// ImproveFunction in template.yaml for why this work moved out of a goroutine
		// inside WebFunction and into its own Lambda invocation.
		store, llmClient, _ := buildDeps(cfg)
		runner := newImproveRunner(store, llmClient, &cfg)
		lambda.Start(runner.handle)
	default:
		runWeb(cfg)
	}
}

func buildDeps(cfg Config) (*db.Store, *llm.Client, *gmail.Auth) {
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
	llmClient := llm.NewClient(store, cfg.BedrockModel)
	gmailAuth := gmail.NewAuth()
	return store, llmClient, gmailAuth
}

func runWeb(cfg Config) {
	// Fail closed: AUTH_MODE=cfaccess (set by template.yaml exactly when the Function URL
	// is public AuthType NONE) means the in-app Cloudflare Access JWT check is the only
	// auth gate. If the CF vars have drifted away, refuse to serve rather than serve
	// the UI unauthenticated. Checked before any defer is registered (log.Fatalf skips
	// deferred calls) and before touching AWS.
	if cfg.AuthMode == "cfaccess" && (cfg.CfAccessTeamDomain == "" || cfg.CfAccessAud == "") {
		log.Fatalf("AUTH_MODE=cfaccess but CF_ACCESS_TEAM_DOMAIN/CF_ACCESS_AUD unset — the Function URL is public and unverified; refusing to start")
	}

	store, llmClient, gmailAuth := buildDeps(cfg)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Scheduled scanning is handled by the ScanFunction (EventBridge, fixed daily 2 AM ET
	// cron — see ScanSchedule in template.yaml); the web UI's "Scan Now" runs an on-demand
	// pass in-process via scanOnce.
	srv := newServer(ctx, store, llmClient, gmailAuth, &cfg)
	// Security middleware outermost so headers land on every response, including the
	// Cloudflare Access middleware's own 403s.
	handler := newSecurityMiddleware(newCfAccessMiddleware(ctx, cfg.CfAccessTeamDomain, cfg.CfAccessAud)(srv))

	serveHTTP(ctx, handler)
}

// serveHTTP runs an HTTP server on AWS_LWA_PORT (default 5000) with handler, behind the
// Lambda Web Adapter, shutting down gracefully when ctx is cancelled. Shared by runWeb
// and runPush — the two Function URL-backed modes need identical bootstrap/shutdown.
func serveHTTP(ctx context.Context, handler http.Handler) {
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
