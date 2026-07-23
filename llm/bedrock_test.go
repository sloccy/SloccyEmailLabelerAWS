package llm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/aws/smithy-go"
	"github.com/sloccy/ollamail-aws/db"
)

// fakeSettings wraps db.FakeStore to satisfy the Settings interface used by resolveModel.
type fakeSettings struct{ store *db.FakeStore }

func (f *fakeSettings) GetSetting(ctx context.Context, key string) (string, error) {
	return f.store.GetSetting(ctx, key)
}

func TestResolveModel_UsesSettingWhenPresent(t *testing.T) {
	store := db.NewFake()
	ctx := context.Background()
	_ = store.SetGlobalRetention(ctx, db.SetGlobalRetentionParams{}) // just to confirm store works

	// Manually set a classify_model setting via the fake.
	fs := &fakeSettings{store: db.NewFake()}

	// Seed a setting via the SetSetting-compatible approach: since FakeStore has no
	// SetSetting, we use the exported method on *db.Store which calls DynamoDB.
	// Instead, build a small in-process Settings that returns a known value.
	fixed := &fixedSettings{key: SettingClassifyModel, val: "us.amazon.nova-lite-v1:0"}
	c := &Client{defaultModel: "us.amazon.nova-micro-v1:0", settings: fixed}

	got := c.resolveModel(ctx, SettingClassifyModel)
	if got != "us.amazon.nova-lite-v1:0" {
		t.Errorf("resolveModel = %q, want us.amazon.nova-lite-v1:0", got)
	}
	_ = fs
}

func TestResolveModel_FallsBackToDefault(t *testing.T) {
	ctx := context.Background()
	// Settings returns an error (no value set).
	c := &Client{defaultModel: "us.amazon.nova-micro-v1:0", settings: &fixedSettings{}}
	got := c.resolveModel(ctx, SettingClassifyModel)
	if got != "us.amazon.nova-micro-v1:0" {
		t.Errorf("resolveModel = %q, want us.amazon.nova-micro-v1:0", got)
	}
}

func TestResolveModel_NilSettings(t *testing.T) {
	ctx := context.Background()
	c := &Client{defaultModel: "us.amazon.nova-micro-v1:0", settings: nil}
	got := c.resolveModel(ctx, SettingClassifyModel)
	if got != "us.amazon.nova-micro-v1:0" {
		t.Errorf("resolveModel = %q, want us.amazon.nova-micro-v1:0", got)
	}
}

func TestResolveModel_ClassifyAndImproveAreIndependent(t *testing.T) {
	ctx := context.Background()
	multi := &multiSettings{vals: map[string]string{
		SettingClassifyModel: "us.amazon.nova-lite-v1:0",
		SettingImproveModel:  "us.anthropic.claude-haiku-4-5-20251001-v1:0",
	}}
	c := &Client{defaultModel: "us.amazon.nova-micro-v1:0", settings: multi}

	classify := c.resolveModel(ctx, SettingClassifyModel)
	improve := c.resolveModel(ctx, SettingImproveModel)

	if classify != "us.amazon.nova-lite-v1:0" {
		t.Errorf("classify = %q, want nova-lite", classify)
	}
	if improve != "us.anthropic.claude-haiku-4-5-20251001-v1:0" {
		t.Errorf("improve = %q, want claude-haiku", improve)
	}
}

// ---- test helpers ----

type fixedSettings struct {
	key string
	val string
}

func (f *fixedSettings) GetSetting(_ context.Context, key string) (string, error) {
	if key == f.key && f.val != "" {
		return f.val, nil
	}
	return "", errNotFound
}

type multiSettings struct{ vals map[string]string }

func (m *multiSettings) GetSetting(_ context.Context, key string) (string, error) {
	if v, ok := m.vals[key]; ok {
		return v, nil
	}
	return "", errNotFound
}

var errNotFound = errors.New("not found")

// ---- ClassifyEmailBatch: text-parse classification tests ----
//
// Converse is called once per email, no tool-use — see reasoning.go and the removal of
// classifyToolConfig for why (the Converse API doesn't support tool use on the model
// families this project runs, so every response is parsed as text regardless).

// fakeConverseAPI stubs converseAPI. It returns one canned (output, err) pair per call,
// indexed by call order.
type fakeConverseAPI struct {
	outputs []*bedrockruntime.ConverseOutput
	errs    []error
	calls   []*bedrockruntime.ConverseInput
}

