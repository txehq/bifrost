package deepgram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	schemas "github.com/capsohq/bifrost/core/schemas"
)

// The response Deepgram's reference documents for POST /v1/listen (the spacewalk sample),
// with the word list cut to two entries.
const deepgramSingleChannelResponse = `{
  "metadata": {
    "transaction_key": "deprecated",
    "request_id": "a847f427-4ad5-4d67-9b95-db801e58251c",
    "sha256": "154e291ecfa8be6ab8343560bcc109008fa7853eb5372533e8efdefc9b504c33",
    "created": "2024-05-12T18:57:13.426Z",
    "duration": 25.933313,
    "channels": 1,
    "models": ["30089e05-99d1-4376-b32e-c263170674af"],
    "model_info": {
      "30089e05-99d1-4376-b32e-c263170674af": {"name": "2-general-nova", "version": "2024-01-09.29447", "arch": "nova-3"}
    }
  },
  "results": {
    "channels": [
      {
        "alternatives": [
          {
            "transcript": "Yeah. As as much as, it's worth celebrating, the first, spacewalk.",
            "confidence": 0.99902344,
            "words": [
              {"word": "yeah", "start": 0.08, "end": 0.32, "confidence": 0.9975586, "punctuated_word": "Yeah."},
              {"word": "as", "start": 0.32, "end": 0.79999995, "confidence": 0.9921875, "punctuated_word": "As"}
            ]
          },
          {"transcript": "a lower ranked alternative", "confidence": 0.5}
        ],
        "detected_language": "en"
      }
    ]
  }
}`

// A stereo file sent with multichannel=true: one result per channel, metadata.channels 2.
const deepgramMultichannelResponse = `{
  "metadata": {
    "request_id": "6f3a2e5c-0b6f-4a0e-9a0d-3d0f1b2c4d5e",
    "created": "2024-05-12T19:02:44.101Z",
    "duration": 30.5,
    "channels": 2,
    "models": ["30089e05-99d1-4376-b32e-c263170674af"]
  },
  "results": {
    "channels": [
      {"alternatives": [{"transcript": "thank you for calling", "confidence": 0.99}]},
      {"alternatives": [{"transcript": "hello i'd like to order", "confidence": 0.98}]}
    ]
  }
}`

// upstreamRequest is what the stand-in for api.deepgram.com received.
type upstreamRequest struct {
	method, path, rawQuery, auth, contentType string
	query                                     map[string][]string
	body                                      []byte
}

// deepgramUpstream answers every request with status and body and records what it got.
type deepgramUpstream struct {
	*httptest.Server
	mu       sync.Mutex
	requests []upstreamRequest
}

func newDeepgramUpstream(t testing.TB, status int, body string) *deepgramUpstream {
	t.Helper()
	upstream := &deepgramUpstream{}
	upstream.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received, _ := io.ReadAll(r.Body)
		upstream.mu.Lock()
		upstream.requests = append(upstream.requests, upstreamRequest{
			method: r.Method, path: r.URL.Path, rawQuery: r.URL.RawQuery, auth: r.Header.Get("Authorization"),
			contentType: r.Header.Get("Content-Type"), query: r.URL.Query(), body: received,
		})
		upstream.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("dg-request-id", "a847f427-4ad5-4d67-9b95-db801e58251c")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

// only returns the single request the upstream received, failing when there was not
// exactly one (contract C3: one request, one upstream call).
func (u *deepgramUpstream) only(t testing.TB) upstreamRequest {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.requests) != 1 {
		t.Fatalf("upstream calls = %d, want exactly one", len(u.requests))
	}
	return u.requests[0]
}

func (u *deepgramUpstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.requests)
}

func newTestDeepgramProvider(t testing.TB, baseURL string) *DeepgramProvider {
	t.Helper()
	provider, err := NewDeepgramProvider(&schemas.ProviderConfig{
		NetworkConfig:       schemas.NetworkConfig{BaseURL: baseURL, DefaultRequestTimeoutInSeconds: 10, AllowPrivateNetwork: true},
		SendBackRawRequest:  true,
		SendBackRawResponse: true,
	}, nil)
	if err != nil {
		t.Fatalf("NewDeepgramProvider: %v", err)
	}
	return provider
}

