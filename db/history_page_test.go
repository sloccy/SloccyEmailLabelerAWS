package db

import (
	"fmt"
	"testing"
)

// seedHistoryRow appends a history row directly to a FakeStore, bypassing
// BatchInsertProcessingResults (whose Timestamp is always Now()) so tests can control
// exact timestamps and ids — needed to build interleaved, multi-account fixtures for
// cursor-pagination tests.
func seedHistoryRow(s *FakeStore, id, accountID int64, ts, subject, sender string) {
	s.history = append(s.history, &CategorizationHistory{
		ID:        id,
		Timestamp: ts,
		AccountID: accountID,
		Subject:   subject,
		Sender:    sender,
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
	for i := range n {
		// Zero-padded seconds so lexicographic == chronological order, matching tsKey.
		ts := "2026-08-01 00:00:" + padSeconds(i)
		seedHistoryRow(s, int64(i)+1, accID, ts, "subject", "sender@example.com")
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
	for i := range n {
		ts := "2026-08-01 00:01:" + padSeconds(i%60)
		subject := "newsletter digest"
		if i == 3 || i == 35 { // sparse matches, spread across the dataset
			subject = "Special Offer Inside"
		}
		seedHistoryRow(s, int64(i)+1, accID, ts, subject, "sender@example.com")
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
	seedHistoryRow(s, 1, acc1, "2026-08-01 00:00:03", "a-newest", "s")
	seedHistoryRow(s, 2, acc2, "2026-08-01 00:00:02", "b-middle", "s")
	seedHistoryRow(s, 3, acc1, "2026-08-01 00:00:01", "a-oldest", "s")

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

// TestGetHistoryFiltered_ScannedAdvancesOnEmptyPages covers the accounting the History
// tab's row budget runs on. A sparse search returns zero matches on most pages, so a budget
// counting matched rows would barely move and the scroll would issue unbounded requests;
// Scanned counts what was actually read, so every page — including the ones that matched
// nothing — makes progress toward the ceiling.
func TestGetHistoryFiltered_ScannedAdvancesOnEmptyPages(t *testing.T) {
	s := NewFake()
	accID, _ := s.UpsertAccount(t.Context(), UpsertAccountParams{Email: "scanned@example.com"})

	const n = 40
	for i := range n {
		seedHistoryRow(s, int64(i)+1, accID, "2026-08-01 00:01:"+padSeconds(i%60), "newsletter digest", "sender@example.com")
	}

	cursor := ""
	var totalScanned int64
	for pages := 0; ; pages++ {
		if pages > n {
			t.Fatalf("pagination did not terminate after %d pages", pages)
		}
		page, err := s.GetHistoryFiltered(t.Context(), HistoryFilter{
			AccountID: &accID, Limit: 5, Cursor: cursor, SubjectQ: "nothing matches this",
		})
		if err != nil {
			t.Fatalf("GetHistoryFiltered: %v", err)
		}
		if len(page.Rows) != 0 {
			t.Fatalf("filter should match nothing, got %d rows", len(page.Rows))
		}
		if page.Scanned == 0 {
			t.Fatal("a page that read rows but matched none reported Scanned = 0; the row budget would never advance")
		}
		totalScanned += page.Scanned
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if totalScanned != n {
		t.Errorf("total Scanned across the walk = %d, want %d (every row examined exactly once)", totalScanned, n)
	}
}

// TestGetHistoryFiltered_RuleFilterEmptyPageKeepsCursor is the regression test for
// pagination ending early. PromptID goes to DynamoDB as a FilterExpression, which runs
// *after* Limit — so a page can come back with every fetched item dropped while unread rows
// remain below it. The merge is then empty, and the cursor has to come from how deep the
// Query read rather than from any surviving row; returning "" instead would report "no more
// history" with the only matching email still unreached.
func TestGetHistoryFiltered_RuleFilterEmptyPageKeepsCursor(t *testing.T) {
	s := NewFake()
	accID, _ := s.UpsertAccount(t.Context(), UpsertAccountParams{Email: "rulefilter@example.com"})

	wantPrompt := int64(9)
	otherPrompt := int64(3)
	const n = 30
	for i := range n {
		h := &CategorizationHistory{
			ID:        int64(i) + 1,
			Timestamp: "2026-08-01 00:02:" + padSeconds(i%60),
			AccountID: accID,
			Subject:   "subject",
			Sender:    "sender@example.com",
			PromptID:  &otherPrompt,
		}
		if i == 0 { // the single match is the *oldest* row, well past the first page
			h.PromptID = &wantPrompt
		}
		s.history = append(s.history, h)
	}

	var matched []CategorizationHistory
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > n {
			t.Fatalf("pagination did not terminate after %d pages", pages)
		}
		page, err := s.GetHistoryFiltered(t.Context(), HistoryFilter{
			AccountID: &accID, Limit: 5, Cursor: cursor, PromptID: &wantPrompt,
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

	if len(matched) != 1 {
		t.Fatalf("expected the single rule-9 row to be reachable, got %d matches", len(matched))
	}
	if matched[0].ID != 1 {
		t.Errorf("matched id = %d, want 1", matched[0].ID)
	}
}

// TestHistoryMergeCut_StopsAtFloor covers the multi-account gap directly. Each account gets
// its own Limit-bounded Query, so they can read to different depths; the merged view is only
// complete down to the shallowest of them. Consuming past that point would drop the
// deeper-unread account's rows out of the walk while advancing the cursor past them, and
// nothing would ever go back for them.
func TestHistoryMergeCut_StopsAtFloor(t *testing.T) {
	// Account A read down to :05 (floor); account B happened to read down to :01.
	all := []CategorizationHistory{
		{ID: 1, Timestamp: "2026-08-01 00:00:09"},
		{ID: 2, Timestamp: "2026-08-01 00:00:05"},
		{ID: 3, Timestamp: "2026-08-01 00:00:03"}, // below the floor: B's, A's are unfetched
		{ID: 4, Timestamp: "2026-08-01 00:00:01"},
	}
	floor := tsKey("2026-08-01 00:00:05", 2)

	rows, next := historyMergeCut(all, HistoryFilter{}, 50, floor)

	if len(rows) != 2 || rows[0].ID != 1 || rows[1].ID != 2 {
		t.Fatalf("rows = %+v, want only the two rows at or above the floor", rows)
	}
	if next != floor {
		t.Errorf("nextCursor = %q, want the floor %q so the skipped rows are re-read", next, floor)
	}
}

// TestHistoryMergeCut_EmptyMergeUsesFloor is the unit-level counterpart to
// TestGetHistoryFiltered_RuleFilterEmptyPageKeepsCursor: with nothing left to examine after
// a DynamoDB-side filter, the truncation depth is the only remaining evidence that more data
// exists, so it has to become the cursor.
func TestHistoryMergeCut_EmptyMergeUsesFloor(t *testing.T) {
	floor := tsKey("2026-08-01 00:00:05", 2)

	rows, next := historyMergeCut(nil, HistoryFilter{}, 50, floor)

	if len(rows) != 0 {
		t.Fatalf("rows = %+v, want none", rows)
	}
	if next != floor {
		t.Errorf("nextCursor = %q, want the floor %q — an empty page must not end pagination", next, floor)
	}
}

// TestHistoryMergeCut_ExhaustedEndsWalk pins the other side of that rule: with no truncated
// account there is genuinely nothing below, and the walk has to actually stop.
func TestHistoryMergeCut_ExhaustedEndsWalk(t *testing.T) {
	all := []CategorizationHistory{
		{ID: 1, Timestamp: "2026-08-01 00:00:09", Subject: "keep"},
		{ID: 2, Timestamp: "2026-08-01 00:00:05", Subject: "drop"},
	}

	rows, next := historyMergeCut(all, HistoryFilter{SubjectQ: "keep"}, 50, "")

	if len(rows) != 1 || rows[0].ID != 1 {
		t.Fatalf("rows = %+v, want just the matching row", rows)
	}
	if next != "" {
		t.Errorf("nextCursor = %q, want \"\" once every partition is exhausted", next)
	}
}

// padSeconds formats n as a two-digit, zero-padded string for building ordered
// "HH:MM:SS"-style fixture timestamps.
func padSeconds(n int) string {
	return fmt.Sprintf("%02d", n)
}
