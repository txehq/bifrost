package datasheet

import (
	"reflect"
	"regexp"
	"strings"
	"sync"

	"github.com/capsohq/bifrost/core/schemas"
	configstoreTables "github.com/capsohq/bifrost/framework/configstore/tables"
)

// Output tokens per image for image models whose datasheet row prices images only per image
// token (fork, TXE-7492). A refused image answer (TXE-7302) has no usage to read, so its estimate
// needs a token count per image to apply the row's output_cost_per_image_token to. Only the
// response-limit estimate reads this table. A delivered answer is priced from the vendor's usage.
//
// Each figure is the vendor's published number, or derived from it as the comment says, read on
// the date given. A model with no published figure has no entry unless its comment says why: the
// check in estimatedimagetokens_test.go fails when a token-priced image row in the upstream
// datasheet has neither an entry nor an exemption. Operators cannot override these figures; if they
// ever need to, they move to a datasheet field.

// estimatedImageTokens is one model family's output tokens per image, by quality then size.
type estimatedImageTokens struct {
	tokens map[string]map[string]int // quality -> size -> output tokens per image
	floor  bool                      // a floor borrowed from another model, not this model's figure
}

// perImage returns the tokens for one image at size and quality. A size or quality the table does
// not list (auto, unset, or one the vendor publishes no figure for) takes the dearest figure the
// table holds for what is known, so the estimate fails closed.
func (e estimatedImageTokens) perImage(size, quality string) int {
	dearest := func(bySize map[string]int) int {
		most := 0
		for _, tokens := range bySize {
			most = max(most, tokens)
		}
		return most
	}
	pick := func(bySize map[string]int) int {
		if tokens, ok := bySize[size]; ok {
			return tokens
		}
		return dearest(bySize)
	}
	if bySize, ok := e.tokens[quality]; ok {
		return pick(bySize)
	}
	most := 0
	for _, bySize := range e.tokens {
		most = max(most, pick(bySize))
	}
	return most
}

var (
	// gptImageTokensBeforeGPTImage2 is OpenAI's table for "GPT Image models prior to gpt-image-2"
	// (https://developers.openai.com/api/docs/guides/image-generation.md, "Models prior to
	// gpt-image-2", read 2026-11-16). It matches the per-image prices published for gpt-image-1
	// ($0.167 high 1024x1024 at $40/M), gpt-image-1.5 and chatgpt-image-latest ($0.133 at $32/M).
	gptImageTokensBeforeGPTImage2 = estimatedImageTokens{tokens: map[string]map[string]int{
		"low":    {"1024x1024": 272, "1024x1536": 408, "1536x1024": 400},
		"medium": {"1024x1024": 1056, "1024x1536": 1584, "1536x1024": 1568},
		"high":   {"1024x1024": 4160, "1024x1536": 6240, "1536x1024": 6208},
	}}

	// gptImage1MiniTokens is derived from gpt-image-1-mini's per-image prices
	// (https://developers.openai.com/api/docs/models/gpt-image-1-mini, read 2026-11-16) at its $8/M
	// image output rate (https://developers.openai.com/api/docs/pricing, read 2026-11-16), rounded
	// up: $0.036 high 1024x1024 is 4500 tokens. Its prices do not match the token table above.
	gptImage1MiniTokens = estimatedImageTokens{tokens: map[string]map[string]int{
		"low":    {"1024x1024": 625, "1024x1536": 750, "1536x1024": 750},
		"medium": {"1024x1024": 1375, "1024x1536": 1875, "1536x1024": 1875},
		"high":   {"1024x1024": 4500, "1024x1536": 6500, "1536x1024": 6500},
	}}

	// gptImage2Tokens is derived from gpt-image-2's per-image prices in "Historical output costs"
	// (https://developers.openai.com/api/docs/guides/image-generation.md, read 2026-11-16) at its
	// $30/M image output rate (https://developers.openai.com/api/docs/pricing, read 2026-11-16),
	// rounded up: $0.211 high 1024x1024 is 7034 tokens. OpenAI lists only these three sizes; any
	// other size takes the dearest.
	gptImage2Tokens = estimatedImageTokens{tokens: map[string]map[string]int{
		"low":    {"1024x1024": 200, "1024x1536": 167, "1536x1024": 167},
		"medium": {"1024x1024": 1767, "1024x1536": 1367, "1536x1024": 1367},
		"high":   {"1024x1024": 7034, "1024x1536": 5500, "1536x1024": 5500},
	}}

	// gptImage2FloorTokens is a FLOOR, not the vendor's figure. OpenAI publishes no per-image
	// figure for gpt-image-2.5 or gpt-image-2.6 (their model pages and the image generation guide,
	// read 2026-11-16) and says their token consumption differs by model and quality, while their
	// token rates match GPT Image 2. Every image is charged gpt-image-2's dearest published figure.
	// It under-charges the xhigh and max qualities and sizes above 1536 pixels, which consume more.
	gptImage2FloorTokens = estimatedImageTokens{
		tokens: map[string]map[string]int{"high": {"1024x1024": 7034}},
		floor:  true,
	}

	// estimatedImageTokenFamilies maps a model name, without a -YYYY-MM-DD snapshot suffix and
	// lowercased, to its figures. The databricks- names are Databricks' serving names for the same
	// OpenAI models.
	estimatedImageTokenFamilies = map[string]estimatedImageTokens{
		"gpt-image-1":                 gptImageTokensBeforeGPTImage2,
		"databricks-gpt-image-1":      gptImageTokensBeforeGPTImage2,
		"gpt-image-1.5":               gptImageTokensBeforeGPTImage2,
		"databricks-gpt-image-1-5":    gptImageTokensBeforeGPTImage2,
		"chatgpt-image-latest":        gptImageTokensBeforeGPTImage2,
		"gpt-image-1-mini":            gptImage1MiniTokens,
		"databricks-gpt-image-1-mini": gptImage1MiniTokens,
		"gpt-image-2":                 gptImage2Tokens,
		"databricks-gpt-image-2":      gptImage2Tokens,
		"gpt-image-2.5-flare":         gptImage2FloorTokens,
		"gpt-image-2.5-sunburst":      gptImage2FloorTokens,
		"gpt-image-2.6-flare":         gptImage2FloorTokens,
		"gpt-image-2.6-sunburst":      gptImage2FloorTokens,
	}

	modelSnapshotDateSuffix = regexp.MustCompile(`-\d{4}-\d{2}-\d{2}$`)
)

