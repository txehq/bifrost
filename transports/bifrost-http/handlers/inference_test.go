package handlers

import (
	"bytes"
	"mime/multipart"
	"slices"
	"testing"

	schemas "github.com/capsohq/bifrost/core/schemas"
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
	if p.AspectRatio == nil || *p.AspectRatio != "16:9" {
		t.Fatalf("aspect_ratio = %v, want 16:9", p.AspectRatio)
	}
	if p.Resolution == nil || *p.Resolution != "720p" {
		t.Fatalf("resolution = %v, want 720p", p.Resolution)
	}
	if p.LastFrame == nil || *p.LastFrame != "https://example.com/last.png" {
		t.Fatalf("last_frame = %v", p.LastFrame)
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
	if req.Params.Resolution == nil || *req.Params.Resolution != "2K" {
		t.Fatalf("resolution = %v, want 2K", req.Params.Resolution)
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
