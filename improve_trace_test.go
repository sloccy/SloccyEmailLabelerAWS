package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sloccy/ollamail-aws/db"
	"github.com/sloccy/ollamail-aws/llm"
)

// fakeTraceStore is a minimal traceStore double: it records every event handed to
// AppendSuggestionTrace, in the batches it arrived in, and can be told to fail every call
// (to prove a trace-write failure never propagates).
type fakeTraceStore struct {
	mu      sync.Mutex
	batches [][]db.SuggestionTraceEvent
	err     error
}

func (f *fakeTraceStore) AppendSuggestionTrace(_ context.Context, _ int64, events []db.SuggestionTraceEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.batches = append(f.batches, events)
	return nil
}

// all flattens every batch into one ordered slice, the shape most assertions want.
func (f *fakeTraceStore) all() []db.SuggestionTraceEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []db.SuggestionTraceEvent
	for _, b := range f.batches {
		out = append(out, b...)
	}
	return out
}

// TestTraceWriter_TextCoalescesUntilByteThreshold checks that small deltas accumulate into
// one buffered item instead of one DynamoDB write per delta — the whole point of
// coalescing, since a streamed answer can arrive in dozens of small chunks.
func TestTraceWriter_TextCoalescesUntilByteThreshold(t *testing.T) {
	store := &fakeTraceStore{}
	tw := newTraceWriter(store, 1, 0)
	ctx := context.Background()

	small := strings.Repeat("a", 10)
	for range 5 {
		tw.Text(ctx, db.TraceKindAnswer, 1, small)
	}
	if got := len(store.all()); got != 0 {
		t.Fatalf("expected no flush yet (50 bytes < traceFlushBytes=%d), got %d events written", traceFlushBytes, got)
	}

	// Push past the threshold.
	big := strings.Repeat("b", traceFlushBytes)
	tw.Text(ctx, db.TraceKindAnswer, 1, big)

	events := store.all()
	if len(events) != 1 {
		t.Fatalf("expected exactly one flushed event once the byte threshold trips, got %d", len(events))
	}
	want := strings.Repeat("a", 50) + big
	if events[0].Text != want {
		t.Errorf("flushed text = %q, want the full coalesced buffer %q", events[0].Text, want)
	}
	if events[0].Kind != db.TraceKindAnswer || events[0].Round != 1 {
		t.Errorf("flushed event = %+v, want Kind=answer Round=1", events[0])
	}
}

// TestTraceWriter_TextFlushesOnTimeThreshold checks the time-based flush path
// independently of byte volume — a slow trickle of tiny deltas must still surface within
// traceFlushInterval rather than waiting indefinitely for traceFlushBytes.
func TestTraceWriter_TextFlushesOnTimeThreshold(t *testing.T) {
	store := &fakeTraceStore{}
	tw := newTraceWriter(store, 1, 0)
	ctx := context.Background()

	tw.Text(ctx, db.TraceKindThinking, 1, "a little thinking")
	if got := len(store.all()); got != 0 {
		t.Fatalf("expected no flush yet, got %d events", got)
	}

	// Backdate lastFlush directly rather than sleeping traceFlushInterval in a test.
	tw.mu.Lock()
	tw.lastFlush = time.Now().Add(-traceFlushInterval - time.Millisecond)
	tw.mu.Unlock()

	tw.Text(ctx, db.TraceKindThinking, 1, " more")
	events := store.all()
	if len(events) != 1 {
		t.Fatalf("expected exactly one flushed event once the time threshold trips, got %d", len(events))
	}
	if events[0].Text != "a little thinking more" {
		t.Errorf("flushed text = %q, want the coalesced buffer", events[0].Text)
	}
}

