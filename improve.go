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
// (status='generating') is already inserted/marked by the caller — seedImproveSuggestions
// (recategorize.go), handleBulkRecategorize (recategorize_bulk.go), or
// handlePromptSuggestionRegenerate (server.go) — before the async invoke fires, so the
// worker only ever needs the id plus enough context to run one improve call.
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

// writeFailure stamps a terminal 'failed' status directly, bypassing the normal
// improveAndFinalizeSuggestion path — used when something has gone wrong badly enough
// (a panic, a blown deadline, a missing prompt row) that the normal path can't be trusted
// to write it itself. ctx may already be past its deadline (that's often why this is being
// called), so the write uses a fresh bounded context detached from it rather than
// inheriting an already-expired one.
func (r *improveRunner) writeFailure(ctx context.Context, tw *traceWriter, suggestionID int64, cause error) {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	// The trace's error event is emitted after the status write, same reasoning as
	// improveAndFinalizeSuggestion's done event: the trace poll's terminal signal must mean
	// "the suggestion's status is actually final," not merely "about to be."
	if err := finalizeFailure(writeCtx, r.store, suggestionID, cause); err != nil {
		slog.Error("improve worker: write failure status failed", "suggestion_id", suggestionID, "err", err)
	}
	tw.Event(writeCtx, db.TraceKindError, 0, cause.Error())
}

// rawLimitForVerdict bounds how many rows gatherRawExamples pulls per verdict before any
// sampling happens. confirmed_positive gets a much wider window than the other two:
// passive confirmation (processor.processEmail) writes one on every ordinary classify
// match, not just on a manual correction, so it can grow far faster than
// false_negative/false_positive — sampleExamples needs a wide enough raw pool to actually
// find diverse manual/recurring rows before a wall of recent passive confirms would
// otherwise fill the window on its own. Both limits are cheap regardless of corpus size:
// ListExamplesByVerdict's cost is bounded by the Limit passed in, not by how large the
// partition has grown.
func rawLimitForVerdict(verdict string) int32 {
	if verdict == db.VerdictConfirmedPositive {
		return 200
	}
	return 60
}

