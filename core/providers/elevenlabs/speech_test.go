package elevenlabs

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

// speechBody marshals the converted request the way it is sent upstream and decodes it into a
// plain map, so a test sees exactly which keys the body carries.
func speechBody(t *testing.T, req *schemas.BifrostSpeechRequest) map[string]any {
	t.Helper()
	converted := ToElevenlabsSpeechRequest(req)
	if converted == nil {
		t.Fatal("ToElevenlabsSpeechRequest returned nil")
	}
	raw, err := json.Marshal(converted)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
	return body
}

func speechRequest(params *schemas.SpeechParameters) *schemas.BifrostSpeechRequest {
	return &schemas.BifrostSpeechRequest{
		Provider: schemas.Elevenlabs,
		Model:    "eleven_turbo_v2_5",
		Input:    &schemas.SpeechInput{Input: "hello"},
		Params:   params,
	}
}

// Contract C4 (TXE-6524): voice_settings carries only what the caller gave. Before, the five
// settings were plain values with no omitempty, so a request with only speed also sent
// stability 0, similarity_boost 0, style 0 and use_speaker_boost false, replacing the vendor's
// defaults 0.5, 0.75, 0 and true.
func TestToElevenlabsSpeechRequest_VoiceSettingsCarryOnlyWhatTheCallerGave(t *testing.T) {
	cases := []struct {
		name   string
		params *schemas.SpeechParameters
		want   map[string]any // nil: no voice_settings key at all
	}{
		{"only speed", &schemas.SpeechParameters{Speed: schemas.Ptr(1.2)}, map[string]any{"speed": 1.2}},
		{"only stability", &schemas.SpeechParameters{ExtraParams: map[string]any{"stability": 0.3}}, map[string]any{"stability": 0.3}},
		{"only similarity_boost", &schemas.SpeechParameters{ExtraParams: map[string]any{"similarity_boost": 0.9}}, map[string]any{"similarity_boost": 0.9}},
		{"only style", &schemas.SpeechParameters{ExtraParams: map[string]any{"style": 0.4}}, map[string]any{"style": 0.4}},
		{"only use_speaker_boost", &schemas.SpeechParameters{ExtraParams: map[string]any{"use_speaker_boost": true}}, map[string]any{"use_speaker_boost": true}},
		{
			// A zero or false the caller wrote is the caller's value and is sent.
			"explicit zero and false are sent",
			&schemas.SpeechParameters{Speed: schemas.Ptr(0.0), ExtraParams: map[string]any{"stability": 0.0, "style": 0.0, "use_speaker_boost": false}},
			map[string]any{"speed": 0.0, "stability": 0.0, "style": 0.0, "use_speaker_boost": false},
		},
		{
			"all five",
			&schemas.SpeechParameters{Speed: schemas.Ptr(0.9), ExtraParams: map[string]any{"stability": 0.1, "similarity_boost": 0.2, "style": 0.3, "use_speaker_boost": false}},
			map[string]any{"speed": 0.9, "stability": 0.1, "similarity_boost": 0.2, "style": 0.3, "use_speaker_boost": false},
		},
		{"none", &schemas.SpeechParameters{ExtraParams: map[string]any{"seed": 7}}, nil},
		{"no params", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := speechBody(t, speechRequest(tc.params))
			got, present := body["voice_settings"]
			if tc.want == nil {
				if present {
					t.Fatalf("voice_settings = %v, want the key absent", got)
				}
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("voice_settings = %v, want exactly %v", got, tc.want)
			}
		})
	}
}

