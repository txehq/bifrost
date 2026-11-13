package bifrost

import (
	"fmt"

	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	"github.com/capsohq/bifrost/core/schemas"
)

// A custom provider's allowed_requests is the operator's least-privilege control: an operation
// it does not allow never reaches the vendor with the provider's keys (fork, TXE-7370). Core
// enforces it here, for every request type, before a key is chosen:
//
//   - checkAllowedRequest, from the request worker, for every request the queue carries;
//   - SelectKeyForProviderRequestType, for the WebSocket and WebRTC sessions (realtime, live,
//     WebSocket Responses), which take a key from core and dial the vendor themselves.
//
// The provider methods keep their own CheckOperationAllowed calls. They are no longer the
// only gate: a method that omits one is still covered here, and
// TestCustomProviderAllowedRequestsGateEveryCall fails when a call of any base reaches the
// upstream with the operation not allowed.
//
// The operation checked is the one dispatch sends upstream, as the provider methods always
// checked it: a text completion served as chat is checked as chat, a chat request served
// through the Responses API (compat conversion or reasoning.mode) as responses, a Responses
// request served as chat as chat, and an emulated decision as responses. Streams keep their
// stream flag.
//
// Request types Bifrost issues on its own behalf:
//
//   - list_models: the all-providers listing skips a provider that does not allow it, and the
//     model catalog falls back to the static datasheet (both unchanged).
//   - batch_retrieve, batch_results, video_retrieve, video_download from the provider-job
//     accounting sweeper: exempt here (schemas.BifrostContextKeyProviderJobAccounting). The
//     sweeper only reads the state of a job a caller was allowed to create, so it can bill it;
//     a refusal is final to the sweeper and would leave the job unbilled. The provider methods
//     that gate these operations themselves still refuse them, as they did before.
//   - responses, from a GPT Live session's backend delegation: checked as responses. The
//     OpenAI provider already refused it when responses was not allowed.

// upstreamOperation returns the operation dispatch sends to the provider for req.
func upstreamOperation(ctx *schemas.BifrostContext, provider schemas.Provider, req *schemas.BifrostRequest) schemas.RequestType {
	var changeType schemas.RequestType
	if ctx != nil {
		changeType, _ = ctx.Value(schemas.BifrostContextKeyChangeRequestType).(schemas.RequestType)
	}
	switch req.RequestType {
	case schemas.TextCompletionRequest, schemas.TextCompletionStreamRequest:
		// handleProviderRequest and handleProviderStreamRequest convert only a request with input.
		if changeType == schemas.ChatCompletionRequest && req.TextCompletionRequest != nil && req.TextCompletionRequest.Input != nil {
			return streamVariant(schemas.ChatCompletionRequest, req.RequestType == schemas.TextCompletionStreamRequest)
		}
	case schemas.ChatCompletionRequest, schemas.ChatCompletionStreamRequest:
		if changeType == schemas.ResponsesRequest || chatReasoningModeRoutesToResponses(ctx, provider, req.ChatRequest) {
			return streamVariant(schemas.ResponsesRequest, req.RequestType == schemas.ChatCompletionStreamRequest)
		}
	case schemas.ResponsesRequest, schemas.ResponsesStreamRequest:
		if changeType == schemas.ChatCompletionRequest {
			return streamVariant(schemas.ChatCompletionRequest, req.RequestType == schemas.ResponsesStreamRequest)
		}
	case schemas.DecisionRequest:
		if changeType == schemas.ResponsesRequest {
			return schemas.ResponsesRequest
		}
	}
	return req.RequestType
}

func streamVariant(operation schemas.RequestType, stream bool) schemas.RequestType {
	if !stream {
		return operation
	}
	switch operation {
	case schemas.ChatCompletionRequest:
		return schemas.ChatCompletionStreamRequest
	case schemas.ResponsesRequest:
		return schemas.ResponsesStreamRequest
	}
	return operation
}

// allowedRequestsExempt reports whether ctx is one of Bifrost's own provider-job accounting
// polls. The key is reserved, so a plugin cannot set it, and only a bool true counts.
func allowedRequestsExempt(ctx *schemas.BifrostContext) bool {
	if ctx == nil {
		return false
	}
	exempt, _ := ctx.Value(schemas.BifrostContextKeyProviderJobAccounting).(bool)
	return exempt
}

// checkAllowedRequest refuses req when config is a custom provider whose allowed_requests does
// not allow the operation dispatch would send upstream. It returns nil for a built-in provider,
// a custom provider without allowed_requests, and Bifrost's own job-accounting polls.
func checkAllowedRequest(ctx *schemas.BifrostContext, provider schemas.Provider, config *schemas.ProviderConfig, req *schemas.BifrostRequest) *schemas.BifrostError {
	if config == nil || config.CustomProviderConfig == nil || config.CustomProviderConfig.AllowedRequests == nil {
		return nil
	}
	if allowedRequestsExempt(ctx) {
		return nil
	}
	return providerUtils.CheckOperationAllowed(provider.GetProviderKey(), config.CustomProviderConfig, upstreamOperation(ctx, provider, req))
}

// errOperationNotAllowed is the error SelectKeyForProviderRequestType returns for an operation
// the custom provider's allowed_requests does not allow.
func errOperationNotAllowed(requestType schemas.RequestType, providerKey schemas.ModelProvider) error {
	return fmt.Errorf("%s is not supported by %s provider", requestType, providerKey)
}
