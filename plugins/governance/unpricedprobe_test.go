package governance

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/capsohq/bifrost/framework/grant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func probeImageRequest(model string) *schemas.BifrostRequest {
	return &schemas.BifrostRequest{
		RequestType:            schemas.ImageGenerationRequest,
		ImageGenerationRequest: &schemas.BifrostImageGenerationRequest{Provider: schemas.OpenAI, Model: model},
	}
}

// probeImageAnswer is an image answer with no token usage: token-image prices only image tokens,
// so it prices at $0.
func probeImageAnswer(model string) *schemas.BifrostResponse {
	return &schemas.BifrostResponse{ImageGenerationResponse: &schemas.BifrostImageGenerationResponse{
		Data: []schemas.ImageData{{URL: "https://example.invalid/1.png"}},
		ExtraFields: schemas.BifrostResponseExtraFields{
			RequestType: schemas.ImageGenerationRequest, Provider: schemas.OpenAI, OriginalModelRequested: model,
			RoutingInfo: schemas.RoutingInfo{Provider: schemas.OpenAI, Model: model},
		},
	}}
}

func requireProbeRefusal(t *testing.T, shortCircuit *schemas.LLMPluginShortCircuit, msg string) {
	t.Helper()
	require.NotNil(t, shortCircuit, msg)
	require.NotNil(t, shortCircuit.Error, msg)
	require.NotNil(t, shortCircuit.Error.StatusCode, msg)
	assert.Equal(t, 429, *shortCircuit.Error.StatusCode, msg)
	require.NotNil(t, shortCircuit.Error.Type, msg)
	assert.Equal(t, string(DecisionModelPricingUnconfirmed), *shortCircuit.Error.Type, msg)
	assert.Equal(t, schemas.ErrorTypePolicyRateLimited, shortCircuit.Error.ExtraFields.ErrorType, msg)
	assert.Equal(t, int64(unpricedProbeRetryAfterMs), shortCircuit.Error.ExtraFields.RetryAfter, msg)
}

// TXE-7977: N concurrent requests of one budgeted key on a row whose answers price at $0 yield at
// most one $0 answer. The first is admitted and holds the row; the others are refused 429 while it
// is in flight; its $0 answer then refuses the row (TXE-7957), so none of them gets another.
func TestConcurrentRequestsTakeAtMostOneUnpricedAnswer(t *testing.T) {
	plugin, _ := unpricedBudgetPlugin(t)
	const n = 16
	contexts := make([]*schemas.BifrostContext, n)
	shortCircuits := make([]*schemas.LLMPluginShortCircuit, n)
	var wg sync.WaitGroup
	for i := range n {
		contexts[i] = resolverCtx(plugin.store, "sk-bf-budget")
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, shortCircuit, err := plugin.PreLLMHook(contexts[i], probeImageRequest("token-image"))
			assert.NoError(t, err)
			shortCircuits[i] = shortCircuit
		}(i)
	}
	wg.Wait()
	admitted := -1
	for i, shortCircuit := range shortCircuits {
		if shortCircuit == nil {
			require.Equal(t, -1, admitted, "a second concurrent request was admitted on a row no answer has priced")
			admitted = i
			continue
		}
		requireProbeRefusal(t, shortCircuit, "a concurrent request on an unconfirmed row")
	}
	require.NotEqual(t, -1, admitted, "one request is admitted")

	_, _, err := plugin.PostLLMHook(contexts[admitted], probeImageAnswer("token-image"), nil)
	require.NoError(t, err)
	plugin.wg.Wait()
	requireUnpricedRefusal(t, preLLM(t, plugin, "sk-bf-budget", probeImageRequest("token-image")), "after the one $0 answer the row is refused")
}

