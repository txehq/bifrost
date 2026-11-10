package googlecloudtts_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/providers/googlecloudtts"
	"github.com/capsohq/bifrost/core/schemas"
)

// The request and response samples are Google's own, from "Create audio from text by
// using the command line"
// (https://docs.cloud.google.com/text-to-speech/docs/create-audio-text-command-line),
// with a WaveNet voice of the same locale in place of the Standard one. The documented
// audioContent is shortened there; sampleAudioContent is its first 52 characters.
const (
	sampleText         = "Android is a mobile operating system developed by Google, based on the Linux kernel and designed primarily for touchscreen mobile devices such as smartphones and tablets."
	sampleVoice        = "en-GB-Wavenet-A"
	sampleLanguageCode = "en-gb"
	sampleAudioContent = "//NExAASCCIIAAhEAGAAEMW4kAYPnwwIKw/BBTpwTvB+IAxIfghU"
	sampleResponseBody = `{"audioContent": "` + sampleAudioContent + `"}`

	// Google's error envelope (google.rpc.Status in JSON) for a voice that does not exist.
	invalidArgumentBody = `{"error":{"code":400,"message":"Voice 'en-GB-Wavenet-ZZ' does not exist. Is it misspelled?","status":"INVALID_ARGUMENT"}}`
)

type capturedRequest struct {
	method string
	path   string
	query  string
	header http.Header
	body   []byte
}

// newMockCloudTTS starts a server that records every request and answers each with respond.
func newMockCloudTTS(t *testing.T, respond func(w http.ResponseWriter)) (*googlecloudtts.GoogleCloudTTSProvider, func() []capturedRequest) {
	t.Helper()
	var mu sync.Mutex
	var captured []capturedRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		captured = append(captured, capturedRequest{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, header: r.Header.Clone(), body: body})
		mu.Unlock()
		respond(w)
	}))
	t.Cleanup(server.Close)

	provider, err := googlecloudtts.NewGoogleCloudTTSProvider(&schemas.ProviderConfig{
		NetworkConfig: schemas.NetworkConfig{
			BaseURL:                        server.URL,
			DefaultRequestTimeoutInSeconds: 10,
			AllowPrivateNetwork:            true,
		},
	}, bifrost.NewDefaultLogger(schemas.LogLevelError))
	if err != nil {
		t.Fatalf("NewGoogleCloudTTSProvider: %v", err)
	}
	return provider, func() []capturedRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]capturedRequest(nil), captured...)
	}
}

func jsonResponse(status int, body string) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json; charset=UTF-8")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

