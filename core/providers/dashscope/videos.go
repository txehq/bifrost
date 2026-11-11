package dashscope

import (
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	schemas "github.com/capsohq/bifrost/core/schemas"
)

const (
	dashScopeVideoSynthesisPath = "/api/v1/services/aigc/video-generation/video-synthesis"
	dashScopeTasksPath          = "/api/v1/tasks"

	// dashScopeVideoLinkLifetime is how long DashScope keeps a task and its video_url: 24 hours
	// (contract C7, the Alibaba Wan row).
	dashScopeVideoLinkLifetime = 24 * time.Hour

	// DashScope's task states, verbatim.
	dashScopeTaskPending   = "PENDING"
	dashScopeTaskRunning   = "RUNNING"
	dashScopeTaskSucceeded = "SUCCEEDED"
	dashScopeTaskFailed    = "FAILED"
	dashScopeTaskCanceled  = "CANCELED"
	dashScopeTaskUnknown   = "UNKNOWN"

	// dashScopeCancelRefusedCode is the code of the 400 DashScope answers when the task to
	// cancel is no longer PENDING.
	dashScopeCancelRefusedCode = "UnsupportedOperation"
)

// dashScopeVideoStatuses maps DashScope's six task states one to one onto the task object's
// status. UNKNOWN is what DashScope reports once a task is past its 24-hour lifetime (and for
// an id it never issued): the task and its link are gone, so it is expired.
var dashScopeVideoStatuses = providerUtils.VideoStatusTable{
	dashScopeTaskPending:   schemas.VideoStatusQueued,
	dashScopeTaskRunning:   schemas.VideoStatusInProgress,
	dashScopeTaskSucceeded: schemas.VideoStatusCompleted,
	dashScopeTaskFailed:    schemas.VideoStatusFailed,
	dashScopeTaskCanceled:  schemas.VideoStatusCancelled,
	dashScopeTaskUnknown:   schemas.VideoStatusExpired,
}

// dashScopeContentFilterCodes are the codes of a task DashScope's moderation failed: the
// prompt, an input image or the generated video was blocked.
var dashScopeContentFilterCodes = []string{"DataInspectionFailed", "IPInfringementSuspect"}

// dashScopeTaskTimeZone is the zone of submit_time, scheduled_time and end_time: DashScope
// documents them as UTC+8 on every site.
var dashScopeTaskTimeZone = time.FixedZone("UTC+8", 8*60*60)

// dashScopeVideoGenerationFields are the unified video create fields the provider converts.
// Core refuses any other priced field, and every task control field, before a key is chosen
// (contract C4 BF4, C7 BF12): DashScope has no callback, external task id, deadline or output
// location parameter. Which of these a Wan family takes is decided by the converter.
var dashScopeVideoGenerationFields = []string{
	"seconds", "size", "audio", "aspect_ratio", "resolution", "last_frame", "reference_images",
}

// SupportedUnifiedFields declares the unified fields video create delivers (contract C4, BF4).
func (provider *DashScopeProvider) SupportedUnifiedFields(requestType schemas.RequestType) ([]string, bool) {
	if requestType == schemas.VideoGenerationRequest {
		return dashScopeVideoGenerationFields, true
	}
	return nil, false
}

// wanFamily is the request shape a Wan model takes on the video-synthesis endpoint.
type wanFamily int

const (
	wanUnknown wanFamily = iota
	// wanLegacy is Wan 2.6 and earlier: text-to-video takes parameters.size, image-to-video
	// takes input.img_url and parameters.resolution.
	wanLegacy
	// wan27 takes parameters.resolution (and ratio for text-to-video) and input.media with
	// first_frame and last_frame.
	wan27
	// wan30 takes parameters.resolution and ratio, and input.media with first_frame,
	// last_frame and the reference types.
	wan30
)

// wanLegacyPrefixes are the model-name prefixes of the Wan 2.6-and-earlier families.
var wanLegacyPrefixes = []string{"wan2.6", "wan2.5", "wan2.2", "wan2.1", "wanx2.1"}

// wanFamilyOf returns the request shape of a model, by its name. A dated snapshot
// (wan2.7-t2v-2026-06-12) belongs to its family. A model whose request fields are not known
// (HappyHorse, a future Wan) is wanUnknown and is refused, never sent in a guessed shape.
func wanFamilyOf(model string) wanFamily {
	name := strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.HasPrefix(name, "wan3."):
		return wan30
	case strings.HasPrefix(name, "wan2.7"):
		return wan27
	}
	for _, prefix := range wanLegacyPrefixes {
		if strings.HasPrefix(name, prefix) {
			return wanLegacy
		}
	}
	return wanUnknown
}

