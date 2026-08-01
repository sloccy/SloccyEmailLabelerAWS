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

// singleRecategorizeVerdicts computes, for a single-email recategorization, which prompts
// get a permanent example recorded and what verdict each gets. This mirrors the checkbox
// state the user actually saw in recategorize_form.html: a rule left checked before and
// after is a genuine affirmation (confirmed_positive) because the user looked at an
// explicit checkbox for it and chose to leave it checked. A rule left unchecked before and
// after says nothing — the user never affirmed or denied it — so it records nothing.
// Compare bulkRecategorizeVerdict, whose table differs because a bulk "apply to all" /
// "remove from all" action isn't a per-email review and can't imply the same affirmation.
func singleRecategorizeVerdicts(currentIDs, requestedIDs map[int64]bool, addedIDs, removedIDs []int64) map[int64]string {
	verdicts := make(map[int64]string, len(addedIDs)+len(removedIDs)+len(currentIDs))
	for pid := range currentIDs {
		if requestedIDs[pid] {
			verdicts[pid] = db.VerdictConfirmedPositive
		}
	}
	for _, pid := range addedIDs {
		verdicts[pid] = db.VerdictFalseNegative
	}
	for _, pid := range removedIDs {
		verdicts[pid] = db.VerdictFalsePositive
	}
	return verdicts
}

// buildPromptExamples turns a promptID->verdict map into example rows ready for
// InsertPromptExamples. Every row from one recategorization shares the same email metadata
// (account/message/sender/subject/excerpt/note) — they all describe the same underlying
// correction, just from a different rule's point of view.
func buildPromptExamples(accountID int64, messageID, sender, subject, excerpt, note string, verdicts map[int64]string) []db.PromptExample {
	if len(verdicts) == 0 {
		return nil
	}
	examples := make([]db.PromptExample, 0, len(verdicts))
	for promptID, verdict := range verdicts {
		examples = append(examples, db.PromptExample{
			PromptID:    promptID,
			AccountID:   accountID,
			MessageID:   messageID,
			Verdict:     verdict,
			Sender:      sender,
			Subject:     subject,
			BodyExcerpt: excerpt,
			Note:        note,
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

	// Record permanent examples for every rule this correction touched or affirmed — see
	// singleRecategorizeVerdicts for the exact table. This is the corpus AI prompt
	// improvement now draws from (selectExamplesForPrompt), instead of seeing only whatever
	// single email triggered the current round.
	verdicts := singleRecategorizeVerdicts(currentIDs, requested, addedIDs, removedIDs)
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
			gmail.CollapseExcerpt(msg.Body, db.ExampleExcerptRunes), note, verdicts)
		if err := s.store.InsertPromptExamples(ctx, examples); err != nil {
			slog.Error("recategorize: insert prompt examples", "err", err)
		}
	}

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
		go s.runImproveSuggestions(s.ctx, suggestionIDs, promptByID, note)
	}
}

// selectExamplesForPrompt reads a rule's example corpus: the newest ~10 of each verdict
// (false_negative/false_positive/confirmed_positive), deduped by message across verdict
// groups. Feeds both ImprovePromptInstructions' three example slices and
// ReplayAgainstExamples' scoring set from a single read, rather than querying the corpus
// twice for the same data.
func (s *server) selectExamplesForPrompt(ctx context.Context, promptID int64) []db.PromptExample {
	// Raw fetch is generous — 40 per verdict, not the eventual 10-per-verdict target —
	// because dedupeBySenderSubject below can collapse many rows down to one when a
	// recurring sender (a daily digest, a templated receipt) has been writing a
	// confirmed_positive on every match via passive confirmation
	// (processor.processEmail), not just on manual corrections. Widening this is cheap:
	// ListExamplesByVerdict's cost is Limit-bounded regardless of corpus size.
	const perVerdictRawLimit = 40
	const perVerdictTarget = 10

	var all []db.PromptExample
	for _, v := range []string{db.VerdictFalseNegative, db.VerdictFalsePositive, db.VerdictConfirmedPositive} {
		examples, err := s.store.ListExamplesByVerdict(ctx, promptID, v, perVerdictRawLimit)
		if err != nil {
			slog.Error("select examples for prompt", "prompt_id", promptID, "verdict", v, "err", err)
			continue
		}
		all = append(all, examples...)
	}
	all = filterResolved(all)

	// The same message can appear in more than one verdict's top-N if it was corrected more
	// than once over time (e.g. false_positive once, confirmed_positive later after the rule
	// was fixed). Keeping every occurrence would hand the improver a live contradiction —
	// "this email is both a false positive and a confirmed positive" — so only the newest
	// occurrence survives, by db.PromptExample.ID (monotonically increasing, and shared
	// across every write path that can produce a PromptExample — see
	// db.InsertPromptExamples' doc comment). Iterating `all` in its original per-verdict-
	// query order (each already newest-first) and keeping only each message's
	// first-encountered survivor preserves that newest-first ordering in the output without
	// a second sort.
	newestID := make(map[string]int64, len(all))
	for _, ex := range all {
		if cur, ok := newestID[ex.MessageID]; !ok || ex.ID > cur {
			newestID[ex.MessageID] = ex.ID
		}
	}
	survivors := make([]db.PromptExample, 0, len(all))
	seen := make(map[string]bool, len(all))
	for _, ex := range all {
		if newestID[ex.MessageID] != ex.ID || seen[ex.MessageID] {
			continue
		}
		seen[ex.MessageID] = true
		survivors = append(survivors, ex)
	}

	return dedupeBySenderSubject(survivors, perVerdictTarget)
}

