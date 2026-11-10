package schemas

import "slices"

// VideoStatus is the lifecycle status of a video job.
type VideoStatus string

const (
	VideoStatusQueued     VideoStatus = "queued"
	VideoStatusInProgress VideoStatus = "in_progress"
	VideoStatusCompleted  VideoStatus = "completed"
	VideoStatusFailed     VideoStatus = "failed"
	// Fork (TXE-6521, contract C7 BF11): a vendor's cancelled and expired states are their own
	// statuses, not folded into "failed". A provider maps to them through its status table
	// (providers/utils.VideoStatusTable).
	VideoStatusCancelled VideoStatus = "cancelled"
	VideoStatusExpired   VideoStatus = "expired"
)

// VideoStatuses is every status a task object can carry, in lifecycle order.
var VideoStatuses = []VideoStatus{
	VideoStatusQueued,
	VideoStatusInProgress,
	VideoStatusCompleted,
	VideoStatusFailed,
	VideoStatusCancelled,
	VideoStatusExpired,
}

// IsTerminal reports whether the task will not change state again: completed, failed,
// cancelled or expired.
func (s VideoStatus) IsTerminal() bool {
	switch s {
	case VideoStatusCompleted, VideoStatusFailed, VideoStatusCancelled, VideoStatusExpired:
		return true
	default:
		return false
	}
}

type VideoOutputType string

const (
	VideoOutputTypeBase64 VideoOutputType = "base64"
	VideoOutputTypeURL    VideoOutputType = "url"
	// VideoOutputTypeContent marks an artifact that can only be fetched with the vendor's key
	// (OpenAI and Azure /videos/{id}/content, Vertex gs://, Gemini Files). The entry carries no
	// url: the caller fetches it through GET /v1/videos/{id}/content (fork, TXE-6521, contract C9).
	VideoOutputTypeContent VideoOutputType = "content"
)

// VideoCreateError is the error payload when video generation fails.
type VideoCreateError struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// ContentFilterInfo contains information about content that was filtered due to safety policies.
// This is a provider-agnostic structure for representing content filtering results.
type ContentFilterInfo struct {
	FilteredCount int      `json:"filtered_count,omitempty"` // Number of items filtered
	Reasons       []string `json:"reasons,omitempty"`        // Human-readable reasons for filtering
}

type VideoOutput struct {
	ID          string          `json:"id,omitempty"` // provider-side asset identifier, when one is assigned
	Type        VideoOutputType `json:"type"`         // "url" | "base64" | "content"
	URL         *string         `json:"url,omitempty"`
	Base64Data  *string         `json:"base64,omitempty"`
	ContentType string          `json:"content_type"`
}

// VideoReferenceInput represents a reference image for video generation
type VideoReferenceInput struct {
	Image         []byte `json:"image"`                    // Image bytes
	ReferenceType string `json:"reference_type,omitempty"` // "style" or "asset" (Gemini: "REFERENCE_TYPE_STYLE" or "REFERENCE_TYPE_ASSET")
}

type VideoObject struct {
	ID                 string            `json:"id"`
	Object             string            `json:"object"` // always "video"
	Model              string            `json:"model"`
	Status             VideoStatus       `json:"status"`
	CreatedAt          int64             `json:"created_at"`
	CompletedAt        *int64            `json:"completed_at,omitempty"`
	ExpiresAt          *int64            `json:"expires_at,omitempty"`
	Progress           *float64          `json:"progress,omitempty"`
	Prompt             string            `json:"prompt"`
	RemixedFromVideoID *string           `json:"remixed_from_video_id,omitempty"`
	Seconds            *string           `json:"seconds"`
	Size               string            `json:"size"`
	Error              *VideoCreateError `json:"error,omitempty"`
}

// --- Video Generation ---

type BifrostVideoGenerationRequest struct {
	Provider       ModelProvider              `json:"provider"`
	Model          string                     `json:"model"`
	Input          *VideoGenerationInput      `json:"input"`
	Params         *VideoGenerationParameters `json:"params,omitempty"`
	Fallbacks      []Fallback                 `json:"fallbacks,omitempty"`
	RawRequestBody []byte                     `json:"-"`
}

