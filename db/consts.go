package db

const (
	SuggestionStatusGenerating = "generating"
	SuggestionStatusPending    = "pending"
	SuggestionStatusApplied    = "applied"
	SuggestionStatusDismissed  = "dismissed"
	SuggestionStatusFailed     = "failed"
	TriggerKindFalsePositive   = "false_positive"
	TriggerKindFalseNegative   = "false_negative"
)

// PromptExample verdicts. Exactly two buckets: every reviewed email is either evidence a
// rule should match (VerdictConfirmedPositive) or evidence it should not
// (VerdictConfirmedNegative) — there is no separate "the rule got this wrong" verdict
// anymore. That distinction is now carried by PromptExample.Missed instead of by which
// bucket the row lands in (see db/models.go's doc comment on PromptExample), so a rule's
// example corpus stays exactly two lists no matter how the row was produced.
//
// VerdictConfirmedNegative is deliberately never written for a rule that simply didn't
// match and still doesn't — only for a rule the user actually unchecked (Missed: true, a
// real "this was wrong" correction). Every active rule not involved in a given review would
// otherwise pick up a negative example on every single email anyone reviews, burying the
// rules that actually need attention in negatives nobody explicitly confirmed. See
// singleRecategorizeVerdicts/bulkVerdictsAndPlan (recategorize.go/recategorize_bulk.go).
const (
	VerdictConfirmedPositive = "confirmed_positive"
	VerdictConfirmedNegative = "confirmed_negative"
)

// VerdictOrder is the fixed display/processing order for the two verdicts above — the
// order examples get sampled per-verdict, pruned per-verdict, and shown to the user, so a
// change here changes all of those consistently at once instead of drifting between
// separately-written copies of this same slice.
var VerdictOrder = []string{VerdictConfirmedPositive, VerdictConfirmedNegative}

// SuggestionTraceEvent kinds (see db/models.go's doc comment on SuggestionTraceEvent).
// round_start/candidate/replay_start/replay_done/note/error/done are structural — they mark
// a state change in the improve round and are flushed immediately by the trace writer.
// answer/thinking are streamed deltas, coalesced before being written.
const (
	TraceKindRoundStart  = "round_start"
	TraceKindAnswer      = "answer"
	TraceKindThinking    = "thinking"
	TraceKindCandidate   = "candidate"
	TraceKindReplayStart = "replay_start"
	TraceKindReplayDone  = "replay_done"
	TraceKindNote        = "note"
	TraceKindError       = "error"
	TraceKindDone        = "done"
)

// PromptVersion.Source values (see db/models.go's doc comment on PromptVersion).
const (
	PromptVersionSourceInitial    = "initial"
	PromptVersionSourceSuggestion = "suggestion"
	PromptVersionSourceManual     = "manual"
)

// DynamoDB attribute names and expression placeholders reused across queries.
const (
	attrTTL       = "ttl"
	attrStatus    = "status"
	attrMessageID = "messageId"
	attrAccountID = "accountId"
	attrLabelName = "labelName"
	attrCreatedAt = "createdAt"

	exprPK        = ":pk"
	exprStatus    = ":st"
	exprUpdatedAt = ":ua"
)
