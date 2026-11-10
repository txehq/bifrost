package assemblyai_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
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
	"github.com/capsohq/bifrost/core/providers/assemblyai"
	"github.com/capsohq/bifrost/core/providers/gemini"
	"github.com/capsohq/bifrost/core/schemas"
)

// TestAssemblyAI runs the shared live scenarios. It needs a real key and is skipped without one.
func TestAssemblyAI(t *testing.T) {
	t.Parallel()
	if strings.TrimSpace(os.Getenv("ASSEMBLYAI_API_KEY")) == "" {
		t.Skip("Skipping AssemblyAI tests because ASSEMBLYAI_API_KEY is not set")
	}

	client, ctx, cancel, err := llmtests.SetupTest()
	if err != nil {
		t.Fatalf("Error initializing test setup: %v", err)
	}
	defer cancel()
	defer client.Shutdown()

	testConfig := llmtests.ComprehensiveTestConfig{
		Provider:           schemas.AssemblyAI,
		TranscriptionModel: "universal-3-5-pro",
		// AssemblyAI has no text-to-speech: the round-trip cases synthesize their audio with OpenAI.
		ExternalTTSProvider: schemas.OpenAI,
		ExternalTTSModel:    "gpt-4o-mini-tts",
		Scenarios: llmtests.TestScenarios{
			Transcription: true,
		},
	}

	t.Run("AssemblyAITests", func(t *testing.T) {
		llmtests.RunAllComprehensiveTests(t, client, ctx, testConfig)
	})
}

// syncTranscriptBody is a Sync API 200 body in the shape of the vendor's API reference
// (SyncTranscriptResponse): words without timings, as returned when timestamps is off.
const syncTranscriptBody = `{"text":"The quick brown fox.","words":[{"text":"The","confidence":0.99},{"text":"quick","confidence":0.98},{"text":"brown","confidence":0.97},{"text":"fox.","confidence":0.96}],"confidence":0.975,"audio_duration_ms":3312,"session_id":"0d6c1f7e-6a3b-4a55-9f0e-2a4b8f1c9d10","request_time_ms":412.6}`

// syncTimedTranscriptBody is the same response with timestamps on; the vendor omits start
// and end for a word it could not align.
const syncTimedTranscriptBody = `{"text":"The quick fox.","words":[{"text":"The","confidence":0.99,"start":140,"end":260},{"text":"quick","confidence":0.98},{"text":"fox.","confidence":0.96,"start":900,"end":1500}],"confidence":0.97,"audio_duration_ms":1500,"session_id":"0d6c1f7e-6a3b-4a55-9f0e-2a4b8f1c9d11"}`

// wavAudio is a minimal RIFF/WAVE header, enough for the audio part's Content-Type.
var wavAudio = append([]byte("RIFF\x24\x00\x00\x00WAVEfmt "), make([]byte, 24)...)

type capturedRequest struct {
	method string
	path   string
	header http.Header
	body   []byte
}

// newMockAssemblyAI starts a server that records each request and replies with respond.
func newMockAssemblyAI(tb testing.TB, respond func(w http.ResponseWriter)) (*assemblyai.AssemblyAIProvider, func() []capturedRequest) {
	tb.Helper()
	var mu sync.Mutex
	var captured []capturedRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		captured = append(captured, capturedRequest{method: r.Method, path: r.URL.Path, header: r.Header.Clone(), body: body})
		mu.Unlock()
		respond(w)
	}))
	tb.Cleanup(server.Close)

	provider, err := assemblyai.NewAssemblyAIProvider(&schemas.ProviderConfig{
		NetworkConfig: schemas.NetworkConfig{
			BaseURL:                        server.URL,
			DefaultRequestTimeoutInSeconds: 10,
			AllowPrivateNetwork:            true,
		},
	}, bifrost.NewDefaultLogger(schemas.LogLevelError))
	if err != nil {
		tb.Fatalf("NewAssemblyAIProvider: %v", err)
	}
	return provider, func() []capturedRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]capturedRequest(nil), captured...)
	}
}