func deepgramTestKey() schemas.Key {
	return schemas.Key{ID: "deepgram-test", Name: "deepgram-test", Value: *schemas.NewSecretVar("dg-secret"), Models: schemas.WhiteList{"*"}, Weight: 1}
}

func fileRequest(extraParams map[string]interface{}) *schemas.BifrostTranscriptionRequest {
	return &schemas.BifrostTranscriptionRequest{
		Provider: schemas.Deepgram,
		Model:    "nova-3",
		Input:    &schemas.TranscriptionInput{File: []byte("RIFF\x24\x00\x00\x00WAVEfmt fake-wav"), Filename: "call.wav"},
		Params:   &schemas.TranscriptionParameters{ExtraParams: extraParams},
	}
}

func rawResponseMap(t testing.TB, raw interface{}) map[string]interface{} {
	t.Helper()
	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal raw response: %v", err)
	}
	var out map[string]interface{}
	if err := json.Unmarshal(encoded, &out); err != nil {
		t.Fatalf("raw response is not a JSON object: %v (%s)", err, encoded)
	}
	return out
}

// A Nova-3 transcription of a file: the audio bytes are the body, the key goes in
// "Authorization: Token", text is the first alternative, usage is Deepgram's billable
// duration and raw_response carries metadata.request_id (contract C5, C14 layer 1).
func TestTranscriptionFileSingleChannel(t *testing.T) {
	upstream := newDeepgramUpstream(t, http.StatusOK, deepgramSingleChannelResponse)
	provider := newTestDeepgramProvider(t, upstream.URL)
	request := fileRequest(nil)
	request.Params.Language = schemas.Ptr("en")

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	resp, bifrostErr := provider.Transcription(ctx, deepgramTestKey(), request)
	if bifrostErr != nil {
		t.Fatalf("transcription failed: %s", bifrostErr.GetErrorString())
	}

	got := upstream.only(t)
	if got.method != http.MethodPost || got.path != "/v1/listen" {
		t.Fatalf("upstream request = %s %s, want POST /v1/listen", got.method, got.path)
	}
	if got.auth != "Token dg-secret" {
		t.Fatalf("Authorization = %q, want the Token scheme", got.auth)
	}
	if got.rawQuery != "language=en&model=nova-3" {
		t.Fatalf("query = %q, want language=en&model=nova-3", got.rawQuery)
	}
	if got.contentType != "audio/wav" || string(got.body) != string(request.Input.File) {
		t.Fatalf("body = %q (%s), want the audio bytes as audio/wav", got.body, got.contentType)
	}

	if resp.Text != "Yeah. As as much as, it's worth celebrating, the first, spacewalk." {
		t.Fatalf("text = %q, want the first alternative's transcript", resp.Text)
	}
	if resp.Usage == nil || resp.Usage.Type != "duration" || resp.Usage.Seconds == nil || *resp.Usage.Seconds != 25.933313 {
		t.Fatalf("usage = %+v, want {type: duration, seconds: 25.933313}", resp.Usage)
	}
	if resp.Usage.InputTokens != nil || resp.Usage.OutputTokens != nil || resp.Usage.TotalTokens != nil {
		t.Fatalf("usage = %+v: Deepgram reports no tokens, none may be filled in", resp.Usage)
	}
	if resp.Language == nil || *resp.Language != "en" {
		t.Fatalf("language = %v, want the detected language", resp.Language)
	}
	metadata, _ := rawResponseMap(t, resp.ExtraFields.RawResponse)["metadata"].(map[string]interface{})
	if metadata["request_id"] != "a847f427-4ad5-4d67-9b95-db801e58251c" {
		t.Fatalf("raw_response.metadata = %v, want the vendor's request_id", metadata)
	}
	if resp.ExtraFields.ProviderResponseHeaders["Dg-Request-Id"] == "" && resp.ExtraFields.ProviderResponseHeaders["dg-request-id"] == "" {
		t.Fatalf("provider response headers = %v, want the vendor's headers", resp.ExtraFields.ProviderResponseHeaders)
	}
	if resp.ExtraFields.RawRequest != nil {
		t.Fatalf("raw_request = %v: audio bytes are not sent back as a raw request", resp.ExtraFields.RawRequest)
	}
}

