package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/sloccy/ollamail/db"
)

var fenceRe = regexp.MustCompile(`(?s)^` + "```" + `(?:json)?\s*|\s*` + "```" + `$`)
var blankRunRe = regexp.MustCompile(`\n{3,}`)

const (
	jsonKeyModel       = "model"
	jsonKeyRole        = "role"
	roleUser           = "user"
	jsonKeyStream      = "stream"
	jsonKeyMessages    = "messages"
	jsonKeyContent     = "content"
	jsonKeyOptions     = "options"
	jsonKeyTemperature = "temperature"
	jsonKeyNumPredict  = "num_predict"
	jsonKeyNumCtx      = "num_ctx"
)

type Client struct {
	host       string
	model      string
	numCtx     int
	timeout    time.Duration
	httpClient *http.Client
}

func NewClient(host, model string, numCtx int, timeout time.Duration) *Client {
	return &Client{
		host:       strings.TrimRight(host, "/"),
		model:      model,
		numCtx:     numCtx,
		timeout:    timeout,
		httpClient: &http.Client{Timeout: timeout},
	}
}

func (c *Client) Model() string { return c.model }

// ============================================================
// Model management
// ============================================================

func (c *Client) EnsureModelPulled(store *db.Store) error {
	ctx := context.Background()
	exists, err := c.modelExists(ctx)
	if err != nil {
		store.Log("WARNING", fmt.Sprintf("Could not check Ollama model: %v", err))
		return err
	}
	if !exists {
		store.Log("INFO", fmt.Sprintf("Pulling model %s from Ollama... (this may take a while)", c.model))
		if err := c.pullModel(ctx); err != nil {
			store.Log("WARNING", fmt.Sprintf("Could not pull Ollama model: %v", err))
			return err
		}
		store.Log("INFO", fmt.Sprintf("Model %s ready.", c.model))
	}
	return nil
}

