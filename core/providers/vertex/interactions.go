package vertex

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/capsohq/bifrost/core/providers/gemini"
	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

// Gemini Omni video on Vertex AI runs on the Interactions API (fork, TXE-6538), with the same
// request and response shapes as the Gemini API; the converters are gemini's. What Vertex
// documents (Gemini Enterprise Agent Platform, "Interactions API developer guide"):
//
//	POST https://aiplatform.googleapis.com/v1/projects/{project}/locations/{location}/interactions
//	GET  .../interactions/{id}
//	POST .../interactions/{id}/cancel
//
// on the global endpoint and the us and eu multi-region endpoints
// (aiplatform.{us|eu}.rep.googleapis.com); a single region is not supported. A stored
// interaction is kept 7 days.
//
// What Vertex does not document, and this code therefore does not send or map: webhook_config
// (callback_url and client_reference_id are refused), the form of a delivery=uri artifact
// (delivery is sent only when the caller names it, and a uri artifact is not fetched), and how a
// filtered interaction looks.

// vertexInteractionRetention is how long Vertex keeps a stored interaction, and with it the
// inline artifact: "Stored interactions persist for 7 days and are automatically deleted".
const vertexInteractionRetention = 7 * 24 * time.Hour

// vertexInteractionVideoOptions are the gemini converter options for Vertex.
var vertexInteractionVideoOptions = gemini.InteractionVideoOptions{
	StoredRetention: vertexInteractionRetention,
}

// vertexInteractionLocations are the locations the Interactions API serves.
var vertexInteractionLocations = []string{"global", "us", "eu"}

// vertexInteractionLocation returns region when the Interactions API serves it and a
// configuration error otherwise. The region is never replaced by another one: where an
// interaction runs and is stored is the operator's choice, made on the key or the alias.
func vertexInteractionLocation(region string) (string, *schemas.BifrostError) {
	for _, location := range vertexInteractionLocations {
		if region == location {
			return region, nil
		}
	}
	return "", providerUtils.NewConfigurationError(fmt.Sprintf(
		"the Vertex Interactions API serves only the locations %s, got region %q: set the region of the key or of the model alias to one of them",
		strings.Join(vertexInteractionLocations, ", "), region))
}

// vertexInteractionURL returns the URL of an interaction resource name
// ("projects/{project}/locations/{location}/interactions/{id}"), on the host of its own
// location, and false when name is not one. That resource name is the vendor task id of the
// unified task id: it carries the project and the location, so a query needs neither from the
// key that happens to be selected, and its "interactions" segment tells it from a Veo
// operation name.
func vertexInteractionURL(name string) (string, bool, *schemas.BifrostError) {
	raw := strings.Split(name, "/")
	if len(raw) != 6 || raw[4] != "interactions" {
		return "", false, nil
	}
	parts, bifrostErr := parseVertexResourceName(name, "video_id", "projects", "", "locations", "", "interactions", "")
	if bifrostErr != nil {
		return "", true, bifrostErr
	}
	if _, bifrostErr := vertexInteractionLocation(parts[3]); bifrostErr != nil {
		return "", true, providerUtils.NewBifrostBadRequestError("invalid video_id: unexpected region")
	}
	return getVertexAPIBaseURL(parts[3], "v1") + "/" + strings.Join(parts, "/"), true, nil
}

