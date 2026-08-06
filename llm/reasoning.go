package llm

import (
	"slices"
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
// non-off SettingImproveReasoningEffort values that model family actually distinguishes
// (levels) and the AdditionalModelRequestFields shape for each. Unlike reasoningRegistry
// (which only ever turns reasoning *off*), this table turns it *on* — used by the improve
// call, which unlike classify benefits from letting a capable model actually think before
// proposing a rule rewrite (see SettingImproveReasoningEffort). levels is also what the
// Settings UI renders as options, so it must list only what the family actually does
// something different for — not just what its API happens to accept without erroring.
type reasoningEffortRegistryEntry struct {
	substr string
	levels []string
	fields func(effort string) map[string]any
}

// reasoningEffortRegistry is a maintained capability map, same spirit as
// reasoningRegistry above: a model swap needing a new entry here is expected, not a bug.
// An unmatched model id (or ReasoningEffortOff) resolves to nil fields — a safe no-op.
var reasoningEffortRegistry = []reasoningEffortRegistryEntry{
	// GLM-5 on Bedrock takes additionalModelRequestFields.reasoning_config as a bare
	// string, forwarded to the model as its native "reasoning_effort" parameter. Verified
	// live against zai.glm-5 in us-east-1: the API itself accepts "none"/"low"/"medium"/
	// "high" (confirmed via the ValidationException message when an invalid value like
	// "xhigh" or "minimal" is sent, which lists the accepted enum), but only "high"
	// actually turns reasoning on — "none"/"low"/"medium" were all indistinguishable from
	// omitting the field entirely (same 2-token output, no ReasoningContent block),
	// while "high" alone produced a real ~1600-character reasoning trace and ~500 output
	// tokens. So although the API has four accepted values, the model only has two
	// observable behaviors — this is presented as on/off, not a ladder, and "on" is the
	// only level that maps to a non-nil field.
	{substr: "glm", levels: []string{ReasoningEffortOn}, fields: func(string) map[string]any {
		return map[string]any{"reasoning_config": "high"}
	}},
}

// ReasoningEffortLevels returns the non-off SettingImproveReasoningEffort values modelID's
// family actually distinguishes, looked up by case-insensitive substring match against
// reasoningEffortRegistry. Returns nil for an unrecognized model — the Settings UI reads
// this as "reasoning effort isn't controllable for this model" and disables the control
// rather than offering choices that would silently do nothing.
func ReasoningEffortLevels(modelID string) []string {
	lower := strings.ToLower(modelID)
	for _, e := range reasoningEffortRegistry {
		if strings.Contains(lower, e.substr) {
			return e.levels
		}
	}
	return nil
}

// reasoningEffortFields returns the AdditionalModelRequestFields map that turns reasoning
// on at the given effort for modelID, looked up by case-insensitive substring match
// against reasoningEffortRegistry. Returns nil for ReasoningEffortOff, an unrecognized
// model id, or an effort outside that family's levels — all safe no-ops, matching
// reasoningOff's own "unmatched is a no-op, not an error" behavior. Callers must not treat
// a nil result here as license to fall back to reasoningOff: asking for reasoning and
// suppressing it are opposite intents, so an unsupported effort should leave the model at
// its own default, not invert into suppression.
func reasoningEffortFields(modelID, effort string) map[string]any {
	if effort == "" || effort == ReasoningEffortOff {
		return nil
	}
	lower := strings.ToLower(modelID)
	for _, e := range reasoningEffortRegistry {
		if !strings.Contains(lower, e.substr) {
			continue
		}
		if !slices.Contains(e.levels, effort) {
			return nil
		}
		return e.fields(effort)
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
