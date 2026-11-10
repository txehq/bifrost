package elevenlabs

import (
	"math"
	"testing"

	schemas "github.com/capsohq/bifrost/core/schemas"
)

func TestIsElevenlabsSoundModel(t *testing.T) {
	cases := map[string]bool{
		"eleven_text_to_sound_v2": true,
		"eleven_text_to_sound_v3": true, // future-proof: prefix match
		"eleven_multilingual_v2":  false,
		"eleven_turbo_v2_5":       false,
		"":                        false,
		"scribe_v1":               false,
	}
	for model, want := range cases {
		if got := schemas.IsElevenlabsSoundModel(model); got != want {
			t.Errorf("IsElevenlabsSoundModel(%q) = %v, want %v", model, got, want)
		}
	}
}

func TestToElevenlabsSoundGenerationRequest_NilInput(t *testing.T) {
	if got := ToElevenlabsSoundGenerationRequest(nil, nil); got != nil {
		t.Errorf("expected nil for nil request, got %+v", got)
	}
	if got := ToElevenlabsSoundGenerationRequest(nil, &schemas.BifrostSpeechRequest{}); got != nil {
		t.Errorf("expected nil for request without input, got %+v", got)
	}
}

func TestToElevenlabsSoundGenerationRequest_BasicMapping(t *testing.T) {
	req := &schemas.BifrostSpeechRequest{
		Model: "eleven_text_to_sound_v2",
		Input: &schemas.SpeechInput{Input: "glass shattering"},
	}
	out := ToElevenlabsSoundGenerationRequest(nil, req)
	if out == nil {
		t.Fatal("expected non-nil result")
	}
	if out.Text != "glass shattering" {
		t.Errorf("Text = %q, want %q", out.Text, "glass shattering")
	}
	if out.ModelID != "eleven_text_to_sound_v2" {
		t.Errorf("ModelID = %q, want %q", out.ModelID, "eleven_text_to_sound_v2")
	}
	if out.DurationSeconds != nil || out.Loop != nil || out.PromptInfluence != nil {
		t.Errorf("expected unset optional fields, got duration=%v loop=%v influence=%v", out.DurationSeconds, out.Loop, out.PromptInfluence)
	}
}

func TestToElevenlabsSoundGenerationRequest_ParamsFromExtraParams(t *testing.T) {
	extra := map[string]interface{}{
		"duration_seconds":    float64(2),
		"loop":                true,
		"prompt_influence":    float64(0.7),
		"unknown_passthrough": "keep-me",
	}
	req := &schemas.BifrostSpeechRequest{
		Model:  "eleven_text_to_sound_v2",
		Input:  &schemas.SpeechInput{Input: "thunder"},
		Params: &schemas.SpeechParameters{ExtraParams: extra},
	}
	out := ToElevenlabsSoundGenerationRequest(nil, req)
	if out.DurationSeconds == nil || *out.DurationSeconds != 2 {
		t.Errorf("DurationSeconds = %v, want 2", out.DurationSeconds)
	}
	if out.Loop == nil || *out.Loop != true {
		t.Errorf("Loop = %v, want true", out.Loop)
	}
	if out.PromptInfluence == nil || *out.PromptInfluence != 0.7 {
		t.Errorf("PromptInfluence = %v, want 0.7", out.PromptInfluence)
	}
	// Consumed keys must be removed; unknown keys must remain for passthrough.
	if _, ok := out.ExtraParams["duration_seconds"]; ok {
		t.Error("duration_seconds should be removed from ExtraParams")
	}
	if _, ok := out.ExtraParams["loop"]; ok {
		t.Error("loop should be removed from ExtraParams")
	}
	if _, ok := out.ExtraParams["prompt_influence"]; ok {
		t.Error("prompt_influence should be removed from ExtraParams")
	}
	if _, ok := out.ExtraParams["unknown_passthrough"]; !ok {
		t.Error("unknown_passthrough should be preserved in ExtraParams")
	}
	// The caller's original map must remain intact so fallback/retry reuse keeps
	// the SFX params (regression guard for the in-place mutation bug).
	for _, k := range []string{"duration_seconds", "loop", "prompt_influence"} {
		if _, ok := extra[k]; !ok {
			t.Errorf("caller's ExtraParams[%q] was mutated/removed; must be preserved", k)
		}
	}
}