func (b *BifrostVideoGenerationRequest) GetRawRequestBody() []byte {
	return b.RawRequestBody
}

func (b *BifrostVideoGenerationRequest) GetExtraParams() map[string]interface{} {
	if b == nil || b.Params == nil {
		return nil
	}
	return b.Params.ExtraParams
}

type VideoGenerationInput struct {
	Prompt         string  `json:"prompt"`
	InputReference *string `json:"input_reference,omitempty"` // Primary image for image-to-video (OpenAI-compatible)
	VideoURI       *string `json:"video_uri,omitempty"`       // Source video for video-to-video and video tool tasks
}

type VideoGenerationParameters struct {
	Seconds *string `json:"seconds,omitempty"`
	Size    string  `json:"size,omitempty"`

	NegativePrompt *string        `json:"negative_prompt,omitempty"`
	Seed           *int           `json:"seed,omitempty"`
	Type           *string        `json:"type,omitempty"`          // operation selector, e.g. "3d", "upscale"
	OutputFormat   *string        `json:"output_format,omitempty"` // container, e.g. "mp4", "webm", "mov"
	Audio          *bool          `json:"audio,omitempty"`
	ExtraParams    map[string]any `json:"-"`

	// Fork (TXE-6518, contract C4 BF3): unified video fields with one name on every vendor. A
	// provider converts each one to its vendor's own field; until it does, the value also stays in
	// ExtraParams under the same name (see UnifiedVideoGenerationFields), so a provider that read it
	// from there keeps working unchanged. Each decodes tolerantly: a value of another JSON type
	// (Runware 3D's numeric resolution) leaves the typed field empty, never fails the request.
	AspectRatio     TolerantString       `json:"aspect_ratio,omitempty"`     // e.g. "16:9", "9:16", "1:1"
	Resolution      TolerantString       `json:"resolution,omitempty"`       // tier, e.g. "480p", "720p", "1080p"
	LastFrame       TolerantString       `json:"last_frame,omitempty"`       // last frame: a URL or a data URL, like input_reference
	ReferenceImages VideoReferenceImages `json:"reference_images,omitempty"` // reference images: URLs or data URLs

	// Upscale operations (type "upscale"); mutually exclusive.
	UpscaleFactor    *int `json:"upscale_factor,omitempty"`
	TargetMegapixels *int `json:"target_megapixels,omitempty"`

	// Fork (TXE-6521, contract C7 BF12): the four task control fields, see TaskControlParameters.
	TaskControlParameters
}

// TaskControlParameters are the task control fields of an async task create request (fork,
// TXE-6521, contract C7 BF12): one name on every vendor, embedded in the create parameters of each
// task operation. A provider sends each one in its vendor's own field. A provider that does not
// take one never drops it: the request is refused with caller_unsupported_parameter before any
// upstream call (see CheckTaskControlFields).
type TaskControlParameters struct {
	// CallbackURL is where the vendor notifies task state changes. It is sent to the vendor
	// unchanged; Bifrost never substitutes an address of its own and receives no callback.
	CallbackURL *string `json:"callback_url,omitempty"`
	// ClientReferenceID is the caller's own task id, sent in the vendor's external-task-id or
	// tag field so the caller can find the task again in the vendor's records.
	ClientReferenceID *string `json:"client_reference_id,omitempty"`
	// ExpiresAfterSeconds is the upper bound on the task's whole lifetime, counted from creation.
	ExpiresAfterSeconds *int `json:"expires_after_seconds,omitempty"`
	// OutputStorageURI is where the vendor writes the artifact.
	OutputStorageURI *string `json:"output_storage_uri,omitempty"`
}

// The JSON names of the task control fields.
const (
	TaskControlFieldCallbackURL         = "callback_url"
	TaskControlFieldClientReferenceID   = "client_reference_id"
	TaskControlFieldExpiresAfterSeconds = "expires_after_seconds"
	TaskControlFieldOutputStorageURI    = "output_storage_uri"
)

