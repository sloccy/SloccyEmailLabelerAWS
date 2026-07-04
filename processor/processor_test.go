package processor

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/sloccy/ollamail-aws/db"
	gmailpkg "github.com/sloccy/ollamail-aws/gmail"
	"github.com/sloccy/ollamail-aws/llm"
)

// ============================================================
// Helpers
// ============================================================

func newTestStore(t *testing.T) *db.FakeStore {
	t.Helper()
	return db.NewFake()
}

func newLLMServer(t *testing.T, response string) *llm.FakeClient {
	t.Helper()
	return llm.NewFakeClient(response)
}

// ============================================================
// ModifyForPrompt
// ============================================================

func TestModifyForPrompt(t *testing.T) {
	tests := []struct {
		name       string
		prompt     db.Prompt
		labelID    string
		wantAdd    []string
		wantRemove []string
		wantTrash  bool
	}{
		{
			name:    "label only",
			prompt:  db.Prompt{LabelName: "Newsletters"},
			labelID: "Label_1",
			wantAdd: []string{"Label_1"},
		},
		{
			name:    "label not resolved yet — no add",
			prompt:  db.Prompt{LabelName: "Newsletters"},
			labelID: "",
			wantAdd: nil,
		},
		{
			name:       "spam adds SPAM, removes INBOX",
			prompt:     db.Prompt{ActionSpam: 1},
			wantAdd:    []string{gmailpkg.LabelSpam},
			wantRemove: []string{gmailpkg.LabelInbox},
		},
		{
			name:      "trash sets trash flag, no label mutation",
			prompt:    db.Prompt{ActionTrash: 1},
			wantTrash: true,
		},
		{
			name:       "archive removes INBOX",
			prompt:     db.Prompt{ActionArchive: 1},
			wantRemove: []string{gmailpkg.LabelInbox},
		},
		{
			name:       "mark read removes UNREAD, composes with label",
			prompt:     db.Prompt{LabelName: "Receipts", ActionMarkRead: 1},
			labelID:    "Label_2",
			wantAdd:    []string{"Label_2"},
			wantRemove: []string{gmailpkg.LabelUnread},
		},
		{
			name:       "spam takes priority over trash/archive",
			prompt:     db.Prompt{ActionSpam: 1, ActionTrash: 1, ActionArchive: 1},
			wantAdd:    []string{gmailpkg.LabelSpam},
			wantRemove: []string{gmailpkg.LabelInbox},
			wantTrash:  false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mod, trash := ModifyForPrompt(tc.prompt, tc.labelID)
			if !slicesEqual(mod.AddLabels, tc.wantAdd) {
				t.Errorf("AddLabels = %v, want %v", mod.AddLabels, tc.wantAdd)
			}
			if !slicesEqual(mod.RemoveLabels, tc.wantRemove) {
				t.Errorf("RemoveLabels = %v, want %v", mod.RemoveLabels, tc.wantRemove)
			}
			if trash != tc.wantTrash {
				t.Errorf("trash = %v, want %v", trash, tc.wantTrash)
			}
			if len(mod.MessageIDs) != 0 {
				t.Errorf("MessageIDs should be left for the caller to set, got %v", mod.MessageIDs)
			}
		})
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ============================================================
// filterPrompts
// ============================================================

func TestFilterPrompts(t *testing.T) {
	prompt := func(id int64, active int64, accountID *int64) db.Prompt {
		return db.Prompt{ID: id, Name: "P", Instructions: "x", Active: active, AccountID: accountID}
	}

	global := func(id int64, active int64) db.Prompt {
		return prompt(id, active, nil)
	}
	forAccount := func(id int64, active int64, accID int64) db.Prompt {
		return prompt(id, active, &accID)
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
	return db.Account{ID: 1, Email: "test@example.com", CredentialsJSON: `{}`, Active: 1}
}

func TestProcessEmail_MatchedPrompt(t *testing.T) {
	// LLM returns {"1": true} — prompt 1 matches.
	llmClient := newLLMServer(t, `{"1": true}`)

	account := newTestAccount()
	msg := gmailpkg.Message{ID: "msg1", Subject: "Newsletter", Sender: "news@test.com", Body: "content"}
	prompts := []db.Prompt{
		{ID: 10, Name: "Newsletter", LabelName: "newsletters", Active: 1, Instructions: "label newsletters"},
	}
	labelCache := map[string]string{"newsletters": "Label_42"}

	modifies, trashIDs, _ := processEmail(t.Context(), llmClient, account, msg, prompts, labelCache, false, "", "")

	if len(modifies) == 0 {
		t.Fatal("expected at least one modify")
	}
	found := false
	for _, m := range modifies {
		if contains(m.MessageIDs, "msg1") {
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
	llmClient := newLLMServer(t, `{"1": false}`)

	account := newTestAccount()
	msg := gmailpkg.Message{ID: "msg2", Subject: "Regular", Sender: "user@test.com"}
	prompts := []db.Prompt{
		{ID: 10, Name: "Newsletter", LabelName: "newsletters", Active: 1, Instructions: "label newsletters"},
	}

	modifies, trashIDs, _ := processEmail(t.Context(), llmClient, account, msg, prompts, nil, false, "", "")

	if len(modifies) != 0 {
		t.Errorf("expected no modifies for no-match, got %v", modifies)
	}
	if len(trashIDs) != 0 {
		t.Errorf("expected no trash, got %v", trashIDs)
	}
}

func TestProcessEmail_TrashAction(t *testing.T) {
	llmClient := newLLMServer(t, `{"1": true}`)

	account := newTestAccount()
	msg := gmailpkg.Message{ID: "trash1", Subject: "Spam", Sender: "spam@test.com"}
	prompts := []db.Prompt{
		{ID: 5, Name: "Spam", LabelName: "spam", ActionTrash: 1, Active: 1, Instructions: "trash spam"},
	}

	_, trashIDs, _ := processEmail(t.Context(), llmClient, account, msg, prompts, map[string]string{}, false, "", "")

	if !contains(trashIDs, "trash1") {
		t.Errorf("expected trash1 in trashIDs, got %v", trashIDs)
	}
}

func TestProcessEmail_StopProcessing(t *testing.T) {
	// Both prompts match, but prompt 1 has StopProcessing=1.
	llmClient := newLLMServer(t, `{"1": true, "2": true}`)

	account := newTestAccount()
	msg := gmailpkg.Message{ID: "stop1", Subject: "Test"}
	prompts := []db.Prompt{
		{ID: 1, Name: "First", LabelName: "l1", StopProcessing: 1, Active: 1, Instructions: "stop"},
		{ID: 2, Name: "Second", LabelName: "l2", Active: 1, Instructions: "should not run"},
	}
	labelCache := map[string]string{"l1": "L1", "l2": "L2"}

	modifies, _, _ := processEmail(t.Context(), llmClient, account, msg, prompts, labelCache, false, "", "")

	for _, m := range modifies {
		if contains(m.AddLabels, "L2") {
			t.Error("L2 label should not be applied after StopProcessing")
		}
	}
}

func TestProcessEmail_LLMError(t *testing.T) {
	llmClient := llm.NewFakeErrorClient()

	account := newTestAccount()
	msg := gmailpkg.Message{ID: "err1", Subject: "Test"}
	prompts := []db.Prompt{{ID: 1, Name: "P", LabelName: "l", Active: 1, Instructions: "x"}}

	modifies, trashIDs, job := processEmail(t.Context(), llmClient, account, msg, prompts, nil, false, "", "")

	// On LLM error, processEmail returns nil and does NOT mark the message processed.
	if len(modifies) != 0 || len(trashIDs) != 0 {
		t.Errorf("expected nil on LLM error, got modifies=%v trash=%v", modifies, trashIDs)
	}
	if job.messageID != "" {
		t.Errorf("expected empty messageID (not marked processed) on LLM error, got %q", job.messageID)
	}
}

func TestProcessEmail_ArchiveAction(t *testing.T) {
	llmClient := newLLMServer(t, `{"1": true}`)

	account := newTestAccount()
	msg := gmailpkg.Message{ID: "arch1", Subject: "Archive me"}
	prompts := []db.Prompt{
		{ID: 1, Name: "Archive", LabelName: "", ActionArchive: 1, Active: 1, Instructions: "archive"},
	}

	modifies, _, _ := processEmail(t.Context(), llmClient, account, msg, prompts, nil, false, "", "")

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
	llmClient := newLLMServer(t, `{"1": true}`)

	account := newTestAccount()
	msg := gmailpkg.Message{ID: "read1", Subject: "Mark me read"}
	prompts := []db.Prompt{
		{ID: 1, Name: "MarkRead", LabelName: "", ActionMarkRead: 1, Active: 1, Instructions: "mark read"},
	}

	modifies, _, _ := processEmail(t.Context(), llmClient, account, msg, prompts, nil, false, "", "")

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
	llmClient := newLLMServer(t, `{"1": true}`)

	accID, _ := store.UpsertAccount(t.Context(), db.UpsertAccountParams{Email: "test@example.com"})
	account := db.Account{ID: accID, Email: "test@example.com", Active: 1}
	msg := gmailpkg.Message{ID: "hist1", Subject: "Newsletter Match", Sender: "news@test.com"}
	prompts := []db.Prompt{
		{ID: 1, Name: "NL", LabelName: "newsletters", Active: 1, Instructions: "label nl"},
	}
	labelCache := map[string]string{"newsletters": "L1"}

	_, _, job := processEmail(t.Context(), llmClient, account, msg, prompts, labelCache, false, "", "")
	applyWriteJob(t.Context(), store, job)

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

// TestProcessEmail_LlmDebugGatedOnDebugLogging asserts the fattest per-email write (raw
// Gmail message + full LLM request/response) is only built and returned for the writer
// to persist when DebugLogging is on — keeping normal operation to just the batched
// logs+history write plus the processed marker, which matters at low provisioned WCU.
func TestProcessEmail_LlmDebugGatedOnDebugLogging(t *testing.T) {
	llmClient := newLLMServer(t, `{"1": true}`)
	account := newTestAccount()
	msg := gmailpkg.Message{ID: "dbg1", Subject: "Test", Sender: "a@b.com"}
	prompts := []db.Prompt{{ID: 1, Name: "P", LabelName: "l", Active: 1, Instructions: "x"}}
	labelCache := map[string]string{"l": "L1"}

	_, _, job := processEmail(t.Context(), llmClient, account, msg, prompts, labelCache, false, "", "")
	if job.llmDebug != nil {
		t.Error("expected llmDebug to be nil when DebugLogging is false")
	}

	_, _, job = processEmail(t.Context(), llmClient, account, msg, prompts, labelCache, true, "", "")
	if job.llmDebug == nil {
		t.Error("expected llmDebug to be populated when DebugLogging is true")
	}
}

func TestProcessEmail_NoMatchWritesSentinelHistory(t *testing.T) {
	store := newTestStore(t)
	llmClient := newLLMServer(t, `{"1": false}`)

	accID, _ := store.UpsertAccount(t.Context(), db.UpsertAccountParams{Email: "test@example.com"})
	account := db.Account{ID: accID, Email: "test@example.com", Active: 1}
	msg := gmailpkg.Message{ID: "nomatch1", Subject: "No Match"}
	prompts := []db.Prompt{{ID: 1, Name: "P", Active: 1, Instructions: "x"}}

	_, _, job := processEmail(t.Context(), llmClient, account, msg, prompts, nil, false, "", "")
	applyWriteJob(t.Context(), store, job)

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
// classifyConcurrency
// ============================================================

func TestClassifyConcurrency(t *testing.T) {
	tests := []struct {
		in   int
		want int
	}{
		{0, 1},
		{-5, 1},
		{1, 1},
		{6, 6},
	}
	for _, tc := range tests {
		if got := classifyConcurrency(tc.in); got != tc.want {
			t.Errorf("classifyConcurrency(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// ============================================================
// Concurrent classification (mirrors the worker-pool fan-out in processMessageIDs)
// ============================================================

// TestProcessEmail_ConcurrentFanOut drives processEmail from many goroutines against a
// shared LLM client and label cache — the same setup processMessageIDs now uses to classify
// a batch of emails in parallel instead of one at a time — while a single writer goroutine
// drains their writeJobs sequentially, mirroring processMessageIDs' jobCh pattern that keeps
// DynamoDB writes serialized (one in flight at a time) regardless of classify concurrency.
// It asserts every email's classify result and DB write landed correctly regardless of
// goroutine scheduling, and (run with -race) that the shared FakeStore, the writer, and the
// mutex-guarded accumulators are race-free.
func TestProcessEmail_ConcurrentFanOut(t *testing.T) {
	store := newTestStore(t)
	llmClient := newLLMServer(t, `{"1": true}`)
	account := newTestAccount()
	prompts := []db.Prompt{
		{ID: 10, Name: "Newsletter", LabelName: "newsletters", Active: 1, Instructions: "label newsletters"},
	}
	labelCache := map[string]string{"newsletters": "Label_42"}

	const n = 20
	const concurrency = 6
	sem := make(chan struct{}, classifyConcurrency(concurrency))
	var wg sync.WaitGroup
	var mu sync.Mutex
	seen := make(map[string]bool, n)

	jobCh := make(chan writeJob, concurrency)
	var writerWG sync.WaitGroup
	writerWG.Add(1)
	go func() {
		defer writerWG.Done()
		for job := range jobCh {
			applyWriteJob(t.Context(), store, job)
		}
	}()

	for i := range n {
		msg := gmailpkg.Message{ID: fmt.Sprintf("msg%d", i), Subject: "Newsletter", Sender: "news@test.com", Body: "content"}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			modifies, _, job := processEmail(t.Context(), llmClient, account, msg, prompts, labelCache, false, "", "")

			mu.Lock()
			for _, m := range modifies {
				if contains(m.AddLabels, "Label_42") && len(m.MessageIDs) > 0 {
					seen[m.MessageIDs[0]] = true
				}
			}
			mu.Unlock()

			jobCh <- job
		}()
	}
	wg.Wait()
	close(jobCh)
	writerWG.Wait()

	if len(seen) != n {
		t.Fatalf("expected %d labeled messages, got %d: %v", n, len(seen), seen)
	}

	// Every message's write should have landed via the serialized writer: marked
	// processed and present in history.
	var ids []string
	for i := range n {
		ids = append(ids, fmt.Sprintf("msg%d", i))
	}
	unprocessed, err := store.FilterUnprocessed(t.Context(), account.ID, ids)
	if err != nil {
		t.Fatalf("FilterUnprocessed: %v", err)
	}
	if len(unprocessed) != 0 {
		t.Errorf("expected all %d messages marked processed, still unprocessed: %v", n, unprocessed)
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
