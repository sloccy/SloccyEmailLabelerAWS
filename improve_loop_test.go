package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sloccy/ollamail-aws/db"
	"github.com/sloccy/ollamail-aws/llm"
)

// ============================================================
// parseImproveMaxRounds
// ============================================================

func TestParseImproveMaxRounds(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"typical value", "3", 3},
		{"minimum", "1", 1},
		{"at the cap", "5", 5},
		{"above the cap clamps down", "9", llm.ImproveMaxRoundsCap},
		{"zero falls back to default", "0", llm.ImproveMaxRoundsDefault},
		{"negative falls back to default", "-2", llm.ImproveMaxRoundsDefault},
		{"garbage falls back to default", "not-a-number", llm.ImproveMaxRoundsDefault},
		{"float falls back to default", "2.5", llm.ImproveMaxRoundsDefault},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseImproveMaxRounds(c.in); got != c.want {
				t.Errorf("parseImproveMaxRounds(%q) = %d, want %d", c.in, got, c.want)
			}
		})
	}
}

// ============================================================
// roundFitsDeadline / hasTimeForAnotherRound
// ============================================================

func TestRoundFitsDeadline(t *testing.T) {
	cases := []struct {
		name      string
		remaining time.Duration
		lastRound time.Duration
		want      bool
	}{
		{"plenty of room", 10 * time.Minute, 30 * time.Second, true},
		{"exactly at the margin: not enough", 30*time.Second*3/2 + improveWorkerMargin, 30 * time.Second, false},
		{"just over the margin: enough", 30*time.Second*3/2 + improveWorkerMargin + time.Second, 30 * time.Second, true},
		{"last round took a while, little left", 45 * time.Second, 40 * time.Second, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := roundFitsDeadline(c.remaining, c.lastRound); got != c.want {
				t.Errorf("roundFitsDeadline(%v, %v) = %v, want %v", c.remaining, c.lastRound, got, c.want)
			}
		})
	}
}

func TestHasTimeForAnotherRound_NoDeadlineAlwaysTrue(t *testing.T) {
	// Local dev / the in-process fallback path (dispatchImprove) runs off a context with
	// no deadline at all — nothing to run out of, so every round should be allowed.
	ctx := context.Background()
	if !hasTimeForAnotherRound(ctx, 10*time.Hour) {
		t.Error("hasTimeForAnotherRound with no ctx deadline = false, want true regardless of lastRound")
	}
}

func TestHasTimeForAnotherRound_RespectsDeadline(t *testing.T) {
	// The threshold is lastRound*1.5 + improveWorkerMargin (20s) — the margin dominates
	// for a short lastRound, so the deadline here has to clear 20s+ to read as "plenty."
	ctx, cancel := context.WithTimeout(context.Background(), improveWorkerMargin+time.Minute)
	defer cancel()
	if !hasTimeForAnotherRound(ctx, time.Millisecond) {
		t.Error("expected true: well over a minute past the margin remains against a 1ms last round")
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer cancel2()
	time.Sleep(2 * time.Millisecond) // let the deadline actually pass
	if hasTimeForAnotherRound(ctx2, time.Millisecond) {
		t.Error("expected false: the deadline has already passed")
	}
}

// ============================================================
// selectBestRound
// ============================================================

func TestSelectBestRound(t *testing.T) {
	cases := []struct {
		name   string
		rounds []db.SuggestionRoundSummary
		want   int
	}{
		{"empty", nil, -1},
		{"single round", []db.SuggestionRoundSummary{{N: 1, Passed: 5}}, 0},
		{"strictly increasing: last wins", []db.SuggestionRoundSummary{{N: 1, Passed: 5}, {N: 2, Passed: 8}, {N: 3, Passed: 10}}, 2},
		{"a later round regresses: earlier best wins", []db.SuggestionRoundSummary{{N: 1, Passed: 9}, {N: 2, Passed: 6}}, 0},
		{"tie keeps the earlier round", []db.SuggestionRoundSummary{{N: 1, Passed: 7}, {N: 2, Passed: 7}}, 0},
		{"tie then a real improvement", []db.SuggestionRoundSummary{{N: 1, Passed: 7}, {N: 2, Passed: 7}, {N: 3, Passed: 9}}, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := selectBestRound(c.rounds); got != c.want {
				t.Errorf("selectBestRound(%v) = %d, want %d", c.rounds, got, c.want)
			}
		})
	}
}

// ============================================================
// improveLoopStop
// ============================================================

func TestImproveLoopStop(t *testing.T) {
	perfect := llm.ReplayResult{Total: 10, Passed: 10}
	partial := llm.ReplayResult{Total: 10, Passed: 7}
	noEvidence := llm.ReplayResult{Total: 0, Passed: 0}

	cases := []struct {
		name               string
		n, maxRounds       int
		replayOn           bool
		replay             llm.ReplayResult
		improved, timeLeft bool
		wantStop           bool
		wantReasonNonEmpty bool
	}{
		{"replay disabled always stops", 1, 3, false, partial, true, true, true, false},
		{"perfect score stops even mid-budget", 1, 3, true, perfect, true, true, true, true},
		{"round 1 partial score with budget and time left continues", 1, 3, true, partial, true, true, false, false},
		{"budget exhausted stops silently", 3, 3, true, partial, true, true, true, false},
		{"round 1 'not improved' is not a stop reason (nothing to compare against yet)", 1, 3, true, partial, false, true, false, false},
		{"round 2+ no improvement stops", 2, 3, true, partial, false, true, true, true},
		{"round 2+ improved keeps going", 2, 3, true, partial, true, true, false, false},
		{"out of time stops even if improved", 2, 3, true, partial, true, false, true, true},
		{"zero total (no examples) with a perfect-shaped 0/0 does not falsely stop as perfect", 1, 3, true, noEvidence, true, true, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stop, reason := improveLoopStop(c.n, c.maxRounds, c.replayOn, c.replay, c.improved, c.timeLeft)
			if stop != c.wantStop {
				t.Errorf("stop = %v, want %v (reason=%q)", stop, c.wantStop, reason)
			}
			if c.wantReasonNonEmpty && reason == "" {
				t.Errorf("expected a non-empty trace note, got none")
			}
			if !c.wantReasonNonEmpty && reason != "" {
				t.Errorf("expected no trace note, got %q", reason)
			}
		})
	}
}

