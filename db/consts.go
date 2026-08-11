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

// PromptExample verdicts. VerdictFalsePositive/VerdictFalseNegative intentionally reuse
// the TriggerKind* string values above — they describe the same two failure modes, just
// recorded against a permanent example instead of a one-shot suggestion trigger.
// VerdictConfirmedPositive is new: a rule the user left checked/applied during a
// recategorization, i.e. a positive the correction affirmed rather than changed. There is
// deliberately no "confirmed negative" — see the recategorize verdict tables in
// recategorize.go for why leaving a rule unchecked/unapplied records nothing.
const (
	VerdictFalsePositive     = TriggerKindFalsePositive
	VerdictFalseNegative     = TriggerKindFalseNegative
	VerdictConfirmedPositive = "confirmed_positive"
)

// VerdictOrder is the fixed display/processing order for the three verdicts above — the
// order examples get sampled per-verdict, pruned per-verdict, and shown to the user, so a
// change here changes all of those consistently at once instead of drifting between
// separately-written copies of this same slice.
var VerdictOrder = []string{VerdictFalseNegative, VerdictFalsePositive, VerdictConfirmedPositive}

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

// PromptExample.Source values (see db/models.go's doc comment on PromptExample). Distinct
// from the PromptVersionSource* constants above — they describe different things (which
// rule text was live vs. how this example's verdict was recorded) and happen to share the
// word "manual" for an unrelated reason (a human did it), not because they're the same
// concept.
const (
	ExampleSourceManual  = "manual"
	ExampleSourcePassive = "passive"
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
