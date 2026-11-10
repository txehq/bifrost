// Package kling implements Kuaishou's Kling video generation on the Kling international site.
//
// It speaks the API on Kling's new design standard (https://kling.ai/document-api, read
// 2026-11-11): Bearer API key, one create path per model and input kind
// (/text-to-video/{model}, /image-to-video/{model}, /omni-video/{model}), and one task object
// queried by the task id or by the caller's own id (GET /tasks?task_ids= / ?external_task_ids=)
// or listed by cursor (POST /tasks). The legacy API (/v1/videos/..., access key and secret key
// signed into a JWT) is not implemented.
//
// Every operation is one call to Kling: nothing is retried, polled or stored here. Kling has no
// cancel API, so the provider does not implement schemas.VideoCanceller and core answers the
// cancel route with provider_cancel_unsupported.
package kling

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	schemas "github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

// klingDefaultBaseURL is the API host of the Kling international site, for servers outside
// mainland China.
const klingDefaultBaseURL = "https://api-singapore.klingai.com"

// klingTasksPath serves both queries: GET by id, POST by cursor.
const klingTasksPath = "/tasks"

// The two query parameters of GET /tasks: Kling's task id, or the id the caller gave the task.
const (
	klingTaskIDsParam         = "task_ids"
	klingExternalTaskIDsParam = "external_task_ids"
)

// klingListMaxLimit is the largest page POST /tasks returns.
const klingListMaxLimit = 500

// KlingProvider implements the Provider interface for Kling video generation.
type KlingProvider struct {
	logger               schemas.Logger
	client               *fasthttp.Client
	customProviderConfig *schemas.CustomProviderConfig // allowed_requests and request_path_overrides of a custom provider built on kling
	networkConfig        schemas.NetworkConfig
	sendBackRawRequest   bool
	sendBackRawResponse  bool
}

// NewKlingProvider creates a Kling provider. Every Kling call is a short JSON request, so the
// provider holds only the unary client.
func NewKlingProvider(config *schemas.ProviderConfig, logger schemas.Logger) (*KlingProvider, error) {
	config.CheckAndSetDefaults()

	requestTimeout := time.Second * time.Duration(config.NetworkConfig.DefaultRequestTimeoutInSeconds)
	client := &fasthttp.Client{
		ReadTimeout:         requestTimeout,
		WriteTimeout:        requestTimeout,
		MaxConnsPerHost:     config.NetworkConfig.MaxConnsPerHost,
		MaxIdleConnDuration: time.Second * time.Duration(config.NetworkConfig.KeepAliveTimeoutInSeconds),
		MaxConnWaitTimeout:  requestTimeout,
		MaxConnDuration:     time.Second * time.Duration(schemas.DefaultMaxConnDurationInSeconds),
		ConnPoolStrategy:    fasthttp.FIFO,
	}
	client = providerUtils.ConfigureProxy(client, config.ProxyConfig, logger)
	client = providerUtils.ConfigureDialer(client, config.NetworkConfig.AllowPrivateNetwork)
	client = providerUtils.ConfigureTLS(client, config.NetworkConfig, logger)

	if config.NetworkConfig.BaseURL == "" {
		config.NetworkConfig.BaseURL = klingDefaultBaseURL
	}
	config.NetworkConfig.BaseURL = strings.TrimRight(config.NetworkConfig.BaseURL, "/")

	return &KlingProvider{
		logger:               logger,
		client:               client,
		customProviderConfig: config.CustomProviderConfig,
		networkConfig:        config.NetworkConfig,
		sendBackRawRequest:   config.SendBackRawRequest,
		sendBackRawResponse:  config.SendBackRawResponse,
	}, nil
}

// GetProviderKey returns kling, or the custom provider's own name when one is built on it.
func (provider *KlingProvider) GetProviderKey() schemas.ModelProvider {
	return providerUtils.GetProviderName(schemas.Kling, provider.customProviderConfig)
}

