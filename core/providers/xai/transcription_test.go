package xai_test

import (
	"bytes"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"

	"github.com/capsohq/bifrost/core/providers/xai"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// xaiFormPart is one part of a captured multipart form, in the order it was written.
type xaiFormPart struct {
	name, filename, value string
}

// xaiFormParts reads a captured multipart request in wire order.
func xaiFormParts(t *testing.T, request xaiCapturedRequest) []xaiFormPart {
	t.Helper()
	mediaType, params, err := mime.ParseMediaType(request.contentType)
	require.NoError(t, err)
	require.Equal(t, "multipart/form-data", mediaType)
	reader := multipart.NewReader(bytes.NewReader(request.body), params["boundary"])
	var parts []xaiFormPart
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			return parts
		}
		require.NoError(t, err)
		value, err := io.ReadAll(part)
		require.NoError(t, err)
		parts = append(parts, xaiFormPart{name: part.FormName(), filename: part.FileName(), value: string(value)})
	}
}

const xaiTranscript = `{"text":"The balance is $167,983.15.","language":"en","duration":8.4,"words":[{"text":"The","start":0,"end":0.24,"confidence":0.33,"speaker":0},{"text":"balance","start":0.24,"end":0.64,"confidence":0.67,"speaker":1}],"channels":[{"index":0,"language":"en","text":"The balance is $167,983.15."}]}`

// A transcription writes file as the last form field with xAI's extra fields unchanged before
// it (a repeated field once per value), and returns the duration xAI reported as the unified
// usage, its words, and the raw body (TXE-6429, contract C4/C5).
func TestXAITranscriptionFormAndResponse(t *testing.T) {
	t.Parallel()
	provider, captured := newMockXAI(t, map[string]http.HandlerFunc{
		"POST /v1/stt": xaiJSON(http.StatusOK, xaiTranscript),
	})
	ctx := xaiTestContext(t)
	ctx.SetValue(schemas.BifrostContextKeyCaptureRawResponse, true)

	resp, bifrostErr := provider.Transcription(ctx, xaiTestKey(), &schemas.BifrostTranscriptionRequest{
		Provider: schemas.XAI,
		Model:    "grok-voice-transcribe-2.0",
		Input:    &schemas.TranscriptionInput{File: []byte("RIFFaudio"), Filename: "call.wav"},
		Params: &schemas.TranscriptionParameters{
			Language: schemas.Ptr("en"),
			Prompt:   schemas.Ptr("exempt from the field check and not an xAI field"),
			ExtraParams: map[string]interface{}{
				"keyterm":       []string{"Tokun", "Bifrost"},
				"diarize":       "true",
				"multichannel":  "true",
				"vad_threshold": 0.4,
				"format":        true,
			},
		},
	})
	require.Nil(t, bifrostErr)

	requests := captured()
	require.Len(t, requests, 1)
	require.Equal(t, "Bearer xai-test", requests[0].auth)
	parts := xaiFormParts(t, requests[0])
	require.Equal(t, []xaiFormPart{
		{name: "model", value: "grok-voice-transcribe-2.0"},
		{name: "language", value: "en"},
		{name: "diarize", value: "true"},
		{name: "format", value: "true"},
		{name: "keyterm", value: "Tokun"},
		{name: "keyterm", value: "Bifrost"},
		{name: "multichannel", value: "true"},
		{name: "vad_threshold", value: "0.4"},
		{name: "file", filename: "call.wav", value: "RIFFaudio"},
	}, parts)

	require.Equal(t, "The balance is $167,983.15.", resp.Text)
	require.Equal(t, schemas.Ptr("en"), resp.Language)
	require.Equal(t, schemas.Ptr(8.4), resp.Duration)
	require.Equal(t, []schemas.TranscriptionWord{
		{Word: "The", Start: 0, End: 0.24, Speaker: schemas.Ptr("0")},
		{Word: "balance", Start: 0.24, End: 0.64, Speaker: schemas.Ptr("1")},
	}, resp.Words)

	encoded, err := schemas.Marshal(resp)
	require.NoError(t, err)
	wire := gjson.ParseBytes(encoded)
	assert.Equal(t, "duration", wire.Get("usage.type").String())
	assert.Equal(t, 8.4, wire.Get("usage.seconds").Float())
	assert.False(t, wire.Get("usage.input_tokens").Exists(), "xAI reports no tokens, so none are invented")
	assert.Equal(t, int64(0), wire.Get("extra_fields.raw_response.channels.0.index").Int())
	assert.Equal(t, 0.33, wire.Get("extra_fields.raw_response.words.0.confidence").Float())
}

// A URL input goes to xAI unchanged as the url form field, with no file part: Bifrost does
// not download it.
func TestXAITranscriptionURLInput(t *testing.T) {
	t.Parallel()
	provider, captured := newMockXAI(t, map[string]http.HandlerFunc{
		"POST /v1/stt": xaiJSON(http.StatusOK, `{"text":"hello","duration":1.25}`),
	})
	require.True(t, schemas.AcceptsTranscriptionURLInput(provider))

	request := &schemas.BifrostTranscriptionRequest{
		Provider: schemas.XAI,
		Model:    "grok-voice-transcribe-2.0",
		Input:    &schemas.TranscriptionInput{},
		Params:   &schemas.TranscriptionParameters{ExtraParams: map[string]interface{}{"url": "https://example.com/audio.mp3?sig=abc"}},
	}
	require.True(t, request.IsURLOnly())
	resp, bifrostErr := provider.Transcription(xaiTestContext(t), xaiTestKey(), request)
	require.Nil(t, bifrostErr)
	require.Equal(t, "hello", resp.Text)
	require.Equal(t, &schemas.TranscriptionUsage{Type: "duration", Seconds: schemas.Ptr(1.25)}, resp.Usage)
	require.Empty(t, resp.Words)

	requests := captured()
	require.Len(t, requests, 1)
	require.Equal(t, []xaiFormPart{
		{name: "model", value: "grok-voice-transcribe-2.0"},
		{name: "url", value: "https://example.com/audio.mp3?sig=abc"},
	}, xaiFormParts(t, requests[0]))
}

