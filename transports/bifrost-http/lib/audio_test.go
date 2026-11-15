package lib

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

func TestSpeechContentType(t *testing.T) {
	wav := []byte("RIFF\x24\x00\x00\x00WAVEfmt ")
	for _, tc := range []struct {
		name   string
		audio  []byte
		format string
		want   string
	}{
		{"wav bytes win over an mp3 request", wav, "mp3", "audio/wav"},
		{"id3 mp3", []byte("ID3\x04\x00frames"), "", "audio/mpeg"},
		{"flac", []byte("fLaC\x00\x00"), "flac", "audio/flac"},
		{"ogg opus", []byte("OggS\x00\x02"), "opus", "audio/ogg"},
		{"aiff", []byte("FORM\x00\x00\x00\x00AIFF"), "", "audio/aiff"},
		{"m4a", []byte("\x00\x00\x00\x20ftypM4A "), "", "audio/mp4"},
		{"adif aac", []byte("ADIF\x00"), "aac", "audio/aac"},
		// Raw formats have no signature; a PCM sample that happens to look like a frame sync
		// must still be labeled from the request.
		{"openai pcm", []byte{0xFF, 0xFB, 0x10, 0x00}, "pcm", "audio/pcm"},
		{"elevenlabs pcm", []byte{0x01, 0x02, 0x03, 0x04}, "pcm_16000", "audio/pcm"},
		{"elevenlabs ulaw", []byte{0x7F, 0x7F, 0x7F, 0x7F}, "ulaw_8000", "audio/basic"},
		{"alaw", []byte{0xD5, 0xD5, 0xD5, 0xD5}, "alaw_8000", "audio/x-alaw-basic"},
		// MiniMax pcmu_raw is mu-law, not linear PCM; pcmu_wav is a WAV file.
		{"minimax pcmu_raw", []byte{0x7F, 0xFF, 0x7F, 0xFF}, "pcmu_raw", "audio/basic"},
		{"minimax pcmu_wav", wav, "pcmu_wav", "audio/wav"},
		{"elevenlabs mp3 frame", []byte{0xFF, 0xFB, 0x90, 0x00}, "mp3_44100_128", "audio/mpeg"},
		{"bare mpeg frame, default format", []byte{0xFF, 0xF3, 0x90, 0x00}, "", "audio/mpeg"},
		{"bare adts frame, default format", []byte{0xFF, 0xF1, 0x50, 0x80}, "", "audio/aac"},
		{"nothing known, default format", []byte{0x00, 0x01, 0x02, 0x03}, "", "audio/mpeg"},
		{"nothing known, unknown format", []byte{0x00, 0x01, 0x02, 0x03}, "speex", "application/octet-stream"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := SpeechContentType(tc.audio, tc.format); got != tc.want {
				t.Fatalf("SpeechContentType(%q) = %q, want %q", tc.format, got, tc.want)
			}
		})
	}
}

// A non-streaming speech response used to be written as audio/mpeg with no usage anywhere. Now
// the body stays the audio bytes, the type follows the audio, and the unified usage travels in
// x-bifrost-usage.
func TestWriteSpeechAudioResponse(t *testing.T) {
	resp := &schemas.BifrostSpeechResponse{
		Audio: []byte("RIFF\x24\x00\x00\x00WAVEfmt "),
		Usage: &schemas.SpeechUsage{InputChars: 40, InputTokens: 7, OutputTokens: 90},
	}
	var ctx fasthttp.RequestCtx
	SetSpeechUsageHeader(&ctx, resp.Usage)
	WriteSpeechAudioResponse(&ctx, resp, "mp3")

	if got := string(ctx.Response.Header.ContentType()); got != "audio/wav" {
		t.Fatalf("Content-Type = %q, want audio/wav", got)
	}
	if got := string(ctx.Response.Header.Peek("Content-Disposition")); got != "attachment; filename=speech.wav" {
		t.Fatalf("Content-Disposition = %q", got)
	}
	if !slices.Equal(ctx.Response.Body(), resp.Audio) {
		t.Fatalf("body = %q, want the audio bytes", ctx.Response.Body())
	}
	var usage map[string]any
	if err := json.Unmarshal(ctx.Response.Header.Peek(HeaderBifrostUsage), &usage); err != nil {
		t.Fatalf("x-bifrost-usage is not JSON: %q: %v", ctx.Response.Header.Peek(HeaderBifrostUsage), err)
	}
	if usage["input_chars"] != float64(40) || usage["input_tokens"] != float64(7) || usage["output_tokens"] != float64(90) {
		t.Fatalf("x-bifrost-usage = %v", usage)
	}
}

