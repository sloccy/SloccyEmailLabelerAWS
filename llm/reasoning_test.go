package llm

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
)

func TestReasoningOff_RegistryMatch(t *testing.T) {
	cases := []struct {
		name       string
		modelID    string
		wantSystem string
	}{
		{"qwen3-32b", "qwen.qwen3-32b-v1:0", "/no_think"},
		{"qwen mixed case", "Qwen.Qwen3-235B-A22B-2507-v1:0", "/no_think"},
		{"nemotron", "nvidia.nemotron-9b-v2", "detailed thinking off"},
		{"unrecognized model", "meta.llama3-1-70b-instruct-v1:0", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := reasoningOff(c.modelID, "")
			if got.system != c.wantSystem {
				t.Errorf("reasoningOff(%q, \"\").system = %q, want %q", c.modelID, got.system, c.wantSystem)
			}
		})
	}
}

func TestReasoningOff_OverrideBeatsRegistry(t *testing.T) {
	got := reasoningOff("qwen.qwen3-32b-v1:0", "/think_less")
	if got.system != "/think_less" {
		t.Errorf("system = %q, want override to win over registry", got.system)
	}
}

func TestReasoningOff_OverrideAloneAppliesToUnknownModel(t *testing.T) {
	got := reasoningOff("some-brand-new-model-id", "detailed thinking off")
	if got.system != "detailed thinking off" {
		t.Errorf("system = %q, want override applied even with no registry match", got.system)
	}
}

func TestReasoningOff_NoMatchNoOverrideIsZero(t *testing.T) {
	got := reasoningOff("meta.llama3-1-70b-instruct-v1:0", "")
	if !got.isZero() {
		t.Errorf("got %#v, want zero directive", got)
	}
}

func TestReasoningEffortFields_OffIsNoop(t *testing.T) {
	if got := reasoningEffortFields("zai.glm-5", ReasoningEffortOff); got != nil {
		t.Errorf("reasoningEffortFields(glm, off) = %#v, want nil", got)
	}
	if got := reasoningEffortFields("zai.glm-5", ""); got != nil {
		t.Errorf("reasoningEffortFields(glm, \"\") = %#v, want nil (empty treated same as off)", got)
	}
}

func TestReasoningEffortFields_UnrecognizedModelIsNoop(t *testing.T) {
	if got := reasoningEffortFields("meta.llama3-1-70b-instruct-v1:0", ReasoningEffortHigh); got != nil {
		t.Errorf("reasoningEffortFields(unrecognized, high) = %#v, want nil", got)
	}
}

// TestReasoningEffortFields_GLMBinaryToggle locks in GLM-5's Bedrock reasoning contract:
// additionalModelRequestFields.reasoning_config is a bare on/off switch (AWS's own docs
// only show "high"), not a graduated ladder — so every non-off effort this setting offers
// must map to that same one "on" value, not to distinct per-level fields that don't exist
// on this model family.
func TestReasoningEffortFields_GLMBinaryToggle(t *testing.T) {
	for _, effort := range []string{ReasoningEffortLow, ReasoningEffortMedium, ReasoningEffortHigh} {
		got := reasoningEffortFields("zai.glm-5", effort)
		want := map[string]any{"reasoning_config": "high"}
		if len(got) != len(want) || got["reasoning_config"] != want["reasoning_config"] {
			t.Errorf("reasoningEffortFields(glm, %q) = %#v, want %#v", effort, got, want)
		}
	}
}

func TestReasoningEffortFields_MatchIsCaseInsensitive(t *testing.T) {
	got := reasoningEffortFields("ZAI.GLM-5", ReasoningEffortHigh)
	if got == nil || got["reasoning_config"] != "high" {
		t.Errorf("reasoningEffortFields(uppercased model id, high) = %#v, want reasoning_config=high", got)
	}
}

func textOutputForDetect(text string) types.ConverseOutput {
	return &types.ConverseOutputMemberMessage{
		Value: types.Message{
			Role:    types.ConversationRoleAssistant,
			Content: []types.ContentBlock{&types.ContentBlockMemberText{Value: text}},
		},
	}
}

func TestDetectReasoning_InlineThinkBlock(t *testing.T) {
	raw := "<think>reasoning here</think>{\"1\": true}"
	if !detectReasoning(textOutputForDetect(raw), raw) {
		t.Error("expected detectReasoning to report true for a <think> block in the raw text")
	}
}

func TestDetectReasoning_ReasoningContentBlock(t *testing.T) {
	output := &types.ConverseOutputMemberMessage{
		Value: types.Message{
			Role: types.ConversationRoleAssistant,
			Content: []types.ContentBlock{
				&types.ContentBlockMemberReasoningContent{
					Value: &types.ReasoningContentBlockMemberReasoningText{
						Value: types.ReasoningTextBlock{Text: aws.String("thinking...")},
					},
				},
				&types.ContentBlockMemberText{Value: `{"1": true}`},
			},
		},
	}
	if !detectReasoning(output, `{"1": true}`) {
		t.Error("expected detectReasoning to report true for a ReasoningContent block")
	}
}

func TestDetectReasoning_NoneWhenSuppressed(t *testing.T) {
	raw := `{"1": true}`
	if detectReasoning(textOutputForDetect(raw), raw) {
		t.Error("expected detectReasoning to report false when no reasoning is present")
	}
}
