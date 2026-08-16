package retention

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/sloccy/ollamail-aws/db"
	gmailpkg "github.com/sloccy/ollamail-aws/gmail"
)

func newTestStore(t *testing.T) *db.FakeStore {
	t.Helper()
	return db.NewFake()
}

// gmailServer sets up a fake Gmail API server and returns a gmail.Client backed by it.
func gmailServer(t *testing.T, mux *http.ServeMux) *gmailpkg.Client {
	t.Helper()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	restore := gmailpkg.SetBaseURLForTest(srv.URL)
	t.Cleanup(restore)
	return gmailpkg.NewTestClient()
}

// ============================================================
// No retention rules → no HTTP calls
// ============================================================

func TestCleanup_NoRules(t *testing.T) {
	store := newTestStore(t)
	accID, _ := store.UpsertAccount(t.Context(), db.UpsertAccountParams{Email: "a@test.com"})

	var trashCalled atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/messages/batchModify", func(w http.ResponseWriter, _ *http.Request) {
		trashCalled.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	// Paginate handler: return empty results.
	mux.HandleFunc("/messages", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"messages": []any{}, "nextPageToken": ""})
	})

	svc := gmailServer(t, mux)
	Cleanup(t.Context(), store, svc, accID)

	if trashCalled.Load() != 0 {
		t.Errorf("expected 0 trash calls for account with no rules, got %d", trashCalled.Load())
	}
}

// ============================================================
// Label retention rule: old emails are trashed
// ============================================================

func TestCleanup_LabelRule_TrashesOldMessages(t *testing.T) {
	store := newTestStore(t)
	accID, _ := store.UpsertAccount(t.Context(), db.UpsertAccountParams{Email: "b@test.com"})

	// Add a label retention rule: newsletters older than 30 days.
	if err := store.AddLabelRetention(t.Context(), db.AddLabelRetentionParams{
		AccountID: accID,
		LabelName: "newsletters",
		Days:      30,
	}); err != nil {
		t.Fatalf("AddLabelRetention: %v", err)
	}

	var trashCalled atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/messages", func(w http.ResponseWriter, _ *http.Request) {
		// Return 2 old message IDs on first call; empty on subsequent calls.
		if trashCalled.Load() == 0 {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"messages": []map[string]string{{"id": "old1"}, {"id": "old2"}},
			})
		} else {
			_ = json.NewEncoder(w).Encode(map[string]any{"messages": []any{}})
		}
	})
	mux.HandleFunc("/messages/batchModify", func(w http.ResponseWriter, _ *http.Request) {
		trashCalled.Add(1)
		w.WriteHeader(http.StatusOK)
	})

	svc := gmailServer(t, mux)
	Cleanup(t.Context(), store, svc, accID)

	if trashCalled.Load() == 0 {
		t.Error("expected at least one trash (batchModify) call")
	}
}

// ============================================================
// Label exemption: exempt labels are skipped
// ============================================================

