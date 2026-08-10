package db

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// This file is the safety net for the hand-rolled-marshaling → attributevalue migration:
// it locks in the exact DynamoDB wire format (attribute names, NULL-vs-omit behavior) that
// the live `ollamail` table already has 1274 items of, so a future refactor can't silently
// drift the format. Two kinds of coverage:
//   - Round-trip: build a struct, marshal it, unmarshal it back, assert equality — plus
//     explicit assertions on which attributes are present/omitted/NULL for edge cases.
//   - Fixture decode: literal item maps shaped exactly like what `aws dynamodb query`
//     returned against the live table (attribute names/types verified by hand against the
//     real table this session; values are synthetic, not the real account's data) decoded
//     via the itemToX functions, asserting the exact expected struct.

func attrS(v string) types.AttributeValue { return &types.AttributeValueMemberS{Value: v} }
func attrN(v string) types.AttributeValue { return &types.AttributeValueMemberN{Value: v} }
func attrNull() types.AttributeValue      { return &types.AttributeValueMemberNULL{Value: true} }

// ============================================================
// Account
// ============================================================

func TestAccountRoundTrip_Populated(t *testing.T) {
	scan := "2026-07-01 12:00:00"
	a := Account{
		ID:              1,
		Email:           "user@example.com",
		CredentialsJSON: `{"access_token":"placeholder"}`,
		AddedAt:         "2026-06-01 00:00:00",
		LastScanAt:      &scan,
		Active:          1,
		WatchHistoryID:  "12345",
		WatchExpiration: 1783607644719,
	}
	item := accountItem(a)

	if got, ok := item["PK"].(*types.AttributeValueMemberS); !ok || got.Value != "ACCOUNT" {
		t.Errorf("PK = %v", item["PK"])
	}
	if got, ok := item["SK"].(*types.AttributeValueMemberS); !ok || got.Value != "00000000000000000001" {
		t.Errorf("SK = %v", item["SK"])
	}

	// OAuth tokens live in SSM (see hydrateAccountToken), never in the table: accountItem
	// must strip CredentialsJSON no matter what the in-memory Account carries.
	if creds, ok := item["creds"].(*types.AttributeValueMemberS); ok && creds.Value != "" {
		t.Errorf("creds persisted to item: %q; accountItem must strip tokens", creds.Value)
	}

	got := itemToAccount(item)
	if got.CredentialsJSON != "" {
		t.Errorf("CredentialsJSON = %q, want empty (tokens are hydrated from SSM, not the item)", got.CredentialsJSON)
	}
	if got.ID != a.ID || got.Email != a.Email ||
		got.AddedAt != a.AddedAt || got.Active != a.Active ||
		got.WatchHistoryID != a.WatchHistoryID || got.WatchExpiration != a.WatchExpiration {
		t.Errorf("round trip mismatch: got %+v, want %+v", got, a)
	}
	if got.LastScanAt == nil || *got.LastScanAt != scan {
		t.Errorf("LastScanAt = %v, want %q", got.LastScanAt, scan)
	}
}

func TestAccountRoundTrip_ZeroValue(t *testing.T) {
	a := Account{ID: 2, Email: "new@example.com", CredentialsJSON: "{}", AddedAt: "2026-07-01 00:00:00", Active: 1}
	item := accountItem(a)

	// LastScanAt unset: must be an explicit NULL attribute, not omitted — matching the old
	// accountItem's `else { item["lastScan"] = NULL }` branch.
	if _, ok := item["lastScan"]; !ok {
		t.Error("lastScan attribute missing; want explicit NULL")
	}
	if _, isNull := item["lastScan"].(*types.AttributeValueMemberNULL); !isNull {
		t.Errorf("lastScan = %T, want NULL", item["lastScan"])
	}
	// WatchHistoryID/WatchExpiration unset: must be omitted entirely, not written as
	// NULL/zero — matching the old conditional `if a.WatchHistoryID != "" { ... }` branch.
	if _, ok := item["watchHist"]; ok {
		t.Error("watchHist should be omitted when empty, but is present")
	}
	if _, ok := item["watchExp"]; ok {
		t.Error("watchExp should be omitted when zero, but is present")
	}

	got := itemToAccount(item)
	if got.LastScanAt != nil {
		t.Errorf("LastScanAt = %v, want nil", got.LastScanAt)
	}
	if got.WatchHistoryID != "" || got.WatchExpiration != 0 {
		t.Errorf("WatchHistoryID/WatchExpiration = %q/%d, want zero values", got.WatchHistoryID, got.WatchExpiration)
	}
}

