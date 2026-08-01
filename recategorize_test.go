package main

import (
	"testing"

	"github.com/sloccy/ollamail-aws/db"
)

// TestSingleRecategorizeVerdicts locks in the single-email verdict table from the plan:
// a rule left checked before and after is a genuine affirmation (confirmed_positive)
// because the user saw an explicit checkbox and chose to leave it checked; a rule left
// unchecked before and after records nothing, since the user never affirmed or denied it.
func TestSingleRecategorizeVerdicts(t *testing.T) {
	// Prompt 1: newly checked (added).  Prompt 2: newly unchecked (removed).
	// Prompt 3: left checked (kept).   Prompt 4: left unchecked (untouched).
	currentIDs := map[int64]bool{2: true, 3: true}
	requestedIDs := map[int64]bool{1: true, 3: true}
	addedIDs := []int64{1}
	removedIDs := []int64{2}

	got := singleRecategorizeVerdicts(currentIDs, requestedIDs, addedIDs, removedIDs)

	want := map[int64]string{
		1: db.VerdictFalseNegative,
		2: db.VerdictFalsePositive,
		3: db.VerdictConfirmedPositive,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d verdicts, want %d: %+v", len(got), len(want), got)
	}
	for pid, wantVerdict := range want {
		if got[pid] != wantVerdict {
			t.Errorf("prompt %d: verdict = %q, want %q", pid, got[pid], wantVerdict)
		}
	}
	if _, ok := got[4]; ok {
		t.Errorf("prompt 4 (left unchecked) should record nothing, got %q", got[4])
	}
}

func TestSingleRecategorizeVerdicts_NoChanges(t *testing.T) {
	// Nothing added or removed, nothing kept-checked either (empty current/requested) —
	// should record nothing at all, not zero-value verdicts.
	got := singleRecategorizeVerdicts(map[int64]bool{}, map[int64]bool{}, nil, nil)
	if len(got) != 0 {
		t.Errorf("expected no verdicts, got %+v", got)
	}
}

// TestBulkVerdictsAndPlan locks in the bulk verdict table from the plan, which
// deliberately differs from the single-email table: only rules explicitly set to apply/
// remove produce a verdict, since a bulk action isn't a per-email review.
func TestBulkVerdictsAndPlan(t *testing.T) {
	promptByID := map[int64]db.Prompt{
		1: {ID: 1, Name: "apply-missing"},  // apply, not already applied -> false_negative
		2: {ID: 2, Name: "apply-present"},  // apply, already applied -> confirmed_positive
		3: {ID: 3, Name: "remove-present"}, // remove, already applied -> false_positive
		4: {ID: 4, Name: "remove-missing"}, // remove, not applied -> nothing
		5: {ID: 5, Name: "untouched"},      // no change -> nothing
	}
	current := map[int64]bool{2: true, 3: true, 5: true}
	applyIDs := []int64{1, 2}
	removeIDs := []int64{3, 4}

	verdicts, keptIDs, added := bulkVerdictsAndPlan(current, applyIDs, removeIDs, promptByID)

	wantVerdicts := map[int64]string{
		1: db.VerdictFalseNegative,
		2: db.VerdictConfirmedPositive,
		3: db.VerdictFalsePositive,
	}
	if len(verdicts) != len(wantVerdicts) {
		t.Fatalf("got %d verdicts, want %d: %+v", len(verdicts), len(wantVerdicts), verdicts)
	}
	for pid, want := range wantVerdicts {
		if verdicts[pid] != want {
			t.Errorf("prompt %d: verdict = %q, want %q", pid, verdicts[pid], want)
		}
	}
	for _, untouched := range []int64{4, 5} {
		if _, ok := verdicts[untouched]; ok {
			t.Errorf("prompt %d should record nothing, got %q", untouched, verdicts[untouched])
		}
	}

	// History plan: kept = current prompts not being removed (2, 5 — prompt 3 is removed).
	keptSet := map[int64]bool{}
	for _, id := range keptIDs {
		keptSet[id] = true
	}
	if !keptSet[2] || !keptSet[5] || keptSet[3] {
		t.Errorf("keptIDs = %v, want {2, 5} and not 3", keptIDs)
	}
	// Added: prompt 1 only (prompt 2 was already applied, so it's kept, not added).
	if len(added) != 1 || added[0].ID != 1 {
		t.Errorf("added = %+v, want [prompt 1]", added)
	}
}

func TestBulkVerdictsAndPlan_ApplyAndRemoveSameRuleIgnored(t *testing.T) {
	// Defensive case: a rule can't actually be both "apply to all" and "remove from all"
	// through the UI (mutually exclusive checkboxes — see bulkActionToggle in app.js), but
	// the handler should not crash or record a contradictory verdict if it happens anyway.
	promptByID := map[int64]db.Prompt{1: {ID: 1}}
	verdicts, _, added := bulkVerdictsAndPlan(map[int64]bool{}, []int64{1}, []int64{1}, promptByID)
	if _, ok := verdicts[1]; ok {
		t.Errorf("contradictory apply+remove should record nothing, got %q", verdicts[1])
	}
	if len(added) != 0 {
		t.Errorf("contradictory apply+remove should not add a history row, got %+v", added)
	}
}

