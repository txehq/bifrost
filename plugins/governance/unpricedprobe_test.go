package governance

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/capsohq/bifrost/framework/configstore"
	"github.com/capsohq/bifrost/framework/grant"
	"github.com/capsohq/bifrost/framework/modelcatalog"
	"github.com/capsohq/bifrost/plugins/governance/internal/testtime"
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

// A request that passes the refusal check and then takes the hold just after another request's $0
// answer was recorded and its hold released is refused on the second read of the record, and
// leaves no hold behind.
func TestRefusalRecordedBeforeTheHoldIsTakenIsReadAgain(t *testing.T) {
	plugin, _ := unpricedBudgetPlugin(t)
	plugin.resolver.probes.afterAcquire = func() {
		plugin.resolver.probes.afterAcquire = nil
		require.True(t, plugin.resolver.noteUnpricedAnswer(schemas.OpenAI, "token-image", "", schemas.ImageGenerationRequest, schemas.ImageGenerationRequest, 0, nil, "settled-meanwhile", []string{"budget-unpriced"}, "vk:vk-budget"))
	}
	ctx := resolverCtx(plugin.store, "sk-bf-budget")
	_, shortCircuit, err := plugin.PreLLMHook(ctx, probeImageRequest("token-image"))
	require.NoError(t, err)
	requireUnpricedRefusal(t, shortCircuit, "the refusal recorded while the hold was being taken")
	assert.Nil(t, ctx.Value(unpricedProbeContextKey), "the refused request holds nothing")
	_, busy := plugin.resolver.probes.acquire(unpricedScopes([]string{"budget-unpriced"}, "vk:vk-budget"), []string{"anything"})
	assert.False(t, busy)
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
	stale.confirm("row")
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

	budgets := []string{"budget-unpriced"}
	probe := func(ctx *schemas.BifrostContext) *EvaluationResult {
		return plugin.resolver.probeUnconfirmed(ctx, schemas.ChatCompletionRequest, nil, schemas.OpenAI, "gpt-4o", schemas.ChatCompletionRequest, nil, budgets)
	}
	require.Nil(t, probe(alice))
	assert.Nil(t, probe(bob), "another user holds their own probe")

	anonymous := withIdentity(nil)
	require.Nil(t, probe(anonymous))
	refused := probe(withIdentity(nil))
	require.NotNil(t, refused, "requests that presented nothing share one hold")
	assert.Equal(t, DecisionModelPricingUnconfirmed, refused.Decision)
}

// A request core ends without running its post-hooks (a full queue, a cancel while queued) frees
// its hold when its context ends.
func TestProbeIsReleasedWhenTheRequestContextEnds(t *testing.T) {
	plugin, _ := unpricedBudgetPlugin(t)
	parent, cancel := context.WithCancel(context.Background())
	ctx := schemas.NewBifrostContext(parent, schemas.NoDeadline)
	g := grant.New()
	g.SetIdentity(grant.NewIdentity(grant.NewCredential(grant.CredentialVirtualKey, "v"), nil, &schemas.EntityRef{ID: "vk-ctx"}, nil, nil, nil, nil))
	ctx.SetGrant(g)
	budgets := []string{"budget-unpriced"}
	require.Nil(t, plugin.resolver.probeUnconfirmed(ctx, schemas.ChatCompletionRequest, nil, schemas.OpenAI, "gpt-4o", schemas.ChatCompletionRequest, nil, budgets))
	scopes := unpricedScopes(budgets, "vk:vk-ctx")
	_, rows := plugin.resolver.priceIdentities(schemas.OpenAI, "gpt-4o", schemas.ChatCompletionRequest, nil)
	_, busy := plugin.resolver.probes.acquire(scopes, rows)
	require.True(t, busy, "held while the request runs")
	cancel()
	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		held, busy := plugin.resolver.probes.acquire(scopes, rows)
		assert.False(c, busy, "released when the request's context ended")
		held.release()
	}, testtime.Scale(time.Second), testtime.Scale(10*time.Millisecond))
}