// senderSubjectKey normalizes a sender+subject pair for dedupeBySenderSubject: trimmed and
// case-folded, so trailing whitespace or casing differences (a mail client rendering
// headers slightly differently across sends of the same templated email) don't defeat the
// dedup. \x00 as a separator can't appear in either field, so it can't collide across a
// sender/subject boundary.
func senderSubjectKey(sender, subject string) string {
	return strings.ToLower(strings.TrimSpace(sender)) + "\x00" + strings.ToLower(strings.TrimSpace(subject))
}

// dedupeBySenderSubject walks examples — already newest-first within each verdict's
// contiguous span, per selectExamplesForPrompt's ordering guarantee — and, independently
// per verdict, keeps only the first (i.e. newest) occurrence of each sender+subject pair,
// capping each verdict at perVerdictTarget. Without this, a recurring sender could fill an
// entire verdict's example budget with near-identical rows: passive confirmation
// (processor.processEmail) writes a confirmed_positive on every ordinary classify match,
// so a daily newsletter from the same sender+subject would otherwise dominate the
// "already correct" evidence fed to the improver instead of a diverse sample.
func dedupeBySenderSubject(examples []db.PromptExample, perVerdictTarget int) []db.PromptExample {
	seen := make(map[string]map[string]bool)
	count := make(map[string]int)
	out := make([]db.PromptExample, 0, len(examples))
	for _, ex := range examples {
		if count[ex.Verdict] >= perVerdictTarget {
			continue
		}
		key := senderSubjectKey(ex.Sender, ex.Subject)
		if seen[ex.Verdict] == nil {
			seen[ex.Verdict] = make(map[string]bool)
		}
		if seen[ex.Verdict][key] {
			continue
		}
		seen[ex.Verdict][key] = true
		count[ex.Verdict]++
		out = append(out, ex)
	}
	return out
}

// filterResolved drops examples whose problem this rule has already been fixed for — see
// db.PromptExample.ResolvedBySuggestionID's doc comment. Applied first, ahead of both dedup
// passes above in selectExamplesForPrompt, so a resolved example can never win either dedup
// and end up in the output: showing the improver a case it already fixed is meaningless
// unless the rule regressed and missed it again, in which case a fresh (unresolved)
// correction on that email will already be in the pool.
func filterResolved(examples []db.PromptExample) []db.PromptExample {
	out := make([]db.PromptExample, 0, len(examples))
	for _, ex := range examples {
		if ex.ResolvedBySuggestionID != nil {
			continue
		}
		out = append(out, ex)
	}
	return out
}

// problemExampleKeys picks out the false_negative/false_positive entries from examples —
// the "problems" a suggestion built from them is meant to fix — and returns enough
// per-example key info (db.ResolvedExampleKey) for Store.MarkExamplesResolved to find and
// mark them once the suggestion is applied. confirmed_positive examples are never
// included: they aren't problems to resolve, they're guardrails a rewrite shouldn't have
// broken, and marking one resolved would just hide it from future improve rounds for no
// reason.
func problemExampleKeys(examples []db.PromptExample) []db.ResolvedExampleKey {
	var keys []db.ResolvedExampleKey
	for _, ex := range examples {
		if ex.Verdict == db.VerdictConfirmedPositive {
			continue
		}
		keys = append(keys, db.ResolvedExampleKey{
			PromptID:  ex.PromptID,
			Verdict:   ex.Verdict,
			CreatedAt: ex.CreatedAt,
			ID:        ex.ID,
		})
	}
	return keys
}

