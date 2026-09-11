package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/sloccy/ollamail-aws/db"
	"github.com/sloccy/ollamail-aws/llm"
)

// ============================================================
// Prompt improvement: async worker
// ============================================================
//
// This file used to live entirely inside recategorize.go, running each improve+replay
// round in a bare `go` goroutine inside WebFunction. Lambda freezes an execution
// environment the instant its HTTP response is written, so that goroutine got no CPU and
// sat on an open Bedrock connection across freezes; bedrockHTTPTimeout (llm/bedrock.go,
// 14m30s wall-clock) kept ticking through the freeze regardless, and if the environment
// was reaped before the call returned, nothing ever wrote a terminal status — the
// suggestion just sat on "generating" forever. See ImproveFunction's doc comment in
// template.yaml for the full failure mode.
//
// The fix: this logic now runs inside its own Lambda (MODE=improve, see main.go),
// invoked asynchronously by server.dispatchImprove. It stays alive for the whole
// invocation regardless of what happens to WebFunction. improveRunner is the shared
// implementation — used by the worker (newImproveRunner + lambda.Start(runner.handle) in
// main.go) and, when IMPROVE_FUNCTION_NAME isn't set (local dev, tests), by an in-process
// goroutine off the server's long-lived context — so the two paths can't drift apart, the
// same reasoning improveAndFinalizeSuggestion's doc comment originally gave for sharing
// this code between the batch and regenerate call sites.

// improveTarget is one suggestion row to work in an improveEvent. The row itself
// (status='generating') is already inserted/marked by the caller —
// server.startImproveQueueEntry (called from handleImproveQueueStart/StartAll once a queued
// rule's flag is actually started) or handlePromptSuggestionRegenerate (server.go) — before
// the async invoke fires, so the worker only ever needs the id plus enough context to run
// one improve call.
type improveTarget struct {
	SuggestionID         int64             `json:"suggestion_id"`
	PromptID             int64             `json:"prompt_id"`
	OriginalInstructions string            `json:"original_instructions"`
	PriorConversation    []llm.ChatMessage `json:"prior_conversation,omitempty"`
	Note                 string            `json:"note,omitempty"`
	UserComment          string            `json:"user_comment,omitempty"`
}

// improveEvent is the async Lambda payload for the MODE=improve worker. The batch path
// (one note, no prior conversation, shared across every rule flagged in one
// recategorization) and the regenerate path (no note, a real prior conversation, a
// per-call user comment) don't share a shape — encoding that per-target rather than once
// per event lets one event carry a mixed batch without the worker caring which path built
// it.
type improveEvent struct {
	Targets []improveTarget `json:"targets"`
}

// improveRunner executes improve+replay rounds against the store/llm client it's given.
// Constructed once in main.go's MODE=improve case (for the worker Lambda) and once in
// newServer (for the local-fallback path in dispatchImprove).
type improveRunner struct {
	store *db.Store
	llm   *llm.Client
	cfg   *Config
}

func newImproveRunner(store *db.Store, llmClient *llm.Client, cfg *Config) *improveRunner {
	return &improveRunner{store: store, llm: llmClient, cfg: cfg}
}

// improveWorkerMargin is reserved off the invoking Lambda's own deadline (ctx.Deadline(),
// set automatically by the Lambda Go runtime from the function's configured Timeout) so a
// suggestion's improve+replay round always leaves time to write a terminal status instead
// of being killed mid-flight — see runOne's deferred failure write, the backstop for
// exactly that case.
const improveWorkerMargin = 20 * time.Second

// handle is the MODE=improve Lambda entry point (main.go: lambda.Start(runner.handle)),
// and also what the local-fallback path in server.dispatchImprove calls directly. Errors
// from individual targets are handled (and logged) inside runOne, not returned here — one
// bad suggestion in a batch must not stop the rest, and there is no caller waiting on this
// call's error for an async (Event) invocation anyway.
func (r *improveRunner) handle(ctx context.Context, event improveEvent) error {
	slog.Info("improve worker start", "count", len(event.Targets))
	for _, t := range event.Targets {
		r.runOne(ctx, t)
	}
	slog.Info("improve worker done", "count", len(event.Targets))
	return nil
}

// runOne claims and works a single suggestion, guaranteeing it reaches a terminal status
// (pending or failed) before returning. Two layers behind improveAndFinalizeSuggestion's
// own normal pending/failed writes:
//  1. ClaimPromptSuggestion — an async (Event) Lambda invocation is automatically retried
//     by AWS up to twice on error, and without a claim a retry would redo (and re-bill)
//     the same improve+replay round from scratch instead of skipping a suggestion another
//     attempt already finished or is still working.
//  2. The deferred failure write — catches a panic or a deadline expiring before
//     improveAndFinalizeSuggestion reaches its own terminal write. db's
//     generatingStaleAfter read-side check (store.go) is the last-resort backstop behind
//     even this, for the case where the worker invocation never ran at all (e.g. the
//     async Invoke call itself failed — see server.failDispatch, which handles that case
//     directly instead of relying on staleness).
func (r *improveRunner) runOne(ctx context.Context, t improveTarget) {
	claimed, err := r.store.ClaimPromptSuggestion(ctx, t.SuggestionID)
	if err != nil {
		slog.Error("improve worker: claim suggestion", "suggestion_id", t.SuggestionID, "err", err)
		return
	}
	if !claimed {
		slog.Info("improve worker: suggestion already claimed, skipping", "suggestion_id", t.SuggestionID)
		return
	}

	// A regenerate can reach this point more than once for the same suggestion id (see
	// MarkPromptSuggestionGenerating's doc comment), each time as a fresh worker
	// invocation — so the trace's seq counter has to be seeded from what's already
	// written, not restarted at 0, or a regenerate round would silently overwrite the
	// first round's items (see newTraceWriter's doc comment). Best-effort: a failed
	// lookup just starts a new trace segment at 0, which risks one SK collision with an
	// existing item rather than losing the whole round over a transient read error.
	startSeq, err := r.store.LatestSuggestionTraceSeq(ctx, t.SuggestionID)
	if err != nil {
		slog.Warn("improve worker: could not read prior trace seq, starting from 0", "suggestion_id", t.SuggestionID, "err", err)
	}
	tw := newTraceWriter(r.store, t.SuggestionID, startSeq)

	p, err := r.store.GetPrompt(ctx, t.PromptID)
	if err != nil {
		slog.Error("improve worker: get prompt", "suggestion_id", t.SuggestionID, "prompt_id", t.PromptID, "err", err)
		r.writeFailure(ctx, tw, t.SuggestionID, err)
		return
	}

	callCtx := ctx
	if dl, ok := ctx.Deadline(); ok {
		var cancel context.CancelFunc
		callCtx, cancel = context.WithDeadline(ctx, dl.Add(-improveWorkerMargin))
		defer cancel()
	}

	done := false
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("improve worker: panic", "suggestion_id", t.SuggestionID, "recover", rec)
			r.writeFailure(ctx, tw, t.SuggestionID, fmt.Errorf("panic: %v", rec))
			return
		}
		if !done {
			r.writeFailure(ctx, tw, t.SuggestionID, errors.New("worker deadline exceeded before a result was written"))
		}
	}()

	r.improveAndFinalizeSuggestion(callCtx, tw, t.SuggestionID, p, t.OriginalInstructions, t.PriorConversation, t.Note, t.UserComment)
	done = true
}

// finalizeFailure stamps a terminal 'failed' status with cause's message as UserComment —
// the one write shape shared by every path that gives up on a suggestion rather than
// letting improveAndFinalizeSuggestion finish it normally: writeFailure (a panic/blown
// deadline/missing prompt row), round 1 failing with no earlier round to fall back on, and
// failDispatch (the hand-off to the worker itself never got to run). Callers log their own
// contextual message on error; this only carries the write itself.
func finalizeFailure(ctx context.Context, store *db.Store, sid int64, cause error) error {
	return store.FinalizePromptSuggestion(ctx, db.FinalizePromptSuggestionParams{
		ID:                    sid,
		SuggestedInstructions: "",
		ConversationJSON:      "[]",
		Status:                db.SuggestionStatusFailed,
		UserComment:           cause.Error(),
	})
}

