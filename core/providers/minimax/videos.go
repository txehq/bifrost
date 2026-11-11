package minimax

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

// MiniMax video generation (fork, TXE-6428, contract C3–C7, C9).
//
// MiniMax has two video APIs. v2 (/v2/video_generation, the MiniMax-H3 models) takes a content
// array and reports usage on the task. v1 (/v1/video_generation, the MiniMax-Hailuo models) takes
// flat fields, reports no usage, and delivers the artifact as a file whose download_url comes from
// /v1/files/retrieve and lives one hour. A v1 task's id carries miniMaxV1TaskIDPrefix, so a query,
// download or cancel, which carry only the id, reach the right API.

const (
	miniMaxDefaultVideoDuration = 6
	miniMaxVideoMaxDuration     = 15
	miniMaxDefaultVideoTier     = "768P"

	// miniMaxV1DefaultVideoDuration is the duration /v1/video_generation runs when none is sent.
	miniMaxV1DefaultVideoDuration = 6

	// miniMaxV1TaskIDPrefix marks a v1 (Hailuo) task in the id Bifrost returns:
	// "v1-<MiniMax task id>:minimax". v1 and v2 task ids are both numeric strings, so without the
	// mark a query could not tell which API to ask.
	miniMaxV1TaskIDPrefix = "v1-"

	// miniMaxV1LinkLifetime is how long a v1 download_url lives (MiniMax: "valid for 1 hour").
	// MiniMax does not state a lifetime for a v2 content.url, so v2 sets no expires_at.
	miniMaxV1LinkLifetime = time.Hour
)

// miniMaxVideoFields are the unified video create fields MiniMax takes, plus the one task control
// field it forwards (callback_url → callback_url, contract C7). Core refuses every other unified
// field with caller_unsupported_parameter before a key is chosen; the converters refuse again the
// ones a v1 model cannot take.
var miniMaxVideoFields = []string{
	"aspect_ratio",
	"last_frame",
	"reference_images",
	"resolution",
	"seconds",
	"size",
	schemas.TaskControlFieldCallbackURL,
}

// SupportedUnifiedFields declares the unified fields MiniMax video generation delivers (contract C4
// BF4, C7 BF12).
func (provider *MiniMaxProvider) SupportedUnifiedFields(requestType schemas.RequestType) ([]string, bool) {
	if requestType != schemas.VideoGenerationRequest {
		return nil, false
	}
	return miniMaxVideoFields, true
}

// miniMaxVideoRatios are the concrete ratios /v2/video_generation accepts besides "adaptive".
var miniMaxVideoRatios = []struct {
	name  string
	ratio float64
}{
	{"21:9", 21.0 / 9},
	{"16:9", 16.0 / 9},
	{"4:3", 4.0 / 3},
	{"1:1", 1},
	{"3:4", 3.0 / 4},
	{"9:16", 9.0 / 16},
}

// miniMaxV2VideoTiers are the resolution tiers /v2/video_generation accepts.
var miniMaxV2VideoTiers = []string{"480P", "768P", "2K"}

// miniMaxV2VideoStatuses maps a v2 task's status one to one (contract C7 BF11).
var miniMaxV2VideoStatuses = providerUtils.VideoStatusTable{
	"queued":    schemas.VideoStatusQueued,
	"running":   schemas.VideoStatusInProgress,
	"succeeded": schemas.VideoStatusCompleted,
	"failed":    schemas.VideoStatusFailed,
	"cancelled": schemas.VideoStatusCancelled,
}

// miniMaxV1VideoStatuses maps a v1 query's status. Preparing comes before Queueing; both are queued.
var miniMaxV1VideoStatuses = providerUtils.VideoStatusTable{
	"Preparing":  schemas.VideoStatusQueued,
	"Queueing":   schemas.VideoStatusQueued,
	"Processing": schemas.VideoStatusInProgress,
	"Success":    schemas.VideoStatusCompleted,
	"Fail":       schemas.VideoStatusFailed,
}

func isMiniMaxH3MaxModel(model string) bool {
	return strings.EqualFold(model, "MiniMax-H3-Max")
}

// isMiniMaxV1VideoModel reports whether model is served by /v1/video_generation: the Hailuo models
// (MiniMax-Hailuo-2.3, MiniMax-Hailuo-2.3-Fast, MiniMax-Hailuo-02).
func isMiniMaxV1VideoModel(model string) bool {
	return strings.HasPrefix(strings.ToLower(model), "minimax-hailuo-")
}

// miniMaxV1TaskID returns the MiniMax task id of a v1 task and true when taskID (the Bifrost id
// without its provider suffix) names one.
func miniMaxV1TaskID(taskID string) (string, bool) {
	return strings.CutPrefix(taskID, miniMaxV1TaskIDPrefix)
}

