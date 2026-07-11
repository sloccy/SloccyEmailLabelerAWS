package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrock"
	bedrocktypes "github.com/aws/aws-sdk-go-v2/service/bedrock/types"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/aws/aws-sdk-go-v2/service/pricing"
)

// bedrockHTTPTimeout is the HTTP client read-timeout for Bedrock calls. Flex-tier requests
// are queued at lower priority and can take minutes to return (vs. the near-instant default
// tier), so this needs to be generous — set just under the Lambda's own hard timeout (900s)
// so a killed request surfaces as a clean Lambda timeout rather than a silent hang. Everything
// after the Converse call returns (parsing, logging, DynamoDB writes) runs in well under a
// second, so only a 30s margin is reserved for it rather than a full minute.
const bedrockHTTPTimeout = 14*time.Minute + 30*time.Second

// LogLevelTimeout is the db.Log level written when a Converse call is aborted by
// bedrockHTTPTimeout, so the dashboard can count these over a rolling window.
const LogLevelTimeout = "TIMEOUT"

// isBedrockTimeout reports whether err stems from the client-side bedrockHTTPTimeout
// (a queued flex-tier request that never returned) rather than some other failure
// (bad request, throttling, etc).
func isBedrockTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var te interface{ Timeout() bool }
	if errors.As(err, &te) {
		return te.Timeout()
	}
	return false
}

// Settings is the minimal interface the Client needs to look up per-call model
// overrides. *db.Store and *db.FakeStore both satisfy this.
type Settings interface {
	GetSetting(ctx context.Context, key string) (string, error)
}

// Setting keys for the two independent model selections.
const (
	SettingClassifyModel = "classify_model"
	SettingImproveModel  = "improve_model"
	// SettingClassifyTier holds the Bedrock service tier ("standard" or "flex") used for
	// classification requests. Flex trades latency for lower cost; see resolveClassifyTier.
	SettingClassifyTier = "classify_tier"
	// SettingClassifyReasoningDirective holds an optional override for the
	// reasoning-suppression system-prompt switch reasoningOff would otherwise pick from
	// reasoningRegistry (reasoning.go) based on the classify model's id. Only needed for
	// a model family the registry doesn't recognize yet; empty (the default) means "use
	// the registry, or no-op if the model isn't in it."
	SettingClassifyReasoningDirective = "classify_reasoning_directive"
)

// Values for SettingClassifyTier.
const (
	ClassifyTierStandard = "standard"
	ClassifyTierFlex     = "flex"
)

// ModelOption is one entry in the model-selection dropdown.
type ModelOption struct {
	ID             string  // value sent to Bedrock (modelId or inferenceProfileId)
	Label          string  // human-readable display name
	InputCostPer1M float64 // standard on-demand input price per 1M tokens; CostUnknown if unpriced
	FlexCostPer1M  float64 // flex-tier input price per 1M tokens; CostUnknown if unpriced or not flex-capable
	// ProfileRegion is the cross-region inference-profile geography ("us", "global", "eu",
	// "apac", "us-gov"), or "" for a bare/single-datacenter foundation-model id.
	ProfileRegion string
	Flex          bool // true when the AWS Price List API reports a flex-tier SKU
}

// converseAPI is the subset of *bedrockruntime.Client used for chat calls. Narrowing to
// an interface lets tests substitute a fake without a network round-trip.
type converseAPI interface {
	Converse(ctx context.Context, params *bedrockruntime.ConverseInput, optFns ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error)
	ConverseStream(ctx context.Context, params *bedrockruntime.ConverseStreamInput, optFns ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseStreamOutput, error)
}

// Client wraps the Bedrock runtime client with per-call model resolution.
type Client struct {
	br converseAPI

	// awsCfg backs the lazily-built control-plane/pricing clients below. Only
	// ListAvailableModels (the settings-UI model dropdown) ever touches them — scan/push
	// cold starts never call it, so building bc/pc eagerly in NewClient would be pure
	// waste on those paths.
	awsCfg aws.Config
	bcOnce sync.Once
	bc     *bedrock.Client // control-plane, for listing
	pcOnce sync.Once
	pc     *pricing.Client // AWS Price List API, for dynamic pricing + flex eligibility

	defaultModel string
	settings     Settings
}

