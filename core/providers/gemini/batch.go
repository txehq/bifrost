package gemini

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bytedance/sonic"
	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

// ToGeminiBatchGenerateContentRequest converts one Bifrost batch request body into the
// Gemini GenerateContentRequest shape used for batch input. OpenAI-style bodies (with
// "messages") have their messages converted to Gemini "contents"/"systemInstruction";
// bodies already in Gemini form are unmarshaled directly. Shared by the Gemini and Vertex
// batch paths so both emit identical request bodies.
func ToGeminiBatchGenerateContentRequest(body map[string]interface{}) (GeminiBatchGenerateContentRequest, error) {
	var geminiReq GeminiBatchGenerateContentRequest

	requestBytes, err := providerUtils.MarshalSorted(body)
	if err != nil {
		return geminiReq, fmt.Errorf("failed to marshal gemini request: %w", err)
	}
	if err := sonic.Unmarshal(requestBytes, &geminiReq); err != nil {
		return geminiReq, fmt.Errorf("failed to unmarshal gemini request: %w", err)
	}

	// OpenAI-style body: convert "messages" into Gemini "contents"/"systemInstruction".
	if rawMessages, ok := body["messages"]; ok {
		messagesBytes, err := providerUtils.MarshalSorted(rawMessages)
		if err != nil {
			return geminiReq, fmt.Errorf("failed to marshal messages: %w", err)
		}
		var chatMessages []schemas.ChatMessage
		if err := sonic.Unmarshal(messagesBytes, &chatMessages); err != nil {
			return geminiReq, fmt.Errorf("failed to unmarshal messages: %w", err)
		}
		contents, systemInstruction, err := convertBifrostMessagesToGemini(chatMessages)
		if err != nil {
			return geminiReq, fmt.Errorf("failed to convert messages: %w", err)
		}
		geminiReq.Contents = contents
		geminiReq.SystemInstruction = systemInstruction
	}

	return geminiReq, nil
}

// normalizeGeminiBatchState returns a batch state in its BATCH_STATE_* spelling. The
// generativelanguage API documents both BATCH_STATE_* and JOB_STATE_* for the same states,
// and which one REST returns is not established, so both are read as one.
func normalizeGeminiBatchState(state string) string {
	if suffix, ok := strings.CutPrefix(state, "JOB_STATE_"); ok {
		switch suffix {
		case "QUEUED":
			return GeminiBatchStatePending
		case "PARTIALLY_SUCCEEDED":
			return GeminiBatchStateSucceeded
		}
		return "BATCH_STATE_" + suffix
	}
	return state
}

// isGeminiBatchStateProcessing reports whether a batch has not reached a terminal state.
func isGeminiBatchStateProcessing(state string) bool {
	switch normalizeGeminiBatchState(state) {
	case GeminiBatchStatePending, GeminiBatchStateRunning:
		return true
	}
	return false
}

// isGeminiBatchStateDone reports whether a batch reached a terminal state.
func isGeminiBatchStateDone(state string) bool {
	switch normalizeGeminiBatchState(state) {
	case GeminiBatchStateSucceeded, GeminiBatchStateFailed, GeminiBatchStateCancelled, GeminiBatchStateExpired:
		return true
	}
	return false
}

// ToBifrostBatchStatus converts Gemini batch job state to Bifrost status. BATCH_STATE_*
// and JOB_STATE_* spellings of a state map to the same status.
func ToBifrostBatchStatus(geminiState string) schemas.BatchStatus {
	if normalized := normalizeGeminiBatchState(geminiState); normalized != geminiState {
		if status := ToBifrostBatchStatus(normalized); string(status) != normalized {
			return status
		}
		// A JOB_STATE_* with no known counterpart is reported as the vendor sent it.
		return schemas.BatchStatus(geminiState)
	}
	switch geminiState {
	case GeminiBatchStatePending, GeminiBatchStateRunning:
		return schemas.BatchStatusInProgress
	case GeminiBatchStateSucceeded:
		return schemas.BatchStatusCompleted
	case GeminiBatchStateFailed:
		return schemas.BatchStatusFailed
	case GeminiBatchStateCancelling:
		return schemas.BatchStatusCancelling
	case GeminiBatchStateCancelled:
		return schemas.BatchStatusCancelled
	case GeminiBatchStateExpired:
		return schemas.BatchStatusExpired
	default:
		return schemas.BatchStatus(geminiState)
	}
}

