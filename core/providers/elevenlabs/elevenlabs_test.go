package elevenlabs_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/internal/llmtests"
	"github.com/capsohq/bifrost/core/providers/elevenlabs"

	"github.com/capsohq/bifrost/core/schemas"
)

func TestElevenlabs(t *testing.T) {
	t.Parallel()
	if strings.TrimSpace(os.Getenv("ELEVENLABS_API_KEY")) == "" {
		t.Skip("Skipping Elevenlabs tests because ELEVENLABS_API_KEY is not set")
	}

	client, ctx, cancel, err := llmtests.SetupTest()
	if err != nil {
		t.Fatalf("Error initializing test setup: %v", err)
	}
	defer cancel()
	defer client.Shutdown()

	realtimeAgentID := strings.TrimSpace(os.Getenv("ELEVENLABS_AGENT_ID"))
	hasRealtimeAgent := false

	testConfig := llmtests.ComprehensiveTestConfig{
		Provider:             schemas.Elevenlabs,
		SpeechSynthesisModel: "eleven_turbo_v2_5",
		TranscriptionModel:   "scribe_v1",
		RealtimeModel:        realtimeAgentID,
		Scenarios: llmtests.TestScenarios{
			TextCompletion:        false,
			TextCompletionStream:  false,
			SimpleChat:            false,
			CompletionStream:      false,
			MultiTurnConversation: false,
			ToolCalls:             false,
			MultipleToolCalls:     false,
			End2EndToolCalling:    false,
			AutomaticFunctionCall: false,
			ImageURL:              false,
			ImageBase64:           false,
			MultipleImages:        false,
			CompleteEnd2End:       false,
			SpeechSynthesis:       true,
			SpeechSynthesisStream: true,
			Transcription:         true,
			TranscriptionStream:   false,
			Embedding:             false,
			Reasoning:             false,
			ListModels:            false,
			Realtime:              hasRealtimeAgent,
		},
	}

	t.Run("ElevenlabsTests", func(t *testing.T) {
		llmtests.RunAllComprehensiveTests(t, client, ctx, testConfig)
	})
}

// upstreamCall is one request the provider sent to the local ElevenLabs stand-in.
type upstreamCall struct {
	path  string
	query string
	key   string
	body  []byte              // JSON routes
	form  map[string][]string // multipart routes: the non-file fields
	file  string              // multipart routes: the uploaded file's name
}

// newMockElevenlabs starts a local stand-in for api.elevenlabs.io that records every request
// and answers with respond, and returns a provider pointed at it.
func newMockElevenlabs(t *testing.T, respond http.HandlerFunc) (*elevenlabs.ElevenlabsProvider, func() []upstreamCall) {
	t.Helper()
	var mu sync.Mutex
	var calls []upstreamCall
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := upstreamCall{path: r.URL.Path, query: r.URL.RawQuery, key: r.Header.Get("xi-api-key")}
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Errorf("parse upstream form: %v", err)
			}
			call.form = r.MultipartForm.Value
			if files := r.MultipartForm.File["file"]; len(files) > 0 {
				call.file = files[0].Filename
			}
		} else {
			call.body, _ = io.ReadAll(r.Body)
		}
		mu.Lock()
		calls = append(calls, call)
		mu.Unlock()
		respond(w, r)
	}))
	t.Cleanup(server.Close)

	provider := elevenlabs.NewElevenlabsProvider(&schemas.ProviderConfig{
		NetworkConfig: schemas.NetworkConfig{
			BaseURL:                        server.URL,
			DefaultRequestTimeoutInSeconds: 10,
			AllowPrivateNetwork:            true,
		},
		SendBackRawResponse: true,
	}, bifrost.NewDefaultLogger(schemas.LogLevelError))
	return provider, func() []upstreamCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]upstreamCall(nil), calls...)
	}
}

