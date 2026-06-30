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
