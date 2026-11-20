package governance

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/capsohq/bifrost/framework/configstore"
	configstoreTables "github.com/capsohq/bifrost/framework/configstore/tables"
	"github.com/capsohq/bifrost/framework/jobaccounting"
	"github.com/capsohq/bifrost/framework/modelcatalog"
	"github.com/capsohq/bifrost/framework/modelcatalog/datasheet"
	"github.com/capsohq/bifrost/plugins/governance/internal/testtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// declaredRequestTypes reads every RequestType constant from core/schemas, so a request type added
// later is checked without editing this test.
func declaredRequestTypes(t *testing.T) []schemas.RequestType {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, "../../core/schemas", nil, 0)
	require.NoError(t, err)
	var types []schemas.RequestType
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				gen, ok := decl.(*ast.GenDecl)
				if !ok || gen.Tok != token.CONST {
					continue
				}
				for _, spec := range gen.Specs {
					value, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					ident, ok := value.Type.(*ast.Ident)
					if !ok || ident.Name != "RequestType" {
						continue
					}
					for _, v := range value.Values {
						lit, ok := v.(*ast.BasicLit)
						if !ok || lit.Kind != token.STRING {
							continue
						}
						s, err := strconv.Unquote(lit.Value)
						require.NoError(t, err)
						types = append(types, schemas.RequestType(s))
					}
				}
			}
		}
	}
	require.Greater(t, len(types), 50, "found only %d RequestType constants in core/schemas; the parser no longer sees them", len(types))
	return types
}

// TestEveryRequestTypeHasABudgetPricing is the request-type half of the TXE-7957 class check: every
// request type says how a dollar budget is charged for it, so a new operation cannot reach a
// provider under a budget with nobody having decided whether its answers are priced. A type that
// is no longer declared must leave the table.
func TestEveryRequestTypeHasABudgetPricing(t *testing.T) {
	declared := map[schemas.RequestType]bool{}
	for _, requestType := range declaredRequestTypes(t) {
		declared[requestType] = true
		_, classified := requestTypeBudgetPricing[requestType]
		assert.True(t, classified, "request type %q has no entry in requestTypeBudgetPricing: say whether its answer is priced, priced at settlement, or never charged", requestType)
	}
	for requestType := range requestTypeBudgetPricing {
		assert.True(t, declared[requestType], "requestTypeBudgetPricing lists %q, which core/schemas no longer declares", requestType)
	}
	// What is priced at settlement must be what job accounting settles; anything else priced on a
	// queued create would charge nothing and never be judged.
	for requestType, pricing := range requestTypeBudgetPricing {
		if pricing == budgetPricingSettled {
			assert.Contains(t, []schemas.RequestType{schemas.VideoGenerationRequest, schemas.VideoEditRequest, schemas.VideoRemixRequest, schemas.BatchCreateRequest}, requestType,
				"%s is marked priced at settlement, but framework/jobaccounting settles only video and batch jobs", requestType)
		}
	}
}

// The unpriced set refuses a row only for the budgets its $0 answer was charged to, and a budget
// that fills is refused on every row while other budgets are not.
func TestUnpricedAnswers(t *testing.T) {
	u := newUnpricedAnswers()
	u.record([]string{"team", "key-a"}, "openai|image_generation|token-image")

	budget, refused := u.refused([]string{"key-b", "team"}, "openai|image_generation|token-image")
	assert.True(t, refused, "a budget the $0 answer was charged to is refused on that row")
	assert.Equal(t, "team", budget)
	_, refused = u.refused([]string{"key-b"}, "openai|image_generation|token-image")
	assert.False(t, refused, "another tenant's budget is not refused")
	_, refused = u.refused([]string{"key-a"}, "openai|chat|gpt-4o")
	assert.False(t, refused, "another row is not refused")
	u.record([]string{"key-a"}, "")
	_, refused = u.refused([]string{"key-a"}, "")
	assert.False(t, refused, "a request with no row is the gate's to refuse, not the set's")

	for i := 0; len(u.byBudget["key-a"].rows) < maxUnpricedRowsPerBudget; i++ {
		u.record([]string{"key-a"}, "row-"+strconv.Itoa(i))
	}
	_, refused = u.refused([]string{"key-a"}, "never-seen")
	assert.False(t, refused)
	u.record([]string{"key-a"}, "one-too-many")
	_, refused = u.refused([]string{"key-a"}, "never-seen")
	assert.True(t, refused, "a full budget is refused on every row rather than forget one")
	_, refused = u.refused([]string{"key-b"}, "never-seen")
	assert.False(t, refused, "a full budget refuses only itself")
	assert.LessOrEqual(t, len(u.byBudget["key-a"].rows), maxUnpricedRowsPerBudget)
}

// The hook tests below drive PreLLMHook, PostLLMHook and ReportUsage, which exist before TXE-7957.