// ToGeminiBatchStatus converts Bifrost batch status to Gemini batch job state.
func ToGeminiBatchStatus(status schemas.BatchStatus) string {
	switch status {
	case schemas.BatchStatusValidating, schemas.BatchStatusInProgress:
		return GeminiBatchStateRunning
	case schemas.BatchStatusFinalizing:
		return GeminiBatchStateRunning
	case schemas.BatchStatusCompleted, schemas.BatchStatusEnded:
		return GeminiBatchStateSucceeded
	case schemas.BatchStatusFailed:
		return GeminiBatchStateFailed
	case schemas.BatchStatusCancelling:
		return GeminiBatchStateCancelling
	case schemas.BatchStatusCancelled:
		return GeminiBatchStateCancelled
	case schemas.BatchStatusExpired:
		return GeminiBatchStateExpired
	default:
		return GeminiBatchStateUnspecified
	}
}

// ToGeminiBatchJobResponse converts Bifrost batch create response to Gemini batch job response format.
func ToGeminiBatchJobResponse(resp *schemas.BifrostBatchCreateResponse) *GeminiBatchJobResponse {
	if resp == nil {
		return nil
	}

	succeededCount := resp.RequestCounts.Succeeded
	if succeededCount == 0 {
		succeededCount = resp.RequestCounts.Completed
	}

	geminiResp := &GeminiBatchJobResponse{
		Name: resp.ID,
		Metadata: &GeminiBatchMetadata{
			Name:       resp.ID,
			Type:       "type.googleapis.com/google.ai.generativelanguage.v1beta.BatchPredictionJob",
			CreateTime: formatGeminiTimestamp(resp.CreatedAt),
			UpdateTime: formatGeminiTimestamp(resp.CreatedAt),
			State:      ToGeminiBatchStatus(resp.Status),
			BatchStats: &GeminiBatchStats{
				RequestCount:           resp.RequestCounts.Total,
				PendingRequestCount:    max(0, resp.RequestCounts.Total-succeededCount-resp.RequestCounts.Failed),
				SuccessfulRequestCount: succeededCount,
			},
		},
	}

	if resp.OperationName != nil && *resp.OperationName != "" {
		geminiResp.Metadata.Name = *resp.OperationName
		geminiResp.Name = *resp.OperationName
	}

	if resp.InputFileID != "" {
		geminiResp.Metadata.InputConfig = &GeminiBatchMetadataInputConfig{
			FileName: resp.InputFileID,
		}
	}

	if resp.OutputFileID != nil && *resp.OutputFileID != "" {
		geminiResp.Dest = &GeminiBatchDest{
			FileName: *resp.OutputFileID,
		}
		geminiResp.Metadata.Output = &GeminiBatchMetadataOutputConfig{
			ResponsesFile: *resp.OutputFileID,
		}
	}

	if resp.Status == schemas.BatchStatusCompleted ||
		resp.Status == schemas.BatchStatusEnded ||
		resp.Status == schemas.BatchStatusFailed ||
		resp.Status == schemas.BatchStatusExpired ||
		resp.Status == schemas.BatchStatusCancelled {
		geminiResp.Done = true
	}

	return geminiResp
}

