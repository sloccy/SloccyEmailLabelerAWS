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

	// CurrentVersionID is the PromptVersion.ID of this rule's live instructions text — 0
	// for a prompt that predates the version ledger and hasn't been edited since. Kept
	// denormalized here, rather than requiring a lookup into the PVER# partition, so
	// stamping a PromptExample with "which rule text produced this verdict" (see
	// PromptExample.PromptVersionID) costs zero extra reads on the hot classify path
	// (processor.processEmail already loads this Prompt row for every match).
	CurrentVersionID int64 `dynamodbav:"currentVersionId,omitempty"`
}

// PromptVersion is one historical text of a rule, plus what that text actually got wrong
// while it was live — the improve loop's long-term memory (see improve.go's package doc
// comment). Without this, every improve round starts from a blank slate and can re-propose
// a phrasing that was already tried on this exact rule and already failed. Stored under
// PK = PVER#<promptId>, SK = padID(id), no TTL — permanent, like PromptExample, since a
// rule's edit history is exactly the kind of evidence that should never silently expire.
// ID comes from the same atomic nextID counter every other low-write-volume entity in this
// package uses (see InsertPromptVersion) — prompt edits happen a handful of times a week at
// most, nowhere near the write volume localIDs exists to spare (PromptExample, history,
// llm_debug).
type PromptVersion struct {
	ID           int64  `dynamodbav:"id"`
	PromptID     int64  `dynamodbav:"promptId"`
	CreatedAt    string `dynamodbav:"createdAt"`
	Instructions string `dynamodbav:"instructions"`
	// Source is one of the PromptVersionSource* constants (db/consts.go): "initial" (rule
	// created), "suggestion" (an AI suggestion was applied), or "manual" (the user edited
	// the rule text directly). Manual edits matter here as much as suggestions do — without
	// them, a user's own fix would be invisible to a later improve round, which could then
	// happily re-propose whatever the user just moved away from.
	Source string `dynamodbav:"source"`
	// SuggestionID is set only when Source is "suggestion" — the PromptSuggestion this
	// version came from, for cross-referencing.
	SuggestionID *int64 `dynamodbav:"suggestionId"`

	// Replay evidence captured at the moment this version went live — copied from the
	// winning PromptSuggestion round when Source is "suggestion"; zero-value (and
	// meaningless — see ReplayTotal == 0 convention used elsewhere in this package) for
	// "manual" and "initial", which have no replay score to carry.
	ReplayModel  string `dynamodbav:"replayModel,omitempty"`
	ReplayTotal  int64  `dynamodbav:"replayTotal,omitempty"`
	ReplayPassed int64  `dynamodbav:"replayPassed,omitempty"`

	// ObservedFP/ObservedFN accrue *after* this version went live, via
	// IncrementVersionObserved — each false_positive/false_negative correction recorded
	// while this was the current version adds one. This is what lets a later improve round
	// see not just "how this version scored in the lab" (ReplayPassed/ReplayTotal) but "how
	// it actually did in production."
	ObservedFP int64 `dynamodbav:"observedFp,omitempty"`
	ObservedFN int64 `dynamodbav:"observedFn,omitempty"`
}

// ExampleExcerptRunes bounds a PromptExample's stored body excerpt (via
// gmail.CollapseExcerpt). Small on purpose: selectExamplesForImprove (improve.go) feeds
// up to several dozen examples into one improve call, so each excerpt has to stay compact
// enough that the whole set fits comfortably in a small model's context — full email bodies
// would blow that budget for even a modest corpus.
const ExampleExcerptRunes = 400

