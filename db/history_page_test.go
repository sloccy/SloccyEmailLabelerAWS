package db

import (
	"fmt"
	"testing"
)

// seedHistoryRow appends a history row directly to a FakeStore, bypassing
// BatchInsertProcessingResults (whose Timestamp is always Now()) so tests can control
// exact timestamps and ids — needed to build interleaved, multi-account fixtures for
// cursor-pagination tests.
func seedHistoryRow(s *FakeStore, id, accountID int64, ts, subject, sender string, promptID *int64) {
	s.history = append(s.history, &CategorizationHistory{
		ID:        id,
		Timestamp: ts,
		AccountID: accountID,
		Subject:   subject,
		Sender:    sender,
		PromptID:  promptID,
	})
}

// TestGetHistoryFiltered_CursorWalk_NoDuplicatesOrGaps pages through a full account's
// history with a page size much smaller than the dataset, then verifies the concatenated
// pages reproduce the exact same set, in the exact same order, as one unpaginated call —
// the core cursor-correctness property the infinite-scroll UI depends on.
func TestGetHistoryFiltered_CursorWalk_NoDuplicatesOrGaps(t *testing.T) {
	s := NewFake()
	accID, _ := s.UpsertAccount(t.Context(), UpsertAccountParams{Email: "walk@example.com"})

	const n = 27
	for i := int64(0); i < n; i++ {
		// Zero-padded seconds so lexicographic == chronological order, matching tsKey.
		ts := "2026-08-01 00:00:" + padSeconds(i)
		seedHistoryRow(s, i+1, accID, ts, "subject", "sender@example.com", nil)
	}

	var walked []CategorizationHistory
	cursor := ""
	pages := 0
	for {
		pages++
		if pages > n+2 {
			t.Fatalf("pagination did not terminate after %d pages", pages)
		}
		page, err := s.GetHistoryFiltered(t.Context(), HistoryFilter{AccountID: &accID, Limit: 5, Cursor: cursor})
		if err != nil {
			t.Fatalf("GetHistoryFiltered: %v", err)
		}
		walked = append(walked, page.Rows...)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}

	full, err := s.GetHistoryFiltered(t.Context(), HistoryFilter{AccountID: &accID, Limit: n})
	if err != nil {
		t.Fatalf("GetHistoryFiltered (unpaginated): %v", err)
	}
	if full.NextCursor != "" {
		t.Fatalf("unpaginated call with Limit=%d unexpectedly left more data (NextCursor=%q)", n, full.NextCursor)
	}
	if len(walked) != len(full.Rows) {
		t.Fatalf("walked %d rows across %d pages, want %d (one unpaginated call)", len(walked), pages, len(full.Rows))
	}
	seen := make(map[int64]bool, len(walked))
	for i, h := range walked {
		if seen[h.ID] {
			t.Fatalf("row id %d returned more than once across pages", h.ID)
		}
		seen[h.ID] = true
		if h.ID != full.Rows[i].ID {
			t.Fatalf("row %d: paginated id %d != unpaginated id %d — order diverged", i, h.ID, full.Rows[i].ID)
		}
	}
}

// TestGetHistoryFiltered_SparseFilterTerminates covers the short/empty-page path: a
// subject filter that only matches two rows out of a much larger, mostly-non-matching
// dataset. Because SubjectQ is applied in Go after each account's DynamoDB Query (see
// GetHistoryFiltered's doc comment), most pages here return zero matches — the cursor
// must still advance every time so the walk terminates instead of looping forever.
func TestGetHistoryFiltered_SparseFilterTerminates(t *testing.T) {
	s := NewFake()
	accID, _ := s.UpsertAccount(t.Context(), UpsertAccountParams{Email: "sparse@example.com"})

	const n = 40
	for i := int64(0); i < n; i++ {
		ts := "2026-08-01 00:01:" + padSeconds(i%60)
		subject := "newsletter digest"
		if i == 3 || i == 35 { // sparse matches, spread across the dataset
			subject = "Special Offer Inside"
		}
		seedHistoryRow(s, i+1, accID, ts, subject, "sender@example.com", nil)
	}

	var matched []CategorizationHistory
	cursor := ""
	pages := 0
	for {
		pages++
		if pages > n+2 {
			t.Fatalf("pagination did not terminate after %d pages", pages)
		}
		page, err := s.GetHistoryFiltered(t.Context(), HistoryFilter{
			AccountID: &accID, Limit: 5, Cursor: cursor, SubjectQ: "special offer",
		})
		if err != nil {
			t.Fatalf("GetHistoryFiltered: %v", err)
		}
		matched = append(matched, page.Rows...)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}

	if len(matched) != 2 {
		t.Fatalf("expected exactly 2 matches, got %d: %+v", len(matched), matched)
	}
	if pages < 2 {
		t.Errorf("expected the sparse filter to take multiple short/empty pages, took %d", pages)
	}
}

// TestGetHistoryFiltered_MultiAccountMergeOrder verifies that with no AccountID filter,
// rows from different accounts interleave by timestamp rather than being grouped by
// account — required for the single global cursor to stay valid across every partition.
func TestGetHistoryFiltered_MultiAccountMergeOrder(t *testing.T) {
	s := NewFake()
	acc1, _ := s.UpsertAccount(t.Context(), UpsertAccountParams{Email: "acc1@example.com"})
	acc2, _ := s.UpsertAccount(t.Context(), UpsertAccountParams{Email: "acc2@example.com"})

	// Interleaved: acc2's row sits chronologically between acc1's two rows.
	seedHistoryRow(s, 1, acc1, "2026-08-01 00:00:03", "a-newest", "s", nil)
	seedHistoryRow(s, 2, acc2, "2026-08-01 00:00:02", "b-middle", "s", nil)
	seedHistoryRow(s, 3, acc1, "2026-08-01 00:00:01", "a-oldest", "s", nil)

	page, err := s.GetHistoryFiltered(t.Context(), HistoryFilter{Limit: 10})
	if err != nil {
		t.Fatalf("GetHistoryFiltered: %v", err)
	}
	if len(page.Rows) != 3 {
		t.Fatalf("expected 3 merged rows, got %d", len(page.Rows))
	}
	wantOrder := []int64{1, 2, 3} // newest ts first, regardless of account
	for i, want := range wantOrder {
		if page.Rows[i].ID != want {
			t.Errorf("row %d: id = %d, want %d (merged newest-first order broken)", i, page.Rows[i].ID, want)
		}
	}
}

// padSeconds formats n as a two-digit, zero-padded string for building ordered
// "HH:MM:SS"-style fixture timestamps.
func padSeconds(n int64) string {
	return fmt.Sprintf("%02d", n)
}
