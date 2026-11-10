package xai

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bytedance/sonic"
	"github.com/capsohq/bifrost/core/providers/gemini"
	"github.com/capsohq/bifrost/core/providers/openai"
	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

// xAI's batch API (TXE-6664, contract C8) is not OpenAI-shaped: a batch is created empty by
// name, requests are added to it with a batch_request_id each, and results are paged from
// /v1/batches/{id}/results rather than downloaded as a file. A batch has no status field;
// its state is a set of counters.

// xaiBatchChatKey is the batch_request (and result response) member that carries a chat
// completion. It is the only line type this provider converts (the MVP's).
const xaiBatchChatKey = "chat_get_completion"

// xaiDefaultBatchName names a batch the caller gave neither metadata nor a display name;
// xAI's examples always send a name.
const xaiDefaultBatchName = "bifrost-batch"

// nowFunc is the clock the batch status mapping reads; tests pin it.
var nowFunc = time.Now

// XAIBatchCreateRequest is the body of POST /v1/batches.
type XAIBatchCreateRequest struct {
	Name        string `json:"name"`
	InputFileID string `json:"input_file_id,omitempty"`
}

// XAIBatchAddRequestsRequest is the body of POST /v1/batches/{batch_id}/requests.
type XAIBatchAddRequestsRequest struct {
	BatchRequests []XAIBatchRequest `json:"batch_requests"`
}

// XAIBatchRequest is one request added to a batch. BatchRequest holds exactly one member
// named for the request type (chat_get_completion for chat) whose value is that endpoint's
// request body.
type XAIBatchRequest struct {
	BatchRequestID string                     `json:"batch_request_id"`
	BatchRequest   map[string]json.RawMessage `json:"batch_request"`
}

// XAIBatchState holds a batch's request counters.
type XAIBatchState struct {
	NumRequests  int `json:"num_requests"`
	NumPending   int `json:"num_pending"`
	NumSuccess   int `json:"num_success"`
	NumError     int `json:"num_error"`
	NumCancelled int `json:"num_cancelled"`
}

// XAIBatch is xAI's batch object. The time fields are read under both the protobuf names
// (create_time, expire_time, cancel_time) and the names the guide uses (expires_at), as
// RFC 3339 strings or Unix seconds: the vendor's reference was not reachable when this was
// written, and no response was recorded (see the TXE-6664 PR).
type XAIBatch struct {
	BatchID     string        `json:"batch_id"`
	Name        string        `json:"name"`
	CreateTime  xaiTimestamp  `json:"create_time"`
	CreatedAt   xaiTimestamp  `json:"created_at"`
	ExpireTime  xaiTimestamp  `json:"expire_time"`
	ExpiresAt   xaiTimestamp  `json:"expires_at"`
	CancelTime  xaiTimestamp  `json:"cancel_time"`
	CancelledAt xaiTimestamp  `json:"cancelled_at"`
	State       XAIBatchState `json:"state"`
}

// XAIBatchListResponse is the body of GET /v1/batches.
type XAIBatchListResponse struct {
	Batches         []XAIBatch `json:"batches"`
	PaginationToken string     `json:"pagination_token"`
}

// XAIBatchResultsResponse is one page of GET /v1/batches/{batch_id}/results.
type XAIBatchResultsResponse struct {
	Results         []XAIBatchResult `json:"results"`
	PaginationToken string           `json:"pagination_token"`
}

// XAIBatchResult is one result line. A succeeded line carries batch_result.response with a
// member named for the request type; a failed one carries error_message (the guide) or an
// error object (the vendor sheet). State is read when present.
type XAIBatchResult struct {
	BatchRequestID string               `json:"batch_request_id"`
	BatchResult    *XAIBatchResultBody  `json:"batch_result,omitempty"`
	State          string               `json:"state,omitempty"`
	ErrorMessage   string               `json:"error_message,omitempty"`
	Error          *XAIBatchResultError `json:"error,omitempty"`
}

// XAIBatchResultBody wraps a result line's response or error.
type XAIBatchResultBody struct {
	Response map[string]json.RawMessage `json:"response,omitempty"`
	Error    *XAIBatchResultError       `json:"error,omitempty"`
}