// ToGeminiBatchRetrieveResponse converts a Bifrost batch retrieve response to Gemini batch job response format.
func ToGeminiBatchRetrieveResponse(resp *schemas.BifrostBatchRetrieveResponse) *GeminiBatchJobResponse {
	if resp == nil {
		return nil
	}

	succeededCount := resp.RequestCounts.Succeeded
	if succeededCount == 0 {
		succeededCount = resp.RequestCounts.Completed
	}

	pendingCount := resp.RequestCounts.Pending
	if pendingCount == 0 && resp.RequestCounts.Total > 0 {
		processedCount := resp.RequestCounts.Completed
		if processedCount == 0 {
			processedCount = succeededCount
		}
		pendingCount = resp.RequestCounts.Total - processedCount - resp.RequestCounts.Failed
		if pendingCount < 0 {
			pendingCount = 0
		}
	}

	geminiResp := &GeminiBatchJobResponse{
		Name: resp.ID,
		Metadata: &GeminiBatchMetadata{
			Name:       resp.ID,
			Type:       "type.googleapis.com/google.ai.generativelanguage.v1beta.BatchPredictionJob",
			CreateTime: formatGeminiTimestamp(resp.CreatedAt),
			UpdateTime: formatGeminiTimestamp(resp.CreatedAt),
			State:      ToGeminiBatchStatus(resp.Status),
			BatchStats: &GeminiBatchStats{
				RequestCount:           resp.RequestCounts.Total,
				PendingRequestCount:    pendingCount,
				SuccessfulRequestCount: succeededCount,
			},
		},
	}

	if resp.OperationName != nil && *resp.OperationName != "" {
		geminiResp.Metadata.Name = *resp.OperationName
		geminiResp.Name = *resp.OperationName
	}

	if resp.Done != nil {
		geminiResp.Done = *resp.Done
	} else {
		geminiResp.Done = resp.Status == schemas.BatchStatusCompleted ||
			resp.Status == schemas.BatchStatusEnded ||
			resp.Status == schemas.BatchStatusFailed ||
			resp.Status == schemas.BatchStatusExpired ||
			resp.Status == schemas.BatchStatusCancelled
	}

	if resp.InputFileID != "" {
		geminiResp.Metadata.InputConfig = &GeminiBatchMetadataInputConfig{
			FileName: resp.InputFileID,
		}
	}

	if resp.OutputFileID != nil && *resp.OutputFileID != "" {
		geminiResp.Dest = &GeminiBatchDest{
			FileName: *resp.OutputFileID,
		}
		geminiResp.Metadata.Output = &GeminiBatchMetadataOutputConfig{
			ResponsesFile: *resp.OutputFileID,
		}
	}

	// Set end time from the most relevant terminal timestamp
	var endTime int64
	if resp.CompletedAt != nil {
		endTime = *resp.CompletedAt
	} else if resp.FailedAt != nil {
		endTime = *resp.FailedAt
	} else if resp.ExpiredAt != nil {
		endTime = *resp.ExpiredAt
	} else if resp.CancelledAt != nil {
		endTime = *resp.CancelledAt
	}
	if endTime > 0 {
		geminiResp.Metadata.EndTime = formatGeminiTimestamp(endTime)
	}

	return geminiResp
}

// ToGeminiBatchListResponse converts a Bifrost batch list response to Gemini format.
func ToGeminiBatchListResponse(resp *schemas.BifrostBatchListResponse) *GeminiBatchListResponse {
	if resp == nil {
		return nil
	}

	operations := make([]GeminiBatchJobResponse, 0, len(resp.Data))
	for i := range resp.Data {
		if geminiResp := ToGeminiBatchRetrieveResponse(&resp.Data[i]); geminiResp != nil {
			operations = append(operations, *geminiResp)
		}
	}

	geminiListResp := &GeminiBatchListResponse{
		Operations: operations,
	}

	if resp.NextCursor != nil {
		geminiListResp.NextPageToken = *resp.NextCursor
	}

	return geminiListResp
}

// parseGeminiTimestamp converts Gemini RFC3339 timestamp to Unix timestamp.
func parseGeminiTimestamp(timestamp string) int64 {
	if timestamp == "" {
		return 0
	}
	t, err := time.Parse(time.RFC3339, timestamp)
	if err != nil {
		return 0
	}
	return t.Unix()
}

// batchResultsFileSource opens a batch's responses file as a streamed download through the
// files download endpoint (alt=media), so a page of results is read without holding the
// file.
func (provider *GeminiProvider) batchResultsFileSource(ctx *schemas.BifrostContext, key schemas.Key, fileName string) (providerUtils.BatchResultsSource, *schemas.BifrostError) {
	// The base URL is like https://generativelanguage.googleapis.com/v1beta; downloads are
	// served from https://generativelanguage.googleapis.com/download/v1beta.
	baseURL := strings.Replace(provider.networkConfig.BaseURL, "/v1beta", "/download/v1beta", 1)
	filePath, bifrostErr := geminiResourcePath(fileName, "files", "file_id")
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	url := fmt.Sprintf("%s/%s:download?alt=media", baseURL, filePath)
	return func(offset int64) (*providerUtils.ResultStream, *schemas.BifrostError) {
		provider.logger.Debug("gemini batch results file download url: " + url)
		return providerUtils.OpenResultStream(ctx, provider.streamingClient, offset, func(req *fasthttp.Request) {
			providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)
			req.SetRequestURI(url)
			req.Header.SetMethod(http.MethodGet)
			setGeminiAuthHeader(req, key.Value.GetValue())
		}, parseGeminiError)
	}, nil
}