// terminalWriteCtx returns the context a *terminal* status write must use — the round's
// own ctx may already be past its deadline (that's often exactly why a terminal write is
// happening now, e.g. a stalled improve call that ran the round out its whole budget), so
// the write is detached from it and given its own small budget rather than inheriting an
// already-expired one. Scoped to terminal writes only: mid-round trace narration
// (tw.Event elsewhere) should keep using the round's own ctx, since a cancelled round has
// no business continuing to narrate.
func terminalWriteCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
}

// writeFailure stamps a terminal 'failed' status directly, bypassing the normal
// improveAndFinalizeSuggestion path — used when something has gone wrong badly enough
// (a panic, a blown deadline, a missing prompt row) that the normal path can't be trusted
// to write it itself. ctx may already be past its deadline (that's often why this is being
// called), so the write uses a fresh bounded context detached from it rather than
// inheriting an already-expired one.
func (r *improveRunner) writeFailure(ctx context.Context, tw *traceWriter, suggestionID int64, cause error) {
	writeCtx, cancel := terminalWriteCtx(ctx)
	defer cancel()
	// The trace's error event is emitted after the status write, same reasoning as
	// improveAndFinalizeSuggestion's done event: the trace poll's terminal signal must mean
	// "the suggestion's status is actually final," not merely "about to be."
	if err := finalizeFailure(writeCtx, r.store, suggestionID, cause); err != nil {
		slog.Error("improve worker: write failure status failed", "suggestion_id", suggestionID, "err", err)
	}
	tw.Event(writeCtx, db.TraceKindError, 0, cause.Error())
}

// rawExampleLimit bounds how many rows gatherRawExamples pulls per verdict before any
// sampling happens. Both verdicts share one limit now: every example comes from an explicit
// human review (recategorize or confirm), so there's no passive write source that grows one
// verdict faster than the other the way the old confirmed_positive-only auto-confirmation
// did. Cheap regardless of corpus size either way: ListExamplesByVerdict's cost is bounded
// by the Limit passed in, not by how large the partition has grown.
const rawExampleLimit = 120

// gatherRawExamples reads a rule's whole raw example window (see rawExampleLimit),
// marks recurrences, drops resolved rows, and collapses the same message appearing under
// more than one verdict down to its newest occurrence. This is the shared foundation
// everything downstream samples from at whatever cap fits its purpose: selectExamplesForImprove
// calls it directly for the small, token-bounded improve-prompt set, and
// improveAndFinalizeSuggestion calls it once per round and samples the result a second time
// at replayExampleCap for the larger validation set — one raw fetch, two different-sized
// samples, rather than fetching the corpus twice for the same round.
func gatherRawExamples(ctx context.Context, store *db.Store, promptID int64) []db.PromptExample {
	var all []db.PromptExample
	for _, v := range db.VerdictOrder {
		examples, err := store.ListExamplesByVerdict(ctx, promptID, v, rawExampleLimit)
		if err != nil {
			slog.Error("gather raw examples", "prompt_id", promptID, "verdict", v, "err", err)
			continue
		}
		all = append(all, examples...)
	}
	all = markRecurrences(all)
	all = filterResolved(all)

	// The same message can appear in more than one verdict's raw window if it was corrected
	// more than once over time (e.g. false_positive once, confirmed_positive later after the
	// rule was fixed). Keeping every occurrence would hand the improver a live contradiction
	// — "this email is both a false positive and a confirmed positive" — so only the newest
	// occurrence survives, by db.PromptExample.ID (monotonically increasing, and shared
	// across every write path that can produce a PromptExample — see
	// db.InsertPromptExamples' doc comment). Iterating `all` in its original per-verdict-
	// query order (each already newest-first) and keeping only each message's
	// first-encountered survivor preserves that newest-first ordering in the output without
	// a second sort.
	newestID := make(map[string]int64, len(all))
	for _, ex := range all {
		if cur, ok := newestID[ex.MessageID]; !ok || ex.ID > cur {
			newestID[ex.MessageID] = ex.ID
		}
	}
	survivors := make([]db.PromptExample, 0, len(all))
	seen := make(map[string]bool, len(all))
	for _, ex := range all {
		if newestID[ex.MessageID] != ex.ID || seen[ex.MessageID] {
			continue
		}
		seen[ex.MessageID] = true
		survivors = append(survivors, ex)
	}
	return survivors
}

// parseExampleCap is the pure parsing/clamping core shared by improveExampleCap and
// replayExampleCap, mirroring parseImproveMaxRounds' shape: unset, unparsable, or
// non-positive falls back to def; anything above maxCap clamps down to it.
func parseExampleCap(raw string, def, maxCap int) int {
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return def
	}
	if n > maxCap {
		return maxCap
	}
	return n
}

// improveExampleCap resolves llm.SettingImproveExampleCap — how many examples per verdict
// selectExamplesForImprove curates for the improve prompt itself. A free function (not an
// improveRunner method) so server.go's suggestionDetailView can call it with just a
// *db.Store, matching selectExamplesForImprove's own signature.
func improveExampleCap(ctx context.Context, store *db.Store) int {
	v, err := store.GetSetting(ctx, llm.SettingImproveExampleCap)
	if err != nil || v == "" {
		return llm.ImproveExampleCapDefault
	}
	return parseExampleCap(v, llm.ImproveExampleCapDefault, llm.ImproveExampleCapMax)
}

// replayExampleCap resolves llm.SettingReplayExampleCap — how many examples per verdict
// ReplayAgainstExamples scores a candidate rule against. improveAndFinalizeSuggestion
// doesn't call this directly (it resolves the same setting from its own batched
// loadSettings map instead, to avoid a second GetSetting round trip in the hot round
// loop) — this free-function form exists for prune.go's prunePromptExamples, a once-daily
// scheduled call with no batched settings map of its own to read from.
func replayExampleCap(ctx context.Context, store *db.Store) int {
	v, err := store.GetSetting(ctx, llm.SettingReplayExampleCap)
	if err != nil || v == "" {
		return llm.ReplayExampleCapDefault
	}
	return parseExampleCap(v, llm.ReplayExampleCapDefault, llm.ReplayExampleCapMax)
}

// parseReplayConcurrency is the pure parsing/clamping core for llm.SettingReplayConcurrency
// — how many classify calls ReplayAgainstExamples runs at once (see
// llm.ReplayConcurrencyDefault's doc comment for why this must be bounded, not
// 0/unbounded). Unset, unparsable, or non-positive falls back to the default; there's no
// separate cap constant the way the example caps have one — an operator setting this by
// hand is trusted the same way SettingClassifyModel is. Takes the raw setting value
// directly (mirroring parseExampleCap/parseImproveMaxRounds) so improveAndFinalizeSuggestion
// can resolve it from the one batched loadSettings map instead of its own GetSetting round
// trip.
func parseReplayConcurrency(raw string) int {
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return llm.ReplayConcurrencyDefault
	}
	return n
}

// selectExamplesForImprove curates the small, token-bounded example set the improve prompt
// itself sees (llm.ImproveRequest's three example slices) — and, via
// server.suggestionDetailView, what the user sees on the suggestion detail page, so the two
// always show the same examples the model actually saw. See sampleExamples for the
// selection policy.
func selectExamplesForImprove(ctx context.Context, store *db.Store, promptID int64) []db.PromptExample {
	return sampleExamples(gatherRawExamples(ctx, store, promptID), improveExampleCap(ctx, store))
}

// The larger, more representative sample ReplayAgainstExamples scores a candidate rule
// against — deliberately independent of and bigger than selectExamplesForImprove's set,
// since replay's cost is one classify call per example, not prompt tokens, so a wider
// sample buys a materially less noisy pass/fail score for comparatively little — has no
// standalone named entry point the way selectExamplesForImprove does: its only caller,
// improveAndFinalizeSuggestion, already has the raw window in hand (see gatherRawExamples'
// doc comment) and just samples it again at replayExampleCap, only when replay is actually
// enabled (llm.SettingImproveReplay, resolved once per round via loadSettings).

// senderSubjectKey normalizes a sender+subject pair for the round-robin sampler below:
// trimmed and case-folded, so trailing whitespace or casing differences (a mail client
// rendering headers slightly differently across sends of the same templated email) don't
// defeat the grouping. \x00 as a separator can't appear in either field, so it can't
// collide across a sender/subject boundary.
func senderSubjectKey(sender, subject string) string {
	return strings.ToLower(strings.TrimSpace(sender)) + "\x00" + strings.ToLower(strings.TrimSpace(subject))
}

