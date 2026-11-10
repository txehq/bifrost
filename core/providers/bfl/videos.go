package bfl

import (
	"fmt"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	schemas "github.com/capsohq/bifrost/core/schemas"
)

// FLUX 3 Video (TXE-6617, contract C4 to C7 and C9, BFL vendor sheet B15 to B19). BFL serves it
// as a task on POST /v1/flux-3-video; the result is polled on get_result, as images are. The
// endpoint has no webhook_url, external id or deadline, so no task control field is declared and
// all four are refused before any upstream call (recovery method 5: Tokun stores the create
// response first). BFL has no cancel, delete or list API for tasks.

// videoModel is the FLUX 3 Video endpoint, which is also its model name.
const videoModel = "flux-3-video"

// videoLinkLifetime is how long BFL keeps a FLUX 3 Video result link, about two hours (BFL vendor
// sheet; contract C7).
const videoLinkLifetime = 2 * time.Hour

// videoFields are the unified video fields FLUX 3 Video delivers. Everything else (size,
// output_format, last_frame, reference_images, the upscale fields, the task control fields) is
// refused by core before a key is chosen; seed and negative_prompt have no BFL counterpart and
// are not sent (contract C4).
var videoFields = []string{"seconds", "aspect_ratio", "resolution", "audio"}

// videoResolutions maps the unified resolution tiers to BFL's: hd is 720p, fhd 1080p, qhd 1440p,
// uhd 2160p. BFL's own values pass unchanged.
var videoResolutions = map[string]string{
	"720p": "hd", "1080p": "fhd", "1440p": "qhd", "2160p": "uhd", "4k": "uhd",
	"hd": "hd", "fhd": "fhd", "qhd": "qhd", "uhd": "uhd",
}

// toBFLVideoBody converts a video create into the body of POST /v1/flux-3-video.
func toBFLVideoBody(request *schemas.BifrostVideoGenerationRequest) (*bflImageBody, error) {
	fields := map[string]any{}
	var extra map[string]any
	if request.Params != nil {
		// The typed unified fields are converted below; their raw copies must not reach BFL too.
		extra = extraWithout(request.Params.ExtraParams, schemas.UnifiedVideoGenerationFields)
	}
	if request.Input != nil && request.Input.Prompt != "" {
		fields["prompt"] = request.Input.Prompt
	}
	mode := "t2v"
	if request.Input != nil && request.Input.InputReference != nil && *request.Input.InputReference != "" {
		if _, given := extra["keyframes"]; given {
			return nil, refuse("input_reference", "give the first frame as input_reference or as keyframes, not both")
		}
		fields["keyframes"] = bflImageInput(*request.Input.InputReference)
		mode = "i2v"
	}
	if request.Input != nil && request.Input.VideoURI != nil && *request.Input.VideoURI != "" {
		if mode == "i2v" {
			return nil, refuse("video_uri", "FLUX 3 Video takes keyframes or a start video, not both")
		}
		fields["start_video"] = *request.Input.VideoURI
		mode = "v2v"
	}
	// A mode the caller names (draft_enhance, or an explicit t2v/i2v/v2v) is BFL's and wins.
	if _, given := extra["mode"]; !given {
		fields["mode"] = mode
	}
	if p := request.Params; p != nil {
		if p.Seconds != nil {
			if *p.Seconds == "auto" {
				fields["duration"] = "auto"
			} else {
				seconds, err := strconv.Atoi(strings.TrimSpace(*p.Seconds))
				if err != nil {
					return nil, refuse("seconds", "FLUX 3 Video takes a whole number of seconds")
				}
				fields["duration"] = seconds
			}
		}
		if p.AspectRatio != "" {
			fields["aspect_ratio"] = string(p.AspectRatio)
		}
		if p.Resolution != "" {
			tier, ok := videoResolutions[strings.ToLower(string(p.Resolution))]
			if !ok {
				return nil, refuse("resolution", "FLUX 3 Video takes 720p (hd), 1080p (fhd), 1440p (qhd) or 2160p (uhd)")
			}
			fields["resolution"] = tier
		}
		if p.Audio != nil {
			fields["generate_audio"] = *p.Audio
		}
	}
	return &bflImageBody{fields: fields, extra: extra}, nil
}

