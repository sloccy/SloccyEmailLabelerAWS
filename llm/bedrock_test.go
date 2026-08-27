package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

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
// indexed by call order. Converse and ConverseStream calls are tracked and indexed
// independently — ImprovePromptInstructions only ever drives ConverseStream, so the two
// call lists never need to interleave for any test in this file.
type fakeConverseAPI struct {
	outputs []*bedrockruntime.ConverseOutput
	errs    []error
	calls   []*bedrockruntime.ConverseInput

	// streamEvents[i] is the canned event sequence for the i-th ConverseStream call;
	// streamErrs[i], if set, is returned instead (before any events, matching how a
	// request-shape rejection like ValidationException actually arrives from Bedrock —
	// see converseAPI's doc comment on why a fake can only ever produce a
	// *bedrockruntime.ConverseStreamEventStream, never the SDK's own ConverseStreamOutput).
	// streamMidErrs[i], if set, is what Err() reports after streamEvents[i] has been
	// delivered — a failure discovered mid-stream (e.g. a dropped connection), distinct
	// from streamErrs' pre-stream rejection.
	streamEvents  [][]types.ConverseStreamOutput
	streamErrs    []error
	streamMidErrs []error
	streamCalls   []*bedrockruntime.ConverseStreamInput

	// streamReaders[i], if set, builds the i-th ConverseStream call's reader directly from
	// the ctx that call received, instead of the pre-buffered channel streamEvents produces
	// (built before ConverseStream ever sees ctx, so it can't react to cancellation). Used
	// by the stall-guard tests below, which need the reader itself to notice ctx.Done() —
	// exactly what a real Bedrock connection does when its context is cancelled.
	streamReaders []func(ctx context.Context) bedrockruntime.ConverseStreamOutputReader
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

func (f *fakeConverseAPI) ConverseStream(ctx context.Context, params *bedrockruntime.ConverseStreamInput, _ ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseStreamEventStream, error) {
	i := len(f.streamCalls)
	f.streamCalls = append(f.streamCalls, params)
	if i < len(f.streamErrs) && f.streamErrs[i] != nil {
		return nil, f.streamErrs[i]
	}
	if i < len(f.streamReaders) && f.streamReaders[i] != nil {
		return bedrockruntime.NewConverseStreamEventStream(func(es *bedrockruntime.ConverseStreamEventStream) {
			es.Reader = f.streamReaders[i](ctx)
		}), nil
	}
	var events []types.ConverseStreamOutput
	if i < len(f.streamEvents) {
		events = f.streamEvents[i]
	}
	var midErr error
	if i < len(f.streamMidErrs) {
		midErr = f.streamMidErrs[i]
	}
	return bedrockruntime.NewConverseStreamEventStream(func(es *bedrockruntime.ConverseStreamEventStream) {
		es.Reader = newFakeStreamReader(events, midErr)
	}), nil
}

// fakeStreamReader implements bedrockruntime.ConverseStreamOutputReader by replaying a
// canned, already-buffered sequence of events, then reporting err (nil for a clean stream)
// from Err() once the channel has drained.
type fakeStreamReader struct {
	ch  chan types.ConverseStreamOutput
	err error
}

func newFakeStreamReader(events []types.ConverseStreamOutput, err error) *fakeStreamReader {
	ch := make(chan types.ConverseStreamOutput, len(events))
	for _, e := range events {
		ch <- e
	}
	close(ch)
	return &fakeStreamReader{ch: ch, err: err}
}

func (r *fakeStreamReader) Events() <-chan types.ConverseStreamOutput { return r.ch }
func (r *fakeStreamReader) Close() error                              { return nil }
func (r *fakeStreamReader) Err() error                                { return r.err }

// ctxStreamReader is fakeStreamReader's ctx-aware counterpart: fakeStreamReader fills its
// channel before ConverseStream is even called, so a stall-guard timeout — which cancels
// the ctx the reader was given — has nothing to interrupt. ctxStreamReader instead feeds
// its channel from a goroutine that watches ctx.Done(), the way a real Bedrock connection
// is torn down when its context is cancelled. Built via delayedStreamReader/
// silentStreamReader below rather than directly.
type ctxStreamReader struct {
	ch chan types.ConverseStreamOutput

	mu  sync.Mutex
	err error
}

func (r *ctxStreamReader) Events() <-chan types.ConverseStreamOutput { return r.ch }
func (r *ctxStreamReader) Close() error                              { return nil }
func (r *ctxStreamReader) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}
func (r *ctxStreamReader) setErr(err error) {
	r.mu.Lock()
	r.err = err
	r.mu.Unlock()
}

// delayedStreamReader replays events one at a time, waiting delay between each, then
// closes cleanly — the shape a stall-guard test needs to prove that a steady trickle of
// deltas (each resetting the guard) keeps a call alive well past the guard's budget in
// aggregate. If ctx is cancelled before every event is sent (a guard firing mid-stream),
// the reader stops early and reports ctx's cancellation cause.
func delayedStreamReader(events []types.ConverseStreamOutput, delay time.Duration) func(ctx context.Context) bedrockruntime.ConverseStreamOutputReader {
	return func(ctx context.Context) bedrockruntime.ConverseStreamOutputReader {
		ch := make(chan types.ConverseStreamOutput)
		r := &ctxStreamReader{ch: ch}
		go func() {
			defer close(ch)
			for _, e := range events {
				select {
				case <-ctx.Done():
					r.setErr(context.Cause(ctx))
					return
				case <-time.After(delay):
				}
				select {
				case ch <- e:
				case <-ctx.Done():
					r.setErr(context.Cause(ctx))
					return
				}
			}
		}()
		return r
	}
}

// silentStreamReader never sends a single event — simulating a call that produces no
// output at all (the gap before a queued flex-tier request's first delta, or a fully
// wedged connection) — until ctx is cancelled, at which point it reports ctx's
// cancellation cause. It never closes on its own, the way a genuinely stalled connection
// wouldn't either.
func silentStreamReader() func(ctx context.Context) bedrockruntime.ConverseStreamOutputReader {
	return func(ctx context.Context) bedrockruntime.ConverseStreamOutputReader {
		ch := make(chan types.ConverseStreamOutput)
		r := &ctxStreamReader{ch: ch}
		go func() {
			defer close(ch)
			<-ctx.Done()
			r.setErr(context.Cause(ctx))
		}()
		return r
	}
}

// textDeltaEvent, reasoningDeltaEvent, and stopEvent build the ConverseStreamOutput union
// members ImprovePromptInstructions/streamGenerate switch on, for streamEvents fixtures.
func textDeltaEvent(s string) types.ConverseStreamOutput {
	return &types.ConverseStreamOutputMemberContentBlockDelta{
		Value: types.ContentBlockDeltaEvent{Delta: &types.ContentBlockDeltaMemberText{Value: s}},
	}
}

