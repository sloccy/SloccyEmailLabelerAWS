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

// directionInput identifies the input-token price direction, as opposed to output.
const directionInput = "input"

// pricingRegion is where the AWS Price List API is queried. It's a single global
// catalog (the API itself only runs in us-east-1/ap-south-1, regardless of which
// regions the priced models actually run in) — so this has no bearing on the geo
// policy applied elsewhere (see ListAvailableModels, ModelOption.ProfileRegion, and
// classifyModelAllowed). Note the catalog still isn't exhaustive: Anthropic's current
// Claude lineup (3.5+, 4.x, 5) has no published Bedrock on-demand SKU at all under any
// name, so those models always resolve to CostUnknown regardless of matching logic.
const pricingRegion = "us-east-1"

// usageTypeRegionPrefixRe strips the Price List usage type's region shorthand (e.g.
// "USE1-", "EU-", "APS4-", "SAE1-") — always a leading run of uppercase letters/digits,
// distinct from the model id that follows.
var usageTypeRegionPrefixRe = regexp.MustCompile(`^[A-Z][A-Z0-9]*-`)

// AWS's Bedrock Price List uses two incompatible naming conventions for the same
// models, and this package has to bridge both to Bedrock's own model ids:
//
//   - Current-generation ("mantle") entries: a dotted, lowercase id matching Bedrock's
//     own model id space, e.g. "qwen.qwen3-next-80b-a3b-instruct-mantle-input-tokens-standard",
//     with an explicit service_tier attribute (standard/priority/batch/flex).
//   - Legacy entries: a bare PascalCase family name with no dot, e.g.
//     "NovaLite-input-tokens" or "Llama4-Maverick-17B-input-tokens-batch", with no
//     service_tier attribute — the tier (if any) is only encoded as a usage-type suffix.
//
// bedrockPriceBaseID extracts the base id and on-demand tier from a usage type,
// handling both forms. It intentionally rejects anything beyond a clean tier suffix
// (cache-read/cross-region/custom-model/ProvisionedThroughput/Customization/RFT-Training
// SKUs share the same base name) so those variant prices can never be mistaken for the
// plain on-demand price.
func bedrockPriceBaseID(usageType string) (base, direction, tier string, ok bool) {
	s := usageTypeRegionPrefixRe.ReplaceAllString(usageType, "")

	var rest string
	if before, after, found := strings.Cut(s, "-mantle-"); found {
		base = strings.ToLower(before)
		rest = after
	} else {
		m := legacyDirectionRe.FindStringIndex(s)
		if m == nil {
			return "", "", "", false
		}
		base = strings.ToLower(s[:m[0]])
		rest = s[m[0]+1:] // +1 skips the leading "-" the regex matched on
	}

	var tail string
	switch {
	case strings.HasPrefix(rest, "input-tokens"):
		direction = directionInput
		tail = rest[len("input-tokens"):]
	case strings.HasPrefix(rest, "output-tokens"):
		direction = "output"
		tail = rest[len("output-tokens"):]
	default:
		return "", "", "", false
	}
	if tail == "" {
		return base, direction, "", true
	}
	for _, t := range []string{"standard", "priority", "batch", "flex"} {
		if tail == "-"+t {
			return base, direction, t, true
		}
	}
	return "", "", "", false
}

var legacyDirectionRe = regexp.MustCompile(`-(?:input-tokens|output-tokens)`)

// trailingNoiseRe strips id suffixes that are noise for matching, not part of the
// model family/size: API version markers ("-v1:0"), context-window/modality markers
// (":128k", ":8k", ":mm"), and release-date stamps ("-20240307"). Applied repeatedly
// so several can chain, e.g. "claude-3-haiku-20240307-v1:0" strips both.
var trailingNoiseRe = regexp.MustCompile(`(?:-v?\d+:\d+|:(?:\d+k|mm)|-\d{8})$`)

func stripTrailingNoise(id string) string {
	for {
		loc := trailingNoiseRe.FindStringIndex(id)
		if loc == nil {
			return id
		}
		id = id[:loc[0]]
	}
}

