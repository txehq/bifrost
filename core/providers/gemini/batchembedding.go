package gemini

import (
	"bytes"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/bytedance/sonic"
	"github.com/bytedance/sonic/ast"
	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	"github.com/capsohq/bifrost/core/schemas"
)

// Embedding batches run through models/{model}:asyncBatchEmbedContent, a different method
// from the generateContent batch with the same job object around it. What Google documents,
// and what it does not, as read on 2026-11-10:
//
//   - The method, its EmbedContentBatch resource, InlinedEmbedContentRequest {request,
//     metadata} and InlinedEmbedContentResponse {metadata, error | response} are in the API
//     reference (https://ai.google.dev/api/embeddings, "models.asyncBatchEmbedContent").
//     The reference does not name the method's response type; the job is read here as the
//     same Operation the generateContent batch returns (name, metadata.state,
//     metadata.batchStats, metadata.output, response), which is where Google's SDK reads it
//     (googleapis/python-genai, batches.py, _BatchJob_from_mldev).
//   - State spelling: the reference's BatchState enum is BATCH_STATE_*; the Batch API guide
//     (https://ai.google.dev/gemini-api/docs/batch-api) lists JOB_STATE_* and reads it from
//     metadata.state. Which one REST returns is not established by either page, so both
//     are read as one (normalizeGeminiBatchState).
//   - Inline responses: the reference names output.inlinedResponses; the SDK reads
//     metadata.output.inlinedEmbedContentResponses.inlinedResponses. Both are read.
//   - Result order: the reference says inlined responses and the responses file are "in the
//     same order as the input requests". Putting a multi-input line back together relies on
//     that order, and checks it against the index each request carried.
//   - Idempotency: there is no idempotency key. The guide says "The creation of a batch job
//     is not idempotent. If you send the same creation request twice, two separate batch
//     jobs will be created." A caller that lost a create response finds its job in the list
//     by display name.
//   - Limits: the guide gives 20MB for an inline request and 2GB for an input file, and a
//     job expires after 48 hours pending or running. A per-batch request count, a
//     concurrent-batch limit and an enqueued-token quota are not stated on either page.

// isNativeGeminiEmbeddingBatchBody reports whether a batch line's body is already a Gemini
// EmbedContentRequest (it has content and no OpenAI input).
func isNativeGeminiEmbeddingBatchBody(body map[string]any) bool {
	if _, ok := body["content"]; !ok {
		return false
	}
	_, hasInput := body["input"]
	return !hasInput
}

// IsEmbeddingBatch reports whether a batch create is an embedding batch: its first inline
// line runs /v1/embeddings, or, with no inline lines (a file or a raw body), the batch's
// endpoint is /v1/embeddings. Every other line of an inline batch must then run the same url.
func IsEmbeddingBatch(request *schemas.BifrostBatchCreateRequest) bool {
	if request == nil {
		return false
	}
	if len(request.Requests) > 0 {
		return request.Requests[0].Endpoint(request.Endpoint) == schemas.BatchEndpointEmbeddings
	}
	return request.Endpoint == schemas.BatchEndpointEmbeddings
}

// ToGeminiEmbeddingBatchLineRequests builds the EmbedContentRequests Gemini runs for one
// inline /v1/embeddings batch line, as encoded JSON. The line's body is parsed into the
// request the sync route builds from it and converted with the sync operation's converter,
// so each request is an entry of the :batchEmbedContents body the sync route sends for the
// same body. An EmbedContentRequest embeds one content, so a line whose input holds several
// items becomes that many requests, in input order.
//
// Fields outside the unified request are merged into every request, as the sync route merges
// them into its body: an embedding batch has no body of its own to carry them.
//
// A body already in Gemini shape (content, no input) is sent as it is.
//
// batchEndpoint is the batch's own endpoint, which a line with no url inherits. A line whose
// operation is not /v1/embeddings is refused: a batch runs one url, and a line with no url
// under a chat batch endpoint is a chat line even when the first line named /v1/embeddings.
func ToGeminiEmbeddingBatchLineRequests(item schemas.BatchRequestItem, batchEndpoint schemas.BatchEndpoint, provider schemas.ModelProvider, model string) ([][]byte, error) {
	body := item.RequestBody()
	if body == nil {
		return nil, &schemas.BatchLineError{CustomID: item.CustomID, Param: "body", Message: "body is required"}
	}
	if operation := batchLineOperation(item.Endpoint(batchEndpoint)); operation != schemas.BatchEndpointEmbeddings {
		return nil, &schemas.BatchLineError{CustomID: item.CustomID, Param: "url", Unsupported: true, Message: fmt.Sprintf("url %s differs from the batch's other lines (%s): a batch runs one url", operation, schemas.BatchEndpointEmbeddings)}
	}
	if isNativeGeminiEmbeddingBatchBody(body) {
		encoded, err := providerUtils.MarshalSorted(body)
		if err != nil {
			return nil, err
		}
		return [][]byte{encoded}, nil
	}
	embeddingRequest, err := item.ToBifrostEmbeddingRequest(provider, model)
	if err != nil {
		return nil, &schemas.BatchLineError{CustomID: item.CustomID, Param: "body", Message: err.Error()}
	}
	// The sync route refuses an encoding_format other than float before any upstream call;
	// the converter drops the field, so without this a base64 line would run, be billed and
	// come back as floats.
	if err := RejectNonFloatEncodingFormat(embeddingRequest); err != nil {
		return nil, &schemas.BatchLineError{CustomID: item.CustomID, Param: "body.encoding_format", Unsupported: true, Message: "encoding_format other than float is not supported: Google embedding endpoints return float vectors only"}
	}
	converted, err := ToGeminiEmbeddingRequest(embeddingRequest)
	if err != nil {
		return nil, &schemas.BatchLineError{CustomID: item.CustomID, Param: "body", Message: err.Error()}
	}
	requests := make([][]byte, len(converted.Requests))
	for i := range converted.Requests {
		entry := converted.Requests[i]
		entry.ExtraParams = converted.ExtraParams
		encoded, err := providerUtils.MarshalConvertedRequestBody(&entry, true)
		if err != nil {
			return nil, err
		}
		requests[i] = encoded
	}
	return requests, nil
}