// unpricedBudgetPlugin is a governance plugin over a price book with a priced chat model (gpt-4o),
// a free one (free-chat), an image model priced only per image token (token-image), a video model
// priced per second (clip-1), a model priced only by an override scoped to the openai provider key
// (key-priced), a priced model that override's key makes free (free-on-key), and none for
// house-llm; seconds-chat is a chat row priced only per second, which a token answer does not
// carry. vk-budget and vk-other-tenant each have their own $100 budget; vk-open has none.
func unpricedBudgetPlugin(t testing.TB) (*GovernancePlugin, *LocalGovernanceStore) {
	t.Helper()
	sheet := filepath.Join(t.TempDir(), "pricing.json")
	require.NoError(t, os.WriteFile(sheet, []byte(`{
		"gpt-4o":{"provider":"openai","mode":"chat","input_cost_per_token":0.0000025,"output_cost_per_token":0.00001},
		"free-chat":{"provider":"openai","mode":"chat","input_cost_per_token":0,"output_cost_per_token":0},
		"token-image":{"provider":"openai","mode":"image_generation","output_cost_per_image_token":0.00004},
		"clip-1":{"provider":"openai","mode":"video_generation","output_cost_per_second":0.1},
		"free-on-key":{"provider":"openai","mode":"chat","input_cost_per_token":0.000001,"output_cost_per_token":0.000002},
		"embed-1":{"provider":"openai","mode":"embedding","input_cost_per_token":0.0000001},
		"alias-label":{"provider":"openai","mode":"chat","input_cost_per_token":0.000001,"output_cost_per_token":0.000002},
		"container-4g":{"provider":"openai","mode":"container_create","code_interpreter_cost_per_session":0.12},
		"responses-free":{"provider":"openai","mode":"chat","input_cost_per_token":0.000001,"output_cost_per_token":0.000002},
		"seconds-chat":{"provider":"openai","mode":"chat","input_cost_per_second":0.001}
	}`), 0o600))
	ds := datasheet.New(nil, NewMockLogger(), datasheet.Config{URL: "file://" + sheet})
	require.NoError(t, ds.LoadFromURLIntoMemory(context.Background()))
	keyID := "openai-key-1"
	require.NoError(t, ds.SetOverrides([]configstoreTables.TablePricingOverride{{
		ID: "key-priced-override", ScopeKind: string(datasheet.ScopeKindProviderKey), ProviderKeyID: &keyID,
		MatchType: string(datasheet.MatchTypeExact), Pattern: "key-priced", RequestTypes: []schemas.RequestType{schemas.ChatCompletionRequest},
		PricingPatchJSON: `{"input_cost_per_token":0.000001,"output_cost_per_token":0.000002}`,
	}, {
		ID: "free-on-key-override", ScopeKind: string(datasheet.ScopeKindProviderKey), ProviderKeyID: &keyID,
		MatchType: string(datasheet.MatchTypeExact), Pattern: "free-on-key", RequestTypes: []schemas.RequestType{schemas.ChatCompletionRequest},
		PricingPatchJSON: `{"input_cost_per_token":0,"output_cost_per_token":0}`,
	}, {
		ID: "responses-free-override", ScopeKind: string(datasheet.ScopeKindGlobal),
		MatchType: string(datasheet.MatchTypeExact), Pattern: "responses-free", RequestTypes: []schemas.RequestType{schemas.ResponsesRequest},
		PricingPatchJSON: `{"input_cost_per_token":0,"output_cost_per_token":0}`,
	}}))
	catalog := modelcatalog.NewTestCatalogWithDatasheet(ds)

	logger := NewMockLogger()
	budget := buildBudgetWithUsage("budget-unpriced", 100.0, 0, "1d")
	budgeted := buildVirtualKeyWithBudget("vk-budget", "sk-bf-budget", "Budget VK", budget)
	otherBudget := buildBudgetWithUsage("budget-other-tenant", 100.0, 0, "1d")
	otherTenant := buildVirtualKeyWithBudget("vk-other-tenant", "sk-bf-other", "Other tenant VK", otherBudget)
	open := buildVirtualKey("vk-open", "sk-bf-open", "Open VK", true)
	open.ProviderConfigs = []configstoreTables.TableVirtualKeyProviderConfig{buildProviderConfig("openai", []string{"*"})}
	store, err := NewLocalGovernanceStore(context.Background(), logger, nil, &configstore.GovernanceConfig{
		VirtualKeys: []configstoreTables.TableVirtualKey{*budgeted, *otherTenant, *open},
		Budgets:     []configstoreTables.TableBudget{*budget, *otherBudget},
	}, nil, nil)
	require.NoError(t, err)
	providers := &mockInMemoryStore{configuredProviders: map[schemas.ModelProvider]configstore.ProviderConfig{
		schemas.OpenAI: {Keys: []schemas.Key{{ID: keyID, Aliases: schemas.KeyAliases{"alias-label": {ModelID: "free-chat"}}}}},
	}}
	plugin, err := InitFromStore(context.Background(), &Config{IsVkMandatory: boolPtr(false)}, logger, store, nil, catalog, nil, providers)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, plugin.Cleanup()) })
	return plugin, store
}

func requireUnpricedRefusal(t *testing.T, shortCircuit *schemas.LLMPluginShortCircuit, what string) {
	t.Helper()
	require.NotNil(t, shortCircuit, "%s: a request under a dollar budget that cannot be priced must be refused before the call", what)
	require.NotNil(t, shortCircuit.Error, what)
	require.NotNil(t, shortCircuit.Error.StatusCode, what)
	assert.Equal(t, 403, *shortCircuit.Error.StatusCode, what)
	assert.Equal(t, schemas.ErrorTypePolicyModelBlocked, shortCircuit.Error.ExtraFields.ErrorType, what)
	require.NotNil(t, shortCircuit.Error.Type, what)
	assert.Equal(t, "model_unpriced", *shortCircuit.Error.Type, what)
}

func chatOn(model string) *schemas.BifrostRequest {
	return &schemas.BifrostRequest{RequestType: schemas.ChatCompletionRequest, ChatRequest: &schemas.BifrostChatRequest{Provider: schemas.OpenAI, Model: model}}
}

func preLLM(t testing.TB, plugin *GovernancePlugin, vk string, req *schemas.BifrostRequest) *schemas.LLMPluginShortCircuit {
	t.Helper()
	_, shortCircuit, err := plugin.PreLLMHook(resolverCtx(plugin.store, vk), req)
	require.NoError(t, err)
	return shortCircuit
}

