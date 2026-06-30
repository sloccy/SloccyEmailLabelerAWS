package processor

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sloccy/ollamail/db"
	gmailpkg "github.com/sloccy/ollamail/gmail"
	"github.com/sloccy/ollamail/llm"
)

// ============================================================
// Helpers
// ============================================================

func newTestStore(t *testing.T) *db.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := db.Open(path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	if err := s.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func newLLMServer(t *testing.T, response string) *llm.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := map[string]any{"message": map[string]string{"content": response}}
		b, _ := json.Marshal(resp)
		w.Header().Set("Content-Type", "application/json")
		w.Write(b) //nolint:errcheck,gosec
	}))
	t.Cleanup(srv.Close)
	return llm.NewClient(srv.URL, "test-model", 4096, 5*time.Second)
}

// ============================================================
// filterPrompts
// ============================================================

func TestFilterPrompts(t *testing.T) {
	prompt := func(id int64, active int64, accountID sql.NullInt64) db.Prompt {
		return db.Prompt{ID: id, Name: "P", Instructions: "x", Active: active, AccountID: accountID}
	}

	global := func(id int64, active int64) db.Prompt {
		return prompt(id, active, sql.NullInt64{Valid: false})
	}
	forAccount := func(id int64, active int64, accID int64) db.Prompt {
		return prompt(id, active, sql.NullInt64{Int64: accID, Valid: true})
	}

	tests := []struct {
		name      string
		prompts   []db.Prompt
		accountID int64
		wantIDs   []int64
	}{
		{
			name:      "global active prompt included",
			prompts:   []db.Prompt{global(1, 1)},
			accountID: 5,
			wantIDs:   []int64{1},
		},
		{
			name:      "inactive prompt excluded",
			prompts:   []db.Prompt{global(1, 0)},
			accountID: 5,
			wantIDs:   nil,
		},
		{
			name:      "account-specific prompt for this account included",
			prompts:   []db.Prompt{forAccount(2, 1, 5)},
			accountID: 5,
			wantIDs:   []int64{2},
		},
		{
			name:      "account-specific prompt for other account excluded",
			prompts:   []db.Prompt{forAccount(3, 1, 99)},
			accountID: 5,
			wantIDs:   nil,
		},
		{
			name: "mixed: global active + inactive + other account + this account",
			prompts: []db.Prompt{
				global(1, 1),         // include
				global(2, 0),         // exclude: inactive
				forAccount(3, 1, 5),  // include: this account
				forAccount(4, 1, 99), // exclude: other account
				forAccount(5, 0, 5),  // exclude: inactive
			},
			accountID: 5,
			wantIDs:   []int64{1, 3},
		},
		{
			name:      "empty input",
			prompts:   nil,
			accountID: 1,
			wantIDs:   nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := filterPrompts(tc.prompts, tc.accountID)
			if len(got) != len(tc.wantIDs) {
				t.Fatalf("len = %d, want %d; got IDs: %v", len(got), len(tc.wantIDs), idsOf(got))
			}
			for i, p := range got {
				if p.ID != tc.wantIDs[i] {
					t.Errorf("[%d] ID = %d, want %d", i, p.ID, tc.wantIDs[i])
				}
			}
		})
	}
}

func idsOf(prompts []db.Prompt) []int64 {
	ids := make([]int64, len(prompts))
	for i, p := range prompts {
		ids[i] = p.ID
	}
	return ids
}

// ============================================================
// marshalGmailDebug
// ============================================================