func testContext(t *testing.T) *schemas.BifrostContext {
	t.Helper()
	ctx, cancel := schemas.NewBifrostContextWithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func testKey() schemas.Key {
	return schemas.Key{Value: *schemas.NewSecretVar("test-key"), Models: []string{"*"}}
}

// speechRequest builds a wavenet request. A request must name a voice of its model's family,
// so params that name no voice get sampleVoice.
func speechRequest(input string, params *schemas.SpeechParameters) *schemas.BifrostSpeechRequest {
	withVoice := schemas.SpeechParameters{}
	if params != nil {
		withVoice = *params
	}
	if withVoice.VoiceConfig == nil {
		withVoice.VoiceConfig = voice(sampleVoice)
	}
	params = &withVoice
	return &schemas.BifrostSpeechRequest{
		Provider: schemas.GoogleCloudTTS,
		Model:    "wavenet",
		Input:    &schemas.SpeechInput{Input: input},
		Params:   params,
	}
}

func voice(name string) *schemas.SpeechVoiceInput {
	return &schemas.SpeechVoiceInput{Voice: schemas.Ptr(name)}
}

// sentBody decodes the one request body the mock received.
func sentBody(t *testing.T, reqs []capturedRequest) map[string]any {
	t.Helper()
	if len(reqs) != 1 {
		t.Fatalf("expected 1 upstream request, got %d", len(reqs))
	}
	var body map[string]any
	if err := json.Unmarshal(reqs[0].body, &body); err != nil {
		t.Fatalf("upstream body is not JSON: %v: %s", err, reqs[0].body)
	}
	return body
}

func TestSpeechSendsSynthesizeBodyAndReturnsDecodedAudio(t *testing.T) {
	provider, captured := newMockCloudTTS(t, jsonResponse(http.StatusOK, sampleResponseBody))

	resp, bifrostErr := provider.Speech(testContext(t), testKey(), speechRequest(sampleText, &schemas.SpeechParameters{
		VoiceConfig:    voice(sampleVoice),
		LanguageCode:   schemas.Ptr(sampleLanguageCode),
		ResponseFormat: "mp3",
		Speed:          schemas.Ptr(1.25),
	}))
	if bifrostErr != nil {
		t.Fatalf("Speech error: %+v", bifrostErr.Error)
	}

	reqs := captured()
	got := sentBody(t, reqs)
	want := map[string]any{
		"input":       map[string]any{"text": sampleText},
		"voice":       map[string]any{"languageCode": sampleLanguageCode, "name": sampleVoice},
		"audioConfig": map[string]any{"audioEncoding": "MP3", "speakingRate": 1.25},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("upstream body = %s, want %v", reqs[0].body, want)
	}
	if reqs[0].method != http.MethodPost || reqs[0].path != "/v1/text:synthesize" || reqs[0].query != "" {
		t.Errorf("upstream request = %s %s?%s, want POST /v1/text:synthesize", reqs[0].method, reqs[0].path, reqs[0].query)
	}
	if key := reqs[0].header.Get("x-goog-api-key"); key != "test-key" {
		t.Errorf("x-goog-api-key = %q, want the key value", key)
	}
	if auth := reqs[0].header.Get("Authorization"); auth != "" {
		t.Errorf("Authorization = %q, want none: the key travels in x-goog-api-key only", auth)
	}

	wantAudio, err := base64.StdEncoding.DecodeString(sampleAudioContent)
	if err != nil {
		t.Fatalf("sample audio is not base64: %v", err)
	}
	if !bytes.Equal(resp.Audio, wantAudio) {
		t.Errorf("audio = %d bytes, want the %d decoded bytes of audioContent", len(resp.Audio), len(wantAudio))
	}
	// The vendor reports no usage, so the provider reports none (contract C5).
	if resp.Usage != nil {
		t.Errorf("usage = %+v, want none: text:synthesize reports no usage", resp.Usage)
	}
	if resp.AudioBase64 != nil || resp.Alignment != nil {
		t.Errorf("response carries fields the vendor did not send: %+v", resp)
	}
}

func TestSpeechRequestMapping(t *testing.T) {
	ssml := `<speak>Hello <break time="300ms"/> world</speak>`
	tests := []struct {
		name   string
		input  string
		params *schemas.SpeechParameters
		want   map[string]any
	}{
		{
			name:  "no params sends the required audioEncoding as mp3",
			input: "hi",
			want: map[string]any{
				"input": map[string]any{"text": "hi"}, "voice": map[string]any{"name": sampleVoice}, "audioConfig": map[string]any{"audioEncoding": "MP3"},
			},
		},
		{
			name:   "an SSML document goes to input.ssml",
			input:  ssml,
			params: &schemas.SpeechParameters{VoiceConfig: voice("en-US-Wavenet-D"), LanguageCode: schemas.Ptr("en-US")},
			want: map[string]any{
				"input": map[string]any{"ssml": ssml}, "voice": map[string]any{"languageCode": "en-US", "name": "en-US-Wavenet-D"}, "audioConfig": map[string]any{"audioEncoding": "MP3"},
			},
		},
		{
			name:  "an XML declaration and a comment may precede the speak root",
			input: `<?xml version="1.0"?>` + "\n<!-- greeting -->\n" + ssml,
			want: map[string]any{
				"input": map[string]any{"ssml": `<?xml version="1.0"?>` + "\n<!-- greeting -->\n" + ssml}, "voice": map[string]any{"name": sampleVoice}, "audioConfig": map[string]any{"audioEncoding": "MP3"},
			},
		},
		{
			name:  "a declaration before another root element stays text",
			input: `<?xml version="1.0"?><note>hi</note>`,
			want: map[string]any{
				"input": map[string]any{"text": `<?xml version="1.0"?><note>hi</note>`}, "voice": map[string]any{"name": sampleVoice}, "audioConfig": map[string]any{"audioEncoding": "MP3"},
			},
		},
		{
			name:  "an unterminated comment stays text",
			input: "<!-- <speak>hi</speak>",
			want: map[string]any{
				"input": map[string]any{"text": "<!-- <speak>hi</speak>"}, "voice": map[string]any{"name": sampleVoice}, "audioConfig": map[string]any{"audioEncoding": "MP3"},
			},
		},
		{
			name:  "text that only mentions a speak tag stays text",
			input: "<speaker> is not SSML, and neither is a late <speak>",
			want: map[string]any{
				"input": map[string]any{"text": "<speaker> is not SSML, and neither is a late <speak>"}, "voice": map[string]any{"name": sampleVoice}, "audioConfig": map[string]any{"audioEncoding": "MP3"},
			},
		},
		{
			name:   "opus is Opus in an Ogg container",
			input:  "hi",
			params: &schemas.SpeechParameters{ResponseFormat: "opus"},
			want: map[string]any{
				"input": map[string]any{"text": "hi"}, "voice": map[string]any{"name": sampleVoice}, "audioConfig": map[string]any{"audioEncoding": "OGG_OPUS"},
			},
		},
		{
			name:   "wav is LINEAR16, which carries a WAV header",
			input:  "hi",
			params: &schemas.SpeechParameters{ResponseFormat: "wav"},
			want: map[string]any{
				"input": map[string]any{"text": "hi"}, "voice": map[string]any{"name": sampleVoice}, "audioConfig": map[string]any{"audioEncoding": "LINEAR16"},
			},
		},
		{
			name:   "pcm is headerless PCM",
			input:  "hi",
			params: &schemas.SpeechParameters{ResponseFormat: "pcm"},
			want: map[string]any{
				"input": map[string]any{"text": "hi"}, "voice": map[string]any{"name": sampleVoice}, "audioConfig": map[string]any{"audioEncoding": "PCM"},
			},
		},
		{
			name:   "a vendor encoding is taken under its own name",
			input:  "hi",
			params: &schemas.SpeechParameters{ResponseFormat: "MULAW"},
			want: map[string]any{
				"input": map[string]any{"text": "hi"}, "voice": map[string]any{"name": sampleVoice}, "audioConfig": map[string]any{"audioEncoding": "MULAW"},
			},
		},
		{
			name:   "the speaking rate bounds are sent as given",
			input:  "hi",
			params: &schemas.SpeechParameters{Speed: schemas.Ptr(0.25)},
			want: map[string]any{
				"input": map[string]any{"text": "hi"}, "voice": map[string]any{"name": sampleVoice}, "audioConfig": map[string]any{"audioEncoding": "MP3", "speakingRate": 0.25},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider, captured := newMockCloudTTS(t, jsonResponse(http.StatusOK, sampleResponseBody))
			if _, bifrostErr := provider.Speech(testContext(t), testKey(), speechRequest(tt.input, tt.params)); bifrostErr != nil {
				t.Fatalf("Speech error: %+v", bifrostErr.Error)
			}
			reqs := captured()
			if got := sentBody(t, reqs); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("upstream body = %s, want %v", reqs[0].body, tt.want)
			}
		})
	}
}

