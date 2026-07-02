package llm

import "testing"

func TestBedrockPriceBaseID(t *testing.T) {
	cases := []struct {
		usageType string
		base      string
		direction string
		ok        bool
	}{
		{"USE1-qwen.qwen3-next-80b-a3b-instruct-mantle-input-tokens-flex", "qwen.qwen3-next-80b-a3b-instruct", "input", true},
		{"USE1-nvidia.nemotron-super-3-120b-mantle-output-tokens-flex", "nvidia.nemotron-super-3-120b", "output", true},
		{"EU-qwen.qwen3-coder-480b-a35b-instruct-mantle-input-tokens-flex", "qwen.qwen3-coder-480b-a35b-instruct", "input", true},
		{"USE1-mistral.voxtral-small-24b-2507-mantle-input-tokens-standard", "mistral.voxtral-small-24b-2507", "input", true},
		{"USE1-xai.grok-4.3-mantle-cache-read-tokens-flex", "", "", false}, // not input/output
		{"USE1-Claude2.0-input-tokens", "", "", false},                     // legacy, no "-mantle-"
		{"USE1-NovaLite-input-tokens-custom-model", "", "", false},
	}
	for _, c := range cases {
		base, direction, ok := bedrockPriceBaseID(c.usageType)
		if base != c.base || direction != c.direction || ok != c.ok {
			t.Errorf("bedrockPriceBaseID(%q) = (%q, %q, %v), want (%q, %q, %v)",
				c.usageType, base, direction, ok, c.base, c.direction, c.ok)
		}
	}
}

func TestPricingCatalogLookups(t *testing.T) {
	cat := &pricingCatalog{
		inputPricePer1M: map[string]float64{
			"qwen.qwen3-next-80b-a3b-instruct": 0.07,
			"nvidia.nemotron-super-3-120b":     0.15,
		},
		flexCapable: map[string]bool{
			"qwen.qwen3-next-80b-a3b-instruct": true,
		},
	}

	// Exact match.
	if got := cat.inputCostPer1M("qwen.qwen3-next-80b-a3b-instruct"); got != 0.07 {
		t.Errorf("inputCostPer1M exact match = %v, want 0.07", got)
	}
	// Bedrock id carries a version suffix the price-list id doesn't — prefix match.
	if got := cat.inputCostPer1M("qwen.qwen3-next-80b-a3b-instruct-v1:0"); got != 0.07 {
		t.Errorf("inputCostPer1M version-suffix match = %v, want 0.07", got)
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

func TestPricingIDsMatch(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"qwen.qwen3-next-80b-a3b-instruct", "qwen.qwen3-next-80b-a3b-instruct-v1:0", true},
		{"qwen.qwen3-next-80b-a3b-instruct-v1:0", "qwen.qwen3-next-80b-a3b-instruct", true},
		{"nova", "nova-premier", false}, // too short to safely prefix-match
		{"amazon.nova-pro-v1:0", "amazon.nova-premier-v1:0", false},
	}
	for _, c := range cases {
		if got := pricingIDsMatch(c.a, c.b); got != c.want {
			t.Errorf("pricingIDsMatch(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
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