// Once the post-hook has taken the hold for settlement, the request's context ending (its HTTP
// handler returning first) does not free it: it lasts until the usage worker records the answer.
func TestTakenProbeOutlivesTheRequestContext(t *testing.T) {
	plugin, _ := unpricedBudgetPlugin(t)
	parent, cancel := context.WithCancel(context.Background())
	ctx := schemas.NewBifrostContext(parent, schemas.NoDeadline)
	g := grant.New()
	g.SetIdentity(grant.NewIdentity(grant.NewCredential(grant.CredentialVirtualKey, "v"), nil, &schemas.EntityRef{ID: "vk-taken"}, nil, nil, nil, nil))
	ctx.SetGrant(g)
	budgets := []string{"budget-unpriced"}
	require.Nil(t, plugin.resolver.probeUnconfirmed(ctx, schemas.ChatCompletionRequest, nil, schemas.OpenAI, "gpt-4o", schemas.ChatCompletionRequest, nil, budgets))
	taken := takeUnpricedProbe(ctx, nil, false, true)
	require.NotNil(t, taken)
	cancel()
	time.Sleep(testtime.Scale(50 * time.Millisecond))
	scopes := unpricedScopes(budgets, "vk:vk-taken")
	_, rows := plugin.resolver.priceIdentities(schemas.OpenAI, "gpt-4o", schemas.ChatCompletionRequest, nil)
	_, busy := plugin.resolver.probes.acquire(scopes, rows)
	assert.True(t, busy, "still held after the request's context ended")
	taken.release()
	_, busy = plugin.resolver.probes.acquire(scopes, rows)
	assert.False(t, busy, "released by settlement")
}

// Confirming the row one key priced an answer on leaves the request's other candidate rows (another
// key's override) held one request at a time.
func TestConfirmingOneRowLeavesTheOthersProbed(t *testing.T) {
	probes := newUnpricedProbes()
	scopes := []string{"s"}
	first, busy := probes.acquire(scopes, []string{"key-a-row", "key-b-row"})
	require.False(t, busy)
	first.confirm("key-a-row")
	first.confirm("not-held")
	first.release()
	second, busy := probes.acquire(scopes, []string{"key-a-row", "key-b-row"})
	require.False(t, busy)
	require.NotNil(t, second)
	assert.Equal(t, []string{"s\x00key-b-row"}, second.keys, "only the unconfirmed row is held")
	_, busy = probes.acquire(scopes, []string{"key-a-row", "key-b-row"})
	assert.True(t, busy, "the other key's row is still probed")
	_, busy = probes.acquire(scopes, []string{"not-held"})
	assert.False(t, busy)
}

// An answer served under a key alias is confirmed on the row the alias resolves to, the row
// admission held, not on the label's own row.
func TestProbeConfirmsTheServingKeysAliasTarget(t *testing.T) {
	plugin, _ := unpricedBudgetPlugin(t)
	scopes := []string{"s"}
	target, _ := plugin.resolver.priceIdentity(schemas.OpenAI, "free-chat", schemas.ChatCompletionRequest, nil)
	require.NotEmpty(t, target)
	probe, busy := plugin.resolver.probes.acquire(scopes, []string{target})
	require.False(t, busy)
	probe.provider, probe.model, probe.mode = schemas.OpenAI, "alias-label", schemas.ChatCompletionRequest
	plugin.resolver.confirmProbe(probe, schemas.ChatCompletionRequest, &modelcatalog.PricingLookupScopes{SelectedKeyID: "openai-key-1"})
	probe.release()
	again, busy := plugin.resolver.probes.acquire(scopes, []string{target})
	assert.False(t, busy)
	assert.Nil(t, again, "the alias target's row is confirmed")
}

// An answer priced in another mode than the row the request held (a passthrough the gate held on
// the model's chat row, priced as embeddings by its path) confirms nothing.
func TestProbeConfirmsOnlyTheRowTheAnswerWasPricedOn(t *testing.T) {
	plugin, _ := unpricedBudgetPlugin(t)
	scopes := []string{"s"}
	chatRow, _ := plugin.resolver.priceIdentity(schemas.OpenAI, "dual-1", schemas.ChatCompletionRequest, nil)
	require.NotEmpty(t, chatRow)
	probe, busy := plugin.resolver.probes.acquire(scopes, []string{chatRow})
	require.False(t, busy)
	probe.provider, probe.model, probe.mode = schemas.OpenAI, "dual-1", schemas.PassthroughRequest
	plugin.resolver.confirmProbe(probe, schemas.EmbeddingRequest, nil)
	probe.release()
	again, busy := plugin.resolver.probes.acquire(scopes, []string{chatRow})
	assert.False(t, busy)
	assert.NotNil(t, again, "the chat row is still unconfirmed")
	again.release()

	again.provider, again.model, again.mode = schemas.OpenAI, "dual-1", schemas.PassthroughRequest
	plugin.resolver.confirmProbe(again, schemas.ChatCompletionRequest, nil)
	confirmed, _ := plugin.resolver.probes.acquire(scopes, []string{chatRow})
	assert.Nil(t, confirmed, "an answer priced on the chat row confirms it")
}

