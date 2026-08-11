package main

// ============================================================
// Example corpus pruning — the daily reverse of selection
// ============================================================
//
// db.PromptExample has no TTL: it's a rule's permanent history, and at this app's scale
// that's cheap (see the "Growth and retention" doc comment on the Prompt examples section,
// db/store.go). But it's genuinely unbounded — passive confirmation
// (processor.processEmail) writes a confirmed_positive row on every routine match, not just
// a manual correction, so a heavily-used rule's corpus grows forever with nothing to stop
// it short of the user manually clearing it.
//
// This file runs the example-selection sampler (improve.go's sampleVerdict) in reverse:
// whatever falls outside the same priority order selection already uses — recurred first,
// then manually-reviewed, then passive, each spread across sender/subject buckets — is, by
// construction, exactly what selection would never show the improver or score against, so
// it's safe to delete permanently. See pruneKeepSet (improve.go) for the actual policy,
// including why resolved examples are kept separately from live ones.
//
// Runs once a day inside the existing 2 AM ET catch-up scan (scanOnce, scan.go) — no new
// scheduled infrastructure.

import (
	"context"
	"log/slog"

	"github.com/sloccy/ollamail-aws/db"
)

// pruneReadCeiling bounds how many raw rows one verdict's prune pass reads in a single
// scheduled run — comfortably above the prune cap's default (2x replayExampleCap, ~80/
// verdict) so a normal prune pass is never itself truncated by this ceiling, but still a
// hard bound (mirroring retention.maxRetentionIDs' own "bounded read per scheduled run"
// precedent) so a verdict that's somehow grown far past what one run can handle degrades
// gracefully — this run prunes what's within the ceiling, a later run catches the rest —
// rather than reading an unbounded partition in one invocation.
const pruneReadCeiling = 2000

// pruneCapMultiplier is how much headroom the prune cap keeps over replayExampleCap — 2x,
// not 1x, so raising replay_example_cap later (e.g. for a less noisy validation score) finds
// the larger sample it needs still sitting in storage, instead of it already having been
// pruned away to the old, smaller number.
const pruneCapMultiplier = 2

// prunePromptExamples runs the daily reverse-of-selection prune across every active
// prompt's example corpus. Called once per scheduled scan (scanOnce, scan.go), after the
// per-account labeling loop so a slow prune pass never delays the labeling work that's the
// actual point of the scan — over the same prompt list scanOnce already loaded, so this
// costs no extra read to get started. Best-effort per prompt and per verdict: a failure
// pruning one rule's corpus is logged and skipped, never allowed to abort the rest of the
// scan or block any other prompt's prune.
func prunePromptExamples(ctx context.Context, store *db.Store, prompts []db.Prompt) {
	promptCap := pruneCapMultiplier * replayExampleCap(ctx, store)
	for _, p := range prompts {
		// CountExamplesByVerdict already counts all three verdicts in one call (3
		// Select:COUNT queries) — call it once per prompt here rather than once per verdict
		// inside pruneVerdict, which would repeat the same 3 queries 3x for nothing.
		counts, err := store.CountExamplesByVerdict(ctx, p.ID)
		if err != nil {
			slog.Error("prune: count examples", "prompt_id", p.ID, "err", err)
			continue
		}
		for _, verdict := range db.VerdictOrder {
			pruneVerdict(ctx, store, p.ID, verdict, promptCap, counts[verdict])
		}
	}
}

// pruneVerdict prunes one prompt's one verdict, if it needs it: a cheap count precheck
// (shouldPruneVerdict, improve.go, against count — already fetched by the caller via one
// CountExamplesByVerdict call per prompt) skips the expensive read+rank+delete path
// entirely once a corpus has stabilized near cap, which is the common case on most
// scheduled runs.
func pruneVerdict(ctx context.Context, store *db.Store, promptID int64, verdict string, promptCap int, count int64) {
	if !shouldPruneVerdict(count, promptCap) {
		return
	}

	raw, err := store.ListExamplesByVerdict(ctx, promptID, verdict, pruneReadCeiling)
	if err != nil {
		slog.Error("prune: list examples", "prompt_id", promptID, "verdict", verdict, "err", err)
		return
	}
	keep := pruneKeepSet(raw, promptCap)

	var toDelete []db.PromptExample
	for _, ex := range raw {
		if !keep[ex.ID] {
			toDelete = append(toDelete, ex)
		}
	}
	if len(toDelete) == 0 {
		return
	}
	if err := store.DeletePromptExamples(ctx, toDelete); err != nil {
		slog.Error("prune: delete examples", "prompt_id", promptID, "verdict", verdict, "attempted", len(toDelete), "err", err)
		return
	}
	slog.Info("prune: examples deleted", "prompt_id", promptID, "verdict", verdict, "deleted", len(toDelete), "kept", len(keep))
}
