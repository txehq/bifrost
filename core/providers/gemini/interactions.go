package gemini

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bytedance/sonic"
	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

// Gemini Omni video runs on the Interactions API (fork, TXE-6538): POST /interactions creates a
// background interaction, GET /interactions/{id} queries it and POST /interactions/{id}/cancel
// cancels it. The Gemini API and Vertex AI share the request and response shapes, so the
// converters in this file serve both providers; InteractionVideoOptions carries what differs.
//
// Source of every field name here: https://ai.google.dev/static/api/interactions.openapi.json
// (CreateModelInteractionParams, VideoResponseFormat, VideoConfig, WebhookConfig, Interaction,
// Usage) and https://ai.google.dev/gemini-api/docs/omni. What neither documents is not mapped:
// see ToBifrostInteractionVideoResponse.

// InteractionVideoIDPrefix marks a unified task id as an interaction, so a query can tell it
// from a predictLongRunning operation name ("operations/..." or "models/.../operations/...").
const InteractionVideoIDPrefix = "interactions/"

// InteractionFileLifetime is how long the Gemini API keeps a delivery=uri artifact: it is a
// Gemini Files file, and the Files API deletes a file 48 hours after it is created.
const InteractionFileLifetime = 48 * time.Hour

// Interaction delivery modes (VideoResponseFormat.delivery).
const (
	InteractionDeliveryInline = "inline"
	InteractionDeliveryURI    = "uri"
)

// The Interactions API statuses (Interaction.status).
const (
	InteractionStatusInProgress = "in_progress"
	InteractionStatusCompleted  = "completed"
	InteractionStatusFailed     = "failed"
	InteractionStatusCancelled  = "cancelled"
)

// interactionVideoStatuses maps the Interactions API statuses that have a task-object status of
// the same meaning. The other four the API documents (requires_action, incomplete,
// budget_exceeded, requires_continuation) are deliberately absent: which task-object status each
// one means is not decided (TXE-6538 open question), so VideoStatusTable.Apply reports them as
// in_progress and the caller reads the vendor's own word in provider_status.
var interactionVideoStatuses = providerUtils.VideoStatusTable{
	InteractionStatusInProgress: schemas.VideoStatusInProgress,
	InteractionStatusCompleted:  schemas.VideoStatusCompleted,
	InteractionStatusFailed:     schemas.VideoStatusFailed,
	InteractionStatusCancelled:  schemas.VideoStatusCancelled,
}

// OmniVideoUnifiedFields are the unified video create fields an Omni model takes on the
// Interactions API, by JSON name. seed and negative_prompt are unpriced (contract C4) and need no
// entry: seed is sent in generation_config.seed, negative_prompt has no vendor field and is
// ignored. Every other field of the create parameters has no vendor field and is refused.
var OmniVideoUnifiedFields = []string{
	"aspect_ratio",
	"last_frame",
	"reference_images",
	"resolution",
	"seconds",
	schemas.TaskControlFieldCallbackURL,
	schemas.TaskControlFieldClientReferenceID,
}

// VideoGenerationUnifiedFields is what the gemini provider declares for video create. The
// declaration is per operation, not per model, so it is the union of two models families:
// every create parameter the predictLongRunning (Veo) path accepted before the declaration
// existed, which keeps that path's behaviour unchanged, plus the two task control fields the
// Interactions API takes. Each path then refuses what it cannot deliver itself: the Omni
// converter checks OmniVideoUnifiedFields, and the Veo path refuses the control fields.
var VideoGenerationUnifiedFields = []string{
	"aspect_ratio",
	"audio",
	"last_frame",
	"negative_prompt",
	"output_format",
	"reference_images",
	"resolution",
	"seconds",
	"seed",
	"size",
	"target_megapixels",
	"type",
	"upscale_factor",
	schemas.TaskControlFieldCallbackURL,
	schemas.TaskControlFieldClientReferenceID,
}

// IsOmniVideoModel reports whether model is a Gemini Omni model, which generates video through
// the Interactions API and not through predictLongRunning. It also looks at the canonical model
// name of the alias resolved in ctx, so an alias whose wire id is opaque is still recognised.
func IsOmniVideoModel(ctx *schemas.BifrostContext, model string) bool {
	if isOmniModelName(model) {
		return true
	}
	if ctx == nil {
		return false
	}
	return isOmniModelName(schemas.ResolveCanonicalModel(ctx, model))
}

