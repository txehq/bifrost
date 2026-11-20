package governance

import (
	"fmt"
	"sync"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/capsohq/bifrost/framework/modelcatalog"
)

// A dollar budget is charged what the price book says an answer cost. An answer the price book
// cannot price costs $0, so before TXE-7957 a request under a budget could be repeated on an
// unpriced model without spending anything. Governance now refuses that request before the
// upstream call, and fails closed after the fact when an answer it admitted still prices at $0.

// DecisionModelUnpriced refuses a request under a dollar budget whose model the price book cannot
// price. It is a policy refusal of the model (403, policy_model_blocked): no upstream call is made
// and nothing is charged.
const DecisionModelUnpriced Decision = "model_unpriced"

// budgetPricing is how a request type is charged to a dollar budget.
type budgetPricing int

const (
	// budgetPricingNone: never charged. A read, a list, a delete, a cancel or a file operation
	// costs nothing at the vendor, or is priced as part of the create it belongs to.
	budgetPricingNone budgetPricing = iota
	// budgetPricingAnswer: priced on its own answer, in the post-hook.
	budgetPricingAnswer
	// budgetPricingSettled: priced when its job settles (framework/jobaccounting), not on the
	// create's answer, which is a queued job.
	budgetPricingSettled
)

// requestTypeBudgetPricing classifies every request type. TestEveryRequestTypeHasABudgetPricing
// reads every RequestType constant in core/schemas and fails when one is missing here, so a new
// operation cannot reach a provider under a budget without someone deciding how it is charged.
// A type missing at run time is gated as priced on its answer: failing closed.
var requestTypeBudgetPricing = map[schemas.RequestType]budgetPricing{
	schemas.TextCompletionRequest:        budgetPricingAnswer,
	schemas.TextCompletionStreamRequest:  budgetPricingAnswer,
	schemas.ChatCompletionRequest:        budgetPricingAnswer,
	schemas.ChatCompletionStreamRequest:  budgetPricingAnswer,
	schemas.ResponsesRequest:             budgetPricingAnswer,
	schemas.ResponsesStreamRequest:       budgetPricingAnswer,
	schemas.WebSocketResponsesRequest:    budgetPricingAnswer,
	schemas.CompactionRequest:            budgetPricingAnswer,
	schemas.EmbeddingRequest:             budgetPricingAnswer,
	schemas.SpeechRequest:                budgetPricingAnswer,
	schemas.SpeechStreamRequest:          budgetPricingAnswer,
	schemas.TranscriptionRequest:         budgetPricingAnswer,
	schemas.TranscriptionStreamRequest:   budgetPricingAnswer,
	schemas.ImageGenerationRequest:       budgetPricingAnswer,
	schemas.ImageGenerationStreamRequest: budgetPricingAnswer,
	schemas.ImageEditRequest:             budgetPricingAnswer,
	schemas.ImageEditStreamRequest:       budgetPricingAnswer,
	schemas.ImageVariationRequest:        budgetPricingAnswer,
	schemas.RerankRequest:                budgetPricingAnswer,
	schemas.DecisionRequest:              budgetPricingAnswer,
	schemas.OCRRequest:                   budgetPricingAnswer,
	schemas.RealtimeRequest:              budgetPricingAnswer,
	schemas.LiveRequest:                  budgetPricingAnswer,
	schemas.ContainerCreateRequest:       budgetPricingAnswer,
	schemas.CachedContentCreateRequest:   budgetPricingAnswer,
	schemas.PassthroughRequest:           budgetPricingAnswer,
	schemas.PassthroughStreamRequest:     budgetPricingAnswer,
	// An image task is billed once, at its create (TXE-6431): the create's answer is priced.
	schemas.ImageGenerationTaskRequest: budgetPricingAnswer,
	schemas.ImageEditTaskRequest:       budgetPricingAnswer,

	schemas.VideoGenerationRequest: budgetPricingSettled,
	schemas.VideoEditRequest:       budgetPricingSettled,
	schemas.VideoRemixRequest:      budgetPricingSettled,
	schemas.BatchCreateRequest:     budgetPricingSettled,

	schemas.ListModelsRequest:              budgetPricingNone,
	schemas.ModelRetrieveRequest:           budgetPricingNone,
	schemas.ResponsesRetrieveRequest:       budgetPricingNone,
	schemas.ResponsesRetrieveStreamRequest: budgetPricingNone,
	schemas.ResponsesDeleteRequest:         budgetPricingNone,
	schemas.ResponsesCancelRequest:         budgetPricingNone,
	schemas.ResponsesInputItemsRequest:     budgetPricingNone,
	schemas.VideoRetrieveRequest:           budgetPricingNone,
	schemas.VideoDownloadRequest:           budgetPricingNone,
	schemas.VideoDeleteRequest:             budgetPricingNone,
	schemas.VideoListRequest:               budgetPricingNone,
	schemas.VideoCancelRequest:             budgetPricingNone,
	schemas.ImageTaskRetrieveRequest:       budgetPricingNone,
	schemas.ImageTaskCancelRequest:         budgetPricingNone,
	schemas.BatchListRequest:               budgetPricingNone,
	schemas.BatchRetrieveRequest:           budgetPricingNone,
	schemas.BatchCancelRequest:             budgetPricingNone,
	schemas.BatchResultsRequest:            budgetPricingNone,
	schemas.BatchDeleteRequest:             budgetPricingNone,
	schemas.FileUploadRequest:              budgetPricingNone,
	schemas.FileListRequest:                budgetPricingNone,
	schemas.FileRetrieveRequest:            budgetPricingNone,
	schemas.FileDeleteRequest:              budgetPricingNone,
	schemas.FileContentRequest:             budgetPricingNone,
	schemas.CachedContentListRequest:       budgetPricingNone,
	schemas.CachedContentRetrieveRequest:   budgetPricingNone,
	schemas.CachedContentUpdateRequest:     budgetPricingNone,
	schemas.CachedContentDeleteRequest:     budgetPricingNone,
	schemas.ContainerListRequest:           budgetPricingNone,
	schemas.ContainerRetrieveRequest:       budgetPricingNone,
	schemas.ContainerDeleteRequest:         budgetPricingNone,
	schemas.ContainerFileCreateRequest:     budgetPricingNone,
	schemas.ContainerFileListRequest:       budgetPricingNone,
	schemas.ContainerFileRetrieveRequest:   budgetPricingNone,
	schemas.ContainerFileContentRequest:    budgetPricingNone,
	schemas.ContainerFileDeleteRequest:     budgetPricingNone,
	schemas.CountTokensRequest:             budgetPricingNone, // the vendors' token counters are free
	schemas.LiveContentRequest:             budgetPricingNone, // downloads a stored session's recording
	schemas.LiveSidebandRequest:            budgetPricingNone, // control frames of a live session, priced as the session
	schemas.MCPToolExecutionRequest:        budgetPricingNone, // a tool Bifrost runs itself; MCP tool cost is charged on its own path
	schemas.UnknownRequest:                 budgetPricingNone, // never reaches a provider
}