func respondWith(status int, contentType, body string, headers ...string) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", contentType)
		for i := 0; i+1 < len(headers); i += 2 {
			w.Header().Set(headers[i], headers[i+1])
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

func testContext(tb testing.TB) *schemas.BifrostContext {
	tb.Helper()
	ctx, cancel := schemas.NewBifrostContextWithTimeout(context.Background(), 10*time.Second)
	tb.Cleanup(cancel)
	return ctx
}

func testKey() schemas.Key {
	return schemas.Key{Value: *schemas.NewSecretVar("aai-test-key"), Models: []string{"*"}}
}

func transcriptionRequest(params *schemas.TranscriptionParameters) *schemas.BifrostTranscriptionRequest {
	return &schemas.BifrostTranscriptionRequest{
		Provider: schemas.AssemblyAI,
		Model:    "universal-3-5-pro",
		Input:    &schemas.TranscriptionInput{File: wavAudio, Filename: "sample.wav"},
		Params:   params,
	}
}

type multipartPart struct {
	contentType string
	filename    string
	data        []byte
}

// multipartParts reads the upstream request's parts by form name, in order.
func multipartParts(t *testing.T, req capturedRequest) (map[string]multipartPart, []string) {
	t.Helper()
	mediaType, params, err := mime.ParseMediaType(req.header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" {
		t.Fatalf("content type %q: %v", req.header.Get("Content-Type"), err)
	}
	reader := multipart.NewReader(bytes.NewReader(req.body), params["boundary"])
	parts := map[string]multipartPart{}
	var order []string
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("multipart: %v", err)
		}
		data, _ := io.ReadAll(part)
		parts[part.FormName()] = multipartPart{contentType: part.Header.Get("Content-Type"), filename: part.FileName(), data: data}
		order = append(order, part.FormName())
	}
	return parts, order
}

// configOf decodes the upstream config part, failing when it is absent.
func configOf(t *testing.T, req capturedRequest) map[string]interface{} {
	t.Helper()
	parts, _ := multipartParts(t, req)
	part, ok := parts["config"]
	if !ok {
		t.Fatalf("upstream request has no config part")
	}
	if part.contentType != "application/json" {
		t.Errorf("config part Content-Type = %q, want application/json", part.contentType)
	}
	var config map[string]interface{}
	if err := json.Unmarshal(part.data, &config); err != nil {
		t.Fatalf("config part is not a JSON object: %v: %s", err, part.data)
	}
	return config
}