// DefaultModel is the fallback Bedrock model id used only when nothing else specifies
// one (no classify_model setting and no BEDROCK_MODEL env var) — e.g. on a fresh
// install before Settings has been configured. The model actually used for
// classification at runtime is whatever's configured there; nothing elsewhere in this
// package should assume this specific model's behavior (see reasoningRegistry in
// reasoning.go for why that assumption bit us before).
const DefaultModel = "us.amazon.nova-micro-v1:0"

// newBedrockRetryer builds the aws.Retryer used by NewClient: adaptive retry + client-side
// rate limiting (classification is fanned out across goroutines — see
// processor.ProcessConfig.ClassifyConcurrency — so concurrent requests are more likely to
// hit on-demand throttling; back off instead of failing fast), plus retries for
// signature/clock-skew errors. A Lambda execution environment frozen and thawed between
// invocations can briefly sign a request with a stale wall clock, which Bedrock rejects as
// InvalidSignatureException even though the request itself was fine — the SDK doesn't treat
// that code as retryable by default, so it's added explicitly here. A retry a moment later
// re-signs with the (by then re-synced) clock and succeeds. Factored out of NewClient so
// tests can exercise the retry classification without a network round-trip.
func newBedrockRetryer() aws.Retryer {
	return retry.AddWithErrorCodes(
		retry.NewAdaptiveMode(func(o *retry.AdaptiveModeOptions) {
			o.StandardOptions = append(o.StandardOptions, func(so *retry.StandardOptions) {
				so.MaxAttempts = 5
			})
		}),
		"InvalidSignatureException",
		"RequestExpired",
		"RequestTimeTooSkewed",
		"SignatureDoesNotMatch",
	)
}

// NewClient creates a Bedrock client.
// settings provides per-call model lookups (classify_model / improve_model keys).
// defaultModel is the fallback when neither a setting nor BEDROCK_MODEL is set.
func NewClient(settings Settings, defaultModel string) *Client {
	if defaultModel == "" {
		defaultModel = os.Getenv("BEDROCK_MODEL")
	}
	if defaultModel == "" {
		defaultModel = DefaultModel
	}
	cfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		// Generous read-timeout so the HTTP layer doesn't abort a queued flex-tier request
		// before the Lambda's own timeout does.
		awsconfig.WithHTTPClient(awshttp.NewBuildableClient().WithTimeout(bedrockHTTPTimeout)),
		// Adaptive retry + client-side rate limiting, plus clock-skew signature errors;
		// see newBedrockRetryer.
		awsconfig.WithRetryer(newBedrockRetryer),
	)
	if err != nil {
		panic(fmt.Sprintf("bedrock: load aws config: %v", err))
	}
	return &Client{
		br:           bedrockruntime.NewFromConfig(cfg),
		awsCfg:       cfg,
		defaultModel: defaultModel,
		settings:     settings,
	}
}

// controlPlane lazily builds the Bedrock control-plane client (ListFoundationModels/
// ListInferenceProfiles), built on first use rather than in NewClient.
func (c *Client) controlPlane() *bedrock.Client {
	c.bcOnce.Do(func() { c.bc = bedrock.NewFromConfig(c.awsCfg) })
	return c.bc
}

// pricingClient lazily builds the AWS Price List API client, built on first use rather
// than in NewClient. The Price List API only runs in us-east-1 (and ap-south-1); pin it
// there regardless of c.awsCfg.Region — it's a read-only pricing catalog, not LLM traffic.
func (c *Client) pricingClient() *pricing.Client {
	c.pcOnce.Do(func() {
		c.pc = pricing.NewFromConfig(c.awsCfg, func(o *pricing.Options) { o.Region = pricingRegion })
	})
	return c.pc
}

// resolveSetting looks up key in the store; falls back to def when unset, empty, or the
// store has no value.
func (c *Client) resolveSetting(ctx context.Context, key, def string) string {
	if c.settings != nil {
		if v, err := c.settings.GetSetting(ctx, key); err == nil && v != "" {
			return v
		}
	}
	return def
}

// resolveModel looks up the setting key in the store; falls back to defaultModel.
func (c *Client) resolveModel(ctx context.Context, key string) string {
	return c.resolveSetting(ctx, key, c.defaultModel)
}

// resolveClassifyTier looks up the classification service tier; defaults to "standard".
func (c *Client) resolveClassifyTier(ctx context.Context) string {
	return c.resolveSetting(ctx, SettingClassifyTier, ClassifyTierStandard)
}