// improveRequestExamples groups a rule's example corpus into the three llm.ExampleRef
// slices llm.ImproveRequest expects, keyed by each example's stored Verdict.
func improveRequestExamples(examples []db.PromptExample) (shouldMatch, shouldNotMatch, alreadyCorrect []llm.ExampleRef) {
	for _, ex := range examples {
		ref := llm.ExampleRef{Sender: ex.Sender, Subject: ex.Subject, Excerpt: ex.BodyExcerpt}
		switch ex.Verdict {
		case db.VerdictFalseNegative:
			shouldMatch = append(shouldMatch, ref)
		case db.VerdictFalsePositive:
			shouldNotMatch = append(shouldNotMatch, ref)
		case db.VerdictConfirmedPositive:
			alreadyCorrect = append(alreadyCorrect, ref)
		}
	}
	return shouldMatch, shouldNotMatch, alreadyCorrect
}

// replayExamplesFor converts a rule's example corpus into llm.ReplayExample values:
// false_negative and confirmed_positive examples are expected to match the candidate rule,
// false_positive examples are expected not to.
func replayExamplesFor(examples []db.PromptExample) []llm.ReplayExample {
	out := make([]llm.ReplayExample, len(examples))
	for i, ex := range examples {
		out[i] = llm.ReplayExample{
			Verdict: ex.Verdict,
			Sender:  ex.Sender,
			Subject: ex.Subject,
			Excerpt: ex.BodyExcerpt,
			Want:    ex.Verdict != db.VerdictFalsePositive,
		}
	}
	return out
}

// improveReplayEnabled reports whether a candidate suggestion should be replay-validated
// against the classify model (see llm.ReplayAgainstExamples). Defaults to enabled — "1" or
// unset — since it's a correctness signal the user would otherwise have to eyeball; set
// llm.SettingImproveReplay to anything else in Settings to skip the extra classify calls.
func (s *server) improveReplayEnabled(ctx context.Context) bool {
	v, err := s.store.GetSetting(ctx, llm.SettingImproveReplay)
	return err != nil || v == "" || v == "1"
}