func TestAssemblyAITranscription(t *testing.T) {
	tests := []struct {
		name        string
		params      *schemas.TranscriptionParameters
		response    string
		wantConfig  map[string]interface{} // nil: no config part
		wantText    string
		wantSeconds float64
		wantWords   []schemas.TranscriptionWord
	}{
		{
			name:        "no request fields sends the audio alone",
			response:    syncTranscriptBody,
			wantText:    "The quick brown fox.",
			wantSeconds: 3.312,
		},
		{
			name:        "language and prompt go in the config part",
			params:      &schemas.TranscriptionParameters{Language: schemas.Ptr("en"), Prompt: schemas.Ptr("A fable."), ResponseFormat: schemas.Ptr("json")},
			response:    syncTranscriptBody,
			wantConfig:  map[string]interface{}{"language_codes": []interface{}{"en"}, "prompt": "A fable."},
			wantText:    "The quick brown fox.",
			wantSeconds: 3.312,
		},
		{
			name:        "word granularity turns timestamps on and maps milliseconds to seconds",
			params:      &schemas.TranscriptionParameters{ResponseFormat: schemas.Ptr("verbose_json"), TimestampGranularities: []string{"word"}},
			response:    syncTimedTranscriptBody,
			wantConfig:  map[string]interface{}{"timestamps": true},
			wantText:    "The quick fox.",
			wantSeconds: 1.5,
			wantWords: []schemas.TranscriptionWord{
				{Word: "The", Start: 0.14, End: 0.26},
				{Word: "quick"}, // not aligned by the vendor: keeps its place with zero times
				{Word: "fox.", Start: 0.9, End: 1.5},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider, captured := newMockAssemblyAI(t, respondWith(http.StatusOK, "application/json", tt.response))

			ctx := testContext(t)
			ctx.SetValue(schemas.BifrostContextKeyCaptureRawResponse, true)
			resp, bifrostErr := provider.Transcription(ctx, testKey(), transcriptionRequest(tt.params))
			if bifrostErr != nil {
				t.Fatalf("Transcription error: %+v", bifrostErr.Error)
			}

			// One request is one upstream call (contract C3).
			reqs := captured()
			if len(reqs) != 1 {
				t.Fatalf("upstream calls = %d, want exactly 1", len(reqs))
			}
			req := reqs[0]
			if req.method != http.MethodPost || req.path != "/v1/transcribe" {
				t.Errorf("upstream request = %s %s, want POST /v1/transcribe", req.method, req.path)
			}
			if got := req.header.Get("X-AAI-Model"); got != "universal-3-5-pro" {
				t.Errorf("X-AAI-Model = %q, want universal-3-5-pro", got)
			}
			if got := req.header.Get("Authorization"); got != "aai-test-key" {
				t.Errorf("Authorization = %q, want the bare key", got)
			}

			parts, order := multipartParts(t, req)
			audio, ok := parts["audio"]
			if !ok || !bytes.Equal(audio.data, wavAudio) {
				t.Fatalf("audio part missing or altered (parts %v)", order)
			}
			if audio.contentType != "audio/wav" || audio.filename != "sample.wav" {
				t.Errorf("audio part = %q %q, want audio/wav sample.wav", audio.contentType, audio.filename)
			}
			if tt.wantConfig == nil {
				if !reflect.DeepEqual(order, []string{"audio"}) {
					t.Errorf("parts = %v, want the audio part alone", order)
				}
			} else {
				if !reflect.DeepEqual(order, []string{"audio", "config"}) {
					t.Errorf("parts = %v, want audio then config", order)
				}
				if got := configOf(t, req); !reflect.DeepEqual(got, tt.wantConfig) {
					t.Errorf("config = %v, want %v", got, tt.wantConfig)
				}
			}

			if resp.Text != tt.wantText {
				t.Errorf("text = %q, want %q", resp.Text, tt.wantText)
			}
			if resp.Usage == nil || resp.Usage.Type != "duration" || resp.Usage.Seconds == nil || *resp.Usage.Seconds != tt.wantSeconds {
				t.Errorf("usage = %+v, want duration %v seconds", resp.Usage, tt.wantSeconds)
			}
			if resp.Duration == nil || *resp.Duration != tt.wantSeconds {
				t.Errorf("duration = %v, want %v", resp.Duration, tt.wantSeconds)
			}
			if !reflect.DeepEqual(resp.Words, tt.wantWords) {
				t.Errorf("words = %+v, want %+v", resp.Words, tt.wantWords)
			}
			raw, err := json.Marshal(resp.ExtraFields.RawResponse)
			if err != nil {
				t.Fatalf("raw response: %v", err)
			}
			var gotRaw, wantRaw interface{}
			_ = json.Unmarshal(raw, &gotRaw)
			_ = json.Unmarshal([]byte(tt.response), &wantRaw)
			if !reflect.DeepEqual(gotRaw, wantRaw) {
				t.Errorf("raw_response = %s, want the vendor body", raw)
			}
		})
	}
}

// The raw response is returned only when asked for, and usage stays empty when the vendor
// reports no duration: Bifrost fills in no quantity of its own (contract C5).
func TestAssemblyAITranscriptionLeavesUnreportedUsageEmpty(t *testing.T) {
	provider, _ := newMockAssemblyAI(t, respondWith(http.StatusOK, "application/json", `{"text":"Hi.","words":[{"text":"Hi.","confidence":0.9}],"confidence":0.9,"session_id":"s"}`))

	resp, bifrostErr := provider.Transcription(testContext(t), testKey(), transcriptionRequest(nil))
	if bifrostErr != nil {
		t.Fatalf("Transcription error: %+v", bifrostErr.Error)
	}
	if resp.Usage != nil || resp.Duration != nil {
		t.Errorf("usage = %+v duration = %v, want both empty", resp.Usage, resp.Duration)
	}
	if resp.Words != nil {
		t.Errorf("words = %+v, want none without timings", resp.Words)
	}
	if resp.ExtraFields.RawResponse != nil {
		t.Errorf("raw_response = %v, want none when not requested", resp.ExtraFields.RawResponse)
	}
}

