package governance

import (
	"testing"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests use only hooks that exist before TXE-7963, so they run red on a checkout without it.

func requireUnsettledRefusal(t *testing.T, shortCircuit *schemas.LLMPluginShortCircuit, decision, what string) {
	t.Helper()
	require.NotNil(t, shortCircuit, "%s: a queued answer nothing will charge must be refused under a dollar budget before the call", what)
	require.NotNil(t, shortCircuit.Error, what)
	require.NotNil(t, shortCircuit.Error.StatusCode, what)
	assert.Equal(t, 403, *shortCircuit.Error.StatusCode, what)
	assert.Equal(t, schemas.ErrorTypePolicyModelBlocked, shortCircuit.Error.ExtraFields.ErrorType, what)
	require.NotNil(t, shortCircuit.Error.Type, what)
	assert.Equal(t, decision, *shortCircuit.Error.Type, what)
}

func responsesOn(requestType schemas.RequestType, model string, background *bool) *schemas.BifrostRequest {
	req := &schemas.BifrostResponsesRequest{Provider: schemas.OpenAI, Model: model}
	if background != nil {
		req.Params = &schemas.ResponsesParameters{Background: background}
	}
	return &schemas.BifrostRequest{RequestType: requestType, ResponsesRequest: req}
}

func passthroughResponses(body string) *schemas.BifrostRequest {
	return &schemas.BifrostRequest{RequestType: schemas.PassthroughRequest, PassthroughRequest: &schemas.BifrostPassthroughRequest{
		Provider: schemas.OpenAI, Model: "gpt-4o", Method: "POST", Path: "/v1/responses", Body: []byte(body),
	}}
}

// A background Responses create answers "queued" with no usage, and the retrieve that delivers the
// finished answer is never priced, so under a dollar budget it would be charged $0 on any model.
// It is refused before the call, on every route that can send one. In the foreground, and on a
// key with no dollar budget, it is served.
func TestBackgroundResponsesUnderADollarBudgetIsRefusedBeforeTheCall(t *testing.T) {
	plugin, _ := unpricedBudgetPlugin(t)
	pre := func(vk string, req *schemas.BifrostRequest) *schemas.LLMPluginShortCircuit {
		t.Helper()
		_, shortCircuit, err := plugin.PreLLMHook(resolverCtx(plugin.store, vk), req)
		require.NoError(t, err)
		return shortCircuit
	}
	background, foreground := true, false

	for _, requestType := range []schemas.RequestType{schemas.ResponsesRequest, schemas.ResponsesStreamRequest, schemas.WebSocketResponsesRequest} {
		requireUnsettledRefusal(t, pre("sk-bf-budget", responsesOn(requestType, "gpt-4o", &background)), "background_unsettled",
			string(requestType)+" with background: true on a priced model")
	}
	requireUnsettledRefusal(t, pre("sk-bf-budget", passthroughResponses(`{"model":"gpt-4o","input":"hi","background":true}`)), "background_unsettled",
		"a passthrough Responses body with background: true")
	requireUnsettledRefusal(t, pre("sk-bf-budget", passthroughResponses(`{"model":"gpt-4o","background":false,"input":"hi","background":true}`)), "background_unsettled",
		"a passthrough body whose duplicated background key ends true, as a last-key-wins decoder reads it")
	interactions := passthroughResponses(`{"model":"gemini-omni","input":"hi","background":true}`)
	interactions.PassthroughRequest.Path = "/v1beta/interactions"
	requireUnsettledRefusal(t, pre("sk-bf-budget", interactions), "background_unsettled",
		"a passthrough Interactions body with background: true")

	assert.Nil(t, pre("sk-bf-budget", responsesOn(schemas.ResponsesRequest, "gpt-4o", &foreground)), "background: false is a finished, priced answer")
	assert.Nil(t, pre("sk-bf-budget", responsesOn(schemas.ResponsesRequest, "gpt-4o", nil)), "a Responses create with no background is served")
	assert.Nil(t, pre("sk-bf-budget", passthroughResponses(`{"model":"gpt-4o","input":"hi"}`)), "a passthrough body with no background is served")
	assert.Nil(t, pre("sk-bf-open", responsesOn(schemas.ResponsesRequest, "gpt-4o", &background)), "a key with no dollar budget has nothing to charge")
}

// A video or batch create is charged when framework/jobaccounting settles its job. With provider
// job accounting off (client.disable_provider_job_accounting, the production setting) nothing
// settles, so under a dollar budget every job would be charged $0. Until the transport says jobs
// are settled, they are refused under a dollar budget; a key with no budget is served.
func TestSettledJobUnderADollarBudgetIsRefusedWhenNothingSettlesIt(t *testing.T) {
	plugin, _ := unpricedBudgetPlugin(t)
	video := &schemas.BifrostRequest{
		RequestType:            schemas.VideoGenerationRequest,
		VideoGenerationRequest: &schemas.BifrostVideoGenerationRequest{Provider: schemas.OpenAI, Model: "clip-1"},
	}
	_, shortCircuit, err := plugin.PreLLMHook(resolverCtx(plugin.store, "sk-bf-budget"), video)
	require.NoError(t, err)
	requireUnsettledRefusal(t, shortCircuit, "job_unsettled", "a priced video create under a budget with no job accounting")

	_, shortCircuit, err = plugin.PreLLMHook(resolverCtx(plugin.store, "sk-bf-open"), video)
	require.NoError(t, err)
	assert.Nil(t, shortCircuit, "a key with no dollar budget is served")
}
