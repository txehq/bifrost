package azurespeech_test

import (
	"bytes"
	"context"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/internal/llmtests"
	"github.com/capsohq/bifrost/core/providers/azurespeech"
	"github.com/capsohq/bifrost/core/schemas"
)

// maiVoice is the voice name the vendor sheet documents: the part after the colon is the
// model Azure AI Speech bills.
const maiVoice = "en-US-Harper:MAI-Voice-2.1-Flash"

// TestAzureSpeech is the live entrypoint (make test-core PROVIDER=azurespeech). It
// synthesizes one sentence and transcribes the audio it got back, so it needs a Speech
// resource: AZURE_SPEECH_API_KEY and AZURE_SPEECH_ENDPOINT (a region or the resource
// endpoint). AZURE_SPEECH_VOICE overrides the voice.
func TestAzureSpeech(t *testing.T) {
	t.Parallel()
	if strings.TrimSpace(os.Getenv("AZURE_SPEECH_API_KEY")) == "" || strings.TrimSpace(os.Getenv("AZURE_SPEECH_ENDPOINT")) == "" {
		t.Skip("Skipping Azure AI Speech tests because AZURE_SPEECH_API_KEY or AZURE_SPEECH_ENDPOINT is not set")
	}

	client, ctx, cancel, err := llmtests.SetupTest()
	if err != nil {
		t.Fatalf("Error initializing test setup: %v", err)
	}
	defer cancel()
	defer client.Shutdown()

	voice := strings.TrimSpace(os.Getenv("AZURE_SPEECH_VOICE"))
	if voice == "" {
		voice = maiVoice
	}
	bfCtx := schemas.NewBifrostContext(ctx, schemas.NoDeadline)
	speech, bifrostErr := client.SpeechRequest(bfCtx, &schemas.BifrostSpeechRequest{
		Provider: schemas.AzureSpeech,
		Model:    "mai-voice-2.1-flash",
		Input:    &schemas.SpeechInput{Input: "Bifrost routes this sentence through Azure AI Speech."},
		Params: &schemas.SpeechParameters{
			VoiceConfig:    &schemas.SpeechVoiceInput{Voice: &voice},
			ResponseFormat: "wav",
		},
	})
	if bifrostErr != nil {
		t.Fatalf("speech: %+v", bifrostErr.Error)
	}
	if len(speech.Audio) == 0 {
		t.Fatal("speech returned no audio")
	}

	transcription, bifrostErr := client.TranscriptionRequest(bfCtx, &schemas.BifrostTranscriptionRequest{
		Provider: schemas.AzureSpeech,
		Model:    "mai-transcribe-2",
		Input:    &schemas.TranscriptionInput{File: speech.Audio, Filename: "speech.wav"},
		Params:   &schemas.TranscriptionParameters{Language: schemas.Ptr("en")},
	})
	if bifrostErr != nil {
		t.Fatalf("transcription: %+v", bifrostErr.Error)
	}
	if !strings.Contains(strings.ToLower(transcription.Text), "bifrost") {
		t.Errorf("transcript %q does not contain the synthesized sentence", transcription.Text)
	}
	if transcription.Usage == nil || transcription.Usage.Seconds == nil || *transcription.Usage.Seconds <= 0 {
		t.Errorf("usage = %+v, want the vendor's duration", transcription.Usage)
	}
}

type capturedRequest struct {
	method string
	path   string
	query  string
	header http.Header
	body   []byte
}

// newMockServer starts a server that records each request and replies with the response
// registered for its method and path.
func newMockServer(t testing.TB, responses map[string]http.HandlerFunc) (string, func() []capturedRequest) {
	t.Helper()
	var mu sync.Mutex
	var captured []capturedRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		captured = append(captured, capturedRequest{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, header: r.Header.Clone(), body: body})
		mu.Unlock()
		if respond, ok := responses[r.Method+" "+r.URL.Path]; ok {
			respond(w, r)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(server.Close)
	return server.URL, func() []capturedRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]capturedRequest(nil), captured...)
	}
}

// newProvider builds a provider whose network_config.base_url is baseURL ("" for none).
func newProvider(t testing.TB, baseURL string) *azurespeech.AzureSpeechProvider {
	t.Helper()
	provider, err := azurespeech.NewAzureSpeechProvider(&schemas.ProviderConfig{
		NetworkConfig: schemas.NetworkConfig{
			BaseURL:                        baseURL,
			DefaultRequestTimeoutInSeconds: 10,
			AllowPrivateNetwork:            true,
		},
	}, bifrost.NewDefaultLogger(schemas.LogLevelError))
	if err != nil {
		t.Fatalf("NewAzureSpeechProvider: %v", err)
	}
	return provider
}

// newMockAzureSpeech starts a mock server and a provider that has it as its resource
// endpoint through network_config.base_url.
func newMockAzureSpeech(t testing.TB, responses map[string]http.HandlerFunc) (*azurespeech.AzureSpeechProvider, func() []capturedRequest) {
	t.Helper()
	url, captured := newMockServer(t, responses)
	return newProvider(t, url), captured
}

const (
	speechPath        = "POST /tts/cognitiveservices/v1"
	transcriptionPath = "POST /speechtotext/transcriptions:transcribe"
)

func audioResponse(contentType string, audio []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(audio)
	}
}