// Separately priced features travel as extra fields and must reach the vendor's config
// unchanged (contract C4). A multipart form delivers them as strings.
func TestAssemblyAITranscriptionExtraParamsReachConfig(t *testing.T) {
	tests := []struct {
		name       string
		params     *schemas.TranscriptionParameters
		wantConfig map[string]interface{}
	}{
		{
			name: "form fields are sent as the JSON values they spell",
			params: &schemas.TranscriptionParameters{ExtraParams: map[string]interface{}{
				"timestamps":           "true",
				"sample_rate":          "16000",
				"channels":             "1",
				"keyterms_prompt":      []string{"Bifrost", "Tokun"},
				"conversation_context": "42",
				"language_codes":       `["en","es"]`,
				"future_feature":       "enabled",
			}},
			wantConfig: map[string]interface{}{
				"timestamps":           true,
				"sample_rate":          float64(16000),
				"channels":             float64(1),
				"keyterms_prompt":      []interface{}{"Bifrost", "Tokun"},
				"conversation_context": "42",
				"language_codes":       []interface{}{"en", "es"},
				"future_feature":       "enabled",
			},
		},
		{
			name: "typed values from a Go caller are sent as given",
			params: &schemas.TranscriptionParameters{ExtraParams: map[string]interface{}{
				"timestamps":      true,
				"keyterms_prompt": []interface{}{"Bifrost"},
				"nested":          map[string]interface{}{"depth": float64(2)},
			}},
			wantConfig: map[string]interface{}{
				"timestamps":      true,
				"keyterms_prompt": []interface{}{"Bifrost"},
				"nested":          map[string]interface{}{"depth": float64(2)},
			},
		},
		{
			name: "the config field carries a whole config object",
			params: &schemas.TranscriptionParameters{ExtraParams: map[string]interface{}{
				"config": `{"timestamps":true,"conversation_context":["Hello.","true"],"sample_rate":8000}`,
			}},
			wantConfig: map[string]interface{}{
				"timestamps":           true,
				"conversation_context": []interface{}{"Hello.", "true"},
				"sample_rate":          float64(8000),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider, captured := newMockAssemblyAI(t, respondWith(http.StatusOK, "application/json", syncTranscriptBody))

			if _, bifrostErr := provider.Transcription(testContext(t), testKey(), transcriptionRequest(tt.params)); bifrostErr != nil {
				t.Fatalf("Transcription error: %+v", bifrostErr.Error)
			}
			reqs := captured()
			if len(reqs) != 1 {
				t.Fatalf("upstream calls = %d, want exactly 1", len(reqs))
			}
			if got := configOf(t, reqs[0]); !reflect.DeepEqual(got, tt.wantConfig) {
				t.Errorf("config = %v, want %v", got, tt.wantConfig)
			}
		})
	}
}

// An extra field never replaces a config member the unified fields set (the shared
// extra-params rule): the request is refused with a 400 naming the extra field, and nothing
// is sent upstream.
func TestAssemblyAITranscriptionExtraParamNeverReplacesAUnifiedField(t *testing.T) {
	tests := []struct {
		name      string
		params    *schemas.TranscriptionParameters
		wantParam string
	}{
		{
			name: "language_codes beside language",
			params: &schemas.TranscriptionParameters{
				Language:    schemas.Ptr("en"),
				ExtraParams: map[string]interface{}{"language_codes": []string{"en", "fr"}},
			},
			wantParam: "language_codes",
		},
		{
			name: "prompt inside the config field beside prompt",
			params: &schemas.TranscriptionParameters{
				Prompt:      schemas.Ptr("A fable."),
				ExtraParams: map[string]interface{}{"config": `{"prompt":"Another."}`},
			},
			wantParam: "prompt",
		},
		{
			name: "timestamps beside the word granularity",
			params: &schemas.TranscriptionParameters{
				TimestampGranularities: []string{"word"},
				ExtraParams:            map[string]interface{}{"timestamps": "false"},
			},
			wantParam: "timestamps",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider, captured := newMockAssemblyAI(t, respondWith(http.StatusOK, "application/json", syncTranscriptBody))

			_, bifrostErr := provider.Transcription(testContext(t), testKey(), transcriptionRequest(tt.params))
			if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusBadRequest {
				t.Fatalf("expected a 400, got %+v", bifrostErr)
			}
			if bifrostErr.ExtraFields.ErrorType != schemas.ErrorTypeCallerInvalidRequest {
				t.Errorf("error_type = %q, want caller_invalid_request", bifrostErr.ExtraFields.ErrorType)
			}
			if got, _ := bifrostErr.Error.Param.(string); got != tt.wantParam {
				t.Errorf("param = %v, want %q", bifrostErr.Error.Param, tt.wantParam)
			}
			if n := len(captured()); n != 0 {
				t.Errorf("upstream calls = %d, want none", n)
			}
		})
	}
}