// ResolveClassifySettings resolves the classify model, service tier, and reasoning-
// directive override once. Callers classifying many emails in one pass (e.g. a scan)
// should call this once up front and pass the result into each ClassifyEmailBatch call,
// instead of re-resolving (a GetSetting DynamoDB read) per email.
func (c *Client) ResolveClassifySettings(ctx context.Context) (model, tier, reasoningOverride string) {
	return c.resolveModel(ctx, SettingClassifyModel), c.resolveClassifyTier(ctx), c.resolveSetting(ctx, SettingClassifyReasoningDirective, "")
}

// ============================================================
// Public types (preserved from the pre-Bedrock LLM client for caller compatibility)
// ============================================================

type Email struct {
	Sender  string
	Subject string
	Body    string
	Snippet string
}

type Prompt struct {
	ID           int64
	Name         string
	Instructions string
}

type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

type ClassifyResult struct {
	Results     map[int64]bool
	RequestJSON string
	RawResponse string
	LatencyMs   int64 // wall time of the Bedrock Converse call, in milliseconds

	// Token usage from the Converse call's Usage block, populated on a best-effort basis
	// (Bedrock's Usage field is optional in the SDK response) so ad-hoc token-cost
	// investigations don't need a debug build. See ClassifyEmailBatch and logUsage.
	InputTokens  int32
	OutputTokens int32
	TotalTokens  int32

	// ReasoningDetected is true if the model produced chain-of-thought/reasoning
	// content despite the reasoning-suppression directive (see reasoningOff in
	// reasoning.go) — the signal that suppression is or isn't actually working, since
	// no Bedrock API reports this directly.
	ReasoningDetected bool
}

type StreamChunk struct {
	Text string
	Err  error
}

type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ImproveRequest struct {
	PromptName           string
	LabelName            string
	OriginalInstructions string
	TriggerKind          string
	EmailSubject         string
	EmailSender          string
	EmailBody            string
	PriorConversation    []ChatMessage
	UserComment          string
}

// ============================================================
// Classification
// ============================================================

func buildBody(email Email, prompts []Prompt) string {
	var sb strings.Builder
	for i, p := range prompts {
		fmt.Fprintf(&sb, "%d. %s: %s\n", i+1, p.Name, p.Instructions)
	}
	rulesText := sb.String()

	// The example covers every rule number, not just the first couple — this prompt is
	// the model's only signal for the expected output shape (Converse tool-use isn't
	// attempted; see reasoning.go for why several model families this project has run
	// don't support it there), so a partial example would leave it guessing whether to
	// include keys for the rules it omitted.
	exampleParts := make([]string, len(prompts))
	for i := range exampleParts {
		exampleParts[i] = fmt.Sprintf(`"%d": false`, i+1)
	}

	body := email.Body
	if body == "" {
		body = email.Snippet
	}
	body = strings.ReplaceAll(body, "\r", "")

	return fmt.Sprintf(`You are an email classification assistant. For each rule below, decide if the label should be applied to the email that follows.

Rules:
%s
Respond with a single JSON object and nothing else. Include exactly one boolean key per rule number listed above, even when the answer is false — do not omit any rule.
Example (with a rule for every number, matching the count above): {%s}
Output ONLY that JSON object. Do not include any explanation, reasoning, preamble, "<think>" block, or markdown code fences before or after it.

Email:
From: %s
Subject: %s
Body:
%s`,
		rulesText, strings.Join(exampleParts, ", "),
		email.Sender, email.Subject, body)
}

// jsonTypeKey is the JSON field name "type", used by the debug request preview's
// serviceTier payload (see ClassifyEmailBatch).
const jsonTypeKey = "type"

// mapKeysToResults converts a {"1": true, "2": false, ...} map (1-based rule index →
// verdict) into the prompt-ID-keyed result map ClassifyEmailBatch returns. Shared by
// both the tool-use and text-fallback decode paths.
func mapKeysToResults(parsed map[string]any, prompts []Prompt) map[int64]bool {
	results := make(map[int64]bool, len(prompts))
	for k, v := range parsed {
		var idx int
		if _, err := fmt.Sscanf(k, "%d", &idx); err != nil {
			continue
		}
		idx-- // 1-based → 0-based
		if idx >= 0 && idx < len(prompts) {
			b, _ := v.(bool)
			results[prompts[idx].ID] = b
		}
	}
	return results
}

