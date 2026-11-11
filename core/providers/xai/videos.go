package xai

import (
	"fmt"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"time"

	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	schemas "github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

// xAI video generation (fork, TXE-6429, contract C4/C5/C6/C7/C9). The vendor API is a task:
// POST /v1/videos/generations answers {"request_id"}, GET /v1/videos/{request_id} answers the
// task. xAI has no cancel API, no list API and no way to find a task without its request_id
// (recovery method 5), so a create is written to xAI at most once (see createClient).

// XAIVideoImage is an image input of a video request: a public URL or a data URL, sent as given.
type XAIVideoImage struct {
	URL string `json:"url"`
}

// XAIVideoOutputTarget is where xAI uploads the finished video.
type XAIVideoOutputTarget struct {
	UploadURL string `json:"upload_url"` // a presigned URL xAI PUTs the video to
}

// XAIVideoGenerationRequest is the body of POST /v1/videos/generations.
type XAIVideoGenerationRequest struct {
	Model           string                `json:"model"`
	Prompt          string                `json:"prompt,omitempty"`
	Duration        *int                  `json:"duration,omitempty"`
	AspectRatio     string                `json:"aspect_ratio,omitempty"`
	Resolution      string                `json:"resolution,omitempty"`
	Image           *XAIVideoImage        `json:"image,omitempty"`
	LastFrame       *XAIVideoImage        `json:"last_frame,omitempty"`
	ReferenceImages []XAIVideoImage       `json:"reference_images,omitempty"`
	Output          *XAIVideoOutputTarget `json:"output,omitempty"`

	// ExtraParams are the caller's fields outside the unified structure (keyframes,
	// reference_audios, storage_options, user, generate_audio and any other xAI field). They
	// are merged into the body unchanged.
	ExtraParams map[string]interface{} `json:"-"`
}

// GetExtraParams implements providerUtils.RequestBodyWithExtraParams.
func (r *XAIVideoGenerationRequest) GetExtraParams() map[string]interface{} {
	return r.ExtraParams
}

// XAIVideoGenerationResponse is the answer to a create.
type XAIVideoGenerationResponse struct {
	RequestID string `json:"request_id"`
}

// XAIVideoTask is the answer to GET /v1/videos/{request_id}.
type XAIVideoTask struct {
	Status   string             `json:"status"`
	Progress *float64           `json:"progress,omitempty"`
	Model    string             `json:"model,omitempty"`
	Error    *XAIVideoTaskError `json:"error,omitempty"`
	Video    *XAIVideoResult    `json:"video,omitempty"`
	Usage    *XAIVideoUsage     `json:"usage,omitempty"`
}

// XAIVideoTaskError is the error of a failed task.
type XAIVideoTaskError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// XAIVideoResult is the video of a finished task.
type XAIVideoResult struct {
	URL      *string  `json:"url,omitempty"`      // a temporary public link; empty when the video was withheld
	Duration *float64 `json:"duration,omitempty"` // seconds
	// RespectModeration is false when xAI's moderation withheld the video. A pointer, so an
	// absent field is never read as a block.
	RespectModeration *bool `json:"respect_moderation,omitempty"`
}

// XAIVideoUsage is what xAI bills the task by.
type XAIVideoUsage struct {
	CostInUsdTicks *int64 `json:"cost_in_usd_ticks,omitempty"` // 1 USD = 1e10 ticks
	OutputTokens   *int   `json:"output_tokens,omitempty"`
}

// xaiVideoStatuses maps xAI's task status strings one to one. "pending" covers both waiting
// and generating (xAI has no separate queued state), so it is in_progress.
var xaiVideoStatuses = providerUtils.VideoStatusTable{
	"pending": schemas.VideoStatusInProgress,
	"done":    schemas.VideoStatusCompleted,
	"failed":  schemas.VideoStatusFailed,
	"expired": schemas.VideoStatusExpired,
}

// xaiVideoModerationReason is the reason recorded in content_filter when xAI withholds a
// finished video. xAI gives no reason code: the only signal is video.respect_moderation=false,
// so the field's name stands for the code.
const xaiVideoModerationReason = "respect_moderation"

// xaiVideoUnifiedFields are the unified video create fields xAI takes. Core refuses every
// other one with 400 caller_unsupported_parameter before a key is chosen: audio and
// output_format (no xAI field is specified for them), type and the upscale fields, and the
// task control fields callback_url, client_reference_id and expires_after_seconds, which
// cannot reach xAI. prompt, seed and negative_prompt are exempt from the check (contract C4);
// xAI has no field for the last two, so they are not sent.
var xaiVideoUnifiedFields = []string{
	"seconds",
	"size",
	"aspect_ratio",
	"resolution",
	"last_frame",
	"reference_images",
	schemas.TaskControlFieldOutputStorageURI,
}

// xaiVideoAspectRatioNames are the aspect ratios xAI documents, as xAI writes them.
var xaiVideoAspectRatioNames = []string{"1:1", "16:9", "9:16", "4:3", "3:4", "3:2", "2:3", "21:9", "5:2"}

// xaiVideoAspectRatios maps a width:height pair in lowest terms to xAI's name for that ratio.
// It is built from the names, so a name that is not in lowest terms ("21:9" is 7:3) is still
// found by the reduced pair a size gives.
var xaiVideoAspectRatios = func() map[[2]int]string {
	ratios := make(map[[2]int]string, len(xaiVideoAspectRatioNames))
	for _, name := range xaiVideoAspectRatioNames {
		w, h, _ := strings.Cut(name, ":")
		width, _ := strconv.Atoi(w)
		height, _ := strconv.Atoi(h)
		divisor := gcd(width, height)
		ratios[[2]int{width / divisor, height / divisor}] = name
	}
	return ratios
}()

// xaiVideoResolutions are the resolution tiers xAI documents, by short edge in pixels.
var xaiVideoResolutions = map[int]string{480: "480p", 720: "720p", 1080: "1080p"}

// xaiVideoSize converts an OpenAI-style "WIDTHxHEIGHT" size into xAI's aspect_ratio and
// resolution. xAI has no size field, so only a size that is exactly one of its ratios at one
// of its tiers can be delivered; anything else is refused, never moved to the nearest one.
func xaiVideoSize(size string) (aspectRatio, resolution string, err error) {
	refuse := func(why string) (string, string, error) {
		return "", "", providerUtils.ConverterBifrostError(schemas.NewUnsupportedParameterError("size", fmt.Sprintf(
			"size %q cannot be sent to xAI: %s; send aspect_ratio and resolution instead", size, why)))
	}
	w, h, ok := strings.Cut(strings.ToLower(strings.TrimSpace(size)), "x")
	if !ok {
		return refuse("expected WIDTHxHEIGHT")
	}
	width, errW := strconv.Atoi(strings.TrimSpace(w))
	height, errH := strconv.Atoi(strings.TrimSpace(h))
	if errW != nil || errH != nil || width <= 0 || height <= 0 {
		return refuse("expected WIDTHxHEIGHT")
	}
	divisor := gcd(width, height)
	aspectRatio, ok = xaiVideoAspectRatios[[2]int{width / divisor, height / divisor}]
	if !ok {
		return refuse("its aspect ratio is not one xAI generates")
	}
	resolution, ok = xaiVideoResolutions[min(width, height)]
	if !ok {
		return refuse("its short edge is not 480, 720 or 1080")
	}
	return aspectRatio, resolution, nil
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// ToXAIVideoGenerationRequest converts a Bifrost video create into xAI's body. Every unified
// field is sent as given or refused with caller_unsupported_parameter: none is dropped,
// rounded or replaced (contract C4). Input images go as the caller gave them, a URL or a data
// URL; Bifrost fetches nothing.
func ToXAIVideoGenerationRequest(bifrostReq *schemas.BifrostVideoGenerationRequest) (*XAIVideoGenerationRequest, error) {
	if bifrostReq == nil || bifrostReq.Input == nil {
		return nil, providerUtils.InvalidRequestErrorf("video generation input is required")
	}
	if bifrostReq.Input.VideoURI != nil && *bifrostReq.Input.VideoURI != "" {
		return nil, providerUtils.ConverterBifrostError(schemas.NewUnsupportedParameterError("video_uri",
			"xAI video generation takes no source video"))
	}
	req := &XAIVideoGenerationRequest{Model: bifrostReq.Model, Prompt: bifrostReq.Input.Prompt}
	if ref := bifrostReq.Input.InputReference; ref != nil && *ref != "" {
		req.Image = &XAIVideoImage{URL: *ref}
	}

	params := bifrostReq.Params
	if params == nil {
		return req, nil
	}
	if params.Seconds != nil && *params.Seconds != "" {
		seconds, err := strconv.Atoi(strings.TrimSpace(*params.Seconds))
		if err != nil {
			return nil, providerUtils.ConverterBifrostError(schemas.NewUnsupportedParameterError("seconds", fmt.Sprintf(
				"seconds %q cannot be sent to xAI, which takes a whole number of seconds", *params.Seconds)))
		}
		req.Duration = &seconds
	}
	req.AspectRatio = string(params.AspectRatio)
	req.Resolution = string(params.Resolution)
	if params.Size != "" {
		aspectRatio, resolution, err := xaiVideoSize(params.Size)
		if err != nil {
			return nil, err
		}
		// size names both; a separate aspect_ratio or resolution that says something else
		// leaves no single value to send.
		if (req.AspectRatio != "" && req.AspectRatio != aspectRatio) || (req.Resolution != "" && !strings.EqualFold(req.Resolution, resolution)) {
			return nil, providerUtils.ConverterBifrostError(schemas.NewUnsupportedParameterError("size", fmt.Sprintf(
				"size %q is %s at %s, which contradicts the aspect_ratio or resolution sent with it", params.Size, aspectRatio, resolution)))
		}
		if req.AspectRatio == "" {
			req.AspectRatio = aspectRatio
		}
		if req.Resolution == "" {
			req.Resolution = resolution
		}
	}
	if params.LastFrame != "" {
		req.LastFrame = &XAIVideoImage{URL: string(params.LastFrame)}
	}
	for _, ref := range params.ReferenceImages {
		req.ReferenceImages = append(req.ReferenceImages, XAIVideoImage{URL: ref})
	}
	if uri := params.OutputStorageURI; uri != nil && *uri != "" {
		req.Output = &XAIVideoOutputTarget{UploadURL: *uri}
	}

	if len(params.ExtraParams) > 0 {
		req.ExtraParams = make(map[string]interface{}, len(params.ExtraParams))
		maps.Copy(req.ExtraParams, params.ExtraParams)
		// The handlers keep each unified field in the extra params too (TXE-6518). One that was
		// converted above is removed, so the merge does not put the caller's string form over
		// xAI's object form. One the typed field could not hold (xAI's own {"url": ...} object
		// for last_frame) stays and is merged unchanged.
		if params.AspectRatio != "" {
			delete(req.ExtraParams, "aspect_ratio")
		}
		if params.Resolution != "" {
			delete(req.ExtraParams, "resolution")
		}
		if params.LastFrame != "" {
			delete(req.ExtraParams, "last_frame")
		}
		if len(params.ReferenceImages) > 0 {
			delete(req.ExtraParams, "reference_images")
		}
	}
	return req, nil
}

// ToBifrostVideoGenerationResponse converts an xAI task into the task object. Every usage
// field is xAI's own report; one xAI did not send stays empty (contract C5).
func ToBifrostVideoGenerationResponse(task *XAIVideoTask) *schemas.BifrostVideoGenerationResponse {
	resp := &schemas.BifrostVideoGenerationResponse{Object: "video", Model: task.Model, Progress: task.Progress}
	xaiVideoStatuses.Apply(resp, task.Status)

	if task.Error != nil && (task.Error.Code != "" || task.Error.Message != "") {
		resp.Error = &schemas.VideoCreateError{Code: task.Error.Code, Message: task.Error.Message}
	}
	if video := task.Video; video != nil {
		if video.Duration != nil {
			resp.Seconds = schemas.Ptr(strconv.FormatFloat(*video.Duration, 'f', -1, 64))
		}
		switch {
		case video.RespectModeration != nil && !*video.RespectModeration:
			// xAI reports a moderated task as done with no link. The task object reports a
			// blocked task as failed with content_filter (contract C6); provider_status keeps
			// xAI's own word.
			resp.Status = schemas.VideoStatusFailed
			resp.ContentFilter = &schemas.ContentFilterInfo{FilteredCount: 1, Reasons: []string{xaiVideoModerationReason}}
			if resp.Error == nil {
				resp.Error = &schemas.VideoCreateError{Message: "xAI withheld the video: video.respect_moderation is false"}
			}
		case video.URL != nil && *video.URL != "":
			resp.Videos = []schemas.VideoOutput{{Type: schemas.VideoOutputTypeURL, URL: schemas.Ptr(*video.URL), ContentType: "video/mp4"}}
		}
	}
	resp.Usage = xaiVideoUsage(task)
	return resp
}

// xaiVideoUsage builds the task object's usage from what xAI reported, nil when it reported
// nothing. cost keeps its upstream meaning (USD, so Bifrost prices the task at xAI's own
// figure); vendor_cost carries the same report in xAI's own unit.
func xaiVideoUsage(task *XAIVideoTask) *schemas.VideoUsage {
	usage := &schemas.VideoUsage{}
	reported := false
	if task.Video != nil && task.Video.Duration != nil {
		usage.OutputSeconds = schemas.Ptr(*task.Video.Duration)
		reported = true
	}
	if task.Usage != nil {
		if task.Usage.OutputTokens != nil {
			usage.OutputTokens = schemas.Ptr(*task.Usage.OutputTokens)
			reported = true
		}
		if ticks := task.Usage.CostInUsdTicks; ticks != nil {
			usage.VendorCost = &schemas.VendorCost{Amount: float64(*ticks), Unit: schemas.VendorCostUnitUSDTicks}
			if *ticks > 0 {
				usage.Cost = &schemas.BifrostCost{TotalCost: float64(*ticks) / 1e10}
			}
			reported = true
		}
	}
	if !reported {
		return nil
	}
	return usage
}

// parseXAIVideoError parses an error response of the video endpoints. A usage-policy refusal
// keeps xAI's status and body and is declared caller_content_filtered (contract C6). xAI
// documents no code for one, so it is recognised by the codes every vendor shares
// (schemas.IsContentFilterError).
func parseXAIVideoError(resp *fasthttp.Response) *schemas.BifrostError {
	bifrostErr := ParseXAIError(resp)
	if schemas.IsContentFilterError(bifrostErr) {
		bifrostErr.ExtraFields.ErrorType = schemas.ErrorTypeCallerContentFiltered
	}
	return bifrostErr
}

// refuseLargePayload refuses a request in large-payload mode. That mode streams the caller's
// body upstream unparsed, but none of xAI's media endpoints takes the unified body: each needs
// its own field names (and /v1/stt its own field order), which means converting a body the
// mode does not hold. The request is refused with a 400 before any upstream call instead of
// reaching xAI empty.
func refuseLargePayload(ctx *schemas.BifrostContext, operation string) *schemas.BifrostError {
	if !providerUtils.IsLargePayloadPassthroughEnabled(ctx) {
		return nil
	}
	return providerUtils.NewBifrostBadRequestError("large payload passthrough is not supported for xai " + operation +
		": the request is converted to xAI's own fields and cannot be streamed through unparsed; send a smaller body")
}

// rejectModelExtraParam refuses an extra field named model, in any letter case. Governance and
// key selection authorize the request's typed model; an extra field is merged over the
// converted body (or written to the form) after that, so a "model" among them would send the
// key to a model nobody authorized. The HTTP handlers never produce one; a Go SDK caller can.
func rejectModelExtraParam(extraParams map[string]interface{}) *schemas.BifrostError {
	for name := range extraParams {
		if strings.EqualFold(name, "model") {
			return providerUtils.NewBifrostBadRequestError("extra params cannot set model; set the request's model")
		}
	}
	return nil
}

// doVideoRequest sends one JSON request to an xAI video endpoint and returns a copy of the
// body of a 2xx answer. A create passes singleAttempt, which writes it to xAI at most once.
func (provider *XAIProvider) doVideoRequest(ctx *schemas.BifrostContext, key schemas.Key, method, path string, jsonData []byte, singleAttempt, sendBackRawRequest, sendBackRawResponse bool) ([]byte, time.Duration, *schemas.BifrostError) {
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)
	req.SetRequestURI(provider.networkConfig.BaseURL + providerUtils.GetPathFromContext(ctx, path))
	req.Header.SetMethod(method)
	if value := key.Value.GetValue(); value != "" {
		req.Header.Set("Authorization", "Bearer "+value)
	}
	if jsonData != nil {
		req.Header.SetContentType("application/json")
		req.SetBody(jsonData)
	}

	var (
		latency    time.Duration
		bifrostErr *schemas.BifrostError
		wait       func()
	)
	if singleAttempt {
		// A send with no response also makes the failure terminal for core's retries.
		latency, bifrostErr, wait = providerUtils.SendSingleAttempt(ctx, provider.createClient, req, resp)
	} else {
		latency, bifrostErr, wait = providerUtils.MakeRequestWithContext(ctx, provider.client, req, resp)
	}
	defer wait()
	if bifrostErr != nil {
		return nil, latency, bifrostErr
	}
	ctx.SetValue(schemas.BifrostContextKeyProviderResponseHeaders, providerUtils.ExtractProviderResponseHeaders(resp))
	if resp.StatusCode() < fasthttp.StatusOK || resp.StatusCode() >= fasthttp.StatusMultipleChoices {
		return nil, latency, providerUtils.EnrichError(ctx, parseXAIVideoError(resp), jsonData, nil, sendBackRawRequest, sendBackRawResponse, latency)
	}
	body, err := providerUtils.CheckAndDecodeBody(resp)
	if err != nil {
		if singleAttempt {
			// xAI accepted the create; only its answer is unreadable.
			providerUtils.MarkUpstreamWriteUnknown(ctx)
		}
		rawErrBody := append([]byte(nil), resp.Body()...)
		return nil, latency, providerUtils.EnrichError(ctx, providerUtils.NewBifrostOperationError(schemas.ErrProviderResponseDecode, err), jsonData, rawErrBody, sendBackRawRequest, sendBackRawResponse, latency)
	}
	return append([]byte(nil), body...), latency, nil
}

// VideoGeneration creates an xAI video task. The create is written to xAI at most once, also
// when the connection fails: xAI cannot find a task again, so a second copy would be a second
// billed task nobody tracks. That holds at both layers that could repeat it: the HTTP client
// does not resend, and a create that got no response is not retried by core whatever
// max_retries says (providerUtils.SendSingleAttempt). The caller's extra fields are always
// merged into the body.
func (provider *XAIProvider) VideoGeneration(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostVideoGenerationRequest) (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError) {
	if bifrostErr := providerUtils.CheckOperationAllowed(schemas.XAI, provider.customProviderConfig, schemas.VideoGenerationRequest); bifrostErr != nil {
		return nil, bifrostErr
	}
	if bifrostErr := refuseLargePayload(ctx, "video generation"); bifrostErr != nil {
		return nil, bifrostErr
	}
	if bifrostErr := rejectModelExtraParam(request.GetExtraParams()); bifrostErr != nil {
		return nil, bifrostErr
	}
	providerName := provider.GetProviderKey()
	ctx.SetValue(schemas.BifrostContextKeyPassthroughExtraParams, true)
	jsonData, bifrostErr := providerUtils.CheckContextAndGetRequestBody(ctx, request, func() (providerUtils.RequestBodyWithExtraParams, error) {
		return ToXAIVideoGenerationRequest(request)
	})
	if bifrostErr != nil {
		return nil, bifrostErr
	}

	sendBackRawRequest := providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest)
	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)
	body, latency, bifrostErr := provider.doVideoRequest(ctx, key, http.MethodPost, "/v1/videos/generations", jsonData, true, sendBackRawRequest, sendBackRawResponse)
	if bifrostErr != nil {
		return nil, bifrostErr
	}

	// From here xAI has answered 2xx: the task may exist. An answer that cannot be used loses
	// its id as surely as no answer, so it is not retried or sent to a fallback either.
	var created XAIVideoGenerationResponse
	rawRequest, rawResponse, bifrostErr := providerUtils.HandleProviderResponseCtx(ctx, body, &created, jsonData, sendBackRawRequest, sendBackRawResponse)
	if bifrostErr != nil {
		providerUtils.MarkUpstreamWriteUnknown(ctx)
		return nil, providerUtils.SetErrorLatency(bifrostErr, latency)
	}
	if created.RequestID == "" {
		providerUtils.MarkUpstreamWriteUnknown(ctx)
		return nil, providerUtils.EnrichError(ctx, providerUtils.NewBifrostOperationError("xai returned no request_id", nil), jsonData, body, sendBackRawRequest, sendBackRawResponse, latency)
	}

	// xAI answers a create with the id alone: no status and no time, so the task object says
	// queued and carries neither provider_status nor created_at.
	bifrostResp := &schemas.BifrostVideoGenerationResponse{
		ID:     providerUtils.AddVideoIDProviderSuffix(created.RequestID, providerName),
		Model:  request.Model,
		Object: "video",
		Status: schemas.VideoStatusQueued,
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

// VideoRetrieve queries an xAI video task once.
func (provider *XAIProvider) VideoRetrieve(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostVideoRetrieveRequest) (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError) {
	if bifrostErr := providerUtils.CheckOperationAllowed(schemas.XAI, provider.customProviderConfig, schemas.VideoRetrieveRequest); bifrostErr != nil {
		return nil, bifrostErr
	}
	return provider.retrieveVideoTask(ctx, key, request)
}

// retrieveVideoTask is the query behind VideoRetrieve and VideoDownload, each of which checks
// its own permission first.
func (provider *XAIProvider) retrieveVideoTask(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostVideoRetrieveRequest) (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError) {
	providerName := provider.GetProviderKey()
	requestID := providerUtils.StripVideoIDProviderSuffix(request.ID, providerName)
	escapedID, idErr := providerUtils.EscapeResourceID(requestID, "video_id")
	if idErr != nil {
		return nil, idErr
	}

	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)
	body, latency, bifrostErr := provider.doVideoRequest(ctx, key, http.MethodGet, "/v1/videos/"+escapedID, nil, false, false, sendBackRawResponse)
	if bifrostErr != nil {
		return nil, bifrostErr
	}

	var task XAIVideoTask
	_, rawResponse, bifrostErr := providerUtils.HandleProviderResponseCtx(ctx, body, &task, nil, false, sendBackRawResponse)
	if bifrostErr != nil {
		return nil, providerUtils.SetErrorLatency(bifrostErr, latency)
	}

	bifrostResp := ToBifrostVideoGenerationResponse(&task)
	bifrostResp.ID = providerUtils.AddVideoIDProviderSuffix(requestID, providerName)
	bifrostResp.ExtraFields.Latency = latency.Milliseconds()
	if sendBackRawResponse {
		bifrostResp.ExtraFields.RawResponse = rawResponse
	}
	return bifrostResp, nil
}