// Vendor fields outside the unified structure reach text:synthesize through the
// extra-params channel, merged into the object the mapped fields are in (contract C4).
func TestSpeechMergesExtraParamsIntoTheBody(t *testing.T) {
	provider, captured := newMockCloudTTS(t, jsonResponse(http.StatusOK, sampleResponseBody))
	ctx := testContext(t)
	ctx.SetValue(schemas.BifrostContextKeyPassthroughExtraParams, true)

	_, bifrostErr := provider.Speech(ctx, testKey(), speechRequest("hi", &schemas.SpeechParameters{
		VoiceConfig:    voice("en-US-Wavenet-D"),
		LanguageCode:   schemas.Ptr("en-US"),
		ResponseFormat: "wav",
		// The speech route leaves language_code among the extra params as well as parsing it;
		// it is mapped to voice.languageCode and must not reach the vendor a second time.
		ExtraParams: map[string]any{
			"audioConfig":   map[string]any{"pitch": -2.0, "sampleRateHertz": 16000.0},
			"language_code": "en-US",
		},
	}))
	if bifrostErr != nil {
		t.Fatalf("Speech error: %+v", bifrostErr.Error)
	}
	reqs := captured()
	want := map[string]any{
		"input":       map[string]any{"text": "hi"},
		"voice":       map[string]any{"languageCode": "en-US", "name": "en-US-Wavenet-D"},
		"audioConfig": map[string]any{"audioEncoding": "LINEAR16", "pitch": -2.0, "sampleRateHertz": 16000.0},
	}
	if got := sentBody(t, reqs); !reflect.DeepEqual(got, want) {
		t.Errorf("upstream body = %s, want %v", reqs[0].body, want)
	}
}

func TestSpeechRefusesWhatTheVendorCannotTake(t *testing.T) {
	tests := []struct {
		name      string
		params    *schemas.SpeechParameters
		wantParam string
	}{
		{"a format with no AudioEncoding", &schemas.SpeechParameters{ResponseFormat: "aac"}, "response_format"},
		{"flac", &schemas.SpeechParameters{ResponseFormat: "flac"}, "response_format"},
		{"a speed above the range is not clamped", &schemas.SpeechParameters{Speed: schemas.Ptr(2.5)}, "speed"},
		{"a speed below the range is not clamped", &schemas.SpeechParameters{Speed: schemas.Ptr(0.1)}, "speed"},
		{"a multi-speaker voice list", &schemas.SpeechParameters{VoiceConfig: &schemas.SpeechVoiceInput{MultiVoiceConfig: []schemas.VoiceConfig{{Speaker: "a", Voice: "en-US-Wavenet-D"}}}}, "voice"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider, captured := newMockCloudTTS(t, jsonResponse(http.StatusOK, sampleResponseBody))
			_, bifrostErr := provider.Speech(testContext(t), testKey(), speechRequest("hi", tt.params))
			assertUnsupportedParameter(t, bifrostErr, tt.wantParam)
			if n := len(captured()); n != 0 {
				t.Errorf("made %d upstream calls for a refused request, want 0", n)
			}
		})
	}
}