// estimatedImageTokensFor returns the figures for a catalog model name, if it has any.
func estimatedImageTokensFor(model string) (estimatedImageTokens, bool) {
	figures, ok := estimatedImageTokenFamilies[modelSnapshotDateSuffix.ReplaceAllString(strings.ToLower(model), "")]
	return figures, ok
}

// estimateImageTokenCost prices an estimate on a row that prices images only per image token: the
// estimated images at the table's tokens per image (the dearest over sizes and qualities when the
// request named none) at the row's output_cost_per_image_token. It returns 0 for a row with no
// image token rate, a row that sets any per-image or per-pixel output price (zero included: an
// explicit 0 is a free image, not a missing price), or a model with no figures.
func (s *Store) estimateImageTokenCost(estimate *schemas.EstimatedImageUsage, routingInfo schemas.RoutingInfo, requestType schemas.RequestType, scopes LookupScopes, sizes, qualities []string) float64 {
	pricing := s.resolvePricing(routingInfo, requestType, scopes)
	if pricing == nil || pricing.OutputCostPerImageToken == nil || *pricing.OutputCostPerImageToken <= 0 || setsPerImageOutputPrice(pricing) {
		return 0
	}
	figures, ok := estimatedImageTokensFor(pricing.Model)
	if !ok {
		return 0
	}
	tokens := 0
	for _, size := range sizes {
		for _, quality := range qualities {
			tokens = max(tokens, figures.perImage(size, quality))
		}
	}
	// The product is taken in float64: n comes from the client, and an int product can wrap to a
	// negative or tiny charge.
	return float64(estimate.OutputImages) * float64(tokens) * *pricing.OutputCostPerImageToken
}

// perImageOutputPriceFields are the pricing row's per-image and per-pixel output price fields:
// every *float64 field whose name starts OutputCostPerImage or OutputCostPerPixel, except the
// per-image-token rates. They are read from the struct so a field added later is covered.
var perImageOutputPriceFields = sync.OnceValue(func() []int {
	var fields []int
	t := reflect.TypeFor[configstoreTables.TableModelPricing]()
	for i := range t.NumField() {
		f := t.Field(i)
		if f.Type != reflect.TypeFor[*float64]() || strings.Contains(f.Name, "Token") {
			continue
		}
		if strings.HasPrefix(f.Name, "OutputCostPerImage") || strings.HasPrefix(f.Name, "OutputCostPerPixel") {
			fields = append(fields, i)
		}
	}
	return fields
})

// setsPerImageOutputPrice reports whether the row sets any per-image or per-pixel output price.
func setsPerImageOutputPrice(pricing *configstoreTables.TableModelPricing) bool {
	v := reflect.ValueOf(pricing).Elem()
	for _, i := range perImageOutputPriceFields() {
		if !v.Field(i).IsNil() {
			return true
		}
	}
	return false
}