// interactionCall is one call to the Vertex Interactions API with the key's credentials. It
// returns the decoded body of a 200 and the response headers.
func (provider *VertexProvider) interactionCall(ctx *schemas.BifrostContext, key schemas.Key, method, completeURL string, jsonData []byte) ([]byte, map[string]string, time.Duration, *schemas.BifrostError) {
	sendBackRawRequest := providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest)
	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)

	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	req.Header.SetMethod(method)
	providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)
	if jsonData != nil {
		req.Header.SetContentType("application/json")
		req.SetBody(jsonData)
	}
	if apiKey := key.Value.GetValue(); apiKey != "" {
		completeURL = fmt.Sprintf("%s?key=%s", completeURL, url.QueryEscape(apiKey))
	} else {
		tokenSource, err := provider.getAuthTokenSource(key)
		if err != nil {
			return nil, nil, 0, providerUtils.NewBifrostOperationError("error creating auth token source", err)
		}
		token, err := tokenSource.Token()
		if err != nil {
			return nil, nil, 0, providerUtils.NewBifrostOperationError("error getting token", err)
		}
		req.Header.Set("Authorization", "Bearer "+token.AccessToken)
	}
	req.SetRequestURI(completeURL)

	latency, bifrostErr, wait := providerUtils.MakeRequestWithContext(ctx, provider.client, req, resp)
	defer wait()
	if bifrostErr != nil {
		return nil, nil, latency, providerUtils.EnrichError(ctx, bifrostErr, jsonData, nil, sendBackRawRequest, sendBackRawResponse, latency)
	}
	headers := providerUtils.ExtractProviderResponseHeaders(resp)
	ctx.SetValue(schemas.BifrostContextKeyProviderResponseHeaders, headers)

	if resp.StatusCode() != fasthttp.StatusOK {
		if resp.StatusCode() == fasthttp.StatusUnauthorized || resp.StatusCode() == fasthttp.StatusForbidden {
			provider.removeVertexClient(key)
		}
		respBody := append([]byte(nil), resp.Body()...)
		return nil, headers, latency, providerUtils.EnrichError(ctx, parseVertexError(resp), jsonData, respBody, sendBackRawRequest, sendBackRawResponse, latency)
	}
	body, err := providerUtils.CheckAndDecodeBody(resp)
	if err != nil {
		return nil, headers, latency, providerUtils.NewBifrostOperationError(schemas.ErrProviderResponseDecode, err)
	}
	return append([]byte(nil), body...), headers, latency, nil
}

// interactionTaskObject parses an Interaction body and converts it to the task object. The task
// id is the interaction's resource name under resourcePrefix
// ("projects/{project}/locations/{location}").
func (provider *VertexProvider) interactionTaskObject(ctx *schemas.BifrostContext, resourcePrefix string, body, jsonData []byte, headers map[string]string, latency time.Duration) (*schemas.BifrostVideoGenerationResponse, *gemini.GeminiInteraction, *schemas.BifrostError) {
	sendBackRawRequest := providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest)
	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)

	var interaction gemini.GeminiInteraction
	rawRequest, rawResponse, bifrostErr := providerUtils.HandleProviderResponse(body, &interaction, jsonData, sendBackRawRequest, sendBackRawResponse)
	if bifrostErr != nil {
		return nil, nil, bifrostErr
	}
	bifrostResp, bifrostErr := gemini.ToBifrostInteractionVideoResponse(&interaction, vertexInteractionVideoOptions)
	if bifrostErr != nil {
		return nil, nil, bifrostErr
	}
	bifrostResp.ID = providerUtils.AddVideoIDProviderSuffix(resourcePrefix+"/interactions/"+bifrostResp.ID, provider.GetProviderKey())
	bifrostResp.ExtraFields.Latency = latency.Milliseconds()
	bifrostResp.ExtraFields.ProviderResponseHeaders = headers
	if sendBackRawRequest {
		bifrostResp.ExtraFields.RawRequest = rawRequest
	}
	if sendBackRawResponse {
		bifrostResp.ExtraFields.RawResponse = rawResponse
	}
	return bifrostResp, &interaction, nil
}

// interactionVideoCreate creates an Omni video task on Vertex: POST .../interactions with
// background true.
func (provider *VertexProvider) interactionVideoCreate(ctx *schemas.BifrostContext, key schemas.Key, bifrostReq *schemas.BifrostVideoGenerationRequest) (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError) {
	jsonData, bifrostErr := providerUtils.CheckContextAndGetRequestBody(
		ctx,
		bifrostReq,
		func() (providerUtils.RequestBodyWithExtraParams, error) {
			return gemini.ToGeminiInteractionVideoRequest(bifrostReq, vertexInteractionVideoOptions)
		},
	)
	if bifrostErr != nil {
		return nil, bifrostErr
	}

	projectID := resolveVertexProjectID(ctx, key)
	if projectID == "" {
		return nil, providerUtils.NewConfigurationError("project ID is not set")
	}
	region := resolveVertexRegion(ctx, key)
	if region == "" {
		return nil, providerUtils.NewConfigurationError("region is not set in key config")
	}
	location, bifrostErr := vertexInteractionLocation(region)
	if bifrostErr != nil {
		return nil, bifrostErr
	}

	resourcePrefix := "projects/" + projectID + "/locations/" + location
	body, headers, latency, bifrostErr := provider.interactionCall(ctx, key, http.MethodPost, getVertexProjectLocationURL(location, "v1", projectID)+"/interactions", jsonData)
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	bifrostResp, _, bifrostErr := provider.interactionTaskObject(ctx, resourcePrefix, body, jsonData, headers, latency)
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	if bifrostResp.Model == "" {
		bifrostResp.Model = gemini.NormalizeModelName(bifrostReq.Model)
	}
	return bifrostResp, nil
}