func bodyResponse(status int, contentType, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

func testContext(t testing.TB) *schemas.BifrostContext {
	t.Helper()
	ctx, cancel := schemas.NewBifrostContextWithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func testKey() schemas.Key {
	return schemas.Key{Value: *schemas.NewSecretVar("speech-resource-key"), Models: []string{"*"}}
}

func speechRequest(voice string, params *schemas.SpeechParameters) *schemas.BifrostSpeechRequest {
	if params == nil {
		params = &schemas.SpeechParameters{}
	}
	if voice != "" {
		params.VoiceConfig = &schemas.SpeechVoiceInput{Voice: &voice}
	}
	return &schemas.BifrostSpeechRequest{
		Provider: schemas.AzureSpeech,
		Model:    "mai-voice-2.1-flash",
		Input:    &schemas.SpeechInput{Input: "I'm excited to try text to speech!"},
		Params:   params,
	}
}

func transcriptionRequest(params *schemas.TranscriptionParameters) *schemas.BifrostTranscriptionRequest {
	return &schemas.BifrostTranscriptionRequest{
		Provider: schemas.AzureSpeech,
		Model:    "mai-transcribe-2",
		Input:    &schemas.TranscriptionInput{File: []byte("RIFFfake-wav"), Filename: "YourAudioFile.wav"},
		Params:   params,
	}
}

func passthroughPostHook(_ *schemas.BifrostContext, result *schemas.BifrostResponse, err *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError) {
	return result, err
}

// requireUnsupportedParameter asserts err is the 400 caller_unsupported_parameter error
// naming param (contract C4).
func requireUnsupportedParameter(t *testing.T, err *schemas.BifrostError, param string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected caller_unsupported_parameter for %q, got no error", param)
	}
	if err.StatusCode == nil || *err.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %v, want 400", err.StatusCode)
	}
	if err.ExtraFields.ErrorType != schemas.ErrorTypeCallerUnsupportedParameter {
		t.Errorf("error_type = %q, want %q", err.ExtraFields.ErrorType, schemas.ErrorTypeCallerUnsupportedParameter)
	}
	if err.Error == nil || err.Error.Param != param {
		t.Errorf("error.param = %v, want %q", err.Error, param)
	}
}

// The acceptance case of the vendor sheet: voice en-US-Harper:MAI-Voice-2.1-Flash, a speed
// and response_format mp3 send the expected SSML and X-Microsoft-OutputFormat, once, and
// the audio comes back with no usage (real-time synthesis reports none).
func TestAzureSpeechSpeechSendsSSMLAndOutputFormat(t *testing.T) {
	audio := []byte("ID3fake-mp3-bytes")
	provider, captured := newMockAzureSpeech(t, map[string]http.HandlerFunc{
		speechPath: audioResponse("audio/mpeg", audio),
	})

	response, bifrostErr := provider.Speech(testContext(t), testKey(), speechRequest(maiVoice, &schemas.SpeechParameters{
		Speed:          schemas.Ptr(1.25),
		ResponseFormat: "mp3",
	}))
	if bifrostErr != nil {
		t.Fatalf("Speech error: %+v", bifrostErr.Error)
	}
	if !bytes.Equal(response.Audio, audio) {
		t.Errorf("audio = %q, want %q", response.Audio, audio)
	}
	if response.Usage != nil {
		t.Errorf("usage = %+v, want none: the vendor reports no character count", response.Usage)
	}

	requests := captured()
	if len(requests) != 1 {
		t.Fatalf("upstream calls = %d, want exactly 1", len(requests))
	}
	got := requests[0]
	wantSSML := `<speak version="1.0" xmlns="http://www.w3.org/2001/10/synthesis" xml:lang="en-US">` +
		`<voice name="en-US-Harper:MAI-Voice-2.1-Flash"><prosody rate="1.25">I&#39;m excited to try text to speech!</prosody></voice></speak>`
	if string(got.body) != wantSSML {
		t.Errorf("SSML body =\n%s\nwant\n%s", got.body, wantSSML)
	}
	if v := got.header.Get("X-Microsoft-OutputFormat"); v != "audio-24khz-48kbitrate-mono-mp3" {
		t.Errorf("X-Microsoft-OutputFormat = %q", v)
	}
	if v := got.header.Get("Content-Type"); v != "application/ssml+xml" {
		t.Errorf("Content-Type = %q", v)
	}
	if v := got.header.Get("Ocp-Apim-Subscription-Key"); v != "speech-resource-key" {
		t.Errorf("Ocp-Apim-Subscription-Key = %q", v)
	}
	if v := got.header.Get("User-Agent"); v == "" {
		t.Error("User-Agent is required by the text to speech API and was not sent")
	}
	if v := got.header.Get("Authorization"); v != "" {
		t.Errorf("Authorization = %q, want none: the resource key goes in Ocp-Apim-Subscription-Key", v)
	}
}

// The Content-Type of the speech response is the vendor's own for the audio it returned
// (TXE-6530): it is kept as sent, and left empty when the vendor sent none or a non-audio one.
func TestAzureSpeechSpeechKeepsTheVendorAudioContentType(t *testing.T) {
	cases := []struct {
		name, vendor, want string
	}{
		{name: "wav", vendor: "audio/x-wav", want: "audio/x-wav"},
		{name: "opus with codec parameter", vendor: "audio/ogg; codecs=opus", want: "audio/ogg; codecs=opus"},
		{name: "upper case", vendor: "Audio/MPEG", want: "Audio/MPEG"},
		{name: "not an audio type", vendor: "application/octet-stream", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider, _ := newMockAzureSpeech(t, map[string]http.HandlerFunc{
				speechPath: audioResponse(tc.vendor, []byte("audio-bytes")),
			})
			response, bifrostErr := provider.Speech(testContext(t), testKey(), speechRequest(maiVoice, nil))
			if bifrostErr != nil {
				t.Fatalf("Speech error: %+v", bifrostErr.Error)
			}
			if response.ContentType != tc.want {
				t.Errorf("ContentType = %q, want %q", response.ContentType, tc.want)
			}
		})
	}
}

