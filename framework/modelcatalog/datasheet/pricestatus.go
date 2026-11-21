package datasheet

import (
	"reflect"
	"strings"
	"sync"

	"github.com/capsohq/bifrost/core/schemas"
	configstoreTables "github.com/capsohq/bifrost/framework/configstore/tables"
)

// PriceStatus is what the price book knows about a model's price for one kind of request,
// before any answer exists (fork, TXE-7957).
type PriceStatus int

const (
	// Unpriced: no row and no pricing override resolves, or the row that resolves sets no price
	// at all. Any answer it delivers is charged $0.
	Unpriced PriceStatus = iota
	// Free: the row sets prices and every one of them is $0, so a $0 charge is the price.
	Free
	// Priced: the row sets at least one price above $0.
	Priced
)

func (s PriceStatus) String() string {
	switch s {
	case Free:
		return "free"
	case Priced:
		return "priced"
	default:
		return "unpriced"
	}
}

// anyModePricingRequestTypes are the modes a request whose pricing mode is not known before its
// answer is priced in: a passthrough body or a batch line can be any of them. Same list, same
// order, as GetPricingEntryForModel.
var anyModePricingRequestTypes = []schemas.RequestType{
	schemas.TextCompletionRequest,
	schemas.ChatCompletionRequest,
	schemas.ResponsesRequest,
	schemas.EmbeddingRequest,
	schemas.RerankRequest,
	schemas.DecisionRequest,
	schemas.SpeechRequest,
	schemas.TranscriptionRequest,
	schemas.ImageGenerationRequest,
	schemas.ImageEditRequest,
	schemas.ImageVariationRequest,
	schemas.VideoGenerationRequest,
	schemas.OCRRequest,
}

// PriceStatus reports whether a (provider, model) can be priced for requestType, through the same
// lookup pricing uses: the row under the provider (and every fallback resolvePricing tries), then
// the pricing overrides in scope, including override-only pricing for a model with no row.
//
// A request type with no pricing mode of its own (passthrough, batch create) reports the best
// status over every mode a body or batch line can be priced in.
//
// It reads which prices are set, not whether an answer will carry what they price: a row priced
// only per token prices an answer with no token usage at $0. Governance charges such an answer
// as unpriced after the fact (TXE-7957).
func (s *Store) PriceStatus(provider schemas.ModelProvider, model string, requestType schemas.RequestType, scopes *LookupScopes) PriceStatus {
	_, status := s.PriceIdentity(provider, model, requestType, scopes)
	return status
}

// PriceIdentity is PriceStatus with the identity of the pricing row that decided it: the row's
// provider, mode and model as the price book files them, so every spelling of a model a lookup
// folds onto one row (case, aliases, a custom provider's base) has one identity. A request priced
// by an override alone, with no row, is identified by the override that matched it, so every model
// one wildcard override prices shares one identity. Empty when unpriced with no row at all.
func (s *Store) PriceIdentity(provider schemas.ModelProvider, model string, requestType schemas.RequestType, scopes *LookupScopes) (string, PriceStatus) {
	if model == "" {
		return "", Unpriced
	}
	var lookupScopes LookupScopes
	if scopes != nil {
		lookupScopes = *scopes
	}
	if lookupScopes.Provider == "" {
		lookupScopes.Provider = string(provider)
	}
	routing := schemas.RoutingInfo{Provider: provider, Model: model}
	if normalizeRequestType(requestType) != "unknown" {
		row := s.resolvePricing(routing, requestType, lookupScopes)
		return s.rowIdentity(row, model, requestType, lookupScopes), rowPriceStatus(row)
	}
	bestIdentity, best := "", Unpriced
	for _, mode := range anyModePricingRequestTypes {
		row := s.resolvePricing(routing, mode, lookupScopes)
		if row == nil {
			continue
		}
		if status := rowPriceStatus(row); bestIdentity == "" || status > best {
			best, bestIdentity = status, s.rowIdentity(row, model, mode, lookupScopes)
		}
		if best == Priced {
			break
		}
	}
	return bestIdentity, best
}

