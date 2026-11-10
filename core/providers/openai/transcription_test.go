package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/bytedance/sonic"
	"github.com/capsohq/bifrost/core/schemas"
)

// testLogger is a minimal no-op logger implementation for testing.
type testLogger struct{}

func (l *testLogger) Debug(msg string, args ...any)                     {}
func (l *testLogger) Info(msg string, args ...any)                      {}
func (l *testLogger) Warn(msg string, args ...any)                      {}
func (l *testLogger) Error(msg string, args ...any)                     {}
func (l *testLogger) Fatal(msg string, args ...any)                     {}
func (l *testLogger) SetLevel(level schemas.LogLevel)                   {}
func (l *testLogger) SetOutputType(outputType schemas.LoggerOutputType) {}
func (l *testLogger) LogHTTPRequest(level schemas.LogLevel, msg string) schemas.LogEventBuilder {
	return schemas.NoopLogEvent
}

// TestTranscription_DiarizedJSON_StringSegmentID reproduces
// https://github.com/capsohq/bifrost/issues/5002: OpenAI's
// response_format=diarized_json (used by gpt-4o-transcribe-diarize) returns
// segments with a string id (e.g. "seg_154") and speaker/type fields, which
// don't fit TranscriptionSegment's int id. Previously this crashed the whole
// request with a JSON type-mismatch error; it should now decode cleanly into
// DiarizedSegments.
func TestTranscription_DiarizedJSON_StringSegmentID(t *testing.T) {
	responseBody := `{
		"task": "transcribe",
		"text": "Hello there, how are you?",
		"duration": 521.5,
		"segments": [
			{"id": "seg_153", "type": "transcript.text.segment", "speaker": "A", "start": 519.0, "end": 520.5, "text": "Hello there,"},
			{"id": "seg_154", "type": "transcript.text.segment", "speaker": "B", "start": 520.968, "end": 522.0, "text": "how are you?"}
		],
		"usage": {"type": "duration", "seconds": 521.5}
	}`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(responseBody))
	}))
	defer server.Close()

	provider := NewOpenAIProvider(&schemas.ProviderConfig{
		NetworkConfig: schemas.NetworkConfig{
			BaseURL:                        server.URL,
			DefaultRequestTimeoutInSeconds: 30,
		},
	}, &testLogger{})

	responseFormat := "diarized_json"
	request := &schemas.BifrostTranscriptionRequest{
		Model: "gpt-4o-transcribe-diarize",
		Input: &schemas.TranscriptionInput{
			File:     []byte("fake-audio-bytes"),
			Filename: "sample.mp3",
		},
		Params: &schemas.TranscriptionParameters{
			ResponseFormat: &responseFormat,
		},
	}

	ctx, cancel := schemas.NewBifrostContextWithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, bifrostErr := provider.Transcription(ctx, schemas.Key{Value: *schemas.NewSecretVar("test-key")}, request)
	if bifrostErr != nil {
		t.Fatalf("expected no error, got: %v", bifrostErr.Error.Message)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}
	if resp.Text != "Hello there, how are you?" {
		t.Fatalf("unexpected text: %q", resp.Text)
	}
	if len(resp.DiarizedSegments) != 2 {
		t.Fatalf("expected 2 diarized segments, got %d", len(resp.DiarizedSegments))
	}
	if resp.DiarizedSegments[1].ID != "seg_154" {
		t.Fatalf("expected string id %q, got %q", "seg_154", resp.DiarizedSegments[1].ID)
	}
	if resp.DiarizedSegments[1].Speaker != "B" {
		t.Fatalf("expected speaker %q, got %q", "B", resp.DiarizedSegments[1].Speaker)
	}
	if len(resp.Segments) != 0 {
		t.Fatalf("expected verbose-json Segments to stay empty, got %d", len(resp.Segments))
	}
	if resp.Usage == nil || resp.Usage.Seconds == nil || *resp.Usage.Seconds != 521.5 {
		t.Fatalf("expected fractional usage.seconds=521.5, got %+v", resp.Usage)
	}

	// Confirm the typed (non-raw) response marshals segments back under the
	// shared "segments" key with the diarized shape intact.
	marshaled, err := sonic.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}
	var roundTrip map[string]interface{}
	if err := sonic.Unmarshal(marshaled, &roundTrip); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	segments, ok := roundTrip["segments"].([]interface{})
	if !ok || len(segments) != 2 {
		t.Fatalf("expected 2 segments under \"segments\" key, got %+v", roundTrip["segments"])
	}
	firstSeg, ok := segments[0].(map[string]interface{})
	if !ok || firstSeg["id"] != "seg_153" {
		t.Fatalf("expected string id %q in marshaled segment, got %+v", "seg_153", firstSeg)
	}
}

