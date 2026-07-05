package db

import (
	"database/sql"
)

// Account, Prompt, CategorizationHistory, Log, LlmDebug, and PromptSuggestion carry
// `dynamodbav` tags: db/store.go marshals/unmarshals them via attributevalue.MarshalMap/
// UnmarshalMap instead of hand-rolled item builders. PK/SK are never tagged here — they're
// key-schema (partition/sort key), constructed directly in store.go from other fields
// (e.g. account id, account id + timestamp), not 1:1 with a single struct field.
//
// Nullable fields use *string/*int64 (not sql.NullString/sql.NullInt64): attributevalue's
// default behavior for a nil pointer is to marshal it as a DynamoDB NULL attribute, which
// matches the wire format these fields already had. sql.NullString/NullInt64 would instead
// marshal as a nested map (two fields, String/Valid) — a silent, incompatible format change
// for a table with existing production data — so this conversion is required, not optional.
// AccountRetention, LabelExemption, LabelRetention, and EmailCorrection stay hand-rolled:
// their AccountID is derived from the partition key, not a stored attribute, so they don't
// fit the same tag-per-field mapping.

type Account struct {
	ID              int64   `dynamodbav:"id"`
	Email           string  `dynamodbav:"email"`
	CredentialsJSON string  `dynamodbav:"creds"`
	AddedAt         string  `dynamodbav:"addedAt"`
	LastScanAt      *string `dynamodbav:"lastScan"`
	Active          int64   `dynamodbav:"active"`
	// Gmail push (users.watch) state. WatchExpiration is epoch millis; the watch is
	// renewed on the scheduled scan before it lapses (Gmail expires it after ~7 days).
	// omitempty: historically left out of the item entirely (not written as NULL) when
	// unset — getStr/getInt64 already treat "missing" and "zero" identically on read.
	WatchHistoryID  string `dynamodbav:"watchHist,omitempty"`
	WatchExpiration int64  `dynamodbav:"watchExp,omitempty"`
}

type AccountRetention struct {
	AccountID  int64
	GlobalDays sql.NullInt64
}

type CategorizationHistory struct {
	ID           int64   `dynamodbav:"id"`
	Timestamp    string  `dynamodbav:"ts"`
	AccountID    int64   `dynamodbav:"accountId"`
	AccountEmail string  `dynamodbav:"accountEmail"`
	MessageID    string  `dynamodbav:"messageId"`
	Subject      string  `dynamodbav:"subject"`
	Sender       string  `dynamodbav:"sender"`
	PromptID     *int64  `dynamodbav:"promptId"`
	PromptName   *string `dynamodbav:"promptName"`
	LabelName    *string `dynamodbav:"labelName"`
	Actions      string  `dynamodbav:"actions"`
	LlmResponse  string  `dynamodbav:"llmResponse"`
	DurationMs   int64   `dynamodbav:"durationMs"`
}

type EmailCorrection struct {
	ID               int64
	CreatedAt        string
	AccountID        int64
	MessageID        string
	AddedPrompts     string
	RemovedPrompts   string
	CurrentPromptIds string
	Note             string
}

type LabelExemption struct {
	ID        int64
	AccountID int64
	LabelName string
}

type LabelRetention struct {
	ID        int64
	AccountID int64
	LabelName string
	Days      int64
}

type LlmDebug struct {
	ID           int64  `dynamodbav:"id"`
	Timestamp    string `dynamodbav:"ts"`
	AccountID    int64  `dynamodbav:"accountId"`
	AccountEmail string `dynamodbav:"accountEmail"`
	MessageID    string `dynamodbav:"messageId"`
	Subject      string `dynamodbav:"subject"`
	Sender       string `dynamodbav:"sender"`
	GmailRaw     string `dynamodbav:"gmailRaw"`
	LlmRequest   string `dynamodbav:"llmRequest"`
	LlmResponse  string `dynamodbav:"llmResponse"`
}

type Log struct {
	ID        int64  `dynamodbav:"id"`
	Timestamp string `dynamodbav:"ts"`
	Level     string `dynamodbav:"level"`
	Message   string `dynamodbav:"msg"`
}

type Prompt struct {
	ID             int64  `dynamodbav:"id"`
	Name           string `dynamodbav:"name"`
	Instructions   string `dynamodbav:"instructions"`
	LabelName      string `dynamodbav:"labelName"`
	Active         int64  `dynamodbav:"active"`
	CreatedAt      string `dynamodbav:"createdAt"`
	ActionArchive  int64  `dynamodbav:"actionArchive"`
	ActionSpam     int64  `dynamodbav:"actionSpam"`
	ActionTrash    int64  `dynamodbav:"actionTrash"`
	ActionMarkRead int64  `dynamodbav:"actionMarkRead"`
	SortOrder      int64  `dynamodbav:"sortOrder"`
	StopProcessing int64  `dynamodbav:"stopProcessing"`
	AccountID      *int64 `dynamodbav:"accountId"`
}

type PromptSuggestion struct {
	ID                    int64  `dynamodbav:"id"`
	CreatedAt             string `dynamodbav:"createdAt"`
	UpdatedAt             string `dynamodbav:"updatedAt"`
	PromptID              int64  `dynamodbav:"promptId"`
	CorrectionID          *int64 `dynamodbav:"correctionId"`
	TriggerKind           string `dynamodbav:"triggerKind"`
	MessageID             string `dynamodbav:"messageId"`
	EmailSubject          string `dynamodbav:"emailSubject"`
	EmailSender           string `dynamodbav:"emailSender"`
	EmailBodySnapshot     string `dynamodbav:"emailBodySnapshot"`
	OriginalInstructions  string `dynamodbav:"originalInstructions"`
	SuggestedInstructions string `dynamodbav:"suggestedInstructions"`
	ConversationJSON      string `dynamodbav:"conversationJson"`
	UserComment           string `dynamodbav:"userComment"`
	Status                string `dynamodbav:"status"`
}

type Setting struct {
	Key   string
	Value string
}