// XAIBatchResultError is a failed line's error. Code is kept as the vendor wrote it,
// string or number.
type XAIBatchResultError struct {
	Code    any    `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// xaiTimestamp decodes a time written as an RFC 3339 string, Unix seconds (number or
// numeric string), Unix milliseconds, or a protobuf {seconds} object. Anything else,
// including null, leaves it unset.
type xaiTimestamp struct {
	unix *int64
}

func (t *xaiTimestamp) UnmarshalJSON(data []byte) error {
	s := strings.TrimSpace(string(data))
	if s == "" || s == "null" {
		return nil
	}
	if strings.HasPrefix(s, "{") {
		var obj struct {
			Seconds any `json:"seconds"`
		}
		if err := sonic.Unmarshal(data, &obj); err == nil && obj.Seconds != nil {
			t.setNumber(fmt.Sprint(obj.Seconds))
		}
		return nil
	}
	if strings.HasPrefix(s, `"`) {
		var str string
		if err := sonic.Unmarshal(data, &str); err != nil {
			return nil
		}
		if ts, err := time.Parse(time.RFC3339Nano, str); err == nil {
			u := ts.Unix()
			t.unix = &u
			return nil
		}
		t.setNumber(str)
		return nil
	}
	t.setNumber(s)
	return nil
}

func (t *xaiTimestamp) setNumber(s string) {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || f <= 0 {
		return
	}
	u := int64(f)
	if u > 1e12 { // milliseconds
		u /= 1000
	}
	t.unix = &u
}

func firstTime(ts ...xaiTimestamp) *int64 {
	for _, t := range ts {
		if t.unix != nil {
			v := *t.unix
			return &v
		}
	}
	return nil
}

func (b *XAIBatch) createdAt() int64 {
	if v := firstTime(b.CreateTime, b.CreatedAt); v != nil {
		return *v
	}
	return 0
}

func (b *XAIBatch) expiresAt() *int64   { return firstTime(b.ExpireTime, b.ExpiresAt) }
func (b *XAIBatch) cancelledAt() *int64 { return firstTime(b.CancelTime, b.CancelledAt) }

// ToBifrostBatchStatus maps xAI's counters to the unified batch status. xAI reports no
// status, so it is derived: a cancelled batch is cancelling while requests are still
// pending; a batch with requests and none pending is completed; a batch past its expiry
// with requests pending is expired; a batch with no requests yet (a file still being read)
// is validating; anything else is in progress.
func (b *XAIBatch) ToBifrostBatchStatus() schemas.BatchStatus {
	if b.cancelledAt() != nil {
		if b.State.NumPending > 0 {
			return schemas.BatchStatusCancelling
		}
		return schemas.BatchStatusCancelled
	}
	if b.State.NumRequests > 0 && b.State.NumPending == 0 {
		return schemas.BatchStatusCompleted
	}
	if exp := b.expiresAt(); exp != nil && nowFunc().Unix() >= *exp {
		return schemas.BatchStatusExpired
	}
	if b.State.NumRequests == 0 {
		return schemas.BatchStatusValidating
	}
	return schemas.BatchStatusInProgress
}

// ToBifrostRequestCounts maps xAI's counters to request_counts. completed and succeeded both
// count the lines that succeeded (OpenAI's completed counts successes only).
func (b *XAIBatch) ToBifrostRequestCounts() schemas.BatchRequestCounts {
	return schemas.BatchRequestCounts{
		Total:     b.State.NumRequests,
		Completed: b.State.NumSuccess,
		Failed:    b.State.NumError,
		Succeeded: b.State.NumSuccess,
		Canceled:  b.State.NumCancelled,
		Pending:   b.State.NumPending,
	}
}

// ToBifrostBatchRetrieveResponse converts an xAI batch to the unified batch object.
func (b *XAIBatch) ToBifrostBatchRetrieveResponse() schemas.BifrostBatchRetrieveResponse {
	status := b.ToBifrostBatchStatus()
	resp := schemas.BifrostBatchRetrieveResponse{
		ID:            b.BatchID,
		Object:        "batch",
		Status:        status,
		RequestCounts: b.ToBifrostRequestCounts(),
		Metadata:      decodeXAIBatchMetadata(b.Name),
		CreatedAt:     b.createdAt(),
		ExpiresAt:     b.expiresAt(),
	}
	if b.Name != "" {
		resp.DisplayName = schemas.Ptr(b.Name)
	}
	if at := b.cancelledAt(); at != nil {
		resp.CancellingAt = at
		if status == schemas.BatchStatusCancelled {
			resp.CancelledAt = at
		}
	}
	if status == schemas.BatchStatusExpired {
		resp.ExpiredAt = b.expiresAt()
	}
	return resp
}