// sampleExamples curates up to perVerdictCap examples per verdict from a rule's raw, deduped
// corpus (gatherRawExamples) — this replaces the old "keep the first N distinct
// sender+subject pairs in recency order" dedup, which converges on whichever few senders
// happen to be freshest rather than actually spreading across what the corpus contains.
// Independently per verdict, three priority tiers fill the budget in order (see
// sampleVerdict): examples that recurred after a prior fix, then examples the rule actually
// got wrong (Missed), then plain confirmations — each of the latter two round-robinned
// across sender+subject buckets rather than taken newest-first. See
// db.PromptExample.Missed/.Recurred for what feeds the tiering.
func sampleExamples(examples []db.PromptExample, perVerdictCap int) []db.PromptExample {
	if perVerdictCap <= 0 {
		return nil
	}
	byVerdict := make(map[string][]db.PromptExample, len(db.VerdictOrder))
	for _, ex := range examples {
		byVerdict[ex.Verdict] = append(byVerdict[ex.Verdict], ex)
	}
	var out []db.PromptExample
	for _, v := range db.VerdictOrder {
		out = append(out, sampleVerdict(byVerdict[v], perVerdictCap)...)
	}
	return out
}

// recurredBudget bounds how much of a verdict's cap Tier 1 (recurred examples) may consume
// on its own — half, rounded up — so a rule with many regressions still leaves room for
// positive/negative signal beyond "everything is broken," rather than an improve prompt
// that's all regressions and nothing else. Unused budget rolls over to the later tiers (see
// sampleVerdict): this only ever shrinks how much recurred examples can take, never
// guarantees them a fixed share when there are fewer of them than that.
func recurredBudget(perVerdictCap int) int {
	return (perVerdictCap + 1) / 2
}

// sampleVerdict applies the three-tier policy (see sampleExamples) to one verdict's
// candidates, which must already be newest-first (gatherRawExamples' contract).
func sampleVerdict(candidates []db.PromptExample, verdictCap int) []db.PromptExample {
	var recurred, missed, confirmed []db.PromptExample
	for _, ex := range candidates {
		switch {
		case ex.Recurred:
			recurred = append(recurred, ex)
		case ex.Missed:
			missed = append(missed, ex)
		default:
			confirmed = append(confirmed, ex)
		}
	}

	out := make([]db.PromptExample, 0, verdictCap)
	budget := recurredBudget(verdictCap)
	for _, ex := range recurred {
		if len(out) >= verdictCap || len(out) >= budget {
			break
		}
		out = append(out, ex)
	}
	out = append(out, roundRobinBySender(missed, verdictCap-len(out))...)
	out = append(out, roundRobinBySender(confirmed, verdictCap-len(out))...)
	return out
}

// roundRobinBySender groups candidates (already newest-first) into senderSubjectKey
// buckets, preserving each bucket's newest-first order internally, then takes one item per
// bucket in rotating passes until budget is exhausted or every bucket runs dry — spreading
// picks across every distinct sender/subject pattern present in candidates instead of
// converging on whichever few are freshest.
func roundRobinBySender(candidates []db.PromptExample, budget int) []db.PromptExample {
	if budget <= 0 || len(candidates) == 0 {
		return nil
	}
	buckets := make(map[string][]db.PromptExample)
	var bucketOrder []string
	for _, ex := range candidates {
		key := senderSubjectKey(ex.Sender, ex.Subject)
		if _, ok := buckets[key]; !ok {
			bucketOrder = append(bucketOrder, key)
		}
		buckets[key] = append(buckets[key], ex)
	}

	out := make([]db.PromptExample, 0, budget)
	for len(out) < budget {
		tookAny := false
		for _, key := range bucketOrder {
			if len(out) >= budget {
				break
			}
			if len(buckets[key]) == 0 {
				continue
			}
			out = append(out, buckets[key][0])
			buckets[key] = buckets[key][1:]
			tookAny = true
		}
		if !tookAny {
			break
		}
	}
	return out
}

// pruneBuffer is the hysteresis margin shouldPruneVerdict adds on top of a verdict's cap
// before a prune pass does any real work — without it, a verdict sitting right at cap would
// trigger a prune (of only a couple of items) on every single scheduled run once its size
// stabilizes, paying for the read+rank+delete pass for near-zero benefit each time.
const pruneBuffer = 10

// shouldPruneVerdict reports whether a verdict's example count justifies the expensive
// read+rank+delete pass pruneVerdict (prune.go) runs — the cheap precheck this guards,
// db.Store.CountExamplesByVerdict, is 3 Select:COUNT queries regardless of corpus size, so
// most scheduled runs stop here for most prompts once a corpus has stabilized near cap.
func shouldPruneVerdict(count int64, verdictCap int) bool {
	return count > int64(verdictCap)+pruneBuffer
}

// pruneKeepSet decides, from one verdict's bounded raw read (newest-first, per
// ListExamplesByVerdict's contract — pruneVerdict in prune.go is the only caller, passing a
// much wider raw window than selection ever reads), which examples survive a daily prune
// pass. This is the reverse of selectExamplesForImprove and improveAndFinalizeSuggestion's
// replay sampling (both wrap sampleExamples at a different cap): whatever falls outside the
// keep set this returns is exactly what sampleVerdict would never pick for either purpose,
// so it's safe to delete permanently (see db.Store.DeletePromptExamples).
//
// Unlike selection's gatherRawExamples, a resolved example isn't dropped outright here — it
// still has one job left, letting markRecurrences flag a live row as a regression — so it's
// kept by recency alone, up to cap, independent of the live pool below. Tiering doesn't apply
// to a resolved row: nobody ever sees it, only "how far back can a regression still be
// detected" does, and that's answered by recency. Live (unresolved) examples are ranked
// exactly like selection (sampleVerdict, the same recurred > manual > passive, round-robin
// priority) and kept up to cap.
func pruneKeepSet(raw []db.PromptExample, verdictCap int) map[int64]bool {
	marked := markRecurrences(raw) // needs resolved and live rows together to find a regression

	var live, resolved []db.PromptExample
	for _, ex := range marked {
		if ex.ResolvedBySuggestionID != nil {
			resolved = append(resolved, ex)
		} else {
			live = append(live, ex)
		}
	}

	keep := make(map[int64]bool, verdictCap*2)
	for _, ex := range sampleVerdict(live, verdictCap) {
		keep[ex.ID] = true
	}
	// resolved is already newest-first — ListExamplesByVerdict's query order, undisturbed by
	// markRecurrences (which mutates in place, no reordering) — so no re-ranking is needed,
	// just take the newest cap.
	for i, ex := range resolved {
		if i >= verdictCap {
			break
		}
		keep[ex.ID] = true
	}
	return keep
}

// markRecurrences flags each still-live (unresolved) example whose problem a prior
// suggestion already claimed to fix — an older row for the same message and verdict (or,
// failing that, the same sender+subject, since a re-sent templated email can arrive with a
// new MessageID) has ResolvedBySuggestionID set. Must run before filterResolved, which
// drops every resolved row: this is the one point in gatherRawExamples' pipeline where both
// the resolved and unresolved rows for the same problem are still present
// together, which is exactly what's needed to tell "this was already tried and failed"
// apart from "this is a first-time problem." Without it, a regression looks to the
// improver exactly like a brand-new problem — filterResolved just drops the old resolved
// row and the fresh one shows up with no memory attached, so the improver has no way to
// know a small edit already failed here once and a bigger change is warranted (see
// ExampleRef.Recurred, llm/bedrock.go, for where this actually reaches the improve prompt).
// Mutates examples in place (and returns the same slice) rather than copying — this runs
// on every improve round and every suggestion-detail page view, so it stays a single pass
// with no extra allocation for the case (the overwhelming majority) where nothing recurs.
func markRecurrences(examples []db.PromptExample) []db.PromptExample {
	type msgKey struct{ id, verdict string }
	type senderKey struct{ key, verdict string }
	resolvedByMessage := make(map[msgKey]int64, len(examples))
	resolvedBySender := make(map[senderKey]int64, len(examples))
	for _, ex := range examples {
		if ex.ResolvedBySuggestionID == nil {
			continue
		}
		mk := msgKey{ex.MessageID, ex.Verdict}
		if _, ok := resolvedByMessage[mk]; !ok {
			resolvedByMessage[mk] = ex.PromptVersionID
		}
		sk := senderKey{senderSubjectKey(ex.Sender, ex.Subject), ex.Verdict}
		if _, ok := resolvedBySender[sk]; !ok {
			resolvedBySender[sk] = ex.PromptVersionID
		}
	}
	for i, ex := range examples {
		if ex.ResolvedBySuggestionID != nil {
			continue
		}
		if v, ok := resolvedByMessage[msgKey{ex.MessageID, ex.Verdict}]; ok {
			examples[i].Recurred = true
			examples[i].RecurredFromVersion = v
			continue
		}
		if v, ok := resolvedBySender[senderKey{senderSubjectKey(ex.Sender, ex.Subject), ex.Verdict}]; ok {
			examples[i].Recurred = true
			examples[i].RecurredFromVersion = v
		}
	}
	return examples
}

