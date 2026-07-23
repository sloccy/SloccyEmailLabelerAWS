package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sloccy/ollamail-aws/db"
	"github.com/sloccy/ollamail-aws/gmail"
	"github.com/sloccy/ollamail-aws/llm"
	"github.com/sloccy/ollamail-aws/processor"
)

// ============================================================
// Recategorize
// ============================================================

type recategorizeFormData struct {
	HistoryID int64
	MessageID string
	AccountID int64
	Subject   string
	Sender    string
	Prompts   []promptCheckbox
}

type promptCheckbox struct {
	Prompt  db.Prompt
	Checked bool
}

func (s *server) handleRecategorizeForm(w http.ResponseWriter, r *http.Request) {
	id, ok := requireID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	row, err := s.store.GetHistoryRow(ctx, id)
	if err != nil {
		http.Error(w, "history row not found", http.StatusNotFound)
		return
	}

	currentIDs, err := s.store.GetCurrentPromptIDsForMessage(ctx, row.AccountID, row.MessageID)
	if err != nil {
		currentIDs = map[int64]bool{}
	}

	prompts, _ := s.store.ListActivePromptsForAccount(ctx, row.AccountID)

	checkboxes := make([]promptCheckbox, len(prompts))
	for i, p := range prompts {
		checkboxes[i] = promptCheckbox{Prompt: p, Checked: currentIDs[p.ID]}
	}

	data := recategorizeFormData{
		HistoryID: id,
		MessageID: row.MessageID,
		AccountID: row.AccountID,
		Subject:   row.Subject,
		Sender:    row.Sender,
		Prompts:   checkboxes,
	}
	s.fragmentResponse(w, "recategorize_form.html", data, "")
}

func (s *server) handleRecategorize(w http.ResponseWriter, r *http.Request) {
	id, ok := requireID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	row, err := s.store.GetHistoryRow(ctx, id)
	if err != nil {
		http.Error(w, "history row not found", http.StatusNotFound)
		return
	}

	// Build set of newly-requested prompt IDs
	requested := make(map[int64]bool)
	for _, v := range r.Form["prompt_ids"] {
		pid, err := strconv.ParseInt(v, 10, 64)
		if err == nil {
			requested[pid] = true
		}
	}

	// Build set of prompts to improve
	improveSet := make(map[int64]bool)
	for _, v := range r.Form["improve_prompt_ids"] {
		pid, err := strconv.ParseInt(v, 10, 64)
		if err == nil {
			improveSet[pid] = true
		}
	}

	note := r.FormValue("note")

	// Current applied prompts
	currentIDs, _ := s.store.GetCurrentPromptIDsForMessage(ctx, row.AccountID, row.MessageID)

	// Load all prompts for this account (to get labels + actions)
	allPrompts, _ := s.store.ListActivePromptsForAccount(ctx, row.AccountID)
	promptByID := make(map[int64]db.Prompt, len(allPrompts))
	for _, p := range allPrompts {
		promptByID[p.ID] = p
	}

	// Compute diffs
	var addedIDs, removedIDs []int64
	for pid := range requested {
		if !currentIDs[pid] {
			addedIDs = append(addedIDs, pid)
		}
	}
	for pid := range currentIDs {
		if !requested[pid] {
			removedIDs = append(removedIDs, pid)
		}
	}

	// Build new current set for storage
	newCurrentIDs := map[int64]bool{}
	for pid := range currentIDs {
		newCurrentIDs[pid] = true
	}
	for _, pid := range addedIDs {
		newCurrentIDs[pid] = true
	}
	for _, pid := range removedIDs {
		delete(newCurrentIDs, pid)
	}

	// Apply Gmail changes if we have an account
	account, gmailErr := s.store.GetAccount(ctx, row.AccountID)
	var svc *gmail.Client
	if gmailErr == nil && row.AccountID != 0 {
		svc, _ = s.gmailServiceFor(ctx, account)
	}

	s.applyRecategorizeToGmail(ctx, svc, row, promptByID, addedIDs, removedIDs)

	// Rewrite history so it mirrors the post-correction labeling state
	var keptIDs []int64
	for pid := range newCurrentIDs {
		if !slices.Contains(addedIDs, pid) {
			keptIDs = append(keptIDs, pid)
		}
	}
	var addedPrompts []db.Prompt
	for _, pid := range addedIDs {
		if p, ok := promptByID[pid]; ok {
			addedPrompts = append(addedPrompts, p)
		}
	}
	_ = s.store.RewriteHistoryForMessage(ctx, row.MessageID, keptIDs, addedPrompts, db.CategorizationHistory{
		AccountID:    row.AccountID,
		AccountEmail: row.AccountEmail,
		MessageID:    row.MessageID,
		Subject:      row.Subject,
		Sender:       row.Sender,
	})

	// Build CSV of new current prompt IDs
	var newCurrentSlice []string
	for pid := range newCurrentIDs {
		newCurrentSlice = append(newCurrentSlice, strconv.FormatInt(pid, 10))
	}

	var addedCSV, removedCSV []string
	for _, pid := range addedIDs {
		addedCSV = append(addedCSV, strconv.FormatInt(pid, 10))
	}
	for _, pid := range removedIDs {
		removedCSV = append(removedCSV, strconv.FormatInt(pid, 10))
	}

	correctionID, corrErr := s.store.InsertEmailCorrection(ctx, db.InsertEmailCorrectionParams{
		AccountID:        row.AccountID,
		MessageID:        row.MessageID,
		AddedPrompts:     strings.Join(addedCSV, ","),
		RemovedPrompts:   strings.Join(removedCSV, ","),
		CurrentPromptIds: strings.Join(newCurrentSlice, ","),
		Note:             note,
	})

	// Insert placeholder suggestion rows immediately, then kick off the LLM in the background.
	s.seedImproveSuggestions(ctx, svc, row, promptByID, addedIDs, improveSet, correctionID, corrErr)

	setHxTrigger(w, map[string]any{
		triggerShowToast:              map[string]any{toastKeyMessage: "Recategorization applied", jsonKeyType: "success"},
		"closeModal":                  "recategorize-modal",
		triggerRefreshSuggestionBadge: "1",
		"refreshHistory":              "1",
		"refreshSuggestions":          "1",
	})
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
}