// Deepgram bills every channel of a multichannel request for the full duration, so usage
// is metadata.duration × metadata.channels; text joins the channels' transcripts.
func TestTranscriptionMultichannelUsageIsDurationTimesChannels(t *testing.T) {
	upstream := newDeepgramUpstream(t, http.StatusOK, deepgramMultichannelResponse)
	provider := newTestDeepgramProvider(t, upstream.URL)

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	resp, bifrostErr := provider.Transcription(ctx, deepgramTestKey(), fileRequest(map[string]interface{}{"multichannel": "true"}))
	if bifrostErr != nil {
		t.Fatalf("transcription failed: %s", bifrostErr.GetErrorString())
	}
	if got := upstream.only(t).rawQuery; got != "model=nova-3&multichannel=true" {
		t.Fatalf("query = %q, want model=nova-3&multichannel=true", got)
	}
	if resp.Usage == nil || resp.Usage.Type != "duration" || resp.Usage.Seconds == nil || *resp.Usage.Seconds != 61 {
		t.Fatalf("usage = %+v, want {type: duration, seconds: 61} (30.5 × 2 channels)", resp.Usage)
	}
	if resp.Text != "thank you for calling\nhello i'd like to order" {
		t.Fatalf("text = %q, want both channels joined by a newline", resp.Text)
	}
	metadata, _ := rawResponseMap(t, resp.ExtraFields.RawResponse)["metadata"].(map[string]interface{})
	if metadata["channels"] != float64(2) || metadata["duration"] != 30.5 {
		t.Fatalf("raw_response.metadata = %v, want the vendor's duration and channel count kept", metadata)
	}
}

// The options Deepgram prices separately travel as extra params and must reach it as query
// parameters under their own names and values (contract C4), a repeated field once per value.
func TestTranscriptionExtraParamsReachDeepgramAsQueryParameters(t *testing.T) {
	upstream := newDeepgramUpstream(t, http.StatusOK, deepgramSingleChannelResponse)
	provider := newTestDeepgramProvider(t, upstream.URL)

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	_, bifrostErr := provider.Transcription(ctx, deepgramTestKey(), fileRequest(map[string]interface{}{
		// As the multipart route delivers them: one value a string, repeats a []string.
		"diarize":      "true",
		"smart_format": "true",
		"redact":       []string{"pci", "ssn"},
		"keyterm":      []string{"Bifrost gateway", "Tokun"},
		// As a Go SDK caller may write them.
		"utterances": true,
		"utt_split":  0.8,
		"numerals":   json.Number("1"),
		"tag":        []interface{}{"a&b", 7},
	}))
	if bifrostErr != nil {
		t.Fatalf("transcription failed: %s", bifrostErr.GetErrorString())
	}
	got := upstream.only(t)
	want := map[string][]string{
		"model":        {"nova-3"},
		"diarize":      {"true"},
		"smart_format": {"true"},
		"redact":       {"pci", "ssn"},
		"keyterm":      {"Bifrost gateway", "Tokun"},
		"utterances":   {"true"},
		"utt_split":    {"0.8"},
		"numerals":     {"1"},
		"tag":          {"a&b", "7"},
	}
	if !reflect.DeepEqual(got.query, want) {
		t.Fatalf("query = %v\nwant    %v", got.query, want)
	}
	// Keys are sorted, so the same request always produces the same URL.
	if !strings.HasPrefix(got.rawQuery, "diarize=true&keyterm=Bifrost+gateway&keyterm=Tokun&model=nova-3&") {
		t.Fatalf("raw query = %q, want sorted keys with repeated values in order", got.rawQuery)
	}
}