// geminiEmbeddingBatchRequestItems builds the inline requests of an embedding batch from
// its lines. Each request's metadata carries the line's custom_id and url, and, for a line
// sent as several requests, the request's position in the line and the line's request count.
func geminiEmbeddingBatchRequestItems(request *schemas.BifrostBatchCreateRequest, model string, resolve func(string) string) ([]GeminiBatchRequestItem, *schemas.BifrostError) {
	items := make([]GeminiBatchRequestItem, 0, len(request.Requests))
	for i, line := range request.Requests {
		if err := CheckBatchLineModel(line, schemas.Gemini, model, resolve); err != nil {
			return nil, NewBatchLineBadRequestError(i, err)
		}
		lineRequests, err := ToGeminiEmbeddingBatchLineRequests(line, request.Endpoint, schemas.Gemini, model)
		if err != nil {
			return nil, NewBatchLineBadRequestError(i, err)
		}
		for inputIndex, lineRequest := range lineRequests {
			metadata := &GeminiBatchRequestMetadata{
				Key:      line.CustomID,
				Endpoint: string(schemas.BatchEndpointEmbeddings),
			}
			if len(lineRequests) > 1 {
				metadata.InputIndex = strconv.Itoa(inputIndex)
				metadata.InputCount = strconv.Itoa(len(lineRequests))
			}
			items = append(items, GeminiBatchRequestItem{Request: lineRequest, Metadata: metadata})
		}
	}
	return items, nil
}

// geminiEmbeddingKey is the key only an EmbedContentResponse has. As a JSON key or value
// its quotes are unescaped, so a body without these bytes holds no embedding response.
var geminiEmbeddingKey = []byte(`"embedding"`)

// isGeminiEmbedContentResponse reports whether a batch line's response is an
// EmbedContentResponse: it has an embedding, which a GenerateContentResponse never has.
// The scan for the key costs nothing for a response without it; the lookup that follows
// skips over the response's values without decoding them.
func isGeminiEmbedContentResponse(response []byte) bool {
	if !bytes.Contains(response, geminiEmbeddingKey) {
		return false
	}
	node, err := sonic.Get(response, "embedding")
	if err != nil || !node.Exists() {
		return false
	}
	return node.TypeSafe() == ast.V_OBJECT
}

// sniffGeminiEmbeddingResponses sets EmbedResponse on the inline responses of a batch job
// that are EmbedContentResponses. body is the job as the vendor sent it and inlined the
// responses geminiBatchOutput read from it. A job is decoded directly, as a generateContent
// batch; only a body that holds the embedding key is read a second time, with each response
// kept raw, to find the responses that are embeddings. A generateContent job therefore costs
// one scan of its body and no allocation here.
func sniffGeminiEmbeddingResponses(body []byte, inlined []GeminiInlinedResponse) error {
	if len(inlined) == 0 || !bytes.Contains(body, geminiEmbeddingKey) {
		return nil
	}
	var job GeminiRawBatchJob
	if err := sonic.Unmarshal(body, &job); err != nil {
		return err
	}
	// The same choice geminiBatchOutput made, so the raw responses line up with inlined.
	pick := func(output *GeminiRawBatchOutput) []GeminiRawInlinedResponse {
		if output == nil {
			return nil
		}
		if output.InlinedResponses != nil && len(output.InlinedResponses.InlinedResponses) > 0 {
			return output.InlinedResponses.InlinedResponses
		}
		if output.InlinedEmbedContentResponses != nil {
			return output.InlinedEmbedContentResponses.InlinedResponses
		}
		return nil
	}
	raw := pick(job.Response)
	if len(raw) == 0 && job.Metadata != nil {
		raw = pick(job.Metadata.Output)
	}
	if len(raw) != len(inlined) {
		return fmt.Errorf("gemini batch job has %d inline responses, read %d", len(raw), len(inlined))
	}
	for i := range raw {
		if !isGeminiEmbedContentResponse(raw[i].Response) {
			continue
		}
		embedResponse := &GeminiEmbeddingResponse{}
		if err := sonic.Unmarshal(raw[i].Response, embedResponse); err != nil {
			return err
		}
		inlined[i].EmbedResponse = embedResponse
		inlined[i].Response = nil
	}
	return nil
}

