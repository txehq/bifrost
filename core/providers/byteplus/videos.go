package byteplus

import (
	"fmt"
	"maps"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	schemas "github.com/capsohq/bifrost/core/schemas"
)

// ToBytePlusVideoGenerationRequest converts a Bifrost video generation request to a Seedance task body.
// Frame images, reference media and a draft task become content items; resolution, ratio and the rest
// of Seedance's generation fields are read from size and extra params, and anything unrecognised passes through.
func ToBytePlusVideoGenerationRequest(bifrostReq *schemas.BifrostVideoGenerationRequest) (*BytePlusVideoGenerationRequest, error) {
	if bifrostReq == nil || bifrostReq.Input == nil {
		return nil, providerUtils.InvalidRequestErrorf("prompt is required")
	}
	var draftTaskID string
	if bifrostReq.Params != nil {
		draftTaskID, _ = bifrostReq.Params.ExtraParams["draft_task_id"].(string)
	}
	prompt := strings.TrimSpace(bifrostReq.Input.Prompt)
	if prompt == "" && draftTaskID == "" {
		return nil, providerUtils.InvalidRequestErrorf("prompt is required")
	}
	req := &BytePlusVideoGenerationRequest{Model: bifrostReq.Model}
	if prompt != "" {
		req.Content = append(req.Content, BytePlusVideoContentItem{Type: "text", Text: bifrostReq.Input.Prompt})
	}
	if draftTaskID != "" {
		// Accept the draft's ID as Bifrost returned it, with its provider suffix.
		draftTaskID = providerUtils.StripVideoIDProviderSuffix(draftTaskID, schemas.BytePlus)
		req.Content = append(req.Content, BytePlusVideoContentItem{Type: "draft_task", DraftTask: &BytePlusDraftTask{ID: draftTaskID}})
	}
	hasFrameImage := false
	if bifrostReq.Input.InputReference != nil && *bifrostReq.Input.InputReference != "" {
		url, err := sanitizeSeedanceMediaURL(*bifrostReq.Input.InputReference)
		if err != nil {
			return nil, providerUtils.InvalidRequestErrorf("invalid input reference: %v", err)
		}
		req.Content = append(req.Content, BytePlusVideoContentItem{Type: "image_url", Role: "first_frame", ImageURL: &BytePlusVideoMediaURL{URL: url}})
		hasFrameImage = true
	}
	if bifrostReq.Input.VideoURI != nil && *bifrostReq.Input.VideoURI != "" {
		req.Content = append(req.Content, BytePlusVideoContentItem{Type: "video_url", Role: "reference_video", VideoURL: &BytePlusVideoMediaURL{URL: *bifrostReq.Input.VideoURI}})
	}

	params := bifrostReq.Params
	if params == nil {
		return req, nil
	}
	duration, bifrostErr := seedanceDuration(params.Seconds)
	if bifrostErr != nil {
		return nil, providerUtils.ConverterBifrostError(bifrostErr)
	}
	req.Duration = duration
	// Fork (TXE-6426, contract C4): resolution and ratio come from the unified resolution and
	// aspect_ratio, or from a size that names one Seedance tier and ratio exactly. A size that
	// disagrees with a field sent beside it is refused, never overridden.
	resolution, ratio := strings.TrimSpace(string(params.Resolution)), strings.TrimSpace(string(params.AspectRatio))
	if params.Size != "" {
		sizeResolution, sizeRatio, bifrostErr := bytePlusVideoSize(params.Size)
		if bifrostErr != nil {
			return nil, providerUtils.ConverterBifrostError(bifrostErr)
		}
		if (resolution != "" && !strings.EqualFold(resolution, sizeResolution)) || (ratio != "" && ratio != sizeRatio) {
			return nil, providerUtils.ConverterBifrostError(schemas.NewUnsupportedParameterError("size", fmt.Sprintf(
				"size %q is %s at %s, which disagrees with the resolution or aspect_ratio sent with it; send size alone, or resolution and aspect_ratio", params.Size, sizeResolution, sizeRatio)))
		}
		resolution, ratio = sizeResolution, sizeRatio
	}
	if resolution != "" {
		req.Resolution = &resolution
	}
	if ratio != "" {
		req.Ratio = &ratio
	}
	req.Seed = params.Seed
	req.GenerateAudio = params.Audio
	req.OutputFormat = params.OutputFormat
	// Fork (TXE-6426, contract C7 BF12): the two task control fields Ark takes. callback_url goes
	// unchanged; expires_after_seconds is Ark's execution_expires_after, refused outside Ark's range.
	if params.CallbackURL != nil && *params.CallbackURL != "" {
		req.CallbackURL = params.CallbackURL
	}
	if params.ExpiresAfterSeconds != nil {
		if v := *params.ExpiresAfterSeconds; v < seedanceMinExpiresAfter || v > seedanceMaxExpiresAfter {
			return nil, providerUtils.ConverterBifrostError(schemas.NewUnsupportedParameterError(schemas.TaskControlFieldExpiresAfterSeconds, fmt.Sprintf(
				"expires_after_seconds must be between %d and %d for byteplus video; got %d", seedanceMinExpiresAfter, seedanceMaxExpiresAfter, v)))
		}
		req.ExecutionExpiresAfter = params.ExpiresAfterSeconds
	}
	// The unified last_frame and reference_images (TXE-6518) are converted here; the handler's copy
	// of each unified field in ExtraParams is dropped below so none reaches Ark under its unified name.
	hasReferenceImages := false
	if v := strings.TrimSpace(string(params.LastFrame)); v != "" {
		url, err := sanitizeSeedanceMediaURL(v)
		if err != nil {
			return nil, providerUtils.InvalidRequestErrorf("invalid last_frame: %v", err)
		}
		req.Content = append(req.Content, BytePlusVideoContentItem{Type: "image_url", Role: "last_frame", ImageURL: &BytePlusVideoMediaURL{URL: url}})
		hasFrameImage = true
	}
	if len(params.ReferenceImages) > 0 {
		if hasFrameImage {
			return nil, providerUtils.InvalidRequestErrorf("reference_images cannot be combined with first or last frame images")
		}
		for _, ref := range params.ReferenceImages {
			url, err := sanitizeSeedanceMediaURL(ref)
			if err != nil {
				return nil, providerUtils.InvalidRequestErrorf("invalid reference image: %v", err)
			}
			req.Content = append(req.Content, BytePlusVideoContentItem{Type: "image_url", Role: "reference_image", ImageURL: &BytePlusVideoMediaURL{URL: url}})
		}
		hasReferenceImages = true
	}
	if len(params.ExtraParams) == 0 {
		return req, nil
	}

	// Fork: extra params are merged over this body and replace its keys, so a raw "content" would
	// put references (a draft task, media URLs) on the wire that neither this converter nor
	// governance's ownership check saw. The converter owns content.
	if extraParamNamed(params.ExtraParams, "content") {
		return nil, providerUtils.InvalidRequestErrorf("extra params cannot set content; use prompt, input_reference, video_uri, last_frame, reference_images, reference_videos, reference_audios or draft_task_id")
	}
	req.ExtraParams = make(map[string]interface{}, len(params.ExtraParams))
	maps.Copy(req.ExtraParams, params.ExtraParams)
	delete(req.ExtraParams, "draft_task_id")
	// A caller that set the typed field has it converted above; the copy the HTTP handler keeps
	// in ExtraParams is dropped. A caller that set only the extra (the Go SDK) is converted here.
	if v, ok := req.ExtraParams["aspect_ratio"].(string); ok && v != "" && req.Ratio == nil {
		req.Ratio = &v
	}
	if string(params.LastFrame) != "" {
		delete(req.ExtraParams, "last_frame")
	}
	if hasReferenceImages {
		delete(req.ExtraParams, "reference_images")
	}
	if string(params.Resolution) != "" {
		delete(req.ExtraParams, "resolution")
	}
	if v, ok := req.ExtraParams["last_frame"].(string); ok && v != "" {
		url, err := sanitizeSeedanceMediaURL(v)
		if err != nil {
			return nil, providerUtils.InvalidRequestErrorf("invalid last_frame: %v", err)
		}
		req.Content = append(req.Content, BytePlusVideoContentItem{Type: "image_url", Role: "last_frame", ImageURL: &BytePlusVideoMediaURL{URL: url}})
		hasFrameImage = true
		delete(req.ExtraParams, "last_frame")
	}
	if refs, err := schemas.ConvertViaJSON[[]string](req.ExtraParams["reference_images"]); err == nil && len(refs) > 0 {
		if hasFrameImage {
			return nil, providerUtils.InvalidRequestErrorf("reference_images cannot be combined with first or last frame images")
		}
		for _, ref := range refs {
			url, err := sanitizeSeedanceMediaURL(ref)
			if err != nil {
				return nil, providerUtils.InvalidRequestErrorf("invalid reference image: %v", err)
			}
			req.Content = append(req.Content, BytePlusVideoContentItem{Type: "image_url", Role: "reference_image", ImageURL: &BytePlusVideoMediaURL{URL: url}})
		}
		delete(req.ExtraParams, "reference_images")
	}
	if refs, err := schemas.ConvertViaJSON[[]string](req.ExtraParams["reference_videos"]); err == nil && len(refs) > 0 {
		for _, ref := range refs {
			req.Content = append(req.Content, BytePlusVideoContentItem{Type: "video_url", Role: "reference_video", VideoURL: &BytePlusVideoMediaURL{URL: ref}})
		}
		delete(req.ExtraParams, "reference_videos")
	}
	if refs, err := schemas.ConvertViaJSON[[]string](req.ExtraParams["reference_audios"]); err == nil && len(refs) > 0 {
		for _, ref := range refs {
			req.Content = append(req.Content, BytePlusVideoContentItem{Type: "audio_url", Role: "reference_audio", AudioURL: &BytePlusVideoMediaURL{URL: ref}})
		}
		delete(req.ExtraParams, "reference_audios")
	}
	if v, ok := req.ExtraParams["resolution"].(string); ok && v != "" {
		req.Resolution = &v
		delete(req.ExtraParams, "resolution")
	}
	// The copy of aspect_ratio the handlers keep among the extra params is dropped, so it
	// is not also sent as a field Ark does not know. Ark's own name, ratio, read below,
	// works only for a request that also sends aspect_ratio with the same value; core
	// refuses it otherwise (schemas.CheckExtraParamPriceFieldsFor).
	delete(req.ExtraParams, "aspect_ratio")
	if v, ok := req.ExtraParams["ratio"].(string); ok && v != "" {
		req.Ratio = &v
		delete(req.ExtraParams, "ratio")
	}
	if v, ok := req.ExtraParams["callback_url"].(string); ok && v != "" {
		req.CallbackURL = &v
		delete(req.ExtraParams, "callback_url")
	}
	return req, nil
}

