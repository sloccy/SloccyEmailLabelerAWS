package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/sloccy/ollamail-aws/processor"
	"github.com/sloccy/ollamail-aws/retention"
)

// runScan performs one full email-labeling pass and returns.
// In Lambda, this is invoked by EventBridge on a schedule.
func runScan(cfg Config) {
	store, llmClient, gmailAuth, _ := buildDeps(cfg)
	defer func() { _ = store.Close() }()

	ctx := context.Background()

	_ = store.TrimLogs(ctx, cfg.LogRetentionDays)
	_ = store.TrimProcessedEmails(ctx, cfg.GmailLookbackHours)
	_ = store.TrimHistory(ctx, cfg.LogRetentionDays)

	accounts, err := store.ListAccounts(ctx)
	if err != nil {
		slog.Error("list accounts", "err", err)
		return
	}
	prompts, err := store.ListActivePrompts(ctx)
	if err != nil {
		slog.Error("list prompts", "err", err)
		return
	}

	procCfg := processor.ProcessConfig{
		LookbackHours:  cfg.GmailLookbackHours,
		MaxResults:     int64(cfg.GmailMaxResults),
		BodyTruncation: cfg.EmailBodyTrunc,
		DebugLogging:   cfg.DebugLogging,
	}

	for _, account := range accounts {
		if account.Active == 0 {
			continue
		}
		start := time.Now()
		wrapper, err := processor.ProcessAccount(ctx, store, llmClient, gmailAuth, account, prompts, procCfg)
		if err != nil {
			slog.Error("process account", "email", account.Email, "err", err)
			store.Log("ERROR", "Scan failed for "+account.Email+": "+err.Error())
			continue
		}
		if wrapper != nil {
			retention.Cleanup(ctx, store, wrapper.Svc, account.ID)
		}
		slog.Info("scan complete", "email", account.Email, "elapsed", time.Since(start))
	}
}