// improveAndFinalizeSuggestion runs one improve-plus-optional-replay round for a single
// suggestion and writes the result. It rebuilds ShouldMatch/ShouldNotMatch/AlreadyCorrect
// fresh from the prompt's *current* example corpus on every call — including on a
// regenerate round, where the pre-corpus version of this code instead replayed a
// conversation frozen around one snapshot email — then calls the improver (a first round
// when priorConv is empty and note carries the correction comment, a refinement round when
// priorConv is non-empty and userComment carries the user's feedback on the previous
// suggestion), optionally replays the candidate against the same corpus on the classify
// model (see llm.ReplayAgainstExamples — deliberately not the improve model just used
// above it), and finalizes the suggestion row. Shared by runImproveSuggestions (the initial
// batch triggered by a recategorization) and regeneratePromptSuggestion (a single manual
// "Regenerate with AI" round) so this logic can't drift between the two call sites.
func (s *server) improveAndFinalizeSuggestion(ctx context.Context, sid int64, p db.Prompt, originalInstructions string, priorConv []llm.ChatMessage, note, userComment string) {
	examples := s.selectExamplesForPrompt(ctx, p.ID)
	shouldMatch, shouldNotMatch, alreadyCorrect := improveRequestExamples(examples)

	suggested, conv, llmErr := s.llm.ImprovePromptInstructions(ctx, llm.ImproveRequest{
		PromptName:           p.Name,
		LabelName:            p.LabelName,
		OriginalInstructions: originalInstructions,
		ShouldMatch:          shouldMatch,
		ShouldNotMatch:       shouldNotMatch,
		AlreadyCorrect:       alreadyCorrect,
		UserNote:             note,
		PriorConversation:    priorConv,
		UserComment:          userComment,
	})
	if llmErr != nil {
		slog.Error("improve prompt", "suggestion_id", sid, "prompt_id", p.ID, "err", llmErr)
		if err := s.store.FinalizePromptSuggestion(ctx, db.FinalizePromptSuggestionParams{
			ID:                    sid,
			SuggestedInstructions: "",
			ConversationJSON:      "[]",
			Status:                db.SuggestionStatusFailed,
			UserComment:           llmErr.Error(),
		}); err != nil {
			slog.Error("finalize suggestion failed", "suggestion_id", sid, "err", err)
		}
		return
	}

	convJSON, _ := json.Marshal(conv) //nolint:errchkjson // []llm.ChatMessage cannot fail
	// Recorded on every generate/regenerate round, so applying whichever round's
	// suggestion the user actually accepts marks the examples that shaped *that* version —
	// not stale keys from an earlier round if the corpus shifted in between (see
	// improveAndFinalizeSuggestion's doc comment).
	problemKeysJSON, _ := json.Marshal(problemExampleKeys(examples)) //nolint:errchkjson // []db.ResolvedExampleKey cannot fail
	finalize := db.FinalizePromptSuggestionParams{
		ID:                    sid,
		SuggestedInstructions: suggested,
		ConversationJSON:      string(convJSON),
		Status:                db.SuggestionStatusPending,
		UserComment:           userComment,
		ProblemExampleKeys:    string(problemKeysJSON),
	}
	if s.improveReplayEnabled(ctx) {
		replay := s.llm.ReplayAgainstExamples(ctx, s.store, suggested, replayExamplesFor(examples), s.cfg.ClassifyConcurrency)
		failuresJSON, _ := json.Marshal(replay.Failures) //nolint:errchkjson // []llm.ReplayFailure cannot fail
		finalize.ReplayModel = replay.Model
		finalize.ReplayTotal = int64(replay.Total)
		finalize.ReplayPassed = int64(replay.Passed)
		finalize.ReplayBaseline = replayBaseline(examples)
		finalize.ReplayFailures = string(failuresJSON)
	}
	if err := s.store.FinalizePromptSuggestion(ctx, finalize); err != nil {
		slog.Error("finalize suggestion failed", "suggestion_id", sid, "err", err)
	}
	slog.Info("improve suggestion ready", "suggestion_id", sid, "prompt_id", p.ID, "replay_total", finalize.ReplayTotal, "replay_passed", finalize.ReplayPassed)
}

// runImproveSuggestions generates prompt improvement suggestions for each flagged prompt
// from a recategorization. The suggestion rows (status='generating') must already exist in
// the DB before this is called. Runs in a goroutine so the recategorize handler can return
// without waiting for Bedrock.
func (s *server) runImproveSuggestions(
	baseCtx context.Context,
	suggestionIDs map[int64]int64,
	promptByID map[int64]db.Prompt,
	note string,
) {
	ctx, cancel := context.WithTimeout(baseCtx, 20*time.Minute)
	defer cancel()

	slog.Info("improve suggestions start", "count", len(suggestionIDs))
	for pid, sid := range suggestionIDs {
		p, ok := promptByID[pid]
		if !ok {
			continue
		}
		s.improveAndFinalizeSuggestion(ctx, sid, p, p.Instructions, nil, note, "")
	}
	slog.Debug("improve suggestions done", "count", len(suggestionIDs))
}

// regeneratePromptSuggestion runs one manual "Regenerate with AI" round in the background —
// handlePromptSuggestionRegenerate (server.go) marks the suggestion 'generating' and
// returns immediately before spawning this, since a full round (improve call plus ~30
// replay classify calls) would blow the WebFunction's 30s Lambda timeout if run
// synchronously in the request, the same reason the initial round has always been
// backgrounded.
func (s *server) regeneratePromptSuggestion(baseCtx context.Context, sid int64, p db.Prompt, originalInstructions string, priorConv []llm.ChatMessage, userComment string) {
	ctx, cancel := context.WithTimeout(baseCtx, 20*time.Minute)
	defer cancel()
	s.improveAndFinalizeSuggestion(ctx, sid, p, originalInstructions, priorConv, "", userComment)
}

// replayBaseline is the free baseline ReplayResult.Passed is compared against: how many of
// the same examples the *original* rule already got right, derived from the verdict
// recorded when each example was created rather than by re-running the original
// instructions. false_negative and false_positive examples were misses by definition
// (that's why they were recorded); confirmed_positive examples were hits.
func replayBaseline(examples []db.PromptExample) int64 {
	var n int64
	for _, ex := range examples {
		if ex.Verdict == db.VerdictConfirmedPositive {
			n++
		}
	}
	return n
}