// parseGeminiBatchResultsJSONL parses a whole responses file. sourceIndex of each line
// names the fallback id of a line that carries no key.
func parseGeminiBatchResultsJSONL(body []byte, warnf func(string, ...any)) ([]schemas.BatchResultItem, []schemas.BatchError) {
	results := make([]schemas.BatchResultItem, 0)
	lineIndex := 0
	parseResult := providerUtils.ParseJSONL(body, func(line []byte) error {
		sourceIndex := lineIndex
		lineIndex++
		resultItem, err := parseGeminiBatchResultsFileLine(line, fmt.Sprintf("request-%d", sourceIndex), false)
		if err != nil {
			if warnf != nil {
				warnf("gemini batch results file parse error: %s", strconv.QuoteToASCII(err.Error()))
			}
			return err
		}
		results = append(results, resultItem)
		return nil
	})

	return results, parseResult.Errors
}

// parseGeminiBatchResultsFileLine parses one line of a responses file into a result item.
func parseGeminiBatchResultsFileLine(line []byte, fallbackID string, includeRaw bool) (schemas.BatchResultItem, error) {
	var resultLine GeminiBatchFileResultLine
	if err := sonic.Unmarshal(line, &resultLine); err != nil {
		return schemas.BatchResultItem{}, err
	}
	return geminiFileResultToBatchResultItem(resultLine, fallbackID, includeRaw)
}

func geminiFileResultToBatchResultItem(resultLine GeminiBatchFileResultLine, fallbackID string, includeRaw ...bool) (schemas.BatchResultItem, error) {
	customID := resultLine.CustomID
	if customID == "" {
		customID = resultLine.Key
	}
	if customID == "" {
		customID = fallbackID
	}
	if resultLine.Error != nil {
		return BatchResultItemFromGeminiResponse(customID, "", nil, resultLine.Error, false), nil
	}
	if len(resultLine.Response) == 0 || strings.TrimSpace(string(resultLine.Response)) == "null" {
		return schemas.BatchResultItem{}, fmt.Errorf("gemini batch result line has neither response nor error")
	}

	// A file written through an OpenAI-compatible integration wraps the body as
	// {status_code, body}; it is already in OpenAI shape.
	var envelope GeminiFileResponseLine
	if err := sonic.Unmarshal(resultLine.Response, &envelope); err != nil {
		return schemas.BatchResultItem{CustomID: customID}, err
	}
	if envelope.StatusCode != 0 || envelope.Body != nil {
		item := schemas.BatchResultItem{
			CustomID: customID,
			Status:   schemas.BatchResultStatusSucceeded,
			Response: &schemas.BatchResultResponse{
				StatusCode: envelope.StatusCode,
				Body:       normalizeBatchBody(envelope.Body),
			},
		}
		if envelope.StatusCode < 200 || envelope.StatusCode >= 300 {
			item.Status = schemas.BatchResultStatusFailed
		}
		return item, nil
	}

	var native GenerateContentResponse
	if err := sonic.Unmarshal(resultLine.Response, &native); err != nil {
		return schemas.BatchResultItem{CustomID: customID}, err
	}
	// A responses file carries no per-line url, so its lines are chat lines.
	return BatchResultItemFromGeminiResponse(customID, "", &native, nil, len(includeRaw) > 0 && includeRaw[0]), nil
}

