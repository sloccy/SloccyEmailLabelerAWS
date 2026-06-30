package gmail

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// newTestClient returns a Client backed by the given httptest.Server.
// It also swaps gmailBase to the server URL and restores it on cleanup.
func newTestClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	old := gmailBase
	gmailBase = srv.URL
	t.Cleanup(func() { gmailBase = old })
	return &Client{http: &http.Client{}}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v) //nolint:errchkjson
}

// ============================================================
// Client.get / Client.post
// ============================================================

func TestClientGet_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "want GET", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, map[string]string{"labels": "[]"})
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv)
	var out map[string]string
	if err := c.get(t.Context(), "/anything", nil, &out); err != nil {
		t.Fatalf("get: %v", err)
	}
}

func TestClientGet_ErrorEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv)
	var out any
	err := c.get(t.Context(), "/anything", nil, &out)
	if err == nil {
		t.Fatal("expected error for 403 response")
	}
}

func TestClientPost_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "want POST", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, map[string]string{"id": "L1", "name": "test"})
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv)
	var out map[string]string
	if err := c.post(t.Context(), "/labels", map[string]string{"name": "test"}, &out); err != nil {
		t.Fatalf("post: %v", err)
	}
	if out["id"] != "L1" {
		t.Errorf("id = %q", out["id"])
	}
}

func TestClientPost_ErrorEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv)
	err := c.post(t.Context(), "/labels", nil, nil)
	if err == nil {
		t.Fatal("expected error for 400 response")
	}
}

// ============================================================
// retryTransport
// ============================================================

func TestRetryTransport_200OnFirstAttempt(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	rt := &retryTransport{base: http.DefaultTransport}
	hc := &http.Client{Transport: rt}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_ = resp.Body.Close()
	if calls.Load() != 1 {
		t.Errorf("expected 1 call, got %d", calls.Load())
	}
}

// TestRetryTransport_RetryOn500 verifies that a 500 followed by 200 succeeds.
func TestRetryTransport_RetryOn500(t *testing.T) {
	old := retryBaseBackoff
	retryBaseBackoff = time.Millisecond
	t.Cleanup(func() { retryBaseBackoff = old })

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	rt := &retryTransport{base: http.DefaultTransport}
	hc := &http.Client{Transport: rt}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}
	if calls.Load() != 2 {
		t.Errorf("expected 2 calls, got %d", calls.Load())
	}
}

// ============================================================
// ListLabels
// ============================================================

func TestListLabels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/labels" {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, apiListLabelsResponse{
			Labels: []apiLabel{
				{ID: "L2", Name: "Zeta"},
				{ID: "L1", Name: "Alpha"},
			},
		})
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv)
	labels, err := ListLabels(t.Context(), c)
	if err != nil {
		t.Fatalf("ListLabels: %v", err)
	}
	if len(labels) != 2 {
		t.Fatalf("len = %d", len(labels))
	}
	// Should be sorted by name.
	if labels[0].Name != "Alpha" || labels[1].Name != "Zeta" {
		t.Errorf("not sorted: %v", labels)
	}
}

func TestListLabels_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv)
	_, err := ListLabels(t.Context(), c)
	if err == nil {
		t.Fatal("expected error")
	}
}

// ============================================================
// BuildLabelCache
// ============================================================

func TestBuildLabelCache_CreatesNewLabel(t *testing.T) {
	var postCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/labels":
			writeJSON(w, apiListLabelsResponse{Labels: []apiLabel{{ID: "L1", Name: "existing"}}})
		case r.Method == http.MethodPost && r.URL.Path == "/labels":
			postCalls.Add(1)
			writeJSON(w, apiLabel{ID: "L99", Name: "new-label"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv)
	cache, err := BuildLabelCache(t.Context(), c, []string{"existing", "new-label"})
	if err != nil {
		t.Fatalf("BuildLabelCache: %v", err)
	}
	if cache["existing"] != "L1" {
		t.Errorf("existing label ID = %q", cache["existing"])
	}
	if cache["new-label"] != "L99" {
		t.Errorf("new-label ID = %q", cache["new-label"])
	}
	if postCalls.Load() != 1 {
		t.Errorf("expected 1 POST call for new label, got %d", postCalls.Load())
	}
}

// ============================================================
// ListRecentMessageIDs (single page)
// ============================================================

func TestListRecentMessageIDs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, apiListMessagesResponse{
			Messages: []apiMessageRef{{ID: "m1"}, {ID: "m2"}},
		})
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv)
	ids, err := ListRecentMessageIDs(t.Context(), c, 24, 50)
	if err != nil {
		t.Fatalf("ListRecentMessageIDs: %v", err)
	}
	if len(ids) != 2 {
		t.Errorf("len = %d", len(ids))
	}
}