func TestToAzureSpeechSSML(t *testing.T) {
	const open = `<speak version="1.0" xmlns="http://www.w3.org/2001/10/synthesis" xml:lang="`
	tests := []struct {
		name       string
		input      string
		voice      string
		params     schemas.SpeechParameters
		wantSSML   string
		wantFormat string
	}{
		{
			name:       "no speed sends no prosody and defaults to mp3",
			input:      "Hello",
			voice:      maiVoice,
			wantSSML:   open + `en-US"><voice name="en-US-Harper:MAI-Voice-2.1-Flash">Hello</voice></speak>`,
			wantFormat: "audio-24khz-48kbitrate-mono-mp3",
		},
		{
			name:       "language_code is the document language",
			input:      "Hola",
			voice:      maiVoice,
			params:     schemas.SpeechParameters{LanguageCode: schemas.Ptr("es-ES"), ResponseFormat: "wav"},
			wantSSML:   open + `es-ES"><voice name="en-US-Harper:MAI-Voice-2.1-Flash">Hola</voice></speak>`,
			wantFormat: "riff-24khz-16bit-mono-pcm",
		},
		{
			name:       "the voice locale is used when no language_code is given",
			input:      "你好",
			voice:      "zh-CN-XiaoxiaoNeural",
			params:     schemas.SpeechParameters{ResponseFormat: "pcm"},
			wantSSML:   open + `zh-CN"><voice name="zh-CN-XiaoxiaoNeural">你好</voice></speak>`,
			wantFormat: "raw-24khz-16bit-mono-pcm",
		},
		{
			name:       "markup in the input is escaped, never sent as SSML",
			input:      `a < b & "c" <break time="1s"/>`,
			voice:      maiVoice,
			params:     schemas.SpeechParameters{Speed: schemas.Ptr(0.5), ResponseFormat: "opus"},
			wantSSML:   open + `en-US"><voice name="en-US-Harper:MAI-Voice-2.1-Flash"><prosody rate="0.5">a &lt; b &amp; &#34;c&#34; &lt;break time=&#34;1s&#34;/&gt;</prosody></voice></speak>`,
			wantFormat: "ogg-24khz-16bit-mono-opus",
		},
		{
			name:       "a voice name cannot break out of its attribute",
			input:      "Hi",
			voice:      `x"><voice name="other`,
			wantSSML:   open + `en-US"><voice name="x&#34;&gt;&lt;voice name=&#34;other">Hi</voice></speak>`,
			wantFormat: "audio-24khz-48kbitrate-mono-mp3",
		},
		{
			name:       "an Azure output format name is sent as given",
			input:      "Hi",
			voice:      maiVoice,
			params:     schemas.SpeechParameters{Speed: schemas.Ptr(2.0), ResponseFormat: "audio-48khz-192kbitrate-mono-mp3"},
			wantSSML:   open + `en-US"><voice name="en-US-Harper:MAI-Voice-2.1-Flash"><prosody rate="2">Hi</prosody></voice></speak>`,
			wantFormat: "audio-48khz-192kbitrate-mono-mp3",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			params := tt.params
			request := speechRequest(tt.voice, &params)
			request.Input.Input = tt.input
			ssml, format, bifrostErr := azurespeech.ToAzureSpeechSSML(request)
			if bifrostErr != nil {
				t.Fatalf("unexpected error: %+v", bifrostErr.Error)
			}
			if string(ssml) != tt.wantSSML {
				t.Errorf("SSML =\n%s\nwant\n%s", ssml, tt.wantSSML)
			}
			if format != tt.wantFormat {
				t.Errorf("output format = %q, want %q", format, tt.wantFormat)
			}
		})
	}
}

// A value Azure AI Speech cannot take as given is refused with the field's name; it is
// never clamped or replaced (contract C4), and nothing is sent upstream.
func TestAzureSpeechSpeechRejectsUndeliverableValues(t *testing.T) {
	tests := []struct {
		name   string
		params schemas.SpeechParameters
		param  string
	}{
		{"speed above the prosody range", schemas.SpeechParameters{Speed: schemas.Ptr(2.5)}, "speed"},
		{"speed below the prosody range", schemas.SpeechParameters{Speed: schemas.Ptr(0.25)}, "speed"},
		{"aac has no Azure output format", schemas.SpeechParameters{ResponseFormat: "aac"}, "response_format"},
		{"flac has no Azure output format", schemas.SpeechParameters{ResponseFormat: "flac"}, "response_format"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider, captured := newMockAzureSpeech(t, map[string]http.HandlerFunc{
				speechPath: audioResponse("audio/mpeg", []byte("never")),
			})
			params := tt.params
			_, bifrostErr := provider.Speech(testContext(t), testKey(), speechRequest(maiVoice, &params))
			requireUnsupportedParameter(t, bifrostErr, tt.param)

			params = tt.params
			_, bifrostErr = provider.SpeechStream(testContext(t), passthroughPostHook, func(context.Context) {}, testKey(), speechRequest(maiVoice, &params))
			requireUnsupportedParameter(t, bifrostErr, tt.param)

			if n := len(captured()); n != 0 {
				t.Errorf("upstream calls = %d, want 0", n)
			}
		})
	}

	t.Run("multi-speaker voice", func(t *testing.T) {
		request := speechRequest("", &schemas.SpeechParameters{VoiceConfig: &schemas.SpeechVoiceInput{
			MultiVoiceConfig: []schemas.VoiceConfig{{Speaker: "a", Voice: maiVoice}},
		}})
		_, _, bifrostErr := azurespeech.ToAzureSpeechSSML(request)
		requireUnsupportedParameter(t, bifrostErr, "voice")
	})

	t.Run("a missing voice is a 400, not an unsupported parameter", func(t *testing.T) {
		_, _, bifrostErr := azurespeech.ToAzureSpeechSSML(speechRequest("", nil))
		if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusBadRequest {
			t.Fatalf("error = %+v, want a 400", bifrostErr)
		}
		if bifrostErr.ExtraFields.ErrorType != schemas.ErrorTypeCallerInvalidRequest {
			t.Errorf("error_type = %q, want %q", bifrostErr.ExtraFields.ErrorType, schemas.ErrorTypeCallerInvalidRequest)
		}
	})
}