// ============================================================
// buildReplayFeedbackTurn
// ============================================================

func TestBuildReplayFeedbackTurn(t *testing.T) {
	examples := []db.PromptExample{
		{Sender: "a@example.com", Subject: "Newsletter A", BodyExcerpt: "weekly digest content"},
		{Sender: "b@example.com", Subject: "Order #123", BodyExcerpt: "your order has shipped"},
		{Sender: "c@example.com", Subject: "Promo blast", BodyExcerpt: "50% off everything"},
	}
	replay := llm.ReplayResult{
		Total: 5, Passed: 3,
		Failures: []llm.ReplayFailure{
			{Verdict: db.VerdictFalsePositive, Got: true, ExampleIndex: 0},  // wrongly matched
			{Verdict: db.VerdictFalseNegative, Got: false, ExampleIndex: 1}, // wrongly missed
		},
	}

	turn := buildReplayFeedbackTurn(replay, examples)

	if !strings.Contains(turn, "3/5") {
		t.Errorf("feedback turn missing the score: %s", turn)
	}
	if !strings.Contains(turn, "WRONGLY CAUGHT") || !strings.Contains(turn, "a@example.com") || !strings.Contains(turn, "weekly digest content") {
		t.Errorf("feedback turn missing the wrongly-matched example's body: %s", turn)
	}
	if !strings.Contains(turn, "MISSED") || !strings.Contains(turn, "b@example.com") || !strings.Contains(turn, "your order has shipped") {
		t.Errorf("feedback turn missing the wrongly-missed example's body: %s", turn)
	}
	if strings.Contains(turn, "c@example.com") {
		t.Errorf("feedback turn mentions an example that wasn't a failure: %s", turn)
	}
}

func TestBuildReplayFeedbackTurn_NoFailuresOmitsBothGroups(t *testing.T) {
	// Shouldn't normally be called with zero failures (the loop stops on a perfect score
	// before ever building a feedback turn), but must degrade sanely rather than panic or
	// print empty headers if it ever is.
	turn := buildReplayFeedbackTurn(llm.ReplayResult{Total: 5, Passed: 5}, nil)
	if strings.Contains(turn, "WRONGLY CAUGHT") || strings.Contains(turn, "MISSED") {
		t.Errorf("expected no failure group headers with zero failures: %s", turn)
	}
}

func TestBuildReplayFeedbackTurn_OutOfRangeIndexSkippedNotPanicked(t *testing.T) {
	// Defensive: ExampleIndex is meaningful only within the same process/call that ran the
	// replay (see its doc comment) — a caller bug that mismatches examples and failures
	// must not panic the whole improve round over a display detail.
	examples := []db.PromptExample{{Sender: "a@example.com", Subject: "s"}}
	replay := llm.ReplayResult{Total: 2, Passed: 0, Failures: []llm.ReplayFailure{
		{Got: true, ExampleIndex: 5},
		{Got: false, ExampleIndex: -1},
	}}
	turn := buildReplayFeedbackTurn(replay, examples) // must not panic
	if strings.Contains(turn, "a@example.com") {
		t.Errorf("expected the out-of-range failures to be skipped, not mapped to an unrelated example: %s", turn)
	}
}

// ============================================================
// terminalWriteCtx
// ============================================================

// TestTerminalWriteCtx_DetachedFromParentCancellation guards the reason this exists: a
// terminal status write (finalizeFailure / FinalizePromptSuggestion) can be reached exactly
// because the round's own ctx already expired or was cancelled — e.g. llm's stall guard
// firing (see llm.errImproveStalled) after running the round's whole budget — so the write
// itself must not inherit that cancellation, or it would fail before ever landing the
// status the suggestion needs to leave "generating".
func TestTerminalWriteCtx_DetachedFromParentCancellation(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	cancel() // simulate an already-expired/cancelled round ctx

	writeCtx, writeCancel := terminalWriteCtx(parent)
	defer writeCancel()

	select {
	case <-writeCtx.Done():
		t.Fatal("terminalWriteCtx's context is already done — it inherited the parent's cancellation instead of detaching from it")
	default:
	}
}

// TestTerminalWriteCtx_HasABoundedDeadline checks the write still can't hang forever even
// though it's detached from the parent — same reasoning writeFailure's doc comment already
// gave for the 10s budget it used inline before this helper existed.
func TestTerminalWriteCtx_HasABoundedDeadline(t *testing.T) {
	writeCtx, cancel := terminalWriteCtx(context.Background())
	defer cancel()

	dl, ok := writeCtx.Deadline()
	if !ok {
		t.Fatal("terminalWriteCtx's context has no deadline — a terminal write must still be bounded")
	}
	if remaining := time.Until(dl); remaining <= 0 || remaining > 10*time.Second {
		t.Errorf("terminalWriteCtx deadline = %v from now, want (0, 10s]", remaining)
	}
}