// TaskControlFields lists the JSON names of the task control fields, sorted.
var TaskControlFields = []string{
	TaskControlFieldCallbackURL,
	TaskControlFieldClientReferenceID,
	TaskControlFieldExpiresAfterSeconds,
	TaskControlFieldOutputStorageURI,
}

// Present returns, sorted, the JSON names of the task control fields that are set.
func (p *TaskControlParameters) Present() []string {
	return p.appendPresent(nil)
}

// appendPresent appends the JSON names of the control fields that are set to dst. The request
// worker passes a stack buffer, so the check allocates nothing.
func (p *TaskControlParameters) appendPresent(dst []string) []string {
	if p == nil {
		return dst
	}
	if p.CallbackURL != nil {
		dst = append(dst, TaskControlFieldCallbackURL)
	}
	if p.ClientReferenceID != nil {
		dst = append(dst, TaskControlFieldClientReferenceID)
	}
	if p.ExpiresAfterSeconds != nil {
		dst = append(dst, TaskControlFieldExpiresAfterSeconds)
	}
	if p.OutputStorageURI != nil {
		dst = append(dst, TaskControlFieldOutputStorageURI)
	}
	return dst
}

// TaskControl returns the task control fields of the task create request br carries, or nil when
// br is not a task create request. An operation that gains the fields is added here, which is all
// CheckTaskControlFields needs to cover it.
func (br *BifrostRequest) TaskControl() *TaskControlParameters {
	if br == nil {
		return nil
	}
	if br.VideoGenerationRequest != nil && br.VideoGenerationRequest.Params != nil {
		return &br.VideoGenerationRequest.Params.TaskControlParameters
	}
	return nil
}

// CheckTaskControlFields refuses a task control field the provider does not take (contract C7,
// C4 convention three). A control field decides whether the caller can recover or bound the task,
// so support is opt-in: a provider takes a field only when it implements UnifiedFieldDeclarer and
// lists the field for req's request type. Every other provider, including one that declares
// nothing, gets 400 caller_unsupported_parameter naming the first unsupported field. Core runs it
// before a key is chosen, so a refused request makes no upstream call.
func CheckTaskControlFields(provider any, req *BifrostRequest) *BifrostError {
	var buf [4]string
	present := req.TaskControl().appendPresent(buf[:0])
	if len(present) == 0 {
		return nil
	}
	var supported []string
	if declarer, ok := provider.(UnifiedFieldDeclarer); ok {
		if fields, declared := declarer.SupportedUnifiedFields(req.RequestType); declared {
			supported = fields
		}
	}
	for _, field := range present {
		if !slices.Contains(supported, field) {
			return NewUnsupportedParameterError(field, "")
		}
	}
	return nil
}

// DefaultVideoDuration is the default video duration in seconds for Gemini/Vertex when not specified.
const DefaultVideoDuration = "8"

// BifrostVideoGenerationResponse represents the video generation job response in bifrost format.
type BifrostVideoGenerationResponse struct {
	ID                 string             `json:"id,omitempty"`
	CompletedAt        *int64             `json:"completed_at,omitempty"`          // Unix timestamp (seconds) when the job completed
	CreatedAt          int64              `json:"created_at,omitempty"`            // Unix timestamp (seconds) when the job was created
	Error              *VideoCreateError  `json:"error,omitempty"`                 // Error payload if generation failed
	ExpiresAt          *int64             `json:"expires_at,omitempty"`            // Unix timestamp (seconds) when downloadable assets expire
	Model              string             `json:"model,omitempty"`                 // Video generation model that produced the job
	Object             string             `json:"object,omitempty"`                // Object type, always "video"
	Progress           *float64           `json:"progress,omitempty"`              // Approximate completion percentage (0-100)
	Prompt             string             `json:"prompt,omitempty"`                // Prompt used to generate the video
	RemixedFromVideoID *string            `json:"remixed_from_video_id,omitempty"` // Source video ID if this is a remix
	Seconds            *string            `json:"seconds,omitempty"`               // Duration of the generated clip in seconds
	Size               string             `json:"size,omitempty"`                  // Resolution of the generated video
	Status             VideoStatus        `json:"status,omitempty"`                // Current lifecycle status of the video job
	ProviderStatus     string             `json:"provider_status,omitempty"`       // The vendor's raw status string, verbatim (fork, TXE-6521, contract C7 BF11)
	Videos             []VideoOutput      `json:"videos,omitempty"`                // Generated videos (supports multiple videos)
	ContentFilter      *ContentFilterInfo `json:"content_filter,omitempty"`        // Information about content filtering (if applicable)
	Usage              *VideoUsage        `json:"usage,omitempty"`                 // Provider-reported usage/cost, when the provider returns it

	ExtraFields BifrostResponseExtraFields `json:"extra_fields,omitempty"`
}

