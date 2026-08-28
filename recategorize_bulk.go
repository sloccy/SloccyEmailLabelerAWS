package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sloccy/ollamail-aws/db"
	"github.com/sloccy/ollamail-aws/gmail"
)

// ============================================================
// Bulk recategorize
// ============================================================
//
// The single-email recategorize flow (recategorize.go) diffs one message's exact checkbox
// state against its current labeling. Bulk instead applies a uniform action per rule — "no
// change" / "apply to all" / "remove from all" — across every selected email, so the
// verdict table and the DynamoDB access pattern both differ enough to warrant their own
// file rather than threading a second mode through handleRecategorize.
//
// Efficiency is the whole point of this file: a 50-email selection must complete well
// inside the WebFunction's 30s Lambda timeout (template.yaml) and stay within the table's
// provisioned 2 RCU/2 WCU. That means "one query per involved account," never "one query
// per selected message" — see GetHistoryStateForMessages and RewriteHistoryForMessages in
// db/store.go, which this handler calls once per account regardless of how many messages
// in that account were selected.

// bulkRecategorizeMaxEmails caps a single bulk action. This is the 30s Lambda-timeout
// budget expressed as a selection size, not an arbitrary UI limit — enforced here again
// (not just client-side in static/app.js) since the client-side cap is only a UX nicety.
const bulkRecategorizeMaxEmails = 50

// bulkExcerptBudget bounds the wall-clock time spent fetching body excerpts for
// db.PromptExample rows across the whole bulk request (not per account/message). Excerpts
// are fetched synchronously in the request — like the single-email path, examples are the
// durable record and can't depend on a background goroutine surviving a Lambda freeze — so
// if the budget runs out, remaining examples are written with an empty excerpt
// (subject/sender still carry most of the signal) rather than blocking the whole action.
const bulkExcerptBudget = 10 * time.Second

// bulkMessageKey identifies one selected email: which account it belongs to and its Gmail
// message id. Parsed from the "selections" form field, one "<accountID>:<messageID>" value
// per checkbox — a self-contained encoding was chosen over two parallel arrays
// (message_ids[]/account_ids[]) so a form field ever getting reordered can't silently pair
// the wrong account with the wrong message.
type bulkMessageKey struct {
	AccountID int64
	MessageID string
}

