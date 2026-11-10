package handlers

import (
	"testing"

	"github.com/capsohq/bifrost/core/providers/gemini"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

// TestPrepareSpeechRequestVoiceValidation locks in the transport-layer voice
// requirement and its ElevenLabs carve-out. The transport cannot resolve
// virtual-key aliases, so it cannot tell a voice-less ElevenLabs sound-effect
// model (including an alias that resolves to one) from a text-to-speech model
// that omitted its voice. For ElevenLabs the voice check is therefore deferred
// to ElevenlabsProvider.Speech (which runs after alias resolution); every other
// provider keeps the handler-level "voice is required" 400.
func TestPrepareSpeechRequestVoiceValidation(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantError bool
	}{
		{
			// Regression: an alias like "my-sfx" -> "eleven_text_to_sound_v2"
			// carries no "eleven_text_to_sound" substring, so a name-based check
			// would wrongly 400 it before the provider could dispatch it.
			name:      "elevenlabs alias without voice is deferred to provider",
			body:      `{"model":"elevenlabs/my-sfx","input":"glass shattering on concrete"}`,
			wantError: false,
		},
		{
			name:      "elevenlabs literal sound model without voice is allowed",
			body:      `{"model":"elevenlabs/eleven_text_to_sound_v2","input":"rain on a tin roof"}`,
			wantError: false,
		},
		{
			name:      "elevenlabs tts without voice is deferred to provider (no transport 400)",
			body:      `{"model":"elevenlabs/eleven_multilingual_v2","input":"hello there"}`,
			wantError: false,
		},
		{
			name:      "non-elevenlabs provider without voice still errors at the transport",
			body:      `{"model":"openai/tts-1","input":"hello there"}`,
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := &fasthttp.RequestCtx{}
			ctx.Request.SetBodyString(tt.body)

			_, _, err := prepareSpeechRequest(ctx, nil)
			if tt.wantError && err == nil {
				t.Fatalf("expected voice-required error, got nil")
			}
			if !tt.wantError && err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
			if tt.wantError && err != nil && err.Error() != "voice is required for speech completion" {
				t.Fatalf("error = %q, want %q", err.Error(), "voice is required for speech completion")
			}
		})
	}
}

// TXE-6539, the /v1/audio/speech route into the Gemini speech conversion: max_output_tokens
// is parsed as the unified field and reaches Gemini as generationConfig.maxOutputTokens, and a
// field Gemini cannot take (speed, instructions, language_code) is one core's unified-field
// check refuses by name against what the Gemini provider declares.
func TestPrepareSpeechRequestGeminiUnifiedFields(t *testing.T) {
	prepare := func(t *testing.T, body string) *schemas.BifrostSpeechRequest {
		t.Helper()
		ctx := &fasthttp.RequestCtx{}
		ctx.Request.SetBodyString(body)
		_, bifrostReq, err := prepareSpeechRequest(ctx, nil)
		if err != nil {
			t.Fatalf("prepareSpeechRequest: %v", err)
		}
		return bifrostReq
	}

	t.Run("max_output_tokens reaches generationConfig.maxOutputTokens", func(t *testing.T) {
		bifrostReq := prepare(t, `{"model":"gemini/gemini-3.8-flash-tts","input":"hello","voice":"Kore","max_output_tokens":2048,"stream_format":"sse"}`)
		if bifrostReq.Params == nil || bifrostReq.Params.MaxOutputTokens == nil || *bifrostReq.Params.MaxOutputTokens != 2048 {
			t.Fatalf("max_output_tokens not parsed into the unified field: %+v", bifrostReq.Params)
		}
		if fieldErr := schemas.CheckUnifiedFields(&schemas.BifrostRequest{SpeechRequest: bifrostReq}, gemini.SpeechUnifiedFields); fieldErr != nil {
			t.Fatalf("a deliverable request was refused: %+v", fieldErr.Error)
		}
		upstream, err := gemini.ToGeminiSpeechRequest(bifrostReq)
		if err != nil {
			t.Fatalf("ToGeminiSpeechRequest: %v", err)
		}
		if upstream.GenerationConfig.MaxOutputTokens != 2048 {
			t.Errorf("generationConfig.maxOutputTokens = %d, want 2048", upstream.GenerationConfig.MaxOutputTokens)
		}
		if _, leaked := upstream.ExtraParams["max_output_tokens"]; leaked {
			t.Error("max_output_tokens must not also be merged into the Gemini body under its unified name")
		}
	})

	for field, value := range map[string]string{"speed": "1.5", "instructions": `"Speak slowly."`, "language_code": `"en-US"`} {
		t.Run(field+" is refused by name", func(t *testing.T) {
			bifrostReq := prepare(t, `{"model":"gemini/gemini-3.8-flash-tts","input":"hello","voice":"Kore","`+field+`":`+value+`}`)
			fieldErr := schemas.CheckUnifiedFields(&schemas.BifrostRequest{SpeechRequest: bifrostReq}, gemini.SpeechUnifiedFields)
			if fieldErr == nil {
				t.Fatalf("%s was not refused", field)
			}
			if fieldErr.Error.Param != field || fieldErr.ExtraFields.ErrorType != schemas.ErrorTypeCallerUnsupportedParameter || *fieldErr.StatusCode != 400 {
				t.Errorf("got status %d error_type %q param %q, want 400 caller_unsupported_parameter %q",
					*fieldErr.StatusCode, fieldErr.ExtraFields.ErrorType, fieldErr.Error.Param, field)
			}
		})
	}
}
