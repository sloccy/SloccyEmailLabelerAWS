package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
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
// so a killed request surfaces as a clean Lambda timeout rather than a silent hang.
const bedrockHTTPTimeout = 14 * time.Minute

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
	br           converseAPI
	bc           *bedrock.Client // control-plane, for listing
	pc           *pricing.Client // AWS Price List API, for dynamic pricing + flex eligibility
	defaultModel string
	settings     Settings
}

// DefaultModel is the Bedrock model id used when nothing else specifies one.
const DefaultModel = "us.amazon.nova-micro-v1:0"

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
		// Adaptive retry + client-side rate limiting: with classification now fanned out
		// across goroutines (see processor.ProcessConfig.ClassifyConcurrency), concurrent
		// requests are more likely to hit on-demand throttling, so back off instead of
		// failing fast.
		awsconfig.WithRetryMode(aws.RetryModeAdaptive),
		awsconfig.WithRetryMaxAttempts(5),
	)
	if err != nil {
		panic(fmt.Sprintf("bedrock: load aws config: %v", err))
	}
	return &Client{
		br: bedrockruntime.NewFromConfig(cfg),
		bc: bedrock.NewFromConfig(cfg),
		// The Price List API only runs in us-east-1 (and ap-south-1); pin it there
		// regardless of cfg.Region — it's a read-only pricing catalog, not LLM traffic.
		pc:           pricing.NewFromConfig(cfg, func(o *pricing.Options) { o.Region = pricingRegion }),
		defaultModel: defaultModel,
		settings:     settings,
	}
}

// Model returns the default model (used by tests and the troubleshooting UI).
func (c *Client) Model() string { return c.defaultModel }

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