func (f *fakeConverseAPI) Converse(_ context.Context, params *bedrockruntime.ConverseInput, _ ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error) {
	i := len(f.calls)
	f.calls = append(f.calls, params)
	var err error
	if i < len(f.errs) {
		err = f.errs[i]
	}
	if err != nil {
		return nil, err
	}
	if i < len(f.outputs) {
		return f.outputs[i], nil
	}
	return &bedrockruntime.ConverseOutput{}, nil
}

func (f *fakeConverseAPI) ConverseStream(_ context.Context, _ *bedrockruntime.ConverseStreamInput, _ ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseStreamOutput, error) {
	return nil, errors.New("fakeConverseAPI: ConverseStream not implemented")
}

func textOutput(text string) *bedrockruntime.ConverseOutput {
	return &bedrockruntime.ConverseOutput{
		Output: &types.ConverseOutputMemberMessage{
			Value: types.Message{
				Role:    types.ConversationRoleAssistant,
				Content: []types.ContentBlock{&types.ContentBlockMemberText{Value: text}},
			},
		},
	}
}

func testPrompts() []Prompt {
	return []Prompt{
		{ID: 101, Name: "newsletter", Instructions: "matches newsletters"},
		{ID: 102, Name: "receipt", Instructions: "matches receipts"},
	}
}

func testEmail() Email {
	return Email{Sender: "a@example.com", Subject: "hello", Body: "world"}
}

func TestClassifyEmailBatch_ParsesTextResponse(t *testing.T) {
	fake := &fakeConverseAPI{
		outputs: []*bedrockruntime.ConverseOutput{
			textOutput(`{"1": false, "2": true}`),
		},
	}
	c := &Client{br: fake, defaultModel: "us.amazon.nova-micro-v1:0"}
	res, err := c.ClassifyEmailBatch(context.Background(), db.NewFake(), testEmail(), testPrompts(), c.defaultModel, TierStandard, "", false)
	if err != nil {
		t.Fatalf("ClassifyEmailBatch error: %v", err)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("expected exactly one Converse call, got %d", len(fake.calls))
	}
	if fake.calls[0].ToolConfig != nil {
		t.Errorf("classify call should not set ToolConfig (unsupported on Converse for the models this project runs)")
	}
	if res.Results[101] || !res.Results[102] {
		t.Errorf("Results = %v, want {101:false, 102:true}", res.Results)
	}
	if res.LatencyMs < 0 {
		t.Errorf("LatencyMs = %d, want >= 0", res.LatencyMs)
	}
}

func TestClassifyEmailBatch_CallFails(t *testing.T) {
	fake := &fakeConverseAPI{
		errs: []error{errors.New("ThrottlingException: rate exceeded")},
	}
	c := &Client{br: fake, defaultModel: "us.amazon.nova-micro-v1:0"}
	_, err := c.ClassifyEmailBatch(context.Background(), db.NewFake(), testEmail(), testPrompts(), c.defaultModel, TierStandard, "", false)
	if err == nil {
		t.Fatal("expected error when the Converse call fails")
	}
	if len(fake.calls) != 1 {
		t.Fatalf("expected exactly one Converse call, got %d", len(fake.calls))
	}
}

func TestClassifyEmailBatch_AppliesReasoningDirectiveFromRegistry(t *testing.T) {
	// Model id matches the "qwen" registry entry (reasoning.go) — the system blocks
	// should carry the invariant role/output-contract first, then the soft-switch
	// appended automatically, with no setting required.
	fake := &fakeConverseAPI{
		outputs: []*bedrockruntime.ConverseOutput{textOutput(`{"1": true, "2": false}`)},
	}
	c := &Client{br: fake, defaultModel: "qwen.qwen3-32b-v1:0"}
	_, err := c.ClassifyEmailBatch(context.Background(), db.NewFake(), testEmail(), testPrompts(), c.defaultModel, TierStandard, "", false)
	if err != nil {
		t.Fatalf("ClassifyEmailBatch error: %v", err)
	}
	sys := fake.calls[0].System
	if len(sys) != 2 {
		t.Fatalf("expected two system content blocks (contract + reasoning switch), got %d", len(sys))
	}
	block, ok := sys[len(sys)-1].(*types.SystemContentBlockMemberText)
	if !ok || block.Value != "/no_think" {
		t.Errorf("last System block = %#v, want /no_think text block", sys[len(sys)-1])
	}
}

