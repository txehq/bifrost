package kling

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	schemas "github.com/capsohq/bifrost/core/schemas"
)

// klingLinkLifetime is how long Kling keeps a generated result: "Generated results are retained
// for 30 days" (task object, outputs[].url). expires_at is the completion time plus this.
const klingLinkLifetime = 30 * 24 * time.Hour

// klingVideoContentType is the type of a generated video. Kling documents no container choice.
const klingVideoContentType = "video/mp4"

// klingVideoGenerationFields are the unified video create fields this provider delivers (contract
// C4, C7). Core refuses any other unified field, and any task control field not listed here
// (expires_after_seconds, output_storage_uri), with caller_unsupported_parameter before a key is
// chosen. seed is exempt from core's check, so validateKlingVideoRequest refuses it.
var klingVideoGenerationFields = []string{
	"seconds", "resolution", "aspect_ratio", "audio", "last_frame",
	schemas.TaskControlFieldCallbackURL, schemas.TaskControlFieldClientReferenceID,
}

// klingEndpoint is one create path and the tiers its settings take, from the model's reference
// page. A nil list means the path has no such setting, so the unified field cannot be delivered.
type klingEndpoint struct {
	path             string
	promptInContents bool            // the prompt is a contents item, not the top-level prompt
	durations        []int           // settings.duration
	resolutions      []string        // settings.resolution
	aspectRatios     []string        // settings.aspect_ratio
	audio            map[bool]string // unified audio true/false -> settings.audio
}

// klingModel is the create paths of one model. A model served by the omni path takes text and
// frame images on that one path; the others split by input kind.
type klingModel struct {
	textToVideo  *klingEndpoint
	imageToVideo *klingEndpoint
	omni         *klingEndpoint
}