// TestTranscription_DiarizedJSON_OmitsAbsentDurationAndTask reflects OpenAI's
// actual (as observed live) diarized_json wire response, which omits
// top-level "duration" and "task" entirely - despite the openai-python SDK's
// TranscriptionDiarized model listing both as required. Bifrost must not
// fabricate "duration":0 / "task":"" for fields the upstream never sent.
func TestTranscription_DiarizedJSON_OmitsAbsentDurationAndTask(t *testing.T) {
	responseBody := `{
		"text": "Hello, this is a sample test.",
		"segments": [
			{"type": "transcript.text.segment", "text": " Hello,", "speaker": "A", "start": 0.0, "end": 0.3, "id": "seg_0"}
		],
		"usage": {"type": "tokens", "total_tokens": 100, "input_tokens": 20, "output_tokens": 80}
	}`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(responseBody))
	}))
	defer server.Close()

	provider := NewOpenAIProvider(&schemas.ProviderConfig{
		NetworkConfig: schemas.NetworkConfig{
			BaseURL:                        server.URL,
			DefaultRequestTimeoutInSeconds: 30,
		},
	}, &testLogger{})

	responseFormat := "diarized_json"
	request := &schemas.BifrostTranscriptionRequest{
		Model: "gpt-4o-transcribe-diarize",
		Input: &schemas.TranscriptionInput{
			File:     []byte("fake-audio-bytes"),
			Filename: "sample.mp3",
		},
		Params: &schemas.TranscriptionParameters{
			ResponseFormat: &responseFormat,
		},
	}

	ctx, cancel := schemas.NewBifrostContextWithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, bifrostErr := provider.Transcription(ctx, schemas.Key{Value: *schemas.NewSecretVar("test-key")}, request)
	if bifrostErr != nil {
		t.Fatalf("expected no error, got: %v", bifrostErr.Error.Message)
	}
	if resp.Duration != nil {
		t.Fatalf("expected Duration to stay nil (absent upstream), got %v", *resp.Duration)
	}
	if resp.Task != nil {
		t.Fatalf("expected Task to stay nil (absent upstream), got %q", *resp.Task)
	}

	marshaled, err := sonic.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}
	var roundTrip map[string]interface{}
	if err := sonic.Unmarshal(marshaled, &roundTrip); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if _, present := roundTrip["duration"]; present {
		t.Fatalf(`expected "duration" key to be omitted, got: %v`, roundTrip["duration"])
	}
	if _, present := roundTrip["task"]; present {
		t.Fatalf(`expected "task" key to be omitted, got: %v`, roundTrip["task"])
	}
}