func isOmniModelName(model string) bool {
	return strings.Contains(strings.ToLower(NormalizeModelName(model)), "gemini-omni")
}

// GeminiInteractionRequest is the body of POST /interactions for a model interaction
// (CreateModelInteractionParams), limited to the fields video generation sends.
type GeminiInteractionRequest struct {
	Model            string                             `json:"model"`
	Input            []GeminiInteractionContent         `json:"input"`
	ResponseFormat   *GeminiInteractionVideoFormat      `json:"response_format,omitempty"`
	GenerationConfig *GeminiInteractionGenerationConfig `json:"generation_config,omitempty"`
	Background       bool                               `json:"background"`
	WebhookConfig    *GeminiInteractionWebhookConfig    `json:"webhook_config,omitempty"`
	ExtraParams      map[string]any                     `json:"-"`
}

// GetExtraParams implements providerUtils.RequestBodyWithExtraParams.
func (r *GeminiInteractionRequest) GetExtraParams() map[string]interface{} {
	if r == nil {
		return nil
	}
	return r.ExtraParams
}

// GeminiInteractionContent is one content block (TextContent, ImageContent, VideoContent).
type GeminiInteractionContent struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"` // base64
	URI      string `json:"uri,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
}

// GeminiInteractionVideoFormat is VideoResponseFormat.
type GeminiInteractionVideoFormat struct {
	Type        string `json:"type"` // always "video"
	Delivery    string `json:"delivery,omitempty"`
	AspectRatio string `json:"aspect_ratio,omitempty"`
	Duration    string `json:"duration,omitempty"` // google-duration, e.g. "8s"
	Resolution  string `json:"resolution,omitempty"`
}

// GeminiInteractionGenerationConfig is the part of GenerationConfig video generation sends.
type GeminiInteractionGenerationConfig struct {
	Seed        *int                          `json:"seed,omitempty"`
	VideoConfig *GeminiInteractionVideoConfig `json:"video_config,omitempty"`
}

// GeminiInteractionVideoConfig is VideoConfig.
type GeminiInteractionVideoConfig struct {
	Task string `json:"task,omitempty"`
}

// GeminiInteractionWebhookConfig is WebhookConfig.
type GeminiInteractionWebhookConfig struct {
	URIs         []string          `json:"uris,omitempty"`
	UserMetadata map[string]string `json:"user_metadata,omitempty"`
}

// InteractionUserMetadataClientReferenceKey is the user_metadata key that carries the unified
// client_reference_id. The vendor returns user_metadata on every webhook event.
const InteractionUserMetadataClientReferenceKey = "client_reference_id"

// GeminiInteraction is the Interaction resource, limited to what the task object reads.
type GeminiInteraction struct {
	ID      string                   `json:"id"`
	Status  string                   `json:"status"`
	Model   string                   `json:"model,omitempty"`
	Created string                   `json:"created,omitempty"`
	Updated string                   `json:"updated,omitempty"`
	Steps   []GeminiInteractionStep  `json:"steps,omitempty"`
	Usage   *GeminiInteractionUsage  `json:"usage,omitempty"`
	Errors  []GeminiInteractionError `json:"errors,omitempty"`
}

// GeminiInteractionStep is one step. Content stays raw: only a model_output step's content is
// read, and the other step types (thought, tool calls) carry shapes this code never decodes.
type GeminiInteractionStep struct {
	Type    string          `json:"type"`
	Content json.RawMessage `json:"content,omitempty"`
}

// GeminiInteractionUsage is Usage, limited to the per-modality breakdowns.
type GeminiInteractionUsage struct {
	InputTokensByModality  []GeminiInteractionModalityTokens `json:"input_tokens_by_modality,omitempty"`
	OutputTokensByModality []GeminiInteractionModalityTokens `json:"output_tokens_by_modality,omitempty"`
}

// GeminiInteractionModalityTokens is ModalityTokens.
type GeminiInteractionModalityTokens struct {
	Modality string `json:"modality"`
	Tokens   *int   `json:"tokens,omitempty"`
}

// GeminiInteractionError is one entry of Interaction.errors.
type GeminiInteractionError struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// InteractionVideoOptions is what differs between the Gemini API and Vertex AI.
type InteractionVideoOptions struct {
	// DefaultDelivery is sent as response_format.delivery when the caller names none. Empty
	// sends no delivery, which leaves the vendor's default (inline).
	DefaultDelivery string
	// Webhooks reports whether webhook_config is documented for this API. When false,
	// callback_url and client_reference_id are refused: they are never dropped.
	Webhooks bool
	// FileLifetime is the documented lifetime of a delivery=uri artifact, 0 when none is.
	FileLifetime time.Duration
	// StoredRetention is the documented, fixed time the vendor keeps a stored interaction,
	// counted from creation; 0 when it is not fixed. An inline artifact lives in the
	// interaction itself, so this is when it stops being downloadable.
	StoredRetention time.Duration
}

// GeminiInteractionVideoOptions are the options of the Gemini API. delivery defaults to uri:
// the Omni guide tells callers to use it for any video over 4 MB, which inline cannot carry.
// A stored interaction is kept 55 days on the paid tier and 1 day on the free tier, so there
// is no fixed retention to report.
var GeminiInteractionVideoOptions = InteractionVideoOptions{
	DefaultDelivery: InteractionDeliveryURI,
	Webhooks:        true,
	FileLifetime:    InteractionFileLifetime,
}

// omniVideoRequestError is the caller_unsupported_parameter error of the Omni converter.
func omniVideoRequestError(param, message string) error {
	return providerUtils.ConverterBifrostError(schemas.NewUnsupportedParameterError(param, message))
}

// ToGeminiInteractionVideoRequest converts a unified video create request for an Omni model to
// the body of POST /interactions.
//
// Unified field → vendor field: prompt → a text block of input; input_reference → the first
// image block (the first frame); last_frame → the second image block (the last frame);
// reference_images → image blocks; video_uri → a video block; seconds → response_format.duration
// ("8" → "8s"); aspect_ratio, resolution → response_format, as given; seed →
// generation_config.seed; callback_url → webhook_config.uris; client_reference_id →
// webhook_config.user_metadata.client_reference_id. The extra param delivery ("inline", "uri")
// → response_format.delivery. background is always true: the unified route is a task route, and
// the vendor requires it with webhook_config.
//
// A field with no vendor field is refused with caller_unsupported_parameter, never dropped or
// changed (contract C4): size, audio, output_format, type, upscale_factor, target_megapixels,
// expires_after_seconds, output_storage_uri. A value is sent as given: the vendor, not Bifrost,
// rejects an aspect ratio, resolution or duration it does not offer.
//
// The vendor reads the roles of images from generation_config.video_config.task, so the
// converter sets it when the unified fields name a role: image_to_video for a first (and last)
// frame, reference_to_video for reference images. The two cannot be combined in one request.
func ToGeminiInteractionVideoRequest(bifrostReq *schemas.BifrostVideoGenerationRequest, opts InteractionVideoOptions) (*GeminiInteractionRequest, error) {
	if bifrostReq == nil || bifrostReq.Input == nil {
		return nil, fmt.Errorf("bifrost request or input is nil")
	}

	if bifrostErr := schemas.CheckUnifiedFields(&schemas.BifrostRequest{
		RequestType:            schemas.VideoGenerationRequest,
		VideoGenerationRequest: bifrostReq,
	}, OmniVideoUnifiedFields); bifrostErr != nil {
		return nil, providerUtils.ConverterBifrostError(bifrostErr)
	}

	req := &GeminiInteractionRequest{
		Model:          NormalizeModelName(bifrostReq.Model),
		Background:     true,
		ResponseFormat: &GeminiInteractionVideoFormat{Type: "video", Delivery: opts.DefaultDelivery},
	}

	params := bifrostReq.Params
	if params == nil {
		params = &schemas.VideoGenerationParameters{}
	}

	if !opts.Webhooks {
		const reason = "parameter %q is not supported: webhook_config is not documented for the Interactions API on this provider"
		if params.CallbackURL != nil {
			return nil, omniVideoRequestError(schemas.TaskControlFieldCallbackURL, fmt.Sprintf(reason, schemas.TaskControlFieldCallbackURL))
		}
		if params.ClientReferenceID != nil {
			return nil, omniVideoRequestError(schemas.TaskControlFieldClientReferenceID, fmt.Sprintf(reason, schemas.TaskControlFieldClientReferenceID))
		}
	}

	// Images, in the order the vendor reads their roles.
	var task string
	firstFrame := ""
	if bifrostReq.Input.InputReference != nil {
		firstFrame = *bifrostReq.Input.InputReference
	}
	if firstFrame != "" {
		block, err := interactionImageContent(firstFrame, "input_reference")
		if err != nil {
			return nil, err
		}
		req.Input = append(req.Input, block)
		task = "image_to_video"
	}
	if lastFrame := string(params.LastFrame); lastFrame != "" {
		if firstFrame == "" {
			return nil, omniVideoRequestError("last_frame",
				"parameter \"last_frame\" needs input_reference: the Interactions API reads a last frame only as the second of two images")
		}
		block, err := interactionImageContent(lastFrame, "last_frame")
		if err != nil {
			return nil, err
		}
		req.Input = append(req.Input, block)
	}
	if len(params.ReferenceImages) > 0 {
		if task != "" {
			return nil, omniVideoRequestError("reference_images",
				"parameter \"reference_images\" cannot be combined with input_reference: the Interactions API takes one video task per request")
		}
		for _, ref := range params.ReferenceImages {
			block, err := interactionImageContent(ref, "reference_images")
			if err != nil {
				return nil, err
			}
			req.Input = append(req.Input, block)
		}
		task = "reference_to_video"
	}
	if bifrostReq.Input.VideoURI != nil && *bifrostReq.Input.VideoURI != "" {
		req.Input = append(req.Input, GeminiInteractionContent{Type: "video", URI: *bifrostReq.Input.VideoURI})
	}
	req.Input = append(req.Input, GeminiInteractionContent{Type: "text", Text: bifrostReq.Input.Prompt})

	if params.Seconds != nil {
		seconds, err := strconv.Atoi(strings.TrimSpace(*params.Seconds))
		if err != nil || seconds <= 0 {
			return nil, omniVideoRequestError("seconds", fmt.Sprintf(
				"parameter \"seconds\" must be a whole number of seconds, got %q", *params.Seconds))
		}
		req.ResponseFormat.Duration = strconv.Itoa(seconds) + "s"
	}
	req.ResponseFormat.AspectRatio = string(params.AspectRatio)
	req.ResponseFormat.Resolution = string(params.Resolution)

	if params.Seed != nil || task != "" {
		req.GenerationConfig = &GeminiInteractionGenerationConfig{Seed: params.Seed}
		if task != "" {
			req.GenerationConfig.VideoConfig = &GeminiInteractionVideoConfig{Task: task}
		}
	}

	if params.CallbackURL != nil || params.ClientReferenceID != nil {
		req.WebhookConfig = &GeminiInteractionWebhookConfig{}
		if params.CallbackURL != nil {
			req.WebhookConfig.URIs = []string{*params.CallbackURL}
		}
		if params.ClientReferenceID != nil {
			req.WebhookConfig.UserMetadata = map[string]string{
				InteractionUserMetadataClientReferenceKey: *params.ClientReferenceID,
			}
		}
	}

	// Extra params: delivery is read here; the unified fields the handlers also keep under
	// their own name are already converted above and must not be merged into the body again.
	if len(params.ExtraParams) > 0 {
		extra := make(map[string]any, len(params.ExtraParams))
		for name, value := range params.ExtraParams {
			extra[name] = value
		}
		for _, name := range schemas.UnifiedVideoGenerationFields {
			delete(extra, name)
		}
		if raw, ok := extra["delivery"]; ok {
			delivery, isString := raw.(string)
			if !isString || (delivery != InteractionDeliveryInline && delivery != InteractionDeliveryURI) {
				return nil, omniVideoRequestError("delivery", "parameter \"delivery\" must be \"inline\" or \"uri\"")
			}
			req.ResponseFormat.Delivery = delivery
			delete(extra, "delivery")
		}
		if len(extra) > 0 {
			req.ExtraParams = extra
		}
	}

	return req, nil
}

// interactionImageContent builds the image block for one unified image value. A data URL is
// decoded into data and mime_type; any other value is a URL and is sent unchanged in uri
// (contract C4 agreement four: Bifrost never downloads an input).
func interactionImageContent(value, field string) (GeminiInteractionContent, error) {
	if !strings.HasPrefix(value, "data:") {
		return GeminiInteractionContent{Type: "image", URI: value}, nil
	}
	mimeType, payload, ok := parseVideoDataURL(value)
	if !ok || mimeType == "" {
		return GeminiInteractionContent{}, omniVideoRequestError(field, fmt.Sprintf(
			"parameter %q is a data URL without a media type or without data", field))
	}
	return GeminiInteractionContent{Type: "image", Data: payload, MimeType: mimeType}, nil
}

// InteractionVideoArtifact returns the first video block of the interaction's model_output
// steps, or nil when it has none. A step whose content is not a list of blocks is skipped.
func InteractionVideoArtifact(interaction *GeminiInteraction) *GeminiInteractionContent {
	if interaction == nil {
		return nil
	}
	for _, step := range interaction.Steps {
		if step.Type != "model_output" || len(step.Content) == 0 {
			continue
		}
		var blocks []GeminiInteractionContent
		if err := sonic.Unmarshal(step.Content, &blocks); err != nil {
			continue
		}
		for i := range blocks {
			if blocks[i].Type == "video" && (blocks[i].Data != "" || blocks[i].URI != "") {
				return &blocks[i]
			}
		}
	}
	return nil
}

// ToBifrostInteractionVideoResponse converts an Interaction to the task object. The id is the
// vendor's interaction id; the caller wraps it in its provider's task-id form.
//
//   - status and provider_status come from Interaction.status through interactionVideoStatuses.
//   - created_at is Interaction.created; completed_at is Interaction.updated once the status is
//     terminal, the last time the vendor says the interaction changed.
//   - videos has one entry of type "content" when a model_output step holds a video: both forms
//     the vendor returns it in, a Gemini Files uri and inline base64 data, need the vendor's key
//     (the inline bytes are inside the keyed GET), so the caller fetches it through
//     GET /v1/videos/{id}/content (contract C9). The bytes are not copied into the task object.
//   - expires_at is completed_at plus opts.FileLifetime for a uri artifact, else created_at plus
//     opts.StoredRetention, else empty.
//   - usage.output_tokens is the tokens of the video entry of usage.output_tokens_by_modality,
//     set only when the vendor reports that entry. The input modalities have no field in the
//     task object's usage (it has no input token count) and stay in the raw response.
//   - error is the first entry of Interaction.errors, verbatim.
//
// Not mapped, because no document shows it: how an interaction that was blocked by the content
// safety filters looks. content_filter is therefore never set, and a blocked interaction
// surfaces as the vendor reports it: an HTTP error on create, or a failed interaction whose
// error is passed through.
func ToBifrostInteractionVideoResponse(interaction *GeminiInteraction, opts InteractionVideoOptions) (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError) {
	if interaction == nil {
		return nil, providerUtils.NewBifrostOperationError("interaction is nil", nil)
	}
	if interaction.ID == "" {
		return nil, providerUtils.NewBifrostOperationError("the provider returned an interaction without an id", nil)
	}

	response := &schemas.BifrostVideoGenerationResponse{
		ID:     interaction.ID,
		Object: "video",
		Model:  interaction.Model,
	}
	interactionVideoStatuses.Apply(response, interaction.Status)

	if created, ok := parseInteractionTime(interaction.Created); ok {
		response.CreatedAt = created
	}
	if response.Status.IsTerminal() {
		if updated, ok := parseInteractionTime(interaction.Updated); ok {
			response.CompletedAt = schemas.Ptr(updated)
		}
	}

	if response.Status == schemas.VideoStatusCompleted {
		if artifact := InteractionVideoArtifact(interaction); artifact != nil {
			contentType := artifact.MimeType
			if strings.TrimSpace(contentType) == "" {
				contentType = defaultVideoContentType
			}
			response.Videos = []schemas.VideoOutput{{Type: schemas.VideoOutputTypeContent, ContentType: contentType}}
			switch {
			case artifact.URI != "" && opts.FileLifetime > 0:
				providerUtils.SetVideoExpiresAt(response, nil, opts.FileLifetime)
			case opts.StoredRetention > 0 && response.CreatedAt > 0:
				response.ExpiresAt = schemas.Ptr(response.CreatedAt + int64(opts.StoredRetention/time.Second))
			}
		}
	}

	if response.Status == schemas.VideoStatusFailed && len(interaction.Errors) > 0 {
		response.Error = &schemas.VideoCreateError{
			Code:    interaction.Errors[0].Code,
			Message: interaction.Errors[0].Message,
		}
	}

	if interaction.Usage != nil {
		for _, entry := range interaction.Usage.OutputTokensByModality {
			if entry.Modality == "video" && entry.Tokens != nil {
				response.Usage = &schemas.VideoUsage{OutputTokens: schemas.Ptr(*entry.Tokens)}
				break
			}
		}
	}

	return response, nil
}

func parseInteractionTime(value string) (int64, bool) {
	if value == "" {
		return 0, false
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return 0, false
	}
	return t.Unix(), true
}

// CheckInteractionCancellable returns the provider_cancel_unsupported error for an interaction
// the vendor will not cancel, nil for one it will. The cancel method "only applies to
// background interactions that are still running", so every status but in_progress is refused
// without calling it. rawResponse is attached so the caller sees the state the refusal rests on.
func CheckInteractionCancellable(interaction *GeminiInteraction, rawResponse interface{}) *schemas.BifrostError {
	if interaction != nil && interaction.Status == InteractionStatusInProgress {
		return nil
	}
	status := ""
	if interaction != nil {
		status = interaction.Status
	}
	unsupported := schemas.NewVideoCancelUnsupportedError(fmt.Sprintf(
		"the interaction is %q; the Interactions API cancels an interaction only while it is in_progress", status))
	unsupported.ExtraFields.RawResponse = rawResponse
	return unsupported
}

// DecodeInteractionVideoData decodes the inline base64 video of an interaction.
func DecodeInteractionVideoData(artifact *GeminiInteractionContent) ([]byte, string, *schemas.BifrostError) {
	content, err := base64.StdEncoding.DecodeString(artifact.Data)
	if err != nil {
		return nil, "", providerUtils.NewBifrostOperationError("failed to decode base64 video data", err)
	}
	contentType := artifact.MimeType
	if strings.TrimSpace(contentType) == "" {
		contentType = defaultVideoContentType
	}
	return content, contentType, nil
}

// interactionIDFromVideoID returns the interaction id of a unified task id whose provider
// suffix is already stripped, and false for any other id (a predictLongRunning operation name).
func interactionIDFromVideoID(videoID string) (string, bool) {
	id, ok := strings.CutPrefix(videoID, InteractionVideoIDPrefix)
	if !ok || id == "" {
		return "", false
	}
	return id, true
}

// interactionCall is one call to the Interactions API: it sends method to path (relative to the
// provider's base URL) with the key and returns the decoded response body of a 200.
func (provider *GeminiProvider) interactionCall(ctx *schemas.BifrostContext, key schemas.Key, method, path string, jsonData []byte) ([]byte, time.Duration, *schemas.BifrostError) {
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)
	req.SetRequestURI(provider.networkConfig.BaseURL + providerUtils.GetPathFromContext(ctx, path))
	req.Header.SetMethod(method)
	setGeminiAuthHeader(req, key.Value.GetValue())
	if jsonData != nil {
		req.Header.SetContentType("application/json")
		req.SetBody(jsonData)
	}

	latency, bifrostErr, wait := providerUtils.MakeRequestWithContext(ctx, provider.client, req, resp)
	defer wait()
	if bifrostErr != nil {
		return nil, latency, bifrostErr
	}
	if resp.StatusCode() != fasthttp.StatusOK {
		respBody := append([]byte(nil), resp.Body()...)
		return nil, latency, providerUtils.EnrichError(ctx, parseGeminiError(resp), jsonData, respBody, provider.sendBackRawRequest, provider.sendBackRawResponse, latency)
	}
	body, err := providerUtils.CheckAndDecodeBody(resp)
	if err != nil {
		return nil, latency, providerUtils.EnrichError(ctx, providerUtils.NewBifrostOperationError(schemas.ErrProviderResponseDecode, err), jsonData, nil, provider.sendBackRawRequest, provider.sendBackRawResponse, latency)
	}
	return append([]byte(nil), body...), latency, nil
}

// interactionTaskObject parses an Interaction body and converts it to the task object with this
// provider's task id. It returns the parsed interaction as well, for the callers that read the
// artifact or the status.
func (provider *GeminiProvider) interactionTaskObject(ctx *schemas.BifrostContext, body, jsonData []byte, latency time.Duration) (*schemas.BifrostVideoGenerationResponse, *GeminiInteraction, *schemas.BifrostError) {
	var interaction GeminiInteraction
	rawRequest, rawResponse, bifrostErr := providerUtils.HandleProviderResponse(body, &interaction, jsonData, providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest), providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse))
	if bifrostErr != nil {
		return nil, nil, bifrostErr
	}
	bifrostResp, bifrostErr := ToBifrostInteractionVideoResponse(&interaction, GeminiInteractionVideoOptions)
	if bifrostErr != nil {
		return nil, nil, bifrostErr
	}
	bifrostResp.ID = providerUtils.AddVideoIDProviderSuffix(InteractionVideoIDPrefix+bifrostResp.ID, provider.GetProviderKey())
	bifrostResp.ExtraFields.Latency = latency.Milliseconds()
	if providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest) {
		bifrostResp.ExtraFields.RawRequest = rawRequest
	}
	if providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse) {
		bifrostResp.ExtraFields.RawResponse = rawResponse
	}
	return bifrostResp, &interaction, nil
}

// interactionVideoCreate creates an Omni video task: POST /interactions with background true.
func (provider *GeminiProvider) interactionVideoCreate(ctx *schemas.BifrostContext, key schemas.Key, bifrostReq *schemas.BifrostVideoGenerationRequest) (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError) {
	jsonData, bifrostErr := providerUtils.CheckContextAndGetRequestBody(
		ctx,
		bifrostReq,
		func() (providerUtils.RequestBodyWithExtraParams, error) {
			return ToGeminiInteractionVideoRequest(bifrostReq, GeminiInteractionVideoOptions)
		},
	)
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	body, latency, bifrostErr := provider.interactionCall(ctx, key, http.MethodPost, "/interactions", jsonData)
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	bifrostResp, _, bifrostErr := provider.interactionTaskObject(ctx, body, jsonData, latency)
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	if bifrostResp.Model == "" {
		bifrostResp.Model = NormalizeModelName(bifrostReq.Model)
	}
	return bifrostResp, nil
}

// interactionVideoRetrieve queries an Omni video task: GET /interactions/{id}.
func (provider *GeminiProvider) interactionVideoRetrieve(ctx *schemas.BifrostContext, key schemas.Key, interactionID string) (*schemas.BifrostVideoGenerationResponse, *GeminiInteraction, *schemas.BifrostError) {
	escapedID, bifrostErr := providerUtils.EscapeResourceID(interactionID, "video_id")
	if bifrostErr != nil {
		return nil, nil, bifrostErr
	}
	body, latency, bifrostErr := provider.interactionCall(ctx, key, http.MethodGet, "/interactions/"+escapedID, nil)
	if bifrostErr != nil {
		return nil, nil, bifrostErr
	}
	return provider.interactionTaskObject(ctx, body, nil, latency)
}

// interactionVideoCancel cancels an Omni video task. It queries the interaction once and calls
// POST /interactions/{id}/cancel only while it is in_progress; any other state is refused with
// provider_cancel_unsupported without a cancel call. The vendor's DELETE is never called.
func (provider *GeminiProvider) interactionVideoCancel(ctx *schemas.BifrostContext, key schemas.Key, interactionID string) (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError) {
	current, interaction, bifrostErr := provider.interactionVideoRetrieve(ctx, key, interactionID)
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	if bifrostErr := CheckInteractionCancellable(interaction, current.ExtraFields.RawResponse); bifrostErr != nil {
		return nil, bifrostErr
	}
	escapedID, bifrostErr := providerUtils.EscapeResourceID(interactionID, "video_id")
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	body, latency, bifrostErr := provider.interactionCall(ctx, key, http.MethodPost, "/interactions/"+escapedID+"/cancel", nil)
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	bifrostResp, _, bifrostErr := provider.interactionTaskObject(ctx, body, nil, latency)
	return bifrostResp, bifrostErr
}

// interactionVideoDownload returns the artifact of a completed Omni video task: the inline
// bytes of the interaction, or the Gemini Files file its uri names, fetched with the key.
func (provider *GeminiProvider) interactionVideoDownload(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostVideoDownloadRequest, interactionID string) (*schemas.BifrostVideoDownloadResponse, *schemas.BifrostError) {
	videoResp, interaction, bifrostErr := provider.interactionVideoRetrieve(ctx, key, interactionID)
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	if videoResp.Status != schemas.VideoStatusCompleted {
		return nil, providerUtils.NewBifrostOperationError(
			fmt.Sprintf("video not ready, current status: %s", videoResp.Status), nil)
	}
	artifact := InteractionVideoArtifact(interaction)
	if artifact == nil {
		return nil, providerUtils.NewBifrostOperationError("the interaction holds no video", nil)
	}

	bifrostResp := &schemas.BifrostVideoDownloadResponse{VideoID: request.ID}
	if artifact.Data != "" {
		content, contentType, bifrostErr := DecodeInteractionVideoData(artifact)
		if bifrostErr != nil {
			return nil, bifrostErr
		}
		bifrostResp.Content = content
		bifrostResp.ContentType = contentType
		bifrostResp.ExtraFields.Latency = videoResp.ExtraFields.Latency
		return bifrostResp, nil
	}

	downloadURL, bifrostErr := geminiFileDownloadURL(provider.networkConfig.BaseURL, artifact.URI)
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)
	providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)
	req.SetRequestURI(downloadURL)
	req.Header.SetMethod(http.MethodGet)
	setGeminiAuthHeader(req, key.Value.GetValue())
	latency, bifrostErr, wait := providerUtils.MakeRequestWithContextFollowRedirects(ctx, provider.client, req, resp, 5)
	defer wait()
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	if resp.StatusCode() != fasthttp.StatusOK {
		respBody := append([]byte(nil), resp.Body()...)
		return nil, providerUtils.EnrichError(ctx, parseGeminiError(resp), nil, respBody, provider.sendBackRawRequest, provider.sendBackRawResponse, latency)
	}
	body, err := providerUtils.CheckAndDecodeBody(resp)
	if err != nil {
		return nil, providerUtils.NewBifrostOperationError(schemas.ErrProviderResponseDecode, err)
	}
	bifrostResp.Content = append([]byte(nil), body...)
	bifrostResp.ContentType = string(resp.Header.ContentType())
	if bifrostResp.ContentType == "" {
		bifrostResp.ContentType = videoResp.Videos[0].ContentType
	}
	bifrostResp.ExtraFields.Latency = latency.Milliseconds()
	return bifrostResp, nil
}

// geminiFileDownloadURL returns the URL the key is sent to for a delivery=uri artifact. The
// vendor documents the uri as "https://generativelanguage.googleapis.com/v1beta/files/{id}:download?alt=media"
// and its SDK samples as "files/{id}"; both are accepted, and the media download form is added
// to a file name that lacks it. The key is a credential, so the URL must be on the host of the
// provider's own base URL: a uri that points anywhere else is refused, never fetched.
func geminiFileDownloadURL(baseURL, uri string) (string, *schemas.BifrostError) {
	base, err := url.Parse(baseURL)
	if err != nil || base.Host == "" {
		return "", providerUtils.NewBifrostOperationError("the provider base URL is not a valid URL", err)
	}
	target := uri
	if strings.HasPrefix(uri, "files/") {
		target = strings.TrimRight(baseURL, "/") + "/" + uri
	}
	parsed, err := url.Parse(target)
	if err != nil || parsed.Host == "" {
		return "", providerUtils.NewBifrostOperationError("the provider returned a video uri that is not a URL", err)
	}
	if parsed.Scheme != base.Scheme || !strings.EqualFold(parsed.Host, base.Host) {
		return "", providerUtils.NewBifrostOperationError(
			fmt.Sprintf("the provider returned a video uri on host %q, outside the provider's own host %q; it is not fetched with the key", parsed.Host, base.Host), nil)
	}
	if !strings.Contains(parsed.Path, "/files/") {
		return "", providerUtils.NewBifrostOperationError("the provider returned a video uri that is not a Gemini Files file", nil)
	}
	if !strings.HasSuffix(parsed.Path, ":download") {
		parsed.Path += ":download"
		parsed.RawPath = ""
		query := parsed.Query()
		query.Set("alt", "media")
		parsed.RawQuery = query.Encode()
	}
	return parsed.String(), nil
}
