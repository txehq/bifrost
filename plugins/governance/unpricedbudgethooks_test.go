package governance

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/capsohq/bifrost/framework/configstore"
	configstoreTables "github.com/capsohq/bifrost/framework/configstore/tables"
	"github.com/capsohq/bifrost/framework/jobaccounting"
	"github.com/capsohq/bifrost/framework/modelcatalog"
	"github.com/capsohq/bifrost/framework/modelcatalog/datasheet"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests use only hooks that exist before TXE-7957, so they run red on a checkout without it.

// unpricedBudgetPlugin is a governance plugin over a price book with a priced chat model (gpt-4o),
// a free one (free-chat), an image model priced only per image token (token-image), a video model
// priced per second (clip-1), and none for house-llm. vk-budget has a $100 budget; vk-open has none.
func unpricedBudgetPlugin(t *testing.T) (*GovernancePlugin, *LocalGovernanceStore) {
	t.Helper()
	sheet := filepath.Join(t.TempDir(), "pricing.json")
	require.NoError(t, os.WriteFile(sheet, []byte(`{
		"gpt-4o":{"provider":"openai","mode":"chat","input_cost_per_token":0.0000025,"output_cost_per_token":0.00001},
		"free-chat":{"provider":"openai","mode":"chat","input_cost_per_token":0,"output_cost_per_token":0},
		"token-image":{"provider":"openai","mode":"image_generation","output_cost_per_image_token":0.00004},
		"clip-1":{"provider":"openai","mode":"video_generation","output_cost_per_second":0.1}
	}`), 0o600))
	ds := datasheet.New(nil, NewMockLogger(), datasheet.Config{URL: "file://" + sheet})
	require.NoError(t, ds.LoadFromURLIntoMemory(context.Background()))
	catalog := modelcatalog.NewTestCatalogWithDatasheet(ds)

	logger := NewMockLogger()
	budget := buildBudgetWithUsage("budget-unpriced", 100.0, 0, "1d")
	budgeted := buildVirtualKeyWithBudget("vk-budget", "sk-bf-budget", "Budget VK", budget)
	open := buildVirtualKey("vk-open", "sk-bf-open", "Open VK", true)
	open.ProviderConfigs = []configstoreTables.TableVirtualKeyProviderConfig{buildProviderConfig("openai", []string{"*"})}
	store, err := NewLocalGovernanceStore(context.Background(), logger, nil, &configstore.GovernanceConfig{
		VirtualKeys: []configstoreTables.TableVirtualKey{*budgeted, *open},
		Budgets:     []configstoreTables.TableBudget{*budget},
	}, nil, nil)
	require.NoError(t, err)
	plugin, err := InitFromStore(context.Background(), &Config{IsVkMandatory: boolPtr(false)}, logger, store, nil, catalog, nil, nil)
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

// A virtual key with a dollar budget cannot obtain answers from a model the price book cannot
// price: each request is refused before the upstream call, however often it is repeated. Priced
// and free models, a key with no budget, and an operation that is never charged are unaffected.
func TestUnpricedModelUnderADollarBudgetIsRefusedBeforeTheCall(t *testing.T) {
	plugin, _ := unpricedBudgetPlugin(t)
	pre := func(vk string, req *schemas.BifrostRequest) *schemas.LLMPluginShortCircuit {
		t.Helper()
		_, shortCircuit, err := plugin.PreLLMHook(resolverCtx(plugin.store, vk), req)
		require.NoError(t, err)
		return shortCircuit
	}

	for i := 0; i < 3; i++ {
		requireUnpricedRefusal(t, pre("sk-bf-budget", chatOn("house-llm")), "chat on a model with no row")
	}
	requireUnpricedRefusal(t, pre("sk-bf-budget", &schemas.BifrostRequest{
		RequestType: schemas.ChatCompletionStreamRequest, ChatRequest: &schemas.BifrostChatRequest{Provider: schemas.OpenAI, Model: "house-llm"},
	}), "a streamed chat on a model with no row")
	requireUnpricedRefusal(t, pre("sk-bf-budget", &schemas.BifrostRequest{
		RequestType: schemas.EmbeddingRequest, EmbeddingRequest: &schemas.BifrostEmbeddingRequest{Provider: schemas.OpenAI, Model: "gpt-4o"},
	}), "an embedding on a model priced only for chat")

	assert.Nil(t, pre("sk-bf-budget", chatOn("gpt-4o")), "a priced model is served under a budget")
	assert.Nil(t, pre("sk-bf-budget", chatOn("free-chat")), "a model the price book prices at $0 is free, not unpriced")
	assert.Nil(t, pre("sk-bf-open", chatOn("house-llm")), "a key with no dollar budget has nothing to charge, so it is not refused")
	assert.Nil(t, pre("sk-bf-budget", &schemas.BifrostRequest{
		RequestType: schemas.FileListRequest, FileListRequest: &schemas.BifrostFileListRequest{Provider: schemas.OpenAI},
	}), "an operation that is never charged is not priced")
}

// An answer the gate admitted that still priced at $0 under a dollar budget fails closed: the
// next request on that model under a budget is refused before the call. Image tokens are the
// row's only price here, and the answer reports none.
func TestUnpricedAnswerUnderADollarBudgetRefusesTheModelAfter(t *testing.T) {
	plugin, store := unpricedBudgetPlugin(t)
	image := func() *schemas.BifrostRequest {
		return &schemas.BifrostRequest{
			RequestType:            schemas.ImageGenerationRequest,
			ImageGenerationRequest: &schemas.BifrostImageGenerationRequest{Provider: schemas.OpenAI, Model: "token-image"},
		}
	}
	ctx := resolverCtx(plugin.store, "sk-bf-budget")
	ctx.SetValue(schemas.BifrostContextKeyRequestID, "req-image-1")
	_, shortCircuit, err := plugin.PreLLMHook(ctx, image())
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
		_, shortCircuit, err := plugin.PreLLMHook(resolverCtx(plugin.store, "sk-bf-budget"), image())
		assert.NoError(c, err)
		if assert.NotNil(c, shortCircuit, "the model is refused under a budget once an answer priced at $0") && assert.NotNil(c, shortCircuit.Error) {
			assert.Equal(c, schemas.ErrorTypePolicyModelBlocked, shortCircuit.Error.ExtraFields.ErrorType)
		}
	}, time.Second, 10*time.Millisecond)

	_, shortCircuit, err = plugin.PreLLMHook(resolverCtx(plugin.store, "sk-bf-open"), image())
	require.NoError(t, err)
	assert.Nil(t, shortCircuit, "a key with no dollar budget is not refused")
}

