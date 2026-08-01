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
// AccountRetention, LabelExemption, and LabelRetention stay hand-rolled: their AccountID is
// derived from the partition key, not a stored attribute, so they don't fit the same
// tag-per-field mapping.

type Account struct {
	ID    int64  `dynamodbav:"id"`
	Email string `dynamodbav:"email"`
	// CredentialsJSON is the Gmail OAuth token, held in memory only: it lives in an SSM
	// SecureString (db/store.go tokenParamName), never in the table, so the "-" tag
	// excludes it from item (un)marshaling entirely.
	CredentialsJSON string  `dynamodbav:"-"`
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

// ExampleExcerptRunes bounds a PromptExample's stored body excerpt (via
// gmail.CollapseExcerpt). Small on purpose: selectExamplesForPrompt (recategorize.go) feeds
// up to several dozen examples into one improve call, so each excerpt has to stay compact
// enough that the whole set fits comfortably in a small model's context — full email bodies
// would blow that budget for even a modest corpus.
const ExampleExcerptRunes = 400

// PromptExample is a single labeled example: one rule's verdict on one email, kept
// permanently so prompt improvement can be grounded in real history. Written from two
// sources — a manual recategorization (any verdict), and passive confirmation on ordinary
// classification (confirmed_positive only: every email a rule matches and the user never
// corrects becomes evidence the rule is right about it; see processor.processEmail). Stored
// under PK = EXAMPLE#<promptId>, SK = <verdict>#<ts>#<padID(id)> — the verdict prefix lets
// ListExamplesByVerdict fetch a bounded, balanced sample (e.g. the newest N false positives)
// without reading the rest of the partition, so cost stays flat as the corpus grows. IDs
// from both write paths come from the same monotonically-ordered source (localID/localIDs,
// not the atomic nextIDs counter — see db/store.go), which is what lets
// selectExamplesForPrompt's "newest verdict wins" dedup correctly drop a passively-confirmed
// row once a later manual correction supersedes it. No TTL: see the "Growth and retention"
// note in db/store.go's prompt-examples section for why unbounded retention stays cheap even
// with passive confirmation's much higher write volume than manual correction alone.
type PromptExample struct {
	ID          int64  `dynamodbav:"id"`
	CreatedAt   string `dynamodbav:"createdAt"`
	PromptID    int64  `dynamodbav:"promptId"`
	AccountID   int64  `dynamodbav:"accountId"`
	MessageID   string `dynamodbav:"messageId"`
	Verdict     string `dynamodbav:"verdict"`
	Sender      string `dynamodbav:"sender"`
	Subject     string `dynamodbav:"subject"`
	BodyExcerpt string `dynamodbav:"bodyExcerpt"`
	Note        string `dynamodbav:"note"`

	// ResolvedBySuggestionID is nil for a still-live example. Set to a PromptSuggestion's
	// ID once that suggestion — built from this example, among others — is applied: the
	// rule text has now actually incorporated whatever this example was evidence of, so
	// selectExamplesForPrompt (recategorize.go) excludes it from future improve rounds.
	// Only ever set on false_negative/false_positive rows; confirmed_positive examples
	// aren't "problems" to resolve. If the fix didn't actually work, a later correction on
	// the same email writes a fresh, unresolved row (examples are append-only) that the
	// existing newest-wins dedup naturally surfaces — nothing here needs to be undone by
	// hand. No omitempty: nil marshals as an explicit DynamoDB NULL, matching this
	// codebase's established nullable-pointer convention (see the package doc comment
	// above and CategorizationHistory.PromptID/PromptSuggestion.CorrectionID).
	ResolvedBySuggestionID *int64 `dynamodbav:"resolvedBySuggestionId"`
}

// ResolvedExampleKey is enough to reconstruct one PromptExample's DynamoDB key
// (PK = EXAMPLE#<PromptID>, SK = <Verdict>#<CreatedAt>#<padID(ID)> via pkExample/exampleSK)
// without a lookup. Stored (JSON-encoded) on PromptSuggestion.ProblemExampleKeys so
// applying a suggestion knows exactly which examples to mark resolved.
type ResolvedExampleKey struct {
	PromptID  int64  `json:"promptId"`
	Verdict   string `json:"verdict"`
	CreatedAt string `json:"createdAt"`
	ID        int64  `json:"id"`
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

	// Replay validation: the candidate SuggestedInstructions re-run through the
	// classify model (never the improve model — see llm.ReplayAgainstExamples) against
	// the example corpus used to generate the suggestion. ReplayTotal == 0 means replay
	// hasn't run (older items, or improve_replay disabled) — the UI renders the block
	// only when ReplayTotal > 0. ReplayBaseline is derived for free from the stored
	// verdicts at correction time (false_negative/false_positive = miss, confirmed_positive
	// = hit), not by re-running the original rule.
	ReplayModel    string `dynamodbav:"replayModel,omitempty"`
	ReplayTotal    int64  `dynamodbav:"replayTotal,omitempty"`
	ReplayPassed   int64  `dynamodbav:"replayPassed,omitempty"`
	ReplayBaseline int64  `dynamodbav:"replayBaseline,omitempty"`
	ReplayFailures string `dynamodbav:"replayFailures,omitempty"` // JSON []ReplayFailure

	// ProblemExampleKeys is a JSON-encoded []ResolvedExampleKey identifying the
	// false_negative/false_positive PromptExample rows this suggestion (in its current,
	// possibly-regenerated form) was built from. Recorded on every generate/regenerate
	// round (improveAndFinalizeSuggestion, recategorize.go) and consumed once, when this
	// suggestion is applied (ApplyPromptSuggestionAndUpdatePrompt calls MarkExamplesResolved
	// with the parsed keys) — never on dismiss, since nothing about the rule changed then.
	ProblemExampleKeys string `dynamodbav:"problemExampleKeys,omitempty"`
}

type Setting struct {
	Key   string
	Value string
}
