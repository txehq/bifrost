package gemini

// Gemini Omni video (gemini-omni-1.1-flash) on the Interactions API
// (https://aistudio.google.com/docs/omni, read 2026-11-04): the Gemini API's video since the
// Veo previews were shut down on 2026-10-22. A video is an interaction created in the
// background and polled until it completes; its model output step carries the video, inline
// as base64 on a GET. Its Bifrost video ID is "interactions/<id>", beside Veo's operation names.

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bytedance/sonic"
	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	schemas "github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

const (
	// interactionsVideoIDPrefix marks a Bifrost video ID as an interaction's.
	interactionsVideoIDPrefix = "interactions/"
	// interactionsAPIRevision pins the Interactions API's response shape (steps).
	interactionsAPIRevision = "2026-05-20"
)

// IsInteractionsVideoModel reports a model whose video the Interactions API serves (Gemini Omni).
func IsInteractionsVideoModel(model string) bool {
	return strings.HasPrefix(strings.ToLower(NormalizeModelName(model)), "gemini-omni")
}

// InteractionContent is one content item of an interaction's input or of a step.
type InteractionContent struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	URI      string `json:"uri,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
}

// InteractionVideoFormat is an interaction's response_format for video output.
type InteractionVideoFormat struct {
	Type        string `json:"type"`
	AspectRatio string `json:"aspect_ratio,omitempty"`
	Resolution  string `json:"resolution,omitempty"`
}

// InteractionVideoRequest creates a video interaction. Input is the prompt alone, or the
// reference media and the prompt as content items.
type InteractionVideoRequest struct {
	Model          string                  `json:"model"`
	Input          interface{}             `json:"input"`
	ResponseFormat *InteractionVideoFormat `json:"response_format,omitempty"`
	Background     bool                    `json:"background"`
	ExtraParams    map[string]interface{}  `json:"-"`
}

// GetExtraParams implements the request body interface for passthrough extra params.
func (r *InteractionVideoRequest) GetExtraParams() map[string]interface{} { return r.ExtraParams }

// InteractionStep is one step of an interaction: the user input, a thought, or the model output.
type InteractionStep struct {
	Type    string               `json:"type"`
	Content []InteractionContent `json:"content,omitempty"`
}

// Interaction is the Interactions API's interaction resource, as far as a video reads it.
type Interaction struct {
	ID      string            `json:"id"`
	Status  string            `json:"status"`
	Model   string            `json:"model,omitempty"`
	Created string            `json:"created,omitempty"`
	Steps   []InteractionStep `json:"steps,omitempty"`
	Error   *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// video is the interaction's output video, nil until the model output step carries one.
func (i *Interaction) video() *InteractionContent {
	for s := len(i.Steps) - 1; s >= 0; s-- {
		if i.Steps[s].Type != "model_output" {
			continue
		}
		for c := range i.Steps[s].Content {
			if i.Steps[s].Content[c].Type == "video" {
				return &i.Steps[s].Content[c]
			}
		}
	}
	return nil
}

// ToInteractionVideoRequest converts a Bifrost video request to an Omni interaction: the
// reference image and video as content items before the prompt, and size as the aspect ratio
// (16:9 or 9:16) and the resolution of its short side. Omni documents no duration or count
// of videos, so seconds is not sent.
func ToInteractionVideoRequest(bifrostReq *schemas.BifrostVideoGenerationRequest) (*InteractionVideoRequest, error) {
	if bifrostReq == nil || bifrostReq.Input == nil || strings.TrimSpace(bifrostReq.Input.Prompt) == "" {
		return nil, fmt.Errorf("prompt is required")
	}
	req := &InteractionVideoRequest{Model: NormalizeModelName(bifrostReq.Model), Background: true,
		ResponseFormat: &InteractionVideoFormat{Type: "video"}}
	var items []InteractionContent
	if ref := bifrostReq.Input.InputReference; ref != nil && *ref != "" {
		item, err := interactionMediaItem("image", *ref)
		if err != nil {
			return nil, fmt.Errorf("invalid input reference: %w", err)
		}
		items = append(items, item)
	}
	if uri := bifrostReq.Input.VideoURI; uri != nil && *uri != "" {
		items = append(items, InteractionContent{Type: "video", URI: *uri})
	}
	if len(items) == 0 {
		req.Input = bifrostReq.Input.Prompt
	} else {
		req.Input = append(items, InteractionContent{Type: "text", Text: bifrostReq.Input.Prompt})
	}
	if bifrostReq.Params != nil {
		if bifrostReq.Params.Size != "" {
			ratio, resolution, err := interactionVideoSize(bifrostReq.Params.Size)
			if err != nil {
				return nil, err
			}
			req.ResponseFormat.AspectRatio, req.ResponseFormat.Resolution = ratio, resolution
		}
		req.ExtraParams = bifrostReq.Params.ExtraParams
	}
	return req, nil
}

// interactionMediaItem is a content item for a media reference: inline data for a data URL,
// otherwise the link as its uri.
func interactionMediaItem(kind, ref string) (InteractionContent, error) {
	sanitized, err := schemas.SanitizeImageURLWithAllowedSchemes(ref, defaultGeminiImageURLSchemes...)
	if err != nil {
		return InteractionContent{}, err
	}
	info := schemas.ExtractURLTypeInfo(sanitized)
	item := InteractionContent{Type: kind}
	if info.MediaType != nil {
		item.MimeType = *info.MediaType
	}
	if info.DataURLWithoutPrefix != nil {
		item.Data = *info.DataURLWithoutPrefix
	} else {
		item.URI = sanitized
	}
	return item, nil
}

// interactionVideoSize maps an OpenAI size (WIDTHxHEIGHT) to Omni's aspect ratio and resolution.
func interactionVideoSize(size string) (string, string, error) {
	w, h, ok := strings.Cut(strings.ToLower(size), "x")
	width, errW := strconv.Atoi(w)
	height, errH := strconv.Atoi(h)
	if !ok || errW != nil || errH != nil || width <= 0 || height <= 0 {
		return "", "", fmt.Errorf("size %q is not WIDTHxHEIGHT", size)
	}
	ratio := ""
	switch {
	case width*9 == height*16:
		ratio = "16:9"
	case width*16 == height*9:
		ratio = "9:16"
	default:
		return "", "", fmt.Errorf("size %q is neither 16:9 nor 9:16, the aspect ratios Gemini Omni produces", size)
	}
	resolutions := map[int]string{360: "360p", 720: "720p", 1080: "1080p", 2160: "4k"}
	resolution, ok := resolutions[min(width, height)]
	if !ok {
		return "", "", fmt.Errorf("size %q has no Gemini Omni resolution (360p, 720p, 1080p, 4k)", size)
	}
	return ratio, resolution, nil
}

// interactionVideoStatus maps an interaction's status to a video's.
func interactionVideoStatus(status string) schemas.VideoStatus {
	switch status {
	case "completed":
		return schemas.VideoStatusCompleted
	case "in_progress":
		return schemas.VideoStatusInProgress
	case "queued":
		return schemas.VideoStatusQueued
	}
	// failed, cancelled, incomplete, requires_action, requires_continuation: a video
	// interaction that stops anywhere else produced no video.
	return schemas.VideoStatusFailed
}

// toBifrostInteractionVideo is the video state of an interaction. The video's bytes stay out:
// a GET carries them inline, and a status read must not; VideoDownload reads them.
func toBifrostInteractionVideo(interaction *Interaction, model string, providerName schemas.ModelProvider) *schemas.BifrostVideoGenerationResponse {
	if model == "" {
		model = interaction.Model
	}
	resp := &schemas.BifrostVideoGenerationResponse{
		ID:     providerUtils.AddVideoIDProviderSuffix(interactionsVideoIDPrefix+interaction.ID, providerName),
		Model:  model,
		Object: "video",
		Status: interactionVideoStatus(interaction.Status),
	}
	if created, err := time.Parse(time.RFC3339, interaction.Created); err == nil {
		resp.CreatedAt = created.Unix()
	}
	switch resp.Status {
	case schemas.VideoStatusCompleted:
		if v := interaction.video(); v != nil && v.URI != "" {
			resp.Videos = []schemas.VideoOutput{{Type: schemas.VideoOutputTypeURL, URL: schemas.Ptr(v.URI), ContentType: v.MimeType}}
		}
	case schemas.VideoStatusFailed:
		resp.Error = &schemas.VideoCreateError{Code: interaction.Status, Message: "the interaction ended " + interaction.Status + " without a video"}
		if interaction.Error != nil {
			resp.Error = &schemas.VideoCreateError{Code: interaction.Error.Code, Message: interaction.Error.Message}
		}
	}
	return resp
}

// interactionID is the interaction a Bifrost video ID names, escaped for a path, and whether
// the ID is an interaction's.
func interactionID(videoID string) (string, bool, *schemas.BifrostError) {
	id, ok := strings.CutPrefix(videoID, interactionsVideoIDPrefix)
	if !ok {
		return "", false, nil
	}
	escaped, bifrostErr := providerUtils.EscapeResourceID(id, "video_id")
	return escaped, true, bifrostErr
}

// doInteractionRequest sends one Interactions API request and returns the 2xx body.
func (provider *GeminiProvider) doInteractionRequest(ctx *schemas.BifrostContext, key schemas.Key, method, path string, jsonData []byte) ([]byte, time.Duration, *schemas.BifrostError) {
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)
	providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)
	req.SetRequestURI(provider.networkConfig.BaseURL + providerUtils.GetPathFromContext(ctx, path))
	req.Header.SetMethod(method)
	req.Header.Set("Api-Revision", interactionsAPIRevision)
	setGeminiAuthHeader(req, key.Value.GetValue())
	if jsonData != nil {
		req.Header.SetContentType("application/json")
		req.SetBody(jsonData)
	}
	sendBackRawRequest := providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest)
	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)
	latency, bifrostErr, wait := providerUtils.MakeRequestWithContext(ctx, provider.client, req, resp)
	defer wait()
	if bifrostErr != nil {
		return nil, latency, bifrostErr
	}
	if resp.StatusCode() < fasthttp.StatusOK || resp.StatusCode() >= fasthttp.StatusMultipleChoices {
		return nil, latency, providerUtils.EnrichError(ctx, parseGeminiError(resp), jsonData, append([]byte(nil), resp.Body()...), sendBackRawRequest, sendBackRawResponse, latency)
	}
	body, err := providerUtils.CheckAndDecodeBody(resp)
	if err != nil {
		return nil, latency, providerUtils.EnrichError(ctx, providerUtils.NewBifrostOperationError(schemas.ErrProviderResponseDecode, err), jsonData, nil, sendBackRawRequest, sendBackRawResponse, latency)
	}
	return append([]byte(nil), body...), latency, nil
}

// interactionVideoGeneration creates an Omni video interaction in the background.
func (provider *GeminiProvider) interactionVideoGeneration(ctx *schemas.BifrostContext, key schemas.Key, bifrostReq *schemas.BifrostVideoGenerationRequest) (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError) {
	jsonData, bifrostErr := providerUtils.CheckContextAndGetRequestBody(ctx, bifrostReq, func() (providerUtils.RequestBodyWithExtraParams, error) {
		return ToInteractionVideoRequest(bifrostReq)
	})
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	body, latency, bifrostErr := provider.doInteractionRequest(ctx, key, http.MethodPost, "/interactions", jsonData)
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	var interaction Interaction
	rawRequest, rawResponse, bifrostErr := providerUtils.HandleProviderResponse(body, &interaction, jsonData,
		providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest), providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse))
	if bifrostErr != nil {
		return nil, providerUtils.SetErrorLatency(bifrostErr, latency)
	}
	if interaction.ID == "" {
		return nil, providerUtils.SetErrorLatency(providerUtils.NewBifrostOperationError("gemini returned an interaction without an id", nil), latency)
	}
	resp := toBifrostInteractionVideo(&interaction, bifrostReq.Model, provider.GetProviderKey())
	if resp.CreatedAt == 0 {
		resp.CreatedAt = time.Now().Unix()
	}
	resp.ExtraFields.Latency = latency.Milliseconds()
	if providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest) {
		resp.ExtraFields.RawRequest = rawRequest
	}
	if providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse) {
		resp.ExtraFields.RawResponse = rawResponse
	}
	return resp, nil
}

// getInteraction reads an interaction by its escaped id.
func (provider *GeminiProvider) getInteraction(ctx *schemas.BifrostContext, key schemas.Key, id string) (*Interaction, []byte, time.Duration, *schemas.BifrostError) {
	body, latency, bifrostErr := provider.doInteractionRequest(ctx, key, http.MethodGet, "/interactions/"+id, nil)
	if bifrostErr != nil {
		return nil, nil, latency, bifrostErr
	}
	var interaction Interaction
	if err := sonic.Unmarshal(body, &interaction); err != nil {
		return nil, nil, latency, providerUtils.NewBifrostOperationError(schemas.ErrProviderResponseUnmarshal, err)
	}
	return &interaction, body, latency, nil
}

// interactionVideoRetrieve reads a video interaction's state.
func (provider *GeminiProvider) interactionVideoRetrieve(ctx *schemas.BifrostContext, key schemas.Key, id string) (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError) {
	interaction, body, latency, bifrostErr := provider.getInteraction(ctx, key, id)
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	resp := toBifrostInteractionVideo(interaction, "", provider.GetProviderKey())
	resp.ExtraFields.Latency = latency.Milliseconds()
	if providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse) {
		resp.ExtraFields.RawResponse = string(body)
	}
	return resp, nil
}

// interactionVideoDownload returns a completed interaction's video: its inline data, or the
// file its uri names.
func (provider *GeminiProvider) interactionVideoDownload(ctx *schemas.BifrostContext, key schemas.Key, videoID, id string) (*schemas.BifrostVideoDownloadResponse, *schemas.BifrostError) {
	interaction, _, latency, bifrostErr := provider.getInteraction(ctx, key, id)
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	if status := interactionVideoStatus(interaction.Status); status != schemas.VideoStatusCompleted {
		return nil, providerUtils.NewBifrostOperationError(fmt.Sprintf("video not ready, current status: %s", status), nil)
	}
	video := interaction.video()
	if video == nil {
		return nil, providerUtils.NewBifrostOperationError("the completed interaction carries no video", nil)
	}
	contentType := video.MimeType
	if contentType == "" {
		contentType = "video/mp4"
	}
	if video.Data != "" {
		content, err := base64.StdEncoding.DecodeString(video.Data)
		if err != nil {
			return nil, providerUtils.NewBifrostOperationError("failed to decode base64 video data", err)
		}
		resp := &schemas.BifrostVideoDownloadResponse{VideoID: videoID, Content: content, ContentType: contentType}
		resp.ExtraFields.Latency = latency.Milliseconds()
		return resp, nil
	}
	if video.URI == "" {
		return nil, providerUtils.NewBifrostOperationError("the interaction's video has neither data nor a uri", nil)
	}
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)
	providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)
	req.SetRequestURI(video.URI)
	req.Header.SetMethod(http.MethodGet)
	setGeminiAuthHeader(req, key.Value.GetValue())
	fileLatency, bifrostErr, wait := providerUtils.MakeRequestWithContextFollowRedirects(ctx, provider.client, req, resp, 5)
	defer wait()
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	if resp.StatusCode() != fasthttp.StatusOK {
		return nil, providerUtils.SetErrorLatency(providerUtils.NewBifrostOperationError(fmt.Sprintf("failed to download video: HTTP %d", resp.StatusCode()), nil), fileLatency)
	}
	content, err := providerUtils.CheckAndDecodeBody(resp)
	if err != nil {
		return nil, providerUtils.NewBifrostOperationError(schemas.ErrProviderResponseDecode, err)
	}
	out := &schemas.BifrostVideoDownloadResponse{VideoID: videoID, Content: append([]byte(nil), content...), ContentType: contentType}
	out.ExtraFields.Latency = (latency + fileLatency).Milliseconds()
	return out, nil
}

// interactionVideoDelete cancels a running video interaction and deletes a finished one.
func (provider *GeminiProvider) interactionVideoDelete(ctx *schemas.BifrostContext, key schemas.Key, videoID, id string) (*schemas.BifrostVideoDeleteResponse, *schemas.BifrostError) {
	interaction, _, latency, bifrostErr := provider.getInteraction(ctx, key, id)
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	method, path := http.MethodDelete, "/interactions/"+id
	if status := interactionVideoStatus(interaction.Status); status == schemas.VideoStatusInProgress || status == schemas.VideoStatusQueued {
		method, path = http.MethodPost, "/interactions/"+id+"/cancel"
	}
	_, opLatency, bifrostErr := provider.doInteractionRequest(ctx, key, method, path, nil)
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	resp := &schemas.BifrostVideoDeleteResponse{ID: videoID, Object: "video.deleted", Deleted: true}
	resp.ExtraFields.Latency = (latency + opLatency).Milliseconds()
	return resp, nil
}
