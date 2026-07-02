package llm

import "testing"

func TestBedrockPriceBaseID(t *testing.T) {
	cases := []struct {
		usageType string
		base      string
		direction string
		tier      string
		ok        bool
	}{
		{"USE1-qwen.qwen3-next-80b-a3b-instruct-mantle-input-tokens-flex", "qwen.qwen3-next-80b-a3b-instruct", "input", "flex", true},
		{"USE1-nvidia.nemotron-super-3-120b-mantle-output-tokens-flex", "nvidia.nemotron-super-3-120b", "output", "flex", true},
		{"EU-qwen.qwen3-coder-480b-a35b-instruct-mantle-input-tokens-flex", "qwen.qwen3-coder-480b-a35b-instruct", "input", "flex", true},
		{"USE1-mistral.voxtral-small-24b-2507-mantle-input-tokens-standard", "mistral.voxtral-small-24b-2507", "input", "standard", true},
		{"USE1-xai.grok-4.3-mantle-cache-read-tokens-flex", "", "", "", false}, // not input/output
		{"USE1-Claude2.0-input-tokens", "claude2.0", "input", "", true},        // legacy, no "-mantle-", no tier suffix
		{"USE1-NovaLite-input-tokens-priority", "novalite", "input", "priority", true},
		{"USE1-NovaLite-input-tokens-custom-model", "", "", "", false}, // legacy variant SKU, not a plain price
	}
	for _, c := range cases {
		base, direction, tier, ok := bedrockPriceBaseID(c.usageType)
		if base != c.base || direction != c.direction || tier != c.tier || ok != c.ok {
			t.Errorf("bedrockPriceBaseID(%q) = (%q, %q, %q, %v), want (%q, %q, %q, %v)",
				c.usageType, base, direction, tier, ok, c.base, c.direction, c.tier, c.ok)
		}
	}
}

func TestPricingCatalogLookups(t *testing.T) {
	// Catalog keys are normalized (see normalizeKey/candidateKeys) — this mirrors what
	// fetchPricingCatalog actually stores, regardless of which of the two Price List
	// naming conventions (dotted "mantle" id vs legacy PascalCase) supplied the price.
	cat := &pricingCatalog{
		inputPricePer1M: map[string]float64{
			normalizeKey("qwen.qwen3-next-80b-a3b-instruct"): 0.07,
			normalizeKey("nvidia.nemotron-super-3-120b"):     0.15,
			normalizeKey("NovaLite"):                         0.06, // legacy-style key: no dot to strip
		},
		flexCapable: map[string]bool{
			normalizeKey("qwen.qwen3-next-80b-a3b-instruct"): true,
		},
	}

	// Exact match.
	if got := cat.inputCostPer1M("qwen.qwen3-next-80b-a3b-instruct"); got != 0.07 {
		t.Errorf("inputCostPer1M exact match = %v, want 0.07", got)
	}
	// Bedrock id carries a version suffix the price-list id doesn't — stripped as noise,
	// so this is still an exact match once normalized.
	if got := cat.inputCostPer1M("qwen.qwen3-next-80b-a3b-instruct-v1:0"); got != 0.07 {
		t.Errorf("inputCostPer1M version-suffix match = %v, want 0.07", got)
	}
	// Bedrock's dotted id carries a vendor prefix ("amazon.") the legacy price-list
	// name doesn't — resolved via the vendor-stripped candidate key.
	if got := cat.inputCostPer1M("amazon.nova-lite-v1:0"); got != 0.06 {
		t.Errorf("inputCostPer1M legacy vendor-prefix match = %v, want 0.06", got)
	}
	// No match at all.
	if got := cat.inputCostPer1M("some.unknown-model-v9:0"); got != CostUnknown {
		t.Errorf("inputCostPer1M unknown = %v, want CostUnknown", got)
	}

	if !cat.isFlexCapable("qwen.qwen3-next-80b-a3b-instruct-v1:0") {
		t.Error("isFlexCapable() = false, want true for known flex model")
	}
	if cat.isFlexCapable("nvidia.nemotron-super-3-120b") {
		t.Error("isFlexCapable() = true, want false for non-flex model")
	}
}

func TestKeysMatch(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{normalizeKey("qwen.qwen3-next-80b-a3b-instruct"), normalizeKey("qwen.qwen3-next-80b-a3b-instruct-v1:0"), true},
		{normalizeKey("qwen.qwen3-next-80b-a3b-instruct-v1:0"), normalizeKey("qwen.qwen3-next-80b-a3b-instruct"), true},
		{normalizeKey("nova"), normalizeKey("nova-premier"), false},                          // too short to safely prefix-match
		{normalizeKey("llama3-1-70b"), normalizeKey("llama3-2-70b"), false},                  // same family, different model
		{normalizeKey("MistralLarge"), normalizeKey("mistral-large-3-675b-instruct"), false}, // prefix swallows into a version number — must not match
		{normalizeKey("llama3-1-70b"), normalizeKey("llama3-1-70b-instruct"), true},          // legit suffix drift
	}
	for _, c := range cases {
		if got := keysMatch(c.a, c.b); got != c.want {
			t.Errorf("keysMatch(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestBaseModelID(t *testing.T) {
	cases := map[string]string{
		"us.anthropic.claude-opus-4-8":      "anthropic.claude-opus-4-8",
		"global.amazon.nova-2-lite-v1:0":    "amazon.nova-2-lite-v1:0",
		"us.stability.stable-image-inpaint": "stability.stable-image-inpaint",
		"amazon.nova-micro-v1:0":            "amazon.nova-micro-v1:0", // no prefix
		"apac.anthropic.claude-3-haiku":     "anthropic.claude-3-haiku",
	}
	for in, want := range cases {
		if got := baseModelID(in); got != want {
			t.Errorf("baseModelID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestProfileRegion(t *testing.T) {
	cases := map[string]string{
		"us.anthropic.claude-opus-4-8":    "us",
		"global.amazon.nova-2-lite-v1:0":  "global",
		"eu.anthropic.claude-3-haiku":     "eu",
		"apac.anthropic.claude-3-haiku":   "apac",
		"us-gov.anthropic.claude-3-haiku": "us-gov",
		"amazon.nova-micro-v1:0":          "", // bare foundation-model id, no profile
		"google.gemma-3-4b-it":            "",
	}
	for in, want := range cases {
		if got := profileRegion(in); got != want {
			t.Errorf("profileRegion(%q) = %q, want %q", in, got, want)
		}
	}
}
