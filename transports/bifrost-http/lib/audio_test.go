package lib

import (
	"encoding/json"
	"slices"
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
}

func TestSpeechFilenameForMuLaw(t *testing.T) {
	if got := speechFilenameForContentType("audio/basic", "pcmu_raw"); got != "speech.ulaw" {
		t.Fatalf("filename = %q, want speech.ulaw", got)
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