// Contract C4 (TXE-6524): duration_seconds and prompt_influence reach ElevenLabs as given or
// the request is refused; nothing is clamped into range. Before, 40 seconds was sent as 30 and
// 1.5 as 1 with no error.
func TestCheckSoundGenerationRanges(t *testing.T) {
	cases := []struct {
		name      string
		extra     map[string]interface{}
		wantParam string // "": accepted
	}{
		{"duration 40 above range", map[string]interface{}{"duration_seconds": float64(40)}, "duration_seconds"},
		{"duration 0.1 below range", map[string]interface{}{"duration_seconds": 0.1}, "duration_seconds"},
		{"duration NaN", map[string]interface{}{"duration_seconds": math.NaN()}, "duration_seconds"},
		{"influence 1.5 above range", map[string]interface{}{"prompt_influence": 1.5}, "prompt_influence"},
		{"influence -0.5 below range", map[string]interface{}{"prompt_influence": -0.5}, "prompt_influence"},
		{"both out of range names duration first", map[string]interface{}{"duration_seconds": float64(99), "prompt_influence": float64(5)}, "duration_seconds"},
		{"lower bounds accepted", map[string]interface{}{"duration_seconds": 0.5, "prompt_influence": float64(0)}, ""},
		{"upper bounds accepted", map[string]interface{}{"duration_seconds": float64(30), "prompt_influence": float64(1)}, ""},
		{"within range", map[string]interface{}{"duration_seconds": 12.5, "prompt_influence": 0.3}, ""},
		{"neither given", map[string]interface{}{"loop": true}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := &schemas.BifrostSpeechRequest{
				Model:  "eleven_text_to_sound_v2",
				Input:  &schemas.SpeechInput{Input: "x"},
				Params: &schemas.SpeechParameters{ExtraParams: c.extra},
			}
			bifrostErr := checkSoundGenerationRanges(req)
			if c.wantParam == "" {
				if bifrostErr != nil {
					t.Fatalf("unexpected error: %+v", bifrostErr.Error)
				}
				// An accepted value is sent exactly as given.
				out := ToElevenlabsSoundGenerationRequest(nil, req)
				if v, ok := c.extra["duration_seconds"]; ok && (out.DurationSeconds == nil || *out.DurationSeconds != v) {
					t.Errorf("DurationSeconds = %v, want %v unchanged", out.DurationSeconds, v)
				}
				if v, ok := c.extra["prompt_influence"]; ok && (out.PromptInfluence == nil || *out.PromptInfluence != v) {
					t.Errorf("PromptInfluence = %v, want %v unchanged", out.PromptInfluence, v)
				}
				return
			}
			if bifrostErr == nil {
				t.Fatal("expected caller_unsupported_parameter, got no error")
			}
			if bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != 400 {
				t.Errorf("status = %v, want 400", bifrostErr.StatusCode)
			}
			if bifrostErr.ExtraFields.ErrorType != schemas.ErrorTypeCallerUnsupportedParameter {
				t.Errorf("error_type = %q, want %q", bifrostErr.ExtraFields.ErrorType, schemas.ErrorTypeCallerUnsupportedParameter)
			}
			if bifrostErr.Error == nil || bifrostErr.Error.Param != c.wantParam {
				t.Errorf("error.param = %+v, want %q", bifrostErr.Error, c.wantParam)
			}
		})
	}
	if err := checkSoundGenerationRanges(nil); err != nil {
		t.Errorf("nil request: %+v", err)
	}
	if err := checkSoundGenerationRanges(&schemas.BifrostSpeechRequest{}); err != nil {
		t.Errorf("request without params: %+v", err)
	}
}

// The converter itself never changes a value: an out-of-range one is refused before it runs.
func TestToElevenlabsSoundGenerationRequest_DoesNotClamp(t *testing.T) {
	req := &schemas.BifrostSpeechRequest{
		Model: "eleven_text_to_sound_v2",
		Input: &schemas.SpeechInput{Input: "x"},
		Params: &schemas.SpeechParameters{ExtraParams: map[string]interface{}{
			"duration_seconds": float64(40),
			"prompt_influence": 1.5,
		}},
	}
	out := ToElevenlabsSoundGenerationRequest(nil, req)
	if out.DurationSeconds == nil || *out.DurationSeconds != 40 {
		t.Errorf("DurationSeconds = %v, want 40 as given", out.DurationSeconds)
	}
	if out.PromptInfluence == nil || *out.PromptInfluence != 1.5 {
		t.Errorf("PromptInfluence = %v, want 1.5 as given", out.PromptInfluence)
	}
}

func TestToElevenlabsSoundGenerationRequest_ResolvesAliasModelName(t *testing.T) {
	ctx := schemas.NewBifrostContext(nil, schemas.NoDeadline)
	ctx.SetValue(schemas.BifrostContextKeyResolvedAlias, &schemas.ResolvedAlias{
		Key: "best-sfx",
		Config: &schemas.AliasConfig{
			ModelID:   "best-sfx",
			ModelName: schemas.Ptr("eleven_text_to_sound_v2"),
		},
	})
	req := &schemas.BifrostSpeechRequest{
		Model: "best-sfx",
		Input: &schemas.SpeechInput{Input: "thunder"},
	}
	out := ToElevenlabsSoundGenerationRequest(ctx, req)
	if out == nil {
		t.Fatal("expected non-nil result")
	}
	if out.ModelID != "eleven_text_to_sound_v2" {
		t.Errorf("ModelID = %q, want canonical %q", out.ModelID, "eleven_text_to_sound_v2")
	}
}