// The converter consumes keys from a copy: the caller's ExtraParams keep every key, and keys
// the converter does not know stay in the request's extra params for the passthrough merge.
func TestToElevenlabsSpeechRequest_LeavesCallerExtraParamsWhole(t *testing.T) {
	extra := map[string]any{"stability": 0.3, "seed": 7, "previous_text": "before", "vendor_only_switch": "keep"}
	req := speechRequest(&schemas.SpeechParameters{ExtraParams: extra})

	first := ToElevenlabsSpeechRequest(req)
	second := ToElevenlabsSpeechRequest(req)

	for _, key := range []string{"stability", "seed", "previous_text", "vendor_only_switch"} {
		if _, ok := extra[key]; !ok {
			t.Errorf("caller's ExtraParams lost %q", key)
		}
	}
	for i, converted := range []*ElevenlabsSpeechRequest{first, second} {
		if converted.VoiceSettings == nil || converted.VoiceSettings.Stability == nil || *converted.VoiceSettings.Stability != 0.3 {
			t.Errorf("conversion %d: stability = %+v, want 0.3", i+1, converted.VoiceSettings)
		}
		if converted.Seed == nil || *converted.Seed != 7 {
			t.Errorf("conversion %d: seed = %v, want 7", i+1, converted.Seed)
		}
		if !reflect.DeepEqual(converted.ExtraParams, map[string]any{"vendor_only_switch": "keep"}) {
			t.Errorf("conversion %d: extra params = %v, want only vendor_only_switch", i+1, converted.ExtraParams)
		}
	}
}

// Contract C5 (TXE-6524): vendor_cost is the character-cost header as ElevenLabs sent it, in
// credits, and nothing at all when the header is absent or not a usable number.
func TestCharacterCostFromHeaders(t *testing.T) {
	cases := []struct {
		name   string
		header *string
		want   *schemas.VendorCost
	}{
		{"integer credits", schemas.Ptr("42"), &schemas.VendorCost{Amount: 42, Unit: schemas.VendorCostUnitCredit}},
		{"zero is a report, not a missing value", schemas.Ptr("0"), &schemas.VendorCost{Amount: 0, Unit: schemas.VendorCostUnitCredit}},
		{"fractional credits", schemas.Ptr(" 12.5 "), &schemas.VendorCost{Amount: 12.5, Unit: schemas.VendorCostUnitCredit}},
		{"absent", nil, nil},
		{"empty", schemas.Ptr(""), nil},
		{"not a number", schemas.Ptr("n/a"), nil},
		{"negative", schemas.Ptr("-3"), nil},
		{"NaN", schemas.Ptr("NaN"), nil},
		{"infinite", schemas.Ptr("+Inf"), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var resp fasthttp.Response
			if tc.header != nil {
				resp.Header.Set("character-cost", *tc.header)
			}
			got := characterCostFromHeaders(&resp)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("characterCostFromHeaders = %+v, want %+v", got, tc.want)
			}
			usage := speechUsageFromVendorCost(got)
			if tc.want == nil {
				if usage != nil {
					t.Fatalf("usage = %+v, want nil when the vendor reported no cost", usage)
				}
				return
			}
			// Only vendor_cost is filled: no counted characters, no seconds, no tokens.
			if want := (schemas.SpeechUsage{VendorCost: tc.want}); !reflect.DeepEqual(*usage, want) {
				t.Fatalf("usage = %+v, want only vendor_cost %+v", *usage, *tc.want)
			}
		})
	}
	if got := characterCostFromHeaders(nil); got != nil {
		t.Fatalf("nil response: %+v, want nil", got)
	}
}

// Cost on the request path: one conversion per TTS request. "speed only" is the common shape
// (no extra params, so no map copy); "with settings" pays the copy of the caller's map.
func BenchmarkToElevenlabsSpeechRequest(b *testing.B) {
	b.Run("speed_only", func(b *testing.B) {
		req := speechRequest(&schemas.SpeechParameters{Speed: schemas.Ptr(1.1)})
		b.ReportAllocs()
		for b.Loop() {
			_ = ToElevenlabsSpeechRequest(req)
		}
	})
	b.Run("with_settings", func(b *testing.B) {
		req := speechRequest(&schemas.SpeechParameters{Speed: schemas.Ptr(1.1), ExtraParams: map[string]any{"stability": 0.4, "similarity_boost": 0.8, "seed": 7}})
		b.ReportAllocs()
		for b.Loop() {
			_ = ToElevenlabsSpeechRequest(req)
		}
	})
}

func BenchmarkCharacterCostFromHeaders(b *testing.B) {
	var resp fasthttp.Response
	resp.Header.Set("character-cost", "42")
	b.ReportAllocs()
	for b.Loop() {
		_ = speechUsageFromVendorCost(characterCostFromHeaders(&resp))
	}
}
