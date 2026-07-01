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
