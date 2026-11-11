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
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/providers/gemini"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/capsohq/bifrost/framework/configstore"
	"github.com/capsohq/bifrost/transports/v2/bifrost-http/lib"
	"github.com/stretchr/testify/require"
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

// file_format is a unified field (TranscriptionParameters.Format). The native route listed it
// as known, which kept it out of extra params, and then never set it: a provider that needs it
// to name headerless audio (AssemblyAI's raw PCM, TXE-6435) never saw it.
func TestPrepareTranscriptionRequest_FileFormat(t *testing.T) {
	req, _, err := prepareTranscriptionRequest(multipartTranscriptionCtx(t, [][2]string{
		{"model", "assemblyai/universal-3-5-pro"},
		{"file_format", "pcm"},
	}), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.Params.Format == nil || *req.Params.Format != "pcm" {
		t.Fatalf("file_format = %v, want pcm", req.Params.Format)
	}
	if _, leaked := req.Params.ExtraParams["file_format"]; leaked {
		t.Fatalf("file_format leaked into extra params: %+v", req.Params.ExtraParams)
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

// multipartTranscriptionCtxWithoutFile builds a /v1/audio/transcriptions request with no file part.
func multipartTranscriptionCtxWithoutFile(t testing.TB, fields [][2]string) *fasthttp.RequestCtx {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for _, f := range fields {
		if err := writer.WriteField(f[0], f[1]); err != nil {
			t.Fatalf("write field %s: %v", f[0], err)
		}
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

// Groq takes an audio URL in place of the file (contract C4). The native route must accept "url"
// without "file" and hand it to core as the url extra param, leaving the provider to decide; core
// refuses it for a provider that takes only a file. Before, the route answered 400 "file is required".
func TestPrepareTranscriptionRequest_URLWithoutFile(t *testing.T) {
	const audioURL = "https://example.com/audio/clip.m4a"
	req, _, err := prepareTranscriptionRequest(multipartTranscriptionCtxWithoutFile(t, [][2]string{
		{"model", "groq/whisper-large-v3-turbo"},
		{"url", audioURL},
		{"response_format", "verbose_json"},
	}), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.Input == nil || req.Input.File != nil {
		t.Fatalf("input = %+v, want an input with no file", req.Input)
	}
	if got, ok := req.AudioURL(); !ok || got != audioURL {
		t.Fatalf("audio url = %q (%v), want %q", got, ok, audioURL)
	}
	if !req.IsURLOnly() {
		t.Fatal("a request with a url and no file must be URL-only")
	}
}

// With neither a file nor a url (or an empty url) the route still answers "file is required".
func TestPrepareTranscriptionRequest_NoFileNoURL(t *testing.T) {
	for name, fields := range map[string][][2]string{
		"no url":    {{"model", "groq/whisper-large-v3-turbo"}},
		"empty url": {{"model", "groq/whisper-large-v3-turbo"}, {"url", ""}},
	} {
		_, _, err := prepareTranscriptionRequest(multipartTranscriptionCtxWithoutFile(t, fields), nil)
		if err == nil || err.Error() != "file is required" {
			t.Fatalf("%s: err = %v, want file is required", name, err)
		}
	}
}

// A file with a url is a file request: the file is sent, and the url is the vendor's extra param.
func TestPrepareTranscriptionRequest_FileAndURLIsNotURLOnly(t *testing.T) {
	req, _, err := prepareTranscriptionRequest(multipartTranscriptionCtx(t, [][2]string{
		{"model", "groq/whisper-large-v3-turbo"},
		{"url", "https://example.com/audio/clip.m4a"},
	}), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(req.Input.File) != "ID3fake-mp3" || req.IsURLOnly() {
		t.Fatalf("input = %+v, URL-only = %v; want the file kept and not URL-only", req.Input, req.IsURLOnly())
	}
}

// TXE-6539, the /v1/audio/transcriptions route into the Gemini transcription conversion:
// generateContent has no language, timestamp or output-format field, so language and
// timestamp_granularities are fields core's unified-field check refuses by name against what
// the Gemini provider declares, and a response_format other than json is a value the provider
// refuses. prompt and a json response_format are delivered.
func TestPrepareTranscriptionRequest_GeminiUnifiedFields(t *testing.T) {
	prepare := func(fields [][2]string) *schemas.BifrostTranscriptionRequest {
		req, _, err := prepareTranscriptionRequest(multipartTranscriptionCtx(t, append([][2]string{{"model", "gemini/gemini-3.5-transcribe"}}, fields...)), nil)
		if err != nil {
			t.Fatalf("prepareTranscriptionRequest: %v", err)
		}
		return req
	}
	assertRefused := func(fieldErr *schemas.BifrostError, param string) {
		t.Helper()
		if fieldErr == nil {
			t.Fatalf("%s was not refused", param)
		}
		if fieldErr.Error.Param != param || fieldErr.ExtraFields.ErrorType != schemas.ErrorTypeCallerUnsupportedParameter || *fieldErr.StatusCode != 400 {
			t.Errorf("got status %d error_type %q param %q, want 400 caller_unsupported_parameter %q",
				*fieldErr.StatusCode, fieldErr.ExtraFields.ErrorType, fieldErr.Error.Param, param)
		}
	}
	declared := func(req *schemas.BifrostTranscriptionRequest) *schemas.BifrostError {
		return schemas.CheckUnifiedFields(&schemas.BifrostRequest{TranscriptionRequest: req}, gemini.TranscriptionUnifiedFields)
	}

	assertRefused(declared(prepare([][2]string{{"language", "en"}})), "language")
	assertRefused(declared(prepare([][2]string{{"timestamp_granularities[]", "word"}})), "timestamp_granularities")

	srt := prepare([][2]string{{"response_format", "srt"}})
	if fieldErr := declared(srt); fieldErr != nil {
		t.Fatalf("response_format is a declared field, refused as a field: %+v", fieldErr.Error)
	}
	assertRefused(gemini.ValidateTranscriptionRequest(srt), "response_format")

	delivered := prepare([][2]string{{"prompt", "Transcribe the call."}, {"response_format", "json"}, {"temperature", "0.2"}})
	if fieldErr := declared(delivered); fieldErr != nil {
		t.Fatalf("a deliverable request was refused: %+v", fieldErr.Error)
	}
	if fieldErr := gemini.ValidateTranscriptionRequest(delivered); fieldErr != nil {
		t.Fatalf("a deliverable request was refused: %+v", fieldErr.Error)
	}
	upstream := gemini.ToGeminiTranscriptionRequest(delivered)
	if got := upstream.Contents[0].Parts[0].Text; got != "Transcribe the call." {
		t.Errorf("prompt sent to Gemini = %q", got)
	}
	// The multipart temperature field reaches generationConfig.temperature.
	if got := upstream.GenerationConfig.Temperature; got == nil || *got != 0.2 {
		t.Errorf("generationConfig.temperature = %v, want 0.2", got)
	}
	if _, leaked := upstream.ExtraParams["temperature"]; leaked {
		t.Error("temperature must not also be merged into the Gemini body as an extra param")
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

// prompt_cache_options is a typed chat field (Kimi prices a cache write by its ttl), so it is a
// known field like prompt_cache_key: it reaches the provider through ChatParameters whether or
// not the caller opened the extra-param channel, and is not duplicated into the extra params.
func TestChatPromptCacheOptionsIsAKnownField(t *testing.T) {
	body := []byte(`{"model":"moonshot/kimi-k2.6","messages":[{"role":"user","content":"hi"}],"prompt_cache_key":"tenant-42","prompt_cache_options":{"ttl":"1h"},"vendor_only_switch":true}`)

	extra, err := extractExtraParams(body, chatParamsKnownFields)
	if err != nil {
		t.Fatalf("extractExtraParams: %v", err)
	}
	if _, present := extra["prompt_cache_options"]; present {
		t.Errorf("prompt_cache_options was reported as an extra param: %v", extra)
	}
	if _, present := extra["prompt_cache_key"]; present {
		t.Errorf("prompt_cache_key was reported as an extra param: %v", extra)
	}
	if extra["vendor_only_switch"] != true {
		t.Errorf("an unknown field must still be an extra param: %v", extra)
	}

	var req ChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if req.ChatParameters == nil || req.PromptCacheOptions == nil || req.PromptCacheOptions.TTL == nil || *req.PromptCacheOptions.TTL != "1h" {
		t.Fatalf("typed prompt_cache_options = %+v, want ttl 1h", req.ChatParameters)
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

// Every SpeechParameters field is either a known field of the speech route or listed in
// schemas.UnifiedSpeechFieldsKeptAsExtraParams, the fields the route also leaves among the
// extra params (TXE-7077). The list is what core's speech-provider test walks, so a field
// added to SpeechParameters and to neither place would be copied into the extra params with
// no provider checked for forwarding it twice.
func TestSpeechFieldsKeptAsExtraParamsAreExactlyTheUnknownOnes(t *testing.T) {
	kept := map[string]bool{}
	for _, name := range schemas.UnifiedSpeechFieldsKeptAsExtraParams {
		kept[name] = true
		if speechParamsKnownFields[name] {
			t.Errorf("%s is in speechParamsKnownFields and in schemas.UnifiedSpeechFieldsKeptAsExtraParams: the route does not keep a known field", name)
		}
	}
	paramsType := reflect.TypeOf(schemas.SpeechParameters{})
	for i := 0; i < paramsType.NumField(); i++ {
		name, _, _ := strings.Cut(paramsType.Field(i).Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		if !speechParamsKnownFields[name] && !kept[name] {
			t.Errorf("SpeechParameters field %s is not in speechParamsKnownFields, so the route leaves it among the extra params: list it in schemas.UnifiedSpeechFieldsKeptAsExtraParams", name)
		}
		delete(kept, name)
	}
	for name := range kept {
		t.Errorf("schemas.UnifiedSpeechFieldsKeptAsExtraParams lists %s, which is not a SpeechParameters field", name)
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

// TXE-6524, contract C6, on ElevenLabs: a 200 with no audio is 502 provider_empty_result, and
// the vendor's character-cost header still reaches the caller, because ElevenLabs has charged
// for the request. The success usage header does not: an error carries no success usage.
func TestWriteSpeechAudioElevenlabsEmptyAudioKeepsCharacterCost(t *testing.T) {
	ctx := &fasthttp.RequestCtx{}
	usage := &schemas.SpeechUsage{VendorCost: &schemas.VendorCost{Amount: 9, Unit: schemas.VendorCostUnitCredit}}
	resp := &schemas.BifrostSpeechResponse{Usage: usage, ExtraFields: schemas.BifrostResponseExtraFields{
		RequestType:             schemas.SpeechRequest,
		Provider:                schemas.Elevenlabs,
		ProviderResponseHeaders: map[string]string{"character-cost": "9", "request-id": "el-empty"},
	}}
	// What the speech handler does before writing the body.
	lib.ApplyBifrostResponseHeaders(ctx, nil, resp.ExtraFields)
	lib.SetSpeechUsageHeader(ctx, resp.Usage)
	writeSpeechAudio(ctx, resp, "mp3_44100_128")

	if got := ctx.Response.StatusCode(); got != fasthttp.StatusBadGateway {
		t.Fatalf("status = %d, want 502", got)
	}
	if got := sentErrorType(t, ctx); got != string(schemas.ErrorTypeProviderEmptyResult) {
		t.Fatalf("error_type = %q, want provider_empty_result", got)
	}
	if got := string(ctx.Response.Header.Peek("character-cost")); got != "9" {
		t.Fatalf("character-cost = %q, want the vendor's 9 forwarded", got)
	}
	if got := string(ctx.Response.Header.Peek("request-id")); got != "el-empty" {
		t.Fatalf("request-id = %q, want el-empty", got)
	}
	if got := ctx.Response.Header.Peek(lib.HeaderBifrostUsage); got != nil {
		t.Fatalf("x-bifrost-usage = %q on a 502", got)
	}
}

// TXE-6524 on ElevenLabs byte-mode TTS: the audio keeps the Content-Type of the format asked
// for (the shared writer of TXE-6530), the vendor's character-cost header is forwarded
// unchanged, and x-bifrost-usage carries the same credits as usage.vendor_cost.
func TestWriteSpeechAudioElevenlabsFormatsAndVendorCost(t *testing.T) {
	cases := []struct {
		format, wantType string
		audio            []byte
	}{
		{"mp3_44100_128", "audio/mpeg", []byte{0xFF, 0xFB, 0x90, 0x00}},
		{"", "audio/mpeg", []byte("ID3\x04\x00")},
		{"opus_48000_128", "audio/ogg", []byte("OggS\x00\x02")},
		{"pcm_24000", "audio/pcm", []byte{0x01, 0x02, 0x03, 0x04}},
		{"pcm_44100", "audio/pcm", []byte{0xFF, 0xFB, 0x03, 0x04}}, // raw samples that look like an MPEG frame
		{"wav_24000", "audio/wav", []byte("RIFF\x24\x00\x00\x00WAVEfmt ")},
		{"ulaw_8000", "audio/basic", []byte{0x7F, 0x7F}},
	}
	for _, tc := range cases {
		t.Run(tc.format, func(t *testing.T) {
			ctx := &fasthttp.RequestCtx{}
			resp := &schemas.BifrostSpeechResponse{
				Audio: tc.audio,
				Usage: &schemas.SpeechUsage{InputChars: 11, VendorCost: &schemas.VendorCost{Amount: 11, Unit: schemas.VendorCostUnitCredit}},
				ExtraFields: schemas.BifrostResponseExtraFields{
					RequestType:             schemas.SpeechRequest,
					Provider:                schemas.Elevenlabs,
					ProviderResponseHeaders: map[string]string{"character-cost": "11"},
				},
			}
			lib.ApplyBifrostResponseHeaders(ctx, nil, resp.ExtraFields)
			lib.SetSpeechUsageHeader(ctx, resp.Usage)
			writeSpeechAudio(ctx, resp, tc.format)

			if got := ctx.Response.StatusCode(); got != fasthttp.StatusOK {
				t.Fatalf("status = %d: %s", got, ctx.Response.Body())
			}
			if got := string(ctx.Response.Header.ContentType()); got != tc.wantType {
				t.Fatalf("Content-Type = %q, want %q", got, tc.wantType)
			}
			if !bytes.Equal(ctx.Response.Body(), tc.audio) {
				t.Fatalf("body = %q, want the audio bytes", ctx.Response.Body())
			}
			if got := string(ctx.Response.Header.Peek("character-cost")); got != "11" {
				t.Fatalf("character-cost = %q, want 11 forwarded unchanged", got)
			}
			var usage struct {
				VendorCost *schemas.VendorCost `json:"vendor_cost"`
			}
			if err := json.Unmarshal(ctx.Response.Header.Peek(lib.HeaderBifrostUsage), &usage); err != nil {
				t.Fatalf("x-bifrost-usage %q: %v", ctx.Response.Header.Peek(lib.HeaderBifrostUsage), err)
			}
			if usage.VendorCost == nil || *usage.VendorCost != (schemas.VendorCost{Amount: 11, Unit: schemas.VendorCostUnitCredit}) {
				t.Fatalf("x-bifrost-usage vendor_cost = %+v, want {11 credit}", usage.VendorCost)
			}
		})
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

// deepgramUpstream stands in for api.deepgram.com: it records each request's query,
// Content-Type and body and answers with status and body.
type deepgramUpstream struct {
	status int
	body   string

	mu       sync.Mutex
	requests []deepgramUpstreamRequest
}

type deepgramUpstreamRequest struct {
	path, query, contentType, auth string
	body                           []byte
}

func (u *deepgramUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	u.mu.Lock()
	u.requests = append(u.requests, deepgramUpstreamRequest{
		path: r.URL.Path, query: r.URL.RawQuery, contentType: r.Header.Get("Content-Type"), auth: r.Header.Get("Authorization"), body: body,
	})
	u.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(u.status)
	_, _ = io.WriteString(w, u.body)
}

func (u *deepgramUpstream) only(t *testing.T) deepgramUpstreamRequest {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.requests) != 1 {
		t.Fatalf("upstream calls = %d, want exactly one", len(u.requests))
	}
	return u.requests[0]
}

func newDeepgramTestHandler(t *testing.T, upstream *deepgramUpstream) *CompletionHandler {
	t.Helper()
	srv := httptest.NewServer(upstream)
	t.Cleanup(srv.Close)
	client, err := bifrost.Init(context.Background(), schemas.BifrostConfig{
		Account: audioUpstreamAccount{baseURLs: map[schemas.ModelProvider]string{schemas.Deepgram: srv.URL}},
		Logger:  bifrost.NewDefaultLogger(schemas.LogLevelError),
	})
	if err != nil {
		t.Fatalf("bifrost.Init: %v", err)
	}
	t.Cleanup(client.Shutdown)
	return &CompletionHandler{client: client, config: &lib.Config{ClientConfig: &configstore.ClientConfig{}}}
}

const deepgramTwoChannelBody = `{"metadata":{"request_id":"a847f427-4ad5-4d67-9b95-db801e58251c","duration":30.5,"channels":2},` +
	`"results":{"channels":[{"alternatives":[{"transcript":"thank you for calling"}]},{"alternatives":[{"transcript":"hello"}]}]}}`

// POST /v1/audio/transcriptions with model deepgram/nova-3 and a url in place of the file:
// the route accepts it and Deepgram receives the JSON body {"url": ...}, with the options the
// caller wrote as form fields in the query string under their own names (contract C4). The
// response carries Deepgram's billable duration as usage, duration × channels (C5).
func TestTranscriptionRouteSendsURLWithoutFileToDeepgram(t *testing.T) {
	const audioURL = "https://example.com/audio/call.wav"
	upstream := &deepgramUpstream{status: http.StatusOK, body: deepgramTwoChannelBody}
	h := newDeepgramTestHandler(t, upstream)

	ctx := multipartTranscriptionCtxWithoutFile(t, [][2]string{
		{"model", "deepgram/nova-3"},
		{"url", audioURL},
		{"language", "en"},
		{"multichannel", "true"},
		{"keyterm", "Bifrost gateway"},
		{"keyterm", "Tokun"},
	})
	h.transcription(ctx)

	if ctx.Response.StatusCode() != fasthttp.StatusOK {
		t.Fatalf("status = %d, body %s", ctx.Response.StatusCode(), ctx.Response.Body())
	}
	got := upstream.only(t)
	if got.path != "/v1/listen" || got.auth != "Token sk-test" {
		t.Fatalf("upstream request = %s with %q, want /v1/listen with the Token scheme", got.path, got.auth)
	}
	if got.contentType != "application/json" || string(got.body) != `{"url":"`+audioURL+`"}` {
		t.Fatalf("upstream body = %s (%s), want the JSON url body", got.body, got.contentType)
	}
	if got.query != "keyterm=Bifrost+gateway&keyterm=Tokun&language=en&model=nova-3&multichannel=true" {
		t.Fatalf("upstream query = %q, want every option under its own name and no url", got.query)
	}
	var resp struct {
		Text  string `json:"text"`
		Usage struct {
			Type    string  `json:"type"`
			Seconds float64 `json:"seconds"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(ctx.Response.Body(), &resp); err != nil {
		t.Fatalf("response is not JSON: %v: %s", err, ctx.Response.Body())
	}
	if resp.Text != "thank you for calling\nhello" || resp.Usage.Type != "duration" || resp.Usage.Seconds != 61 {
		t.Fatalf("response = %s, want both channels and usage {type: duration, seconds: 61}", ctx.Response.Body())
	}
}

// The same route with a file: the audio bytes are Deepgram's request body.
func TestTranscriptionRouteSendsFileBytesToDeepgram(t *testing.T) {
	upstream := &deepgramUpstream{status: http.StatusOK, body: deepgramTwoChannelBody}
	h := newDeepgramTestHandler(t, upstream)

	ctx := multipartTranscriptionCtx(t, [][2]string{{"model", "deepgram/nova-3"}, {"diarize", "true"}})
	h.transcription(ctx)

	if ctx.Response.StatusCode() != fasthttp.StatusOK {
		t.Fatalf("status = %d, body %s", ctx.Response.StatusCode(), ctx.Response.Body())
	}
	got := upstream.only(t)
	if got.contentType != "audio/mpeg" || string(got.body) != "ID3fake-mp3" || got.query != "diarize=true&model=nova-3" {
		t.Fatalf("upstream request = %q (%s) ?%s, want the audio bytes and the options in the query", got.body, got.contentType, got.query)
	}
}

// A Deepgram error reaches the caller with Deepgram's status code, text and code, and
// extra_fields.error_type (contract C6). A field Deepgram cannot take is 400
// caller_unsupported_parameter and makes no upstream call (C4).
func TestTranscriptionRouteReturnsDeepgramErrors(t *testing.T) {
	upstream := &deepgramUpstream{status: http.StatusPaymentRequired, body: `{"err_code":"ASR_PAYMENT_REQUIRED","err_msg":"Project does not have enough credits for an ASR request and does not have an overage agreement.","request_id":"r-402"}`}
	h := newDeepgramTestHandler(t, upstream)

	type errorBody struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Param   string `json:"param"`
		} `json:"error"`
		ExtraFields struct {
			ErrorType string `json:"error_type"`
			Provider  string `json:"provider"`
		} `json:"extra_fields"`
	}

	ctx := multipartTranscriptionCtx(t, [][2]string{{"model", "deepgram/nova-3"}})
	h.transcription(ctx)
	var vendorErr errorBody
	if err := json.Unmarshal(ctx.Response.Body(), &vendorErr); err != nil {
		t.Fatalf("error response is not JSON: %v: %s", err, ctx.Response.Body())
	}
	if ctx.Response.StatusCode() != fasthttp.StatusPaymentRequired || vendorErr.ExtraFields.ErrorType != string(schemas.ErrorTypeProviderBilling) ||
		vendorErr.Error.Code != "ASR_PAYMENT_REQUIRED" || !strings.HasPrefix(vendorErr.Error.Message, "Project does not have enough credits") || vendorErr.ExtraFields.Provider != "deepgram" {
		t.Fatalf("status %d, body %s; want Deepgram's 402 as provider_billing", ctx.Response.StatusCode(), ctx.Response.Body())
	}
	upstream.only(t)

	// The native route parses response_format into the unified field and leaves temperature and
	// include[] in the extra params; each is refused the same way, and none reaches Deepgram.
	for field, param := range map[[2]string]string{
		{"response_format", "srt"}: "response_format",
		{"temperature", "0.2"}:     "temperature",
		{"include[]", "logprobs"}:  "include",
	} {
		ctx = multipartTranscriptionCtx(t, [][2]string{{"model", "deepgram/nova-3"}, field})
		h.transcription(ctx)
		var refused errorBody
		if err := json.Unmarshal(ctx.Response.Body(), &refused); err != nil {
			t.Fatalf("error response is not JSON: %v: %s", err, ctx.Response.Body())
		}
		if ctx.Response.StatusCode() != fasthttp.StatusBadRequest || refused.ExtraFields.ErrorType != string(schemas.ErrorTypeCallerUnsupportedParameter) || refused.Error.Param != param {
			t.Fatalf("%s: status %d, body %s; want 400 caller_unsupported_parameter naming %s", field[0], ctx.Response.StatusCode(), ctx.Response.Body(), param)
		}
		upstream.only(t)
	}
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
		// Gemini answers raw PCM, returned as is when pcm is named (wav is the default,
		// TXE-6539); it used to go out labeled audio/mpeg.
		ctx := speechCtx(`{"model":"gemini/gemini-2.5-flash-preview-tts","input":"Hello","voice":"Kore","response_format":"pcm"}`)
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

// TXE-6606 (contract C4): /v1/images/generations JSON reaches xAI's wire; the xaiEgress helper is
// in inference_imageedit_test.go.
func TestXAIImageGenerationIngressToEgress(t *testing.T) {
	t.Run("JSON body: quality, resolution, aspect_ratio and n reach xAI", func(t *testing.T) {
		egress := &xaiEgress{}
		provider := egress.provider(t)
		_, req, err := prepareImageGenerationRequest(jsonRequestCtx("/v1/images/generations",
			`{"model":"xai/grok-imagine-image-2.0","prompt":"a lighthouse","n":2,"quality":"medium","resolution":"2K","aspect_ratio":"16:9"}`), nil)
		require.NoError(t, err)
		require.Equal(t, schemas.XAI, req.Provider)

		response, bifrostErr := provider.ImageGeneration(xaiEgressCtx(), xaiEgressKey, req)
		require.Nil(t, bifrostErr)
		require.Len(t, response.Data, 1)

		path, ctype, body := egress.only(t)
		require.Equal(t, "/v1/images/generations", path)
		require.Equal(t, "application/json", ctype)
		require.Equal(t, "grok-imagine-image-2.0", body["model"])
		require.Equal(t, "medium", body["quality"])
		require.Equal(t, "2k", body["resolution"], "the unified tier is sent in xAI's spelling, once")
		require.Equal(t, "16:9", body["aspect_ratio"])
		require.EqualValues(t, 2, body["n"])
	})

	// TXE-6960: size is parsed into the typed params by the parser that /v1/images/generations and
	// /v1/async/images/generations share, and xAI refuses it before any upstream call.
	t.Run("JSON body: size is refused before any upstream call", func(t *testing.T) {
		egress := &xaiEgress{}
		provider := egress.provider(t)
		_, req, err := prepareImageGenerationRequest(jsonRequestCtx("/v1/async/images/generations",
			`{"model":"xai/grok-imagine-image-2.0","prompt":"a lighthouse","size":"1024x1024"}`), nil)
		require.NoError(t, err)
		require.NotNil(t, req.Params.Size, "the parser carries size into the typed params")

		_, bifrostErr := provider.ImageGeneration(xaiEgressCtx(), xaiEgressKey, req)
		require.NotNil(t, bifrostErr)
		require.Equal(t, 400, *bifrostErr.StatusCode)
		require.Equal(t, schemas.ErrorTypeCallerUnsupportedParameter, bifrostErr.ExtraFields.ErrorType)
		require.Equal(t, "size", bifrostErr.Error.Param)
		require.Zero(t, egress.calls())
	})

	t.Run("JSON body: deferred is refused before any upstream call", func(t *testing.T) {
		egress := &xaiEgress{}
		provider := egress.provider(t)
		_, req, err := prepareImageGenerationRequest(jsonRequestCtx("/v1/images/generations",
			`{"model":"xai/grok-imagine-image-2.0","prompt":"a lighthouse","deferred":true}`), nil)
		require.NoError(t, err)

		_, bifrostErr := provider.ImageGeneration(xaiEgressCtx(), xaiEgressKey, req)
		require.NotNil(t, bifrostErr)
		require.Equal(t, schemas.ErrorTypeCallerUnsupportedParameter, bifrostErr.ExtraFields.ErrorType)
		require.Equal(t, "deferred", bifrostErr.Error.Param)
		require.Zero(t, egress.calls())
	})
}

// DashScope Qwen3-TTS answers with a link to the audio, not the bytes. /v1/audio/speech must
// answer JSON carrying the vendor's url, expires_at and usage (billed_characters from the
// vendor's characters), not a 502 for missing audio bytes (TXE-6433, contract C9 and C5).
// Qwen3-ASR takes an audio URL unchanged and reports seconds.
func TestDashScopeSpeechLinkAndTranscriptionURL(t *testing.T) {
	const audioLink = "https://dashscope-result-sgp.oss-ap-southeast-1.aliyuncs.com/1d/a.wav?Expires=1766113409&Signature=x"
	tts := &audioUpstream{contentType: "application/json", body: []byte(`{"status_code":200,"request_id":"5c63c65c","code":"","message":"",` +
		`"output":{"text":null,"finish_reason":"stop","choices":null,"audio":{"data":"","url":"` + audioLink + `","id":"audio_5c63c65c","expires_at":1766113409}},` +
		`"usage":{"input_tokens":0,"output_tokens":0,"characters":195}}`)}
	h := newAudioTestHandler(t, map[schemas.ModelProvider]*audioUpstream{schemas.DashScope: tts})

	t.Run("speech link", func(t *testing.T) {
		ctx := speechCtx(`{"model":"dashscope/qwen3-tts-flash","input":"Hello","voice":"Cherry","language_code":"English"}`)
		h.speech(ctx)
		if ctx.Response.StatusCode() != fasthttp.StatusOK {
			t.Fatalf("status %d: %s", ctx.Response.StatusCode(), ctx.Response.Body())
		}
		if got := string(ctx.Response.Header.ContentType()); !strings.HasPrefix(got, "application/json") {
			t.Fatalf("Content-Type = %q, want JSON", got)
		}
		var body struct {
			URL       string         `json:"url"`
			ExpiresAt int64          `json:"expires_at"`
			Usage     map[string]any `json:"usage"`
		}
		if err := json.Unmarshal(ctx.Response.Body(), &body); err != nil {
			t.Fatalf("body is not JSON: %v: %s", err, ctx.Response.Body())
		}
		if body.URL != audioLink || body.ExpiresAt != 1766113409 {
			t.Fatalf("url, expires_at = %q, %d; want the vendor's link and expiry", body.URL, body.ExpiresAt)
		}
		if body.Usage["billed_characters"] != float64(195) {
			t.Fatalf("usage = %v, want billed_characters 195", body.Usage)
		}
		if usage := speechUsageHeader(t, ctx); usage["billed_characters"] != float64(195) {
			t.Fatalf("x-bifrost-usage = %v, want billed_characters 195", usage)
		}
		tts.mu.Lock()
		sent := string(tts.gotBody)
		tts.mu.Unlock()
		for _, want := range []string{`"text":"Hello"`, `"voice":"Cherry"`, `"language_type":"English"`} {
			if !strings.Contains(sent, want) {
				t.Fatalf("vendor body %s lacks %s", sent, want)
			}
		}
		if strings.Contains(sent, `"language_code"`) {
			t.Fatalf("vendor body carries the unified language_code: %s", sent)
		}
	})

	t.Run("speech refuses speed without an upstream call", func(t *testing.T) {
		tts.mu.Lock()
		before := tts.gotRequests
		tts.mu.Unlock()
		ctx := speechCtx(`{"model":"dashscope/qwen3-tts-flash","input":"Hello","voice":"Cherry","speed":1.5}`)
		h.speech(ctx)
		if ctx.Response.StatusCode() != fasthttp.StatusBadRequest {
			t.Fatalf("status %d: %s, want 400", ctx.Response.StatusCode(), ctx.Response.Body())
		}
		if !strings.Contains(string(ctx.Response.Body()), `"caller_unsupported_parameter"`) || !strings.Contains(string(ctx.Response.Body()), `"param":"speed"`) {
			t.Fatalf("body = %s, want caller_unsupported_parameter naming speed", ctx.Response.Body())
		}
		tts.mu.Lock()
		after := tts.gotRequests
		tts.mu.Unlock()
		if after != before {
			t.Fatalf("a refused request reached the vendor")
		}
	})

	t.Run("transcription url", func(t *testing.T) {
		const clip = "https://example.com/audio/clip.wav"
		tts.mu.Lock()
		tts.body = []byte(`{"output":{"choices":[{"finish_reason":"stop","message":{"annotations":[{"language":"en","type":"audio_info","emotion":"neutral"}],` +
			`"content":[{"text":"Welcome to Alibaba Cloud."}],"role":"assistant"}}]},` +
			`"usage":{"input_tokens_details":{"text_tokens":0},"output_tokens_details":{"text_tokens":6},"seconds":1},"request_id":"568e2bf0"}`)
		tts.mu.Unlock()
		ctx := multipartTranscriptionCtxWithoutFile(t, [][2]string{{"model", "dashscope/qwen3-asr-flash"}, {"url", clip}})
		h.transcription(ctx)
		if ctx.Response.StatusCode() != fasthttp.StatusOK {
			t.Fatalf("status %d: %s", ctx.Response.StatusCode(), ctx.Response.Body())
		}
		var body struct {
			Text  string         `json:"text"`
			Usage map[string]any `json:"usage"`
		}
		if err := json.Unmarshal(ctx.Response.Body(), &body); err != nil {
			t.Fatalf("body is not JSON: %v", err)
		}
		if body.Text != "Welcome to Alibaba Cloud." || body.Usage["seconds"] != float64(1) || body.Usage["type"] != "duration" {
			t.Fatalf("body = %s, want the vendor's text and seconds", ctx.Response.Body())
		}
		tts.mu.Lock()
		sent := string(tts.gotBody)
		tts.mu.Unlock()
		if !strings.Contains(sent, `{"audio":"`+clip+`"}`) {
			t.Fatalf("vendor body %s does not carry the URL unchanged", sent)
		}
		if strings.Contains(sent, `"url"`) {
			t.Fatalf("vendor body carries the url extra param at the top level: %s", sent)
		}
	})
}