// filterResolved drops examples whose problem this rule has already been fixed for — see
// db.PromptExample.ResolvedBySuggestionID's doc comment. Applied after markRecurrences
// (which needs the resolved rows still present to compare against) and ahead of
// gatherRawExamples' own message-level dedup, so a resolved example can never win it and
// end up in the output: showing the improver a case it already fixed is meaningless unless
// the rule regressed and missed it again, in which case a fresh (unresolved) correction on
// that email will already be in the pool, now marked Recurred.
func filterResolved(examples []db.PromptExample) []db.PromptExample {
	out := make([]db.PromptExample, 0, len(examples))
	for _, ex := range examples {
		if ex.ResolvedBySuggestionID != nil {
			continue
		}
		out = append(out, ex)
	}
	return out
}

// problemExampleKeys picks out the Missed entries from examples — the "problems" a
// suggestion built from them is meant to fix — and returns enough per-example key info
// (db.ResolvedExampleKey) for Store.MarkExamplesResolved to find and mark them once the
// suggestion is applied. Plain confirmations (Missed == false) are never included: they
// aren't problems to resolve, they're guardrails a rewrite shouldn't have broken, and
// marking one resolved would just hide it from future improve rounds for no reason.
//
// Gated on fixedMessageIDs (see fixedByReplay) when replayOn: a Missed example only resolves
// if the winning round's replay actually scored it against the candidate and got it right.
// Without this gate, applying a suggestion resolved every Missed example it was built from
// regardless of whether the winning candidate still failed it in replay — filterResolved
// (this file) then drops that example from every future improve round, so the evidence for
// a problem the rewrite never actually fixed silently disappeared. An example replay never
// scored at all (outside the replay sample, or errored out — errored examples don't appear
// in fixedMessageIDs; see ReplayResult.PassedIndices' doc comment, llm/bedrock.go) has no
// evidence either way and, like a still-failing one, stays unresolved.
//
// replayOn=false means there's no evidence to gate on at all (replay didn't run — disabled,
// or the improve call failed before any round completed), so every Missed key resolves,
// matching this function's behavior before replay-gated resolution existed.
func problemExampleKeys(examples []db.PromptExample, replayOn bool, fixedMessageIDs map[string]bool) []db.ResolvedExampleKey {
	var keys []db.ResolvedExampleKey
	for _, ex := range examples {
		if !ex.Missed {
			continue
		}
		if replayOn && !fixedMessageIDs[ex.MessageID] {
			continue
		}
		keys = append(keys, db.ResolvedExampleKey{
			PromptID:  ex.PromptID,
			Verdict:   ex.Verdict,
			CreatedAt: ex.CreatedAt,
			ID:        ex.ID,
		})
	}
	return keys
}

// fixedByReplay returns the MessageIDs the winning round's replay actually confirmed the
// candidate rule got right (ReplayResult.PassedIndices), resolved back to their source
// example via replayExamples — the same slice, same order, that replayExamplesFor built the
// scored []llm.ReplayExample from, so PassedIndices' positions line up with it directly.
// Safe to call with a zero-value ReplayResult (replay off, or a round that failed before
// scoring): PassedIndices is nil, so the returned map is empty and problemExampleKeys'
// replayOn=false branch does the resolving instead.
func fixedByReplay(replay llm.ReplayResult, replayExamples []db.PromptExample) map[string]bool {
	fixed := make(map[string]bool, len(replay.PassedIndices))
	for _, idx := range replay.PassedIndices {
		if idx >= 0 && idx < len(replayExamples) {
			fixed[replayExamples[idx].MessageID] = true
		}
	}
	return fixed
}

// improveRequestExamples groups a rule's example corpus into the two llm.ExampleRef slices
// llm.ImproveRequest expects, keyed by each example's stored Verdict, carrying Missed
// through onto the ref so formatExampleRefs (llm/bedrock.go) can flag which ones the rule
// actually got wrong.
func improveRequestExamples(examples []db.PromptExample) (shouldMatch, shouldNotMatch []llm.ExampleRef) {
	for _, ex := range examples {
		ref := llm.ExampleRef{Sender: ex.Sender, Subject: ex.Subject, Excerpt: ex.BodyExcerpt, Recurred: ex.Recurred, Missed: ex.Missed}
		switch ex.Verdict {
		case db.VerdictConfirmedPositive:
			shouldMatch = append(shouldMatch, ref)
		case db.VerdictConfirmedNegative:
			shouldNotMatch = append(shouldNotMatch, ref)
		}
	}
	return shouldMatch, shouldNotMatch
}

// replayExamplesFor converts a rule's example corpus into llm.ReplayExample values:
// confirmed_positive examples are expected to match the candidate rule, confirmed_negative
// examples are expected not to. shown is the (much smaller) example set the improve prompt
// itself was built from (selectExamplesForImprove/improveRequestExamples) — any example in
// examples whose MessageID also appears in shown is marked HeldOut=false; everything else
// is HeldOut=true, i.e. an example the model rewriting the rule never saw. See
// ReplayExample.HeldOut's doc comment (llm/bedrock.go) for why that split is what lets
// selectBestRound penalize a rewrite that merely enumerates the examples it was shown.
func replayExamplesFor(examples []db.PromptExample, shown []db.PromptExample) []llm.ReplayExample {
	shownIDs := make(map[string]bool, len(shown))
	for _, ex := range shown {
		shownIDs[ex.MessageID] = true
	}
	out := make([]llm.ReplayExample, len(examples))
	for i, ex := range examples {
		out[i] = llm.ReplayExample{
			Verdict: ex.Verdict,
			Sender:  ex.Sender,
			Subject: ex.Subject,
			Excerpt: ex.BodyExcerpt,
			Want:    ex.Verdict == db.VerdictConfirmedPositive,
			HeldOut: !shownIDs[ex.MessageID],
			// WasCorrect is the inverse of Missed, not of Verdict — see
			// llm.ReplayExample.WasCorrect's doc comment. A confirmed_negative example the
			// rule already correctly left unmatched is just as much "already correct" as a
			// confirmed_positive one it already matched; Missed is what actually says
			// whether the rule agreed with this verdict before the review that produced it.
			WasCorrect: !ex.Missed,
		}
	}
	return out
}

// versionLister is the one method attemptsForPrompt needs from db.Store — declared locally
// rather than widening db.StoreIface, matching this codebase's established pattern for a
// narrow, consumer-declared interface (see llm.Settings/llm.StoreLogger, and traceStore in
// improve_trace.go). *db.Store satisfies this implicitly.
type versionLister interface {
	ListPromptVersions(ctx context.Context, promptID int64, limit int32) ([]db.PromptVersion, error)
}

// attemptsFetchLimit is a little more than llm's PAST ATTEMPTS cap (3 lines,
// llm.maxAttemptLines): it has to cover the current version — excluded below, since
// OriginalInstructions already shows that text — plus enough slack that a version or two
// with no replay evidence (which formatAttempts, llm/bedrock.go, skips) doesn't starve the
// display down to fewer than 3 attempts when more exist.
const attemptsFetchLimit = 6