// TestTraceWriter_EventFlushesPendingTextFirst checks that a structural event (round
// boundary, candidate, replay result, done) never leaves streamed text stranded in the
// buffer — it must go out in the same batch, ahead of the structural event, so the two
// stay in the order they actually happened.
func TestTraceWriter_EventFlushesPendingTextFirst(t *testing.T) {
	store := &fakeTraceStore{}
	tw := newTraceWriter(store, 1, 0)
	ctx := context.Background()

	tw.Text(ctx, db.TraceKindAnswer, 1, "partial answer")
	tw.Event(ctx, db.TraceKindCandidate, 1, "Match newsletters.")

	events := store.all()
	if len(events) != 2 {
		t.Fatalf("expected 2 events (flushed text + the structural event) in one batch, got %d: %+v", len(events), events)
	}
	if events[0].Kind != db.TraceKindAnswer || events[0].Text != "partial answer" {
		t.Errorf("events[0] = %+v, want the flushed answer text first", events[0])
	}
	if events[1].Kind != db.TraceKindCandidate || events[1].Text != "Match newsletters." {
		t.Errorf("events[1] = %+v, want the candidate event second", events[1])
	}
	if events[0].Seq >= events[1].Seq {
		t.Errorf("Seq must be strictly increasing: events[0].Seq=%d events[1].Seq=%d", events[0].Seq, events[1].Seq)
	}
}

// TestTraceWriter_KindSwitchFlushesPreviousBuffer checks that an answer delta arriving
// while a thinking buffer is pending (or vice versa) flushes the old buffer first, rather
// than concatenating two different kinds of text into one item.
func TestTraceWriter_KindSwitchFlushesPreviousBuffer(t *testing.T) {
	store := &fakeTraceStore{}
	tw := newTraceWriter(store, 1, 0)
	ctx := context.Background()

	tw.Text(ctx, db.TraceKindThinking, 1, "thinking about it")
	tw.Text(ctx, db.TraceKindAnswer, 1, "the answer")

	events := store.all()
	if len(events) != 1 {
		t.Fatalf("expected the thinking buffer to flush when the answer delta arrived, got %d events: %+v", len(events), events)
	}
	if events[0].Kind != db.TraceKindThinking || events[0].Text != "thinking about it" {
		t.Errorf("flushed event = %+v, want the thinking buffer flushed intact", events[0])
	}
}

// TestTraceWriter_SeqMonotonicAcrossEventAndText checks that Seq keeps incrementing
// across both Event and Text calls without resetting or colliding — the property
// ListSuggestionTrace's "SK > :after" cursor query depends on.
func TestTraceWriter_SeqMonotonicAcrossEventAndText(t *testing.T) {
	store := &fakeTraceStore{}
	tw := newTraceWriter(store, 1, 0)
	ctx := context.Background()

	tw.Event(ctx, db.TraceKindRoundStart, 1, "")
	big := strings.Repeat("x", traceFlushBytes) // forces an immediate flush
	tw.Text(ctx, db.TraceKindAnswer, 1, big)
	tw.Event(ctx, db.TraceKindDone, 1, "")

	events := store.all()
	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d: %+v", len(events), events)
	}
	for i := 1; i < len(events); i++ {
		if events[i].Seq <= events[i-1].Seq {
			t.Errorf("Seq not strictly increasing at index %d: %+v", i, events)
		}
	}
}

// TestTraceWriter_FlushForcesOutPartialBuffer checks the explicit Flush escape hatch: a
// caller that's about to stop writing (without a structural event of its own to piggyback
// on) can still guarantee nothing buffered is lost.
func TestTraceWriter_FlushForcesOutPartialBuffer(t *testing.T) {
	store := &fakeTraceStore{}
	tw := newTraceWriter(store, 1, 0)
	ctx := context.Background()

	tw.Text(ctx, db.TraceKindAnswer, 1, "not enough to trip either threshold")
	if got := len(store.all()); got != 0 {
		t.Fatalf("expected nothing flushed yet, got %d", got)
	}

	tw.Flush(ctx)

	events := store.all()
	if len(events) != 1 || events[0].Text != "not enough to trip either threshold" {
		t.Fatalf("Flush did not force out the pending buffer: %+v", events)
	}

	// Flush with nothing pending must be a no-op, not an empty write.
	tw.Flush(ctx)
	if got := len(store.all()); got != 1 {
		t.Errorf("Flush with nothing pending wrote an extra event: %d total", got)
	}
}