// A virtual key with a dollar budget cannot obtain answers from a model the price book cannot
// price: each request is refused before the upstream call, however often it is repeated. Priced
// and free models, a model priced only once its key is selected, a key with no budget, an inline
// batch whose items are all priced, and an operation that is never charged are unaffected.
func TestUnpricedModelUnderADollarBudgetIsRefusedBeforeTheCall(t *testing.T) {
	plugin, _ := unpricedBudgetPlugin(t)

	for i := 0; i < 3; i++ {
		requireUnpricedRefusal(t, preLLM(t, plugin, "sk-bf-budget", chatOn("house-llm")), "chat on a model with no row")
	}
	requireUnpricedRefusal(t, preLLM(t, plugin, "sk-bf-budget", &schemas.BifrostRequest{
		RequestType: schemas.ChatCompletionStreamRequest, ChatRequest: &schemas.BifrostChatRequest{Provider: schemas.OpenAI, Model: "house-llm"},
	}), "a streamed chat on a model with no row")
	requireUnpricedRefusal(t, preLLM(t, plugin, "sk-bf-budget", &schemas.BifrostRequest{
		RequestType: schemas.EmbeddingRequest, EmbeddingRequest: &schemas.BifrostEmbeddingRequest{Provider: schemas.OpenAI, Model: "gpt-4o"},
	}), "an embedding on a model priced only for chat")

	assert.Nil(t, preLLM(t, plugin, "sk-bf-budget", chatOn("gpt-4o")), "a priced model is served under a budget")
	assert.Nil(t, preLLM(t, plugin, "sk-bf-budget", chatOn("free-chat")), "a model the price book prices at $0 is free, not unpriced")
	assert.Nil(t, preLLM(t, plugin, "sk-bf-budget", chatOn("key-priced")), "a model priced by an override on the provider's key is priced before the key is selected")
	assert.Nil(t, preLLM(t, plugin, "sk-bf-open", chatOn("house-llm")), "a key with no dollar budget has nothing to charge, so it is not refused")
	assert.Nil(t, preLLM(t, plugin, "sk-bf-budget", &schemas.BifrostRequest{
		RequestType: schemas.FileListRequest, FileListRequest: &schemas.BifrostFileListRequest{Provider: schemas.OpenAI},
	}), "an operation that is never charged is not priced")

	inlineBatch := func(model string) *schemas.BifrostRequest {
		return &schemas.BifrostRequest{RequestType: schemas.BatchCreateRequest, BatchCreateRequest: &schemas.BifrostBatchCreateRequest{
			Provider: schemas.OpenAI,
			Requests: []schemas.BatchRequestItem{{CustomID: "1", Body: map[string]any{"model": model}}},
		}}
	}
	assert.Nil(t, preLLM(t, plugin, "sk-bf-budget", inlineBatch("gpt-4o")), "an inline batch naming no model of its own, whose items are priced, is served")
	requireUnpricedRefusal(t, preLLM(t, plugin, "sk-bf-budget", inlineBatch("house-llm")), "an inline batch with an item on a model with no row")
	requireUnpricedRefusal(t, preLLM(t, plugin, "sk-bf-budget", &schemas.BifrostRequest{RequestType: schemas.BatchCreateRequest, BatchCreateRequest: &schemas.BifrostBatchCreateRequest{
		Provider: schemas.OpenAI,
		Requests: []schemas.BatchRequestItem{{CustomID: "1", Body: map[string]any{"model": "gpt-4o"}}, {CustomID: "2", Body: map[string]any{"messages": []any{}}}},
	}}), "an inline batch with a request naming no model")
	// An inline batch is settled in its endpoint's mode (jobaccounting.BatchRequestType), so a model
	// is priced in that mode, not in whichever mode prices it.
	endpointBatch := func(endpoint schemas.BatchEndpoint, itemURL, model string) *schemas.BifrostRequest {
		return &schemas.BifrostRequest{RequestType: schemas.BatchCreateRequest, BatchCreateRequest: &schemas.BifrostBatchCreateRequest{
			Provider: schemas.OpenAI, Endpoint: endpoint,
			Requests: []schemas.BatchRequestItem{{CustomID: "1", URL: itemURL, Body: map[string]any{"model": model}}},
		}}
	}
	requireUnpricedRefusal(t, preLLM(t, plugin, "sk-bf-budget", endpointBatch(schemas.BatchEndpointEmbeddings, "", "gpt-4o")), "an embeddings batch on a model priced only for chat")
	assert.Nil(t, preLLM(t, plugin, "sk-bf-budget", endpointBatch(schemas.BatchEndpointChatCompletions, "", "gpt-4o")), "a chat batch on a model priced for chat is served")
	assert.Nil(t, preLLM(t, plugin, "sk-bf-budget", endpointBatch(schemas.BatchEndpointEmbeddings, "", "embed-1")), "an embeddings batch on a model priced for embeddings is served")
	requireUnpricedRefusal(t, preLLM(t, plugin, "sk-bf-budget", endpointBatch(schemas.BatchEndpointChatCompletions, "/v1/embeddings", "gpt-4o")), "a request whose own url is embeddings is priced as one")
	envelopeModel := "gpt-4o"
	envelope := endpointBatch(schemas.BatchEndpointEmbeddings, "", "embed-1")
	envelope.BatchCreateRequest.Model = &envelopeModel
	envelope.BatchCreateRequest.Requests = append(envelope.BatchCreateRequest.Requests, schemas.BatchRequestItem{CustomID: "2", Body: map[string]any{"input": "x"}})
	requireUnpricedRefusal(t, preLLM(t, plugin, "sk-bf-budget", envelope), "a request naming no model runs the batch's model, priced in the batch's mode")

	fileBatch := func(model *string) *schemas.BifrostRequest {
		return &schemas.BifrostRequest{RequestType: schemas.BatchCreateRequest, BatchCreateRequest: &schemas.BifrostBatchCreateRequest{
			Provider: schemas.OpenAI, Model: model, InputFileID: "file-abc",
		}}
	}
	requireUnpricedRefusal(t, preLLM(t, plugin, "sk-bf-budget", fileBatch(nil)), "a batch whose models are only inside its uploaded input")
	// OpenAI's batch API runs the models its input rows name: a priced model on the request is only
	// a hint, so it does not admit the batch.
	priced := "gpt-4o"
	requireUnpricedRefusal(t, preLLM(t, plugin, "sk-bf-budget", fileBatch(&priced)), "an openai file batch naming a priced model it does not bind")
	assert.Nil(t, preLLM(t, plugin, "sk-bf-open", fileBatch(nil)), "a file batch on a key with no dollar budget is not refused")

	// A passthrough whose body names its models (a Runware task array) names none on the request:
	// each body model is gated on its own, and the envelope is not refused for naming none.
	passthrough := func(body string) *schemas.BifrostRequest {
		return &schemas.BifrostRequest{RequestType: schemas.PassthroughRequest, PassthroughRequest: &schemas.BifrostPassthroughRequest{
			Provider: schemas.OpenAI, Method: "POST", Path: "/v1/tasks", Body: []byte(body),
		}}
	}
	assert.Nil(t, preLLM(t, plugin, "sk-bf-budget", passthrough(`[{"model":"gpt-4o"},{"model":"gpt-4o"}]`)), "a passthrough whose tasks all name one priced model is served")
	requireUnpricedRefusal(t, preLLM(t, plugin, "sk-bf-budget", passthrough(`[{"model":"gpt-4o"},{"model":"free-chat"}]`)), "a task array mixing models, whose one answer cannot be priced model by model")
	assert.Nil(t, preLLM(t, plugin, "sk-bf-open", passthrough(`[{"model":"gpt-4o"},{"model":"free-chat"}]`)), "a mixed task array on a key with no dollar budget is served")
	requireUnpricedRefusal(t, preLLM(t, plugin, "sk-bf-budget", passthrough(`[{"model":"gpt-4o"},{"model":"house-llm"}]`)), "a passthrough with a body model that has no row")
	requireUnpricedRefusal(t, preLLM(t, plugin, "sk-bf-budget", passthrough(`{"prompt":"no model anywhere"}`)), "a passthrough naming no model anywhere")
	requireUnpricedRefusal(t, preLLM(t, plugin, "sk-bf-budget", passthrough(`[{"model":"gpt-4o"},{"prompt":"runs the provider's default model"}]`)), "a task array with a task naming no model")
	// A body naming its model twice never reaches governance: core refuses a forwarded body that
	// repeats a member (TXE-7994, TestRepeatedBodyModelCannotRunAModelTheKeyIsNotAllowed).
	mismatched := passthrough(`{"model":"free-chat"}`)
	mismatched.PassthroughRequest.Model = "gpt-4o"
	requireUnpricedRefusal(t, preLLM(t, plugin, "sk-bf-budget", mismatched), "a request naming one model whose body names another")
	matched := passthrough(`{"model":"gpt-4o"}`)
	matched.PassthroughRequest.Model = "gpt-4o"
	assert.Nil(t, preLLM(t, plugin, "sk-bf-budget", matched), "a request whose body names the same model is served")

	// Creating Gemini cached content is charged by the vendor but priced by no Bifrost answer.
	cachedContent := &schemas.BifrostRequest{RequestType: schemas.CachedContentCreateRequest, CachedContentCreateRequest: &schemas.BifrostCachedContentCreateRequest{
		Provider: schemas.OpenAI, Model: "gpt-4o",
	}}
	requireUnpricedRefusal(t, preLLM(t, plugin, "sk-bf-budget", cachedContent), "cached content creation under a budget")
	assert.Nil(t, preLLM(t, plugin, "sk-bf-open", cachedContent), "cached content creation on a key with no dollar budget is not refused")
}

