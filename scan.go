package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/sloccy/ollamail-aws/db"
	"github.com/sloccy/ollamail-aws/gmail"
	"github.com/sloccy/ollamail-aws/llm"
	"github.com/sloccy/ollamail-aws/processor"
	"github.com/sloccy/ollamail-aws/retention"
)

// scanOnce runs one full email-labeling pass against already-built deps.
// Shared by the scheduled ScanFunction and the web UI "Scan Now" button.
// Retention of logs/history/processed-markers is enforced entirely by item-level DynamoDB
// TTLs — no trim pass needed for those. Prompt examples are the one exception: they have no
// TTL by design (permanent history), so prunePromptExamples (prune.go) runs once per scan to
// keep each rule's corpus bounded instead of growing forever — see its own doc comment.
func scanOnce(ctx context.Context, store *db.Store, llmClient *llm.Client, gmailAuth *gmail.Auth, cfg *Config) {
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

	procCfg := cfg.processConfig(false)

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
			// Renew the Gmail push subscription (Gmail expires watch() after ~7 days)
			// while we already hold an authenticated client for this account.
			if cfg.PubSubTopic != "" {
				if res, werr := wrapper.Svc.Watch(ctx, cfg.PubSubTopic); werr != nil {
					slog.Error("renew watch", "email", account.Email, "err", werr)
				} else {
					_ = store.UpdateAccountWatch(ctx, db.UpdateAccountWatchParams{
						ID:         account.ID,
						HistoryID:  res.HistoryID,
						Expiration: res.Expiration,
					})
				}
			}
			retention.Cleanup(ctx, store, wrapper.Svc, account.ID)
		}
		slog.Info("scan complete", "email", account.Email, "elapsed", time.Since(start))
	}

	// DynamoDB-only, no Gmail client needed — runs once per scan, not per account, since
	// PromptExample's partition key is per-prompt, not per-account (see prune.go).
	prunePromptExamples(ctx, store, prompts)
}
