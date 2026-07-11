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

// Log levels used for db.LogEntry / store.Log throughout this file.
const (
	logInfo    = "INFO"
	logWarning = "WARNING"
	logDebug   = "DEBUG"
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

// ReverseModifyForPrompt returns the Gmail label changes that undo what ModifyForPrompt
// would apply for prompt p — used by the recategorize "remove" path when a previously
// matched prompt is unmarked. Derived by swapping ModifyForPrompt's add/remove labels
// (untrash mirrors trash: both mean "handle via the separate batch trash/untrash call, not
// a label op") instead of re-deriving the action→label mapping a second time, so the two
// can't drift apart.
func ReverseModifyForPrompt(p db.Prompt, labelID string) (mod gmailpkg.Modify, untrash bool) {
	fwd, trash := ModifyForPrompt(p, labelID)
	mod.AddLabels = fwd.RemoveLabels
	mod.RemoveLabels = fwd.AddLabels
	return mod, trash
}

// NewAccountGmailService builds an authenticated Gmail client for account, wiring the
// OAuth token-refresh callback to persist any rotated credentials back to store. Shared
// by setupAccountContext (scan/push) and the web server's ad-hoc Gmail lookups
// (retention labels, recategorize) so both paths build the client identically.
func NewAccountGmailService(ctx context.Context, store db.StoreIface, gmailAuth *gmailpkg.Auth, account db.Account) (*gmailpkg.Client, error) {
	oauthCfg, err := gmailAuth.Config()
	if err != nil {
		return nil, fmt.Errorf("load oauth config: %w", err)
	}
	svc, err := gmailpkg.NewService(ctx, account.CredentialsJSON, oauthCfg, func(newCreds string) {
		_ = store.UpdateAccountCredentials(ctx, db.UpdateAccountCredentialsParams{
			CredentialsJSON: newCreds,
			ID:              account.ID,
		})
	})
	if err != nil {
		return nil, fmt.Errorf("create gmail service: %w", err)
	}
	return svc, nil
}

// setupAccountContext creates a Gmail client and filters prompts for the given account.
// Shared by ProcessAccount and the account-history backfill/reprocessing paths.
func setupAccountContext(ctx context.Context, store db.StoreIface, gmailAuth *gmailpkg.Auth, account db.Account, allPrompts []db.Prompt) (*gmailpkg.Client, []db.Prompt, error) {
	svc, err := NewAccountGmailService(ctx, store, gmailAuth, account)
	if err != nil {
		return nil, nil, err
	}
	return svc, filterPrompts(allPrompts, account.ID), nil
}

// bufferedLogger collects log entries in memory instead of writing each one straight to
// DynamoDB. It satisfies llm.StoreLogger, so it's a drop-in stand-in for the live store on
// the ClassifyEmailBatch call in processEmail: the buffered entries are flushed together
// with the email's history through BatchInsertProcessingResults instead of costing a
// separate counter-update-plus-PutItem per log line.
type bufferedLogger struct {
	entries []db.LogEntry
}

func (b *bufferedLogger) Log(level, message string) {
	b.entries = append(b.entries, db.LogEntry{Level: level, Message: message})
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
			store.Log(logInfo, fmt.Sprintf("[%s] No new emails to process.", account.Email))
		}
		_ = store.UpdateLastScan(ctx, account.ID)
		return nil
	}
	store.Log(logInfo, fmt.Sprintf("[%s] Processing %d new email(s) against %d rule(s).", account.Email, len(unprocessed), len(prompts)))

	// Resolve the classify model/tier/reasoning-override once for the whole batch
	// instead of per email — they don't change mid-pass, and re-resolving is a
	// DynamoDB GetSetting round trip.
	model, tier, reasoningOverride := llmClient.ResolveClassifySettings(ctx)

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

	// Same for every email in this batch, so it's built once here rather than per email
	// inside the classify fan-out below.
	llmPrompts := make([]llm.Prompt, len(prompts))
	for i, p := range prompts {
		llmPrompts[i] = llm.Prompt{ID: p.ID, Name: p.Name, Instructions: p.Instructions}
	}

	// Fetch and classify messages. Fetching is already concurrent (IterMessageDetails);
	// classification is now fanned out too, capped at cfg.ClassifyConcurrency, since flex-tier
	// Bedrock requests can queue for minutes and no longer have to be serialized one-by-one.
	msgCh, errCh := gmailpkg.IterMessageDetails(ctx, svc, unprocessed, cfg.BodyTruncation)

	var allModifies []gmailpkg.Modify
	var trashIDs []string
	var mu sync.Mutex
	var wg sync.WaitGroup

	// DynamoDB writes are deliberately kept off the classify fan-out: a single writer
	// goroutine drains jobCh sequentially, so the table only ever sees one write in
	// flight at a time regardless of ClassifyConcurrency. That's what lets the table run
	// on a low provisioned capacity instead of on-demand. The channel is buffered to
	// ClassifyConcurrency so a burst of simultaneous classify completions doesn't stall
	// waiting on the writer. Streaming (rather than collecting all jobs and writing after
	// wg.Wait) matters on a large backfill: a 900s Lambda timeout still leaves every
	// already-classified email persisted and marked processed, so the next scan resumes
	// instead of redoing work.
	jobCh := make(chan writeJob, classifyConcurrency(cfg.ClassifyConcurrency))
	var writerWG sync.WaitGroup
	writerWG.Go(func() {
		for job := range jobCh {
			applyWriteJob(ctx, store, job)
		}
	})

	sem := make(chan struct{}, classifyConcurrency(cfg.ClassifyConcurrency))
	for msg := range msgCh {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			modifies, trash, job := processEmail(ctx, llmClient, account, msg, prompts, llmPrompts, labelCache, cfg.DebugLogging, model, tier, reasoningOverride)

			mu.Lock()
			allModifies = append(allModifies, modifies...)
			trashIDs = append(trashIDs, trash...)
			mu.Unlock()

			jobCh <- job
		}()
	}
	wg.Wait()
	close(jobCh)
	writerWG.Wait()
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