func TestMarshalGmailDebug(t *testing.T) {
	msg := gmailpkg.Message{
		ID:      "m1",
		Subject: "Hello",
		Sender:  "sender@example.com",
		Body:    "body text",
		Snippet: "body",
	}
	raw := marshalGmailDebug(msg)
	if raw == "" || raw == "{}" {
		t.Fatalf("marshalGmailDebug returned %q", raw)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if m["id"] != "m1" {
		t.Errorf("id = %v", m["id"])
	}
	if m["subject"] != "Hello" {
		t.Errorf("subject = %v", m["subject"])
	}
}

// ============================================================
// processEmail (unexported but accessible in same package)
// ============================================================

func newTestAccount() db.Account {
	return db.Account{ID: 1, Email: "test@example.com", CredentialsJson: `{}`, Active: 1}
}

func TestProcessEmail_MatchedPrompt(t *testing.T) {
	store := newTestStore(t)
	// LLM returns {"1": true} — prompt 1 matches.
	ollamaClient := newLLMServer(t, `{"1": true}`)

	account := newTestAccount()
	msg := gmailpkg.Message{ID: "msg1", Subject: "Newsletter", Sender: "news@test.com", Body: "content"}
	prompts := []db.Prompt{
		{ID: 10, Name: "Newsletter", LabelName: "newsletters", Active: 1, Instructions: "label newsletters"},
	}
	labelCache := map[string]string{"newsletters": "Label_42"}

	modifies, trashIDs := processEmail(t.Context(), store, ollamaClient, account, msg, prompts, labelCache, false)

	if len(modifies) == 0 {
		t.Fatal("expected at least one modify")
	}
	found := false
	for _, m := range modifies {
		if m.MessageID == "msg1" {
			found = true
			if !contains(m.AddLabels, "Label_42") {
				t.Errorf("AddLabels = %v, want Label_42", m.AddLabels)
			}
		}
	}
	if !found {
		t.Error("modify for msg1 not found")
	}
	if len(trashIDs) != 0 {
		t.Errorf("unexpected trash IDs: %v", trashIDs)
	}
}

func TestProcessEmail_NoMatch(t *testing.T) {
	store := newTestStore(t)
	ollamaClient := newLLMServer(t, `{"1": false}`)

	account := newTestAccount()
	msg := gmailpkg.Message{ID: "msg2", Subject: "Regular", Sender: "user@test.com"}
	prompts := []db.Prompt{
		{ID: 10, Name: "Newsletter", LabelName: "newsletters", Active: 1, Instructions: "label newsletters"},
	}

	modifies, trashIDs := processEmail(t.Context(), store, ollamaClient, account, msg, prompts, nil, false)

	if len(modifies) != 0 {
		t.Errorf("expected no modifies for no-match, got %v", modifies)
	}
	if len(trashIDs) != 0 {
		t.Errorf("expected no trash, got %v", trashIDs)
	}
}

func TestProcessEmail_TrashAction(t *testing.T) {
	store := newTestStore(t)
	ollamaClient := newLLMServer(t, `{"1": true}`)

	account := newTestAccount()
	msg := gmailpkg.Message{ID: "trash1", Subject: "Spam", Sender: "spam@test.com"}
	prompts := []db.Prompt{
		{ID: 5, Name: "Spam", LabelName: "spam", ActionTrash: 1, Active: 1, Instructions: "trash spam"},
	}

	_, trashIDs := processEmail(t.Context(), store, ollamaClient, account, msg, prompts, map[string]string{}, false)

	if !contains(trashIDs, "trash1") {
		t.Errorf("expected trash1 in trashIDs, got %v", trashIDs)
	}
}

func TestProcessEmail_StopProcessing(t *testing.T) {
	store := newTestStore(t)
	// Both prompts match, but prompt 1 has StopProcessing=1.
	ollamaClient := newLLMServer(t, `{"1": true, "2": true}`)

	account := newTestAccount()
	msg := gmailpkg.Message{ID: "stop1", Subject: "Test"}
	prompts := []db.Prompt{
		{ID: 1, Name: "First", LabelName: "l1", StopProcessing: 1, Active: 1, Instructions: "stop"},
		{ID: 2, Name: "Second", LabelName: "l2", Active: 1, Instructions: "should not run"},
	}
	labelCache := map[string]string{"l1": "L1", "l2": "L2"}

	modifies, _ := processEmail(t.Context(), store, ollamaClient, account, msg, prompts, labelCache, false)

	for _, m := range modifies {
		if contains(m.AddLabels, "L2") {
			t.Error("L2 label should not be applied after StopProcessing")
		}
	}
}

func TestProcessEmail_LLMError(t *testing.T) {
	store := newTestStore(t)
	errSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "ollama down", http.StatusInternalServerError)
	}))
	t.Cleanup(errSrv.Close)
	ollamaClient := llm.NewClient(errSrv.URL, "m", 4096, time.Second)

	account := newTestAccount()
	msg := gmailpkg.Message{ID: "err1", Subject: "Test"}
	prompts := []db.Prompt{{ID: 1, Name: "P", LabelName: "l", Active: 1, Instructions: "x"}}

	modifies, trashIDs := processEmail(t.Context(), store, ollamaClient, account, msg, prompts, nil, false)

	// On LLM error, processEmail returns nil and does NOT mark the message processed.
	if len(modifies) != 0 || len(trashIDs) != 0 {
		t.Errorf("expected nil on LLM error, got modifies=%v trash=%v", modifies, trashIDs)
	}
}

