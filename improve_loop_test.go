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
		name             string
		rounds           []db.SuggestionRoundSummary
		heldOutSubmitted int64
		want             int
	}{
		{"empty", nil, 0, -1},
		{"single round", []db.SuggestionRoundSummary{{N: 1, Passed: 5, Total: 10}}, 0, 0},
		{"strictly increasing pass rate: last wins", []db.SuggestionRoundSummary{
			{N: 1, Passed: 5, Total: 10}, {N: 2, Passed: 8, Total: 10}, {N: 3, Passed: 10, Total: 10},
		}, 0, 2},
		{"a later round regresses: earlier best wins", []db.SuggestionRoundSummary{
			{N: 1, Passed: 9, Total: 10}, {N: 2, Passed: 6, Total: 10},
		}, 0, 0},
		{"tie on rate keeps the earlier round", []db.SuggestionRoundSummary{
			{N: 1, Passed: 7, Total: 10}, {N: 2, Passed: 7, Total: 10},
		}, 0, 0},
		{"tie then a real improvement", []db.SuggestionRoundSummary{
			{N: 1, Passed: 7, Total: 10}, {N: 2, Passed: 7, Total: 10}, {N: 3, Passed: 9, Total: 10},
		}, 0, 2},
		{"a round with inadequate coverage loses to one with adequate coverage, even at a lower raw Passed", []db.SuggestionRoundSummary{
			{N: 1, Passed: 20, Total: 20, Errored: 20}, // 20/40 submitted scored = 50% coverage, below the 0.6 floor
			{N: 2, Passed: 3, Total: 3},                // 3/3 submitted scored = 100% coverage
		}, 0, 1},
		{"neither round has adequate coverage: falls back to rate, then earlier", []db.SuggestionRoundSummary{
			{N: 1, Passed: 1, Total: 2, Errored: 20},
			{N: 2, Passed: 1, Total: 2, Errored: 20},
		}, 0, 0},
		{"equal overall rate but round 2's held-out rate is worse: round 1 wins on held-out", []db.SuggestionRoundSummary{
			{N: 1, Passed: 8, Total: 10, HeldOutPassed: 5, HeldOutTotal: 5},
			{N: 2, Passed: 8, Total: 10, HeldOutPassed: 1, HeldOutTotal: 5},
		}, 5, 0},
		{"held-out coverage inadequate for one round: falls back to overall rate", []db.SuggestionRoundSummary{
			{N: 1, Passed: 8, Total: 10, HeldOutPassed: 1, HeldOutTotal: 1}, // held-out coverage 1/5 = 20%, below floor
			{N: 2, Passed: 9, Total: 10, HeldOutPassed: 4, HeldOutTotal: 5},
		}, 5, 1},
		{"equal held-out rate: shorter candidate wins", []db.SuggestionRoundSummary{
			{N: 1, Passed: 8, Total: 10, HeldOutPassed: 4, HeldOutTotal: 5, Candidate: "a much longer rewritten rule than the other one"},
			{N: 2, Passed: 8, Total: 10, HeldOutPassed: 4, HeldOutTotal: 5, Candidate: "short rule"},
		}, 5, 1},
		{"equal rate and equal length: keeps the earlier round", []db.SuggestionRoundSummary{
			{N: 1, Passed: 8, Total: 10, Candidate: "same length"},
			{N: 2, Passed: 8, Total: 10, Candidate: "same length"},
		}, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := selectBestRound(c.rounds, c.heldOutSubmitted); got != c.want {
				t.Errorf("selectBestRound(%v, %d) = %d, want %d", c.rounds, c.heldOutSubmitted, got, c.want)
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
	// lowCoverage mirrors the production bug this fixes: a "perfect" 3/3 that's actually
	// 3 of 26 submitted examples, the other 23 having errored out under an unbounded
	// classify fan-out — see minReplayCoverage's doc comment.
	lowCoverage := llm.ReplayResult{Total: 3, Passed: 3, Errored: 23}

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
		{"3/3 out of 26 submitted is not a perfect score at low coverage", 1, 3, true, lowCoverage, true, true, false, false},
		{"a low-coverage round can still exhaust the round budget", 3, 3, true, lowCoverage, true, true, true, false},
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
			{Verdict: db.VerdictConfirmedNegative, Got: true, ExampleIndex: 0},  // wrongly matched
			{Verdict: db.VerdictConfirmedPositive, Got: false, ExampleIndex: 1}, // wrongly missed
		},
	}

	turn := buildReplayFeedbackTurn("a generic candidate rule", replay, examples, nil)

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

func TestBuildReplayFeedbackTurn_HeldOutScoreShown(t *testing.T) {
	replay := llm.ReplayResult{Total: 10, Passed: 8, HeldOutTotal: 4, HeldOutPassed: 2}
	turn := buildReplayFeedbackTurn("a generic candidate rule", replay, nil, nil)
	if !strings.Contains(turn, "2/4") {
		t.Errorf("feedback turn missing the held-out score: %s", turn)
	}
}

func TestBuildReplayFeedbackTurn_CitesExamplesNoted(t *testing.T) {
	replayLLMExamples := []llm.ReplayExample{
		{Sender: "notifications@github.com", Subject: "PR merged"},
	}
	turn := buildReplayFeedbackTurn("Email is from github and mentions a pull request", llm.ReplayResult{Total: 1, Passed: 0}, nil, replayLLMExamples)
	if !strings.Contains(turn, "github") {
		t.Errorf("expected the cited example domain to be called out: %s", turn)
	}
}

func TestBuildReplayFeedbackTurn_NoFailuresOmitsBothGroups(t *testing.T) {
	// Shouldn't normally be called with zero failures (the loop stops on a perfect score
	// before ever building a feedback turn), but must degrade sanely rather than panic or
	// print empty headers if it ever is.
	turn := buildReplayFeedbackTurn("a rule", llm.ReplayResult{Total: 5, Passed: 5}, nil, nil)
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
	turn := buildReplayFeedbackTurn("a rule", replay, examples, nil) // must not panic
	if strings.Contains(turn, "a@example.com") {
		t.Errorf("expected the out-of-range failures to be skipped, not mapped to an unrelated example: %s", turn)
	}
}

// TestBuildReplayFeedbackTurn_HeldOutFailuresNeverShowContent guards the whole reason
// buildReplayFeedbackTurn splits by HeldOut: selectBestRound ranks a round on its held-out
// pass rate precisely because it can't be satisfied by enumerating the examples the model
// was shown — if a held-out failure's sender/subject/excerpt ever leaked into the feedback
// turn, the very next round's "held-out" score would actually be measuring an example the
// model had just been handed.
func TestBuildReplayFeedbackTurn_HeldOutFailuresNeverShowContent(t *testing.T) {
	examples := []db.PromptExample{
		{Sender: "shown@example.com", Subject: "Shown failure", BodyExcerpt: "shown body"},
		{Sender: "secret@example.com", Subject: "Held-out failure", BodyExcerpt: "held-out body"},
	}
	replayLLMExamples := []llm.ReplayExample{
		{HeldOut: false},
		{HeldOut: true},
	}
	replay := llm.ReplayResult{
		Total: 5, Passed: 3, HeldOutTotal: 1,
		Failures: []llm.ReplayFailure{
			{Verdict: db.VerdictConfirmedNegative, Got: true, ExampleIndex: 0},  // shown, wrongly matched
			{Verdict: db.VerdictConfirmedPositive, Got: false, ExampleIndex: 1}, // held-out, wrongly missed
		},
	}

	turn := buildReplayFeedbackTurn("a generic candidate rule", replay, examples, replayLLMExamples)

	if strings.Contains(turn, "secret@example.com") || strings.Contains(turn, "Held-out failure") || strings.Contains(turn, "held-out body") {
		t.Errorf("held-out failure's content leaked into the feedback turn: %s", turn)
	}
	if !strings.Contains(turn, "shown@example.com") || !strings.Contains(turn, "shown body") {
		t.Errorf("shown failure's content missing from the feedback turn: %s", turn)
	}
	if !strings.Contains(turn, "1 of the 1 emails you weren't shown") {
		t.Errorf("expected a count-only held-out summary line: %s", turn)
	}
}

// ============================================================
// roundBetter / balanced accuracy
// ============================================================

// TestRoundBetter_BalancedAccuracyBeatsMatchEverything guards the fix for a lopsided corpus
// (mostly confirmed_positive, a handful of confirmed_negative — see
// db.VerdictConfirmedNegative's doc comment): round A matches everything, scoring perfectly
// on the large positive bucket and failing every negative; round B is slightly worse on raw
// accuracy but actually distinguishes the two. Balanced accuracy must prefer B even though
// raw accuracy would have picked A.
func TestRoundBetter_BalancedAccuracyBeatsMatchEverything(t *testing.T) {
	matchEverything := db.SuggestionRoundSummary{
		N: 1, Candidate: "match everything",
		Passed: 18, Total: 20, PosTotal: 18, PosPassed: 18, NegTotal: 2, NegPassed: 0,
	}
	discriminates := db.SuggestionRoundSummary{
		N: 2, Candidate: "discriminates",
		Passed: 17, Total: 20, PosTotal: 18, PosPassed: 16, NegTotal: 2, NegPassed: 1,
	}
	if matchEverything.Passed <= discriminates.Passed {
		// sanity check on the fixture: raw accuracy alone would pick the wrong round
		t.Fatalf("fixture bug: expected matchEverything's raw Passed to exceed discriminates'")
	}
	if !roundBetter(discriminates, matchEverything, 0) {
		t.Error("expected the discriminating round to win on balanced accuracy despite a lower raw pass rate")
	}
	if roundBetter(matchEverything, discriminates, 0) {
		t.Error("expected the match-everything round to lose on balanced accuracy")
	}
}

// TestRoundBetter_OneEmptyBucketFallsBackToRawRate checks that an empty bucket (Balanced
// returns -1, llm.ClassCounts) doesn't crash the comparison or wrongly declare a winner —
// it just falls through to raw pass rate, same as before balanced accuracy existed.
func TestRoundBetter_OneEmptyBucketFallsBackToRawRate(t *testing.T) {
	a := db.SuggestionRoundSummary{N: 1, Passed: 8, Total: 10, PosTotal: 10, PosPassed: 8} // NegTotal 0
	b := db.SuggestionRoundSummary{N: 2, Passed: 6, Total: 10, PosTotal: 10, PosPassed: 6}
	if !roundBetter(a, b, 0) {
		t.Error("expected a (higher raw pass rate) to win when neither round has negative-bucket evidence")
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

// ============================================================
// replayExamplesFor
// ============================================================

// TestReplayExamplesFor_HeldOutMarking checks the HeldOut/WasCorrect split
// ReplayAgainstExamples' held-out scoring depends on: an example is HeldOut when its
// MessageID doesn't appear in the "shown" set (the corpus the improve prompt itself saw),
// and WasCorrect is the inverse of Missed (not of Verdict) — see
// llm.ReplayExample.WasCorrect's doc comment for why a confirmed_negative example the rule
// already correctly left unmatched is just as "correct" as a confirmed_positive one it
// already matched.
func TestReplayExamplesFor_HeldOutMarking(t *testing.T) {
	shown := []db.PromptExample{
		{MessageID: "m1", Verdict: db.VerdictConfirmedPositive, Missed: true},
	}
	all := []db.PromptExample{
		{MessageID: "m1", Verdict: db.VerdictConfirmedPositive, Missed: true},  // shown -> not held out; Missed -> not WasCorrect
		{MessageID: "m2", Verdict: db.VerdictConfirmedNegative, Missed: true},  // not shown -> held out; Missed -> not WasCorrect
		{MessageID: "m3", Verdict: db.VerdictConfirmedPositive, Missed: false}, // not shown -> held out; not Missed -> WasCorrect
	}
	out := replayExamplesFor(all, shown)
	if len(out) != 3 {
		t.Fatalf("len(out) = %d, want 3", len(out))
	}
	if out[0].HeldOut {
		t.Errorf("m1 was shown to the improve prompt, want HeldOut=false")
	}
	if !out[1].HeldOut {
		t.Errorf("m2 was not shown, want HeldOut=true")
	}
	if !out[2].HeldOut {
		t.Errorf("m3 was not shown, want HeldOut=true")
	}
	if out[0].WasCorrect || out[1].WasCorrect {
		t.Errorf("a Missed example must never be WasCorrect, regardless of verdict")
	}
	if !out[2].WasCorrect {
		t.Errorf("a non-Missed example must be WasCorrect")
	}
	if !out[0].Want || !out[2].Want {
		t.Errorf("confirmed_positive examples must have Want=true")
	}
	if out[1].Want {
		t.Errorf("confirmed_negative example must have Want=false")
	}
}
