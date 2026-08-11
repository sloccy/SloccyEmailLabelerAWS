package main

import (
	"context"
	"fmt"
	"sort"
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
	promptByID := map[int64]db.Prompt{
		5: {ID: 5, CurrentVersionID: 42},
		7: {ID: 7}, // no version yet — should stamp 0, not panic on a missing entry's zero value
	}
	got := buildPromptExamples(1, "msg1", "a@example.com", "Hello", "excerpt", "my note", verdicts, promptByID)
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
	if byPrompt[5].PromptVersionID != 42 {
		t.Errorf("prompt 5 PromptVersionID = %d, want 42 (stamped from promptByID.CurrentVersionID)", byPrompt[5].PromptVersionID)
	}
	if byPrompt[7].PromptVersionID != 0 {
		t.Errorf("prompt 7 PromptVersionID = %d, want 0 (no version recorded yet)", byPrompt[7].PromptVersionID)
	}
}

func TestBuildPromptExamples_MissingPromptFromMapStampsZero(t *testing.T) {
	// Defensive: shouldn't happen (the caller builds promptByID from the same prompts the
	// verdict map came from), but a promptID absent from the map must degrade to "no
	// version," not panic on a missing map entry.
	verdicts := map[int64]string{99: db.VerdictFalseNegative}
	got := buildPromptExamples(1, "msg1", "a", "b", "c", "d", verdicts, map[int64]db.Prompt{})
	if len(got) != 1 || got[0].PromptVersionID != 0 {
		t.Errorf("got %+v, want one example with PromptVersionID=0", got)
	}
}

func TestBuildPromptExamples_EmptyVerdictsProducesNil(t *testing.T) {
	if got := buildPromptExamples(1, "msg1", "a", "b", "c", "d", nil, nil); got != nil {
		t.Errorf("expected nil for empty verdicts, got %+v", got)
	}
}

// fakeVersionObserver records every IncrementVersionObservedBy call, keyed by
// (promptID, versionID, verdict), for TestIncrementVersionObservedFor to assert against.
type fakeVersionObserver struct {
	calls map[[3]any]int64
}

func (f *fakeVersionObserver) IncrementVersionObservedBy(_ context.Context, promptID, versionID int64, verdict string, n int64) {
	if f.calls == nil {
		f.calls = map[[3]any]int64{}
	}
	f.calls[[3]any{promptID, versionID, verdict}] += n
}

