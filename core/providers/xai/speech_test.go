package xai_test

import (
	"encoding/base64"
	"net/http"
	"strings"
	"testing"

	"github.com/capsohq/bifrost/core/providers/xai"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func xaiSpeechRequest(params *schemas.SpeechParameters) *schemas.BifrostSpeechRequest {
	return &schemas.BifrostSpeechRequest{
		Provider: schemas.XAI,
		Model:    "grok-voice-tts",
		Input:    &schemas.SpeechInput{Input: "Hello world."},
		Params:   params,
	}
}

// A speech request maps the unified fields onto /v1/tts fields, takes the rest of
// output_format from the extra fields, merges the other extra fields unchanged, and returns
// the audio bytes xAI answered with (TXE-6429, contract C4/C5).
func TestXAISpeechMapsUnifiedFields(t *testing.T) {
	t.Parallel()
	provider, captured := newMockXAI(t, map[string]http.HandlerFunc{
		"POST /v1/tts": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "audio/wav")
			_, _ = w.Write([]byte("RIFFwavdata"))
		},
	})

	resp, bifrostErr := provider.Speech(xaiTestContext(t), xaiTestKey(), xaiSpeechRequest(&schemas.SpeechParameters{
		VoiceConfig:    &schemas.SpeechVoiceInput{Voice: schemas.Ptr("ara")},
		ResponseFormat: "wav",
		Speed:          schemas.Ptr(1.2),
		LanguageCode:   schemas.Ptr("pt-BR"),
		ExtraParams: map[string]interface{}{
			// The handlers keep these typed fields in the extra params too.
			"language_code":      "pt-BR",
			"sample_rate":        float64(44100),
			"bit_rate":           float64(192000),
			"text_normalization": true,
			"replace":            map[string]interface{}{"xAI": "ex ay eye"},
		},
	}))
	require.Nil(t, bifrostErr)
	require.Equal(t, []byte("RIFFwavdata"), resp.Audio)
	require.Equal(t, "audio/wav", resp.ContentType)
	require.Nil(t, resp.Usage, "xAI reports no usage with plain audio, so none is invented")

	requests := captured()
	require.Len(t, requests, 1)
	require.Equal(t, "Bearer xai-test", requests[0].auth)
	body := gjson.ParseBytes(requests[0].body)
	assert.Equal(t, "Hello world.", body.Get("text").String())
	assert.Equal(t, "ara", body.Get("voice_id").String())
	assert.Equal(t, "pt-BR", body.Get("language").String())
	assert.Equal(t, "wav", body.Get("output_format.codec").String())
	assert.Equal(t, int64(44100), body.Get("output_format.sample_rate").Int())
	assert.Equal(t, int64(192000), body.Get("output_format.bit_rate").Int())
	assert.Equal(t, 1.2, body.Get("speed").Float())
	assert.Equal(t, gjson.True, body.Get("text_normalization").Type)
	assert.Equal(t, "ex ay eye", body.Get("replace.xAI").String())
	for _, absent := range []string{"input", "voice", "response_format", "language_code", "sample_rate", "bit_rate", "with_timestamps", "model"} {
		assert.False(t, body.Get(absent).Exists(), "%s must not reach xAI", absent)
	}
}

// xAI requires language; a request without language_code sends "auto", and one without a
// response_format sends no output_format, so xAI's own defaults apply.
func TestXAISpeechDefaults(t *testing.T) {
	t.Parallel()
	provider, captured := newMockXAI(t, map[string]http.HandlerFunc{
		"POST /v1/tts": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "audio/mpeg")
			_, _ = w.Write([]byte("mp3data"))
		},
	})
	resp, bifrostErr := provider.Speech(xaiTestContext(t), xaiTestKey(), xaiSpeechRequest(&schemas.SpeechParameters{
		VoiceConfig: &schemas.SpeechVoiceInput{Voice: schemas.Ptr("eve")},
	}))
	require.Nil(t, bifrostErr)
	require.Equal(t, "audio/mpeg", resp.ContentType)

	// A sample rate without a response_format needs the codec output_format requires.
	_, bifrostErr = provider.Speech(xaiTestContext(t), xaiTestKey(), xaiSpeechRequest(&schemas.SpeechParameters{
		VoiceConfig: &schemas.SpeechVoiceInput{Voice: schemas.Ptr("eve")},
		ExtraParams: map[string]interface{}{"sample_rate": float64(16000)},
	}))
	require.Nil(t, bifrostErr)

	requests := captured()
	require.Len(t, requests, 2)
	first := gjson.ParseBytes(requests[0].body)
	assert.Equal(t, "auto", first.Get("language").String())
	assert.False(t, first.Get("output_format").Exists())
	assert.False(t, first.Get("speed").Exists())
	second := gjson.ParseBytes(requests[1].body)
	assert.Equal(t, "mp3", second.Get("output_format.codec").String())
	assert.Equal(t, int64(16000), second.Get("output_format.sample_rate").Int())
}