// TestTranscription_VerboseJSON_StillWorks ensures the pre-existing
// verbose_json/int-id path is unaffected by the diarized_json branch added
// alongside it.
func TestTranscription_VerboseJSON_StillWorks(t *testing.T) {
	responseBody := `{
		"task": "transcribe",
		"text": "Hello there.",
		"segments": [
			{"id": 0, "seek": 0, "start": 0.0, "end": 1.5, "text": "Hello there.", "tokens": [1,2,3], "temperature": 0.0, "avg_logprob": -0.1, "compression_ratio": 1.2, "no_speech_prob": 0.01}
		]
	}`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(responseBody))
	}))
	defer server.Close()

	provider := NewOpenAIProvider(&schemas.ProviderConfig{
		NetworkConfig: schemas.NetworkConfig{
			BaseURL:                        server.URL,
			DefaultRequestTimeoutInSeconds: 30,
		},
	}, &testLogger{})

	responseFormat := "verbose_json"
	request := &schemas.BifrostTranscriptionRequest{
		Model: "whisper-1",
		Input: &schemas.TranscriptionInput{
			File:     []byte("fake-audio-bytes"),
			Filename: "sample.mp3",
		},
		Params: &schemas.TranscriptionParameters{
			ResponseFormat: &responseFormat,
		},
	}

	ctx, cancel := schemas.NewBifrostContextWithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, bifrostErr := provider.Transcription(ctx, schemas.Key{Value: *schemas.NewSecretVar("test-key")}, request)
	if bifrostErr != nil {
		t.Fatalf("expected no error, got: %v", bifrostErr.Error.Message)
	}
	if len(resp.Segments) != 1 || resp.Segments[0].ID != 0 {
		t.Fatalf("expected 1 verbose-json segment with int id 0, got %+v", resp.Segments)
	}
	if len(resp.DiarizedSegments) != 0 {
		t.Fatalf("expected DiarizedSegments to stay empty, got %d", len(resp.DiarizedSegments))
	}
}

func multipartPartOrder(t *testing.T, contentType string, body []byte) []string {
	t.Helper()
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		t.Fatalf("ParseMediaType(%q): %v", contentType, err)
	}
	boundary := params["boundary"]
	if boundary == "" {
		t.Fatalf("missing boundary in %q", contentType)
	}

	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	var order []string
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("NextPart(): %v", err)
		}
		order = append(order, part.FormName())
		_, _ = io.Copy(io.Discard, part)
		_ = part.Close()
	}
	return order
}

func TestParseTranscriptionFormDataBodyFromRequest_OrdersMetadataBeforeFile(t *testing.T) {
	language := "en"
	prompt := "transcribe this"
	responseFormat := "verbose_json"
	temperature := 0.2
	stream := true

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	req := &OpenAITranscriptionRequest{
		Model:    "whisper-1",
		File:     []byte("audio-bytes"),
		Filename: "sample.mp3",
		TranscriptionParameters: schemas.TranscriptionParameters{
			Language:               &language,
			Prompt:                 &prompt,
			ResponseFormat:         &responseFormat,
			Temperature:            &temperature,
			TimestampGranularities: []string{"word"},
			Include:                []string{"logprobs"},
		},
		Stream: &stream,
	}

	if bifrostErr := ParseTranscriptionFormDataBodyFromRequest(writer, req, schemas.OpenAI); bifrostErr != nil {
		t.Fatalf("unexpected bifrost error: %v", bifrostErr.Error.Message)
	}

	order := multipartPartOrder(t, writer.FormDataContentType(), body.Bytes())
	if len(order) == 0 {
		t.Fatal("expected multipart parts to be written")
	}
	if order[len(order)-1] != "file" {
		t.Fatalf("expected file part last, got order %v", order)
	}
	if order[0] != "model" {
		t.Fatalf("expected model part first, got order %v", order)
	}
}

// multipartFieldValue returns the value of the first multipart part with the
// given name, or "" if absent.
func multipartFieldValue(t *testing.T, contentType string, body []byte, name string) string {
	t.Helper()
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		t.Fatalf("ParseMediaType(%q): %v", contentType, err)
	}
	boundary := params["boundary"]
	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("NextPart(): %v", err)
		}
		if part.FormName() == name {
			data, _ := io.ReadAll(part)
			_ = part.Close()
			return string(data)
		}
		_, _ = io.Copy(io.Discard, part)
		_ = part.Close()
	}
	return ""
}