// A $0 answer the serving key's override prices at $0 is free, not unpriced, though another
// scope prices the model: it refuses nothing.
func TestFreeAnswerUnderTheServingKeysPriceDoesNotRefuseTheModel(t *testing.T) {
	plugin, _ := unpricedBudgetPlugin(t)
	ctx := resolverCtx(plugin.store, "sk-bf-budget")
	_, shortCircuit, err := plugin.PreLLMHook(ctx, chatOn("free-on-key"))
	require.NoError(t, err)
	require.Nil(t, shortCircuit)
	ctx.SetValue(schemas.BifrostContextKeySelectedKeyID, "openai-key-1")
	_, _, err = plugin.PostLLMHook(ctx, &schemas.BifrostResponse{ChatResponse: &schemas.BifrostChatResponse{
		Usage: &schemas.BifrostLLMUsage{PromptTokens: 100, CompletionTokens: 10, TotalTokens: 110},
		ExtraFields: schemas.BifrostResponseExtraFields{
			RequestType: schemas.ChatCompletionRequest, Provider: schemas.OpenAI, OriginalModelRequested: "free-on-key",
			RoutingInfo: schemas.RoutingInfo{Provider: schemas.OpenAI, Model: "free-on-key"},
		},
	}}, nil)
	require.NoError(t, err)
	plugin.wg.Wait()
	assert.Nil(t, preLLM(t, plugin, "sk-bf-budget", chatOn("free-on-key")), "an answer free under the key that served it does not refuse the model")
}

// A $0 answer recorded under the key that served it is found by the next request, which has not
// chosen a key yet: the gate checks every row the request could be priced as. key-priced is priced
// only by an override on the openai key, and the answer reports no usage.
func TestUnpricedAnswerUnderAKeyScopedPriceRefusesBeforeKeySelection(t *testing.T) {
	plugin, _ := unpricedBudgetPlugin(t)
	ctx := resolverCtx(plugin.store, "sk-bf-budget")
	_, shortCircuit, err := plugin.PreLLMHook(ctx, chatOn("key-priced"))
	require.NoError(t, err)
	require.Nil(t, shortCircuit, "a model priced by the key's override is admitted")
	ctx.SetValue(schemas.BifrostContextKeySelectedKeyID, "openai-key-1")
	_, _, err = plugin.PostLLMHook(ctx, &schemas.BifrostResponse{ChatResponse: &schemas.BifrostChatResponse{ExtraFields: schemas.BifrostResponseExtraFields{
		RequestType: schemas.ChatCompletionRequest, Provider: schemas.OpenAI, OriginalModelRequested: "key-priced",
		RoutingInfo: schemas.RoutingInfo{Provider: schemas.OpenAI, Model: "key-priced"},
	}}}, nil)
	require.NoError(t, err)
	plugin.wg.Wait()
	requireUnpricedRefusal(t, preLLM(t, plugin, "sk-bf-budget", chatOn("key-priced")), "the next request before its key is selected")
	assert.Nil(t, preLLM(t, plugin, "sk-bf-budget", chatOn("gpt-4o")), "another model under the same budget is still served")
}

// An answer the gate admitted that still priced at $0 under a dollar budget fails closed for that
// budget: the next request charged to it on the same price book row, however the model is spelled,
// is refused before the call. Another tenant's budget and a key with no budget are not refused.
// Image tokens are the row's only price here, and the answer reports none.
func TestUnpricedAnswerUnderADollarBudgetRefusesTheModelAfter(t *testing.T) {
	plugin, store := unpricedBudgetPlugin(t)
	image := func(model string) *schemas.BifrostRequest {
		return &schemas.BifrostRequest{
			RequestType:            schemas.ImageGenerationRequest,
			ImageGenerationRequest: &schemas.BifrostImageGenerationRequest{Provider: schemas.OpenAI, Model: model},
		}
	}
	ctx := resolverCtx(plugin.store, "sk-bf-budget")
	ctx.SetValue(schemas.BifrostContextKeyRequestID, "req-image-1")
	_, shortCircuit, err := plugin.PreLLMHook(ctx, image("token-image"))
	require.NoError(t, err)
	require.Nil(t, shortCircuit, "a model with a price is admitted")
	_, _, err = plugin.PostLLMHook(ctx, &schemas.BifrostResponse{ImageGenerationResponse: &schemas.BifrostImageGenerationResponse{
		Data: []schemas.ImageData{{URL: "https://example.invalid/1.png"}},
		ExtraFields: schemas.BifrostResponseExtraFields{
			RequestType: schemas.ImageGenerationRequest, Provider: schemas.OpenAI, OriginalModelRequested: "token-image",
			RoutingInfo: schemas.RoutingInfo{Provider: schemas.OpenAI, Model: "token-image"},
		},
	}}, nil)
	require.NoError(t, err)
	plugin.wg.Wait()
	assert.Zero(t, store.GetGovernanceData(context.Background()).Budgets["budget-unpriced"].CurrentUsage, "the first answer priced at $0")

	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		_, shortCircuit, err := plugin.PreLLMHook(resolverCtx(plugin.store, "sk-bf-budget"), image("token-image"))
		assert.NoError(c, err)
		if assert.NotNil(c, shortCircuit, "the model is refused under the budget once an answer priced at $0 against it") && assert.NotNil(c, shortCircuit.Error) {
			assert.Equal(c, schemas.ErrorTypePolicyModelBlocked, shortCircuit.Error.ExtraFields.ErrorType)
		}
	}, testtime.Scale(time.Second), testtime.Scale(10*time.Millisecond))

	assert.NotNil(t, preLLM(t, plugin, "sk-bf-budget", image("TOKEN-IMAGE")), "a spelling of the model that resolves to the same row is refused too")
	assert.Nil(t, preLLM(t, plugin, "sk-bf-open", image("token-image")), "a key with no dollar budget is not refused")
	assert.Nil(t, preLLM(t, plugin, "sk-bf-other", image("token-image")), "another tenant's budget never saw the $0 answer, so it is not refused")
}

// A background Responses create that is still queued, one that failed, and an answer served from
// the semantic cache carry no vendor usage: none is an unpriced answer, and none refuses the model.
func TestQueuedOrCachedAnswerDoesNotRefuseTheModel(t *testing.T) {
	plugin, _ := unpricedBudgetPlugin(t)
	responses := func(status string) *schemas.BifrostResponse {
		return &schemas.BifrostResponse{ResponsesResponse: &schemas.BifrostResponsesResponse{Status: &status, ExtraFields: schemas.BifrostResponseExtraFields{
			RequestType: schemas.ResponsesRequest, Provider: schemas.OpenAI, OriginalModelRequested: "gpt-4o",
			RoutingInfo: schemas.RoutingInfo{Provider: schemas.OpenAI, Model: "gpt-4o"},
		}}}
	}
	responsesRequest := &schemas.BifrostRequest{RequestType: schemas.ResponsesRequest, ResponsesRequest: &schemas.BifrostResponsesRequest{Provider: schemas.OpenAI, Model: "gpt-4o"}}
	cases := map[string]struct {
		request *schemas.BifrostRequest
		answer  *schemas.BifrostResponse
	}{
		"queued": {responsesRequest, responses("queued")},
		"failed": {responsesRequest, responses("failed")},
		"cached": {chatOn("gpt-4o"), &schemas.BifrostResponse{ChatResponse: &schemas.BifrostChatResponse{ExtraFields: schemas.BifrostResponseExtraFields{
			RequestType: schemas.ChatCompletionRequest, Provider: schemas.OpenAI, OriginalModelRequested: "gpt-4o",
			RoutingInfo: schemas.RoutingInfo{Provider: schemas.OpenAI, Model: "gpt-4o"},
			CacheDebug:  &schemas.BifrostCacheMetadata{CacheHit: true},
		}}}},
	}
	for name, tc := range cases {
		ctx := resolverCtx(plugin.store, "sk-bf-budget")
		_, shortCircuit, err := plugin.PreLLMHook(ctx, tc.request)
		require.NoError(t, err)
		require.Nil(t, shortCircuit, name)
		_, _, err = plugin.PostLLMHook(ctx, tc.answer, nil)
		require.NoError(t, err)
		plugin.wg.Wait()
		assert.Nil(t, preLLM(t, plugin, "sk-bf-budget", tc.request), "%s: the model is still served", name)
	}
}