// encodeXAIBatchName chooses the batch name. xAI has no metadata field, and the name is
// what its list shows, so the batch's metadata travels in the name as a query string
// ("tokun_batch_id=batch_123"), the form decodeXAIBatchMetadata reads back (contract C8
// agreement two, C7 recovery method 4). A display name is the name when there is no
// metadata; sending both is refused, since one of them would be lost.
func encodeXAIBatchName(displayName *string, metadata map[string]string) (string, *schemas.BifrostError) {
	hasDisplayName := displayName != nil && strings.TrimSpace(*displayName) != ""
	if len(metadata) > 0 {
		if hasDisplayName {
			return "", schemas.NewUnsupportedParameterError("display_name",
				"xai keeps a batch's metadata in its name, so display_name and metadata cannot both be sent")
		}
		values := url.Values{}
		for k, v := range metadata {
			if k == "" {
				return "", providerUtils.NewBifrostBadRequestError("metadata keys must not be empty")
			}
			values.Set(k, v)
		}
		return values.Encode(), nil
	}
	if hasDisplayName {
		return *displayName, nil
	}
	return xaiDefaultBatchName, nil
}

// decodeXAIBatchMetadata reads back the metadata encodeXAIBatchName wrote into a name. A
// name that is not a query string of single-valued, non-empty keys carries no metadata.
func decodeXAIBatchMetadata(name string) map[string]string {
	if !strings.Contains(name, "=") {
		return nil
	}
	values, err := url.ParseQuery(name)
	if err != nil || len(values) == 0 {
		return nil
	}
	metadata := make(map[string]string, len(values))
	for k, v := range values {
		if k == "" || len(v) != 1 {
			return nil
		}
		metadata[k] = v[0]
	}
	return metadata
}