// parseBulkSelections parses and dedupes the "selections" form values.
func parseBulkSelections(values []string) []bulkMessageKey {
	seen := make(map[bulkMessageKey]bool, len(values))
	var out []bulkMessageKey
	for _, v := range values {
		aidStr, mid, ok := strings.Cut(v, ":")
		if !ok || mid == "" {
			continue
		}
		aid, err := strconv.ParseInt(aidStr, 10, 64)
		if err != nil {
			continue
		}
		k := bulkMessageKey{AccountID: aid, MessageID: mid}
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

// bulkVerdictsAndPlan computes one message's verdicts and history-rewrite plan from its
// current prompt-id state plus the bulk apply/remove action sets. This is the bulk
// counterpart to singleRecategorizeVerdicts (recategorize.go), and deliberately has a
// different table: a bulk "apply to all" / "remove from all" action isn't a per-email
// review, so unlike the single-email checkbox table, there is no "left unchanged, still
// records something" case — only rules the user explicitly pointed at this message's rule
// set produce a verdict, and "remove" on a rule that wasn't even applied records nothing at
// all, same reasoning as singleRecategorizeVerdicts' "still doesn't match" skip — see
// db.VerdictConfirmedNegative's doc comment.
//
//	apply, not already applied  -> confirmed_positive, missed  (rule missed it)
//	apply, already applied      -> confirmed_positive
//	remove, already applied     -> confirmed_negative, missed  (rule wrongly caught it)
//	remove, not applied         -> (nothing)
//	no change                   -> (nothing)
func bulkVerdictsAndPlan(current map[int64]bool, applyIDs, removeIDs []int64, promptByID map[int64]db.Prompt) (verdicts map[int64]exampleVerdict, keptIDs []int64, added []db.Prompt) {
	applySet := idSet(applyIDs)
	removeSet := idSet(removeIDs)
	verdicts = make(map[int64]exampleVerdict)

	for _, pid := range applyIDs {
		if removeSet[pid] {
			continue // contradictory (apply and remove on the same rule); ignore defensively
		}
		if current[pid] {
			verdicts[pid] = exampleVerdict{Verdict: db.VerdictConfirmedPositive}
		} else {
			verdicts[pid] = exampleVerdict{Verdict: db.VerdictConfirmedPositive, Missed: true}
			if p, ok := promptByID[pid]; ok {
				added = append(added, p)
			}
		}
	}
	for _, pid := range removeIDs {
		if applySet[pid] {
			continue // contradictory; same defensive ignore as the apply loop above
		}
		if !current[pid] {
			continue // wasn't applied; nothing was actually corrected here
		}
		verdicts[pid] = exampleVerdict{Verdict: db.VerdictConfirmedNegative, Missed: true}
	}
	for pid := range current {
		if !removeSet[pid] {
			keptIDs = append(keptIDs, pid)
		}
	}
	return verdicts, keptIDs, added
}

// fetchExcerptsBounded fetches body excerpts for messageIDs via svc, bounded by
// concurrency and ctx's deadline (see bulkExcerptBudget). Messages that fail to fetch or
// don't complete before the deadline are simply absent from the result map — callers treat
// a missing excerpt as "" rather than failing the example row over it.
func fetchExcerptsBounded(ctx context.Context, svc *gmail.Client, messageIDs []string, concurrency int) map[string]string {
	if len(messageIDs) == 0 {
		return nil
	}
	if concurrency < 1 {
		concurrency = 1
	}
	result := make(map[string]string, len(messageIDs))
	var mu sync.Mutex
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, mid := range messageIDs {
		select {
		case <-ctx.Done():
			// Budget already exhausted; remaining messages just don't get an excerpt.
			continue
		default:
		}
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			msg, err := gmail.FetchMessage(ctx, svc, mid, 0)
			if err != nil {
				return
			}
			excerpt := gmail.CollapseExcerpt(msg.Body, db.ExampleExcerptRunes)
			mu.Lock()
			result[mid] = excerpt
			mu.Unlock()
		})
	}
	wg.Wait()
	return result
}

// bulkTriggerKind reports why a rule was flagged for improvement in a bulk action: "apply"
// actions mean the rule missed some of the selection (false_negative), "remove" actions
// mean it wrongly caught some of it (false_positive). Every prompt id offered for
// improvement in the bulk form is guaranteed to be in exactly one of applySet/removeSet —
// the UI only shows the improve checkbox next to a rule once an action is chosen for it.
func bulkTriggerKind(pid int64, applySet map[int64]bool) string {
	if applySet[pid] {
		return db.TriggerKindFalseNegative
	}
	return db.TriggerKindFalsePositive
}

// bulkRecategorizeFormData feeds bulk_recategorize_form.html. Selections is re-serialized
// as hidden "selections" inputs in the form (same "<accountID>:<messageID>" encoding used
// on both the GET query string that built this data and the POST that submits the form),
// so the browser round-trips the selection without the server needing to remember it
// between requests.
type bulkRecategorizeFormData struct {
	Selections []bulkMessageKey
	Count      int
	Prompts    []promptView // AccountEmail set (via dbPromptToView) for account-scoped rules
}

