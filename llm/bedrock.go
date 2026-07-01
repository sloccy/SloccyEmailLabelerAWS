package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrock"
	bedrocktypes "github.com/aws/aws-sdk-go-v2/service/bedrock/types"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
)

var fenceRe = regexp.MustCompile(`(?s)^` + "```" + `(?:json)?\s*|\s*` + "```" + `$`)

// Settings is the minimal interface the Client needs to look up per-call model
// overrides. *db.Store and *db.FakeStore both satisfy this.
type Settings interface {
	GetSetting(ctx context.Context, key string) (string, error)
}

// Setting keys for the two independent model selections.
const (
	SettingClassifyModel = "classify_model"
	SettingImproveModel  = "improve_model"
	// SettingScanInterval holds the user-selected scan cadence in minutes. The web UI
	// writes it and rewrites the EventBridge Scheduler schedule to match.
	SettingScanInterval = "scan_interval_minutes"
)

// ModelOption is one entry in the model-selection dropdown.
type ModelOption struct {
	ID             string  // value sent to Bedrock (modelId or inferenceProfileId)
	Label          string  // human-readable display name
	InputCostPer1M float64 // on-demand input price per 1M tokens; CostUnknown if unpriced
}

// Client wraps the Bedrock runtime client with per-call model resolution.
type Client struct {
	br           *bedrockruntime.Client
	bc           *bedrock.Client // control-plane, for listing
	defaultModel string
	settings     Settings
}

// NewClient creates a Bedrock client.
// settings provides per-call model lookups (classify_model / improve_model keys).
// defaultModel is the fallback when neither a setting nor BEDROCK_MODEL is set.
func NewClient(settings Settings, defaultModel string) *Client {
	if defaultModel == "" {
		defaultModel = os.Getenv("BEDROCK_MODEL")
	}
	if defaultModel == "" {
		defaultModel = "us.amazon.nova-micro-v1:0"
	}
	cfg, err := awsconfig.LoadDefaultConfig(context.Background())
	if err != nil {
		panic(fmt.Sprintf("bedrock: load aws config: %v", err))
	}
	// Hard-require a US region for all LLM traffic (data residency). The cross-region
	// inference profiles we use (us.*) only fan out within the US, and the IAM policy
	// denies non-US regions — this guarantees the client never even attempts one.
	if !strings.HasPrefix(cfg.Region, "us-") {
		cfg.Region = "us-east-1"
	}
	return &Client{
		br:           bedrockruntime.NewFromConfig(cfg),
		bc:           bedrock.NewFromConfig(cfg),
		defaultModel: defaultModel,
		settings:     settings,
	}
}

// Model returns the default model (used by tests and the troubleshooting UI).
func (c *Client) Model() string { return c.defaultModel }

// resolveModel looks up the setting key in the store; falls back to defaultModel.
func (c *Client) resolveModel(ctx context.Context, key string) string {
	if c.settings != nil {
		if v, err := c.settings.GetSetting(ctx, key); err == nil && v != "" {
			return v
		}
	}
	return c.defaultModel
}

// ============================================================
// Public types (unchanged from ollama.go for caller compatibility)
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

	exampleParts := make([]string, min(2, len(prompts)))
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
Respond with ONLY a JSON object where each key is the rule's number (1, 2, 3...) and the value is true or false.
Example: {%s}
No explanation, no markdown, just the JSON object.

Email:
From: %s
Subject: %s
Body:
%s`,
		rulesText, strings.Join(exampleParts, ", "),
		email.Sender, email.Subject, body)
}

func (c *Client) classifyPayload(ctx context.Context, email Email, prompts []Prompt) ([]types.Message, *types.InferenceConfiguration, string) {
	body := buildBody(email, prompts)
	numPredict := int32(32)
	if n := len(prompts) * 10; n > 32 {
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
	model := c.resolveModel(ctx, SettingClassifyModel)
	return msgs, inf, model
}

// BuildClassifyRequestJSON returns the serialised Bedrock Converse payload (for Troubleshooting UI).
func (c *Client) BuildClassifyRequestJSON(email Email, prompts []Prompt) string {
	if len(prompts) == 0 {
		return ""
	}
	msgs, inf, model := c.classifyPayload(context.Background(), email, prompts)
	payload := map[string]any{
		"modelId":         model,
		"messages":        msgs,
		"inferenceConfig": inf,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func (c *Client) ClassifyEmailBatch(ctx context.Context, store StoreLogger, email Email, prompts []Prompt) (ClassifyResult, error) {
	if len(prompts) == 0 {
		return ClassifyResult{}, nil
	}

	msgs, inf, model := c.classifyPayload(ctx, email, prompts)

	reqJSON, marshalErr := json.Marshal(map[string]any{"modelId": model, "messages": msgs, "inferenceConfig": inf})
	if marshalErr != nil {
		reqJSON = []byte("{}")
	}
	res := ClassifyResult{RequestJSON: string(reqJSON)}

	subject := email.Subject
	if len(subject) > 60 {
		subject = subject[:60]
	}
	store.Log("INFO", fmt.Sprintf("LLM classifying '%s' against %d rule(s) (model: %s)", subject, len(prompts), model))

	out, err := c.br.Converse(ctx, &bedrockruntime.ConverseInput{
		ModelId:         aws.String(model),
		Messages:        msgs,
		InferenceConfig: inf,
	})
	if err != nil {
		store.Log("ERROR", fmt.Sprintf("LLM request failed: %v", err))
		return res, &Error{Msg: fmt.Sprintf("LLM request failed: %v", err)}
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

	cleaned := strings.TrimSpace(fenceRe.ReplaceAllString(raw, ""))
	var parsed map[string]any
	if err := json.Unmarshal([]byte(cleaned), &parsed); err != nil {
		store.Log("ERROR", fmt.Sprintf("LLM parse error: %v | raw: %s", err, raw))
		return res, &Error{Msg: fmt.Sprintf("LLM parse error: %v", err)}
	}

	res.Results = make(map[int64]bool, len(prompts))
	for k, v := range parsed {
		var idx int
		if _, err := fmt.Sscanf(k, "%d", &idx); err != nil {
			continue
		}
		idx-- // 1-based → 0-based
		if idx >= 0 && idx < len(prompts) {
			b, _ := v.(bool)
			res.Results[prompts[idx].ID] = b
		}
	}
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

// ListAvailableModels returns Bedrock models suitable for text classification: text-in,
// text-out, streaming, on-demand foundation models, unioned with the system-defined
// inference profiles whose underlying model is likewise text-in/text-out. Image, embedding,
// and other non-text models (e.g. stability.*, *embed*) are excluded. Sorted cheapest input
// cost first, unpriced last.
func (c *Client) ListAvailableModels(ctx context.Context) ([]ModelOption, error) {
	seen := make(map[string]bool)
	var opts []ModelOption

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
		opts = append(opts, ModelOption{ID: id, Label: label, InputCostPer1M: inputCostPer1M(id)})
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
		opts = append(opts, ModelOption{ID: id, Label: label, InputCostPer1M: inputCostPer1M(id)})
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