// A vendor error comes back with the vendor's status, its detail and title, and the
// error_type for that status, after one call (contract C3, C6).
func TestAssemblyAITranscriptionVendorError(t *testing.T) {
	tests := []struct {
		name          string
		status        int
		body          string
		headers       []string
		wantMessage   string
		wantCode      string
		wantErrorType schemas.ErrorType
		wantRetryMS   int64
	}{
		{
			name:          "bad audio",
			status:        http.StatusBadRequest,
			body:          `{"status":400,"title":"Audio Too Short","detail":"Audio must be at least 80 ms long."}`,
			wantMessage:   "Audio must be at least 80 ms long.",
			wantCode:      "Audio Too Short",
			wantErrorType: schemas.ErrorTypeCallerInvalidRequest,
		},
		{
			name:          "unauthorized",
			status:        http.StatusUnauthorized,
			body:          `{"status":401,"title":"Unauthorized","detail":"Missing or invalid API key."}`,
			wantMessage:   "Missing or invalid API key.",
			wantCode:      "Unauthorized",
			wantErrorType: schemas.ErrorTypeProviderAuthFailed,
		},
		{
			name:          "rate limited with a retry hint",
			status:        http.StatusTooManyRequests,
			body:          `{"status":429,"title":"Too Many Requests","detail":"Rate limit exceeded."}`,
			headers:       []string{"Retry-After", "7"},
			wantMessage:   "Rate limit exceeded.",
			wantCode:      "Too Many Requests",
			wantErrorType: schemas.ErrorTypeProviderRateLimited,
			wantRetryMS:   7000,
		},
		{
			name:          "inference timeout",
			status:        http.StatusGatewayTimeout,
			body:          `{"status":504,"title":"Inference Timeout","detail":"The request exceeded the 30-second deadline."}`,
			wantMessage:   "The request exceeded the 30-second deadline.",
			wantCode:      "Inference Timeout",
			wantErrorType: schemas.ErrorTypeProviderTimeout,
		},
		{
			name:          "title without detail",
			status:        http.StatusServiceUnavailable,
			body:          `{"status":503,"title":"Capacity Exceeded"}`,
			wantMessage:   "Capacity Exceeded",
			wantCode:      "Capacity Exceeded",
			wantErrorType: schemas.ErrorTypeProviderOverloaded,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider, captured := newMockAssemblyAI(t, respondWith(tt.status, "application/problem+json", tt.body, tt.headers...))

			ctx := testContext(t)
			ctx.SetValue(schemas.BifrostContextKeyCaptureRawResponse, true)
			resp, bifrostErr := provider.Transcription(ctx, testKey(), transcriptionRequest(nil))
			if resp != nil || bifrostErr == nil {
				t.Fatalf("expected an error, got response %+v", resp)
			}
			if n := len(captured()); n != 1 {
				t.Errorf("upstream calls = %d, want exactly 1 (no second call on an error)", n)
			}
			if bifrostErr.IsBifrostError {
				t.Errorf("a vendor error must not be marked as Bifrost's own")
			}
			if bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != tt.status {
				t.Errorf("status = %v, want the vendor's %d", bifrostErr.StatusCode, tt.status)
			}
			if bifrostErr.Error == nil || bifrostErr.Error.Message != tt.wantMessage {
				t.Errorf("message = %+v, want %q", bifrostErr.Error, tt.wantMessage)
			}
			if bifrostErr.Error.Code == nil || *bifrostErr.Error.Code != tt.wantCode {
				t.Errorf("code = %v, want %q", bifrostErr.Error.Code, tt.wantCode)
			}
			bifrostErr.ExtraFields.RequestType = schemas.TranscriptionRequest
			if got := bifrostErr.ResolveErrorType(); got != tt.wantErrorType {
				t.Errorf("error_type = %q, want %q", got, tt.wantErrorType)
			}
			if bifrostErr.ExtraFields.RetryAfter != tt.wantRetryMS {
				t.Errorf("retry_after_ms = %d, want %d", bifrostErr.ExtraFields.RetryAfter, tt.wantRetryMS)
			}
			raw, _ := json.Marshal(bifrostErr.ExtraFields.RawResponse)
			var gotRaw, wantRaw interface{}
			_ = json.Unmarshal(raw, &gotRaw)
			_ = json.Unmarshal([]byte(tt.body), &wantRaw)
			if !reflect.DeepEqual(gotRaw, wantRaw) {
				t.Errorf("raw_response = %s, want the vendor's error body", raw)
			}
		})
	}
}

