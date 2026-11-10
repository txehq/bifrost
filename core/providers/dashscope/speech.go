package dashscope

import (
	"time"

	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	schemas "github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

// multimodalGenerationPath is DashScope's native endpoint for Qwen3-TTS and Qwen3-ASR.
const multimodalGenerationPath = "/api/v1/services/aigc/multimodal-generation/generation"

// speechFields are the unified TTS fields Qwen3-TTS takes: voice → input.voice and
// language_code → input.language_type (input is the request's text, not a Params field).
// response_format, speed and instructions have no Qwen3-TTS counterpart, so core refuses
// them with caller_unsupported_parameter before a key is chosen (contract C4, BF4).
var speechFields = []string{"voice", "language_code"}

// transcriptionFields are the unified STT fields Qwen3-ASR takes: language →
// asr_options.language, prompt → the system message's context text, and response_format,
// of which only "json" can be delivered (checked in the converter).
// timestamp_granularities and every other field are refused.
var transcriptionFields = []string{"language", "prompt", "response_format"}

// speechConvertedExtraParams are names that may sit in ExtraParams as well as in the typed
// Params (the HTTP route keeps unknown JSON keys there). Bifrost has already converted
// them into the vendor's own field, so they never also reach the body's top level.
var speechConvertedExtraParams = []string{"language_code"}

// ToDashScopeTTSRequest converts a unified speech request into a Qwen3-TTS body. A
// multi-speaker voice config cannot be delivered and is refused naming voice.
func ToDashScopeTTSRequest(request *schemas.BifrostSpeechRequest) (*DashScopeTTSRequest, error) {
	if request == nil || request.Input == nil || request.Input.Input == "" {
		return nil, providerUtils.ConverterBifrostError(schemas.NewUnsupportedParameterError("input", "input is required for DashScope speech"))
	}
	body := &DashScopeTTSRequest{
		Model: request.Model,
		Input: DashScopeTTSInput{Text: request.Input.Input},
	}
	params := request.Params
	if params == nil || params.VoiceConfig == nil || params.VoiceConfig.Voice == nil || *params.VoiceConfig.Voice == "" {
		return nil, providerUtils.ConverterBifrostError(schemas.NewUnsupportedParameterError("voice", "DashScope speech takes one voice name; multi-speaker voice configs are not supported"))
	}
	body.Input.Voice = *params.VoiceConfig.Voice
	if params.LanguageCode != nil {
		body.Input.LanguageType = *params.LanguageCode
	}
	body.ExtraParams = withoutKeys(params.ExtraParams, speechConvertedExtraParams...)
	return body, nil
}

// ToBifrostSpeechResponse converts a Qwen3-TTS answer. The audio is the vendor's link and
// its expiry (contract C9); usage.characters is the vendor's billed character count and
// goes to usage.billed_characters (contract C5). An answer with neither a link nor audio
// data is the vendor's empty result.
func ToBifrostSpeechResponse(resp *DashScopeTTSResponse) (*schemas.BifrostSpeechResponse, *schemas.BifrostError) {
	audio := resp.Output.Audio
	if audio == nil || (audio.URL == "" && audio.Data == "") {
		return nil, schemas.NewProviderEmptyResultError("DashScope speech response carries no audio link")
	}
	out := &schemas.BifrostSpeechResponse{}
	if audio.URL != "" {
		out.URL = schemas.Ptr(audio.URL)
		out.ExpiresAt = audio.ExpiresAt
	} else {
		// A non-streamed call answers with a link; base64 data here is not documented for
		// the sync call. Keep it as the vendor sent it rather than decode it.
		out.AudioBase64 = schemas.Ptr(audio.Data)
	}
	if u := resp.Usage; u != nil {
		usage := &schemas.SpeechUsage{BilledCharacters: u.Characters}
		if u.InputTokens != nil {
			usage.InputTokens = *u.InputTokens
		}
		if u.OutputTokens != nil {
			usage.OutputTokens = *u.OutputTokens
		}
		if u.TotalTokens != nil {
			usage.TotalTokens = *u.TotalTokens
		}
		out.Usage = usage
	}
	return out, nil
}

// Speech synthesizes speech with Qwen3-TTS: one non-streamed call to the regional base. The
// response carries the vendor's audio link, its expires_at and the vendor's usage.
func (provider *DashScopeProvider) Speech(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostSpeechRequest) (*schemas.BifrostSpeechResponse, *schemas.BifrostError) {
	jsonData, bifrostErr := providerUtils.CheckContextAndGetRequestBody(ctx, request,
		func() (providerUtils.RequestBodyWithExtraParams, error) {
			return ToDashScopeTTSRequest(request)
		})
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	sendBackRawRequest := providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest)
	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)

	url := provider.baseURLFor(schemas.SpeechRequest) + multimodalGenerationPath
	body, latency, bifrostErr := provider.post(ctx, key, url, jsonData, sendBackRawRequest, sendBackRawResponse)
	if bifrostErr != nil {
		return nil, bifrostErr
	}

	var ttsResp DashScopeTTSResponse
	rawRequest, rawResponse, bifrostErr := providerUtils.HandleProviderResponseCtx(ctx, body, &ttsResp, jsonData, sendBackRawRequest, sendBackRawResponse)
	if bifrostErr != nil {
		return nil, providerUtils.SetErrorLatency(bifrostErr, latency)
	}
	if bifrostErr := vendorErrorIn200(ttsResp.Code, ttsResp.Message); bifrostErr != nil {
		return nil, providerUtils.EnrichError(ctx, bifrostErr, jsonData, body, sendBackRawRequest, sendBackRawResponse, latency)
	}
	bifrostResp, bifrostErr := ToBifrostSpeechResponse(&ttsResp)
	if bifrostErr != nil {
		return nil, providerUtils.EnrichError(ctx, bifrostErr, jsonData, body, sendBackRawRequest, sendBackRawResponse, latency)
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

// vendorErrorIn200 turns a DashScope 200 that carries an error code into the error it
// stands for (contract C6): a content-inspection refusal is the caller's content, any other
// code the vendor's failure. Nil when code is empty.
func vendorErrorIn200(code, message string) *schemas.BifrostError {
	if code == "" {
		return nil
	}
	if message == "" {
		message = "DashScope returned error code " + code
	}
	cause := schemas.VendorErrorCauseProvider
	if dashScopeErrorType(code) == schemas.ErrorTypeCallerContentFiltered {
		cause = schemas.VendorErrorCauseContentFiltered
	}
	return schemas.NewVendorErrorObjectError(cause, code, message)
}

// post sends one JSON request with DashScope's bearer key and returns a copy of the decoded
// body of a 2xx answer. A non-2xx answer becomes an error that keeps the vendor's status,
// code and body (parseDashScopeError).
func (provider *DashScopeProvider) post(ctx *schemas.BifrostContext, key schemas.Key, url string, jsonData []byte, sendBackRawRequest, sendBackRawResponse bool) ([]byte, time.Duration, *schemas.BifrostError) {
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	provider.setRequestHeaders(ctx, req, key)
	req.SetRequestURI(url)
	req.Header.SetMethod(fasthttp.MethodPost)
	req.Header.SetContentType("application/json")
	req.SetBody(jsonData)

	latency, bifrostErr, wait := providerUtils.MakeRequestWithContext(ctx, provider.client, req, resp)
	defer wait()
	if bifrostErr != nil {
		return nil, latency, bifrostErr
	}
	if resp.StatusCode() < fasthttp.StatusOK || resp.StatusCode() >= fasthttp.StatusMultipleChoices {
		var errBody []byte
		if decoded, err := providerUtils.CheckAndDecodeBody(resp); err == nil {
			errBody = append([]byte(nil), decoded...)
		}
		return nil, latency, providerUtils.EnrichError(ctx, parseDashScopeError(resp), jsonData, errBody, sendBackRawRequest, sendBackRawResponse, latency)
	}

	body, err := providerUtils.CheckAndDecodeBody(resp)
	if err != nil {
		rawErrBody := append([]byte(nil), resp.Body()...)
		return nil, latency, providerUtils.EnrichError(ctx, providerUtils.NewBifrostOperationError(schemas.ErrProviderResponseDecode, err), jsonData, rawErrBody, sendBackRawRequest, sendBackRawResponse, latency)
	}
	return append([]byte(nil), body...), latency, nil
}

// withoutKeys returns a copy of params without the given keys, or nil when nothing is left.
func withoutKeys(params map[string]interface{}, keys ...string) map[string]interface{} {
	if len(params) == 0 {
		return nil
	}
	out := make(map[string]interface{}, len(params))
	for k, v := range params {
		out[k] = v
	}
	for _, k := range keys {
		delete(out, k)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