// A request with an audio URL and no file reaches Deepgram as the JSON body {"url": ...}:
// Deepgram fetches it, Bifrost does not (contract C4, agreement four). The url is not also
// sent as a query parameter.
func TestTranscriptionURLInputSendsJSONBody(t *testing.T) {
	const audioURL = "https://dpgr.am/spacewalk.wav?x=1&y=2"
	upstream := newDeepgramUpstream(t, http.StatusOK, deepgramSingleChannelResponse)
	provider := newTestDeepgramProvider(t, upstream.URL)

	request := &schemas.BifrostTranscriptionRequest{
		Provider: schemas.Deepgram,
		Model:    "nova-3",
		Input:    &schemas.TranscriptionInput{},
		Params: &schemas.TranscriptionParameters{ExtraParams: map[string]interface{}{
			schemas.TranscriptionURLParam: audioURL,
			"punctuate":                   "true",
		}},
	}
	if !request.IsURLOnly() || !schemas.AcceptsTranscriptionURLInput(provider) {
		t.Fatal("a URL-only request must be one Deepgram declares it takes")
	}

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	resp, bifrostErr := provider.Transcription(ctx, deepgramTestKey(), request)
	if bifrostErr != nil {
		t.Fatalf("transcription failed: %s", bifrostErr.GetErrorString())
	}
	got := upstream.only(t)
	if got.contentType != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got.contentType)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(got.body, &body); err != nil || len(body) != 1 || body["url"] != audioURL {
		t.Fatalf("body = %s, want exactly {\"url\": %q}", got.body, audioURL)
	}
	if got.rawQuery != "model=nova-3&punctuate=true" {
		t.Fatalf("query = %q, want model=nova-3&punctuate=true (no url)", got.rawQuery)
	}
	if resp.Usage == nil || resp.Usage.Seconds == nil || *resp.Usage.Seconds != 25.933313 {
		t.Fatalf("usage = %+v, want the vendor's duration", resp.Usage)
	}
	if rawRequest := rawResponseMap(t, resp.ExtraFields.RawRequest); rawRequest["url"] != audioURL {
		t.Fatalf("raw_request = %v, want the JSON body sent to Deepgram", rawRequest)
	}
}