// The provider declares the unified fields it takes, so core refuses any other before a
// key is chosen (contract C4, schemas.CheckDeclaredUnifiedFields).
func TestAzureSpeechDeclaresUnifiedFields(t *testing.T) {
	provider, _ := newMockAzureSpeech(t, nil)

	speech := func(mutate func(*schemas.SpeechParameters)) *schemas.BifrostRequest {
		request := speechRequest(maiVoice, &schemas.SpeechParameters{
			Speed: schemas.Ptr(1.0), ResponseFormat: "mp3", LanguageCode: schemas.Ptr("en-US"),
		})
		if mutate != nil {
			mutate(request.Params)
		}
		return &schemas.BifrostRequest{RequestType: schemas.SpeechRequest, SpeechRequest: request}
	}
	if err := schemas.CheckDeclaredUnifiedFields(provider, speech(nil)); err != nil {
		t.Errorf("voice, speed, response_format and language_code must be accepted, got %+v", err.Error)
	}
	requireUnsupportedParameter(t, schemas.CheckDeclaredUnifiedFields(provider, speech(func(p *schemas.SpeechParameters) {
		p.Instructions = "speak warmly"
	})), "instructions")
	requireUnsupportedParameter(t, schemas.CheckDeclaredUnifiedFields(provider, speech(func(p *schemas.SpeechParameters) {
		p.WithTimestamps = schemas.Ptr(true)
	})), "with_timestamps")

	stream := speech(func(p *schemas.SpeechParameters) { p.Instructions = "speak warmly" })
	stream.RequestType = schemas.SpeechStreamRequest
	requireUnsupportedParameter(t, schemas.CheckDeclaredUnifiedFields(provider, stream), "instructions")

	transcription := func(mutate func(*schemas.TranscriptionParameters)) *schemas.BifrostRequest {
		request := transcriptionRequest(&schemas.TranscriptionParameters{
			Language:               schemas.Ptr("en"),
			Prompt:                 schemas.Ptr("exempt from the check"),
			ResponseFormat:         schemas.Ptr("verbose_json"),
			TimestampGranularities: []string{"word"},
		})
		if mutate != nil {
			mutate(request.Params)
		}
		return &schemas.BifrostRequest{RequestType: schemas.TranscriptionRequest, TranscriptionRequest: request}
	}
	if err := schemas.CheckDeclaredUnifiedFields(provider, transcription(nil)); err != nil {
		t.Errorf("language, prompt, response_format and timestamp_granularities must be accepted, got %+v", err.Error)
	}
	requireUnsupportedParameter(t, schemas.CheckDeclaredUnifiedFields(provider, transcription(func(p *schemas.TranscriptionParameters) {
		p.Temperature = schemas.Ptr(0.2)
	})), "temperature")
}

// The key's azure_key_config.endpoint is where its Speech resource lives, and it wins over
// the provider's base URL, so keys of one provider can sit in different resources.
func TestAzureSpeechKeyCarriesTheResourceLocation(t *testing.T) {
	keyURL, keyCaptured := newMockServer(t, map[string]http.HandlerFunc{
		speechPath:        audioResponse("audio/mpeg", []byte("from-key-endpoint")),
		transcriptionPath: bodyResponse(http.StatusOK, "application/json", transcriptionSample),
	})
	provider, baseCaptured := newMockAzureSpeech(t, map[string]http.HandlerFunc{
		speechPath: audioResponse("audio/mpeg", []byte("from-base-url")),
	})

	key := testKey()
	key.AzureKeyConfig = &schemas.AzureKeyConfig{Endpoint: *schemas.NewSecretVar(keyURL + "/")}
	response, bifrostErr := provider.Speech(testContext(t), key, speechRequest(maiVoice, nil))
	if bifrostErr != nil {
		t.Fatalf("Speech error: %+v", bifrostErr.Error)
	}
	if string(response.Audio) != "from-key-endpoint" {
		t.Errorf("audio = %q, want the key endpoint's", response.Audio)
	}
	if _, bifrostErr := provider.Transcription(testContext(t), key, transcriptionRequest(nil)); bifrostErr != nil {
		t.Fatalf("Transcription error: %+v", bifrostErr.Error)
	}
	if n := len(keyCaptured()); n != 2 {
		t.Errorf("the key's endpoint was called %d times, want 2", n)
	}
	if n := len(baseCaptured()); n != 0 {
		t.Errorf("the provider base URL was called %d times, want 0", n)
	}
}

// A provider with no base URL and a key with no endpoint cannot know which Speech resource
// to call; it says what to configure and sends nothing.
func TestAzureSpeechNeedsAResourceLocation(t *testing.T) {
	provider := newProvider(t, "")
	_, bifrostErr := provider.Speech(testContext(t), testKey(), speechRequest(maiVoice, nil))
	if bifrostErr == nil || !strings.Contains(bifrostErr.Error.Message, "azure_key_config.endpoint") {
		t.Fatalf("error = %+v, want one naming azure_key_config.endpoint", bifrostErr)
	}
	_, bifrostErr = provider.Transcription(testContext(t), testKey(), transcriptionRequest(nil))
	if bifrostErr == nil || !strings.Contains(bifrostErr.Error.Message, "azure_key_config.endpoint") {
		t.Fatalf("error = %+v, want one naming azure_key_config.endpoint", bifrostErr)
	}
}

// A vendor error keeps its status, and the vendor's code and message when it sent a body.
func TestAzureSpeechSpeechVendorErrorKeepsStatus(t *testing.T) {
	t.Run("401 with no body", func(t *testing.T) {
		provider, captured := newMockAzureSpeech(t, map[string]http.HandlerFunc{
			speechPath: bodyResponse(http.StatusUnauthorized, "", ""),
		})
		_, bifrostErr := provider.Speech(testContext(t), testKey(), speechRequest(maiVoice, nil))
		if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusUnauthorized {
			t.Fatalf("error = %+v, want the vendor's 401", bifrostErr)
		}
		if bifrostErr.IsBifrostError {
			t.Error("a vendor error must not be marked is_bifrost_error")
		}
		if got := bifrostErr.ResolveErrorType(); got != schemas.ErrorTypeProviderAuthFailed {
			t.Errorf("error_type = %q, want %q", got, schemas.ErrorTypeProviderAuthFailed)
		}
		if n := len(captured()); n != 1 {
			t.Errorf("upstream calls = %d, want exactly 1", n)
		}
	})

	t.Run("400 with a JSON body", func(t *testing.T) {
		provider, _ := newMockAzureSpeech(t, map[string]http.HandlerFunc{
			speechPath: bodyResponse(http.StatusBadRequest, "application/json", `{"error":{"code":"BadRequest","message":"Unsupported voice en-US-Nobody."}}`),
		})
		_, bifrostErr := provider.Speech(testContext(t), testKey(), speechRequest("en-US-Nobody", nil))
		if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusBadRequest {
			t.Fatalf("error = %+v, want the vendor's 400", bifrostErr)
		}
		if bifrostErr.Error.Code == nil || *bifrostErr.Error.Code != "BadRequest" || bifrostErr.Error.Message != "Unsupported voice en-US-Nobody." {
			t.Errorf("code = %v message = %q, want the vendor's", bifrostErr.Error.Code, bifrostErr.Error.Message)
		}
		if got := bifrostErr.ResolveErrorType(); got != schemas.ErrorTypeCallerInvalidRequest {
			t.Errorf("error_type = %q, want %q", got, schemas.ErrorTypeCallerInvalidRequest)
		}
	})
}