// writeJob carries one email's DynamoDB writes so they can be applied by the single
// serial writer in processMessageIDs instead of inline inside the classify goroutine.
// messageID == "" means "don't mark processed" (the LLM-error retry case); llmDebug == nil
// means skip the (comparatively large) LLM-debug write entirely.
type writeJob struct {
	accountID int64
	logs      []db.LogEntry
	history   []db.HistoryEntry
	messageID string
	llmDebug  *db.AddLlmDebugParams
}

// applyWriteJob persists one writeJob. Errors are logged, not propagated — matching the
// prior inline behavior where a DB write failure doesn't block or retry email processing.
func applyWriteJob(ctx context.Context, store db.StoreIface, job writeJob) {
	if err := store.BatchInsertProcessingResults(ctx, job.logs, job.history, job.accountID, job.messageID); err != nil {
		slog.Error("db write failed", "err", err)
	}
	if job.llmDebug != nil {
		if err := store.RecordLlmDebug(ctx, *job.llmDebug); err != nil {
			slog.Error("llm debug write failed", "err", err)
		}
	}
}

func processEmail(
	ctx context.Context,
	llmClient llm.ClientIface,
	account db.Account,
	msg gmailpkg.Message,
	prompts []db.Prompt,
	llmPrompts []llm.Prompt,
	labelCache map[string]string,
	debugLogging bool,
	model, tier, reasoningOverride string,
) (modifies []gmailpkg.Modify, trashIDs []string, job writeJob) {
	email := llm.Email{
		Sender:  msg.Sender,
		Subject: msg.Subject,
		Body:    msg.Body,
		Snippet: msg.Snippet,
	}

	logs := []db.LogEntry{{
		Level: logInfo,
		Message: fmt.Sprintf("[%s] Classifying: '%s' from %s",
			account.Email, gmailpkg.Truncate(msg.Subject, 60), gmailpkg.Truncate(msg.Sender, 60)),
	}}

	// Buffer the classify call's own log lines instead of writing each one straight to
	// DynamoDB (a counter update + a PutItem per line): ClassifyEmailBatch logs several
	// times per call, and buffering lets all of it flush through the single
	// BatchInsertProcessingResults call below, alongside history, instead.
	logger := &bufferedLogger{}
	classified, llmErr := llmClient.ClassifyEmailBatch(ctx, logger, email, llmPrompts, model, tier, reasoningOverride, debugLogging)
	logs = append(logs, logger.entries...)

	var history []db.HistoryEntry

	if llmErr != nil {
		logs = append(logs, db.LogEntry{Level: logWarning, Message: fmt.Sprintf("LLM error for %q: %v — will retry", msg.Subject, llmErr)})
		// Don't mark processed (messageID left ""); will retry.
		return nil, nil, writeJob{accountID: account.ID, logs: logs}
	}

	var matched []string
	for _, p := range prompts {
		if classified.Results[p.ID] {
			matched = append(matched, p.Name)
		}
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
			Level:   logInfo,
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
			DurationMs:   classified.LatencyMs,
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
			DurationMs:   classified.LatencyMs,
		})
	}

	if len(matched) > 0 {
		logs = append(logs, db.LogEntry{Level: logInfo, Message: fmt.Sprintf("[%s] Processed %q: %d match(es): %v", account.Email, msg.Subject, len(matched), matched)})
	} else {
		logs = append(logs, db.LogEntry{Level: logInfo, Message: fmt.Sprintf("[%s] Processed %q: 0 match(es): none", account.Email, msg.Subject)})
	}
	if debugLogging {
		logs = append(logs, db.LogEntry{Level: logDebug, Message: "LLM response: " + classified.RawResponse})
	}

	// LLM debug (raw Gmail message + full LLM request/response) is the fattest per-email
	// write, so it's only built and written when DebugLogging is on — keeping normal
	// operation to just the batched logs+history write plus the processed marker.
	var llmDebug *db.AddLlmDebugParams
	if debugLogging {
		llmDebug = &db.AddLlmDebugParams{
			AccountID:    account.ID,
			AccountEmail: account.Email,
			MessageID:    msg.ID,
			Subject:      msg.Subject,
			Sender:       msg.Sender,
			GmailRaw:     marshalGmailDebug(msg),
			LlmRequest:   classified.RequestJSON,
			LlmResponse:  classified.RawResponse,
		}
	}

	job = writeJob{
		accountID: account.ID,
		logs:      logs,
		history:   history,
		messageID: msg.ID,
		llmDebug:  llmDebug,
	}

	return modifies, trashIDs, job
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