// TolerantString is a unified string field that existed under the same name as a vendor's extra
// param before it was unified (fork, TXE-6518). It decodes only a JSON string. Any other JSON value
// (a number such as Runware 3D's "resolution": 1024, an object, an array) leaves it empty instead
// of failing the request: the handlers keep the raw value in ExtraParams, where the provider that
// understands it reads it, exactly as before the field existed. Empty means absent; with omitempty
// an empty value is not serialised.
type TolerantString string

// UnmarshalJSON implements json.Unmarshaler; see TolerantString.
func (s *TolerantString) UnmarshalJSON(data []byte) error {
	var v string
	if err := Unmarshal(data, &v); err != nil {
		*s = ""
		return nil
	}
	*s = TolerantString(v)
	return nil
}

// VideoReferenceImages is the unified reference_images list: each entry a URL or a data URL.
//
// It decodes only an array of strings. Any other JSON value (Runway's [{"uri": ..., "tag": ...}]
// objects, which callers sent as an extra param before the field was unified) leaves the list empty
// instead of failing the request: the handlers keep the raw value in ExtraParams, where the provider
// that understands that shape reads it, exactly as before the field existed.
type VideoReferenceImages []string

// UnmarshalJSON implements json.Unmarshaler; see VideoReferenceImages.
func (v *VideoReferenceImages) UnmarshalJSON(data []byte) error {
	var list []string
	if err := Unmarshal(data, &list); err != nil {
		*v = nil
		return nil
	}
	*v = list
	return nil
}

// UnifiedVideoGenerationFields are the video create fields that became unified in the fork
// (TXE-6518). The HTTP handlers parse them into VideoGenerationParameters and also keep them in
// ExtraParams under the same name: before they were unified they reached providers only as extra
// params, and several providers (byteplus, minimax, and every provider that forwards its extra
// params) still read them from there. A provider that converts the typed field deletes the extra.
var UnifiedVideoGenerationFields = []string{"aspect_ratio", "resolution", "last_frame", "reference_images"}

// VideoUsage carries provider-reported usage for a video (or other async media) generation job.
// Providers that report an exact price (e.g. Runware's per-task cost) populate Cost so pricing uses
// it verbatim instead of a datasheet estimate.
//
// Fork (TXE-6518, contract C5 BF6): the other fields are the billable quantities the vendor
// reported for the task. Each one is set only from the vendor's own report: Bifrost never fills
// one with a value it counted, a request parameter or a default, so a nil field means the vendor
// did not report it. Cost keeps its upstream meaning and may be computed by Bifrost.
type VideoUsage struct {
	Cost *BifrostCost `json:"cost,omitempty"`

	OutputTokens   *int        `json:"output_tokens,omitempty"`   // tokens the vendor bills the output by (Seedance completion_tokens)
	OutputSeconds  *float64    `json:"output_seconds,omitempty"`  // seconds of output the vendor billed
	InputSeconds   *float64    `json:"input_seconds,omitempty"`   // seconds of input media the vendor billed
	InputImages    *int        `json:"input_images,omitempty"`    // input images the vendor billed
	ComputeSeconds *float64    `json:"compute_seconds,omitempty"` // run time a hosting platform bills by (Replicate metrics.predict_time)
	VendorCost     *VendorCost `json:"vendor_cost,omitempty"`     // money or credits the vendor reported
}