// gatherRawExamples reads a rule's whole raw example window (see rawLimitForVerdict),
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
		examples, err := store.ListExamplesByVerdict(ctx, promptID, v, rawLimitForVerdict(v))
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
// ReplayAgainstExamples scores a candidate rule against (see improveAndFinalizeSuggestion).
// Deliberately independent of and larger than improveExampleCap (see
// SettingReplayExampleCap's doc comment, llm/bedrock.go).
func replayExampleCap(ctx context.Context, store *db.Store) int {
	v, err := store.GetSetting(ctx, llm.SettingReplayExampleCap)
	if err != nil || v == "" {
		return llm.ReplayExampleCapDefault
	}
	return parseExampleCap(v, llm.ReplayExampleCapDefault, llm.ReplayExampleCapMax)
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
// enabled (improveReplayEnabled).

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
// sampleVerdict): examples that recurred after a prior fix, then manually-reviewed
// examples, then passively-confirmed ones — each of the latter two round-robinned across
// sender+subject buckets rather than taken newest-first. See db.PromptExample.Source and
// .Recurred for what feeds the tiering.
func sampleExamples(examples []db.PromptExample, perVerdictCap int) []db.PromptExample {
	if perVerdictCap <= 0 {
		return nil
	}
	byVerdict := make(map[string][]db.PromptExample, 3)
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
	var recurred, manual, passive []db.PromptExample
	for _, ex := range candidates {
		switch {
		case ex.Recurred:
			recurred = append(recurred, ex)
		case ex.Source == db.ExampleSourceManual:
			manual = append(manual, ex)
		default:
			// Passive, or "" for a row written before Source tracking existed — treated the
			// same as passive: neither carries the "a human explicitly reviewed this" signal
			// manual does.
			passive = append(passive, ex)
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
	out = append(out, roundRobinBySender(manual, verdictCap-len(out))...)
	out = append(out, roundRobinBySender(passive, verdictCap-len(out))...)
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

// problemExampleKeys picks out the false_negative/false_positive entries from examples —
// the "problems" a suggestion built from them is meant to fix — and returns enough
// per-example key info (db.ResolvedExampleKey) for Store.MarkExamplesResolved to find and
// mark them once the suggestion is applied. confirmed_positive examples are never
// included: they aren't problems to resolve, they're guardrails a rewrite shouldn't have
// broken, and marking one resolved would just hide it from future improve rounds for no
// reason.
func problemExampleKeys(examples []db.PromptExample) []db.ResolvedExampleKey {
	var keys []db.ResolvedExampleKey
	for _, ex := range examples {
		if ex.Verdict == db.VerdictConfirmedPositive {
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

// improveRequestExamples groups a rule's example corpus into the three llm.ExampleRef
// slices llm.ImproveRequest expects, keyed by each example's stored Verdict.
func improveRequestExamples(examples []db.PromptExample) (shouldMatch, shouldNotMatch, alreadyCorrect []llm.ExampleRef) {
	for _, ex := range examples {
		ref := llm.ExampleRef{Sender: ex.Sender, Subject: ex.Subject, Excerpt: ex.BodyExcerpt, Recurred: ex.Recurred}
		switch ex.Verdict {
		case db.VerdictFalseNegative:
			shouldMatch = append(shouldMatch, ref)
		case db.VerdictFalsePositive:
			shouldNotMatch = append(shouldNotMatch, ref)
		case db.VerdictConfirmedPositive:
			alreadyCorrect = append(alreadyCorrect, ref)
		}
	}
	return shouldMatch, shouldNotMatch, alreadyCorrect
}

// replayExamplesFor converts a rule's example corpus into llm.ReplayExample values:
// false_negative and confirmed_positive examples are expected to match the candidate rule,
// false_positive examples are expected not to.
func replayExamplesFor(examples []db.PromptExample) []llm.ReplayExample {
	out := make([]llm.ReplayExample, len(examples))
	for i, ex := range examples {
		out[i] = llm.ReplayExample{
			Verdict: ex.Verdict,
			Sender:  ex.Sender,
			Subject: ex.Subject,
			Excerpt: ex.BodyExcerpt,
			Want:    ex.Verdict != db.VerdictFalsePositive,
		}
	}
	return out
}

// improveReplayEnabled reports whether a candidate suggestion should be replay-validated
// against the classify model (see llm.ReplayAgainstExamples). Defaults to enabled — "1" or
// unset — since it's a correctness signal the user would otherwise have to eyeball; set
// llm.SettingImproveReplay to anything else in Settings to skip the extra classify calls.
func (r *improveRunner) improveReplayEnabled(ctx context.Context) bool {
	v, err := r.store.GetSetting(ctx, llm.SettingImproveReplay)
	return err != nil || v == "" || v == "1"
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

// improveMaxRounds resolves llm.SettingImproveMaxRounds. Read directly via GetSetting
// rather than through llm.Client: the loop is orchestrated by this package, not by the LLM
// client (same reasoning SettingImproveReplay's doc comment gives).
func (r *improveRunner) improveMaxRounds(ctx context.Context) int {
	v, err := r.store.GetSetting(ctx, llm.SettingImproveMaxRounds)
	if err != nil || v == "" {
		return llm.ImproveMaxRoundsDefault
	}
	return parseImproveMaxRounds(v)
}

// selectBestRound returns the index into rounds of the best-scoring round: strictly
// higher Passed wins; a tie keeps the earlier round, since it's closer to the user's
// original scope and later rounds tend to drift wider chasing a handful of edge cases.
// Returns -1 for an empty slice. Used both to pick the round improveAndFinalizeSuggestion
// finalizes and, inline in its loop, to answer "did the round I just ran actually improve
// on what came before?" (see improveLoopStop) — one comparison rule, not two.
func selectBestRound(rounds []db.SuggestionRoundSummary) int {
	best := -1
	for i, rd := range rounds {
		if best == -1 || rd.Passed > rounds[best].Passed {
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
	if replay.Total > 0 && replay.Passed == replay.Total {
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
func buildReplayFeedbackTurn(replay llm.ReplayResult, examples []db.PromptExample) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "VALIDATION: your last rewrite scored %d/%d on the real classifier.\n", replay.Passed, replay.Total)

	var wronglyMatched, wronglyMissed []llm.ReplayFailure
	for _, f := range replay.Failures {
		if f.Got {
			wronglyMatched = append(wronglyMatched, f)
		} else {
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

	fmt.Fprintf(&sb, "\nRewrite again to fix these without breaking the %d it already gets right. Same constraints.", replay.Passed)
	return sb.String()
}

// improveAndFinalizeSuggestion runs a bounded improve<->replay loop for a single
// suggestion and writes the best-scoring round's result. Round 1 behaves exactly as the
// single-shot version of this function always did: it rebuilds ShouldMatch/ShouldNotMatch/
// AlreadyCorrect fresh from the prompt's *current* example corpus (including on a
// regenerate round, where the pre-corpus version of this code instead replayed a
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
	// One raw fetch, sampled twice at different caps — see gatherRawExamples' doc comment
	// on why this avoids re-querying the corpus twice in one round. examples (small,
	// token-bounded) drives the improve prompt itself and what gets marked resolved;
	// replayExamples (larger, more representative) is only built when replay will actually
	// use it — no reason to pay for that if replay is off.
	raw := gatherRawExamples(ctx, r.store, p.ID)
	examples := sampleExamples(raw, improveExampleCap(ctx, r.store))
	shouldMatch, shouldNotMatch, alreadyCorrect := improveRequestExamples(examples)
	replayOn := r.improveReplayEnabled(ctx)
	maxRounds := r.improveMaxRounds(ctx)
	if !replayOn {
		// No score to iterate on — same behavior as the pre-loop code, exactly one round.
		maxRounds = 1
	}
	var replayExamples []db.PromptExample
	if replayOn {
		replayExamples = sampleExamples(raw, replayExampleCap(ctx, r.store))
	}

	req := llm.ImproveRequest{
		PromptName: p.Name, LabelName: p.LabelName, OriginalInstructions: originalInstructions,
		ShouldMatch: shouldMatch, ShouldNotMatch: shouldNotMatch, AlreadyCorrect: alreadyCorrect,
		UserNote: note, PriorConversation: priorConv, UserComment: userComment,
		PastAttempts: attemptsForPrompt(ctx, r.store, p),
	}

	// rounds, candidates, convs, and replays are parallel slices, one entry per completed
	// round — kept separate from db.SuggestionRoundSummary (which only needs N/Candidate/
	// Passed/Total for persistence) because the full conversation and replay failures for
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
				// same as the single-shot version of this function always did.
				if err := finalizeFailure(ctx, r.store, sid, llmErr); err != nil {
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
			// concurrency 0: unbounded fan-out. This used to pass cfg.ClassifyConcurrency
			// (default 6) because the goroutine it ran in shared WebFunction's 128MB/30s
			// budget with live HTTP requests; running inside ImproveFunction's own
			// 1024MB/900s invocation, there's no such budget to protect, and
			// ReplayAgainstExamples' own concurrency<=0 handling (llm/bedrock.go) skips
			// the semaphore entirely — Bedrock's adaptive retryer (newBedrockRetryer)
			// already provides the real backpressure. A bounded round budget (maxRounds)
			// is what keeps this fan-out from multiplying unboundedly, not a concurrency
			// limit here.
			replay = r.llm.ReplayAgainstExamples(ctx, r.store, suggested, replayExamplesFor(replayExamples), 0)
			tw.Event(ctx, db.TraceKindReplayDone, round, fmt.Sprintf("%d/%d", replay.Passed, replay.Total))
		}
		rounds = append(rounds, db.SuggestionRoundSummary{N: n, Candidate: suggested, Passed: int64(replay.Passed), Total: int64(replay.Total)})
		candidates = append(candidates, suggested)
		convs = append(convs, conv)
		replays = append(replays, replay)

		// "Improved" means round n is the best seen across every round up to and
		// including it — reusing selectBestRound's own comparison rule (strict >, ties
		// favor earlier) rather than duplicating it, so there's exactly one definition of
		// "better" for both the stop decision and the final pick after the loop ends.
		improved := selectBestRound(rounds) == len(rounds)-1
		timeRemains := hasTimeForAnotherRound(ctx, time.Since(roundStart))
		if stop, reason := improveLoopStop(n, maxRounds, replayOn, replay, improved, timeRemains); stop {
			if reason != "" {
				tw.Event(ctx, db.TraceKindNote, round, reason)
			}
			break
		}

		req.PriorConversation = conv
		req.UserComment = buildReplayFeedbackTurn(replay, replayExamples)
	}

	bestIdx := selectBestRound(rounds)
	bestN := rounds[bestIdx].N
	bestSuggested := candidates[bestIdx]
	bestConv := convs[bestIdx]
	bestReplay := replays[bestIdx]

	convJSON, _ := json.Marshal(bestConv) //nolint:errchkjson // []llm.ChatMessage cannot fail
	// Recorded on every generate/regenerate round, so applying whichever round's
	// suggestion the user actually accepts marks the examples that shaped *that* version —
	// not stale keys from an earlier round if the corpus shifted in between (see this
	// function's doc comment).
	problemKeysJSON, _ := json.Marshal(problemExampleKeys(examples)) //nolint:errchkjson // []db.ResolvedExampleKey cannot fail
	roundsJSON, _ := json.Marshal(rounds)                            //nolint:errchkjson // []db.SuggestionRoundSummary cannot fail
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
		failuresJSON, _ := json.Marshal(bestReplay.Failures) //nolint:errchkjson // []llm.ReplayFailure cannot fail
		finalize.ReplayModel = bestReplay.Model
		finalize.ReplayTotal = int64(bestReplay.Total)
		finalize.ReplayPassed = int64(bestReplay.Passed)
		finalize.ReplayBaseline = replayBaseline(replayExamples)
		finalize.ReplayFailures = string(failuresJSON)
	}
	// The done event is emitted only after FinalizePromptSuggestion actually lands — the
	// trace poll's completion signal (see the trace endpoint, server.go) tells the browser
	// it's safe to re-fetch the suggestion card, and that's only true once the terminal
	// status has been written, not merely decided.
	if err := r.store.FinalizePromptSuggestion(ctx, finalize); err != nil {
		slog.Error("finalize suggestion failed", "suggestion_id", sid, "err", err)
		tw.Event(ctx, db.TraceKindError, int64(bestN), "saved the suggestion but failed to record its final status: "+err.Error())
		return
	}
	tw.Event(ctx, db.TraceKindDone, int64(bestN), "")
	slog.Info("improve suggestion ready", "suggestion_id", sid, "prompt_id", p.ID, "rounds_run", len(rounds), "best_round", bestN, "replay_total", finalize.ReplayTotal, "replay_passed", finalize.ReplayPassed)
}

// replayBaseline is the free baseline ReplayResult.Passed is compared against: how many of
// the same examples the *original* rule already got right, derived from the verdict
// recorded when each example was created rather than by re-running the original
// instructions. false_negative and false_positive examples were misses by definition
// (that's why they were recorded); confirmed_positive examples were hits.
func replayBaseline(examples []db.PromptExample) int64 {
	var n int64
	for _, ex := range examples {
		if ex.Verdict == db.VerdictConfirmedPositive {
			n++
		}
	}
	return n
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