// A 200 with no audio and no error object is 502 provider_empty_result (contract C6).
func TestAzureSpeechSpeechEmptyAudioIsProviderEmptyResult(t *testing.T) {
	provider, _ := newMockAzureSpeech(t, map[string]http.HandlerFunc{
		speechPath: audioResponse("audio/mpeg", nil),
	})
	requireEmptyResult := func(t *testing.T, bifrostErr *schemas.BifrostError) {
		t.Helper()
		if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusBadGateway {
			t.Fatalf("error = %+v, want a 502", bifrostErr)
		}
		if bifrostErr.ExtraFields.ErrorType != schemas.ErrorTypeProviderEmptyResult {
			t.Errorf("error_type = %q, want %q", bifrostErr.ExtraFields.ErrorType, schemas.ErrorTypeProviderEmptyResult)
		}
	}

	_, bifrostErr := provider.Speech(testContext(t), testKey(), speechRequest(maiVoice, nil))
	requireEmptyResult(t, bifrostErr)

	stream, bifrostErr := provider.SpeechStream(testContext(t), passthroughPostHook, func(context.Context) {}, testKey(), speechRequest(maiVoice, nil))
	if bifrostErr != nil {
		t.Fatalf("SpeechStream setup error: %+v", bifrostErr.Error)
	}
	var chunks []*schemas.BifrostStreamChunk
	for chunk := range stream {
		chunks = append(chunks, chunk)
	}
	if len(chunks) != 1 || chunks[0].BifrostError == nil {
		t.Fatalf("expected exactly one error chunk, got %d chunks", len(chunks))
	}
	requireEmptyResult(t, chunks[0].BifrostError)
}

// Each chunk of audio is forwarded when it arrives (contract C10): the fixture sends the
// first chunk and then holds the response open until the test has received that chunk
// from the stream. A provider that buffered the body would deadlock here and time out.
func TestAzureSpeechSpeechStreamForwardsChunksOnArrival(t *testing.T) {
	first, second := bytes.Repeat([]byte{0xaa}, 512), bytes.Repeat([]byte{0xbb}, 512)
	firstReceived := make(chan struct{})
	provider, captured := newMockAzureSpeech(t, map[string]http.HandlerFunc{
		speechPath: func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "audio/mpeg")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(first)
			w.(http.Flusher).Flush()
			select {
			case <-firstReceived:
			case <-r.Context().Done():
				return
			case <-time.After(8 * time.Second):
				return
			}
			_, _ = w.Write(second)
		},
	})

	stream, bifrostErr := provider.SpeechStream(testContext(t), passthroughPostHook, func(context.Context) {}, testKey(), speechRequest(maiVoice, &schemas.SpeechParameters{
		Speed:          schemas.Ptr(1.25),
		ResponseFormat: "mp3",
	}))
	if bifrostErr != nil {
		t.Fatalf("SpeechStream error: %+v", bifrostErr.Error)
	}

	var audio []byte
	var types []schemas.SpeechStreamResponseType
	released := false
	timeout := time.After(9 * time.Second)
	for done := false; !done; {
		select {
		case chunk, ok := <-stream:
			if !ok {
				done = true
				break
			}
			if chunk.BifrostError != nil {
				t.Fatalf("unexpected stream error: %+v", chunk.BifrostError.Error)
			}
			response := chunk.BifrostSpeechStreamResponse
			types = append(types, response.Type)
			audio = append(audio, response.Audio...)
			if response.Usage != nil {
				t.Errorf("usage = %+v, want none: the vendor reports no character count", response.Usage)
			}
			if !released && len(audio) >= len(first) {
				if len(audio) > len(first) {
					t.Fatalf("received %d bytes before the fixture sent its second chunk", len(audio))
				}
				released = true
				close(firstReceived)
			}
		case <-timeout:
			t.Fatal("the first chunk was not forwarded while the vendor response was still open")
		}
	}

	if !bytes.Equal(audio, append(append([]byte(nil), first...), second...)) {
		t.Errorf("audio = %d bytes, want the two chunks in order (%d bytes)", len(audio), len(first)+len(second))
	}
	if len(types) < 3 || types[len(types)-1] != schemas.SpeechStreamResponseTypeDone {
		t.Errorf("event types = %v, want deltas followed by speech.audio.done", types)
	}
	for _, eventType := range types[:len(types)-1] {
		if eventType != schemas.SpeechStreamResponseTypeDelta {
			t.Errorf("event types = %v, want only deltas before the final event", types)
		}
	}

	requests := captured()
	if len(requests) != 1 {
		t.Fatalf("upstream calls = %d, want exactly 1", len(requests))
	}
	if v := requests[0].header.Get("X-Microsoft-OutputFormat"); v != "audio-24khz-48kbitrate-mono-mp3" {
		t.Errorf("X-Microsoft-OutputFormat = %q", v)
	}
	if !bytes.Contains(requests[0].body, []byte(`<prosody rate="1.25">`)) {
		t.Errorf("stream SSML missing the prosody rate: %s", requests[0].body)
	}
}

// A vendor error before the first frame is returned as the vendor's status, not as a
// started stream carrying an error frame (contract C10).
func TestAzureSpeechSpeechStreamVendorErrorBeforeFirstFrame(t *testing.T) {
	provider, _ := newMockAzureSpeech(t, map[string]http.HandlerFunc{
		speechPath: bodyResponse(http.StatusTooManyRequests, "", ""),
	})
	stream, bifrostErr := provider.SpeechStream(testContext(t), passthroughPostHook, func(context.Context) {}, testKey(), speechRequest(maiVoice, nil))
	if stream != nil {
		t.Fatal("a stream was started for a vendor error")
	}
	if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("error = %+v, want the vendor's 429", bifrostErr)
	}
	if got := bifrostErr.ResolveErrorType(); got != schemas.ErrorTypeProviderRateLimited {
		t.Errorf("error_type = %q, want %q", got, schemas.ErrorTypeProviderRateLimited)
	}
}