// getSecondsFromVideoRequest extracts Seconds from video-related requests.
func getSecondsFromVideoRequest(req *BifrostRequest) *string {
	if req == nil {
		return nil
	}
	useDefaultForSeconds := func(p ModelProvider) bool {
		return p == Gemini || p == Vertex
	}
	if req.VideoGenerationRequest != nil {
		var seconds *string
		if req.VideoGenerationRequest.Params != nil {
			seconds = req.VideoGenerationRequest.Params.Seconds
		}
		if seconds == nil && useDefaultForSeconds(req.VideoGenerationRequest.Provider) {
			seconds = Ptr(DefaultVideoDuration)
		}
		return seconds
	}
	if req.VideoRemixRequest != nil && useDefaultForSeconds(req.VideoRemixRequest.Provider) {
		return Ptr(DefaultVideoDuration)
	}
	return nil
}

// BackfillParams populates response fields from the original request that are needed
// for cost calculation but may not be returned by the provider.
// - Seconds (duration from request params or default), only when the provider reported none
func (r *BifrostVideoGenerationResponse) BackfillParams(req *BifrostRequest) {
	if r == nil || req == nil {
		return
	}
	// Fork: keep a duration the provider already reported. A provider can normalize the requested
	// duration (MiniMax clamps H3-Max to 5-15s), and that is the clip that was ordered and billed.
	if r.Seconds == nil {
		r.Seconds = getSecondsFromVideoRequest(req)
	}
	if r.Model == "" {
		switch {
		case req.VideoGenerationRequest != nil:
			r.Model = req.VideoGenerationRequest.Model
		case req.VideoEditRequest != nil:
			r.Model = req.VideoEditRequest.Model
		}
	}
}

// --- Video Edit ---

// BifrostVideoEditRequest represents a video edit request in bifrost format. Model is optional:
// when the source video is referenced by an ID that already carries its provider suffix, the
// provider is resolved from that ID and the upstream API infers the model from the source.
type BifrostVideoEditRequest struct {
	Provider       ModelProvider        `json:"provider"`
	Model          string               `json:"model,omitempty"`
	Input          *VideoEditInput      `json:"input"`
	Params         *VideoEditParameters `json:"params,omitempty"`
	Fallbacks      []Fallback           `json:"fallbacks,omitempty"`
	RawRequestBody []byte               `json:"-"`
}

// GetRawRequestBody implements [utils.RequestBodyGetter].
func (b *BifrostVideoEditRequest) GetRawRequestBody() []byte {
	return b.RawRequestBody
}

func (b *BifrostVideoEditRequest) GetExtraParams() map[string]interface{} {
	if b == nil || b.Params == nil {
		return nil
	}
	return b.Params.ExtraParams
}

type VideoEditInput struct {
	Video  VideoInput `json:"video"`
	Prompt string     `json:"prompt"`
}

// VideoInput is the source video for an edit, in whichever form the caller supplied it. Exactly
// one of the three is expected; providers that cannot accept a given form reject it.
type VideoInput struct {
	Video []byte `json:"video,omitempty"` // raw bytes, from a multipart upload
	URL   string `json:"url,omitempty"`   // public URL or data URI
	ID    string `json:"id,omitempty"`    // provider-side video ID
}

// VideoEditParameters holds the knobs video edit APIs actually accept. Geometry and duration are
// deliberately absent: an edit inherits both from its source video, so no provider takes them.
type VideoEditParameters struct {
	Type         *string        `json:"type,omitempty"` // operation selector, e.g. "upscale", "background_removal"
	Seed         *int           `json:"seed,omitempty"`
	OutputFormat *string        `json:"output_format,omitempty"` // container, e.g. "mp4", "webm", "mov"
	ExtraParams  map[string]any `json:"-"`

	// Upscale operations (type "upscale"); mutually exclusive.
	UpscaleFactor    *int `json:"upscale_factor,omitempty"`
	TargetMegapixels *int `json:"target_megapixels,omitempty"`
}

// BifrostVideoEditResponse is the video edit job response. Video edit returns the same job object
// as generation, so callers poll and download it through the existing video endpoints.
type BifrostVideoEditResponse = BifrostVideoGenerationResponse

// --- Video Remix ---