// A request that turns off its usage (stream_options.include_usage false) is held even on a row an
// earlier answer confirmed: a confirmed row says nothing about an answer that reports no usage.
func TestRequestSuppressingUsageIsProbedOnAConfirmedRow(t *testing.T) {
	off, on := false, true
	chat := func(include *bool) *schemas.BifrostRequest {
		req := &schemas.BifrostRequest{RequestType: schemas.ChatCompletionStreamRequest, ChatRequest: &schemas.BifrostChatRequest{Provider: schemas.OpenAI, Model: "gpt-4o"}}
		if include != nil {
			req.ChatRequest.Params = &schemas.ChatParameters{StreamOptions: &schemas.ChatStreamOptions{IncludeUsage: include}}
		}
		return req
	}
	assert.True(t, suppressesUsage(nil, chat(&off)))
	assert.False(t, suppressesUsage(nil, chat(&on)))
	assert.False(t, suppressesUsage(nil, chat(nil)))
	assert.True(t, suppressesUsage(nil, &schemas.BifrostRequest{PassthroughRequest: &schemas.BifrostPassthroughRequest{Body: []byte(`{"model":"m","stream_options":{"include_usage":false}}`)}}))
	assert.False(t, suppressesUsage(nil, &schemas.BifrostRequest{PassthroughRequest: &schemas.BifrostPassthroughRequest{Body: []byte(`{"model":"m","stream_options":{"include_usage":true}}`)}}))
	assert.True(t, suppressesUsage(nil, &schemas.BifrostRequest{ChatRequest: &schemas.BifrostChatRequest{RawRequestBody: []byte(`{"stream_options":{"include_usage":false}}`)}}))
	for _, value := range []string{`0`, `"false"`, `null`} {
		assert.True(t, suppressesUsage(nil, &schemas.BifrostRequest{PassthroughRequest: &schemas.BifrostPassthroughRequest{Body: []byte(`{"stream_options":{"include_usage":` + value + `}}`)}}), value)
	}
	assert.False(t, suppressesUsage(nil, &schemas.BifrostRequest{PassthroughRequest: &schemas.BifrostPassthroughRequest{Body: []byte(`{"stream_options":{}}`)}}))
	stream := func(body string) *schemas.BifrostRequest {
		return &schemas.BifrostRequest{RequestType: schemas.PassthroughStreamRequest, PassthroughRequest: &schemas.BifrostPassthroughRequest{Body: []byte(body)}}
	}
	assert.True(t, suppressesUsage(nil, stream(`{"model":"m","stream":true}`)), "a forwarded stream that does not ask for usage")
	assert.True(t, suppressesUsage(nil, stream(`not json`)), "a forwarded stream whose body cannot be read")
	assert.False(t, suppressesUsage(nil, stream(`{"model":"m","stream":true,"stream_options":{"include_usage":true}}`)))
	assert.True(t, suppressesUsage(nil, &schemas.BifrostRequest{RequestType: schemas.PassthroughRequest, PassthroughRequest: &schemas.BifrostPassthroughRequest{Body: []byte(`{"model":"m","stream":true}`)}}), "a body asking to stream")
	assert.False(t, suppressesUsage(nil, &schemas.BifrostRequest{RequestType: schemas.PassthroughRequest, PassthroughRequest: &schemas.BifrostPassthroughRequest{Body: []byte(`{"model":"m"}`)}}), "a unary passthrough")

	probes := newUnpricedProbes()
	scopes := []string{"s"}
	first, _ := probes.acquire(scopes, []string{"row"})
	first.confirm("row")
	first.release()
	confirmed, busy := probes.acquire(scopes, []string{"row"})
	require.False(t, busy)
	require.Nil(t, confirmed, "the row is confirmed")
	held, busy := probes.acquireRows(nil, scopes, []string{"row"}, true)
	require.False(t, busy)
	require.NotNil(t, held, "a usage-suppressing request holds the confirmed row")
	_, busy = probes.acquireRows(nil, scopes, []string{"row"}, true)
	assert.True(t, busy, "a concurrent usage-suppressing request is held off")
	_, busy = probes.acquire(scopes, []string{"row"})
	assert.False(t, busy, "a request reporting usage is not")
}