// wanLegacySizes are the sizes DashScope documents for parameters.size, the 480P, 720P and
// 1080P tiers of Wan 2.6 and earlier. A size outside them is refused, never moved to the
// nearest tier (contract C4 agreement three); which of these tiers one model offers is the
// vendor's own check.
var wanLegacySizes = []string{
	"832*480", "480*832", "624*624",
	"1280*720", "720*1280", "960*960", "1088*832", "832*1088",
	"1920*1080", "1080*1920", "1440*1440", "1632*1248", "1248*1632",
}

// unsupportedVideoParameter is the converter's refusal of a field the model's family cannot
// take: 400 caller_unsupported_parameter with the field in error.param, no upstream call.
// dashScopeOwnedBodyFields are the keys of the body's input and parameters objects that the
// converter writes from the request's own fields: every field of DashScopeVideoInput and
// DashScopeVideoParameters. TestDashScopeOwnedBodyFieldsCoverTheTypedBody keeps the two equal.
var dashScopeOwnedBodyFields = []struct {
	name   string
	fields []string
}{
	{"input", []string{"prompt", "negative_prompt", "img_url", "media"}},
	{"parameters", []string{"size", "resolution", "ratio", "duration", "audio", "seed", "watermark", "prompt_extend"}},
}

func unsupportedVideoParameter(param, format string, args ...any) error {
	return providerUtils.ConverterBifrostError(schemas.NewUnsupportedParameterError(param, fmt.Sprintf(format, args...)))
}