// An answer that priced above $0 confirms the row for the key: concurrent requests on it are then
// admitted.
func TestPricedAnswerLiftsTheProbe(t *testing.T) {
	plugin, _ := unpricedBudgetPlugin(t)
	chat := &schemas.BifrostRequest{RequestType: schemas.ChatCompletionRequest, ChatRequest: &schemas.BifrostChatRequest{Provider: schemas.OpenAI, Model: "gpt-4o"}}
	first := resolverCtx(plugin.store, "sk-bf-budget")
	_, shortCircuit, err := plugin.PreLLMHook(first, chat)
	require.NoError(t, err)
	require.Nil(t, shortCircuit)
	requireProbeRefusal(t, preLLM(t, plugin, "sk-bf-budget", chat), "concurrent with the first, before it is priced")

	_, _, err = plugin.PostLLMHook(first, &schemas.BifrostResponse{ChatResponse: &schemas.BifrostChatResponse{
		Model: "gpt-4o",
		Usage: &schemas.BifrostLLMUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
		ExtraFields: schemas.BifrostResponseExtraFields{
			RequestType: schemas.ChatCompletionRequest, Provider: schemas.OpenAI, OriginalModelRequested: "gpt-4o",
			RoutingInfo: schemas.RoutingInfo{Provider: schemas.OpenAI, Model: "gpt-4o"},
		},
	}}, nil)
	require.NoError(t, err)
	plugin.wg.Wait()

	held := resolverCtx(plugin.store, "sk-bf-budget")
	_, shortCircuit, err = plugin.PreLLMHook(held, chat)
	require.NoError(t, err)
	require.Nil(t, shortCircuit)
	assert.Nil(t, preLLM(t, plugin, "sk-bf-budget", chat), "a confirmed row admits concurrent requests")
}

// A priced answer a provider's server-side fallback served confirms nothing: it was priced as
// another model.
func TestFallbackAnswerDoesNotConfirmTheProbedRow(t *testing.T) {
	plugin, _ := unpricedBudgetPlugin(t)
	chat := &schemas.BifrostRequest{RequestType: schemas.ChatCompletionRequest, ChatRequest: &schemas.BifrostChatRequest{Provider: schemas.OpenAI, Model: "seconds-chat"}}
	first := resolverCtx(plugin.store, "sk-bf-budget")
	_, shortCircuit, err := plugin.PreLLMHook(first, chat)
	require.NoError(t, err)
	require.Nil(t, shortCircuit)
	fallback := "gpt-4o"
	_, _, err = plugin.PostLLMHook(first, &schemas.BifrostResponse{ChatResponse: &schemas.BifrostChatResponse{
		Model: "seconds-chat",
		Usage: &schemas.BifrostLLMUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
		ExtraFields: schemas.BifrostResponseExtraFields{
			RequestType: schemas.ChatCompletionRequest, Provider: schemas.OpenAI, OriginalModelRequested: "seconds-chat",
			RoutingInfo: schemas.RoutingInfo{Provider: schemas.OpenAI, Model: "seconds-chat", ServerSideFallbackModel: &fallback},
		},
	}}, nil)
	require.NoError(t, err)
	plugin.wg.Wait()
	held := resolverCtx(plugin.store, "sk-bf-budget")
	_, shortCircuit, err = plugin.PreLLMHook(held, chat)
	require.NoError(t, err)
	require.Nil(t, shortCircuit, "the fallback's answer released the row")
	requireProbeRefusal(t, preLLM(t, plugin, "sk-bf-budget", chat), "but did not confirm it: a concurrent request is still held off")
}

// A request that fails, or a key with no dollar budget, holds nothing afterwards.
func TestProbeIsReleasedOnErrorAndSkippedWithoutABudget(t *testing.T) {
	plugin, _ := unpricedBudgetPlugin(t)
	first := resolverCtx(plugin.store, "sk-bf-budget")
	_, shortCircuit, err := plugin.PreLLMHook(first, probeImageRequest("token-image"))
	require.NoError(t, err)
	require.Nil(t, shortCircuit)
	_, _, err = plugin.PostLLMHook(first, nil, &schemas.BifrostError{
		StatusCode:  schemas.Ptr(500),
		Error:       &schemas.ErrorField{Message: "upstream failed"},
		ExtraFields: schemas.BifrostErrorExtraFields{RequestType: schemas.ImageGenerationRequest, Provider: schemas.OpenAI, OriginalModelRequested: "token-image"},
	})
	require.NoError(t, err)
	plugin.wg.Wait()
	assert.Nil(t, preLLM(t, plugin, "sk-bf-budget", probeImageRequest("token-image")), "a failed request released the row")

	open := resolverCtx(plugin.store, "sk-bf-open")
	_, shortCircuit, err = plugin.PreLLMHook(open, probeImageRequest("token-image"))
	require.NoError(t, err)
	require.Nil(t, shortCircuit)
	assert.Nil(t, preLLM(t, plugin, "sk-bf-open", probeImageRequest("token-image")), "a key with no dollar budget is never probed")
}