// checkMiniMaxVideoParams refuses the unified fields MiniMax cannot deliver. Core runs the same
// declaration check before a key is chosen; repeating it here keeps the converter safe when it is
// called directly. seed is exempt from the core check (it does not change the price), but MiniMax
// has no seed, so it is refused too rather than dropped (contract C4, ticket item B8).
func checkMiniMaxVideoParams(bifrostReq *schemas.BifrostVideoGenerationRequest, supported []string) error {
	params := bifrostReq.Params
	if params == nil {
		return nil
	}
	if params.Seed != nil {
		return providerUtils.ConverterBifrostError(schemas.NewUnsupportedParameterError("seed", "minimax video generation has no seed parameter"))
	}
	br := &schemas.BifrostRequest{RequestType: schemas.VideoGenerationRequest, VideoGenerationRequest: bifrostReq}
	if bifrostErr := schemas.CheckUnifiedFields(br, supported); bifrostErr != nil {
		return providerUtils.ConverterBifrostError(bifrostErr)
	}
	return nil
}

// miniMaxVideoExtraParams copies the request's extra params for the extra-field channel (merged into
// the vendor body unchanged, contract C4 agreement two). A unified field the handler also kept as an
// extra param under its own name is removed once the typed field carries it, so it is sent once, in
// the vendor's own field. An extra param that would overwrite a field Bifrost builds is refused.
func miniMaxVideoExtraParams(params *schemas.VideoGenerationParameters, builtFields []string) (map[string]interface{}, error) {
	if params == nil || len(params.ExtraParams) == 0 {
		return nil, nil
	}
	extra := make(map[string]interface{}, len(params.ExtraParams))
	maps.Copy(extra, params.ExtraParams)
	typed := map[string]bool{
		"aspect_ratio":     params.AspectRatio != "",
		"resolution":       params.Resolution != "",
		"last_frame":       params.LastFrame != "",
		"reference_images": len(params.ReferenceImages) > 0,
	}
	for name, set := range typed {
		if set {
			delete(extra, name)
		}
	}
	// ratio is MiniMax's own name for aspect_ratio. Core lets it through only beside an
	// aspect_ratio of the same value (schemas.VendorPriceFieldNames, TXE-7065), which the
	// typed Ratio already carries, so it is not sent twice.
	if _, ok := extra["ratio"]; ok && params.AspectRatio != "" {
		delete(extra, "ratio")
	}
	for key := range extra {
		for _, name := range builtFields {
			if providerUtils.ExtraParamNamesField(key, name) {
				return nil, providerUtils.ExtraParamConflictError(key, name)
			}
		}
	}
	return extra, nil
}