// ToXAIBatchRequests converts inline OpenAI-shape lines into xAI batch requests. Each chat
// line's body goes through the converter the sync chat route uses (openai.ToOpenAIChatRequest
// with provider xai), so a line reaches xAI with the parameters it would have on the sync
// route. batch_request_id is the line's custom_id. Only /v1/chat/completions lines are
// converted; any other url is refused as an unsupported parameter.
//
// A batch runs one model, batchModel, as Gemini and Vertex batches do: a line naming another
// model (after the key's aliases, resolve) is refused, and every line runs as batchModel.
// Governance picks the batch's provider key from the grants for batchModel, so a line that
// ran another model would run it on a key that may not be granted for it.
func ToXAIBatchRequests(ctx *schemas.BifrostContext, endpoint schemas.BatchEndpoint, batchModel string, resolve func(string) string, items []schemas.BatchRequestItem) ([]XAIBatchRequest, *schemas.BifrostError) {
	out := make([]XAIBatchRequest, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	for i, item := range items {
		if strings.TrimSpace(item.CustomID) == "" {
			return nil, providerUtils.NewBifrostBadRequestError(fmt.Sprintf("requests[%d].custom_id is required", i))
		}
		if _, dup := seen[item.CustomID]; dup {
			return nil, providerUtils.NewBifrostBadRequestError(fmt.Sprintf("requests[%d].custom_id %q is not unique", i, item.CustomID))
		}
		seen[item.CustomID] = struct{}{}
		lineURL := item.URL
		if lineURL == "" {
			lineURL = string(endpoint)
		}
		if lineURL != string(schemas.BatchEndpointChatCompletions) {
			return nil, schemas.NewUnsupportedParameterError("url",
				fmt.Sprintf("xai batches take %s lines; requests[%d].url is %q", schemas.BatchEndpointChatCompletions, i, lineURL))
		}
		if item.Method != "" && !strings.EqualFold(item.Method, http.MethodPost) {
			return nil, providerUtils.NewBifrostBadRequestError(fmt.Sprintf("requests[%d].method must be POST", i))
		}
		if len(item.RequestBody()) == 0 {
			return nil, providerUtils.NewBifrostBadRequestError(fmt.Sprintf("requests[%d].body is required", i))
		}
		if err := gemini.CheckBatchLineModel(item, schemas.XAI, batchModel, resolve); err != nil {
			bifrostErr := providerUtils.NewBifrostBadRequestError(fmt.Sprintf("requests[%d].body.model: %v", i, err))
			bifrostErr.Error.Param = fmt.Sprintf("requests[%d].body.model", i)
			return nil, bifrostErr
		}
		body, err := toXAIBatchChatBody(ctx, item, batchModel)
		if err != nil {
			return nil, providerUtils.NewBifrostBadRequestError(fmt.Sprintf("requests[%d].body: %v", i, err))
		}
		out = append(out, XAIBatchRequest{
			BatchRequestID: item.CustomID,
			BatchRequest:   map[string]json.RawMessage{xaiBatchChatKey: body},
		})
	}
	return out, nil
}

// toXAIBatchChatBody runs one chat line through the sync route's conversion: the shared
// line decoder (schemas.BatchRequestItem.ToBifrostChatRequest: messages, chat parameters,
// every unmodelled field as an extra param), the OpenAI converter with provider xai, and
// the sync encoder, which merges the extra params only when passthrough is enabled, as
// the sync route does. The line runs as the batch's model (see ToXAIBatchRequests).
func toXAIBatchChatBody(ctx *schemas.BifrostContext, item schemas.BatchRequestItem, model string) (json.RawMessage, error) {
	bifrostReq, err := item.ToBifrostChatRequest(schemas.XAI, model)
	if err != nil {
		return nil, err
	}
	xaiReq := openai.ToOpenAIChatRequest(ctx, bifrostReq)
	if xaiReq == nil {
		return nil, fmt.Errorf("messages is required")
	}
	mergeExtraParams := ctx != nil && ctx.Value(schemas.BifrostContextKeyPassthroughExtraParams) == true
	converted, err := providerUtils.MarshalConvertedRequestBody(xaiReq, mergeExtraParams)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(converted), nil
}

// ToBifrostBatchResultItem converts one xAI result line to the unified shape: custom_id is
// the batch_request_id, a chat response becomes an OpenAI chat body whose usage is the sync
// route's unified usage plus vendor_cost in usd_ticks, and a failed line carries the
// vendor's error.
func (r *XAIBatchResult) ToBifrostBatchResultItem() schemas.BatchResultItem {
	item := schemas.BatchResultItem{CustomID: r.BatchRequestID}
	status := normalizeXAIResultState(r.State)

	var response map[string]json.RawMessage
	resultErr := r.Error
	if r.BatchResult != nil {
		response = r.BatchResult.Response
		if resultErr == nil {
			resultErr = r.BatchResult.Error
		}
	}

	if status == "" || status == schemas.BatchResultStatusSucceeded {
		if body, ok := xaiResultBody(response); ok {
			item.Status = schemas.BatchResultStatusSucceeded
			item.Response = &schemas.BatchResultResponse{StatusCode: fasthttp.StatusOK, Body: body}
			return item
		}
		status = schemas.BatchResultStatusFailed
	}

	item.Status = status
	item.Error = &schemas.BatchResultError{Message: r.ErrorMessage}
	if resultErr != nil {
		if resultErr.Code != nil {
			item.Error.Code = fmt.Sprint(resultErr.Code)
		}
		if resultErr.Message != "" {
			item.Error.Message = resultErr.Message
		}
	}
	if item.Error.Code == "" && item.Error.Message == "" {
		item.Error.Message = "xai returned no response and no error for this request"
	}
	return item
}

func normalizeXAIResultState(state string) schemas.BatchResultStatus {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "succeeded", "success", "completed":
		return schemas.BatchResultStatusSucceeded
	case "failed", "error", "errored":
		return schemas.BatchResultStatusFailed
	case "cancelled", "canceled":
		return schemas.BatchResultStatusCancelled
	case "expired":
		return schemas.BatchResultStatusExpired
	default:
		return ""
	}
}

// xaiResultBody returns a succeeded line's response.body. A chat completion is decoded the
// way the sync chat route decodes it and normalized the same way; another response type is
// returned as the vendor wrote it.
func xaiResultBody(response map[string]json.RawMessage) (map[string]any, bool) {
	if raw, ok := response[xaiBatchChatKey]; ok && len(raw) > 0 && string(raw) != "null" {
		body, err := toBatchChatBody(raw)
		if err == nil {
			return body, true
		}
	}
	for _, raw := range response {
		if len(raw) == 0 || string(raw) == "null" {
			continue
		}
		var body map[string]any
		if err := sonic.Unmarshal(raw, &body); err == nil && len(body) > 0 {
			return body, true
		}
	}
	return nil, false
}