func budgetPricingOf(requestType schemas.RequestType) budgetPricing {
	if pricing, ok := requestTypeBudgetPricing[requestType]; ok {
		return pricing
	}
	return budgetPricingAnswer
}

// pricedModel is the name a request is priced under before its answer exists. A container is
// priced from the datasheet's container rows, not from a model.
func pricedModel(requestType schemas.RequestType, model string) string {
	if requestType == schemas.ContainerCreateRequest {
		return "container"
	}
	return model
}

// unpricedAnswers remembers the (provider, model, request type) triples whose admitted answers
// still priced at $0 under a dollar budget, so the next request on one is refused before the
// upstream call. It is bounded: past maxUnpricedAnswers distinct triples, every request under a
// budget whose answer is charged by this replica is refused until restart, rather than letting a
// caller grow the set without limit or slip past a full one.
type unpricedAnswers struct {
	mu   sync.RWMutex
	seen map[unpricedTriple]struct{}
	full bool
}

type unpricedTriple struct {
	provider    schemas.ModelProvider
	model       string
	requestType schemas.RequestType
}

const maxUnpricedAnswers = 4096

func newUnpricedAnswers() *unpricedAnswers {
	return &unpricedAnswers{seen: make(map[unpricedTriple]struct{})}
}

// settledJobs is the request type every job priced at settlement is remembered under: settlement
// reports a job's provider and models, not the create that started it.
const settledJobs schemas.RequestType = "settled_job"

func unpricedKey(provider schemas.ModelProvider, model string, requestType schemas.RequestType) unpricedTriple {
	if budgetPricingOf(requestType) == budgetPricingSettled {
		requestType = settledJobs
	}
	// Streamed and unstreamed requests share their answer's pricing.
	return unpricedTriple{provider: provider, model: model, requestType: baseRequestType(requestType)}
}

func (u *unpricedAnswers) record(provider schemas.ModelProvider, model string, requestType schemas.RequestType) {
	if u == nil {
		return
	}
	key := unpricedKey(provider, model, requestType)
	u.mu.Lock()
	defer u.mu.Unlock()
	if _, ok := u.seen[key]; ok {
		return
	}
	if len(u.seen) >= maxUnpricedAnswers {
		u.full = true
		return
	}
	u.seen[key] = struct{}{}
}

func (u *unpricedAnswers) contains(provider schemas.ModelProvider, model string, requestType schemas.RequestType) bool {
	if u == nil {
		return false
	}
	u.mu.RLock()
	defer u.mu.RUnlock()
	if u.full {
		return true
	}
	_, ok := u.seen[unpricedKey(provider, model, requestType)]
	return ok
}

// baseRequestType folds a streamed request type onto the unstreamed one it is priced as.
func baseRequestType(requestType schemas.RequestType) schemas.RequestType {
	switch requestType {
	case schemas.TextCompletionStreamRequest:
		return schemas.TextCompletionRequest
	case schemas.ChatCompletionStreamRequest:
		return schemas.ChatCompletionRequest
	case schemas.ResponsesStreamRequest:
		return schemas.ResponsesRequest
	case schemas.SpeechStreamRequest:
		return schemas.SpeechRequest
	case schemas.TranscriptionStreamRequest:
		return schemas.TranscriptionRequest
	case schemas.ImageGenerationStreamRequest:
		return schemas.ImageGenerationRequest
	case schemas.ImageEditStreamRequest:
		return schemas.ImageEditRequest
	case schemas.PassthroughStreamRequest:
		return schemas.PassthroughRequest
	}
	return requestType
}