// ToDashScopeVideoGenerationRequest converts a video create request to the video-synthesis
// body of the model's Wan family. A unified field the family cannot take is refused with
// caller_unsupported_parameter; nothing is dropped, replaced or clamped.
func ToDashScopeVideoGenerationRequest(bifrostReq *schemas.BifrostVideoGenerationRequest) (*DashScopeVideoRequest, error) {
	if bifrostReq == nil || bifrostReq.Input == nil {
		return nil, providerUtils.InvalidRequestErrorf("prompt is required")
	}
	family := wanFamilyOf(bifrostReq.Model)
	if family == wanUnknown {
		bifrostErr := providerUtils.NewBifrostBadRequestError(fmt.Sprintf(
			"dashscope: model %q is not a Wan video model this provider converts (wan3.0, wan2.7, wan2.6, wan2.5, wan2.2, wan2.1)", bifrostReq.Model))
		bifrostErr.ExtraFields.ErrorType = schemas.ErrorTypeCallerModelUnknown
		return nil, providerUtils.ConverterBifrostError(bifrostErr)
	}
	if bifrostReq.Input.VideoURI != nil && *bifrostReq.Input.VideoURI != "" {
		return nil, unsupportedVideoParameter("video_uri", "dashscope: video_uri is not converted for Wan video generation")
	}

	req := &DashScopeVideoRequest{
		Model: bifrostReq.Model,
		Input: DashScopeVideoInput{Prompt: bifrostReq.Input.Prompt},
	}
	params := bifrostReq.Params
	if params == nil {
		params = &schemas.VideoGenerationParameters{}
	}
	vendorParams := &DashScopeVideoParameters{}

	extras := maps.Clone(params.ExtraParams)
	// The handlers keep the four newer unified fields in the extra params too. The typed
	// field is the one converted, so the copy is removed; a copy with no typed value held a
	// JSON type the unified field does not take, which no Wan field takes either.
	typedPresent := map[string]bool{
		"aspect_ratio":     params.AspectRatio != "",
		"resolution":       params.Resolution != "",
		"last_frame":       params.LastFrame != "",
		"reference_images": len(params.ReferenceImages) > 0,
	}
	for _, name := range schemas.UnifiedVideoGenerationFields {
		// An empty string or an empty list is the field left out, as it is for the typed field.
		if value, ok := extras[name]; ok && !typedPresent[name] && !isEmptyUnifiedValue(value) {
			return nil, unsupportedVideoParameter(name, "dashscope: %s must be a string (reference_images: an array of strings)", name)
		}
		delete(extras, name)
	}

	firstFrame := ""
	if bifrostReq.Input.InputReference != nil && *bifrostReq.Input.InputReference != "" {
		url, err := schemas.SanitizeImageURL(*bifrostReq.Input.InputReference)
		if err != nil {
			return nil, providerUtils.InvalidRequestErrorf("invalid input_reference: %v", err)
		}
		firstFrame = url
	}

	switch family {
	case wan27, wan30:
		if firstFrame != "" {
			req.Input.Media = append(req.Input.Media, DashScopeVideoMedia{Type: "first_frame", URL: firstFrame})
		}
		if params.LastFrame != "" {
			url, err := schemas.SanitizeImageURL(string(params.LastFrame))
			if err != nil {
				return nil, providerUtils.InvalidRequestErrorf("invalid last_frame: %v", err)
			}
			req.Input.Media = append(req.Input.Media, DashScopeVideoMedia{Type: "last_frame", URL: url})
		}
		if family == wan27 && len(params.ReferenceImages) > 0 {
			return nil, unsupportedVideoParameter("reference_images", "dashscope: %s takes a first and a last frame, not reference images", bifrostReq.Model)
		}
		for _, ref := range params.ReferenceImages {
			url, err := schemas.SanitizeImageURL(ref)
			if err != nil {
				return nil, providerUtils.InvalidRequestErrorf("invalid reference image: %v", err)
			}
			req.Input.Media = append(req.Input.Media, DashScopeVideoMedia{Type: "reference_image", URL: url})
		}
		for _, extra := range []struct{ name, mediaType string }{
			{"reference_videos", "reference_video"},
			{"reference_audios", "reference_audio"},
		} {
			value, ok := extras[extra.name]
			if !ok {
				continue
			}
			if family != wan30 {
				return nil, unsupportedVideoParameter(extra.name, "dashscope: %s takes no %s input", bifrostReq.Model, extra.mediaType)
			}
			urls, err := schemas.ConvertViaJSON[[]string](value)
			if err != nil {
				return nil, providerUtils.InvalidRequestErrorf("%s must be an array of URLs", extra.name)
			}
			for _, url := range urls {
				if strings.TrimSpace(url) == "" {
					return nil, providerUtils.InvalidRequestErrorf("%s holds an empty URL", extra.name)
				}
				req.Input.Media = append(req.Input.Media, DashScopeVideoMedia{Type: extra.mediaType, URL: url})
			}
			delete(extras, extra.name)
		}
		if params.Size != "" {
			return nil, unsupportedVideoParameter("size", "dashscope: %s takes resolution and aspect_ratio, not size", bifrostReq.Model)
		}
		vendorParams.Resolution = strings.ToUpper(string(params.Resolution))
		if params.AspectRatio != "" {
			// Wan 2.7 image-to-video has no ratio parameter: the clip follows the first frame.
			if family == wan27 && len(req.Input.Media) > 0 {
				return nil, unsupportedVideoParameter("aspect_ratio", "dashscope: %s follows the first frame's aspect ratio", bifrostReq.Model)
			}
			vendorParams.Ratio = string(params.AspectRatio)
		}

	case wanLegacy:
		for _, name := range []string{"reference_videos", "reference_audios"} {
			if _, ok := extras[name]; ok {
				return nil, unsupportedVideoParameter(name, "dashscope: %s takes no reference input", bifrostReq.Model)
			}
		}
		if params.LastFrame != "" {
			return nil, unsupportedVideoParameter("last_frame", "dashscope: %s takes no last frame", bifrostReq.Model)
		}
		if len(params.ReferenceImages) > 0 {
			return nil, unsupportedVideoParameter("reference_images", "dashscope: %s takes no reference images", bifrostReq.Model)
		}
		if params.AspectRatio != "" {
			return nil, unsupportedVideoParameter("aspect_ratio", "dashscope: %s has no aspect ratio parameter", bifrostReq.Model)
		}
		if firstFrame != "" {
			// Image-to-video: the clip follows the first frame, and resolution picks the tier.
			req.Input.ImgURL = schemas.Ptr(firstFrame)
			if params.Size != "" {
				return nil, unsupportedVideoParameter("size", "dashscope: %s image-to-video takes resolution, not size", bifrostReq.Model)
			}
			vendorParams.Resolution = strings.ToUpper(string(params.Resolution))
		} else {
			if params.Resolution != "" {
				return nil, unsupportedVideoParameter("resolution", "dashscope: %s text-to-video takes size, not resolution", bifrostReq.Model)
			}
			if params.Size != "" {
				size, ok := wanLegacySize(params.Size)
				if !ok {
					return nil, unsupportedVideoParameter("size", "dashscope: size %q is not one of %s's sizes (%s)",
						params.Size, bifrostReq.Model, strings.ReplaceAll(strings.Join(wanLegacySizes, ", "), "*", "x"))
				}
				vendorParams.Size = size
			}
		}
	}

	if strings.TrimSpace(req.Input.Prompt) == "" && req.Input.ImgURL == nil && len(req.Input.Media) == 0 {
		return nil, providerUtils.InvalidRequestErrorf("prompt is required")
	}

	if params.Seconds != nil && *params.Seconds != "" {
		// duration is a whole number of seconds; a fractional value is refused, not rounded.
		duration, err := strconv.Atoi(strings.TrimSpace(*params.Seconds))
		if err != nil {
			return nil, unsupportedVideoParameter("seconds", "dashscope: seconds %q is not a whole number of seconds", *params.Seconds)
		}
		vendorParams.Duration = &duration
	}
	vendorParams.Audio = params.Audio
	vendorParams.Seed = params.Seed
	if params.NegativePrompt != nil && *params.NegativePrompt != "" {
		req.Input.NegativePrompt = params.NegativePrompt
	}
	for _, flag := range []struct {
		name string
		dst  **bool
	}{
		{"watermark", &vendorParams.Watermark},
		{"prompt_extend", &vendorParams.PromptExtend},
	} {
		value, ok := extras[flag.name]
		if !ok {
			continue
		}
		enabled, isBool := value.(bool)
		if !isBool {
			return nil, providerUtils.InvalidRequestErrorf("%s must be true or false", flag.name)
		}
		*flag.dst = &enabled
		delete(extras, flag.name)
	}

	if *vendorParams != (DashScopeVideoParameters{}) {
		req.Parameters = vendorParams
	}
	// An extra param named input or parameters is merged into the body's object of that name.
	// A key this converter writes there from the request's own fields is refused: nested, it
	// is out of sight of core's price-field check, which reads the top-level names, and the
	// vendor would be sent (and bill for) a size, a tier or a duration the request does not show.
	// Names are compared the way the merger compares them (case and underscores ignored), so
	// "Parameters" or "SIZE" is refused like the exact spelling. The refusal names the keys as
	// the caller wrote them; the lowest in sort order when several match.
	refused := ""
	for key, value := range extras {
		for _, object := range dashScopeOwnedBodyFields {
			if !providerUtils.SameJSONField(key, object.name) {
				continue
			}
			nested := extraParamObject(value)
			for nestedKey := range nested {
				for _, field := range object.fields {
					if providerUtils.SameJSONField(nestedKey, field) {
						if name := key + "." + nestedKey; refused == "" || name < refused {
							refused = name
						}
					}
				}
			}
		}
	}
	if refused != "" {
		return nil, unsupportedVideoParameter(refused, "dashscope: %s is set from the request's own fields, not from an extra param", refused)
	}

	if len(extras) > 0 {
		req.ExtraParams = extras
	}
	return req, nil
}