// VideoDownload fetches the finished video from the public link xAI gave (contract C9).
func (provider *XAIProvider) VideoDownload(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostVideoDownloadRequest) (*schemas.BifrostVideoDownloadResponse, *schemas.BifrostError) {
	if bifrostErr := providerUtils.CheckOperationAllowed(schemas.XAI, provider.customProviderConfig, schemas.VideoDownloadRequest); bifrostErr != nil {
		return nil, bifrostErr
	}
	// xAI produces the video and nothing else: a thumbnail or a sprite sheet cannot be
	// delivered, and answering with the video instead would be a different artifact.
	if request.Variant != nil && *request.Variant != "" && *request.Variant != schemas.VideoDownloadVariantVideo {
		return nil, schemas.NewUnsupportedParameterError("variant", fmt.Sprintf(
			"variant %q is not available from xAI, which produces only the video", *request.Variant))
	}
	task, bifrostErr := provider.retrieveVideoTask(ctx, key, &schemas.BifrostVideoRetrieveRequest{Provider: request.Provider, ID: request.ID})
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	// Nothing to download yet, or ever (a failed, expired or moderated task): the caller asked
	// too early or for a task with no video, which is the caller's request, not a Bifrost fault.
	if task.Status != schemas.VideoStatusCompleted {
		return nil, providerUtils.NewBifrostBadRequestError(fmt.Sprintf("video not ready, current status: %s", task.Status))
	}
	if len(task.Videos) == 0 || task.Videos[0].URL == nil || *task.Videos[0].URL == "" {
		return nil, schemas.NewProviderEmptyResultError("xAI reported the task done but gave no video link")
	}

	// The link comes from xAI's response, so it is fetched through the SSRF-hardened media
	// fetcher (public addresses only, checked at dial time and on every redirect) and without
	// credentials: it is a public link, and the key belongs to api.x.ai alone.
	start := time.Now()
	contentType, body, err := providerUtils.FetchMediaURL(ctx, *task.Videos[0].URL)
	latency := time.Since(start)
	if err != nil {
		return nil, providerUtils.SetErrorLatency(providerUtils.NewBifrostOperationError("failed to download video", err), latency)
	}
	bifrostResp := &schemas.BifrostVideoDownloadResponse{
		VideoID: request.ID,
		Content: body,
		// /v1/videos/{id}/content serves these bytes on the gateway's own origin, so only a
		// video/* type is passed on.
		ContentType: providerUtils.VideoContentTypeOr(contentType, "video/mp4"),
	}
	bifrostResp.ExtraFields.Latency = latency.Milliseconds()
	return bifrostResp, nil
}

// VideoDelete is not supported: xAI has no API to delete a video task.
func (provider *XAIProvider) VideoDelete(_ *schemas.BifrostContext, _ schemas.Key, _ *schemas.BifrostVideoDeleteRequest) (*schemas.BifrostVideoDeleteResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.VideoDeleteRequest, provider.GetProviderKey())
}

// VideoList is not supported: xAI has no API to list video tasks.
func (provider *XAIProvider) VideoList(_ *schemas.BifrostContext, _ schemas.Key, _ *schemas.BifrostVideoListRequest) (*schemas.BifrostVideoListResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.VideoListRequest, provider.GetProviderKey())
}