// A unified field /v1/stt cannot take is refused before any upstream call, a request with
// neither a file nor a url is a 400, and an xAI error keeps its status and message.
func TestXAITranscriptionRefusalsAndErrors(t *testing.T) {
	t.Parallel()
	provider, captured := newMockXAI(t, nil)
	declared := func(params *schemas.TranscriptionParameters) *schemas.BifrostError {
		return schemas.CheckDeclaredUnifiedFields(provider, &schemas.BifrostRequest{
			RequestType: schemas.TranscriptionRequest,
			TranscriptionRequest: &schemas.BifrostTranscriptionRequest{
				Provider: schemas.XAI, Model: "grok-voice-transcribe-2.0", Input: &schemas.TranscriptionInput{File: []byte("a")}, Params: params,
			},
		})
	}
	requireUnsupportedParameter(t, declared(&schemas.TranscriptionParameters{ResponseFormat: schemas.Ptr("verbose_json")}), "response_format")
	requireUnsupportedParameter(t, declared(&schemas.TranscriptionParameters{TimestampGranularities: []string{"word"}}), "timestamp_granularities")
	require.Nil(t, declared(&schemas.TranscriptionParameters{Language: schemas.Ptr("en"), Prompt: schemas.Ptr("names")}))

	_, bifrostErr := provider.Transcription(xaiTestContext(t), xaiTestKey(), &schemas.BifrostTranscriptionRequest{
		Provider: schemas.XAI, Model: "grok-voice-transcribe-2.0", Input: &schemas.TranscriptionInput{},
	})
	require.NotNil(t, bifrostErr)
	require.Equal(t, schemas.Ptr(http.StatusBadRequest), bifrostErr.StatusCode)
	require.Empty(t, captured(), "a refused request must not call xAI")

	failing, _ := newMockXAI(t, map[string]http.HandlerFunc{
		"POST /v1/stt": xaiJSON(http.StatusBadRequest, `{"code":"invalid_argument","error":"format requires language"}`),
	})
	_, bifrostErr = failing.Transcription(xaiTestContext(t), xaiTestKey(), &schemas.BifrostTranscriptionRequest{
		Provider: schemas.XAI, Model: "grok-voice-transcribe-2.0", Input: &schemas.TranscriptionInput{File: []byte("a"), Filename: "a.wav"},
	})
	require.NotNil(t, bifrostErr)
	require.Equal(t, schemas.Ptr(http.StatusBadRequest), bifrostErr.StatusCode)
	require.Equal(t, "format requires language", bifrostErr.Error.Message)
}

// An object-valued extra field (a Go SDK caller) is written with sorted keys, so the same
// request is the same form bytes every time.
func TestXAITranscriptionFormObjectValueIsStable(t *testing.T) {
	t.Parallel()
	request := &schemas.BifrostTranscriptionRequest{
		Model: "grok-voice-transcribe-2.0",
		Input: &schemas.TranscriptionInput{File: []byte("a"), Filename: "a.wav"},
		Params: &schemas.TranscriptionParameters{ExtraParams: map[string]interface{}{
			"options": map[string]interface{}{"zeta": 1, "alpha": true, "mid": "x", "beta": []interface{}{"b", "a"}},
		}},
	}
	for range 20 {
		var form bytes.Buffer
		writer := multipart.NewWriter(&form)
		require.NoError(t, writer.SetBoundary("xai-test-boundary"))
		require.NoError(t, xai.WriteXAITranscriptionForm(writer, request))
		require.NoError(t, writer.Close())
		require.Contains(t, form.String(), "name=\"options\"\r\n\r\n{\"alpha\":true,\"beta\":[\"b\",\"a\"],\"mid\":\"x\",\"zeta\":1}\r\n")
	}
}

// BenchmarkWriteXAITranscriptionForm measures building the multipart body of a transcription
// for 1 MiB of audio: the form is the caller's audio copied once plus its fields.
func BenchmarkWriteXAITranscriptionForm(b *testing.B) {
	request := &schemas.BifrostTranscriptionRequest{
		Model: "grok-voice-transcribe-2.0",
		Input: &schemas.TranscriptionInput{File: []byte(strings.Repeat("a", 1<<20)), Filename: "call.wav"},
		Params: &schemas.TranscriptionParameters{
			Language:    schemas.Ptr("en"),
			ExtraParams: map[string]interface{}{"keyterm": []string{"Tokun", "Bifrost"}, "diarize": "true", "multichannel": "true"},
		},
	}
	b.ReportAllocs()
	b.SetBytes(1 << 20)
	for b.Loop() {
		var form bytes.Buffer
		writer := multipart.NewWriter(&form)
		if err := xai.WriteXAITranscriptionForm(writer, request); err != nil {
			b.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkToBifrostTranscriptionResponse measures the answer conversion for 200 words.
func BenchmarkToBifrostTranscriptionResponse(b *testing.B) {
	response := &xai.XAITranscriptionResponse{Text: strings.Repeat("word ", 200), Language: "en", Duration: schemas.Ptr(84.0)}
	for i := range 200 {
		response.Words = append(response.Words, xai.XAITranscriptionWord{Text: "word", Start: float64(i), End: float64(i) + 0.4, Speaker: schemas.Ptr(i % 2)})
	}
	b.ReportAllocs()
	for b.Loop() {
		_ = xai.ToBifrostTranscriptionResponse(response)
	}
}