type BifrostVideoRemixRequest struct {
	ID             string                `json:"id"`
	Provider       ModelProvider         `json:"provider"`
	Input          *VideoGenerationInput `json:"input"`
	ExtraParams    map[string]any        `json:"-"`
	RawRequestBody []byte                `json:"-"`
}

func (b *BifrostVideoRemixRequest) GetRawRequestBody() []byte {
	return b.RawRequestBody
}

func (b *BifrostVideoRemixRequest) GetExtraParams() map[string]interface{} {
	if b == nil {
		return nil
	}
	return b.ExtraParams
}

// --- Video List ---

type BifrostVideoListRequest struct {
	Provider ModelProvider `json:"provider"`
	After    *string       `json:"after,omitempty"`
	Limit    *int          `json:"limit,omitempty"`
	Order    *string       `json:"order,omitempty"`
}

type BifrostVideoListResponse struct {
	Object      string                     `json:"object"` // "list"
	Data        []VideoObject              `json:"data"`
	FirstID     *string                    `json:"first_id,omitempty"`
	HasMore     *bool                      `json:"has_more,omitempty"`
	LastID      *string                    `json:"last_id,omitempty"`
	ExtraFields BifrostResponseExtraFields `json:"extra_fields"`
}

// --- Video Retrieve / Delete ---

type BifrostVideoReferenceRequest struct {
	Provider ModelProvider `json:"provider"`
	ID       string        `json:"id"`
}

type BifrostVideoDeleteRequest = BifrostVideoReferenceRequest
type BifrostVideoRetrieveRequest = BifrostVideoReferenceRequest

// --- Video Cancel ---

// BifrostVideoCancelRequest names the task POST /v1/videos/{id}/cancel cancels (fork, TXE-6521,
// contract C7 BF13). The response is the task object, as for a retrieve.
type BifrostVideoCancelRequest = BifrostVideoReferenceRequest

// VideoCanceller is implemented by a provider whose vendor has a cancel API for video tasks.
// Cancel is not part of the Provider interface: a provider that does not implement it has no
// cancel API, and core answers 400 provider_cancel_unsupported without an upstream call.
//
// VideoCancel calls the vendor's cancel API once and returns the task object. It must never
// remove a finished task's artifact or record. When the vendor does not allow cancelling in the
// task's current state it returns NewVideoCancelUnsupportedError. A vendor whose cancel API is
// also its delete API (ModelArk, MiniMax v2 DELETE) goes through
// providers/utils.CancelVideoWhileQueued, which queries first and calls it only while the task is
// queued.
type VideoCanceller interface {
	VideoCancel(ctx *BifrostContext, key Key, request *BifrostVideoCancelRequest) (*BifrostVideoGenerationResponse, *BifrostError)
}

// NewVideoCancelUnsupportedError is the error of the cancel route when the task cannot be
// cancelled: the vendor has no cancel API, or does not allow cancelling in the task's current
// state. 400, error_type provider_cancel_unsupported. message says which, for the caller; empty
// gives the no-cancel-API default.
func NewVideoCancelUnsupportedError(message string) *BifrostError {
	if message == "" {
		message = "this provider has no API to cancel a video task"
	}
	return &BifrostError{
		StatusCode: Ptr(400),
		Error: &ErrorField{
			Type:    Ptr("invalid_request_error"),
			Code:    Ptr(string(ErrorTypeProviderCancelUnsupported)),
			Message: message,
		},
		ExtraFields: BifrostErrorExtraFields{
			RequestType: VideoCancelRequest,
			ErrorType:   ErrorTypeProviderCancelUnsupported,
		},
	}
}

// CheckVideoCancelSupported returns the provider_cancel_unsupported error when req is a video
// cancel and provider has no cancel API (it does not implement VideoCanceller), nil otherwise.
// Core runs it before a key is chosen.
func CheckVideoCancelSupported(provider any, req *BifrostRequest) *BifrostError {
	if req == nil || req.RequestType != VideoCancelRequest {
		return nil
	}
	if _, ok := provider.(VideoCanceller); ok {
		return nil
	}
	return NewVideoCancelUnsupportedError("")
}