func TestIncrementVersionObservedFor(t *testing.T) {
	// Deliberately includes a confirmed_positive example — IncrementVersionObservedBy
	// itself (db/store.go) is responsible for skipping it, not this loop, so this test
	// doubles as a check that the loop really does pass through every distinct key
	// unconditionally. Also includes two examples sharing the same (promptID, versionID,
	// verdict) — the case incrementVersionObservedFor's aggregation exists for — to confirm
	// they collapse into one call carrying the combined count rather than two separate ones.
	examples := []db.PromptExample{
		{PromptID: 1, PromptVersionID: 10, Verdict: db.VerdictFalsePositive},
		{PromptID: 1, PromptVersionID: 10, Verdict: db.VerdictFalsePositive},
		{PromptID: 2, PromptVersionID: 20, Verdict: db.VerdictFalseNegative},
		{PromptID: 3, PromptVersionID: 30, Verdict: db.VerdictConfirmedPositive},
	}
	f := &fakeVersionObserver{}
	incrementVersionObservedFor(t.Context(), f, examples)

	want := map[[3]any]int64{
		{int64(1), int64(10), db.VerdictFalsePositive}:     2,
		{int64(2), int64(20), db.VerdictFalseNegative}:     1,
		{int64(3), int64(30), db.VerdictConfirmedPositive}: 1,
	}
	if len(f.calls) != len(want) {
		t.Fatalf("expected %d distinct calls, got %d: %+v", len(want), len(f.calls), f.calls)
	}
	for k, n := range want {
		if got := f.calls[k]; got != n {
			t.Errorf("call %v: count = %d, want %d", k, got, n)
		}
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

// TestRoundRobinBySender_TightBudgetKeepsNewestPerBucket checks the scarce-budget case: with
// only one distinct sender+subject bucket and room for just one pick, the newest survives —
// this is the same "recurring sender doesn't dominate" guarantee the old flat dedup gave,
// just expressed as "the newest wins the one slot" rather than "collapse to exactly one
// forever" (see TestRoundRobinBySender_SpreadsAcrossBuckets for why round-robin, unlike the
// old dedup, will take more than one from a bucket once every other bucket is exhausted and
// budget remains).
func TestRoundRobinBySender_TightBudgetKeepsNewestPerBucket(t *testing.T) {
	examples := []db.PromptExample{
		{ID: 3, MessageID: "newest", Sender: "digest@example.com", Subject: "Weekly Digest"},
		{ID: 2, MessageID: "middle", Sender: "digest@example.com", Subject: "Weekly Digest"},
		{ID: 1, MessageID: "oldest", Sender: "digest@example.com", Subject: "Weekly Digest"},
	}
	got := roundRobinBySender(examples, 1)
	if len(got) != 1 || got[0].MessageID != "newest" {
		t.Fatalf("got %+v, want just the newest example", got)
	}
}

// TestRoundRobinBySender_DrainsASingleBucketWhenNoOthersCompete checks the flip side: with
// only one bucket present at all, round-robin has nothing to interleave against, so it
// correctly drains that whole bucket up to budget rather than artificially withholding
// examples that exist nowhere else in the corpus.
func TestRoundRobinBySender_DrainsASingleBucketWhenNoOthersCompete(t *testing.T) {
	examples := []db.PromptExample{
		{ID: 3, MessageID: "newest", Sender: "digest@example.com", Subject: "Weekly Digest"},
		{ID: 2, MessageID: "middle", Sender: "digest@example.com", Subject: "Weekly Digest"},
		{ID: 1, MessageID: "oldest", Sender: "digest@example.com", Subject: "Weekly Digest"},
	}
	got := roundRobinBySender(examples, 10)
	if len(got) != 3 {
		t.Fatalf("got %d examples, want all 3 (one bucket, nothing to spread across, budget unused otherwise): %+v", len(got), got)
	}
}

// TestRoundRobinBySender_SpreadsAcrossBuckets is the actual fix this sampler exists for:
// unlike a flat "first N distinct, newest-first" scan, round-robin takes one from every
// bucket before taking a second from any one — so a sender with many recent emails (all the
// same recurring sender+subject pattern) doesn't crowd out a sender with fewer, older ones.
func TestRoundRobinBySender_SpreadsAcrossBuckets(t *testing.T) {
	var examples []db.PromptExample
	// Sender A: 5 recent emails, all the same recurring sender+subject pattern (a daily
	// digest) — one bucket.
	for i := int64(10); i < 15; i++ {
		examples = append(examples, db.PromptExample{ID: i, MessageID: fmt.Sprintf("a%d", i), Sender: "a@example.com", Subject: "Daily digest"})
	}
	// Sender B: 1 older email — a second, distinct bucket.
	examples = append(examples, db.PromptExample{ID: 1, MessageID: "b1", Sender: "b@example.com", Subject: "only one"})
	// Newest-first, matching gatherRawExamples' contract.
	sort.Slice(examples, func(i, j int) bool { return examples[i].ID > examples[j].ID })

	got := roundRobinBySender(examples, 2)
	if len(got) != 2 {
		t.Fatalf("got %d examples, want 2", len(got))
	}
	senders := map[string]bool{}
	for _, ex := range got {
		senders[ex.Sender] = true
	}
	if !senders["a@example.com"] || !senders["b@example.com"] {
		t.Errorf("expected one pick from each sender, got %+v (a flat recency scan would pick two from sender A's digest instead)", got)
	}
}

// TestRoundRobinBySender_NormalizesCaseAndWhitespace checks the bucket key is trimmed and
// case-folded, so a mail client rendering the same templated email's headers with different
// casing/whitespace across sends still collapses into one bucket.
func TestRoundRobinBySender_NormalizesCaseAndWhitespace(t *testing.T) {
	examples := []db.PromptExample{
		{ID: 2, MessageID: "newest", Sender: "Digest@Example.com", Subject: "  Weekly Digest"},
		{ID: 1, MessageID: "oldest", Sender: "digest@example.com ", Subject: "Weekly Digest  "},
	}
	got := roundRobinBySender(examples, 1)
	if len(got) != 1 || got[0].MessageID != "newest" {
		t.Fatalf("got %+v, want just the newest (both normalize to the same bucket)", got)
	}
}

// TestRoundRobinBySender_RespectsBudget checks the budget cap: even with no bucket
// collisions at all (every example a distinct sender), the sampler stops at budget.
func TestRoundRobinBySender_RespectsBudget(t *testing.T) {
	var examples []db.PromptExample
	for i := int64(1); i <= 5; i++ {
		examples = append(examples, db.PromptExample{
			ID: i, MessageID: "distinct", Sender: fmt.Sprintf("sender%d@example.com", i), Subject: "distinct subject",
		})
	}
	got := roundRobinBySender(examples, 3)
	if len(got) != 3 {
		t.Fatalf("got %d examples, want 3 (capped by budget)", len(got))
	}
}

// ============================================================
// sampleExamples (tiered: recurred > manual > passive, round-robin within tiers)
// ============================================================

// TestSampleExamples_RecurredPrioritizedOverManualAndPassive checks Tier 1: a recurred
// example wins a slot even when non-recurred manual/passive examples are newer.
func TestSampleExamples_RecurredPrioritizedOverManualAndPassive(t *testing.T) {
	examples := []db.PromptExample{
		{ID: 3, MessageID: "m3", Verdict: db.VerdictFalsePositive, Sender: "c@example.com", Subject: "s3", Source: db.ExampleSourceManual},
		{ID: 2, MessageID: "m2", Verdict: db.VerdictFalsePositive, Sender: "b@example.com", Subject: "s2", Source: db.ExampleSourcePassive},
		{ID: 1, MessageID: "m1", Verdict: db.VerdictFalsePositive, Sender: "a@example.com", Subject: "s1", Recurred: true},
	}
	got := sampleExamples(examples, 1)
	if len(got) != 1 || got[0].MessageID != "m1" {
		t.Errorf("got %+v, want the single recurred example even though it's the oldest", got)
	}
}

// TestSampleExamples_ManualPrioritizedOverPassive checks Tier 2 vs Tier 3: with no
// recurrence in play, a manually-reviewed example wins over a passively-confirmed one even
// when the passive example is newer.
func TestSampleExamples_ManualPrioritizedOverPassive(t *testing.T) {
	examples := []db.PromptExample{
		{ID: 2, MessageID: "m2", Verdict: db.VerdictConfirmedPositive, Sender: "b@example.com", Subject: "s2", Source: db.ExampleSourcePassive},
		{ID: 1, MessageID: "m1", Verdict: db.VerdictConfirmedPositive, Sender: "a@example.com", Subject: "s1", Source: db.ExampleSourceManual},
	}
	got := sampleExamples(examples, 1)
	if len(got) != 1 || got[0].MessageID != "m1" {
		t.Errorf("got %+v, want the manual example even though the passive one is newer", got)
	}
}

// TestSampleExamples_RecurredBudgetLeavesRoomForOtherTiers checks that Tier 1 is capped
// (recurredBudget) rather than allowed to consume the whole per-verdict budget — a rule
// with many regressions should still show some non-regression signal.
func TestSampleExamples_RecurredBudgetLeavesRoomForOtherTiers(t *testing.T) {
	var examples []db.PromptExample
	for i := int64(1); i <= 4; i++ {
		examples = append(examples, db.PromptExample{
			ID: i, MessageID: fmt.Sprintf("r%d", i), Verdict: db.VerdictFalsePositive,
			Sender: fmt.Sprintf("r%d@example.com", i), Subject: "s", Recurred: true,
		})
	}
	examples = append(examples, db.PromptExample{
		ID: 5, MessageID: "m", Verdict: db.VerdictFalsePositive, Sender: "manual@example.com", Subject: "s", Source: db.ExampleSourceManual,
	})

	got := sampleExamples(examples, 4) // recurredBudget(4) == 2
	var recurredCount, manualCount int
	for _, ex := range got {
		if ex.Recurred {
			recurredCount++
		}
		if ex.Source == db.ExampleSourceManual {
			manualCount++
		}
	}
	if recurredCount != 2 {
		t.Errorf("recurredCount = %d, want 2 (capped at half the budget)", recurredCount)
	}
	if manualCount != 1 {
		t.Errorf("manualCount = %d, want 1 (the manual example should fill the leftover budget)", manualCount)
	}
}

// TestSampleExamples_IndependentPerVerdict checks a cap applies separately to each verdict
// — the same sender+subject legitimately appearing under two verdicts (e.g. wrongly caught
// once, later correctly matched) must not have one verdict's budget affect the other's.
func TestSampleExamples_IndependentPerVerdict(t *testing.T) {
	examples := []db.PromptExample{
		{ID: 2, MessageID: "m2", Verdict: db.VerdictConfirmedPositive, Sender: "a@example.com", Subject: "Your receipt"},
		{ID: 1, MessageID: "m1", Verdict: db.VerdictFalsePositive, Sender: "a@example.com", Subject: "Your receipt"},
	}
	got := sampleExamples(examples, 10)
	if len(got) != 2 {
		t.Fatalf("got %d examples, want 2 (one per verdict): %+v", len(got), got)
	}
}

func TestSampleExamples_ZeroCapReturnsNil(t *testing.T) {
	if got := sampleExamples([]db.PromptExample{{Verdict: db.VerdictFalsePositive}}, 0); got != nil {
		t.Errorf("sampleExamples with cap 0 = %+v, want nil", got)
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