// PriceIdentityForRouting is the identity of the row accounting prices an answer on for routing:
// resolvePricing on the same routing (a key alias's catalog name and wire model id, a server-side
// fallback), with overrides keyed as resolvePricing keys them (the fallback, else the alias's wire
// model id, else the model). Reading each candidate model alone would key the override by the
// catalog name, so two keys with different overrides on their wire ids would share one identity
// (TXE-7977). Empty when no row prices it.
func (s *Store) PriceIdentityForRouting(routing schemas.RoutingInfo, requestType schemas.RequestType, scopes *LookupScopes) string {
	var lookupScopes LookupScopes
	if scopes != nil {
		lookupScopes = *scopes
	}
	if lookupScopes.Provider == "" {
		lookupScopes.Provider = string(routing.Provider)
	}
	overrideKey := ""
	if routing.ServerSideFallbackModel != nil {
		overrideKey = *routing.ServerSideFallbackModel
	}
	if overrideKey == "" && routing.ResolvedKeyAlias != nil {
		overrideKey = routing.ResolvedKeyAlias.ModelID
	}
	if overrideKey == "" {
		overrideKey = routing.Model
	}
	if overrideKey == "" {
		return ""
	}
	if normalizeRequestType(requestType) == "unknown" {
		for _, mode := range anyModePricingRequestTypes {
			if row := s.resolvePricing(routing, mode, lookupScopes); row != nil && rowPriceStatus(row) != Unpriced {
				return s.rowIdentity(row, overrideKey, mode, lookupScopes)
			}
		}
		return ""
	}
	row := s.resolvePricing(routing, requestType, lookupScopes)
	if row == nil {
		return ""
	}
	return s.rowIdentity(row, overrideKey, requestType, lookupScopes)
}

// AnswerPriceStatus is the verdict on a delivered answer, read from the cost calculation that
// charges it rather than from its total alone (fork, TXE-8016):
//
//   - Priced: its own cost (without guardrail, routing, MCP and semantic-cache calls priced beside
//     it) is above $0.
//   - Free: its row prices nothing above zero at all (a free model, an override pricing it at $0, a
//     free server-side fallback), or it costs $0 because the service-tier rates its usage met are
//     set to zero (flex, priority, fast or ultrafast). The second is read by pricing it again with
//     each explicit zero tier rate replaced by a positive stand-in: an answer that then costs
//     something met those rates. Only tier rates are stood in for: an answer whose usage met a zero
//     standard rate while the units its row prices above zero went unreported is unpriced.
//   - Unpriced: it costs $0 and met no rate: no row prices it, or its usage is in units its row
//     does not price.
//
// The calculation is accounting's own (CalculateCostBreakdown), so the verdict follows the same
// alias, server-side fallback, service tier and override accounting chose.
func (s *Store) AnswerPriceStatus(result *schemas.BifrostResponse, scopes *LookupScopes) PriceStatus {
	if result == nil {
		return Unpriced
	}
	own := func(cost *schemas.BifrostCost) float64 {
		if cost == nil {
			return 0
		}
		total := cost.TotalCost
		if details := cost.AdditionalCostDetails; details != nil {
			total -= details.GuardrailCost + details.RoutingCost + details.MCPCost + details.SemanticCacheCost
		}
		return total
	}
	if own(s.CalculateCostBreakdown(result, scopes)) > 0 {
		return Priced
	}
	var standIn LookupScopes
	if scopes != nil {
		standIn = *scopes
	}
	standIn.zeroRatesAs = 1
	if own(s.CalculateCostBreakdown(result, &standIn)) > 0 {
		return Free
	}
	// A row that prices nothing above zero is free whatever usage its answer reported.
	if extra := result.GetExtraFields(); extra != nil {
		routing := extra.RoutingInfo
		if routing.Provider == "" {
			routing.Provider = extra.Provider
		}
		if routing.Model == "" {
			routing.Model = extra.OriginalModelRequested
		}
		requestType := extra.PricingRequestType
		if requestType == "" {
			requestType = extra.RequestType
		}
		var lookupScopes LookupScopes
		if scopes != nil {
			lookupScopes = *scopes
		}
		if routing.Model != "" && rowPriceStatus(s.resolvePricingRow(routing, requestType, lookupScopes)) == Free {
			return Free
		}
	}
	return Unpriced
}