// refuseUnpricedUnderBudget is the gate evaluateLimits runs once a request's budgets have passed:
// a request answering to any dollar budget is refused when what it will be charged cannot be
// priced. Nil when the request is allowed. A deployment with no price book has no prices to
// charge at all and is not gated here.
func (r *BudgetResolver) refuseUnpricedUnderBudget(ctx *schemas.BifrostContext, evaluationRequest *EvaluationRequest, budgets []schemas.Limit) *EvaluationResult {
	if len(budgets) == 0 || r.modelCatalog == nil {
		return nil
	}
	requestType := evaluationRequest.RequestType
	if budgetPricingOf(requestType) == budgetPricingNone {
		return nil
	}
	provider := evaluationRequest.Provider
	model := pricedModel(requestType, evaluationRequest.Model)
	if r.unpriced.contains(provider, model, requestType) {
		return &EvaluationResult{
			Decision: DecisionModelUnpriced,
			Reason: fmt.Sprintf("model %q on provider %q delivered an answer the price book could not price, so it cannot be charged to a dollar budget; add a pricing override for it",
				model, provider),
		}
	}
	scopes := modelcatalog.PricingLookupScopesFromContext(ctx, string(provider))
	if r.priceStatus(provider, model, requestType, scopes) == modelcatalog.PriceUnpriced {
		return &EvaluationResult{
			Decision: DecisionModelUnpriced,
			Reason: fmt.Sprintf("model %q on provider %q has no price for %s requests, so it cannot be charged to a dollar budget; add a pricing override for it",
				model, provider, requestType),
		}
	}
	return nil
}

// noteUnpricedAnswer is called for an answer charged to at least one dollar budget. When the
// answer priced at $0 and the price book does not say the model is free, it is logged as
// unpriced and its triple is refused under a budget from now on (fail closed after the fact).
// Reports whether it was unpriced.
func (r *BudgetResolver) noteUnpricedAnswer(provider schemas.ModelProvider, model string, requestType schemas.RequestType, cost float64, scopes *modelcatalog.PricingLookupScopes, requestID string) bool {
	if r == nil || r.modelCatalog == nil || cost > 0 {
		return false
	}
	model = pricedModel(requestType, model)
	if r.priceStatus(provider, model, requestType, scopes) == modelcatalog.PriceFree {
		return false
	}
	r.unpriced.record(provider, model, requestType)
	if r.logger != nil {
		r.logger.Warn("governance: unpriced answer under a dollar budget (TXE-7957): request %s, provider %s, model %s, request type %s was charged $0; requests under a budget on it are refused from now on until it is priced",
			requestID, provider, model, requestType)
	}
	return true
}

// deliveredAnswer reports whether a response is an answer the vendor has finished and priced. A
// cache hit was answered by Bifrost, not the vendor, and a background Responses create that is
// still queued or in progress has no usage yet.
func deliveredAnswer(result *schemas.BifrostResponse) bool {
	if result == nil {
		return false
	}
	if extra := result.GetExtraFields(); extra != nil && extra.CacheDebug != nil && extra.CacheDebug.CacheHit {
		return false
	}
	if result.ResponsesResponse != nil && result.ResponsesResponse.Status != nil {
		switch *result.ResponsesResponse.Status {
		case "queued", "in_progress":
			return false
		}
	}
	return true
}

// priceStatus is the best status over the request's model and every model a configured key of
// its provider aliases it to. Which key serves the request is chosen after governance runs, and
// pricing reads the alias's target first, so a label that is not a catalog row is priced as the
// model it names. A key that serves the label unpriced while another prices it is caught after
// the fact (noteUnpricedAnswer).
func (r *BudgetResolver) priceStatus(provider schemas.ModelProvider, model string, requestType schemas.RequestType, scopes *modelcatalog.PricingLookupScopes) modelcatalog.PriceStatus {
	best := r.modelCatalog.PriceStatus(provider, model, requestType, scopes)
	if best == modelcatalog.PricePriced || model == "" || r.governanceInMemoryStore == nil {
		return best
	}
	config, ok := configuredProvider(r.governanceInMemoryStore, provider)
	if !ok {
		return best
	}
	for _, key := range config.Keys {
		alias := key.Aliases.ResolveConfig(model)
		if alias == nil {
			continue
		}
		candidates := [2]string{alias.ModelID}
		if alias.ModelName != nil {
			candidates[1] = *alias.ModelName
		}
		for _, candidate := range candidates {
			if candidate == "" || candidate == model {
				continue
			}
			if status := r.modelCatalog.PriceStatus(provider, candidate, requestType, scopes); status > best {
				best = status
				if best == modelcatalog.PricePriced {
					return best
				}
			}
		}
	}
	return best
}