// extraParamObject returns an extra param's value as a JSON object, or nil when it is not one.
// The HTTP handlers give a map[string]any; a Go SDK caller may give any value that encodes to
// an object (map[string]string, a struct, json.RawMessage), and the merger merges those too,
// so they are read through their JSON form.
func extraParamObject(value any) map[string]any {
	if object, ok := value.(map[string]any); ok {
		return object
	}
	if value == nil {
		return nil
	}
	encoded, err := schemas.Marshal(value)
	if err != nil {
		return nil
	}
	var object map[string]any
	if err := schemas.Unmarshal(encoded, &object); err != nil {
		return nil
	}
	return object
}

// isEmptyUnifiedValue reports whether an extra param holds nothing: null, "" or an empty list.
func isEmptyUnifiedValue(value any) bool {
	switch v := value.(type) {
	case nil:
		return true
	case string:
		return v == ""
	case []any:
		return len(v) == 0
	case []string:
		return len(v) == 0
	}
	return false
}

// wanLegacySize turns a unified size ("1280x720") into DashScope's form ("1280*720"), and
// reports whether it is one of the documented sizes. Only the unified spelling is taken:
// accounting prices the size as the request wrote it and reads "WxH" alone, so "1280*720" or
// "1280X720" would reach DashScope and be priced without its resolution band.
func wanLegacySize(size string) (string, bool) {
	for _, known := range wanLegacySizes {
		if size == strings.Replace(known, "*", "x", 1) {
			return known, true
		}
	}
	return "", false
}