// SupportedUnifiedFields declares the unified fields and task control fields video create
// delivers, so core refuses any other with caller_unsupported_parameter before a key is chosen
// (contract C4, C7).
func (provider *KlingProvider) SupportedUnifiedFields(requestType schemas.RequestType) ([]string, bool) {
	if requestType == schemas.VideoGenerationRequest {
		return klingVideoGenerationFields, true
	}
	return nil, false
}

// VideoGeneration creates a Kling video task with one call to the model's create path and returns
// the task object, usually queued. The caller's client_reference_id is sent as
// options.external_task_id and its callback_url as options.callback_url, both unchanged.
func (provider *KlingProvider) VideoGeneration(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostVideoGenerationRequest) (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError) {
	if bifrostErr := providerUtils.CheckOperationAllowed(schemas.Kling, provider.customProviderConfig, schemas.VideoGenerationRequest); bifrostErr != nil {
		return nil, bifrostErr
	}
	endpoint, bifrostErr := klingEndpointFor(request)
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	if bifrostErr := validateKlingVideoRequest(request, endpoint); bifrostErr != nil {
		return nil, bifrostErr
	}
	jsonData, bifrostErr := providerUtils.CheckContextAndGetRequestBody(ctx, request,
		func() (providerUtils.RequestBodyWithExtraParams, error) {
			return toKlingVideoRequest(request, endpoint)
		})
	if bifrostErr != nil {
		return nil, bifrostErr
	}

	sendBackRawRequest := providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest)
	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)

	requestURL := provider.requestURL(ctx, endpoint.path, schemas.VideoGenerationRequest)
	body, latency, bifrostErr := provider.do(ctx, key, fasthttp.MethodPost, requestURL, jsonData, sendBackRawRequest, sendBackRawResponse)
	if bifrostErr != nil {
		return nil, bifrostErr
	}

	var createResp KlingCreateResponse
	rawRequest, rawResponse, bifrostErr := providerUtils.HandleProviderResponseCtx(ctx, body, &createResp, jsonData, sendBackRawRequest, sendBackRawResponse)
	if bifrostErr != nil {
		return nil, providerUtils.SetErrorLatency(bifrostErr, latency)
	}
	if bifrostErr := klingEnvelopeError(&createResp.KlingEnvelope); bifrostErr != nil {
		return nil, providerUtils.EnrichError(ctx, bifrostErr, jsonData, body, sendBackRawRequest, sendBackRawResponse, latency)
	}
	if createResp.Data == nil || createResp.Data.ID == "" {
		return nil, providerUtils.EnrichError(ctx, schemas.NewProviderEmptyResultError("kling answered the create call with no task id"), jsonData, body, sendBackRawRequest, sendBackRawResponse, latency)
	}

	bifrostResp, bifrostErr := ToBifrostVideoGenerationResponse(createResp.Data)
	if bifrostErr != nil {
		return nil, providerUtils.EnrichError(ctx, bifrostErr, jsonData, body, sendBackRawRequest, sendBackRawResponse, latency)
	}
	bifrostResp.ID = providerUtils.AddVideoIDProviderSuffix(bifrostResp.ID, provider.GetProviderKey())
	bifrostResp.Model = request.Model
	bifrostResp.ExtraFields.Latency = latency.Milliseconds()
	if sendBackRawRequest {
		bifrostResp.ExtraFields.RawRequest = rawRequest
	}
	if sendBackRawResponse {
		bifrostResp.ExtraFields.RawResponse = rawResponse
	}
	return bifrostResp, nil
}

// VideoRetrieve queries a task by Kling's task id with one call: GET /tasks?task_ids=.
func (provider *KlingProvider) VideoRetrieve(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostVideoRetrieveRequest) (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError) {
	if request.IDType == schemas.VideoIDTypeClientReference {
		return provider.VideoRetrieveByClientReferenceID(ctx, key, request)
	}
	return provider.queryTask(ctx, key, klingTaskIDsParam, "task id", request.ID)
}

