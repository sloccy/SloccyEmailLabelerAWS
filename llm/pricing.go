package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/pricing"
	pricingtypes "github.com/aws/aws-sdk-go-v2/service/pricing/types"
)

// CostUnknown is the sentinel InputCostPer1M for models with no known price.
// It sorts last in the model dropdown.
const CostUnknown = -1.0

// pricingRegion is where the AWS Price List API is queried. It's a single global
// catalog (the API itself only runs in us-east-1/ap-south-1, regardless of which
// regions the priced models actually run in), and Bedrock's us-east-1 offering covers
// effectively every model, so this has no bearing on the geo policy applied elsewhere
// (see ListAvailableModels, ModelOption.ProfileRegion, and classifyModelAllowed).
const pricingRegion = "us-east-1"

// usageTypeRegionPrefixRe strips the Price List usage type's region shorthand (e.g.
// "USE1-", "EU-", "APS4-", "SAE1-") — always a leading run of uppercase letters/digits,
// distinct from the lowercase/dotted Bedrock model id that follows.
var usageTypeRegionPrefixRe = regexp.MustCompile(`^[A-Z][A-Z0-9]*-`)

// bedrockPriceBaseID extracts the Bedrock model id embedded in a Price List usage
// type, e.g. "USE1-qwen.qwen3-next-80b-a3b-instruct-mantle-input-tokens-flex" ->
// ("qwen.qwen3-next-80b-a3b-instruct", "input", true). Only "mantle"-branded usage
// types (current-generation Bedrock models) carry a cleanly matchable id; legacy usage
// types (e.g. "USE1-Claude2.0-input-tokens", "USE1-NovaLite-input-tokens-custom-model")
// don't follow this convention and are skipped — those older models aren't flex-eligible
// either, so nothing is lost by not pricing them (they fall back to CostUnknown).
func bedrockPriceBaseID(usageType string) (base, direction string, ok bool) {
	s := usageTypeRegionPrefixRe.ReplaceAllString(usageType, "")
	idx := strings.Index(s, "-mantle-")
	if idx < 0 {
		return "", "", false
	}
	base = strings.ToLower(s[:idx])
	rest := s[idx+len("-mantle-"):]
	switch {
	case strings.HasPrefix(rest, "input-tokens"):
		return base, "input", true
	case strings.HasPrefix(rest, "output-tokens"):
		return base, "output", true
	default:
		// e.g. "-mantle-cache-read-tokens-flex" — not needed for dropdown pricing.
		return "", "", false
	}
}

// pricingCatalog is a snapshot of Bedrock's US on-demand pricing and flex-tier
// eligibility, built entirely from the AWS Price List API — no hardcoded model data.
// Keys are lowercase Bedrock base model ids (region prefix already stripped) as
// recovered by bedrockPriceBaseID.
type pricingCatalog struct {
	inputPricePer1M map[string]float64
	flexCapable     map[string]bool
}

// fetchPricingCatalog queries the AWS Price List API for Amazon Bedrock's us-east-1
// on-demand catalog and derives per-model input pricing and flex-tier eligibility.
func fetchPricingCatalog(ctx context.Context, pc *pricing.Client) (*pricingCatalog, error) {
	cat := &pricingCatalog{inputPricePer1M: map[string]float64{}, flexCapable: map[string]bool{}}

	paginator := pricing.NewGetProductsPaginator(pc, &pricing.GetProductsInput{
		ServiceCode: aws.String("AmazonBedrock"),
		Filters: []pricingtypes.Filter{
			{Type: pricingtypes.FilterTypeTermMatch, Field: aws.String("regionCode"), Value: aws.String(pricingRegion)},
		},
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("get products: %w", err)
		}
		for _, raw := range page.PriceList {
			var e priceListEntry
			if err := json.Unmarshal([]byte(raw), &e); err != nil {
				continue
			}
			base, direction, ok := bedrockPriceBaseID(e.Product.Attributes.UsageType)
			if !ok {
				continue
			}
			if e.Product.Attributes.ServiceTier == "flex" {
				cat.flexCapable[base] = true
				continue // flex pricing itself isn't shown in the dropdown, only standard price
			}
			if direction != "input" {
				continue
			}
			if price := firstOnDemandPricePer1K(e); price > 0 {
				cat.inputPricePer1M[base] = price * 1000 // $/1K tokens -> $/1M tokens
			}
		}
	}
	return cat, nil
}

// priceListEntry is the subset of a Price List product JSON blob (one element of
// GetProductsOutput.PriceList) this package reads. See:
// https://docs.aws.amazon.com/awsaccountbilling/latest/aboutv2/using-price-list-query-api.html
type priceListEntry struct {
	Product struct {
		Attributes struct {
			UsageType   string `json:"usagetype"`
			ServiceTier string `json:"service_tier"`
		} `json:"attributes"`
	} `json:"product"`
	Terms struct {
		OnDemand map[string]struct {
			PriceDimensions map[string]struct {
				PricePerUnit struct {
					USD string `json:"USD"`
				} `json:"pricePerUnit"`
			} `json:"priceDimensions"`
		} `json:"OnDemand"`
	} `json:"terms"`
}

func firstOnDemandPricePer1K(e priceListEntry) float64 {
	for _, term := range e.Terms.OnDemand {
		for _, dim := range term.PriceDimensions {
			if v, err := strconv.ParseFloat(dim.PricePerUnit.USD, 64); err == nil {
				return v
			}
		}
	}
	return 0
}

// pricingIDsMatch compares a Bedrock base model id against a Price List base id with a
// same-family prefix match, tolerating version-suffix drift between the two id spaces
// (see https://github.com/aws/aws-sdk-go-v2/issues/3397). Guards against short/generic
// ids matching unrelated families (e.g. bare "nova" matching every Nova variant) by
// requiring a minimum shared length.
func pricingIDsMatch(a, b string) bool {
	if a == b {
		return true
	}
	shorter, longer := a, b
	if len(a) > len(b) {
		shorter, longer = b, a
	}
	if len(shorter) < 8 {
		return false
	}
	return strings.HasPrefix(longer, shorter)
}

// inputCostPer1M returns the on-demand input price per 1M tokens for a Bedrock base
// model id (region prefix already stripped), or CostUnknown if the catalog has no match.
func (cat *pricingCatalog) inputCostPer1M(baseModelID string) float64 {
	id := strings.ToLower(baseModelID)
	if price, ok := cat.inputPricePer1M[id]; ok {
		return price
	}
	for base, price := range cat.inputPricePer1M {
		if pricingIDsMatch(id, base) {
			return price
		}
	}
	return CostUnknown
}

// isFlexCapable reports whether a Bedrock base model id (region prefix already
// stripped) appears in the Price List catalog with a flex-tier SKU.
func (cat *pricingCatalog) isFlexCapable(baseModelID string) bool {
	id := strings.ToLower(baseModelID)
	if cat.flexCapable[id] {
		return true
	}
	for base := range cat.flexCapable {
		if pricingIDsMatch(id, base) {
			return true
		}
	}
	return false
}