// geminiBatchOutput extracts the batch output (a responses file name or inline responses)
// from a Gemini batch job response. The generativelanguage REST API reports the output
// under the Operation's top-level `response` field, mirrored in `metadata.output`; the
// `dest` field is a client-SDK-only abstraction and is never present on the wire. The
// top-level response is preferred, with metadata.output as a fallback.
func geminiBatchOutput(resp *GeminiBatchJobResponse) (fileName string, inlined []GeminiInlinedResponse) {
	if resp == nil {
		return "", nil
	}
	if resp.Response != nil {
		fileName = resp.Response.ResponsesFile
		if resp.Response.InlinedResponses != nil {
			inlined = resp.Response.InlinedResponses.InlinedResponses
		}
	}
	if fileName == "" && len(inlined) == 0 && resp.Metadata != nil && resp.Metadata.Output != nil {
		fileName = resp.Metadata.Output.ResponsesFile
		if resp.Metadata.Output.InlinedResponses != nil {
			inlined = resp.Metadata.Output.InlinedResponses.InlinedResponses
		}
	}
	return fileName, inlined
}

// BatchResultItemFromGeminiResponse converts one batch line's outcome into a result item in
// the OpenAI shape of the line's endpoint, using the sync operation's response converter,
// so the body and its unified usage are what the sync route returns for the same response:
//
//   - /v1/images/generations: an image generation response, the images kept in data[].b64_json
//     with per-modality usage;
//   - any other endpoint, or none: a chat completion.
//
// A line the vendor failed carries the vendor's error and status failed. With includeRaw the
// native GenerateContentResponse (safetyRatings included) is in body.extra_fields.raw_response,
// where the sync route puts it. Shared by the Gemini and Vertex batch paths.
func BatchResultItemFromGeminiResponse(customID string, endpoint schemas.BatchEndpoint, native *GenerateContentResponse, vendorErr *GeminiBatchErrorInfo, includeRaw bool) schemas.BatchResultItem {
	item := schemas.BatchResultItem{CustomID: customID}
	if vendorErr != nil {
		item.Status = schemas.BatchResultStatusFailed
		item.Error = &schemas.BatchResultError{
			Code:    fmt.Sprintf("%d", vendorErr.Code),
			Message: vendorErr.Message,
		}
		return item
	}
	if native == nil {
		// Neither a response nor an error: the vendor returned no outcome for the line.
		item.Status = schemas.BatchResultStatusFailed
		return item
	}

	var converted any
	switch endpoint {
	case schemas.BatchEndpointImageGeneration:
		imageResponse, bifrostErr := native.ToBifrostImageGenerationResponse()
		if bifrostErr != nil || imageResponse == nil {
			// The sync route answers this response with an error (no image came back).
			item.Status = schemas.BatchResultStatusFailed
			item.Error = &schemas.BatchResultError{Message: "image generation returned no image"}
			if bifrostErr != nil && bifrostErr.Error != nil {
				if bifrostErr.Error.Message != "" {
					item.Error.Message = bifrostErr.Error.Message
				}
				if bifrostErr.Error.Code != nil {
					item.Error.Code = *bifrostErr.Error.Code
				}
			}
			return item
		}
		converted = imageResponse
	default:
		converted = native.ToBifrostChatResponse()
	}

	body, err := batchResultBody(converted)
	if err != nil {
		item.Status = schemas.BatchResultStatusFailed
		item.Error = &schemas.BatchResultError{Message: "failed to encode batch result: " + err.Error()}
		return item
	}
	// extra_fields describes a sync call (latency, routing); a batch line has only the
	// vendor's raw response to report there.
	delete(body, "extra_fields")
	if includeRaw {
		if raw, rawErr := batchResultBody(native); rawErr == nil {
			body["extra_fields"] = map[string]any{"raw_response": raw}
		}
	}
	item.Status = schemas.BatchResultStatusSucceeded
	item.Response = &schemas.BatchResultResponse{StatusCode: http.StatusOK, Body: body}
	return item
}

// batchResultBody encodes a response as the generic JSON object a result line carries.
func batchResultBody(response any) (map[string]any, error) {
	encoded, err := providerUtils.MarshalSorted(response)
	if err != nil {
		return nil, err
	}
	body := map[string]any{}
	if err := sonic.Unmarshal(encoded, &body); err != nil {
		return nil, err
	}
	return body, nil
}

