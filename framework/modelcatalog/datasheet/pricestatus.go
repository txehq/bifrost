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
// by an override alone, with no row, is identified by the provider, mode and the lowercased model.
// Empty when unpriced with no row at all.
func (s *Store) PriceIdentity(provider schemas.ModelProvider, model string, requestType schemas.RequestType, scopes *LookupScopes) (string, PriceStatus) {
	if model == "" {
		return "", Unpriced
	}
	var lookupScopes LookupScopes
	if scopes != nil {
		lookupScopes = *scopes
	}
	routing := schemas.RoutingInfo{Provider: provider, Model: model}
	if normalizeRequestType(requestType) != "unknown" {
		row := s.resolvePricing(routing, requestType, lookupScopes)
		return rowIdentity(row, provider, model, requestType), rowPriceStatus(row)
	}
	bestIdentity, best := "", Unpriced
	for _, mode := range anyModePricingRequestTypes {
		row := s.resolvePricing(routing, mode, lookupScopes)
		if row == nil {
			continue
		}
		if status := rowPriceStatus(row); bestIdentity == "" || status > best {
			best, bestIdentity = status, rowIdentity(row, provider, model, mode)
		}
		if best == Priced {
			break
		}
	}
	return bestIdentity, best
}

// rowIdentity names a resolved pricing row; see PriceIdentity.
func rowIdentity(row *configstoreTables.TableModelPricing, provider schemas.ModelProvider, model string, requestType schemas.RequestType) string {
	if row == nil {
		return ""
	}
	if row.Model != "" {
		return row.Provider + "|" + row.Mode + "|" + row.Model
	}
	return "override|" + string(provider) + "|" + normalizeRequestType(requestType) + "|" + strings.ToLower(model)
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
