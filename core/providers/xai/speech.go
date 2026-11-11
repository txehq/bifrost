package xai

import (
	"encoding/base64"
	"fmt"
	"maps"
	"math"
	"net/http"
	"slices"
	"strings"

	"github.com/bytedance/sonic"
	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	schemas "github.com/capsohq/bifrost/core/schemas"
	"github.com/tidwall/sjson"
	"github.com/valyala/fasthttp"
)

// xAI text to speech (fork, TXE-6429, contract C4/C5): POST /v1/tts. xAI answers with the
// audio bytes, or, when with_timestamps is true, with a JSON envelope holding the audio as
// base64, its duration and character timings.

// XAISpeechOutputFormat is the output_format object of a /v1/tts request.
type XAISpeechOutputFormat struct {
	Codec      string `json:"codec"`
	SampleRate *int   `json:"sample_rate,omitempty"`
	BitRate    *int   `json:"bit_rate,omitempty"`
}

// XAISpeechRequest is the body of POST /v1/tts.
type XAISpeechRequest struct {
	Text           string                 `json:"text"`
	VoiceID        string                 `json:"voice_id,omitempty"`
	Language       string                 `json:"language"`
	OutputFormat   *XAISpeechOutputFormat `json:"output_format,omitempty"`
	Speed          *float64               `json:"speed,omitempty"`
	WithTimestamps *bool                  `json:"with_timestamps,omitempty"`

	// ExtraParams are the caller's fields outside the unified structure (text_normalization,
	// replace, optimize_streaming_latency and any other xAI field), merged unchanged.
	ExtraParams map[string]interface{} `json:"-"`
}

// GetExtraParams implements providerUtils.RequestBodyWithExtraParams.
func (r *XAISpeechRequest) GetExtraParams() map[string]interface{} {
	return r.ExtraParams
}

// XAISpeechResponse is the JSON envelope xAI answers with when with_timestamps is true.
type XAISpeechResponse struct {
	Audio           string               `json:"audio"`        // base64 audio in the requested codec
	ContentType     string               `json:"content_type"` // media type of the decoded audio
	Duration        *float64             `json:"duration,omitempty"`
	AudioTimestamps *XAISpeechTimestamps `json:"audio_timestamps,omitempty"`
}

// XAISpeechTimestamps are the character timings of the envelope.
type XAISpeechTimestamps struct {
	GraphChars []string          `json:"graph_chars"`
	GraphTimes []XAISpeechTiming `json:"graph_times"`
}

// XAISpeechTiming is one character's start and end, in seconds. xAI's reference shows it as
// {"start", "end"} and its guide as a [start, end] pair; both decode.
type XAISpeechTiming struct {
	Start float64
	End   float64
}

// UnmarshalJSON implements json.Unmarshaler; see XAISpeechTiming.
func (t *XAISpeechTiming) UnmarshalJSON(data []byte) error {
	var object struct {
		Start float64 `json:"start"`
		End   float64 `json:"end"`
	}
	if err := sonic.Unmarshal(data, &object); err == nil {
		t.Start, t.End = object.Start, object.End
		return nil
	}
	var pair []float64
	if err := sonic.Unmarshal(data, &pair); err != nil {
		return err
	}
	if len(pair) != 2 {
		return fmt.Errorf("graph_times entry has %d values, want 2", len(pair))
	}
	t.Start, t.End = pair[0], pair[1]
	return nil
}

// xaiSpeechDefaultLanguage is sent when the caller gives no language_code. xAI requires
// language and takes "auto" to detect it from the text.
const xaiSpeechDefaultLanguage = "auto"

// xaiSpeechDefaultCodec is xAI's default codec, also the default of the unified
// response_format. It is written only when the caller sets a sample rate or bit rate without
// a response_format, because output_format requires codec.
const xaiSpeechDefaultCodec = "mp3"

// xaiSpeechCodecs are the codecs /v1/tts documents. A response_format outside them cannot be
// delivered and is refused.
var xaiSpeechCodecs = []string{"mp3", "wav", "pcm", "mulaw", "alaw"}

// xaiSpeechUnifiedFields are the unified speech fields /v1/tts takes. Core refuses every other
// one (instructions, pronunciation_dictionary_locators, enable_logging and the boolean
// optimize_streaming_latency) with 400 caller_unsupported_parameter before a key is chosen.
var xaiSpeechUnifiedFields = []string{"voice", "response_format", "speed", "language_code", "with_timestamps"}

// The extra fields that carry the rest of output_format.
const (
	xaiSpeechSampleRateParam = "sample_rate"
	xaiSpeechBitRateParam    = "bit_rate"
)

