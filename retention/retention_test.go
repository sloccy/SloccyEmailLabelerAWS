package retention

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/sloccy/ollamail/db"
	gmailpkg "github.com/sloccy/ollamail/gmail"
)

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
		json.NewEncoder(w).Encode(map[string]any{"messages": []any{}, "nextPageToken": ""}) //nolint:errcheck,gosec
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
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"messages": []map[string]string{{"id": "old1"}, {"id": "old2"}},
			})
		} else {
			json.NewEncoder(w).Encode(map[string]any{"messages": []any{}}) //nolint:errcheck,gosec
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
	store.AddLabelRetention(t.Context(), db.AddLabelRetentionParams{ //nolint:errcheck,gosec
		AccountID: accID, LabelName: "newsletters", Days: 7,
	})
	store.AddLabelExemption(t.Context(), db.AddLabelExemptionParams{ //nolint:errcheck,gosec
		AccountID: accID, LabelName: "newsletters",
	})

	mux := http.NewServeMux()
	mux.HandleFunc("/messages", func(w http.ResponseWriter, _ *http.Request) {
		t.Error("/messages should not be called for exempt label — cleanup should skip it")
		json.NewEncoder(w).Encode(map[string]any{"messages": []any{}}) //nolint:errcheck,gosec
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
	store.SetGlobalRetention(t.Context(), db.SetGlobalRetentionParams{ //nolint:errcheck,gosec
		AccountID:  accID,
		GlobalDays: sql.NullInt64{Int64: 60, Valid: true},
	})

	var trashCalled atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/messages", func(w http.ResponseWriter, _ *http.Request) {
		if trashCalled.Load() == 0 {
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"messages": []map[string]string{{"id": "global1"}},
			})
		} else {
			json.NewEncoder(w).Encode(map[string]any{"messages": []any{}}) //nolint:errcheck,gosec
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