// attemptsForPrompt builds ImproveRequest.PastAttempts for p — its version history, newest
// first, excluding the current version. This is the improve loop's cross-session memory:
// without it, every improve round starts from a blank slate and can re-propose a phrasing
// this exact rule already tried and already failed (see llm.AttemptRef's doc comment).
// Best-effort: a lookup failure just means no attempt history this round, not a failed
// round — the improver still has the example corpus to work from either way.
func attemptsForPrompt(ctx context.Context, store versionLister, p db.Prompt) []llm.AttemptRef {
	versions, err := store.ListPromptVersions(ctx, p.ID, attemptsFetchLimit)
	if err != nil {
		slog.Error("attempts for prompt", "prompt_id", p.ID, "err", err)
		return nil
	}
	attempts := make([]llm.AttemptRef, 0, len(versions))
	for _, v := range versions {
		if v.ID == p.CurrentVersionID {
			continue
		}
		attempts = append(attempts, llm.AttemptRef{
			Instructions: v.Instructions,
			Passed:       int(v.ReplayPassed),
			Total:        int(v.ReplayTotal),
		})
	}
	return attempts
}

// parseImproveMaxRounds is the pure parsing/clamping core of improveMaxRounds, factored
// out so the boundary behavior (unset, unparsable, zero/negative, above the cap) is
// unit-testable without a live store — GetSetting needs *db.Store, which (like every other
// *db.Store-backed method in this codebase touching s.ddb directly) has no in-package fake
// seam. Empty/unparsable/less-than-1 all fall back to llm.ImproveMaxRoundsDefault; anything
// above llm.ImproveMaxRoundsCap clamps down to it.
func parseImproveMaxRounds(raw string) int {
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return llm.ImproveMaxRoundsDefault
	}
	if n > llm.ImproveMaxRoundsCap {
		return llm.ImproveMaxRoundsCap
	}
	return n
}

// minReplayCoverage is the minimum fraction of a round's submitted replay examples that
// must actually have been scored (Total / (Total+Errored)) before that score is trusted
// for anything — calling improveLoopStop's "perfect score" branch, or letting
// selectBestRound rank a round on its pass rate rather than falling back to a tie-break. A
// literal 3/3 out of 26 submitted examples is not a perfect score, it's an outage: the
// unbounded classify fan-out this codebase used to run (see ReplayConcurrencyDefault,
// llm/bedrock.go) could time out most of a round's corpus at once and still read as a
// flawless result. 0.6 is a floor, not a target — chosen so an isolated handful of
// throttled calls doesn't discard an otherwise-good round, while a batch that mostly
// failed reliably falls below it.
const minReplayCoverage = 0.6

// hasAdequateCoverage reports whether scored/submitted clears minReplayCoverage.
// submitted == 0 (nothing was even attempted) is never adequate, regardless of the
// threshold — there's no score to have coverage of.
func hasAdequateCoverage(scored, submitted int64) bool {
	if submitted <= 0 {
		return false
	}
	return float64(scored)/float64(submitted) >= minReplayCoverage
}

// passRate is passed/total as a float, or -1 for a zero total — so "no evidence at all"
// always compares as worse than any real rate instead of reading identically to a 0% one.
func passRate(passed, total int64) float64 {
	if total <= 0 {
		return -1
	}
	return float64(passed) / float64(total)
}

// classCounts builds an llm.ClassCounts from one round's stored per-bucket fields
// (db.SuggestionRoundSummary), so roundBetter can reuse llm.ClassCounts.Balanced instead of
// duplicating that formula here.
func classCounts(posTotal, posPassed, negTotal, negPassed int64) llm.ClassCounts {
	return llm.ClassCounts{
		PosTotal: int(posTotal), PosPassed: int(posPassed),
		NegTotal: int(negTotal), NegPassed: int(negPassed),
	}
}

// scoreBetter compares two scores where -1 means "no such score" (passRate's zero-total
// case, or llm.ClassCounts.Balanced with an empty bucket) — never treated as beating a real
// score. decided is false when this comparison doesn't settle anything (either score is
// missing, or they're numerically equal), telling the caller to fall through to the next
// comparison in roundBetter's ordering rather than declare a winner on a non-difference.
func scoreBetter(a, b float64) (decided, aWins bool) {
	if a < 0 || b < 0 || a == b {
		return false, false
	}
	return true, a > b
}

// roundBetter reports whether a is strictly better than b, under selectBestRound's
// ordering. heldOutSubmitted is passed through unchanged from selectBestRound.
//
// The comparison runs in order, each step only settling the result when it actually
// distinguishes a from b (see scoreBetter) — otherwise falling through to the next:
//  1. adequate coverage (hasAdequateCoverage) beats inadequate.
//  2. When both rounds have adequate held-out coverage, held-out balanced accuracy
//     (llm.ClassCounts.Balanced — mean of positive recall and negative specificity, see its
//     doc comment for why raw accuracy alone lets a rule that matches everything win).
//  3. Held-out raw pass rate, the same held-out score this comparison used before balanced
//     accuracy existed — reached only when balanced accuracy tied or wasn't computable
//     (one bucket empty).
//  4. When both rounds have adequate overall coverage, overall balanced accuracy.
//  5. Overall raw pass rate.
//  6. Shorter candidate wins — concision has to earn a win through the score instead of
//     being asked for in the prompt and ignored (see improveSystemPrompt, llm/bedrock.go,
//     and this repo's word-count-fixation fix).
//  7. Fully tied: keep the earlier round (caller only replaces on strict >).
func roundBetter(a, b db.SuggestionRoundSummary, heldOutSubmitted int64) bool {
	aCov := hasAdequateCoverage(a.Total, a.Total+a.Errored)
	bCov := hasAdequateCoverage(b.Total, b.Total+b.Errored)
	if aCov != bCov {
		return aCov
	}

	if aCov && bCov && heldOutSubmitted > 0 &&
		hasAdequateCoverage(a.HeldOutTotal, heldOutSubmitted) && hasAdequateCoverage(b.HeldOutTotal, heldOutSubmitted) {
		aBal := classCounts(a.HeldOutPosTotal, a.HeldOutPosPassed, a.HeldOutNegTotal, a.HeldOutNegPassed).Balanced()
		bBal := classCounts(b.HeldOutPosTotal, b.HeldOutPosPassed, b.HeldOutNegTotal, b.HeldOutNegPassed).Balanced()
		if decided, aWins := scoreBetter(aBal, bBal); decided {
			return aWins
		}
		aRate, bRate := passRate(a.HeldOutPassed, a.HeldOutTotal), passRate(b.HeldOutPassed, b.HeldOutTotal)
		if decided, aWins := scoreBetter(aRate, bRate); decided {
			return aWins
		}
	}

	if aCov && bCov {
		aBal := classCounts(a.PosTotal, a.PosPassed, a.NegTotal, a.NegPassed).Balanced()
		bBal := classCounts(b.PosTotal, b.PosPassed, b.NegTotal, b.NegPassed).Balanced()
		if decided, aWins := scoreBetter(aBal, bBal); decided {
			return aWins
		}
	}

	aRate, bRate := passRate(a.Passed, a.Total), passRate(b.Passed, b.Total)
	if decided, aWins := scoreBetter(aRate, bRate); decided {
		return aWins
	}

	if len(a.Candidate) != len(b.Candidate) {
		return len(a.Candidate) < len(b.Candidate)
	}
	return false // fully tied: keep the earlier round (caller only replaces on strict >)
}

// selectBestRound returns the index into rounds of the best-scoring round. Used both to
// pick the round improveAndFinalizeSuggestion finalizes and, inline in its loop, to answer
// "did the round I just ran actually improve on what came before?" (see improveLoopStop) —
// one comparison rule (roundBetter), not two.
//
// heldOutSubmitted is how many of the corpus's replay examples the improve prompt never
// saw (llm.ReplayExample.HeldOut) — fixed for the whole suggestion, since the corpus
// itself doesn't change round to round, only the candidate text does. See roundBetter for
// the exact comparison order: adequate coverage beats inadequate, then held-out pass rate
// (when both rounds have adequate held-out coverage) else overall pass rate, then shorter
// candidate, then earlier round. If no round has adequate coverage, every comparison falls
// through to the pass-rate/length/order tie-breaks on whatever was scored — the caller is
// expected to surface a trace note in that case (see improveAndFinalizeSuggestion).
//
// Returns -1 for an empty slice.
func selectBestRound(rounds []db.SuggestionRoundSummary, heldOutSubmitted int64) int {
	best := -1
	for i, rd := range rounds {
		if best == -1 || roundBetter(rd, rounds[best], heldOutSubmitted) {
			best = i
		}
	}
	return best
}

