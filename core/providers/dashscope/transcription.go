package dashscope

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	schemas "github.com/capsohq/bifrost/core/schemas"
)

// AcceptsTranscriptionURLInput declares that Qwen3-ASR takes an audio URL in place of the
// file: the URL is written unchanged into the audio content item (contract C4, agreement four).
func (provider *DashScopeProvider) AcceptsTranscriptionURLInput() bool {
	return true
}

// transcriptionConvertedExtraParams are extra-param names Bifrost converts itself, so they
// never also reach the body's top level: the audio URL goes into the audio content item.
var transcriptionConvertedExtraParams = []string{schemas.TranscriptionURLParam}

// ToDashScopeASRRequest converts a unified transcription request into a Qwen3-ASR body.
//
// The audio is one {"audio": ...} content item of the user message. A URL (the "url" extra
// param, which may also be a data URL Tokun built) is written unchanged; Bifrost never
// downloads it. File bytes are sent as a base64 data URL, the form DashScope documents for
// Qwen3-ASR-Flash, up to 10 MB encoded; a larger file is refused with 413 before it is
// encoded. A request carrying both a file and a URL is refused naming url, since one of them
// would be dropped.
//
// prompt becomes the system message's context text; language becomes asr_options.language;
// response_format takes only "json", the shape Bifrost returns.
func ToDashScopeASRRequest(request *schemas.BifrostTranscriptionRequest) (*DashScopeASRRequest, error) {
	if request == nil {
		return nil, providerUtils.ConverterBifrostError(schemas.NewUnsupportedParameterError("file", "file or url is required for DashScope transcription"))
	}
	var params schemas.TranscriptionParameters
	if request.Params != nil {
		params = *request.Params
	}
	if params.ResponseFormat != nil && *params.ResponseFormat != "" && *params.ResponseFormat != "json" {
		return nil, providerUtils.ConverterBifrostError(schemas.NewUnsupportedParameterError("response_format", "DashScope transcription returns json only"))
	}

	hasFile := request.Input != nil && len(request.Input.File) > 0
	audioURL, hasURL := request.AudioURL()
	var audio string
	switch {
	case hasFile && hasURL:
		return nil, providerUtils.ConverterBifrostError(schemas.NewUnsupportedParameterError(schemas.TranscriptionURLParam, "send the audio as file or as url, not both"))
	case hasURL:
		audio = audioURL
	case hasFile:
		mediaType := audioMediaType(request.Input.Filename, request.Input.File)
		if n := audioDataURLLen(mediaType, len(request.Input.File)); n > maxAudioDataURLBytes {
			return nil, providerUtils.ConverterBifrostError(audioTooLargeError(n))
		}
		audio = audioDataURL(mediaType, request.Input.File)
	default:
		return nil, providerUtils.ConverterBifrostError(schemas.NewUnsupportedParameterError("file", "file or url is required for DashScope transcription"))
	}

	messages := make([]DashScopeASRMessage, 0, 2)
	if params.Prompt != nil && *params.Prompt != "" {
		messages = append(messages, DashScopeASRMessage{Role: "system", Content: []DashScopeASRContent{{Text: schemas.Ptr(*params.Prompt)}}})
	}
	messages = append(messages, DashScopeASRMessage{Role: "user", Content: []DashScopeASRContent{{Audio: audio}}})

	body := &DashScopeASRRequest{
		Model:       request.Model,
		Input:       DashScopeASRInput{Messages: messages},
		ExtraParams: withoutKeys(params.ExtraParams, transcriptionConvertedExtraParams...),
	}
	if params.Language != nil && *params.Language != "" {
		body.Parameters = &DashScopeASRParameters{ASROptions: &DashScopeASROptions{Language: *params.Language}}
	}
	return body, nil
}

// ToBifrostTranscriptionResponse converts a Qwen3-ASR answer: the first choice's text items
// joined as text, the audio_info language, and the vendor's usage. usage.seconds is the
// vendor's billed audio duration (type "duration"); the token details are kept as reported.
// Nothing Bifrost measured itself goes into usage (contract C5). An answer with no choice is
// the vendor's empty result.
func ToBifrostTranscriptionResponse(resp *DashScopeASRResponse) (*schemas.BifrostTranscriptionResponse, *schemas.BifrostError) {
	if len(resp.Output.Choices) == 0 {
		return nil, schemas.NewProviderEmptyResultError("DashScope transcription response carries no choices")
	}
	message := resp.Output.Choices[0].Message
	var text strings.Builder
	for _, item := range message.Content {
		if item.Text != nil {
			text.WriteString(*item.Text)
		}
	}
	out := &schemas.BifrostTranscriptionResponse{Text: text.String(), Task: schemas.Ptr("transcribe")}
	for _, annotation := range message.Annotations {
		if annotation.Language != "" {
			out.Language = schemas.Ptr(annotation.Language)
			break
		}
	}
	if u := resp.Usage; u != nil {
		usage := &schemas.TranscriptionUsage{
			InputTokens:  u.InputTokens,
			OutputTokens: u.OutputTokens,
			TotalTokens:  u.TotalTokens,
			Seconds:      u.Seconds,
		}
		if u.Seconds != nil {
			usage.Type = "duration"
		} else {
			usage.Type = "tokens"
		}
		if d := u.InputTokensDetails; d != nil && (d.TextTokens != nil || d.AudioTokens != nil) {
			details := &schemas.TranscriptionUsageInputTokenDetails{}
			if d.TextTokens != nil {
				details.TextTokens = *d.TextTokens
			}
			if d.AudioTokens != nil {
				details.AudioTokens = *d.AudioTokens
			}
			usage.InputTokenDetails = details
		}
		if usage.OutputTokens == nil && u.OutputTokensDetails != nil && u.OutputTokensDetails.TextTokens != nil {
			// The sync answer reports output tokens only as output_tokens_details.text_tokens;
			// Qwen3-ASR outputs text only, so that is the output token count.
			usage.OutputTokens = schemas.Ptr(*u.OutputTokensDetails.TextTokens)
		}
		out.Usage = usage
	}
	return out, nil
}