// TestTranscriptionRequest_PreservesFilenameEndToEnd covers issue #5670: the
// ingress conversion must carry the client's filename into the Bifrost request
// so the outbound multipart form labels the audio with its real container.
// M4A/MP4/webm bytes are not magic-byte sniffable, so a dropped filename hits
// the audio.mp3 fallback and OpenAI rejects the mislabelled upload.
func TestTranscriptionRequest_PreservesFilenameEndToEnd(t *testing.T) {
	ingress := &OpenAITranscriptionRequest{
		Model:    "whisper-1",
		File:     []byte("\x00\x00\x00\x20ftypM4A fake-m4a-bytes"),
		Filename: "recording.m4a",
	}

	bifrostReq := ingress.ToBifrostTranscriptionRequest(nil)
	if bifrostReq.Input.Filename != "recording.m4a" {
		t.Fatalf("ingress conversion dropped filename: %q", bifrostReq.Input.Filename)
	}

	egress := ToOpenAITranscriptionRequest(bifrostReq)
	if egress == nil {
		t.Fatal("egress request is nil")
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if bifrostErr := ParseTranscriptionFormDataBodyFromRequest(writer, egress, schemas.OpenAI); bifrostErr != nil {
		t.Fatalf("unexpected bifrost error: %v", bifrostErr.Error.Message)
	}

	if got := multipartFilePartName(t, writer.FormDataContentType(), body.Bytes()); got != "recording.m4a" {
		t.Errorf("wire multipart file part named %q, want %q (sniffing fallback engaged)", got, "recording.m4a")
	}
}

// multipartFilePartName returns the filename of the first "file" part in the
// multipart body, or "" if absent.
func multipartFilePartName(t *testing.T, contentType string, body []byte) string {
	t.Helper()
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		t.Fatalf("ParseMediaType(%q): %v", contentType, err)
	}
	reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("NextPart(): %v", err)
		}
		name := part.FormName()
		filename := part.FileName()
		_, _ = io.Copy(io.Discard, part)
		_ = part.Close()
		if name == "file" {
			return filename
		}
	}
	return ""
}

func TestParseTranscriptionFormDataBodyFromRequest_ChunkingStrategyString(t *testing.T) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	req := &OpenAITranscriptionRequest{
		Model:    "gpt-4o-transcribe-diarize",
		File:     []byte("audio-bytes"),
		Filename: "sample.mp3",
		TranscriptionParameters: schemas.TranscriptionParameters{
			ExtraParams: map[string]interface{}{
				"chunking_strategy": "auto",
			},
		},
	}

	if bifrostErr := ParseTranscriptionFormDataBodyFromRequest(writer, req, schemas.OpenAI); bifrostErr != nil {
		t.Fatalf("unexpected bifrost error: %v", bifrostErr.Error.Message)
	}

	contentType := writer.FormDataContentType()
	if got := multipartFieldValue(t, contentType, body.Bytes(), "chunking_strategy"); got != "auto" {
		t.Fatalf("expected chunking_strategy=auto written verbatim, got %q", got)
	}

	order := multipartPartOrder(t, contentType, body.Bytes())
	if order[len(order)-1] != "file" {
		t.Fatalf("expected file part last, got order %v", order)
	}
}

func TestParseTranscriptionFormDataBodyFromRequest_ChunkingStrategyObject(t *testing.T) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	req := &OpenAITranscriptionRequest{
		Model:    "gpt-4o-transcribe-diarize",
		File:     []byte("audio-bytes"),
		Filename: "sample.mp3",
		TranscriptionParameters: schemas.TranscriptionParameters{
			ExtraParams: map[string]interface{}{
				"chunking_strategy": map[string]interface{}{
					"type":      "server_vad",
					"threshold": 0.5,
				},
			},
		},
	}

	if bifrostErr := ParseTranscriptionFormDataBodyFromRequest(writer, req, schemas.OpenAI); bifrostErr != nil {
		t.Fatalf("unexpected bifrost error: %v", bifrostErr.Error.Message)
	}

	got := multipartFieldValue(t, writer.FormDataContentType(), body.Bytes(), "chunking_strategy")
	if got == "" {
		t.Fatal("expected chunking_strategy object to be written as a form field")
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal([]byte(got), &decoded); err != nil {
		t.Fatalf("expected chunking_strategy to be valid JSON, got %q: %v", got, err)
	}
	if decoded["type"] != "server_vad" {
		t.Fatalf("expected type=server_vad, got %v", decoded["type"])
	}
}

