package main

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/sloccy/ollamail-aws/db"
	"github.com/sloccy/ollamail-aws/gmail"
	"github.com/sloccy/ollamail-aws/processor"
)

// ============================================================
// Recategorize
// ============================================================

// parseIDList parses a repeated form field of decimal int64 ids, skipping any value that
// doesn't parse rather than failing the whole request over one bad value. Shared by the
// single-email and bulk recategorize handlers.
func parseIDList(values []string) []int64 {
	var out []int64
	for _, v := range values {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			out = append(out, id)
		}
	}
	return out
}

func idSet(ids []int64) map[int64]bool {
	set := make(map[int64]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	return set
}

// joinIDs renders ids as a comma-separated decimal string, for the CSV-encoded prompt-id
// columns (AddedPrompts/RemovedPrompts/CurrentPromptIds) db.InsertEmailCorrectionParams
// expects. Returns "" for an empty/nil slice.
func joinIDs(ids []int64) string {
	if len(ids) == 0 {
		return ""
	}
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(parts, ",")
}

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

// exampleVerdict is one rule's outcome for one reviewed email: which of the two buckets it
// belongs in, and whether the user actually changed this rule's checkbox (a real correction)
// versus left it as it was (a plain confirmation). See db.PromptExample.Missed.
type exampleVerdict struct {
	Verdict string
	Missed  bool
}

// singleRecategorizeVerdicts computes, for a single-email recategorization, an
// exampleVerdict for *every* active prompt on the account — not just the ones the user
// touched. This mirrors the full checkbox state the user actually saw in
// recategorize_form.html: every rule on that form got an explicit checked/unchecked
// decision, so every rule's post-correction state is real signal, whether or not it
// changed. Compare bulkVerdictsAndPlan, whose table only covers rules the user explicitly
// pointed at, because a bulk "apply to all" / "remove from all" action isn't a per-email
// review and doesn't put every rule in front of the user the way this form does.
func singleRecategorizeVerdicts(allPromptIDs []int64, currentIDs, requestedIDs map[int64]bool) map[int64]exampleVerdict {
	verdicts := make(map[int64]exampleVerdict, len(allPromptIDs))
	for _, pid := range allPromptIDs {
		was, is := currentIDs[pid], requestedIDs[pid]
		v := exampleVerdict{Missed: was != is}
		if is {
			v.Verdict = db.VerdictConfirmedPositive
		} else {
			v.Verdict = db.VerdictConfirmedNegative
		}
		verdicts[pid] = v
	}
	return verdicts
}

// exampleTriggerKind maps one example's (Verdict, Missed) pair to the TriggerKind* value
// IncrementVersionObservedBy expects, or "" for a plain affirmation (Missed == false) —
// there's nothing to attribute to the version ledger when the rule already had it right.
// A rule that should have matched and didn't (confirmed_positive, missed) is a false
// negative; a rule that shouldn't have matched but did (confirmed_negative, missed) is a
// false positive.
func exampleTriggerKind(ex db.PromptExample) string {
	if !ex.Missed {
		return ""
	}
	if ex.Verdict == db.VerdictConfirmedPositive {
		return db.TriggerKindFalseNegative
	}
	return db.TriggerKindFalsePositive
}

// incrementVersionObservedFor updates each Missed example's PromptVersion with what it
// actually turned out to be — one more piece of production evidence against whatever rule
// text was live when the mismatched email came in (see db.PromptVersion.ObservedFP/
// ObservedFN). Plain affirmations (Missed == false) are skipped via exampleTriggerKind
// returning "", not filtered here, so this can just iterate every example unconditionally.
//
// Examples sharing the same (promptID, versionID, kind) — routine from the bulk
// recategorize path, where the same rule version is touched by many messages in one action
// — are aggregated into a single ADD update for their combined count, rather than one
// UpdateItem per example; a 50-email bulk action touching a handful of rules costs a
// handful of writes here instead of up to 50. Best-effort and fire-and-forget by design
// (matches IncrementVersionObservedBy's own doc comment) — bookkeeping for a future improve
// round must never be able to slow down or fail a correction the user is actively waiting on.
func incrementVersionObservedFor(ctx context.Context, store versionObserver, examples []db.PromptExample) {
	type versionKind struct {
		promptID, versionID int64
		kind                string
	}
	counts := make(map[versionKind]int64, len(examples))
	for _, ex := range examples {
		kind := exampleTriggerKind(ex)
		if kind == "" {
			continue
		}
		counts[versionKind{ex.PromptID, ex.PromptVersionID, kind}]++
	}
	for k, n := range counts {
		store.IncrementVersionObservedBy(ctx, k.promptID, k.versionID, k.kind, n)
	}
}

// versionObserver is the one method incrementVersionObservedFor needs — declared locally
// so a test can supply a trivial fake without pulling in the rest of db.Store's surface.
type versionObserver interface {
	IncrementVersionObservedBy(ctx context.Context, promptID, versionID int64, kind string, n int64)
}

// buildPromptExamples turns a promptID->exampleVerdict map into example rows ready for
// InsertPromptExamples. Every row from one recategorization shares the same email metadata
// (account/message/sender/subject/excerpt/note) — they all describe the same underlying
// review, just from a different rule's point of view. promptByID stamps each example
// with the rule's CurrentVersionID — which text actually produced this verdict — so a
// later improve round can attribute a recurring problem to the version that caused it (see
// db.PromptExample.PromptVersionID/Recurred). A promptID missing from promptByID (shouldn't
// happen — the caller builds it from the same prompts the verdict map was computed against)
// just stamps 0, same as a pre-ledger example.
func buildPromptExamples(accountID int64, messageID, sender, subject, excerpt, note string, verdicts map[int64]exampleVerdict, promptByID map[int64]db.Prompt) []db.PromptExample {
	if len(verdicts) == 0 {
		return nil
	}
	examples := make([]db.PromptExample, 0, len(verdicts))
	for promptID, v := range verdicts {
		examples = append(examples, db.PromptExample{
			PromptID:        promptID,
			AccountID:       accountID,
			MessageID:       messageID,
			Verdict:         v.Verdict,
			Missed:          v.Missed,
			Sender:          sender,
			Subject:         subject,
			BodyExcerpt:     excerpt,
			Note:            note,
			PromptVersionID: promptByID[promptID].CurrentVersionID,
		})
	}
	return examples
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

	requestedList := parseIDList(r.Form["prompt_ids"])
	requested := idSet(requestedList)
	improveSet := idSet(parseIDList(r.Form["improve_prompt_ids"]))

	note := r.FormValue("note")

	// Current applied prompts
	currentIDs, _ := s.store.GetCurrentPromptIDsForMessage(ctx, row.AccountID, row.MessageID)

	// Load all prompts for this account (to get labels + actions)
	allPrompts, _ := s.store.ListActivePromptsForAccount(ctx, row.AccountID)
	promptByID := make(map[int64]db.Prompt, len(allPrompts))
	allPromptIDs := make([]int64, len(allPrompts))
	for i, p := range allPrompts {
		promptByID[p.ID] = p
		allPromptIDs[i] = p.ID
	}

	// Compute diffs. keptIDs (prompts requested and already current) is derived here
	// alongside removedIDs rather than in a second pass over newCurrentIDs below: since
	// requested is exactly the post-correction current set (see requestedList's use as
	// CurrentPromptIds further down), keptIDs = currentIDs ∩ requested and removedIDs =
	// currentIDs \ requested fall out of one loop.
	var addedIDs, removedIDs, keptIDs []int64
	for pid := range requested {
		if !currentIDs[pid] {
			addedIDs = append(addedIDs, pid)
		}
	}
	for pid := range currentIDs {
		if requested[pid] {
			keptIDs = append(keptIDs, pid)
		} else {
			removedIDs = append(removedIDs, pid)
		}
	}

	// Apply Gmail changes if we have an account
	account, gmailErr := s.store.GetAccount(ctx, row.AccountID)
	var svc *gmail.Client
	if gmailErr == nil && row.AccountID != 0 {
		svc, _ = s.gmailServiceFor(ctx, account)
	}

	s.applyRecategorizeToGmail(ctx, svc, []string{row.MessageID}, promptByID, addedIDs, removedIDs)

	// Record a permanent example for every active rule on the account, not just the ones
	// this correction touched — see singleRecategorizeVerdicts for the exact table. This is
	// the corpus AI prompt improvement now draws from (selectExamplesForImprove,
	// improve.go), instead of seeing only whatever single email triggered the current round.
	verdicts := singleRecategorizeVerdicts(allPromptIDs, currentIDs, requested)
	var msg gmail.Message
	haveMsg := false
	if svc != nil && (len(verdicts) > 0 || len(improveSet) > 0) {
		// Fetched once at the larger 4000-char size (matching the existing suggestion
		// snapshot) and reused for both the example excerpt (further truncated below) and
		// seedImproveSuggestions' EmailBodySnapshot — a second FetchMessage call here would
		// be a second Gmail API round trip for content we already have.
		if m, err := gmail.FetchMessage(ctx, svc, row.MessageID, 0); err == nil {
			msg = m
			haveMsg = true
		} else {
			slog.Error("recategorize: fetch message for examples", "err", err)
		}
	}
	if len(verdicts) > 0 && haveMsg {
		sender, subject := msg.Sender, msg.Subject
		if sender == "" {
			sender = row.Sender
		}
		if subject == "" {
			subject = row.Subject
		}
		examples := buildPromptExamples(row.AccountID, row.MessageID, sender, subject,
			gmail.CollapseExcerpt(msg.Body, db.ExampleExcerptRunes), note, verdicts, promptByID)
		if err := s.store.InsertPromptExamples(ctx, examples); err != nil {
			slog.Error("recategorize: insert prompt examples", "err", err)
		}
		incrementVersionObservedFor(ctx, s.store, examples)
	}

	// Rewrite history so it mirrors the post-correction labeling state
	var addedPrompts []db.Prompt
	for _, pid := range addedIDs {
		if p, ok := promptByID[pid]; ok {
			addedPrompts = append(addedPrompts, p)
		}
	}
	// RewriteHistoryForMessages, not a dedicated single-message method — one-element plan
	// slice, same batched delete+put path the bulk recategorize handler uses.
	_ = s.store.RewriteHistoryForMessages(ctx, row.AccountID, row.AccountEmail, []db.RewriteMessagePlan{{
		MessageID: row.MessageID, Subject: row.Subject, Sender: row.Sender,
		KeptIDs: keptIDs, AddedPrompts: addedPrompts,
	}})

	correctionID, corrErr := s.store.InsertEmailCorrection(ctx, db.InsertEmailCorrectionParams{
		AccountID:        row.AccountID,
		MessageID:        row.MessageID,
		AddedPrompts:     joinIDs(addedIDs),
		RemovedPrompts:   joinIDs(removedIDs),
		CurrentPromptIds: joinIDs(requestedList),
		Note:             note,
	})

	// Insert placeholder suggestion rows immediately, then kick off the LLM in the background.
	s.seedImproveSuggestions(ctx, row, promptByID, addedIDs, improveSet, correctionID, corrErr, note, msg, haveMsg)

	setHxTrigger(w, map[string]any{
		triggerShowToast:              map[string]any{toastKeyMessage: "Recategorization applied", jsonKeyType: toastTypeSuccess},
		"closeModal":                  "recategorize-modal",
		triggerRefreshSuggestionBadge: "1",
		"refreshHistory":              "1",
		"refreshSuggestions":          "1",
	})
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
}

// handleConfirmCategorization is the "the labeling is already right" counterpart to
// handleRecategorize: it records the message's current prompt-match state as a confirmed
// example for every active rule on the account, without touching Gmail, history, or
// suggestions. This is how a user builds up the improve corpus for emails that were
// already labeled correctly — recategorizing them would be a no-op diff that (before this
// handler existed) recorded nothing at all.
func (s *server) handleConfirmCategorization(w http.ResponseWriter, r *http.Request) {
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
	s.confirmCategorization(ctx, row.AccountID, row.MessageID, row.Sender, row.Subject)
	setHxTrigger(w, map[string]any{
		triggerShowToast: map[string]any{toastKeyMessage: "Categorization confirmed", jsonKeyType: toastTypeSuccess},
	})
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
}

// confirmCategorization records accountID/messageID's current prompt-match state as a
// confirmed example for every active rule on the account — every currently-applied prompt
// gets confirmed_positive, every other active prompt gets confirmed_negative, all with
// Missed: false, since nothing here was actually corrected. Shared by the single (see
// handleConfirmCategorization above) and bulk (recategorize_bulk.go) confirm handlers.
// Unlike handleRecategorize, a failed message fetch doesn't cancel the write — sender/
// subject from the history row are still useful signal even with an empty body excerpt,
// matching the bulk recategorize path's own tolerance for a missing excerpt (see
// fetchExcerptsBounded, recategorize_bulk.go).
func (s *server) confirmCategorization(ctx context.Context, accountID int64, messageID, fallbackSender, fallbackSubject string) {
	currentIDs, err := s.store.GetCurrentPromptIDsForMessage(ctx, accountID, messageID)
	if err != nil {
		currentIDs = map[int64]bool{}
	}
	allPrompts, _ := s.store.ListActivePromptsForAccount(ctx, accountID)
	if len(allPrompts) == 0 {
		return
	}
	promptByID := make(map[int64]db.Prompt, len(allPrompts))
	allPromptIDs := make([]int64, len(allPrompts))
	for i, p := range allPrompts {
		promptByID[p.ID] = p
		allPromptIDs[i] = p.ID
	}
	verdicts := singleRecategorizeVerdicts(allPromptIDs, currentIDs, currentIDs)

	sender, subject, excerpt := fallbackSender, fallbackSubject, ""
	account, gmailErr := s.store.GetAccount(ctx, accountID)
	if gmailErr == nil && accountID != 0 {
		if svc, err := s.gmailServiceFor(ctx, account); err == nil {
			if m, err := gmail.FetchMessage(ctx, svc, messageID, 0); err == nil {
				if m.Sender != "" {
					sender = m.Sender
				}
				if m.Subject != "" {
					subject = m.Subject
				}
				excerpt = gmail.CollapseExcerpt(m.Body, db.ExampleExcerptRunes)
			} else {
				slog.Error("confirm categorization: fetch message for examples", "err", err)
			}
		}
	}

	examples := buildPromptExamples(accountID, messageID, sender, subject, excerpt, "", verdicts, promptByID)
	if err := s.store.InsertPromptExamples(ctx, examples); err != nil {
		slog.Error("confirm categorization: insert prompt examples", "err", err)
	}
	incrementVersionObservedFor(ctx, s.store, examples)
}

// applyRecategorizeToGmail pushes an add/remove prompt diff to Gmail for messageIDs: it
// builds a label cache for any newly-needed labels, applies the added prompts' label/action
// changes (via processor.ModifyForPrompt), and reverses the removed prompts' changes (via
// processor.ReverseModifyForPrompt, including restoring INBOX for any message untrashed by a
// removed prompt). No-op if svc is nil or there's nothing to apply. Shared by the
// single-email path (recategorize.go, one-element messageIDs) and the bulk path
// (recategorize_bulk.go, one call per account covering every selected message in it) — Gmail's
// batchModify add/remove is idempotent, so applying the same diff across multiple messages at
// once is safe.
func (s *server) applyRecategorizeToGmail(ctx context.Context, svc *gmail.Client, messageIDs []string, promptByID map[int64]db.Prompt, addedIDs, removedIDs []int64) {
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
		mod, _, _ := processor.ModifyForPrompt(p, labelCache[p.LabelName])
		mod.MessageIDs = messageIDs
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
		mod.MessageIDs = messageIDs
		if untrash {
			// Untrash: remove TRASH, add INBOX
			trashReverseIDs = append(trashReverseIDs, messageIDs...)
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
// flagged for improvement, then hands the batch off to the improve worker (see
// server.dispatchImprove, improve.go) so handleRecategorize can return without waiting on
// Bedrock. No-op if improveSet is empty or svc is nil (fetching the message body needs a
// live Gmail client).
func (s *server) seedImproveSuggestions(ctx context.Context, row db.CategorizationHistory, promptByID map[int64]db.Prompt, addedIDs []int64, improveSet map[int64]bool, correctionID int64, corrErr error, note string, msg gmail.Message, haveMsg bool) {
	// msg/haveMsg come from handleRecategorize's single upfront fetch (see the example
	// recording block above it) rather than fetching again here — same message, same
	// content, no reason for a second Gmail API round trip.
	if len(improveSet) == 0 || !haveMsg {
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

	var targets []improveTarget
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
		targets = append(targets, improveTarget{
			SuggestionID:         sid,
			PromptID:             p.ID,
			OriginalInstructions: p.Instructions,
			Note:                 note,
		})
	}
	s.dispatchImprove(ctx, targets)
}
