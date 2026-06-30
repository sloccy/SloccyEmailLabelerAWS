package llm

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sloccy/ollamail/db"
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

// newTestClient creates a Client pointed at the given test server.
func newTestClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	return NewClient(srv.URL, "test-model", 4096, 5*time.Second)
}

// ============================================================
// Pure helpers
// ============================================================

func TestFenceRe(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "no fence", input: `{"a":1}`, want: `{"a":1}`},
		{name: "json fence", input: "```json\n{\"a\":1}\n```", want: `{"a":1}`},
		{name: "plain fence", input: "```\n{\"a\":1}\n```", want: `{"a":1}`},
		{name: "leading whitespace after fence", input: "```json  \n{\"a\":1}\n```", want: `{"a":1}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := strings.TrimSpace(fenceRe.ReplaceAllString(tc.input, ""))
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBlankRunRe(t *testing.T) {
	input := "line1\n\n\n\nline2\n\n\nline3"
	got := blankRunRe.ReplaceAllString(input, "\n\n")
	if strings.Count(got, "\n\n\n") > 0 {
		t.Errorf("still has 3+ consecutive newlines: %q", got)
	}
	if !strings.Contains(got, "line1") || !strings.Contains(got, "line2") || !strings.Contains(got, "line3") {
		t.Errorf("content lost: %q", got)
	}
}

func TestBuildBody(t *testing.T) {
	email := Email{Sender: "s@test.com", Subject: "Hello", Body: "Test body"}
	prompts := []Prompt{
		{ID: 1, Name: "Newsletters", Instructions: "Label newsletters"},
		{ID: 2, Name: "Promos", Instructions: "Label promotions"},
	}
	body := buildBody(email, prompts)
	if !strings.Contains(body, "1. Newsletters") {
		t.Error("missing prompt 1")
	}
	if !strings.Contains(body, "2. Promos") {
		t.Error("missing prompt 2")
	}
	if !strings.Contains(body, "s@test.com") {
		t.Error("missing sender")
	}
	if !strings.Contains(body, "Hello") {
		t.Error("missing subject")
	}
	if !strings.Contains(body, "Test body") {
		t.Error("missing body")
	}
}

func TestBuildBody_UsesSnippetWhenBodyEmpty(t *testing.T) {
	email := Email{Sender: "s@test.com", Subject: "Sub", Body: "", Snippet: "snippet text"}
	body := buildBody(email, []Prompt{{ID: 1, Name: "P", Instructions: "do stuff"}})
	if !strings.Contains(body, "snippet text") {
		t.Error("should use snippet when body is empty")
	}
}

func TestBuildClassifyRequestJSON_Empty(t *testing.T) {
	c := NewClient("http://localhost", "m", 4096, time.Second)
	got := c.BuildClassifyRequestJSON(Email{}, nil)
	if got != "" {
		t.Errorf("expected empty string for 0 prompts, got %q", got)
	}
}

func TestBuildClassifyRequestJSON_Valid(t *testing.T) {
	c := NewClient("http://localhost", "mymodel", 4096, time.Second)
	prompts := []Prompt{{ID: 1, Name: "N", Instructions: "newsletters"}}
	raw := c.BuildClassifyRequestJSON(Email{Sender: "s@x.com", Subject: "Hi"}, prompts)
	if raw == "" {
		t.Fatal("expected non-empty JSON")
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if payload["model"] != "mymodel" {
		t.Errorf("model = %v", payload["model"])
	}
}

// ============================================================
// modelExists / EnsureModelPulled
// ============================================================

func TestModelExists_200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/show" {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv)
	exists, err := c.modelExists(t.Context())
	if err != nil {
		t.Fatalf("modelExists: %v", err)
	}
	if !exists {
		t.Error("expected exists=true for 200 response")
	}
}

func TestModelExists_404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv)
	exists, err := c.modelExists(t.Context())
	if err != nil {
		t.Fatalf("modelExists: %v", err)
	}
	if exists {
		t.Error("expected exists=false for 404 response")
	}
}

// ============================================================
// doChat
// ============================================================

func chatResp(content string) []byte {
	b, _ := json.Marshal(map[string]any{ //nolint:errchkjson
		"message": map[string]string{"content": content},
	})
	return b
}

func TestDoChat_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(chatResp(`{"1": true}`)) //nolint:errcheck,gosec
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv)
	got, err := c.doChat(t.Context(), map[string]any{"model": "m"})
	if err != nil {
		t.Fatalf("doChat: %v", err)
	}
	if got != `{"1": true}` {
		t.Errorf("got %q", got)
	}
}

func TestDoChat_FencedJSON(t *testing.T) {
	fenced := "```json\n{\"1\": true}\n```"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(chatResp(fenced)) //nolint:errcheck,gosec
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv)
	raw, err := c.doChat(t.Context(), map[string]any{})
	if err != nil {
		t.Fatalf("doChat: %v", err)
	}
	// doChat returns the raw content; the caller strips fences.
	if raw != fenced {
		t.Errorf("doChat returned %q", raw)
	}
	// Verify that fenceRe strips the fences correctly.
	stripped := strings.TrimSpace(fenceRe.ReplaceAllString(raw, ""))
	if stripped != `{"1": true}` {
		t.Errorf("fenceRe.Replace = %q", stripped)
	}
}