// geminiInlineResponseToBatchResultItem converts a single Gemini inline batch response
// into a Bifrost BatchResultItem. customIDFallback is used when the response carries no
// metadata key. The line's endpoint comes from the metadata the create sent with it.
func geminiInlineResponseToBatchResultItem(inlineResp GeminiInlinedResponse, customIDFallback string, includeRaw ...bool) schemas.BatchResultItem {
	customID := customIDFallback
	endpoint := schemas.BatchEndpoint("")
	if inlineResp.Metadata != nil {
		if inlineResp.Metadata.Key != "" {
			customID = inlineResp.Metadata.Key
		}
		endpoint = schemas.BatchEndpoint(inlineResp.Metadata.Endpoint)
	}
	return BatchResultItemFromGeminiResponse(customID, endpoint, inlineResp.Response, inlineResp.Error, len(includeRaw) > 0 && includeRaw[0])
}

// isNativeGeminiBatchBody reports whether a batch line's body is already a Gemini
// GenerateContentRequest (it has contents and none of the OpenAI request fields).
func isNativeGeminiBatchBody(body map[string]any) bool {
	if _, ok := body["contents"]; !ok {
		return false
	}
	_, hasMessages := body["messages"]
	_, hasPrompt := body["prompt"]
	return !hasMessages && !hasPrompt
}

// ToGeminiBatchLineRequest builds the GenerateContentRequest Gemini runs for one inline
// batch line, as encoded JSON, and returns the endpoint the line's result is converted
// back to. An OpenAI-shape body is converted with the sync operation's converter and
// encoded as the sync route encodes it, so the line and a sync request with the same body
// reach the vendor with the same parameters:
//
//   - /v1/chat/completions: ToGeminiChatCompletionRequest, extra params merged when the
//     caller asked for passthrough, as on the chat route;
//   - /v1/images/generations: ToGeminiImageGenerationRequest, extra params always merged,
//     as on the image route.
//
// A body already in Gemini shape (contents, no messages) is sent as it is, and its result
// is a chat line. Any other url is an error: batchGenerateContent cannot run it. provider
// and model are the batch's. Shared by the Gemini and Vertex batch paths.
func ToGeminiBatchLineRequest(ctx *schemas.BifrostContext, item schemas.BatchRequestItem, provider schemas.ModelProvider, model string, batchEndpoint schemas.BatchEndpoint) ([]byte, schemas.BatchEndpoint, error) {
	body := item.RequestBody()
	if body == nil {
		return nil, "", &schemas.BatchLineError{CustomID: item.CustomID, Param: "body", Message: "body is required"}
	}
	if isNativeGeminiBatchBody(body) {
		geminiReq, err := ToGeminiBatchGenerateContentRequest(body)
		if err != nil {
			return nil, "", &schemas.BatchLineError{CustomID: item.CustomID, Param: "body", Message: err.Error()}
		}
		encoded, err := providerUtils.MarshalSorted(geminiReq)
		if err != nil {
			return nil, "", err
		}
		return encoded, "", nil
	}

	endpoint := item.Endpoint(batchEndpoint)
	if endpoint == "" {
		if _, ok := body["messages"]; ok {
			endpoint = schemas.BatchEndpointChatCompletions
		}
	}
	switch endpoint {
	case schemas.BatchEndpointChatCompletions:
		chatRequest, err := item.ToBifrostChatRequest(provider, model)
		if err != nil {
			return nil, "", &schemas.BatchLineError{CustomID: item.CustomID, Param: "body", Message: err.Error()}
		}
		converted, err := ToGeminiChatCompletionRequest(ctx, chatRequest)
		if err != nil {
			return nil, "", &schemas.BatchLineError{CustomID: item.CustomID, Param: "body", Message: err.Error()}
		}
		if converted == nil {
			return nil, "", &schemas.BatchLineError{CustomID: item.CustomID, Param: "body", Message: "chat request could not be converted"}
		}
		mergeExtraParams := ctx != nil && ctx.Value(schemas.BifrostContextKeyPassthroughExtraParams) == true
		encoded, err := providerUtils.MarshalConvertedRequestBody(converted, mergeExtraParams)
		if err != nil {
			return nil, "", err
		}
		return encoded, endpoint, nil
	case schemas.BatchEndpointImageGeneration:
		if schemas.IsImagenModel(model) {
			return nil, "", &schemas.BatchLineError{CustomID: item.CustomID, Param: "url", Message: fmt.Sprintf("model %s is an Imagen model, which batchGenerateContent cannot run", model)}
		}
		imageRequest, err := item.ToBifrostImageGenerationRequest(provider, model)
		if err != nil {
			return nil, "", &schemas.BatchLineError{CustomID: item.CustomID, Param: "body", Message: err.Error()}
		}
		converted := ToGeminiImageGenerationRequest(imageRequest)
		if converted == nil {
			return nil, "", &schemas.BatchLineError{CustomID: item.CustomID, Param: "body", Message: "image generation request could not be converted"}
		}
		encoded, err := providerUtils.MarshalConvertedRequestBody(converted, true)
		if err != nil {
			return nil, "", err
		}
		return encoded, endpoint, nil
	case "":
		return nil, "", &schemas.BatchLineError{CustomID: item.CustomID, Param: "url", Message: "url is required: the line's body is not a chat completion"}
	default:
		return nil, "", &schemas.BatchLineError{CustomID: item.CustomID, Param: "url", Message: fmt.Sprintf("url %s cannot run in a generateContent batch (supported: %s, %s)", endpoint, schemas.BatchEndpointChatCompletions, schemas.BatchEndpointImageGeneration)}
	}
}

