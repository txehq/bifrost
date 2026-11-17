package anthropic

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/bytedance/sonic"
	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	"github.com/capsohq/bifrost/core/schemas"
)

// Inline batch lines in the OpenAI shape (contract C8). A line whose url is
// /v1/chat/completions carries an OpenAI chat completion body; it is converted with the sync
// chat route's converter, so the vendor receives the same Messages body through a batch as
// through the sync route. Anthropic's own Message Batches API and Claude on Vertex both take
// the result. A succeeded Message comes back as the sync route's chat.completion.

// BatchLineIsOpenAIChat reports whether a batch line runs as an OpenAI chat completion that
// must be converted to a Messages body: its url, or the batch's endpoint when the line names
// none, is /v1/chat/completions. A line with no url in a batch with no endpoint is an
// Anthropic-native line (the /anthropic/v1/messages/batches route sends these) and runs
// unchanged.
func BatchLineIsOpenAIChat(item schemas.BatchRequestItem, batchEndpoint schemas.BatchEndpoint) bool {
	return item.Endpoint(batchEndpoint) == schemas.BatchEndpointChatCompletions
}

// CheckBatchLineURL refuses a line whose url is an operation a Messages batch cannot run.
// Anthropic batches run chat only: an OpenAI chat completion line or a native Messages line.
func CheckBatchLineURL(item schemas.BatchRequestItem, batchEndpoint schemas.BatchEndpoint) error {
	switch endpoint := item.Endpoint(batchEndpoint); endpoint {
	case "", schemas.BatchEndpointChatCompletions, schemas.BatchEndpointMessages:
		return nil
	default:
		return &schemas.BatchLineError{
			CustomID:    item.CustomID,
			Param:       "url",
			Unsupported: true,
			Message:     fmt.Sprintf("url %s cannot run in a Messages batch (supported: %s, %s)", endpoint, schemas.BatchEndpointChatCompletions, schemas.BatchEndpointMessages),
		}
	}
}

// BatchChatLineParams converts one batch line whose body is an OpenAI chat completion into
// the Messages body the sync chat route sends for the same request: the line is read with
// schemas.BatchRequestItem.ToBifrostChatRequest (the sync route's reading of the body) and
// built with BuildAnthropicChatRequestBody under cfg, the config the provider's sync route
// uses. provider is the batch's provider key and model the batch's model.
//
// A line the caller got wrong returns a *schemas.BatchLineError; anything else is Bifrost's.
// The beta headers the body needs are added to ctx, as on the sync route.
func BatchChatLineParams(ctx *schemas.BifrostContext, item schemas.BatchRequestItem, provider schemas.ModelProvider, model string, cfg AnthropicRequestBuildConfig) (map[string]any, error) {
	chatRequest, err := item.ToBifrostChatRequest(provider, model)
	if err != nil {
		return nil, &schemas.BatchLineError{CustomID: item.CustomID, Param: "body", Message: err.Error()}
	}
	// A batch line has no raw body of its own: the request builder must take the typed path
	// even when the batch request's context asks for raw bodies.
	if useRaw, _ := ctx.Value(schemas.BifrostContextKeyUseRawRequestBody).(bool); useRaw {
		ctx.SetValue(schemas.BifrostContextKeyUseRawRequestBody, false)
		defer ctx.SetValue(schemas.BifrostContextKeyUseRawRequestBody, true)
	}
	cfg.IsStreaming = false
	body, bifrostErr := BuildAnthropicChatRequestBody(ctx, chatRequest, cfg)
	if bifrostErr != nil {
		message := "anthropic request build failed"
		if bifrostErr.Error != nil && bifrostErr.Error.Message != "" {
			message = bifrostErr.Error.Message
		}
		if bifrostErr.StatusCode != nil && *bifrostErr.StatusCode == http.StatusBadRequest {
			return nil, &schemas.BatchLineError{CustomID: item.CustomID, Param: "body", Message: message}
		}
		return nil, fmt.Errorf("%s", message)
	}
	if len(body) == 0 {
		// The builder short-circuits to a nil body under large-payload passthrough.
		return nil, fmt.Errorf("anthropic request build produced an empty body")
	}
	params := map[string]any{}
	if err := sonic.Unmarshal(body, &params); err != nil {
		return nil, err
	}
	return params, nil
}