type BifrostVideoDeleteResponse struct {
	ID          string                     `json:"id"`
	Deleted     bool                       `json:"deleted"`
	Object      string                     `json:"object,omitempty"` // "video.deleted"
	ExtraFields BifrostResponseExtraFields `json:"extra_fields"`
}

// --- Video Download ---

type BifrostVideoDownloadRequest struct {
	Provider    ModelProvider         `json:"provider"`
	ID          string                `json:"id"`
	Variant     *VideoDownloadVariant `json:"variant,omitempty"`
	ExtraParams map[string]any        `json:"-"`
}

type VideoDownloadVariant string

const (
	VideoDownloadVariantVideo       VideoDownloadVariant = "video"
	VideoDownloadVariantThumbnail   VideoDownloadVariant = "thumbnail"
	VideoDownloadVariantSpriteSheet VideoDownloadVariant = "sprite_sheet"
)

type BifrostVideoDownloadResponse struct {
	VideoID     string `json:"video_id"`
	Content     []byte `json:"-"`                      // Raw video content (not serialized)
	ContentType string `json:"content_type,omitempty"` // MIME type (e.g., "video/mp4", "image/png" for thumbnails)

	ExtraFields BifrostResponseExtraFields `json:"extra_fields"`
}

type VideoLogParams struct {
	VideoID string `json:"video_id"`
}

// BifrostVideoDebug is the video-kind detail carried on a log row. It mirrors
// BifrostBatchDebug: video generation is billed at settlement, minutes after the
// job was submitted, so the cost lands on its own aggregate row rather than on the
// submission. Without this the aggregate row is indistinguishable from the
// submission it settles — same object, same model, differing only by a cost.
//
// The row's Object stays the real request type (video_generation / video_remix /
// video_edit) rather than a synthetic one, so a later repricing pass still finds
// the right rates. Accounting being non-nil is what marks the aggregate row.
type BifrostVideoDebug struct {
	VideoID string `json:"video_id,omitempty"`
	// Status is the provider's video lifecycle status as last known when this row
	// was written. On the aggregate cost row it is the status settlement observed,
	// which is always terminal.
	Status VideoStatus `json:"status,omitempty"`
	// Accounting is set only on the aggregate cost row, and is what distinguishes
	// it from the submission row it settles.
	Accounting *VideoAccountingDebug `json:"accounting,omitempty"`
	// SettledByRequestID is the request that triggered settlement — a client poll, or
	// empty when the sweeper got there first. The row's parent points at the request
	// that CREATED the job so the cost rolls up onto it, so this is where "which call
	// actually settled this" survives, for debugging a double-settle or a stuck job.
	SettledByRequestID string `json:"settled_by_request_id,omitempty"`
}

func (d *BifrostVideoDebug) IsZero() bool {
	if d == nil {
		return true
	}
	return d.VideoID == "" && d.Status == "" && d.Accounting == nil
}

// VideoAccountingDebug records how a settled video was priced, so an operator can
// see why a row cost what it did without re-deriving it from the provider.
type VideoAccountingDebug struct {
	// Seconds and Size are the dimensions the price was actually computed from,
	// after merging the terminal response over the request captured at submission.
	Seconds *int   `json:"seconds,omitempty"`
	Size    string `json:"size,omitempty"`
	// OutputCount is how many clips the job returned; the per-second rate is
	// multiplied by it.
	OutputCount int `json:"output_count,omitempty"`
	// Incomplete marks a row whose cost is known to be short — priced with no rate,
	// or from dimensions the provider never confirmed.
	Incomplete bool `json:"incomplete,omitempty"`
	// RequestType is the request that created the job. The aggregate row's Object is
	// "video_retrieve" so it reads as the settlement it is, which means Object can no
	// longer name the rates this was priced at — a repricing pass reads it from here.
	RequestType RequestType `json:"request_type,omitempty"`
	// ProviderCost is the figure the provider reported for the job, when it reported
	// one. A provider is always right about its own bill, so repricing must return
	// this verbatim rather than substituting a catalog estimate.
	ProviderCost *float64 `json:"provider_cost,omitempty"`
}
