package governance

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/capsohq/bifrost/core/providers/openai"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/capsohq/bifrost/framework/jobaccounting"
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
		if strings.HasPrefix(model, "container-") {
			return model
		}
		return "container"
	}
	return model
}

// unpricedAnswers remembers, per dollar budget and holder, the pricing rows whose admitted answers
// still priced at $0 against that budget, so the holder's next request charged to it on such a row
// is refused before the upstream call.
//
// It is keyed on what governance controls, never on what a caller sends: a budget id (budgets
// are made by operators), the virtual key the answer was admitted under (unpricedHolder), and the
// identity of the price book row the model resolved to (every spelling of a model the lookup folds
// onto one row has one identity). A $0 answer refuses that row only for the holder that received
// it, on the budgets it was charged to: one caller's answer never refuses another caller sharing
// the budget, nor another tenant. A budget shared by K keys admits at most K such answers per row.
// Each (budget, holder) scope holds at most maxUnpricedRowsPerBudget rows; a scope that fills is
// refused on every row it would be charged for, until restart, rather than forgetting one.
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

// refused reports the first of the scopes that has seen a $0 answer on row, or is full. holdsAny
// reports whether any of the scopes has seen a $0 answer, so a request charged to none of them
// skips the per-row check. Scopes are unpricedScopes of a request's budgets and holder.
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

// unpricedScopes keys a request's budgets by the holder it was admitted under (TXE-7957).
func unpricedScopes(budgetIDs []string, holder string) []string {
	scopes := make([]string, 0, len(budgetIDs))
	for _, budgetID := range budgetIDs {
		scopes = append(scopes, budgetID+"\x00"+holder)
	}
	return scopes
}