// transcriptionSample is the response of the fast transcription quickstart (Microsoft
// Learn, "Use the fast transcription API"), cut to its first two phrases.
const transcriptionSample = `{
	"durationMilliseconds": 182439,
	"combinedPhrases": [
		{"text": "Good afternoon. This is Sam."}
	],
	"phrases": [
		{
			"offsetMilliseconds": 960,
			"durationMilliseconds": 640,
			"text": "Good afternoon.",
			"words": [
				{"text": "Good", "offsetMilliseconds": 960, "durationMilliseconds": 240},
				{"text": "afternoon.", "offsetMilliseconds": 1200, "durationMilliseconds": 400}
			],
			"locale": "en-US",
			"confidence": 0.93554276
		},
		{
			"offsetMilliseconds": 1600,
			"durationMilliseconds": 640,
			"text": "This is Sam.",
			"words": [
				{"text": "This", "offsetMilliseconds": 1600, "durationMilliseconds": 240},
				{"text": "is", "offsetMilliseconds": 1840, "durationMilliseconds": 120},
				{"text": "Sam.", "offsetMilliseconds": 1960, "durationMilliseconds": 280}
			],
			"locale": "en-US",
			"confidence": 0.93554276
		}
	]
}`

// multipartParts parses a captured multipart request into its text fields and the bytes
// and filename of its audio part.
func multipartParts(t *testing.T, req capturedRequest) (fields map[string]string, audio []byte, filename string) {
	t.Helper()
	mediaType, params, err := mime.ParseMediaType(req.header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" {
		t.Fatalf("content type %q: %v", req.header.Get("Content-Type"), err)
	}
	reader := multipart.NewReader(bytes.NewReader(req.body), params["boundary"])
	fields = map[string]string{}
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("multipart: %v", err)
		}
		data, _ := io.ReadAll(part)
		if part.FormName() == "audio" {
			audio, filename = data, part.FileName()
			continue
		}
		fields[part.FormName()] = string(data)
	}
	return fields, audio, filename
}

// The acceptance case of the vendor sheet: mai-transcribe-2 with a language sends the
// multipart audio + definition once, and returns the text and the billed duration from
// durationMilliseconds.
func TestAzureSpeechTranscription(t *testing.T) {
	provider, captured := newMockAzureSpeech(t, map[string]http.HandlerFunc{
		transcriptionPath: bodyResponse(http.StatusOK, "application/json", transcriptionSample),
	})
	ctx := testContext(t)
	ctx.SetValue(schemas.BifrostContextKeyCaptureRawResponse, true)

	response, bifrostErr := provider.Transcription(ctx, testKey(), transcriptionRequest(&schemas.TranscriptionParameters{
		Language: schemas.Ptr("en"),
		// A field outside the unified structure is not sent without the passthrough header.
		ExtraParams: map[string]interface{}{"diarization": `{"enabled":true}`},
	}))
	if bifrostErr != nil {
		t.Fatalf("Transcription error: %+v", bifrostErr.Error)
	}

	requests := captured()
	if len(requests) != 1 {
		t.Fatalf("upstream calls = %d, want exactly 1", len(requests))
	}
	got := requests[0]
	if got.query != "api-version=2025-10-15" {
		t.Errorf("query = %q, want api-version=2025-10-15", got.query)
	}
	if v := got.header.Get("Ocp-Apim-Subscription-Key"); v != "speech-resource-key" {
		t.Errorf("Ocp-Apim-Subscription-Key = %q", v)
	}
	fields, audio, filename := multipartParts(t, got)
	if string(audio) != "RIFFfake-wav" || filename != "YourAudioFile.wav" {
		t.Errorf("audio part = %q (%s), want the request's file", audio, filename)
	}
	wantDefinition := `{"enhancedMode":{"enabled":true,"model":"MAI-Transcribe-2"},"locales":["en"]}`
	if fields["definition"] != wantDefinition {
		t.Errorf("definition = %s\nwant         %s", fields["definition"], wantDefinition)
	}
	if len(fields) != 1 {
		t.Errorf("form fields = %v, want only definition next to the audio", fields)
	}

	if response.Text != "Good afternoon. This is Sam." {
		t.Errorf("text = %q", response.Text)
	}
	if response.Usage == nil || response.Usage.Type != "duration" || response.Usage.Seconds == nil || *response.Usage.Seconds != 182.439 {
		t.Errorf("usage = %+v, want {type: duration, seconds: 182.439}", response.Usage)
	}
	if response.Usage != nil && (response.Usage.InputTokens != nil || response.Usage.OutputTokens != nil) {
		t.Errorf("usage = %+v, want no token counts: the vendor reports none", response.Usage)
	}
	if response.Language == nil || *response.Language != "en-US" {
		t.Errorf("language = %v, want en-US", response.Language)
	}
	if len(response.Segments) != 2 || response.Segments[1].Text != "This is Sam." || response.Segments[1].Start != 1.6 || response.Segments[1].End != 2.24 {
		t.Errorf("segments = %+v", response.Segments)
	}
	if len(response.Words) != 5 || response.Words[0].Word != "Good" || response.Words[0].Start != 0.96 || response.Words[0].End != 1.2 {
		t.Errorf("words = %+v", response.Words)
	}
	raw, ok := response.ExtraFields.RawResponse.(map[string]interface{})
	if !ok || raw["durationMilliseconds"] == nil || raw["combinedPhrases"] == nil {
		t.Errorf("raw_response = %#v, want the vendor's body", response.ExtraFields.RawResponse)
	}
}