func TestClassifyEmailBatch_ReasoningOverrideBeatsRegistry(t *testing.T) {
	fake := &fakeConverseAPI{
		outputs: []*bedrockruntime.ConverseOutput{textOutput(`{"1": true, "2": false}`)},
	}
	c := &Client{br: fake, defaultModel: "qwen.qwen3-32b-v1:0"}
	_, err := c.ClassifyEmailBatch(context.Background(), db.NewFake(), testEmail(), testPrompts(), c.defaultModel, TierStandard, "/custom_switch", false)
	if err != nil {
		t.Fatalf("ClassifyEmailBatch error: %v", err)
	}
	sys := fake.calls[0].System
	block, ok := sys[len(sys)-1].(*types.SystemContentBlockMemberText)
	if !ok || block.Value != "/custom_switch" {
		t.Errorf("last System block = %#v, want override text block", sys[len(sys)-1])
	}
}

func TestClassifyEmailBatch_NoDirectiveForUnknownModel(t *testing.T) {
	fake := &fakeConverseAPI{
		outputs: []*bedrockruntime.ConverseOutput{textOutput(`{"1": true, "2": false}`)},
	}
	c := &Client{br: fake, defaultModel: "meta.llama3-1-70b-instruct-v1:0"}
	_, err := c.ClassifyEmailBatch(context.Background(), db.NewFake(), testEmail(), testPrompts(), c.defaultModel, TierStandard, "", false)
	if err != nil {
		t.Fatalf("ClassifyEmailBatch error: %v", err)
	}
	sys := fake.calls[0].System
	if len(sys) != 1 {
		t.Fatalf("expected exactly one system content block (the invariant contract, no reasoning switch), got %d: %#v", len(sys), sys)
	}
	block, ok := sys[0].(*types.SystemContentBlockMemberText)
	if !ok || block.Value != classifySystemPrompt {
		t.Errorf("System[0] = %#v, want classifySystemPrompt", sys[0])
	}
}

func TestClassifyEmailBatch_MaxTokensTruncationReportedDistinctly(t *testing.T) {
	// A response with no closing JSON object (e.g. the model ran out of tokens
	// mid-object) paired with StopReason=max_tokens should surface as a dedicated
	// truncation error rather than the generic "no JSON object found" one, and
	// ClassifyResult.StopReason should record it.
	truncated := textOutput(`{"1": true, "2": fal`)
	truncated.StopReason = types.StopReasonMaxTokens
	fake := &fakeConverseAPI{outputs: []*bedrockruntime.ConverseOutput{truncated}}
	c := &Client{br: fake, defaultModel: "us.amazon.nova-micro-v1:0"}
	res, err := c.ClassifyEmailBatch(context.Background(), db.NewFake(), testEmail(), testPrompts(), c.defaultModel, TierStandard, "", false)
	if err == nil {
		t.Fatal("expected an error for a truncated, unparseable response")
	}
	if !strings.Contains(err.Error(), "truncated at max_tokens") {
		t.Errorf("err = %q, want it to mention truncation at max_tokens", err.Error())
	}
	if res.StopReason != string(types.StopReasonMaxTokens) {
		t.Errorf("res.StopReason = %q, want %q", res.StopReason, types.StopReasonMaxTokens)
	}
}

// recordingLogger captures every Log call so tests can assert on level/message.
type recordingLogger struct{ entries []string }

func (r *recordingLogger) Log(level, message string) {
	r.entries = append(r.entries, level+": "+message)
}

// fakeTimeoutError mimics the net/http client-timeout error shape (a Timeout() bool method),
// which is what actually surfaces when bedrockHTTPTimeout aborts a stuck Converse call.
type fakeTimeoutError struct{}

func (fakeTimeoutError) Error() string { return "context deadline exceeded (Client.Timeout exceeded)" }
func (fakeTimeoutError) Timeout() bool { return true }

