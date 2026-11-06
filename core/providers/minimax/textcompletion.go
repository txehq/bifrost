// Fork (txehq/bifrost): MiniMax text completions.
//
// Upstream's MiniMax provider (d20cb6ace) returns "unsupported" for text completions. The fork's
// earlier MiniMax provider served them through MiniMax's Anthropic-compatible text generation
// endpoint, and this file keeps that behaviour on the upstream provider. The streaming variant runs
// on the provider's streaming client, so a long stream is governed by the idle timeout and not by
// the unary ReadTimeout.

package minimax

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/capsohq/bifrost/core/providers/anthropic"
	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	schemas "github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

// miniMaxTextGenerationPath is the Anthropic-compatible text generation path, resolved against the
// base URL's origin so a base URL with a path prefix still reaches it.
const miniMaxTextGenerationPath = "/text/v1/messages"

// miniMaxBaseOrigin returns scheme://host of baseURL, or baseURL itself when it does not parse.
func miniMaxBaseOrigin(baseURL string) string {
	if parsed, err := url.Parse(baseURL); err == nil && parsed.Scheme != "" && parsed.Host != "" {
		return parsed.Scheme + "://" + parsed.Host
	}
	return baseURL
}

func (provider *MiniMaxProvider) buildTextGenerationURL(ctx *schemas.BifrostContext) string {
	return provider.baseOrigin + providerUtils.GetPathFromContext(ctx, miniMaxTextGenerationPath)
}

// parseMiniMaxAnthropicError maps an Anthropic-shaped error body from the text generation endpoint.
func parseMiniMaxAnthropicError(resp *fasthttp.Response, requestType schemas.RequestType, providerName schemas.ModelProvider, model string) *schemas.BifrostError {
	var errorResp anthropic.AnthropicError
	bifrostErr := providerUtils.HandleProviderAPIError(resp, &errorResp)
	if errorResp.Error != nil {
		if bifrostErr.Error == nil {
			bifrostErr.Error = &schemas.ErrorField{}
		}
		bifrostErr.Error.Type = &errorResp.Error.Type
		bifrostErr.Error.Message = errorResp.Error.Message
	}
	bifrostErr.ExtraFields.Provider = providerName
	bifrostErr.ExtraFields.OriginalModelRequested = model
	bifrostErr.ExtraFields.RequestType = requestType
	return bifrostErr
}

// miniMaxAnthropicBetaHeader returns the anthropic-beta header value carried in extra params, if any.
func miniMaxAnthropicBetaHeader(request *schemas.BifrostTextCompletionRequest) string {
	if request.Params == nil || request.Params.ExtraParams == nil {
		return ""
	}
	if betaHeader, ok := schemas.SafeExtractStringPointer(request.Params.ExtraParams["anthropic_beta"]); ok && betaHeader != nil {
		return *betaHeader
	}
	if betaHeaders, ok := schemas.SafeExtractStringSlice(request.Params.ExtraParams["anthropic_beta"]); ok && len(betaHeaders) > 0 {
		return strings.Join(betaHeaders, ",")
	}
	return ""
}

// toMiniMaxTextGenerationBody converts a text completion request into the Anthropic messages body.
func (provider *MiniMaxProvider) toMiniMaxTextGenerationBody(ctx *schemas.BifrostContext, request *schemas.BifrostTextCompletionRequest, stream bool) ([]byte, *schemas.BifrostError) {
	chatReq := request.ToBifrostChatRequest()
	if chatReq == nil {
		return nil, providerUtils.NewBifrostOperationError("invalid text completion request", nil)
	}
	chatReq.Provider = provider.GetProviderKey()
	anthropicReq, err := anthropic.ToAnthropicChatRequest(ctx, chatReq)
	if err != nil {
		return nil, providerUtils.NewBifrostOperationError("failed to convert request for minimax text generation", err)
	}
	if stream {
		anthropicReq.Stream = schemas.Ptr(true)
	} else {
		anthropicReq.Stream = nil
	}
	return providerUtils.CheckContextAndGetRequestBody(
		ctx,
		request,
		func() (providerUtils.RequestBodyWithExtraParams, error) {
			return anthropicReq, nil
		},
	)
}