// parseDashScopeTaskTime parses a task time ("2025-09-29 14:18:52.331", UTC+8) to Unix
// seconds. 0 when the vendor sent none or it does not parse.
func parseDashScopeTaskTime(value string) int64 {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	parsed, err := time.ParseInLocation("2006-01-02 15:04:05", value, dashScopeTaskTimeZone)
	if err != nil {
		return 0
	}
	return parsed.Unix()
}

// ToBifrostVideoGenerationResponse converts a DashScope task (the create response or a task
// query) to the task object. The id carries no provider suffix yet.
func ToBifrostVideoGenerationResponse(task *DashScopeTaskResponse) (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError) {
	if task == nil || task.Output == nil {
		return nil, providerUtils.NewBifrostOperationError("dashscope returned no task", nil)
	}
	output := task.Output
	resp := &schemas.BifrostVideoGenerationResponse{
		ID:        output.TaskID,
		Object:    "video",
		CreatedAt: parseDashScopeTaskTime(output.SubmitTime),
		Prompt:    output.OrigPrompt,
	}
	dashScopeVideoStatuses.Apply(resp, output.TaskStatus)

	if resp.Status.IsTerminal() {
		if endTime := parseDashScopeTaskTime(output.EndTime); endTime > 0 {
			resp.CompletedAt = schemas.Ptr(endTime)
		}
	}
	if output.TaskStatus == dashScopeTaskFailed && (output.Code != "" || output.Message != "") {
		resp.Error = &schemas.VideoCreateError{Code: output.Code, Message: output.Message}
		for _, code := range dashScopeContentFilterCodes {
			if output.Code == code {
				resp.ContentFilter = &schemas.ContentFilterInfo{FilteredCount: 1, Reasons: []string{output.Code}}
			}
		}
	}
	if resp.Status == schemas.VideoStatusCompleted {
		if output.VideoURL != "" {
			resp.Videos = []schemas.VideoOutput{{Type: schemas.VideoOutputTypeURL, URL: schemas.Ptr(output.VideoURL), ContentType: "video/mp4"}}
		}
		// DashScope keeps a task and its link for 24 hours from submission, so the link's
		// expiry is counted from submit_time, the earlier and safe bound.
		if resp.CreatedAt > 0 {
			resp.ExpiresAt = providerUtils.VideoExpiresAt(nil, schemas.Ptr(resp.CreatedAt), dashScopeVideoLinkLifetime)
		}
	}
	if usage := task.Usage; usage != nil && (usage.OutputVideoDuration != nil || usage.InputVideoDuration != nil) {
		resp.Usage = &schemas.VideoUsage{OutputSeconds: usage.OutputVideoDuration, InputSeconds: usage.InputVideoDuration}
		if usage.OutputVideoDuration != nil && *usage.OutputVideoDuration > 0 {
			resp.Seconds = schemas.Ptr(strconv.FormatFloat(*usage.OutputVideoDuration, 'f', -1, 64))
		}
	}
	return resp, nil
}

// dashScopeErrorObjectError is the error for a 2xx that carries DashScope's error object
// (code, message) and no task: 400 for a moderation block, 502 otherwise, with the vendor's
// code in error.code (contract C6 agreement one).
func dashScopeErrorObjectError(task *DashScopeTaskResponse) *schemas.BifrostError {
	cause := schemas.VendorErrorCauseProvider
	errorType := dashScopeErrorType(task.Code)
	if errorType == schemas.ErrorTypeCallerContentFiltered {
		cause = schemas.VendorErrorCauseContentFiltered
	}
	bifrostErr := schemas.NewVendorErrorObjectError(cause, task.Code, task.Message)
	if errorType != "" {
		bifrostErr.ExtraFields.ErrorType = errorType
	}
	return bifrostErr
}

