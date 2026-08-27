package main

import (
	"context"
	"fmt"
	"testing"

	"github.com/sloccy/ollamail-aws/db"
	"github.com/sloccy/ollamail-aws/llm"
)

// ============================================================
// parseExampleCap
// ============================================================

func TestParseExampleCap(t *testing.T) {
	const def, maxCap = 12, 20
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"typical value", "8", 8},
		{"minimum", "1", 1},
		{"at the cap", "20", 20},
		{"above the cap clamps down", "50", maxCap},
		{"zero falls back to default", "0", def},
		{"negative falls back to default", "-3", def},
		{"garbage falls back to default", "lots", def},
		{"empty falls back to default", "", def},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseExampleCap(c.in, def, maxCap); got != c.want {
				t.Errorf("parseExampleCap(%q, %d, %d) = %d, want %d", c.in, def, maxCap, got, c.want)
			}
		})
	}
}

// ============================================================
// markRecurrences
// ============================================================

func TestMarkRecurrences_SameMessageRecurs(t *testing.T) {
	resolvedBy := int64(42)
	examples := []db.PromptExample{
		{ID: 1, MessageID: "msg1", Verdict: db.VerdictConfirmedNegative, ResolvedBySuggestionID: &resolvedBy, PromptVersionID: 7},
		{ID: 2, MessageID: "msg1", Verdict: db.VerdictConfirmedNegative}, // live, same message+verdict -> recurred
	}
	got := markRecurrences(examples)

	live := got[1]
	if !live.Recurred {
		t.Errorf("expected the live row to be marked Recurred, got %+v", live)
	}
	if live.RecurredFromVersion != 7 {
		t.Errorf("RecurredFromVersion = %d, want 7 (the resolved row's PromptVersionID)", live.RecurredFromVersion)
	}
	if got[0].Recurred {
		t.Errorf("the resolved row itself must not be marked Recurred: %+v", got[0])
	}
}

func TestMarkRecurrences_DifferentVerdictNotRecurred(t *testing.T) {
	resolvedBy := int64(1)
	examples := []db.PromptExample{
		{ID: 1, MessageID: "msg1", Verdict: db.VerdictConfirmedNegative, ResolvedBySuggestionID: &resolvedBy},
		{ID: 2, MessageID: "msg1", Verdict: db.VerdictConfirmedPositive}, // same message, different verdict
	}
	got := markRecurrences(examples)
	if got[1].Recurred {
		t.Errorf("a different verdict on the same message must not count as a recurrence: %+v", got[1])
	}
}

func TestMarkRecurrences_NoResolvedHistoryNotRecurred(t *testing.T) {
	examples := []db.PromptExample{
		{ID: 1, MessageID: "msg1", Verdict: db.VerdictConfirmedNegative},
	}
	got := markRecurrences(examples)
	if got[0].Recurred {
		t.Errorf("a first-time problem with no resolved history must not be marked recurred: %+v", got[0])
	}
}

// TestMarkRecurrences_SenderSubjectFallback checks the secondary match path: a re-sent
// templated email can arrive with a new MessageID, so falling back to sender+subject still
// catches the recurrence.
func TestMarkRecurrences_SenderSubjectFallback(t *testing.T) {
	resolvedBy := int64(9)
	examples := []db.PromptExample{
		{ID: 1, MessageID: "msg-old", Sender: "Newsletter@Example.com", Subject: " Weekly Digest ",
			Verdict: db.VerdictConfirmedPositive, ResolvedBySuggestionID: &resolvedBy, PromptVersionID: 3},
		{ID: 2, MessageID: "msg-new", Sender: "newsletter@example.com", Subject: "weekly digest",
			Verdict: db.VerdictConfirmedPositive}, // different MessageID, same sender+subject case/whitespace-insensitive
	}
	got := markRecurrences(examples)
	if !got[1].Recurred || got[1].RecurredFromVersion != 3 {
		t.Errorf("expected the sender+subject fallback to catch this recurrence, got %+v", got[1])
	}
}

func TestMarkRecurrences_ResolvedRowsUnaffected(t *testing.T) {
	// filterResolved runs after markRecurrences and drops resolved rows regardless — this
	// just confirms markRecurrences doesn't itself alter which rows are resolved/live.
	resolvedBy := int64(1)
	examples := []db.PromptExample{
		{ID: 1, MessageID: "msg1", Verdict: db.VerdictConfirmedNegative, ResolvedBySuggestionID: &resolvedBy},
	}
	got := markRecurrences(examples)
	if len(got) != 1 || got[0].ResolvedBySuggestionID == nil {
		t.Errorf("markRecurrences must not drop or alter resolution state: %+v", got)
	}
}

// ============================================================
// attemptsForPrompt
// ============================================================