// TestTraceWriter_StartSeqContinuesAcrossRegenerate guards the regenerate correctness fix:
// newTraceWriter must start counting from startSeq, not always 0, or a second worker
// invocation for the same suggestion (a regenerate) would reuse Seq 1..N and silently
// overwrite the first round's trace items (same PK+SK) instead of continuing past them.
func TestTraceWriter_StartSeqContinuesAcrossRegenerate(t *testing.T) {
	store := &fakeTraceStore{}
	ctx := context.Background()

	// Round 1: a fresh suggestion, starts at 0.
	first := newTraceWriter(store, 1, 0)
	first.Event(ctx, db.TraceKindRoundStart, 1, "")
	first.Event(ctx, db.TraceKindDone, 1, "")
	firstEvents := store.all()
	if len(firstEvents) != 2 || firstEvents[1].Seq != 2 {
		t.Fatalf("round 1 events = %+v, want 2 events ending at Seq 2", firstEvents)
	}

	// Regenerate: a brand-new traceWriter for the same suggestion, seeded with the
	// highest seq the (fake) store already has — exactly what runOne does via
	// db.Store.LatestSuggestionTraceSeq.
	second := newTraceWriter(store, 1, firstEvents[1].Seq)
	second.Event(ctx, db.TraceKindRoundStart, 1, "")

	all := store.all()
	if len(all) != 3 {
		t.Fatalf("expected 3 total events after the regenerate round, got %d: %+v", len(all), all)
	}
	if all[2].Seq != 3 {
		t.Errorf("regenerate round's first event Seq = %d, want 3 (continuing past round 1, not colliding with it)", all[2].Seq)
	}
}

// TestTraceWriter_WriteFailureIsSwallowed is the load-bearing test for this file's whole
// design: narrating a round must never be able to fail the round itself. A store that
// always errors must not panic, must not return an error to the caller (Event/Text/Flush
// are all void), and callers must be able to keep calling them.
func TestTraceWriter_WriteFailureIsSwallowed(t *testing.T) {
	store := &fakeTraceStore{err: errors.New("dynamodb: throttled")}
	tw := newTraceWriter(store, 1, 0)
	ctx := context.Background()

	tw.Event(ctx, db.TraceKindRoundStart, 1, "")
	tw.Text(ctx, db.TraceKindAnswer, 1, strings.Repeat("x", traceFlushBytes))
	tw.Flush(ctx)
	tw.Event(ctx, db.TraceKindDone, 1, "")
	// Reaching this line without a panic is the assertion — Event/Text/Flush have no error
	// return for exactly this reason, so there's nothing else to check.
	t.Log("no panic from a permanently-failing trace store")
}

// TestTraceWriter_SinkRoutesTextAndReasoningSeparately checks the llm.ImproveSink adapter:
// a StreamChunk's Text and Reasoning must land as db.TraceKindAnswer and
// db.TraceKindThinking respectively, never merged.
func TestTraceWriter_SinkRoutesTextAndReasoningSeparately(t *testing.T) {
	store := &fakeTraceStore{}
	tw := newTraceWriter(store, 1, 0)
	ctx := context.Background()
	sink := tw.Sink(ctx, 1)

	sink(llm.StreamChunk{Reasoning: strings.Repeat("r", traceFlushBytes)})
	sink(llm.StreamChunk{Text: strings.Repeat("t", traceFlushBytes)})

	events := store.all()
	if len(events) != 2 {
		t.Fatalf("expected 2 flushed events (one per kind), got %d: %+v", len(events), events)
	}
	if events[0].Kind != db.TraceKindThinking {
		t.Errorf("events[0].Kind = %q, want %q", events[0].Kind, db.TraceKindThinking)
	}
	if events[1].Kind != db.TraceKindAnswer {
		t.Errorf("events[1].Kind = %q, want %q", events[1].Kind, db.TraceKindAnswer)
	}
}