func (c *Client) modelExists(ctx context.Context) (bool, error) {
	body, err := json.Marshal(map[string]string{jsonKeyModel: c.model})
	if err != nil {
		return false, err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.host+"/api/show", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode == http.StatusOK, nil
}

func (c *Client) pullModel(ctx context.Context) error {
	body, err := json.Marshal(map[string]any{jsonKeyModel: c.model, jsonKeyStream: true})
	if err != nil {
		return err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.host+"/api/pull", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// Use unlimited timeout client for pull
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	// Drain streaming response
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// ============================================================
// Classification
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

// Error is returned when the LLM fails to produce a usable response.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

func (c *Client) buildClassifyPayload(email Email, prompts []Prompt) map[string]any {
	body := buildBody(email, prompts)
	numPredict := 32
	if n := len(prompts) * 10; n > numPredict {
		numPredict = n
	}
	return map[string]any{
		jsonKeyModel: c.model,
		jsonKeyMessages: []map[string]string{
			{jsonKeyRole: roleUser, jsonKeyContent: body},
		},
		"think":       false,
		"format":      "json",
		jsonKeyStream: false,
		jsonKeyOptions: map[string]any{
			jsonKeyTemperature: 0,
			jsonKeyNumPredict:  numPredict,
			jsonKeyNumCtx:      c.numCtx,
		},
	}
}

// BuildClassifyRequestJSON returns the serialised Ollama payload for the given
// email and prompts without making a network call. Returns "" when prompts is empty.
func (c *Client) BuildClassifyRequestJSON(email Email, prompts []Prompt) string {
	if len(prompts) == 0 {
		return ""
	}
	b, err := json.Marshal(c.buildClassifyPayload(email, prompts))
	if err != nil {
		return "{}"
	}
	return string(b)
}

// ClassifyResult holds the output of ClassifyEmailBatch.
type ClassifyResult struct {
	Results     map[int64]bool
	RequestJSON string
	RawResponse string
}

func (c *Client) ClassifyEmailBatch(ctx context.Context, store *db.Store, email Email, prompts []Prompt) (ClassifyResult, error) {
	if len(prompts) == 0 {
		return ClassifyResult{}, nil
	}

	payload := c.buildClassifyPayload(email, prompts)

	requestBytes, err := json.Marshal(payload)
	if err != nil {
		requestBytes = []byte("{}")
	}
	res := ClassifyResult{RequestJSON: string(requestBytes)}

	subject := email.Subject
	if len(subject) > 60 {
		subject = subject[:60]
	}
	store.Log("INFO", fmt.Sprintf("LLM classifying '%s' against %d rule(s)", subject, len(prompts)))

	raw, err := c.doChat(ctx, payload)
	if err != nil {
		store.Log("ERROR", fmt.Sprintf("LLM request failed: %v", err))
		return res, &Error{Msg: fmt.Sprintf("LLM request failed: %v", err)}
	}

	store.Log("INFO", fmt.Sprintf("LLM classify response: content=%d chars", len(raw)))
	if len(raw) > 0 {
		preview := raw
		if len(preview) > 500 {
			preview = preview[:500]
		}
		store.Log("INFO", "LLM raw content: "+preview)
	}

	res.RawResponse = raw
	raw = strings.TrimSpace(fenceRe.ReplaceAllString(raw, ""))

	var parsed map[string]any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		store.Log("ERROR", fmt.Sprintf("LLM parse error: %v | raw: %s", err, raw))
		return res, &Error{Msg: fmt.Sprintf("LLM parse error: %v", err)}
	}

	res.Results = make(map[int64]bool, len(prompts))
	for k, v := range parsed {
		var idx int
		if _, err := fmt.Sscanf(k, "%d", &idx); err != nil {
			continue
		}
		idx-- // 1-based to 0-based
		if idx >= 0 && idx < len(prompts) {
			b, _ := v.(bool)
			res.Results[prompts[idx].ID] = b
		}
	}
	return res, nil
}

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
	body = blankRunRe.ReplaceAllString(body, "\n\n")

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

// ============================================================
// Streaming prompt generation
// ============================================================

// StreamChunk is yielded by StreamGeneratePromptInstruction.
type StreamChunk struct {
	Text string
	Err  error
}

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
	payload := map[string]any{
		jsonKeyModel: c.model,
		jsonKeyMessages: []map[string]string{
			{
				jsonKeyRole:    "system",
				jsonKeyContent: "You write email filter rules for an AI classifier. Output only the rule text. No preamble, no drafts, no self-critique, no quotes, no explanation.",
			},
			{
				jsonKeyRole: roleUser,
				jsonKeyContent: fmt.Sprintf(
					"Write a 2-4 sentence classifier instruction for emails matching: %q\n\n"+
						"The instruction must describe: what the email is about, its purpose/intent, "+
						"and what distinguishes it from similar-but-non-matching emails. "+
						"Do not use keywords or sender addresses as criteria — focus on meaning and context.\n\n"+
						"Output ONLY the instruction text.",
					description),
			},
		},
		jsonKeyStream: true,
		jsonKeyOptions: map[string]any{
			jsonKeyTemperature: 0.7,
			jsonKeyNumPredict:  2048,
			jsonKeyNumCtx:      c.numCtx,
		},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.host+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var chunk struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			Done bool `json:"done"`
		}
		if err := json.Unmarshal(line, &chunk); err != nil {
			continue
		}
		if chunk.Message.Content != "" {
			select {
			case ch <- StreamChunk{Text: chunk.Message.Content}:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if chunk.Done {
			break
		}
	}
	slog.Info("stream_generate finished")
	return scanner.Err()
}

// ============================================================
// Prompt Improvement
// ============================================================

// ChatMessage is a single turn in a conversation.
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ImproveRequest carries everything needed to generate an improved prompt instruction.
type ImproveRequest struct {
	PromptName           string
	LabelName            string
	OriginalInstructions string
	TriggerKind          string // "false_negative" | "false_positive"
	EmailSubject         string
	EmailSender          string
	EmailBody            string
	PriorConversation    []ChatMessage
	UserComment          string
}

const improveSystemPrompt = `You are a careful editor of email-classification rules. You are given one existing rule (its name, target Gmail label, and current instructions) and one concrete email that the rule handled incorrectly. Your job is to rewrite the instructions so that the same rule would have handled this email correctly, without damaging its behavior on emails it currently classifies correctly.

CRITICAL OUTPUT REQUIREMENT: Your entire response must be ONLY the rewritten rule instructions — nothing else. No preamble, no explanation, no "Here is the updated rule:", no quoting of the email, no markdown formatting, no commentary. Think as long as you need internally, but the only thing you output is the new instructions text itself.

Rules for rewriting:
1. Preserve the rule's original intent. Do not widen scope beyond what the name and label imply. Do not turn a narrow rule into a catch-all.
2. Never use the specific sender address, subject line, or body phrases of the example email as matching criteria. The example is an illustration, not a fingerprint. Match on meaning, purpose, and context.
3. If trigger_kind is false_negative: explain what category of email was missed and add language that would match it. If trigger_kind is false_positive: add exclusions or clarify the scope so emails like this one are no longer matched.
4. Keep the output 2-6 sentences. Plain prose. No bullet lists, no code blocks, no markdown headings.
5. If the user comments on your suggestion, treat the comment as authoritative feedback and produce another revision that addresses it while still obeying rules 1-4.

Remember: output ONLY the rewritten instructions text. No other text whatsoever.`

// ImprovePromptInstructions calls the LLM to rewrite a prompt's instructions based on
// a misclassification example. It returns the revised text, the full conversation (for
// subsequent iterations), and any error.
func (c *Client) ImprovePromptInstructions(ctx context.Context, req ImproveRequest) (string, []ChatMessage, error) {
	messages := []map[string]string{{jsonKeyRole: "system", jsonKeyContent: improveSystemPrompt}}

	if len(req.PriorConversation) > 0 {
		for _, m := range req.PriorConversation {
			messages = append(messages, map[string]string{jsonKeyRole: m.Role, jsonKeyContent: m.Content})
		}
		// Append latest user comment
		messages = append(messages, map[string]string{
			jsonKeyRole:    roleUser,
			jsonKeyContent: req.UserComment,
		})
	} else {
		userMsg := fmt.Sprintf(
			"RULE NAME: %s\nTARGET LABEL: %s\nTRIGGER: %s\n\nCURRENT INSTRUCTIONS:\n%s\n\nMISHANDLED EMAIL:\nFrom: %s\nSubject: %s\nBody:\n%s\n\nRewrite the instructions per the system rules.",
			req.PromptName, req.LabelName, req.TriggerKind,
			req.OriginalInstructions,
			req.EmailSender, req.EmailSubject, req.EmailBody,
		)
		messages = append(messages, map[string]string{jsonKeyRole: roleUser, jsonKeyContent: userMsg})
	}

	payload := map[string]any{
		jsonKeyModel:    c.model,
		jsonKeyMessages: messages,
		jsonKeyStream:   false,
		jsonKeyOptions: map[string]any{
			jsonKeyTemperature: 0.4,
			jsonKeyNumPredict:  16384,
			jsonKeyNumCtx:      c.numCtx,
		},
	}

	raw, err := c.doChat(ctx, payload)
	if err != nil {
		return "", nil, err
	}
	suggestion := strings.TrimSpace(raw)

	// Build updated conversation for storage
	var conv []ChatMessage
	conv = append(conv, req.PriorConversation...)
	if len(req.PriorConversation) > 0 {
		conv = append(conv, ChatMessage{Role: roleUser, Content: req.UserComment})
	} else {
		conv = append(conv, ChatMessage{Role: roleUser, Content: fmt.Sprintf(
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
// Internal
// ============================================================

func (c *Client) doChat(ctx context.Context, payload map[string]any) (string, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.host+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("ollama %d: %s", resp.StatusCode, string(b))
	}

	var result struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	return result.Message.Content, nil
}