// The model is what governance admits and what the caller is priced on; text:synthesize has
// no model field, so the voice name alone decides the family Google serves. A voice of
// another family, a name with no family, or no voice at all is refused before any upstream call.
func TestSpeechRefusesAVoiceOutsideTheModelFamily(t *testing.T) {
	tests := []struct {
		name  string
		model string
		voice *schemas.SpeechVoiceInput
	}{
		{"a Chirp3-HD voice under wavenet", "wavenet", voice("en-US-Chirp3-HD-Achernar")},
		{"a Studio voice under standard", "standard", voice("en-US-Studio-O")},
		{"a Chirp-HD voice under chirp3-hd", "chirp3-hd", voice("en-US-Chirp-HD-F")},
		{"a WaveNet voice under a model that is no family", "tts-1", voice("en-US-Wavenet-D")},
		{"a named voice under instant-custom-voice", "instant-custom-voice", voice("en-US-Wavenet-D")},
		{"a name with no family", "wavenet", voice("alloy")},
		{"a name with an empty part", "wavenet", voice("en-US--D")},
		{"an empty voice name", "wavenet", voice("")},
		{"no voice", "wavenet", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider, captured := newMockCloudTTS(t, jsonResponse(http.StatusOK, sampleResponseBody))
			req := &schemas.BifrostSpeechRequest{
				Provider: schemas.GoogleCloudTTS,
				Model:    tt.model,
				Input:    &schemas.SpeechInput{Input: "hi"},
				Params:   &schemas.SpeechParameters{VoiceConfig: tt.voice, LanguageCode: schemas.Ptr("en-US")},
			}
			_, bifrostErr := provider.Speech(testContext(t), testKey(), req)
			assertUnsupportedParameter(t, bifrostErr, "voice")
			if n := len(captured()); n != 0 {
				t.Errorf("made %d upstream calls for a refused request, want 0", n)
			}
		})
	}

	t.Run("no params at all", func(t *testing.T) {
		provider, captured := newMockCloudTTS(t, jsonResponse(http.StatusOK, sampleResponseBody))
		req := &schemas.BifrostSpeechRequest{Provider: schemas.GoogleCloudTTS, Model: "wavenet", Input: &schemas.SpeechInput{Input: "hi"}}
		_, bifrostErr := provider.Speech(testContext(t), testKey(), req)
		assertUnsupportedParameter(t, bifrostErr, "voice")
		if n := len(captured()); n != 0 {
			t.Errorf("made %d upstream calls for a refused request, want 0", n)
		}
	})
}

// Each priced family takes its own voices; the model name is compared without regard to case.
func TestSpeechTakesAVoiceOfTheModelFamily(t *testing.T) {
	for model, voiceName := range map[string]string{
		"wavenet":   "cmn-CN-Wavenet-A",
		"neural2":   "en-US-Neural2-A",
		"standard":  "yue-HK-Standard-A",
		"studio":    "en-US-Studio-O",
		"polyglot":  "en-US-Polyglot-1",
		"chirp3-hd": "en-US-Chirp3-HD-Achernar",
		"Chirp3-HD": "en-US-Chirp3-HD-Achernar",
	} {
		t.Run(model, func(t *testing.T) {
			provider, captured := newMockCloudTTS(t, jsonResponse(http.StatusOK, sampleResponseBody))
			req := &schemas.BifrostSpeechRequest{
				Provider: schemas.GoogleCloudTTS,
				Model:    model,
				Input:    &schemas.SpeechInput{Input: "hi"},
				Params:   &schemas.SpeechParameters{VoiceConfig: voice(voiceName)},
			}
			if _, bifrostErr := provider.Speech(testContext(t), testKey(), req); bifrostErr != nil {
				t.Fatalf("Speech error: %+v", bifrostErr.Error)
			}
			reqs := captured()
			if len(reqs) != 1 {
				t.Fatalf("made %d upstream calls, want exactly 1", len(reqs))
			}
			if got := sentBody(t, reqs)["voice"]; !reflect.DeepEqual(got, map[string]any{"name": voiceName}) {
				t.Errorf("upstream voice = %v, want the name %q as given", got, voiceName)
			}
		})
	}
}

// Google lets an SSML <voice name="..."> element read part of the input in a voice of another
// family (https://docs.cloud.google.com/text-to-speech/docs/ssml#select_a_voice), so the voice
// parameter alone does not bind the family. An SSML document that selects a voice outside the
// model's family, or in a way whose family cannot be established, is refused on input before
// any upstream call.
func TestSpeechRefusesAnSSMLVoiceOutsideTheModelFamily(t *testing.T) {
	tests := map[string]string{
		"a Studio voice inside a wavenet request": `<speak><voice name="en-US-Studio-O">Hello</voice></speak>`,
		"single quotes":                                    `<speak><voice name='en-US-Studio-O'>Hello</voice></speak>`,
		"spaces around the equals sign":                    `<speak><voice name = "en-US-Studio-O">Hello</voice></speak>`,
		"the name after another attribute":                 `<speak><voice language="en-US" name="en-US-Neural2-A">Hello</voice></speak>`,
		"a greater-than sign in another attribute":         `<speak><voice variant="a>b" name="en-US-Studio-O">Hello</voice></speak>`,
		"an upper-case tag":                                `<speak><VOICE NAME="en-US-Studio-O">Hello</VOICE></speak>`,
		"the second of two voices":                         `<speak><voice name="en-US-Wavenet-A">Hi</voice><voice name="en-US-Studio-O">Hello</voice></speak>`,
		"a voice selected by gender":                       `<speak><voice gender="female">Hello</voice></speak>`,
		"a voice with no attributes":                       `<speak><voice>Hello</voice></speak>`,
		"a name with no family":                            `<speak><voice name="alloy">Hello</voice></speak>`,
		"a character reference that spells another family": `<speak><voice name="en-US-&#83;tudio-O">Hello</voice></speak>`,
		"an undefined entity in the name":                  `<speak><voice name="en-US-Wavenet-&who;">Hello</voice></speak>`,
		// A commented-out <voice whose unclosed attribute would swallow the real element in a
		// scanner that does not know XML comments.
		"a real voice element between two comments": `<speak><!-- <voice name="en-US-Wavenet-B" x=" --><voice name='en-US-Studio-O'>Hello</voice><!-- " > --></speak>`,
		"a voice element after a CDATA section":     `<speak><![CDATA[<voice name="en-US-Wavenet-A" x="]]><voice name="en-US-Studio-O">Hello</voice></speak>`,
		"a namespace prefix":                        `<speak xmlns:s="urn:s"><s:voice name="en-US-Studio-O">Hello</s:voice></speak>`,
		"a document type inside the document":       `<speak><!DOCTYPE speak [<!ENTITY v "x">]>Hello</speak>`,
		"an unquoted value":                         `<speak><voice name=en-US-Wavenet-A>Hello</voice></speak>`,
		"an unterminated value":                     `<speak><voice name="en-US-Wavenet-A>Hello</voice></speak>`,
		"a tag cut off at the end":                  `<speak>Hello <voice`,
		"behind an XML declaration":                 `<?xml version="1.0"?><speak><voice name="en-US-Studio-O">Hello</voice></speak>`,
	}
	for name, ssml := range tests {
		t.Run(name, func(t *testing.T) {
			provider, captured := newMockCloudTTS(t, jsonResponse(http.StatusOK, sampleResponseBody))
			_, bifrostErr := provider.Speech(testContext(t), testKey(), speechRequest(ssml, &schemas.SpeechParameters{VoiceConfig: voice("en-US-Wavenet-D")}))
			assertUnsupportedParameter(t, bifrostErr, "input")
			if n := len(captured()); n != 0 {
				t.Errorf("made %d upstream calls for a refused request, want 0", n)
			}
		})
	}
}

