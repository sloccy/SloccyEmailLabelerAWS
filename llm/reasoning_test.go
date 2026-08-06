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

// TestReasoningEffortFields_DefaultOnForUnverifiedModel locks in the default-on design: a
// model this project has never explicitly tested (i.e. not in reasoningEffortExempt) is
// assumed to honor additionalModelRequestFields.reasoning_config — verified broadly enough
// across independent vendors (see reasoningEffortSupported's doc comment) that "assume it
// works" beats "require a code change per new model," with ImprovePromptInstructions'
// ValidationException retry as the fallback if a given model turns out not to.
func TestReasoningEffortFields_DefaultOnForUnverifiedModel(t *testing.T) {
	got := reasoningEffortFields("some.brand-new-reasoning-model-v1:0", ReasoningEffortOn)
	want := map[string]any{"reasoning_config": "high"}
	if len(got) != len(want) || got["reasoning_config"] != want["reasoning_config"] {
		t.Errorf("reasoningEffortFields(unverified model, on) = %#v, want %#v", got, want)
	}
}

func TestReasoningEffortFields_UnknownEffortIsNoop(t *testing.T) {
	if got := reasoningEffortFields("zai.glm-5", "bogus"); got != nil {
		t.Errorf("reasoningEffortFields(glm, bogus) = %#v, want nil", got)
	}
}

// TestReasoningEffortFields_ExemptModelsAreNoop locks in the two live-verified exceptions:
// nvidia.nemotron-nano-9b-v2 silently ignored the field (identical output to baseline), and
// mistral.magistral-small-2509 reasons unconditionally in plain text regardless of it — for
// both, requesting "on" must not produce a field that would just be a no-op or a lie about
// what the model is actually doing.
func TestReasoningEffortFields_ExemptModelsAreNoop(t *testing.T) {
	for _, model := range []string{"nvidia.nemotron-nano-9b-v2", "mistral.magistral-small-2509"} {
		if got := reasoningEffortFields(model, ReasoningEffortOn); got != nil {
			t.Errorf("reasoningEffortFields(%q, on) = %#v, want nil (exempt model)", model, got)
		}
	}
}

// TestReasoningEffortLevels_ExemptModelIsNil checks the Settings UI's "disable the control"
// signal for the two confirmed exceptions.
func TestReasoningEffortLevels_ExemptModelIsNil(t *testing.T) {
	for _, model := range []string{"nvidia.nemotron-nano-9b-v2", "mistral.magistral-small-2509"} {
		if got := ReasoningEffortLevels(model); got != nil {
			t.Errorf("ReasoningEffortLevels(%q) = %#v, want nil", model, got)
		}
	}
}

// TestReasoningEffortLevels_NonExemptModelIsOnOff checks the UI-facing side of default-on:
// any model not on the exempt list — including ones with no dedicated registry entry at
// all — gets exactly the one on/off level, not a four-value ladder no known model actually
// has.
func TestReasoningEffortLevels_NonExemptModelIsOnOff(t *testing.T) {
	for _, model := range []string{"zai.glm-5", "deepseek.v3.2", "some.brand-new-model"} {
		got := ReasoningEffortLevels(model)
		if len(got) != 1 || got[0] != ReasoningEffortOn {
			t.Errorf("ReasoningEffortLevels(%q) = %#v, want [%q]", model, got, ReasoningEffortOn)
		}
	}
}

func TestReasoningEffortFields_MatchIsCaseInsensitive(t *testing.T) {
	got := reasoningEffortFields("ZAI.GLM-5", ReasoningEffortOn)
	if got == nil || got["reasoning_config"] != "high" {
		t.Errorf("reasoningEffortFields(uppercased model id, on) = %#v, want reasoning_config=high", got)
	}
	// The exempt-list match must also be case-insensitive, not just the default-on path.
	if got := reasoningEffortFields("NVIDIA.NEMOTRON-NANO-9B-V2", ReasoningEffortOn); got != nil {
		t.Errorf("reasoningEffortFields(uppercased exempt model, on) = %#v, want nil", got)
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