func TestBuildPromptExamples(t *testing.T) {
	verdicts := map[int64]string{5: db.VerdictFalseNegative, 7: db.VerdictConfirmedPositive}
	got := buildPromptExamples(1, "msg1", "a@example.com", "Hello", "excerpt", "my note", verdicts)
	if len(got) != 2 {
		t.Fatalf("got %d examples, want 2: %+v", len(got), got)
	}
	byPrompt := map[int64]db.PromptExample{}
	for _, ex := range got {
		byPrompt[ex.PromptID] = ex
		if ex.AccountID != 1 || ex.MessageID != "msg1" || ex.Sender != "a@example.com" ||
			ex.Subject != "Hello" || ex.BodyExcerpt != "excerpt" || ex.Note != "my note" {
			t.Errorf("example metadata mismatch: %+v", ex)
		}
	}
	if byPrompt[5].Verdict != db.VerdictFalseNegative {
		t.Errorf("prompt 5 verdict = %q, want false_negative", byPrompt[5].Verdict)
	}
	if byPrompt[7].Verdict != db.VerdictConfirmedPositive {
		t.Errorf("prompt 7 verdict = %q, want confirmed_positive", byPrompt[7].Verdict)
	}
}

func TestBuildPromptExamples_EmptyVerdictsProducesNil(t *testing.T) {
	if got := buildPromptExamples(1, "msg1", "a", "b", "c", "d", nil); got != nil {
		t.Errorf("expected nil for empty verdicts, got %+v", got)
	}
}

func TestParseBulkSelections(t *testing.T) {
	got := parseBulkSelections([]string{"1:abc", "2:def", "1:abc", "bad-no-colon", "notanumber:xyz"})
	want := []bulkMessageKey{{AccountID: 1, MessageID: "abc"}, {AccountID: 2, MessageID: "def"}}
	if len(got) != len(want) {
		t.Fatalf("got %d selections, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("selection[%d] = %+v, want %+v", i, got[i], w)
		}
	}
}

func TestBulkTriggerKind(t *testing.T) {
	applySet := map[int64]bool{1: true}
	if got := bulkTriggerKind(1, applySet); got != db.TriggerKindFalseNegative {
		t.Errorf("apply rule trigger kind = %q, want false_negative", got)
	}
	if got := bulkTriggerKind(2, applySet); got != db.TriggerKindFalsePositive {
		t.Errorf("remove rule trigger kind = %q, want false_positive", got)
	}
}

// TestDedupeBySenderSubject_SameVerdictSameSenderSubject is the core case the dedup exists
// for: passive confirmation (processor.processEmail) writes a confirmed_positive on every
// match, so a recurring sender+subject (a daily digest, a templated receipt) would otherwise
// fill a verdict's example budget with near-identical rows. Only the newest (first, per
// selectExamplesForPrompt's ordering guarantee) survives.
func TestDedupeBySenderSubject_SameVerdictSameSenderSubject(t *testing.T) {
	examples := []db.PromptExample{
		{ID: 3, MessageID: "newest", Verdict: db.VerdictConfirmedPositive, Sender: "digest@example.com", Subject: "Weekly Digest"},
		{ID: 2, MessageID: "middle", Verdict: db.VerdictConfirmedPositive, Sender: "digest@example.com", Subject: "Weekly Digest"},
		{ID: 1, MessageID: "oldest", Verdict: db.VerdictConfirmedPositive, Sender: "digest@example.com", Subject: "Weekly Digest"},
	}
	got := dedupeBySenderSubject(examples, 10)
	if len(got) != 1 {
		t.Fatalf("got %d examples, want 1: %+v", len(got), got)
	}
	if got[0].MessageID != "newest" {
		t.Errorf("survivor = %q, want the newest (%q)", got[0].MessageID, "newest")
	}
}

// TestDedupeBySenderSubject_DifferentVerdictsIndependent checks the dedup is scoped per
// verdict ("each category" in the request) — the same sender+subject pair legitimately
// appearing in two different verdict groups (e.g. this sender's subject line used to be
// wrongly caught, and is now correctly matched) must not have one group suppress the other.
func TestDedupeBySenderSubject_DifferentVerdictsIndependent(t *testing.T) {
	examples := []db.PromptExample{
		{ID: 2, MessageID: "m2", Verdict: db.VerdictConfirmedPositive, Sender: "a@example.com", Subject: "Your receipt"},
		{ID: 1, MessageID: "m1", Verdict: db.VerdictFalsePositive, Sender: "a@example.com", Subject: "Your receipt"},
	}
	got := dedupeBySenderSubject(examples, 10)
	if len(got) != 2 {
		t.Fatalf("got %d examples, want 2 (one per verdict): %+v", len(got), got)
	}
}

