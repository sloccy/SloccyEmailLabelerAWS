package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
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

	p, err := r.store.GetPrompt(ctx, t.PromptID)
	if err != nil {
		slog.Error("improve worker: get prompt", "suggestion_id", t.SuggestionID, "prompt_id", t.PromptID, "err", err)
		r.writeFailure(ctx, t.SuggestionID, err)
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
			r.writeFailure(ctx, t.SuggestionID, fmt.Errorf("panic: %v", rec))
			return
		}
		if !done {
			r.writeFailure(ctx, t.SuggestionID, fmt.Errorf("worker deadline exceeded before a result was written"))
		}
	}()

	r.improveAndFinalizeSuggestion(callCtx, t.SuggestionID, p, t.OriginalInstructions, t.PriorConversation, t.Note, t.UserComment)
	done = true
}

// writeFailure stamps a terminal 'failed' status directly, bypassing the normal
// improveAndFinalizeSuggestion path — used when something has gone wrong badly enough
// (a panic, a blown deadline, a missing prompt row) that the normal path can't be trusted
// to write it itself. ctx may already be past its deadline (that's often why this is being
// called), so the write uses a fresh bounded context detached from it rather than
// inheriting an already-expired one.
func (r *improveRunner) writeFailure(ctx context.Context, suggestionID int64, cause error) {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := r.store.FinalizePromptSuggestion(writeCtx, db.FinalizePromptSuggestionParams{
		ID:                    suggestionID,
		SuggestedInstructions: "",
		ConversationJSON:      "[]",
		Status:                db.SuggestionStatusFailed,
		UserComment:           cause.Error(),
	}); err != nil {
		slog.Error("improve worker: write failure status failed", "suggestion_id", suggestionID, "err", err)
	}
}

// selectExamplesForPrompt reads a rule's example corpus: the newest ~10 of each verdict
// (false_negative/false_positive/confirmed_positive), deduped by message across verdict
// groups. Feeds both ImprovePromptInstructions' three example slices and
// ReplayAgainstExamples' scoring set from a single read, rather than querying the corpus
// twice for the same data. A free function (not an improveRunner method) since server.go's
// suggestionDetailView also calls it, purely for display, with no improve round involved.
func selectExamplesForPrompt(ctx context.Context, store *db.Store, promptID int64) []db.PromptExample {
	// Raw fetch is generous — 40 per verdict, not the eventual 10-per-verdict target —
	// because dedupeBySenderSubject below can collapse many rows down to one when a
	// recurring sender (a daily digest, a templated receipt) has been writing a
	// confirmed_positive on every match via passive confirmation
	// (processor.processEmail), not just on manual corrections. Widening this is cheap:
	// ListExamplesByVerdict's cost is Limit-bounded regardless of corpus size.
	const perVerdictRawLimit = 40
	const perVerdictTarget = 10

	var all []db.PromptExample
	for _, v := range []string{db.VerdictFalseNegative, db.VerdictFalsePositive, db.VerdictConfirmedPositive} {
		examples, err := store.ListExamplesByVerdict(ctx, promptID, v, perVerdictRawLimit)
		if err != nil {
			slog.Error("select examples for prompt", "prompt_id", promptID, "verdict", v, "err", err)
			continue
		}
		all = append(all, examples...)
	}
	all = filterResolved(all)

	// The same message can appear in more than one verdict's top-N if it was corrected more
	// than once over time (e.g. false_positive once, confirmed_positive later after the rule
	// was fixed). Keeping every occurrence would hand the improver a live contradiction —
	// "this email is both a false positive and a confirmed positive" — so only the newest
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

	return dedupeBySenderSubject(survivors, perVerdictTarget)
}

// senderSubjectKey normalizes a sender+subject pair for dedupeBySenderSubject: trimmed and
// case-folded, so trailing whitespace or casing differences (a mail client rendering
// headers slightly differently across sends of the same templated email) don't defeat the
// dedup. \x00 as a separator can't appear in either field, so it can't collide across a
// sender/subject boundary.
func senderSubjectKey(sender, subject string) string {
	return strings.ToLower(strings.TrimSpace(sender)) + "\x00" + strings.ToLower(strings.TrimSpace(subject))
}

// dedupeBySenderSubject walks examples — already newest-first within each verdict's
// contiguous span, per selectExamplesForPrompt's ordering guarantee — and, independently
// per verdict, keeps only the first (i.e. newest) occurrence of each sender+subject pair,
// capping each verdict at perVerdictTarget. Without this, a recurring sender could fill an
// entire verdict's example budget with near-identical rows: passive confirmation
// (processor.processEmail) writes a confirmed_positive on every ordinary classify match,
// so a daily newsletter from the same sender+subject would otherwise dominate the
// "already correct" evidence fed to the improver instead of a diverse sample.
func dedupeBySenderSubject(examples []db.PromptExample, perVerdictTarget int) []db.PromptExample {
	seen := make(map[string]map[string]bool)
	count := make(map[string]int)
	out := make([]db.PromptExample, 0, len(examples))
	for _, ex := range examples {
		if count[ex.Verdict] >= perVerdictTarget {
			continue
		}
		key := senderSubjectKey(ex.Sender, ex.Subject)
		if seen[ex.Verdict] == nil {
			seen[ex.Verdict] = make(map[string]bool)
		}
		if seen[ex.Verdict][key] {
			continue
		}
		seen[ex.Verdict][key] = true
		count[ex.Verdict]++
		out = append(out, ex)
	}
	return out
}

