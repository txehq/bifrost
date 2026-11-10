package handlers

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/capsohq/bifrost/framework/configstore"
	"github.com/capsohq/bifrost/transports/v2/bifrost-http/lib"
	"github.com/valyala/fasthttp"
)

// multipartTranscriptionCtx builds a /v1/audio/transcriptions request; fields are ordered pairs so a key can repeat.
func multipartTranscriptionCtx(t testing.TB, fields [][2]string) *fasthttp.RequestCtx {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for _, f := range fields {
		if err := writer.WriteField(f[0], f[1]); err != nil {
			t.Fatalf("write field %s: %v", f[0], err)
		}
	}
	part, err := writer.CreateFormFile("file", "clip.mp3")
	if err != nil {
		t.Fatalf("create file part: %v", err)
	}
	if _, err := part.Write([]byte("ID3fake-mp3")); err != nil {
		t.Fatalf("write file part: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod("POST")
	ctx.Request.SetRequestURI("/v1/audio/transcriptions")
	ctx.Request.Header.SetContentType(writer.FormDataContentType())
	ctx.Request.SetBody(body.Bytes())
	return ctx
}

// The native route must read repeated timestamp_granularities[] fields the way the /openai route does.
// Before, they fell into extra params as one string, so providers that map granularities (MiniMax's
// timestamp_level) never saw them and OpenAI received only the first value.
func TestPrepareTranscriptionRequest_TimestampGranularities(t *testing.T) {
	req, _, err := prepareTranscriptionRequest(multipartTranscriptionCtx(t, [][2]string{
		{"model", "minimax/asr-1.0"},
		{"response_format", "verbose_json"},
		{"timestamp_granularities[]", "word"},
		{"timestamp_granularities[]", "segment"},
	}), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := req.Params.TimestampGranularities; !slices.Equal(got, []string{"word", "segment"}) {
		t.Fatalf("timestamp granularities = %v, want [word segment]", got)
	}
	if _, leaked := req.Params.ExtraParams["timestamp_granularities[]"]; leaked {
		t.Fatalf("timestamp_granularities[] leaked into extra params: %+v", req.Params.ExtraParams)
	}
}

func TestPrepareTranscriptionRequest_NoTimestampGranularities(t *testing.T) {
	req, _, err := prepareTranscriptionRequest(multipartTranscriptionCtx(t, [][2]string{
		{"model", "minimax/asr-1.0"},
	}), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.Params.TimestampGranularities != nil {
		t.Fatalf("timestamp granularities = %v, want none", req.Params.TimestampGranularities)
	}
}

// BenchmarkPrepareTranscriptionRequest measures native-route form parsing. The
// timestamp_granularities[] read is one map lookup on the parsed form; the no_granularities
// case is the common request.
func BenchmarkPrepareTranscriptionRequest(b *testing.B) {
	for _, tc := range []struct {
		name   string
		fields [][2]string
	}{
		{name: "no_granularities", fields: [][2]string{{"model", "openai/whisper-1"}, {"response_format", "json"}}},
		{name: "granularities", fields: [][2]string{{"model", "openai/whisper-1"}, {"response_format", "verbose_json"}, {"timestamp_granularities[]", "word"}, {"timestamp_granularities[]", "segment"}}},
		// An unknown field with one value is the common extra-param case; it must not pay for the
		// repeated-field path (TXE-6530).
		{name: "single_extra_field", fields: [][2]string{{"model", "openai/gpt-4o-transcribe"}, {"include[]", "logprobs"}}},
		{name: "repeated_extra_field", fields: [][2]string{{"model", "openai/gpt-4o-transcribe"}, {"include[]", "logprobs"}, {"include[]", "segments"}}},
	} {
		b.Run(tc.name, func(b *testing.B) {
			template := multipartTranscriptionCtx(b, tc.fields)
			body := append([]byte(nil), template.Request.Body()...)
			contentType := string(template.Request.Header.ContentType())
			b.ReportAllocs()
			for b.Loop() {
				ctx := &fasthttp.RequestCtx{}
				ctx.Request.Header.SetMethod("POST")
				ctx.Request.SetRequestURI("/v1/audio/transcriptions")
				ctx.Request.Header.SetContentType(contentType)
				ctx.Request.SetBody(body)
				if _, _, err := prepareTranscriptionRequest(ctx, nil); err != nil {
					b.Fatalf("prepareTranscriptionRequest: %v", err)
				}
			}
		})
	}
}

func jsonRequestCtx(uri, body string) *fasthttp.RequestCtx {
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod("POST")
	ctx.Request.SetRequestURI(uri)
	ctx.Request.Header.SetContentType("application/json")
	ctx.Request.SetBody([]byte(body))
	return ctx
}

// TXE-6518 (contract C4 BF3): the four unified video fields reach the typed parameters, and also
// stay in ExtraParams under their own names, which is where byteplus, minimax and every provider
// that forwards its extra params read them before they were unified.
func TestPrepareVideoGenerationRequest_UnifiedFields(t *testing.T) {
	req, err := prepareVideoGenerationRequest(jsonRequestCtx("/v1/videos",
		`{"model":"byteplus/seedance-2-0-fast","prompt":"a fox","input_reference":"https://example.com/first.png",`+
			`"aspect_ratio":"16:9","resolution":"720p","last_frame":"https://example.com/last.png",`+
			`"reference_images":["https://example.com/a.png","data:image/png;base64,AAAA"],"watermark":false}`), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	p := req.Params
	if p.AspectRatio != "16:9" {
		t.Fatalf("aspect_ratio = %q, want 16:9", p.AspectRatio)
	}
	if p.Resolution != "720p" {
		t.Fatalf("resolution = %q, want 720p", p.Resolution)
	}
	if p.LastFrame != "https://example.com/last.png" {
		t.Fatalf("last_frame = %q", p.LastFrame)
	}
	if !slices.Equal([]string(p.ReferenceImages), []string{"https://example.com/a.png", "data:image/png;base64,AAAA"}) {
		t.Fatalf("reference_images = %v", p.ReferenceImages)
	}
	for _, name := range schemas.UnifiedVideoGenerationFields {
		if _, ok := p.ExtraParams[name]; !ok {
			t.Errorf("%s missing from ExtraParams %v: providers that read it there would lose it", name, p.ExtraParams)
		}
	}
	if p.ExtraParams["watermark"] != false {
		t.Errorf("watermark = %v, the vendor switch must stay an extra param", p.ExtraParams["watermark"])
	}
	if req.Input == nil || req.Input.InputReference == nil || *req.Input.InputReference != "https://example.com/first.png" {
		t.Fatalf("input_reference = %+v", req.Input)
	}
}

// Runway callers send reference_images as objects. The typed list ignores that shape instead of
// failing the whole request, and the objects still reach Runway through ExtraParams.
func TestPrepareVideoGenerationRequest_ObjectReferenceImagesStayExtra(t *testing.T) {
	req, err := prepareVideoGenerationRequest(jsonRequestCtx("/v1/videos",
		`{"model":"runway/gen4_turbo","prompt":"a fox","reference_images":[{"uri":"https://example.com/a.png","tag":"fox"}]}`), nil)
	if err != nil {
		t.Fatalf("object reference_images must not fail the request: %v", err)
	}
	if req.Params.ReferenceImages != nil {
		t.Fatalf("typed reference_images = %v, want nil for the object form", req.Params.ReferenceImages)
	}
	refs, ok := req.Params.ExtraParams["reference_images"].([]any)
	if !ok || len(refs) != 1 {
		t.Fatalf("ExtraParams reference_images = %#v, want the raw objects", req.Params.ExtraParams["reference_images"])
	}
}

// Runware 3D tasks take a numeric resolution, sent before the field was unified. It must not fail
// the request: the typed field stays empty and the number reaches Runware as an extra param.
func TestPrepareVideoGenerationRequest_NumericResolutionStaysExtra(t *testing.T) {
	req, err := prepareVideoGenerationRequest(jsonRequestCtx("/v1/videos",
		`{"model":"runware/tencent:hunyuan3d@2.1","input_reference":"https://example.com/chair.png","type":"3d","resolution":1024}`), nil)
	if err != nil {
		t.Fatalf("numeric resolution must not fail the request: %v", err)
	}
	if req.Params.Resolution != "" {
		t.Fatalf("typed resolution = %q, want empty for a number", req.Params.Resolution)
	}
	if v, ok := req.Params.ExtraParams["resolution"].(float64); !ok || v != 1024 {
		t.Fatalf("ExtraParams resolution = %#v, want the number 1024", req.Params.ExtraParams["resolution"])
	}
}

func TestPrepareVideoGenerationRequest_Validation(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"bad json", `{`, "Invalid request payload"},
		{"no prompt or input", `{"model":"openai/sora-2","resolution":"720p"}`, "prompt, input_reference or video_uri is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := prepareVideoGenerationRequest(jsonRequestCtx("/v1/videos", tc.body), nil)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

// TXE-6518 (contract C4 BF3): the unified image resolution reaches the typed parameters on
// /v1/images/generations and stays an extra param under its own name.
func TestPrepareImageGenerationRequest_Resolution(t *testing.T) {
	_, req, err := prepareImageGenerationRequest(jsonRequestCtx("/v1/images/generations",
		`{"model":"gemini/gemini-3-pro-image-preview","prompt":"a teapot","aspect_ratio":"16:9","resolution":"2K"}`), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.Params.Resolution != "2K" {
		t.Fatalf("resolution = %q, want 2K", req.Params.Resolution)
	}
	if req.Params.ExtraParams["resolution"] != "2K" {
		t.Fatalf("ExtraParams resolution = %v, want 2K", req.Params.ExtraParams["resolution"])
	}
	if _, ok := req.Params.ExtraParams["aspect_ratio"]; ok {
		t.Fatalf("aspect_ratio was already unified and must not become an extra param")
	}
}

// The unified fields are kept out of the known-field tables on purpose (see the comment above
// imageGenerationParamsKnownFields). Adding one there would silently stop it reaching every
// provider that still reads it as an extra param.
func TestUnifiedFieldsAreNotKnownFields(t *testing.T) {
	for _, name := range schemas.UnifiedVideoGenerationFields {
		if videoGenerationParamsKnownFields[name] {
			t.Errorf("%s is in videoGenerationParamsKnownFields", name)
		}
	}
	for _, name := range schemas.UnifiedImageFields {
		if imageGenerationParamsKnownFields[name] {
			t.Errorf("%s is in imageGenerationParamsKnownFields", name)
		}
		if imageEditParamsKnownFields[name] {
			t.Errorf("%s is in imageEditParamsKnownFields", name)
		}
	}
}

// A vendor 200 with no audio is 502 provider_empty_result, not the caller's 400, and keeps
// the vendor's response headers.
func TestWriteSpeechAudioMissingAudioIsProviderEmptyResult(t *testing.T) {
	ctx := &fasthttp.RequestCtx{}
	resp := &schemas.BifrostSpeechResponse{ExtraFields: schemas.BifrostResponseExtraFields{
		RequestType:             schemas.SpeechRequest,
		Provider:                schemas.Elevenlabs,
		ProviderResponseHeaders: map[string]string{"request-id": "el-123"},
	}}
	// What the speech handler does before writing the body.
	lib.ApplyBifrostResponseHeaders(ctx, nil, resp.ExtraFields)
	writeSpeechAudio(ctx, resp, "mp3")

	if got := ctx.Response.StatusCode(); got != fasthttp.StatusBadGateway {
		t.Fatalf("status = %d, want 502", got)
	}
	if got := sentErrorType(t, ctx); got != string(schemas.ErrorTypeProviderEmptyResult) {
		t.Fatalf("error_type = %q", got)
	}
	if got := string(ctx.Response.Header.Peek("request-id")); got != "el-123" {
		t.Fatalf("vendor header request-id = %q, want el-123", got)
	}
	if got := string(ctx.Response.Header.Peek(lib.HeaderBifrostProvider)); got != "elevenlabs" {
		t.Fatalf("provider header = %q", got)
	}

	// An empty body is no audio too: a vendor 200 whose body is empty.
	empty := &fasthttp.RequestCtx{}
	writeSpeechAudio(empty, &schemas.BifrostSpeechResponse{Audio: []byte{}}, "mp3")
	if got := empty.Response.StatusCode(); got != fasthttp.StatusBadGateway {
		t.Fatalf("empty audio: status = %d, want 502", got)
	}

	ok := &fasthttp.RequestCtx{}
	writeSpeechAudio(ok, &schemas.BifrostSpeechResponse{Audio: []byte("ID3")}, "mp3")
	if ok.Response.StatusCode() != fasthttp.StatusOK || string(ok.Response.Body()) != "ID3" {
		t.Fatalf("audio response: status %d body %q", ok.Response.StatusCode(), ok.Response.Body())
	}
}

// A repeated unknown form field (include[]=a&include[]=b) used to keep only its first value.
func TestPrepareTranscriptionRequest_RepeatedExtraFieldKeepsEveryValue(t *testing.T) {
	req, _, err := prepareTranscriptionRequest(multipartTranscriptionCtx(t, [][2]string{
		{"model", "openai/gpt-4o-transcribe"},
		{"include[]", "logprobs"},
		{"include[]", "segments"},
		{"chunking_strategy", "auto"},
	}), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, _ := req.Params.ExtraParams["include[]"].([]string); !slices.Equal(got, []string{"logprobs", "segments"}) {
		t.Fatalf("include[] = %#v, want [logprobs segments]", req.Params.ExtraParams["include[]"])
	}
	if got := req.Params.ExtraParams["chunking_strategy"]; got != "auto" {
		t.Fatalf("chunking_strategy = %#v, want the single value as a string", got)
	}
}

// audioUpstreamAccount points OpenAI, MiniMax and Gemini at local test servers.
type audioUpstreamAccount struct {
	baseURLs map[schemas.ModelProvider]string
}

func (a audioUpstreamAccount) GetConfiguredProviders() ([]schemas.ModelProvider, error) {
	out := make([]schemas.ModelProvider, 0, len(a.baseURLs))
	for p := range a.baseURLs {
		out = append(out, p)
	}
	return out, nil
}

func (a audioUpstreamAccount) GetKeysForProvider(_ context.Context, p schemas.ModelProvider) ([]schemas.Key, error) {
	if _, ok := a.baseURLs[p]; !ok {
		return nil, nil
	}
	return []schemas.Key{{ID: "k-" + string(p), Value: *schemas.NewSecretVar("sk-test"), Models: schemas.WhiteList{"*"}, Weight: 1}}, nil
}

func (a audioUpstreamAccount) GetConfigForProvider(p schemas.ModelProvider) (*schemas.ProviderConfig, error) {
	baseURL, ok := a.baseURLs[p]
	if !ok {
		return nil, fmt.Errorf("unsupported provider %s", p)
	}
	nc := schemas.DefaultNetworkConfig
	nc.BaseURL = baseURL
	return &schemas.ProviderConfig{NetworkConfig: nc, ConcurrencyAndBufferSize: schemas.DefaultConcurrencyAndBufferSize}, nil
}

// audioUpstream answers with a fixed response and records the last request it saw.
type audioUpstream struct {
	contentType string
	body        []byte

	mu          sync.Mutex
	gotType     string
	gotBody     []byte
	gotRequests int
}

func (u *audioUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	u.mu.Lock()
	u.gotType, u.gotBody = r.Header.Get("Content-Type"), body
	u.gotRequests++
	u.mu.Unlock()
	w.Header().Set("Content-Type", u.contentType)
	_, _ = w.Write(u.body)
}

func newAudioTestHandler(t *testing.T, upstreams map[schemas.ModelProvider]*audioUpstream) *CompletionHandler {
	t.Helper()
	baseURLs := make(map[schemas.ModelProvider]string, len(upstreams))
	for p, u := range upstreams {
		srv := httptest.NewServer(u)
		t.Cleanup(srv.Close)
		baseURLs[p] = srv.URL
	}
	client, err := bifrost.Init(context.Background(), schemas.BifrostConfig{
		Account: audioUpstreamAccount{baseURLs: baseURLs},
		Logger:  bifrost.NewDefaultLogger(schemas.LogLevelError),
	})
	if err != nil {
		t.Fatalf("bifrost.Init: %v", err)
	}
	t.Cleanup(client.Shutdown)
	return &CompletionHandler{client: client, config: &lib.Config{ClientConfig: &configstore.ClientConfig{}}}
}

func speechCtx(body string) *fasthttp.RequestCtx {
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod(fasthttp.MethodPost)
	ctx.Request.SetRequestURI("/v1/audio/speech")
	ctx.Request.Header.SetContentType("application/json")
	ctx.Request.SetBodyString(body)
	return ctx
}

func speechUsageHeader(t *testing.T, ctx *fasthttp.RequestCtx) map[string]any {
	t.Helper()
	raw := ctx.Response.Header.Peek(lib.HeaderBifrostUsage)
	if raw == nil {
		return nil
	}
	var usage map[string]any
	if err := json.Unmarshal(raw, &usage); err != nil {
		t.Fatalf("x-bifrost-usage is not JSON: %q: %v", raw, err)
	}
	return usage
}

// The non-streaming /v1/audio/speech response used to be the audio with a fixed
// Content-Type: audio/mpeg and no usage anywhere. It now keeps the audio as the body, labels
// it with its real format and carries the vendor's usage in x-bifrost-usage (contract C5).
func TestSpeechNonStreamingCarriesUsageHeaderAndContentType(t *testing.T) {
	wav := []byte("RIFF\x24\x00\x00\x00WAVEfmt \x10\x00\x00\x00")
	pcm := []byte{0x01, 0x00, 0x02, 0x00, 0x03, 0x00}
	miniMax := &audioUpstream{contentType: "application/json", body: []byte(`{"data":{"audio":"` + hex.EncodeToString(wav) +
		`","status":2},"extra_info":{"audio_length":1200,"usage_characters":13},"trace_id":"t1","base_resp":{"status_code":0,"status_msg":"success"}}`)}
	gemini := &audioUpstream{contentType: "application/json", body: []byte(`{"candidates":[{"content":{"role":"model","parts":[{"inlineData":{"mimeType":"audio/L16;codec=pcm;rate=24000","data":"` +
		base64.StdEncoding.EncodeToString(pcm) + `"}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":42,"totalTokenCount":49}}`)}
	openAI := &audioUpstream{contentType: "audio/flac", body: []byte("fLaC\x00\x00\x00\x22")}
	h := newAudioTestHandler(t, map[schemas.ModelProvider]*audioUpstream{
		schemas.MiniMax: miniMax, schemas.Gemini: gemini, schemas.OpenAI: openAI,
	})

	t.Run("minimax", func(t *testing.T) {
		ctx := speechCtx(`{"model":"minimax/speech-2.6-hd","input":"你好, world","voice":"English_Graceful_Lady","response_format":"wav"}`)
		h.speech(ctx)
		if ctx.Response.StatusCode() != fasthttp.StatusOK {
			t.Fatalf("status %d: %s", ctx.Response.StatusCode(), ctx.Response.Body())
		}
		if !bytes.Equal(ctx.Response.Body(), wav) {
			t.Fatalf("body = %q, want the decoded audio", ctx.Response.Body())
		}
		if got := string(ctx.Response.Header.ContentType()); got != "audio/wav" {
			t.Fatalf("Content-Type = %q, want audio/wav", got)
		}
		usage := speechUsageHeader(t, ctx)
		// MiniMax's usage_characters (13) used to be overwritten by the code-point count (9).
		if usage == nil || usage["input_chars"] != float64(13) || usage["billed_characters"] != float64(13) {
			t.Fatalf("x-bifrost-usage = %v, want MiniMax's usage_characters 13 in input_chars and billed_characters", usage)
		}
	})

	t.Run("gemini", func(t *testing.T) {
		// Gemini accepts only wav or no response_format; with none it answers raw PCM, which
		// used to go out labeled audio/mpeg.
		ctx := speechCtx(`{"model":"gemini/gemini-2.5-flash-preview-tts","input":"Hello","voice":"Kore"}`)
		h.speech(ctx)
		if ctx.Response.StatusCode() != fasthttp.StatusOK {
			t.Fatalf("status %d: %s", ctx.Response.StatusCode(), ctx.Response.Body())
		}
		if !bytes.Equal(ctx.Response.Body(), pcm) {
			t.Fatalf("body = %q, want the PCM bytes", ctx.Response.Body())
		}
		if got := string(ctx.Response.Header.ContentType()); got != "audio/L16;codec=pcm;rate=24000" {
			t.Fatalf("Content-Type = %q, want Gemini's audio/L16 type", got)
		}
		usage := speechUsageHeader(t, ctx)
		if usage == nil || usage["input_tokens"] != float64(7) || usage["output_tokens"] != float64(42) {
			t.Fatalf("x-bifrost-usage = %v, want Gemini's usageMetadata tokens", usage)
		}
	})

	t.Run("gemini wav", func(t *testing.T) {
		ctx := speechCtx(`{"model":"gemini/gemini-2.5-flash-preview-tts","input":"Hello","voice":"Kore","response_format":"wav"}`)
		h.speech(ctx)
		if ctx.Response.StatusCode() != fasthttp.StatusOK {
			t.Fatalf("status %d: %s", ctx.Response.StatusCode(), ctx.Response.Body())
		}
		if got := string(ctx.Response.Header.ContentType()); got != "audio/wav" {
			t.Fatalf("Content-Type = %q, want audio/wav", got)
		}
		if !bytes.HasPrefix(ctx.Response.Body(), []byte("RIFF")) {
			t.Fatalf("body = %q, want a WAV file", ctx.Response.Body())
		}
	})

	t.Run("openai", func(t *testing.T) {
		ctx := speechCtx(`{"model":"openai/gpt-4o-mini-tts","input":"Hello","voice":"alloy","response_format":"flac","language_code":"en","with_timestamps":true}`)
		h.speech(ctx)
		if ctx.Response.StatusCode() != fasthttp.StatusOK {
			t.Fatalf("status %d: %s", ctx.Response.StatusCode(), ctx.Response.Body())
		}
		if got := string(ctx.Response.Header.ContentType()); got != "audio/flac" {
			t.Fatalf("Content-Type = %q, want OpenAI's audio/flac", got)
		}
		// OpenAI reports no usage for non-streaming speech; no token count may be invented.
		usage := speechUsageHeader(t, ctx)
		for _, key := range []string{"input_tokens", "output_tokens", "total_tokens", "billed_characters"} {
			if v, ok := usage[key]; ok && v != float64(0) {
				t.Fatalf("x-bifrost-usage = %v, carries invented %s", usage, key)
			}
		}
		openAI.mu.Lock()
		sent := string(openAI.gotBody)
		openAI.mu.Unlock()
		for _, key := range []string{"language_code", "with_timestamps"} {
			if strings.Contains(sent, `"`+key+`"`) {
				t.Fatalf("OpenAI received ElevenLabs field %s: %s", key, sent)
			}
		}
	})
}

// A repeated transcription form field must reach the vendor with every value (contract C4).
func TestTranscriptionForwardsRepeatedFormFieldToVendor(t *testing.T) {
	openAI := &audioUpstream{contentType: "application/json", body: []byte(`{"text":"hi","usage":{"type":"duration","seconds":1}}`)}
	h := newAudioTestHandler(t, map[schemas.ModelProvider]*audioUpstream{schemas.OpenAI: openAI})

	ctx := multipartTranscriptionCtx(t, [][2]string{
		{"model", "openai/gpt-4o-transcribe"},
		{"include[]", "logprobs"},
		{"include[]", "segments"},
	})
	h.transcription(ctx)
	if ctx.Response.StatusCode() != fasthttp.StatusOK {
		t.Fatalf("status %d: %s", ctx.Response.StatusCode(), ctx.Response.Body())
	}

	openAI.mu.Lock()
	contentType, body := openAI.gotType, openAI.gotBody
	openAI.mu.Unlock()
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		t.Fatalf("vendor Content-Type %q: %v", contentType, err)
	}
	form, err := multipart.NewReader(bytes.NewReader(body), params["boundary"]).ReadForm(1 << 20)
	if err != nil {
		t.Fatalf("read vendor form: %v", err)
	}
	if got := form.Value["include[]"]; !slices.Equal(got, []string{"logprobs", "segments"}) {
		t.Fatalf("vendor include[] = %q, want [logprobs segments]", got)
	}
}

// The speech handler writes x-bifrost-usage before the body; when the vendor's 200 carried no
// audio, the 502 provider_empty_result must not keep the success usage header.
func TestWriteSpeechAudioEmptyAudioDropsUsageHeader(t *testing.T) {
	ctx := &fasthttp.RequestCtx{}
	lib.SetSpeechUsageHeader(ctx, &schemas.SpeechUsage{InputTokens: 3})
	writeSpeechAudio(ctx, &schemas.BifrostSpeechResponse{Usage: &schemas.SpeechUsage{InputTokens: 3}}, "wav")
	if got := ctx.Response.StatusCode(); got != fasthttp.StatusBadGateway {
		t.Fatalf("status = %d, want 502", got)
	}
	if got := ctx.Response.Header.Peek(lib.HeaderBifrostUsage); got != nil {
		t.Fatalf("x-bifrost-usage = %q on a 502", got)
	}

	ok := &fasthttp.RequestCtx{}
	writeSpeechAudio(ok, &schemas.BifrostSpeechResponse{Audio: []byte("RIFF\x24\x00\x00\x00WAVEfmt ")}, "mp3")
	if got := string(ok.Response.Header.ContentType()); got != "audio/wav" {
		t.Fatalf("Content-Type = %q, want audio/wav from the bytes", got)
	}
}