// <voice> elements of the model's own family are sent as written, and so is text that only
// mentions the tag.
func TestSpeechTakesSSMLVoicesOfTheModelFamily(t *testing.T) {
	tests := map[string]struct {
		input string
		field string
	}{
		"two wavenet voices":                  {`<speak><voice name="en-US-Wavenet-A">Hi</voice> <voice name='en-GB-Wavenet-B' >Hello</voice></speak>`, "ssml"},
		"another element starting with voice": {`<speak><voiceover>Hi</voiceover></speak>`, "ssml"},
		"an escaped tag is text":              {`<speak>Write &lt;voice name="en-US-Studio-O"&gt; to switch.</speak>`, "ssml"},
		"a voice element inside a comment":    {`<speak><!-- <voice name="en-US-Studio-O"> -->Hello</speak>`, "ssml"},
		"a voice element inside CDATA":        {`<speak><![CDATA[<voice name="en-US-Studio-O">]]>Hello</speak>`, "ssml"},
		"a character reference in the name":   {`<speak><voice name="en-US-Wavenet-&#65;">Hello</voice></speak>`, "ssml"},
		// A document that opens with a document type is not taken as SSML at all: it goes to
		// input.text, where Google reads no markup.
		"a document type before the root is text": {`<!DOCTYPE speak [<!ENTITY v "&#60;voice name='en-US-Studio-O'&#62;Hello&#60;/voice&#62;">]><speak>&v;</speak>`, "text"},
		"a mid-document doctype word is text":     {`Write <!DOCTYPE speak> before <voice name="en-US-Studio-O">.`, "text"},
		"plain text is not scanned":               {`To switch, write <voice name="en-US-Studio-O">.`, "text"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			provider, captured := newMockCloudTTS(t, jsonResponse(http.StatusOK, sampleResponseBody))
			if _, bifrostErr := provider.Speech(testContext(t), testKey(), speechRequest(tt.input, &schemas.SpeechParameters{VoiceConfig: voice("en-US-Wavenet-D")})); bifrostErr != nil {
				t.Fatalf("Speech error: %+v", bifrostErr.Error)
			}
			reqs := captured()
			if len(reqs) != 1 {
				t.Fatalf("made %d upstream calls, want exactly 1", len(reqs))
			}
			if got := sentBody(t, reqs)["input"]; !reflect.DeepEqual(got, map[string]any{tt.field: tt.input}) {
				t.Errorf("upstream input = %v, want %s as given", got, tt.field)
			}
		})
	}
}

// The extra-params channel merges caller fields into the body after the voice and the SSML
// have been checked, so it may not carry voice or input, whether or not the merge is on.
func TestSpeechRefusesProtectedFieldsAsExtraParams(t *testing.T) {
	for _, field := range []string{"voice", "input"} {
		for _, passthrough := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s passthrough=%v", field, passthrough), func(t *testing.T) {
				provider, captured := newMockCloudTTS(t, jsonResponse(http.StatusOK, sampleResponseBody))
				ctx := testContext(t)
				ctx.SetValue(schemas.BifrostContextKeyPassthroughExtraParams, passthrough)
				_, bifrostErr := provider.Speech(ctx, testKey(), speechRequest("hi", &schemas.SpeechParameters{
					VoiceConfig: voice("en-US-Wavenet-D"),
					ExtraParams: map[string]any{field: map[string]any{"name": "en-US-Chirp3-HD-Achernar", "ssml": `<speak><voice name="en-US-Studio-O">hi</voice></speak>`}},
				}))
				assertUnsupportedParameter(t, bifrostErr, field)
				if n := len(captured()); n != 0 {
					t.Errorf("made %d upstream calls for a refused request, want 0", n)
				}
			})
		}
	}
}