type fakeVersionLister struct {
	versions []db.PromptVersion
	err      error
}

func (f *fakeVersionLister) ListPromptVersions(_ context.Context, _ int64, _ int32) ([]db.PromptVersion, error) {
	return f.versions, f.err
}

func TestAttemptsForPrompt_ExcludesCurrentVersion(t *testing.T) {
	store := &fakeVersionLister{versions: []db.PromptVersion{
		{ID: 3, Instructions: "current text", ReplayPassed: 9, ReplayTotal: 10},
		{ID: 2, Instructions: "older text", ReplayPassed: 7, ReplayTotal: 10},
		{ID: 1, Instructions: "oldest text", ReplayPassed: 5, ReplayTotal: 10},
	}}
	p := db.Prompt{ID: 5, CurrentVersionID: 3}

	got := attemptsForPrompt(context.Background(), store, p)

	if len(got) != 2 {
		t.Fatalf("got %d attempts, want 2 (current version excluded): %+v", len(got), got)
	}
	for _, a := range got {
		if a.Instructions == "current text" {
			t.Errorf("the current version must not appear in PastAttempts: %+v", got)
		}
	}
}

func TestAttemptsForPrompt_MapsReplayEvidence(t *testing.T) {
	store := &fakeVersionLister{versions: []db.PromptVersion{
		{ID: 1, Instructions: "v1", ReplayPassed: 6, ReplayTotal: 8},
	}}
	got := attemptsForPrompt(context.Background(), store, db.Prompt{ID: 1, CurrentVersionID: 99})
	if len(got) != 1 || got[0].Passed != 6 || got[0].Total != 8 || got[0].Instructions != "v1" {
		t.Errorf("got %+v, want one attempt {v1, 6, 8}", got)
	}
}

func TestAttemptsForPrompt_LookupErrorReturnsNilNotPanic(t *testing.T) {
	store := &fakeVersionLister{err: context.DeadlineExceeded}
	got := attemptsForPrompt(context.Background(), store, db.Prompt{ID: 1})
	if got != nil {
		t.Errorf("expected nil attempts on a lookup error, got %+v", got)
	}
}

func TestAttemptsForPrompt_EmptyHistory(t *testing.T) {
	store := &fakeVersionLister{}
	got := attemptsForPrompt(context.Background(), store, db.Prompt{ID: 1})
	if len(got) != 0 {
		t.Errorf("expected no attempts for a prompt with no version history, got %+v", got)
	}
}

// TestAttemptsForPrompt_FeedsIntoImproveRequest is an integration check that the
// llm.AttemptRef shape attemptsForPrompt produces is exactly what buildImproveUserTurn
// (llm/bedrock.go) expects — a regression in either place would otherwise only surface as
// a silently-empty PAST ATTEMPTS section, not a compile error, since both sides just move
// plain structs.
func TestAttemptsForPrompt_FeedsIntoImproveRequest(t *testing.T) {
	store := &fakeVersionLister{versions: []db.PromptVersion{
		{ID: 1, Instructions: "Match newsletters.", ReplayPassed: 7, ReplayTotal: 10},
	}}
	attempts := attemptsForPrompt(context.Background(), store, db.Prompt{ID: 1, CurrentVersionID: 2})
	if len(attempts) != 1 {
		t.Fatalf("got %d attempts, want 1", len(attempts))
	}
	if attempts[0] != (llm.AttemptRef{Instructions: "Match newsletters.", Passed: 7, Total: 10}) {
		t.Errorf("got %+v, want the exact llm.AttemptRef shape", attempts[0])
	}
}

// ============================================================
// shouldPruneVerdict
// ============================================================

func TestShouldPruneVerdict(t *testing.T) {
	const verdictCap = 40
	cases := []struct {
		name  string
		count int64
		want  bool
	}{
		{"well below cap", 10, false},
		{"exactly at cap", 40, false},
		{"within the buffer above cap", 40 + pruneBuffer, false},
		{"just past the buffer", 40 + pruneBuffer + 1, true},
		{"well above cap", 500, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldPruneVerdict(c.count, verdictCap); got != c.want {
				t.Errorf("shouldPruneVerdict(%d, %d) = %v, want %v", c.count, verdictCap, got, c.want)
			}
		})
	}
}

// ============================================================
// pruneKeepSet
// ============================================================

