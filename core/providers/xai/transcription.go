package xai

import (
	"bytes"
	"fmt"
	"mime/multipart"
	"net/http"
	"slices"
	"strconv"

	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	schemas "github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

// xAI speech to text (fork, TXE-6429, contract C4/C5): POST /v1/stt, multipart. xAI reads the
// form as a stream and may ignore a field that follows the file, so file is always written
// last.

// XAITranscriptionWord is one word of an /v1/stt answer.
type XAITranscriptionWord struct {
	Text    string  `json:"text"`
	Start   float64 `json:"start"`
	End     float64 `json:"end"`
	Speaker *int    `json:"speaker,omitempty"` // only with diarize=true
}

// XAITranscriptionResponse is the answer to POST /v1/stt. channels (multichannel=true) has no
// unified field; it reaches the caller in extra_fields.raw_response.
type XAITranscriptionResponse struct {
	Text     string                 `json:"text"`
	Language string                 `json:"language,omitempty"`
	Duration *float64               `json:"duration,omitempty"` // seconds of audio
	Words    []XAITranscriptionWord `json:"words,omitempty"`
}

// xaiTranscriptionUnifiedFields are the unified transcription fields /v1/stt takes. Core
// refuses every other one (response_format, timestamp_granularities, temperature and the
// rest) with 400 caller_unsupported_parameter before a key is chosen; prompt is exempt from
// the check (contract C4) and xAI has no field for it, so it is not sent.
var xaiTranscriptionUnifiedFields = []string{"language"}

// AcceptsTranscriptionURLInput declares that /v1/stt takes an audio URL in place of the file:
// the URL goes to xAI unchanged as the url form field and Bifrost does not download it.
func (provider *XAIProvider) AcceptsTranscriptionURLInput() bool {
	return true
}

// WriteXAITranscriptionForm writes the multipart form of an /v1/stt request: model, language,
// the caller's extra fields unchanged (a repeated field such as keyterm once per value, url
// among them), and the file last.
func WriteXAITranscriptionForm(writer *multipart.Writer, request *schemas.BifrostTranscriptionRequest) error {
	if request.Model != "" {
		if err := writer.WriteField("model", request.Model); err != nil {
			return err
		}
	}
	if params := request.Params; params != nil {
		if params.Language != nil && *params.Language != "" {
			if err := writer.WriteField("language", *params.Language); err != nil {
				return err
			}
		}
		// Sorted, so the same request always produces the same form.
		names := make([]string, 0, len(params.ExtraParams))
		for name := range params.ExtraParams {
			names = append(names, name)
		}
		slices.Sort(names)
		for _, name := range names {
			if name == "file" || name == "model" || name == "language" {
				continue // written from the typed fields
			}
			for _, value := range xaiFormValues(params.ExtraParams[name]) {
				if err := writer.WriteField(name, value); err != nil {
					return err
				}
			}
		}
	}
	if request.Input == nil || len(request.Input.File) == 0 {
		return nil
	}
	filename := request.Input.Filename
	if filename == "" {
		filename = providerUtils.AudioFilenameFromBytes(request.Input.File)
	}
	fileWriter, err := writer.CreateFormFile("file", filename)
	if err != nil {
		return err
	}
	_, err = fileWriter.Write(request.Input.File)
	return err
}

// xaiFormValues renders one extra field as the form values to write under its name: a list
// gives one value per entry, anything else one value. Text is written as it is; a number or
// boolean (from a Go SDK caller) in its JSON form.
func xaiFormValues(value interface{}) []string {
	switch v := value.(type) {
	case nil:
		return nil
	case string:
		return []string{v}
	case []string:
		return v
	case []interface{}:
		values := make([]string, 0, len(v))
		for _, entry := range v {
			values = append(values, xaiFormValues(entry)...)
		}
		return values
	default:
		// Sorted keys, so an object-valued field is the same bytes on every request.
		encoded, err := providerUtils.MarshalSorted(v)
		if err != nil {
			return []string{fmt.Sprint(v)}
		}
		return []string{string(encoded)}
	}
}

// ToBifrostTranscriptionResponse converts an /v1/stt answer. usage carries the audio duration
// xAI reported and nothing else; it is absent when xAI reported none (contract C5).
func ToBifrostTranscriptionResponse(response *XAITranscriptionResponse) *schemas.BifrostTranscriptionResponse {
	bifrostResp := &schemas.BifrostTranscriptionResponse{Text: response.Text}
	if response.Language != "" {
		bifrostResp.Language = schemas.Ptr(response.Language)
	}
	if response.Duration != nil {
		bifrostResp.Duration = schemas.Ptr(*response.Duration)
		bifrostResp.Usage = &schemas.TranscriptionUsage{Type: "duration", Seconds: schemas.Ptr(*response.Duration)}
	}
	if len(response.Words) > 0 {
		bifrostResp.Words = make([]schemas.TranscriptionWord, 0, len(response.Words))
		for _, word := range response.Words {
			converted := schemas.TranscriptionWord{Word: word.Text, Start: word.Start, End: word.End}
			if word.Speaker != nil {
				converted.Speaker = schemas.Ptr(strconv.Itoa(*word.Speaker))
			}
			bifrostResp.Words = append(bifrostResp.Words, converted)
		}
	}
	return bifrostResp
}

// Transcription performs a speech to text request against xAI's /v1/stt.
func (provider *XAIProvider) Transcription(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostTranscriptionRequest) (*schemas.BifrostTranscriptionResponse, *schemas.BifrostError) {
	if bifrostErr := providerUtils.CheckOperationAllowed(schemas.XAI, provider.customProviderConfig, schemas.TranscriptionRequest); bifrostErr != nil {
		return nil, bifrostErr
	}
	if bifrostErr := refuseLargePayload(ctx, "transcription"); bifrostErr != nil {
		return nil, bifrostErr
	}
	if request == nil {
		return nil, providerUtils.NewBifrostBadRequestError("transcription request is not provided")
	}
	if request.Params != nil {
		if bifrostErr := rejectModelExtraParam(request.Params.ExtraParams); bifrostErr != nil {
			return nil, bifrostErr
		}
	}
	if _, hasURL := request.AudioURL(); !hasURL && (request.Input == nil || len(request.Input.File) == 0) {
		return nil, providerUtils.NewBifrostBadRequestError("a transcription file or url is required")
	}

	var form bytes.Buffer
	writer := multipart.NewWriter(&form)
	if err := WriteXAITranscriptionForm(writer, request); err != nil {
		return nil, providerUtils.NewBifrostOperationError("failed to build the multipart transcription request", err)
	}
	if err := writer.Close(); err != nil {
		return nil, providerUtils.NewBifrostOperationError("failed to finalize the multipart transcription request", err)
	}
	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)

	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)
	req.SetRequestURI(provider.networkConfig.BaseURL + providerUtils.GetPathFromContext(ctx, "/v1/stt"))
	req.Header.SetMethod(http.MethodPost)
	req.Header.SetContentType(writer.FormDataContentType())
	if value := key.Value.GetValue(); value != "" {
		req.Header.Set("Authorization", "Bearer "+value)
	}
	req.SetBody(form.Bytes())

	latency, bifrostErr, wait := providerUtils.MakeRequestWithContext(ctx, provider.client, req, resp)
	defer wait()
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	ctx.SetValue(schemas.BifrostContextKeyProviderResponseHeaders, providerUtils.ExtractProviderResponseHeaders(resp))
	if resp.StatusCode() < fasthttp.StatusOK || resp.StatusCode() >= fasthttp.StatusMultipleChoices {
		// The multipart body holds the caller's audio, so it is never sent back as raw_request.
		return nil, providerUtils.EnrichError(ctx, ParseXAIError(resp), nil, nil, false, sendBackRawResponse, latency)
	}
	body, err := providerUtils.CheckAndDecodeBody(resp)
	if err != nil {
		return nil, providerUtils.SetErrorLatency(providerUtils.NewBifrostOperationError(schemas.ErrProviderResponseDecode, err), latency)
	}

	var transcription XAITranscriptionResponse
	_, rawResponse, bifrostErr := providerUtils.HandleProviderResponseCtx(ctx, body, &transcription, nil, false, sendBackRawResponse)
	if bifrostErr != nil {
		return nil, providerUtils.SetErrorLatency(bifrostErr, latency)
	}
	bifrostResp := ToBifrostTranscriptionResponse(&transcription)
	bifrostResp.ExtraFields.Latency = latency.Milliseconds()
	if sendBackRawResponse {
		bifrostResp.ExtraFields.RawResponse = rawResponse
	}
	return bifrostResp, nil
}
