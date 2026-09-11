package db

import (
	"fmt"
	"testing"
)

// TestMergeQueueEntry_FirstFlagCreatesEntry checks the zero-value existing (no queue row
// yet for this rule) produces a fresh entry with CreatedAt stamped and FlaggedCount at 1.
func TestMergeQueueEntry_FirstFlagCreatesEntry(t *testing.T) {
	ref := QueuedEmailRef{MessageID: "m1", Sender: "a@example.com", Subject: "Weekly digest", TriggerKind: TriggerKindFalseNegative}
	got := mergeQueueEntry(ImproveQueueEntry{}, 5, ref, "why", "2026-09-10 12:00:00")

	if got.PromptID != 5 {
		t.Errorf("PromptID = %d, want 5", got.PromptID)
	}
	if got.FlaggedCount != 1 {
		t.Errorf("FlaggedCount = %d, want 1", got.FlaggedCount)
	}
	if got.CreatedAt != "2026-09-10 12:00:00" || got.UpdatedAt != "2026-09-10 12:00:00" {
		t.Errorf("CreatedAt/UpdatedAt = %q/%q, want both stamped to now", got.CreatedAt, got.UpdatedAt)
	}
	if len(got.Emails) != 1 || got.Emails[0] != ref {
		t.Errorf("Emails = %+v, want [%+v]", got.Emails, ref)
	}
	if len(got.Notes) != 1 || got.Notes[0] != "why" {
		t.Errorf("Notes = %v, want [\"why\"]", got.Notes)
	}
}

// TestMergeQueueEntry_RepeatedFlagsCollapseToOneEntry is the core behavior this whole queue
// exists for: flagging the same rule from three different emails must collapse into one
// entry with FlaggedCount 3, CreatedAt preserved from the first flag, not one row per flag.
func TestMergeQueueEntry_RepeatedFlagsCollapseToOneEntry(t *testing.T) {
	entry := ImproveQueueEntry{}
	entry = mergeQueueEntry(entry, 5, QueuedEmailRef{MessageID: "m1"}, "", "2026-09-10 12:00:00")
	entry = mergeQueueEntry(entry, 5, QueuedEmailRef{MessageID: "m2"}, "", "2026-09-10 12:05:00")
	entry = mergeQueueEntry(entry, 5, QueuedEmailRef{MessageID: "m3"}, "", "2026-09-10 12:10:00")

	if entry.FlaggedCount != 3 {
		t.Errorf("FlaggedCount = %d, want 3", entry.FlaggedCount)
	}
	if entry.CreatedAt != "2026-09-10 12:00:00" {
		t.Errorf("CreatedAt = %q, want the first flag's timestamp preserved", entry.CreatedAt)
	}
	if entry.UpdatedAt != "2026-09-10 12:10:00" {
		t.Errorf("UpdatedAt = %q, want the latest flag's timestamp", entry.UpdatedAt)
	}
	if len(entry.Emails) != 3 {
		t.Fatalf("Emails = %+v, want 3 entries", entry.Emails)
	}
	if entry.Emails[0].MessageID != "m3" {
		t.Errorf("Emails[0].MessageID = %q, want %q (newest-first)", entry.Emails[0].MessageID, "m3")
	}
}

// TestMergeQueueEntry_EmailsCapped checks queuedEmailDisplayCap bounds the display slice
// while FlaggedCount keeps counting past it — display samples, not the source of truth.
func TestMergeQueueEntry_EmailsCapped(t *testing.T) {
	entry := ImproveQueueEntry{}
	for range queuedEmailDisplayCap + 5 {
		entry = mergeQueueEntry(entry, 5, QueuedEmailRef{MessageID: "m"}, "", "2026-09-10 12:00:00")
	}
	if entry.FlaggedCount != int64(queuedEmailDisplayCap+5) {
		t.Errorf("FlaggedCount = %d, want %d (uncapped)", entry.FlaggedCount, queuedEmailDisplayCap+5)
	}
	if len(entry.Emails) != queuedEmailDisplayCap {
		t.Errorf("len(Emails) = %d, want %d (capped)", len(entry.Emails), queuedEmailDisplayCap)
	}
}

// TestMergeQueueEntry_NotesDedupedAndCapped checks a repeated note isn't duplicated, an
// empty note is never recorded, and the note slice is capped like Emails.
func TestMergeQueueEntry_NotesDedupedAndCapped(t *testing.T) {
	entry := ImproveQueueEntry{}
	entry = mergeQueueEntry(entry, 5, QueuedEmailRef{}, "same note", "2026-09-10 12:00:00")
	entry = mergeQueueEntry(entry, 5, QueuedEmailRef{}, "same note", "2026-09-10 12:01:00")
	entry = mergeQueueEntry(entry, 5, QueuedEmailRef{}, "", "2026-09-10 12:02:00")
	if len(entry.Notes) != 1 {
		t.Fatalf("Notes = %v, want exactly one deduped entry", entry.Notes)
	}

	entry = ImproveQueueEntry{}
	for i := range queuedNoteDisplayCap + 3 {
		entry = mergeQueueEntry(entry, 5, QueuedEmailRef{}, fmt.Sprintf("note %d", i), "2026-09-10 12:00:00")
	}
	if len(entry.Notes) != queuedNoteDisplayCap {
		t.Errorf("len(Notes) = %d, want %d (capped)", len(entry.Notes), queuedNoteDisplayCap)
	}
}