// With with_timestamps xAI answers a JSON envelope. Its base64 audio is decoded and returned
// as bytes, its duration is the unified usage.audio_seconds, its character timings are the
// alignment, and raw_response carries the envelope without the audio.
func TestXAISpeechWithTimestampsEnvelope(t *testing.T) {
	t.Parallel()
	audio := []byte("ID3 mp3 audio bytes")
	envelope := `{"audio":"` + base64.StdEncoding.EncodeToString(audio) + `","content_type":"audio/mpeg","duration":2.6,"audio_timestamps":{"graph_chars":["H","i"],"graph_times":[{"start":0,"end":0.06},[0.06,0.12]]}}`
	provider, captured := newMockXAI(t, map[string]http.HandlerFunc{
		"POST /v1/tts": xaiJSON(http.StatusOK, envelope),
	})
	ctx := xaiTestContext(t)
	ctx.SetValue(schemas.BifrostContextKeyCaptureRawResponse, true)

	resp, bifrostErr := provider.Speech(ctx, xaiTestKey(), xaiSpeechRequest(&schemas.SpeechParameters{
		VoiceConfig:    &schemas.SpeechVoiceInput{Voice: schemas.Ptr("eve")},
		WithTimestamps: schemas.Ptr(true),
		ExtraParams:    map[string]interface{}{"with_timestamps": true},
	}))
	require.Nil(t, bifrostErr)
	require.Equal(t, audio, resp.Audio)
	require.Equal(t, "audio/mpeg", resp.ContentType)
	require.NotNil(t, resp.Usage)
	require.Equal(t, 3, resp.Usage.AudioSeconds)
	require.Equal(t, &schemas.SpeechAlignment{
		Characters:       []string{"H", "i"},
		CharStartTimesMs: []float64{0, 60},
		CharEndTimesMs:   []float64{60, 120},
	}, resp.Alignment)

	raw, err := schemas.Marshal(resp.ExtraFields.RawResponse)
	require.NoError(t, err)
	rawBody := gjson.ParseBytes(raw)
	assert.False(t, rawBody.Get("audio").Exists(), "raw_response must not repeat the audio")
	assert.Equal(t, 2.6, rawBody.Get("duration").Float())
	assert.Equal(t, "audio/mpeg", rawBody.Get("content_type").String())

	requests := captured()
	require.Len(t, requests, 1)
	assert.Equal(t, gjson.True, gjson.GetBytes(requests[0].body, "with_timestamps").Type)
}

// A unified field /v1/tts cannot take is refused with 400 caller_unsupported_parameter and
// no upstream call: instructions and the other undeclared fields by core's check, a codec
// xAI does not produce and a multi-speaker voice by the converter.
func TestXAISpeechRefusesUndeliverableFields(t *testing.T) {
	t.Parallel()
	provider, captured := newMockXAI(t, nil)
	voice := &schemas.SpeechVoiceInput{Voice: schemas.Ptr("eve")}

	declared := func(params *schemas.SpeechParameters) *schemas.BifrostError {
		return schemas.CheckDeclaredUnifiedFields(provider, &schemas.BifrostRequest{
			RequestType: schemas.SpeechRequest, SpeechRequest: xaiSpeechRequest(params),
		})
	}
	requireUnsupportedParameter(t, declared(&schemas.SpeechParameters{VoiceConfig: voice, Instructions: "speak slowly"}), "instructions")
	requireUnsupportedParameter(t, declared(&schemas.SpeechParameters{VoiceConfig: voice, EnableLogging: schemas.Ptr(true)}), "enable_logging")
	require.Nil(t, declared(&schemas.SpeechParameters{
		VoiceConfig: voice, ResponseFormat: "mp3", Speed: schemas.Ptr(1.0), LanguageCode: schemas.Ptr("en"), WithTimestamps: schemas.Ptr(true),
	}))

	_, bifrostErr := provider.Speech(xaiTestContext(t), xaiTestKey(), xaiSpeechRequest(&schemas.SpeechParameters{VoiceConfig: voice, ResponseFormat: "opus"}))
	requireUnsupportedParameter(t, bifrostErr, "response_format")
	_, bifrostErr = provider.Speech(xaiTestContext(t), xaiTestKey(), xaiSpeechRequest(&schemas.SpeechParameters{
		VoiceConfig: &schemas.SpeechVoiceInput{MultiVoiceConfig: []schemas.VoiceConfig{{Speaker: "a", Voice: "eve"}, {Speaker: "b", Voice: "leo"}}},
	}))
	requireUnsupportedParameter(t, bifrostErr, "voice")
	_, bifrostErr = provider.Speech(xaiTestContext(t), xaiTestKey(), xaiSpeechRequest(&schemas.SpeechParameters{
		VoiceConfig: voice, ExtraParams: map[string]interface{}{"sample_rate": 22050.5},
	}))
	requireUnsupportedParameter(t, bifrostErr, "sample_rate")
	require.Empty(t, captured(), "a refused request must not call xAI")
}

