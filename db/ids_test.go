package db

import (
	"testing"
	"time"
)

// TestLocalID_NeverZero guards the requireID/pathInt "0 = invalid/unset" sentinel used
// throughout server.go — localID must never produce it.
func TestLocalID_NeverZero(t *testing.T) {
	for range 10000 {
		if id := localID(); id == 0 {
			t.Fatal("localID returned 0")
		}
	}
}

// TestLocalID_UniqueAcrossRapidCalls exercises the per-process sequence counter that
// dedupes ids minted within the same millisecond.
func TestLocalID_UniqueAcrossRapidCalls(t *testing.T) {
	const n = 10000
	seen := make(map[int64]bool, n)
	for range n {
		id := localID()
		if seen[id] {
			t.Fatalf("duplicate id %d", id)
		}
		seen[id] = true
	}
}

// TestLocalIDs_ReturnsDistinctIDs mirrors how BatchInsertProcessingResults consumes
// localIDs: a batch of ids for one email's worth of log/history rows.
func TestLocalIDs_ReturnsDistinctIDs(t *testing.T) {
	ids := localIDs(50)
	if len(ids) != 50 {
		t.Fatalf("len = %d, want 50", len(ids))
	}
	seen := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if id == 0 {
			t.Fatal("localIDs produced a 0 id")
		}
		if seen[id] {
			t.Fatalf("duplicate id %d in batch", id)
		}
		seen[id] = true
	}
}

// TestLocalID_RoughlyTimeOrdered is a bonus property (not required for correctness here
// — logs/history are ordered by tsKey's timestamp prefix, not by id — but it's free and
// worth locking in since a regression would be a sign the generator is broken).
func TestLocalID_RoughlyTimeOrdered(t *testing.T) {
	first := localID()
	time.Sleep(5 * time.Millisecond)
	second := localID()
	if second <= first {
		t.Errorf("expected second id (%d) > first id (%d) after a sleep", second, first)
	}
}

func TestPkExample(t *testing.T) {
	if got, want := pkExample(5), "EXAMPLE#5"; got != want {
		t.Errorf("pkExample(5) = %q, want %q", got, want)
	}
}

// TestExampleSK_SortsNewestFirstWithinVerdict guards the ordering ListExamplesByVerdict
// depends on: a ScanIndexForward:false query over one verdict's SK range must return the
// newest example first, which requires SK to sort lexicographically by timestamp within a
// fixed verdict prefix.
func TestExampleSK_SortsNewestFirstWithinVerdict(t *testing.T) {
	older := exampleSK(VerdictFalsePositive, "2026-07-01 12:00:00", 1)
	newer := exampleSK(VerdictFalsePositive, "2026-07-02 12:00:00", 2)
	if older >= newer {
		t.Errorf("exampleSK ordering: older SK %q should sort before newer SK %q", older, newer)
	}
}

// TestExampleSK_TieBreaksByID guards the same-millisecond case: two examples inserted in
// the same InsertPromptExamples batch share a timestamp (Now() is called once per batch),
// so the padded id must be what keeps their SKs distinct and ordered.
func TestExampleSK_TieBreaksByID(t *testing.T) {
	ts := "2026-07-01 12:00:00"
	first := exampleSK(VerdictConfirmedPositive, ts, 1)
	second := exampleSK(VerdictConfirmedPositive, ts, 2)
	if first == second {
		t.Errorf("exampleSK produced identical SKs for different ids: %q", first)
	}
	if first >= second {
		t.Errorf("exampleSK tie-break ordering: id=1 SK %q should sort before id=2 SK %q", first, second)
	}
}