// A unified field the Sync API cannot deliver is refused with caller_unsupported_parameter
// and no upstream call, never dropped (contract C4).
func TestAssemblyAITranscriptionRefusesUndeliverableFields(t *testing.T) {
	tests := []struct {
		name      string
		params    *schemas.TranscriptionParameters
		wantParam string
	}{
		{"subtitle format", &schemas.TranscriptionParameters{ResponseFormat: schemas.Ptr("srt")}, "response_format"},
		{"diarized format", &schemas.TranscriptionParameters{ResponseFormat: schemas.Ptr("diarized_json")}, "response_format"},
		{"segment granularity", &schemas.TranscriptionParameters{TimestampGranularities: []string{"word", "segment"}}, "timestamp_granularities"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider, captured := newMockAssemblyAI(t, respondWith(http.StatusOK, "application/json", syncTranscriptBody))

			_, bifrostErr := provider.Transcription(testContext(t), testKey(), transcriptionRequest(tt.params))
			if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusBadRequest {
				t.Fatalf("expected a 400, got %+v", bifrostErr)
			}
			if bifrostErr.ExtraFields.ErrorType != schemas.ErrorTypeCallerUnsupportedParameter {
				t.Errorf("error_type = %q, want caller_unsupported_parameter", bifrostErr.ExtraFields.ErrorType)
			}
			if got, _ := bifrostErr.Error.Param.(string); got != tt.wantParam {
				t.Errorf("param = %v, want %q", bifrostErr.Error.Param, tt.wantParam)
			}
			if n := len(captured()); n != 0 {
				t.Errorf("upstream calls = %d, want none", n)
			}
		})
	}
}

// The provider declares its unified transcription fields, so core refuses any other before
// choosing a key.
func TestAssemblyAIDeclaresUnifiedTranscriptionFields(t *testing.T) {
	provider, _ := newMockAssemblyAI(t, respondWith(http.StatusOK, "application/json", syncTranscriptBody))

	accepted := transcriptionRequest(&schemas.TranscriptionParameters{
		Language: schemas.Ptr("en"), Prompt: schemas.Ptr("p"), ResponseFormat: schemas.Ptr("json"), TimestampGranularities: []string{"word"}, Format: schemas.Ptr("wav"),
	})
	if bifrostErr := schemas.CheckDeclaredUnifiedFields(provider, &schemas.BifrostRequest{RequestType: schemas.TranscriptionRequest, TranscriptionRequest: accepted}); bifrostErr != nil {
		t.Errorf("supported fields refused: %+v", bifrostErr.Error)
	}

	refused := transcriptionRequest(&schemas.TranscriptionParameters{Temperature: schemas.Ptr(0.2)})
	bifrostErr := schemas.CheckDeclaredUnifiedFields(provider, &schemas.BifrostRequest{RequestType: schemas.TranscriptionRequest, TranscriptionRequest: refused})
	if bifrostErr == nil || bifrostErr.ExtraFields.ErrorType != schemas.ErrorTypeCallerUnsupportedParameter {
		t.Fatalf("temperature must be refused as caller_unsupported_parameter, got %+v", bifrostErr)
	}
	if got, _ := bifrostErr.Error.Param.(string); got != "temperature" {
		t.Errorf("param = %v, want temperature", bifrostErr.Error.Param)
	}

	if _, declared := provider.SupportedUnifiedFields(schemas.ChatCompletionRequest); declared {
		t.Errorf("only transcription is declared")
	}
}