var (
	klingDurations3To15  = []int{3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	klingDurations3To10  = []int{3, 4, 5, 6, 7, 8, 9, 10}
	klingDurations5Or10  = []int{5, 10}
	klingResolutionsTo4K = []string{"720p", "1080p", "4k"}
	klingResolutionsHD   = []string{"720p", "1080p"}
	klingAspectRatios    = []string{"16:9", "9:16", "1:1"}
	// audio "native" is sound generated with the video; "original" keeps the sound of a
	// reference video, the only audio Kling O1 has.
	klingAudioNative   = map[bool]string{true: "native", false: "off"}
	klingAudioOriginal = map[bool]string{true: "original", false: "off"}
)

// klingModels are the models this provider serves, by the model name in Kling's paths. A model
// that is not here is refused: its tiers are unknown, so an out-of-tier value could not be
// refused (contract C4 agreement three). Kling 2.1 has no page on the new design standard.
var klingModels = map[string]klingModel{
	"kling-3.0": {
		textToVideo: &klingEndpoint{
			path: "/text-to-video/kling-3.0", durations: klingDurations3To15, resolutions: klingResolutionsTo4K,
			aspectRatios: klingAspectRatios, audio: klingAudioNative,
		},
		imageToVideo: &klingEndpoint{
			path: "/image-to-video/kling-3.0", promptInContents: true, durations: klingDurations3To15,
			resolutions: klingResolutionsTo4K, audio: klingAudioNative,
		},
	},
	"kling-2.5-turbo": {
		textToVideo: &klingEndpoint{
			path: "/text-to-video/kling-2.5-turbo", durations: klingDurations5Or10, resolutions: klingResolutionsHD,
			aspectRatios: klingAspectRatios,
		},
		imageToVideo: &klingEndpoint{
			path: "/image-to-video/kling-2.5-turbo", promptInContents: true, durations: klingDurations5Or10,
			resolutions: klingResolutionsHD,
		},
	},
	"kling-3.0-omni": {
		omni: &klingEndpoint{
			path: "/omni-video/kling-3.0-omni", promptInContents: true, durations: klingDurations3To15,
			resolutions: klingResolutionsTo4K, aspectRatios: klingAspectRatios, audio: klingAudioNative,
		},
	},
	"kling-o1": {
		omni: &klingEndpoint{
			path: "/omni-video/kling-o1", promptInContents: true, durations: klingDurations3To10,
			resolutions: klingResolutionsHD, aspectRatios: klingAspectRatios, audio: klingAudioOriginal,
		},
	},
}

// klingModelNames lists the served models, sorted, for error messages.
func klingModelNames() []string {
	names := make([]string, 0, len(klingModels))
	for name := range klingModels {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// klingEndpointFor picks the create path of a request: the omni path for an omni model, else
// image-to-video when the request has a first frame and text-to-video when it has none.
func klingEndpointFor(request *schemas.BifrostVideoGenerationRequest) (*klingEndpoint, *schemas.BifrostError) {
	model, ok := klingModels[strings.ToLower(strings.TrimSpace(request.Model))]
	if !ok {
		bifrostErr := providerUtils.NewBifrostBadRequestError(fmt.Sprintf(
			"kling: model %q is not served by this provider (served: %s)", request.Model, strings.Join(klingModelNames(), ", ")))
		bifrostErr.ExtraFields.ErrorType = schemas.ErrorTypeCallerModelUnknown
		return nil, bifrostErr
	}
	if model.omni != nil {
		return model.omni, nil
	}
	if klingFirstFrame(request) != "" {
		return model.imageToVideo, nil
	}
	return model.textToVideo, nil
}

// klingFirstFrame returns the request's first frame (input_reference), or "".
func klingFirstFrame(request *schemas.BifrostVideoGenerationRequest) string {
	if request == nil || request.Input == nil || request.Input.InputReference == nil {
		return ""
	}
	return strings.TrimSpace(*request.Input.InputReference)
}

// klingSettingsKeys are the settings fields the converter writes from unified fields. An extra
// field of the same name would overwrite the converted value after governance and pricing read
// the request, so it is refused.
var klingSettingsKeys = []string{"duration", "resolution", "aspect_ratio", "audio"}

// klingBodyContainers are the fields of the create body the converter builds, and "model", which
// Kling takes in the path. An extra field named after one would land inside settings as a value
// Kling does not define there; an extra model is what a caller would send to reach a model its
// key does not allow, and is refused like every other provider refuses it. The other names a
// vendor may read the model under, and stream, are refused from the shared list
// (providerUtils.ExtraParamUnsettableField), because this converter places extra fields itself
// and so never passes through MergeExtraParamsIntoJSON, which applies that list elsewhere.
var klingBodyContainers = []string{"model", "prompt", "contents", "settings", "options"}

// klingWatermarkExtra is the extra field that asks for a watermarked result
// (options.watermark_info.enabled).
const klingWatermarkExtra = "watermark"

// validateKlingVideoRequest refuses, before any upstream call, every value this request carries
// that Kling cannot take as given on the chosen path (contract C4 agreement three): Bifrost never
// drops such a field, changes it or clamps it into range.
func validateKlingVideoRequest(request *schemas.BifrostVideoGenerationRequest, endpoint *klingEndpoint) *schemas.BifrostError {
	if request.Input != nil && request.Input.VideoURI != nil && strings.TrimSpace(*request.Input.VideoURI) != "" {
		return schemas.NewUnsupportedParameterError("video_uri", "kling: a source video (video_uri) is not supported by this provider")
	}
	params := request.Params
	if params == nil {
		return nil
	}
	if params.Seed != nil {
		return schemas.NewUnsupportedParameterError("seed", "kling: the video API has no seed parameter")
	}
	if params.LastFrame != "" && klingFirstFrame(request) == "" {
		return schemas.NewUnsupportedParameterError("last_frame",
			"kling: last_frame needs a first frame (input_reference); last-frame-only generation is not supported")
	}
	if params.Seconds != nil && *params.Seconds != "" {
		if _, bifrostErr := klingDuration(*params.Seconds, endpoint); bifrostErr != nil {
			return bifrostErr
		}
	}
	if params.Resolution != "" {
		if _, bifrostErr := klingResolution(string(params.Resolution), endpoint); bifrostErr != nil {
			return bifrostErr
		}
	}
	if params.AspectRatio != "" {
		if endpoint.aspectRatios == nil {
			return schemas.NewUnsupportedParameterError("aspect_ratio", fmt.Sprintf(
				"kling: %s takes no aspect_ratio; the output follows the first frame", endpoint.path))
		}
		if !slices.Contains(endpoint.aspectRatios, string(params.AspectRatio)) {
			return schemas.NewUnsupportedParameterError("aspect_ratio", fmt.Sprintf(
				"kling: aspect_ratio %q is not one of %s for %s", params.AspectRatio, strings.Join(endpoint.aspectRatios, ", "), endpoint.path))
		}
	}
	if params.Audio != nil && endpoint.audio == nil {
		return schemas.NewUnsupportedParameterError("audio", fmt.Sprintf("kling: %s has no audio setting", endpoint.path))
	}
	return validateKlingExtraParams(params)
}

// klingDuration converts the unified seconds to settings.duration. Kling takes whole seconds from
// a fixed list per path; anything else is refused, never rounded.
func klingDuration(seconds string, endpoint *klingEndpoint) (int, *schemas.BifrostError) {
	duration, err := strconv.Atoi(strings.TrimSpace(seconds))
	if err != nil || !slices.Contains(endpoint.durations, duration) {
		allowed := make([]string, len(endpoint.durations))
		for i, d := range endpoint.durations {
			allowed[i] = strconv.Itoa(d)
		}
		return 0, schemas.NewUnsupportedParameterError("seconds", fmt.Sprintf(
			"kling: seconds %q is not one of %s for %s", seconds, strings.Join(allowed, ", "), endpoint.path))
	}
	return duration, nil
}

// klingResolution converts the unified resolution tier to settings.resolution, in Kling's
// spelling ("4K" is sent as "4k"). A tier the path does not offer is refused.
func klingResolution(resolution string, endpoint *klingEndpoint) (string, *schemas.BifrostError) {
	tier := strings.ToLower(strings.TrimSpace(resolution))
	if !slices.Contains(endpoint.resolutions, tier) {
		return "", schemas.NewUnsupportedParameterError("resolution", fmt.Sprintf(
			"kling: resolution %q is not one of %s for %s", resolution, strings.Join(endpoint.resolutions, ", "), endpoint.path))
	}
	return tier, nil
}

// validateKlingExtraParams refuses the extra fields the converter cannot place. The handlers keep
// a copy of each unified field under its own name in the extra params (see
// schemas.UnifiedVideoGenerationFields); that copy is skipped when the typed field is set, and
// refused when it is not, which means the caller sent a value of another JSON type.
func validateKlingExtraParams(params *schemas.VideoGenerationParameters) *schemas.BifrostError {
	for name, value := range params.ExtraParams {
		lower := strings.ToLower(name)
		switch {
		case slices.Contains(schemas.UnifiedVideoGenerationFields, name):
			if klingUnifiedFieldIsSet(params, name) {
				continue
			}
			return schemas.NewUnsupportedParameterError(name, fmt.Sprintf("kling: %s must be a string", name))
		case lower == klingWatermarkExtra:
			if _, ok := value.(bool); !ok {
				return schemas.NewUnsupportedParameterError(name, "kling: watermark must be true or false")
			}
		case providerUtils.ExtraParamUnsettableField(name) != "":
			return newKlingExtraParamError(name, fmt.Sprintf(
				"kling: extra params cannot set %q: %s is decided by the gateway from the request, never by an extra param",
				name, providerUtils.ExtraParamUnsettableField(name)))
		case providerUtils.ExtraParamNamesField(name, klingSettingsKeys...):
			return newKlingExtraParamError(name, fmt.Sprintf(
				"kling: extra params cannot set %q; use the request's own seconds, resolution, aspect_ratio or audio field", name))
		case providerUtils.ExtraParamNamesField(name, klingBodyContainers...):
			return newKlingExtraParamError(name, fmt.Sprintf(
				"kling: extra params cannot set %q; extra fields are merged into settings", name))
		}
	}
	return nil
}

// klingUnifiedFieldIsSet reports whether the typed unified field of that JSON name is set.
func klingUnifiedFieldIsSet(params *schemas.VideoGenerationParameters, name string) bool {
	switch name {
	case "aspect_ratio":
		return params.AspectRatio != ""
	case "resolution":
		return params.Resolution != ""
	case "last_frame":
		return params.LastFrame != ""
	case "reference_images":
		return len(params.ReferenceImages) > 0
	default:
		return false
	}
}

// newKlingExtraParamError is a 400 for an extra field the converter owns.
func newKlingExtraParamError(name, message string) *schemas.BifrostError {
	bifrostErr := providerUtils.NewBifrostBadRequestError(message)
	bifrostErr.Error.Param = name
	bifrostErr.ExtraFields.ErrorType = schemas.ErrorTypeCallerInvalidRequest
	return bifrostErr
}

// klingImageInput returns an input image in the form Kling takes: a URL unchanged, or the Base64
// payload of a data URL. Kling's image fields take "URL or Base64", the bare payload, so the
// data: prefix is removed (contract C4 agreement four).
func klingImageInput(image string) string {
	image = strings.TrimSpace(image)
	if !strings.HasPrefix(strings.ToLower(image), "data:") {
		return image
	}
	if _, payload, ok := strings.Cut(image, ";base64,"); ok {
		return payload
	}
	return image
}

// ToKlingVideoGenerationRequest converts a unified video create request to the body of the Kling
// create path its model and inputs select. A request Kling cannot take as given is refused with
// the same error VideoGeneration answers.
//
//	prompt              -> prompt (text-to-video) or contents[{type: prompt}]
//	input_reference     -> contents[{type: first_frame}]
//	last_frame          -> contents[{type: last_frame}]
//	seconds             -> settings.duration
//	resolution          -> settings.resolution
//	aspect_ratio        -> settings.aspect_ratio
//	audio               -> settings.audio ("native" or "original" by model, or "off")
//	callback_url        -> options.callback_url
//	client_reference_id -> options.external_task_id
//	extra "watermark"   -> options.watermark_info.enabled
//	other extra fields  -> settings.<name>
//
// negative_prompt has no Kling field (the prompt itself carries negative descriptions) and is not
// sent; contract C4 exempts it from the refusal rule.
func ToKlingVideoGenerationRequest(request *schemas.BifrostVideoGenerationRequest) (*KlingVideoRequest, error) {
	if request == nil {
		return nil, providerUtils.InvalidRequestErrorf("prompt is required")
	}
	endpoint, bifrostErr := klingEndpointFor(request)
	if bifrostErr == nil {
		bifrostErr = validateKlingVideoRequest(request, endpoint)
	}
	if bifrostErr != nil {
		return nil, providerUtils.InvalidRequestErrorf("%s", bifrostErr.Error.Message)
	}
	return toKlingVideoRequest(request, endpoint)
}

// toKlingVideoRequest builds the body for a request that has passed validateKlingVideoRequest
// for endpoint: it sends every value as given.
func toKlingVideoRequest(request *schemas.BifrostVideoGenerationRequest, endpoint *klingEndpoint) (*KlingVideoRequest, error) {
	if request == nil || request.Input == nil {
		return nil, providerUtils.InvalidRequestErrorf("prompt is required")
	}
	prompt := request.Input.Prompt
	firstFrame := klingFirstFrame(request)
	body := &KlingVideoRequest{}
	if endpoint.promptInContents {
		if strings.TrimSpace(prompt) != "" {
			body.Contents = append(body.Contents, KlingContent{Type: klingContentPrompt, Text: prompt})
		}
		if firstFrame != "" {
			body.Contents = append(body.Contents, KlingContent{Type: klingContentFirstFrame, URL: klingImageInput(firstFrame)})
		}
		if len(body.Contents) == 0 {
			return nil, providerUtils.InvalidRequestErrorf("prompt or input_reference is required")
		}
	} else {
		if strings.TrimSpace(prompt) == "" {
			return nil, providerUtils.InvalidRequestErrorf("prompt is required")
		}
		body.Prompt = prompt
	}

	params := request.Params
	if params == nil {
		return body, nil
	}
	if params.LastFrame != "" {
		body.Contents = append(body.Contents, KlingContent{Type: klingContentLastFrame, URL: klingImageInput(string(params.LastFrame))})
	}

	settings := make(map[string]interface{}, 4+len(params.ExtraParams))
	options := &KlingOptions{CallbackURL: params.CallbackURL, ExternalTaskID: params.ClientReferenceID}
	for name, value := range params.ExtraParams {
		switch {
		case slices.Contains(schemas.UnifiedVideoGenerationFields, name):
			// The handlers' copy of a unified field; the typed field below is the one sent.
		case strings.ToLower(name) == klingWatermarkExtra:
			if enabled, ok := value.(bool); ok {
				options.WatermarkInfo = &KlingWatermarkInfo{Enabled: enabled}
			}
		default:
			settings[name] = value
		}
	}
	if params.Seconds != nil && *params.Seconds != "" {
		duration, bifrostErr := klingDuration(*params.Seconds, endpoint)
		if bifrostErr != nil {
			return nil, providerUtils.InvalidRequestErrorf("%s", bifrostErr.Error.Message)
		}
		settings["duration"] = duration
	}
	if params.Resolution != "" {
		resolution, bifrostErr := klingResolution(string(params.Resolution), endpoint)
		if bifrostErr != nil {
			return nil, providerUtils.InvalidRequestErrorf("%s", bifrostErr.Error.Message)
		}
		settings["resolution"] = resolution
	}
	if params.AspectRatio != "" {
		settings["aspect_ratio"] = string(params.AspectRatio)
	}
	if params.Audio != nil && endpoint.audio != nil {
		settings["audio"] = endpoint.audio[*params.Audio]
	}
	if len(settings) > 0 {
		body.Settings = settings
	}
	if options.CallbackURL != nil || options.ExternalTaskID != nil || options.WatermarkInfo != nil {
		body.Options = options
	}
	return body, nil
}

// klingVideoStatuses maps Kling's task status strings to the task object's status, one to one
// (contract C7). Kling has no cancelled or expired state.
var klingVideoStatuses = providerUtils.VideoStatusTable{
	"submitted":  schemas.VideoStatusQueued,
	"processing": schemas.VideoStatusInProgress,
	"succeeded":  schemas.VideoStatusCompleted,
	"failed":     schemas.VideoStatusFailed,
}

// klingRiskControlMarkers are the phrases of a failed task's message that mark a content block.
// Kling's task object has no failure code: its reference says only that message shows the reason
// "such as triggering the content risk control of the platform", and the error code table calls
// the same event the "content security policy". The exact strings Kling writes are not
// documented, so the match is on those two documented phrases and their Chinese forms.
var klingRiskControlMarkers = []string{"risk control", "content security", "风控", "内容安全"}

// klingIsRiskControlFailure reports whether a failed task's message says the platform's content
// risk control blocked it.
func klingIsRiskControlFailure(message string) bool {
	lower := strings.ToLower(message)
	for _, marker := range klingRiskControlMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// klingMillisToSeconds converts Kling's Unix milliseconds to Unix seconds.
func klingMillisToSeconds(millis int64) int64 {
	return millis / 1000
}

// ToBifrostVideoGenerationResponse converts Kling's task object to the unified task object. The
// create response, both queries and the list share it.
//
// Usage holds only what Kling reported: usage.output_seconds is the sum of outputs[].duration of
// the generated videos, and usage.vendor_cost is the Units of the billing[] records charged to a
// resource package (charge_type "unit"), unit "unit". A balance deduction (charge_type "cash") is
// money, not Units, and stays in the raw response. Nothing is filled from the request.
func ToBifrostVideoGenerationResponse(task *KlingTask) (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError) {
	if task == nil {
		return nil, providerUtils.NewBifrostOperationError("kling task is nil", nil)
	}
	resp := &schemas.BifrostVideoGenerationResponse{
		ID:     task.ID,
		Object: "video",
	}
	if task.CreateTime > 0 {
		resp.CreatedAt = klingMillisToSeconds(task.CreateTime)
	}
	klingVideoStatuses.Apply(resp, task.Status)

	switch resp.Status {
	case schemas.VideoStatusCompleted:
		if task.UpdateTime > 0 {
			resp.CompletedAt = schemas.Ptr(klingMillisToSeconds(task.UpdateTime))
		}
	case schemas.VideoStatusFailed:
		// Kling's task object carries a failure message and no failure code.
		resp.Error = &schemas.VideoCreateError{Message: task.Message}
		if klingIsRiskControlFailure(task.Message) {
			resp.ContentFilter = &schemas.ContentFilterInfo{FilteredCount: 1, Reasons: []string{task.Message}}
		}
	}

	var outputSeconds float64
	var reportedSeconds bool
	for i := range task.Outputs {
		output := &task.Outputs[i]
		if output.Type != klingOutputVideo || output.URL == "" {
			continue
		}
		resp.Videos = append(resp.Videos, schemas.VideoOutput{
			ID:          output.ID,
			Type:        schemas.VideoOutputTypeURL,
			URL:         schemas.Ptr(output.URL),
			ContentType: klingVideoContentType,
		})
		if seconds, err := strconv.ParseFloat(strings.TrimSpace(output.Duration), 64); err == nil && seconds > 0 {
			outputSeconds += seconds
			reportedSeconds = true
			if resp.Seconds == nil {
				resp.Seconds = schemas.Ptr(strings.TrimSpace(output.Duration))
			}
		}
	}
	providerUtils.SetVideoExpiresAt(resp, nil, klingLinkLifetime)

	usage := &schemas.VideoUsage{}
	if reportedSeconds {
		usage.OutputSeconds = schemas.Ptr(outputSeconds)
	}
	usage.VendorCost = klingUnitsCharged(task.Billing)
	if usage.OutputSeconds != nil || usage.VendorCost != nil {
		resp.Usage = usage
	}
	return resp, nil
}

// klingUnitsCharged sums the Units of the billing records charged to a resource package. It is
// nil when Kling reported no such record, and nil when any of them has an amount that is not a
// number: a partial sum would under-report the bill.
func klingUnitsCharged(billing []KlingBilling) *schemas.VendorCost {
	var units float64
	var charged bool
	for i := range billing {
		if billing[i].ChargeType != klingChargeTypeUnit {
			continue
		}
		amount, err := strconv.ParseFloat(strings.TrimSpace(billing[i].Amount), 64)
		if err != nil {
			return nil
		}
		units += amount
		charged = true
	}
	if !charged {
		return nil
	}
	return &schemas.VendorCost{Amount: units, Unit: schemas.VendorCostUnitUnit}
}

// ToBifrostVideoObject converts a listed Kling task to a video list entry.
func ToBifrostVideoObject(task *KlingTask, providerName schemas.ModelProvider) schemas.VideoObject {
	obj := schemas.VideoObject{
		ID:     providerUtils.AddVideoIDProviderSuffix(task.ID, providerName),
		Object: "video",
	}
	resp, bifrostErr := ToBifrostVideoGenerationResponse(task)
	if bifrostErr != nil || resp == nil {
		return obj
	}
	obj.Status = resp.Status
	obj.CreatedAt = resp.CreatedAt
	obj.CompletedAt = resp.CompletedAt
	obj.ExpiresAt = resp.ExpiresAt
	obj.Seconds = resp.Seconds
	obj.Error = resp.Error
	return obj
}