// TestAccountDecode_LiveShape decodes a fixture shaped exactly like the live ACCOUNT item
// captured via `aws dynamodb query` against the deployed table this session (field names
// and types verified by hand; values are synthetic).
func TestAccountDecode_LiveShape(t *testing.T) {
	item := map[string]types.AttributeValue{
		"PK":        attrS("ACCOUNT"),
		"SK":        attrS("00000000000000000001"),
		"active":    attrN("1"),
		"addedAt":   attrS("2026-07-01 12:30:59"),
		"creds":     attrS(`{"access_token":"placeholder","token_type":"Bearer","refresh_token":"placeholder","expiry":"2026-07-03T02:37:50Z","expires_in":3599}`),
		"email":     attrS("slocum.brendan@gmail.com"),
		"id":        attrN("1"),
		"lastScan":  attrS("2026-07-03 01:37:55"),
		"watchExp":  attrN("1783607644719"),
		"watchHist": attrS("5141606"),
	}
	got := itemToAccount(item)
	want := Account{
		ID:              1,
		Email:           "slocum.brendan@gmail.com",
		AddedAt:         "2026-07-01 12:30:59",
		Active:          1,
		WatchHistoryID:  "5141606",
		WatchExpiration: 1783607644719,
	}
	if got.ID != want.ID || got.Email != want.Email || got.AddedAt != want.AddedAt ||
		got.Active != want.Active || got.WatchHistoryID != want.WatchHistoryID || got.WatchExpiration != want.WatchExpiration {
		t.Errorf("decoded = %+v, want %+v", got, want)
	}
	if got.LastScanAt == nil || *got.LastScanAt != "2026-07-03 01:37:55" {
		t.Errorf("LastScanAt = %v", got.LastScanAt)
	}
}

// ============================================================
// Prompt
// ============================================================

func TestPromptRoundTrip_WithAccount(t *testing.T) {
	accID := int64(5)
	p := Prompt{
		ID: 10, Name: "Security Alerts", Instructions: "matches security emails",
		LabelName: "Security Alerts", Active: 1, CreatedAt: "2026-07-01 14:38:35",
		ActionArchive: 0, ActionSpam: 0, ActionTrash: 0, ActionMarkRead: 0,
		SortOrder: 0, StopProcessing: 1, AccountID: &accID,
	}
	item := promptToItem(p)
	got := itemToPrompt(item)
	if got.AccountID == nil || *got.AccountID != accID {
		t.Errorf("AccountID = %v, want %d", got.AccountID, accID)
	}
	got.AccountID = nil
	p.AccountID = nil
	if got != p {
		t.Errorf("round trip mismatch: got %+v, want %+v", got, p)
	}
}

func TestPromptRoundTrip_GlobalNoAccount(t *testing.T) {
	p := Prompt{ID: 1, Name: "Global Rule", Instructions: "x", Active: 1, CreatedAt: "2026-07-01 00:00:00"}
	item := promptToItem(p)

	if _, ok := item["accountId"]; !ok {
		t.Error("accountId attribute missing; want explicit NULL for a global prompt")
	}
	if _, isNull := item["accountId"].(*types.AttributeValueMemberNULL); !isNull {
		t.Errorf("accountId = %T, want NULL", item["accountId"])
	}

	got := itemToPrompt(item)
	if got.AccountID != nil {
		t.Errorf("AccountID = %v, want nil", got.AccountID)
	}
}

func TestPromptDecode_LiveShape(t *testing.T) {
	item := map[string]types.AttributeValue{
		"PK": attrS("PROMPT"), "SK": attrS("00000000000000000001"),
		"instructions":   attrS("Matches automated notifications informing the user of account access events."),
		"accountId":      attrNull(),
		"actionSpam":     attrN("0"),
		"createdAt":      attrS("2026-07-01 14:38:35"),
		"name":           attrS("Security Alerts"),
		"actionArchive":  attrN("0"),
		"active":         attrN("1"),
		"labelName":      attrS("Security Alerts"),
		"stopProcessing": attrN("1"),
		"actionTrash":    attrN("0"),
		"actionMarkRead": attrN("0"),
		"id":             attrN("1"),
		"sortOrder":      attrN("0"),
	}
	got := itemToPrompt(item)
	if got.AccountID != nil {
		t.Errorf("AccountID = %v, want nil (global prompt)", got.AccountID)
	}
	if got.ID != 1 || got.Name != "Security Alerts" || got.LabelName != "Security Alerts" ||
		got.Active != 1 || got.StopProcessing != 1 {
		t.Errorf("decoded = %+v", got)
	}
}

// ============================================================
// CategorizationHistory / HistoryEntry
// ============================================================

func TestHistoryRoundTrip_Matched(t *testing.T) {
	arg := HistoryEntry{
		AccountID: 1, AccountEmail: "user@example.com", MessageID: "msg1",
		Subject: "Sale", Sender: "shop@example.com",
		PromptID: ptr(int64(101)), PromptName: ptr("Newsletter"), LabelName: ptr("Newsletters"),
		Actions: "labeled → Newsletters", LlmResponse: `{"1":true}`, DurationMs: 842,
	}
	item := historyItem(1, "2026-07-01 12:00:00", arg)

	if _, ok := item["ttl"]; !ok {
		t.Error("ttl missing from history item")
	}

	got := itemToHistory(item)
	if got.ID != 1 || got.Timestamp != "2026-07-01 12:00:00" || got.AccountID != arg.AccountID ||
		got.MessageID != arg.MessageID || got.Subject != arg.Subject || got.Sender != arg.Sender ||
		got.Actions != arg.Actions || got.LlmResponse != arg.LlmResponse || got.DurationMs != arg.DurationMs {
		t.Errorf("round trip mismatch: got %+v", got)
	}
	if got.PromptID == nil || *got.PromptID != 101 {
		t.Errorf("PromptID = %v, want 101", got.PromptID)
	}
	if got.PromptName == nil || *got.PromptName != "Newsletter" {
		t.Errorf("PromptName = %v, want Newsletter", got.PromptName)
	}
	if got.LabelName == nil || *got.LabelName != "Newsletters" {
		t.Errorf("LabelName = %v, want Newsletters", got.LabelName)
	}
}