// improveLoopStop decides whether improveAndFinalizeSuggestion's loop should stop after
// round n, given that round's outcome — factored out so the stop policy is unit-testable
// independent of a live LLM/store. replayOn=false means the loop always stops after round
// 1 regardless of every other input, since without a score there's nothing to iterate on.
// improved reports whether round n (the one just run) is the best seen across every round
// up to and including it — the caller computes this via selectBestRound; a tie with an
// earlier round counts as "not improved." timeRemains is whether enough of the worker's
// deadline is left for another attempt (hasTimeForAnotherRound). reason is a short,
// human-readable string suitable for a trace note; empty when stopping needs no
// explanation (budget exhausted) or when not stopping at all.
func improveLoopStop(n, maxRounds int, replayOn bool, replay llm.ReplayResult, improved, timeRemains bool) (stop bool, reason string) {
	if !replayOn {
		return true, ""
	}
	// hasAdequateCoverage guards this: a literal Passed==Total is meaningless when most of
	// the corpus errored out (see minReplayCoverage's doc comment) — without this guard a
	// round that only managed to classify 3 of 26 submitted examples and got all 3 right
	// would end the loop on an "outage that happened to agree with itself," not a validated
	// rewrite.
	if replay.Total > 0 && replay.Passed == replay.Total &&
		hasAdequateCoverage(int64(replay.Total), int64(replay.Total+replay.Errored)) {
		return true, "perfect score, stopping"
	}
	if n >= maxRounds {
		return true, ""
	}
	if n > 1 && !improved {
		return true, "no improvement over the best round so far, stopping"
	}
	if !timeRemains {
		return true, "not enough time left for another round, stopping"
	}
	return false, ""
}

// roundFitsDeadline is the pure predicate hasTimeForAnotherRound wraps around
// ctx.Deadline() — given how long the last round took and how much time remains, is there
// enough room for another? The 1.5x headroom (not 1x) accounts for a later round tending
// to run longer than the one before it: the conversation grows every round, and a replay
// fan-out competing with a busier Bedrock adaptive retryer can take longer under load than
// it did last time. improveWorkerMargin is added on top since that cushion still has to be
// there afterward for runOne's own deferred failure write.
func roundFitsDeadline(remaining, lastRound time.Duration) bool {
	return remaining > lastRound*3/2+improveWorkerMargin
}

// hasTimeForAnotherRound resolves ctx's deadline and applies roundFitsDeadline. No
// deadline at all (local dev, or the local-fallback goroutine off the server's long-lived
// context — see dispatchImprove) means there's nothing to run out of, so it's always true
// in that case rather than refusing every round after the first.
func hasTimeForAnotherRound(ctx context.Context, lastRound time.Duration) bool {
	dl, ok := ctx.Deadline()
	if !ok {
		return true
	}
	return roundFitsDeadline(time.Until(dl), lastRound)
}

// buildReplayFeedbackTurn renders the next user turn when a round's replay score didn't
// stop the loop on its own — the improver's only view of its own previous mistake, which
// is exactly what was missing before this loop existed (see this file's package doc
// comment). Correlates each llm.ReplayFailure back to its full source example (body
// excerpt included) via ExampleIndex — see that field's doc comment for why the index,
// not a copy of the example, is what ReplayFailure carries. Grouped by direction (wrongly
// matched vs. wrongly missed) rather than by verdict: direction is what the model has to
// act on, and Got alone already determines it without needing Verdict. Deliberately terse,
// in line with this repo's recent token-compression commits (05b44f0, df611fe) — only the
// failures are shown, not a restatement of the whole corpus, since PriorConversation
// already carries that from the first turn.
//
// candidate is the round's own rewrite text (the thing being critiqued), checked via
// llm.CitesExamples against replayLLMExamples for a lifted sender domain/brand — a
// generalization failure the score alone doesn't surface (a rewrite that names "github.com"
// can still score well if the corpus happens to be mostly GitHub mail). Purely advisory:
// prepended to the feedback when it fires, never blocks or lowers the score.
//
// Held-out failures (llm.ReplayExample.HeldOut, via replayLLMExamples) are deliberately
// never shown verbatim — only a count. selectBestRound ranks a round on its held-out pass
// rate precisely because a rewrite can't satisfy it by enumerating the examples it was
// shown; printing a held-out failure's sender/subject/excerpt here would hand the model
// exactly that example on the very next round, so from round 2 on the "held-out" score
// would actually be measuring examples the model had already seen. The count alone still
// tells the model it's losing points outside the sample, which is the generalization
// pressure this function exists to apply — it just can't be satisfied by memorizing lines.
func buildReplayFeedbackTurn(candidate string, replay llm.ReplayResult, examples []db.PromptExample, replayLLMExamples []llm.ReplayExample) string {
	var sb strings.Builder

	if hits := llm.CitesExamples(candidate, replayLLMExamples); len(hits) > 0 {
		fmt.Fprintf(&sb, "Your last rewrite named %s from the examples' senders. Replace it with the property those emails share, not the name itself.\n\n", strings.Join(hits, ", "))
	}

	fmt.Fprintf(&sb, "VALIDATION: your last rewrite scored %d/%d on the real classifier", replay.Passed, replay.Total)
	if replay.HeldOutTotal > 0 {
		fmt.Fprintf(&sb, " (%d/%d of those were emails you weren't shown)", replay.HeldOutPassed, replay.HeldOutTotal)
	}
	sb.WriteString(".\n")

	// isHeldOut is defensive about a bad index the same way the old code's bounds check on
	// examples was: never trust ExampleIndex blindly, even though replayLLMExamples and
	// examples are always the same length in practice (replayExamplesFor's contract).
	isHeldOut := func(idx int) bool {
		return idx >= 0 && idx < len(replayLLMExamples) && replayLLMExamples[idx].HeldOut
	}

	var wronglyMatched, wronglyMissed []llm.ReplayFailure
	var heldOutMatched, heldOutMissed int
	for _, f := range replay.Failures {
		switch {
		case isHeldOut(f.ExampleIndex) && f.Got:
			heldOutMatched++
		case isHeldOut(f.ExampleIndex):
			heldOutMissed++
		case f.Got:
			wronglyMatched = append(wronglyMatched, f)
		default:
			wronglyMissed = append(wronglyMissed, f)
		}
	}
	writeGroup := func(label string, fails []llm.ReplayFailure) {
		if len(fails) == 0 {
			return
		}
		fmt.Fprintf(&sb, "\n%s:\n", label)
		for _, f := range fails {
			if f.ExampleIndex < 0 || f.ExampleIndex >= len(examples) {
				continue // defensive: examples is the exact slice replay was scored against, but never trust an index blindly
			}
			ex := examples[f.ExampleIndex]
			fmt.Fprintf(&sb, "- %s | %s | %s\n", ex.Sender, ex.Subject, ex.BodyExcerpt)
		}
	}
	writeGroup("STILL WRONGLY CAUGHT (must not match)", wronglyMatched)
	writeGroup("STILL MISSED (must match)", wronglyMissed)
	if heldOutFails := heldOutMatched + heldOutMissed; heldOutFails > 0 {
		fmt.Fprintf(&sb, "\nAlso failing: %d of the %d emails you weren't shown (%d wrongly caught, %d missed). You can't see them — fix the category, not these lines.\n",
			heldOutFails, replay.HeldOutTotal, heldOutMatched, heldOutMissed)
	}

	fmt.Fprintf(&sb, "\nRewrite again to fix these without breaking the %d it already gets right. Prefer the shortest rewrite that does. Same constraints.", replay.Passed)
	return sb.String()
}