// toBatchChatBody converts xAI's chat completion to the OpenAI chat body the sync route
// returns: usage normalized by normalizeXAIChatUsage (reasoning tokens folded into
// completion_tokens, cost from cost_in_usd_ticks), plus usage.vendor_cost carrying
// cost_in_usd_ticks unchanged (contract C5).
func toBatchChatBody(raw []byte) (map[string]any, error) {
	var chat schemas.BifrostChatResponse
	if err := sonic.Unmarshal(raw, &chat); err != nil {
		return nil, err
	}
	normalizeXAIChatUsage(&chat)
	encoded, err := providerUtils.MarshalSorted(&chat)
	if err != nil {
		return nil, err
	}
	var body map[string]any
	if err := sonic.Unmarshal(encoded, &body); err != nil {
		return nil, err
	}
	delete(body, "extra_fields")
	if chat.Usage == nil {
		delete(body, "usage")
		return body, nil
	}
	if chat.Usage.CostInUsdTicks != nil {
		if usage, ok := body["usage"].(map[string]any); ok {
			usage["vendor_cost"] = schemas.VendorCost{
				Amount: float64(*chat.Usage.CostInUsdTicks),
				Unit:   schemas.VendorCostUnitUSDTicks,
			}
		}
	}
	return body, nil
}

// xaiResponse is one decoded 2xx response from xAI.
type xaiResponse struct {
	body    []byte
	headers map[string]string
	latency time.Duration
}

// do sends one request to xAI with key's bearer token. A non-2xx answer is the vendor's
// error, parsed by ParseXAIError.
func (provider *XAIProvider) do(ctx *schemas.BifrostContext, key schemas.Key, method, path string, query url.Values, body []byte) (*xaiResponse, *schemas.BifrostError) {
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	requestURL := provider.networkConfig.BaseURL + path
	if encoded := query.Encode(); encoded != "" {
		requestURL += "?" + encoded
	}
	providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)
	req.SetRequestURI(requestURL)
	req.Header.SetMethod(method)
	if body != nil {
		req.Header.SetContentType("application/json")
		req.SetBody(body)
	}
	if value := key.Value.GetValue(); value != "" {
		req.Header.Set("Authorization", "Bearer "+value)
	}

	sendBackRawRequest := providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest)
	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)

	latency, bifrostErr, wait := providerUtils.MakeRequestWithContext(ctx, provider.client, req, resp)
	defer wait()
	if bifrostErr != nil {
		return nil, providerUtils.EnrichError(ctx, bifrostErr, body, nil, sendBackRawRequest, sendBackRawResponse, latency)
	}
	if resp.StatusCode() < 200 || resp.StatusCode() >= 300 {
		provider.logger.Debug("error from %s provider: status %d", provider.GetProviderKey(), resp.StatusCode())
		return nil, providerUtils.EnrichError(ctx, ParseXAIError(resp), body, nil, sendBackRawRequest, sendBackRawResponse, latency)
	}
	decoded, err := providerUtils.CheckAndDecodeBody(resp)
	if err != nil {
		return nil, providerUtils.EnrichError(ctx, providerUtils.NewBifrostOperationError(schemas.ErrProviderResponseDecode, err), body, nil, sendBackRawRequest, sendBackRawResponse, latency)
	}
	return &xaiResponse{
		body:    append([]byte(nil), decoded...),
		headers: providerUtils.ExtractProviderResponseHeaders(resp),
		latency: latency,
	}, nil
}

// decode parses an xAI response into out and fills the extra fields every batch and file
// response carries: latency, response headers and, when asked for, the raw request and
// response.
func decodeXAI[T any](ctx *schemas.BifrostContext, provider *XAIProvider, resp *xaiResponse, requestBody []byte, out *T) (schemas.BifrostResponseExtraFields, *schemas.BifrostError) {
	sendBackRawRequest := providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest)
	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)
	rawRequest, rawResponse, bifrostErr := providerUtils.HandleProviderResponse(resp.body, out, requestBody, sendBackRawRequest, sendBackRawResponse)
	if bifrostErr != nil {
		return schemas.BifrostResponseExtraFields{}, providerUtils.EnrichError(ctx, bifrostErr, requestBody, resp.body, sendBackRawRequest, sendBackRawResponse, resp.latency)
	}
	extra := schemas.BifrostResponseExtraFields{
		Latency:                 resp.latency.Milliseconds(),
		ProviderResponseHeaders: resp.headers,
	}
	if sendBackRawRequest {
		extra.RawRequest = rawRequest
	}
	if sendBackRawResponse {
		extra.RawResponse = rawResponse
	}
	return extra, nil
}