// Transcription transcribes audio with Qwen3-ASR: one synchronous call to the workspace base
// (network_config.workspace_base_url, the regional base when unset).
func (provider *DashScopeProvider) Transcription(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostTranscriptionRequest) (*schemas.BifrostTranscriptionResponse, *schemas.BifrostError) {
	jsonData, bifrostErr := providerUtils.CheckContextAndGetRequestBody(ctx, request,
		func() (providerUtils.RequestBodyWithExtraParams, error) {
			return ToDashScopeASRRequest(request)
		})
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	sendBackRawRequest := providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest)
	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)

	url := provider.baseURLFor(schemas.TranscriptionRequest) + multimodalGenerationPath
	body, latency, bifrostErr := provider.post(ctx, key, url, jsonData, sendBackRawRequest, sendBackRawResponse)
	if bifrostErr != nil {
		return nil, bifrostErr
	}

	var asrResp DashScopeASRResponse
	rawRequest, rawResponse, bifrostErr := providerUtils.HandleProviderResponseCtx(ctx, body, &asrResp, jsonData, sendBackRawRequest, sendBackRawResponse)
	if bifrostErr != nil {
		return nil, providerUtils.SetErrorLatency(bifrostErr, latency)
	}
	if bifrostErr := vendorErrorIn200(asrResp.Code, asrResp.Message); bifrostErr != nil {
		return nil, providerUtils.EnrichError(ctx, bifrostErr, jsonData, body, sendBackRawRequest, sendBackRawResponse, latency)
	}
	bifrostResp, bifrostErr := ToBifrostTranscriptionResponse(&asrResp)
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

// maxAudioDataURLBytes is the largest base64 data URL Qwen3-ASR takes, as DashScope documents
// it (10 MB encoded). A larger file is refused before it is encoded: encoding and marshalling
// it would cost several times its size in memory for a request the vendor rejects anyway.
const maxAudioDataURLBytes = 10 << 20

// audioDataURLLen is the length of the data URL audioDataURL builds for size bytes of audio.
func audioDataURLLen(mediaType string, size int) int {
	return len("data:") + len(mediaType) + len(";base64,") + base64.StdEncoding.EncodedLen(size)
}

// audioTooLargeError is the 413 for an upload whose data URL would exceed maxAudioDataURLBytes.
// A larger recording can be sent as url instead, which DashScope fetches itself.
func audioTooLargeError(dataURLLen int) *schemas.BifrostError {
	return &schemas.BifrostError{
		StatusCode: schemas.Ptr(http.StatusRequestEntityTooLarge),
		Error: &schemas.ErrorField{
			Type:    schemas.Ptr("invalid_request_error"),
			Code:    schemas.Ptr(string(schemas.ErrorTypeCallerInvalidRequest)),
			Message: fmt.Sprintf("the file is %d bytes as a base64 data URL; DashScope transcription takes at most %d. Send a larger recording as url", dataURLLen, maxAudioDataURLBytes),
			Param:   "file",
		},
		ExtraFields: schemas.BifrostErrorExtraFields{ErrorType: schemas.ErrorTypeCallerInvalidRequest},
	}
}

// audioDataURL returns data as a base64 data URL, encoded straight into one buffer sized for
// it, so the audio is copied once rather than once for the encoding and again for the prefix.
func audioDataURL(mediaType string, data []byte) string {
	var b strings.Builder
	b.Grow(audioDataURLLen(mediaType, len(data)))
	b.WriteString("data:")
	b.WriteString(mediaType)
	b.WriteString(";base64,")
	encoder := base64.NewEncoder(base64.StdEncoding, &b)
	_, _ = encoder.Write(data) // strings.Builder never returns an error
	_ = encoder.Close()
	return b.String()
}

// audioMediaType is the media type written into the data URL for uploaded bytes: from the
// file name's extension, else from the bytes' container signature, else
// application/octet-stream (the vendor decides whether it can read it).
func audioMediaType(filename string, data []byte) string {
	switch strings.ToLower(strings.TrimPrefix(filepath.Ext(filename), ".")) {
	case "wav":
		return "audio/wav"
	case "mp3", "mpeg", "mpga":
		return "audio/mpeg"
	case "m4a", "mp4":
		return "audio/mp4"
	case "aac":
		return "audio/aac"
	case "flac":
		return "audio/flac"
	case "ogg", "oga", "opus":
		return "audio/ogg"
	case "webm":
		return "audio/webm"
	case "amr":
		return "audio/amr"
	}
	switch {
	case len(data) >= 12 && bytes.Equal(data[:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WAVE")):
		return "audio/wav"
	case bytes.HasPrefix(data, []byte("ID3")), len(data) >= 2 && data[0] == 0xFF && data[1]&0xE0 == 0xE0:
		return "audio/mpeg"
	case bytes.HasPrefix(data, []byte("fLaC")):
		return "audio/flac"
	case bytes.HasPrefix(data, []byte("OggS")):
		return "audio/ogg"
	}
	return "application/octet-stream"
}
