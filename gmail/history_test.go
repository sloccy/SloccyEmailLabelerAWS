package gmail

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestListHistoryAddedMessageIDs_FiltersAndDedupes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Assert we ask Gmail to filter server-side to inbox message-adds.
		q := r.URL.Query()
		if q.Get("historyTypes") != "messageAdded" || q.Get("labelId") != LabelInbox {
			http.Error(w, "missing filters", http.StatusBadRequest)
			return
		}
		if q.Get("startHistoryId") != "100" {
			http.Error(w, "wrong start", http.StatusBadRequest)
			return
		}
		// Two records; "m1" appears twice (should dedupe).
		writeJSON(w, apiHistoryResponse{
			History: []apiHistoryRecord{
				{MessagesAdded: []apiHistoryMessageAdded{{Message: apiMessageRef{ID: "m1"}}}},
				{MessagesAdded: []apiHistoryMessageAdded{{Message: apiMessageRef{ID: "m2"}}, {Message: apiMessageRef{ID: "m1"}}}},
			},
			HistoryID: "175",
		})
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv)
	ids, latest, err := ListHistoryAddedMessageIDs(t.Context(), c, "100")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if latest != "175" {
		t.Errorf("latest = %q, want 175", latest)
	}
	if len(ids) != 2 || ids[0] != "m1" || ids[1] != "m2" {
		t.Errorf("ids = %v, want [m1 m2]", ids)
	}
}

func TestListHistoryAddedMessageIDs_TooOld(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv)
	_, _, err := ListHistoryAddedMessageIDs(t.Context(), c, "1")
	if !errors.Is(err, ErrHistoryTooOld) {
		t.Fatalf("err = %v, want ErrHistoryTooOld", err)
	}
}

func TestListHistoryAddedMessageIDs_Paginates(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Query().Get("pageToken") == "" {
			writeJSON(w, apiHistoryResponse{
				History:       []apiHistoryRecord{{MessagesAdded: []apiHistoryMessageAdded{{Message: apiMessageRef{ID: "a"}}}}},
				NextPageToken: "p2",
				HistoryID:     "200",
			})
			return
		}
		writeJSON(w, apiHistoryResponse{
			History:   []apiHistoryRecord{{MessagesAdded: []apiHistoryMessageAdded{{Message: apiMessageRef{ID: "b"}}}}},
			HistoryID: "201",
		})
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv)
	ids, latest, err := ListHistoryAddedMessageIDs(t.Context(), c, "50")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
	if len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
		t.Errorf("ids = %v, want [a b]", ids)
	}
	if latest != "201" {
		t.Errorf("latest = %q, want 201", latest)
	}
}