// extractJSONObject scans s for the first top-level {...} span that is itself valid
// JSON, and returns it, or "" if none is found. Used to pull classification JSON out of
// a text-fallback response (see ClassifyEmailBatch) that may be wrapped in prose,
// markdown fences, or a reasoning model's "<think>...</think>" preamble — anything
// outside the braces is simply ignored rather than needing to be pattern-matched and
// stripped. A reasoning model's prose can itself contain unquoted "{...}" asides (e.g.
// "seems {like a} match") that balance but aren't valid JSON; those are skipped in
// favor of the next candidate rather than returned as a false match.
func extractJSONObject(s string) string {
	for start := strings.IndexByte(s, '{'); start >= 0; {
		end, ok := balancedBraceEnd(s, start)
		if !ok {
			return ""
		}
		candidate := s[start : end+1]
		if json.Valid([]byte(candidate)) {
			return candidate
		}
		next := strings.IndexByte(s[end+1:], '{')
		if next < 0 {
			return ""
		}
		start = end + 1 + next
	}
	return ""
}

// balancedBraceEnd returns the index of the '}' that closes the '{' at s[start],
// respecting quoted strings and backslash-escaped characters within them so braces
// inside string values don't affect nesting depth. ok is false if the braces never
// balance before the end of s.
func balancedBraceEnd(s string, start int) (end int, ok bool) {
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i, true
			}
		}
	}
	return 0, false
}

// logUsage records the Converse call's token usage onto res and logs it. Usage is
// populated best-effort: the SDK response's Usage field isn't guaranteed non-nil, so a
// nil usage is skipped quietly rather than logged as misleading zeros.
func logUsage(store StoreLogger, res *ClassifyResult, usage *types.TokenUsage, tierLabel string) {
	if usage == nil {
		return
	}
	res.InputTokens = aws.ToInt32(usage.InputTokens)
	res.OutputTokens = aws.ToInt32(usage.OutputTokens)
	res.TotalTokens = aws.ToInt32(usage.TotalTokens)
	store.Log("INFO", fmt.Sprintf("LLM classify tokens: input=%d output=%d total=%d (tier: %s)",
		res.InputTokens, res.OutputTokens, res.TotalTokens, tierLabel))
}

// classifyPayload returns the request pieces for a classify Converse call: the user
// message, inference config, and — when modelID's family is in reasoningRegistry, or
// reasoningOverride is set — the system content block and/or additional model request
// fields that suppress that model's chain-of-thought output (see reasoning.go).
func classifyPayload(email Email, prompts []Prompt, modelID, reasoningOverride string) ([]types.Message, *types.InferenceConfiguration, []types.SystemContentBlock, document.Interface) {
	body := buildBody(email, prompts)
	// Reasoning-capable models can emit chain-of-thought as ordinary output tokens even
	// when asked to suppress it (see reasoning.go) — some don't honor the suppression
	// directive on every call. MaxTokens is sized to absorb that rather than truncate a
	// response mid-stream; it's a ceiling, not a target, so it costs nothing when the
	// model finishes well under it.
	maxTokens := int32(3000)
	if n := len(prompts) * 12; n > 3000 {
		maxTokens = int32(min(n, math.MaxInt32)) //nolint:gosec // bounded to int32 range by min()
	}
	msgs := []types.Message{
		{
			Role:    types.ConversationRoleUser,
			Content: []types.ContentBlock{&types.ContentBlockMemberText{Value: body}},
		},
	}
	inf := &types.InferenceConfiguration{
		MaxTokens:   aws.Int32(maxTokens),
		Temperature: aws.Float32(0),
	}

	var sys []types.SystemContentBlock
	var fields document.Interface
	if d := reasoningOff(modelID, reasoningOverride); !d.isZero() {
		if d.system != "" {
			sys = []types.SystemContentBlock{&types.SystemContentBlockMemberText{Value: d.system}}
		}
		if d.fields != nil {
			fields = document.NewLazyDocument(d.fields)
		}
	}
	return msgs, inf, sys, fields
}

