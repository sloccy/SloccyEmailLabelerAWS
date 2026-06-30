package processor

import (
	"context"
	"log/slog"
	"slices"

	"github.com/sloccy/ollamail/db"
	gmailpkg "github.com/sloccy/ollamail/gmail"
	"github.com/sloccy/ollamail/llm"
)

// BackfillLlmDebug seeds the llm_debug table with the 3 most recent processed
// emails when the table is empty on boot. It re-fetches each message from Gmail
// and rebuilds the LLM request JSON locally (no LLM call). llm_response is
// taken from categorization_history. If the table already has any rows, this is
// a no-op.
func BackfillLlmDebug(ctx context.Context, store *db.Store, ollamaClient *llm.Client, gmailAuth *gmailpkg.Auth, cfg ProcessConfig) error {
	// Purge rows missing gmail_raw or llm_request so the backfill can retry
	// rather than being stuck on incomplete rows from a prior boot.
	if err := store.DeleteIncompleteLlmDebug(ctx); err != nil {
		slog.Warn("backfill: purge incomplete rows", "err", err)
	}

	existing, err := store.GetLatestLlmDebug(ctx)
	if err != nil {
		return err
	}
	if len(existing) > 0 {
		return nil
	}

	history, err := store.GetHistory(ctx, 3)
	if err != nil {
		return err
	}
	if len(history) == 0 {
		return nil
	}

	allPrompts, err := store.ListPrompts(ctx)
	if err != nil {
		return err
	}

	// Build a gmail client + filtered prompts per account (at most one per unique account_id).
	type svcEntry struct {
		svc     *gmailpkg.Client
		prompts []db.Prompt
	}
	svcCache := make(map[int64]*svcEntry, len(history))
	for _, h := range history {
		if _, ok := svcCache[h.AccountID]; ok {
			continue
		}
		account, err := store.GetAccount(ctx, h.AccountID)
		if err != nil {
			slog.Warn("backfill: get account", "account_id", h.AccountID, "err", err)
			continue
		}
		svc, prompts, err := setupAccountContext(ctx, store, gmailAuth, account, allPrompts)
		if err != nil {
			slog.Warn("backfill: setup account", "account", account.Email, "err", err)
			continue
		}
		svcCache[h.AccountID] = &svcEntry{svc: svc, prompts: prompts}
	}

	// Fetch all messages concurrently, grouped by account client.
	msgsByAccount := make(map[int64][]string, len(svcCache))
	for _, h := range history {
		if _, ok := svcCache[h.AccountID]; ok {
			msgsByAccount[h.AccountID] = append(msgsByAccount[h.AccountID], h.MessageID)
		}
	}
	messages := make(map[string]gmailpkg.Message, len(history))
	for accountID, ids := range msgsByAccount {
		entry := svcCache[accountID]
		msgCh, errCh := gmailpkg.IterMessageDetails(ctx, entry.svc, ids, cfg.BodyTruncation)
		for msg := range msgCh {
			messages[msg.ID] = msg
		}
		if err := <-errCh; err != nil {
			slog.Warn("backfill: fetch messages", "account_id", accountID, "err", err)
		}
	}

	// Insert oldest-first (history is id DESC) so the newest row gets the highest llm_debug id.
	for _, h := range slices.Backward(history) {
		entry, ok := svcCache[h.AccountID]
		if !ok {
			continue
		}
		msg, ok := messages[h.MessageID]
		if !ok {
			slog.Warn("backfill: message not fetched, skipping", "message_id", h.MessageID)
			continue
		}

		gmailRaw := marshalGmailDebug(msg)

		var llmRequest string
		if len(entry.prompts) > 0 {
			llmPrompts := make([]llm.Prompt, len(entry.prompts))
			for j, p := range entry.prompts {
				llmPrompts[j] = llm.Prompt{ID: p.ID, Name: p.Name, Instructions: p.Instructions}
			}
			llmRequest = ollamaClient.BuildClassifyRequestJSON(llm.Email{
				Sender:  msg.Sender,
				Subject: msg.Subject,
				Body:    msg.Body,
				Snippet: msg.Snippet,
			}, llmPrompts)
		}

		if llmRequest == "" {
			slog.Warn("backfill: no prompts for account, skipping", "message_id", h.MessageID)
			continue
		}

		if err := store.AddLlmDebug(ctx, db.AddLlmDebugParams{
			AccountID:    h.AccountID,
			AccountEmail: h.AccountEmail,
			MessageID:    h.MessageID,
			Subject:      h.Subject,
			Sender:       h.Sender,
			GmailRaw:     gmailRaw,
			LlmRequest:   llmRequest,
			LlmResponse:  h.LlmResponse,
		}); err != nil {
			slog.Warn("backfill: insert llm_debug", "message_id", h.MessageID, "err", err)
		}
	}

	return store.TrimLlmDebug(ctx)
}