func TestDoChat_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv)
	_, err := c.doChat(t.Context(), map[string]any{})
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}

// ============================================================
// ClassifyEmailBatch
// ============================================================

func TestClassifyEmailBatch_HappyPath(t *testing.T) {
	store := newTestStore(t)
	prompts := []Prompt{
		{ID: 10, Name: "Newsletter", Instructions: "label newsletters"},
		{ID: 20, Name: "Promo", Instructions: "label promos"},
	}
	email := Email{Sender: "s@test.com", Subject: "Hello", Body: "body"}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// 1-based: prompt 1 (ID=10) = true, prompt 2 (ID=20) = false
		w.Write(chatResp(`{"1": true, "2": false}`)) //nolint:errcheck,gosec
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv)
	res, err := c.ClassifyEmailBatch(t.Context(), store, email, prompts)
	if err != nil {
		t.Fatalf("ClassifyEmailBatch: %v", err)
	}
	if !res.Results[10] {
		t.Error("prompt ID 10 should be true")
	}
	if res.Results[20] {
		t.Error("prompt ID 20 should be false")
	}
}

func TestClassifyEmailBatch_Empty(t *testing.T) {
	store := newTestStore(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("unexpected HTTP call: empty prompts should short-circuit before any request")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL, "m", 4096, time.Second)
	res, err := c.ClassifyEmailBatch(t.Context(), store, Email{}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Results != nil {
		t.Error("expected nil results for empty prompts")
	}
}

func TestClassifyEmailBatch_LLMError(t *testing.T) {
	store := newTestStore(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "server down", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv)
	prompts := []Prompt{{ID: 1, Name: "P", Instructions: "do stuff"}}
	_, err := c.ClassifyEmailBatch(t.Context(), store, Email{Subject: "x"}, prompts)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestClassifyEmailBatch_ParseError(t *testing.T) {
	store := newTestStore(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(chatResp("not valid json at all")) //nolint:errcheck,gosec
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv)
	prompts := []Prompt{{ID: 1, Name: "P", Instructions: "do stuff"}}
	_, err := c.ClassifyEmailBatch(t.Context(), store, Email{Subject: "x"}, prompts)
	if err == nil {
		t.Fatal("expected parse error")
	}
}

// ============================================================
// Streaming (StreamGeneratePromptInstruction)
// ============================================================

func TestStreamGenerate_HappyPath(t *testing.T) {
	frames := []map[string]any{
		{"message": map[string]string{"content": "Hello "}, "done": false},
		{"message": map[string]string{"content": "world"}, "done": false},
		{"message": map[string]string{"content": ""}, "done": true},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		for _, f := range frames {
			b, _ := json.Marshal(f)
			fmt.Fprintf(w, "%s\n", b) //nolint:errcheck
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv)
	ch := c.StreamGeneratePromptInstruction(t.Context(), "newsletter emails")
	var buf strings.Builder
	for chunk := range ch {
		if chunk.Err != nil {
			t.Fatalf("stream error: %v", chunk.Err)
		}
		buf.WriteString(chunk.Text)
	}
	if buf.String() != "Hello world" {
		t.Errorf("got %q", buf.String())
	}
}

func TestStreamGenerate_MalformedFrame(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// First frame is malformed JSON, second is valid with done=true.
		fmt.Fprintf(w, "not-json\n") //nolint:errcheck
		b, _ := json.Marshal(map[string]any{"message": map[string]string{"content": "ok"}, "done": true})
		fmt.Fprintf(w, "%s\n", b) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv)
	ch := c.StreamGeneratePromptInstruction(t.Context(), "test")
	var got strings.Builder
	for chunk := range ch {
		if chunk.Err == nil {
			got.WriteString(chunk.Text)
		}
	}
	// Malformed frame is skipped; valid frame yields "ok".
	if got.String() != "ok" {
		t.Errorf("got %q, want %q", got.String(), "ok")
	}
}

// ============================================================
// Timeout propagation
// ============================================================

func TestTimeout_ContextDeadline(t *testing.T) {
	// The handler holds the response open for 500ms; the client times out after 50ms.
	// httptest.Server.Close waits at most 500ms for the handler to finish.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL, "m", 4096, 50*time.Millisecond)
	_, err := c.doChat(t.Context(), map[string]any{})
	if err == nil {
		t.Fatal("expected timeout error")
	}
}