// BatchLineModel is the model a converted line runs on: the batch's model, or, in a batch
// that names none, the line's own body model with any provider prefix removed.
func BatchLineModel(item schemas.BatchRequestItem, provider schemas.ModelProvider, batchModel *string) (string, error) {
	if batchModel != nil && strings.TrimSpace(*batchModel) != "" {
		return *batchModel, nil
	}
	if lineModel, ok := item.RequestBody()["model"].(string); ok && strings.TrimSpace(lineModel) != "" {
		_, bare := schemas.ParseModelString(lineModel, provider)
		return bare, nil
	}
	return "", &schemas.BatchLineError{CustomID: item.CustomID, Param: "body.model", Message: "model is required: neither the batch nor the line names one"}
}

// batchResultStatus maps a Messages batch result type to the line status of contract C8.
func batchResultStatus(resultType string) schemas.BatchResultStatus {
	switch resultType {
	case "succeeded":
		return schemas.BatchResultStatusSucceeded
	case "expired":
		return schemas.BatchResultStatusExpired
	case "canceled", "cancelled":
		return schemas.BatchResultStatusCancelled
	default:
		// "errored", and any type this code does not know, is not a success.
		return schemas.BatchResultStatusFailed
	}
}

// batchResultError is the vendor's error for a line that did not succeed. An errored line
// carries Anthropic's error envelope ({"type":"error","error":{"type","message"}}); an
// expired or canceled line carries none, so its result type is the error.
func batchResultError(result AnthropicBatchResultData) *schemas.BatchResultError {
	if result.Error != nil {
		code, message := result.Error.Type, result.Error.Message
		if inner := result.Error.Error; inner != nil {
			code, message = inner.Type, inner.Message
		}
		return &schemas.BatchResultError{Code: code, Message: message}
	}
	switch result.Type {
	case "expired":
		return &schemas.BatchResultError{Code: "expired", Message: "the request expired before the batch processed it"}
	case "canceled", "cancelled":
		return &schemas.BatchResultError{Code: "canceled", Message: "the batch was canceled before the request was processed"}
	}
	return &schemas.BatchResultError{Code: result.Type, Message: "the request returned no result"}
}

// BatchResultLineToOpenAI converts one line of a Messages batch results file into the
// OpenAI-shape result line of contract C8: a succeeded Message becomes the chat.completion
// the sync chat route returns for it, with the unified usage; any other result carries the
// vendor's error and its status. includeRaw keeps the original Message in
// body.extra_fields.raw_response, for reconciliation.
func BatchResultLineToOpenAI(ctx *schemas.BifrostContext, line AnthropicBatchResultItem, includeRaw bool) schemas.BatchResultItem {
	item := schemas.BatchResultItem{CustomID: line.CustomID, Status: batchResultStatus(line.Result.Type)}
	if item.Status != schemas.BatchResultStatusSucceeded {
		item.Error = batchResultError(line.Result)
		return item
	}
	if line.Result.Message == nil {
		item.Status = schemas.BatchResultStatusFailed
		item.Error = &schemas.BatchResultError{Message: "the succeeded result carries no message"}
		return item
	}

	encoded, err := sonic.Marshal(line.Result.Message)
	if err != nil {
		return batchResultEncodeFailure(item, err)
	}
	var message AnthropicMessageResponse
	if err := sonic.Unmarshal(encoded, &message); err != nil {
		return batchResultEncodeFailure(item, err)
	}
	chatResponse := message.ToBifrostChatResponse(ctx)
	if chatResponse.Object == "" {
		// The line is an OpenAI chat completion body (contract C8), so it says so.
		chatResponse.Object = "chat.completion"
	}
	encoded, err = providerUtils.MarshalSorted(chatResponse)
	if err != nil {
		return batchResultEncodeFailure(item, err)
	}
	body := map[string]any{}
	if err := sonic.Unmarshal(encoded, &body); err != nil {
		return batchResultEncodeFailure(item, err)
	}
	// extra_fields describes a sync call (latency, routing); a batch line has only the
	// vendor's raw response to report there.
	delete(body, "extra_fields")
	if includeRaw {
		body["extra_fields"] = map[string]any{"raw_response": line.Result.Message}
	}
	item.Response = &schemas.BatchResultResponse{
		StatusCode: http.StatusOK,
		Body:       schemas.UnifyBatchResultUsage(schemas.BatchEndpointChatCompletions, body),
	}
	return item
}

