package llm

import (
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
)

// reasoningDirective describes how to suppress a thinking-capable model's chain-of-
// thought output for classification. Different model families expose different
// switches: some respond to a soft-switch phrase in the system prompt (e.g. Qwen3's
// "/no_think", NVIDIA Nemotron's "detailed thinking off"), others need a dedicated
// request field instead (AdditionalModelRequestFields on ConverseInput). Both are
// optional and a zero-value directive is a no-op — classifyPayload only sets what's
// populated.
type reasoningDirective struct {
	system string         // soft-switch text to inject as a system content block
	fields map[string]any // extra fields for ConverseInput.AdditionalModelRequestFields
}

func (d reasoningDirective) isZero() bool {
	return d.system == "" && d.fields == nil
}

// reasoningRegistryEntry pairs a case-insensitive model-id substring with the
// directive that suppresses that family's reasoning output.
type reasoningRegistryEntry struct {
	substr    string
	directive reasoningDirective
}

// reasoningRegistry is a maintained capability map for the model families this
// project has actually run against Bedrock classification — it is NOT a
// single-model assumption baked into the classify path. (That was the earlier bug:
// comments throughout this package assumed Amazon Nova specifically, which broke
// down the moment the configured model changed. This table exists so a model swap
// only needs a new entry here, not a re-audit of the whole classify path.) An
// unmatched model id resolves to a zero directive — a safe no-op, not an error —
// so an unrecognized model still classifies correctly, just without reasoning
// suppressed (see detectReasoning, which reports when that's happening).
var reasoningRegistry = []reasoningRegistryEntry{
	{substr: "qwen", directive: reasoningDirective{system: "/no_think"}},
	{substr: "nemotron", directive: reasoningDirective{system: "detailed thinking off"}},
}

// reasoningOff returns the directive that suppresses reasoning output for modelID,
// looked up by case-insensitive substring match against reasoningRegistry. override,
// when non-empty, replaces the registry's system string — the escape hatch for a
// model family the registry doesn't know about yet (see
// SettingClassifyReasoningDirective). A model matching no registry entry and given
// no override returns a zero directive (no-op).
func reasoningOff(modelID, override string) reasoningDirective {
	var d reasoningDirective
	lower := strings.ToLower(modelID)
	for _, e := range reasoningRegistry {
		if strings.Contains(lower, e.substr) {
			d = e.directive
			break
		}
	}
	if override != "" {
		d.system = override
	}
	return d
}

// reasoningEffortRegistryEntry pairs a case-insensitive model-id substring with the
// AdditionalModelRequestFields shape that model family expects for a non-off reasoning
// effort. Unlike reasoningRegistry (which only ever turns reasoning *off*), this table
// turns it *on* at a given effort — used by the improve call, which unlike classify
// benefits from letting a capable model actually think before proposing a rule rewrite
// (see SettingImproveReasoningEffort).
type reasoningEffortRegistryEntry struct {
	substr string
	fields func(effort string) map[string]any
}

// reasoningEffortRegistry is a maintained capability map, same spirit as
// reasoningRegistry above: a model swap needing a new entry here is expected, not a bug.
// An unmatched model id (or ReasoningEffortOff) resolves to nil fields — a safe no-op.
var reasoningEffortRegistry = []reasoningEffortRegistryEntry{
	// GLM-5 on Bedrock exposes reasoning as a bare on/off switch via
	// additionalModelRequestFields.reasoning_config — there is no separate
	// low/medium/high; every non-off effort maps to the same "high" value AWS'
	// documentation shows. See https://docs.aws.amazon.com/bedrock/latest/userguide/model-card-zai-glm-5.html.
	{substr: "glm", fields: func(string) map[string]any {
		return map[string]any{"reasoning_config": "high"}
	}},
}

// reasoningEffortFields returns the AdditionalModelRequestFields map that turns reasoning
// on at the given effort for modelID, looked up by case-insensitive substring match
// against reasoningEffortRegistry. Returns nil for ReasoningEffortOff or an unrecognized
// model id — both safe no-ops, matching reasoningOff's own "unmatched is a no-op, not an
// error" behavior.
func reasoningEffortFields(modelID, effort string) map[string]any {
	if effort == "" || effort == ReasoningEffortOff {
		return nil
	}
	lower := strings.ToLower(modelID)
	for _, e := range reasoningEffortRegistry {
		if strings.Contains(lower, e.substr) {
			return e.fields(effort)
		}
	}
	return nil
}

// detectReasoning reports whether a classify response contains reasoning/
// chain-of-thought content despite the reasoning directive — either as a dedicated
// Converse ReasoningContent block, or as an inline "<think>...</think>" span in the
// text output (the two forms observed across the model families reasoningRegistry
// covers). This is the "is suppression actually working" signal: no Bedrock API
// reports it directly, so ClassifyEmailBatch logs this on every call instead.
func detectReasoning(output types.ConverseOutput, rawText string) bool {
	if msg, ok := output.(*types.ConverseOutputMemberMessage); ok {
		for _, block := range msg.Value.Content {
			if _, ok := block.(*types.ContentBlockMemberReasoningContent); ok {
				return true
			}
		}
	}
	return strings.Contains(rawText, "<think>")
}