// The body is built from the unified request even when the caller asks for its own body to be
// sent as it is: a body the checks did not see could name any voice.
func TestSpeechNeverSendsTheCallersRawBody(t *testing.T) {
	provider, captured := newMockCloudTTS(t, jsonResponse(http.StatusOK, sampleResponseBody))
	ctx := testContext(t)
	ctx.SetValue(schemas.BifrostContextKeyUseRawRequestBody, true)
	req := speechRequest("hi", &schemas.SpeechParameters{VoiceConfig: voice("en-US-Wavenet-D")})
	req.RawRequestBody = []byte(`{"input":{"text":"hi"},"voice":{"name":"en-US-Chirp3-HD-Achernar"},"audioConfig":{"audioEncoding":"MP3"}}`)
	if _, bifrostErr := provider.Speech(ctx, testKey(), req); bifrostErr != nil {
		t.Fatalf("Speech error: %+v", bifrostErr.Error)
	}
	want := map[string]any{
		"input": map[string]any{"text": "hi"}, "voice": map[string]any{"name": "en-US-Wavenet-D"}, "audioConfig": map[string]any{"audioEncoding": "MP3"},
	}
	if got := sentBody(t, captured()); !reflect.DeepEqual(got, want) {
		t.Errorf("upstream body = %v, want the body built from the unified request %v", got, want)
	}
}

func assertUnsupportedParameter(t *testing.T, bifrostErr *schemas.BifrostError, wantParam string) {
	t.Helper()
	if bifrostErr == nil {
		t.Fatal("expected caller_unsupported_parameter, got no error")
	}
	if bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %v, want 400", bifrostErr.StatusCode)
	}
	if bifrostErr.ExtraFields.ErrorType != schemas.ErrorTypeCallerUnsupportedParameter {
		t.Errorf("error_type = %q, want %q", bifrostErr.ExtraFields.ErrorType, schemas.ErrorTypeCallerUnsupportedParameter)
	}
	if bifrostErr.Error == nil || bifrostErr.Error.Param != wantParam {
		t.Errorf("error.param = %+v, want %q", bifrostErr.Error, wantParam)
	}
}

// The fields the provider does not take at all are refused by core from the provider's
// declaration, before a key is chosen.
func TestDeclaredUnifiedFields(t *testing.T) {
	provider, _ := newMockCloudTTS(t, jsonResponse(http.StatusOK, sampleResponseBody))

	supported := &schemas.SpeechParameters{VoiceConfig: voice(sampleVoice), LanguageCode: schemas.Ptr("en-GB"), ResponseFormat: "mp3", Speed: schemas.Ptr(1.0)}
	for _, requestType := range []schemas.RequestType{schemas.SpeechRequest, schemas.SpeechStreamRequest} {
		req := &schemas.BifrostRequest{RequestType: requestType, SpeechRequest: speechRequest("hi", supported)}
		if bifrostErr := schemas.CheckDeclaredUnifiedFields(provider, req); bifrostErr != nil {
			t.Errorf("%s: the mapped fields were refused: %+v", requestType, bifrostErr.Error)
		}
	}

	refused := []struct {
		name      string
		params    *schemas.SpeechParameters
		wantParam string
	}{
		{"instructions", &schemas.SpeechParameters{VoiceConfig: voice(sampleVoice), Instructions: "speak slowly"}, "instructions"},
		{"with_timestamps", &schemas.SpeechParameters{VoiceConfig: voice(sampleVoice), WithTimestamps: schemas.Ptr(true)}, "with_timestamps"},
		{"pronunciation_dictionary_locators", &schemas.SpeechParameters{VoiceConfig: voice(sampleVoice), PronunciationDictionaryLocators: []schemas.SpeechPronunciationDictionaryLocator{{PronunciationDictionaryID: "d"}}}, "pronunciation_dictionary_locators"},
	}
	for _, tt := range refused {
		t.Run(tt.name, func(t *testing.T) {
			req := &schemas.BifrostRequest{RequestType: schemas.SpeechRequest, SpeechRequest: speechRequest("hi", tt.params)}
			assertUnsupportedParameter(t, schemas.CheckDeclaredUnifiedFields(provider, req), tt.wantParam)
		})
	}

	if _, declared := provider.SupportedUnifiedFields(schemas.ChatCompletionRequest); declared {
		t.Error("declared unified fields for an operation the provider does not serve")
	}
}

// stream_format selects the stream route, and text:synthesize has no stream to deliver.
func TestSpeechStreamRefusesStreamFormat(t *testing.T) {
	provider, captured := newMockCloudTTS(t, jsonResponse(http.StatusOK, sampleResponseBody))
	stream, bifrostErr := provider.SpeechStream(testContext(t), nil, nil, testKey(), speechRequest("hi", &schemas.SpeechParameters{VoiceConfig: voice(sampleVoice)}))
	if stream != nil {
		t.Error("got a stream from a provider that cannot stream")
	}
	assertUnsupportedParameter(t, bifrostErr, "stream_format")
	if n := len(captured()); n != 0 {
		t.Errorf("made %d upstream calls for a refused request, want 0", n)
	}
}