// An xAI error keeps its status and its own message and code.
func TestXAISpeechVendorError(t *testing.T) {
	t.Parallel()
	provider, _ := newMockXAI(t, map[string]http.HandlerFunc{
		"POST /v1/tts": xaiJSON(http.StatusNotFound, `{"code":"not_found","error":"unknown voice_id"}`),
	})
	_, bifrostErr := provider.Speech(xaiTestContext(t), xaiTestKey(), xaiSpeechRequest(&schemas.SpeechParameters{
		VoiceConfig: &schemas.SpeechVoiceInput{Voice: schemas.Ptr("nobody")},
	}))
	require.NotNil(t, bifrostErr)
	require.Equal(t, schemas.Ptr(http.StatusNotFound), bifrostErr.StatusCode)
	require.Equal(t, "unknown voice_id", bifrostErr.Error.Message)
	require.Equal(t, schemas.Ptr("not_found"), bifrostErr.Error.Code)
}

// BenchmarkToXAISpeechRequest measures the conversion a speech request pays before it is sent.
func BenchmarkToXAISpeechRequest(b *testing.B) {
	minimal := xaiSpeechRequest(&schemas.SpeechParameters{VoiceConfig: &schemas.SpeechVoiceInput{Voice: schemas.Ptr("eve")}})
	full := xaiSpeechRequest(&schemas.SpeechParameters{
		VoiceConfig: &schemas.SpeechVoiceInput{Voice: schemas.Ptr("ara")}, ResponseFormat: "mp3", Speed: schemas.Ptr(1.2),
		LanguageCode: schemas.Ptr("en"), WithTimestamps: schemas.Ptr(true),
		ExtraParams: map[string]interface{}{"language_code": "en", "with_timestamps": true, "sample_rate": float64(44100), "bit_rate": float64(192000), "text_normalization": true},
	})
	for name, request := range map[string]*schemas.BifrostSpeechRequest{"voice only": minimal, "every unified field": full} {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := xai.ToXAISpeechRequest(request); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkToBifrostSpeechResponse measures the answer conversion for 64 KiB of audio: plain
// bytes are handed on as they are, the with_timestamps envelope is parsed and its base64
// audio decoded once.
func BenchmarkToBifrostSpeechResponse(b *testing.B) {
	audio := []byte(strings.Repeat("a", 64<<10))
	envelope := []byte(`{"audio":"` + base64.StdEncoding.EncodeToString(audio) + `","content_type":"audio/mpeg","duration":2.6,"audio_timestamps":{"graph_chars":["H","i"],"graph_times":[{"start":0,"end":0.06},{"start":0.06,"end":0.12}]}}`)
	b.Run("audio bytes", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, bifrostErr := xai.ToBifrostSpeechResponse("audio/mpeg", audio); bifrostErr != nil {
				b.Fatal(bifrostErr.Error.Message)
			}
		}
	})
	b.Run("timestamps envelope", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, bifrostErr := xai.ToBifrostSpeechResponse("application/json", envelope); bifrostErr != nil {
				b.Fatal(bifrostErr.Error.Message)
			}
		}
	})
}