func mockContext(t *testing.T) *schemas.BifrostContext {
	t.Helper()
	ctx, cancel := schemas.NewBifrostContextWithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func mockKey() schemas.Key {
	return schemas.Key{Value: *schemas.NewSecretVar("mock-xi-key"), Models: []string{"*"}}
}

func mockSpeechRequest(model string, params *schemas.SpeechParameters) *schemas.BifrostSpeechRequest {
	if params == nil {
		params = &schemas.SpeechParameters{}
	}
	if params.VoiceConfig == nil {
		params.VoiceConfig = &schemas.SpeechVoiceInput{Voice: schemas.Ptr("voice-1")}
	}
	return &schemas.BifrostSpeechRequest{Provider: schemas.Elevenlabs, Model: model, Input: &schemas.SpeechInput{Input: "hello there"}, Params: params}
}

func mockTranscriptionRequest(extra map[string]interface{}) *schemas.BifrostTranscriptionRequest {
	return &schemas.BifrostTranscriptionRequest{
		Provider: schemas.Elevenlabs,
		Model:    "scribe_v2",
		Input:    &schemas.TranscriptionInput{File: []byte("RIFF\x00\x00\x00\x00WAVE"), Filename: "clip.wav"},
		Params:   &schemas.TranscriptionParameters{ExtraParams: extra},
	}
}

// Contract C4 and C5 for byte-mode TTS (TXE-6524): only the caller's voice setting is sent,
// usage.vendor_cost is the character-cost header in credits, the header itself is still
// forwarded, the provider fills nothing it was not told (no characters, no seconds), and the
// request reaches ElevenLabs exactly once.
func TestElevenlabsSpeechSendsOnlyCallerVoiceSettingsAndReportsVendorCost(t *testing.T) {
	provider, calls := newMockElevenlabs(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Header().Set("character-cost", "11")
		_, _ = w.Write([]byte("ID3-audio-bytes"))
	})

	resp, bifrostErr := provider.Speech(mockContext(t), mockKey(), mockSpeechRequest("eleven_turbo_v2_5", &schemas.SpeechParameters{Speed: schemas.Ptr(1.2)}))
	if bifrostErr != nil {
		t.Fatalf("Speech: %+v", bifrostErr.Error)
	}

	got := calls()
	if len(got) != 1 {
		t.Fatalf("upstream calls = %d, want exactly 1", len(got))
	}
	if got[0].path != "/v1/text-to-speech/voice-1" || got[0].key != "mock-xi-key" {
		t.Fatalf("upstream call = %s with key %q", got[0].path, got[0].key)
	}
	var body map[string]any
	if err := json.Unmarshal(got[0].body, &body); err != nil {
		t.Fatalf("upstream body %s: %v", got[0].body, err)
	}
	if want := map[string]any{"speed": 1.2}; !reflect.DeepEqual(body["voice_settings"], want) {
		t.Fatalf("upstream voice_settings = %v, want exactly %v", body["voice_settings"], want)
	}

	if string(resp.Audio) != "ID3-audio-bytes" {
		t.Fatalf("audio = %q", resp.Audio)
	}
	wantUsage := &schemas.SpeechUsage{VendorCost: &schemas.VendorCost{Amount: 11, Unit: schemas.VendorCostUnitCredit}}
	if !reflect.DeepEqual(resp.Usage, wantUsage) {
		t.Fatalf("usage = %+v, want only vendor_cost {11 credit}", resp.Usage)
	}
	if got := resp.ExtraFields.ProviderResponseHeaders["Character-Cost"]; got != "11" {
		t.Fatalf("forwarded character-cost = %q (headers %v), want 11 unchanged", got, resp.ExtraFields.ProviderResponseHeaders)
	}
}

// Without a character-cost header the provider reports no usage at all.
func TestElevenlabsSpeechWithoutCharacterCostReportsNoUsage(t *testing.T) {
	provider, _ := newMockElevenlabs(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ID3-audio-bytes"))
	})
	resp, bifrostErr := provider.Speech(mockContext(t), mockKey(), mockSpeechRequest("eleven_turbo_v2_5", nil))
	if bifrostErr != nil {
		t.Fatalf("Speech: %+v", bifrostErr.Error)
	}
	if resp.Usage != nil {
		t.Fatalf("usage = %+v, want none", resp.Usage)
	}
}

// Contract C4 (TXE-6524): an out-of-range sound-effect duration or prompt influence is 400
// caller_unsupported_parameter naming the field, and ElevenLabs is never called.
func TestElevenlabsSoundGenerationOutOfRangeMakesNoUpstreamCall(t *testing.T) {
	for param, value := range map[string]float64{"duration_seconds": 40, "prompt_influence": 1.5} {
		t.Run(param, func(t *testing.T) {
			provider, calls := newMockElevenlabs(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte("ID3-sound"))
			})
			req := mockSpeechRequest("eleven_text_to_sound_v2", &schemas.SpeechParameters{ExtraParams: map[string]interface{}{param: value}})
			resp, bifrostErr := provider.Speech(mockContext(t), mockKey(), req)
			if bifrostErr == nil {
				t.Fatalf("expected an error, got response %+v", resp)
			}
			if bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != 400 {
				t.Errorf("status = %v, want 400", bifrostErr.StatusCode)
			}
			if got := bifrostErr.ResolveErrorType(); got != schemas.ErrorTypeCallerUnsupportedParameter {
				t.Errorf("error_type = %q, want caller_unsupported_parameter", got)
			}
			if bifrostErr.Error == nil || bifrostErr.Error.Param != param {
				t.Errorf("error.param = %+v, want %q", bifrostErr.Error, param)
			}
			if n := len(calls()); n != 0 {
				t.Errorf("upstream calls = %d, want none", n)
			}
		})
	}
}