// A job settles at $0 only when it owed nothing: job accounting reports a settlement to governance
// only when it is priced and complete. A failed or cancelled video settled at $0 does not refuse
// the model.
func TestUnbilledSettledJobDoesNotRefuseTheModel(t *testing.T) {
	plugin, _ := unpricedBudgetPlugin(t)
	video := &schemas.BifrostRequest{
		RequestType:            schemas.VideoGenerationRequest,
		VideoGenerationRequest: &schemas.BifrostVideoGenerationRequest{Provider: schemas.OpenAI, Model: "clip-1"},
	}
	require.Nil(t, preLLM(t, plugin, "sk-bf-budget", video), "a priced video model is admitted")
	require.NoError(t, plugin.ReportUsage(context.Background(), jobaccounting.UsageReport{
		RequestID: "video-job-1", Provider: schemas.OpenAI, Model: "clip-1", Cost: 0, BudgetIDs: []string{"budget-unpriced"},
	}))
	assert.Nil(t, preLLM(t, plugin, "sk-bf-budget", video), "a cancelled video's $0 settlement is its price, not a missing one")
	requireUnpricedRefusal(t, preLLM(t, plugin, "sk-bf-budget", &schemas.BifrostRequest{
		RequestType:            schemas.VideoGenerationRequest,
		VideoGenerationRequest: &schemas.BifrostVideoGenerationRequest{Provider: schemas.OpenAI, Model: "house-video"},
	}), "a video on a model with no row")
}

// BenchmarkUnpricedBudgetGate measures PreLLMHook on the paths TXE-7957 touches: a key with no
// dollar budget (the gate returns at once), a priced model under a budget (one price book lookup),
// and a model priced only by a key-scoped override under a budget (a lookup per configured key).
func BenchmarkUnpricedBudgetGate(b *testing.B) {
	plugin, _ := unpricedBudgetPlugin(b)
	for _, bc := range []struct {
		name, vk, model string
	}{
		{"no_budget", "sk-bf-open", "gpt-4o"},
		{"budget_priced", "sk-bf-budget", "gpt-4o"},
		{"budget_key_scoped_override", "sk-bf-budget", "key-priced"},
		{"no_budget_passthrough_task_array", "sk-bf-open", ""},
	} {
		b.Run(bc.name, func(b *testing.B) {
			req := chatOn(bc.model)
			if bc.model == "" {
				req = &schemas.BifrostRequest{RequestType: schemas.PassthroughRequest, PassthroughRequest: &schemas.BifrostPassthroughRequest{
					Provider: schemas.OpenAI, Method: "POST", Path: "/v1/tasks", Body: []byte(`[{"model":"gpt-4o"},{"model":"free-chat"},{"model":"gpt-4o"}]`),
				}}
			}
			b.ReportAllocs()
			for b.Loop() {
				if shortCircuit := preLLM(b, plugin, bc.vk, req); shortCircuit != nil {
					b.Fatalf("refused: %v", shortCircuit.Error)
				}
			}
		})
	}
}

// A passthrough answer charged under no model of its own is judged by the models its body names:
// a $0 answer refuses them under the budget it was charged to.
func TestUnpricedPassthroughAnswerRefusesItsBodyModels(t *testing.T) {
	plugin, _ := unpricedBudgetPlugin(t)
	req := &schemas.BifrostRequest{RequestType: schemas.PassthroughRequest, PassthroughRequest: &schemas.BifrostPassthroughRequest{
		Provider: schemas.OpenAI, Method: "POST", Path: "/v1/images", Body: []byte(`[{"model":"token-image"}]`),
	}}
	ctx := resolverCtx(plugin.store, "sk-bf-budget")
	_, shortCircuit, err := plugin.PreLLMHook(ctx, req)
	require.NoError(t, err)
	require.Nil(t, shortCircuit, "a passthrough whose body model is priced is admitted")
	_, _, err = plugin.PostLLMHook(ctx, &schemas.BifrostResponse{PassthroughResponse: &schemas.BifrostPassthroughResponse{
		PassthroughUsage: &schemas.BifrostPassthroughUsage{},
		ExtraFields: schemas.BifrostResponseExtraFields{
			RequestType: schemas.PassthroughRequest, Provider: schemas.OpenAI,
			RoutingInfo: schemas.RoutingInfo{Provider: schemas.OpenAI},
		},
	}}, nil)
	require.NoError(t, err)
	plugin.wg.Wait()
	requireUnpricedRefusal(t, preLLM(t, plugin, "sk-bf-budget", req), "the same passthrough after its answer priced at $0")
}

// A semantic-cache lookup priced beside a $0 answer (a cache miss) does not hide it either.
func TestUnpricedAnswerBesideAPricedCacheLookupRefusesTheModelAfter(t *testing.T) {
	plugin, _ := unpricedBudgetPlugin(t)
	image := &schemas.BifrostRequest{
		RequestType:            schemas.ImageGenerationRequest,
		ImageGenerationRequest: &schemas.BifrostImageGenerationRequest{Provider: schemas.OpenAI, Model: "token-image"},
	}
	ctx := resolverCtx(plugin.store, "sk-bf-budget")
	_, shortCircuit, err := plugin.PreLLMHook(ctx, image)
	require.NoError(t, err)
	require.Nil(t, shortCircuit)
	embedProvider, embedModel, tokens := "openai", "embed-1", 1000
	_, _, err = plugin.PostLLMHook(ctx, &schemas.BifrostResponse{ImageGenerationResponse: &schemas.BifrostImageGenerationResponse{
		Data: []schemas.ImageData{{URL: "https://example.invalid/1.png"}},
		ExtraFields: schemas.BifrostResponseExtraFields{
			RequestType: schemas.ImageGenerationRequest, Provider: schemas.OpenAI, OriginalModelRequested: "token-image",
			RoutingInfo: schemas.RoutingInfo{Provider: schemas.OpenAI, Model: "token-image"},
			CacheDebug:  &schemas.BifrostCacheMetadata{CacheHit: false, ProviderUsed: &embedProvider, ModelUsed: &embedModel, InputTokens: &tokens},
		},
	}}, nil)
	require.NoError(t, err)
	plugin.wg.Wait()
	requireUnpricedRefusal(t, preLLM(t, plugin, "sk-bf-budget", image), "the model whose answer priced at $0 beside a priced cache lookup")
}