// improveAndFinalizeSuggestion runs a bounded improve<->replay loop for a single
// suggestion and writes the best-scoring round's result. Round 1 behaves exactly as the
// single-shot version of this function always did: it rebuilds ShouldMatch/ShouldNotMatch
// fresh from the prompt's *current* example corpus (including on a regenerate round, where
// the pre-corpus version of this code instead replayed a
// conversation frozen around one snapshot email), calls the improver (a first round when
// priorConv is empty and note carries the correction comment, a refinement round when
// priorConv is non-empty and userComment carries the user's feedback on the previous
// suggestion), and optionally replays the candidate against the same corpus on the
// classify model (see llm.ReplayAgainstExamples — deliberately not the improve model just
// used above it).
//
// What's new: if replay is on and the round didn't score perfectly, the loop feeds the
// replay failures back to the improver as the next user turn (buildReplayFeedbackTurn) and
// tries again, up to improveMaxRounds. It stops early — perfect score, a round that
// doesn't beat the best score seen so far, the round budget, or not enough of the worker's
// remaining deadline left for another attempt (hasTimeForAnotherRound) — and finalizes
// whichever round scored highest, not necessarily the last one: a later round is free to
// try something that makes the score worse, and the loop keeps a strictly-better-or-bust
// standard rather than trusting "more recent" as "better." A round-1 improve-call failure
// is still fatal, exactly as before (there's no earlier round to fall back on); an improve-
// call failure on round 2+ just stops the loop and finalizes the best round already found,
// rather than losing a good candidate to a transient Bedrock error.
//
// Shared by every improveTarget runOne works, batch or regenerate alike, so this logic
// can't drift between the two call sites — see this file's package doc comment.
func (r *improveRunner) improveAndFinalizeSuggestion(ctx context.Context, tw *traceWriter, sid int64, p db.Prompt, originalInstructions string, priorConv []llm.ChatMessage, note, userComment string) {
	// One settings Query for the whole round, resolved with the same pure parsers
	// parseExampleCap/parseImproveMaxRounds/parseReplayConcurrency use — instead of five
	// independent GetSetting round trips (one per setting) this used to make every round.
	settings := loadSettings(ctx, r.store)
	improveCap := parseExampleCap(settings[llm.SettingImproveExampleCap], llm.ImproveExampleCapDefault, llm.ImproveExampleCapMax)
	replayCap := parseExampleCap(settings[llm.SettingReplayExampleCap], llm.ReplayExampleCapDefault, llm.ReplayExampleCapMax)
	// llm.SettingImproveReplay defaults to enabled (unset or "1") — it's a correctness
	// signal the user would otherwise have to eyeball; set it to anything else to skip the
	// extra classify calls replay costs.
	replayOn := settingOr(settings, llm.SettingImproveReplay, "1") == "1"
	maxRounds := parseImproveMaxRounds(settings[llm.SettingImproveMaxRounds])
	replayConc := parseReplayConcurrency(settings[llm.SettingReplayConcurrency])

	// One raw fetch, sampled twice at different caps — see gatherRawExamples' doc comment
	// on why this avoids re-querying the corpus twice in one round. examples (small,
	// token-bounded) drives the improve prompt itself and what gets marked resolved;
	// replayExamples (larger, more representative) is only built when replay will actually
	// use it — no reason to pay for that if replay is off.
	raw := gatherRawExamples(ctx, r.store, p.ID)
	examples := sampleExamples(raw, improveCap)
	shouldMatch, shouldNotMatch := improveRequestExamples(examples)
	if !replayOn {
		// No score to iterate on — same behavior as the pre-loop code, exactly one round.
		maxRounds = 1
	}
	// replayLLMExamples is built once, outside the round loop: the corpus itself (which
	// examples, and which of them were shown to the improve prompt) doesn't change round
	// to round, only the candidate text being scored against it does. HeldOut is set by
	// comparing against examples (the improve prompt's own set) — see replayExamplesFor.
	var replayExamples []db.PromptExample
	var replayLLMExamples []llm.ReplayExample
	var heldOutSubmitted int64
	var concurrency int
	if replayOn {
		replayExamples = sampleExamples(raw, replayCap)
		replayLLMExamples = replayExamplesFor(replayExamples, examples)
		for _, ex := range replayLLMExamples {
			if ex.HeldOut {
				heldOutSubmitted++
			}
		}
		concurrency = replayConc
	}

	req := llm.ImproveRequest{
		PromptName: p.Name, LabelName: p.LabelName, OriginalInstructions: originalInstructions,
		ShouldMatch: shouldMatch, ShouldNotMatch: shouldNotMatch,
		UserNote: note, PriorConversation: priorConv, UserComment: userComment,
		PastAttempts: attemptsForPrompt(ctx, r.store, p),
	}

	// rounds, candidates, convs, and replays are parallel slices, one entry per completed
	// round — kept separate from db.SuggestionRoundSummary (which only needs a handful of
	// scalar fields for persistence) because the full conversation and replay failures for
	// every round would be wasteful to carry in what gets JSON-marshaled onto the
	// suggestion row; only the winning round's need to survive past this function.
	var rounds []db.SuggestionRoundSummary
	var candidates []string
	var convs [][]llm.ChatMessage
	var replays []llm.ReplayResult

	for n := 1; n <= maxRounds; n++ {
		round := int64(n)
		tw.Event(ctx, db.TraceKindRoundStart, round, "")
		roundStart := time.Now()

		suggested, conv, llmErr := r.llm.ImprovePromptInstructions(ctx, req, tw.Sink(ctx, round))
		if llmErr != nil {
			slog.Error("improve prompt", "suggestion_id", sid, "prompt_id", p.ID, "round", n, "err", llmErr)
			tw.Event(ctx, db.TraceKindError, round, llmErr.Error())
			if len(rounds) == 0 {
				// Round 1 failing is fatal — there's no earlier candidate to fall back on,
				// same as the single-shot version of this function always did. Detached,
				// bounded write ctx (see terminalWriteCtx): a stalled improve call can run
				// the round's own ctx out to its deadline before returning this error, and
				// the terminal status write must still land even then.
				writeCtx, cancel := terminalWriteCtx(ctx)
				err := finalizeFailure(writeCtx, r.store, sid, llmErr)
				cancel()
				if err != nil {
					slog.Error("finalize suggestion failed", "suggestion_id", sid, "err", err)
				}
				return
			}
			tw.Event(ctx, db.TraceKindNote, round, "keeping the best round found so far after this error")
			break
		}
		tw.Event(ctx, db.TraceKindCandidate, round, suggested)

		var replay llm.ReplayResult
		if replayOn {
			tw.Event(ctx, db.TraceKindReplayStart, round, "")
			// Bounded fan-out (replayConcurrency, SettingReplayConcurrency) — see
			// ReplayConcurrencyDefault's doc comment (llm/bedrock.go) for why an
			// unbounded fan-out here is what used to collapse most of a round's corpus to
			// "excluded from score" under Bedrock throttling: every call's timeout clock
			// started at once, so throttling took the whole batch out together.
			replay = r.llm.ReplayAgainstExamples(ctx, r.store, suggested, replayLLMExamples, concurrency)
			tw.Event(ctx, db.TraceKindReplayDone, round, fmt.Sprintf("%d/%d (held-out %d/%d, %d errored)", replay.Passed, replay.Total, replay.HeldOutPassed, replay.HeldOutTotal, replay.Errored))
			if hits := llm.CitesExamples(suggested, replayLLMExamples); len(hits) > 0 {
				tw.Event(ctx, db.TraceKindNote, round, fmt.Sprintf("this rewrite names %s from the examples' senders instead of generalizing", strings.Join(hits, ", ")))
			}
		}
		rounds = append(rounds, db.SuggestionRoundSummary{
			N: n, Candidate: suggested,
			Passed: int64(replay.Passed), Total: int64(replay.Total), Errored: int64(replay.Errored),
			HeldOutTotal: int64(replay.HeldOutTotal), HeldOutPassed: int64(replay.HeldOutPassed),
			PosTotal: int64(replay.All.PosTotal), PosPassed: int64(replay.All.PosPassed),
			NegTotal: int64(replay.All.NegTotal), NegPassed: int64(replay.All.NegPassed),
			HeldOutPosTotal: int64(replay.HeldOut.PosTotal), HeldOutPosPassed: int64(replay.HeldOut.PosPassed),
			HeldOutNegTotal: int64(replay.HeldOut.NegTotal), HeldOutNegPassed: int64(replay.HeldOut.NegPassed),
		})
		candidates = append(candidates, suggested)
		convs = append(convs, conv)
		replays = append(replays, replay)

		// "Improved" means round n is the best seen across every round up to and
		// including it — reusing selectBestRound's own comparison rule (roundBetter)
		// rather than duplicating it, so there's exactly one definition of "better" for
		// both the stop decision and the final pick after the loop ends.
		improved := selectBestRound(rounds, heldOutSubmitted) == len(rounds)-1
		timeRemains := hasTimeForAnotherRound(ctx, time.Since(roundStart))
		if stop, reason := improveLoopStop(n, maxRounds, replayOn, replay, improved, timeRemains); stop {
			if reason != "" {
				tw.Event(ctx, db.TraceKindNote, round, reason)
			}
			break
		}

		req.PriorConversation = conv
		req.UserComment = buildReplayFeedbackTurn(suggested, replay, replayExamples, replayLLMExamples)
	}

	bestIdx := selectBestRound(rounds, heldOutSubmitted)
	bestN := rounds[bestIdx].N
	bestSuggested := candidates[bestIdx]
	bestConv := convs[bestIdx]
	bestReplay := replays[bestIdx]

	convJSON, convErr := json.Marshal(bestConv)
	// Recorded on every generate/regenerate round, so applying whichever round's
	// suggestion the user actually accepts marks the examples that shaped *that* version —
	// not stale keys from an earlier round if the corpus shifted in between (see this
	// function's doc comment). Gated on the winning round's own replay evidence
	// (fixedByReplay) — see problemExampleKeys' doc comment for why a Missed example the
	// winning candidate never actually confirmed fixed must not resolve.
	problemKeysJSON, keysErr := json.Marshal(problemExampleKeys(examples, replayOn, fixedByReplay(bestReplay, replayExamples)))
	roundsJSON, roundsErr := json.Marshal(rounds)
	// These are concrete slices of plain structs, so a failure means one of those types
	// grew a field JSON can't encode. Bail rather than finalize the suggestion with a
	// field silently empty, which would surface much later as a corrupt row.
	if err := errors.Join(convErr, keysErr, roundsErr); err != nil {
		slog.Error("marshal suggestion fields", "suggestion_id", sid, "err", err)
		return
	}
	finalize := db.FinalizePromptSuggestionParams{
		ID:                    sid,
		SuggestedInstructions: bestSuggested,
		ConversationJSON:      string(convJSON),
		Status:                db.SuggestionStatusPending,
		UserComment:           userComment,
		ProblemExampleKeys:    string(problemKeysJSON),
		RoundsJSON:            string(roundsJSON),
		RoundsRun:             int64(len(rounds)),
		BestRound:             int64(bestN),
	}
	if replayOn {
		failuresJSON, failuresErr := json.Marshal(bestReplay.Failures)
		if failuresErr != nil {
			slog.Error("marshal replay failures", "suggestion_id", sid, "err", failuresErr)
			return
		}
		finalize.ReplayModel = bestReplay.Model
		finalize.ReplayTotal = int64(bestReplay.Total)
		finalize.ReplayPassed = int64(bestReplay.Passed)
		finalize.ReplayBaseline = int64(bestReplay.Baseline)
		finalize.ReplayErrored = int64(bestReplay.Errored)
		finalize.ReplayHeldOutTotal = int64(bestReplay.HeldOutTotal)
		finalize.ReplayHeldOutPassed = int64(bestReplay.HeldOutPassed)
		finalize.ReplayFailures = string(failuresJSON)
		finalize.ReplayPosTotal = int64(bestReplay.All.PosTotal)
		finalize.ReplayPosPassed = int64(bestReplay.All.PosPassed)
		finalize.ReplayNegTotal = int64(bestReplay.All.NegTotal)
		finalize.ReplayNegPassed = int64(bestReplay.All.NegPassed)
		adequateCoverage := hasAdequateCoverage(finalize.ReplayTotal, finalize.ReplayTotal+finalize.ReplayErrored)
		if !adequateCoverage {
			tw.Event(ctx, db.TraceKindNote, int64(bestN), fmt.Sprintf("only %d of %d submitted examples were actually scored — this score is not reliable", finalize.ReplayTotal, finalize.ReplayTotal+finalize.ReplayErrored))
		}
		// NoGain: the winning round didn't beat the rule it's proposing to replace, over the
		// same scored examples (ReplayBaseline shares ReplayTotal's exact denominator — see
		// its doc comment, db/models.go). selectBestRound only ever ranks rounds against each
		// other, so without this a rewrite that's measurably worse than what's already live
		// would still finalize as an ordinary pending suggestion. Only checked with adequate
		// coverage — a low-coverage comparison isn't trustworthy either way (see
		// hasAdequateCoverage's doc comment).
		if adequateCoverage && finalize.ReplayPassed <= finalize.ReplayBaseline {
			finalize.NoGain = true
			tw.Event(ctx, db.TraceKindNote, int64(bestN), fmt.Sprintf("no better than the current rule (%d/%d vs. %d/%d already) — review before applying", finalize.ReplayPassed, finalize.ReplayTotal, finalize.ReplayBaseline, finalize.ReplayTotal))
		}
	}
	// The done event is emitted only after FinalizePromptSuggestion actually lands — the
	// trace poll's completion signal (see the trace endpoint, server.go) tells the browser
	// it's safe to re-fetch the suggestion card, and that's only true once the terminal
	// status has been written, not merely decided.
	// Detached, bounded write ctx (see terminalWriteCtx): the round's own ctx may already be
	// at (or past) its deadline here — e.g. a stalled round that ran the full budget before
	// improveLoopStop finally gave up — and the terminal status write must still land.
	writeCtx, cancel := terminalWriteCtx(ctx)
	err := r.store.FinalizePromptSuggestion(writeCtx, finalize)
	cancel()
	if err != nil {
		slog.Error("finalize suggestion failed", "suggestion_id", sid, "err", err)
		tw.Event(ctx, db.TraceKindError, int64(bestN), "saved the suggestion but failed to record its final status: "+err.Error())
		return
	}
	tw.Event(ctx, db.TraceKindDone, int64(bestN), "")
	slog.Info("improve suggestion ready", "suggestion_id", sid, "prompt_id", p.ID, "rounds_run", len(rounds), "best_round", bestN, "replay_total", finalize.ReplayTotal, "replay_passed", finalize.ReplayPassed, "replay_errored", finalize.ReplayErrored)
}