// ResolveClassifySettings resolves the classify model and service tier once. Callers
// classifying many emails in one pass (e.g. a scan) should call this once up front and
// pass the result into each ClassifyEmailBatch call, instead of re-resolving (a
// GetSetting DynamoDB read) per email.
func (c *Client) ResolveClassifySettings(ctx context.Context) (model, tier string) {
	return c.resolveModel(ctx, SettingClassifyModel), c.resolveClassifyTier(ctx)
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

	// The example covers every rule number, not just the first couple — when a
	// classification falls back to plain-text parsing (see ClassifyEmailBatch), the
	// model only has this prompt to go on, and a partial example leaves it guessing
	// whether to include keys for the rules it omitted.
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

// classifyToolName is the forced tool that gets Bedrock to return classification
// results as schema-validated JSON instead of free text (see classifyToolConfig).
const classifyToolName = "record_labels"

// classifyToolDescription documents the forced tool to the model.
const classifyToolDescription = "Record true/false for each numbered classification rule."

// classifyToolSchema returns the JSON schema requiring a boolean for every rule
// number (1-based, matching buildBody's numbering). Shared by classifyToolConfig
// (the real SDK request) and classifyToolConfigJSON (the human-readable debug/preview
// request), so the two never drift.
// jsonTypeKey is the JSON Schema / Bedrock serviceTier "type" field name, factored out
// because it recurs across the schema, the wire-shaped preview, and serviceTier maps.
const jsonTypeKey = "type"

func classifyToolSchema(prompts []Prompt) map[string]any {
	properties := make(map[string]any, len(prompts))
	required := make([]string, len(prompts))
	for i := range prompts {
		key := strconv.Itoa(i + 1)
		properties[key] = map[string]any{jsonTypeKey: "boolean"}
		required[i] = key
	}
	return map[string]any{
		jsonTypeKey:            "object",
		"properties":           properties,
		"required":             required,
		"additionalProperties": false,
	}
}

// classifyToolConfig builds the forced-tool-use configuration for structured
// classification output: one tool whose input schema requires a boolean for every
// rule number. ToolChoice forces the model to call it — this is Bedrock's native
// structured-output mechanism, replacing the prompt-coaxed "respond with only JSON"
// text approach. If a model rejects the tool-use call outright, ClassifyEmailBatch
// falls back to text parsing.
func classifyToolConfig(prompts []Prompt) *types.ToolConfiguration {
	schema := classifyToolSchema(prompts)
	return &types.ToolConfiguration{
		Tools: []types.Tool{
			&types.ToolMemberToolSpec{
				Value: types.ToolSpecification{
					Name:        aws.String(classifyToolName),
					Description: aws.String(classifyToolDescription),
					InputSchema: &types.ToolInputSchemaMemberJson{Value: document.NewLazyDocument(schema)},
				},
			},
		},
		ToolChoice: &types.ToolChoiceMemberTool{Value: types.SpecificToolChoice{Name: aws.String(classifyToolName)}},
	}
}

// classifyToolConfigJSON returns the wire-shaped (Bedrock REST field names, not Go SDK
// field names) representation of classifyToolConfig for the request-preview/debug JSON.
// The real SDK ToolConfiguration can't be passed to encoding/json directly: its schema
// is wrapped in an opaque document.Interface with no MarshalJSON, so it would render as
// "{}" in the preview.
func classifyToolConfigJSON(prompts []Prompt) map[string]any {
	return map[string]any{
		"tools": []map[string]any{
			{
				"toolSpec": map[string]any{
					"name":        classifyToolName,
					"description": classifyToolDescription,
					"inputSchema": map[string]any{"json": classifyToolSchema(prompts)},
				},
			},
		},
		"toolChoice": map[string]any{"tool": map[string]any{"name": classifyToolName}},
	}
}

// extractToolUse pulls the forced tool call's input out of a Converse response. ok is
// false when the model didn't call the tool — the model may have ignored the forced
// ToolChoice, signalling ClassifyEmailBatch to fall back to text parsing.
func extractToolUse(output types.ConverseOutput, toolName string) (parsed map[string]any, rawJSON string, ok bool) {
	msg, isMsg := output.(*types.ConverseOutputMemberMessage)
	if !isMsg {
		return nil, "", false
	}
	for _, block := range msg.Value.Content {
		tu, isToolUse := block.(*types.ContentBlockMemberToolUse)
		if !isToolUse || aws.ToString(tu.Value.Name) != toolName || tu.Value.Input == nil {
			continue
		}
		var m map[string]any
		// SDK quirk: a document.Interface built via document.NewLazyDocument (the
		// send-side marshaler; used by tests to fake a response) populates m via the
		// same pointer indirection the real receive-side unmarshaler uses, but then
		// spuriously errors on internal double-processing of the already-populated
		// value. Real Bedrock responses decode via the receive-side unmarshaler and
		// never hit this, so len(m)==0 distinguishes a genuine decode failure from
		// this cosmetic error.
		if err := tu.Value.Input.UnmarshalSmithyDocument(&m); err != nil && len(m) == 0 {
			continue
		}
		b, err := json.Marshal(m)
		if err != nil {
			continue
		}
		return m, string(b), true
	}
	return nil, "", false
}

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

// classifyPayload returns the request pieces plus the service tier ("flex" or "" for
// standard) selected for classification.
func classifyPayload(email Email, prompts []Prompt) ([]types.Message, *types.InferenceConfiguration) {
	body := buildBody(email, prompts)
	// Floor covers the forced tool-use JSON payload (schema-validated, so no markdown/
	// prose overhead) plus the fallback's fenced free-text JSON for the same rule count.
	numPredict := int32(64)
	if n := len(prompts) * 12; n > 64 {
		numPredict = int32(min(n, math.MaxInt32)) //nolint:gosec // bounded to int32 range by min()
	}
	msgs := []types.Message{
		{
			Role:    types.ConversationRoleUser,
			Content: []types.ContentBlock{&types.ContentBlockMemberText{Value: body}},
		},
	}
	inf := &types.InferenceConfiguration{
		MaxTokens:   aws.Int32(numPredict),
		Temperature: aws.Float32(0),
	}
	return msgs, inf
}

// BuildClassifyRequestJSON returns the serialised Bedrock Converse payload (for Troubleshooting UI).
func (c *Client) BuildClassifyRequestJSON(email Email, prompts []Prompt) string {
	if len(prompts) == 0 {
		return ""
	}
	model, tier := c.ResolveClassifySettings(context.Background())
	msgs, inf := classifyPayload(email, prompts)
	payload := map[string]any{
		"modelId":         model,
		"messages":        msgs,
		"inferenceConfig": inf,
		"toolConfig":      classifyToolConfigJSON(prompts),
	}
	if tier == ClassifyTierFlex {
		payload["serviceTier"] = map[string]string{jsonTypeKey: ClassifyTierFlex}
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// ClassifyEmailBatch classifies one email against prompts using the given model and
// service tier. Callers classifying many emails in one pass should resolve model/tier
// once via ResolveClassifySettings and reuse them across calls.
func (c *Client) ClassifyEmailBatch(ctx context.Context, store StoreLogger, email Email, prompts []Prompt, model, tier string) (ClassifyResult, error) {
	if len(prompts) == 0 {
		return ClassifyResult{}, nil
	}

	msgs, inf := classifyPayload(email, prompts)

	reqPayload := map[string]any{
		"modelId":         model,
		"messages":        msgs,
		"inferenceConfig": inf,
		"toolConfig":      classifyToolConfigJSON(prompts),
	}
	var svcTier *types.ServiceTier
	if tier == ClassifyTierFlex {
		svcTier = &types.ServiceTier{Type: types.ServiceTierTypeFlex}
		reqPayload["serviceTier"] = map[string]string{jsonTypeKey: ClassifyTierFlex}
	}
	reqJSON, marshalErr := json.Marshal(reqPayload)
	if marshalErr != nil {
		reqJSON = []byte("{}")
	}
	res := ClassifyResult{RequestJSON: string(reqJSON)}

	subject := email.Subject
	if len(subject) > 60 {
		subject = subject[:60]
	}
	tierLabel := tier
	if tierLabel == "" {
		tierLabel = ClassifyTierStandard
	}
	store.Log("INFO", fmt.Sprintf("LLM classifying '%s' against %d rule(s) (model: %s, tier: %s)", subject, len(prompts), model, tierLabel))

	plainConverse := func() (*bedrockruntime.ConverseOutput, error) {
		return c.br.Converse(ctx, &bedrockruntime.ConverseInput{
			ModelId:         aws.String(model),
			Messages:        msgs,
			InferenceConfig: inf,
			ServiceTier:     svcTier,
		})
	}

	// Preferred path: force tool use so Bedrock returns schema-validated JSON
	// directly (see classifyToolConfig). If the model rejects the tool-use call
	// outright, retry once without ToolConfig below. When the call succeeds but the
	// model didn't call the tool anyway, fall through to the same text-parsing logic
	// used by the retry, applied to this response.
	out, err := c.br.Converse(ctx, &bedrockruntime.ConverseInput{
		ModelId:         aws.String(model),
		Messages:        msgs,
		InferenceConfig: inf,
		ServiceTier:     svcTier,
		ToolConfig:      classifyToolConfig(prompts),
	})
	if err == nil {
		if parsed, rawJSON, ok := extractToolUse(out.Output, classifyToolName); ok {
			store.Log("INFO", fmt.Sprintf("LLM classify response via tool-use: %d field(s)", len(parsed)))
			res.RawResponse = rawJSON
			res.Results = mapKeysToResults(parsed, prompts)
			return res, nil
		}
		store.Log("INFO", "LLM response had no tool-use block; falling back to text parsing")
	} else {
		store.Log("INFO", fmt.Sprintf("LLM tool-use call failed (%v); retrying without tool use", err))
		out, err = plainConverse()
		if err != nil {
			store.Log("ERROR", fmt.Sprintf("LLM request failed: %v", err))
			return res, &Error{Msg: fmt.Sprintf("LLM request failed: %v", err)}
		}
	}

	raw := extractText(out.Output)
	store.Log("INFO", fmt.Sprintf("LLM classify response: content=%d chars", len(raw)))
	if len(raw) > 0 {
		preview := raw
		if len(preview) > 500 {
			preview = preview[:500]
		}
		store.Log("INFO", "LLM raw content: "+preview)
	}
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
		userMsg := fmt.Sprintf(
			"RULE NAME: %s\nTARGET LABEL: %s\nTRIGGER: %s\n\nCURRENT INSTRUCTIONS:\n%s\n\nMISHANDLED EMAIL:\nFrom: %s\nSubject: %s\nBody:\n%s\n\nRewrite the instructions per the system rules.",
			req.PromptName, req.LabelName, req.TriggerKind,
			req.OriginalInstructions,
			req.EmailSender, req.EmailSubject, req.EmailBody,
		)
		msgs = append(msgs, types.Message{
			Role:    types.ConversationRoleUser,
			Content: []types.ContentBlock{&types.ContentBlockMemberText{Value: userMsg}},
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
		conv = append(conv, ChatMessage{Role: "user", Content: fmt.Sprintf(
			"RULE NAME: %s\nTARGET LABEL: %s\nTRIGGER: %s\n\nCURRENT INSTRUCTIONS:\n%s\n\nMISHANDLED EMAIL:\nFrom: %s\nSubject: %s\nBody:\n%s\n\nRewrite the instructions per the system rules.",
			req.PromptName, req.LabelName, req.TriggerKind,
			req.OriginalInstructions,
			req.EmailSender, req.EmailSubject, req.EmailBody,
		)})
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
	cat, err := fetchPricingCatalog(ctx, c.pc)
	if err != nil {
		cat = &pricingCatalog{
			inputPricePer1M:     map[string]float64{},
			flexInputPricePer1M: map[string]float64{},
			flexCapable:         map[string]bool{},
		}
	}

	// One unfiltered catalog call: drives the foundation-model list AND supplies the
	// modality of the models that back each inference profile.
	fmOut, err := c.bc.ListFoundationModels(ctx, &bedrock.ListFoundationModelsInput{})
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
	ipOut, err := c.bc.ListInferenceProfiles(ctx, &bedrock.ListInferenceProfilesInput{
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