// filterResolved drops examples whose problem this rule has already been fixed for — see
// db.PromptExample.ResolvedBySuggestionID's doc comment. Applied first, ahead of both dedup
// passes above in selectExamplesForPrompt, so a resolved example can never win either dedup
// and end up in the output: showing the improver a case it already fixed is meaningless
// unless the rule regressed and missed it again, in which case a fresh (unresolved)
// correction on that email will already be in the pool.
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
		ref := llm.ExampleRef{Sender: ex.Sender, Subject: ex.Subject, Excerpt: ex.BodyExcerpt}
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

// improveAndFinalizeSuggestion runs one improve-plus-optional-replay round for a single
// suggestion and writes the result. It rebuilds ShouldMatch/ShouldNotMatch/AlreadyCorrect
// fresh from the prompt's *current* example corpus on every call — including on a
// regenerate round, where the pre-corpus version of this code instead replayed a
// conversation frozen around one snapshot email — then calls the improver (a first round
// when priorConv is empty and note carries the correction comment, a refinement round when
// priorConv is non-empty and userComment carries the user's feedback on the previous
// suggestion), optionally replays the candidate against the same corpus on the classify
// model (see llm.ReplayAgainstExamples — deliberately not the improve model just used
// above it), and finalizes the suggestion row. Shared by every improveTarget runOne works,
// batch or regenerate alike, so this logic can't drift between the two call sites — see
// this file's package doc comment.
func (r *improveRunner) improveAndFinalizeSuggestion(ctx context.Context, sid int64, p db.Prompt, originalInstructions string, priorConv []llm.ChatMessage, note, userComment string) {
	examples := selectExamplesForPrompt(ctx, r.store, p.ID)
	shouldMatch, shouldNotMatch, alreadyCorrect := improveRequestExamples(examples)

	suggested, conv, llmErr := r.llm.ImprovePromptInstructions(ctx, llm.ImproveRequest{
		PromptName:           p.Name,
		LabelName:            p.LabelName,
		OriginalInstructions: originalInstructions,
		ShouldMatch:          shouldMatch,
		ShouldNotMatch:       shouldNotMatch,
		AlreadyCorrect:       alreadyCorrect,
		UserNote:             note,
		PriorConversation:    priorConv,
		UserComment:          userComment,
	})
	if llmErr != nil {
		slog.Error("improve prompt", "suggestion_id", sid, "prompt_id", p.ID, "err", llmErr)
		if err := r.store.FinalizePromptSuggestion(ctx, db.FinalizePromptSuggestionParams{
			ID:                    sid,
			SuggestedInstructions: "",
			ConversationJSON:      "[]",
			Status:                db.SuggestionStatusFailed,
			UserComment:           llmErr.Error(),
		}); err != nil {
			slog.Error("finalize suggestion failed", "suggestion_id", sid, "err", err)
		}
		return
	}

	convJSON, _ := json.Marshal(conv) //nolint:errchkjson // []llm.ChatMessage cannot fail
	// Recorded on every generate/regenerate round, so applying whichever round's
	// suggestion the user actually accepts marks the examples that shaped *that* version —
	// not stale keys from an earlier round if the corpus shifted in between (see
	// improveAndFinalizeSuggestion's doc comment).
	problemKeysJSON, _ := json.Marshal(problemExampleKeys(examples)) //nolint:errchkjson // []db.ResolvedExampleKey cannot fail
	finalize := db.FinalizePromptSuggestionParams{
		ID:                    sid,
		SuggestedInstructions: suggested,
		ConversationJSON:      string(convJSON),
		Status:                db.SuggestionStatusPending,
		UserComment:           userComment,
		ProblemExampleKeys:    string(problemKeysJSON),
	}
	if r.improveReplayEnabled(ctx) {
		// concurrency 0: unbounded fan-out. This used to pass cfg.ClassifyConcurrency
		// (default 6) because the goroutine it ran in shared WebFunction's 128MB/30s
		// budget with live HTTP requests; running inside ImproveFunction's own 1024MB/900s
		// invocation, there's no such budget to protect, and ReplayAgainstExamples' own
		// concurrency<=0 handling (llm/bedrock.go) skips the semaphore entirely — Bedrock's
		// adaptive retryer (newBedrockRetryer) already provides the real backpressure.
		replay := r.llm.ReplayAgainstExamples(ctx, r.store, suggested, replayExamplesFor(examples), 0)
		failuresJSON, _ := json.Marshal(replay.Failures) //nolint:errchkjson // []llm.ReplayFailure cannot fail
		finalize.ReplayModel = replay.Model
		finalize.ReplayTotal = int64(replay.Total)
		finalize.ReplayPassed = int64(replay.Passed)
		finalize.ReplayBaseline = replayBaseline(examples)
		finalize.ReplayFailures = string(failuresJSON)
	}
	if err := r.store.FinalizePromptSuggestion(ctx, finalize); err != nil {
		slog.Error("finalize suggestion failed", "suggestion_id", sid, "err", err)
	}
	slog.Info("improve suggestion ready", "suggestion_id", sid, "prompt_id", p.ID, "replay_total", finalize.ReplayTotal, "replay_passed", finalize.ReplayPassed)
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
	for _, t := range targets {
		if err := s.store.FinalizePromptSuggestion(ctx, db.FinalizePromptSuggestionParams{
			ID:                    t.SuggestionID,
			SuggestedInstructions: "",
			ConversationJSON:      "[]",
			Status:                db.SuggestionStatusFailed,
			UserComment:           "failed to start: " + cause.Error(),
		}); err != nil {
			slog.Error("dispatch improve: write failure status failed", "suggestion_id", t.SuggestionID, "err", err)
		}
	}
}