// applyRecategorizeToGmail pushes an add/remove prompt diff to Gmail: it builds a label
// cache for any newly-needed labels, applies the added prompts' label/action changes (via
// processor.ModifyForPrompt), and reverses the removed prompts' changes (via
// processor.ReverseModifyForPrompt, including restoring INBOX for any message untrashed by a
// removed prompt). No-op if svc is nil or there's nothing to apply.
func (s *server) applyRecategorizeToGmail(ctx context.Context, svc *gmail.Client, row db.CategorizationHistory, promptByID map[int64]db.Prompt, addedIDs, removedIDs []int64) {
	if svc == nil || (len(addedIDs) == 0 && len(removedIDs) == 0) {
		return
	}

	var neededLabels []string
	for _, pid := range addedIDs {
		if p, ok := promptByID[pid]; ok && p.LabelName != "" {
			neededLabels = append(neededLabels, p.LabelName)
		}
	}
	labelCache, _ := gmail.BuildLabelCache(ctx, svc, neededLabels)

	// Apply added prompts. Shares its label/action mapping with the classify path
	// (processor.ModifyForPrompt) — note it doesn't trash on add, same as before;
	// that gap is preserved as-is, not fixed here.
	var addModifies []gmail.Modify
	for _, pid := range addedIDs {
		p, ok := promptByID[pid]
		if !ok {
			continue
		}
		mod, _ := processor.ModifyForPrompt(p, labelCache[p.LabelName])
		mod.MessageIDs = []string{row.MessageID}
		if len(mod.AddLabels) > 0 || len(mod.RemoveLabels) > 0 {
			addModifies = append(addModifies, mod)
		}
	}
	if len(addModifies) > 0 {
		_ = gmail.BatchModifyEmails(ctx, svc, addModifies)
	}

	// Reverse removed prompts. Derives from the same action→label mapping
	// ModifyForPrompt uses (see processor.ReverseModifyForPrompt) so the add and remove
	// paths can't drift apart.
	var removeModifies []gmail.Modify
	var trashReverseIDs []string
	for _, pid := range removedIDs {
		p, ok := promptByID[pid]
		if !ok {
			continue
		}
		mod, untrash := processor.ReverseModifyForPrompt(p, labelCache[p.LabelName])
		mod.MessageIDs = []string{row.MessageID}
		if untrash {
			// Untrash: remove TRASH, add INBOX
			trashReverseIDs = append(trashReverseIDs, row.MessageID)
		}
		if len(mod.AddLabels) > 0 || len(mod.RemoveLabels) > 0 {
			removeModifies = append(removeModifies, mod)
		}
	}
	if len(removeModifies) > 0 {
		_ = gmail.BatchModifyEmails(ctx, svc, removeModifies)
	}
	if len(trashReverseIDs) > 0 {
		_ = gmail.BatchModifyEmails(ctx, svc, []gmail.Modify{{
			MessageIDs:   trashReverseIDs,
			AddLabels:    []string{gmail.LabelInbox},
			RemoveLabels: []string{gmail.LabelTrash},
		}})
	}
}