func TestUnpricedProbes(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	probes := newUnpricedProbes()
	probes.now = func() time.Time { return now }
	scopes := unpricedScopes([]string{"b"}, "vk-1")

	held, busy := probes.acquire(scopes, []string{"row"})
	require.False(t, busy)
	require.NotNil(t, held)
	_, busy = probes.acquire(scopes, []string{"row"})
	assert.True(t, busy, "a held row")
	other, busy := probes.acquire(unpricedScopes([]string{"b"}, "vk-2"), []string{"row"})
	assert.False(t, busy, "another key on the same budget holds its own probe")
	other.release()

	now = now.Add(unpricedProbeStaleAfter)
	stale, busy := probes.acquire(scopes, []string{"row"})
	assert.False(t, busy, "a probe whose request never returned is freed after unpricedProbeStaleAfter")
	held.release()
	_, busy = probes.acquire(scopes, []string{"row"})
	assert.True(t, busy, "the expired probe's late release leaves its replacement's hold in place")
	stale.confirm()
	stale.release()
	stale.release()
	confirmed, busy := probes.acquire(scopes, []string{"row"})
	assert.False(t, busy)
	assert.Nil(t, confirmed, "a confirmed row is not held")

	assert.False(t, probesUnpriced(schemas.LiveRequest), "a live unit holds its row for a whole session")
	assert.False(t, probesUnpriced(schemas.VideoGenerationRequest), "a settled job is not an answer")
	assert.True(t, probesUnpriced(schemas.ChatCompletionRequest))
}

// A request is held under the most specific principal it presented: its virtual key, else its
// user, else a digest of its credential. Requests that presented nothing share one hold, so they
// are serialized rather than left unprobed.
func TestProbeHolderIsTheMostSpecificPrincipal(t *testing.T) {
	plugin, _ := unpricedBudgetPlugin(t)
	withIdentity := func(identity *grant.Identity) *schemas.BifrostContext {
		ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
		g := grant.New()
		if identity != nil {
			g.SetIdentity(identity)
		}
		ctx.SetGrant(g)
		return ctx
	}
	session := grant.NewCredential(grant.CredentialSessionToken, "session-secret")
	alice := withIdentity(grant.NewIdentity(session, &schemas.UserRef{ID: "alice"}, nil, nil, nil, nil, nil))
	bob := withIdentity(grant.NewIdentity(session, &schemas.UserRef{ID: "bob"}, nil, nil, nil, nil, nil))
	assert.Equal(t, "user:alice", unpricedHolder(alice))
	tokenOnly := withIdentity(grant.NewIdentity(grant.NewCredential(grant.CredentialAPIKey, "api-secret"), nil, nil, nil, nil, nil, nil))
	assert.NotContains(t, unpricedHolder(tokenOnly), "api-secret", "a credential is held by its digest")
	assert.Contains(t, unpricedHolder(tokenOnly), "credential:api_key:")

	row, budgets := "openai|chat|gpt-4o", []string{"budget-unpriced"}
	require.Nil(t, plugin.resolver.probeUnconfirmed(alice, schemas.ChatCompletionRequest, row, budgets))
	assert.Nil(t, plugin.resolver.probeUnconfirmed(bob, schemas.ChatCompletionRequest, row, budgets), "another user holds their own probe")

	anonymous := withIdentity(nil)
	require.Nil(t, plugin.resolver.probeUnconfirmed(anonymous, schemas.ChatCompletionRequest, row, budgets))
	refused := plugin.resolver.probeUnconfirmed(withIdentity(nil), schemas.ChatCompletionRequest, row, budgets)
	require.NotNil(t, refused, "requests that presented nothing share one hold")
	assert.Equal(t, DecisionModelPricingUnconfirmed, refused.Decision)
}

// A stream holds its row until its final chunk.
func TestTakeUnpricedProbeWaitsForTheFinalChunk(t *testing.T) {
	probes := newUnpricedProbes()
	probe, _ := probes.acquire([]string{"s"}, []string{"row"})
	ctx := resolverCtx(nil, "")
	ctx.SetValue(unpricedProbeContextKey, probe)
	assert.Nil(t, takeUnpricedProbe(ctx, nil, true, false), "a mid-stream chunk keeps the hold")
	assert.Same(t, probe, takeUnpricedProbe(ctx, nil, true, true), "the final chunk takes it")
	assert.Nil(t, takeUnpricedProbe(ctx, nil, true, true), "taken once")
}
