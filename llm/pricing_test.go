package llm

import "testing"

func TestInputCostPer1M(t *testing.T) {
	cases := []struct {
		id   string
		want float64
	}{
		{"us.amazon.nova-micro-v1:0", 0.035},                  // cross-region prefix stripped by substring match
		{"amazon.nova-lite-v1:0", 0.06},                       // foundation-model form
		{"us.anthropic.claude-3-5-haiku-20241022-v1:0", 0.80}, // specific beats claude-3-haiku
		{"anthropic.claude-3-haiku-20240307-v1:0", 0.25},
		{"anthropic.claude-3-opus-20240229-v1:0", 15.00},
		{"meta.llama3-1-8b-instruct-v1:0", 0.22},
		{"some.unknown-model-v9:0", CostUnknown},
	}
	for _, c := range cases {
		if got := inputCostPer1M(c.id); got != c.want {
			t.Errorf("inputCostPer1M(%q) = %v, want %v", c.id, got, c.want)
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