func TestHistoryRoundTrip_NoMatch(t *testing.T) {
	// "No match" entries (processor.go) leave PromptID/PromptName/LabelName unset.
	arg := HistoryEntry{
		AccountID: 1, AccountEmail: "user@example.com", MessageID: "msg2",
		Subject: "Newsletter", Sender: "news@example.com",
		Actions: "no match", LlmResponse: `{"1":false}`,
	}
	item := historyItem(2, "2026-07-01 12:01:00", arg)

	for _, attr := range []string{"promptId", "promptName", "labelName"} {
		if _, ok := item[attr]; !ok {
			t.Errorf("%s attribute missing; want explicit NULL", attr)
		}
		if _, isNull := item[attr].(*types.AttributeValueMemberNULL); !isNull {
			t.Errorf("%s = %T, want NULL", attr, item[attr])
		}
	}

	got := itemToHistory(item)
	if got.PromptID != nil || got.PromptName != nil || got.LabelName != nil {
		t.Errorf("expected all three nullable fields nil, got PromptID=%v PromptName=%v LabelName=%v",
			got.PromptID, got.PromptName, got.LabelName)
	}
}

func TestHistoryDecode_LiveShape(t *testing.T) {
	item := map[string]types.AttributeValue{
		"PK": attrS("HIST#1"), "SK": attrS("2026-07-01 14:49:20#00000000000000000001"),
		"subject":      attrS("[sloccy/SloccyEmailLabelerAWS] Run failed"),
		"ts":           attrS("2026-07-01 14:49:20"),
		"accountId":    attrN("1"),
		"ttl":          attrN("1790693360"),
		"promptId":     attrNull(),
		"accountEmail": attrS("slocum.brendan@gmail.com"),
		"llmResponse":  attrS(`{"1": false, "2": false, "3": false, "4": false}`),
		"sender":       attrS("sloccy <notifications@github.com>"),
		"labelName":    attrNull(),
		"messageId":    attrS("19f1e173e4265130"),
		"id":           attrN("1"),
		"actions":      attrS("no match"),
		"promptName":   attrNull(),
	}
	got := itemToHistory(item)
	if got.PromptID != nil || got.PromptName != nil || got.LabelName != nil {
		t.Errorf("expected nullable fields nil for a live no-match row, got PromptID=%v PromptName=%v LabelName=%v",
			got.PromptID, got.PromptName, got.LabelName)
	}
	if got.ID != 1 || got.AccountID != 1 || got.MessageID != "19f1e173e4265130" || got.Actions != "no match" {
		t.Errorf("decoded = %+v", got)
	}
}

// ============================================================
// Log
// ============================================================

func TestLogRoundTrip(t *testing.T) {
	arg := LogEntry{Level: "INFO", Message: "Manual scan triggered"}
	item := logItem(1, "2026-07-01 12:31:07", arg)
	got := itemToLog(item)
	want := Log{ID: 1, Timestamp: "2026-07-01 12:31:07", Level: "INFO", Message: "Manual scan triggered"}
	if got != want {
		t.Errorf("round trip mismatch: got %+v, want %+v", got, want)
	}
}

func TestLogDecode_LiveShape(t *testing.T) {
	item := map[string]types.AttributeValue{
		"PK": attrS("LOG"), "SK": attrS("2026-07-01 12:31:07#00000000000000000001"),
		"ts": attrS("2026-07-01 12:31:07"), "msg": attrS("Manual scan triggered"),
		"level": attrS("INFO"), "ttl": attrN("1790685067"), "id": attrN("1"),
	}
	got := itemToLog(item)
	want := Log{ID: 1, Timestamp: "2026-07-01 12:31:07", Level: "INFO", Message: "Manual scan triggered"}
	if got != want {
		t.Errorf("decoded = %+v, want %+v", got, want)
	}
}

// ============================================================
// LlmDebug
// ============================================================

func TestLlmDebugRoundTrip(t *testing.T) {
	arg := AddLlmDebugParams{
		AccountID: 1, AccountEmail: "user@example.com", MessageID: "msg1",
		Subject: "Large Purchase Approved", Sender: "American Express <notify@amex.com>",
		GmailRaw: `{"id":"msg1"}`, LlmRequest: `{"modelId":"x"}`, LlmResponse: `{"1":false}`,
	}
	item := llmDebugItem(46, "2026-07-03 00:27:13", arg)
	got := itemToLlmDebug(item)
	want := LlmDebug{
		ID: 46, Timestamp: "2026-07-03 00:27:13", AccountID: arg.AccountID, AccountEmail: arg.AccountEmail,
		MessageID: arg.MessageID, Subject: arg.Subject, Sender: arg.Sender,
		GmailRaw: arg.GmailRaw, LlmRequest: arg.LlmRequest, LlmResponse: arg.LlmResponse,
	}
	if got != want {
		t.Errorf("round trip mismatch: got %+v, want %+v", got, want)
	}
}

