package processor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/sloccy/ollamail-aws/db"
	gmailpkg "github.com/sloccy/ollamail-aws/gmail"
	"github.com/sloccy/ollamail-aws/llm"
)

// ptr returns a pointer to v — for building the *int64/*string nullable fields on
// db.HistoryEntry from a computed value (Go has no address-of operator for a struct
// field expression like p.ID).
func ptr[T any](v T) *T { return &v }

// labelNamePtr returns nil for an empty label (matching the "no label" NULL-sentinel
// semantics of db.HistoryEntry.LabelName), or a pointer to name otherwise.
func labelNamePtr(name string) *string {
	if name == "" {
		return nil
	}
	return &name
}

// ModifyForPrompt returns the Gmail label changes to apply when prompt p matches an
// email, plus whether the message should be trashed (kept separate from mod since
// trashing is a distinct Gmail API call, not a label add/remove). labelID is the
// resolved Gmail label id for p.LabelName (pass "" if unresolved/not needed).
// Shared by the classify path (processEmail) and the recategorize "add" path
// (server.go's handleRecategorize) so the two can't drift.
func ModifyForPrompt(p db.Prompt, labelID string) (mod gmailpkg.Modify, trash bool) {
	if p.LabelName != "" && labelID != "" {
		mod.AddLabels = append(mod.AddLabels, labelID)
	}
	switch {
	case p.ActionSpam != 0:
		mod.AddLabels = append(mod.AddLabels, gmailpkg.LabelSpam)
		mod.RemoveLabels = append(mod.RemoveLabels, gmailpkg.LabelInbox)
	case p.ActionTrash != 0:
		trash = true
	case p.ActionArchive != 0:
		mod.RemoveLabels = append(mod.RemoveLabels, gmailpkg.LabelInbox)
	}
	if p.ActionMarkRead != 0 {
		mod.RemoveLabels = append(mod.RemoveLabels, gmailpkg.LabelUnread)
	}
	return mod, trash
}