// The Gemini-shaped route (/genai generateContent with inline audio) reaches the provider
// as a transcription request without a filename: the audio is identified by its signature
// and the request's text is the config prompt.
func TestAssemblyAITranscriptionFromGeminiShapedRequest(t *testing.T) {
	provider, captured := newMockAssemblyAI(t, respondWith(http.StatusOK, "application/json", syncTranscriptBody))

	geminiReq := &gemini.GeminiGenerationRequest{
		Model: "assemblyai/universal-3-5-pro",
		Contents: []gemini.Content{{
			Role: "user",
			Parts: []*gemini.Part{
				{Text: "A fable about a fox."},
				{InlineData: &gemini.Blob{MIMEType: "audio/wav", Data: base64.StdEncoding.EncodeToString(wavAudio)}},
			},
		}},
	}
	ctx := testContext(t)
	request, err := geminiReq.ToBifrostTranscriptionRequest(ctx)
	if err != nil {
		t.Fatalf("ToBifrostTranscriptionRequest: %v", err)
	}
	if request.Provider != schemas.AssemblyAI || request.Model != "universal-3-5-pro" {
		t.Fatalf("request routed to %s/%s", request.Provider, request.Model)
	}
	if bifrostErr := schemas.CheckDeclaredUnifiedFields(provider, &schemas.BifrostRequest{RequestType: schemas.TranscriptionRequest, TranscriptionRequest: request}); bifrostErr != nil {
		t.Fatalf("the Gemini-shaped request was refused: %+v", bifrostErr.Error)
	}

	resp, bifrostErr := provider.Transcription(ctx, testKey(), request)
	if bifrostErr != nil {
		t.Fatalf("Transcription error: %+v", bifrostErr.Error)
	}
	if resp.Text != "The quick brown fox." {
		t.Errorf("text = %q", resp.Text)
	}
	reqs := captured()
	if len(reqs) != 1 {
		t.Fatalf("upstream calls = %d, want exactly 1", len(reqs))
	}
	parts, _ := multipartParts(t, reqs[0])
	if got := parts["audio"].contentType; got != "audio/wav" || !bytes.Equal(parts["audio"].data, wavAudio) {
		t.Errorf("audio part = %q (%d bytes), want the inline WAV as audio/wav", got, len(parts["audio"].data))
	}
	if got, want := configOf(t, reqs[0]), map[string]interface{}{"prompt": "A fable about a fox."}; !reflect.DeepEqual(got, want) {
		t.Errorf("config = %v, want %v", got, want)
	}
}

// The audio part's Content-Type must match the format: the Sync API rejects a mismatch.
func TestAssemblyAITranscriptionAudioContentType(t *testing.T) {
	tests := []struct {
		name       string
		audio      []byte
		filename   string
		fileFormat string
		want       string
	}{
		{"raw pcm by file_format", []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06}, "", "pcm", "audio/pcm"},
		{"file_format as a MIME type", []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06}, "", "audio/mp3", "audio/mpeg"},
		{"file_format wins over the extension", []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06}, "clip.mp3", "flac", "audio/flac"},
		{"the signature wins over file_format", wavAudio, "", "mp3", "audio/wav"},
		{"an unknown file_format falls through", []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06}, "clip.ogg", "aiff", "audio/ogg"},
		{"wav by signature", wavAudio, "upload.bin", "", "audio/wav"},
		{"flac by signature", []byte("fLaC\x00\x00\x00\x22rest"), "", "", "audio/flac"},
		{"ogg by signature", []byte("OggS\x00\x02rest-of-page"), "", "", "audio/ogg"},
		{"webm by signature", []byte{0x1A, 0x45, 0xDF, 0xA3, 0x9F, 0x42, 0x86, 0x81}, "", "", "audio/webm"},
		{"m4a by signature", []byte("\x00\x00\x00\x20ftypM4A \x00\x00\x00\x00"), "", "", "audio/mp4"},
		{"mp3 by ID3 tag", []byte("ID3\x04\x00\x00\x00\x00\x00\x00"), "", "", "audio/mpeg"},
		{"mp3 by frame header", []byte{0xFF, 0xFB, 0x90, 0x00, 0x00, 0x00}, "", "", "audio/mpeg"},
		{"aac by ADTS header", []byte{0xFF, 0xF1, 0x50, 0x80, 0x00, 0x00}, "", "", "audio/aac"},
		{"raw pcm by extension", []byte{0xFF, 0xFB, 0x01, 0x02, 0x03, 0x04}, "capture.PCM", "", "audio/pcm"},
		{"unknown bytes are not guessed", []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06}, "blob", "", "application/octet-stream"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider, captured := newMockAssemblyAI(t, respondWith(http.StatusOK, "application/json", syncTranscriptBody))

			request := transcriptionRequest(nil)
			request.Input = &schemas.TranscriptionInput{File: tt.audio, Filename: tt.filename}
			if tt.fileFormat != "" {
				request.Params = &schemas.TranscriptionParameters{Format: schemas.Ptr(tt.fileFormat)}
			}
			if _, bifrostErr := provider.Transcription(testContext(t), testKey(), request); bifrostErr != nil {
				t.Fatalf("Transcription error: %+v", bifrostErr.Error)
			}
			parts, _ := multipartParts(t, captured()[0])
			if got := parts["audio"].contentType; got != tt.want {
				t.Errorf("audio Content-Type = %q, want %q", got, tt.want)
			}
			if !bytes.Equal(parts["audio"].data, tt.audio) {
				t.Errorf("audio bytes were altered")
			}
		})
	}
}