// ============================================================
// PromptSuggestion
// ============================================================

func TestSuggestionRoundTrip_WithCorrection(t *testing.T) {
	arg := InsertPromptSuggestionParams{
		PromptID: 5, CorrectionID: sql.NullInt64{Int64: 42, Valid: true}, TriggerKind: "false_positive",
		MessageID: "msg1", EmailSubject: "Sale", EmailSender: "shop@example.com",
		EmailBodySnapshot: "body", OriginalInstructions: "orig", SuggestedInstructions: "",
		ConversationJSON: "[]", Status: "generating",
	}
	item := suggestionItem(1, "2026-07-01 12:00:00", arg)
	got := itemToSuggestion(item)
	if got.CorrectionID == nil || *got.CorrectionID != 42 {
		t.Errorf("CorrectionID = %v, want 42", got.CorrectionID)
	}
	if got.ID != 1 || got.PromptID != 5 || got.TriggerKind != "false_positive" || got.Status != "generating" {
		t.Errorf("decoded = %+v", got)
	}
}

func TestSuggestionRoundTrip_NoCorrection(t *testing.T) {
	arg := InsertPromptSuggestionParams{PromptID: 5, TriggerKind: "false_negative", MessageID: "msg2", Status: "generating"}
	item := suggestionItem(2, "2026-07-01 12:00:00", arg)

	if _, ok := item["correctionId"]; !ok {
		t.Error("correctionId attribute missing; want explicit NULL")
	}
	if _, isNull := item["correctionId"].(*types.AttributeValueMemberNULL); !isNull {
		t.Errorf("correctionId = %T, want NULL", item["correctionId"])
	}

	got := itemToSuggestion(item)
	if got.CorrectionID != nil {
		t.Errorf("CorrectionID = %v, want nil", got.CorrectionID)
	}
}

// TestSuggestionRoundTrip_ReplayFields locks in the wire format for the replay-validation
// fields added to PromptSuggestion (see db/models.go): they're plain MarshalMap/UnmarshalMap
// fields, not part of suggestionItem's InsertPromptSuggestionParams construction (replay
// results only exist once FinalizePromptSuggestion writes them, after the improve+replay
// round completes), so this round-trips the struct directly rather than via suggestionItem.
func TestSuggestionRoundTrip_ReplayFields(t *testing.T) {
	want := PromptSuggestion{
		ID: 9, PromptID: 5, Status: "pending",
		ReplayModel: "us.amazon.nova-micro-v1:0", ReplayTotal: 30, ReplayPassed: 27, ReplayBaseline: 24,
		ReplayFailures: `[{"Verdict":"false_positive","Sender":"a@example.com","Subject":"s","Got":true}]`,
	}
	item := mustMarshalMap(want)
	got := unmarshalItem[PromptSuggestion](item)
	if got != want {
		t.Errorf("round trip mismatch: got %+v, want %+v", got, want)
	}
}

// TestSuggestionRoundTrip_ReplayFieldsOmittedWhenZero checks that a suggestion which never
// ran replay (improve_replay disabled, or an older item from before these fields existed)
// round-trips to the zero value rather than some other sentinel — the UI's "render the
// validation block only when ReplayTotal > 0" check (see suggestionDetailView in server.go)
// depends on this.
func TestSuggestionRoundTrip_ReplayFieldsOmittedWhenZero(t *testing.T) {
	want := PromptSuggestion{ID: 9, PromptID: 5, Status: "pending"}
	item := mustMarshalMap(want)
	for _, attr := range []string{"replayModel", "replayTotal", "replayPassed", "replayBaseline", "replayFailures"} {
		if _, ok := item[attr]; ok {
			t.Errorf("item[%q] present; want omitted (omitempty) when zero-value", attr)
		}
	}
	got := unmarshalItem[PromptSuggestion](item)
	if got.ReplayTotal != 0 || got.ReplayPassed != 0 || got.ReplayBaseline != 0 || got.ReplayModel != "" || got.ReplayFailures != "" {
		t.Errorf("decoded replay fields not zero: %+v", got)
	}
}

// ============================================================
// PromptExample
// ============================================================

func TestPromptExampleRoundTrip(t *testing.T) {
	want := PromptExample{
		ID: 3, CreatedAt: "2026-07-01 12:00:00", PromptID: 5, AccountID: 1,
		MessageID: "msg1", Verdict: VerdictFalseNegative,
		Sender: "a@example.com", Subject: "Hello", BodyExcerpt: "excerpt text", Note: "note text",
	}
	item := promptExampleItem(want)

	if got, ok := item["PK"].(*types.AttributeValueMemberS); !ok || got.Value != "EXAMPLE#5" {
		t.Errorf("PK = %v, want EXAMPLE#5", item["PK"])
	}
	wantSK := "false_negative#2026-07-01 12:00:00#00000000000000000003"
	if got, ok := item["SK"].(*types.AttributeValueMemberS); !ok || got.Value != wantSK {
		t.Errorf("SK = %v, want %q", item["SK"], wantSK)
	}

	got := itemToPromptExample(item)
	if got != want {
		t.Errorf("round trip mismatch: got %+v, want %+v", got, want)
	}
}