// A routing classifier priced beside a $0 answer does not hide it: the answer is judged by its
// own cost.
func TestUnpricedAnswerBesideAPricedRoutingCallRefusesTheModelAfter(t *testing.T) {
	plugin, _ := unpricedBudgetPlugin(t)
	image := &schemas.BifrostRequest{
		RequestType:            schemas.ImageGenerationRequest,
		ImageGenerationRequest: &schemas.BifrostImageGenerationRequest{Provider: schemas.OpenAI, Model: "token-image"},
	}
	ctx := resolverCtx(plugin.store, "sk-bf-budget")
	_, shortCircuit, err := plugin.PreLLMHook(ctx, image)
	require.NoError(t, err)
	require.Nil(t, shortCircuit)
	classifier, classifierModel, tokens := "openai", "gpt-4o", 1000
	_, _, err = plugin.PostLLMHook(ctx, &schemas.BifrostResponse{ImageGenerationResponse: &schemas.BifrostImageGenerationResponse{
		Data: []schemas.ImageData{{URL: "https://example.invalid/1.png"}},
		ExtraFields: schemas.BifrostResponseExtraFields{
			RequestType: schemas.ImageGenerationRequest, Provider: schemas.OpenAI, OriginalModelRequested: "token-image",
			RoutingInfo: schemas.RoutingInfo{Provider: schemas.OpenAI, Model: "token-image"},
			RoutingMetadata: &schemas.BifrostRoutingMetadata{Calls: []schemas.BifrostRoutingCall{{
				RequestType: schemas.ChatCompletionRequest, ProviderUsed: &classifier, ModelUsed: &classifierModel, InputTokens: &tokens, CountTowardBudgets: true,
			}}},
		},
	}}, nil)
	require.NoError(t, err)
	plugin.wg.Wait()
	requireUnpricedRefusal(t, preLLM(t, plugin, "sk-bf-budget", image), "the model whose answer priced at $0 beside a priced classifier")
}

// An image request the content filter answered with no image delivered nothing to price: its $0
// refuses nothing.
func TestContentFilteredImageAnswerDoesNotRefuseTheModel(t *testing.T) {
	plugin, _ := unpricedBudgetPlugin(t)
	image := &schemas.BifrostRequest{
		RequestType:            schemas.ImageGenerationRequest,
		ImageGenerationRequest: &schemas.BifrostImageGenerationRequest{Provider: schemas.OpenAI, Model: "token-image"},
	}
	ctx := resolverCtx(plugin.store, "sk-bf-budget")
	_, shortCircuit, err := plugin.PreLLMHook(ctx, image)
	require.NoError(t, err)
	require.Nil(t, shortCircuit)
	_, _, err = plugin.PostLLMHook(ctx, &schemas.BifrostResponse{ImageGenerationResponse: &schemas.BifrostImageGenerationResponse{
		ContentFilter: &schemas.ContentFilterInfo{FilteredCount: 1},
		ExtraFields: schemas.BifrostResponseExtraFields{
			RequestType: schemas.ImageGenerationRequest, Provider: schemas.OpenAI, OriginalModelRequested: "token-image",
			RoutingInfo: schemas.RoutingInfo{Provider: schemas.OpenAI, Model: "token-image"},
		},
	}}, nil)
	require.NoError(t, err)
	plugin.wg.Wait()
	assert.Nil(t, preLLM(t, plugin, "sk-bf-budget", image), "a fully filtered answer does not refuse the model")
}

// A passthrough whose body names its model, answered with no usage the provider could read
// (Runware without includeCost), is an answer at $0 all the same, and refuses the model after.
func TestPassthroughAnswerWithoutUsageRefusesItsBodyModel(t *testing.T) {
	plugin, _ := unpricedBudgetPlugin(t)
	req := &schemas.BifrostRequest{RequestType: schemas.PassthroughRequest, PassthroughRequest: &schemas.BifrostPassthroughRequest{
		Provider: schemas.OpenAI, Method: "POST", Path: "/v1/images", Body: []byte(`[{"model":"token-image"}]`),
	}}
	ctx := resolverCtx(plugin.store, "sk-bf-budget")
	_, shortCircuit, err := plugin.PreLLMHook(ctx, req)
	require.NoError(t, err)
	require.Nil(t, shortCircuit)
	_, _, err = plugin.PostLLMHook(ctx, &schemas.BifrostResponse{PassthroughResponse: &schemas.BifrostPassthroughResponse{
		StatusCode: 200,
		ExtraFields: schemas.BifrostResponseExtraFields{
			RequestType: schemas.PassthroughRequest, Provider: schemas.OpenAI,
			RoutingInfo: schemas.RoutingInfo{Provider: schemas.OpenAI},
		},
	}}, nil)
	require.NoError(t, err)
	plugin.wg.Wait()
	requireUnpricedRefusal(t, preLLM(t, plugin, "sk-bf-budget", req), "the same passthrough after an answer with no usage")
}

// A key alias that serves a priced label as a free model prices the answer as accounting does,
// from the alias's target: its $0 is the price, and refuses nothing.
func TestAnswerFreeUnderTheServingAliasDoesNotRefuseTheModel(t *testing.T) {
	plugin, _ := unpricedBudgetPlugin(t)
	ctx := resolverCtx(plugin.store, "sk-bf-budget")
	_, shortCircuit, err := plugin.PreLLMHook(ctx, chatOn("alias-label"))
	require.NoError(t, err)
	require.Nil(t, shortCircuit)
	ctx.SetValue(schemas.BifrostContextKeySelectedKeyID, "openai-key-1")
	_, _, err = plugin.PostLLMHook(ctx, &schemas.BifrostResponse{ChatResponse: &schemas.BifrostChatResponse{
		Usage: &schemas.BifrostLLMUsage{PromptTokens: 100, CompletionTokens: 10, TotalTokens: 110},
		ExtraFields: schemas.BifrostResponseExtraFields{
			RequestType: schemas.ChatCompletionRequest, Provider: schemas.OpenAI, OriginalModelRequested: "alias-label",
			RoutingInfo: schemas.RoutingInfo{Provider: schemas.OpenAI, Model: "alias-label", ResolvedKeyAlias: &schemas.ResolvedKeyAlias{ModelID: "free-chat"}},
		},
	}}, nil)
	require.NoError(t, err)
	plugin.wg.Wait()
	assert.Nil(t, preLLM(t, plugin, "sk-bf-budget", chatOn("alias-label")), "an answer free under the serving alias does not refuse the label")
}