// NewBatchLineBadRequestError answers a batch line the caller got wrong with a 400 that
// names the line and the field.
func NewBatchLineBadRequestError(index int, err error) *schemas.BifrostError {
	var lineErr *schemas.BatchLineError
	if !errors.As(err, &lineErr) {
		return providerUtils.NewBifrostOperationError(fmt.Sprintf("failed to convert batch request item %d", index), err)
	}
	lineErr.Index = index
	bifrostErr := providerUtils.NewBifrostBadRequestError(lineErr.Error())
	bifrostErr.Error.Param = fmt.Sprintf("requests[%d].%s", index, lineErr.Param)
	return bifrostErr
}

// CheckBatchLineModel refuses a line whose body names a model other than the batch's: a
// batch runs one model, and a line asking for another would silently run on the batch's.
// resolve maps a caller's model name to the model the key serves it as.
func CheckBatchLineModel(item schemas.BatchRequestItem, provider schemas.ModelProvider, batchModel string, resolve func(string) string) error {
	lineModel, ok := item.RequestBody()["model"].(string)
	if !ok || lineModel == "" {
		return nil
	}
	_, bare := schemas.ParseModelString(lineModel, provider)
	bare = strings.TrimPrefix(bare, "models/")
	if bare == batchModel || lineModel == batchModel || (resolve != nil && resolve(bare) == batchModel) {
		return nil
	}
	return &schemas.BatchLineError{CustomID: item.CustomID, Param: "body.model", Message: fmt.Sprintf("model %s differs from the batch's model %s: a batch runs one model", lineModel, batchModel)}
}

// normalizeBatchBody converts camelCase usage keys (promptTokens, completionTokens,
// totalTokens) to snake_case as expected by the batch accounting pricing code.
func normalizeBatchBody(body map[string]interface{}) map[string]interface{} {
	if body == nil {
		return nil
	}
	out := make(map[string]interface{}, len(body))
	for k, v := range body {
		out[k] = v
	}
	if raw, ok := body["usage"]; ok {
		if usage, ok := raw.(map[string]interface{}); ok {
			normalized := make(map[string]interface{}, len(usage))
			for k, v := range usage {
				switch k {
				case "promptTokens":
					normalized["prompt_tokens"] = v
				case "completionTokens":
					normalized["completion_tokens"] = v
				case "totalTokens":
					normalized["total_tokens"] = v
				default:
					normalized[k] = v
				}
			}
			out["usage"] = normalized
		}
	}
	return out
}

// extractGeminiUsageMetadata extracts usage metadata (as ints) from Gemini response
func extractGeminiUsageMetadata(geminiResponse *GenerateContentResponse) (int, int, int) {
	var inputTokens, outputTokens, totalTokens int
	if geminiResponse.UsageMetadata != nil {
		usageMetadata := geminiResponse.UsageMetadata
		inputTokens = int(usageMetadata.PromptTokenCount)
		outputTokens = int(usageMetadata.CandidatesTokenCount)
		totalTokens = int(usageMetadata.TotalTokenCount)
	}
	return inputTokens, outputTokens, totalTokens
}
