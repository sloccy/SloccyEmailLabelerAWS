package db

import (
	"database/sql"
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