// hasGeminiMultiInputLines reports whether any inline response says it is one of several
// requests a line was sent as. It allocates nothing, so a batch with no such line is paged
// without grouping.
func hasGeminiMultiInputLines(inlined []GeminiInlinedResponse) bool {
	for i := range inlined {
		if inlined[i].Metadata != nil && inlined[i].Metadata.InputCount != "" {
			return true
		}
	}
	return false
}

// groupGeminiInlinedResponses splits a batch's inline responses into the lines they answer.
// A response is a line of its own unless its metadata says it is the first of several
// requests one embedding line was sent as; the responses that follow it with the same key
// and count, at the next positions, belong to the same line. Responses arrive in the order
// of the requests, so a line's responses are consecutive.
func groupGeminiInlinedResponses(inlined []GeminiInlinedResponse) []GeminiInlinedLine {
	lines := make([]GeminiInlinedLine, 0, len(inlined))
	for i := 0; i < len(inlined); {
		end := i + 1
		if count, index, ok := geminiInlinedInputPosition(inlined[i]); ok && index == 0 {
			for end < len(inlined) && end-i < count {
				nextCount, nextIndex, nextOK := geminiInlinedInputPosition(inlined[end])
				if !nextOK || nextCount != count || nextIndex != end-i || inlined[end].Metadata.Key != inlined[i].Metadata.Key {
					break
				}
				end++
			}
		}
		lines = append(lines, GeminiInlinedLine{Start: i, End: end})
		i = end
	}
	return lines
}

// geminiInlinedInputPosition reads, from a response's metadata, the request count of the
// line it belongs to and its position in the line. ok is false for a response that is a
// line of its own.
//
// A generateContent response is always a line of its own, whatever its metadata says. The
// metadata is the caller's: a native :batchGenerateContent body is passed through with the
// metadata it carries, and the vendor echoes it. Only an embedding batch is created with
// these fields by the gateway, so only a response that is an embedding or an error is
// grouped. Grouped chat responses would be settled as one line while each was billed.
func geminiInlinedInputPosition(response GeminiInlinedResponse) (count, index int, ok bool) {
	if response.Response != nil || response.Metadata == nil || response.Metadata.InputCount == "" {
		return 0, 0, false
	}
	count, countErr := strconv.Atoi(response.Metadata.InputCount)
	index, indexErr := strconv.Atoi(response.Metadata.InputIndex)
	if countErr != nil || indexErr != nil || count < 2 || index < 0 || index >= count {
		return 0, 0, false
	}
	return count, index, true
}

// isGeminiInlinedEmbeddingResponse reports whether an inline response answers an embedding
// request: it carries an EmbedContentResponse, or the metadata the create sent says so.
func isGeminiInlinedEmbeddingResponse(response GeminiInlinedResponse) bool {
	if response.EmbedResponse != nil {
		return true
	}
	return response.Metadata != nil && response.Metadata.Endpoint == string(schemas.BatchEndpointEmbeddings)
}

// geminiInlinedLineEndpoint returns the operation an inline response's line ran, and
// whether it is known: a failed line of a batch created elsewhere carries neither a
// response nor the metadata that names its operation.
func geminiInlinedLineEndpoint(response GeminiInlinedResponse) (schemas.BatchEndpoint, bool) {
	if isGeminiInlinedEmbeddingResponse(response) {
		return schemas.BatchEndpointEmbeddings, true
	}
	endpoint := schemas.BatchEndpoint("")
	if response.Metadata != nil {
		endpoint = schemas.BatchEndpoint(response.Metadata.Endpoint)
	}
	return endpoint, endpoint != "" || response.Error == nil
}