// handleBulkRecategorizeForm renders the bulk recategorize modal for a client-selected set
// of emails, passed as repeated "selections" query params (the GET counterpart to the POST
// handler's form field of the same name and encoding — see parseBulkSelections). The rule
// list is the union of ListActivePromptsForAccount across every distinct account touched by
// the selection, so an account-scoped rule still shows up (badged in the template) even
// when the selection also spans other accounts.
func (s *server) handleBulkRecategorizeForm(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	selections := parseBulkSelections(r.URL.Query()["selections"])
	if len(selections) == 0 {
		http.Error(w, "no emails selected", http.StatusBadRequest)
		return
	}
	if len(selections) > bulkRecategorizeMaxEmails {
		http.Error(w, fmt.Sprintf("too many emails selected (max %d)", bulkRecategorizeMaxEmails), http.StatusBadRequest)
		return
	}

	accountIDs := make(map[int64]bool)
	for _, sel := range selections {
		accountIDs[sel.AccountID] = true
	}

	promptSeen := make(map[int64]bool)
	var rawPrompts []db.Prompt
	accountEmails := make(map[int64]string, len(accountIDs))
	for aid := range accountIDs {
		if acc, err := s.store.GetAccount(ctx, aid); err == nil {
			accountEmails[aid] = acc.Email
		}
		ps, _ := s.store.ListActivePromptsForAccount(ctx, aid)
		for _, p := range ps {
			if !promptSeen[p.ID] {
				promptSeen[p.ID] = true
				rawPrompts = append(rawPrompts, p)
			}
		}
	}
	prompts := make([]promptView, len(rawPrompts))
	for i, p := range rawPrompts {
		prompts[i] = dbPromptToView(p, accountEmails)
	}

	s.fragmentResponse(w, "bulk_recategorize_form.html", bulkRecategorizeFormData{
		Selections: selections,
		Count:      len(selections),
		Prompts:    prompts,
	}, "")
}

