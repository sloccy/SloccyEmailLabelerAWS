package db

import (
	"testing"
	"time"
)

// TestClaimMessages_SecondClaimBlocked verifies the core dedup gate: once one caller
// claims a message, a second claim attempt for the same id must not win it — this is
// what stops two overlapping Lambda invocations from both paying for an LLM call on the
// same email.
func TestClaimMessages_SecondClaimBlocked(t *testing.T) {
	s := NewFake()
	accID, _ := s.UpsertAccount(t.Context(), UpsertAccountParams{Email: "claim@example.com"})

	first, err := s.ClaimMessages(t.Context(), accID, []string{"m1"})
	if err != nil {
		t.Fatalf("first ClaimMessages: %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("expected first claim to win m1, got %v", first)
	}

	second, err := s.ClaimMessages(t.Context(), accID, []string{"m1"})
	if err != nil {
		t.Fatalf("second ClaimMessages: %v", err)
	}
	if len(second) != 0 {
		t.Fatalf("expected second claim to be blocked, got %v", second)
	}
}

// TestClaimMessages_ReleaseAllowsReclaim verifies ReleaseClaim's purpose: after an LLM
// error, giving up a claim must make the message immediately claimable again instead of
// forcing every other caller to wait out the full lease.
func TestClaimMessages_ReleaseAllowsReclaim(t *testing.T) {
	s := NewFake()
	accID, _ := s.UpsertAccount(t.Context(), UpsertAccountParams{Email: "release@example.com"})

	if claimed, err := s.ClaimMessages(t.Context(), accID, []string{"m1"}); err != nil || len(claimed) != 1 {
		t.Fatalf("initial claim: claimed=%v err=%v", claimed, err)
	}
	if err := s.ReleaseClaim(t.Context(), accID, "m1"); err != nil {
		t.Fatalf("ReleaseClaim: %v", err)
	}

	claimed, err := s.ClaimMessages(t.Context(), accID, []string{"m1"})
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("expected m1 to be reclaimable after release, got %v", claimed)
	}
}

// TestReleaseClaim_CannotDeleteConfirmedMarker verifies the safety property that makes
// ReleaseClaim safe to call unconditionally on the LLM-error path: it must never remove a
// marker that BatchInsertProcessingResults already confirmed, even if a release for the
// same message id is (somehow) still in flight — otherwise a confirmed email could be
// reprocessed.
func TestReleaseClaim_CannotDeleteConfirmedMarker(t *testing.T) {
	s := NewFake()
	accID, _ := s.UpsertAccount(t.Context(), UpsertAccountParams{Email: "confirm@example.com"})

	if err := s.BatchInsertProcessingResults(t.Context(), nil, nil, nil, accID, "m1"); err != nil {
		t.Fatalf("BatchInsertProcessingResults: %v", err)
	}
	if err := s.ReleaseClaim(t.Context(), accID, "m1"); err != nil {
		t.Fatalf("ReleaseClaim: %v", err)
	}

	unprocessed, err := s.FilterUnprocessed(t.Context(), accID, []string{"m1"})
	if err != nil {
		t.Fatalf("FilterUnprocessed: %v", err)
	}
	if len(unprocessed) != 0 {
		t.Fatalf("expected m1 to remain confirmed after a no-op release, got unprocessed=%v", unprocessed)
	}
}

// TestClaimMessages_ExpiredLeaseIsReclaimable verifies the crash-recovery path: a claim
// whose owner never confirmed or released it (e.g. the Lambda was killed mid-classify)
// must become claimable again once its lease elapses, so the message isn't stuck forever.
func TestClaimMessages_ExpiredLeaseIsReclaimable(t *testing.T) {
	s := NewFake()
	accID, _ := s.UpsertAccount(t.Context(), UpsertAccountParams{Email: "expire@example.com"})

	if claimed, err := s.ClaimMessages(t.Context(), accID, []string{"m1"}); err != nil || len(claimed) != 1 {
		t.Fatalf("initial claim: claimed=%v err=%v", claimed, err)
	}
	// Simulate a lease that elapsed without confirmation or release (a crashed owner).
	s.processed[accID]["m1"] = time.Now().Add(-time.Hour)

	unprocessed, err := s.FilterUnprocessed(t.Context(), accID, []string{"m1"})
	if err != nil {
		t.Fatalf("FilterUnprocessed: %v", err)
	}
	if len(unprocessed) != 1 {
		t.Fatalf("expected m1 with an expired lease to read as unprocessed, got %v", unprocessed)
	}

	reclaimed, err := s.ClaimMessages(t.Context(), accID, []string{"m1"})
	if err != nil {
		t.Fatalf("reclaim after expiry: %v", err)
	}
	if len(reclaimed) != 1 {
		t.Fatalf("expected m1 to be reclaimable after its lease expired, got %v", reclaimed)
	}
}

// TestClaimPromptSuggestion_SecondClaimBlocked verifies the core dedup gate
// ClaimPromptSuggestion (improveRunner.runOne's caller, improve.go) relies on: the
// MODE=improve worker Lambda is invoked async (Event), which AWS automatically retries up
// to twice on error, so a second claim attempt for the same suggestion id must lose —
// otherwise a retry would redo (and re-bill) the same improve+replay round from scratch.
func TestClaimPromptSuggestion_SecondClaimBlocked(t *testing.T) {
	s := NewFake()
	sid, err := s.InsertPromptSuggestion(t.Context(), InsertPromptSuggestionParams{
		PromptID: 1, TriggerKind: TriggerKindFalseNegative, Status: SuggestionStatusGenerating,
	})
	if err != nil {
		t.Fatalf("InsertPromptSuggestion: %v", err)
	}

	first, err := s.ClaimPromptSuggestion(t.Context(), sid)
	if err != nil {
		t.Fatalf("first ClaimPromptSuggestion: %v", err)
	}
	if !first {
		t.Fatalf("expected first claim to win suggestion %d", sid)
	}

	second, err := s.ClaimPromptSuggestion(t.Context(), sid)
	if err != nil {
		t.Fatalf("second ClaimPromptSuggestion: %v", err)
	}
	if second {
		t.Fatalf("expected second claim on the same suggestion to be blocked")
	}
}

// TestClaimPromptSuggestion_DifferentSuggestionsIndependent checks that claiming one
// suggestion doesn't block a claim on a different one — the condition is scoped to a
// single item, not some shared state.
func TestClaimPromptSuggestion_DifferentSuggestionsIndependent(t *testing.T) {
	s := NewFake()
	sid1, _ := s.InsertPromptSuggestion(t.Context(), InsertPromptSuggestionParams{PromptID: 1, Status: SuggestionStatusGenerating})
	sid2, _ := s.InsertPromptSuggestion(t.Context(), InsertPromptSuggestionParams{PromptID: 2, Status: SuggestionStatusGenerating})

	if claimed, err := s.ClaimPromptSuggestion(t.Context(), sid1); err != nil || !claimed {
		t.Fatalf("claim sid1: claimed=%v err=%v", claimed, err)
	}
	if claimed, err := s.ClaimPromptSuggestion(t.Context(), sid2); err != nil || !claimed {
		t.Fatalf("claim sid2: claimed=%v err=%v", claimed, err)
	}
}