// VideoGeneration creates a FLUX 3 Video task: exactly one POST /v1/flux-3-video. A failed
// connection is returned as the error; the provider never sends the create again.
func (provider *BFLProvider) VideoGeneration(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostVideoGenerationRequest) (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError) {
	if request.Model != videoModel {
		return nil, providerUtils.NewBifrostBadRequestError(fmt.Sprintf("BFL video generation is %s; model %q is not a BFL video model", videoModel, request.Model))
	}
	ctx.SetValue(schemas.BifrostContextKeyPassthroughExtraParams, true)
	jsonData, bifrostErr := providerUtils.CheckContextAndGetRequestBody(ctx, request, func() (providerUtils.RequestBodyWithExtraParams, error) {
		return toBFLVideoBody(request)
	})
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	sendBackRawRequest := providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest)
	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)

	submittedAt := time.Now().Unix()
	body, latency, bifrostErr := provider.doRequest(ctx, key, http.MethodPost, provider.requestURL(ctx, "/v1/"+videoModel), jsonData, sendBackRawRequest, sendBackRawResponse)
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	var submit BFLAsyncResponse
	rawRequest, rawResponse, bifrostErr := providerUtils.HandleProviderResponseCtx(ctx, body, &submit, jsonData, sendBackRawRequest, sendBackRawResponse)
	if bifrostErr != nil {
		return nil, providerUtils.SetErrorLatency(bifrostErr, latency)
	}
	if submit.ID == "" {
		return nil, providerUtils.EnrichError(ctx, providerUtils.NewBifrostOperationError("BFL returned no task id", nil), jsonData, body, sendBackRawRequest, sendBackRawResponse, latency)
	}

	task := &schemas.BifrostVideoGenerationResponse{
		ID:        providerUtils.AddVideoIDProviderSuffix(encodeTaskID(submit.ID, videoModel, submit.PollingURL, provider.networkConfig.BaseURL), provider.GetProviderKey()),
		Object:    "video",
		Model:     videoModel,
		Status:    schemas.VideoStatusQueued,
		CreatedAt: submittedAt,
	}
	if request.Params != nil && request.Params.Seconds != nil && *request.Params.Seconds != "auto" {
		task.Seconds = schemas.Ptr(strings.TrimSpace(*request.Params.Seconds))
	}
	if submit.Cost != nil {
		task.Usage = &schemas.VideoUsage{VendorCost: creditCost(*submit.Cost)}
	}
	task.ExtraFields.Latency = latency.Milliseconds()
	if sendBackRawRequest {
		task.ExtraFields.RawRequest = rawRequest
	}
	if sendBackRawResponse {
		task.ExtraFields.RawResponse = rawResponse
	}
	return task, nil
}

// VideoRetrieve queries a FLUX 3 Video task once: GET its polling URL (B16, B17).
func (provider *BFLProvider) VideoRetrieve(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostVideoRetrieveRequest) (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError) {
	vendorID := providerUtils.StripVideoIDProviderSuffix(request.ID, provider.GetProviderKey())
	ref, ok := decodeTaskID(vendorID)
	if !ok {
		return nil, providerUtils.NewBifrostBadRequestError("invalid BFL task id")
	}
	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)
	body, latency, bifrostErr := provider.doRequest(ctx, key, http.MethodGet, ref.pollURL(provider.networkConfig.BaseURL), nil, false, sendBackRawResponse)
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	var result BFLResultResponse
	_, rawResponse, bifrostErr := providerUtils.HandleProviderResponseCtx(ctx, body, &result, nil, false, sendBackRawResponse)
	if bifrostErr != nil {
		return nil, providerUtils.SetErrorLatency(bifrostErr, latency)
	}
	task := toVideoTask(&result)
	task.ID = providerUtils.AddVideoIDProviderSuffix(vendorID, provider.GetProviderKey())
	task.ExtraFields.Latency = latency.Milliseconds()
	if sendBackRawResponse {
		task.ExtraFields.RawResponse = rawResponse
	}
	return task, nil
}