// ToXAISpeechRequest converts a Bifrost speech request into xAI's body. Every unified field is
// sent as given or refused with caller_unsupported_parameter (contract C4).
func ToXAISpeechRequest(bifrostReq *schemas.BifrostSpeechRequest) (*XAISpeechRequest, error) {
	if bifrostReq == nil || bifrostReq.Input == nil || bifrostReq.Input.Input == "" {
		return nil, providerUtils.InvalidRequestErrorf("input is required for speech")
	}
	req := &XAISpeechRequest{Text: bifrostReq.Input.Input, Language: xaiSpeechDefaultLanguage}
	params := bifrostReq.Params
	if params == nil {
		return req, nil
	}
	if voice := params.VoiceConfig; voice != nil {
		if len(voice.MultiVoiceConfig) > 0 {
			return nil, providerUtils.ConverterBifrostError(schemas.NewUnsupportedParameterError("voice",
				"xAI text to speech takes one voice, not a multi-speaker voice list"))
		}
		if voice.Voice != nil {
			req.VoiceID = *voice.Voice
		}
	}
	if params.LanguageCode != nil && *params.LanguageCode != "" {
		req.Language = *params.LanguageCode
	}
	req.Speed = params.Speed
	req.WithTimestamps = params.WithTimestamps

	if len(params.ExtraParams) > 0 {
		req.ExtraParams = make(map[string]interface{}, len(params.ExtraParams))
		maps.Copy(req.ExtraParams, params.ExtraParams)
		// The handlers keep these typed fields in the extra params too; they were converted
		// above and must not also reach xAI under their unified names.
		delete(req.ExtraParams, "language_code")
		delete(req.ExtraParams, "with_timestamps")
	}

	sampleRate, err := xaiSpeechIntParam(req.ExtraParams, xaiSpeechSampleRateParam)
	if err != nil {
		return nil, err
	}
	bitRate, err := xaiSpeechIntParam(req.ExtraParams, xaiSpeechBitRateParam)
	if err != nil {
		return nil, err
	}
	codec := strings.ToLower(strings.TrimSpace(params.ResponseFormat))
	if codec != "" && !slices.Contains(xaiSpeechCodecs, codec) {
		return nil, providerUtils.ConverterBifrostError(schemas.NewUnsupportedParameterError("response_format", fmt.Sprintf(
			"response_format %q is not a codec xAI text to speech produces (%s)", params.ResponseFormat, strings.Join(xaiSpeechCodecs, ", "))))
	}
	if codec != "" || sampleRate != nil || bitRate != nil {
		if codec == "" {
			codec = xaiSpeechDefaultCodec
		}
		req.OutputFormat = &XAISpeechOutputFormat{Codec: codec, SampleRate: sampleRate, BitRate: bitRate}
	}
	return req, nil
}

// xaiSpeechIntParam takes a whole-number extra field out of extraParams. A value that is not
// a whole number is refused, never rounded.
func xaiSpeechIntParam(extraParams map[string]interface{}, name string) (*int, error) {
	raw, ok := extraParams[name]
	if !ok {
		return nil, nil
	}
	delete(extraParams, name)
	value, ok := schemas.SafeExtractFloat64(raw)
	if !ok || value != math.Trunc(value) {
		return nil, providerUtils.ConverterBifrostError(schemas.NewUnsupportedParameterError(name, fmt.Sprintf(
			"%s must be a whole number", name)))
	}
	return schemas.Ptr(int(value)), nil
}

// ToBifrostSpeechResponse converts xAI's answer. A JSON answer is the timestamps envelope:
// its base64 audio is decoded and returned as bytes, its duration becomes usage.audio_seconds
// and its character timings the alignment. Any other answer is the audio itself; xAI reports
// no usage with it, so usage stays empty (contract C5).
func ToBifrostSpeechResponse(contentType string, body []byte) (*schemas.BifrostSpeechResponse, *schemas.BifrostError) {
	if !isJSONContentType(contentType) {
		return &schemas.BifrostSpeechResponse{Audio: body, ContentType: contentType}, nil
	}
	var envelope XAISpeechResponse
	if err := sonic.Unmarshal(body, &envelope); err != nil {
		return nil, providerUtils.NewBifrostOperationError(schemas.ErrProviderResponseUnmarshal, err)
	}
	audio, err := base64.StdEncoding.DecodeString(envelope.Audio)
	if err != nil {
		return nil, providerUtils.NewBifrostOperationError("xai returned audio that is not valid base64", err)
	}
	resp := &schemas.BifrostSpeechResponse{Audio: audio, ContentType: envelope.ContentType}
	if envelope.Duration != nil {
		// audio_seconds is a whole number of seconds; the exact figure stays in raw_response.
		resp.Usage = &schemas.SpeechUsage{AudioSeconds: int(math.Round(*envelope.Duration))}
	}
	if timestamps := envelope.AudioTimestamps; timestamps != nil && len(timestamps.GraphChars) == len(timestamps.GraphTimes) && len(timestamps.GraphChars) > 0 {
		alignment := &schemas.SpeechAlignment{
			Characters:       timestamps.GraphChars,
			CharStartTimesMs: make([]float64, len(timestamps.GraphTimes)),
			CharEndTimesMs:   make([]float64, len(timestamps.GraphTimes)),
		}
		for i, timing := range timestamps.GraphTimes {
			alignment.CharStartTimesMs[i] = timing.Start * 1000
			alignment.CharEndTimesMs[i] = timing.End * 1000
		}
		resp.Alignment = alignment
	}
	return resp, nil
}