func TestCleanup_ExemptLabel_Skipped(t *testing.T) {
	store := newTestStore(t)
	accID, _ := store.UpsertAccount(t.Context(), db.UpsertAccountParams{Email: "c@test.com"})

	// Add a retention rule AND an exemption for the same label.
	if err := store.AddLabelRetention(t.Context(), db.AddLabelRetentionParams{
		AccountID: accID, LabelName: "newsletters", Days: 7,
	}); err != nil {
		t.Fatalf("AddLabelRetention: %v", err)
	}
	if err := store.AddLabelExemption(t.Context(), db.AddLabelExemptionParams{
		AccountID: accID, LabelName: "newsletters",
	}); err != nil {
		t.Fatalf("AddLabelExemption: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/messages", func(w http.ResponseWriter, _ *http.Request) {
		t.Error("/messages should not be called for exempt label — cleanup should skip it")
		_ = json.NewEncoder(w).Encode(map[string]any{"messages": []any{}})
	})
	mux.HandleFunc("/messages/batchModify", func(w http.ResponseWriter, _ *http.Request) {
		t.Error("batchModify should not be called for exempt label")
	})

	svc := gmailServer(t, mux)
	Cleanup(t.Context(), store, svc, accID)

	// Reaching here without t.Error means the exempt label's fetch was correctly skipped.
}

// ============================================================
// Global retention rule
// ============================================================

func TestCleanup_GlobalRetention(t *testing.T) {
	store := newTestStore(t)
	accID, _ := store.UpsertAccount(t.Context(), db.UpsertAccountParams{Email: "d@test.com"})

	// Set a global retention of 60 days.
	if err := store.SetGlobalRetention(t.Context(), db.SetGlobalRetentionParams{
		AccountID:  accID,
		GlobalDays: sql.NullInt64{Int64: 60, Valid: true},
	}); err != nil {
		t.Fatalf("SetGlobalRetention: %v", err)
	}

	var trashCalled atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/messages", func(w http.ResponseWriter, _ *http.Request) {
		if trashCalled.Load() == 0 {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"messages": []map[string]string{{"id": "global1"}},
			})
		} else {
			_ = json.NewEncoder(w).Encode(map[string]any{"messages": []any{}})
		}
	})
	mux.HandleFunc("/messages/batchModify", func(w http.ResponseWriter, _ *http.Request) {
		trashCalled.Add(1)
		w.WriteHeader(http.StatusOK)
	})

	svc := gmailServer(t, mux)
	Cleanup(t.Context(), store, svc, accID)

	if trashCalled.Load() == 0 {
		t.Error("expected trash call for global retention rule")
	}
}

func TestClampDays(t *testing.T) {
	cases := []struct {
		in   int64
		want int
	}{
		{-5, 1}, // negative would build a future before: date matching all mail
		{0, 1},
		{1, 1},
		{30, 30},
		{maxRetentionDays, maxRetentionDays},
		{maxRetentionDays + 1, maxRetentionDays},
		{1 << 40, maxRetentionDays}, // would truncate on 32-bit int without the clamp
	}
	for _, c := range cases {
		if got := clampDays(c.in); got != c.want {
			t.Errorf("clampDays(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

// ============================================================
// Global-retention lookup: absence vs failure
// ============================================================

// failingRetentionStore makes GetAccountRetention fail with something other than
// db.ErrNotFound, so the tests below can tell "no global rule" apart from a real
// lookup failure. Everything else is delegated to the real fake.
type failingRetentionStore struct {
	db.StoreIface
	err error
}

func (s failingRetentionStore) GetAccountRetention(context.Context, int64) (db.AccountRetention, error) {
	return db.AccountRetention{}, s.err
}

// A DynamoDB throttle or outage must not look like "nothing configured": swallowing it
// would silently skip global retention, leaving mail that should have been trashed.
func TestCleanupPropagatesGlobalRetentionLookupFailure(t *testing.T) {
	store := newTestStore(t)
	accID, _ := store.UpsertAccount(t.Context(), db.UpsertAccountParams{Email: "a@test.com"})

	mux := http.NewServeMux()
	mux.HandleFunc("/messages", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"messages": []any{}})
	})
	svc := gmailServer(t, mux)

	wantErr := errors.New("ProvisionedThroughputExceededException")
	err := cleanup(t.Context(), failingRetentionStore{StoreIface: store, err: wantErr}, svc, accID)
	if err == nil {
		t.Fatal("cleanup: want error for a failed retention lookup, got nil")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("cleanup: want error wrapping %v, got %v", wantErr, err)
	}
}

// An account with no global rule is a normal outcome, not a failure.
func TestCleanupTreatsMissingGlobalRuleAsSuccess(t *testing.T) {
	store := newTestStore(t)
	accID, _ := store.UpsertAccount(t.Context(), db.UpsertAccountParams{Email: "a@test.com"})

	mux := http.NewServeMux()
	mux.HandleFunc("/messages", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"messages": []any{}})
	})
	svc := gmailServer(t, mux)

	if err := cleanup(t.Context(), store, svc, accID); err != nil {
		t.Fatalf("cleanup: want nil for an account with no global rule, got %v", err)
	}
}