// toVideoTask maps a get_result response to the video task object: the nine statuses one to one,
// provider_status verbatim, content_filter for the two Moderated states, the sample link in
// videos[], expires_at two hours after BFL's end time, and the settled cost in credits (B17).
func toVideoTask(result *BFLResultResponse) *schemas.BifrostVideoGenerationResponse {
	task := &schemas.BifrostVideoGenerationResponse{Object: "video", Model: videoModel}
	statusTable.Apply(task, result.Status)
	if result.Cost != nil {
		task.Usage = &schemas.VideoUsage{VendorCost: creditCost(*result.Cost)}
	}
	sample := readResult(result.Result)
	if sample.endTime != nil {
		task.CompletedAt = schemas.Ptr(int64(*sample.endTime))
	}
	switch task.Status {
	case schemas.VideoStatusCompleted:
		for _, link := range sample.links {
			task.Videos = append(task.Videos, schemas.VideoOutput{
				Type:        schemas.VideoOutputTypeURL,
				URL:         schemas.Ptr(link),
				ContentType: videoContentType(link),
			})
		}
	case schemas.VideoStatusFailed:
		task.Error = &schemas.VideoCreateError{Code: result.Status, Message: failureMessage(result)}
		if moderatedStatuses[result.Status] {
			task.ContentFilter = &schemas.ContentFilterInfo{FilteredCount: 1, Reasons: moderationReasons(result.Details)}
		}
	}
	// Empty unless completed, and without BFL's end time: never the query time.
	providerUtils.SetVideoExpiresAt(task, nil, videoLinkLifetime)
	return task
}

// videoContentType is the content type a result link's extension names; FLUX 3 Video returns MP4.
func videoContentType(link string) string {
	if i := strings.IndexAny(link, "?#"); i >= 0 {
		link = link[:i]
	}
	switch strings.ToLower(path.Ext(link)) {
	case ".webm":
		return "video/webm"
	case ".mov":
		return "video/quicktime"
	}
	return "video/mp4"
}

// VideoDownload fetches a completed task's video from BFL's result link (B18). The link comes
// from BFL's response, so it goes through the SSRF-hardened media fetcher (public addresses only,
// size and time bounded), not the provider client, and no key is sent.
func (provider *BFLProvider) VideoDownload(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostVideoDownloadRequest) (*schemas.BifrostVideoDownloadResponse, *schemas.BifrostError) {
	task, bifrostErr := provider.VideoRetrieve(ctx, key, &schemas.BifrostVideoRetrieveRequest{Provider: request.Provider, ID: request.ID})
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	if task.Status != schemas.VideoStatusCompleted {
		return nil, providerUtils.NewBifrostOperationError(fmt.Sprintf("video not ready, current status: %s", task.Status), nil)
	}
	if len(task.Videos) == 0 || task.Videos[0].URL == nil || *task.Videos[0].URL == "" {
		return nil, providerUtils.NewBifrostOperationError("video URL not available", nil)
	}
	videoURL := *task.Videos[0].URL
	start := time.Now()
	contentType, body, err := providerUtils.FetchMediaURL(ctx, videoURL)
	latency := time.Since(start)
	if err != nil {
		return nil, providerUtils.SetErrorLatency(providerUtils.NewBifrostOperationError("failed to download video", err), latency)
	}
	resp := &schemas.BifrostVideoDownloadResponse{
		VideoID:     request.ID,
		Content:     body,
		ContentType: providerUtils.VideoContentTypeOr(contentType, videoContentType(videoURL)),
	}
	resp.ExtraFields.Latency = latency.Milliseconds()
	return resp, nil
}