func TestWriteSpeechAudioResponseUsesProviderContentType(t *testing.T) {
	resp := &schemas.BifrostSpeechResponse{
		Audio:       []byte{0xFF, 0xFB, 0x10, 0x00},
		ContentType: "audio/L16;codec=pcm;rate=24000",
	}
	var ctx fasthttp.RequestCtx
	WriteSpeechAudioResponse(&ctx, resp, "")

	if got := string(ctx.Response.Header.ContentType()); got != "audio/L16;codec=pcm;rate=24000" {
		t.Fatalf("Content-Type = %q, want the provider's type", got)
	}
	if got := string(ctx.Response.Header.Peek("Content-Disposition")); got != "attachment; filename=speech.pcm" {
		t.Fatalf("Content-Disposition = %q", got)
	}
	if got := ctx.Response.Header.Peek(HeaderBifrostUsage); got != nil {
		t.Fatalf("x-bifrost-usage = %q, want no header when the response has no usage", got)
	}
}

func TestNonEmptyFormValues(t *testing.T) {
	if got := NonEmptyFormValues([]string{"", "a", "", "b"}); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("NonEmptyFormValues = %v, want [a b]", got)
	}
	if got := NonEmptyFormValues([]string{""}); len(got) != 0 {
		t.Fatalf("NonEmptyFormValues = %v, want none", got)
	}
	if got := NonEmptyFormValues([]string{"", "only"}); !slices.Equal(got, []string{"only"}) {
		t.Fatalf("NonEmptyFormValues = %v, want [only]", got)
	}
	// A repeated field's result is a copy: changing it cannot reach the parsed form.
	form := []string{"a", "b"}
	got := NonEmptyFormValues(form)
	got[0] = "changed"
	if form[0] != "a" {
		t.Fatalf("NonEmptyFormValues aliased the form's slice for a repeated field")
	}
}

// A single-valued field is the common extra-param case and must not allocate (AGENTS.md: the
// common case must not regress).
func TestNonEmptyFormValuesSingleValueDoesNotAllocate(t *testing.T) {
	single := []string{"logprobs"}
	if allocs := testing.AllocsPerRun(100, func() { _ = NonEmptyFormValues(single) }); allocs != 0 {
		t.Fatalf("NonEmptyFormValues allocated %v times for one value, want 0", allocs)
	}
}

func TestSpeechFilenameForMuLaw(t *testing.T) {
	if got := speechFilenameForContentType("audio/basic", "pcmu_raw"); got != "speech.ulaw" {
		t.Fatalf("filename = %q, want speech.ulaw", got)
	}
}

// A vendor's own spelling of a WAV or WebM type names the file after the audio, not after
// the mp3 default (TXE-6542: Azure AI Speech answers a riff-* format as audio/x-wav).
func TestSpeechFilenameForVendorContentTypes(t *testing.T) {
	for contentType, want := range map[string]string{
		"audio/x-wav":             "speech.wav",
		"Audio/X-WAV":             "speech.wav",
		"audio/wave":              "speech.wav",
		"audio/vnd.wave":          "speech.wav",
		"audio/webm; codecs=opus": "speech.webm",
	} {
		if got := speechFilenameForContentType(contentType, "riff-48khz-16bit-mono-pcm"); got != want {
			t.Errorf("%s: filename = %q, want %q", contentType, got, want)
		}
	}
}

// BenchmarkWriteSpeechAudioResponse measures the non-streaming speech writer: the usage header
// (one JSON encode of SpeechUsage) plus the Content-Type choice and body. "provider_type" is the
// OpenAI and Gemini case, where the provider recorded the type; "sniffed" derives it from the
// bytes; "no_usage" is a response without usage.
func BenchmarkWriteSpeechAudioResponse(b *testing.B) {
	audio := append([]byte("ID3\x04\x00"), make([]byte, 32*1024)...)
	usage := &schemas.SpeechUsage{InputChars: 36, InputTokens: 7, OutputTokens: 42, TotalTokens: 49}
	for _, tc := range []struct {
		name string
		resp *schemas.BifrostSpeechResponse
	}{
		{"provider_type", &schemas.BifrostSpeechResponse{Audio: audio, Usage: usage, ContentType: "audio/flac"}},
		{"sniffed", &schemas.BifrostSpeechResponse{Audio: audio, Usage: usage}},
		{"no_usage", &schemas.BifrostSpeechResponse{Audio: audio}},
	} {
		b.Run(tc.name, func(b *testing.B) {
			var ctx fasthttp.RequestCtx
			b.ReportAllocs()
			for b.Loop() {
				ctx.Response.Reset()
				SetSpeechUsageHeader(&ctx, tc.resp.Usage)
				WriteSpeechAudioResponse(&ctx, tc.resp, "mp3")
			}
		})
	}
}