func TestToAzureSpeechTranscriptionRequest(t *testing.T) {
	definition := func(t *testing.T, params *schemas.TranscriptionParameters, passthrough bool) string {
		t.Helper()
		provider, captured := newMockAzureSpeech(t, map[string]http.HandlerFunc{
			transcriptionPath: bodyResponse(http.StatusOK, "application/json", transcriptionSample),
		})
		ctx := testContext(t)
		if passthrough {
			ctx.SetValue(schemas.BifrostContextKeyPassthroughExtraParams, true)
		}
		if _, bifrostErr := provider.Transcription(ctx, testKey(), transcriptionRequest(params)); bifrostErr != nil {
			t.Fatalf("Transcription error: %+v", bifrostErr.Error)
		}
		fields, _, _ := multipartParts(t, captured()[0])
		return fields["definition"]
	}

	t.Run("no params sends only the model", func(t *testing.T) {
		want := `{"enhancedMode":{"enabled":true,"model":"MAI-Transcribe-2"}}`
		if got := definition(t, nil, false); got != want {
			t.Errorf("definition = %s, want %s", got, want)
		}
	})

	t.Run("timestamp granularities become modelOptions.timestamps", func(t *testing.T) {
		for granularities, want := range map[string]string{
			"word":         "word",
			"segment":      "segment",
			"segment,word": "word",
		} {
			got := definition(t, &schemas.TranscriptionParameters{
				ResponseFormat:         schemas.Ptr("verbose_json"),
				TimestampGranularities: strings.Split(granularities, ","),
			}, false)
			wantDefinition := `{"enhancedMode":{"enabled":true,"model":"MAI-Transcribe-2","modelOptions":{"timestamps":"` + want + `"}}}`
			if got != wantDefinition {
				t.Errorf("%s: definition = %s, want %s", granularities, got, wantDefinition)
			}
		}
	})

	t.Run("verbose_json asks for segment timing unless a granularity is named, json asks for none", func(t *testing.T) {
		got := definition(t, &schemas.TranscriptionParameters{ResponseFormat: schemas.Ptr("verbose_json")}, false)
		want := `{"enhancedMode":{"enabled":true,"model":"MAI-Transcribe-2","modelOptions":{"timestamps":"segment"}}}`
		if got != want {
			t.Errorf("verbose_json: definition = %s, want %s", got, want)
		}
		got = definition(t, &schemas.TranscriptionParameters{ResponseFormat: schemas.Ptr("json")}, false)
		want = `{"enhancedMode":{"enabled":true,"model":"MAI-Transcribe-2"}}`
		if got != want {
			t.Errorf("json: definition = %s, want %s", got, want)
		}
	})

	t.Run("extra fields are merged into the definition with the passthrough header", func(t *testing.T) {
		got := definition(t, &schemas.TranscriptionParameters{
			Language:               schemas.Ptr("en"),
			TimestampGranularities: []string{"word"},
			ExtraParams: map[string]interface{}{
				// As a multipart form delivers them: JSON-encoded strings.
				"diarization":  `{"enabled":true}`,
				"phraseList":   `{"phrases":["Contoso","Jessie"]}`,
				"enhancedMode": `{"modelOptions":{"transcribeStyle":"clean"}}`,
				// As a Go SDK caller passes them: values.
				"profanityFilterMode": "Masked",
			},
		}, true)
		want := `{"diarization":{"enabled":true},` +
			`"enhancedMode":{"enabled":true,"model":"MAI-Transcribe-2","modelOptions":{"timestamps":"word","transcribeStyle":"clean"}},` +
			`"locales":["en"],"phraseList":{"phrases":["Contoso","Jessie"]},"profanityFilterMode":"Masked"}`
		if got != want {
			t.Errorf("definition = %s\nwant         %s", got, want)
		}
	})

	t.Run("mai-transcribe model names get the documented casing", func(t *testing.T) {
		for model, want := range map[string]string{
			"mai-transcribe-2":   "MAI-Transcribe-2",
			"mai-transcribe-1.5": "MAI-Transcribe-1.5",
			"MAI-Transcribe-2":   "MAI-Transcribe-2",
			"some-other-model":   "some-other-model",
		} {
			request := transcriptionRequest(nil)
			request.Model = model
			converted, bifrostErr := azurespeech.ToAzureSpeechTranscriptionRequest(request, false)
			if bifrostErr != nil {
				t.Fatalf("%s: %+v", model, bifrostErr.Error)
			}
			if got := converted.Definition["enhancedMode"].(map[string]interface{})["model"]; got != want {
				t.Errorf("%s: enhancedMode.model = %v, want %s", model, got, want)
			}
		}
	})

	t.Run("values the API cannot take are refused", func(t *testing.T) {
		for _, format := range []string{"text", "srt", "vtt"} {
			_, bifrostErr := azurespeech.ToAzureSpeechTranscriptionRequest(transcriptionRequest(&schemas.TranscriptionParameters{
				ResponseFormat: schemas.Ptr(format),
			}), false)
			requireUnsupportedParameter(t, bifrostErr, "response_format")
		}
		_, bifrostErr := azurespeech.ToAzureSpeechTranscriptionRequest(transcriptionRequest(&schemas.TranscriptionParameters{
			TimestampGranularities: []string{"character"},
		}), false)
		requireUnsupportedParameter(t, bifrostErr, "timestamp_granularities")
	})
}

// response_format diarized_json turns diarization on and returns speaker-labelled segments.
func TestAzureSpeechTranscriptionDiarized(t *testing.T) {
	provider, captured := newMockAzureSpeech(t, map[string]http.HandlerFunc{
		transcriptionPath: bodyResponse(http.StatusOK, "application/json", `{
			"durationMilliseconds": 4000,
			"combinedPhrases": [{"channel": 0, "text": "Hello there. Hi."}],
			"phrases": [
				{"channel": 0, "speaker": 1, "offsetMilliseconds": 0, "durationMilliseconds": 1500, "text": "Hello there.", "locale": "en-US"},
				{"channel": 0, "speaker": 2, "offsetMilliseconds": 2000, "durationMilliseconds": 500, "text": "Hi.", "locale": "en-US"}
			]
		}`),
	})
	response, bifrostErr := provider.Transcription(testContext(t), testKey(), transcriptionRequest(&schemas.TranscriptionParameters{
		ResponseFormat: schemas.Ptr("diarized_json"),
	}))
	if bifrostErr != nil {
		t.Fatalf("Transcription error: %+v", bifrostErr.Error)
	}
	fields, _, _ := multipartParts(t, captured()[0])
	// Segment timestamps are asked for: the API's default is "none", which would leave
	// every diarized segment without its start and end.
	want := `{"diarization":{"enabled":true},"enhancedMode":{"enabled":true,"model":"MAI-Transcribe-2","modelOptions":{"timestamps":"segment"}}}`
	if fields["definition"] != want {
		t.Errorf("definition = %s, want %s", fields["definition"], want)
	}
	if len(response.Segments) != 0 || len(response.DiarizedSegments) != 2 {
		t.Fatalf("segments = %+v diarized = %+v, want two diarized segments", response.Segments, response.DiarizedSegments)
	}
	second := response.DiarizedSegments[1]
	if second.Speaker != "2" || second.Start != 2 || second.End != 2.5 || second.Text != "Hi." || second.Type != "transcript.text.segment" {
		t.Errorf("second diarized segment = %+v", second)
	}
	if response.Usage == nil || response.Usage.Seconds == nil || *response.Usage.Seconds != 4 {
		t.Errorf("usage = %+v, want 4 seconds", response.Usage)
	}
}