func batchResultEncodeFailure(item schemas.BatchResultItem, err error) schemas.BatchResultItem {
	item.Status = schemas.BatchResultStatusFailed
	item.Error = &schemas.BatchResultError{Message: "failed to convert batch result: " + err.Error()}
	return item
}

// BatchResultLineToNative is the result line the /anthropic/v1/messages/batches route
// returns: the Messages result unchanged, with the line's status beside it.
func BatchResultLineToNative(line AnthropicBatchResultItem) schemas.BatchResultItem {
	item := schemas.BatchResultItem{
		CustomID: line.CustomID,
		Status:   batchResultStatus(line.Result.Type),
		Result: &schemas.BatchResultData{
			Type:    line.Result.Type,
			Message: line.Result.Message,
		},
	}
	if line.Result.Error != nil {
		item.Error = batchResultError(line.Result)
	}
	return item
}

// anthropicBatchCreateRequest builds the Message Batches request body. OpenAI chat
// completion lines are converted with the sync chat route's converter and config; native
// Messages lines run unchanged. converted reports whether any line was converted. A line
// the caller got wrong is a 400 naming the line.
func (provider *AnthropicProvider) anthropicBatchCreateRequest(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostBatchCreateRequest) (*AnthropicBatchCreateRequest, bool, *schemas.BifrostError) {
	providerName := provider.GetProviderKey()
	anthropicReq := &AnthropicBatchCreateRequest{Requests: make([]AnthropicBatchRequestItem, len(request.Requests))}
	converted := false
	for i, item := range request.Requests {
		if err := CheckBatchLineURL(item, request.Endpoint); err != nil {
			return nil, false, providerUtils.NewBatchLineBadRequestError(i, err)
		}
		if !BatchLineIsOpenAIChat(item, request.Endpoint) {
			anthropicReq.Requests[i] = AnthropicBatchRequestItem{CustomID: item.CustomID, Params: item.RequestBody()}
			continue
		}
		if item.RequestBody() == nil {
			return nil, false, providerUtils.NewBatchLineBadRequestError(i, &schemas.BatchLineError{CustomID: item.CustomID, Param: "body", Message: "body is required"})
		}
		model, err := BatchLineModel(item, providerName, request.Model)
		if err != nil {
			return nil, false, providerUtils.NewBatchLineBadRequestError(i, err)
		}
		if request.Model == nil || strings.TrimSpace(*request.Model) == "" {
			// Core resolves a key alias only for the batch's model. A model read from the line
			// itself is resolved here, so an alias reaches Anthropic as the model id it names.
			model = key.Aliases.Resolve(model)
		}
		if err := providerUtils.CheckBatchLineModel(item, providerName, model, key.Aliases.Resolve); err != nil {
			return nil, false, providerUtils.NewBatchLineBadRequestError(i, err)
		}
		params, err := BatchChatLineParams(ctx, item, providerName, model, provider.chatRequestBuildConfig())
		if err != nil {
			return nil, false, providerUtils.NewBatchLineBadRequestError(i, err)
		}
		anthropicReq.Requests[i] = AnthropicBatchRequestItem{CustomID: item.CustomID, Params: params}
		converted = true
	}
	return anthropicReq, converted, nil
}

// chatRequestBuildConfig is the request build config of the provider's sync chat route.
func (provider *AnthropicProvider) chatRequestBuildConfig() AnthropicRequestBuildConfig {
	return AnthropicRequestBuildConfig{
		Provider:                  schemas.Anthropic,
		IsStreaming:               false,
		BetaHeaderOverrides:       provider.networkConfig.BetaHeaderOverrides,
		ShouldSendBackRawRequest:  provider.sendBackRawRequest,
		ShouldSendBackRawResponse: provider.sendBackRawResponse,
	}
}

// isAnthropicNativeBatchRoute reports whether the caller came in on the Anthropic-native
// batch routes (/anthropic/v1/messages/batches), which return Messages results unchanged.
// Every other route returns the OpenAI shape of contract C8.
func isAnthropicNativeBatchRoute(ctx *schemas.BifrostContext) bool {
	if ctx == nil {
		return false
	}
	integration, _ := ctx.Value(schemas.BifrostContextKeyIntegrationType).(string)
	return integration == "anthropic"
}