func (s *server) handleBulkRecategorize(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	selections := parseBulkSelections(r.Form["selections"])
	if len(selections) == 0 {
		http.Error(w, "no emails selected", http.StatusBadRequest)
		return
	}
	if len(selections) > bulkRecategorizeMaxEmails {
		http.Error(w, fmt.Sprintf("too many emails selected (max %d)", bulkRecategorizeMaxEmails), http.StatusBadRequest)
		return
	}

	applyIDs := parseIDList(r.Form["apply_prompt_ids"])
	removeIDs := parseIDList(r.Form["remove_prompt_ids"])
	improveIDs := parseIDList(r.Form["improve_prompt_ids"])
	applySet := idSet(applyIDs)
	improveSet := idSet(improveIDs)
	note := r.FormValue("note")

	byAccount := make(map[int64][]string, len(selections))
	for _, sel := range selections {
		byAccount[sel.AccountID] = append(byAccount[sel.AccountID], sel.MessageID)
	}

	// Derived only from the request-level apply/remove sets, not from any per-account
	// state, so computed once here rather than rebuilt inside the per-account loop below.
	addedCSV := joinIDs(applyIDs)
	removedCSV := joinIDs(removeIDs)

	excerptCtx, excerptCancel := context.WithTimeout(ctx, bulkExcerptBudget)
	defer excerptCancel()

	promptByID := make(map[int64]db.Prompt)
	var allExamples []db.PromptExample
	var allCorrections []db.InsertEmailCorrectionParams

	for accountID, messageIDs := range byAccount {
		account, err := s.store.GetAccount(ctx, accountID)
		if err != nil {
			slog.Error("bulk recategorize: get account", "account_id", accountID, "err", err)
			continue
		}
		svc, err := s.gmailServiceFor(ctx, account)
		if err != nil {
			slog.Error("bulk recategorize: gmail service", "account_id", accountID, "err", err)
			continue
		}

		prompts, _ := s.store.ListActivePromptsForAccount(ctx, accountID)
		for _, p := range prompts {
			promptByID[p.ID] = p
		}

		// One query for this account's current state across every selected message in it —
		// see GetHistoryStateForMessages' doc comment for why this matters at 2 RCU.
		state, err := s.store.GetHistoryStateForMessages(ctx, accountID, messageIDs)
		if err != nil {
			slog.Error("bulk recategorize: get history state", "account_id", accountID, "err", err)
			continue
		}

		// Gmail: apply/remove uniformly across every selected message in this account, via
		// the same path the single-email flow uses (recategorize.go). Unlike the
		// single-email path, no per-message diffing is needed here — Gmail's batchModify
		// add/remove is idempotent, so applying a label a message already has is a harmless
		// no-op — but applyRecategorizeToGmail's messageIDs parameter accepts the whole
		// account's selection uniformly either way.
		s.applyRecategorizeToGmail(ctx, svc, messageIDs, promptByID, applyIDs, removeIDs)

		// Verdicts + history plan per message (pure, from `state` — no further DB reads).
		plans := make([]db.RewriteMessagePlan, 0, len(messageIDs))
		var needExcerpt []string
		perMessageVerdicts := make(map[string]map[int64]exampleVerdict, len(messageIDs))
		for _, mid := range messageIDs {
			st, ok := state[mid]
			if !ok {
				slog.Error("bulk recategorize: message missing from history state", "account_id", accountID, "message_id", mid)
				continue
			}
			verdicts, keptIDs, added := bulkVerdictsAndPlan(st.CurrentPromptIDs, applyIDs, removeIDs, promptByID)
			plans = append(plans, db.RewriteMessagePlan{
				MessageID: mid, Subject: st.Subject, Sender: st.Sender,
				KeptIDs: keptIDs, AddedPrompts: added,
			})
			if len(verdicts) > 0 {
				perMessageVerdicts[mid] = verdicts
				needExcerpt = append(needExcerpt, mid)
			}
		}

		if err := s.store.RewriteHistoryForMessages(ctx, accountID, account.Email, plans); err != nil {
			slog.Error("bulk recategorize: rewrite history", "account_id", accountID, "err", err)
		}

		excerpts := fetchExcerptsBounded(excerptCtx, svc, needExcerpt, s.cfg.ClassifyConcurrency)

		for _, mid := range needExcerpt {
			st := state[mid]
			allCorrections = append(allCorrections, db.InsertEmailCorrectionParams{
				AccountID:      accountID,
				MessageID:      mid,
				AddedPrompts:   addedCSV,
				RemovedPrompts: removedCSV,
				Note:           note,
			})
			examples := buildPromptExamples(accountID, mid, st.Sender, st.Subject, excerpts[mid], note, perMessageVerdicts[mid], promptByID)
			allExamples = append(allExamples, examples...)
		}
	}

	// Batched across the whole action, not per account or per message — see
	// InsertEmailCorrections' doc comment for why a bulk action's corrections don't need
	// the single-email path's sequential nextID.
	if len(allCorrections) > 0 {
		if err := s.store.InsertEmailCorrections(ctx, allCorrections); err != nil {
			slog.Error("bulk recategorize: insert corrections", "count", len(allCorrections), "err", err)
		}
	}

	if len(allExamples) > 0 {
		if err := s.store.InsertPromptExamples(ctx, allExamples); err != nil {
			slog.Error("bulk recategorize: insert prompt examples", "err", err)
		}
		incrementVersionObservedFor(ctx, s.store, allExamples)
	}

	// One suggestion per flagged rule, not per email — the corpus (just written above)
	// already carries every touched message's examples, so the improve worker needs
	// nothing email-specific to run; it reads the corpus fresh via selectExamplesForImprove.
	var targets []improveTarget
	for pid := range improveSet {
		p, ok := promptByID[pid]
		if !ok {
			continue
		}
		sid, err := s.store.InsertPromptSuggestion(ctx, db.InsertPromptSuggestionParams{
			PromptID:              p.ID,
			TriggerKind:           bulkTriggerKind(pid, applySet),
			EmailSubject:          fmt.Sprintf("Bulk recategorization (%d emails)", len(selections)),
			OriginalInstructions:  p.Instructions,
			SuggestedInstructions: "",
			ConversationJSON:      "[]",
			Status:                db.SuggestionStatusGenerating,
		})
		if err != nil {
			slog.Error("bulk recategorize: insert generating suggestion", "prompt_id", pid, "err", err)
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

	setHxTrigger(w, map[string]any{
		triggerShowToast:              map[string]any{toastKeyMessage: fmt.Sprintf("Recategorized %d emails", len(selections)), jsonKeyType: toastTypeSuccess},
		"closeModal":                  "recategorize-modal",
		triggerRefreshSuggestionBadge: "1",
		"refreshHistory":              "1",
		"refreshSuggestions":          "1",
	})
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
}

// handleBulkConfirmCategorization is the bulk counterpart to handleConfirmCategorization —
// records a confirmed_positive example on every selected email for each of its
// currently-applied rules (see confirmCategorization's doc comment for why every other
// active rule gets nothing, not a confirmed_negative), without touching Gmail, history, or
// suggestions. Shares bulkRecategorizeMaxEmails and parseBulkSelections with the bulk
// recategorize handler above; unlike it, there's no apply/remove action set to read, since
// confirm always means "the current state is correct."
func (s *server) handleBulkConfirmCategorization(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	selections := parseBulkSelections(r.Form["selections"])
	if len(selections) == 0 {
		http.Error(w, "no emails selected", http.StatusBadRequest)
		return
	}
	if len(selections) > bulkRecategorizeMaxEmails {
		http.Error(w, fmt.Sprintf("too many emails selected (max %d)", bulkRecategorizeMaxEmails), http.StatusBadRequest)
		return
	}

	byAccount := make(map[int64][]string, len(selections))
	for _, sel := range selections {
		byAccount[sel.AccountID] = append(byAccount[sel.AccountID], sel.MessageID)
	}

	excerptCtx, excerptCancel := context.WithTimeout(ctx, bulkExcerptBudget)
	defer excerptCancel()

	var allExamples []db.PromptExample

	for accountID, messageIDs := range byAccount {
		account, err := s.store.GetAccount(ctx, accountID)
		if err != nil {
			slog.Error("bulk confirm: get account", "account_id", accountID, "err", err)
			continue
		}
		prompts, _ := s.store.ListActivePromptsForAccount(ctx, accountID)
		if len(prompts) == 0 {
			continue
		}
		promptByID := make(map[int64]db.Prompt, len(prompts))
		allPromptIDs := make([]int64, len(prompts))
		for i, p := range prompts {
			promptByID[p.ID] = p
			allPromptIDs[i] = p.ID
		}

		// One query for this account's current state across every selected message in it —
		// same reasoning as handleBulkRecategorize (see GetHistoryStateForMessages' doc
		// comment).
		state, err := s.store.GetHistoryStateForMessages(ctx, accountID, messageIDs)
		if err != nil {
			slog.Error("bulk confirm: get history state", "account_id", accountID, "err", err)
			continue
		}

		var excerpts map[string]string
		if svc, err := s.gmailServiceFor(ctx, account); err == nil {
			excerpts = fetchExcerptsBounded(excerptCtx, svc, messageIDs, s.cfg.ClassifyConcurrency)
		} else {
			slog.Error("bulk confirm: gmail service", "account_id", accountID, "err", err)
		}

		for _, mid := range messageIDs {
			st, ok := state[mid]
			if !ok {
				continue
			}
			verdicts := singleRecategorizeVerdicts(allPromptIDs, st.CurrentPromptIDs, st.CurrentPromptIDs)
			examples := buildPromptExamples(accountID, mid, st.Sender, st.Subject, excerpts[mid], "", verdicts, promptByID)
			allExamples = append(allExamples, examples...)
		}
	}

	if len(allExamples) > 0 {
		if err := s.store.InsertPromptExamples(ctx, allExamples); err != nil {
			slog.Error("bulk confirm: insert prompt examples", "err", err)
		}
		incrementVersionObservedFor(ctx, s.store, allExamples)
	}

	setHxTrigger(w, map[string]any{
		triggerShowToast: map[string]any{toastKeyMessage: fmt.Sprintf("Confirmed %d emails", len(selections)), jsonKeyType: toastTypeSuccess},
	})
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
}
