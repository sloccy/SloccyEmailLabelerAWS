package processor

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/sloccy/ollamail/db"
	gmailpkg "github.com/sloccy/ollamail/gmail"
	"github.com/sloccy/ollamail/llm"
)

// EmailModify tracks label changes for a single message.
type EmailModify struct {
	MessageID    string
	AddLabels    []string
	RemoveLabels []string
}

// setupAccountContext loads OAuth config, creates a Gmail client, and filters
// prompts for the given account. Shared by ProcessAccount and BackfillLlmDebug.
func setupAccountContext(ctx context.Context, store *db.Store, gmailAuth *gmailpkg.Auth, account db.Account, allPrompts []db.Prompt) (*gmailpkg.Client, []db.Prompt, error) {
	oauthCfg, err := gmailAuth.ConfigFromFile()
	if err != nil {
		return nil, nil, fmt.Errorf("load oauth config: %w", err)
	}
	svc, err := gmailpkg.NewService(ctx, account.CredentialsJson, oauthCfg, func(newCreds string) {
		_ = store.UpdateAccountCredentials(ctx, db.UpdateAccountCredentialsParams{
			CredentialsJson: newCreds,
			ID:              account.ID,
		})
	})
	if err != nil {
		return nil, nil, fmt.Errorf("create gmail service: %w", err)
	}
	return svc, filterPrompts(allPrompts, account.ID), nil
}

