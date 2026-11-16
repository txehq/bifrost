package byteplus

import (
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"strconv"
	"time"

	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	schemas "github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

// ArkVideoAPI is Seedance's video task API on an Ark host: create, retrieve, list, cancel, delete and
// download, with Ark's task object read through this package's converters (fork, TXE-7493). BytePlus,
// VolcEngine and ModelArk serve the same API on different hosts, so all three use it; before, volcengine
// and modelark sent OpenAI's video shape to Ark, and a task Ark reported succeeded read back as
// in_progress with no seconds, size or usage.
type ArkVideoAPI struct {
	// Client sends every call. Downloads of the task's video_url go through the hardened media
	// fetcher, never this client.
	Client *fasthttp.Client
	// NetworkConfig supplies the base URL, the extra headers and the stream idle timeout.
	NetworkConfig schemas.NetworkConfig
	// TasksPath is the task collection under the base URL, through request_path_overrides like
	// every other call: /api/v3/contents/generations/tasks on a bare host, or
	// /contents/generations/tasks on a base URL that already ends in /api/v3.
	TasksPath string
	// ProviderName is the name the returned task ids carry.
	ProviderName schemas.ModelProvider
	// BaseProvider, when set, is a second suffix accepted on an incoming id: a custom provider
	// on the volcengine or modelark base also takes an id that names its base (TXE-7085).
	BaseProvider schemas.ModelProvider
	// SendBackRawRequest and SendBackRawResponse are the request's resolved raw settings.
	SendBackRawRequest  bool
	SendBackRawResponse bool
}

// taskID is Ark's task id for a Bifrost video id: the provider suffix removed.
func (api *ArkVideoAPI) taskID(id string) string {
	if api.BaseProvider != "" {
		return providerUtils.StripVideoIDProviderSuffixOrBase(id, api.ProviderName, api.BaseProvider)
	}
	return providerUtils.StripVideoIDProviderSuffix(id, api.ProviderName)
}

// withBareDraftTaskID is the request with extra_params.draft_task_id as Ark's bare task id. A
// caller finalizing a draft passes the id Bifrost returned, which carries this provider's suffix
// (":volcengine", or a custom provider's name, or its base's); the converter strips only
// ":byteplus". The request is copied only when it names a draft, so the common case allocates
// nothing; the caller's request and its extra params are never modified.
func (api *ArkVideoAPI) withBareDraftTaskID(request *schemas.BifrostVideoGenerationRequest) *schemas.BifrostVideoGenerationRequest {
	if request == nil || request.Params == nil {
		return request
	}
	draftTaskID, ok := request.Params.ExtraParams["draft_task_id"].(string)
	if !ok || draftTaskID == "" {
		return request
	}
	bare := api.taskID(draftTaskID)
	if bare == draftTaskID {
		return request
	}
	params := *request.Params
	params.ExtraParams = maps.Clone(request.Params.ExtraParams)
	params.ExtraParams["draft_task_id"] = bare
	copied := *request
	copied.Params = &params
	return &copied
}

// do sends one call to the task API and returns the decoded body of a 2xx answer.
func (api *ArkVideoAPI) do(ctx *schemas.BifrostContext, key schemas.Key, method, path, rawQuery string, jsonData []byte, sendBackRawRequest, sendBackRawResponse bool) ([]byte, time.Duration, *schemas.BifrostError) {
	return doArkRequest(ctx, api.Client, api.NetworkConfig, key, method, path, rawQuery, jsonData, sendBackRawRequest, sendBackRawResponse)
}

// Generate creates a Seedance video generation task. Extra params pass through so Seedance
// task options such as draft, service_tier and return_last_frame reach the wire.
func (api *ArkVideoAPI) Generate(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostVideoGenerationRequest) (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError) {
	providerName := api.ProviderName
	request = api.withBareDraftTaskID(request)
	ctx.SetValue(schemas.BifrostContextKeyPassthroughExtraParams, true)
	jsonData, bifrostErr := providerUtils.CheckContextAndGetRequestBody(
		ctx,
		request,
		func() (providerUtils.RequestBodyWithExtraParams, error) {
			return ToBytePlusVideoGenerationRequest(request)
		})
	if bifrostErr != nil {
		return nil, bifrostErr
	}

	sendBackRawRequest := api.SendBackRawRequest
	sendBackRawResponse := api.SendBackRawResponse

	body, latency, bifrostErr := api.do(ctx, key, http.MethodPost, api.TasksPath, "", jsonData, sendBackRawRequest, sendBackRawResponse)
	if bifrostErr != nil {
		return nil, markSeedanceContentFilterError(bifrostErr)
	}

	var taskResp BytePlusVideoTaskCreationResponse
	rawRequest, rawResponse, bifrostErr := providerUtils.HandleProviderResponseCtx(ctx, body, &taskResp, jsonData, sendBackRawRequest, sendBackRawResponse)
	if bifrostErr != nil {
		return nil, providerUtils.SetErrorLatency(bifrostErr, latency)
	}
	if taskResp.ID == "" {
		return nil, providerUtils.EnrichError(ctx, providerUtils.NewBifrostOperationError(fmt.Sprintf("%s returned no task id", api.ProviderName), nil), jsonData, body, sendBackRawRequest, sendBackRawResponse, latency)
	}

	bifrostResp := &schemas.BifrostVideoGenerationResponse{
		ID:        providerUtils.AddVideoIDProviderSuffix(taskResp.ID, providerName),
		Model:     request.Model,
		Object:    "video",
		Status:    schemas.VideoStatusQueued,
		CreatedAt: time.Now().Unix(),
	}
	if duration := providerUtils.GetJSONField(jsonData, "duration").Int(); duration > 0 {
		bifrostResp.Seconds = schemas.Ptr(strconv.FormatInt(duration, 10))
	}
	bifrostResp.ExtraFields.Latency = latency.Milliseconds()
	if sendBackRawRequest {
		bifrostResp.ExtraFields.RawRequest = rawRequest
	}
	if sendBackRawResponse {
		bifrostResp.ExtraFields.RawResponse = rawResponse
	}
	return bifrostResp, nil
}

// Retrieve queries the status of a Seedance video generation task.
func (api *ArkVideoAPI) Retrieve(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostVideoRetrieveRequest) (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError) {
	providerName := api.ProviderName
	taskID := api.taskID(request.ID)
	escapedTaskID, idErr := providerUtils.EscapeResourceID(taskID, "video_id")
	if idErr != nil {
		return nil, idErr
	}

	sendBackRawResponse := api.SendBackRawResponse
	body, latency, bifrostErr := api.do(ctx, key, http.MethodGet, api.TasksPath+"/"+escapedTaskID, "", nil, false, sendBackRawResponse)
	if bifrostErr != nil {
		return nil, bifrostErr
	}

	var task BytePlusVideoTask
	_, rawResponse, bifrostErr := providerUtils.HandleProviderResponseCtx(ctx, body, &task, nil, false, sendBackRawResponse)
	if bifrostErr != nil {
		return nil, providerUtils.SetErrorLatency(bifrostErr, latency)
	}

	bifrostResp, bifrostErr := ToBifrostVideoGenerationResponse(&task)
	if bifrostErr != nil {
		return nil, providerUtils.SetErrorLatency(bifrostErr, latency)
	}
	if bifrostResp.ID == "" {
		bifrostResp.ID = taskID
	}
	bifrostResp.ID = providerUtils.AddVideoIDProviderSuffix(bifrostResp.ID, providerName)
	bifrostResp.ExtraFields.Latency = latency.Milliseconds()
	if sendBackRawResponse {
		bifrostResp.ExtraFields.RawResponse = rawResponse
	}
	return bifrostResp, nil
}

// Download fetches the generated video from the time-limited URL on a succeeded task.
func (api *ArkVideoAPI) Download(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostVideoDownloadRequest) (*schemas.BifrostVideoDownloadResponse, *schemas.BifrostError) {
	task, bifrostErr := api.Retrieve(ctx, key, &schemas.BifrostVideoRetrieveRequest{Provider: request.Provider, ID: request.ID})
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	if task.Status != schemas.VideoStatusCompleted {
		return nil, providerUtils.NewBifrostOperationError(fmt.Sprintf("video not ready, current status: %s", task.Status), nil)
	}
	if len(task.Videos) == 0 || task.Videos[0].URL == nil || *task.Videos[0].URL == "" {
		return nil, providerUtils.NewBifrostOperationError("video URL not available", nil)
	}

	// Fork: the URL comes from Ark's task response, so it is fetched through the SSRF-hardened media
	// fetcher (public addresses only, checked at dial time and on every redirect, the provider's
	// proxy honored) rather than the provider client, whose dialer admits loopback. No BaseURL is
	// given: Ark's files are on a storage host, never the API's. No credentials are sent; errors
	// name the URL without its signed query. A streamed request is forwarded as the file arrives,
	// with no size cap (TXE-7036); any other is held, within the fetcher's 256 MiB cap.
	videoURL := *task.Videos[0].URL
	return providerUtils.FetchVideoContent(ctx, providerUtils.VideoContentFetch{
		VendorURL:                videoURL,
		Stream:                   request.Stream,
		VideoID:                  request.ID,
		StreamIdleTimeoutSeconds: api.NetworkConfig.StreamIdleTimeoutInSeconds,
		// /v1/videos/{id}/content serves these bytes on the gateway's own origin, so only a
		// video/* type is passed on; anything else becomes the type the task's output format implies.
		ContentType: func(upstream string) string {
			return providerUtils.VideoContentTypeOr(upstream, seedanceVideoContentType(videoURL))
		},
	})
}

// Delete cancels a queued Seedance task or deletes the record of a finished one.
func (api *ArkVideoAPI) Delete(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostVideoDeleteRequest) (*schemas.BifrostVideoDeleteResponse, *schemas.BifrostError) {
	taskID := api.taskID(request.ID)
	escapedTaskID, idErr := providerUtils.EscapeResourceID(taskID, "video_id")
	if idErr != nil {
		return nil, idErr
	}

	_, latency, bifrostErr := api.do(ctx, key, http.MethodDelete, api.TasksPath+"/"+escapedTaskID, "", nil, false, false)
	if bifrostErr != nil {
		return nil, bifrostErr
	}

	// The id answered is the task's with this provider's suffix, whether the caller's id carried
	// it or the gateway had already removed it, as every other task call answers (TXE-7493).
	bifrostResp := &schemas.BifrostVideoDeleteResponse{
		ID:      providerUtils.AddVideoIDProviderSuffix(taskID, api.ProviderName),
		Object:  "video.deleted",
		Deleted: true,
	}
	bifrostResp.ExtraFields.Latency = latency.Milliseconds()
	return bifrostResp, nil
}

// List lists Seedance tasks. Ark pages by number rather than by cursor: the caller names the
// page in Ark's own page_num filter (page 1 when absent) and the response's next is the page number
// to ask for after it. An after cursor is rejected instead of being silently ignored, which would
// make a paging client loop on the same page. Ark's other filters (filter.status, filter.model,
// filter.task_ids) travel unchanged like every vendor filter (fork, contract C7 / BF15).
func (api *ArkVideoAPI) List(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostVideoListRequest) (*schemas.BifrostVideoListResponse, *schemas.BifrostError) {
	if request.After != nil && *request.After != "" {
		return nil, providerUtils.NewBifrostBadRequestError(fmt.Sprintf("%s video list pages by number and does not support an after cursor; send page_num", api.ProviderName))
	}
	values := url.Values{}
	if request.Limit != nil && *request.Limit > 0 {
		values.Set("page_size", strconv.Itoa(*request.Limit))
	}
	providerUtils.AddVideoListFilters(values, request.Filters)
	pageNum := 1
	if raw := values.Get("page_num"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 {
			return nil, providerUtils.NewBifrostBadRequestError("page_num must be a positive integer")
		}
		pageNum = parsed
	} else {
		values.Set("page_num", "1")
	}
	// Whether a page is the last one is worked out from Ark's total and the page size. Ark's
	// default size is not something Bifrost knows, so a caller paging past the first page names
	// the size (limit, or Ark's page_size); it is refused otherwise, not guessed.
	pageSize := 0
	if requested, err := strconv.Atoi(values.Get("page_size")); err == nil && requested > 0 {
		pageSize = requested
	}
	if pageNum > 1 && pageSize == 0 {
		return nil, providerUtils.NewBifrostBadRequestError(fmt.Sprintf("%s video list needs limit or page_size with a page_num above 1", api.ProviderName))
	}

	sendBackRawResponse := api.SendBackRawResponse
	body, latency, bifrostErr := api.do(ctx, key, http.MethodGet, api.TasksPath, values.Encode(), nil, false, sendBackRawResponse)
	if bifrostErr != nil {
		return nil, bifrostErr
	}

	var listResp BytePlusVideoTaskListResponse
	_, rawResponse, bifrostErr := providerUtils.HandleProviderResponseCtx(ctx, body, &listResp, nil, false, sendBackRawResponse)
	if bifrostErr != nil {
		return nil, providerUtils.SetErrorLatency(bifrostErr, latency)
	}

	providerName := api.ProviderName
	bifrostResp := &schemas.BifrostVideoListResponse{
		Object: "list",
		Data:   make([]schemas.VideoObject, 0, len(listResp.Items)),
	}
	for i := range listResp.Items {
		bifrostResp.Data = append(bifrostResp.Data, ToBifrostVideoObject(&listResp.Items[i], providerName))
	}
	if n := len(bifrostResp.Data); n > 0 {
		bifrostResp.FirstID = schemas.Ptr(bifrostResp.Data[0].ID)
		bifrostResp.LastID = schemas.Ptr(bifrostResp.Data[n-1].ID)
	}
	// Ark reports the total across all pages. The tasks before this page are counted by the page
	// size the caller named (required above for every page after the first, where this page's
	// own length would miscount a partly filled last page), and an empty page is always the last.
	hasMore := len(listResp.Items) > 0 && (pageNum-1)*pageSize+len(listResp.Items) < listResp.Total
	bifrostResp.HasMore = schemas.Ptr(hasMore)
	if hasMore {
		bifrostResp.Next = schemas.Ptr(strconv.Itoa(pageNum + 1))
	}
	bifrostResp.ExtraFields.Latency = latency.Milliseconds()
	if sendBackRawResponse {
		bifrostResp.ExtraFields.RawResponse = rawResponse
	}
	return bifrostResp, nil
}

// Cancel cancels a Seedance task that is still queued (fork, TXE-6426, contract C7 BF13).
// Ark's only cancel is DELETE on the task, which on a finished task deletes its record, so the
// task is queried once and DELETE is sent only while Ark reports it queued; in any other state
// the answer is 400 provider_cancel_unsupported and DELETE is never sent. A task Ark started
// between the query and the DELETE is refused by Ark, and that refusal is reported the same way.
func (api *ArkVideoAPI) Cancel(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostVideoCancelRequest) (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError) {
	return providerUtils.CancelVideoWhileQueued(
		func() (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError) {
			return api.Retrieve(ctx, key, &schemas.BifrostVideoRetrieveRequest{Provider: request.Provider, ID: request.ID})
		},
		func() *schemas.BifrostError {
			_, bifrostErr := api.Delete(ctx, key, &schemas.BifrostVideoDeleteRequest{Provider: request.Provider, ID: request.ID})
			if bifrostErr == nil || bifrostErr.IsBifrostError || bifrostErr.StatusCode == nil {
				return bifrostErr
			}
			if status := *bifrostErr.StatusCode; status != http.StatusBadRequest && status != http.StatusConflict {
				return bifrostErr
			}
			message := "the task can no longer be cancelled"
			if bifrostErr.Error != nil && bifrostErr.Error.Message != "" {
				message += ": " + bifrostErr.Error.Message
			}
			unsupported := schemas.NewVideoCancelUnsupportedError(message)
			unsupported.ExtraFields.RawResponse = bifrostErr.ExtraFields.RawResponse
			return unsupported
		},
	)
}
