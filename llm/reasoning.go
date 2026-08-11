package llm

import (
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
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

// applyReasoningOff resolves reasoningOff(modelID, override) and, if it isn't a no-op,
// applies it: appending its system soft-switch text to sys and wrapping its
// AdditionalModelRequestFields as a document.Interface. Returns sys unchanged and a nil
// fields document for a zero directive. Factors out the identical "if d := reasoningOff(...);
// !d.isZero() { ... }" shape classifyPayload, streamGenerate, and ImprovePromptInstructions
// (bedrock.go) each used to paste independently.
func applyReasoningOff(sys []types.SystemContentBlock, modelID, override string) ([]types.SystemContentBlock, document.Interface) {
	d := reasoningOff(modelID, override)
	if d.isZero() {
		return sys, nil
	}
	var fields document.Interface
	if d.system != "" {
		sys = append(sys, sysText(d.system))
	}
	if d.fields != nil {
		fields = document.NewLazyDocument(d.fields)
	}
	return sys, fields
}

// reasoningEffortExempt lists case-insensitive model-id substrings verified live against
// Bedrock to NOT honor additionalModelRequestFields.reasoning_config — every other model is
// assumed to support it (see reasoningEffortSupported). Confirmed exceptions, both via a
// before/after Converse comparison (baseline vs. reasoning_config:"high", checking for a
// ReasoningContent block in the response):
//   - nvidia.nemotron-nano-9b-v2: field silently ignored, output identical to baseline.
//     (Contrast nvidia.nemotron-super-3-120b, same vendor, which DOES respond — this is a
//     model-size quirk, not a family-wide one, so the substring is deliberately narrow.)
//   - mistral.magistral-small-2509: reasons unconditionally inline in the visible text
//     (never as a structured ReasoningContent block), unaffected by the field either way.
//
// A new entry here needs the same live comparison, not a guess.
var reasoningEffortExempt = []string{
	"nemotron-nano",
	"magistral",
}

// reasoningEffortSupported reports whether modelID is expected to honor
// additionalModelRequestFields.reasoning_config:"high" — true unless modelID matches
// reasoningEffortExempt. Verified broadly, not universally: sweeping every reasoning-capable
// chat model available in this account's Bedrock catalog, 9 of 12 spanning DeepSeek,
// Zhipu/GLM (5, 4.7, 4.7-flash), Moonshot/Kimi, MiniMax, Alibaba/Qwen3, NVIDIA Nemotron (the
// larger variant), and OpenAI gpt-oss all turned on visible chain-of-thought for this exact
// field/value regardless of vendor. That consistency across unrelated model providers is
// itself evidence Bedrock translates this specific field server-side rather than every
// vendor happening to use the identical native parameter name — GLM-5's own
// ValidationException on a malformed value once named the backend param "reasoning_effort"
// for a request that sent "reasoning_config", i.e. Bedrock renamed it in transit. So an
// unverified model defaults to "assume it works"; ImprovePromptInstructions'
// ValidationException retry is the safety net for a model where that assumption is wrong.
func reasoningEffortSupported(modelID string) bool {
	lower := strings.ToLower(modelID)
	for _, s := range reasoningEffortExempt {
		if strings.Contains(lower, s) {
			return false
		}
	}
	return true
}

// ReasoningEffortLevels returns the non-off SettingImproveReasoningEffort values modelID
// supports — today always []string{ReasoningEffortOn} for a supported model (see
// reasoningEffortSupported), since no model in reasoningEffortExempt's sibling set has been
// verified to have a real graduated ladder rather than a bare on/off switch (GLM-5
// specifically: "none"/"low"/"medium" were all indistinguishable from omitting the field,
// only "high" did anything — see reasoningEffortFields). Returns nil for an exempt model —
// the Settings UI reads that as "reasoning effort isn't controllable here" and disables the
// control rather than offering a choice that would silently do nothing.
func ReasoningEffortLevels(modelID string) []string {
	if !reasoningEffortSupported(modelID) {
		return nil
	}
	return []string{ReasoningEffortOn}
}

// reasoningEffortFields returns the AdditionalModelRequestFields map that turns reasoning on
// for modelID at the given effort. Returns nil for ReasoningEffortOff, an exempt model, or
// any effort other than ReasoningEffortOn — all safe no-ops, matching reasoningOff's own
// "unmatched is a no-op, not an error" behavior. Callers must not treat a nil result here as
// license to fall back to reasoningOff: asking for reasoning and suppressing it are opposite
// intents, so an unsupported effort should leave the model at its own default, not invert
// into suppression.
func reasoningEffortFields(modelID, effort string) map[string]any {
	if effort != ReasoningEffortOn || !reasoningEffortSupported(modelID) {
		return nil
	}
	return map[string]any{"reasoning_config": "high"}
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