// sanitizeSeedanceMediaURL normalises an image reference, also accepting the asset:// IDs of media
// uploaded to Ark's asset library.
func sanitizeSeedanceMediaURL(rawURL string) (string, error) {
	return schemas.SanitizeImageURLWithAllowedSchemes(rawURL, "http", "https", "asset")
}

// seedanceRatios are the concrete ratios Seedance accepts besides "adaptive".
var seedanceRatios = []struct {
	name string
	w, h int
}{
	{"21:9", 21, 9},
	{"16:9", 16, 9},
	{"4:3", 4, 3},
	{"1:1", 1, 1},
	{"3:4", 3, 4},
	{"9:16", 9, 16},
}

// Ark's range for execution_expires_after, in seconds (contract C7).
const (
	seedanceMinExpiresAfter = 3600
	seedanceMaxExpiresAfter = 259200
)

// Seedance durations in whole seconds: every Seedance model's range lies inside these bounds,
// and Ark refuses a value its model does not take with its own 400. seedanceAutoDuration is
// Ark's "the model chooses the length".
const (
	seedanceMinDuration  = 2
	seedanceMaxDuration  = 15
	seedanceAutoDuration = -1
)

// seedanceLinkLifetime is how long Ark keeps a task's video_url valid (contract C7, C9).
const seedanceLinkLifetime = 24 * time.Hour

