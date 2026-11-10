package openai

import (
	"strings"
	"testing"

	"github.com/capsohq/bifrost/core/schemas"
)

// SpeechParameters is shared with ElevenLabs, and its ElevenLabs-only fields used to reach
// OpenAI's /audio/speech body whenever the caller set them.
func TestToOpenAISpeechRequestDropsElevenLabsFields(t *testing.T) {
	params := &schemas.SpeechParameters{
		VoiceConfig:                     &schemas.SpeechVoiceInput{Voice: schemas.Ptr("alloy")},
		ResponseFormat:                  "wav",
		Instructions:                    "calm",
		Speed:                           schemas.Ptr(1.25),
		LanguageCode:                    schemas.Ptr("en"),
		PronunciationDictionaryLocators: []schemas.SpeechPronunciationDictionaryLocator{{PronunciationDictionaryID: "dict"}},
		EnableLogging:                   schemas.Ptr(false),
		OptimizeStreamingLatency:        schemas.Ptr(true),
		WithTimestamps:                  schemas.Ptr(true),
	}
	req := ToOpenAISpeechRequest(&schemas.BifrostSpeechRequest{
		Model:  "gpt-4o-mini-tts",
		Input:  &schemas.SpeechInput{Input: "Hello"},
		Params: params,
	})
	body, err := schemas.MarshalSorted(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{"language_code", "pronunciation_dictionary_locators", "enable_logging", "optimize_streaming_latency", "with_timestamps"} {
		if strings.Contains(string(body), `"`+key+`"`) {
			t.Errorf("OpenAI speech body carries ElevenLabs field %s: %s", key, body)
		}
	}
	for _, want := range []string{`"voice":"alloy"`, `"response_format":"wav"`, `"instructions":"calm"`, `"speed":1.25`, `"input":"Hello"`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("OpenAI speech body lost %s: %s", want, body)
		}
	}
	// The caller's parameters are copied, not changed: a fallback to ElevenLabs still sees them.
	if params.LanguageCode == nil || params.WithTimestamps == nil {
		t.Fatalf("ToOpenAISpeechRequest changed the caller's parameters: %+v", params)
	}
}