// A form field the caller repeated (include[]=a&include[]=b) reaches ExtraParams as a []string
// and must go out as one form field per value, not as one JSON-encoded field (TXE-6530).
func TestParseTranscriptionFormDataBodyFromRequest_RepeatedExtraField(t *testing.T) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	req := &OpenAITranscriptionRequest{
		Model:    "gpt-4o-transcribe",
		File:     []byte("audio-bytes"),
		Filename: "sample.mp3",
		TranscriptionParameters: schemas.TranscriptionParameters{
			ExtraParams: map[string]interface{}{
				"include[]": []string{"logprobs", "segments"},
			},
		},
	}

	if bifrostErr := ParseTranscriptionFormDataBodyFromRequest(writer, req, schemas.OpenAI); bifrostErr != nil {
		t.Fatalf("unexpected bifrost error: %v", bifrostErr.Error.Message)
	}

	_, params, err := mime.ParseMediaType(writer.FormDataContentType())
	if err != nil {
		t.Fatal(err)
	}
	form, err := multipart.NewReader(bytes.NewReader(body.Bytes()), params["boundary"]).ReadForm(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	got := form.Value["include[]"]
	if len(got) != 2 || got[0] != "logprobs" || got[1] != "segments" {
		t.Fatalf("include[] = %q, want [logprobs segments] as two fields", got)
	}
}

