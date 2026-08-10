package main

import (
	"context"
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
		{ID: 1, MessageID: "msg1", Verdict: db.VerdictFalsePositive, ResolvedBySuggestionID: &resolvedBy, PromptVersionID: 7},
		{ID: 2, MessageID: "msg1", Verdict: db.VerdictFalsePositive}, // live, same message+verdict -> recurred
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
		{ID: 1, MessageID: "msg1", Verdict: db.VerdictFalsePositive, ResolvedBySuggestionID: &resolvedBy},
		{ID: 2, MessageID: "msg1", Verdict: db.VerdictFalseNegative}, // same message, different verdict
	}
	got := markRecurrences(examples)
	if got[1].Recurred {
		t.Errorf("a different verdict on the same message must not count as a recurrence: %+v", got[1])
	}
}

func TestMarkRecurrences_NoResolvedHistoryNotRecurred(t *testing.T) {
	examples := []db.PromptExample{
		{ID: 1, MessageID: "msg1", Verdict: db.VerdictFalsePositive},
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
			Verdict: db.VerdictFalseNegative, ResolvedBySuggestionID: &resolvedBy, PromptVersionID: 3},
		{ID: 2, MessageID: "msg-new", Sender: "newsletter@example.com", Subject: "weekly digest",
			Verdict: db.VerdictFalseNegative}, // different MessageID, same sender+subject case/whitespace-insensitive
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
		{ID: 1, MessageID: "msg1", Verdict: db.VerdictFalsePositive, ResolvedBySuggestionID: &resolvedBy},
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