// A request holds one row per key that may serve it, read under that key: a single key whose override
// patches a catalog row holds only that key's row, never the unpatched default no answer can confirm.
func TestProbeRowsAreTheServingKeysRows(t *testing.T) {
	plugin, _ := unpricedBudgetPlugin(t)
	rows := plugin.resolver.probeRows(nil, schemas.OpenAI, "free-on-key", schemas.ChatCompletionRequest, nil)
	assert.Equal(t, []string{"openai|chat|free-on-key|override|free-on-key-override"}, rows)
	pinned := plugin.resolver.probeRows(nil, schemas.OpenAI, "gpt-4o", schemas.ChatCompletionRequest, &modelcatalog.PricingLookupScopes{SelectedKeyID: "openai-key-1"})
	assert.Equal(t, []string{"openai|chat|gpt-4o"}, pinned)
}

// A stream forwarded with its raw body reaches the vendor without the converter that asks for usage:
// it must ask for it itself. The converted path keeps its default.
func TestRawBodyStreamMustAskForUsage(t *testing.T) {
	raw := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	raw.SetValue(schemas.BifrostContextKeyUseRawRequestBody, true)
	stream := func(body string) *schemas.BifrostRequest {
		return &schemas.BifrostRequest{RequestType: schemas.ChatCompletionStreamRequest, ChatRequest: &schemas.BifrostChatRequest{
			Provider: schemas.OpenAI, Model: "gpt-4o", RawRequestBody: []byte(body),
		}}
	}
	assert.True(t, suppressesUsage(raw, stream(`{"model":"gpt-4o","stream":true}`)), "a raw stream that does not ask for usage")
	assert.False(t, suppressesUsage(raw, stream(`{"model":"gpt-4o","stream":true,"stream_options":{"include_usage":true}}`)))
	assert.False(t, suppressesUsage(nil, stream(`{"model":"gpt-4o","stream":true}`)), "the converted path asks for usage itself")
	text := &schemas.BifrostRequest{RequestType: schemas.TextCompletionStreamRequest, TextCompletionRequest: &schemas.BifrostTextCompletionRequest{RawRequestBody: []byte(`{"stream":true}`)}}
	assert.True(t, suppressesUsage(raw, text))
}

// A disabled key serves nothing, so its row is never held: only the rows enabled keys price.
func TestProbeRowsSkipDisabledKeys(t *testing.T) {
	plugin, _ := unpricedBudgetPlugin(t)
	disabled := false
	plugin.resolver.governanceInMemoryStore = &mockInMemoryStore{configuredProviders: map[schemas.ModelProvider]configstore.ProviderConfig{
		schemas.OpenAI: {Keys: []schemas.Key{{ID: "openai-key-1", Enabled: &disabled}, {ID: "enabled-key"}}},
	}}
	assert.Equal(t, []string{"openai|chat|free-on-key"}, plugin.resolver.probeRows(nil, schemas.OpenAI, "free-on-key", schemas.ChatCompletionRequest, nil),
		"the disabled key's override row is not held")
}

// A hold whose request is still running is not replaced before unpricedProbeMaxHold; once its context
// ends, or the cap passes, a later request may take it.
func TestLiveHoldIsNotReplacedAfterTheStaleThreshold(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	probes := newUnpricedProbes()
	probes.now = func() time.Time { return now }
	owner, cancel := context.WithCancel(context.Background())
	held, busy := probes.acquireRows(owner, []string{"s"}, []string{"row"}, true)
	require.False(t, busy)
	require.NotNil(t, held)
	now = now.Add(2 * unpricedProbeStaleAfter)
	_, busy = probes.acquireRows(nil, []string{"s"}, []string{"row"}, true)
	assert.True(t, busy, "a long stream still running keeps its hold")
	defer cancel()
	now = now.Add(unpricedProbeMaxHold)
	capped, busy := probes.acquireRows(nil, []string{"s"}, []string{"row"}, true)
	assert.False(t, busy, "no hold outlives unpricedProbeMaxHold, even one whose context never ends")
	capped.release()
	now = now.Add(-unpricedProbeMaxHold)
	held, busy = probes.acquireRows(owner, []string{"s"}, []string{"row"}, true)
	require.False(t, busy)
	now = now.Add(2 * unpricedProbeStaleAfter)
	cancel()
	replacement, busy := probes.acquireRows(nil, []string{"s"}, []string{"row"}, true)
	assert.False(t, busy, "its context ended, and the hold is past the threshold")
	replacement.release()
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