func TestAssemblyAITranscriptionRejectsBadInput(t *testing.T) {
	provider, captured := newMockAssemblyAI(t, respondWith(http.StatusOK, "application/json", syncTranscriptBody))

	noFile := transcriptionRequest(nil)
	noFile.Input = &schemas.TranscriptionInput{}
	badConfig := transcriptionRequest(&schemas.TranscriptionParameters{ExtraParams: map[string]interface{}{"config": "[1,2]"}})
	for name, request := range map[string]*schemas.BifrostTranscriptionRequest{"missing file": noFile, "config that is not an object": badConfig} {
		_, bifrostErr := provider.Transcription(testContext(t), testKey(), request)
		if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: expected a 400, got %+v", name, bifrostErr)
		}
	}
	if n := len(captured()); n != 0 {
		t.Errorf("upstream calls = %d, want none", n)
	}
}

// Every operation other than transcription is unsupported (contract C12).
func TestAssemblyAIUnsupportedOperations(t *testing.T) {
	provider, captured := newMockAssemblyAI(t, respondWith(http.StatusOK, "application/json", syncTranscriptBody))
	ctx := testContext(t)

	errs := map[schemas.RequestType]*schemas.BifrostError{}
	_, errs[schemas.ChatCompletionRequest] = provider.ChatCompletion(ctx, testKey(), &schemas.BifrostChatRequest{})
	_, errs[schemas.SpeechRequest] = provider.Speech(ctx, testKey(), &schemas.BifrostSpeechRequest{})
	_, errs[schemas.TranscriptionStreamRequest] = provider.TranscriptionStream(ctx, nil, nil, testKey(), transcriptionRequest(nil))
	_, errs[schemas.EmbeddingRequest] = provider.Embedding(ctx, testKey(), &schemas.BifrostEmbeddingRequest{})
	_, errs[schemas.ListModelsRequest] = provider.ListModels(ctx, []schemas.Key{testKey()}, &schemas.BifrostListModelsRequest{})
	for requestType, bifrostErr := range errs {
		if bifrostErr == nil || bifrostErr.Error == nil || bifrostErr.Error.Code == nil || *bifrostErr.Error.Code != "unsupported_operation" {
			t.Errorf("%s: expected unsupported_operation, got %+v", requestType, bifrostErr)
		}
	}
	if n := len(captured()); n != 0 {
		t.Errorf("upstream calls = %d, want none", n)
	}
	if provider.GetProviderKey() != schemas.AssemblyAI {
		t.Errorf("provider key = %q", provider.GetProviderKey())
	}
}

// BenchmarkAssemblyAITranscription measures one transcription end to end against a local
// server: the request conversion, the multipart build, the call and the response mapping.
func BenchmarkAssemblyAITranscription(b *testing.B) {
	provider, _ := newMockAssemblyAI(b, respondWith(http.StatusOK, "application/json", syncTimedTranscriptBody))
	request := transcriptionRequest(&schemas.TranscriptionParameters{
		Language:               schemas.Ptr("en"),
		TimestampGranularities: []string{"word"},
		ExtraParams:            map[string]interface{}{"keyterms_prompt": []string{"Bifrost", "Tokun"}},
	})
	key := testKey()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
		if _, bifrostErr := provider.Transcription(ctx, key, request); bifrostErr != nil {
			b.Fatalf("Transcription error: %+v", bifrostErr.Error)
		}
	}
}

// BenchmarkToAssemblyAITranscriptionRequest measures the request conversion alone.
func BenchmarkToAssemblyAITranscriptionRequest(b *testing.B) {
	request := transcriptionRequest(&schemas.TranscriptionParameters{
		Language:               schemas.Ptr("en"),
		TimestampGranularities: []string{"word"},
		ExtraParams:            map[string]interface{}{"keyterms_prompt": []string{"Bifrost", "Tokun"}, "sample_rate": "16000"},
	})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, bifrostErr := assemblyai.ToAssemblyAITranscriptionRequest(request); bifrostErr != nil {
			b.Fatalf("conversion error: %+v", bifrostErr.Error)
		}
	}
}