// A vendor error keeps the vendor's status, text and code, and classifies into the
// contract's error_type vocabulary (C6). The bodies are the ones Deepgram documents.
func TestTranscriptionVendorErrorKeepsStatusAndErrorType(t *testing.T) {
	for _, tc := range []struct {
		name          string
		status        int
		body          string
		wantMessage   string
		wantCode      string
		wantErrorType schemas.ErrorType
	}{
		{
			name:          "invalid credentials",
			status:        http.StatusUnauthorized,
			body:          `{"err_code":"INVALID_AUTH","err_msg":"Invalid credentials.","request_id":"b8e1c5a2-0000-4000-8000-000000000001"}`,
			wantMessage:   "Invalid credentials.",
			wantCode:      "INVALID_AUTH",
			wantErrorType: schemas.ErrorTypeProviderAuthFailed,
		},
		{
			name:          "out of credits",
			status:        http.StatusPaymentRequired,
			body:          `{"err_code":"ASR_PAYMENT_REQUIRED","err_msg":"Project does not have enough credits for an ASR request and does not have an overage agreement.","request_id":"b8e1c5a2-0000-4000-8000-000000000002"}`,
			wantMessage:   "Project does not have enough credits for an ASR request and does not have an overage agreement.",
			wantCode:      "ASR_PAYMENT_REQUIRED",
			wantErrorType: schemas.ErrorTypeProviderBilling,
		},
		{
			name:          "no access to the model",
			status:        http.StatusForbidden,
			body:          `{"err_code":"INSUFFICIENT_PERMISSIONS","err_msg":"Project does not have access to the requested model.","request_id":"b8e1c5a2-0000-4000-8000-000000000003"}`,
			wantMessage:   "Project does not have access to the requested model.",
			wantCode:      "INSUFFICIENT_PERMISSIONS",
			wantErrorType: schemas.ErrorTypeProviderAuthFailed,
		},
		{
			name:          "corrupt audio",
			status:        http.StatusBadRequest,
			body:          `{"err_code":"Bad Request","err_msg":"Bad Request: failed to process audio: corrupt or unsupported data","request_id":"b8e1c5a2-0000-4000-8000-000000000004"}`,
			wantMessage:   "Bad Request: failed to process audio: corrupt or unsupported data",
			wantCode:      "Bad Request",
			wantErrorType: schemas.ErrorTypeCallerInvalidRequest,
		},
		{
			name:          "unparseable JSON body",
			status:        http.StatusBadRequest,
			body:          `{"category":"INVALID_JSON","message":"Invalid JSON submitted.","details":"Json deserialize error: expected value at line 1 column 1","request_id":"b8e1c5a2-0000-4000-8000-000000000005"}`,
			wantMessage:   "Invalid JSON submitted. Json deserialize error: expected value at line 1 column 1",
			wantCode:      "INVALID_JSON",
			wantErrorType: schemas.ErrorTypeCallerInvalidRequest,
		},
		{
			name:          "rate limited",
			status:        http.StatusTooManyRequests,
			body:          `{"err_code":"TOO_MANY_REQUESTS","err_msg":"Too many requests. Please try again later","request_id":"b8e1c5a2-0000-4000-8000-000000000006"}`,
			wantMessage:   "Too many requests. Please try again later",
			wantCode:      "TOO_MANY_REQUESTS",
			wantErrorType: schemas.ErrorTypeProviderRateLimited,
		},
		{
			name:          "service unavailable",
			status:        http.StatusServiceUnavailable,
			body:          `{"error_code":"Service Unavailable","err_msg":"Please try again later","request_id":"b8e1c5a2-0000-4000-8000-000000000007"}`,
			wantMessage:   "Please try again later",
			wantCode:      "Service Unavailable",
			wantErrorType: schemas.ErrorTypeProviderOverloaded,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := newDeepgramUpstream(t, tc.status, tc.body)
			provider := newTestDeepgramProvider(t, upstream.URL)

			ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
			resp, bifrostErr := provider.Transcription(ctx, deepgramTestKey(), fileRequest(nil))
			if bifrostErr == nil {
				t.Fatalf("a vendor %d must be an error, got %+v", tc.status, resp)
			}
			upstream.only(t)
			if bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != tc.status {
				t.Fatalf("status = %v, want the vendor's %d", bifrostErr.StatusCode, tc.status)
			}
			if bifrostErr.IsBifrostError {
				t.Fatal("a vendor error is not a Bifrost error")
			}
			if bifrostErr.Error == nil || bifrostErr.Error.Message != tc.wantMessage {
				t.Fatalf("message = %+v, want %q", bifrostErr.Error, tc.wantMessage)
			}
			if bifrostErr.Error.Code == nil || *bifrostErr.Error.Code != tc.wantCode {
				t.Fatalf("code = %v, want %q", bifrostErr.Error.Code, tc.wantCode)
			}
			bifrostErr.ExtraFields.RequestType = schemas.TranscriptionRequest
			if got := bifrostErr.ResolveErrorType(); got != tc.wantErrorType {
				t.Fatalf("error_type = %q, want %q", got, tc.wantErrorType)
			}
			if raw := rawResponseMap(t, bifrostErr.ExtraFields.RawResponse); raw["request_id"] == nil {
				t.Fatalf("raw_response = %v, want the vendor's error body", raw)
			}
		})
	}
}