// Contract C5 (TXE-6524): a sound effect's usage carries the vendor's credits and no
// audio_seconds. Before, audio_seconds was the rounded request duration, a request value. The
// duration goes upstream as given, in one call.
func TestElevenlabsSoundGenerationUsageIsOnlyWhatTheVendorReported(t *testing.T) {
	provider, calls := newMockElevenlabs(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("character-cost", "200")
		_, _ = w.Write([]byte("ID3-sound"))
	})
	req := mockSpeechRequest("eleven_text_to_sound_v2", &schemas.SpeechParameters{ExtraParams: map[string]interface{}{"duration_seconds": 4.6, "prompt_influence": 0.3}})
	resp, bifrostErr := provider.Speech(mockContext(t), mockKey(), req)
	if bifrostErr != nil {
		t.Fatalf("Speech: %+v", bifrostErr.Error)
	}
	got := calls()
	if len(got) != 1 || got[0].path != "/v1/sound-generation" {
		t.Fatalf("upstream calls = %+v, want one to /v1/sound-generation", got)
	}
	var body map[string]any
	if err := json.Unmarshal(got[0].body, &body); err != nil {
		t.Fatalf("upstream body %s: %v", got[0].body, err)
	}
	if body["duration_seconds"] != 4.6 || body["prompt_influence"] != 0.3 {
		t.Fatalf("upstream body = %s, want duration_seconds 4.6 and prompt_influence 0.3 as given", got[0].body)
	}
	wantUsage := &schemas.SpeechUsage{VendorCost: &schemas.VendorCost{Amount: 200, Unit: schemas.VendorCostUnitCredit}}
	if !reflect.DeepEqual(resp.Usage, wantUsage) {
		t.Fatalf("usage = %+v, want only vendor_cost {200 credit} and no audio_seconds", resp.Usage)
	}
	if got := resp.ExtraFields.ProviderResponseHeaders["Character-Cost"]; got != "200" {
		t.Fatalf("forwarded character-cost = %q, want 200 unchanged", got)
	}
}