// parseMiniMaxVideoSeconds reads the unified seconds as a whole number of seconds. A fractional or
// out-of-range value is refused, never rounded or clamped (contract C4 agreement three).
func parseMiniMaxVideoSeconds(seconds string, allowed func(int) bool, rangeText string) (int, error) {
	value, err := strconv.ParseFloat(strings.TrimSpace(seconds), 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value != math.Trunc(value) || !allowed(int(value)) {
		return 0, providerUtils.ConverterBifrostError(schemas.NewUnsupportedParameterError("seconds",
			fmt.Sprintf("seconds %q is not a duration this model takes (%s)", seconds, rangeText)))
	}
	return int(value), nil
}

// sanitizedVideoImage checks a frame or reference image the way every image input is checked.
func sanitizedVideoImage(value, name string) (string, error) {
	sanitized, err := schemas.SanitizeImageURL(value)
	if err != nil {
		return "", providerUtils.InvalidRequestErrorf("invalid %s: %v", name, err)
	}
	return sanitized, nil
}

// ToMiniMaxVideoGenerationRequest converts a Bifrost video generation request to MiniMax's
// /v2/video_generation body. Every price-affecting field reaches MiniMax as given or is refused:
// seconds is not clamped, size must be exactly one tier and ratio, and no ratio is written when the
// caller gave none (MiniMax then uses adaptive).
func ToMiniMaxVideoGenerationRequest(bifrostReq *schemas.BifrostVideoGenerationRequest) (*MiniMaxVideoGenerationRequest, error) {
	if bifrostReq == nil || bifrostReq.Input == nil || strings.TrimSpace(bifrostReq.Input.Prompt) == "" {
		return nil, providerUtils.InvalidRequestErrorf("prompt is required")
	}
	if bifrostReq.Input.VideoURI != nil && *bifrostReq.Input.VideoURI != "" {
		return nil, providerUtils.InvalidRequestErrorf("video_uri is not supported by minimax video generation")
	}
	if err := checkMiniMaxVideoParams(bifrostReq, miniMaxVideoFields); err != nil {
		return nil, err
	}

	req := &MiniMaxVideoGenerationRequest{
		Model:    bifrostReq.Model,
		Content:  []MiniMaxVideoContentItem{{Type: "text", Text: bifrostReq.Input.Prompt}},
		Duration: miniMaxDefaultVideoDuration,
	}
	hasFrameImage := false
	if bifrostReq.Input.InputReference != nil && *bifrostReq.Input.InputReference != "" {
		url, err := sanitizedVideoImage(*bifrostReq.Input.InputReference, "input reference")
		if err != nil {
			return nil, err
		}
		req.Content = append(req.Content, MiniMaxVideoContentItem{Type: "image_url", Role: "first_frame", ImageURL: &MiniMaxVideoMediaURL{URL: url}})
		hasFrameImage = true
	}

	minDuration := 4
	if isMiniMaxH3MaxModel(bifrostReq.Model) {
		minDuration = 5
	}
	params := bifrostReq.Params
	if params == nil {
		req.Resolution = miniMaxDefaultVideoTier
		return req, nil
	}

	if params.Seconds != nil && *params.Seconds != "" {
		duration, err := parseMiniMaxVideoSeconds(*params.Seconds,
			func(d int) bool { return d >= minDuration && d <= miniMaxVideoMaxDuration },
			fmt.Sprintf("%d to %d", minDuration, miniMaxVideoMaxDuration))
		if err != nil {
			return nil, err
		}
		req.Duration = duration
	}
	if params.Resolution != "" {
		req.Resolution = strings.ToUpper(strings.TrimSpace(string(params.Resolution)))
	}
	if params.AspectRatio != "" {
		req.Ratio = schemas.Ptr(strings.TrimSpace(string(params.AspectRatio)))
	}
	if params.Size != "" {
		tier, ratio, ok := miniMaxVideoTierForSize(params.Size)
		if !ok {
			return nil, providerUtils.ConverterBifrostError(schemas.NewUnsupportedParameterError("size",
				fmt.Sprintf("size %q is not exactly one MiniMax resolution tier (480P, 768P, 2K) at one ratio (21:9, 16:9, 4:3, 1:1, 3:4, 9:16); send resolution and aspect_ratio instead", params.Size)))
		}
		if req.Resolution != "" && req.Resolution != tier {
			return nil, providerUtils.ConverterBifrostError(schemas.NewUnsupportedParameterError("size",
				fmt.Sprintf("size %q is the %s tier but resolution is %s", params.Size, tier, req.Resolution)))
		}
		if req.Ratio != nil && *req.Ratio != ratio {
			return nil, providerUtils.ConverterBifrostError(schemas.NewUnsupportedParameterError("size",
				fmt.Sprintf("size %q is ratio %s but aspect_ratio is %s", params.Size, ratio, *req.Ratio)))
		}
		req.Resolution, req.Ratio = tier, schemas.Ptr(ratio)
	}
	if req.Resolution == "" {
		req.Resolution = miniMaxDefaultVideoTier
	}
	if params.LastFrame != "" {
		url, err := sanitizedVideoImage(string(params.LastFrame), "last_frame")
		if err != nil {
			return nil, err
		}
		req.Content = append(req.Content, MiniMaxVideoContentItem{Type: "image_url", Role: "last_frame", ImageURL: &MiniMaxVideoMediaURL{URL: url}})
		hasFrameImage = true
	}
	if len(params.ReferenceImages) > 0 {
		if hasFrameImage {
			return nil, providerUtils.InvalidRequestErrorf("reference_images cannot be combined with first or last frame images")
		}
		for _, ref := range params.ReferenceImages {
			url, err := sanitizedVideoImage(ref, "reference image")
			if err != nil {
				return nil, err
			}
			req.Content = append(req.Content, MiniMaxVideoContentItem{Type: "image_url", Role: "reference_image", ImageURL: &MiniMaxVideoMediaURL{URL: url}})
		}
	}
	if params.CallbackURL != nil && *params.CallbackURL != "" {
		req.CallbackURL = schemas.Ptr(*params.CallbackURL)
	}
	extra, err := miniMaxVideoExtraParams(params, []string{"model", "content", "duration", "callback_url"})
	if err != nil {
		return nil, err
	}
	req.ExtraParams = extra
	return req, nil
}

// miniMaxVideoTierForSize returns the v2 tier and ratio whose "WxH" (as miniMaxVideoSize reports a
// task's size) is exactly size.
func miniMaxVideoTierForSize(size string) (string, string, bool) {
	w, h, err := parseImageSize(size)
	if err != nil {
		return "", "", false
	}
	exact, ok := miniMaxV2SizeTiers[[2]int{w, h}]
	return exact.tier, exact.ratio, ok
}

// miniMaxV2SizeTiers maps every exact v2 size (each tier at each ratio, as miniMaxVideoSize writes it) to
// its tier and ratio, built once so a request's size check is one map load.
var miniMaxV2SizeTiers = func() map[[2]int]struct{ tier, ratio string } {
	sizes := make(map[[2]int]struct{ tier, ratio string }, len(miniMaxV2VideoTiers)*len(miniMaxVideoRatios))
	for _, tier := range miniMaxV2VideoTiers {
		for _, r := range miniMaxVideoRatios {
			w, h, err := parseImageSize(miniMaxVideoSize(tier, r.name))
			if err == nil {
				sizes[[2]int{w, h}] = struct{ tier, ratio string }{tier, r.name}
			}
		}
	}
	return sizes
}()

// MiniMaxV1VideoGenerationRequest is the body for POST /v1/video_generation (the Hailuo models).
// prompt_optimizer, fast_pretreatment and other vendor switches arrive through the extra-field
// channel under MiniMax's own names.
type MiniMaxV1VideoGenerationRequest struct {
	Model           string                 `json:"model"`
	Prompt          string                 `json:"prompt,omitempty"`
	FirstFrameImage *string                `json:"first_frame_image,omitempty"`
	LastFrameImage  *string                `json:"last_frame_image,omitempty"`
	Duration        *int                   `json:"duration,omitempty"`
	Resolution      *string                `json:"resolution,omitempty"`
	CallbackURL     *string                `json:"callback_url,omitempty"`
	ExtraParams     map[string]interface{} `json:"-"`
}

func (r *MiniMaxV1VideoGenerationRequest) GetExtraParams() map[string]interface{} {
	return r.ExtraParams
}

// miniMaxV1VideoFields are the unified fields a v1 model takes: it has no ratio, size or reference
// images.
var miniMaxV1VideoFields = []string{"last_frame", "resolution", "seconds", schemas.TaskControlFieldCallbackURL}

// ToMiniMaxV1VideoGenerationRequest converts a Bifrost video generation request to MiniMax's
// /v1/video_generation body. duration and resolution are sent only when the caller gave them, so
// MiniMax applies its own defaults; seconds must be 6 or 10, the durations v1 offers.
func ToMiniMaxV1VideoGenerationRequest(bifrostReq *schemas.BifrostVideoGenerationRequest) (*MiniMaxV1VideoGenerationRequest, error) {
	if bifrostReq == nil || bifrostReq.Input == nil {
		return nil, providerUtils.InvalidRequestErrorf("prompt is required")
	}
	hasFirstFrame := bifrostReq.Input.InputReference != nil && *bifrostReq.Input.InputReference != ""
	if strings.TrimSpace(bifrostReq.Input.Prompt) == "" && !hasFirstFrame {
		return nil, providerUtils.InvalidRequestErrorf("prompt is required")
	}
	if bifrostReq.Input.VideoURI != nil && *bifrostReq.Input.VideoURI != "" {
		return nil, providerUtils.InvalidRequestErrorf("video_uri is not supported by minimax video generation")
	}
	if err := checkMiniMaxVideoParams(bifrostReq, miniMaxV1VideoFields); err != nil {
		return nil, err
	}

	req := &MiniMaxV1VideoGenerationRequest{Model: bifrostReq.Model, Prompt: bifrostReq.Input.Prompt}
	if hasFirstFrame {
		url, err := sanitizedVideoImage(*bifrostReq.Input.InputReference, "input reference")
		if err != nil {
			return nil, err
		}
		req.FirstFrameImage = &url
	}
	params := bifrostReq.Params
	if params == nil {
		return req, nil
	}
	if params.Seconds != nil && *params.Seconds != "" {
		duration, err := parseMiniMaxVideoSeconds(*params.Seconds, func(d int) bool { return d == 6 || d == 10 }, "6 or 10")
		if err != nil {
			return nil, err
		}
		req.Duration = &duration
	}
	if params.Resolution != "" {
		req.Resolution = schemas.Ptr(strings.ToUpper(strings.TrimSpace(string(params.Resolution))))
	}
	if params.LastFrame != "" {
		url, err := sanitizedVideoImage(string(params.LastFrame), "last_frame")
		if err != nil {
			return nil, err
		}
		req.LastFrameImage = &url
	}
	if params.CallbackURL != nil && *params.CallbackURL != "" {
		req.CallbackURL = schemas.Ptr(*params.CallbackURL)
	}
	extra, err := miniMaxVideoExtraParams(params, []string{"model", "prompt", "first_frame_image", "last_frame_image", "duration", "callback_url"})
	if err != nil {
		return nil, err
	}
	req.ExtraParams = extra
	return req, nil
}

// miniMaxVideoShortEdges maps MiniMax resolution tiers to their short edge in pixels.
var miniMaxVideoShortEdges = map[string]int{
	"480P":  480,
	"720P":  720,
	"768P":  768,
	"1080P": 1080,
	"2K":    1440,
}

// miniMaxVideoSize turns a task's resolution tier and ratio into the "WxH" size the video pricing
// bands read (a short edge of 480 selects the 480p rate). It returns "" when either is unknown, so
// settlement keeps the size captured from the request instead of an unparseable tier label.
func miniMaxVideoSize(resolution, ratio string) string {
	short, ok := miniMaxVideoShortEdges[strings.ToUpper(strings.TrimSpace(resolution))]
	if !ok {
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

// miniMaxContentFilterCode reports whether a MiniMax error code is a content block: 1026 (the input)
// or 1027 (the generated output).
func miniMaxContentFilterCode(code string) bool {
	code = strings.TrimSpace(code)
	return code == "1026" || code == "1027"
}

// ToBifrostVideoGenerationResponse converts a queried MiniMax v2 video task to the task object
// (contract C5, C7, C9): the raw status in provider_status, cancelled kept apart from failed, the
// usage MiniMax reported, and a content_filter when the task failed for a sensitivity code.
func ToBifrostVideoGenerationResponse(task *MiniMaxVideoTask) (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError) {
	if task == nil {
		return nil, providerUtils.NewBifrostOperationError("video task is nil", nil)
	}
	resp := &schemas.BifrostVideoGenerationResponse{
		ID:        task.ID,
		Object:    "video",
		Model:     task.Model,
		CreatedAt: task.CreatedAt,
		Size:      miniMaxVideoSize(task.Resolution, task.Ratio),
	}
	if task.Duration > 0 {
		resp.Seconds = schemas.Ptr(strconv.Itoa(task.Duration))
	}
	miniMaxV2VideoStatuses.Apply(resp, string(task.Status))
	switch resp.Status {
	case schemas.VideoStatusCompleted:
		if task.UpdatedAt > 0 {
			resp.CompletedAt = schemas.Ptr(task.UpdatedAt)
		}
	case schemas.VideoStatusFailed:
		resp.Error = &schemas.VideoCreateError{Code: string(task.Status), Message: "task " + string(task.Status)}
		if task.Error != nil {
			if task.Error.Code != "" {
				resp.Error.Code = task.Error.Code
			}
			if task.Error.Message != "" {
				resp.Error.Message = task.Error.Message
			}
			if miniMaxContentFilterCode(task.Error.Code) {
				resp.ContentFilter = &schemas.ContentFilterInfo{FilteredCount: 1, Reasons: []string{strings.TrimSpace(task.Error.Code)}}
			}
		}
	}
	if task.Content != nil {
		if task.Content.URL != "" {
			resp.Videos = []schemas.VideoOutput{{Type: schemas.VideoOutputTypeURL, URL: schemas.Ptr(task.Content.URL), ContentType: "video/mp4"}}
		}
		resp.Prompt = task.Content.Prompt
	}
	// MiniMax returns no expiry for a v2 content.url and documents no lifetime for it.
	providerUtils.SetVideoExpiresAt(resp, nil, 0)
	resp.Usage = miniMaxVideoUsage(task.Usage)
	return resp, nil
}

// miniMaxVideoUsage maps the usage MiniMax reported on a v2 task to the unified task usage. Only
// what MiniMax reported is set; nil when it reported none of the billed quantities.
func miniMaxVideoUsage(u *MiniMaxVideoTaskUsage) *schemas.VideoUsage {
	if u == nil {
		return nil
	}
	usage := &schemas.VideoUsage{}
	if u.OutputSeconds != nil {
		usage.OutputSeconds = schemas.Ptr(*u.OutputSeconds)
	}
	if u.InputSeconds != nil {
		usage.InputSeconds = schemas.Ptr(*u.InputSeconds)
	}
	if u.InputImageCount != nil {
		usage.InputImages = schemas.Ptr(*u.InputImageCount)
	}
	if u.CompletionTokens != nil {
		usage.OutputTokens = schemas.Ptr(*u.CompletionTokens)
	}
	if *usage == (schemas.VideoUsage{}) {
		return nil
	}
	return usage
}

// MiniMaxV1VideoQueryResponse is returned by GET /v1/query/video_generation?task_id=.
type MiniMaxV1VideoQueryResponse struct {
	TaskID      string           `json:"task_id"`
	Status      string           `json:"status"`
	FileID      MiniMaxFileID    `json:"file_id"`
	VideoWidth  int              `json:"video_width"`
	VideoHeight int              `json:"video_height"`
	BaseResp    *MiniMaxBaseResp `json:"base_resp,omitempty"`
}

// MiniMaxFileID is a MiniMax file id. The v1 query returns it as a string and /v1/files/retrieve as
// a number, so it decodes either.
type MiniMaxFileID string

// UnmarshalJSON implements json.Unmarshaler; see MiniMaxFileID.
func (id *MiniMaxFileID) UnmarshalJSON(data []byte) error {
	raw := strings.TrimSpace(string(data))
	if raw == "null" {
		*id = ""
		return nil
	}
	if unquoted, err := strconv.Unquote(raw); err == nil {
		*id = MiniMaxFileID(unquoted)
		return nil
	}
	*id = MiniMaxFileID(raw)
	return nil
}

// MiniMaxFileRetrieveResponse is returned by GET /v1/files/retrieve?file_id=.
type MiniMaxFileRetrieveResponse struct {
	File *struct {
		FileID      MiniMaxFileID `json:"file_id"`
		Bytes       int64         `json:"bytes"`
		CreatedAt   int64         `json:"created_at"`
		Filename    string        `json:"filename"`
		Purpose     string        `json:"purpose"`
		DownloadURL string        `json:"download_url"`
	} `json:"file"`
	BaseResp *MiniMaxBaseResp `json:"base_resp,omitempty"`
}

// ToBifrostV1VideoGenerationResponse converts a v1 query, and for a succeeded task the retrieved
// file, to the task object. A task that failed for a sensitivity code (base_resp 1026/1027 next to
// status Fail) is a failed task with content_filter, not an error of the query (contract C6
// agreement four). The link expires one hour after the file was created (C7, C9).
func ToBifrostV1VideoGenerationResponse(query *MiniMaxV1VideoQueryResponse, file *MiniMaxFileRetrieveResponse) *schemas.BifrostVideoGenerationResponse {
	resp := &schemas.BifrostVideoGenerationResponse{ID: query.TaskID, Object: "video"}
	miniMaxV1VideoStatuses.Apply(resp, query.Status)
	if query.VideoWidth > 0 && query.VideoHeight > 0 {
		resp.Size = strconv.Itoa(query.VideoWidth) + "x" + strconv.Itoa(query.VideoHeight)
	}
	switch resp.Status {
	case schemas.VideoStatusFailed:
		resp.Error = &schemas.VideoCreateError{Code: query.Status, Message: "task " + query.Status}
		if query.BaseResp != nil && query.BaseResp.StatusCode != 0 {
			code := strconv.Itoa(query.BaseResp.StatusCode)
			resp.Error.Code = code
			if query.BaseResp.StatusMsg != "" {
				resp.Error.Message = query.BaseResp.StatusMsg
			}
			if miniMaxContentFilterCode(code) {
				resp.ContentFilter = &schemas.ContentFilterInfo{FilteredCount: 1, Reasons: []string{code}}
			}
		}
	case schemas.VideoStatusCompleted:
		if file != nil && file.File != nil {
			if file.File.CreatedAt > 0 {
				resp.CompletedAt = schemas.Ptr(file.File.CreatedAt)
			}
			if file.File.DownloadURL != "" {
				resp.Videos = []schemas.VideoOutput{{Type: schemas.VideoOutputTypeURL, URL: schemas.Ptr(file.File.DownloadURL), ContentType: "video/mp4"}}
			}
		}
	}
	providerUtils.SetVideoExpiresAt(resp, nil, miniMaxV1LinkLifetime)
	return resp
}

// VideoGeneration creates a MiniMax video task: the Hailuo models through /v1/video_generation, the
// others (MiniMax-H3) through /v2/video_generation. Extra params are merged into the body, so
// MiniMax switches such as v2 extra.prompt_expansion_mode and v1 prompt_optimizer arrive.
func (provider *MiniMaxProvider) VideoGeneration(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostVideoGenerationRequest) (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError) {
	if bifrostErr := provider.checkAllowed(schemas.VideoGenerationRequest); bifrostErr != nil {
		return nil, bifrostErr
	}
	providerName := provider.GetProviderKey()
	isV1 := isMiniMaxV1VideoModel(request.Model)
	path := "/v2/video_generation"
	if isV1 {
		path = "/v1/video_generation"
	}
	ctx.SetValue(schemas.BifrostContextKeyPassthroughExtraParams, true)
	jsonData, bifrostErr := providerUtils.CheckContextAndGetRequestBody(
		ctx,
		request,
		func() (providerUtils.RequestBodyWithExtraParams, error) {
			if isV1 {
				return ToMiniMaxV1VideoGenerationRequest(request)
			}
			return ToMiniMaxVideoGenerationRequest(request)
		})
	if bifrostErr != nil {
		return nil, bifrostErr
	}

	sendBackRawRequest := providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest)
	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)

	body, latency, bifrostErr := provider.doMiniMaxRequest(ctx, key, http.MethodPost, path, jsonData, sendBackRawRequest, sendBackRawResponse)
	if bifrostErr != nil {
		return nil, bifrostErr
	}

	var taskResp MiniMaxVideoTaskCreationResponse
	rawRequest, rawResponse, bifrostErr := providerUtils.HandleProviderResponseCtx(ctx, body, &taskResp, jsonData, sendBackRawRequest, sendBackRawResponse)
	if bifrostErr != nil {
		return nil, providerUtils.SetErrorLatency(bifrostErr, latency)
	}
	if taskResp.TaskID == "" {
		return nil, providerUtils.EnrichError(ctx, schemas.NewProviderEmptyResultError("minimax returned no task_id"), jsonData, body, sendBackRawRequest, sendBackRawResponse, latency)
	}
	taskID := taskResp.TaskID
	if isV1 {
		taskID = miniMaxV1TaskIDPrefix + taskID
	}

	bifrostResp := &schemas.BifrostVideoGenerationResponse{
		ID:        providerUtils.AddVideoIDProviderSuffix(taskID, providerName),
		Model:     request.Model,
		Object:    "video",
		Status:    schemas.VideoStatusQueued,
		CreatedAt: time.Now().Unix(),
	}
	if duration := providerUtils.GetJSONField(jsonData, "duration").Int(); duration > 0 {
		bifrostResp.Seconds = schemas.Ptr(strconv.FormatInt(duration, 10))
	} else if isV1 {
		// A v1 create without duration runs MiniMax's documented default, and a v1 query reports no
		// duration, so the task's seconds are stated here; usage stays the vendor's own (empty for v1).
		bifrostResp.Seconds = schemas.Ptr(strconv.Itoa(miniMaxV1DefaultVideoDuration))
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

// videoRetrieve queries a MiniMax video task once: v2 through /v2/query/video_generation/{id}; v1
// through /v1/query/video_generation and, once it succeeded, /v1/files/retrieve for the link.
func (provider *MiniMaxProvider) videoRetrieve(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostVideoRetrieveRequest) (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError) {
	providerName := provider.GetProviderKey()
	taskID := providerUtils.StripVideoIDProviderSuffix(request.ID, providerName)
	if v1TaskID, isV1 := miniMaxV1TaskID(taskID); isV1 {
		return provider.videoRetrieveV1(ctx, key, v1TaskID)
	}
	escapedTaskID, idErr := providerUtils.EscapeResourceID(taskID, "video_id")
	if idErr != nil {
		return nil, idErr
	}

	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)

	body, latency, bifrostErr := provider.doMiniMaxRequest(ctx, key, http.MethodGet, "/v2/query/video_generation/"+escapedTaskID, nil, false, sendBackRawResponse)
	if bifrostErr != nil {
		return nil, bifrostErr
	}

	var taskResp MiniMaxVideoTaskResponse
	_, rawResponse, bifrostErr := providerUtils.HandleProviderResponseCtx(ctx, body, &taskResp, nil, false, sendBackRawResponse)
	if bifrostErr != nil {
		return nil, providerUtils.SetErrorLatency(bifrostErr, latency)
	}
	if taskResp.Task == nil {
		return nil, providerUtils.EnrichError(ctx, schemas.NewProviderEmptyResultError("minimax returned no task"), nil, body, false, sendBackRawResponse, latency)
	}

	bifrostResp, bifrostErr := ToBifrostVideoGenerationResponse(taskResp.Task)
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

// videoRetrieveV1 queries a v1 (Hailuo) task. extra_fields.raw_response carries the query's body.
func (provider *MiniMaxProvider) videoRetrieveV1(ctx *schemas.BifrostContext, key schemas.Key, taskID string) (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError) {
	if taskID == "" {
		return nil, providerUtils.NewBifrostBadRequestError("video_id is required")
	}
	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)

	queryPath := "/v1/query/video_generation?" + url.Values{"task_id": {taskID}}.Encode()
	body, latency, bifrostErr := provider.doMiniMaxRequestKeepBaseResp(ctx, key, http.MethodGet, queryPath, nil, false, sendBackRawResponse)
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	var query MiniMaxV1VideoQueryResponse
	_, rawResponse, bifrostErr := providerUtils.HandleProviderResponseCtx(ctx, body, &query, nil, false, sendBackRawResponse)
	if bifrostErr != nil {
		return nil, providerUtils.SetErrorLatency(bifrostErr, latency)
	}
	// A base_resp error next to status Fail is the task's own failure, reported in the task object.
	// Any other base_resp error is the query's (key, rate limit, unknown task).
	if query.Status != "Fail" {
		if baseErr := miniMaxBaseRespError(query.BaseResp); baseErr != nil {
			return nil, providerUtils.EnrichError(ctx, baseErr, nil, body, false, sendBackRawResponse, latency)
		}
	}
	if query.TaskID == "" {
		query.TaskID = taskID
	}

	var file *MiniMaxFileRetrieveResponse
	if query.Status == "Success" {
		if query.FileID == "" {
			return nil, providerUtils.EnrichError(ctx, schemas.NewProviderEmptyResultError("minimax reported the task succeeded but returned no file_id"), nil, body, false, sendBackRawResponse, latency)
		}
		filePath := "/v1/files/retrieve?" + url.Values{"file_id": {string(query.FileID)}}.Encode()
		fileBody, fileLatency, fileErr := provider.doMiniMaxRequest(ctx, key, http.MethodGet, filePath, nil, false, sendBackRawResponse)
		latency += fileLatency
		if fileErr != nil {
			return nil, providerUtils.SetErrorLatency(fileErr, latency)
		}
		file = &MiniMaxFileRetrieveResponse{}
		if _, _, decodeErr := providerUtils.HandleProviderResponseCtx(ctx, fileBody, file, nil, false, false); decodeErr != nil {
			return nil, providerUtils.SetErrorLatency(decodeErr, latency)
		}
		if file.File == nil || file.File.DownloadURL == "" {
			return nil, providerUtils.EnrichError(ctx, schemas.NewProviderEmptyResultError("minimax returned no download_url for the task's file"), nil, fileBody, false, sendBackRawResponse, latency)
		}
	}

	bifrostResp := ToBifrostV1VideoGenerationResponse(&query, file)
	bifrostResp.ID = providerUtils.AddVideoIDProviderSuffix(miniMaxV1TaskIDPrefix+query.TaskID, provider.GetProviderKey())
	bifrostResp.ExtraFields.Latency = latency.Milliseconds()
	if sendBackRawResponse {
		bifrostResp.ExtraFields.RawResponse = rawResponse
	}
	return bifrostResp, nil
}

// VideoCancel cancels a MiniMax video task (contract C7 BF13). The v2 DELETE cancels a queued task
// but deletes a finished one, so the task is queried first and DELETE is sent only while it is
// queued; any other state returns provider_cancel_unsupported with no DELETE. MiniMax has no cancel
// API for a v1 task: provider_cancel_unsupported, with no upstream call.
func (provider *MiniMaxProvider) VideoCancel(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostVideoCancelRequest) (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError) {
	if bifrostErr := provider.checkAllowed(schemas.VideoCancelRequest); bifrostErr != nil {
		return nil, bifrostErr
	}
	if request == nil || request.ID == "" {
		return nil, providerUtils.NewBifrostBadRequestError("video_id is required")
	}
	taskID := providerUtils.StripVideoIDProviderSuffix(request.ID, provider.GetProviderKey())
	if _, isV1 := miniMaxV1TaskID(taskID); isV1 {
		return nil, schemas.NewVideoCancelUnsupportedError("minimax has no API to cancel a v1 (Hailuo) video task")
	}
	escapedTaskID, idErr := providerUtils.EscapeResourceID(taskID, "video_id")
	if idErr != nil {
		return nil, idErr
	}
	return providerUtils.CancelVideoWhileQueued(
		func() (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError) {
			return provider.videoRetrieve(ctx, key, &schemas.BifrostVideoRetrieveRequest{Provider: request.Provider, ID: request.ID})
		},
		func() *schemas.BifrostError {
			body, latency, bifrostErr := provider.doMiniMaxRequest(ctx, key, http.MethodDelete, "/v2/video_generation/"+escapedTaskID, nil, false, false)
			if bifrostErr != nil {
				return bifrostErr
			}
			var deleteResp MiniMaxVideoDeleteResponse
			if _, _, decodeErr := providerUtils.HandleProviderResponseCtx(ctx, body, &deleteResp, nil, false, false); decodeErr != nil {
				return providerUtils.SetErrorLatency(decodeErr, latency)
			}
			if deleteResp.Action != "cancelled" {
				return providerUtils.EnrichError(ctx, providerUtils.NewBifrostOperationError(
					fmt.Sprintf("minimax answered the cancel with action %q, not cancelled", deleteResp.Action), nil), nil, body, false, true, latency)
			}
			return nil
		},
	)
}

// VideoList lists MiniMax v2 video tasks through GET /v2/query/video_generation (contract C7 BF15).
// MiniMax pages by number and has no cursor or order, so an after cursor or an order is refused
// rather than ignored, and only page 1 is reachable; limit is MiniMax's page_size.
func (provider *MiniMaxProvider) VideoList(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostVideoListRequest) (*schemas.BifrostVideoListResponse, *schemas.BifrostError) {
	if bifrostErr := provider.checkAllowed(schemas.VideoListRequest); bifrostErr != nil {
		return nil, bifrostErr
	}
	if request != nil && request.After != nil && *request.After != "" {
		return nil, providerUtils.NewBifrostBadRequestError("minimax video list pages by number and does not support an after cursor")
	}
	if request != nil && request.Order != nil && *request.Order != "" {
		return nil, providerUtils.NewBifrostBadRequestError("minimax video list has no order parameter")
	}
	values := url.Values{}
	values.Set("page_num", "1")
	if request != nil && request.Limit != nil && *request.Limit > 0 {
		values.Set("page_size", strconv.Itoa(*request.Limit))
	}

	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)
	body, latency, bifrostErr := provider.doMiniMaxRequest(ctx, key, http.MethodGet, "/v2/query/video_generation?"+values.Encode(), nil, false, sendBackRawResponse)
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	var listResp MiniMaxVideoTaskListResponse
	_, rawResponse, bifrostErr := providerUtils.HandleProviderResponseCtx(ctx, body, &listResp, nil, false, sendBackRawResponse)
	if bifrostErr != nil {
		return nil, providerUtils.SetErrorLatency(bifrostErr, latency)
	}

	providerName := provider.GetProviderKey()
	bifrostResp := &schemas.BifrostVideoListResponse{
		Object: "list",
		Data:   make([]schemas.VideoObject, 0, len(listResp.Items)),
	}
	for i := range listResp.Items {
		task, convErr := ToBifrostVideoGenerationResponse(&listResp.Items[i])
		if convErr != nil {
			return nil, providerUtils.SetErrorLatency(convErr, latency)
		}
		bifrostResp.Data = append(bifrostResp.Data, schemas.VideoObject{
			ID:          providerUtils.AddVideoIDProviderSuffix(task.ID, providerName),
			Object:      "video",
			Model:       task.Model,
			Status:      task.Status,
			CreatedAt:   task.CreatedAt,
			CompletedAt: task.CompletedAt,
			ExpiresAt:   task.ExpiresAt,
			Prompt:      task.Prompt,
			Seconds:     task.Seconds,
			Size:        task.Size,
			Error:       task.Error,
		})
	}
	if n := len(bifrostResp.Data); n > 0 {
		bifrostResp.FirstID = schemas.Ptr(bifrostResp.Data[0].ID)
		bifrostResp.LastID = schemas.Ptr(bifrostResp.Data[n-1].ID)
	}
	// Only page 1 is reachable (an after cursor is refused above), so there is never a next page to offer.
	bifrostResp.HasMore = schemas.Ptr(false)
	bifrostResp.ExtraFields.Latency = latency.Milliseconds()
	if sendBackRawResponse {
		bifrostResp.ExtraFields.RawResponse = rawResponse
	}
	return bifrostResp, nil
}

// MiniMaxVideoTaskListResponse is returned by GET /v2/query/video_generation.
type MiniMaxVideoTaskListResponse struct {
	Items    []MiniMaxVideoTask `json:"items"`
	Total    int                `json:"total"`
	BaseResp *MiniMaxBaseResp   `json:"base_resp,omitempty"`
}

var _ schemas.VideoCanceller = (*MiniMaxProvider)(nil)
var _ schemas.UnifiedFieldDeclarer = (*MiniMaxProvider)(nil)