// TestPruneKeepSet_LiveRankedLikeSelection checks the core "reverse of selection" property:
// a recurred live example wins a keep slot over more recent, non-recurred live examples —
// exactly the priority sampleVerdict already applies for selection (see
// TestSampleExamples_RecurredPrioritizedOverMissedAndConfirmed, recategorize_test.go), not
// re-tested in depth here.
func TestPruneKeepSet_LiveRankedLikeSelection(t *testing.T) {
	examples := []db.PromptExample{
		{ID: 3, MessageID: "m3", Sender: "c@example.com", Subject: "s3", Missed: true},
		{ID: 2, MessageID: "m2", Sender: "b@example.com", Subject: "s2"},
		{ID: 1, MessageID: "m1", Sender: "a@example.com", Subject: "s1", Recurred: true},
	}
	keep := pruneKeepSet(examples, 1)
	if len(keep) != 1 || !keep[1] {
		t.Errorf("keep = %v, want only ID 1 (the recurred example, even though it's the oldest)", keep)
	}
}

// TestPruneKeepSet_ResolvedKeptByRecencyIndependentOfLive checks that resolved examples get
// their own recency-only cap, separate from the live pool's tiered ranking — pruning a
// verdict with plenty of live examples must not starve the resolved pool, and vice versa.
func TestPruneKeepSet_ResolvedKeptByRecencyIndependentOfLive(t *testing.T) {
	resolvedBy := int64(1)
	var examples []db.PromptExample
	// 3 resolved rows, newest-first (ListExamplesByVerdict's contract) — cap of 2 should
	// keep only the newest 2.
	for i := int64(3); i >= 1; i-- {
		examples = append(examples, db.PromptExample{
			ID: i, MessageID: fmt.Sprintf("resolved%d", i), Sender: fmt.Sprintf("r%d@example.com", i), Subject: "s",
			ResolvedBySuggestionID: &resolvedBy,
		})
	}
	// 1 live example — well within its own cap of 2.
	examples = append(examples, db.PromptExample{ID: 10, MessageID: "live1", Sender: "live@example.com", Subject: "s"})

	keep := pruneKeepSet(examples, 2)
	if keep[1] {
		t.Errorf("keep = %v, want the oldest resolved example (ID 1) pruned (cap 2, 3 resolved rows)", keep)
	}
	if !keep[2] || !keep[3] {
		t.Errorf("keep = %v, want the two newest resolved examples (IDs 2, 3) kept", keep)
	}
	if !keep[10] {
		t.Errorf("keep = %v, want the live example kept — it must not compete with the resolved pool for budget", keep)
	}
}

// TestPruneKeepSet_ResolvedWithinCapPreservesRecurrenceDetection checks the reason resolved
// rows are kept at all: a resolved row within its own cap must still let markRecurrences flag
// a live row with the same message+verdict as Recurred, so [RECURRED] detection keeps working
// after a prune pass, not just before one.
func TestPruneKeepSet_ResolvedWithinCapPreservesRecurrenceDetection(t *testing.T) {
	resolvedBy := int64(9)
	examples := []db.PromptExample{
		{ID: 2, MessageID: "msg1", Verdict: db.VerdictConfirmedNegative, Sender: "a@example.com", Subject: "s", ResolvedBySuggestionID: &resolvedBy, PromptVersionID: 5},
		{ID: 1, MessageID: "msg1", Verdict: db.VerdictConfirmedNegative, Sender: "a@example.com", Subject: "s"},
	}
	pruneKeepSet(examples, 10) // mutates examples in place via markRecurrences, same as gatherRawExamples does
	var live db.PromptExample
	for _, ex := range examples {
		if ex.ID == 1 {
			live = ex
		}
	}
	if !live.Recurred || live.RecurredFromVersion != 5 {
		t.Errorf("live example = %+v, want Recurred=true RecurredFromVersion=5 after pruneKeepSet ran markRecurrences", live)
	}
}

// TestPruneKeepSet_EveryKeptIDIsFromInput is a sanity check against a bookkeeping bug: the
// keep set must never reference an ID that wasn't actually in the input, and must never
// exceed 2x cap (cap for live + cap for resolved).
func TestPruneKeepSet_EveryKeptIDIsFromInput(t *testing.T) {
	resolvedBy := int64(1)
	var examples []db.PromptExample
	for i := int64(1); i <= 20; i++ {
		ex := db.PromptExample{ID: i, MessageID: fmt.Sprintf("m%d", i), Sender: fmt.Sprintf("s%d@example.com", i), Subject: "s"}
		if i%3 == 0 {
			ex.ResolvedBySuggestionID = &resolvedBy
		}
		examples = append(examples, ex)
	}
	valid := make(map[int64]bool, len(examples))
	for _, ex := range examples {
		valid[ex.ID] = true
	}

	keep := pruneKeepSet(examples, 5)
	if len(keep) > 10 {
		t.Errorf("len(keep) = %d, want at most 10 (cap 5 for live + cap 5 for resolved)", len(keep))
	}
	for id := range keep {
		if !valid[id] {
			t.Errorf("keep set references ID %d, which was never in the input", id)
		}
	}
}