// A vendor 200 with no audio is 502 provider_empty_result (contract C6, TXE-7021). The writer
// writes nothing for it: no body, no audio Content-Type, and the success usage header a caller set
// beforehand is removed. It returns the error, carrying the response's routing fields, for the
// caller to send in its route's shape.
func TestWriteSpeechAudioResponseRefusesMissingAudio(t *testing.T) {
	for name, resp := range map[string]*schemas.BifrostSpeechResponse{
		"nil response": nil,
		"no audio":     {ExtraFields: schemas.BifrostResponseExtraFields{Provider: schemas.Elevenlabs, RequestType: schemas.SpeechRequest}},
		"empty audio":  {Audio: []byte{}, ExtraFields: schemas.BifrostResponseExtraFields{Provider: schemas.OpenAI, RequestType: schemas.SpeechRequest}},
	} {
		t.Run(name, func(t *testing.T) {
			var ctx fasthttp.RequestCtx
			SetSpeechUsageHeader(&ctx, &schemas.SpeechUsage{InputChars: 5})
			emptyErr := WriteSpeechAudioResponse(&ctx, resp, "mp3")
			if emptyErr == nil {
				t.Fatal("no error for a response without audio")
			}
			if got := emptyErr.ResolveErrorType(); got != schemas.ErrorTypeProviderEmptyResult {
				t.Fatalf("error_type = %q, want provider_empty_result", got)
			}
			if got := emptyErr.EffectiveHTTPStatus(); got != fasthttp.StatusBadGateway {
				t.Fatalf("status = %d, want 502", got)
			}
			if resp != nil && emptyErr.ExtraFields.Provider != resp.ExtraFields.Provider {
				t.Fatalf("provider = %q, want %q", emptyErr.ExtraFields.Provider, resp.ExtraFields.Provider)
			}
			if len(ctx.Response.Body()) != 0 || ctx.Response.Header.Peek("Content-Disposition") != nil {
				t.Fatalf("the writer wrote a response: body %q", ctx.Response.Body())
			}
			if ctx.Response.Header.Peek(HeaderBifrostUsage) != nil {
				t.Fatal("x-bifrost-usage kept on an error")
			}
		})
	}
	var ctx fasthttp.RequestCtx
	if err := WriteSpeechAudioResponse(&ctx, &schemas.BifrostSpeechResponse{Audio: []byte("ID3")}, "mp3"); err != nil {
		t.Fatalf("audio refused: %+v", err)
	}
}

// Every caller of WriteSpeechAudioResponse must send the provider_empty_result error it returns;
// a caller that drops it answers 200 with an empty body, which is the gap TXE-7021 closed on the
// /openai speech routes. This walks the transport's Go sources and fails a call whose result is
// discarded, as a statement or assigned to _.
func TestEveryWriteSpeechAudioResponseCallerSendsTheEmptyResult(t *testing.T) {
	root := ".." // transports/bifrost-http
	calls := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		isWriter := func(e ast.Expr) bool {
			call, ok := e.(*ast.CallExpr)
			if !ok {
				return false
			}
			switch fn := call.Fun.(type) {
			case *ast.SelectorExpr:
				return fn.Sel.Name == "WriteSpeechAudioResponse"
			case *ast.Ident:
				return fn.Name == "WriteSpeechAudioResponse"
			}
			return false
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				if isWriter(x) {
					calls++
				}
			case *ast.ExprStmt:
				if isWriter(x.X) {
					t.Errorf("%s: WriteSpeechAudioResponse's provider_empty_result error is discarded", fset.Position(x.Pos()))
				}
			case *ast.AssignStmt:
				for i, rhs := range x.Rhs {
					if isWriter(rhs) && i < len(x.Lhs) {
						if id, ok := x.Lhs[i].(*ast.Ident); ok && id.Name == "_" {
							t.Errorf("%s: WriteSpeechAudioResponse's provider_empty_result error is assigned to _", fset.Position(x.Pos()))
						}
					}
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls < 2 {
		t.Fatalf("found %d calls of WriteSpeechAudioResponse; the walk no longer sees the speech handler and the integration router", calls)
	}
}