// A queued acknowledgment (Runware's taskType and taskUUID only) and a stream chunk before the last
// are not answers yet: neither refuses the model, and a later stream's final chunk is judged.
// A passthrough read (polling an operation, fetching a resource) runs no model: it is not refused
// under a dollar budget for naming none, and its $0 answer refuses nothing.
func TestPassthroughReadIsNotPriced(t *testing.T) {
	plugin, _ := unpricedBudgetPlugin(t)
	read := func(model string) *schemas.BifrostRequest {
		return &schemas.BifrostRequest{RequestType: schemas.PassthroughRequest, PassthroughRequest: &schemas.BifrostPassthroughRequest{
			Provider: schemas.OpenAI, Model: model, Method: "GET", Path: "/v1/projects/p/locations/l/operations/987654",
		}}
	}
	assert.Nil(t, preLLM(t, plugin, "sk-bf-budget", read("")), "a model-less GET is served under a dollar budget")
	head := read("")
	head.PassthroughRequest.Method = "head"
	assert.Nil(t, preLLM(t, plugin, "sk-bf-budget", head), "a model-less HEAD is served under a dollar budget")
	post := read("")
	post.PassthroughRequest.Method = "POST"
	requireUnpricedRefusal(t, preLLM(t, plugin, "sk-bf-budget", post), "a model-less POST is still refused")
	withBody := read("")
	withBody.PassthroughRequest.Body = []byte(`{"prompt":"p"}`)
	requireUnpricedRefusal(t, preLLM(t, plugin, "sk-bf-budget", withBody), "a GET carrying a body is priced like any other request")

	// A read naming a model answers at $0 and refuses nothing; the same answer to a POST, which can
	// run the model, refuses it.
	answer := func(method string) {
		t.Helper()
		req := read("gpt-4o")
		req.PassthroughRequest.Method = method
		ctx := resolverCtx(plugin.store, "sk-bf-budget")
		_, shortCircuit, err := plugin.PreLLMHook(ctx, req)
		require.NoError(t, err)
		require.Nil(t, shortCircuit)
		_, _, err = plugin.PostLLMHook(ctx, &schemas.BifrostResponse{PassthroughResponse: &schemas.BifrostPassthroughResponse{
			StatusCode: 200, Body: []byte(`{"name":"operations/987654","done":true}`),
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType: schemas.PassthroughRequest, Provider: schemas.OpenAI, OriginalModelRequested: "gpt-4o",
				RoutingInfo: schemas.RoutingInfo{Provider: schemas.OpenAI, Model: "gpt-4o"},
			},
		}}, nil)
		require.NoError(t, err)
		plugin.wg.Wait()
	}
	chat := &schemas.BifrostRequest{RequestType: schemas.ChatCompletionRequest, ChatRequest: &schemas.BifrostChatRequest{Provider: schemas.OpenAI, Model: "gpt-4o"}}
	answer("GET")
	assert.Nil(t, preLLM(t, plugin, "sk-bf-budget", chat), "a read's $0 answer does not refuse the model it names")
	answer("POST")
	requireUnpricedRefusal(t, preLLM(t, plugin, "sk-bf-budget", chat), "a POST's $0 answer refuses the model it names")
}

func TestQueuedPassthroughAcknowledgmentDoesNotRefuseTheModel(t *testing.T) {
	assert.True(t, passthroughQueuedAcknowledgment([]byte(`{"data":[{"taskType":"imageInference","taskUUID":"a"}]}`)))
	assert.False(t, passthroughQueuedAcknowledgment([]byte(`{"data":[{"taskType":"imageInference","taskUUID":"a","imageURL":"https://example.invalid/1.png"}]}`)))
	assert.False(t, passthroughQueuedAcknowledgment([]byte(`{"data":[]}`)))
	assert.False(t, passthroughQueuedAcknowledgment([]byte(`not json`)))

	plugin, _ := unpricedBudgetPlugin(t)
	answer := func(requestType schemas.RequestType, body string) *schemas.BifrostResponse {
		return &schemas.BifrostResponse{PassthroughResponse: &schemas.BifrostPassthroughResponse{
			StatusCode: 200, Body: []byte(body),
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType: requestType, Provider: schemas.OpenAI,
				RoutingInfo: schemas.RoutingInfo{Provider: schemas.OpenAI},
			},
		}}
	}
	request := func(requestType schemas.RequestType) *schemas.BifrostRequest {
		return &schemas.BifrostRequest{RequestType: requestType, PassthroughRequest: &schemas.BifrostPassthroughRequest{
			Provider: schemas.OpenAI, Method: "POST", Path: "/v1/images", Body: []byte(`[{"model":"token-image"}]`),
		}}
	}

	ctx := resolverCtx(plugin.store, "sk-bf-budget")
	_, shortCircuit, err := plugin.PreLLMHook(ctx, request(schemas.PassthroughRequest))
	require.NoError(t, err)
	require.Nil(t, shortCircuit)
	_, _, err = plugin.PostLLMHook(ctx, answer(schemas.PassthroughRequest, `{"data":[{"taskType":"imageInference","taskUUID":"a"}]}`), nil)
	require.NoError(t, err)
	plugin.wg.Wait()
	assert.Nil(t, preLLM(t, plugin, "sk-bf-budget", request(schemas.PassthroughRequest)), "a queued acknowledgment does not refuse the model")

	// A passthrough the provider refused (a 4xx relayed as the answer) delivered nothing.
	ctx = resolverCtx(plugin.store, "sk-bf-budget")
	_, shortCircuit, err = plugin.PreLLMHook(ctx, request(schemas.PassthroughRequest))
	require.NoError(t, err)
	require.Nil(t, shortCircuit)
	refused := answer(schemas.PassthroughRequest, `{"error":{"message":"invalid request"}}`)
	refused.PassthroughResponse.StatusCode = 400
	_, _, err = plugin.PostLLMHook(ctx, refused, nil)
	require.NoError(t, err)
	plugin.wg.Wait()
	assert.Nil(t, preLLM(t, plugin, "sk-bf-budget", request(schemas.PassthroughRequest)), "a refused passthrough does not refuse the model")

	ctx = resolverCtx(plugin.store, "sk-bf-budget")
	_, shortCircuit, err = plugin.PreLLMHook(ctx, request(schemas.PassthroughStreamRequest))
	require.NoError(t, err)
	require.Nil(t, shortCircuit)
	_, _, err = plugin.PostLLMHook(ctx, answer(schemas.PassthroughStreamRequest, `data: chunk`), nil)
	require.NoError(t, err)
	plugin.wg.Wait()
	assert.Nil(t, preLLM(t, plugin, "sk-bf-budget", request(schemas.PassthroughStreamRequest)), "a chunk before the last does not refuse the model")
	ctx.SetValue(schemas.BifrostContextKeyStreamEndIndicator, true)
	_, _, err = plugin.PostLLMHook(ctx, answer(schemas.PassthroughStreamRequest, `data: last`), nil)
	require.NoError(t, err)
	plugin.wg.Wait()
	requireUnpricedRefusal(t, preLLM(t, plugin, "sk-bf-budget", request(schemas.PassthroughStreamRequest)), "the stream's final chunk, answered with no usage")
}