// usage.seconds comes from the vendor's durationMilliseconds only: a response without it
// leaves usage empty, it is never measured from the file (contract C5). A 200 with no
// transcription at all is 502 provider_empty_result (contract C6).
func TestAzureSpeechTranscriptionUsageAndEmptyResult(t *testing.T) {
	t.Run("no durationMilliseconds leaves usage empty", func(t *testing.T) {
		provider, _ := newMockAzureSpeech(t, map[string]http.HandlerFunc{
			transcriptionPath: bodyResponse(http.StatusOK, "application/json", `{"combinedPhrases":[{"text":"Hello."}]}`),
		})
		response, bifrostErr := provider.Transcription(testContext(t), testKey(), transcriptionRequest(nil))
		if bifrostErr != nil {
			t.Fatalf("Transcription error: %+v", bifrostErr.Error)
		}
		if response.Text != "Hello." || response.Usage != nil || response.Duration != nil {
			t.Errorf("text = %q usage = %+v duration = %v, want the text and no usage", response.Text, response.Usage, response.Duration)
		}
	})

	t.Run("silent audio is an empty transcript, not an error", func(t *testing.T) {
		provider, _ := newMockAzureSpeech(t, map[string]http.HandlerFunc{
			transcriptionPath: bodyResponse(http.StatusOK, "application/json", `{"durationMilliseconds":1500,"combinedPhrases":[],"phrases":[]}`),
		})
		response, bifrostErr := provider.Transcription(testContext(t), testKey(), transcriptionRequest(nil))
		if bifrostErr != nil {
			t.Fatalf("Transcription error: %+v", bifrostErr.Error)
		}
		if response.Text != "" || response.Usage == nil || *response.Usage.Seconds != 1.5 {
			t.Errorf("text = %q usage = %+v, want an empty text billed for 1.5 seconds", response.Text, response.Usage)
		}
	})

	for name, body := range map[string]string{"empty body": "", "empty object": "{}"} {
		t.Run(name+" is provider_empty_result", func(t *testing.T) {
			provider, _ := newMockAzureSpeech(t, map[string]http.HandlerFunc{
				transcriptionPath: bodyResponse(http.StatusOK, "application/json", body),
			})
			_, bifrostErr := provider.Transcription(testContext(t), testKey(), transcriptionRequest(nil))
			if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusBadGateway {
				t.Fatalf("error = %+v, want a 502", bifrostErr)
			}
			if bifrostErr.ExtraFields.ErrorType != schemas.ErrorTypeProviderEmptyResult {
				t.Errorf("error_type = %q, want %q", bifrostErr.ExtraFields.ErrorType, schemas.ErrorTypeProviderEmptyResult)
			}
		})
	}
}

// A vendor error keeps its status and carries the vendor's code and message.
func TestAzureSpeechTranscriptionVendorErrorKeepsStatus(t *testing.T) {
	provider, captured := newMockAzureSpeech(t, map[string]http.HandlerFunc{
		transcriptionPath: bodyResponse(http.StatusUnprocessableEntity, "application/json",
			`{"code":"InvalidRequest","message":"The request is invalid.","innerError":{"code":"InvalidAudio","message":"The audio format is not supported."}}`),
	})
	_, bifrostErr := provider.Transcription(testContext(t), testKey(), transcriptionRequest(nil))
	if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("error = %+v, want the vendor's 422", bifrostErr)
	}
	if bifrostErr.Error.Code == nil || *bifrostErr.Error.Code != "InvalidRequest" {
		t.Errorf("code = %v, want InvalidRequest", bifrostErr.Error.Code)
	}
	if bifrostErr.Error.Message != "The request is invalid. (The audio format is not supported.)" {
		t.Errorf("message = %q", bifrostErr.Error.Message)
	}
	if got := bifrostErr.ResolveErrorType(); got != schemas.ErrorTypeCallerInvalidRequest {
		t.Errorf("error_type = %q, want %q", got, schemas.ErrorTypeCallerInvalidRequest)
	}
	if n := len(captured()); n != 1 {
		t.Errorf("upstream calls = %d, want exactly 1", n)
	}
}

// The operations Azure AI Speech does not have answer unsupported_operation.
func TestAzureSpeechUnsupportedOperations(t *testing.T) {
	provider, captured := newMockAzureSpeech(t, nil)
	if _, bifrostErr := provider.ChatCompletion(testContext(t), testKey(), &schemas.BifrostChatRequest{}); bifrostErr == nil {
		t.Error("ChatCompletion must be unsupported")
	}
	if _, bifrostErr := provider.TranscriptionStream(testContext(t), passthroughPostHook, func(context.Context) {}, testKey(), transcriptionRequest(nil)); bifrostErr == nil {
		t.Error("TranscriptionStream must be unsupported")
	}
	if _, bifrostErr := provider.ListModels(testContext(t), []schemas.Key{testKey()}, &schemas.BifrostListModelsRequest{}); bifrostErr == nil {
		t.Error("ListModels must be unsupported")
	}
	if n := len(captured()); n != 0 {
		t.Errorf("upstream calls = %d, want 0", n)
	}
}

// BenchmarkToAzureSpeechSSML measures the per-request cost of building the SSML body.
func BenchmarkToAzureSpeechSSML(b *testing.B) {
	request := speechRequest(maiVoice, &schemas.SpeechParameters{Speed: schemas.Ptr(1.25), ResponseFormat: "mp3"})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, bifrostErr := azurespeech.ToAzureSpeechSSML(request); bifrostErr != nil {
			b.Fatal(bifrostErr.Error.Message)
		}
	}
}

// BenchmarkToAzureSpeechTranscriptionRequest measures the per-request cost of building the
// transcription definition.
func BenchmarkToAzureSpeechTranscriptionRequest(b *testing.B) {
	request := transcriptionRequest(&schemas.TranscriptionParameters{
		Language:               schemas.Ptr("en"),
		TimestampGranularities: []string{"word"},
	})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, bifrostErr := azurespeech.ToAzureSpeechTranscriptionRequest(request, false); bifrostErr != nil {
			b.Fatal(bifrostErr.Error.Message)
		}
	}
}