// interactionVideoRetrieve queries an Omni video task on Vertex: GET of the interaction's own
// URL (name is its resource name, interactionURL the URL vertexInteractionURL built from it).
func (provider *VertexProvider) interactionVideoRetrieve(ctx *schemas.BifrostContext, key schemas.Key, name, interactionURL string) (*schemas.BifrostVideoGenerationResponse, *gemini.GeminiInteraction, *schemas.BifrostError) {
	body, headers, latency, bifrostErr := provider.interactionCall(ctx, key, http.MethodGet, interactionURL, nil)
	if bifrostErr != nil {
		return nil, nil, bifrostErr
	}
	return provider.interactionTaskObject(ctx, name[:strings.LastIndex(name, "/interactions/")], body, nil, headers, latency)
}

// interactionVideoDownload returns the inline video of a completed Omni task on Vertex.
func (provider *VertexProvider) interactionVideoDownload(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostVideoDownloadRequest, name, interactionURL string) (*schemas.BifrostVideoDownloadResponse, *schemas.BifrostError) {
	videoResp, interaction, bifrostErr := provider.interactionVideoRetrieve(ctx, key, name, interactionURL)
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	if videoResp.Status != schemas.VideoStatusCompleted {
		return nil, providerUtils.NewBifrostOperationError(
			fmt.Sprintf("video not ready, current status: %s", videoResp.Status), nil)
	}
	artifact := gemini.InteractionVideoArtifact(interaction)
	if artifact == nil {
		return nil, providerUtils.NewBifrostOperationError("the interaction holds no video", nil)
	}
	if artifact.Data == "" {
		// Vertex documents no form for a delivery=uri artifact, so there is no host the
		// credentials are known to be meant for. The uri is left to the caller, in the raw
		// response.
		return nil, providerUtils.NewBifrostOperationError(
			"the interaction returned its video as a uri; fetching a uri artifact is not supported for the Vertex Interactions API", nil)
	}
	content, contentType, bifrostErr := gemini.DecodeInteractionVideoData(artifact)
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	bifrostResp := &schemas.BifrostVideoDownloadResponse{
		VideoID:     request.ID,
		Content:     content,
		ContentType: contentType,
	}
	bifrostResp.ExtraFields.Latency = videoResp.ExtraFields.Latency
	bifrostResp.ExtraFields.ProviderResponseHeaders = videoResp.ExtraFields.ProviderResponseHeaders
	return bifrostResp, nil
}

// VideoCancel implements schemas.VideoCanceller (fork, TXE-6538). Only an Omni task has a cancel
// API: POST .../interactions/{id}/cancel. The task is queried once and cancelled only while it
// is in_progress; any other state, and a Veo operation, is refused with
// provider_cancel_unsupported. The vendor's DELETE is never called.
func (provider *VertexProvider) VideoCancel(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostVideoCancelRequest) (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError) {
	if request == nil || request.ID == "" {
		return nil, providerUtils.NewBifrostBadRequestError("video_id is required")
	}
	name := providerUtils.StripVideoIDProviderSuffix(request.ID, provider.GetProviderKey())
	interactionURL, isInteraction, bifrostErr := vertexInteractionURL(name)
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	if !isInteraction {
		return nil, schemas.NewVideoCancelUnsupportedError("this provider cancels only an Interactions API task; a Veo operation has no cancel API")
	}
	current, interaction, bifrostErr := provider.interactionVideoRetrieve(ctx, key, name, interactionURL)
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	if bifrostErr := gemini.CheckInteractionCancellable(interaction, current.ExtraFields.RawResponse); bifrostErr != nil {
		return nil, bifrostErr
	}
	body, headers, latency, bifrostErr := provider.interactionCall(ctx, key, http.MethodPost, interactionURL+"/cancel", nil)
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	bifrostResp, _, bifrostErr := provider.interactionTaskObject(ctx, name[:strings.LastIndex(name, "/interactions/")], body, nil, headers, latency)
	return bifrostResp, bifrostErr
}