func (provider *MiniMaxProvider) extractTextFromChatResponse(chatResp *schemas.BifrostChatResponse, requestType schemas.RequestType) *schemas.BifrostTextCompletionResponse {
	if chatResp == nil {
		return nil
	}

	textResp := &schemas.BifrostTextCompletionResponse{
		ID:      chatResp.ID,
		Model:   chatResp.Model,
		Object:  chatResp.Object,
		Usage:   chatResp.Usage,
		Choices: make([]schemas.BifrostResponseChoice, 0, len(chatResp.Choices)),
		ExtraFields: schemas.BifrostResponseExtraFields{
			RequestType:            requestType,
			Provider:               provider.GetProviderKey(),
			OriginalModelRequested: chatResp.ExtraFields.OriginalModelRequested,
			ChunkIndex:             chatResp.ExtraFields.ChunkIndex,
			Latency:                chatResp.ExtraFields.Latency,
			RawRequest:             chatResp.ExtraFields.RawRequest,
			RawResponse:            chatResp.ExtraFields.RawResponse,
		},
	}

	for _, choice := range chatResp.Choices {
		newChoice := schemas.BifrostResponseChoice{
			Index:        choice.Index,
			FinishReason: choice.FinishReason,
		}

		if choice.ChatStreamResponseChoice != nil && choice.ChatStreamResponseChoice.Delta != nil {
			newChoice.TextCompletionResponseChoice = &schemas.TextCompletionResponseChoice{
				Text: choice.ChatStreamResponseChoice.Delta.Content,
			}
		} else if choice.ChatNonStreamResponseChoice != nil && choice.ChatNonStreamResponseChoice.Message != nil {
			var text *string
			msg := choice.ChatNonStreamResponseChoice.Message
			if msg.Content != nil {
				if msg.Content.ContentStr != nil {
					text = msg.Content.ContentStr
				} else if len(msg.Content.ContentBlocks) > 0 {
					var builder strings.Builder
					for _, block := range msg.Content.ContentBlocks {
						if block.Text != nil {
							builder.WriteString(*block.Text)
						}
					}
					if builder.Len() > 0 {
						combined := builder.String()
						text = &combined
					}
				}
			}
			newChoice.TextCompletionResponseChoice = &schemas.TextCompletionResponseChoice{Text: text}
		}

		textResp.Choices = append(textResp.Choices, newChoice)
	}

	return textResp
}

