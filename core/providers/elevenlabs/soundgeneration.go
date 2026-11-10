package elevenlabs

import (
	"fmt"
	"net/http"

	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	schemas "github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

// ToElevenlabsSoundGenerationRequest maps a Bifrost speech request onto the
// sound-generation body. SFX-specific fields arrive via ExtraParams (they are not
// known fields of the speech schema) and are sent as given: checkSoundGenerationRanges
// refuses an out-of-range value before this runs, nothing is clamped (contract C4).
// model_id uses the canonical model resolved from virtual-key aliases so upstream
// receives a real ElevenLabs identifier, not the user-facing alias string.
func ToElevenlabsSoundGenerationRequest(ctx *schemas.BifrostContext, bifrostReq *schemas.BifrostSpeechRequest) *ElevenlabsSoundGenerationRequest {
	if bifrostReq == nil || bifrostReq.Input == nil {
		return nil
	}

	out := &ElevenlabsSoundGenerationRequest{
		Text:    bifrostReq.Input.Input,
		ModelID: schemas.ResolveCanonicalModel(ctx, bifrostReq.Model),
	}

	if bifrostReq.Params != nil && bifrostReq.Params.ExtraParams != nil {
		// Copy the caller's map before consuming keys: the original
		// BifrostSpeechRequest may be reused (e.g. fallback / multi-key retries),
		// so mutating it in place would silently drop these params on retry.
		extra := make(map[string]interface{}, len(bifrostReq.Params.ExtraParams))
		for k, v := range bifrostReq.Params.ExtraParams {
			extra[k] = v
		}
		out.ExtraParams = extra

		if v, ok := schemas.SafeExtractFloat64Pointer(extra["duration_seconds"]); ok {
			delete(extra, "duration_seconds")
			out.DurationSeconds = v
		}
		if v, ok := schemas.SafeExtractBoolPointer(extra["loop"]); ok {
			delete(extra, "loop")
			out.Loop = v
		}
		if v, ok := schemas.SafeExtractFloat64Pointer(extra["prompt_influence"]); ok {
			delete(extra, "prompt_influence")
			out.PromptInfluence = v
		}
	}

	return out
}

// Ranges ElevenLabs accepts on POST /v1/sound-generation.
const (
	soundDurationSecondsMin = 0.5
	soundDurationSecondsMax = 30.0
	soundPromptInfluenceMin = 0.0
	soundPromptInfluenceMax = 1.0
)

// checkSoundGenerationRanges returns 400 caller_unsupported_parameter, with error.param
// naming the field, when duration_seconds or prompt_influence is outside the range
// ElevenLabs accepts. duration_seconds sets the price, so an out-of-range value is refused
// before any upstream call and never clamped into range (TXE-6524, contract C4). A NaN is
// outside every range.
func checkSoundGenerationRanges(request *schemas.BifrostSpeechRequest) *schemas.BifrostError {
	if request == nil || request.Params == nil || len(request.Params.ExtraParams) == 0 {
		return nil
	}
	extra := request.Params.ExtraParams
	if v, ok := schemas.SafeExtractFloat64Pointer(extra["duration_seconds"]); ok && !(*v >= soundDurationSecondsMin && *v <= soundDurationSecondsMax) {
		return schemas.NewUnsupportedParameterError("duration_seconds", fmt.Sprintf("duration_seconds must be between %g and %g, got %g", soundDurationSecondsMin, soundDurationSecondsMax, *v))
	}
	if v, ok := schemas.SafeExtractFloat64Pointer(extra["prompt_influence"]); ok && !(*v >= soundPromptInfluenceMin && *v <= soundPromptInfluenceMax) {
		return schemas.NewUnsupportedParameterError("prompt_influence", fmt.Sprintf("prompt_influence must be between %g and %g, got %g", soundPromptInfluenceMin, soundPromptInfluenceMax, *v))
	}
	return nil
}

// soundGeneration performs a text-to-sound-effects request against
// POST /v1/sound-generation. It mirrors Speech() but targets a different endpoint
// and has no voice / timestamps concept. Returns audio bytes in BifrostSpeechResponse.
func (provider *ElevenlabsProvider) soundGeneration(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostSpeechRequest) (*schemas.BifrostSpeechResponse, *schemas.BifrostError) {
	if bifrostErr := checkSoundGenerationRanges(request); bifrostErr != nil {
		return nil, bifrostErr
	}

	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)

	// Reuses the speech URL builder for base URL + output_format query handling;
	// only the path differs. enable_logging / optimize_streaming_latency are not
	// set for sound requests, so no TTS-only query params leak in.
	requestURL := provider.buildBaseSpeechRequestURL(ctx, "/v1/sound-generation", schemas.SpeechRequest, request)
	req.SetRequestURI(requestURL)

	req.Header.SetMethod(http.MethodPost)
	req.Header.SetContentType("application/json")
	if key.Value.GetValue() != "" {
		req.Header.Set("xi-api-key", key.Value.GetValue())
	}

	jsonData, bifrostErr := providerUtils.CheckContextAndGetRequestBody(
		ctx,
		request,
		func() (providerUtils.RequestBodyWithExtraParams, error) {
			return ToElevenlabsSoundGenerationRequest(ctx, request), nil
		})
	if bifrostErr != nil {
		return nil, bifrostErr
	}

	if !providerUtils.ApplyLargePayloadRequestBody(ctx, req) {
		req.SetBody(jsonData)
	}

	latency, bifrostErr, wait := providerUtils.MakeRequestWithContext(ctx, provider.client, req, resp)
	defer wait()
	if bifrostErr != nil {
		return nil, providerUtils.EnrichError(ctx, bifrostErr, jsonData, nil, provider.sendBackRawRequest, provider.sendBackRawResponse)
	}
	ctx.SetValue(schemas.BifrostContextKeyProviderResponseHeaders, providerUtils.ExtractProviderResponseHeaders(resp))

	if resp.StatusCode() != fasthttp.StatusOK {
		return nil, providerUtils.EnrichError(ctx, parseElevenlabsError(resp), jsonData, nil, provider.sendBackRawRequest, provider.sendBackRawResponse)
	}

	ft, fh := providerUtils.StartPhaseSpan(ctx, "response-finalize")
	body, err := providerUtils.CheckAndDecodeBody(resp)
	if ft != nil {
		if err != nil {
			ft.EndSpan(fh, schemas.SpanStatusError, err.Error())
		} else {
			ft.EndSpan(fh, schemas.SpanStatusOk, "")
		}
	}
	if err != nil {
		return nil, providerUtils.EnrichError(ctx, providerUtils.NewBifrostOperationError(schemas.ErrProviderResponseDecode, err), jsonData, nil, provider.sendBackRawRequest, provider.sendBackRawResponse)
	}

	bifrostResponse := &schemas.BifrostSpeechResponse{
		Audio: body,
		ExtraFields: schemas.BifrostResponseExtraFields{
			Latency:                 latency.Milliseconds(),
			ProviderResponseHeaders: providerUtils.ExtractProviderResponseHeaders(resp),
		},
	}

	// usage carries only what ElevenLabs reported: the credits of the character-cost header.
	// The requested duration is a request value, not a vendor report, so audio_seconds stays
	// empty (TXE-6524, contract C5).
	bifrostResponse.Usage = speechUsageFromVendorCost(characterCostFromHeaders(resp))

	if providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest) {
		providerUtils.ParseAndSetRawRequest(&bifrostResponse.ExtraFields, jsonData)
	}

	return bifrostResponse, nil
}