// A URL-only transcription (Groq's "url" form field in place of the file, contract C4) must go
// out with the URL as a form field and no file part; the URL is never fetched by Bifrost.
func TestToOpenAITranscriptionRequest_URLOnlyHasURLFieldAndNoFilePart(t *testing.T) {
	audioURL := "https://example.com/audio/clip.m4a"
	request := &schemas.BifrostTranscriptionRequest{
		Model: "whisper-large-v3-turbo",
		Input: &schemas.TranscriptionInput{},
		Params: &schemas.TranscriptionParameters{
			ExtraParams: map[string]interface{}{schemas.TranscriptionURLParam: audioURL},
		},
	}
	reqBody := ToOpenAITranscriptionRequest(request)
	if reqBody == nil {
		t.Fatal("expected a URL-only request to convert, got nil")
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if bifrostErr := ParseTranscriptionFormDataBodyFromRequest(writer, reqBody, schemas.Groq); bifrostErr != nil {
		t.Fatalf("unexpected bifrost error: %v", bifrostErr.Error.Message)
	}
	contentType := writer.FormDataContentType()
	if got := multipartFieldValue(t, contentType, body.Bytes(), "url"); got != audioURL {
		t.Fatalf("url field = %q, want %q", got, audioURL)
	}
	for _, name := range multipartPartOrder(t, contentType, body.Bytes()) {
		if name == "file" {
			t.Fatal("a URL-only request must not carry a file part")
		}
	}
}

// With neither a file nor a URL there is no input to send.
func TestToOpenAITranscriptionRequest_NoFileNoURLIsNil(t *testing.T) {
	for name, request := range map[string]*schemas.BifrostTranscriptionRequest{
		"nil input":   {Model: "whisper-1"},
		"empty input": {Model: "whisper-1", Input: &schemas.TranscriptionInput{}},
		"empty url": {Model: "whisper-1", Input: &schemas.TranscriptionInput{}, Params: &schemas.TranscriptionParameters{
			ExtraParams: map[string]interface{}{schemas.TranscriptionURLParam: ""},
		}},
	} {
		if got := ToOpenAITranscriptionRequest(request); got != nil {
			t.Fatalf("%s: expected nil, got %+v", name, got)
		}
	}
}

// transcribeWithResponse runs one non-streaming transcription against an upstream that answers body.
func transcribeWithResponse(t *testing.T, responseFormat, body string) *schemas.BifrostTranscriptionResponse {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	provider := NewOpenAIProvider(&schemas.ProviderConfig{
		NetworkConfig: schemas.NetworkConfig{BaseURL: server.URL, DefaultRequestTimeoutInSeconds: 30},
	}, &testLogger{})
	request := &schemas.BifrostTranscriptionRequest{
		Model:  "whisper-1",
		Input:  &schemas.TranscriptionInput{File: []byte("fake-audio-bytes"), Filename: "sample.mp3"},
		Params: &schemas.TranscriptionParameters{ResponseFormat: &responseFormat},
	}
	ctx, cancel := schemas.NewBifrostContextWithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, bifrostErr := provider.Transcription(ctx, schemas.Key{Value: *schemas.NewSecretVar("test-key")}, request)
	if bifrostErr != nil {
		t.Fatalf("expected no error, got: %v", bifrostErr.Error.Message)
	}
	return resp
}

// Contract C5: a verbose_json response with the vendor's duration and no usage (Groq's shape,
// sample from console.groq.com/docs/speech-to-text) reports that duration as usage; Bifrost's
// unified usage is where Tokun reads the billed length.
func TestTranscription_VerboseJSONDurationWithoutUsageBecomesDurationUsage(t *testing.T) {
	groqVerboseJSON := `{
		"task": "transcribe",
		"language": "English",
		"duration": 12.4,
		"text": "Your transcribed text appears here.",
		"segments": [
			{"id": 0, "seek": 0, "start": 0, "end": 3.24, "text": " Your transcribed text appears here.", "tokens": [50365, 2812], "temperature": 0, "avg_logprob": -0.097569615, "compression_ratio": 1.6637554, "no_speech_prob": 0.012814695}
		],
		"x_groq": {"id": "req_01jbd6g2qdfw2adyrt2az8hz4w"}
	}`
	resp := transcribeWithResponse(t, "verbose_json", groqVerboseJSON)
	if resp.Usage == nil {
		t.Fatal("expected usage filled from the vendor-reported duration, got none")
	}
	if resp.Usage.Type != "duration" || resp.Usage.Seconds == nil || *resp.Usage.Seconds != 12.4 {
		t.Fatalf("usage = %+v, want {type: duration, seconds: 12.4}", resp.Usage)
	}
	if resp.Usage.InputTokens != nil || resp.Usage.OutputTokens != nil || resp.Usage.TotalTokens != nil {
		t.Fatalf("no token counts were reported, none may be filled: %+v", resp.Usage)
	}

	marshaled, err := sonic.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}
	var wire struct {
		Usage map[string]interface{} `json:"usage"`
	}
	if err := sonic.Unmarshal(marshaled, &wire); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if wire.Usage["type"] != "duration" || wire.Usage["seconds"] != 12.4 {
		t.Fatalf("wire usage = %v, want {type: duration, seconds: 12.4}", wire.Usage)
	}
}

// A response that already carries usage keeps it as the vendor sent it, and a response with no
// duration gets no usage invented (C5 agreement two).
func TestTranscription_ResponseUsageIsNeverReplacedOrInvented(t *testing.T) {
	withUsage := `{"task":"transcribe","duration":12.4,"text":"hi","usage":{"type":"duration","seconds":13}}`
	resp := transcribeWithResponse(t, "verbose_json", withUsage)
	if resp.Usage == nil || resp.Usage.Type != "duration" || resp.Usage.Seconds == nil || *resp.Usage.Seconds != 13 {
		t.Fatalf("usage = %+v, want the vendor's {type: duration, seconds: 13}", resp.Usage)
	}

	withTokens := `{"text":"hi","usage":{"type":"tokens","input_tokens":14,"output_tokens":45,"total_tokens":59}}`
	resp = transcribeWithResponse(t, "json", withTokens)
	if resp.Usage == nil || resp.Usage.Type != "tokens" || resp.Usage.Seconds != nil {
		t.Fatalf("usage = %+v, want the vendor's token usage unchanged", resp.Usage)
	}

	noDuration := `{"text":"hi"}`
	if resp = transcribeWithResponse(t, "json", noDuration); resp.Usage != nil {
		t.Fatalf("usage = %+v, want none when the vendor reported nothing", resp.Usage)
	}
}