// marshalGmailDebug serialises a Gmail message to compact JSON for the debug table.
func marshalGmailDebug(msg gmailpkg.Message) string {
	b, err := json.Marshal(msg)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// ProcessAccount processes all new emails for one account.
// Returns the Gmail service so it can be reused by retention.
func ProcessAccount(ctx context.Context, store *db.Store, ollamaClient *llm.Client, gmailAuth *gmailpkg.Auth, account db.Account, allPrompts []db.Prompt, cfg ProcessConfig) (*gmailpkg.ServiceWrapper, error) {
	svc, prompts, err := setupAccountContext(ctx, store, gmailAuth, account, allPrompts)
	if err != nil {
		return nil, err
	}
	wrapper := &gmailpkg.ServiceWrapper{Svc: svc}

	if len(prompts) == 0 {
		return wrapper, nil
	}

	// List recent messages
	messageIDs, err := gmailpkg.ListRecentMessageIDs(ctx, svc, cfg.LookbackHours, cfg.MaxResults)
	if err != nil {
		return wrapper, fmt.Errorf("list messages: %w", err)
	}

	// Filter out already-processed
	unprocessed, err := store.FilterUnprocessed(ctx, account.ID, messageIDs)
	if err != nil {
		return wrapper, fmt.Errorf("filter unprocessed: %w", err)
	}
	if len(unprocessed) == 0 {
		store.Log("INFO", fmt.Sprintf("[%s] No new emails to process.", account.Email))
		_ = store.UpdateLastScan(ctx, account.ID)
		return wrapper, nil
	}
	store.Log("INFO", fmt.Sprintf("[%s] Processing %d new email(s) against %d rule(s).", account.Email, len(unprocessed), len(prompts)))

	// Build label cache for all needed labels
	var neededLabels []string
	for _, p := range prompts {
		if p.LabelName != "" {
			neededLabels = append(neededLabels, p.LabelName)
		}
	}
	labelCache, err := gmailpkg.BuildLabelCache(ctx, svc, neededLabels)
	if err != nil {
		return wrapper, fmt.Errorf("build label cache: %w", err)
	}

	// Fetch and classify messages
	msgCh, errCh := gmailpkg.IterMessageDetails(ctx, svc, unprocessed, cfg.BodyTruncation)

	var allModifies []gmailpkg.Modify
	var trashIDs []string

	for msg := range msgCh {
		modifies, trash := processEmail(ctx, store, ollamaClient, account, msg, prompts, labelCache, cfg.DebugLogging)
		for _, m := range modifies {
			allModifies = append(allModifies, gmailpkg.Modify{
				MessageIDs:   []string{m.MessageID},
				AddLabels:    m.AddLabels,
				RemoveLabels: m.RemoveLabels,
			})
		}
		trashIDs = append(trashIDs, trash...)
	}
	if err := <-errCh; err != nil {
		slog.Error("fetch message details", "account", account.Email, "err", err)
	}

	// Apply all Gmail modifications
	if len(allModifies) > 0 {
		if err := gmailpkg.BatchModifyEmails(ctx, svc, allModifies); err != nil {
			slog.Error("batch modify failed", "account", account.Email, "err", err)
		}
	}
	if len(trashIDs) > 0 {
		if err := gmailpkg.BatchTrashEmails(ctx, svc, trashIDs); err != nil {
			slog.Error("batch trash failed", "account", account.Email, "err", err)
		}
	}

	// Update last scan timestamp
	_ = store.UpdateLastScan(ctx, account.ID)

	return wrapper, nil
}

func processEmail(
	ctx context.Context,
	store *db.Store,
	ollamaClient *llm.Client,
	account db.Account,
	msg gmailpkg.Message,
	prompts []db.Prompt,
	labelCache map[string]string,
	debugLogging bool,
) (modifies []EmailModify, trashIDs []string) {
	llmPrompts := make([]llm.Prompt, len(prompts))
	for i, p := range prompts {
		llmPrompts[i] = llm.Prompt{ID: p.ID, Name: p.Name, Instructions: p.Instructions}
	}

	email := llm.Email{
		Sender:  msg.Sender,
		Subject: msg.Subject,
		Body:    msg.Body,
		Snippet: msg.Snippet,
	}

	store.Log("INFO", fmt.Sprintf("[%s] Classifying: '%s' from %s",
		account.Email, gmailpkg.Truncate(msg.Subject, 60), gmailpkg.Truncate(msg.Sender, 60)))

	gmailRaw := marshalGmailDebug(msg)

	classified, llmErr := ollamaClient.ClassifyEmailBatch(ctx, store, email, llmPrompts)

	var logs []db.LogEntry
	var history []db.HistoryEntry

	if llmErr != nil {
		logs = append(logs, db.LogEntry{Level: "WARNING", Message: fmt.Sprintf("LLM error for %q: %v — will retry", msg.Subject, llmErr)})
		if err := store.BatchInsertProcessingResults(ctx, logs, nil, account.ID, ""); err != nil {
			slog.Error("db log write failed", "err", err)
		}
		return nil, nil // Don't mark processed; will retry
	}

	var matched []string
	for _, p := range prompts {
		if classified.Results[p.ID] {
			matched = append(matched, p.Name)
		}
	}
	if len(matched) > 0 {
		store.Log("INFO", fmt.Sprintf("[%s] Classification done: %d match(es): %v", account.Email, len(matched), matched))
	} else {
		store.Log("INFO", fmt.Sprintf("[%s] Classification done: 0 match(es): none", account.Email))
	}

	stop := false
	for _, p := range prompts {
		if stop {
			break
		}
		matched := classified.Results[p.ID]
		if !matched {
			continue
		}

		var actions []string
		if p.LabelName != "" {
			actions = append(actions, "labeled → "+p.LabelName)
		}

		mod := EmailModify{MessageID: msg.ID}

		// Apply label
		if p.LabelName != "" {
			if labelID, ok := labelCache[p.LabelName]; ok {
				mod.AddLabels = append(mod.AddLabels, labelID)
			}
		}

		// Apply actions (spam takes priority over trash/archive)
		switch {
		case p.ActionSpam != 0:
			mod.AddLabels = append(mod.AddLabels, gmailpkg.LabelSpam)
			mod.RemoveLabels = append(mod.RemoveLabels, gmailpkg.LabelInbox)
			actions = append(actions, "sent to spam")
		case p.ActionTrash != 0:
			trashIDs = append(trashIDs, msg.ID)
			actions = append(actions, "trashed")
		case p.ActionArchive != 0:
			mod.RemoveLabels = append(mod.RemoveLabels, gmailpkg.LabelInbox)
			actions = append(actions, "archived")
		}

		if p.ActionMarkRead != 0 {
			mod.RemoveLabels = append(mod.RemoveLabels, gmailpkg.LabelUnread)
			actions = append(actions, "marked as read")
		}

		if p.StopProcessing != 0 {
			actions = append(actions, "stopped further rules")
			stop = true
		}

		if len(mod.AddLabels) > 0 || len(mod.RemoveLabels) > 0 {
			modifies = append(modifies, mod)
		}

		logs = append(logs, db.LogEntry{
			Level:   "INFO",
			Message: fmt.Sprintf("[%s] '%s' \u2014 %s (rule: %s)", account.Email, gmailpkg.Truncate(msg.Subject, 60), strings.Join(actions, ", "), p.Name),
		})
		history = append(history, db.HistoryEntry{
			AccountID:    account.ID,
			AccountEmail: account.Email,
			MessageID:    msg.ID,
			Subject:      msg.Subject,
			Sender:       msg.Sender,
			PromptID:     sql.NullInt64{Int64: p.ID, Valid: true},
			PromptName:   sql.NullString{String: p.Name, Valid: true},
			LabelName:    sql.NullString{String: p.LabelName, Valid: p.LabelName != ""},
			Actions:      strings.Join(actions, ", "),
			LlmResponse:  classified.RawResponse,
		})
	}

	// If no prompts matched, record a "no match" entry
	if len(history) == 0 {
		history = append(history, db.HistoryEntry{
			AccountID:    account.ID,
			AccountEmail: account.Email,
			MessageID:    msg.ID,
			Subject:      msg.Subject,
			Sender:       msg.Sender,
			Actions:      "no match",
			LlmResponse:  classified.RawResponse,
		})
	}

	logs = append(logs, db.LogEntry{Level: "INFO", Message: fmt.Sprintf("Processed %q", msg.Subject)})
	if debugLogging {
		logs = append(logs, db.LogEntry{Level: "DEBUG", Message: "LLM response: " + classified.RawResponse})
	}

	if err := store.BatchInsertProcessingResults(ctx, logs, history, account.ID, msg.ID); err != nil {
		slog.Error("db write failed", "err", err)
		// Don't return error — email is processed, don't retry
	}

	if err := store.RecordLlmDebug(ctx, db.AddLlmDebugParams{
		AccountID:    account.ID,
		AccountEmail: account.Email,
		MessageID:    msg.ID,
		Subject:      msg.Subject,
		Sender:       msg.Sender,
		GmailRaw:     gmailRaw,
		LlmRequest:   classified.RequestJSON,
		LlmResponse:  classified.RawResponse,
	}); err != nil {
		slog.Error("llm debug write failed", "err", err)
	}

	return modifies, trashIDs
}

func filterPrompts(prompts []db.Prompt, accountID int64) []db.Prompt {
	var out []db.Prompt
	for _, p := range prompts {
		if p.Active == 0 {
			continue
		}
		if p.AccountID.Valid && p.AccountID.Int64 != accountID {
			continue
		}
		out = append(out, p)
	}
	return out
}

// ProcessConfig holds runtime configuration for the processor.
type ProcessConfig struct {
	LookbackHours  int
	MaxResults     int64
	BodyTruncation int
	DebugLogging   bool
}
