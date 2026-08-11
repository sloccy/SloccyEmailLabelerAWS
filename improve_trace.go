package main

// ============================================================
// Suggestion trace writer
// ============================================================
//
// Before this file, an in-flight improve round was completely opaque: the suggestion sat
// on status "generating" and nothing about what the worker was actually doing — which
// round, what the model had written so far, whether replay had even started — was visible
// until a terminal status landed. traceWriter narrates a round live by appending bounded,
// batched events to DynamoDB (db.SuggestionTraceEvent, PK = SUGG_TRACE#<suggestionId>),
// which the browser polls with a cursor (see the trace endpoint in server.go) — no SSE, no
// change to WebFunction's Function URL invoke mode, and the trace survives a page refresh
// or a closed tab the way an in-memory channel never could.
//
// Every write here is best-effort: a trace-write failure is logged and swallowed, never
// returned to the caller, because narrating a round must never be able to fail the round
// itself — see writeFailure and improveAndFinalizeSuggestion's own terminal writes, which
// this must not interfere with even if DynamoDB is unavailable.

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/sloccy/ollamail-aws/db"
	"github.com/sloccy/ollamail-aws/llm"
)

// traceStore is the slice of db.Store the trace writer needs. Declared locally rather than
// added to db.StoreIface — that interface is the processor/retention contract (see its own
// doc comment), and neither of those consumers care about a suggestion's live trace.
// *db.Store satisfies this implicitly.
type traceStore interface {
	AppendSuggestionTrace(ctx context.Context, suggestionID int64, events []db.SuggestionTraceEvent) error
}

const (
	// traceFlushBytes caps how much streamed text a coalesced answer/thinking item holds
	// before it's flushed, keeping each flush at roughly one DynamoDB WCU (1KB items).
	traceFlushBytes = 400
	// traceFlushInterval is the longest streamed text sits buffered before being flushed
	// even if traceFlushBytes hasn't been reached — matches the browser's poll interval
	// (see the trace endpoint), so a watching client sees roughly one update per poll
	// rather than waiting on a slow stream to fill a buffer.
	traceFlushInterval = 1500 * time.Millisecond
)

// traceWriter buffers one suggestion's live progress and flushes it to DynamoDB in bounded
// batches. One is created per improveRunner.runOne call (see newTraceWriter) and threaded
// through the whole improve+replay round.
type traceWriter struct {
	store        traceStore
	suggestionID int64

	mu        sync.Mutex
	seq       int64
	round     int64
	pendKind  string
	pendText  strings.Builder
	lastFlush time.Time
}

// newTraceWriter starts a trace writer at startSeq — the highest Seq already written for
// this suggestion (0 for a brand-new one). This is not cosmetic: a regenerate re-invokes
// the worker from scratch, and a traceWriter that always started at 0 would silently
// overwrite the previous round's Seq 1..N items (same PK+SK) instead of continuing the
// sequence, corrupting the trace and leaving the polling endpoint's cursor (already past
// those seqs from the first round) unable to ever see the new round's events. Callers get
// startSeq from db.Store.LatestSuggestionTraceSeq.
func newTraceWriter(store traceStore, suggestionID, startSeq int64) *traceWriter {
	return &traceWriter{store: store, suggestionID: suggestionID, seq: startSeq, lastFlush: time.Now()}
}

// nextLocked builds the next trace event and bumps seq. Caller must hold t.mu.
func (t *traceWriter) nextLocked(kind string, round int64, text string) db.SuggestionTraceEvent {
	t.seq++
	return db.SuggestionTraceEvent{Seq: t.seq, CreatedAt: db.Now(), Kind: kind, Round: round, Text: text}
}

// drainPendingLocked pops the coalesced buffer, if any, as a trace event and resets it.
// Caller must hold t.mu. ok is false when there was nothing pending — the common case,
// since most Event calls happen between deltas rather than mid-buffer.
func (t *traceWriter) drainPendingLocked() (ev db.SuggestionTraceEvent, ok bool) {
	if t.pendKind == "" || t.pendText.Len() == 0 {
		return db.SuggestionTraceEvent{}, false
	}
	ev = t.nextLocked(t.pendKind, t.round, t.pendText.String())
	t.pendKind = ""
	t.pendText.Reset()
	t.lastFlush = time.Now()
	return ev, true
}

// Event records a structural, non-coalesced event — round boundaries, the sanitized
// candidate, replay start/done, notes, and the terminal done/error — and flushes
// immediately, together with whatever streamed text was still buffered, in one batch
// write. The UI must never wait on a byte/time threshold to learn a round changed state.
func (t *traceWriter) Event(ctx context.Context, kind string, round int64, text string) {
	t.mu.Lock()
	events := make([]db.SuggestionTraceEvent, 0, 2)
	if pending, ok := t.drainPendingLocked(); ok {
		events = append(events, pending)
	}
	t.round = round
	events = append(events, t.nextLocked(kind, round, text))
	t.mu.Unlock()
	t.write(ctx, events)
}

// Text records one streamed delta — kind is db.TraceKindAnswer or db.TraceKindThinking —
// coalescing into a per-kind buffer until traceFlushBytes or traceFlushInterval trips.
// Switching kinds mid-buffer (an answer delta arriving while a thinking buffer is
// pending, or vice versa) flushes what was pending first, so the two never end up
// concatenated together into one item.
func (t *traceWriter) Text(ctx context.Context, kind string, round int64, delta string) {
	if delta == "" {
		return
	}
	t.mu.Lock()
	var events []db.SuggestionTraceEvent
	if t.pendKind != "" && t.pendKind != kind {
		if ev, ok := t.drainPendingLocked(); ok {
			events = append(events, ev)
		}
	}
	t.pendKind = kind
	t.round = round
	t.pendText.WriteString(delta)
	if t.pendText.Len() >= traceFlushBytes || time.Since(t.lastFlush) >= traceFlushInterval {
		if ev, ok := t.drainPendingLocked(); ok {
			events = append(events, ev)
		}
	}
	t.mu.Unlock()
	if len(events) > 0 {
		t.write(ctx, events)
	}
}

// Sink adapts Text to llm.ImproveSink for one round, so ImprovePromptInstructions can be
// driven directly by trace.Sink(ctx, round).
func (t *traceWriter) Sink(ctx context.Context, round int64) llm.ImproveSink {
	return func(c llm.StreamChunk) {
		if c.Text != "" {
			t.Text(ctx, db.TraceKindAnswer, round, c.Text)
		}
		if c.Reasoning != "" {
			t.Text(ctx, db.TraceKindThinking, round, c.Reasoning)
		}
	}
}

// write appends events to the store, best-effort — see this file's package doc comment.
func (t *traceWriter) write(ctx context.Context, events []db.SuggestionTraceEvent) {
	if len(events) == 0 {
		return
	}
	if err := t.store.AppendSuggestionTrace(ctx, t.suggestionID, events); err != nil {
		slog.Error("suggestion trace: append failed", "suggestion_id", t.suggestionID, "err", err)
	}
}
