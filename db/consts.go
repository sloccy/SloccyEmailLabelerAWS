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