// VideoRetrieveByClientReferenceID queries a task by the client_reference_id it was created with
// (Kling's external_task_id) with one call: GET /tasks?external_task_ids=. It returns the same
// task object as VideoRetrieve, whose id is Kling's task id. It implements
// schemas.VideoClientReferenceRetriever.
func (provider *KlingProvider) VideoRetrieveByClientReferenceID(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostVideoRetrieveRequest) (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError) {
	return provider.queryTask(ctx, key, klingExternalTaskIDsParam, "client_reference_id", request.ID)
}

// queryTask is the query by either id. param is Kling's query parameter and idKind names the id
// in error messages. Kling's parameter takes a comma-separated batch; this route queries one
// task, so an id with a comma is refused instead of being read as several.
func (provider *KlingProvider) queryTask(ctx *schemas.BifrostContext, key schemas.Key, param, idKind, rawID string) (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError) {
	if bifrostErr := providerUtils.CheckOperationAllowed(schemas.Kling, provider.customProviderConfig, schemas.VideoRetrieveRequest); bifrostErr != nil {
		return nil, bifrostErr
	}
	providerName := provider.GetProviderKey()
	// A task id arrives as Bifrost returned it, with its provider suffix and URL-escaped. A
	// client reference is the caller's own string and is sent to Kling exactly as given.
	id := rawID
	if param == klingTaskIDsParam {
		id = providerUtils.StripVideoIDProviderSuffix(rawID, providerName)
	}
	if strings.TrimSpace(id) == "" {
		return nil, providerUtils.NewBifrostBadRequestError(fmt.Sprintf("kling: %s is required", idKind))
	}
	if strings.Contains(id, ",") {
		return nil, providerUtils.NewBifrostBadRequestError(fmt.Sprintf("kling: %s must not contain a comma", idKind))
	}

	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)
	requestURL := provider.requestURL(ctx, klingTasksPath, schemas.VideoRetrieveRequest) + "?" + param + "=" + url.QueryEscape(id)
	body, latency, bifrostErr := provider.do(ctx, key, fasthttp.MethodGet, requestURL, nil, false, sendBackRawResponse)
	if bifrostErr != nil {
		return nil, bifrostErr
	}

	var queryResp KlingQueryResponse
	_, rawResponse, bifrostErr := providerUtils.HandleProviderResponseCtx(ctx, body, &queryResp, nil, false, sendBackRawResponse)
	if bifrostErr != nil {
		return nil, providerUtils.SetErrorLatency(bifrostErr, latency)
	}
	if bifrostErr := klingEnvelopeError(&queryResp.KlingEnvelope); bifrostErr != nil {
		return nil, providerUtils.EnrichError(ctx, bifrostErr, nil, body, false, sendBackRawResponse, latency)
	}
	task := klingTaskFor(queryResp.Data, param, id)
	if task == nil {
		return nil, providerUtils.EnrichError(ctx, newKlingTaskNotFoundError(idKind, id), nil, body, false, sendBackRawResponse, latency)
	}

	bifrostResp, bifrostErr := ToBifrostVideoGenerationResponse(task)
	if bifrostErr != nil {
		return nil, providerUtils.EnrichError(ctx, bifrostErr, nil, body, false, sendBackRawResponse, latency)
	}
	bifrostResp.ID = providerUtils.AddVideoIDProviderSuffix(bifrostResp.ID, providerName)
	bifrostResp.ExtraFields.Latency = latency.Milliseconds()
	if sendBackRawResponse {
		bifrostResp.ExtraFields.RawResponse = rawResponse
	}
	return bifrostResp, nil
}

// klingTaskFor picks the queried task out of a query answer: the entry whose id (or external id,
// for a query by external_task_ids) is the one asked for, else the only entry. Nil when Kling
// returned no task.
func klingTaskFor(tasks []KlingTask, param, id string) *KlingTask {
	for i := range tasks {
		if param == klingExternalTaskIDsParam && tasks[i].ExternalID == id {
			return &tasks[i]
		}
		if param == klingTaskIDsParam && tasks[i].ID == id {
			return &tasks[i]
		}
	}
	if len(tasks) == 1 && tasks[0].ID != "" {
		return &tasks[0]
	}
	return nil
}