// TestPromptExampleRoundTrip_SKVerdictPrefix checks the property ListExamplesByVerdict's
// begins_with(SK, verdict+"#") query depends on: every verdict produces a distinctly
// prefixed SK, so a query scoped to one verdict can never accidentally match another's items
// (e.g. "false_negative#..." must not begin_with "false_positive#", and vice versa — a risk
// specifically because those two strings share a long common prefix, "false_"+"n"/"p"...).
func TestPromptExampleRoundTrip_SKVerdictPrefix(t *testing.T) {
	verdicts := []string{VerdictFalseNegative, VerdictFalsePositive, VerdictConfirmedPositive}
	for _, v := range verdicts {
		e := PromptExample{ID: 1, CreatedAt: "2026-07-01 12:00:00", PromptID: 5, Verdict: v}
		item := promptExampleItem(e)
		skAttr, ok := item["SK"].(*types.AttributeValueMemberS)
		if !ok {
			t.Fatalf("verdict %q: SK attribute type = %T, want *types.AttributeValueMemberS", v, item["SK"])
		}
		sk := skAttr.Value
		if !strings.HasPrefix(sk, v+"#") {
			t.Errorf("verdict %q: SK = %q, want prefix %q", v, sk, v+"#")
		}
		for _, other := range verdicts {
			if other == v {
				continue
			}
			if strings.HasPrefix(sk, other+"#") {
				t.Errorf("verdict %q: SK %q unexpectedly also matches prefix %q", v, sk, other+"#")
			}
		}
	}
}

// TestPromptExampleRoundTrip_LocalIDsProduceDistinctOrderedSKs exercises the exact pattern
// InsertPromptExamples and BatchInsertProcessingResults now both use — several examples
// sharing one batch-level CreatedAt timestamp, with per-example IDs coming from localIDs
// (switched from the atomic nextIDs counter so passive confirmation on classify, which can't
// afford a round trip per email, has the same batched-ID-allocation shape as logs/history).
// Guards against a regression where switching id sources broke the SK's role as a stable
// sort/tie-break key within one batch.
func TestPromptExampleRoundTrip_LocalIDsProduceDistinctOrderedSKs(t *testing.T) {
	ids := localIDs(5)
	ts := "2026-07-01 12:00:00"
	seen := make(map[string]bool, len(ids))
	var prevSK string
	for i, id := range ids {
		e := PromptExample{ID: id, CreatedAt: ts, PromptID: 5, Verdict: VerdictConfirmedPositive}
		item := promptExampleItem(e)
		skAttr, ok := item["SK"].(*types.AttributeValueMemberS)
		if !ok {
			t.Fatalf("example %d: SK attribute type = %T, want *types.AttributeValueMemberS", i, item["SK"])
		}
		sk := skAttr.Value
		if seen[sk] {
			t.Fatalf("example %d: SK %q collided with a previous example in the same batch", i, sk)
		}
		seen[sk] = true
		if i > 0 && sk <= prevSK {
			t.Errorf("example %d: SK %q did not sort after previous SK %q (ids from localIDs must stay ordered within a batch)", i, sk, prevSK)
		}
		prevSK = sk

		got := itemToPromptExample(item)
		if got.ID != id {
			t.Errorf("example %d: round-tripped ID = %d, want %d", i, got.ID, id)
		}
	}
}

// TestPromptExampleRoundTrip_ResolvedBySuggestion checks a resolved example (see
// PromptExample.ResolvedBySuggestionID) survives the marshal/unmarshal round trip — the
// property MarkExamplesResolved's UpdateItem and selectExamplesForPrompt's filterResolved
// both depend on.
func TestPromptExampleRoundTrip_ResolvedBySuggestion(t *testing.T) {
	sid := int64(7)
	want := PromptExample{
		ID: 3, CreatedAt: "2026-07-01 12:00:00", PromptID: 5, AccountID: 1,
		MessageID: "msg1", Verdict: VerdictFalsePositive,
		Sender: "a@example.com", Subject: "Hello", BodyExcerpt: "excerpt",
		ResolvedBySuggestionID: &sid,
	}
	item := promptExampleItem(want)
	got := itemToPromptExample(item)
	if got.ResolvedBySuggestionID == nil || *got.ResolvedBySuggestionID != sid {
		t.Fatalf("ResolvedBySuggestionID = %v, want %d", got.ResolvedBySuggestionID, sid)
	}
	got.ResolvedBySuggestionID, want.ResolvedBySuggestionID = nil, nil
	if got != want {
		t.Errorf("round trip mismatch (excluding ResolvedBySuggestionID, checked above): got %+v, want %+v", got, want)
	}
}