// TestDedupeBySenderSubject_NormalizesCaseAndWhitespace checks the sender/subject
// comparison key is trimmed and case-folded, so a mail client rendering the same templated
// email's headers with different casing/whitespace across sends doesn't defeat the dedup.
func TestDedupeBySenderSubject_NormalizesCaseAndWhitespace(t *testing.T) {
	examples := []db.PromptExample{
		{ID: 2, MessageID: "newest", Verdict: db.VerdictConfirmedPositive, Sender: "Digest@Example.com", Subject: "  Weekly Digest"},
		{ID: 1, MessageID: "oldest", Verdict: db.VerdictConfirmedPositive, Sender: "digest@example.com ", Subject: "Weekly Digest  "},
	}
	got := dedupeBySenderSubject(examples, 10)
	if len(got) != 1 {
		t.Fatalf("got %d examples, want 1 (normalized to the same key): %+v", len(got), got)
	}
	if got[0].MessageID != "newest" {
		t.Errorf("survivor = %q, want the newest (%q)", got[0].MessageID, "newest")
	}
}

// TestDedupeBySenderSubject_CapsPerVerdict checks the per-verdict target cap: even with no
// sender+subject collisions at all, a verdict stops accepting examples once it reaches
// perVerdictTarget, so a very active rule can't blow past the intended budget fed to the
// improver.
func TestDedupeBySenderSubject_CapsPerVerdict(t *testing.T) {
	var examples []db.PromptExample
	for i := int64(1); i <= 5; i++ {
		examples = append(examples, db.PromptExample{
			ID:        i,
			MessageID: "distinct",
			Verdict:   db.VerdictConfirmedPositive,
			Sender:    "sender@example.com",
			Subject:   "distinct subject",
		})
		// Give each a genuinely distinct sender so there's no collision to dedupe away —
		// only the cap should be limiting the count.
		examples[len(examples)-1].Sender = examples[len(examples)-1].Sender + string(rune('a'+i))
	}
	got := dedupeBySenderSubject(examples, 3)
	if len(got) != 3 {
		t.Fatalf("got %d examples, want 3 (capped by perVerdictTarget)", len(got))
	}
}

// TestFilterResolved checks the "already fixed" filter: an example marked resolved by a
// prior applied suggestion is dropped, while an unresolved example with the same verdict is
// untouched — this is what stops the improver from being shown a problem it already solved,
// unless a fresh (necessarily unresolved) correction proves it's live again.
func TestFilterResolved(t *testing.T) {
	resolvedBy := int64(42)
	examples := []db.PromptExample{
		{ID: 1, MessageID: "already-fixed", Verdict: db.VerdictFalsePositive, ResolvedBySuggestionID: &resolvedBy},
		{ID: 2, MessageID: "still-live", Verdict: db.VerdictFalsePositive},
		{ID: 3, MessageID: "confirmed", Verdict: db.VerdictConfirmedPositive},
	}
	got := filterResolved(examples)
	if len(got) != 2 {
		t.Fatalf("got %d examples, want 2 (resolved one dropped): %+v", len(got), got)
	}
	for _, ex := range got {
		if ex.MessageID == "already-fixed" {
			t.Errorf("resolved example %q should have been filtered out", ex.MessageID)
		}
	}
}

// TestProblemExampleKeys checks problemExampleKeys only picks false_negative/
// false_positive entries — confirmed_positive examples are guardrails, not problems, and
// must never end up marked resolved.
func TestProblemExampleKeys(t *testing.T) {
	examples := []db.PromptExample{
		{ID: 1, PromptID: 5, Verdict: db.VerdictFalseNegative, CreatedAt: "2026-07-01 12:00:00"},
		{ID: 2, PromptID: 5, Verdict: db.VerdictFalsePositive, CreatedAt: "2026-07-01 12:00:01"},
		{ID: 3, PromptID: 5, Verdict: db.VerdictConfirmedPositive, CreatedAt: "2026-07-01 12:00:02"},
	}
	got := problemExampleKeys(examples)
	if len(got) != 2 {
		t.Fatalf("got %d keys, want 2 (confirmed_positive excluded): %+v", len(got), got)
	}
	wantIDs := map[int64]bool{1: true, 2: true}
	for _, k := range got {
		if !wantIDs[k.ID] {
			t.Errorf("unexpected key for example %d (verdict %q) in problemExampleKeys output", k.ID, k.Verdict)
		}
		if k.PromptID != 5 {
			t.Errorf("key PromptID = %d, want 5", k.PromptID)
		}
	}
}