// seedanceDuration converts the unified seconds to Ark's integer duration (fork, TXE-6426,
// contract C4). Seedance bills by length and takes whole seconds only, so a fractional value, or
// one outside every Seedance model's range, is refused with caller_unsupported_parameter: it is
// never rounded or clamped. "-1" is the one form for "the model chooses the length".
func seedanceDuration(seconds *string) (*int, *schemas.BifrostError) {
	if seconds == nil || strings.TrimSpace(*seconds) == "" {
		return nil, nil
	}
	value, err := strconv.ParseFloat(strings.TrimSpace(*seconds), 64)
	if err != nil || value != math.Trunc(value) || math.IsInf(value, 0) {
		return nil, schemas.NewUnsupportedParameterError("seconds", fmt.Sprintf(
			"seconds must be a whole number for byteplus video; got %q", *seconds))
	}
	duration := int(value)
	if duration != seedanceAutoDuration && (duration < seedanceMinDuration || duration > seedanceMaxDuration) {
		return nil, schemas.NewUnsupportedParameterError("seconds", fmt.Sprintf(
			"seconds must be between %d and %d, or -1 for a length the model chooses, for byteplus video; got %q", seedanceMinDuration, seedanceMaxDuration, *seconds))
	}
	return &duration, nil
}

// seedanceSizeTiers are the resolution tiers a WIDTHxHEIGHT size can name, by short edge.
var seedanceSizeTiers = map[int]string{480: "480p", 720: "720p", 1080: "1080p"}

