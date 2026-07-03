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