func reasoningDeltaEvent(s string) types.ConverseStreamOutput {
	return &types.ConverseStreamOutputMemberContentBlockDelta{
		Value: types.ContentBlockDeltaEvent{Delta: &types.ContentBlockDeltaMemberReasoningContent{
			Value: &types.ReasoningContentBlockDeltaMemberText{Value: s},
		}},
	}
}

func stopEvent(reason types.StopReason) types.ConverseStreamOutput {
	return &types.ConverseStreamOutputMemberMessageStop{Value: types.MessageStopEvent{StopReason: reason}}
}

// textStreamOutput is textOutput's streaming equivalent: a single text delta followed by
// a normal end_turn stop, the shape every ImprovePromptInstructions test that just wants
// "some rewritten text came back" uses. Every call site wants the same placeholder text,
// unlike textOutput's varied call sites — no parameter to keep unparam happy about it.
func textStreamOutput() []types.ConverseStreamOutput {
	return []types.ConverseStreamOutput{textDeltaEvent("rewritten instructions"), stopEvent(types.StopReasonEndTurn)}
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

func TestClassifyEmailBatch_ParsesMatchListResponse(t *testing.T) {
	fake := &fakeConverseAPI{
		outputs: []*bedrockruntime.ConverseOutput{
			textOutput(`{"m": [2]}`),
		},
	}
	c := &Client{br: fake, defaultModel: "us.amazon.nova-micro-v1:0"}
	res, err := c.ClassifyEmailBatch(context.Background(), db.NewFake(), testEmail(), testPrompts(), c.defaultModel, TierStandard, "", false)
	if err != nil {
		t.Fatalf("ClassifyEmailBatch error: %v", err)
	}
	if res.Results[101] || !res.Results[102] {
		t.Errorf("Results = %v, want {101:false, 102:true}", res.Results)
	}
}

func TestClassifyEmailBatch_ParsesEmptyMatchList(t *testing.T) {
	fake := &fakeConverseAPI{
		outputs: []*bedrockruntime.ConverseOutput{
			textOutput(`{"m": []}`),
		},
	}
	c := &Client{br: fake, defaultModel: "us.amazon.nova-micro-v1:0"}
	res, err := c.ClassifyEmailBatch(context.Background(), db.NewFake(), testEmail(), testPrompts(), c.defaultModel, TierStandard, "", false)
	if err != nil {
		t.Fatalf("ClassifyEmailBatch error: %v", err)
	}
	if res.Results[101] || res.Results[102] {
		t.Errorf("Results = %v, want every prompt false", res.Results)
	}
}

func TestParseClassifyResponse(t *testing.T) {
	prompts := testPrompts() // IDs 101, 102

	cases := []struct {
		name string
		raw  string
		want map[int64]bool
	}{
		{"match list, one hit", `{"m": [2]}`, map[int64]bool{101: false, 102: true}},
		{"match list, both", `{"m": [1, 2]}`, map[int64]bool{101: true, 102: true}},
		{"match list, empty", `{"m": []}`, map[int64]bool{101: false, 102: false}},
		{"match list, numeric string entries", `{"m": ["2"]}`, map[int64]bool{101: false, 102: true}},
		{"match list, out-of-range and non-numeric entries ignored", `{"m": [0, 3, "x", 2]}`, map[int64]bool{101: false, 102: true}},
		{"legacy boolean map", `{"1": true, "2": false}`, map[int64]bool{101: true, 102: false}},
		{"legacy boolean map, partial", `{"2": true}`, map[int64]bool{102: true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var parsed map[string]any
			if err := json.Unmarshal([]byte(c.raw), &parsed); err != nil {
				t.Fatalf("json.Unmarshal(%q): %v", c.raw, err)
			}
			got := parseClassifyResponse(parsed, prompts)
			// Legacy partial maps intentionally omit unset keys (see mapKeysToResults);
			// the match-list path always seeds every prompt false first.
			for id, want := range c.want {
				if got[id] != want {
					t.Errorf("parseClassifyResponse(%q)[%d] = %v, want %v", c.raw, id, got[id], want)
				}
			}
		})
	}
}

func TestBuildUserTurn_ExampleIsConstantRegardlessOfRuleCount(t *testing.T) {
	// The old per-rule boolean example grew with rule count; the {"m": [...]} contract's
	// example doesn't need to, which is half of the token saving this format switch is for.
	one := buildUserTurn(testEmail(), testPrompts()[:1])
	many := buildUserTurn(testEmail(), []Prompt{
		{ID: 1, Name: "a", Instructions: "x"},
		{ID: 2, Name: "b", Instructions: "y"},
		{ID: 3, Name: "c", Instructions: "z"},
		{ID: 4, Name: "d", Instructions: "w"},
		{ID: 5, Name: "e", Instructions: "v"},
	})
	const wantExample = `Example (rule 1 applies, no others): {"m": [1]}`
	if !strings.Contains(one, wantExample) {
		t.Errorf("buildUserTurn with 1 rule missing constant example %q:\n%s", wantExample, one)
	}
	if !strings.Contains(many, wantExample) {
		t.Errorf("buildUserTurn with 5 rules missing constant example %q:\n%s", wantExample, many)
	}
}

func TestStripURLs(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"bare URL", "https://example.com/track?utm_source=x", " "},
		{"URL mid-sentence, surrounding words don't fuse", "click here https://example.com/a/b to unsubscribe", "click here   to unsubscribe"},
		{"http (non-https) URL", "see http://example.com for details", "see   for details"},
		{"no URL is a no-op", "just plain text, no links here", "just plain text, no links here"},
		{"empty input", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := stripURLs(c.in)
			if got != c.want {
				t.Errorf("stripURLs(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestBuildUserTurn_BodyCleanedOfLineFragmentationAndURLs(t *testing.T) {
	// buildUserTurn should turn extractText's one-word-per-line output, NBSP padding, and a
	// visible tracking URL back into flowing, URL-free prose before it reaches the model —
	// none of that carries classification signal, and it's pure token/quality noise as-is.
	email := testEmail()
	email.Body = "Hello\nworld\nfrom\nour\u00a0\u00a0team. https://example.com/track?utm_source=newsletter&utm_campaign=x Unsubscribe here."
	turn := buildUserTurn(email, testPrompts())

	if strings.Contains(turn, "https://") || strings.Contains(turn, "http://") {
		t.Errorf("buildUserTurn should strip URLs from the body, got:\n%s", turn)
	}
	const wantBody = "Hello world from our team. Unsubscribe here."
	if !strings.Contains(turn, wantBody) {
		t.Errorf("buildUserTurn body not collapsed to flowing prose; want it to contain %q, got:\n%s", wantBody, turn)
	}
}

// ---- extractJSONObject ----

func TestSanitizeRuleText(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "Matches newsletters from SaaS products.", "Matches newsletters from SaaS products."},
		{
			"leaked think block",
			"<think>\nThe rule should cover promotional emails too.\n</think>\nMatches promotional emails.",
			"Matches promotional emails.",
		},
		{
			"fenced markdown",
			"```\nMatches receipts and invoices.\n```",
			"Matches receipts and invoices.",
		},
		{
			"fenced with language tag",
			"```text\nMatches shipping notifications.\n```",
			"Matches shipping notifications.",
		},
		{"surrounding double quotes", `"Matches order confirmations."`, "Matches order confirmations."},
		{"surrounding smart quotes", "“Matches calendar invites.”", "Matches calendar invites."},
		{
			"embedded newlines collapse to single spaces",
			"Matches newsletters.\nDo not match transactional receipts.\n\nOr spam.",
			"Matches newsletters. Do not match transactional receipts. Or spam.",
		},
		{
			"multiple internal spaces collapse",
			"Matches   newsletters    from  SaaS products.",
			"Matches newsletters from SaaS products.",
		},
		{"empty input", "", ""},
		{"whitespace only", "   \n\t  ", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := sanitizeRuleText(c.in)
			if got != c.want {
				t.Errorf("sanitizeRuleText(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestBuildImproveUserTurn_EmptyCorpusStaysCoherent(t *testing.T) {
	// A brand-new rule has no recorded examples yet — the prompt must still be a sensible,
	// well-formed request rather than printing empty "SHOULD MATCH:" headings with nothing
	// under them.
	turn := buildImproveUserTurn(ImproveRequest{
		PromptName:           "Newsletters",
		LabelName:            "News",
		OriginalInstructions: "Matches newsletters from SaaS products.",
	})
	if !strings.Contains(turn, "RULE: Newsletters") || !strings.Contains(turn, "LABEL: News") {
		t.Errorf("missing rule/label header: %q", turn)
	}
	if !strings.Contains(turn, "Matches newsletters from SaaS products.") {
		t.Errorf("missing current instructions: %q", turn)
	}
	for _, heading := range []string{"SHOULD MATCH", "SHOULD NOT MATCH", "USER NOTE"} {
		if strings.Contains(turn, heading) {
			t.Errorf("empty section heading %q should be omitted entirely, got: %q", heading, turn)
		}
	}
}

func TestBuildImproveUserTurn_RendersPopulatedSections(t *testing.T) {
	turn := buildImproveUserTurn(ImproveRequest{
		PromptName:           "Newsletters",
		LabelName:            "News",
		OriginalInstructions: "Matches newsletters.",
		ShouldMatch:          []ExampleRef{{Sender: "a@example.com", Subject: "Weekly digest", Excerpt: "top stories this week", Missed: true}},
		ShouldNotMatch:       []ExampleRef{{Sender: "b@example.com", Subject: "Your receipt", Excerpt: "payment confirmed"}},
		UserNote:             "these are spam, not receipts",
	})
	for _, want := range []string{
		"SHOULD MATCH", "[MISSED] a@example.com", "Weekly digest",
		"SHOULD NOT MATCH", "b@example.com", "Your receipt",
		"USER NOTE: these are spam, not receipts",
	} {
		if !strings.Contains(turn, want) {
			t.Errorf("expected user turn to contain %q, got: %q", want, turn)
		}
	}
	if strings.Contains(turn, "[MISSED] b@example.com") {
		t.Errorf("non-missed example must not be prefixed, got: %q", turn)
	}
}

// TestBuildImproveUserTurn_RecurredExamplePrefixedAndClauseAdded checks two things a
// recurring example (ExampleRef.Recurred) must trigger: the "[RECURRED]" line prefix
// (formatExampleRefs) and the extra closing-goals clause telling the model a small edit
// won't fix it. A non-recurred example in the same request must not get either.
func TestBuildImproveUserTurn_RecurredExamplePrefixedAndClauseAdded(t *testing.T) {
	turn := buildImproveUserTurn(ImproveRequest{
		PromptName:           "Newsletters",
		LabelName:            "News",
		OriginalInstructions: "Matches newsletters.",
		ShouldMatch: []ExampleRef{
			{Sender: "a@example.com", Subject: "Weekly digest", Excerpt: "top stories", Recurred: true},
			{Sender: "z@example.com", Subject: "First time", Excerpt: "new problem"},
		},
	})
	if !strings.Contains(turn, "[RECURRED] a@example.com") {
		t.Errorf("expected the recurred example prefixed, got: %q", turn)
	}
	if strings.Contains(turn, "[RECURRED] z@example.com") {
		t.Errorf("non-recurred example must not be prefixed, got: %q", turn)
	}
	if !strings.Contains(turn, "survived an earlier rewrite") {
		t.Errorf("expected the recurred closing clause, got: %q", turn)
	}
}

func TestBuildImproveUserTurn_NoRecurrenceOmitsClause(t *testing.T) {
	turn := buildImproveUserTurn(ImproveRequest{
		PromptName:           "Newsletters",
		LabelName:            "News",
		OriginalInstructions: "Matches newsletters.",
		ShouldMatch:          []ExampleRef{{Sender: "a@example.com", Subject: "s", Excerpt: "e"}},
	})
	if strings.Contains(turn, "survived an earlier rewrite") {
		t.Errorf("no example recurred, clause should be omitted, got: %q", turn)
	}
}

// TestBuildImproveUserTurn_PastAttemptsSection checks the cross-session-memory section:
// rendered between CURRENT and the example groups, only for attempts with evidence
// (Total > 0), and omitted entirely when there's nothing to show.
func TestBuildImproveUserTurn_PastAttemptsSection(t *testing.T) {
	turn := buildImproveUserTurn(ImproveRequest{
		PromptName:           "Newsletters",
		LabelName:            "News",
		OriginalInstructions: "Matches promotional newsletters.",
		PastAttempts: []AttemptRef{
			{Instructions: "Matches newsletters.", Passed: 7, Total: 10},
			{Instructions: "No evidence yet.", Passed: 0, Total: 0},
		},
	})
	if !strings.Contains(turn, "PAST ATTEMPTS") {
		t.Errorf("expected a PAST ATTEMPTS section, got: %q", turn)
	}
	if !strings.Contains(turn, `"Matches newsletters." -> scored 7/10`) {
		t.Errorf("expected the attempt with evidence rendered, got: %q", turn)
	}
	if strings.Contains(turn, "No evidence yet.") {
		t.Errorf("an attempt with Total=0 must be omitted, got: %q", turn)
	}
	if idx1, idx2 := strings.Index(turn, "PAST ATTEMPTS"), strings.Index(turn, "CURRENT:"); idx1 < idx2 {
		t.Errorf("PAST ATTEMPTS must come after CURRENT, got: %q", turn)
	}
}

func TestBuildImproveUserTurn_NoPastAttemptsOmitsSection(t *testing.T) {
	turn := buildImproveUserTurn(ImproveRequest{PromptName: "N", LabelName: "L", OriginalInstructions: "x"})
	if strings.Contains(turn, "PAST ATTEMPTS") {
		t.Errorf("expected no PAST ATTEMPTS section for a rule with no history, got: %q", turn)
	}
}

func TestFormatAttempts(t *testing.T) {
	t.Run("caps at maxAttemptLines and skips zero-evidence entries", func(t *testing.T) {
		attempts := []AttemptRef{
			{Instructions: "v1", Passed: 1, Total: 5},
			{Instructions: "v2", Passed: 0, Total: 0}, // skipped: no evidence
			{Instructions: "v3", Passed: 2, Total: 5},
			{Instructions: "v4", Passed: 3, Total: 5},
			{Instructions: "v5", Passed: 4, Total: 5}, // beyond the cap once v2 is skipped
		}
		got := formatAttempts(attempts)
		if strings.Contains(got, "v2") {
			t.Errorf("zero-evidence attempt should be skipped: %s", got)
		}
		if strings.Contains(got, "v5") {
			t.Errorf("expected only %d attempts with evidence, v5 is the %dth: %s", maxAttemptLines, maxAttemptLines+1, got)
		}
		for _, want := range []string{"v1", "v3", "v4"} {
			if !strings.Contains(got, want) {
				t.Errorf("expected %s in output: %s", want, got)
			}
		}
	})

	t.Run("truncates long instructions", func(t *testing.T) {
		long := strings.Repeat("x", 300)
		got := formatAttempts([]AttemptRef{{Instructions: long, Passed: 1, Total: 1}})
		if strings.Contains(got, long) {
			t.Errorf("expected the 300-char instructions truncated to 200, got the full text")
		}
		if !strings.Contains(got, strings.Repeat("x", 200)) {
			t.Errorf("expected a 200-char prefix, got: %s", got)
		}
	})

	t.Run("empty input", func(t *testing.T) {
		if got := formatAttempts(nil); got != "" {
			t.Errorf("formatAttempts(nil) = %q, want empty", got)
		}
	})
}

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
			fake := &fakeConverseAPI{streamEvents: [][]types.ConverseStreamOutput{textStreamOutput()}}
			cl := &Client{br: fake, defaultModel: "us.amazon.nova-micro-v1:0", settings: c.settings}
			_, _, err := cl.ImprovePromptInstructions(context.Background(), ImproveRequest{
				PromptName:           "newsletter",
				LabelName:            "News",
				OriginalInstructions: "matches newsletters",
				ShouldMatch:          []ExampleRef{{Sender: "a@example.com", Subject: "hello", Excerpt: "world"}},
			}, nil)
			if err != nil {
				t.Fatalf("ImprovePromptInstructions error: %v", err)
			}
			if len(fake.streamCalls) != 1 {
				t.Fatalf("expected exactly one ConverseStream call, got %d", len(fake.streamCalls))
			}
			got := fake.streamCalls[0].ServiceTier
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

// TestImprovePromptInstructions_UnsupportedEffortDoesNotInvertToSuppression guards the fix
// for a real inversion bug, using the one model where it can actually happen:
// nvidia.nemotron-nano-9b-v2 is both exempt from reasoningEffortFields (verified live to
// ignore reasoning_config — see modelCapabilities' noEffort entries) AND matched by
// modelCapabilities' *suppression* entry ("nemotron" -> "detailed thinking off",
// reasoningOff). Requesting
// ReasoningEffortOn on this exact model must leave it at its own default — it must NOT fall
// through into reasoningOff and inject the suppression text, which would answer "turn
// reasoning on" by turning it off, the opposite of what was asked.
func TestImprovePromptInstructions_UnsupportedEffortDoesNotInvertToSuppression(t *testing.T) {
	settings := &fixedSettings{key: SettingImproveReasoningEffort, val: ReasoningEffortOn}
	fake := &fakeConverseAPI{streamEvents: [][]types.ConverseStreamOutput{textStreamOutput()}}
	cl := &Client{br: fake, defaultModel: "nvidia.nemotron-nano-9b-v2", settings: settings}

	_, _, err := cl.ImprovePromptInstructions(context.Background(), ImproveRequest{
		PromptName:           "newsletter",
		LabelName:            "News",
		OriginalInstructions: "matches newsletters",
		ShouldMatch:          []ExampleRef{{Sender: "a@example.com", Subject: "hello", Excerpt: "world"}},
	}, nil)
	if err != nil {
		t.Fatalf("ImprovePromptInstructions error: %v", err)
	}
	if len(fake.streamCalls) != 1 {
		t.Fatalf("expected exactly one ConverseStream call, got %d", len(fake.streamCalls))
	}
	for _, block := range fake.streamCalls[0].System {
		if txt, ok := block.(*types.SystemContentBlockMemberText); ok && strings.Contains(txt.Value, "detailed thinking off") {
			t.Errorf("system prompt contains the Nemotron suppression switch %q — an unsupported effort request must not invert into suppression", txt.Value)
		}
	}
	if fake.streamCalls[0].AdditionalModelRequestFields != nil {
		t.Errorf("AdditionalModelRequestFields = %v, want nil (effort unsupported for this model, nothing should be sent)", fake.streamCalls[0].AdditionalModelRequestFields)
	}
}

// TestImprovePromptInstructions_ValidationExceptionRetriesWithoutFields checks the fail-safe
// for an unverified model's assumed reasoning_config support turning out to be wrong (see
// reasoningEffortSupported's default-on design) — it's an unvalidated passthrough field, so
// this can happen for any model outside the confirmed sweep. If the
// fields-bearing Converse call is rejected with a ValidationException, the call must retry
// once with the fields dropped rather than surface the error and leave the suggestion failed.
func TestImprovePromptInstructions_ValidationExceptionRetriesWithoutFields(t *testing.T) {
	settings := &fixedSettings{key: SettingImproveReasoningEffort, val: ReasoningEffortOn}
	fake := &fakeConverseAPI{
		// The first ConverseStream call errors before any events are ever produced —
		// exactly how a request-shape rejection like ValidationException actually arrives
		// (Bedrock rejects the request before streaming starts), which is what lets the
		// emitted-gate on the retry stay untested here and covered separately by
		// TestImprovePromptInstructions_NoRetryAfterPartialStream below.
		streamErrs:   []error{&types.ValidationException{Message: aws.String("unknown field reasoning_config")}, nil},
		streamEvents: [][]types.ConverseStreamOutput{nil, textStreamOutput()},
	}
	cl := &Client{br: fake, defaultModel: "zai.glm-5", settings: settings}

	text, _, err := cl.ImprovePromptInstructions(context.Background(), ImproveRequest{
		PromptName:           "newsletter",
		LabelName:            "News",
		OriginalInstructions: "matches newsletters",
		ShouldMatch:          []ExampleRef{{Sender: "a@example.com", Subject: "hello", Excerpt: "world"}},
	}, nil)
	if err != nil {
		t.Fatalf("ImprovePromptInstructions error: %v, want the retry to succeed", err)
	}
	if text == "" {
		t.Errorf("got empty rewritten instructions, want the second call's output")
	}
	if len(fake.streamCalls) != 2 {
		t.Fatalf("expected 2 ConverseStream calls (initial + retry), got %d", len(fake.streamCalls))
	}
	if fake.streamCalls[0].AdditionalModelRequestFields == nil {
		t.Errorf("first call should have sent reasoning fields")
	}
	if fake.streamCalls[1].AdditionalModelRequestFields != nil {
		t.Errorf("retry call AdditionalModelRequestFields = %v, want nil (dropped after ValidationException)", fake.streamCalls[1].AdditionalModelRequestFields)
	}
	if got := aws.ToInt32(fake.streamCalls[1].InferenceConfig.MaxTokens); got != 2048 {
		t.Errorf("retry call MaxTokens = %d, want 2048 (reasoning fields dropped, so back to the tight non-reasoning ceiling)", got)
	}
}

// TestImprovePromptInstructions_NoRetryAfterPartialStream guards the streaming-specific
// half of the ValidationException retry gate: once any delta has already reached the
// sink, a retry must not fire even if the stream then errors — the caller may already be
// rendering a partial answer, and silently starting over would look like corrupted output,
// not a clean retry. This can't happen for a real ValidationException (Bedrock rejects a
// bad request field before streaming starts, see the test above) but the gate exists for
// whatever mid-stream failure shape does surface this way, so it's tested directly against
// a mid-stream Err() rather than assumed.
func TestImprovePromptInstructions_NoRetryAfterPartialStream(t *testing.T) {
	settings := &fixedSettings{key: SettingImproveReasoningEffort, val: ReasoningEffortOn}
	streamBroke := errors.New("connection reset mid-stream")
	fake := &fakeConverseAPI{
		streamEvents:  [][]types.ConverseStreamOutput{{textDeltaEvent("partial")}},
		streamMidErrs: []error{streamBroke},
	}
	cl := &Client{br: fake, defaultModel: "zai.glm-5", settings: settings}

	var got []StreamChunk
	_, _, err := cl.ImprovePromptInstructions(context.Background(), ImproveRequest{
		PromptName:           "newsletter",
		LabelName:            "News",
		OriginalInstructions: "matches newsletters",
		ShouldMatch:          []ExampleRef{{Sender: "a@example.com", Subject: "hello", Excerpt: "world"}},
	}, func(c StreamChunk) { got = append(got, c) })
	if !errors.Is(err, streamBroke) {
		t.Fatalf("err = %v, want the mid-stream error surfaced (no retry to swallow it)", err)
	}
	if len(fake.streamCalls) != 1 {
		t.Errorf("expected exactly one ConverseStream call — no retry after a partial stream, got %d", len(fake.streamCalls))
	}
	if len(got) != 1 || got[0].Text != "partial" {
		t.Errorf("sink chunks = %+v, want exactly one Text=\"partial\" chunk", got)
	}
}

// TestImprovePromptInstructions_MaxTokensFollowsReasoningState guards the fix for a real
// truncation bug found while sweeping other models: 2048 tokens is enough for the ~60-word
// answer improveSystemPrompt asks for, but a model that's actually reasoning spends most of
// that budget on the reasoning trace first (observed up to ~2000 chars of it against real
// Bedrock models) and hits the ceiling before ever writing an answer — confirmed live
// against qwen3-32b and zai.glm-4.7-flash, both of which came back with StopReasonMaxTokens
// and zero answer text at a 2048-equivalent budget. MaxTokens must scale up whenever
// reasoning fields are actually being sent, and back down when they're not.
func TestImprovePromptInstructions_MaxTokensFollowsReasoningState(t *testing.T) {
	cases := []struct {
		name     string
		settings Settings
		model    string
		want     int32
	}{
		{"reasoning off: tight ceiling", &fixedSettings{}, "zai.glm-5", 2048},
		{"reasoning on: room for thinking", &fixedSettings{key: SettingImproveReasoningEffort, val: ReasoningEffortOn}, "zai.glm-5", 16384},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake := &fakeConverseAPI{streamEvents: [][]types.ConverseStreamOutput{textStreamOutput()}}
			cl := &Client{br: fake, defaultModel: c.model, settings: c.settings}
			_, _, err := cl.ImprovePromptInstructions(context.Background(), ImproveRequest{
				PromptName:           "newsletter",
				LabelName:            "News",
				OriginalInstructions: "matches newsletters",
				ShouldMatch:          []ExampleRef{{Sender: "a@example.com", Subject: "hello", Excerpt: "world"}},
			}, nil)
			if err != nil {
				t.Fatalf("ImprovePromptInstructions error: %v", err)
			}
			if len(fake.streamCalls) != 1 {
				t.Fatalf("expected exactly one ConverseStream call, got %d", len(fake.streamCalls))
			}
			if got := aws.ToInt32(fake.streamCalls[0].InferenceConfig.MaxTokens); got != c.want {
				t.Errorf("MaxTokens = %d, want %d", got, c.want)
			}
		})
	}
}

// TestImprovePromptInstructions_StreamsAnswerAndReasoningSeparately checks the sink
// contract end to end: answer deltas arrive as StreamChunk.Text, reasoning deltas arrive
// as StreamChunk.Reasoning (never mixed into Text, never fed back into the returned
// conversation), and the final returned suggestion is the sanitized concatenation of the
// answer deltas only.
func TestImprovePromptInstructions_StreamsAnswerAndReasoningSeparately(t *testing.T) {
	fake := &fakeConverseAPI{
		streamEvents: [][]types.ConverseStreamOutput{{
			reasoningDeltaEvent("Let me think about this rule. "),
			textDeltaEvent("Match "),
			reasoningDeltaEvent("Yes, that covers it."),
			textDeltaEvent("newsletters from any sender."),
			stopEvent(types.StopReasonEndTurn),
		}},
	}
	cl := &Client{br: fake, defaultModel: "us.amazon.nova-micro-v1:0"}

	var chunks []StreamChunk
	suggestion, conv, err := cl.ImprovePromptInstructions(context.Background(), ImproveRequest{
		PromptName:           "newsletter",
		LabelName:            "News",
		OriginalInstructions: "matches newsletters",
		ShouldMatch:          []ExampleRef{{Sender: "a@example.com", Subject: "hello", Excerpt: "world"}},
	}, func(c StreamChunk) { chunks = append(chunks, c) })
	if err != nil {
		t.Fatalf("ImprovePromptInstructions error: %v", err)
	}

	var textBuf, reasoningBuf strings.Builder
	for _, c := range chunks {
		textBuf.WriteString(c.Text)
		reasoningBuf.WriteString(c.Reasoning)
		if c.Text != "" && c.Reasoning != "" {
			t.Errorf("chunk mixed Text and Reasoning in one delta: %+v", c)
		}
	}
	gotText, gotReasoning := textBuf.String(), reasoningBuf.String()
	if gotText != "Match newsletters from any sender." {
		t.Errorf("accumulated Text = %q, want %q", gotText, "Match newsletters from any sender.")
	}
	if gotReasoning != "Let me think about this rule. Yes, that covers it." {
		t.Errorf("accumulated Reasoning = %q, want the two reasoning deltas concatenated, got %q", gotReasoning, gotReasoning)
	}
	if suggestion != "Match newsletters from any sender." {
		t.Errorf("suggestion = %q, want the answer text only (no reasoning)", suggestion)
	}
	for _, m := range conv {
		if strings.Contains(m.Content, "think about this rule") {
			t.Errorf("returned conversation leaked reasoning text: %+v", conv)
		}
	}
}

// ---- streamGenerate: drainConverseStream's other caller ----
//
// Unlike ImprovePromptInstructions, streamGenerate had no test coverage before
// drainConverseStream was factored out to share the ConverseStream event loop between the
// two — StreamGeneratePromptInstruction is only exercised through FakeClient (a hand-rolled
// test double) elsewhere in this codebase, never through the real streamGenerate. These two
// guard the behavior drainConverseStream must preserve for this call site: text/reasoning
// deltas routed onto the channel, and a MaxTokens stop reported as truncation.

func TestStreamGenerate_StreamsTextAndReasoning(t *testing.T) {
	fake := &fakeConverseAPI{streamEvents: [][]types.ConverseStreamOutput{{
		reasoningDeltaEvent("thinking about the rule. "),
		textDeltaEvent("Match newsletters "),
		textDeltaEvent("from any sender."),
		stopEvent(types.StopReasonEndTurn),
	}}}
	c := &Client{br: fake, defaultModel: "us.amazon.nova-micro-v1:0"}
	ch := make(chan StreamChunk, 16)

	if err := c.streamGenerate(context.Background(), "newsletters", ch); err != nil {
		t.Fatalf("streamGenerate error: %v", err)
	}
	close(ch)

	var textBuf, reasoningBuf strings.Builder
	for chunk := range ch {
		textBuf.WriteString(chunk.Text)
		reasoningBuf.WriteString(chunk.Reasoning)
	}
	if got := textBuf.String(); got != "Match newsletters from any sender." {
		t.Errorf("accumulated text = %q, want %q", got, "Match newsletters from any sender.")
	}
	if got := reasoningBuf.String(); got != "thinking about the rule. " {
		t.Errorf("accumulated reasoning = %q, want %q", got, "thinking about the rule. ")
	}
}

func TestStreamGenerate_MaxTokensReturnsTruncationError(t *testing.T) {
	fake := &fakeConverseAPI{streamEvents: [][]types.ConverseStreamOutput{{
		textDeltaEvent("Match newsl"),
		stopEvent(types.StopReasonMaxTokens),
	}}}
	c := &Client{br: fake, defaultModel: "us.amazon.nova-micro-v1:0"}
	ch := make(chan StreamChunk, 16)

	err := c.streamGenerate(context.Background(), "newsletters", ch)
	if err == nil || !strings.Contains(err.Error(), "truncated at max_tokens") {
		t.Fatalf("err = %v, want it to mention truncation at max_tokens", err)
	}
}

// TestReplayAgainstExamples_UsesClassifyModelNotImproveModel is the load-bearing test for
// ReplayAgainstExamples: the whole point of replay is to answer "will the model that
// actually labels production mail apply this rule correctly?", which only holds if it's
// scored on classify_model. It's easy to get backwards because every other Bedrock call in
// the improve flow resolves improve_model, and this function is invoked from deep inside
// that same flow. classify_model and improve_model are deliberately set to different,
// distinctively-named ids here so a regression that resolves the wrong one fails loudly.
func TestReplayAgainstExamples_UsesClassifyModelNotImproveModel(t *testing.T) {
	settings := &multiSettings{vals: map[string]string{
		SettingClassifyModel: "classify-model-x",
		SettingImproveModel:  "improve-model-y",
	}}
	fake := &fakeConverseAPI{outputs: []*bedrockruntime.ConverseOutput{
		textOutput(`{"1": true}`),
		textOutput(`{"1": false}`),
		textOutput(`{"1": true}`),
	}}
	cl := &Client{br: fake, defaultModel: "fallback-model", settings: settings}

	examples := []ReplayExample{
		{Verdict: "false_negative", Sender: "a@example.com", Subject: "s1", Excerpt: "e1", Want: true},
		{Verdict: "false_positive", Sender: "b@example.com", Subject: "s2", Excerpt: "e2", Want: false},
		{Verdict: "confirmed_positive", Sender: "c@example.com", Subject: "s3", Excerpt: "e3", Want: true},
	}
	// concurrency: 1 keeps call order deterministic for the per-call ModelId assertion below.
	res := cl.ReplayAgainstExamples(context.Background(), db.NewFake(), "candidate rule text", examples, 1)

	if len(fake.calls) != 3 {
		t.Fatalf("expected 3 Converse calls, got %d", len(fake.calls))
	}
	for i, call := range fake.calls {
		if call.ModelId == nil || *call.ModelId != "classify-model-x" {
			t.Errorf("call %d: ModelId = %v, want %q (classify_model) — must never resolve improve_model", i, call.ModelId, "classify-model-x")
		}
	}
	if res.Model != "classify-model-x" {
		t.Errorf("ReplayResult.Model = %q, want %q", res.Model, "classify-model-x")
	}
	if res.Total != 3 || res.Passed != 3 {
		t.Errorf("Total/Passed = %d/%d, want 3/3 (all three examples scored correctly)", res.Total, res.Passed)
	}
	if len(res.Failures) != 0 {
		t.Errorf("Failures = %+v, want none", res.Failures)
	}
}

// TestReplayAgainstExamples_ScoresMismatchesAsFailures checks the pass/fail bookkeeping
// itself, independent of the model-selection concern above: a candidate that gets an
// example wrong should show up in Failures with what it actually returned.
func TestReplayAgainstExamples_ScoresMismatchesAsFailures(t *testing.T) {
	fake := &fakeConverseAPI{outputs: []*bedrockruntime.ConverseOutput{
		textOutput(`{"1": false}`), // wanted true (false_negative) -> mismatch
		textOutput(`{"1": false}`), // wanted false (false_positive) -> match
	}}
	cl := &Client{br: fake, defaultModel: "m"}

	examples := []ReplayExample{
		{Verdict: "false_negative", Sender: "a@example.com", Subject: "s1", Excerpt: "e1", Want: true},
		{Verdict: "false_positive", Sender: "b@example.com", Subject: "s2", Excerpt: "e2", Want: false},
	}
	res := cl.ReplayAgainstExamples(context.Background(), db.NewFake(), "candidate", examples, 1)

	if res.Total != 2 || res.Passed != 1 {
		t.Fatalf("Total/Passed = %d/%d, want 2/1", res.Total, res.Passed)
	}
	if len(res.Failures) != 1 || res.Failures[0].Verdict != "false_negative" || res.Failures[0].Got != false {
		t.Errorf("Failures = %+v, want one false_negative failure with Got=false", res.Failures)
	}
}

// TestReplayAgainstExamples_ClassifyErrorExcludedNotFailed checks that a Bedrock error for
// one example is excluded from Total rather than counted as a failure — a transient
// classify error isn't a signal about the candidate rule's quality.
func TestReplayAgainstExamples_ClassifyErrorExcludedNotFailed(t *testing.T) {
	fake := &fakeConverseAPI{
		outputs: []*bedrockruntime.ConverseOutput{nil, textOutput(`{"1": true}`)},
		errs:    []error{errors.New("boom"), nil},
	}
	cl := &Client{br: fake, defaultModel: "m"}

	examples := []ReplayExample{
		{Verdict: "false_negative", Sender: "a@example.com", Subject: "s1", Excerpt: "e1", Want: true},
		{Verdict: "confirmed_positive", Sender: "b@example.com", Subject: "s2", Excerpt: "e2", Want: true},
	}
	res := cl.ReplayAgainstExamples(context.Background(), db.NewFake(), "candidate", examples, 1)

	if res.Total != 1 || res.Passed != 1 {
		t.Errorf("Total/Passed = %d/%d, want 1/1 (errored example excluded, not failed)", res.Total, res.Passed)
	}
	if len(res.Failures) != 0 {
		t.Errorf("Failures = %+v, want none", res.Failures)
	}
}

// trackingConverseAPI is a thread-safe fakeConverseAPI variant for tests that fan out
// concurrent Converse calls — the plain fakeConverseAPI's unprotected slice append races
// under concurrent callers. Tracks how many calls were ever in flight at once, which is
// the observable signature of a semaphore actually limiting (or not limiting) concurrency.
type trackingConverseAPI struct {
	mu          sync.Mutex
	calls       int
	inFlight    int
	maxInFlight int
}

func (f *trackingConverseAPI) Converse(_ context.Context, _ *bedrockruntime.ConverseInput, _ ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error) {
	f.mu.Lock()
	f.calls++
	f.inFlight++
	if f.inFlight > f.maxInFlight {
		f.maxInFlight = f.inFlight
	}
	f.mu.Unlock()

	time.Sleep(10 * time.Millisecond) // hold the "in flight" window open long enough to overlap

	f.mu.Lock()
	f.inFlight--
	f.mu.Unlock()

	return textOutput(`{"1": true}`), nil
}

func (f *trackingConverseAPI) ConverseStream(_ context.Context, _ *bedrockruntime.ConverseStreamInput, _ ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseStreamEventStream, error) {
	return nil, errors.New("trackingConverseAPI: ConverseStream not implemented")
}

// TestReplayAgainstExamples_ConcurrencyZeroIsUnbounded locks in concurrency<=0's meaning:
// every example is classified at once, no semaphore — the MODE=improve worker (improve.go)
// always passes 0 now that replay runs inside its own Lambda invocation instead of a
// goroutine sharing WebFunction's request budget. Asserts the fan-out actually overlaps
// (maxInFlight > 1) as the behavioral difference from concurrency: 1, which — per
// TestReplayAgainstExamples_UsesClassifyModelNotImproveModel and the other replay tests
// above, all of which pass 1 for deterministic call ordering — must still serialize.
func TestReplayAgainstExamples_ConcurrencyZeroIsUnbounded(t *testing.T) {
	examples := make([]ReplayExample, 8)
	for i := range examples {
		examples[i] = ReplayExample{Verdict: "confirmed_positive", Sender: fmt.Sprintf("s%d@example.com", i), Subject: "x", Excerpt: "y", Want: true}
	}

	t.Run("concurrency 0 overlaps", func(t *testing.T) {
		fake := &trackingConverseAPI{}
		cl := &Client{br: fake, defaultModel: "m"}
		res := cl.ReplayAgainstExamples(context.Background(), db.NewFake(), "candidate", examples, 0)
		if res.Total != 8 || res.Passed != 8 {
			t.Errorf("Total/Passed = %d/%d, want 8/8", res.Total, res.Passed)
		}
		if fake.calls != 8 {
			t.Errorf("calls = %d, want 8", fake.calls)
		}
		if fake.maxInFlight <= 1 {
			t.Errorf("maxInFlight = %d, want >1 — concurrency<=0 should fan out unbounded, not serialize", fake.maxInFlight)
		}
	})

	t.Run("concurrency 1 serializes", func(t *testing.T) {
		fake := &trackingConverseAPI{}
		cl := &Client{br: fake, defaultModel: "m"}
		res := cl.ReplayAgainstExamples(context.Background(), db.NewFake(), "candidate", examples, 1)
		if res.Total != 8 || res.Passed != 8 {
			t.Errorf("Total/Passed = %d/%d, want 8/8", res.Total, res.Passed)
		}
		if fake.maxInFlight != 1 {
			t.Errorf("maxInFlight = %d, want exactly 1 for concurrency: 1", fake.maxInFlight)
		}
	})
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

// ---- ImprovePromptInstructions: stall guard ----
//
// improveStallTimeout used to bound the call's *total* time, which killed a real
// suggestion 1s after its 54th consecutive reasoning delta — the model was actively
// thinking the whole time, never stalled (see the trace: 54 "thinking" events, 0 "answer",
// then "context deadline exceeded" at exactly 120.163s). These guard the fix: the timeout
// now measures the gap since the last delta, reset on every one, so a call that keeps
// producing output — however slowly in aggregate — is never killed.

func testImproveRequest() ImproveRequest {
	return ImproveRequest{
		PromptName:           "newsletter",
		LabelName:            "News",
		OriginalInstructions: "matches newsletters",
		ShouldMatch:          []ExampleRef{{Sender: "a@example.com", Subject: "hello", Excerpt: "world"}},
	}
}

// TestImprovePromptInstructions_StallGuardResetsOnEveryDelta is the actual regression: a
// steady trickle of deltas, each arriving well inside the budget but summing to far more
// than it, must not be killed — the budget bounds silence, not total call time.
func TestImprovePromptInstructions_StallGuardResetsOnEveryDelta(t *testing.T) {
	budget := 50 * time.Millisecond
	delay := 10 * time.Millisecond // 20 deltas * 10ms = 200ms total, 4x the budget

	events := make([]types.ConverseStreamOutput, 0, 22)
	for range 20 {
		events = append(events, reasoningDeltaEvent("thinking... "))
	}
	events = append(events, textDeltaEvent("Match newsletters."), stopEvent(types.StopReasonEndTurn))

	fake := &fakeConverseAPI{streamReaders: []func(context.Context) bedrockruntime.ConverseStreamOutputReader{
		delayedStreamReader(events, delay),
	}}
	cl := &Client{br: fake, defaultModel: "zai.glm-5", improveStall: budget}

	suggestion, _, err := cl.ImprovePromptInstructions(context.Background(), testImproveRequest(), nil)
	if err != nil {
		t.Fatalf("ImprovePromptInstructions error: %v, want the steady trickle of deltas to keep resetting the guard", err)
	}
	if suggestion != "Match newsletters." {
		t.Errorf("suggestion = %q, want %q", suggestion, "Match newsletters.")
	}
}

// TestImprovePromptInstructions_StallGuardFiresOnGenuineSilence is the guard's other half:
// a call that produces nothing at all must still be killed, and the error must identify
// the stall rather than surface as a bare context.Canceled.
func TestImprovePromptInstructions_StallGuardFiresOnGenuineSilence(t *testing.T) {
	budget := 20 * time.Millisecond
	fake := &fakeConverseAPI{streamReaders: []func(context.Context) bedrockruntime.ConverseStreamOutputReader{
		silentStreamReader(),
	}}
	cl := &Client{br: fake, defaultModel: "zai.glm-5", improveStall: budget}

	start := time.Now()
	_, _, err := cl.ImprovePromptInstructions(context.Background(), testImproveRequest(), nil)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("ImprovePromptInstructions took %v to fail, want well under a second for a %v budget", elapsed, budget)
	}
	if !errors.Is(err, errImproveStalled) {
		t.Errorf("err = %v, want it to wrap errImproveStalled", err)
	}
}

// TestImprovePromptInstructions_ParentCancellationNotMaskedAsStall checks the guard
// doesn't swallow a real caller-driven cancellation (e.g. the improve worker's own
// deadline margin, improve.go's improveWorkerMargin) and misreport it as a stall — the two
// need to stay distinguishable so a genuinely stalled call and a legitimately expired
// round don't look the same in the logs or the suggestion's stored failure message.
func TestImprovePromptInstructions_ParentCancellationNotMaskedAsStall(t *testing.T) {
	budget := 5 * time.Second // long enough that only the parent cancellation below fires
	fake := &fakeConverseAPI{streamReaders: []func(context.Context) bedrockruntime.ConverseStreamOutputReader{
		silentStreamReader(),
	}}
	cl := &Client{br: fake, defaultModel: "zai.glm-5", improveStall: budget}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	_, _, err := cl.ImprovePromptInstructions(ctx, testImproveRequest(), nil)
	if errors.Is(err, errImproveStalled) {
		t.Errorf("err = %v, want the parent's cancellation, not errImproveStalled", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap context.Canceled", err)
	}
}

// TestImprovePromptInstructions_RetryGetsItsOwnFreshGuard checks the stall guard is
// per-attempt, not shared across the ValidationException retry (stream(nil) below): the
// first attempt's guard must not still be armed (or already fired) against the retry's
// own stream.
func TestImprovePromptInstructions_RetryGetsItsOwnFreshGuard(t *testing.T) {
	budget := 30 * time.Millisecond
	settings := &fixedSettings{key: SettingImproveReasoningEffort, val: ReasoningEffortOn}
	fake := &fakeConverseAPI{
		// First call is rejected before streaming starts (a real ValidationException
		// arrives this way) — instant, well inside budget.
		streamErrs: []error{&types.ValidationException{Message: aws.String("unknown field reasoning_config")}},
		// The retry's own stream trickles deltas slower than a total-120s-style cap could
		// tolerate in aggregate, but each gap is inside budget — only passes if the retry
		// got a fresh, unfired guard rather than reusing (or inheriting the state of) the
		// first attempt's.
		streamReaders: []func(context.Context) bedrockruntime.ConverseStreamOutputReader{
			nil, // first call uses streamErrs above instead
			delayedStreamReader([]types.ConverseStreamOutput{
				reasoningDeltaEvent("thinking... "), reasoningDeltaEvent("thinking more... "),
				textDeltaEvent("Match newsletters."), stopEvent(types.StopReasonEndTurn),
			}, budget/2),
		},
	}
	cl := &Client{br: fake, defaultModel: "zai.glm-5", settings: settings, improveStall: budget}

	suggestion, _, err := cl.ImprovePromptInstructions(context.Background(), testImproveRequest(), nil)
	if err != nil {
		t.Fatalf("ImprovePromptInstructions error: %v, want the retry (with its own fresh guard) to succeed", err)
	}
	if len(fake.streamCalls) != 2 {
		t.Fatalf("expected 2 ConverseStream calls (initial + retry), got %d", len(fake.streamCalls))
	}
	if suggestion != "Match newsletters." {
		t.Errorf("suggestion = %q, want %q", suggestion, "Match newsletters.")
	}
}