// bytePlusVideoSize turns a "WIDTHxHEIGHT" size into Seedance's resolution tier and ratio (fork,
// TXE-6426, contract C4). The size must name one exactly: its short edge is a tier's and its long
// edge is that tier at one of Seedance's ratios, the same nominal size seedanceVideoSize reports
// for a task ("1280x720" is 720p at 16:9). Any other size is refused with
// caller_unsupported_parameter; no nearest tier or ratio is guessed, because both set the price.
func bytePlusVideoSize(size string) (string, string, *schemas.BifrostError) {
	unsupported := func() *schemas.BifrostError {
		return schemas.NewUnsupportedParameterError("size", fmt.Sprintf(
			"size %q is not a byteplus video size; send WIDTHxHEIGHT for 480p, 720p or 1080p at 21:9, 16:9, 4:3, 1:1, 3:4 or 9:16 (for example 1280x720), or send resolution and aspect_ratio", size))
	}
	parts := strings.SplitN(strings.ToLower(size), "x", 2)
	if len(parts) != 2 {
		return "", "", unsupported()
	}
	width, errW := strconv.Atoi(strings.TrimSpace(parts[0]))
	height, errH := strconv.Atoi(strings.TrimSpace(parts[1]))
	if errW != nil || errH != nil || width <= 0 || height <= 0 {
		return "", "", unsupported()
	}
	resolution, ok := seedanceSizeTiers[min(width, height)]
	if !ok {
		return "", "", unsupported()
	}
	// The same arithmetic as seedanceVideoSize: the long edge is the short edge scaled by the ratio.
	short, long := min(width, height), max(width, height)
	for _, r := range seedanceRatios {
		if (r.w >= r.h) != (width >= height) && r.w != r.h {
			continue
		}
		if int(math.Round(float64(short)*float64(max(r.w, r.h))/float64(min(r.w, r.h)))) == long {
			return resolution, r.name, nil
		}
	}
	return "", "", unsupported()
}

// seedanceShortEdge returns the short edge in pixels that a Seedance resolution tier is named
// after ("720p" is 720), or 0 for a tier it does not recognise.
func seedanceShortEdge(resolution string) int {
	tier := strings.ToLower(strings.TrimSpace(resolution))
	switch tier {
	case "2k":
		return 1440
	case "4k":
		return 2160
	}
	if digits, ok := strings.CutSuffix(tier, "p"); ok {
		if short, err := strconv.Atoi(digits); err == nil && short > 0 {
			return short
		}
	}
	return 0
}

// seedanceVideoSize turns a task's resolution tier and ratio into the nominal "WxH" size that video
// pricing reads: its bands match the short edge of that string, so the tier label Ark reports
// ("720p") selects no band and, being non-empty, would also replace the size captured from the
// request at settlement. The size is nominal (the tier's short edge scaled by the ratio), not the
// exact pixel grid Seedance renders. It is "" when the tier or the ratio is unknown, as with
// ratio "adaptive", so settlement keeps the size captured from the request.
func seedanceVideoSize(resolution, ratio string) string {
	short := seedanceShortEdge(resolution)
	if short == 0 {
		return ""
	}
	w, h, ok := strings.Cut(ratio, ":")
	if !ok {
		return ""
	}
	rw, errW := strconv.Atoi(strings.TrimSpace(w))
	rh, errH := strconv.Atoi(strings.TrimSpace(h))
	if errW != nil || errH != nil || rw <= 0 || rh <= 0 {
		return ""
	}
	if rw >= rh {
		return strconv.Itoa(int(math.Round(float64(short)*float64(rw)/float64(rh)))) + "x" + strconv.Itoa(short)
	}
	return strconv.Itoa(short) + "x" + strconv.Itoa(int(math.Round(float64(short)*float64(rh)/float64(rw))))
}