// VideoList lists the account's video tasks with one call: POST /tasks, Kling's query by cursor.
// after is Kling's cursor and limit its page size; the product_type filter keeps the page to
// video tasks. The response's last_id is the cursor of the next page while has_more is true, so a
// caller pages with after=last_id as on every other provider. Kling orders the page itself and
// has no order parameter, so order is refused.
func (provider *KlingProvider) VideoList(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostVideoListRequest) (*schemas.BifrostVideoListResponse, *schemas.BifrostError) {
	if bifrostErr := providerUtils.CheckOperationAllowed(schemas.Kling, provider.customProviderConfig, schemas.VideoListRequest); bifrostErr != nil {
		return nil, bifrostErr
	}
	if request.Order != nil && *request.Order != "" {
		return nil, schemas.NewUnsupportedParameterError("order", "kling: the task list has no order parameter")
	}
	listReq := &KlingListRequest{
		Filters: []KlingListFilter{{Key: "product_type", Values: []string{klingOutputVideo}}},
	}
	if request.After != nil {
		listReq.Cursor = *request.After
	}
	if request.Limit != nil {
		if *request.Limit < 1 || *request.Limit > klingListMaxLimit {
			return nil, schemas.NewUnsupportedParameterError("limit", fmt.Sprintf(
				"kling: limit %d is outside 1 to %d", *request.Limit, klingListMaxLimit))
		}
		listReq.Limit = request.Limit
	}
	jsonData, err := providerUtils.MarshalProviderRequest(listReq)
	if err != nil {
		return nil, providerUtils.NewBifrostOperationError(schemas.ErrProviderRequestMarshal, err)
	}

	sendBackRawRequest := providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest)
	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)
	requestURL := provider.requestURL(ctx, klingTasksPath, schemas.VideoListRequest)
	body, latency, bifrostErr := provider.do(ctx, key, fasthttp.MethodPost, requestURL, jsonData, sendBackRawRequest, sendBackRawResponse)
	if bifrostErr != nil {
		return nil, bifrostErr
	}

	var listResp KlingListResponse
	rawRequest, rawResponse, bifrostErr := providerUtils.HandleProviderResponseCtx(ctx, body, &listResp, jsonData, sendBackRawRequest, sendBackRawResponse)
	if bifrostErr != nil {
		return nil, providerUtils.SetErrorLatency(bifrostErr, latency)
	}
	if bifrostErr := klingEnvelopeError(&listResp.KlingEnvelope); bifrostErr != nil {
		return nil, providerUtils.EnrichError(ctx, bifrostErr, jsonData, body, sendBackRawRequest, sendBackRawResponse, latency)
	}

	providerName := provider.GetProviderKey()
	bifrostResp := &schemas.BifrostVideoListResponse{Object: "list", Data: []schemas.VideoObject{}}
	hasMore := false
	if page := listResp.Data; page != nil {
		for i := range page.Result {
			bifrostResp.Data = append(bifrostResp.Data, ToBifrostVideoObject(&page.Result[i], providerName))
		}
		hasMore = page.HasMore && page.NextCursor != ""
		if n := len(bifrostResp.Data); n > 0 {
			bifrostResp.FirstID = schemas.Ptr(bifrostResp.Data[0].ID)
			bifrostResp.LastID = schemas.Ptr(bifrostResp.Data[n-1].ID)
		}
		if hasMore {
			bifrostResp.LastID = schemas.Ptr(page.NextCursor)
		}
	}
	bifrostResp.HasMore = schemas.Ptr(hasMore)
	bifrostResp.ExtraFields.Latency = latency.Milliseconds()
	if sendBackRawRequest {
		bifrostResp.ExtraFields.RawRequest = rawRequest
	}
	if sendBackRawResponse {
		bifrostResp.ExtraFields.RawResponse = rawResponse
	}
	return bifrostResp, nil
}