// geminiInlinedLineToBatchResultItem converts the inline responses of one batch line into
// its result item. model is the batch's model, which an embeddings body names.
func geminiInlinedLineToBatchResultItem(responses []GeminiInlinedResponse, customIDFallback string, model string, includeRaw bool) schemas.BatchResultItem {
	first := responses[0]
	if !isGeminiInlinedEmbeddingResponse(first) {
		return geminiInlineResponseToBatchResultItem(first, customIDFallback, includeRaw)
	}
	customID := customIDFallback
	if first.Metadata != nil && first.Metadata.Key != "" {
		customID = first.Metadata.Key
	}
	// A line sent as several requests succeeds only when every one of them did: a body
	// with some of its embeddings missing would not be the sync response for the line.
	if count, _, ok := geminiInlinedInputPosition(first); ok && len(responses) != count {
		return schemas.BatchResultItem{
			CustomID: customID,
			Status:   schemas.BatchResultStatusFailed,
			Error:    &schemas.BatchResultError{Message: fmt.Sprintf("the vendor returned %d of the line's %d embeddings in order", len(responses), count)},
		}
	}
	embedResponses := make([]*GeminiEmbeddingResponse, 0, len(responses))
	for _, response := range responses {
		if response.Error != nil {
			return BatchResultItemFromGeminiEmbedResponses(customID, model, nil, response.Error, false)
		}
		embedResponses = append(embedResponses, response.EmbedResponse)
	}
	return BatchResultItemFromGeminiEmbedResponses(customID, model, embedResponses, nil, includeRaw)
}

// BatchResultItemFromGeminiEmbedResponses converts the EmbedContentResponses of one
// embedding batch line into a result item whose body is the OpenAI embeddings response the
// sync route returns for them: data[i] is the embedding of the line's i-th input, and usage
// is the sum of the responses' prompt tokens. A line the vendor failed carries the vendor's
// error and status failed. With includeRaw the native responses are in
// body.extra_fields.raw_response: the response itself for a single-input line, the list of
// responses for a line with several inputs.
func BatchResultItemFromGeminiEmbedResponses(customID string, model string, natives []*GeminiEmbeddingResponse, vendorErr *GeminiBatchErrorInfo, includeRaw bool) schemas.BatchResultItem {
	item := schemas.BatchResultItem{CustomID: customID}
	if vendorErr != nil {
		item.Status = schemas.BatchResultStatusFailed
		item.Error = &schemas.BatchResultError{
			Code:    fmt.Sprintf("%d", vendorErr.Code),
			Message: vendorErr.Message,
		}
		return item
	}

	combined := &GeminiEmbeddingResponse{Embeddings: make([]GeminiEmbedding, 0, len(natives))}
	var promptTokens, totalTokens int32
	for _, native := range natives {
		embeddings := geminiResponseEmbeddings(native)
		if len(embeddings) == 0 {
			// Neither an embedding nor an error: the vendor returned no outcome for an input.
			item.Status = schemas.BatchResultStatusFailed
			if len(natives) > 1 {
				item.Error = &schemas.BatchResultError{Message: "the vendor returned no embedding for one of the line's inputs"}
			}
			return item
		}
		combined.Embeddings = append(combined.Embeddings, embeddings...)
		if native.UsageMetadata != nil {
			promptTokens += native.UsageMetadata.PromptTokenCount
			totalTokens += native.UsageMetadata.TotalTokenCount
		}
	}
	if len(combined.Embeddings) == 0 {
		item.Status = schemas.BatchResultStatusFailed
		return item
	}
	if promptTokens > 0 || totalTokens > 0 {
		combined.UsageMetadata = &GenerateContentResponseUsageMetadata{PromptTokenCount: promptTokens, TotalTokenCount: totalTokens}
	}

	body, err := batchResultBody(ToBifrostEmbeddingResponse(combined, model))
	if err != nil {
		item.Status = schemas.BatchResultStatusFailed
		item.Error = &schemas.BatchResultError{Message: "failed to encode batch result: " + err.Error()}
		return item
	}
	// extra_fields describes a sync call (latency, routing); a batch line has only the
	// vendor's raw response to report there.
	delete(body, "extra_fields")
	if includeRaw {
		var raw any = natives
		if len(natives) == 1 {
			raw = natives[0]
		}
		if encoded, rawErr := providerUtils.MarshalSorted(raw); rawErr == nil {
			var decoded any
			if rawErr = schemas.Unmarshal(encoded, &decoded); rawErr == nil {
				body["extra_fields"] = map[string]any{"raw_response": decoded}
			}
		}
	}
	item.Status = schemas.BatchResultStatusSucceeded
	item.Response = &schemas.BatchResultResponse{StatusCode: http.StatusOK, Body: body}
	return item
}

// geminiBatchModelName returns the bare model id of a batch job's metadata.model
// ("models/gemini-embedding-001" → "gemini-embedding-001").
func geminiBatchModelName(metadata *GeminiBatchMetadata) string {
	if metadata == nil {
		return ""
	}
	return strings.TrimPrefix(metadata.Model, "models/")
}