// PromptExample is a single labeled example: one rule's verdict on one email, kept
// permanently so prompt improvement can be grounded in real history. There is exactly one
// write source now — a human reviewing an email on the history page, either recategorizing
// it or explicitly confirming its current labeling is correct (see recategorize.go's
// handleRecategorize/handleConfirmCategorization and recategorize_bulk.go's bulk
// counterparts). Nothing is written automatically: earlier versions of this feature also
// auto-recorded a confirmed_positive on every ordinary classification match nobody had
// corrected yet ("passive confirmation"), which flooded the corpus faster than any human
// could review it and made it impossible to curate. That path is gone; every row here now
// reflects an explicit human judgment. Stored under PK = EXAMPLE#<promptId>,
// SK = <verdict>#<ts>#<padID(id)> — the verdict prefix lets ListExamplesByVerdict fetch a
// bounded, balanced sample without reading the rest of the partition, so cost stays flat as
// the corpus grows. IDs come from a monotonically-ordered source (localID/localIDs, not the
// atomic nextIDs counter — see db/store.go), which is what lets gatherRawExamples'
// (improve.go) "newest verdict wins" dedup correctly drop a superseded row once a later
// correction lands on the same message. No TTL: see the "Growth and retention" note in
// db/store.go's prompt-examples section for why unbounded retention stays cheap — write
// volume is bounded by human review pace, not by scan volume.
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
	// selectExamplesForImprove (improve.go) excludes it from future improve rounds.
	// Only ever set on Missed rows; a plain affirmation isn't a "problem" to resolve. If the
	// fix didn't actually work, a later correction on the same email writes a fresh,
	// unresolved row (examples are append-only) that the existing newest-wins dedup
	// naturally surfaces — nothing here needs to be undone by hand. No omitempty: nil
	// marshals as an explicit DynamoDB NULL, matching this codebase's established
	// nullable-pointer convention (see the package doc comment above and
	// CategorizationHistory.PromptID/PromptSuggestion.CorrectionID).
	ResolvedBySuggestionID *int64 `dynamodbav:"resolvedBySuggestionId"`

	// PromptVersionID is the Prompt.CurrentVersionID that was live when this example was
	// recorded — i.e. which rule text actually produced this verdict. omitempty (unlike
	// ResolvedBySuggestionID above) rather than an explicit NULL: this field was added
	// after ResolvedBySuggestionID's NULL convention was already established, and every
	// existing example row predates it, so writing NULL retroactively would be
	// indistinguishable from "explicitly no version" versus "not tracked yet" — omitting it
	// keeps both cases reading as the same zero value.
	PromptVersionID int64 `dynamodbav:"promptVersionId,omitempty"`

	// Missed is true when this example came from the user actually changing this rule's
	// checkbox during a review — i.e. the rule got this email wrong and the user corrected
	// it. False when the user reviewed the email and left the rule's box exactly as it
	// was: still a real, explicit confirmation, just not evidence of a defect. This is what
	// replaces the old false_negative/false_positive verdicts: Verdict says which side of
	// the rule the email belongs on, Missed says whether the rule already agreed before the
	// review. sampleExamples (improve.go) prioritizes Missed examples when curating which
	// examples the improver actually sees — a case the rule got wrong is stronger evidence
	// than one it already had right. omitempty so a row with nothing to say here (false)
	// marshals the same as a pre-Missed row would have read back, matching this struct's
	// other omitempty fields' reasoning.
	Missed bool `dynamodbav:"missed,omitempty"`

	// Recurred and RecurredFromVersion are computed at read time by markRecurrences
	// (improve.go), never persisted (dynamodbav:"-"): true when an older, already-resolved
	// example exists for the same message and verdict — i.e. a previous suggestion claimed
	// to have fixed exactly this and the rule regressed. This is the signal that lets the
	// improver see "already tried and failed" instead of treating a recurrence as a
	// brand-new problem (see gatherRawExamples' doc comment in improve.go).
	Recurred            bool  `dynamodbav:"-"`
	RecurredFromVersion int64 `dynamodbav:"-"`
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

	// RoundsJSON is a JSON-encoded []SuggestionRoundSummary — one entry per improve->replay
	// round the improve loop ran (improve.go), in order, so the detail view can show the
	// trajectory and why the applied candidate (whichever round scored best, not
	// necessarily the last) won. Kept deliberately lean — no per-round failure list, just
	// the candidate text and its score — since it's carried on every FinalizePromptSuggestion
	// write alongside EmailBodySnapshot, and RoundsRun/BestRound duplicate len(Rounds) and
	// the winning index as plain attributes so the list/badge views don't need to parse
	// this JSON just to show a count. Empty ("") for a suggestion generated before this
	// field existed, or one that failed before completing a round.
	RoundsJSON string `dynamodbav:"roundsJson,omitempty"`
	RoundsRun  int64  `dynamodbav:"roundsRun,omitempty"`
	BestRound  int64  `dynamodbav:"bestRound,omitempty"`
}

// SuggestionRoundSummary is one entry in PromptSuggestion.RoundsJSON: what one
// improve->replay round produced and how it scored. Candidate is the full sanitized rule
// text (not truncated) — a suggestion already stores one full rule text
// (SuggestedInstructions) at the top level, so a handful more of the same size across a
// bounded round count (ImproveMaxRoundsCap, llm/bedrock.go) isn't a meaningfully different
// order of magnitude for one DynamoDB item.
type SuggestionRoundSummary struct {
	N         int    `json:"n"`
	Candidate string `json:"candidate"`
	Passed    int64  `json:"passed"`
	Total     int64  `json:"total"`
}

type Setting struct {
	Key   string
	Value string
}

// SuggestionTraceEvent is one entry in a suggestion's live progress log — what the
// improve worker is doing right now, for the "generating…" UI to poll and render instead
// of showing a spinner with no information behind it. Append-only, written only by the
// worker holding the suggestion's claim (see Store.ClaimPromptSuggestion), so Seq is
// assigned from an in-process counter rather than the atomic nextIDs counter — there is
// never a second writer to race with. Stored under PK = SUGG_TRACE#<suggestionId>,
// SK = padID(seq), with a short TTL (traceTTLDays) — this is a debugging/watch artifact
// whose value decays fast, not a permanent record like PromptExample or PromptSuggestion
// itself.
// json tags (alongside the dynamodbav tags every other model in this file uses) exist
// because this struct, uniquely among them, is also served directly as JSON by the trace
// endpoint (server.go) for the browser's polling loop to consume — the dynamodbav tag
// only governs the DynamoDB wire format via attributevalue.MarshalMap/UnmarshalMap, so
// json tags are additive, not a duplicate source of truth for the same encoding.
type SuggestionTraceEvent struct {
	Seq       int64  `dynamodbav:"seq" json:"seq"`
	CreatedAt string `dynamodbav:"createdAt" json:"createdAt"`
	// Kind is one of the db.TraceKind* constants.
	Kind string `dynamodbav:"kind" json:"kind"`
	// Round is the 1-based improve→replay round this event belongs to, 0 for an event
	// that isn't scoped to a round (e.g. a top-level error before any round started).
	Round int64  `dynamodbav:"round,omitempty" json:"round,omitempty"`
	Text  string `dynamodbav:"text,omitempty" json:"text,omitempty"`
}