// setupAccountContext loads OAuth config, creates a Gmail client, and filters
// prompts for the given account. Shared by ProcessAccount and BackfillLlmDebug.
func setupAccountContext(ctx context.Context, store db.StoreIface, gmailAuth *gmailpkg.Auth, account db.Account, allPrompts []db.Prompt) (*gmailpkg.Client, []db.Prompt, error) {
	oauthCfg, err := gmailAuth.Config()
	if err != nil {
		return nil, nil, fmt.Errorf("load oauth config: %w", err)
	}
	svc, err := gmailpkg.NewService(ctx, account.CredentialsJSON, oauthCfg, func(newCreds string) {
		_ = store.UpdateAccountCredentials(ctx, db.UpdateAccountCredentialsParams{
			CredentialsJSON: newCreds,
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
func ProcessAccount(ctx context.Context, store db.StoreIface, llmClient llm.ClientIface, gmailAuth *gmailpkg.Auth, account db.Account, allPrompts []db.Prompt, cfg ProcessConfig) (*gmailpkg.ServiceWrapper, error) {
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

	return wrapper, processMessageIDs(ctx, store, llmClient, account, svc, prompts, messageIDs, cfg)
}

// ProcessAccountHistory processes only the messages *added to the inbox* since the
// account's stored Gmail history id (WatchHistoryID), returning the new history id to
// persist. This is the push path: by acting on messageAdded events only, it ignores the
// label/read changes the app makes itself — so a push no longer retriggers on our own
// modifications. Falls back to a full lookback scan (returning "") when there is no stored
// history id yet or it has expired; the caller then reseeds from the notification's id.
func ProcessAccountHistory(ctx context.Context, store db.StoreIface, llmClient llm.ClientIface, gmailAuth *gmailpkg.Auth, account db.Account, allPrompts []db.Prompt, cfg ProcessConfig) (string, error) {
	svc, prompts, err := setupAccountContext(ctx, store, gmailAuth, account, allPrompts)
	if err != nil {
		return "", err
	}
	if len(prompts) == 0 {
		return account.WatchHistoryID, nil
	}

	// No baseline yet, or history aged out: full lookback, no reliable new id.
	fullScan := func() (string, error) {
		messageIDs, lerr := gmailpkg.ListRecentMessageIDs(ctx, svc, cfg.LookbackHours, cfg.MaxResults)
		if lerr != nil {
			return "", fmt.Errorf("list messages: %w", lerr)
		}
		return "", processMessageIDs(ctx, store, llmClient, account, svc, prompts, messageIDs, cfg)
	}
	if account.WatchHistoryID == "" {
		return fullScan()
	}

	ids, latest, herr := gmailpkg.ListHistoryAddedMessageIDs(ctx, svc, account.WatchHistoryID)
	if errors.Is(herr, gmailpkg.ErrHistoryTooOld) {
		return fullScan()
	}
	if herr != nil {
		return account.WatchHistoryID, fmt.Errorf("history list: %w", herr)
	}
	if err := processMessageIDs(ctx, store, llmClient, account, svc, prompts, ids, cfg); err != nil {
		return account.WatchHistoryID, err
	}
	return latest, nil
}

// processMessageIDs dedupes the given message IDs against already-processed ones and, for
// any new messages, classifies them and applies label actions. Shared by the lookback scan
// (ProcessAccount) and the push history path (ProcessAccountHistory).
func processMessageIDs(ctx context.Context, store db.StoreIface, llmClient llm.ClientIface, account db.Account, svc *gmailpkg.Client, prompts []db.Prompt, messageIDs []string, cfg ProcessConfig) error {
	// Filter out already-processed
	unprocessed, err := store.FilterUnprocessed(ctx, account.ID, messageIDs)
	if err != nil {
		return fmt.Errorf("filter unprocessed: %w", err)
	}
	if len(unprocessed) == 0 {
		if !cfg.SuppressEmptyLog {
			store.Log("INFO", fmt.Sprintf("[%s] No new emails to process.", account.Email))
		}
		_ = store.UpdateLastScan(ctx, account.ID)
		return nil
	}
	store.Log("INFO", fmt.Sprintf("[%s] Processing %d new email(s) against %d rule(s).", account.Email, len(unprocessed), len(prompts)))

	// Resolve the classify model/tier once for the whole batch instead of per email —
	// they don't change mid-pass, and re-resolving is a DynamoDB GetSetting round trip.
	model, tier := llmClient.ResolveClassifySettings(ctx)

	// Build label cache for all needed labels
	var neededLabels []string
	for _, p := range prompts {
		if p.LabelName != "" {
			neededLabels = append(neededLabels, p.LabelName)
		}
	}
	labelCache, err := gmailpkg.BuildLabelCacheFor(ctx, svc, account.ID, neededLabels)
	if err != nil {
		return fmt.Errorf("build label cache: %w", err)
	}

	// Fetch and classify messages. Fetching is already concurrent (IterMessageDetails);
	// classification is now fanned out too, capped at cfg.ClassifyConcurrency, since flex-tier
	// Bedrock requests can queue for minutes and no longer have to be serialized one-by-one.
	msgCh, errCh := gmailpkg.IterMessageDetails(ctx, svc, unprocessed, cfg.BodyTruncation)

	var allModifies []gmailpkg.Modify
	var trashIDs []string
	var mu sync.Mutex
	var wg sync.WaitGroup

	sem := make(chan struct{}, classifyConcurrency(cfg.ClassifyConcurrency))
	for msg := range msgCh {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			modifies, trash := processEmail(ctx, store, llmClient, account, msg, prompts, labelCache, cfg.DebugLogging, model, tier)

			mu.Lock()
			defer mu.Unlock()
			allModifies = append(allModifies, modifies...)
			trashIDs = append(trashIDs, trash...)
		}()
	}
	wg.Wait()
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

	return nil
}

func processEmail(
	ctx context.Context,
	store db.StoreIface,
	llmClient llm.ClientIface,
	account db.Account,
	msg gmailpkg.Message,
	prompts []db.Prompt,
	labelCache map[string]string,
	debugLogging bool,
	model, tier string,
) (modifies []gmailpkg.Modify, trashIDs []string) {
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

	classified, llmErr := llmClient.ClassifyEmailBatch(ctx, store, email, llmPrompts, model, tier)

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

		mod, trash := ModifyForPrompt(p, labelCache[p.LabelName])
		mod.MessageIDs = []string{msg.ID}

		// Action log text (spam takes priority over trash/archive, matching ModifyForPrompt).
		switch {
		case p.ActionSpam != 0:
			actions = append(actions, "sent to spam")
		case p.ActionTrash != 0:
			actions = append(actions, "trashed")
		case p.ActionArchive != 0:
			actions = append(actions, "archived")
		}
		if trash {
			trashIDs = append(trashIDs, msg.ID)
		}
		if p.ActionMarkRead != 0 {
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
			PromptID:     ptr(p.ID),
			PromptName:   ptr(p.Name),
			LabelName:    labelNamePtr(p.LabelName),
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
		if p.AccountID != nil && *p.AccountID != accountID {
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
	// ClassifyConcurrency caps how many emails are classified against Bedrock in parallel.
	// <= 0 falls back to 1 (fully sequential) — see classifyConcurrency.
	ClassifyConcurrency int
	// SuppressEmptyLog silences the "No new emails to process." log when there's nothing
	// new. Set by the push path, where a no-op run is routine (any inbox touch — read,
	// star, our own label add — wakes the webhook) and would otherwise spam the activity
	// feed; left false for the once-daily scan, where it's a useful "scan ran" signal.
	SuppressEmptyLog bool
}

// classifyConcurrency guards against a zero/negative config value collapsing the semaphore
// buffer to 0, which would deadlock every classify goroutine.
func classifyConcurrency(n int) int {
	if n < 1 {
		return 1
	}
	return n
}