// TestPromptExampleRoundTrip_UnresolvedIsExplicitNull checks a still-live example (the
// overwhelming common case) marshals resolvedBySuggestionId as an explicit DynamoDB NULL,
// not an omitted attribute — matching this codebase's established nullable-pointer
// convention (see db/models.go's package doc comment).
func TestPromptExampleRoundTrip_UnresolvedIsExplicitNull(t *testing.T) {
	e := PromptExample{ID: 1, CreatedAt: "2026-07-01 12:00:00", PromptID: 5, Verdict: VerdictFalsePositive}
	item := promptExampleItem(e)
	if _, ok := item["resolvedBySuggestionId"]; !ok {
		t.Error("resolvedBySuggestionId attribute missing; want explicit NULL")
	}
	if _, isNull := item["resolvedBySuggestionId"].(*types.AttributeValueMemberNULL); !isNull {
		t.Errorf("resolvedBySuggestionId = %T, want NULL", item["resolvedBySuggestionId"])
	}
	got := itemToPromptExample(item)
	if got.ResolvedBySuggestionID != nil {
		t.Errorf("ResolvedBySuggestionID = %v, want nil", got.ResolvedBySuggestionID)
	}
}

// TestSuggestionRoundTrip_ProblemExampleKeys checks PromptSuggestion.ProblemExampleKeys —
// the JSON-encoded []ResolvedExampleKey handlePromptSuggestionApply parses to know which
// examples to mark resolved — round-trips through MarshalMap/UnmarshalMap untouched (it's
// an opaque string field as far as DynamoDB is concerned; the JSON structure itself is
// exercised by recategorize_test.go's TestProblemExampleKeys).
func TestSuggestionRoundTrip_ProblemExampleKeys(t *testing.T) {
	want := PromptSuggestion{
		ID: 9, PromptID: 5, Status: "pending",
		ProblemExampleKeys: `[{"promptId":5,"verdict":"false_positive","createdAt":"2026-07-01 12:00:00","id":3}]`,
	}
	item := mustMarshalMap(want)
	got := unmarshalItem[PromptSuggestion](item)
	if got != want {
		t.Errorf("round trip mismatch: got %+v, want %+v", got, want)
	}
}

// TestSuggestionRoundTrip_RoundsJSON checks PromptSuggestion.RoundsJSON/RoundsRun/BestRound
// — the improve loop's (improve.go) trajectory fields — round-trip through
// MarshalMap/UnmarshalMap. RoundsJSON itself is an opaque string as far as DynamoDB is
// concerned (same treatment as ProblemExampleKeys/ReplayFailures above); the JSON shape
// []SuggestionRoundSummary encodes/decodes is exercised separately below.
func TestSuggestionRoundTrip_RoundsJSON(t *testing.T) {
	want := PromptSuggestion{
		ID: 9, PromptID: 5, Status: "pending",
		RoundsJSON: `[{"n":1,"candidate":"Match newsletters.","passed":7,"total":10},{"n":2,"candidate":"Match promotional newsletters.","passed":9,"total":10}]`,
		RoundsRun:  2,
		BestRound:  2,
	}
	item := mustMarshalMap(want)
	got := unmarshalItem[PromptSuggestion](item)
	if got != want {
		t.Errorf("round trip mismatch: got %+v, want %+v", got, want)
	}
}

// TestSuggestionRoundTrip_RoundsFieldsOmittedWhenZero mirrors
// TestSuggestionRoundTrip_ReplayFieldsOmittedWhenZero: a suggestion that never ran the
// improve loop's trajectory tracking (single-round, or generated before these fields
// existed) must omit the attributes entirely rather than write explicit zero values.
func TestSuggestionRoundTrip_RoundsFieldsOmittedWhenZero(t *testing.T) {
	item := mustMarshalMap(PromptSuggestion{ID: 1, PromptID: 5, Status: "pending"})
	for _, attr := range []string{"roundsJson", "roundsRun", "bestRound"} {
		if _, ok := item[attr]; ok {
			t.Errorf("%s attribute present with zero value, want omitted", attr)
		}
	}
}