// A container is priced from its memory tier's row when it asks for one: a deployment that prices
// only 4g containers serves a 4g create under a budget and refuses one with no tier.
func TestContainerCreateIsPricedByItsMemoryTier(t *testing.T) {
	plugin, _ := unpricedBudgetPlugin(t)
	create := func(memory string) *schemas.BifrostRequest {
		return &schemas.BifrostRequest{RequestType: schemas.ContainerCreateRequest, ContainerCreateRequest: &schemas.BifrostContainerCreateRequest{
			Provider: schemas.OpenAI, MemoryLimit: memory,
		}}
	}
	assert.Nil(t, preLLM(t, plugin, "sk-bf-budget", create("4g")), "a 4g container is priced by the 4g row")
	requireUnpricedRefusal(t, preLLM(t, plugin, "sk-bf-budget", create("")), "a container with no tier, with no base container row")
	requireUnpricedRefusal(t, preLLM(t, plugin, "sk-bf-budget", create("64g")), "a tier with no row, and no base row to fall back to")
}

// A passthrough answer is judged in the mode accounting priced it in (inferred from its path), so an
// override that makes the model free for Responses makes a $0 Responses passthrough its price.
func TestPassthroughAnswerIsJudgedInItsPricedMode(t *testing.T) {
	plugin, _ := unpricedBudgetPlugin(t)
	budgets := []string{"budget-unpriced"}
	assert.False(t, plugin.resolver.noteUnpricedAnswer(schemas.OpenAI, "responses-free", "", schemas.PassthroughRequest, schemas.ResponsesRequest, 0, nil, "r1", budgets),
		"free in the mode it was priced in")
	assert.True(t, plugin.resolver.noteUnpricedAnswer(schemas.OpenAI, "responses-free", "", schemas.PassthroughRequest, schemas.ChatCompletionRequest, 0, nil, "r2", budgets),
		"priced in chat, so a $0 chat answer is unpriced")
}

// A live session's units are all live requests to governance: the voice is priced in the live
// (realtime) mode, and a backend model's delegated turns in the Responses mode. A backend model
// priced only for Responses is admitted under a budget; a model priced in neither is refused.
func TestLiveUnitsArePricedInTheirAccountingMode(t *testing.T) {
	plugin, _ := unpricedBudgetPlugin(t)
	live := func(model string) *schemas.BifrostRequest {
		return &schemas.BifrostRequest{RequestType: schemas.LiveRequest, ResponsesRequest: &schemas.BifrostResponsesRequest{Provider: schemas.OpenAI, Model: model}}
	}
	unit := func(kind, model string) *schemas.LLMPluginShortCircuit {
		ctx := resolverCtx(plugin.store, "sk-bf-budget")
		ctx.SetValue(schemas.BifrostContextKeyLiveUnit, kind)
		_, shortCircuit, err := plugin.PreLLMHook(ctx, live(model))
		require.NoError(t, err)
		return shortCircuit
	}
	assert.Nil(t, unit("backend", "gpt-4o"), "a backend unit is priced in the Responses mode (from the model's chat row)")
	requireUnpricedRefusal(t, unit("voice", "gpt-4o"), "a voice unit on a model with no live (realtime) price")
	requireUnpricedRefusal(t, unit("backend", "house-llm"), "a backend unit on a model with no price")

	// A session ending before its backend turn closes the admitted unit with no usage and no status:
	// a record of no work, which refuses nothing for the budget's other callers. A unit that did work
	// and priced at $0 still refuses its model.
	closeUnit := func(model string, usage *schemas.ResponsesResponseUsage) {
		t.Helper()
		ctx := resolverCtx(plugin.store, "sk-bf-budget")
		ctx.SetValue(schemas.BifrostContextKeyLiveUnit, "backend")
		_, shortCircuit, err := plugin.PreLLMHook(ctx, live(model))
		require.NoError(t, err)
		require.Nil(t, shortCircuit)
		_, _, err = plugin.PostLLMHook(ctx, &schemas.BifrostResponse{ResponsesResponse: &schemas.BifrostResponsesResponse{
			Object: "response", Model: model, Usage: usage,
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType: schemas.LiveRequest, Provider: schemas.OpenAI, PricingRequestType: schemas.ResponsesRequest,
				OriginalModelRequested: model, RoutingInfo: schemas.RoutingInfo{Provider: schemas.OpenAI, Model: model},
			},
		}}, nil)
		require.NoError(t, err)
		plugin.wg.Wait()
	}
	closeUnit("seconds-chat", &schemas.ResponsesResponseUsage{})
	assert.Nil(t, unit("backend", "seconds-chat"), "an empty backend unit does not refuse the model")
	closeUnit("seconds-chat", &schemas.ResponsesResponseUsage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15})
	requireUnpricedRefusal(t, unit("backend", "seconds-chat"), "a backend unit with tokens the model's price does not cover refuses it")
	assert.False(t, emptyResponsesUsage(&schemas.ResponsesResponseUsage{AudioSeconds: new(3.0)}), "a voice window with seconds is not empty")
}

// A provider's server-side fallback answers on another model, which accounting prices: an answer
// free on the model that served it refuses nothing, though the requested model is priced. One the
// serving model's price does not cover refuses the requested model, which the next request reaches
// the fallback through. (A fallback to a model with no row at all is priced as the requested model.)
func TestServerSideFallbackAnswerIsJudgedAsTheServingModel(t *testing.T) {
	plugin, _ := unpricedBudgetPlugin(t)
	chat := &schemas.BifrostRequest{RequestType: schemas.ChatCompletionRequest, ChatRequest: &schemas.BifrostChatRequest{Provider: schemas.OpenAI, Model: "gpt-4o"}}
	answerOn := func(fallback string) {
		t.Helper()
		ctx := resolverCtx(plugin.store, "sk-bf-budget")
		_, shortCircuit, err := plugin.PreLLMHook(ctx, chat)
		require.NoError(t, err)
		require.Nil(t, shortCircuit)
		model := chat.ChatRequest.Model
		answer := &schemas.BifrostResponse{ChatResponse: &schemas.BifrostChatResponse{
			Model: model,
			Usage: &schemas.BifrostLLMUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType: schemas.ChatCompletionRequest, Provider: schemas.OpenAI, OriginalModelRequested: model,
				RoutingInfo: schemas.RoutingInfo{Provider: schemas.OpenAI, Model: model, ServerSideFallbackModel: &fallback},
			},
		}}
		breakdown := plugin.modelCatalog.CalculateCostBreakdown(answer, nil)
		require.NotNil(t, breakdown)
		require.Zero(t, breakdown.TotalCost, "accounting prices the answer on %s at $0", fallback)
		_, _, err = plugin.PostLLMHook(ctx, answer, nil)
		require.NoError(t, err)
		plugin.wg.Wait()
	}
	answerOn("free-chat")
	assert.Nil(t, preLLM(t, plugin, "sk-bf-budget", chat), "a free fallback answer does not refuse the requested model")
	answerOn("seconds-chat")
	requireUnpricedRefusal(t, preLLM(t, plugin, "sk-bf-budget", chat), "an unpriced fallback answer refuses the requested model")

	// A requested model the price book makes free does not make its unpriced fallback free.
	chat = &schemas.BifrostRequest{RequestType: schemas.ChatCompletionRequest, ChatRequest: &schemas.BifrostChatRequest{Provider: schemas.OpenAI, Model: "free-chat"}}
	answerOn("seconds-chat")
	requireUnpricedRefusal(t, preLLM(t, plugin, "sk-bf-budget", chat), "an unpriced fallback answer refuses a free requested model")
}