// A background Responses create that is still queued, and an answer served from the semantic
// cache, carry no vendor usage: neither is an unpriced answer, and neither refuses the model.
func TestQueuedOrCachedAnswerDoesNotRefuseTheModel(t *testing.T) {
	plugin, _ := unpricedBudgetPlugin(t)
	queued := "queued"
	answers := map[string]*schemas.BifrostResponse{
		"queued": {ResponsesResponse: &schemas.BifrostResponsesResponse{Status: &queued, ExtraFields: schemas.BifrostResponseExtraFields{
			RequestType: schemas.ResponsesRequest, Provider: schemas.OpenAI, OriginalModelRequested: "gpt-4o",
			RoutingInfo: schemas.RoutingInfo{Provider: schemas.OpenAI, Model: "gpt-4o"},
		}}},
		"cached": {ChatResponse: &schemas.BifrostChatResponse{ExtraFields: schemas.BifrostResponseExtraFields{
			RequestType: schemas.ChatCompletionRequest, Provider: schemas.OpenAI, OriginalModelRequested: "gpt-4o",
			RoutingInfo: schemas.RoutingInfo{Provider: schemas.OpenAI, Model: "gpt-4o"},
			CacheDebug:  &schemas.BifrostCacheMetadata{CacheHit: true},
		}}},
	}
	requests := map[string]*schemas.BifrostRequest{
		"queued": {RequestType: schemas.ResponsesRequest, ResponsesRequest: &schemas.BifrostResponsesRequest{Provider: schemas.OpenAI, Model: "gpt-4o"}},
		"cached": chatOn("gpt-4o"),
	}
	for name, answer := range answers {
		ctx := resolverCtx(plugin.store, "sk-bf-budget")
		_, shortCircuit, err := plugin.PreLLMHook(ctx, requests[name])
		require.NoError(t, err)
		require.Nil(t, shortCircuit, name)
		_, _, err = plugin.PostLLMHook(ctx, answer, nil)
		require.NoError(t, err)
		plugin.wg.Wait()
		_, shortCircuit, err = plugin.PreLLMHook(resolverCtx(plugin.store, "sk-bf-budget"), requests[name])
		require.NoError(t, err)
		assert.Nil(t, shortCircuit, "%s: the model is still served", name)
	}
}

// A job that settles at $0 under a dollar budget (a video, a batch) fails closed the same way: the
// next job on that model under a budget is refused before the call.
func TestUnpricedSettledJobUnderADollarBudgetRefusesTheModelAfter(t *testing.T) {
	plugin, _ := unpricedBudgetPlugin(t)
	video := func() *schemas.BifrostRequest {
		return &schemas.BifrostRequest{
			RequestType:            schemas.VideoGenerationRequest,
			VideoGenerationRequest: &schemas.BifrostVideoGenerationRequest{Provider: schemas.OpenAI, Model: "clip-1"},
		}
	}
	_, shortCircuit, err := plugin.PreLLMHook(resolverCtx(plugin.store, "sk-bf-budget"), video())
	require.NoError(t, err)
	require.Nil(t, shortCircuit, "a priced video model is admitted")

	require.NoError(t, plugin.ReportUsage(context.Background(), jobaccounting.UsageReport{
		RequestID: "video-job-1", Provider: schemas.OpenAI, Model: "clip-1", Cost: 0, BudgetIDs: []string{"budget-unpriced"},
	}))
	_, shortCircuit, err = plugin.PreLLMHook(resolverCtx(plugin.store, "sk-bf-budget"), video())
	require.NoError(t, err)
	require.NotNil(t, shortCircuit, "a model whose job settled at $0 under a budget is refused under a budget")
	require.NotNil(t, shortCircuit.Error)
	assert.Equal(t, schemas.ErrorTypePolicyModelBlocked, shortCircuit.Error.ExtraFields.ErrorType)
}