func TestIsBedrockTimeout(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain error", errors.New("ValidationException"), false},
		{"context deadline exceeded", context.DeadlineExceeded, true},
		{"wrapped context deadline exceeded", fmt.Errorf("converse: %w", context.DeadlineExceeded), true},
		{"timeout-shaped error", fakeTimeoutError{}, true},
		{"wrapped timeout-shaped error", fmt.Errorf("converse: %w", fakeTimeoutError{}), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isBedrockTimeout(c.err); got != c.want {
				t.Errorf("isBedrockTimeout(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

func TestClassifyEmailBatch_TimeoutLoggedOnce(t *testing.T) {
	// The single Converse call times out; exactly one TIMEOUT log entry should be
	// recorded per ClassifyEmailBatch call.
	fake := &fakeConverseAPI{errs: []error{fakeTimeoutError{}}}
	c := &Client{br: fake, defaultModel: "us.amazon.nova-micro-v1:0"}
	logger := &recordingLogger{}
	_, err := c.ClassifyEmailBatch(context.Background(), logger, testEmail(), testPrompts(), c.defaultModel, TierStandard, "", false)
	if err == nil {
		t.Fatal("expected error when the call times out")
	}
	var timeoutLogs int
	for _, e := range logger.entries {
		if strings.HasPrefix(e, LogLevelTimeout+": ") {
			timeoutLogs++
		}
	}
	if timeoutLogs != 1 {
		t.Errorf("expected exactly 1 TIMEOUT log entry, got %d: %v", timeoutLogs, logger.entries)
	}
}

func TestClassifyEmailBatch_SingleSummaryLogLine(t *testing.T) {
	// A successful classify emits exactly one INFO line — the merged summary carrying
	// latency, tokens, reasoning suppression, and tier. "reasoning: suppressed=" must
	// appear verbatim: the settings page tells the user to grep the logs for it.
	fake := &fakeConverseAPI{
		outputs: []*bedrockruntime.ConverseOutput{
			textOutput(`{"1": false, "2": true}`),
		},
	}
	fake.outputs[0].Usage = &types.TokenUsage{
		InputTokens:  aws.Int32(965),
		OutputTokens: aws.Int32(31),
		TotalTokens:  aws.Int32(996),
	}
	c := &Client{br: fake, defaultModel: "us.amazon.nova-micro-v1:0"}
	logger := &recordingLogger{}
	_, err := c.ClassifyEmailBatch(context.Background(), logger, testEmail(), testPrompts(), c.defaultModel, TierStandard, "", false)
	if err != nil {
		t.Fatalf("ClassifyEmailBatch error: %v", err)
	}
	var infoLines []string
	for _, e := range logger.entries {
		if strings.HasPrefix(e, "INFO: ") {
			infoLines = append(infoLines, e)
		}
	}
	if len(infoLines) != 1 {
		t.Fatalf("expected exactly 1 INFO log entry, got %d: %v", len(infoLines), logger.entries)
	}
	for _, want := range []string{"tokens input=965 output=31 total=996", "reasoning: suppressed=", "(tier: standard)"} {
		if !strings.Contains(infoLines[0], want) {
			t.Errorf("summary line missing %q: %s", want, infoLines[0])
		}
	}
}

// ---- extractJSONObject ----

func TestExtractJSONObject(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", `{"1": true, "2": false}`, `{"1": true, "2": false}`},
		{"fenced markdown", "```json\n{\"1\": true, \"2\": false}\n```", `{"1": true, "2": false}`},
		{"fenced no lang tag", "```\n{\"1\": true}\n```", `{"1": true}`},
		{"prose preamble and suffix", "Sure, here you go:\n{\"1\": true}\nHope that helps!", `{"1": true}`},
		{
			"reasoning think block preamble",
			"<think>\nLet me consider each rule. Rule 1 seems {like a} match.\n</think>\n{\"1\": true, \"2\": false}",
			`{"1": true, "2": false}`,
		},
		{"brace inside string value not counted as nesting", `{"1": true, "note": "a { b } c"}`, `{"1": true, "note": "a { b } c"}`},
		{"escaped quote inside string", `{"note": "say \"hi\" {not a brace}"}`, `{"note": "say \"hi\" {not a brace}"}`},
		{"nested object", `{"1": true, "meta": {"x": 1}}`, `{"1": true, "meta": {"x": 1}}`},
		{"no object present", "I cannot classify this email.", ""},
		{"unbalanced braces", `{"1": true`, ""},
		{"empty string", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := extractJSONObject(c.in)
			if got != c.want {
				t.Errorf("extractJSONObject(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestClassifyEmailBatch_ParsesThinkBlockPreamble(t *testing.T) {
	// Reasoning models (e.g. Qwen3 32B) can prepend a <think>...</think> block even
	// when a reasoning-suppression directive was sent — detectReasoning (reasoning.go)
	// is what surfaces that as ReasoningDetected, but the JSON must still parse.
	fake := &fakeConverseAPI{
		outputs: []*bedrockruntime.ConverseOutput{
			textOutput("<think>\nRule 1: this looks promotional, so true. Rule 2: no receipt language, so false.\n</think>\n{\"1\": true, \"2\": false}"),
		},
	}
	c := &Client{br: fake, defaultModel: "qwen.qwen3-32b-v1:0"}
	res, err := c.ClassifyEmailBatch(context.Background(), db.NewFake(), testEmail(), testPrompts(), c.defaultModel, TierStandard, "", false)
	if err != nil {
		t.Fatalf("ClassifyEmailBatch error: %v", err)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("expected exactly one Converse call, got %d", len(fake.calls))
	}
	if !res.Results[101] || res.Results[102] {
		t.Errorf("Results = %v, want {101:true, 102:false}", res.Results)
	}
	if !res.ReasoningDetected {
		t.Errorf("ReasoningDetected = false, want true (response contains a <think> block)")
	}
}

func TestImprovePromptInstructions_ServiceTierFollowsImproveTierSetting(t *testing.T) {
	cases := []struct {
		name     string
		settings Settings
		wantFlex bool
	}{
		{"default (unset) is standard", &fixedSettings{}, false},
		{"improve_tier=flex sends flex service tier", &multiSettings{vals: map[string]string{SettingImproveTier: TierFlex}}, true},
		{"improve_tier=standard sends no service tier", &multiSettings{vals: map[string]string{SettingImproveTier: TierStandard}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake := &fakeConverseAPI{outputs: []*bedrockruntime.ConverseOutput{textOutput("rewritten instructions")}}
			cl := &Client{br: fake, defaultModel: "us.amazon.nova-micro-v1:0", settings: c.settings}
			_, _, err := cl.ImprovePromptInstructions(context.Background(), ImproveRequest{
				PromptName: "newsletter", LabelName: "News", TriggerKind: "false_negative",
				OriginalInstructions: "matches newsletters",
				EmailSender:          "a@example.com", EmailSubject: "hello", EmailBody: "world",
			})
			if err != nil {
				t.Fatalf("ImprovePromptInstructions error: %v", err)
			}
			if len(fake.calls) != 1 {
				t.Fatalf("expected exactly one Converse call, got %d", len(fake.calls))
			}
			got := fake.calls[0].ServiceTier
			if c.wantFlex {
				if got == nil || got.Type != types.ServiceTierTypeFlex {
					t.Errorf("ServiceTier = %+v, want flex", got)
				}
			} else if got != nil {
				t.Errorf("ServiceTier = %+v, want nil (implicit standard)", got)
			}
		})
	}
}

// TestNewBedrockRetryer_RetriesClockSkewAndThrottling checks that the retryer built by
// newBedrockRetryer treats both the clock-skew signature error codes (added explicitly
// because the SDK doesn't retry them by default — see the Lambda freeze/thaw scenario
// documented on newBedrockRetryer) and a standard throttling code as retryable.
func TestNewBedrockRetryer_RetriesClockSkewAndThrottling(t *testing.T) {
	r := newBedrockRetryer()
	for _, code := range []string{
		"InvalidSignatureException",
		"RequestExpired",
		"RequestTimeTooSkewed",
		"SignatureDoesNotMatch",
		"ThrottlingException", // sanity check: preexisting adaptive-mode behavior still works
	} {
		err := &smithy.GenericAPIError{Code: code, Message: "boom"}
		if !r.IsErrorRetryable(err) {
			t.Errorf("IsErrorRetryable(%s) = false, want true", code)
		}
	}

	if unrelated := (&smithy.GenericAPIError{Code: "ValidationException", Message: "bad input"}); r.IsErrorRetryable(unrelated) {
		t.Errorf("IsErrorRetryable(ValidationException) = true, want false (not a transient error)")
	}
}