// normalizeKey lowercases and strips every non-alphanumeric character, collapsing the
// separator differences between id spaces ("." vs "-" vs ":" vs PascalCase word breaks).
func normalizeKey(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// candidateKeys returns the normalized key(s) a base id could be found under: the full
// id, plus (for dotted ids like "amazon.nova-lite" or "meta.llama3-1-70b-instruct") the
// portion after the first dot. Bedrock's own dotted ids and the Price List's dotted
// "mantle" ids share this shape; legacy PascalCase Price List names have no dot, so
// they only ever produce one key. Two keys are needed because some vendor prefixes
// matter for matching (Bedrock's "amazon." doesn't appear in legacy names like
// "NovaLite") while others coincide with the family name anyway (Price List's
// "DeepSeek-R1" already includes the vendor word that Bedrock's "deepseek." supplies).
func candidateKeys(id string) []string {
	id = stripTrailingNoise(id)
	keys := []string{normalizeKey(id)}
	if _, after, found := strings.Cut(id, "."); found {
		if stripped := normalizeKey(after); stripped != keys[0] {
			keys = append(keys, stripped)
		}
	}
	return keys
}

// pricingCatalog is a snapshot of Bedrock's US on-demand and flex-tier pricing, built
// entirely from the AWS Price List API — no hardcoded model data. Keys are normalized
// (see normalizeKey/candidateKeys) so both Price List naming conventions resolve to the
// same entry.
type pricingCatalog struct {
	inputPricePer1M      map[string]float64
	outputPricePer1M     map[string]float64
	flexInputPricePer1M  map[string]float64
	flexOutputPricePer1M map[string]float64
	flexCapable          map[string]bool
}

// fetchPricingCatalog queries the AWS Price List API for Amazon Bedrock's us-east-1
// on-demand catalog and derives per-model standard and flex-tier input pricing.
func fetchPricingCatalog(ctx context.Context, pc *pricing.Client) (*pricingCatalog, error) {
	cat := &pricingCatalog{
		inputPricePer1M:      map[string]float64{},
		outputPricePer1M:     map[string]float64{},
		flexInputPricePer1M:  map[string]float64{},
		flexOutputPricePer1M: map[string]float64{},
		flexCapable:          map[string]bool{},
	}

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
			base, direction, tier, ok := bedrockPriceBaseID(e.Product.Attributes.UsageType)
			if !ok {
				continue
			}
			// The usage-type tail (see bedrockPriceBaseID) and the service_tier
			// attribute are two independent signals for the same thing on "mantle"
			// entries; legacy entries only ever carry it via the usage-type tail.
			effTier := e.Product.Attributes.ServiceTier
			if effTier == "" {
				effTier = tier
			}
			if effTier == "flex" {
				for _, k := range candidateKeys(base) {
					cat.flexCapable[k] = true
				}
				if price := firstOnDemandPricePer1K(e); price > 0 {
					dst := cat.flexInputPricePer1M
					if direction != directionInput {
						dst = cat.flexOutputPricePer1M
					}
					for _, k := range candidateKeys(base) {
						dst[k] = price * 1000
					}
				}
				continue
			}
			if effTier != "standard" && effTier != "" {
				continue
			}
			price := firstOnDemandPricePer1K(e)
			if price <= 0 {
				continue
			}
			dst := cat.inputPricePer1M
			if direction != directionInput {
				dst = cat.outputPricePer1M
			}
			for _, k := range candidateKeys(base) {
				dst[k] = price * 1000 // $/1K tokens -> $/1M tokens
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

// keysMatch compares two normalized keys with a same-family prefix match, tolerating
// suffix drift between id spaces (see https://github.com/aws/aws-sdk-go-v2/issues/3397).
// Guards against short/generic keys matching unrelated families (e.g. bare "nova"
// matching every Nova variant) by requiring a minimum shared length, and against a
// prefix match swallowing into an adjacent version/size number (e.g. "mistrallarge"
// incorrectly matching "mistrallarge3675binstruct") by requiring the longer key not
// continue with a digit right where the shorter one ends.
func keysMatch(a, b string) bool {
	if a == b {
		return true
	}
	shorter, longer := a, b
	if len(a) > len(b) {
		shorter, longer = b, a
	}
	if len(shorter) < 8 || !strings.HasPrefix(longer, shorter) {
		return false
	}
	if len(longer) == len(shorter) {
		return true
	}
	return longer[len(shorter)] < '0' || longer[len(shorter)] > '9'
}

// lookup finds the first catalog entry matching any candidate key of id, trying exact
// matches (over all candidates) before falling back to the fuzzy keysMatch.
func lookup[V any](cat map[string]V, id string) (V, bool) {
	cands := candidateKeys(id)
	for _, c := range cands {
		if v, ok := cat[c]; ok {
			return v, true
		}
	}
	for _, c := range cands {
		for k, v := range cat {
			if keysMatch(c, k) {
				return v, true
			}
		}
	}
	var zero V
	return zero, false
}

// inputCostPer1M returns the on-demand input price per 1M tokens for a Bedrock base
// model id (region prefix already stripped), or CostUnknown if the catalog has no match.
func (cat *pricingCatalog) inputCostPer1M(baseModelID string) float64 {
	if price, ok := lookup(cat.inputPricePer1M, baseModelID); ok {
		return price
	}
	return CostUnknown
}

// flexCostPer1M returns the flex-tier input price per 1M tokens for a Bedrock base
// model id (region prefix already stripped), or CostUnknown if the catalog has no match.
func (cat *pricingCatalog) flexCostPer1M(baseModelID string) float64 {
	if price, ok := lookup(cat.flexInputPricePer1M, baseModelID); ok {
		return price
	}
	return CostUnknown
}

// outputCostPer1M returns the on-demand output price per 1M tokens for a Bedrock base
// model id (region prefix already stripped), or CostUnknown if the catalog has no match.
func (cat *pricingCatalog) outputCostPer1M(baseModelID string) float64 {
	if price, ok := lookup(cat.outputPricePer1M, baseModelID); ok {
		return price
	}
	return CostUnknown
}

// flexOutputCostPer1M returns the flex-tier output price per 1M tokens for a Bedrock base
// model id (region prefix already stripped), or CostUnknown if the catalog has no match.
func (cat *pricingCatalog) flexOutputCostPer1M(baseModelID string) float64 {
	if price, ok := lookup(cat.flexOutputPricePer1M, baseModelID); ok {
		return price
	}
	return CostUnknown
}

// isFlexCapable reports whether a Bedrock base model id (region prefix already
// stripped) appears in the Price List catalog with a flex-tier SKU.
func (cat *pricingCatalog) isFlexCapable(baseModelID string) bool {
	_, ok := lookup(cat.flexCapable, baseModelID)
	return ok
}