func TestProcessEmail_ArchiveAction(t *testing.T) {
	store := newTestStore(t)
	ollamaClient := newLLMServer(t, `{"1": true}`)

	account := newTestAccount()
	msg := gmailpkg.Message{ID: "arch1", Subject: "Archive me"}
	prompts := []db.Prompt{
		{ID: 1, Name: "Archive", LabelName: "", ActionArchive: 1, Active: 1, Instructions: "archive"},
	}

	modifies, _ := processEmail(t.Context(), store, ollamaClient, account, msg, prompts, nil, false)

	if len(modifies) == 0 {
		t.Fatal("expected a modify for archive action")
	}
	found := false
	for _, m := range modifies {
		if contains(m.RemoveLabels, gmailpkg.LabelInbox) {
			found = true
		}
	}
	if !found {
		t.Error("expected INBOX removal for archive action")
	}
}

func TestProcessEmail_MarkReadAction(t *testing.T) {
	store := newTestStore(t)
	ollamaClient := newLLMServer(t, `{"1": true}`)

	account := newTestAccount()
	msg := gmailpkg.Message{ID: "read1", Subject: "Mark me read"}
	prompts := []db.Prompt{
		{ID: 1, Name: "MarkRead", LabelName: "", ActionMarkRead: 1, Active: 1, Instructions: "mark read"},
	}

	modifies, _ := processEmail(t.Context(), store, ollamaClient, account, msg, prompts, nil, false)

	found := false
	for _, m := range modifies {
		if contains(m.RemoveLabels, gmailpkg.LabelUnread) {
			found = true
		}
	}
	if !found {
		t.Error("expected UNREAD removal for mark-read action")
	}
}

// ============================================================
// history and DB side-effects
// ============================================================

func TestProcessEmail_WritesHistoryAndLlmDebug(t *testing.T) {
	store := newTestStore(t)
	ollamaClient := newLLMServer(t, `{"1": true}`)

	accID, _ := store.UpsertAccount(t.Context(), db.UpsertAccountParams{Email: "test@example.com"})
	account := db.Account{ID: accID, Email: "test@example.com", Active: 1}
	msg := gmailpkg.Message{ID: "hist1", Subject: "Newsletter Match", Sender: "news@test.com"}
	prompts := []db.Prompt{
		{ID: 1, Name: "NL", LabelName: "newsletters", Active: 1, Instructions: "label nl"},
	}
	labelCache := map[string]string{"newsletters": "L1"}

	processEmail(t.Context(), store, ollamaClient, account, msg, prompts, labelCache, false)

	// Verify history was written.
	history, err := store.GetHistoryFiltered(t.Context(), db.HistoryFilter{AccountID: &accID, Limit: 10})
	if err != nil {
		t.Fatalf("GetHistoryFiltered: %v", err)
	}
	if len(history) == 0 {
		t.Error("expected history row after processEmail")
	}

	// Verify message is marked as processed.
	unprocessed, _ := store.FilterUnprocessed(t.Context(), accID, []string{"hist1"})
	if len(unprocessed) != 0 {
		t.Errorf("expected hist1 to be marked processed, FilterUnprocessed returned %v", unprocessed)
	}
}

func TestProcessEmail_NoMatchWritesSentinelHistory(t *testing.T) {
	store := newTestStore(t)
	ollamaClient := newLLMServer(t, `{"1": false}`)

	accID, _ := store.UpsertAccount(t.Context(), db.UpsertAccountParams{Email: "test@example.com"})
	account := db.Account{ID: accID, Email: "test@example.com", Active: 1}
	msg := gmailpkg.Message{ID: "nomatch1", Subject: "No Match"}
	prompts := []db.Prompt{{ID: 1, Name: "P", Active: 1, Instructions: "x"}}

	processEmail(t.Context(), store, ollamaClient, account, msg, prompts, nil, false)

	history, _ := store.GetHistoryFiltered(t.Context(), db.HistoryFilter{Unmatched: true, Limit: 10})
	found := false
	for _, h := range history {
		if h.MessageID == "nomatch1" {
			found = true
		}
	}
	if !found {
		t.Error("expected sentinel (no-match) history row for unmatched email")
	}
}

// ============================================================
// Helpers
// ============================================================

func contains(slice []string, s string) bool {
	for _, v := range slice {
		if v == s || strings.Contains(v, s) {
			return true
		}
	}
	return false
}
