package llm

import (
	"context"
	"encoding/json"
)

// StoreLogger is the logging dependency injected into ClassifyEmailBatch.
// *db.Store and *db.FakeStore both satisfy this via their Log method.
type StoreLogger interface {
	Log(level, message string)
}

// ClientIface is the LLM client contract used by processor, poller, and server.
// *Client satisfies this; *FakeClient satisfies it for tests.
type ClientIface interface {
	ResolveClassifySettings(ctx context.Context) (model, tier, reasoningOverride string)
	ClassifyEmailBatch(ctx context.Context, store StoreLogger, email Email, prompts []Prompt, model, tier, reasoningOverride string, debug bool) (ClassifyResult, error)
	StreamGeneratePromptInstruction(ctx context.Context, description string) <-chan StreamChunk
	ImprovePromptInstructions(ctx context.Context, req ImproveRequest) (string, []ChatMessage, error)
	ListAvailableModels(ctx context.Context) ([]ModelOption, error)
}

// FakeClient is a test double for ClientIface. It returns a canned JSON
// response or a canned error, without making any network calls.
const fakeModelID = "fake-model"

type FakeClient struct {
	response string
	callErr  error
	model    string
}

// NewFakeClient returns a FakeClient that parses response as a JSON classify result.
func NewFakeClient(response string) *FakeClient {
	return &FakeClient{response: response, model: fakeModelID}
}

// NewFakeErrorClient returns a FakeClient whose ClassifyEmailBatch always errors.
func NewFakeErrorClient() *FakeClient {
	return &FakeClient{callErr: &Error{Msg: "fake LLM error"}, model: fakeModelID}
}

func (c *FakeClient) ResolveClassifySettings(_ context.Context) (model, tier, reasoningOverride string) {
	return c.model, ClassifyTierStandard, ""
}

func (c *FakeClient) ClassifyEmailBatch(_ context.Context, _ StoreLogger, _ Email, prompts []Prompt, _, _, _ string, _ bool) (ClassifyResult, error) {
	if c.callErr != nil {
		return ClassifyResult{}, c.callErr
	}
	res := ClassifyResult{
		RawResponse: c.response,
		Results:     make(map[int64]bool),
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(c.response), &parsed); err != nil {
		return res, &Error{Msg: "fake parse error: " + err.Error()}
	}
	res.Results = mapKeysToResults(parsed, prompts)
	return res, nil
}

func (c *FakeClient) StreamGeneratePromptInstruction(ctx context.Context, _ string) <-chan StreamChunk {
	ch := make(chan StreamChunk, 1)
	ch <- StreamChunk{Text: "fake instruction"}
	close(ch)
	return ch
}

func (c *FakeClient) ImprovePromptInstructions(_ context.Context, _ ImproveRequest) (string, []ChatMessage, error) {
	return "fake improved", nil, nil
}

func (c *FakeClient) ListAvailableModels(_ context.Context) ([]ModelOption, error) {
	return []ModelOption{
		{ID: fakeModelID, Label: "Fake Model"},
	}, nil
}