// VideoDownload fetches the generated video from the link on a completed task: one query to
// Kling, then one download of outputs[].url, which needs no key.
func (provider *KlingProvider) VideoDownload(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostVideoDownloadRequest) (*schemas.BifrostVideoDownloadResponse, *schemas.BifrostError) {
	if bifrostErr := providerUtils.CheckOperationAllowed(schemas.Kling, provider.customProviderConfig, schemas.VideoDownloadRequest); bifrostErr != nil {
		return nil, bifrostErr
	}
	task, bifrostErr := provider.queryTask(ctx, key, klingTaskIDsParam, "task id", request.ID)
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	if task.Status != schemas.VideoStatusCompleted {
		return nil, providerUtils.NewBifrostBadRequestError(fmt.Sprintf("video not ready, current status: %s", task.Status))
	}
	if len(task.Videos) == 0 || task.Videos[0].URL == nil || *task.Videos[0].URL == "" {
		return nil, schemas.NewProviderEmptyResultError("kling reported the task succeeded with no video link")
	}

	// The link comes from Kling's task object, so it is fetched through the SSRF-hardened media
	// fetcher (public addresses only, checked at dial time and on every redirect), not the
	// provider client. No credentials are sent.
	start := time.Now()
	contentType, content, err := providerUtils.FetchMediaURL(ctx, *task.Videos[0].URL)
	latency := time.Since(start)
	if err != nil {
		return nil, providerUtils.SetErrorLatency(providerUtils.NewBifrostOperationError("failed to download video", err), latency)
	}
	bifrostResp := &schemas.BifrostVideoDownloadResponse{
		VideoID:     request.ID,
		Content:     content,
		ContentType: providerUtils.VideoContentTypeOr(contentType, klingVideoContentType),
	}
	bifrostResp.ExtraFields.Latency = latency.Milliseconds()
	return bifrostResp, nil
}

// requestURL is the URL of a Kling path, or the custom provider's request_path_overrides entry
// for the request type when it sets one.
func (provider *KlingProvider) requestURL(ctx *schemas.BifrostContext, defaultPath string, requestType schemas.RequestType) string {
	path, isAbsolute := providerUtils.GetRequestPath(ctx, defaultPath, provider.customProviderConfig, requestType)
	if isAbsolute {
		return path
	}
	return provider.networkConfig.BaseURL + path
}

// do sends one request with Kling's bearer key and returns the decoded body of a 2xx answer. A
// non-2xx answer becomes an error that keeps the vendor's status, service code and body.
func (provider *KlingProvider) do(ctx *schemas.BifrostContext, key schemas.Key, method, requestURL string, jsonData []byte, sendBackRawRequest, sendBackRawResponse bool) ([]byte, time.Duration, *schemas.BifrostError) {
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)
	req.SetRequestURI(requestURL)
	req.Header.SetMethod(method)
	req.Header.SetContentType("application/json")
	if value := key.Value.GetValue(); value != "" {
		req.Header.Set("Authorization", "Bearer "+value)
	}
	if jsonData != nil {
		req.SetBody(jsonData)
	}

	latency, bifrostErr, wait := providerUtils.MakeRequestWithContext(ctx, provider.client, req, resp)
	defer wait()
	if bifrostErr != nil {
		return nil, latency, bifrostErr
	}
	if resp.StatusCode() < fasthttp.StatusOK || resp.StatusCode() >= fasthttp.StatusMultipleChoices {
		var errBody []byte
		if decoded, err := providerUtils.CheckAndDecodeBody(resp); err == nil {
			errBody = append([]byte(nil), decoded...)
		}
		return nil, latency, providerUtils.EnrichError(ctx, parseKlingError(resp), jsonData, errBody, sendBackRawRequest, sendBackRawResponse, latency)
	}

	body, err := providerUtils.CheckAndDecodeBody(resp)
	if err != nil {
		rawErrBody := append([]byte(nil), resp.Body()...)
		return nil, latency, providerUtils.EnrichError(ctx, providerUtils.NewBifrostOperationError(schemas.ErrProviderResponseDecode, err), jsonData, rawErrBody, sendBackRawRequest, sendBackRawResponse, latency)
	}
	return append([]byte(nil), body...), latency, nil
}