// seedImproveSuggestions inserts a "generating" placeholder suggestion row for each prompt
// flagged for improvement, then kicks off the LLM call in the background via
// runImproveSuggestions so handleRecategorize can return without waiting on Bedrock. No-op if
// improveSet is empty or svc is nil (fetching the message body needs a live Gmail client).
func (s *server) seedImproveSuggestions(ctx context.Context, svc *gmail.Client, row db.CategorizationHistory, promptByID map[int64]db.Prompt, addedIDs []int64, improveSet map[int64]bool, correctionID int64, corrErr error) {
	if len(improveSet) == 0 || svc == nil {
		return
	}
	msg, fetchErr := gmail.FetchMessage(ctx, svc, row.MessageID, 0)
	if fetchErr != nil {
		slog.Error("recategorize: fetch message for suggestions", "err", fetchErr)
		return
	}

	var corrID sql.NullInt64
	if corrErr == nil {
		corrID = sql.NullInt64{Int64: correctionID, Valid: true}
	}

	triggerKinds := make(map[int64]string, len(improveSet))
	for pid := range improveSet {
		if slices.Contains(addedIDs, pid) {
			triggerKinds[pid] = db.TriggerKindFalseNegative
		} else {
			triggerKinds[pid] = db.TriggerKindFalsePositive
		}
	}

	suggestionIDs := make(map[int64]int64, len(improveSet))
	for pid := range improveSet {
		p, ok := promptByID[pid]
		if !ok {
			continue
		}
		sid, insertErr := s.store.InsertPromptSuggestion(ctx, db.InsertPromptSuggestionParams{
			PromptID:              p.ID,
			CorrectionID:          corrID,
			TriggerKind:           triggerKinds[pid],
			MessageID:             row.MessageID,
			EmailSubject:          row.Subject,
			EmailSender:           row.Sender,
			EmailBodySnapshot:     msg.Body,
			OriginalInstructions:  p.Instructions,
			SuggestedInstructions: "",
			ConversationJSON:      "[]",
			Status:                db.SuggestionStatusGenerating,
		})
		if insertErr != nil {
			slog.Error("recategorize: insert generating suggestion", "prompt_id", pid, "err", insertErr)
			continue
		}
		suggestionIDs[pid] = sid
	}
	if len(suggestionIDs) > 0 {
		go s.runImproveSuggestions(s.ctx, msg, suggestionIDs, promptByID, triggerKinds)
	}
}

// runImproveSuggestions calls the LLM to generate prompt improvement suggestions
// for each flagged prompt. The suggestion rows (status='generating') must already
// exist in the DB before this is called. Runs in a goroutine so the recategorize
// handler can return without waiting for Bedrock.
func (s *server) runImproveSuggestions(
	baseCtx context.Context,
	msg gmail.Message,
	suggestionIDs map[int64]int64,
	promptByID map[int64]db.Prompt,
	triggerKinds map[int64]string,
) {
	ctx, cancel := context.WithTimeout(baseCtx, 20*time.Minute)
	defer cancel()

	slog.Info("improve suggestions start", "count", len(suggestionIDs))

	for pid, sid := range suggestionIDs {
		p, ok := promptByID[pid]
		if !ok {
			continue
		}

		suggested, conv, llmErr := s.llm.ImprovePromptInstructions(ctx, llm.ImproveRequest{
			PromptName:           p.Name,
			LabelName:            p.LabelName,
			OriginalInstructions: p.Instructions,
			TriggerKind:          triggerKinds[pid],
			EmailSubject:         msg.Subject,
			EmailSender:          msg.Sender,
			EmailBody:            msg.Body,
		})
		if llmErr != nil {
			slog.Error("improve prompt", "prompt_id", pid, "err", llmErr)
			if err := s.store.FinalizePromptSuggestion(ctx, db.FinalizePromptSuggestionParams{
				ID:                    sid,
				SuggestedInstructions: "",
				ConversationJSON:      "[]",
				Status:                db.SuggestionStatusFailed,
				UserComment:           llmErr.Error(),
			}); err != nil {
				slog.Error("finalize suggestion failed", "prompt_id", pid, "err", err)
			}
			continue
		}

		convJSON, _ := json.Marshal(conv) //nolint:errchkjson // []ChatMessage cannot fail

		if err := s.store.FinalizePromptSuggestion(ctx, db.FinalizePromptSuggestionParams{
			ID:                    sid,
			SuggestedInstructions: suggested,
			ConversationJSON:      string(convJSON),
			Status:                db.SuggestionStatusPending,
			UserComment:           "",
		}); err != nil {
			slog.Error("finalize suggestion failed", "prompt_id", pid, "err", err)
		}
		slog.Info("improve suggestions: suggestion ready", "prompt_id", pid)
	}

	slog.Debug("improve suggestions done", "count", len(suggestionIDs))
}