// Contract C4 and C5 for transcription (TXE-6524): the addon fields are in the form ElevenLabs
// receives, usage is the vendor's audio_duration_secs, and there is exactly one upstream call.
func TestElevenlabsTranscriptionDeliversAddonsAndReportsBilledSeconds(t *testing.T) {
	provider, calls := newMockElevenlabs(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"language_code":"en","language_probability":0.98,"text":"hello world","words":[{"text":"hello","type":"word","logprob":-0.1,"start":0.0,"end":0.5}],"transcription_id":"tr_1","audio_duration_secs":12.34}`))
	})
	req := mockTranscriptionRequest(map[string]interface{}{
		"keyterms":             []string{"Bifrost", "Tokun"},
		"transcript_edit":      "true",
		"detect_speaker_roles": "true",
		"diarize":              "true",
	})

	resp, bifrostErr := provider.Transcription(mockContext(t), mockKey(), req)
	if bifrostErr != nil {
		t.Fatalf("Transcription: %+v", bifrostErr.Error)
	}

	got := calls()
	if len(got) != 1 || got[0].path != "/v1/speech-to-text" {
		t.Fatalf("upstream calls = %+v, want one to /v1/speech-to-text", got)
	}
	wantForm := map[string][]string{
		"model_id":             {"scribe_v2"},
		"diarize":              {"true"},
		"keyterms":             {"Bifrost", "Tokun"},
		"transcript_edit":      {"true"},
		"detect_speaker_roles": {"true"},
	}
	if !reflect.DeepEqual(got[0].form, wantForm) || got[0].file != "clip.wav" {
		t.Fatalf("upstream form = %v (file %q)\nwant          %v (file clip.wav)", got[0].form, got[0].file, wantForm)
	}
	if resp.Text != "hello world" {
		t.Fatalf("text = %q", resp.Text)
	}
	if resp.Usage == nil || resp.Usage.Type != "duration" || resp.Usage.Seconds == nil || *resp.Usage.Seconds != 12.34 {
		t.Fatalf("usage = %+v, want {type: duration, seconds: 12.34}", resp.Usage)
	}
}

// Contract C6 (TXE-6524): a vendor transcription error keeps the vendor's status, carries an
// error_type and, when raw responses are on, the vendor's error body. Before, the error left
// Transcription without EnrichError.
func TestElevenlabsTranscriptionVendorErrorKeepsStatusAndCarriesErrorType(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      string
		wantType  schemas.ErrorType
		wantMsg   string
		wantInRaw string
	}{
		{"401 invalid key", http.StatusUnauthorized, `{"detail":{"status":"invalid_api_key","message":"Invalid API key"}}`, schemas.ErrorTypeProviderAuthFailed, "Invalid API key", "invalid_api_key"},
		{"422 validation", http.StatusUnprocessableEntity, `{"detail":[{"loc":["body","model_id"],"msg":"Input should be 'scribe_v1' or 'scribe_v2'","type":"enum"}]}`, schemas.ClassifyErrorType(&schemas.BifrostError{StatusCode: schemas.Ptr(422)}, schemas.TranscriptionRequest), "Input should be 'scribe_v1' or 'scribe_v2' [body.model_id]", "model_id"},
		{"429 rate limit", http.StatusTooManyRequests, `{"detail":{"status":"too_many_concurrent_requests","message":"Too many concurrent requests"}}`, schemas.ErrorTypeProviderRateLimited, "Too many concurrent requests", "too_many_concurrent_requests"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider, calls := newMockElevenlabs(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			_, bifrostErr := provider.Transcription(mockContext(t), mockKey(), mockTranscriptionRequest(nil))
			if bifrostErr == nil {
				t.Fatal("expected the vendor error")
			}
			if bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != tc.status {
				t.Errorf("status = %v, want the vendor's %d", bifrostErr.StatusCode, tc.status)
			}
			if bifrostErr.IsBifrostError {
				t.Error("is_bifrost_error is true for a vendor error")
			}
			if bifrostErr.Error == nil || bifrostErr.Error.Message != tc.wantMsg {
				t.Errorf("message = %+v, want %q", bifrostErr.Error, tc.wantMsg)
			}
			bifrostErr.ExtraFields.RequestType = schemas.TranscriptionRequest
			if got := bifrostErr.ResolveErrorType(); got != tc.wantType || got == "" || got == schemas.ErrorTypeOther {
				t.Errorf("error_type = %q, want %q", got, tc.wantType)
			}
			if tc.wantInRaw != "" {
				raw, _ := json.Marshal(bifrostErr.ExtraFields.RawResponse)
				if !strings.Contains(string(raw), tc.wantInRaw) {
					t.Errorf("raw_response = %s, want the vendor's error body", raw)
				}
			}
			if n := len(calls()); n != 1 {
				t.Errorf("upstream calls = %d, want exactly 1 (no retry)", n)
			}
		})
	}
}

// Contract C6 (TXE-6524): a vendor 200 with no transcript and no error object is 502
// provider_empty_result, not a Bifrost failure. Before, it was a Bifrost error with no status.
func TestElevenlabsTranscriptionEmptyAnswerIsProviderEmptyResult(t *testing.T) {
	for name, body := range map[string]string{"no body": "", "empty object": "{}", "null": "null", "no transcripts": `{"transcripts":[]}`} {
		t.Run(name, func(t *testing.T) {
			provider, calls := newMockElevenlabs(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("request-id", "req-empty")
				_, _ = w.Write([]byte(body))
			})
			ctx := mockContext(t)
			resp, bifrostErr := provider.Transcription(ctx, mockKey(), mockTranscriptionRequest(nil))
			if bifrostErr == nil {
				t.Fatalf("expected provider_empty_result, got %+v", resp)
			}
			if bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != 502 {
				t.Errorf("status = %v, want 502", bifrostErr.StatusCode)
			}
			if got := bifrostErr.ResolveErrorType(); got != schemas.ErrorTypeProviderEmptyResult {
				t.Errorf("error_type = %q, want provider_empty_result", got)
			}
			if bifrostErr.IsBifrostError {
				t.Error("is_bifrost_error is true: the vendor answered, Bifrost did not fail")
			}
			// The vendor's headers stay on the context, where the transport forwards them from.
			headers, _ := ctx.Value(schemas.BifrostContextKeyProviderResponseHeaders).(map[string]string)
			if headers["Request-Id"] != "req-empty" {
				t.Errorf("vendor headers on the context = %v, want request-id kept", headers)
			}
			if n := len(calls()); n != 1 {
				t.Errorf("upstream calls = %d, want exactly 1", n)
			}
		})
	}
}