// zeroRatesAs replaces every service-tier price (tierPriceFieldIndexes) explicitly set to zero on
// row with standIn. row is a resolved copy: each field gets a new pointer, so the price book's own
// values are never written.
func zeroRatesAs(row *configstoreTables.TableModelPricing, standIn float64) {
	v := reflect.ValueOf(row).Elem()
	for _, i := range tierPriceFieldIndexes() {
		field := v.Field(i)
		if field.IsNil() || field.Elem().Float() != 0 {
			continue
		}
		value := standIn
		field.Set(reflect.ValueOf(&value))
	}
}

// InferPassthroughRequestType is the request type a passthrough answer is priced as; see
// inferPassthroughRequestType.
func InferPassthroughRequestType(provider schemas.ModelProvider, path string, usage *schemas.BifrostPassthroughUsage) schemas.RequestType {
	return inferPassthroughRequestType(provider, path, usage)
}

// rowIdentity names a resolved pricing row; see PriceIdentity. A row with no model is an
// override-only price, named by the override resolvePricing applied for the same model, mode and
// scopes.
func (s *Store) rowIdentity(row *configstoreTables.TableModelPricing, model string, requestType schemas.RequestType, scopes LookupScopes) string {
	if row == nil {
		return ""
	}
	mode := normalizeRequestType(requestType)
	s.overridesMu.RLock()
	custom := s.customPricing
	s.overridesMu.RUnlock()
	var entry *customPricingEntry
	if custom != nil {
		entry = custom.resolveEntry(model, mode, scopes)
	}
	if row.Model != "" {
		// A row an override patched is priced by the override under these scopes: two keys whose
		// overrides price one catalog row differently are two identities (TXE-7977).
		if entry != nil {
			return row.Provider + "|" + row.Mode + "|" + row.Model + "|override|" + entry.id
		}
		return row.Provider + "|" + row.Mode + "|" + row.Model
	}
	if entry != nil {
		return "override|" + entry.id + "|" + mode
	}
	return "override||" + mode + "|" + model
}

// rowPriceStatus classifies a resolved pricing row by the prices it sets.
func rowPriceStatus(row *configstoreTables.TableModelPricing) PriceStatus {
	if row == nil {
		return Unpriced
	}
	v := reflect.ValueOf(row).Elem()
	status := Unpriced
	for _, i := range priceFieldIndexes() {
		field := v.Field(i)
		if field.IsNil() {
			continue
		}
		if field.Elem().Float() > 0 {
			return Priced
		}
		status = Free
	}
	return status
}

var (
	priceFieldIndexesOnce sync.Once
	priceFieldIndexesList []int
)

// priceFieldIndexes are the TableModelPricing fields that hold a price: every *float64 whose JSON
// name says cost or price. Multipliers, limits and counts (free_input_images,
// unmeasured_video_seconds, inference_geo_us_multiplier, ...) are not prices. A new price column
// is picked up without editing this list; TestPriceFieldIndexesCoverEveryPriceColumn keeps the
// naming rule honest.
var (
	tierPriceFieldIndexesOnce sync.Once
	tierPriceFieldIndexesList []int
)

// tierPriceFieldIndexes are the price columns of a service tier (flex, priority, fast, ultrafast):
// a rate an answer meets only because it was served on that tier.
func tierPriceFieldIndexes() []int {
	tierPriceFieldIndexesOnce.Do(func() {
		t := reflect.TypeOf(configstoreTables.TableModelPricing{})
		for _, i := range priceFieldIndexes() {
			name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
			for _, tier := range []string{"_flex", "_priority", "_ultrafast", "_fast"} {
				if strings.HasSuffix(name, tier) || strings.Contains(name, tier+"_") {
					tierPriceFieldIndexesList = append(tierPriceFieldIndexesList, i)
					break
				}
			}
		}
	})
	return tierPriceFieldIndexesList
}

func priceFieldIndexes() []int {
	priceFieldIndexesOnce.Do(func() {
		t := reflect.TypeOf(configstoreTables.TableModelPricing{})
		floatPtr := reflect.TypeOf((*float64)(nil))
		for i := 0; i < t.NumField(); i++ {
			field := t.Field(i)
			if field.Type != floatPtr {
				continue
			}
			if isPriceJSONName(strings.Split(field.Tag.Get("json"), ",")[0]) {
				priceFieldIndexesList = append(priceFieldIndexesList, i)
			}
		}
	})
	return priceFieldIndexesList
}

func isPriceJSONName(name string) bool {
	return strings.Contains(name, "cost") || strings.Contains(name, "price")
}
