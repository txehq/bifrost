package governance

import (
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/capsohq/bifrost/framework/modelcatalog"
	"github.com/tidwall/gjson"
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
	// budgetPricingUnpriced: charged by the vendor, but Bifrost prices no answer of it, so it is
	// refused under a dollar budget whatever model it names.
	budgetPricingUnpriced
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
	schemas.PassthroughRequest:           budgetPricingAnswer,
	schemas.PassthroughStreamRequest:     budgetPricingAnswer,
	// An image task is billed once, at its create (TXE-6431): the create's answer is priced.
	schemas.ImageGenerationTaskRequest: budgetPricingAnswer,
	schemas.ImageEditTaskRequest:       budgetPricingAnswer,

	// Gemini and Vertex charge for the cached tokens and their storage; Bifrost prices neither.
	schemas.CachedContentCreateRequest: budgetPricingUnpriced,

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

// unpricedAnswers remembers, per dollar budget, the pricing rows whose admitted answers still
// priced at $0 against that budget, so the next request charged to it on such a row is refused
// before the upstream call.
//
// It is keyed on what governance controls, never on what a caller sends: a budget id (budgets
// are made by operators) and the identity of the price book row the model resolved to (every
// spelling of a model the lookup folds onto one row has one identity). A $0 answer refuses that
// row only for the budgets it was charged to, so one tenant's answer never refuses another's.
// Each budget holds at most maxUnpricedRowsPerBudget rows; a budget that fills is refused on
// every row it would be charged for, until restart, rather than forgetting one. Other budgets
// are unaffected.
type unpricedAnswers struct {
	mu       sync.RWMutex
	byBudget map[string]*unpricedRows
}

type unpricedRows struct {
	rows map[string]struct{}
	full bool
}

const maxUnpricedRowsPerBudget = 1024

func newUnpricedAnswers() *unpricedAnswers {
	return &unpricedAnswers{byBudget: make(map[string]*unpricedRows)}
}

func (u *unpricedAnswers) record(budgetIDs []string, row string) {
	if u == nil || row == "" {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, budgetID := range budgetIDs {
		rows := u.byBudget[budgetID]
		if rows == nil {
			rows = &unpricedRows{rows: make(map[string]struct{})}
			u.byBudget[budgetID] = rows
		}
		if _, ok := rows.rows[row]; ok {
			continue
		}
		if len(rows.rows) >= maxUnpricedRowsPerBudget {
			rows.full = true
			continue
		}
		rows.rows[row] = struct{}{}
	}
}

// refused reports the first of budgetIDs that has seen a $0 answer on row, or is full.
// holdsAny reports whether any of budgetIDs has seen a $0 answer, so a request charged to none of
// them skips the per-row check.
func (u *unpricedAnswers) holdsAny(budgetIDs []string) bool {
	if u == nil {
		return false
	}
	u.mu.RLock()
	defer u.mu.RUnlock()
	for _, budgetID := range budgetIDs {
		if rows := u.byBudget[budgetID]; rows != nil && (rows.full || len(rows.rows) > 0) {
			return true
		}
	}
	return false
}

func (u *unpricedAnswers) refused(budgetIDs []string, row string) (string, bool) {
	if u == nil {
		return "", false
	}
	u.mu.RLock()
	defer u.mu.RUnlock()
	for _, budgetID := range budgetIDs {
		rows := u.byBudget[budgetID]
		if rows == nil {
			continue
		}
		if rows.full {
			return budgetID, true
		}
		if _, ok := rows.rows[row]; ok {
			return budgetID, true
		}
	}
	return "", false
}

func limitIDs(limits []schemas.Limit) []string {
	ids := make([]string, 0, len(limits))
	for _, limit := range limits {
		ids = append(ids, limit.ID)
	}
	return ids
}

// refuseUnpricedUnderBudget is the gate evaluateLimits runs once a request's budgets have passed:
// a request answering to any dollar budget is refused when what it will be charged cannot be
// priced, or when an earlier answer on the same price book row priced at $0 against one of its
// budgets. Nil when the request is allowed. A deployment with no price book has no prices to
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
	if budgetPricingOf(requestType) == budgetPricingUnpriced {
		return &EvaluationResult{
			Decision: DecisionModelUnpriced,
			Reason:   fmt.Sprintf("Bifrost has no price for %s requests, so they cannot be charged to a dollar budget", requestType),
		}
	}
	// A passthrough naming no model on the request whose body names a model for every unit of work
	// (the object, or every task of an array): each was gated as its own model before the envelope
	// (PreLLMHook). One with a task naming no model runs the provider's default, which cannot be
	// priced, and is gated as the envelope, which names none.
	if (requestType == schemas.PassthroughRequest || requestType == schemas.PassthroughStreamRequest) && evaluationRequest.Model == "" && evaluationRequest.BodyModelsComplete {
		// One answer is priced for the whole body, so a body mixing models cannot say which of them
		// a $0 answer was not priced for. Under a dollar budget it sends one model per request, as
		// refuseUnchargedBodyModelLimits asks of a body whose models carry their own limits.
		if evaluationRequest.BodyModelCount > 1 {
			return &EvaluationResult{
				Decision: DecisionModelUnpriced,
				Reason: fmt.Sprintf("the request body names %d models, and one answer priced for all of them cannot be charged to a dollar budget model by model; send one model per request",
					evaluationRequest.BodyModelCount),
			}
		}
		return nil
	}
	if requestType == schemas.BatchCreateRequest {
		// An inline batch naming no model of its own: each request was gated as its own model before
		// the envelope (PreLLMHook), so the envelope has nothing left to price, provided every
		// request names one. A request naming none runs a model nothing priced.
		if evaluationRequest.Model == "" && !evaluationRequest.OpaqueBatchInput {
			if evaluationRequest.InlineBatchModelsComplete {
				return nil
			}
			return &EvaluationResult{
				Decision: DecisionModelUnpriced,
				Reason:   "a batch request names no model, on itself or on the batch, so it cannot be priced or charged to a dollar budget; name the model on every request",
			}
		}
		// An uploaded input runs the models its rows name. Only where the provider's batch API runs
		// the model named on the request does that model say what will be charged; elsewhere
		// (OpenAI, Azure, xAI) it is at most a hint, and the rows cannot be priced before they run.
		if evaluationRequest.OpaqueBatchInput && (evaluationRequest.Model == "" || !batchModelBoundByProvider(provider, r.governanceInMemoryStore)) {
			return &EvaluationResult{
				Decision: DecisionModelUnpriced,
				Reason: fmt.Sprintf("a batch on provider %q runs the models named inside its uploaded input, which cannot be priced before it runs, so it cannot be charged to a dollar budget; send its requests inline",
					provider),
			}
		}
	}
	model := pricedModel(requestType, evaluationRequest.Model)
	scopes := modelcatalog.PricingLookupScopesFromContext(ctx, string(provider))
	if _, status := r.priceIdentity(provider, model, requestType, scopes); status == modelcatalog.PriceUnpriced {
		return &EvaluationResult{
			Decision: DecisionModelUnpriced,
			Reason: fmt.Sprintf("model %q on provider %q has no price for %s requests, so it cannot be charged to a dollar budget; add a pricing override for it",
				model, provider, requestType),
		}
	}
	// Which key serves the request, and so which override prices it, is chosen after this runs:
	// every row the request could be priced as is checked against what its budgets remember.
	budgetIDs := limitIDs(budgets)
	if r.unpriced.holdsAny(budgetIDs) {
		_, rows := r.priceIdentities(provider, model, requestType, scopes)
		for _, row := range rows {
			if _, refused := r.unpriced.refused(budgetIDs, row); refused {
				return &EvaluationResult{
					Decision: DecisionModelUnpriced,
					Reason: fmt.Sprintf("model %q on provider %q delivered an answer the price book could not price against this request's dollar budget, so it cannot be charged to it; add a pricing override for it",
						model, provider),
				}
			}
		}
	}
	return nil
}

// noteUnpricedAnswer is called for an answer charged to the dollar budgets budgetIDs. When the
// answer priced at $0 and the price book does not say the model is free, it is logged as
// unpriced, and every price book row the request could be priced as (under each configured key's
// scope) is refused for those budgets from now on (fail closed after the fact). Reports whether it
// was unpriced.
func (r *BudgetResolver) noteUnpricedAnswer(provider schemas.ModelProvider, model string, requestType schemas.RequestType, cost float64, scopes *modelcatalog.PricingLookupScopes, requestID string, budgetIDs []string) bool {
	if r == nil || r.modelCatalog == nil || cost > 0 || len(budgetIDs) == 0 {
		return false
	}
	model = pricedModel(requestType, model)
	// Whether $0 was the price is read under the scope that priced the answer: the key that served
	// it is selected by now, and another key's override says nothing about this answer.
	if r.servedPriceStatus(provider, model, requestType, scopes) == modelcatalog.PriceFree {
		return false
	}
	_, rows := r.priceIdentities(provider, model, requestType, scopes)
	// A model with no row at all is refused before the call by the gate itself; there is no row
	// to remember.
	for _, row := range rows {
		r.unpriced.record(budgetIDs, row)
	}
	if r.logger != nil {
		r.logger.Warn("governance: unpriced answer under a dollar budget (TXE-7957): request %s, provider %s, model %s, request type %s was charged $0 against budgets %v; requests charged to them on it are refused from now on until it is priced",
			requestID, provider, model, requestType, budgetIDs)
	}
	return true
}

// deliveredAnswer reports whether a response is an answer the vendor has finished and priced. A
// cache hit was answered by Bifrost, not the vendor. A background Responses create that is still
// queued or in progress has no usage yet, and one that failed or was cancelled delivered nothing.
func deliveredAnswer(result *schemas.BifrostResponse) bool {
	if result == nil {
		return false
	}
	if extra := result.GetExtraFields(); extra != nil && extra.CacheDebug != nil && extra.CacheDebug.CacheHit {
		return false
	}
	// An image request the provider's content filter answered with no image delivered nothing to
	// price (extractCostInput charges such an answer no output).
	if image := result.ImageGenerationResponse; image != nil && image.ContentFilter != nil && len(image.Data) == 0 {
		return false
	}
	if result.ResponsesResponse != nil && result.ResponsesResponse.Status != nil {
		switch *result.ResponsesResponse.Status {
		case "queued", "in_progress", "failed", "cancelled":
			return false
		}
	}
	return true
}

// priceIdentity is the best status, and the row that decided it, over every way the request can
// be priced once a key serves it. Which key serves it is chosen after governance runs, so each
// configured key of the provider is tried: under that key's pricing scope (an override scoped to a
// provider key, a virtual key's provider key or a user's provider key applies only once the key is
// selected), for the request's model and every model that key aliases it to (pricing reads an
// alias's target first, so a label that is not a catalog row is priced as the model it names). A
// key that serves the request unpriced while another prices it is caught after the fact
// (noteUnpricedAnswer).
func (r *BudgetResolver) priceIdentity(provider schemas.ModelProvider, model string, requestType schemas.RequestType, scopes *modelcatalog.PricingLookupScopes) (string, modelcatalog.PriceStatus) {
	bestRow, best := r.modelCatalog.PriceIdentity(provider, model, requestType, scopes)
	if best == modelcatalog.PricePriced || model == "" || r.governanceInMemoryStore == nil {
		return bestRow, best
	}
	config, ok := configuredProvider(r.governanceInMemoryStore, provider)
	if !ok {
		return bestRow, best
	}
	for _, key := range config.Keys {
		keyScopes := modelcatalog.PricingLookupScopes{}
		if scopes != nil {
			keyScopes = *scopes
		}
		keyScopes.SelectedKeyID = key.ID
		candidates := [3]string{model}
		if alias := key.Aliases.ResolveConfig(model); alias != nil {
			candidates[1] = alias.ModelID
			if alias.ModelName != nil {
				candidates[2] = *alias.ModelName
			}
		}
		for _, candidate := range candidates {
			if candidate == "" {
				continue
			}
			if row, status := r.modelCatalog.PriceIdentity(provider, candidate, requestType, &keyScopes); status > best {
				bestRow, best = row, status
				if best == modelcatalog.PricePriced {
					return bestRow, best
				}
			}
		}
	}
	return bestRow, best
}

// priceIdentities is every price book row the request could be priced as, and the best status
// among them: under the scopes given, and under each configured key's scope for the model and the
// key's alias targets. The gate and the after-the-fact record both read this whole set, so a row
// remembered under the key that served an answer is found by a request that has not chosen a key.
func (r *BudgetResolver) priceIdentities(provider schemas.ModelProvider, model string, requestType schemas.RequestType, scopes *modelcatalog.PricingLookupScopes) (modelcatalog.PriceStatus, []string) {
	var rows []string
	add := func(row string) {
		if row != "" && !slices.Contains(rows, row) {
			rows = append(rows, row)
		}
	}
	firstRow, best := r.modelCatalog.PriceIdentity(provider, model, requestType, scopes)
	add(firstRow)
	if model == "" || r.governanceInMemoryStore == nil {
		return best, rows
	}
	config, ok := configuredProvider(r.governanceInMemoryStore, provider)
	if !ok {
		return best, rows
	}
	for _, key := range config.Keys {
		keyScopes := modelcatalog.PricingLookupScopes{}
		if scopes != nil {
			keyScopes = *scopes
		}
		keyScopes.SelectedKeyID = key.ID
		candidates := [3]string{model}
		if alias := key.Aliases.ResolveConfig(model); alias != nil {
			candidates[1] = alias.ModelID
			if alias.ModelName != nil {
				candidates[2] = *alias.ModelName
			}
		}
		for _, candidate := range candidates {
			if candidate == "" {
				continue
			}
			row, status := r.modelCatalog.PriceIdentity(provider, candidate, requestType, &keyScopes)
			add(row)
			if status > best {
				best = status
			}
		}
	}
	return best, rows
}

// servedPriceStatus is the status of the row the answer was priced under: the serving key's scope,
// and the model resolved the way accounting resolves it (resolvePricing tries the key alias's model
// name, then its model id, then the requested model, and prices from the first that has a row).
func (r *BudgetResolver) servedPriceStatus(provider schemas.ModelProvider, model string, requestType schemas.RequestType, scopes *modelcatalog.PricingLookupScopes) modelcatalog.PriceStatus {
	candidates := []string{model}
	if scopes != nil && scopes.SelectedKeyID != "" && r.governanceInMemoryStore != nil {
		if config, ok := configuredProvider(r.governanceInMemoryStore, provider); ok {
			for _, key := range config.Keys {
				if key.ID != scopes.SelectedKeyID {
					continue
				}
				if alias := key.Aliases.ResolveConfig(model); alias != nil {
					candidates = candidates[:0]
					if alias.ModelName != nil && *alias.ModelName != "" {
						candidates = append(candidates, *alias.ModelName)
					}
					if alias.ModelID != "" {
						candidates = append(candidates, alias.ModelID)
					}
					candidates = append(candidates, model)
				}
				break
			}
		}
	}
	for _, candidate := range candidates {
		if row, status := r.modelCatalog.PriceIdentity(provider, candidate, requestType, scopes); row != "" {
			return status
		}
	}
	return modelcatalog.PriceUnpriced
}

// passthroughGenerated reports whether a passthrough answer is a provider's success: a body that
// named the model it runs was accepted and answered.
func passthroughGenerated(result *schemas.BifrostResponse) bool {
	if result == nil || result.PassthroughResponse == nil {
		return false
	}
	status := result.PassthroughResponse.StatusCode
	return status >= 200 && status < 300
}

// passthroughBodyNamesEveryModel reports whether a passthrough body names a model for every unit
// of work it carries, read as passthroughBodyModels reads it: a JSON object with a non-empty
// string "model", or a non-empty JSON array every element of which is such an object. A task
// naming no model runs whatever the provider defaults to, which governance cannot price.
func passthroughBodyNamesEveryModel(req *schemas.BifrostRequest) bool {
	if req.PassthroughRequest == nil || len(req.PassthroughRequest.Body) == 0 || !gjson.ValidBytes(req.PassthroughRequest.Body) {
		return false
	}
	names := func(object gjson.Result) bool {
		value := object.Get("model")
		return object.IsObject() && value.Type == gjson.String && strings.TrimSpace(value.Str) != ""
	}
	root := gjson.ParseBytes(req.PassthroughRequest.Body)
	switch {
	case root.IsObject():
		return names(root)
	case root.IsArray():
		tasks := root.Array()
		if len(tasks) == 0 {
			return false
		}
		for _, task := range tasks {
			if !names(task) {
				return false
			}
		}
		return true
	}
	return false
}

// inlineBatchNamesEveryModel reports whether an inline batch create names a model on every one of
// its requests, read as BatchCreateModels reads them (the OpenAI-shaped body or the Anthropic-shaped
// params).
func inlineBatchNamesEveryModel(req *schemas.BifrostRequest) bool {
	if req.RequestType != schemas.BatchCreateRequest || req.BatchCreateRequest == nil || len(req.BatchCreateRequest.Requests) == 0 {
		return false
	}
	for _, item := range req.BatchCreateRequest.Requests {
		named := false
		for _, body := range []map[string]any{item.Body, item.Params} {
			if m, ok := body["model"].(string); ok && strings.TrimSpace(m) != "" {
				named = true
			}
		}
		if !named {
			return false
		}
	}
	return true
}