// seedanceVideoStatuses maps Ark's task states onto the task object's status one to one (fork,
// TXE-6426, contract C7 BF11). Ark's cancelled and expired are their own statuses, not failed.
var seedanceVideoStatuses = providerUtils.VideoStatusTable{
	string(BytePlusVideoTaskStatusQueued):    schemas.VideoStatusQueued,
	string(BytePlusVideoTaskStatusRunning):   schemas.VideoStatusInProgress,
	string(BytePlusVideoTaskStatusSucceeded): schemas.VideoStatusCompleted,
	string(BytePlusVideoTaskStatusFailed):    schemas.VideoStatusFailed,
	string(BytePlusVideoTaskStatusCancelled): schemas.VideoStatusCancelled,
	string(BytePlusVideoTaskStatusExpired):   schemas.VideoStatusExpired,
}

// bytePlusVideoStatus maps a Seedance task status onto the Bifrost video lifecycle. A state the
// table does not know is in_progress, never a terminal status.
func bytePlusVideoStatus(status BytePlusVideoTaskStatus) schemas.VideoStatus {
	if mapped, ok := seedanceVideoStatuses.Lookup(string(status)); ok {
		return mapped
	}
	return schemas.VideoStatusInProgress
}

// bytePlusVideoError builds the failure payload for a failed task.
func bytePlusVideoError(task *BytePlusVideoTask) *schemas.VideoCreateError {
	videoErr := &schemas.VideoCreateError{Code: string(task.Status), Message: "task " + string(task.Status)}
	if task.Error != nil {
		if task.Error.Code != "" {
			videoErr.Code = task.Error.Code
		}
		if task.Error.Message != "" {
			videoErr.Message = task.Error.Message
		}
	}
	return videoErr
}

// isSeedanceContentFilterCode reports whether an Ark error code is a moderation refusal:
// InputTextSensitiveContentDetected, InputImageSensitiveContentDetected,
// OutputVideoSensitiveContentDetected and the other *SensitiveContentDetected codes.
func isSeedanceContentFilterCode(code string) bool {
	return strings.Contains(code, "SensitiveContentDetected")
}

// markSeedanceContentFilterError sets error_type caller_content_filtered on an Ark error whose
// code is a moderation refusal of the caller's input (fork, TXE-6426, contract C6), so a blocked
// create is told apart from an invalid one. Any other error is returned unchanged.
func markSeedanceContentFilterError(bifrostErr *schemas.BifrostError) *schemas.BifrostError {
	if bifrostErr != nil && !bifrostErr.IsBifrostError && bifrostErr.Error != nil && bifrostErr.Error.Code != nil && isSeedanceContentFilterCode(*bifrostErr.Error.Code) {
		bifrostErr.ExtraFields.ErrorType = schemas.ErrorTypeCallerContentFiltered
	}
	return bifrostErr
}

// ToBifrostVideoGenerationResponse converts a Seedance task to Bifrost format.
func ToBifrostVideoGenerationResponse(task *BytePlusVideoTask) (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError) {
	if task == nil {
		return nil, providerUtils.NewBifrostOperationError("video task is nil", nil)
	}
	resp := &schemas.BifrostVideoGenerationResponse{
		ID:        task.ID,
		Object:    "video",
		Model:     task.Model,
		CreatedAt: task.CreatedAt,
		Size:      seedanceVideoSize(task.Resolution, task.Ratio),
	}
	// status and provider_status come from the same Ark string (contract C7 BF11).
	seedanceVideoStatuses.Apply(resp, string(task.Status))
	if task.Duration > 0 {
		resp.Seconds = schemas.Ptr(strconv.Itoa(task.Duration))
	}
	// Ark bills a Seedance task by the tokens it reports; they are passed on as reported (C5).
	if task.Usage != nil {
		resp.Usage = &schemas.VideoUsage{OutputTokens: schemas.Ptr(task.Usage.CompletionTokens)}
	}
	switch resp.Status {
	case schemas.VideoStatusCompleted:
		if task.UpdatedAt > 0 {
			resp.CompletedAt = schemas.Ptr(task.UpdatedAt)
		}
	case schemas.VideoStatusFailed:
		resp.Error = bytePlusVideoError(task)
		// A task Ark failed for its content (OutputVideoSensitiveContentDetected and the like) is a
		// content block, reported as one (contract C6): the vendor's code is the reason.
		if task.Error != nil && isSeedanceContentFilterCode(task.Error.Code) {
			resp.ContentFilter = &schemas.ContentFilterInfo{FilteredCount: 1, Reasons: []string{task.Error.Code}}
		}
	}
	if task.Content != nil && task.Content.VideoURL != "" {
		resp.Videos = []schemas.VideoOutput{{Type: schemas.VideoOutputTypeURL, URL: schemas.Ptr(task.Content.VideoURL), ContentType: seedanceVideoContentType(task.Content.VideoURL)}}
		// A task created with return_last_frame also returns its final frame, listed after the video
		// so VideoDownload, which reads the first output, still fetches the video.
		if task.Content.LastFrameURL != "" {
			resp.Videos = append(resp.Videos, schemas.VideoOutput{Type: schemas.VideoOutputTypeURL, URL: schemas.Ptr(task.Content.LastFrameURL), ContentType: "image/jpeg"})
		}
	}
	// Ark returns no expiry: a link lives 24 hours from the task's completion (contract C7, C9).
	providerUtils.SetVideoExpiresAt(resp, nil, seedanceLinkLifetime)
	return resp, nil
}