func TestSpeechVendorErrorKeepsStatusAndClassifies(t *testing.T) {
	tests := []struct {
		name           string
		status         int
		body           string
		wantMessage    string
		wantCode       string
		wantType       string
		wantErrorType  schemas.ErrorType
		wantRetryAfter int64
	}{
		{
			name:          "invalid argument",
			status:        http.StatusBadRequest,
			body:          invalidArgumentBody,
			wantMessage:   "Voice 'en-GB-Wavenet-ZZ' does not exist. Is it misspelled?",
			wantCode:      "400",
			wantType:      "INVALID_ARGUMENT",
			wantErrorType: schemas.ErrorTypeCallerInvalidRequest,
		},
		{
			name:          "a rejected key",
			status:        http.StatusForbidden,
			body:          `{"error":{"code":403,"message":"The request is missing a valid API key.","status":"PERMISSION_DENIED"}}`,
			wantMessage:   "The request is missing a valid API key.",
			wantCode:      "403",
			wantType:      "PERMISSION_DENIED",
			wantErrorType: schemas.ErrorTypeProviderAuthFailed,
		},
		{
			name:           "quota exhausted with a retry hint",
			status:         http.StatusTooManyRequests,
			body:           `{"error":{"code":429,"message":"Quota exceeded for quota metric 'All requests per minute'.","status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"39s"}]}}`,
			wantMessage:    "Quota exceeded for quota metric 'All requests per minute'.",
			wantCode:       "429",
			wantType:       "RESOURCE_EXHAUSTED",
			wantErrorType:  schemas.ErrorTypeProviderRateLimited,
			wantRetryAfter: 39000,
		},
		{
			name:          "a vendor failure",
			status:        http.StatusInternalServerError,
			body:          `{"error":{"code":500,"message":"Internal error encountered.","status":"INTERNAL"}}`,
			wantMessage:   "Internal error encountered.",
			wantCode:      "500",
			wantType:      "INTERNAL",
			wantErrorType: schemas.ErrorTypeProviderServerError,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider, captured := newMockCloudTTS(t, jsonResponse(tt.status, tt.body))
			resp, bifrostErr := provider.Speech(testContext(t), testKey(), speechRequest("hi", &schemas.SpeechParameters{VoiceConfig: voice("en-GB-Wavenet-ZZ"), LanguageCode: schemas.Ptr("en-GB")}))
			if resp != nil || bifrostErr == nil {
				t.Fatalf("expected an error, got response %+v", resp)
			}
			if bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != tt.status {
				t.Errorf("status = %v, want the vendor's %d", bifrostErr.StatusCode, tt.status)
			}
			if bifrostErr.IsBifrostError {
				t.Error("a vendor error is marked as a Bifrost error")
			}
			if bifrostErr.Error == nil || bifrostErr.Error.Message != tt.wantMessage {
				t.Fatalf("error = %+v, want message %q", bifrostErr.Error, tt.wantMessage)
			}
			if bifrostErr.Error.Code == nil || *bifrostErr.Error.Code != tt.wantCode {
				t.Errorf("error.code = %v, want %q", bifrostErr.Error.Code, tt.wantCode)
			}
			if bifrostErr.Error.Type == nil || *bifrostErr.Error.Type != tt.wantType {
				t.Errorf("error.type = %v, want %q", bifrostErr.Error.Type, tt.wantType)
			}
			if bifrostErr.ExtraFields.RetryAfter != tt.wantRetryAfter {
				t.Errorf("retry_after_ms = %d, want %d", bifrostErr.ExtraFields.RetryAfter, tt.wantRetryAfter)
			}
			// What the transport writes as extra_fields.error_type (contract C6).
			if got := schemas.ClassifyErrorType(bifrostErr, schemas.SpeechRequest); got != tt.wantErrorType {
				t.Errorf("error_type = %q, want %q", got, tt.wantErrorType)
			}
			if n := len(captured()); n != 1 {
				t.Errorf("made %d upstream calls, want exactly 1", n)
			}
		})
	}
}

// A 200 with no audio and no error object is the vendor's failure: 502
// provider_empty_result, not a caller error.
func TestSpeechEmptyAudioIsProviderEmptyResult(t *testing.T) {
	for name, body := range map[string]string{"no audioContent": `{}`, "empty audioContent": `{"audioContent":""}`} {
		t.Run(name, func(t *testing.T) {
			provider, _ := newMockCloudTTS(t, jsonResponse(http.StatusOK, body))
			resp, bifrostErr := provider.Speech(testContext(t), testKey(), speechRequest("hi", nil))
			if resp != nil || bifrostErr == nil {
				t.Fatalf("expected an error, got response %+v", resp)
			}
			if bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusBadGateway {
				t.Errorf("status = %v, want 502", bifrostErr.StatusCode)
			}
			if bifrostErr.ExtraFields.ErrorType != schemas.ErrorTypeProviderEmptyResult {
				t.Errorf("error_type = %q, want %q", bifrostErr.ExtraFields.ErrorType, schemas.ErrorTypeProviderEmptyResult)
			}
		})
	}
}

