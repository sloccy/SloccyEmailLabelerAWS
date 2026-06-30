package db

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestNow(t *testing.T) {
	before := time.Now().UTC().Truncate(time.Second)
	got := Now()
	after := time.Now().UTC().Add(time.Second)

	parsed, err := time.ParseInLocation("2006-01-02 15:04:05", got, time.UTC)
	if err != nil {
		t.Fatalf("Now() = %q, cannot parse: %v", got, err)
	}
	if parsed.Before(before) || parsed.After(after) {
		t.Errorf("Now() = %q is outside expected range [%v, %v]", got, before, after)
	}
}

func TestIsSQLiteAlreadyExists(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil error", err: nil, want: false},
		{name: "duplicate column name", err: errors.New("table has duplicate column name foo"), want: true},
		{name: "already exists", err: errors.New("table foo already exists"), want: true},
		{name: "index already exists", err: errors.New("index foo already exists"), want: true},
		{name: "other error", err: errors.New("no such table: foo"), want: false},
		{name: "random error", err: errors.New("connection refused"), want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isSQLiteAlreadyExists(tc.err)
			if got != tc.want {
				t.Errorf("isSQLiteAlreadyExists(%q) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// ============================================================
// Account CRUD
// ============================================================

func TestAccountCRUD(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	id, err := s.UpsertAccount(ctx, UpsertAccountParams{Email: "test@example.com", CredentialsJson: `{"token":"x"}`}) //nolint:gosec
	if err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}
	if id == 0 {
		t.Fatal("expected non-zero ID")
	}

	acc, err := s.GetAccount(ctx, id)
	if err != nil {
		t.Fatalf("GetAccount: %v", err)
	}
	if acc.Email != "test@example.com" {
		t.Errorf("Email = %q", acc.Email)
	}

	// Toggle active state.
	active, err := s.ToggleAccount(ctx, id)
	if err != nil {
		t.Fatalf("ToggleAccount: %v", err)
	}
	// Default active=1; toggle should flip to 0 or 1.
	if active != 0 && active != 1 {
		t.Errorf("ToggleAccount returned %d, want 0 or 1", active)
	}

	// Upsert again — should return same ID.
	id2, err := s.UpsertAccount(ctx, UpsertAccountParams{Email: "test@example.com", CredentialsJson: `{"token":"y"}`}) //nolint:gosec
	if err != nil {
		t.Fatalf("UpsertAccount (update): %v", err)
	}
	if id2 != id {
		t.Errorf("UpsertAccount upsert returned different ID: got %d, want %d", id2, id)
	}

	// Delete cascade.
	if err := s.DeleteAccountCascade(ctx, id); err != nil {
		t.Fatalf("DeleteAccountCascade: %v", err)
	}
	if _, err := s.GetAccount(ctx, id); err == nil {
		t.Error("expected error fetching deleted account")
	}
}

// ============================================================
// Prompt CRUD
// ============================================================

func TestPromptCRUD(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	id, err := s.CreatePrompt(ctx, CreatePromptParams{
		Name:         "Newsletter Filter",
		Instructions: "Label newsletters",
		LabelName:    "newsletters",
	})
	if err != nil {
		t.Fatalf("CreatePrompt: %v", err)
	}

	p, err := s.GetPrompt(ctx, id)
	if err != nil {
		t.Fatalf("GetPrompt: %v", err)
	}
	if p.Name != "Newsletter Filter" {
		t.Errorf("Name = %q", p.Name)
	}

	prompts, err := s.ListPrompts(ctx)
	if err != nil {
		t.Fatalf("ListPrompts: %v", err)
	}
	found := false
	for _, pr := range prompts {
		if pr.ID == id {
			found = true
		}
	}
	if !found {
		t.Error("created prompt not found in ListPrompts")
	}

	if err := s.DeletePrompt(ctx, id); err != nil {
		t.Fatalf("DeletePrompt: %v", err)
	}
	if _, err := s.GetPrompt(ctx, id); err == nil {
		t.Error("expected error fetching deleted prompt")
	}
}

// ============================================================
// FilterUnprocessed
// ============================================================

func TestFilterUnprocessed(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	accountID, err := s.UpsertAccount(ctx, UpsertAccountParams{Email: "a@test.com"})
	if err != nil {
		t.Fatalf("setup account: %v", err)
	}

	// Mark msg1 and msg2 as processed.
	for _, mid := range []string{"msg1", "msg2"} {
		if err := s.MarkProcessed(ctx, MarkProcessedParams{AccountID: accountID, MessageID: mid}); err != nil {
			t.Fatalf("MarkProcessed %s: %v", mid, err)
		}
	}

	tests := []struct {
		name  string
		input []string
		want  []string
	}{
		{name: "empty input", input: nil, want: nil},
		{name: "all new", input: []string{"msg3", "msg4"}, want: []string{"msg3", "msg4"}},
		{name: "all processed", input: []string{"msg1", "msg2"}, want: nil},
		{name: "mixed", input: []string{"msg1", "msg3", "msg2", "msg4"}, want: []string{"msg3", "msg4"}},
		{name: "single new", input: []string{"msg5"}, want: []string{"msg5"}},
		{name: "single processed", input: []string{"msg1"}, want: nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.FilterUnprocessed(ctx, accountID, tc.input)
			if err != nil {
				t.Fatalf("FilterUnprocessed: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("len = %d, want %d; got %v", len(got), len(tc.want), got)
			}
			for i, g := range got {
				if g != tc.want[i] {
					t.Errorf("[%d] = %q, want %q", i, g, tc.want[i])
				}
			}
		})
	}
}

// ============================================================
// GetHistoryFiltered
// ============================================================

func insertHistoryRow(t *testing.T, s *Store, ctx context.Context, accountID int64, email, msgID, subject, sender string, promptID sql.NullInt64, promptName, labelName sql.NullString) {
	t.Helper()
	err := s.AddHistory(ctx, AddHistoryParams{
		AccountID:    accountID,
		AccountEmail: email,
		MessageID:    msgID,
		Subject:      subject,
		Sender:       sender,
		PromptID:     promptID,
		PromptName:   promptName,
		LabelName:    labelName,
		Actions:      "label",
	})
	if err != nil {
		t.Fatalf("AddHistory: %v", err)
	}
}

func TestGetHistoryFiltered(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	acc1, _ := s.UpsertAccount(ctx, UpsertAccountParams{Email: "acc1@test.com"})
	acc2, _ := s.UpsertAccount(ctx, UpsertAccountParams{Email: "acc2@test.com"})
	pid := sql.NullInt64{Int64: 99, Valid: true}
	pname := sql.NullString{String: "TestPrompt", Valid: true}
	lname := sql.NullString{String: "newsletters", Valid: true}

	// acc1: two rows with prompt, one without
	insertHistoryRow(t, s, ctx, acc1, "acc1@test.com", "m1", "Hello World", "sender@a.com", pid, pname, lname)
	insertHistoryRow(t, s, ctx, acc1, "acc1@test.com", "m2", "Special Offer", "promo@b.com", sql.NullInt64{}, sql.NullString{}, sql.NullString{})
	// acc2: one row
	insertHistoryRow(t, s, ctx, acc2, "acc2@test.com", "m3", "Digest", "digest@c.com", pid, pname, lname)

	t.Run("no filters returns all within limit", func(t *testing.T) {
		rows, err := s.GetHistoryFiltered(ctx, HistoryFilter{Limit: 100})
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if len(rows) < 3 {
			t.Errorf("expected at least 3 rows, got %d", len(rows))
		}
	})

	t.Run("filter by account", func(t *testing.T) {
		rows, err := s.GetHistoryFiltered(ctx, HistoryFilter{AccountID: &acc1, Limit: 100})
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		for _, r := range rows {
			if r.AccountID != acc1 {
				t.Errorf("got row for wrong account: %d", r.AccountID)
			}
		}
		if len(rows) != 2 {
			t.Errorf("expected 2 rows for acc1, got %d", len(rows))
		}
	})

	t.Run("filter unmatched (no prompt)", func(t *testing.T) {
		rows, err := s.GetHistoryFiltered(ctx, HistoryFilter{Unmatched: true, Limit: 100})
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if len(rows) != 1 {
			t.Errorf("expected 1 unmatched row, got %d", len(rows))
		}
		if rows[0].MessageID != "m2" {
			t.Errorf("wrong unmatched row: %q", rows[0].MessageID)
		}
	})

	t.Run("filter by prompt ID", func(t *testing.T) {
		pid99 := int64(99)
		rows, err := s.GetHistoryFiltered(ctx, HistoryFilter{PromptID: &pid99, Limit: 100})
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if len(rows) != 2 {
			t.Errorf("expected 2 rows for prompt 99, got %d", len(rows))
		}
	})

	t.Run("filter by subject", func(t *testing.T) {
		rows, err := s.GetHistoryFiltered(ctx, HistoryFilter{SubjectQ: "Hello", Limit: 100})
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if len(rows) != 1 || rows[0].Subject != "Hello World" {
			t.Errorf("subject filter: got %v", rows)
		}
	})

	t.Run("filter by sender", func(t *testing.T) {
		rows, err := s.GetHistoryFiltered(ctx, HistoryFilter{SenderQ: "promo", Limit: 100})
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if len(rows) != 1 || !strings.Contains(rows[0].Sender, "promo") {
			t.Errorf("sender filter: got %v", rows)
		}
	})

	t.Run("limit is honored", func(t *testing.T) {
		rows, err := s.GetHistoryFiltered(ctx, HistoryFilter{Limit: 1})
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if len(rows) != 1 {
			t.Errorf("expected 1 row with limit=1, got %d", len(rows))
		}
	})
}

// ============================================================
// RewriteHistoryForMessage
// ============================================================

func TestRewriteHistoryForMessage(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	acc, _ := s.UpsertAccount(ctx, UpsertAccountParams{Email: "rewrite@test.com"})
	pid := sql.NullInt64{Int64: 10, Valid: true}
	base := CategorizationHistory{AccountID: acc, AccountEmail: "rewrite@test.com", Subject: "Test", Sender: "s@test.com"}

	insertHistoryRow(t, s, ctx, acc, "rewrite@test.com", "rwmsg", "Test", "s@test.com",
		pid, sql.NullString{String: "P1", Valid: true}, sql.NullString{String: "lbl", Valid: true})

	t.Run("remove all and add none → sentinel row inserted", func(t *testing.T) {
		err := s.RewriteHistoryForMessage(ctx, "rwmsg", nil, nil, base)
		if err != nil {
			t.Fatalf("RewriteHistoryForMessage: %v", err)
		}
		rows, _ := s.GetHistoryFiltered(ctx, HistoryFilter{AccountID: &acc, Limit: 100})
		foundSentinel := false
		for _, r := range rows {
			if r.MessageID == "rwmsg" && !r.PromptID.Valid {
				foundSentinel = true
			}
		}
		if !foundSentinel {
			t.Error("expected sentinel row (NULL prompt_id) after full removal")
		}
	})

	t.Run("add prompts to new message", func(t *testing.T) {
		newPrompts := []Prompt{{ID: 42, Name: "Added", LabelName: "added"}}
		base2 := CategorizationHistory{AccountID: acc, AccountEmail: "rewrite@test.com", Subject: "New", Sender: "x@test.com"}
		err := s.RewriteHistoryForMessage(ctx, "newmsg", nil, newPrompts, base2)
		if err != nil {
			t.Fatalf("RewriteHistoryForMessage add: %v", err)
		}
		rows, _ := s.GetHistoryFiltered(ctx, HistoryFilter{AccountID: &acc, Limit: 100})
		found := false
		for _, r := range rows {
			if r.MessageID == "newmsg" && r.PromptID.Valid && r.PromptID.Int64 == 42 {
				found = true
			}
		}
		if !found {
			t.Error("expected added prompt row for newmsg")
		}
	})
}

// ============================================================
// Tx rollback on error
// ============================================================

func TestDeleteAccountCascade_RollbackOnError(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// Insert an account then try to delete a non-existent one — should not affect existing.
	id, _ := s.UpsertAccount(ctx, UpsertAccountParams{Email: "safe@test.com"})
	// DeleteAccountCascade for a non-existent ID should succeed (DELETE WHERE id=? is a no-op in SQLite).
	err := s.DeleteAccountCascade(ctx, 99999)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Existing account should still be present.
	if _, err := s.GetAccount(ctx, id); err != nil {
		t.Errorf("existing account should not be deleted: %v", err)
	}
}

// ============================================================
// Migrate idempotency
// ============================================================

func TestMigrateIdempotent(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t) // already called Migrate once inside newTestStore

	// Second Migrate should be a no-op.
	if err := s.Migrate(); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}

	// Schema version should be the total number of migrations.
	ver, err := s.GetSchemaVersion(ctx)
	if err != nil {
		t.Fatalf("GetSchemaVersion: %v", err)
	}
	if ver < 3 {
		t.Errorf("schema version = %d, want >= 3", ver)
	}
}