// seedanceVideoContentType reports a mov output (output_format "mov") as QuickTime and anything else as MP4.
func seedanceVideoContentType(videoURL string) string {
	if parsed, err := url.Parse(videoURL); err == nil && strings.HasSuffix(strings.ToLower(parsed.Path), ".mov") {
		return "video/quicktime"
	}
	return "video/mp4"
}

// ToBifrostVideoObject converts a listed Seedance task to a Bifrost video list entry.
func ToBifrostVideoObject(task *BytePlusVideoTask, providerName schemas.ModelProvider) schemas.VideoObject {
	obj := schemas.VideoObject{
		ID:        providerUtils.AddVideoIDProviderSuffix(task.ID, providerName),
		Object:    "video",
		Model:     task.Model,
		Status:    bytePlusVideoStatus(task.Status),
		CreatedAt: task.CreatedAt,
		Size:      seedanceVideoSize(task.Resolution, task.Ratio),
	}
	if task.Duration > 0 {
		obj.Seconds = schemas.Ptr(strconv.Itoa(task.Duration))
	}
	switch obj.Status {
	case schemas.VideoStatusCompleted:
		if task.UpdatedAt > 0 {
			obj.CompletedAt = schemas.Ptr(task.UpdatedAt)
			obj.ExpiresAt = providerUtils.VideoExpiresAt(nil, obj.CompletedAt, seedanceLinkLifetime)
		}
	case schemas.VideoStatusFailed:
		obj.Error = bytePlusVideoError(task)
	}
	return obj
}

// seedanceVideoGenerationFields are the unified video create fields the provider delivers to Ark
// (fork, TXE-6426, contract C4 and C7): seconds as duration, size or resolution and aspect_ratio
// as resolution and ratio, audio as generate_audio, last_frame and reference_images as content
// roles, callback_url unchanged and expires_after_seconds as execution_expires_after. Ark has no
// field for client_reference_id, output_storage_uri, negative_prompt or the upscale fields, so
// those are refused, not dropped.
var seedanceVideoGenerationFields = []string{
	"seconds", "size", "seed", "output_format", "audio",
	"aspect_ratio", "resolution", "last_frame", "reference_images",
	schemas.TaskControlFieldCallbackURL, schemas.TaskControlFieldExpiresAfterSeconds,
}

// SupportedUnifiedFields declares the unified fields video create delivers, so core refuses any
// other with caller_unsupported_parameter before a key is chosen (contract C4 BF4, C7 BF12).
func (provider *BytePlusProvider) SupportedUnifiedFields(requestType schemas.RequestType) ([]string, bool) {
	if requestType == schemas.VideoGenerationRequest {
		return seedanceVideoGenerationFields, true
	}
	return nil, false
}

// VideoCancel cancels a Seedance task that is still queued (fork, TXE-6426, contract C7 BF13).
// Ark's only cancel is DELETE on the task, which on a finished task deletes its record, so the
// task is queried once and DELETE is sent only while Ark reports it queued; in any other state
// the answer is 400 provider_cancel_unsupported and DELETE is never sent. A task Ark started
// between the query and the DELETE is refused by Ark, and that refusal is reported the same way.
func (provider *BytePlusProvider) VideoCancel(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostVideoCancelRequest) (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError) {
	return providerUtils.CancelVideoWhileQueued(
		func() (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError) {
			return provider.VideoRetrieve(ctx, key, &schemas.BifrostVideoRetrieveRequest{Provider: request.Provider, ID: request.ID})
		},
		func() *schemas.BifrostError {
			_, bifrostErr := provider.VideoDelete(ctx, key, &schemas.BifrostVideoDeleteRequest{Provider: request.Provider, ID: request.ID})
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