// TextCompletion performs a text completion request through MiniMax's Anthropic-compatible text generation endpoint.
func (provider *MiniMaxProvider) TextCompletion(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostTextCompletionRequest) (*schemas.BifrostTextCompletionResponse, *schemas.BifrostError) {
	jsonData, bifrostErr := provider.toMiniMaxTextGenerationBody(ctx, request, false)
	if bifrostErr != nil {
		return nil, bifrostErr
	}

	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)
	req.SetRequestURI(provider.buildTextGenerationURL(ctx))
	req.Header.SetMethod(http.MethodPost)
	req.Header.SetContentType("application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	if betaHeader := miniMaxAnthropicBetaHeader(request); betaHeader != "" {
		req.Header.Set("anthropic-beta", betaHeader)
	}
	if value := key.Value.GetValue(); value != "" {
		req.Header.Set("Authorization", "Bearer "+value)
	}
	req.SetBody(jsonData)

	latency, bifrostErr, wait := providerUtils.MakeRequestWithContext(ctx, provider.client, req, resp)
	defer wait()
	if bifrostErr != nil {
		return nil, bifrostErr
	}

	sendBackRawRequest := providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest)
	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)

	if resp.StatusCode() != fasthttp.StatusOK {
		return nil, providerUtils.EnrichError(
			ctx,
			parseMiniMaxAnthropicError(resp, schemas.TextCompletionRequest, provider.GetProviderKey(), request.Model),
			jsonData,
			nil,
			sendBackRawRequest,
			sendBackRawResponse,
			latency,
		)
	}

	body, err := providerUtils.CheckAndDecodeBody(resp)
	if err != nil {
		return nil, providerUtils.SetErrorLatency(providerUtils.NewBifrostOperationError(schemas.ErrProviderResponseDecode, err), latency)
	}

	anthropicResp := &anthropic.AnthropicMessageResponse{}
	rawRequest, rawResponse, bifrostErr := providerUtils.HandleProviderResponse(body, anthropicResp, jsonData, sendBackRawRequest, sendBackRawResponse)
	if bifrostErr != nil {
		return nil, providerUtils.SetErrorLatency(bifrostErr, latency)
	}

	chatResp := anthropicResp.ToBifrostChatResponse(ctx)
	chatResp.ExtraFields.Provider = provider.GetProviderKey()
	chatResp.ExtraFields.OriginalModelRequested = request.Model
	chatResp.ExtraFields.Latency = latency.Milliseconds()
	if sendBackRawRequest {
		chatResp.ExtraFields.RawRequest = rawRequest
	}
	if sendBackRawResponse {
		chatResp.ExtraFields.RawResponse = rawResponse
	}
	return provider.extractTextFromChatResponse(chatResp, schemas.TextCompletionRequest), nil
}

// TextCompletionStream performs a streaming text completion request through MiniMax's
// Anthropic-compatible text generation endpoint, on the streaming client.
func (provider *MiniMaxProvider) TextCompletionStream(ctx *schemas.BifrostContext, postHookRunner schemas.PostHookRunner, postHookSpanFinalizer func(context.Context), key schemas.Key, request *schemas.BifrostTextCompletionRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	jsonData, bifrostErr := provider.toMiniMaxTextGenerationBody(ctx, request, true)
	if bifrostErr != nil {
		return nil, bifrostErr
	}

	headers := map[string]string{
		"Content-Type":      "application/json",
		"Accept":            "text/event-stream",
		"Cache-Control":     "no-cache",
		"anthropic-version": "2023-06-01",
	}
	if betaHeader := miniMaxAnthropicBetaHeader(request); betaHeader != "" {
		headers["anthropic-beta"] = betaHeader
	}
	if value := key.Value.GetValue(); value != "" {
		headers["Authorization"] = "Bearer " + value
	}

	chatStream, streamErr := anthropic.HandleAnthropicChatCompletionStreaming(
		ctx,
		provider.streamingClient,
		provider.buildTextGenerationURL(ctx),
		jsonData,
		headers,
		provider.networkConfig.ExtraHeaders,
		provider.networkConfig.StreamIdleTimeoutInSeconds,
		provider.networkConfig.BetaHeaderOverrides,
		providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest),
		providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse),
		provider.GetProviderKey(),
		postHookRunner,
		nil,
		nil,
		provider.logger,
		postHookSpanFinalizer,
	)
	if streamErr != nil {
		return nil, streamErr
	}

	responseChan := make(chan *schemas.BifrostStreamChunk, schemas.DefaultStreamBufferSize)
	go func() {
		defer close(responseChan)
		for chunk := range chatStream {
			if chunk == nil {
				continue
			}
			if chunk.BifrostError != nil {
				errCopy := *chunk.BifrostError
				errCopy.ExtraFields.RequestType = schemas.TextCompletionStreamRequest
				responseChan <- &schemas.BifrostStreamChunk{BifrostError: &errCopy}
				continue
			}
			if chunk.BifrostChatResponse == nil {
				continue
			}
			textResp := provider.extractTextFromChatResponse(chunk.BifrostChatResponse, schemas.TextCompletionStreamRequest)
			if textResp == nil {
				continue
			}
			responseChan <- &schemas.BifrostStreamChunk{BifrostTextCompletionResponse: textResp}
		}
	}()

	return responseChan, nil
}