// eachKey runs call with each key in turn and returns the first success, else the last
// error. When the caller names a key (x-bf-api-key), keys holds only that key, so the
// operation is one call on that key (contract C3).
func eachKey[T any](keys []schemas.Key, call func(schemas.Key) (*T, *schemas.BifrostError)) (*T, *schemas.BifrostError) {
	if len(keys) == 0 {
		return nil, providerUtils.NewConfigurationError("no keys available for this xai operation")
	}
	var lastErr *schemas.BifrostError
	for _, key := range keys {
		out, err := call(key)
		if err == nil {
			return out, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// BatchCreate creates an xAI batch. With inline requests it is two calls: create the batch
// by name, then add every line in one POST /v1/batches/{id}/requests. With input_file_id it
// is one call naming the uploaded JSONL file. When adding the requests fails, the vendor's
// error is returned and names the empty batch that was created; Bifrost does not cancel it.
func (provider *XAIProvider) BatchCreate(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostBatchCreateRequest) (*schemas.BifrostBatchCreateResponse, *schemas.BifrostError) {
	if err := providerUtils.CheckOperationAllowed(schemas.XAI, provider.customProviderConfig, schemas.BatchCreateRequest); err != nil {
		return nil, err
	}
	if request == nil {
		return nil, providerUtils.NewBifrostBadRequestError("batch create request is required")
	}
	if request.InputBlob != nil && strings.TrimSpace(*request.InputBlob) != "" {
		return nil, schemas.NewUnsupportedParameterError("input_blob", "")
	}
	if request.OutputFolder != nil {
		return nil, schemas.NewUnsupportedParameterError("output_folder", "")
	}
	if request.OutputExpiresAfter != nil {
		return nil, schemas.NewUnsupportedParameterError("output_expires_after", "")
	}
	if request.InputFileID != "" && len(request.Requests) > 0 {
		return nil, providerUtils.NewBifrostBadRequestError("input_file_id and requests are exclusive")
	}
	if request.InputFileID == "" && len(request.Requests) == 0 {
		return nil, providerUtils.NewBifrostBadRequestError("either input_file_id or requests is required for xai batches")
	}

	name, nameErr := encodeXAIBatchName(request.DisplayName, request.Metadata)
	if nameErr != nil {
		return nil, nameErr
	}

	// Convert every line before the first call, so a bad line costs no upstream call.
	var addBody []byte
	if len(request.Requests) > 0 {
		batchModel := ""
		if request.Model != nil {
			_, batchModel = schemas.ParseModelString(strings.TrimSpace(*request.Model), schemas.XAI)
		}
		if batchModel == "" {
			bifrostErr := providerUtils.NewBifrostBadRequestError("model is required for an inline xai batch: the batch runs one model, named in the request's model field")
			bifrostErr.Error.Param = "model"
			return nil, bifrostErr
		}
		lines, convErr := ToXAIBatchRequests(ctx, request.Endpoint, batchModel, key.Aliases.Resolve, request.Requests)
		if convErr != nil {
			return nil, convErr
		}
		encoded, err := providerUtils.MarshalSorted(&XAIBatchAddRequestsRequest{BatchRequests: lines})
		if err != nil {
			return nil, providerUtils.NewBifrostOperationError(schemas.ErrProviderRequestMarshal, err)
		}
		addBody = encoded
	}

	createBody, err := providerUtils.MarshalSorted(&XAIBatchCreateRequest{Name: name, InputFileID: request.InputFileID})
	if err != nil {
		return nil, providerUtils.NewBifrostOperationError(schemas.ErrProviderRequestMarshal, err)
	}
	created, bifrostErr := provider.do(ctx, key, http.MethodPost, "/v1/batches", nil, createBody)
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	var batch XAIBatch
	extra, bifrostErr := decodeXAI(ctx, provider, created, createBody, &batch)
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	if batch.BatchID == "" {
		return nil, providerUtils.NewBifrostOperationError("xai created a batch but returned no batch_id", nil)
	}

	if addBody != nil {
		escapedID, idErr := providerUtils.EscapeResourceID(batch.BatchID, "batch_id")
		if idErr != nil {
			return nil, idErr
		}
		added, addErr := provider.do(ctx, key, http.MethodPost, "/v1/batches/"+escapedID+"/requests", nil, addBody)
		if addErr != nil {
			if addErr.Error != nil {
				addErr.Error.Message = fmt.Sprintf("xai created batch %s but refused its requests: %s", batch.BatchID, addErr.Error.Message)
			}
			return nil, addErr
		}
		extra.Latency += added.latency.Milliseconds()
		// The batch object returned by create predates the requests; report the lines as
		// pending rather than echo an empty batch as validating.
		batch.State.NumRequests += len(request.Requests)
		batch.State.NumPending += len(request.Requests)
	}

	unified := batch.ToBifrostBatchRetrieveResponse()
	return &schemas.BifrostBatchCreateResponse{
		ID:               unified.ID,
		Object:           unified.Object,
		DisplayName:      unified.DisplayName,
		Endpoint:         string(request.Endpoint),
		InputFileID:      request.InputFileID,
		CompletionWindow: request.CompletionWindow,
		Status:           unified.Status,
		RequestCounts:    unified.RequestCounts,
		Metadata:         unified.Metadata,
		CreatedAt:        unified.CreatedAt,
		ExpiresAt:        unified.ExpiresAt,
		ExtraFields:      extra,
	}, nil
}

// BatchList lists the team's batches, paged by xAI's pagination_token.
func (provider *XAIProvider) BatchList(ctx *schemas.BifrostContext, keys []schemas.Key, request *schemas.BifrostBatchListRequest) (*schemas.BifrostBatchListResponse, *schemas.BifrostError) {
	if err := providerUtils.CheckOperationAllowed(schemas.XAI, provider.customProviderConfig, schemas.BatchListRequest); err != nil {
		return nil, err
	}
	if request == nil {
		request = &schemas.BifrostBatchListRequest{}
	}
	helper, err := providerUtils.NewSerialListHelper(keys, request.After, provider.logger, true)
	if err != nil {
		return nil, providerUtils.NewBifrostBadRequestError("invalid pagination cursor: " + err.Error())
	}
	key, nativeCursor, ok := helper.GetCurrentKey()
	if !ok {
		return &schemas.BifrostBatchListResponse{Object: "list", Data: []schemas.BifrostBatchRetrieveResponse{}}, nil
	}
	query := url.Values{}
	if request.Limit > 0 {
		query.Set("limit", strconv.Itoa(request.Limit))
	}
	if nativeCursor != "" {
		query.Set("pagination_token", nativeCursor)
	}
	resp, bifrostErr := provider.do(ctx, key, http.MethodGet, "/v1/batches", query, nil)
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	var page XAIBatchListResponse
	extra, bifrostErr := decodeXAI(ctx, provider, resp, nil, &page)
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	data := make([]schemas.BifrostBatchRetrieveResponse, 0, len(page.Batches))
	for i := range page.Batches {
		data = append(data, page.Batches[i].ToBifrostBatchRetrieveResponse())
	}
	out := &schemas.BifrostBatchListResponse{Object: "list", Data: data, ExtraFields: extra}
	if len(data) > 0 {
		out.FirstID = schemas.Ptr(data[0].ID)
		out.LastID = schemas.Ptr(data[len(data)-1].ID)
	}
	next, hasMore := helper.BuildNextCursor(page.PaginationToken != "", page.PaginationToken)
	out.HasMore = hasMore
	if next != "" {
		out.NextCursor = schemas.Ptr(next)
	}
	return out, nil
}

// BatchRetrieve reads one batch.
func (provider *XAIProvider) BatchRetrieve(ctx *schemas.BifrostContext, keys []schemas.Key, request *schemas.BifrostBatchRetrieveRequest) (*schemas.BifrostBatchRetrieveResponse, *schemas.BifrostError) {
	if err := providerUtils.CheckOperationAllowed(schemas.XAI, provider.customProviderConfig, schemas.BatchRetrieveRequest); err != nil {
		return nil, err
	}
	if request == nil || request.BatchID == "" {
		return nil, providerUtils.NewBifrostBadRequestError("batch_id is required")
	}
	escapedID, idErr := providerUtils.EscapeResourceID(request.BatchID, "batch_id")
	if idErr != nil {
		return nil, idErr
	}
	return eachKey(keys, func(key schemas.Key) (*schemas.BifrostBatchRetrieveResponse, *schemas.BifrostError) {
		resp, bifrostErr := provider.do(ctx, key, http.MethodGet, "/v1/batches/"+escapedID, nil, nil)
		if bifrostErr != nil {
			return nil, bifrostErr
		}
		var batch XAIBatch
		extra, bifrostErr := decodeXAI(ctx, provider, resp, nil, &batch)
		if bifrostErr != nil {
			return nil, bifrostErr
		}
		out := batch.ToBifrostBatchRetrieveResponse()
		out.ExtraFields = extra
		return &out, nil
	})
}

// BatchCancel cancels a batch with POST /v1/batches/{id}:cancel. A refusal (400, 409, 412
// or 422 from the cancel call) keeps the vendor's status and body and is marked
// provider_cancel_unsupported (contract C6).
func (provider *XAIProvider) BatchCancel(ctx *schemas.BifrostContext, keys []schemas.Key, request *schemas.BifrostBatchCancelRequest) (*schemas.BifrostBatchCancelResponse, *schemas.BifrostError) {
	if err := providerUtils.CheckOperationAllowed(schemas.XAI, provider.customProviderConfig, schemas.BatchCancelRequest); err != nil {
		return nil, err
	}
	if request == nil || request.BatchID == "" {
		return nil, providerUtils.NewBifrostBadRequestError("batch_id is required")
	}
	escapedID, idErr := providerUtils.EscapeResourceID(request.BatchID, "batch_id")
	if idErr != nil {
		return nil, idErr
	}
	return eachKey(keys, func(key schemas.Key) (*schemas.BifrostBatchCancelResponse, *schemas.BifrostError) {
		resp, bifrostErr := provider.do(ctx, key, http.MethodPost, "/v1/batches/"+escapedID+":cancel", nil, nil)
		if bifrostErr != nil {
			markCancelRefusal(bifrostErr)
			return nil, bifrostErr
		}
		var batch XAIBatch
		extra, bifrostErr := decodeXAI(ctx, provider, resp, nil, &batch)
		if bifrostErr != nil {
			return nil, bifrostErr
		}
		unified := batch.ToBifrostBatchRetrieveResponse()
		return &schemas.BifrostBatchCancelResponse{
			ID:            unified.ID,
			Object:        unified.Object,
			Status:        unified.Status,
			RequestCounts: unified.RequestCounts,
			CancellingAt:  unified.CancellingAt,
			CancelledAt:   unified.CancelledAt,
			ExtraFields:   extra,
		}, nil
	})
}

// markCancelRefusal marks a vendor's refusal of a cancel as provider_cancel_unsupported.
func markCancelRefusal(err *schemas.BifrostError) {
	if err == nil || err.IsBifrostError || err.StatusCode == nil {
		return
	}
	switch *err.StatusCode {
	case fasthttp.StatusBadRequest, fasthttp.StatusConflict, fasthttp.StatusPreconditionFailed, fasthttp.StatusUnprocessableEntity:
		err.ExtraFields.ErrorType = schemas.ErrorTypeProviderCancelUnsupported
	}
}

// BatchDelete is not supported: xAI has no batch delete; batches are kept until they expire.
func (provider *XAIProvider) BatchDelete(_ *schemas.BifrostContext, _ []schemas.Key, _ *schemas.BifrostBatchDeleteRequest) (*schemas.BifrostBatchDeleteResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.BatchDeleteRequest, provider.GetProviderKey())
}

// BatchResults returns one page of a batch's results from /v1/batches/{id}/results. after
// is xAI's pagination_token and limit its page size; has_more and next_cursor come from the
// pagination_token the page returns. Results are available while the batch runs.
func (provider *XAIProvider) BatchResults(ctx *schemas.BifrostContext, keys []schemas.Key, request *schemas.BifrostBatchResultsRequest) (*schemas.BifrostBatchResultsResponse, *schemas.BifrostError) {
	if err := providerUtils.CheckOperationAllowed(schemas.XAI, provider.customProviderConfig, schemas.BatchResultsRequest); err != nil {
		return nil, err
	}
	if request == nil || request.BatchID == "" {
		return nil, providerUtils.NewBifrostBadRequestError("batch_id is required")
	}
	escapedID, idErr := providerUtils.EscapeResourceID(request.BatchID, "batch_id")
	if idErr != nil {
		return nil, idErr
	}
	query := url.Values{}
	if request.Limit > 0 {
		query.Set("limit", strconv.Itoa(request.Limit))
	}
	if request.After != nil && *request.After != "" {
		query.Set("pagination_token", *request.After)
	}
	return eachKey(keys, func(key schemas.Key) (*schemas.BifrostBatchResultsResponse, *schemas.BifrostError) {
		resp, bifrostErr := provider.do(ctx, key, http.MethodGet, "/v1/batches/"+escapedID+"/results", query, nil)
		if bifrostErr != nil {
			return nil, bifrostErr
		}
		var page XAIBatchResultsResponse
		extra, bifrostErr := decodeXAI(ctx, provider, resp, nil, &page)
		if bifrostErr != nil {
			return nil, bifrostErr
		}
		results := make([]schemas.BatchResultItem, 0, len(page.Results))
		for i := range page.Results {
			results = append(results, page.Results[i].ToBifrostBatchResultItem())
		}
		out := &schemas.BifrostBatchResultsResponse{
			BatchID:     request.BatchID,
			Endpoint:    schemas.BatchEndpointChatCompletions,
			Results:     results,
			HasMore:     page.PaginationToken != "",
			ExtraFields: extra,
		}
		if page.PaginationToken != "" {
			out.NextCursor = schemas.Ptr(page.PaginationToken)
		}
		return out, nil
	})
}
