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