// ============================================================
// FetchMessage
// ============================================================

func TestFetchMessage(t *testing.T) {
	body64 := b64("<p>Hello world</p>")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, apiMessage{
			ID:      "m1",
			Snippet: "Hello world",
			Payload: &apiMessagePart{
				MimeType: "text/html",
				Headers: []struct {
					Name  string `json:"name"`
					Value string `json:"value"`
				}{
					{Name: "Subject", Value: "Test Email"},
					{Name: "From", Value: "sender@example.com"},
				},
				Body: &apiMessagePartBody{Data: body64},
			},
		})
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv)
	msg, err := FetchMessage(t.Context(), c, "m1", 0)
	if err != nil {
		t.Fatalf("FetchMessage: %v", err)
	}
	if msg.Subject != "Test Email" {
		t.Errorf("Subject = %q", msg.Subject)
	}
	if msg.Sender != "sender@example.com" {
		t.Errorf("Sender = %q", msg.Sender)
	}
	if msg.ID != "m1" {
		t.Errorf("ID = %q", msg.ID)
	}
}

// ============================================================
// BatchModifyEmails
// ============================================================

func TestBatchModifyEmails(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/messages/batchModify" {
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv)
	mods := []Modify{
		{MessageIDs: []string{"m1", "m2"}, AddLabels: []string{"Label_1"}, RemoveLabels: []string{"INBOX"}},
		{MessageIDs: []string{"m3"}, AddLabels: []string{"Label_1"}, RemoveLabels: []string{"INBOX"}},
	}
	if err := BatchModifyEmails(t.Context(), c, mods); err != nil {
		t.Fatalf("BatchModifyEmails: %v", err)
	}
	// Same add/remove set → grouped into one request.
	if calls.Load() != 1 {
		t.Errorf("expected 1 batchModify call (grouped), got %d", calls.Load())
	}
}

func TestBatchModifyEmails_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "server error", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv)
	err := BatchModifyEmails(t.Context(), c, []Modify{{MessageIDs: []string{"m1"}}})
	if err == nil {
		t.Fatal("expected error")
	}
}

// ============================================================
// BatchTrashEmails
// ============================================================

func TestBatchTrashEmails(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv)
	// 3 IDs → one batch (< 1000).
	if err := BatchTrashEmails(t.Context(), c, []string{"m1", "m2", "m3"}); err != nil {
		t.Fatalf("BatchTrashEmails: %v", err)
	}
	if calls.Load() != 1 {
		t.Errorf("expected 1 call, got %d", calls.Load())
	}
}

// ============================================================
// FetchEmailsOlderThan
// ============================================================

func TestFetchEmailsOlderThan(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		if q == "" {
			http.Error(w, "missing q", http.StatusBadRequest)
			return
		}
		writeJSON(w, apiListMessagesResponse{
			Messages: []apiMessageRef{{ID: "old1"}, {ID: "old2"}},
		})
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv)
	ids, err := FetchEmailsOlderThan(t.Context(), c, 30, "newsletters", nil, 1)
	if err != nil {
		t.Fatalf("FetchEmailsOlderThan: %v", err)
	}
	if len(ids) != 2 {
		t.Errorf("len = %d", len(ids))
	}
}

// ============================================================
// Pagination
// ============================================================

func TestPaginateMessageIDs_MultiPage(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			writeJSON(w, apiListMessagesResponse{
				Messages:      []apiMessageRef{{ID: "p1m1"}, {ID: "p1m2"}},
				NextPageToken: "tok2",
			})
		} else {
			writeJSON(w, apiListMessagesResponse{
				Messages: []apiMessageRef{{ID: "p2m1"}},
			})
		}
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv)
	ids, err := paginateMessageIDs(t.Context(), c, "in:inbox", 50, 0)
	if err != nil {
		t.Fatalf("paginateMessageIDs: %v", err)
	}
	if len(ids) != 3 {
		t.Errorf("len = %d, want 3; ids=%v", len(ids), ids)
	}
	if calls.Load() != 2 {
		t.Errorf("expected 2 page requests, got %d", calls.Load())
	}
}

func TestPaginateMessageIDs_MaxPages(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeJSON(w, apiListMessagesResponse{
			Messages:      []apiMessageRef{{ID: fmt.Sprintf("m%d", calls.Load())}},
			NextPageToken: "always-more",
		})
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv)
	ids, err := paginateMessageIDs(t.Context(), c, "in:inbox", 50, 2)
	if err != nil {
		t.Fatalf("paginateMessageIDs: %v", err)
	}
	if len(ids) != 2 {
		t.Errorf("len = %d, want 2; ids=%v", len(ids), ids)
	}
	if calls.Load() != 2 {
		t.Errorf("expected exactly 2 page requests (maxPages=2), got %d", calls.Load())
	}
}