func TestSpeechRequiresInput(t *testing.T) {
	provider, captured := newMockCloudTTS(t, jsonResponse(http.StatusOK, sampleResponseBody))
	_, bifrostErr := provider.Speech(testContext(t), testKey(), speechRequest("", nil))
	if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusBadRequest {
		t.Fatalf("error = %+v, want a 400", bifrostErr)
	}
	if n := len(captured()); n != 0 {
		t.Errorf("made %d upstream calls for a refused request, want 0", n)
	}
}

// The vendor's raw response is returned when the caller asks for it (contract C5).
func TestSpeechSendsBackRawResponseOnRequest(t *testing.T) {
	provider, _ := newMockCloudTTS(t, jsonResponse(http.StatusOK, sampleResponseBody))
	ctx := testContext(t)
	ctx.SetValue(schemas.BifrostContextKeyCaptureRawResponse, true)

	resp, bifrostErr := provider.Speech(ctx, testKey(), speechRequest("hi", nil))
	if bifrostErr != nil {
		t.Fatalf("Speech error: %+v", bifrostErr.Error)
	}
	raw, ok := resp.ExtraFields.RawResponse.(map[string]any)
	if !ok || raw["audioContent"] != sampleAudioContent {
		t.Errorf("raw_response = %#v, want the vendor's body", resp.ExtraFields.RawResponse)
	}
}

func TestDefaultBaseURL(t *testing.T) {
	config := &schemas.ProviderConfig{}
	if _, err := googlecloudtts.NewGoogleCloudTTSProvider(config, bifrost.NewDefaultLogger(schemas.LogLevelError)); err != nil {
		t.Fatalf("NewGoogleCloudTTSProvider: %v", err)
	}
	if config.NetworkConfig.BaseURL != "https://texttospeech.googleapis.com" {
		t.Errorf("base URL = %q, want https://texttospeech.googleapis.com", config.NetworkConfig.BaseURL)
	}
}

// The request-path cost of the provider (AGENTS.md "Every change states its cost on the
// request flow"). The provider is new, so there is no before: the numbers are its whole
// cost per speech request.

// BenchmarkToGoogleCloudTTSSpeechRequest measures the unified request's conversion into
// the text:synthesize body, the only per-request work before the upstream call besides
// the JSON encode.
func BenchmarkToGoogleCloudTTSSpeechRequest(b *testing.B) {
	ssml := "<speak>" + sampleText + "</speak>"
	for _, bc := range []struct {
		name  string
		input string
	}{{"text", sampleText}, {"ssml", ssml}} {
		req := speechRequest(bc.input, &schemas.SpeechParameters{
			VoiceConfig:    voice(sampleVoice),
			LanguageCode:   schemas.Ptr(sampleLanguageCode),
			ResponseFormat: "mp3",
			Speed:          schemas.Ptr(1.25),
		})
		b.Run(bc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if googlecloudtts.ToGoogleCloudTTSSpeechRequest(req) == nil {
					b.Fatal("no request")
				}
			}
		})
	}
}

// BenchmarkSpeech measures one whole speech request against a loopback upstream that
// answers 48 kB of audio (64 kB of base64): validation, conversion, the JSON encode, the
// call on a kept-alive connection, the JSON decode and the base64 decode.
func BenchmarkSpeech(b *testing.B) {
	audio := bytes.Repeat([]byte{0xff, 0xf3, 0x44, 0xc4}, 12*1024)
	body := []byte(`{"audioContent":"` + base64.StdEncoding.EncodeToString(audio) + `"}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json; charset=UTF-8")
		_, _ = w.Write(body)
	}))
	defer server.Close()
	provider, err := googlecloudtts.NewGoogleCloudTTSProvider(&schemas.ProviderConfig{
		NetworkConfig: schemas.NetworkConfig{BaseURL: server.URL, DefaultRequestTimeoutInSeconds: 10, AllowPrivateNetwork: true},
	}, bifrost.NewDefaultLogger(schemas.LogLevelError))
	if err != nil {
		b.Fatalf("NewGoogleCloudTTSProvider: %v", err)
	}
	ctx, cancel := schemas.NewBifrostContextWithTimeout(context.Background(), time.Minute)
	defer cancel()
	key := testKey()
	req := speechRequest(sampleText, &schemas.SpeechParameters{
		VoiceConfig:    voice(sampleVoice),
		LanguageCode:   schemas.Ptr(sampleLanguageCode),
		ResponseFormat: "mp3",
	})

	b.ReportAllocs()
	b.SetBytes(int64(len(audio)))
	for b.Loop() {
		resp, bifrostErr := provider.Speech(ctx, key, req)
		if bifrostErr != nil {
			b.Fatalf("Speech error: %+v", bifrostErr.Error)
		}
		if len(resp.Audio) != len(audio) {
			b.Fatalf("audio = %d bytes, want %d", len(resp.Audio), len(audio))
		}
	}
}