// A quantity Deepgram did not report is not filled in (contract C5): without a duration
// or without a channel count there is no usage, and the transcript still comes back.
func TestTranscriptionUsageEmptyWhenVendorReportsNone(t *testing.T) {
	for name, body := range map[string]string{
		"no metadata": `{"results":{"channels":[{"alternatives":[{"transcript":"hello"}]}]}}`,
		"no duration": `{"metadata":{"request_id":"r1","channels":1},"results":{"channels":[{"alternatives":[{"transcript":"hello"}]}]}}`,
		"no channels": `{"metadata":{"request_id":"r1","duration":12.5},"results":{"channels":[{"alternatives":[{"transcript":"hello"}]}]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			upstream := newDeepgramUpstream(t, http.StatusOK, body)
			provider := newTestDeepgramProvider(t, upstream.URL)
			ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
			resp, bifrostErr := provider.Transcription(ctx, deepgramTestKey(), fileRequest(nil))
			if bifrostErr != nil {
				t.Fatalf("transcription failed: %s", bifrostErr.GetErrorString())
			}
			if resp.Text != "hello" || resp.Usage != nil {
				t.Fatalf("text = %q, usage = %+v; want the transcript and no usage", resp.Text, resp.Usage)
			}
		})
	}

	// Silence is a normal answer: an empty transcript with the billable duration.
	upstream := newDeepgramUpstream(t, http.StatusOK, `{"metadata":{"request_id":"r1","duration":4,"channels":1},"results":{"channels":[{"alternatives":[{"transcript":"","confidence":0}]}]}}`)
	provider := newTestDeepgramProvider(t, upstream.URL)
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	resp, bifrostErr := provider.Transcription(ctx, deepgramTestKey(), fileRequest(nil))
	if bifrostErr != nil {
		t.Fatalf("transcription of silence failed: %s", bifrostErr.GetErrorString())
	}
	if resp.Text != "" || resp.Usage == nil || *resp.Usage.Seconds != 4 {
		t.Fatalf("text = %q, usage = %+v; want an empty transcript billed for 4 seconds", resp.Text, resp.Usage)
	}
}

// A 200 that carries no results (the shape Deepgram answers a callback request with) is
// not a transcript: 502 provider_empty_result, with the vendor's body kept (contract C6).
func TestTranscriptionNoResultsIsProviderEmptyResult(t *testing.T) {
	upstream := newDeepgramUpstream(t, http.StatusOK, `{"request_id":"a847f427-4ad5-4d67-9b95-db801e58251c"}`)
	provider := newTestDeepgramProvider(t, upstream.URL)

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	resp, bifrostErr := provider.Transcription(ctx, deepgramTestKey(), fileRequest(nil))
	if bifrostErr == nil {
		t.Fatalf("a 200 without results must be an error, got %+v", resp)
	}
	if bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusBadGateway || bifrostErr.ExtraFields.ErrorType != schemas.ErrorTypeProviderEmptyResult {
		t.Fatalf("error = %v / %q, want 502 provider_empty_result", bifrostErr.StatusCode, bifrostErr.ExtraFields.ErrorType)
	}
	if raw := rawResponseMap(t, bifrostErr.ExtraFields.RawResponse); raw["request_id"] != "a847f427-4ad5-4d67-9b95-db801e58251c" {
		t.Fatalf("raw_response = %v, want the vendor's body", raw)
	}
}

// What Bifrost cannot deliver to Deepgram is refused with 400 caller_unsupported_parameter
// naming the field, and nothing is sent (contract C4, agreement three).
func TestTranscriptionRefusesWhatItCannotDeliver(t *testing.T) {
	upstream := newDeepgramUpstream(t, http.StatusOK, deepgramSingleChannelResponse)
	provider := newTestDeepgramProvider(t, upstream.URL)

	for _, tc := range []struct {
		name      string
		mutate    func(*schemas.BifrostTranscriptionRequest)
		wantParam string
	}{
		{"response_format text", func(r *schemas.BifrostTranscriptionRequest) { r.Params.ResponseFormat = schemas.Ptr("text") }, "response_format"},
		{"response_format verbose_json", func(r *schemas.BifrostTranscriptionRequest) { r.Params.ResponseFormat = schemas.Ptr("verbose_json") }, "response_format"},
		{"callback", func(r *schemas.BifrostTranscriptionRequest) {
			r.Params.ExtraParams["callback"] = "https://example.com/hook"
		}, "callback"},
		{"callback_method", func(r *schemas.BifrostTranscriptionRequest) { r.Params.ExtraParams["callback_method"] = "PUT" }, "callback_method"},
		{"model as an extra param", func(r *schemas.BifrostTranscriptionRequest) { r.Params.ExtraParams["model"] = "nova-2" }, "model"},
		{"language as an extra param", func(r *schemas.BifrostTranscriptionRequest) { r.Params.ExtraParams["language"] = "fr" }, "language"},
		{"file and url", func(r *schemas.BifrostTranscriptionRequest) {
			r.Params.ExtraParams[schemas.TranscriptionURLParam] = "https://example.com/a.wav"
		}, "url"},
		{"object value", func(r *schemas.BifrostTranscriptionRequest) {
			r.Params.ExtraParams["extra"] = map[string]interface{}{"k": "v"}
		}, "extra"},
		{"nested list value", func(r *schemas.BifrostTranscriptionRequest) {
			r.Params.ExtraParams["search"] = []interface{}{[]interface{}{"a"}}
		}, "search"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := fileRequest(map[string]interface{}{})
			tc.mutate(request)
			ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
			_, bifrostErr := provider.Transcription(ctx, deepgramTestKey(), request)
			if bifrostErr == nil {
				t.Fatal("the request must be refused")
			}
			if bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusBadRequest ||
				bifrostErr.ExtraFields.ErrorType != schemas.ErrorTypeCallerUnsupportedParameter ||
				bifrostErr.Error == nil || bifrostErr.Error.Param != tc.wantParam {
				t.Fatalf("error = %v / %+v / %+v, want 400 caller_unsupported_parameter naming %q", bifrostErr.StatusCode, bifrostErr.ExtraFields, bifrostErr.Error, tc.wantParam)
			}
		})
	}
	if got := upstream.count(); got != 0 {
		t.Fatalf("upstream calls = %d, want none for refused requests", got)
	}

	// response_format json is what Deepgram answers anyway; it is accepted and not sent.
	request := fileRequest(nil)
	request.Params.ResponseFormat = schemas.Ptr("json")
	request.Params.Prompt = schemas.Ptr("names: Tokun")
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	if _, bifrostErr := provider.Transcription(ctx, deepgramTestKey(), request); bifrostErr != nil {
		t.Fatalf("response_format json failed: %s", bifrostErr.GetErrorString())
	}
	if got := upstream.only(t).rawQuery; got != "model=nova-3" {
		t.Fatalf("query = %q, want model=nova-3 (response_format and prompt are not Deepgram parameters)", got)
	}
}

// The declaration core checks before a key is chosen: language and response_format are
// taken, prompt is exempt, and every other unified transcription field is refused.
func TestSupportedUnifiedFieldsRefusesUndeliverableTranscriptionFields(t *testing.T) {
	provider := newTestDeepgramProvider(t, "http://127.0.0.1:1")
	if fields, declared := provider.SupportedUnifiedFields(schemas.TranscriptionRequest); !declared || !slices.Equal(fields, []string{"language", "response_format"}) {
		t.Fatalf("declaration = %v (%v), want [language response_format]", fields, declared)
	}
	if _, declared := provider.SupportedUnifiedFields(schemas.SpeechRequest); declared {
		t.Fatal("an unsupported operation declares nothing")
	}

	check := func(params *schemas.TranscriptionParameters) *schemas.BifrostError {
		return schemas.CheckDeclaredUnifiedFields(provider, &schemas.BifrostRequest{
			RequestType:          schemas.TranscriptionRequest,
			TranscriptionRequest: &schemas.BifrostTranscriptionRequest{Provider: schemas.Deepgram, Model: "nova-3", Params: params},
		})
	}
	if err := check(&schemas.TranscriptionParameters{
		Language: schemas.Ptr("en"), ResponseFormat: schemas.Ptr("json"), Prompt: schemas.Ptr("names"),
		ExtraParams: map[string]interface{}{"diarize": "true"},
	}); err != nil {
		t.Fatalf("a deliverable request was refused: %s", err.GetErrorString())
	}
	for param, params := range map[string]*schemas.TranscriptionParameters{
		"timestamp_granularities": {TimestampGranularities: []string{"word"}},
		"temperature":             {Temperature: schemas.Ptr(0.2)},
		"include":                 {Include: []string{"logprobs"}},
	} {
		err := check(params)
		if err == nil || err.Error.Param != param || err.ExtraFields.ErrorType != schemas.ErrorTypeCallerUnsupportedParameter {
			t.Fatalf("%s: error = %+v, want caller_unsupported_parameter naming it", param, err)
		}
	}
}

// Deepgram serves transcription only; every other operation answers unsupported (C12).
func TestOtherOperationsAreUnsupported(t *testing.T) {
	provider := newTestDeepgramProvider(t, "http://127.0.0.1:1")
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	key := deepgramTestKey()

	_, chatErr := provider.ChatCompletion(ctx, key, &schemas.BifrostChatRequest{})
	_, speechErr := provider.Speech(ctx, key, &schemas.BifrostSpeechRequest{})
	_, streamErr := provider.TranscriptionStream(ctx, nil, nil, key, &schemas.BifrostTranscriptionRequest{})
	_, listErr := provider.ListModels(ctx, []schemas.Key{key}, &schemas.BifrostListModelsRequest{})
	_, embeddingErr := provider.Embedding(ctx, key, &schemas.BifrostEmbeddingRequest{})
	for name, err := range map[string]*schemas.BifrostError{
		"chat": chatErr, "speech": speechErr, "transcription stream": streamErr, "list models": listErr, "embedding": embeddingErr,
	} {
		if err == nil || err.Error == nil || err.Error.Code == nil || *err.Error.Code != "unsupported_operation" {
			t.Fatalf("%s: error = %+v, want unsupported_operation", name, err)
		}
	}
}

func TestDeepgramAudioContentType(t *testing.T) {
	for _, tc := range []struct {
		filename string
		audio    []byte
		want     string
	}{
		{"meeting.M4A", []byte("\x00\x00\x00\x20ftypM4A "), "audio/mp4"},
		{"clip.webm", []byte("\x1a\x45\xdf\xa3"), "audio/webm"},
		{"clip.opus", []byte("OggS"), "audio/opus"},
		{"", []byte("fLaC\x00\x00\x00\x22"), "audio/flac"},
		{"upload.bin", []byte("RIFF\x24\x00\x00\x00WAVEfmt "), "audio/wav"},
		{"", []byte("ID3\x04"), "audio/mp3"},
	} {
		if got := deepgramAudioContentType(tc.filename, tc.audio); got != tc.want {
			t.Errorf("content type of %q = %q, want %q", tc.filename, got, tc.want)
		}
	}
}

// BenchmarkToDeepgramTranscriptionRequest measures the request conversion, the only work
// Bifrost adds before the upstream call. The audio is not copied: the body aliases the
// caller's slice.
func BenchmarkToDeepgramTranscriptionRequest(b *testing.B) {
	audio := make([]byte, 1<<20)
	copy(audio, "RIFF\x24\x00\x00\x00WAVEfmt ")
	for _, tc := range []struct {
		name    string
		request *schemas.BifrostTranscriptionRequest
	}{
		{name: "file", request: &schemas.BifrostTranscriptionRequest{
			Model: "nova-3", Input: &schemas.TranscriptionInput{File: audio, Filename: "call.wav"},
			Params: &schemas.TranscriptionParameters{ExtraParams: map[string]interface{}{}},
		}},
		{name: "file_with_options", request: &schemas.BifrostTranscriptionRequest{
			Model: "nova-3", Input: &schemas.TranscriptionInput{File: audio, Filename: "call.wav"},
			Params: &schemas.TranscriptionParameters{Language: schemas.Ptr("en"), ExtraParams: map[string]interface{}{
				"diarize": "true", "smart_format": "true", "multichannel": "true", "keyterm": []string{"Bifrost", "Tokun"},
			}},
		}},
		{name: "url", request: &schemas.BifrostTranscriptionRequest{
			Model: "nova-3", Input: &schemas.TranscriptionInput{},
			Params: &schemas.TranscriptionParameters{ExtraParams: map[string]interface{}{
				schemas.TranscriptionURLParam: "https://dpgr.am/spacewalk.wav",
			}},
		}},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := ToDeepgramTranscriptionRequest(tc.request); err != nil {
					b.Fatalf("conversion failed: %s", err.GetErrorString())
				}
			}
		})
	}
}

// BenchmarkToBifrostTranscriptionResponse measures the response conversion after the
// vendor's body is decoded.
func BenchmarkToBifrostTranscriptionResponse(b *testing.B) {
	for name, body := range map[string]string{"single_channel": deepgramSingleChannelResponse, "multichannel": deepgramMultichannelResponse} {
		var decoded DeepgramTranscriptionResponse
		if err := json.Unmarshal([]byte(body), &decoded); err != nil {
			b.Fatalf("decode sample: %v", err)
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if ToBifrostTranscriptionResponse(&decoded) == nil {
					b.Fatal("no response")
				}
			}
		})
	}
}