// VideoGeneration creates a Wan video task: one POST to video-synthesis with
// X-DashScope-Async: enable, which is the only mode the endpoint has.
func (provider *DashScopeProvider) VideoGeneration(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostVideoGenerationRequest) (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError) {
	jsonData, bifrostErr := providerUtils.CheckContextAndGetRequestBody(ctx, request,
		func() (providerUtils.RequestBodyWithExtraParams, error) {
			return ToDashScopeVideoGenerationRequest(request)
		})
	if bifrostErr != nil {
		return nil, bifrostErr
	}

	sendBackRawRequest := providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest)
	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)

	body, latency, bifrostErr := provider.doRequest(ctx, key, schemas.VideoGenerationRequest, http.MethodPost, dashScopeVideoSynthesisPath, jsonData, sendBackRawRequest, sendBackRawResponse)
	if bifrostErr != nil {
		return nil, bifrostErr
	}

	var task DashScopeTaskResponse
	rawRequest, rawResponse, bifrostErr := providerUtils.HandleProviderResponseCtx(ctx, body, &task, jsonData, sendBackRawRequest, sendBackRawResponse)
	if bifrostErr != nil {
		return nil, providerUtils.SetErrorLatency(bifrostErr, latency)
	}
	if task.Output == nil || task.Output.TaskID == "" {
		if task.Code != "" {
			return nil, providerUtils.EnrichError(ctx, dashScopeErrorObjectError(&task), jsonData, body, sendBackRawRequest, sendBackRawResponse, latency)
		}
		return nil, providerUtils.EnrichError(ctx, providerUtils.NewBifrostOperationError("dashscope returned no task_id", nil), jsonData, body, sendBackRawRequest, sendBackRawResponse, latency)
	}

	bifrostResp, bifrostErr := ToBifrostVideoGenerationResponse(&task)
	if bifrostErr != nil {
		return nil, providerUtils.SetErrorLatency(bifrostErr, latency)
	}
	bifrostResp.ID = providerUtils.AddVideoIDProviderSuffix(bifrostResp.ID, provider.GetProviderKey())
	bifrostResp.Model = request.Model
	if bifrostResp.CreatedAt == 0 {
		// The create response carries no submit_time; a later query reports the vendor's.
		bifrostResp.CreatedAt = time.Now().Unix()
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

// VideoRetrieve queries a task once: GET /api/v1/tasks/{id}.
func (provider *DashScopeProvider) VideoRetrieve(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostVideoRetrieveRequest) (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError) {
	return provider.videoRetrieve(ctx, key, schemas.VideoRetrieveRequest, request.ID)
}

// videoRetrieve is the task query behind VideoRetrieve and VideoDownload. requestType picks
// the base, which is the same for both.
func (provider *DashScopeProvider) videoRetrieve(ctx *schemas.BifrostContext, key schemas.Key, requestType schemas.RequestType, videoID string) (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError) {
	providerName := provider.GetProviderKey()
	taskID := providerUtils.StripVideoIDProviderSuffix(videoID, providerName)
	escapedTaskID, idErr := providerUtils.EscapeResourceID(taskID, "video_id")
	if idErr != nil {
		return nil, idErr
	}

	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)
	body, latency, bifrostErr := provider.doRequest(ctx, key, requestType, http.MethodGet, dashScopeTasksPath+"/"+escapedTaskID, nil, false, sendBackRawResponse)
	if bifrostErr != nil {
		return nil, bifrostErr
	}

	var task DashScopeTaskResponse
	_, rawResponse, bifrostErr := providerUtils.HandleProviderResponseCtx(ctx, body, &task, nil, false, sendBackRawResponse)
	if bifrostErr != nil {
		return nil, providerUtils.SetErrorLatency(bifrostErr, latency)
	}
	if task.Output == nil && task.Code != "" {
		return nil, providerUtils.EnrichError(ctx, dashScopeErrorObjectError(&task), nil, body, false, sendBackRawResponse, latency)
	}
	bifrostResp, bifrostErr := ToBifrostVideoGenerationResponse(&task)
	if bifrostErr != nil {
		return nil, providerUtils.EnrichError(ctx, bifrostErr, nil, body, false, sendBackRawResponse, latency)
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

// VideoCancel cancels a task with one call: POST /api/v1/tasks/{id}/cancel. DashScope cancels
// only a PENDING task; for any other state it answers 400 UnsupportedOperation, which is
// returned as provider_cancel_unsupported. The cancel API deletes nothing, so the task is not
// queried first.
//
// DashScope's answer to a cancel is a request id and no task, so the task object carries the
// id and status cancelled and no provider_status: the vendor reported no state.
func (provider *DashScopeProvider) VideoCancel(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostVideoCancelRequest) (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError) {
	providerName := provider.GetProviderKey()
	taskID := providerUtils.StripVideoIDProviderSuffix(request.ID, providerName)
	escapedTaskID, idErr := providerUtils.EscapeResourceID(taskID, "video_id")
	if idErr != nil {
		return nil, idErr
	}

	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)
	body, latency, bifrostErr := provider.doRequest(ctx, key, schemas.VideoCancelRequest, http.MethodPost, dashScopeTasksPath+"/"+escapedTaskID+"/cancel", nil, false, sendBackRawResponse)
	if bifrostErr != nil {
		if isDashScopeCancelRefused(bifrostErr) {
			unsupported := schemas.NewVideoCancelUnsupportedError(bifrostErr.Error.Message)
			unsupported.ExtraFields.RawResponse = bifrostErr.ExtraFields.RawResponse
			unsupported.ExtraFields.Latency = bifrostErr.ExtraFields.Latency
			return nil, unsupported
		}
		return nil, bifrostErr
	}

	bifrostResp := &schemas.BifrostVideoGenerationResponse{
		ID:     providerUtils.AddVideoIDProviderSuffix(taskID, providerName),
		Object: "video",
		Status: schemas.VideoStatusCancelled,
	}
	bifrostResp.ExtraFields.Latency = latency.Milliseconds()
	if sendBackRawResponse && len(body) > 0 {
		var rawResponse interface{}
		if err := schemas.Unmarshal(body, &rawResponse); err == nil {
			bifrostResp.ExtraFields.RawResponse = rawResponse
		} else {
			bifrostResp.ExtraFields.RawResponse = string(body)
		}
	}
	return bifrostResp, nil
}

// isDashScopeCancelRefused reports whether a cancel's error is DashScope refusing because the
// task is no longer PENDING.
func isDashScopeCancelRefused(bifrostErr *schemas.BifrostError) bool {
	return bifrostErr != nil && bifrostErr.StatusCode != nil && *bifrostErr.StatusCode == http.StatusBadRequest &&
		bifrostErr.Error != nil && bifrostErr.Error.Code != nil && *bifrostErr.Error.Code == dashScopeCancelRefusedCode
}

// VideoDownload fetches a finished task's video from its video_url, a signed link that needs
// no key. It queries the task once for the link.
func (provider *DashScopeProvider) VideoDownload(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostVideoDownloadRequest) (*schemas.BifrostVideoDownloadResponse, *schemas.BifrostError) {
	task, bifrostErr := provider.videoRetrieve(ctx, key, schemas.VideoDownloadRequest, request.ID)
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	if task.Status != schemas.VideoStatusCompleted {
		return nil, providerUtils.NewBifrostOperationError(fmt.Sprintf("video not ready, current status: %s", task.Status), nil)
	}
	if len(task.Videos) == 0 || task.Videos[0].URL == nil || *task.Videos[0].URL == "" {
		return nil, providerUtils.NewBifrostOperationError("video URL not available", nil)
	}

	// The URL comes from DashScope's task response, so it is fetched through the SSRF-hardened
	// media fetcher (public addresses only, checked at dial time and on every redirect, the
	// provider's proxy honored), not the provider client. No BaseURL is given: the file is on a
	// storage host, never the API's. No credentials are sent. A streamed request is forwarded
	// as the file arrives, with no size cap; any other is held, within the fetcher's cap.
	return providerUtils.FetchVideoContent(ctx, providerUtils.VideoContentFetch{
		VendorURL:                *task.Videos[0].URL,
		Stream:                   request.Stream,
		VideoID:                  request.ID,
		StreamIdleTimeoutSeconds: provider.networkConfig.StreamIdleTimeoutInSeconds,
		// /v1/videos/{id}/content serves these bytes on the gateway's own origin, so only a
		// video/* type is passed on; anything else becomes video/mp4, which is what Wan writes.
		ContentType: func(upstream string) string {
			return providerUtils.VideoContentTypeOr(upstream, "video/mp4")
		},
	})
}

// ToBifrostVideoObject converts a listed DashScope task to a video list entry. A status the
// table does not know is in_progress, as on a task query: Bifrost does not report a terminal
// state it cannot name.
func ToBifrostVideoObject(task *DashScopeTaskListItem, providerName schemas.ModelProvider) schemas.VideoObject {
	status, ok := dashScopeVideoStatuses.Lookup(task.Status)
	if !ok {
		status = schemas.VideoStatusInProgress
	}
	obj := schemas.VideoObject{
		ID:        providerUtils.AddVideoIDProviderSuffix(task.TaskID, providerName),
		Object:    "video",
		Model:     task.ModelName,
		Status:    status,
		CreatedAt: task.GmtCreate / 1000,
	}
	if status.IsTerminal() && task.EndTime > 0 {
		obj.CompletedAt = schemas.Ptr(task.EndTime / 1000)
	}
	return obj
}

// VideoList lists the account's asynchronous tasks: GET /api/v1/tasks. DashScope pages by
// number: the caller names the page in DashScope's own page_no filter and the response's next
// is the page number to ask for after it, read from DashScope's page_no and total_page. limit
// is sent as page_size. DashScope's other filters (start_time, end_time, model_name, status)
// travel unchanged like every vendor filter (contract C7 / BF15). after and order have no
// DashScope counterpart and are refused, not ignored: an ignored after makes a paging client
// loop on one page.
//
// DashScope's list is of every asynchronous task of the account in the last 24 hours, image
// tasks included; model_name narrows it. Bifrost returns what DashScope lists.
func (provider *DashScopeProvider) VideoList(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostVideoListRequest) (*schemas.BifrostVideoListResponse, *schemas.BifrostError) {
	if request.After != nil && *request.After != "" {
		return nil, providerUtils.NewBifrostBadRequestError("dashscope video list pages by number and does not support an after cursor; send page_no")
	}
	if request.Order != nil && *request.Order != "" {
		return nil, providerUtils.NewBifrostBadRequestError("dashscope video list has no order parameter")
	}
	values := url.Values{}
	if request.Limit != nil && *request.Limit > 0 {
		values.Set("page_size", strconv.Itoa(*request.Limit))
	}
	providerUtils.AddVideoListFilters(values, request.Filters)
	pageNo := 1
	if raw := values.Get("page_no"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 {
			return nil, providerUtils.NewBifrostBadRequestError("page_no must be a positive integer")
		}
		pageNo = parsed
	}

	pathAndQuery := dashScopeTasksPath
	if query := values.Encode(); query != "" {
		pathAndQuery += "?" + query
	}
	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)
	body, latency, bifrostErr := provider.doRequest(ctx, key, schemas.VideoListRequest, http.MethodGet, pathAndQuery, nil, false, sendBackRawResponse)
	if bifrostErr != nil {
		return nil, bifrostErr
	}

	var listResp DashScopeTaskListResponse
	_, rawResponse, bifrostErr := providerUtils.HandleProviderResponseCtx(ctx, body, &listResp, nil, false, sendBackRawResponse)
	if bifrostErr != nil {
		return nil, providerUtils.SetErrorLatency(bifrostErr, latency)
	}
	if listResp.Data == nil && listResp.Code != "" {
		errorObject := &DashScopeTaskResponse{Code: listResp.Code, Message: listResp.Message}
		return nil, providerUtils.EnrichError(ctx, dashScopeErrorObjectError(errorObject), nil, body, false, sendBackRawResponse, latency)
	}

	providerName := provider.GetProviderKey()
	bifrostResp := &schemas.BifrostVideoListResponse{
		Object: "list",
		Data:   make([]schemas.VideoObject, 0, len(listResp.Data)),
	}
	for i := range listResp.Data {
		bifrostResp.Data = append(bifrostResp.Data, ToBifrostVideoObject(&listResp.Data[i], providerName))
	}
	if n := len(bifrostResp.Data); n > 0 {
		bifrostResp.FirstID = schemas.Ptr(bifrostResp.Data[0].ID)
		bifrostResp.LastID = schemas.Ptr(bifrostResp.Data[n-1].ID)
	}
	// The page DashScope says it answered wins over the one asked for; an empty page is
	// always the last.
	if listResp.PageNo > 0 {
		pageNo = listResp.PageNo
	}
	hasMore := len(listResp.Data) > 0 && pageNo < listResp.TotalPage
	bifrostResp.HasMore = schemas.Ptr(hasMore)
	if hasMore {
		bifrostResp.Next = schemas.Ptr(strconv.Itoa(pageNo + 1))
	}
	bifrostResp.ExtraFields.Latency = latency.Milliseconds()
	if sendBackRawResponse {
		bifrostResp.ExtraFields.RawResponse = rawResponse
	}
	return bifrostResp, nil
}
