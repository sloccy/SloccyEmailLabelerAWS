package main

import (
	"database/sql"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/sloccy/ollamail/db"
)

func TestBuildAccountMap(t *testing.T) {
	rows := []db.ListAccountsSafeRow{
		{ID: 1, Email: "a@example.com"},
		{ID: 2, Email: "b@example.com"},
	}
	m := buildAccountMap(rows)
	if m[1] != "a@example.com" {
		t.Errorf("id 1 = %q", m[1])
	}
	if m[2] != "b@example.com" {
		t.Errorf("id 2 = %q", m[2])
	}
	if len(m) != 2 {
		t.Errorf("len = %d, want 2", len(m))
	}
}

func TestBuildAccountMap_Empty(t *testing.T) {
	m := buildAccountMap(nil)
	if len(m) != 0 {
		t.Errorf("empty input should give empty map, got len=%d", len(m))
	}
}

func TestToAccountViews(t *testing.T) {
	rows := []db.ListAccountsSafeRow{
		{ID: 1, Email: "x@example.com", Active: 1, AddedAt: "2024-01-01 00:00:00", LastScanAt: sql.NullString{String: "2024-01-02 00:00:00", Valid: true}},
		{ID: 2, Email: "y@example.com", Active: 0, AddedAt: "2024-02-01 00:00:00", LastScanAt: sql.NullString{}},
	}
	views := toAccountViews(rows)
	if len(views) != 2 {
		t.Fatalf("len = %d, want 2", len(views))
	}

	v0 := views[0]
	if v0.ID != 1 || v0.Email != "x@example.com" {
		t.Errorf("views[0] ID/Email wrong: %+v", v0)
	}
	if !v0.Active {
		t.Error("views[0].Active should be true (Active=1)")
	}
	if v0.LastScanAt != "2024-01-02 00:00:00" {
		t.Errorf("views[0].LastScanAt = %q", v0.LastScanAt)
	}

	v1 := views[1]
	if v1.Active {
		t.Error("views[1].Active should be false (Active=0)")
	}
	if v1.LastScanAt != "" {
		t.Errorf("views[1].LastScanAt should be empty for NullString{Valid:false}, got %q", v1.LastScanAt)
	}
}

func TestDbPromptToView(t *testing.T) {
	accountMap := map[int64]string{5: "owner@example.com"}

	p := db.Prompt{
		ID:             10,
		Name:           "Test Prompt",
		Instructions:   "some instructions",
		LabelName:      "newsletters",
		Active:         1,
		CreatedAt:      "2024-01-01 00:00:00",
		ActionArchive:  1,
		ActionSpam:     0,
		ActionTrash:    1,
		ActionMarkRead: 0,
		StopProcessing: 1,
		AccountID:      sql.NullInt64{Int64: 5, Valid: true},
	}

	pv := dbPromptToView(p, accountMap)

	if pv.ID != 10 {
		t.Errorf("ID = %d", pv.ID)
	}
	if pv.Name != "Test Prompt" {
		t.Errorf("Name = %q", pv.Name)
	}
	if !pv.Active {
		t.Error("Active should be true")
	}
	if !pv.ActionArchive {
		t.Error("ActionArchive should be true")
	}
	if pv.ActionSpam {
		t.Error("ActionSpam should be false")
	}
	if !pv.ActionTrash {
		t.Error("ActionTrash should be true")
	}
	if pv.ActionMarkRead {
		t.Error("ActionMarkRead should be false")
	}
	if !pv.StopProcessing {
		t.Error("StopProcessing should be true")
	}
	if pv.AccountID != 5 {
		t.Errorf("AccountID = %d", pv.AccountID)
	}
	if pv.AccountEmail != "owner@example.com" {
		t.Errorf("AccountEmail = %q", pv.AccountEmail)
	}
}

func TestDbPromptToView_NoAccount(t *testing.T) {
	p := db.Prompt{
		ID:        1,
		AccountID: sql.NullInt64{Valid: false},
	}
	pv := dbPromptToView(p, map[int64]string{})
	if pv.AccountID != 0 {
		t.Errorf("AccountID should be 0 for invalid NullInt64, got %d", pv.AccountID)
	}
	if pv.AccountEmail != "" {
		t.Errorf("AccountEmail should be empty, got %q", pv.AccountEmail)
	}
}

func TestBoolToInt(t *testing.T) {
	if boolToInt(true) != 1 {
		t.Error("boolToInt(true) should be 1")
	}
	if boolToInt(false) != 0 {
		t.Error("boolToInt(false) should be 0")
	}
}

func TestGenerateToken(t *testing.T) {
	tests := []struct{ n int }{
		{8}, {16}, {32},
	}
	for _, tc := range tests {
		t.Run("", func(t *testing.T) {
			tok := generateToken(tc.n)
			// hex.EncodeToString produces 2 chars per byte.
			wantLen := tc.n * 2
			if len(tok) != wantLen {
				t.Errorf("generateToken(%d) len = %d, want %d", tc.n, len(tok), wantLen)
			}
			// Must be valid hex.
			if _, err := hex.DecodeString(tok); err != nil {
				t.Errorf("generateToken(%d) = %q is not valid hex: %v", tc.n, tok, err)
			}
		})
	}

	t.Run("tokens differ", func(t *testing.T) {
		a := generateToken(16)
		b := generateToken(16)
		if a == b {
			t.Error("two calls to generateToken should not produce identical tokens")
		}
	})

	t.Run("lowercase hex only", func(t *testing.T) {
		tok := generateToken(32)
		if strings.ToLower(tok) != tok {
			t.Errorf("expected lowercase hex, got %q", tok)
		}
	})
}