// ============================================================
// Dispatch — hands targets off to the worker, from WebFunction
// ============================================================

// dispatchImprove hands a batch of suggestion targets to the MODE=improve worker via an
// async (Event) Invoke, so the round runs inside its own Lambda invocation for its whole
// duration. Falls back to running improveRunner.handle in-process, in a goroutine off
// s.ctx (the server's long-lived context, not the request's — the request may return
// before the round finishes), when s.improveLambda is nil: cfg.ImproveFunctionName unset,
// which is the case for local dev (`make run`) and the test suite, neither of which has a
// second Lambda to invoke.
func (s *server) dispatchImprove(ctx context.Context, targets []improveTarget) {
	if len(targets) == 0 {
		return
	}
	if s.improveLambda == nil {
		go func() { _ = s.improver.handle(s.ctx, improveEvent{Targets: targets}) }()
		return
	}

	payload, err := json.Marshal(improveEvent{Targets: targets})
	if err != nil {
		slog.Error("dispatch improve: marshal event", "err", err)
		s.failDispatch(ctx, targets, err)
		return
	}
	if _, err := s.improveLambda.Invoke(ctx, &lambda.InvokeInput{
		FunctionName:   aws.String(s.cfg.ImproveFunctionName),
		InvocationType: lambdatypes.InvocationTypeEvent,
		Payload:        payload,
	}); err != nil {
		slog.Error("dispatch improve: invoke failed", "function", s.cfg.ImproveFunctionName, "err", err)
		s.failDispatch(ctx, targets, err)
	}
}

// failDispatch writes a terminal 'failed' status directly on every target when the
// hand-off to the improve worker itself couldn't be made (marshal error, Invoke error) —
// a suggestion must never be left on 'generating' by a failure that happens before the
// worker even starts, since nothing downstream of a failed Invoke call will ever run
// ClaimPromptSuggestion or improveAndFinalizeSuggestion for it.
func (s *server) failDispatch(ctx context.Context, targets []improveTarget, cause error) {
	startErr := fmt.Errorf("failed to start: %w", cause)
	for _, t := range targets {
		if err := finalizeFailure(ctx, s.store, t.SuggestionID, startErr); err != nil {
			slog.Error("dispatch improve: write failure status failed", "suggestion_id", t.SuggestionID, "err", err)
		}
	}
}