// isJSONContentType reports whether a Content-Type header names JSON.
func isJSONContentType(contentType string) bool {
	mediaType, _, _ := strings.Cut(contentType, ";")
	return strings.EqualFold(strings.TrimSpace(mediaType), "application/json")
}

// xaiSpeechRawResponse is the envelope without its audio, for extra_fields.raw_response: the
// audio is already the response body and would double it.
func xaiSpeechRawResponse(body []byte) interface{} {
	stripped, err := sjson.DeleteBytes(body, "audio")
	if err != nil {
		return nil
	}
	var raw interface{}
	if err := sonic.Unmarshal(stripped, &raw); err != nil {
		return nil
	}
	return raw
}

// Speech performs a text to speech request against xAI's /v1/tts. The caller's extra fields
// are always merged into the body.
func (provider *XAIProvider) Speech(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostSpeechRequest) (*schemas.BifrostSpeechResponse, *schemas.BifrostError) {
	if bifrostErr := providerUtils.CheckOperationAllowed(schemas.XAI, provider.customProviderConfig, schemas.SpeechRequest); bifrostErr != nil {
		return nil, bifrostErr
	}
	if bifrostErr := refuseLargePayload(ctx, "speech"); bifrostErr != nil {
		return nil, bifrostErr
	}
	if request != nil && request.Params != nil {
		if bifrostErr := rejectModelExtraParam(request.Params.ExtraParams); bifrostErr != nil {
			return nil, bifrostErr
		}
	}
	ctx.SetValue(schemas.BifrostContextKeyPassthroughExtraParams, true)
	jsonData, bifrostErr := providerUtils.CheckContextAndGetRequestBody(ctx, request, func() (providerUtils.RequestBodyWithExtraParams, error) {
		return ToXAISpeechRequest(request)
	})
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	sendBackRawRequest := providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest)
	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)

	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)
	req.SetRequestURI(provider.networkConfig.BaseURL + providerUtils.GetPathFromContext(ctx, "/v1/tts"))
	req.Header.SetMethod(http.MethodPost)
	req.Header.SetContentType("application/json")
	if value := key.Value.GetValue(); value != "" {
		req.Header.Set("Authorization", "Bearer "+value)
	}
	req.SetBody(jsonData)

	latency, bifrostErr, wait := providerUtils.MakeRequestWithContext(ctx, provider.client, req, resp)
	defer wait()
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	ctx.SetValue(schemas.BifrostContextKeyProviderResponseHeaders, providerUtils.ExtractProviderResponseHeaders(resp))
	if resp.StatusCode() < fasthttp.StatusOK || resp.StatusCode() >= fasthttp.StatusMultipleChoices {
		return nil, providerUtils.EnrichError(ctx, ParseXAIError(resp), jsonData, nil, sendBackRawRequest, sendBackRawResponse, latency)
	}
	body, err := providerUtils.CheckAndDecodeBody(resp)
	if err != nil {
		return nil, providerUtils.SetErrorLatency(providerUtils.NewBifrostOperationError(schemas.ErrProviderResponseDecode, err), latency)
	}
	// The response buffer is pooled; the audio outlives it.
	body = append([]byte(nil), body...)

	contentType := string(resp.Header.ContentType())
	bifrostResp, bifrostErr := ToBifrostSpeechResponse(contentType, body)
	if bifrostErr != nil {
		return nil, providerUtils.EnrichError(ctx, bifrostErr, jsonData, nil, sendBackRawRequest, sendBackRawResponse, latency)
	}
	bifrostResp.ExtraFields.Latency = latency.Milliseconds()
	if sendBackRawRequest {
		providerUtils.ParseAndSetRawRequest(&bifrostResp.ExtraFields, jsonData)
	}
	if sendBackRawResponse && isJSONContentType(contentType) {
		bifrostResp.ExtraFields.RawResponse = xaiSpeechRawResponse(body)
	}
	return bifrostResp, nil
}