// unpricedHolder is the principal a request was admitted under, which an unpriced answer is
// remembered for and an unconfirmed row is probed for: its virtual key, else its user, else a
// digest of the credential it presented (never the credential itself). Empty for a request that
// presented nothing; such requests share one holder on a budget.
func unpricedHolder(ctx *schemas.BifrostContext) string {
	if ctx == nil || ctx.Grant() == nil {
		return ""
	}
	identity := ctx.Grant().Identity()
	if identity == nil {
		return ""
	}
	if vk := identity.VirtualKey(); vk != nil && vk.ID != "" {
		return "vk:" + vk.ID
	}
	if user := identity.User(); user != nil && user.ID != "" {
		return "user:" + user.ID
	}
	if credential := identity.Credential(); credential.Kind != "" && credential.Value != "" {
		sum := sha256.Sum256([]byte(credential.Value))
		return "credential:" + credential.Kind + ":" + hex.EncodeToString(sum[:12])
	}
	return ""
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
	if budgetPricingOf(requestType) == budgetPricingNone || passthroughRead(evaluationRequest.request) {
		return nil
	}
	// A queued answer nothing will price is not charged at all, priced model or not (TXE-7963).
	if refusal := r.refuseUnsettledUnderBudget(evaluationRequest); refusal != nil {
		return refusal
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
	// A body naming a model twice never gets here: core refuses a forwarded body that repeats a
	// member before any plugin reads it (TXE-7994).
	// An OpenAI-shaped container create sent through passthrough names no model: accounting prices it
	// as the container it creates (passthrough_usage.go), so it is gated as a container create.
	if (requestType == schemas.PassthroughRequest || requestType == schemas.PassthroughStreamRequest) && evaluationRequest.Model == "" &&
		passthroughContainerCreate(configuredBaseProvider(r.governanceInMemoryStore, provider), evaluationRequest.request) {
		containerRequest := &EvaluationRequest{RequestType: schemas.ContainerCreateRequest, Provider: provider, request: &schemas.BifrostRequest{
			RequestType: schemas.ContainerCreateRequest, ContainerCreateRequest: &schemas.BifrostContainerCreateRequest{
				Provider: provider, MemoryLimit: passthroughContainerMemoryLimit(evaluationRequest.request),
			},
		}}
		return r.refuseUnpricedUnderBudget(ctx, containerRequest, budgets)
	}
	// A passthrough naming its model on the request whose body names another: the answer is priced
	// as the request's model while the body's runs, so one answer cannot be charged model by model.
	if (requestType == schemas.PassthroughRequest || requestType == schemas.PassthroughStreamRequest) && evaluationRequest.Model != "" && evaluationRequest.request != nil {
		if bodyModel, count := r.distinctBodyModels(provider, evaluationRequest.request); count > 1 || (count == 1 && bodyModel != evaluationRequest.Model) {
			return &EvaluationResult{
				Decision: DecisionModelUnpriced,
				Reason:   fmt.Sprintf("the request names model %q and its body names another, and one answer cannot be charged to a dollar budget model by model; name one model", evaluationRequest.Model),
			}
		} else if count == 1 && !passthroughBodyNamesEveryModel(evaluationRequest.request) {
			// A body naming the model for some of its units of work and not others runs the
			// provider's default on the rest, which the request's model does not price.
			return &EvaluationResult{
				Decision: DecisionModelUnpriced,
				Reason:   fmt.Sprintf("the request names model %q and its body names it for only some of its tasks; the others run the provider's default model, which cannot be priced; name the model on every task", evaluationRequest.Model),
			}
		}
	}
	if (requestType == schemas.PassthroughRequest || requestType == schemas.PassthroughStreamRequest) && evaluationRequest.Model == "" &&
		evaluationRequest.request != nil && passthroughBodyNamesEveryModel(evaluationRequest.request) {
		// One answer is priced for the whole body, so a body mixing models cannot say which of them
		// a $0 answer was not priced for. Under a dollar budget it sends one model per request, as
		// refuseUnchargedBodyModelLimits asks of a body whose models carry their own limits.
		bodyModel, count := r.distinctBodyModels(provider, evaluationRequest.request)
		if count > 1 {
			return &EvaluationResult{
				Decision: DecisionModelUnpriced,
				Reason: fmt.Sprintf("the request body names %d models, and one answer priced for all of them cannot be charged to a dollar budget model by model; send one model per request",
					count),
			}
		}
		// The answer is charged under no model of its own: the body's model, named as it was
		// evaluated, is what an unpriced answer refuses.
		ctx.SetValue(passthroughBodyModelContextKey, bodyModel)
		return r.probeUnconfirmed(ctx, requestType, evaluationRequest.request, provider, bodyModel, liveUnitPricingMode(ctx, requestType),
			modelcatalog.PricingLookupScopesFromContext(ctx, string(provider)), limitIDs(budgets))
	}
	if requestType == schemas.BatchCreateRequest {
		// An inline batch naming no model of its own: each request was gated as its own model before
		// the envelope (PreLLMHook), so the envelope has nothing left to price, provided every
		// request names one. A request naming none runs a model nothing priced.
		if evaluationRequest.Model == "" && !evaluationRequest.OpaqueBatchInput {
			if evaluationRequest.request != nil && inlineBatchNamesEveryModel(evaluationRequest.request) {
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
	containerMemory := ""
	if requestType == schemas.ContainerCreateRequest && evaluationRequest.request != nil && evaluationRequest.request.ContainerCreateRequest != nil {
		containerMemory = evaluationRequest.request.ContainerCreateRequest.MemoryLimit
	}
	model := pricedModel(requestType, evaluationRequest.Model)
	scopes := modelcatalog.PricingLookupScopesFromContext(ctx, string(provider))
	pricedAs := liveUnitPricingMode(ctx, requestType)
	// A container is priced from its memory tier's row ("container-4g") when it asks for one, as
	// accounting prices the container it creates, and from the base row otherwise.
	if requestType == schemas.ContainerCreateRequest && containerMemory != "" {
		if _, status := r.priceIdentity(provider, "container-"+containerMemory, pricedAs, scopes); status != modelcatalog.PriceUnpriced {
			model = "container-" + containerMemory
		}
	}
	// An inline batch is settled in the mode of each request's endpoint (jobaccounting.BatchRequestType):
	// a model is priced in each mode the requests naming it run in, not in whichever mode prices it.
	checkedModes := []schemas.RequestType{pricedAs}
	// A batch the requests do not price (one run from an uploaded file, whose provider binds it to
	// the batch's model) is settled in its endpoint's mode.
	if requestType == schemas.BatchCreateRequest {
		if modes := evaluationRequest.batchModes.of(evaluationRequest.request, evaluationRequest.Model); len(modes) > 0 {
			checkedModes = modes
		} else if evaluationRequest.request != nil && evaluationRequest.request.BatchCreateRequest != nil {
			checkedModes = []schemas.RequestType{jobaccounting.BatchRequestType(evaluationRequest.request.BatchCreateRequest.Endpoint)}
		}
	}
	for _, mode := range checkedModes {
		if r.admissionPriceStatus(ctx, provider, model, mode, scopes) == modelcatalog.PriceUnpriced {
			return &EvaluationResult{
				Decision: DecisionModelUnpriced,
				Reason: fmt.Sprintf("model %q on provider %q has no price for %s requests on a key that may serve this request, so it cannot be charged to a dollar budget; add a pricing override for it",
					model, provider, mode),
			}
		}
		// A batch is settled at batch rates on a batch key: a price its settlement cannot apply (an
		// image row priced per image, a chat row priced per second, a key that serves no batch) parks
		// the job uncharged.
		if requestType == schemas.BatchCreateRequest && r.batchPriceStatus(ctx, provider, model, mode, scopes) == modelcatalog.PriceUnpriced {
			return &EvaluationResult{
				Decision: DecisionModelUnpriced,
				Reason: fmt.Sprintf("model %q on provider %q has no batch price for %s requests, so a batch on it cannot be charged to a dollar budget; add a token-rate pricing override for it",
					model, provider, mode),
			}
		}
	}
	// Which key serves the request, and so which override prices it, is chosen after this runs:
	// every row the request could be priced as is checked against what its budgets remember.
	budgetIDs := unpricedScopes(limitIDs(budgets), unpricedHolder(ctx))
	rememberedRefusal := func() *EvaluationResult {
		if !r.unpriced.holdsAny(budgetIDs) {
			return nil
		}
		for _, mode := range checkedModes {
			_, rows := r.priceIdentities(provider, model, mode, scopes)
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
	if refusal := rememberedRefusal(); refusal != nil {
		return refusal
	}
	if probed := r.probeUnconfirmed(ctx, requestType, evaluationRequest.request, provider, model, pricedAs, scopes, limitIDs(budgets)); probed != nil {
		return probed
	}
	// The request that held the row may have recorded its $0 answer and released the hold between
	// the check above and this request taking the hold (the usage worker records before it
	// releases): read the record again now that this request holds the row.
	if held, ok := ctx.Value(unpricedProbeContextKey).(*unpricedProbe); ok && held != nil {
		if refusal := rememberedRefusal(); refusal != nil {
			held.release()
			ctx.ClearValue(unpricedProbeContextKey)
			return refusal
		}
	}
	return nil
}

// noteUnpricedAnswer is called for an answer charged to the dollar budgets budgetIDs, with the
// verdict on it: what the cost calculation that charged it found (modelcatalog.AnswerPriceStatus,
// TXE-8016), never inferred from a $0 total. An unpriced answer is logged, and every price book
// row the request could be priced as (under each configured key's scope) is refused for holder on
// those budgets from now on (fail closed after the fact). A priced or free answer records nothing.
// Reports whether it was unpriced.
//
// pricedAs is the request type accounting priced the answer as (a passthrough is priced in the
// mode its path implies); requestType is the request's own, which the gate reads rows under. model
// is the model the request named, whose rows admission checks, even when a provider's server-side
// fallback served the answer.
func (r *BudgetResolver) noteUnpricedAnswer(provider schemas.ModelProvider, model string, requestType, pricedAs schemas.RequestType, verdict modelcatalog.PriceStatus, scopes *modelcatalog.PricingLookupScopes, requestID string, budgetIDs []string, holder string) bool {
	if r == nil || r.modelCatalog == nil || verdict != modelcatalog.PriceUnpriced || len(budgetIDs) == 0 {
		return false
	}
	model = pricedModel(requestType, model)
	if pricedAs == "" || normalizesToUnknown(pricedAs) {
		pricedAs = requestType
	}
	_, rows := r.priceIdentities(provider, model, requestType, scopes)
	// A container priced from its memory tier's row is admitted by that row or the base one.
	if requestType == schemas.ContainerCreateRequest && model != "container" {
		_, baseRows := r.priceIdentities(provider, "container", requestType, scopes)
		for _, row := range baseRows {
			if !slices.Contains(rows, row) {
				rows = append(rows, row)
			}
		}
	}
	if pricedAs != requestType {
		_, pricedRows := r.priceIdentities(provider, model, pricedAs, scopes)
		for _, row := range pricedRows {
			if !slices.Contains(rows, row) {
				rows = append(rows, row)
			}
		}
	}
	// A model with no row at all is refused before the call by the gate itself; there is no row
	// to remember.
	holderScopes := unpricedScopes(budgetIDs, holder)
	for _, row := range rows {
		r.unpriced.record(holderScopes, row)
	}
	if r.logger != nil {
		r.logger.Warn("governance: unpriced answer under a dollar budget (TXE-7957): request %s, provider %s, model %s, request type %s was charged $0 against budgets %v for virtual key %q; its requests charged to them on it are refused from now on until it is priced",
			requestID, provider, model, requestType, budgetIDs, holder)
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
	// Nor did a chat answer the provider's filter blocked before generating (Gemini's prompt block:
	// every choice finished by content_filter, and no tokens counted).
	if chat := result.ChatResponse; chat != nil && contentFilteredWithoutUsage(chat) {
		return false
	}
	// A passthrough the provider refused (any status but 2xx) delivered nothing, and nothing is
	// priced on it; one that only acknowledges queued work has not finished; one that counts a
	// prompt's tokens (count_tokens, input_tokens, countTokens) generates nothing and is free.
	if passthrough := result.PassthroughResponse; passthrough != nil {
		if passthroughCountsTokens(passthroughPath(result)) {
			return false
		}
		if passthrough.StatusCode != 0 && (passthrough.StatusCode < 200 || passthrough.StatusCode >= 300) {
			return false
		}
		if passthroughQueuedAcknowledgment(passthrough.Body) || passthroughErrorEnvelope(passthrough.Body) {
			return false
		}
	}
	// A Live billing unit closed with nothing in it (a session ended before its backend turn or with
	// no voice seconds) is a synthetic record of no work, not an answer.
	if response := result.ResponsesResponse; response != nil && response.ExtraFields.RequestType == schemas.LiveRequest && emptyResponsesUsage(response.Usage) {
		return false
	}
	// So is a Realtime turn that produced nothing and reported no usage (an empty response.done).
	if response := result.ResponsesResponse; response != nil && response.ExtraFields.RequestType == schemas.RealtimeRequest &&
		emptyResponsesUsage(response.Usage) && len(response.Output) == 0 {
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
	return r.priceIdentityIn(provider, model, requestType, scopes)
}

// liveUnitPricingMode is the mode a live unit is priced in: the live transport marks each unit
// (BifrostContextKeyLiveUnit), and accounting prices a voice window in the live (realtime) mode
// and a backend model's delegated turn in the Responses mode. Any other request is priced in its
// own type.
func liveUnitPricingMode(ctx *schemas.BifrostContext, requestType schemas.RequestType) schemas.RequestType {
	if requestType == schemas.LiveRequest && ctx != nil {
		if unit, _ := ctx.Value(schemas.BifrostContextKeyLiveUnit).(string); unit == "backend" {
			return schemas.ResponsesRequest
		}
	}
	return requestType
}

// pricingModesOf is the request types an answer of a request type is remembered under: a live
// answer under both of the modes its units are priced in, so the gate finds it whichever unit
// asks next.
func pricingModesOf(requestType schemas.RequestType) []schemas.RequestType {
	if requestType == schemas.LiveRequest {
		return []schemas.RequestType{schemas.LiveRequest, schemas.ResponsesRequest}
	}
	return []schemas.RequestType{requestType}
}

// priceIdentityIn is priceIdentity for one pricing mode.
func (r *BudgetResolver) priceIdentityIn(provider schemas.ModelProvider, model string, requestType schemas.RequestType, scopes *modelcatalog.PricingLookupScopes) (string, modelcatalog.PriceStatus) {
	return r.bestOverKeyScopes(provider, model, scopes, nil, func(candidate string, candidateScopes *modelcatalog.PricingLookupScopes) (string, modelcatalog.PriceStatus) {
		return r.modelCatalog.PriceIdentity(provider, candidate, requestType, candidateScopes)
	})
}

// admissionPriceStatus is the request's price status read only under keys that may serve it
// (servingKeys): a model priced only through a key the caller cannot use, or a disabled one, is not
// priced for it (TXE-8016).
func (r *BudgetResolver) admissionPriceStatus(ctx *schemas.BifrostContext, provider schemas.ModelProvider, model string, requestType schemas.RequestType, scopes *modelcatalog.PricingLookupScopes) modelcatalog.PriceStatus {
	_, status := r.bestOverKeyScopes(provider, model, scopes, r.servingKeys(ctx, provider, model, scopes, false), func(candidate string, candidateScopes *modelcatalog.PricingLookupScopes) (string, modelcatalog.PriceStatus) {
		return r.modelCatalog.PriceIdentity(provider, candidate, requestType, candidateScopes)
	})
	return status
}

// batchPriceStatus is the best batch price status (modelcatalog.BatchPriceStatus) of a batch line
// settled as requestType, under the scopes given and under each key that may serve the batch: one
// with use_for_batch_api, as core selects batch keys (TXE-8016).
func (r *BudgetResolver) batchPriceStatus(ctx *schemas.BifrostContext, provider schemas.ModelProvider, model string, requestType schemas.RequestType, scopes *modelcatalog.PricingLookupScopes) modelcatalog.PriceStatus {
	_, status := r.bestOverKeyScopes(provider, model, scopes, r.servingKeys(ctx, provider, model, scopes, true), func(candidate string, candidateScopes *modelcatalog.PricingLookupScopes) (string, modelcatalog.PriceStatus) {
		return "", r.modelCatalog.BatchPriceStatus(provider, candidate, requestType, candidateScopes)
	})
	return status
}

// servingKeys reports which configured keys may serve a request, as core selects them: the key it
// pinned, else the keys its access allows for the model; enabled ones only, and for a batch only
// those with use_for_batch_api. When none of the provider's keys qualifies (the request will fail
// key selection), every enabled key is read rather than none.
func (r *BudgetResolver) servingKeys(ctx *schemas.BifrostContext, provider schemas.ModelProvider, model string, scopes *modelcatalog.PricingLookupScopes, batch bool) func(schemas.Key) bool {
	pinned := ""
	if scopes != nil {
		pinned = scopes.SelectedKeyID
	}
	var allowed []string
	restricted := false
	if ctx != nil && ctx.Grant() != nil {
		if access := ctx.Grant().Access(); access != nil {
			allowed, restricted = access.KeysForModel(string(provider), model)
		}
	}
	enabled := func(key schemas.Key) bool { return key.Enabled == nil || *key.Enabled }
	serves := func(key schemas.Key) bool {
		if !enabled(key) || (pinned != "" && key.ID != pinned) || (restricted && !slices.Contains(allowed, key.ID)) {
			return false
		}
		return !batch || (key.UseForBatchAPI != nil && *key.UseForBatchAPI)
	}
	if r.governanceInMemoryStore != nil {
		if config, ok := configuredProvider(r.governanceInMemoryStore, provider); ok && !slices.ContainsFunc(config.Keys, serves) {
			return enabled
		}
	}
	return serves
}

// bestOverKeyScopes reads lookup under the scopes given, then, unless that prices the model,
// under the scope of each configured key eligible accepts (every key when nil) for the model and
// the key's alias targets, and returns the best it found: which key serves the request is chosen
// after admission.
func (r *BudgetResolver) bestOverKeyScopes(provider schemas.ModelProvider, model string, scopes *modelcatalog.PricingLookupScopes, eligible func(schemas.Key) bool, lookup func(candidate string, candidateScopes *modelcatalog.PricingLookupScopes) (string, modelcatalog.PriceStatus)) (string, modelcatalog.PriceStatus) {
	bestRow, best := lookup(model, scopes)
	if best == modelcatalog.PricePriced || model == "" || r.governanceInMemoryStore == nil {
		return bestRow, best
	}
	config, ok := configuredProvider(r.governanceInMemoryStore, provider)
	if !ok {
		return bestRow, best
	}
	for _, key := range config.Keys {
		if eligible != nil && !eligible(key) {
			continue
		}
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
			if row, status := lookup(candidate, &keyScopes); status > best {
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
	best := modelcatalog.PriceUnpriced
	var rows []string
	for _, mode := range pricingModesOf(requestType) {
		status, modeRows := r.priceIdentitiesIn(provider, model, mode, scopes)
		if status > best {
			best = status
		}
		for _, row := range modeRows {
			if !slices.Contains(rows, row) {
				rows = append(rows, row)
			}
		}
	}
	return best, rows
}

// priceIdentitiesIn is priceIdentities for one pricing mode.
func (r *BudgetResolver) priceIdentitiesIn(provider schemas.ModelProvider, model string, requestType schemas.RequestType, scopes *modelcatalog.PricingLookupScopes) (modelcatalog.PriceStatus, []string) {
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
// usagelessAnswerVerdict is the verdict on a $0 answer that reported no usage at all (a
// passthrough body model with no usage the provider could read): free when the row the serving key
// prices the model on prices nothing above zero, unpriced otherwise. With no usage there are no
// rates to read, so only the row can say.
func (r *BudgetResolver) usagelessAnswerVerdict(provider schemas.ModelProvider, model string, pricedAs schemas.RequestType, scopes *modelcatalog.PricingLookupScopes) modelcatalog.PriceStatus {
	if r.servedPriceStatus(provider, model, pricedAs, scopes) == modelcatalog.PriceFree {
		return modelcatalog.PriceFree
	}
	return modelcatalog.PriceUnpriced
}

func (r *BudgetResolver) servedPriceStatus(provider schemas.ModelProvider, model string, requestType schemas.RequestType, scopes *modelcatalog.PricingLookupScopes) modelcatalog.PriceStatus {
	for _, candidate := range r.servedCandidates(provider, model, scopes) {
		if row, status := r.modelCatalog.PriceIdentity(provider, candidate, requestType, scopes); row != "" {
			return status
		}
	}
	return modelcatalog.PriceUnpriced
}

// servedCandidates is the order accounting resolves a served model's price in: under the serving
// key's alias, its model name, then its model id, then the label itself; the label alone when no
// key is selected or the key has no alias for it.
func (r *BudgetResolver) servedCandidates(provider schemas.ModelProvider, model string, scopes *modelcatalog.PricingLookupScopes) []string {
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
	return candidates
}

// passthroughGenerated reports whether a passthrough answer is a provider's success: a body that
// named the model it runs was accepted and answered.
func passthroughGenerated(result *schemas.BifrostResponse) bool {
	if result == nil || result.PassthroughResponse == nil {
		return false
	}
	status := result.PassthroughResponse.StatusCode
	return status >= 200 && status < 300 && !passthroughQueuedAcknowledgment(result.PassthroughResponse.Body) &&
		!passthroughErrorEnvelope(result.PassthroughResponse.Body)
}

// passthroughErrorEnvelope reports whether a 2xx passthrough body reports only failure: a
// non-empty top-level "errors" array and no "data" item (Runware answers a failed task with 200).
// A body with any data delivered something and is judged as an answer.
func passthroughErrorEnvelope(body []byte) bool {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return false
	}
	errs := gjson.GetBytes(body, "errors")
	if !errs.IsArray() || len(errs.Array()) == 0 {
		return false
	}
	data := gjson.GetBytes(body, "data")
	return !data.Exists() || (data.IsArray() && len(data.Array()) == 0)
}

// passthroughBodyNamesEveryModel reports whether a passthrough body names a model for every unit
// of work it carries, read as passthroughBodyModels reads it: a JSON object with a non-empty
// string "model", or a non-empty JSON array every element of which is such an object. A task
// naming no model runs whatever the provider defaults to, which governance cannot price.
func passthroughBodyNamesEveryModel(req *schemas.BifrostRequest) bool {
	if req.PassthroughRequest == nil || len(req.PassthroughRequest.Body) == 0 || !gjson.ValidBytes(req.PassthroughRequest.Body) {
		return false
	}
	// Only a body the shared classifier reads as JSON names its models, as passthroughBodyModels
	// reads them (TXE-8012): one declared as another type names none, and is gated as the envelope.
	if schemas.ClassifyForwardedBody(req.ForwardedBodyContentType(), req.PassthroughRequest.Body).Format != schemas.ForwardedBodyJSON {
		return false
	}
	// A model key given twice is read as the first by gjson and as the last by most providers
	// (encoding/json among them, which also matches "Model" or "MODEL"), so the body would be gated
	// on one model and run on another: it is not read as naming its model.
	names := func(object gjson.Result) bool {
		if !object.IsObject() {
			return false
		}
		count := 0
		object.ForEach(func(key, _ gjson.Result) bool {
			if strings.EqualFold(key.Str, schemas.BodyModelKey) {
				count++
			}
			return count < 2
		})
		value := object.Get(schemas.BodyModelKey)
		return count == 1 && value.Type == gjson.String && strings.TrimSpace(value.Str) != ""
	}
	root := gjson.ParseBytes(req.PassthroughRequest.Body)
	switch {
	case root.IsObject():
		return names(root)
	case root.IsArray():
		// Walked in place rather than materialised: a body may hold millions of tasks.
		tasks, all := 0, true
		root.ForEach(func(_, task gjson.Result) bool {
			tasks++
			all = names(task)
			return all
		})
		return tasks > 0 && all
	}
	return false
}

// passthroughContainerMemoryLimit returns the memory tier a passthrough container create names, which
// selects the pricing row it is probed and priced on. It is read from a body the shared classifier
// reads as JSON (TXE-8012) and, failing closed, from any other body that parses as JSON: an
// upstream ignoring the declared type runs the tier the body names, so the default tier is never
// substituted for it.
func passthroughContainerMemoryLimit(req *schemas.BifrostRequest) string {
	if req == nil || req.PassthroughRequest == nil {
		return ""
	}
	body := req.PassthroughRequest.Body
	if schemas.ClassifyForwardedBody(req.ForwardedBodyContentType(), body).Format != schemas.ForwardedBodyJSON && !gjson.ValidBytes(body) {
		return ""
	}
	return gjson.GetBytes(body, "memory_limit").String()
}

// inlineBatchModes indexes, once per request, the request types settlement prices an inline batch's
// requests as, by the model each runs: jobaccounting.BatchRequestType of the request's url, else of
// the batch's endpoint, an empty one included (settlement prices it as chat, as Anthropic's and
// Gemini's batches run Messages and generateContent). A request naming no model runs the batch's
// own. PreLLMHook evaluates every distinct model of the batch, and each pass reads the one index.
type inlineBatchModes struct {
	once    sync.Once
	byModel map[string][]schemas.RequestType
}

// of returns the request types the requests running model are priced as; nil when none runs it.
func (m *inlineBatchModes) of(req *schemas.BifrostRequest, model string) []schemas.RequestType {
	if m == nil {
		return nil
	}
	m.once.Do(func() { m.byModel = indexInlineBatchModes(req) })
	return m.byModel[model]
}

func indexInlineBatchModes(req *schemas.BifrostRequest) map[string][]schemas.RequestType {
	if req == nil || req.RequestType != schemas.BatchCreateRequest || req.BatchCreateRequest == nil || len(req.BatchCreateRequest.Requests) == 0 {
		return nil
	}
	batch := req.BatchCreateRequest
	byModel := make(map[string][]schemas.RequestType)
	for _, item := range batch.Requests {
		models := batchItemModels(item)
		if len(models) == 0 {
			models = []string{batchEnvelopeModel(batch)}
		}
		endpoint := batch.Endpoint
		if item.URL != "" {
			endpoint = schemas.BatchEndpoint(item.URL)
		}
		mode := jobaccounting.BatchRequestType(endpoint)
		for _, model := range models {
			if !slices.Contains(byModel[model], mode) {
				byModel[model] = append(byModel[model], mode)
			}
		}
	}
	return byModel
}

// passthroughContainerCreate reports whether a passthrough on an OpenAI-shaped provider creates a
// container (POST .../containers), classified as the provider classifies it for usage extraction.
func passthroughContainerCreate(base schemas.ModelProvider, req *schemas.BifrostRequest) bool {
	if req == nil || req.PassthroughRequest == nil || (base != schemas.OpenAI && base != schemas.Azure) {
		return false
	}
	return openai.ClassifyPassthroughPath(req.PassthroughRequest.Method, req.PassthroughRequest.Path) == schemas.ContainerCreateRequest
}

// batchEnvelopeModel is the model a batch create names on itself.
func batchEnvelopeModel(batch *schemas.BifrostBatchCreateRequest) string {
	if batch.Model == nil {
		return ""
	}
	return *batch.Model
}

// passthroughPath is the provider path a passthrough answer was served on, "" for any other answer.
func passthroughPath(result *schemas.BifrostResponse) string {
	if result == nil || result.PassthroughResponse == nil {
		return ""
	}
	if path := result.PassthroughResponse.Path; path != "" {
		return path
	}
	return result.PassthroughResponse.ExtraFields.PassthroughPath
}

// passthroughCountsTokens reports whether a passthrough path counts a prompt's tokens without
// generating: Anthropic's /v1/messages/count_tokens, OpenAI's /v1/responses/input_tokens,
// Gemini's and Vertex's :countTokens.
func passthroughCountsTokens(path string) bool {
	if idx := strings.IndexByte(path, '?'); idx >= 0 {
		path = path[:idx]
	}
	path = strings.ToLower(path)
	return strings.HasSuffix(path, "/count_tokens") || strings.HasSuffix(path, "/input_tokens") || strings.HasSuffix(path, ":counttokens")
}

// contentFilteredWithoutUsage reports whether every choice of a chat answer finished by the
// provider's content filter and the answer counted no tokens.
func contentFilteredWithoutUsage(chat *schemas.BifrostChatResponse) bool {
	if len(chat.Choices) == 0 {
		return false
	}
	for _, choice := range chat.Choices {
		if choice.FinishReason == nil || *choice.FinishReason != "content_filter" {
			return false
		}
	}
	usage := chat.Usage
	return usage == nil || (usage.PromptTokens == 0 && usage.CompletionTokens == 0 && usage.TotalTokens == 0)
}

// emptyResponsesUsage reports whether a Responses usage record counts nothing at all.
func emptyResponsesUsage(usage *schemas.ResponsesResponseUsage) bool {
	return usage == nil || (usage.InputTokens == 0 && usage.OutputTokens == 0 && usage.TotalTokens == 0 &&
		(usage.AudioSeconds == nil || *usage.AudioSeconds == 0) && (usage.TextMessages == nil || *usage.TextMessages == 0) &&
		usage.GuardrailUnits == nil && usage.Cost == nil && len(usage.Iterations) == 0)
}

// passthroughRead reports whether a request is a passthrough read (GET or HEAD with no body):
// polling an operation or fetching a resource runs no model, so it has nothing to price. A GET
// carrying a body is forwarded as sent, and an upstream accepting it could run one: it is priced.
func passthroughRead(req *schemas.BifrostRequest) bool {
	if req == nil || req.PassthroughRequest == nil || len(bytes.TrimSpace(req.PassthroughRequest.Body)) > 0 {
		return false
	}
	switch strings.ToUpper(req.PassthroughRequest.Method) {
	case "GET", "HEAD":
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
		if len(batchItemModels(item)) == 0 {
			return false
		}
	}
	return true
}

// distinctBodyModels returns the first of the distinct models a passthrough body names, named as
// PreLLMHook evaluates them (a Gemini or Vertex resource name reduced to its bare id), and how many
// there are.
func (r *BudgetResolver) distinctBodyModels(provider schemas.ModelProvider, req *schemas.BifrostRequest) (string, int) {
	base := configuredBaseProvider(r.governanceInMemoryStore, provider)
	resourceNamed := base == schemas.Gemini || base == schemas.Vertex
	var first string
	distinct := make(map[string]struct{})
	for _, bodyModel := range passthroughBodyModels(req) {
		if resourceNamed {
			bodyModel = bareResourceModel(bodyModel)
		}
		if _, seen := distinct[bodyModel]; !seen {
			distinct[bodyModel] = struct{}{}
			if first == "" {
				first = bodyModel
			}
		}
	}
	return first, len(distinct)
}

// passthroughQueuedAcknowledgment reports whether a passthrough answer only acknowledges queued
// work: a JSON body whose "data" items carry nothing but task bookkeeping (Runware's taskType and
// taskUUID, a status), so no generation has finished and nothing is priced yet.
func passthroughQueuedAcknowledgment(body []byte) bool {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return false
	}
	data := gjson.GetBytes(body, "data")
	if !data.IsArray() {
		return false
	}
	items, queued := 0, true
	data.ForEach(func(_, item gjson.Result) bool {
		items++
		if !item.IsObject() {
			queued = false
			return false
		}
		item.ForEach(func(key, _ gjson.Result) bool {
			switch key.Str {
			case "taskType", "taskUUID", "status", "deliveryMethod":
				return true
			}
			queued = false
			return false
		})
		return queued
	})
	return items > 0 && queued
}

// normalizesToUnknown reports whether a request type has no pricing mode of its own.
func normalizesToUnknown(requestType schemas.RequestType) bool {
	return requestType == schemas.PassthroughRequest || requestType == schemas.PassthroughStreamRequest || requestType == schemas.UnknownRequest
}