// ClassifyEmailBatch classifies one email against prompts using the given model and
// service tier. Callers classifying many emails in one pass should resolve
// model/tier/reasoningOverride once via ResolveClassifySettings and reuse them across
// calls. reasoningOverride overrides reasoningRegistry's suppression directive for a
// model the registry doesn't know about yet (see SettingClassifyReasoningDirective);
// pass "" to use the registry as-is. debug gates building the (comparatively expensive)
// serialized request JSON onto ClassifyResult.RequestJSON — it's only ever read by the
// Troubleshooting UI's debug write, so normal operation skips it.
func (c *Client) ClassifyEmailBatch(ctx context.Context, store StoreLogger, email Email, prompts []Prompt, model, tier, reasoningOverride string, debug bool) (ClassifyResult, error) {
	if len(prompts) == 0 {
		return ClassifyResult{}, nil
	}

	msgs, inf, sys, fields := classifyPayload(email, prompts, model, reasoningOverride)

	var svcTier *types.ServiceTier
	if tier == ClassifyTierFlex {
		svcTier = &types.ServiceTier{Type: types.ServiceTierTypeFlex}
	}

	var res ClassifyResult
	if debug {
		reqPayload := map[string]any{
			"modelId":         model,
			"messages":        msgs,
			"inferenceConfig": inf,
		}
		if len(sys) > 0 {
			if t, ok := sys[0].(*types.SystemContentBlockMemberText); ok {
				reqPayload["system"] = []map[string]string{{"text": t.Value}}
			}
		}
		if tier == ClassifyTierFlex {
			reqPayload["serviceTier"] = map[string]string{jsonTypeKey: ClassifyTierFlex}
		}
		reqJSON, marshalErr := json.Marshal(reqPayload)
		if marshalErr != nil {
			reqJSON = []byte("{}")
		}
		res.RequestJSON = string(reqJSON)
	}

	subject := email.Subject
	if len(subject) > 60 {
		subject = subject[:60]
	}
	tierLabel := tier
	if tierLabel == "" {
		tierLabel = ClassifyTierStandard
	}
	store.Log("INFO", fmt.Sprintf("LLM classifying '%s' against %d rule(s) (model: %s, tier: %s)", subject, len(prompts), model, tierLabel))

	// The Lambda invoking this has a hard 900s (15min) ceiling; bedrockHTTPTimeout aborts
	// a stuck Converse call ~1min before that so the error below can still be logged
	// rather than the whole invocation being silently killed.
	start := time.Now()
	out, err := c.br.Converse(ctx, &bedrockruntime.ConverseInput{
		ModelId:                      aws.String(model),
		Messages:                     msgs,
		System:                       sys,
		InferenceConfig:              inf,
		ServiceTier:                  svcTier,
		AdditionalModelRequestFields: fields,
	})
	res.LatencyMs = time.Since(start).Milliseconds()
	if err != nil {
		if isBedrockTimeout(err) {
			store.Log(LogLevelTimeout, fmt.Sprintf("Bedrock Converse call exceeded the %s client timeout (tier: %s): %v", bedrockHTTPTimeout, tierLabel, err))
		}
		store.Log("ERROR", fmt.Sprintf("LLM request failed after %dms (tier: %s): %v", res.LatencyMs, tierLabel, err))
		return res, &Error{Msg: fmt.Sprintf("LLM request failed: %v", err)}
	}
	logUsage(store, &res, out.Usage, tierLabel)

	raw := extractText(out.Output)
	res.ReasoningDetected = detectReasoning(out.Output, raw)
	store.Log("INFO", fmt.Sprintf("LLM classify reasoning: suppressed=%v", !res.ReasoningDetected))
	combined := fmt.Sprintf("LLM classify: %dms (tier: %s), content=%d chars", res.LatencyMs, tierLabel, len(raw))
	if len(raw) > 0 {
		preview := raw
		if len(preview) > 500 {
			preview = preview[:500]
		}
		combined += " | " + preview
	}
	store.Log("INFO", combined)
	res.RawResponse = raw

	cleaned := extractJSONObject(raw)
	if cleaned == "" {
		store.Log("ERROR", "LLM parse error: no JSON object found | raw: "+raw)
		return res, &Error{Msg: "LLM parse error: no JSON object found in response"}
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(cleaned), &parsed); err != nil {
		store.Log("ERROR", fmt.Sprintf("LLM parse error: %v | raw: %s", err, raw))
		return res, &Error{Msg: fmt.Sprintf("LLM parse error: %v", err)}
	}

	res.Results = mapKeysToResults(parsed, prompts)
	return res, nil
}

// ============================================================
// Streaming prompt generation
// ============================================================

func (c *Client) StreamGeneratePromptInstruction(ctx context.Context, description string) <-chan StreamChunk {
	ch := make(chan StreamChunk, 16)
	go func() {
		defer close(ch)
		if err := c.streamGenerate(ctx, description, ch); err != nil {
			select {
			case ch <- StreamChunk{Err: err}:
			case <-ctx.Done():
			}
		}
	}()
	return ch
}

