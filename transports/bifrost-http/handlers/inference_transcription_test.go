package handlers

import (
	"bytes"
	"mime/multipart"
	"slices"
	"testing"

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
