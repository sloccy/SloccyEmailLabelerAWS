package llm

import "strings"

// CostUnknown is the sentinel InputCostPer1M for models with no known price.
// It sorts last in the model dropdown.
const CostUnknown = -1.0

// priceRule maps a distinctive model-id substring to its on-demand input price
// per 1M tokens (USD, us-east-1). Rules are checked most-specific first so, e.g.,
// "claude-3-5-haiku" wins over "claude-3". Prices are approximate list prices and
// only drive dropdown ordering/labeling, not billing.
type priceRule struct {
	substr string
	price  float64
}

var priceRules = []priceRule{
	// Amazon Nova
	{"nova-micro", 0.035},
	{"nova-2-lite", 0.06},
	{"nova-lite", 0.06},
	{"nova-premier", 2.50},
	{"nova-pro", 0.80},
	// Anthropic Claude (order: more specific first)
	{"claude-3-5-haiku", 0.80},
	{"claude-3-haiku", 0.25},
	{"claude-haiku-4", 1.00},
	{"claude-3-5-sonnet", 3.00},
	{"claude-3-7-sonnet", 3.00},
	{"claude-3-sonnet", 3.00},
	{"claude-sonnet-4", 3.00},
	{"claude-sonnet-5", 3.00},
	{"claude-3-opus", 15.00},
	{"claude-opus-4", 15.00},
	// Meta Llama
	{"llama3-1-8b", 0.22},
	{"llama3-2-1b", 0.10},
	{"llama3-2-3b", 0.15},
	{"llama3-2-11b", 0.16},
	{"llama3-2-90b", 0.72},
	{"llama3-3-70b", 0.72},
	{"llama3-1-70b", 0.72},
	{"llama3-1-405b", 2.40},
	{"llama3-8b", 0.30},
	{"llama3-70b", 2.65},
	{"llama4-scout", 0.17},
	{"llama4-maverick", 0.24},
	// Mistral
	{"mistral-7b", 0.15},
	{"mixtral-8x7b", 0.45},
	{"mistral-large", 4.00},
	{"mistral-small", 0.15},
	{"pixtral-large", 2.00},
	// Cohere
	{"command-r-plus", 3.00},
	{"command-r", 0.50},
	// AI21 Jamba
	{"jamba-1-5-large", 2.00},
	{"jamba-1-5-mini", 0.20},
	// DeepSeek (R1 input ≈ $1.35/1M)
	{"deepseek", 1.35},
	// OpenAI open-weight (gpt-oss on Bedrock)
	{"gpt-oss-120b", 0.15},
	{"gpt-oss-20b", 0.07},
	// Writer Palmyra
	{"palmyra-x5", 2.50},
	{"palmyra-x4", 2.50},
}

// inputCostPer1M returns the on-demand input price per 1M tokens for a model ID,
// or CostUnknown if the model isn't in the price table. It ignores any cross-region
// inference-profile prefix (e.g. "us.", "eu.", "apac.").
func inputCostPer1M(modelID string) float64 {
	id := strings.ToLower(modelID)
	for _, r := range priceRules {
		if strings.Contains(id, r.substr) {
			return r.price
		}
	}
	return CostUnknown
}