func (c *Client) streamGenerate(ctx context.Context, description string, ch chan<- StreamChunk) error {
	model := c.resolveModel(ctx, SettingImproveModel)
	systemPrompt := "You write email filter rules for an AI classifier. Output only the rule text. No preamble, no drafts, no self-critique, no quotes, no explanation."
	userMsg := fmt.Sprintf(
		"Write a 2-4 sentence classifier instruction for emails matching: %q\n\n"+
			"The instruction must describe: what the email is about, its purpose/intent, "+
			"and what distinguishes it from similar-but-non-matching emails. "+
			"Do not use keywords or sender addresses as criteria — focus on meaning and context.\n\n"+
			"Output ONLY the instruction text.",
		description)

	out, err := c.br.ConverseStream(ctx, &bedrockruntime.ConverseStreamInput{
		ModelId: aws.String(model),
		System:  []types.SystemContentBlock{&types.SystemContentBlockMemberText{Value: systemPrompt}},
		Messages: []types.Message{
			{
				Role:    types.ConversationRoleUser,
				Content: []types.ContentBlock{&types.ContentBlockMemberText{Value: userMsg}},
			},
		},
		InferenceConfig: &types.InferenceConfiguration{
			MaxTokens:   aws.Int32(2048),
			Temperature: aws.Float32(0.7),
		},
	})
	if err != nil {
		return err
	}

	stream := out.GetStream()
	defer func() { _ = stream.Close() }()

	for event := range stream.Events() {
		if e, ok := event.(*types.ConverseStreamOutputMemberContentBlockDelta); ok {
			if d, ok := e.Value.Delta.(*types.ContentBlockDeltaMemberText); ok && d.Value != "" {
				select {
				case ch <- StreamChunk{Text: d.Value}:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}
	}
	return stream.Err()
}

// ============================================================
// Prompt improvement
// ============================================================

const improveSystemPrompt = `You are a careful editor of email-classification rules. You are given one existing rule (its name, target Gmail label, and current instructions) and one concrete email that the rule handled incorrectly. Your job is to rewrite the instructions so that the same rule would have handled this email correctly, without damaging its behavior on emails it currently classifies correctly.

CRITICAL OUTPUT REQUIREMENT: Your entire response must be ONLY the rewritten rule instructions — nothing else. No preamble, no explanation, no "Here is the updated rule:", no quoting of the email, no markdown formatting, no commentary. Think as long as you need internally, but the only thing you output is the new instructions text itself.

Rules for rewriting:
1. Preserve the rule's original intent. Do not widen scope beyond what the name and label imply. Do not turn a narrow rule into a catch-all.
2. Never use the specific sender address, subject line, or body phrases of the example email as matching criteria. The example is an illustration, not a fingerprint. Match on meaning, purpose, and context.
3. If trigger_kind is false_negative: explain what category of email was missed and add language that would match it. If trigger_kind is false_positive: add exclusions or clarify the scope so emails like this one are no longer matched.
4. Keep the output 2-6 sentences. Plain prose. No bullet lists, no code blocks, no markdown headings.
5. If the user comments on your suggestion, treat the comment as authoritative feedback and produce another revision that addresses it while still obeying rules 1-4.

Remember: output ONLY the rewritten instructions text. No other text whatsoever.`

func (c *Client) ImprovePromptInstructions(ctx context.Context, req ImproveRequest) (string, []ChatMessage, error) {
	model := c.resolveModel(ctx, SettingImproveModel)
	var msgs []types.Message

	// The first-turn prompt (no prior conversation) is used twice below — once as the
	// Bedrock message, once as the stored conversation entry — so it's built once here
	// rather than duplicating the format string and its 6 args at both call sites.
	firstTurnMsg := fmt.Sprintf(
		"RULE NAME: %s\nTARGET LABEL: %s\nTRIGGER: %s\n\nCURRENT INSTRUCTIONS:\n%s\n\nMISHANDLED EMAIL:\nFrom: %s\nSubject: %s\nBody:\n%s\n\nRewrite the instructions per the system rules.",
		req.PromptName, req.LabelName, req.TriggerKind,
		req.OriginalInstructions,
		req.EmailSender, req.EmailSubject, req.EmailBody,
	)

	if len(req.PriorConversation) > 0 {
		for _, m := range req.PriorConversation {
			role := types.ConversationRoleUser
			if m.Role == "assistant" {
				role = types.ConversationRoleAssistant
			}
			msgs = append(msgs, types.Message{
				Role:    role,
				Content: []types.ContentBlock{&types.ContentBlockMemberText{Value: m.Content}},
			})
		}
		msgs = append(msgs, types.Message{
			Role:    types.ConversationRoleUser,
			Content: []types.ContentBlock{&types.ContentBlockMemberText{Value: req.UserComment}},
		})
	} else {
		msgs = append(msgs, types.Message{
			Role:    types.ConversationRoleUser,
			Content: []types.ContentBlock{&types.ContentBlockMemberText{Value: firstTurnMsg}},
		})
	}

	out, err := c.br.Converse(ctx, &bedrockruntime.ConverseInput{
		ModelId:  aws.String(model),
		System:   []types.SystemContentBlock{&types.SystemContentBlockMemberText{Value: improveSystemPrompt}},
		Messages: msgs,
		InferenceConfig: &types.InferenceConfiguration{
			MaxTokens:   aws.Int32(16384),
			Temperature: aws.Float32(0.4),
		},
	})
	if err != nil {
		return "", nil, err
	}
	suggestion := strings.TrimSpace(extractText(out.Output))

	// Build updated conversation for storage
	var conv []ChatMessage
	conv = append(conv, req.PriorConversation...)
	if len(req.PriorConversation) > 0 {
		conv = append(conv, ChatMessage{Role: "user", Content: req.UserComment})
	} else {
		conv = append(conv, ChatMessage{Role: "user", Content: firstTurnMsg})
	}
	conv = append(conv, ChatMessage{Role: "assistant", Content: suggestion})

	return suggestion, conv, nil
}

// ============================================================
// Model listing (control-plane)
// ============================================================

// modelModality records whether a foundation model takes text in and emits text out.
type modelModality struct{ textIn, textOut bool }

// regionPrefixes are the cross-region inference-profile prefixes stripped to recover the
// underlying foundation-model id (e.g. "us.anthropic.claude-..." -> "anthropic.claude-...").
var regionPrefixes = []string{"us-gov.", "global.", "apac.", "us.", "eu."}

func baseModelID(id string) string {
	for _, p := range regionPrefixes {
		if strings.HasPrefix(id, p) {
			return id[len(p):]
		}
	}
	return id
}

// profileRegion returns the cross-region inference-profile geography embedded in id
// ("us", "global", "eu", "apac", "us-gov"), or "" for a bare foundation-model id (no
// profile — pinned to whichever single region/datacenter the caller's endpoint is in).
func profileRegion(id string) string {
	for _, p := range regionPrefixes {
		if strings.HasPrefix(id, p) {
			return strings.TrimSuffix(p, ".")
		}
	}
	return ""
}

// ListAvailableModels returns Bedrock models suitable for text classification: text-in,
// text-out, streaming, on-demand foundation models, unioned with all system-defined
// inference profiles whose underlying model is likewise text-in/text-out (any geography —
// "us.", "global.", "eu.", "apac.", "us-gov.", or whatever AWS adds next; see
// ModelOption.ProfileRegion). Image, embedding, and other non-text models (e.g.
// stability.*, *embed*) are excluded. ModelOption.Flex marks flex-tier eligibility per the
// AWS Price List API. Callers decide their own geo policy from ProfileRegion/Flex — e.g.
// the Settings UI restricts Standard to "" (bare/single-datacenter)/"us"/"global" but lets
// Flex use any geography, since flex-eligible families currently have no "us." profile at
// all (see dynamic-sources-only project memory). Sorted cheapest input cost first, unpriced
// last.
func (c *Client) ListAvailableModels(ctx context.Context) ([]ModelOption, error) {
	seen := make(map[string]bool)
	var opts []ModelOption

	// Pricing + flex-tier eligibility come entirely from the AWS Price List API — no
	// hardcoded model data. Non-fatal on error (e.g. missing pricing:GetProducts IAM
	// permission): the dropdown still lists models, just without prices/flex info.
	cat, err := fetchPricingCatalog(ctx, c.pricingClient())
	if err != nil {
		cat = &pricingCatalog{
			inputPricePer1M:     map[string]float64{},
			flexInputPricePer1M: map[string]float64{},
			flexCapable:         map[string]bool{},
		}
	}

	// One unfiltered catalog call: drives the foundation-model list AND supplies the
	// modality of the models that back each inference profile.
	fmOut, err := c.controlPlane().ListFoundationModels(ctx, &bedrock.ListFoundationModelsInput{})
	if err != nil {
		return nil, fmt.Errorf("list foundation models: %w", err)
	}
	type summary struct {
		id       string
		modality modelModality
	}
	var summaries []summary
	for _, m := range fmOut.ModelSummaries {
		id := aws.ToString(m.ModelId)
		if id == "" {
			continue
		}
		mod := modelModality{
			textIn:  slices.Contains(m.InputModalities, bedrocktypes.ModelModalityText),
			textOut: slices.Contains(m.OutputModalities, bedrocktypes.ModelModalityText),
		}
		summaries = append(summaries, summary{id: id, modality: mod})

		// Foundation-model dropdown entry: text-in/out, streaming, on-demand.
		if seen[id] || !mod.textIn || !mod.textOut {
			continue
		}
		if m.ResponseStreamingSupported == nil || !*m.ResponseStreamingSupported {
			continue
		}
		if !slices.Contains(m.InferenceTypesSupported, bedrocktypes.InferenceTypeOnDemand) {
			continue
		}
		label := aws.ToString(m.ModelName)
		if label == "" {
			label = id
		}
		seen[id] = true
		opts = append(opts, ModelOption{
			ID:             id,
			Label:          label,
			InputCostPer1M: cat.inputCostPer1M(id),
			FlexCostPer1M:  cat.flexCostPer1M(id),
			Flex:           cat.isFlexCapable(id),
		}) // ProfileRegion left as "" — bare foundation-model id, single datacenter
	}

	// modalityOf resolves a profile's underlying model modality. Foundation ids sometimes
	// carry a context suffix (e.g. "...-v1:0:8k") not present on the profile id, so match by
	// exact id or prefix. Unknown models are kept (conservative) rather than hidden.
	modalityOf := func(base string) (modelModality, bool) {
		for _, s := range summaries {
			if s.id == base || strings.HasPrefix(s.id, base) {
				return s.modality, true
			}
		}
		return modelModality{}, false
	}

	// System-defined inference profiles (cross-region; e.g. us.amazon.nova-micro-v1:0).
	// These are the IDs required for models that mandate an inference profile.
	ipOut, err := c.controlPlane().ListInferenceProfiles(ctx, &bedrock.ListInferenceProfilesInput{
		TypeEquals: bedrocktypes.InferenceProfileTypeSystemDefined,
	})
	if err != nil {
		// Non-fatal: some regions may not support this API yet.
		ipOut = &bedrock.ListInferenceProfilesOutput{}
	}
	for _, p := range ipOut.InferenceProfileSummaries {
		id := aws.ToString(p.InferenceProfileId)
		if id == "" || seen[id] {
			continue
		}
		// Drop image/embedding/other non-text profiles by checking the backing model.
		if mod, known := modalityOf(baseModelID(id)); known && (!mod.textIn || !mod.textOut) {
			continue
		}
		label := aws.ToString(p.InferenceProfileName)
		if label == "" {
			label = id
		}
		seen[id] = true
		base := baseModelID(id)
		opts = append(opts, ModelOption{
			ID:             id,
			Label:          label,
			InputCostPer1M: cat.inputCostPer1M(base),
			FlexCostPer1M:  cat.flexCostPer1M(base),
			ProfileRegion:  profileRegion(id),
			Flex:           cat.isFlexCapable(base),
		})
	}

	// Sort cheapest input cost first; unpriced (CostUnknown) sinks to the bottom,
	// tie-broken by label for a stable order.
	sort.Slice(opts, func(i, j int) bool {
		ci, cj := opts[i].InputCostPer1M, opts[j].InputCostPer1M
		iUnknown, jUnknown := ci < 0, cj < 0
		if iUnknown != jUnknown {
			return !iUnknown // known prices before unknown
		}
		if !iUnknown && ci != cj {
			return ci < cj
		}
		return opts[i].Label < opts[j].Label
	})
	return opts, nil
}

// ============================================================
// Internal helpers
// ============================================================

func extractText(output types.ConverseOutput) string {
	if output == nil {
		return ""
	}
	msg, ok := output.(*types.ConverseOutputMemberMessage)
	if !ok {
		return ""
	}
	var sb strings.Builder
	for _, block := range msg.Value.Content {
		if t, ok := block.(*types.ContentBlockMemberText); ok {
			sb.WriteString(t.Value)
		}
	}
	return sb.String()
}