func TestSuggestionRoundSummaryJSON_RoundTrips(t *testing.T) {
	want := []SuggestionRoundSummary{
		{N: 1, Candidate: "Match newsletters.", Passed: 7, Total: 10},
		{N: 2, Candidate: "Match promotional newsletters.", Passed: 9, Total: 10},
	}
	b, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got []SuggestionRoundSummary
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("round %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
}

// ============================================================
// SuggestionTraceEvent
// ============================================================

// TestSuggestionTraceEventRoundTrip checks the wire format suggestionTraceItem/
// ListSuggestionTrace depend on: PK scoped to the suggestion, SK a zero-padded seq (so
// string sort order matches numeric seq order for the SK > :after cursor query), and every
// field surviving the round trip.
func TestSuggestionTraceEventRoundTrip(t *testing.T) {
	want := SuggestionTraceEvent{
		Seq: 3, CreatedAt: "2026-07-01 12:00:00", Kind: TraceKindAnswer, Round: 1, Text: "Match newsletters",
	}
	item := suggestionTraceItem(42, want)

	if got, ok := item["PK"].(*types.AttributeValueMemberS); !ok || got.Value != "SUGG_TRACE#42" {
		t.Errorf("PK = %v, want SUGG_TRACE#42", item["PK"])
	}
	wantSK := "00000000000000000003"
	if got, ok := item["SK"].(*types.AttributeValueMemberS); !ok || got.Value != wantSK {
		t.Errorf("SK = %v, want %q", item["SK"], wantSK)
	}
	if _, ok := item["ttl"]; !ok {
		t.Error("ttl attribute missing; trace items must expire (traceTTLDays)")
	}

	got := unmarshalItem[SuggestionTraceEvent](item)
	if got != want {
		t.Errorf("round trip mismatch: got %+v, want %+v", got, want)
	}
}

// TestSuggestionTraceEventRoundTrip_SKOrdersBySeq guards the property the cursor query
// (ListSuggestionTrace's "SK > :after") depends on: zero-padded seq strings must sort in
// the same order as the underlying int64 seq, including across the 9->10 digit-count
// boundary where naive string comparison would get it backwards.
func TestSuggestionTraceEventRoundTrip_SKOrdersBySeq(t *testing.T) {
	seqs := []int64{1, 2, 9, 10, 11, 100}
	var prevSK string
	for i, seq := range seqs {
		item := suggestionTraceItem(1, SuggestionTraceEvent{Seq: seq, Kind: TraceKindNote})
		skAttr, ok := item["SK"].(*types.AttributeValueMemberS)
		if !ok {
			t.Fatalf("seq %d: SK attribute type = %T, want *types.AttributeValueMemberS", seq, item["SK"])
		}
		sk := skAttr.Value
		if i > 0 && sk <= prevSK {
			t.Errorf("seq %d: SK %q did not sort after previous SK %q", seq, sk, prevSK)
		}
		prevSK = sk
	}
}

// TestSuggestionTraceEventRoundTrip_OmitsEmptyRoundAndText checks that a structural event
// with no round (e.g. a top-level error before any round started) and no text omits both
// attributes rather than writing zero values — matching this package's established
// omitempty convention for fields that are meaningful only sometimes.
func TestSuggestionTraceEventRoundTrip_OmitsEmptyRoundAndText(t *testing.T) {
	item := suggestionTraceItem(1, SuggestionTraceEvent{Seq: 1, Kind: TraceKindError})
	if _, ok := item["round"]; ok {
		t.Errorf("round attribute present with zero value, want omitted")
	}
	if _, ok := item["text"]; ok {
		t.Errorf("text attribute present with zero value, want omitted")
	}
}

// ============================================================
// Prompt.CurrentVersionID / PromptExample.PromptVersionID
// ============================================================

// TestPromptRoundTrip_CurrentVersionID checks the field InsertPromptVersion writes and
// every classify-path read of a Prompt depends on for zero-extra-read example stamping
// (see PromptExample.PromptVersionID's doc comment) survives the round trip.
func TestPromptRoundTrip_CurrentVersionID(t *testing.T) {
	p := Prompt{ID: 1, Name: "Newsletters", Instructions: "x", Active: 1, CreatedAt: "2026-07-01 00:00:00", CurrentVersionID: 7}
	item := promptToItem(p)
	got := itemToPrompt(item)
	if got != p {
		t.Errorf("round trip mismatch: got %+v, want %+v", got, p)
	}
}

// TestPromptRoundTrip_CurrentVersionIDOmittedWhenZero checks a prompt that predates the
// version ledger (or has never been edited since) omits the attribute entirely, matching
// this codebase's omitempty convention for "not tracked" rather than "explicitly zero."
func TestPromptRoundTrip_CurrentVersionIDOmittedWhenZero(t *testing.T) {
	item := promptToItem(Prompt{ID: 1, Name: "N", Instructions: "x", Active: 1, CreatedAt: "2026-07-01 00:00:00"})
	if _, ok := item["currentVersionId"]; ok {
		t.Errorf("currentVersionId attribute present with zero value, want omitted")
	}
}

// TestPromptExampleRoundTrip_PromptVersionID checks the field buildPromptExamples
// (recategorize.go) and processor.processEmail's passive confirmation both stamp survives
// the round trip, and that Recurred/RecurredFromVersion — computed at read time by
// markRecurrences, never persisted (dynamodbav:"-") — are excluded from the wire format
// entirely rather than round-tripping as false/0 by coincidence.
func TestPromptExampleRoundTrip_PromptVersionID(t *testing.T) {
	want := PromptExample{
		ID: 1, CreatedAt: "2026-07-01 12:00:00", PromptID: 5, Verdict: VerdictFalseNegative,
		Sender: "a@example.com", Subject: "s", PromptVersionID: 12,
	}
	item := promptExampleItem(want)
	if _, ok := item["recurred"]; ok {
		t.Error("recurred must never be written to DynamoDB (dynamodbav:\"-\")")
	}
	got := itemToPromptExample(item)
	if got != want {
		t.Errorf("round trip mismatch: got %+v, want %+v", got, want)
	}
}

func TestPromptExampleRoundTrip_PromptVersionIDOmittedWhenZero(t *testing.T) {
	item := promptExampleItem(PromptExample{ID: 1, CreatedAt: "2026-07-01 12:00:00", PromptID: 5, Verdict: VerdictFalsePositive})
	if _, ok := item["promptVersionId"]; ok {
		t.Errorf("promptVersionId attribute present with zero value, want omitted")
	}
}

// TestPromptExampleRoundTrip_Source checks the field buildPromptExamples
// (recategorize.go, "manual") and processor.processEmail's passive confirmation
// ("passive") stamp survives the round trip — sampleExamples (improve.go) prioritizes
// "manual" over "passive"/empty when curating which examples the improver sees.
func TestPromptExampleRoundTrip_Source(t *testing.T) {
	want := PromptExample{
		ID: 1, CreatedAt: "2026-07-01 12:00:00", PromptID: 5, Verdict: VerdictConfirmedPositive,
		Sender: "a@example.com", Subject: "s", Source: ExampleSourceManual,
	}
	item := promptExampleItem(want)
	got := itemToPromptExample(item)
	if got != want {
		t.Errorf("round trip mismatch: got %+v, want %+v", got, want)
	}
}

// TestPromptExampleRoundTrip_SourceOmittedWhenEmpty checks a row written before Source
// tracking existed reads back as "", not an error or a guessed value — every existing
// example in the live table predates this field.
func TestPromptExampleRoundTrip_SourceOmittedWhenEmpty(t *testing.T) {
	item := promptExampleItem(PromptExample{ID: 1, CreatedAt: "2026-07-01 12:00:00", PromptID: 5, Verdict: VerdictFalsePositive})
	if _, ok := item["source"]; ok {
		t.Errorf("source attribute present with zero value, want omitted")
	}
	got := itemToPromptExample(item)
	if got.Source != "" {
		t.Errorf("Source = %q, want empty for a pre-existing row", got.Source)
	}
}

// ============================================================
// PromptVersion
// ============================================================

func TestPromptVersionRoundTrip(t *testing.T) {
	sid := int64(99)
	want := PromptVersion{
		ID: 3, PromptID: 5, CreatedAt: "2026-07-01 12:00:00",
		Instructions: "Match promotional newsletters.",
		Source:       PromptVersionSourceSuggestion,
		SuggestionID: &sid,
		ReplayModel:  "us.amazon.nova-micro-v1:0",
		ReplayTotal:  10,
		ReplayPassed: 9,
		ObservedFP:   2,
		ObservedFN:   1,
	}
	item := keyedItem(want, pkPromptVersion(want.PromptID), padID(want.ID), 0)

	if got, ok := item["PK"].(*types.AttributeValueMemberS); !ok || got.Value != "PVER#5" {
		t.Errorf("PK = %v, want PVER#5", item["PK"])
	}
	if _, ok := item[attrTTL]; ok {
		t.Error("PromptVersion must have no TTL (permanent, like PromptExample)")
	}

	got := unmarshalItem[PromptVersion](item)
	if got.SuggestionID == nil || *got.SuggestionID != sid {
		t.Fatalf("SuggestionID = %v, want %d", got.SuggestionID, sid)
	}
	got.SuggestionID, want.SuggestionID = nil, nil
	if got != want {
		t.Errorf("round trip mismatch (excluding SuggestionID, checked above): got %+v, want %+v", got, want)
	}
}

// TestPromptVersionRoundTrip_ManualHasNoSuggestionID checks a manual-edit version (no
// PromptSuggestion behind it) marshals SuggestionID as an explicit NULL, matching this
// codebase's established nullable-pointer convention (see db/models.go's package doc
// comment) rather than omitting it.
func TestPromptVersionRoundTrip_ManualHasNoSuggestionID(t *testing.T) {
	item := keyedItem(PromptVersion{ID: 1, PromptID: 5, Source: PromptVersionSourceManual}, pkPromptVersion(5), padID(1), 0)
	if _, ok := item["suggestionId"]; !ok {
		t.Error("suggestionId attribute missing; want explicit NULL")
	}
	if _, isNull := item["suggestionId"].(*types.AttributeValueMemberNULL); !isNull {
		t.Errorf("suggestionId = %T, want NULL", item["suggestionId"])
	}
	got := unmarshalItem[PromptVersion](item)
	if got.SuggestionID != nil {
		t.Errorf("SuggestionID = %v, want nil", got.SuggestionID)
	}
}

// TestPromptVersionRoundTrip_ReplayAndObservedOmittedWhenZero checks an "initial" or
// "manual" version (no replay evidence, and no production corrections observed yet) omits
// all five numeric fields rather than writing explicit zeros.
func TestPromptVersionRoundTrip_ReplayAndObservedOmittedWhenZero(t *testing.T) {
	item := keyedItem(PromptVersion{ID: 1, PromptID: 5, Source: PromptVersionSourceInitial}, pkPromptVersion(5), padID(1), 0)
	for _, attr := range []string{"replayModel", "replayTotal", "replayPassed", "observedFp", "observedFn"} {
		if _, ok := item[attr]; ok {
			t.Errorf("%s attribute present with zero value, want omitted", attr)
		}
	}
}
