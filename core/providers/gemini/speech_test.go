package gemini

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

// The bodies below are Gemini generateContent shapes for a TTS model. geminiPromptBlockedBody
// (responses_test.go) is the vendor's sample of a prompt refused before generation.
const (
	// Six bytes of PCM, base64.
	geminiSpeechPCMBase64 = "AAECAwQF"

	geminiSpeechAudioChunk = `{"candidates":[{"content":{"role":"model","parts":[{"inlineData":{"mimeType":"audio/L16;codec=pcm;rate=24000","data":"` + geminiSpeechPCMBase64 + `"}}]}}]}`

	geminiSpeechUsage = `"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":50,"totalTokenCount":69,"thoughtsTokenCount":12,` +
		`"promptTokensDetails":[{"modality":"TEXT","tokenCount":7}],"candidatesTokensDetails":[{"modality":"AUDIO","tokenCount":50}]}`

	geminiSpeechFinalChunk = `{"candidates":[{"content":{"role":"model","parts":[{"inlineData":{"mimeType":"audio/L16;codec=pcm;rate=24000","data":"` +
		geminiSpeechPCMBase64 + `"}}]},"finishReason":"STOP"}],` + geminiSpeechUsage + `}`

	geminiSpeechSafetyFinishChunk = `{"candidates":[{"finishReason":"PROHIBITED_CONTENT"}],"usageMetadata":{"promptTokenCount":7,"totalTokenCount":7}}`
)

// newGeminiSpeechTestProvider serves body for every request and counts the upstream calls.
func newGeminiSpeechTestProvider(t *testing.T, body string, sse bool) (*GeminiProvider, *atomic.Int32, *atomic.Value) {
	t.Helper()
	calls := &atomic.Int32{}
	lastBody := &atomic.Value{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		reqBody, _ := io.ReadAll(r.Body)
		lastBody.Store(string(reqBody))
		if sse {
			w.Header().Set("Content-Type", "text/event-stream")
		} else {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(ts.Close)
	provider := NewGeminiProvider(&schemas.ProviderConfig{
		NetworkConfig: schemas.NetworkConfig{BaseURL: ts.URL + "/v1beta"},
	}, testNoopLogger{})
	return provider, calls, lastBody
}

func geminiSpeechTestRequest(params *schemas.SpeechParameters) *schemas.BifrostSpeechRequest {
	return &schemas.BifrostSpeechRequest{
		Provider: schemas.Gemini,
		Model:    "gemini-3.8-flash-tts",
		Input:    &schemas.SpeechInput{Input: "Say hello."},
		Params:   params,
	}
}

func geminiSpeechTestKey() schemas.Key {
	return schemas.Key{Value: *schemas.NewSecretVar("dummy-key")}
}

// collectGeminiSpeechStream runs SpeechStream against an SSE body and returns every chunk.
func collectGeminiSpeechStream(t *testing.T, sseBody string, params *schemas.SpeechParameters) ([]*schemas.BifrostStreamChunk, *atomic.Int32) {
	t.Helper()
	provider, calls, _ := newGeminiSpeechTestProvider(t, sseBody, true)
	postHook := func(ctx *schemas.BifrostContext, result *schemas.BifrostResponse, err *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError) {
		return result, err
	}
	stream, bErr := provider.SpeechStream(schemas.NewBifrostContext(context.Background(), schemas.NoDeadline),
		postHook, func(context.Context) {}, geminiSpeechTestKey(), geminiSpeechTestRequest(params))
	require.Nil(t, bErr, "stream setup failed")
	var chunks []*schemas.BifrostStreamChunk
	for chunk := range stream {
		chunks = append(chunks, chunk)
	}
	return chunks, calls
}

func sseEvents(events ...string) string {
	body := ""
	for _, event := range events {
		body += "data: " + event + "\n\n"
	}
	return body
}

// Contract C6: a speech response is audio bytes with nowhere to put a content_filter marker,
// so a generation Gemini blocked with a 200 is a 400 caller_content_filtered carrying the
// vendor's reason verbatim. It used to surface as an empty audio.
func TestSpeechBlockError(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		wantCode    string
		wantMessage string
	}{
		{name: "prompt blocked, vendor sample", body: geminiPromptBlockedBody, wantCode: "SAFETY", wantMessage: "The prompt is blocked due to safety"},
		{name: "prompt blocked, no message", body: `{"promptFeedback":{"blockReason":"PROHIBITED_CONTENT"}}`, wantCode: "PROHIBITED_CONTENT", wantMessage: "Gemini blocked the prompt: PROHIBITED_CONTENT"},
		{name: "safety finishReason", body: `{"candidates":[{"finishReason":"SAFETY","finishMessage":"blocked by safety"}]}`, wantCode: "SAFETY", wantMessage: "blocked by safety"},
		{name: "prohibited content finishReason", body: geminiSpeechSafetyFinishChunk, wantCode: "PROHIBITED_CONTENT", wantMessage: "Gemini blocked the speech: finishReason PROHIBITED_CONTENT"},
		{name: "normal finish", body: geminiSpeechFinalChunk},
		{name: "audio chunk without finishReason", body: geminiSpeechAudioChunk},
		{name: "unsupported language is not a content block", body: `{"candidates":[{"finishReason":"LANGUAGE"}]}`},
		{name: "length is not a content block", body: `{"candidates":[{"finishReason":"MAX_TOKENS"}]}`},
		{name: "no candidates and no feedback", body: `{"usageMetadata":{"promptTokenCount":7}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var resp GenerateContentResponse
			require.NoError(t, sonic.Unmarshal([]byte(tc.body), &resp))
			got := resp.SpeechBlockError()
			if tc.wantCode == "" {
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			require.NotNil(t, got.StatusCode)
			assert.Equal(t, 400, *got.StatusCode)
			assert.Equal(t, schemas.ErrorTypeCallerContentFiltered, got.ExtraFields.ErrorType)
			assert.False(t, got.IsBifrostError, "the vendor answered: not a Bifrost failure")
			require.NotNil(t, got.Error.Code)
			assert.Equal(t, tc.wantCode, *got.Error.Code)
			assert.Equal(t, tc.wantMessage, got.Error.Message)
		})
	}
	assert.Nil(t, (*GenerateContentResponse)(nil).SpeechBlockError())
}

// The non-streaming route: one upstream call, and the block reaches the caller as the error
// with the vendor's raw body when the raw response is asked for.
func TestGeminiSpeechBlockedPromptIsContentFilteredError(t *testing.T) {
	for _, body := range []string{geminiPromptBlockedBody, geminiSpeechSafetyFinishChunk} {
		provider, calls, _ := newGeminiSpeechTestProvider(t, body, false)
		ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
		ctx.SetValue(schemas.BifrostContextKeyCaptureRawResponse, true)

		resp, bErr := provider.Speech(ctx, geminiSpeechTestKey(), geminiSpeechTestRequest(nil))
		require.Nil(t, resp)
		require.NotNil(t, bErr)
		require.NotNil(t, bErr.StatusCode)
		assert.Equal(t, 400, *bErr.StatusCode)
		assert.Equal(t, schemas.ErrorTypeCallerContentFiltered, bErr.ExtraFields.ErrorType)
		assert.True(t, schemas.IsContentFilterError(bErr))
		require.NotNil(t, bErr.Error.Code)
		assert.Contains(t, body, `"`+*bErr.Error.Code+`"`, "error.code is the vendor's reason verbatim")
		assert.NotNil(t, bErr.ExtraFields.RawResponse, "the vendor's body is kept for the caller who asked for it")
		// Tokens the vendor counted before blocking are billed; a block that counted none bills nothing.
		if body == geminiSpeechSafetyFinishChunk {
			require.NotNil(t, bErr.ExtraFields.BilledUsage)
			assert.Equal(t, 7, bErr.ExtraFields.BilledUsage.PromptTokens)
			assert.Equal(t, 7, bErr.ExtraFields.BilledUsage.TotalTokens)
		} else {
			assert.Nil(t, bErr.ExtraFields.BilledUsage)
		}
		assert.EqualValues(t, 1, calls.Load(), "one upstream call per request")
	}
}

// Contract C6 on the stream: a blocked prompt (the first and only chunk) and a safety
// finishReason after audio both end the stream on the content-filter error, never on a done
// frame.
func TestGeminiSpeechStreamBlockedEndsOnContentFilteredError(t *testing.T) {
	cases := []struct {
		name       string
		events     []string
		wantDeltas int
		wantCode   string
		wantBilled int // prompt tokens billed with the error; 0 means no BilledUsage
	}{
		{name: "prompt blocked", events: []string{geminiPromptBlockedBody}, wantCode: "SAFETY"},
		{name: "safety finishReason after audio", events: []string{geminiSpeechAudioChunk, geminiSpeechSafetyFinishChunk}, wantDeltas: 1, wantCode: "PROHIBITED_CONTENT", wantBilled: 7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chunks, calls := collectGeminiSpeechStream(t, sseEvents(tc.events...), nil)
			require.Len(t, chunks, tc.wantDeltas+1)
			for _, chunk := range chunks[:tc.wantDeltas] {
				require.NotNil(t, chunk.BifrostSpeechStreamResponse)
				assert.Equal(t, schemas.SpeechStreamResponseTypeDelta, chunk.BifrostSpeechStreamResponse.Type)
			}
			last := chunks[len(chunks)-1]
			require.NotNil(t, last.BifrostError, "the stream must end on the error")
			assert.Nil(t, last.BifrostSpeechStreamResponse, "no done frame")
			require.NotNil(t, last.BifrostError.StatusCode)
			assert.Equal(t, 400, *last.BifrostError.StatusCode)
			assert.Equal(t, schemas.ErrorTypeCallerContentFiltered, last.BifrostError.ExtraFields.ErrorType)
			require.NotNil(t, last.BifrostError.Error.Code)
			assert.Equal(t, tc.wantCode, *last.BifrostError.Error.Code)
			// The audio already sent was counted by the vendor: the error carries it for the caller's budget.
			if tc.wantBilled == 0 {
				assert.Nil(t, last.BifrostError.ExtraFields.BilledUsage)
			} else {
				require.NotNil(t, last.BifrostError.ExtraFields.BilledUsage)
				assert.Equal(t, tc.wantBilled, last.BifrostError.ExtraFields.BilledUsage.PromptTokens)
			}
			assert.EqualValues(t, 1, calls.Load(), "one upstream call per request")
		})
	}
}

// Contract C10: usage sits in the done frame, so a body that ends without the vendor's end
// marker (a candidate with a finishReason) must not look complete. It used to end on a done
// frame with zero usage.
func TestGeminiSpeechStreamWithoutFinishReasonIsTruncated(t *testing.T) {
	cases := []struct {
		name       string
		events     []string
		wantDeltas int
	}{
		{name: "audio then EOF", events: []string{geminiSpeechAudioChunk, geminiSpeechAudioChunk}, wantDeltas: 2},
		{name: "empty body", events: nil},
		{name: "usage but no finishReason", events: []string{geminiSpeechAudioChunk, `{` + geminiSpeechUsage + `}`}, wantDeltas: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chunks, _ := collectGeminiSpeechStream(t, sseEvents(tc.events...), nil)
			require.Len(t, chunks, tc.wantDeltas+1)
			for _, chunk := range chunks {
				if chunk.BifrostSpeechStreamResponse != nil {
					assert.NotEqual(t, schemas.SpeechStreamResponseTypeDone, chunk.BifrostSpeechStreamResponse.Type, "a truncated stream sends no done frame")
				}
			}
			last := chunks[len(chunks)-1]
			require.NotNil(t, last.BifrostError, "the stream must end on the truncation error")
			assert.Equal(t, schemas.ErrProviderStreamTruncated, last.BifrostError.Error.Message)
			assert.False(t, last.BifrostError.IsBifrostError)
			require.NotNil(t, last.BifrostError.StatusCode)
			assert.Equal(t, 502, *last.BifrostError.StatusCode)
			// Usage the vendor reported before the body ended is billed with the error.
			if tc.name == "usage but no finishReason" {
				require.NotNil(t, last.BifrostError.ExtraFields.BilledUsage)
				assert.Equal(t, 69, last.BifrostError.ExtraFields.BilledUsage.TotalTokens)
			} else {
				assert.Nil(t, last.BifrostError.ExtraFields.BilledUsage)
			}
		})
	}
}

// Contract C5: a complete stream ends on a done frame whose usage carries every token detail
// the vendor reported, thought tokens and the candidates' modality breakdown included.
func TestGeminiSpeechStreamDoneCarriesFullUsage(t *testing.T) {
	chunks, calls := collectGeminiSpeechStream(t, sseEvents(geminiSpeechAudioChunk, geminiSpeechFinalChunk), nil)
	require.Len(t, chunks, 3)
	for _, chunk := range chunks {
		require.Nil(t, chunk.BifrostError)
		require.NotNil(t, chunk.BifrostSpeechStreamResponse)
	}
	assert.Equal(t, []byte{0, 1, 2, 3, 4, 5}, chunks[0].BifrostSpeechStreamResponse.Audio)
	done := chunks[2].BifrostSpeechStreamResponse
	require.Equal(t, schemas.SpeechStreamResponseTypeDone, done.Type)
	require.NotNil(t, done.Usage)
	assert.Equal(t, 7, done.Usage.InputTokens)
	assert.Equal(t, 50, done.Usage.OutputTokens, "output_tokens is the vendor's candidatesTokenCount, thought tokens not added")
	assert.Equal(t, 69, done.Usage.TotalTokens)
	assert.Equal(t, &schemas.SpeechUsageInputTokenDetails{TextTokens: 7}, done.Usage.InputTokenDetails)
	assert.Equal(t, &schemas.SpeechUsageOutputTokenDetails{AudioTokens: 50, ReasoningTokens: 12}, done.Usage.OutputTokenDetails)
	assert.Equal(t, len("Say hello."), done.Usage.InputChars)
	assert.EqualValues(t, 1, calls.Load(), "one upstream call per request")

	wire, err := sonic.Marshal(done.Usage)
	require.NoError(t, err)
	assert.Contains(t, string(wire), `"output_token_details":{"audio_tokens":50,"reasoning_tokens":12}`)
}

func TestConvertGeminiUsageMetadataToSpeechUsageCarriesOutputDetails(t *testing.T) {
	metadata := &GenerateContentResponseUsageMetadata{
		PromptTokenCount:        7,
		CandidatesTokenCount:    52,
		TotalTokenCount:         71,
		ThoughtsTokenCount:      12,
		PromptTokensDetails:     []*ModalityTokenCount{{Modality: ModalityText, TokenCount: 7}},
		CandidatesTokensDetails: []*ModalityTokenCount{{Modality: ModalityAudio, TokenCount: 50}, nil, {Modality: ModalityText, TokenCount: 2}},
	}
	usage := convertGeminiUsageMetadataToSpeechUsage(metadata)
	require.NotNil(t, usage)
	assert.Equal(t, 52, usage.OutputTokens)
	assert.Equal(t, &schemas.SpeechUsageOutputTokenDetails{TextTokens: 2, AudioTokens: 50, ReasoningTokens: 12}, usage.OutputTokenDetails)

	// The native GenAI route converts the unified usage back: the details survive the round trip.
	back := convertBifrostSpeechUsageToGeminiUsageMetadata(usage)
	assert.EqualValues(t, 12, back.ThoughtsTokenCount)
	assert.Equal(t, []*ModalityTokenCount{{Modality: ModalityText, TokenCount: 2}, {Modality: ModalityAudio, TokenCount: 50}}, back.CandidatesTokensDetails)

	// Thought tokens alone are still reported.
	onlyThoughts := convertGeminiUsageMetadataToSpeechUsage(&GenerateContentResponseUsageMetadata{CandidatesTokenCount: 5, ThoughtsTokenCount: 3})
	assert.Equal(t, &schemas.SpeechUsageOutputTokenDetails{ReasoningTokens: 3}, onlyThoughts.OutputTokenDetails)

	// What the vendor did not report stays empty.
	bare := convertGeminiUsageMetadataToSpeechUsage(&GenerateContentResponseUsageMetadata{PromptTokenCount: 1, CandidatesTokenCount: 2, TotalTokenCount: 3})
	assert.Nil(t, bare.OutputTokenDetails)
	wire, err := sonic.Marshal(bare)
	require.NoError(t, err)
	assert.NotContains(t, string(wire), "output_token_details")
}

// Contract C4: the output-token bound reaches Gemini as generationConfig.maxOutputTokens, as
// given, and is not also merged into the body under its unified name.
func TestToGeminiSpeechRequestSendsMaxOutputTokens(t *testing.T) {
	extra := map[string]interface{}{"max_output_tokens": float64(4096), "labels": "kept"}
	req := geminiSpeechTestRequest(&schemas.SpeechParameters{
		VoiceConfig:     &schemas.SpeechVoiceInput{Voice: schemas.Ptr("Kore")},
		MaxOutputTokens: schemas.Ptr(4096),
		ExtraParams:     extra,
	})
	geminiReq, err := ToGeminiSpeechRequest(req)
	require.NoError(t, err)
	assert.EqualValues(t, 4096, geminiReq.GenerationConfig.MaxOutputTokens)
	assert.Equal(t, map[string]interface{}{"labels": "kept"}, geminiReq.ExtraParams)
	assert.Contains(t, extra, "max_output_tokens", "the request's own extra params are not mutated: a retry converts them again")

	wire, err := sonic.Marshal(geminiReq)
	require.NoError(t, err)
	assert.Contains(t, string(wire), `"maxOutputTokens":4096`)
	assert.Contains(t, string(wire), `"voiceName":"Kore"`)

	// No bound, no field.
	geminiReq, err = ToGeminiSpeechRequest(geminiSpeechTestRequest(nil))
	require.NoError(t, err)
	wire, err = sonic.Marshal(geminiReq)
	require.NoError(t, err)
	assert.NotContains(t, string(wire), "maxOutputTokens")
}

// The bound travels on the provider's request body, once.
func TestGeminiSpeechSendsMaxOutputTokensUpstream(t *testing.T) {
	provider, calls, lastBody := newGeminiSpeechTestProvider(t, geminiSpeechFinalChunk, false)
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	_, bErr := provider.Speech(ctx, geminiSpeechTestKey(), geminiSpeechTestRequest(&schemas.SpeechParameters{
		VoiceConfig:     &schemas.SpeechVoiceInput{Voice: schemas.Ptr("Kore")},
		MaxOutputTokens: schemas.Ptr(1024),
	}))
	require.Nil(t, bErr)
	sent, _ := lastBody.Load().(string)
	assert.Contains(t, sent, `"maxOutputTokens":1024`)
	assert.EqualValues(t, 1, calls.Load())
}

// Contract C4: an empty response_format is WAV, as the converter always said it was; it used
// to return the bare PCM (and to panic when the request carried no params at all). pcm names
// the bare PCM explicitly.
func TestGeminiSpeechResponseFormatDefaultsToWAV(t *testing.T) {
	cases := []struct {
		name    string
		params  *schemas.SpeechParameters
		wantWAV bool
	}{
		{name: "no params", params: nil, wantWAV: true},
		{name: "empty response_format", params: &schemas.SpeechParameters{VoiceConfig: &schemas.SpeechVoiceInput{Voice: schemas.Ptr("Kore")}}, wantWAV: true},
		{name: "wav", params: &schemas.SpeechParameters{ResponseFormat: "wav"}, wantWAV: true},
		{name: "pcm", params: &schemas.SpeechParameters{ResponseFormat: "pcm"}, wantWAV: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider, _, _ := newGeminiSpeechTestProvider(t, geminiSpeechFinalChunk, false)
			ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
			resp, bErr := provider.Speech(ctx, geminiSpeechTestKey(), geminiSpeechTestRequest(tc.params))
			require.Nil(t, bErr)
			require.NotNil(t, resp)
			if tc.wantWAV {
				require.Greater(t, len(resp.Audio), 44)
				assert.Equal(t, "RIFF", string(resp.Audio[:4]))
				assert.Equal(t, "WAVE", string(resp.Audio[8:12]))
				assert.Equal(t, []byte{0, 1, 2, 3, 4, 5}, resp.Audio[len(resp.Audio)-6:])
			} else {
				assert.Equal(t, []byte{0, 1, 2, 3, 4, 5}, resp.Audio)
			}
			require.NotNil(t, resp.Usage)
			assert.Equal(t, &schemas.SpeechUsageOutputTokenDetails{AudioTokens: 50, ReasoningTokens: 12}, resp.Usage.OutputTokenDetails)
		})
	}
}

// Contract C4, the value half: a response_format Gemini cannot answer and an output-token
// bound it cannot be sent are refused naming the field, before anything is sent.
func TestGeminiSpeechRejectsUndeliverableValues(t *testing.T) {
	cases := []struct {
		name      string
		params    *schemas.SpeechParameters
		wantParam string
	}{
		{name: "mp3", params: &schemas.SpeechParameters{ResponseFormat: "mp3"}, wantParam: "response_format"},
		{name: "zero bound", params: &schemas.SpeechParameters{MaxOutputTokens: schemas.Ptr(0)}, wantParam: "max_output_tokens"},
		{name: "negative bound", params: &schemas.SpeechParameters{MaxOutputTokens: schemas.Ptr(-1)}, wantParam: "max_output_tokens"},
		{name: "bound past int32", params: &schemas.SpeechParameters{MaxOutputTokens: schemas.Ptr(1 << 40)}, wantParam: "max_output_tokens"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider, calls, _ := newGeminiSpeechTestProvider(t, geminiSpeechFinalChunk, false)
			ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
			req := geminiSpeechTestRequest(tc.params)

			_, bErr := provider.Speech(ctx, geminiSpeechTestKey(), req)
			assertUnsupportedParameter(t, bErr, tc.wantParam)

			stream, bErr := provider.SpeechStream(ctx, nil, func(context.Context) {}, geminiSpeechTestKey(), req)
			assert.Nil(t, stream)
			assertUnsupportedParameter(t, bErr, tc.wantParam)

			assert.Zero(t, calls.Load(), "a refused request makes no upstream call")

			_, err := ToGeminiSpeechRequest(req)
			assert.Error(t, err, "the converter never builds a body from a value it cannot send")
		})
	}
	assert.Nil(t, ValidateSpeechRequest(geminiSpeechTestRequest(&schemas.SpeechParameters{ResponseFormat: "pcm", MaxOutputTokens: schemas.Ptr(16384)})))
	assert.Nil(t, ValidateSpeechRequest(nil))
}

func assertUnsupportedParameter(t *testing.T, bErr *schemas.BifrostError, wantParam string) {
	t.Helper()
	require.NotNil(t, bErr)
	require.NotNil(t, bErr.StatusCode)
	assert.Equal(t, 400, *bErr.StatusCode)
	assert.Equal(t, schemas.ErrorTypeCallerUnsupportedParameter, bErr.ExtraFields.ErrorType)
	require.NotNil(t, bErr.Error)
	assert.Equal(t, wantParam, bErr.Error.Param)
}

// Contract C4, the field half: the provider declares the unified fields generateContent
// delivers, and core's check refuses every other one naming it. speed, instructions and
// language_code used to be dropped silently.
func TestGeminiDeclaresSpeechAndTranscriptionUnifiedFields(t *testing.T) {
	provider := NewGeminiProvider(&schemas.ProviderConfig{}, testNoopLogger{})
	var _ schemas.UnifiedFieldDeclarer = provider

	speech := func(requestType schemas.RequestType, params *schemas.SpeechParameters) *schemas.BifrostRequest {
		return &schemas.BifrostRequest{RequestType: requestType, SpeechRequest: geminiSpeechTestRequest(params)}
	}
	transcription := func(requestType schemas.RequestType, params *schemas.TranscriptionParameters) *schemas.BifrostRequest {
		return &schemas.BifrostRequest{RequestType: requestType, TranscriptionRequest: &schemas.BifrostTranscriptionRequest{
			Provider: schemas.Gemini, Model: "gemini-3.5-transcribe", Input: &schemas.TranscriptionInput{File: []byte("x")}, Params: params,
		}}
	}
	voice := &schemas.SpeechVoiceInput{Voice: schemas.Ptr("Kore")}

	rejected := []struct {
		name      string
		req       func(schemas.RequestType) *schemas.BifrostRequest
		stream    schemas.RequestType
		sync      schemas.RequestType
		wantParam string
	}{
		{name: "speech speed", wantParam: "speed", sync: schemas.SpeechRequest, stream: schemas.SpeechStreamRequest,
			req: func(rt schemas.RequestType) *schemas.BifrostRequest {
				return speech(rt, &schemas.SpeechParameters{VoiceConfig: voice, Speed: schemas.Ptr(1.5)})
			}},
		{name: "speech instructions", wantParam: "instructions", sync: schemas.SpeechRequest, stream: schemas.SpeechStreamRequest,
			req: func(rt schemas.RequestType) *schemas.BifrostRequest {
				return speech(rt, &schemas.SpeechParameters{VoiceConfig: voice, Instructions: "Speak slowly."})
			}},
		{name: "speech language_code", wantParam: "language_code", sync: schemas.SpeechRequest, stream: schemas.SpeechStreamRequest,
			req: func(rt schemas.RequestType) *schemas.BifrostRequest {
				return speech(rt, &schemas.SpeechParameters{VoiceConfig: voice, LanguageCode: schemas.Ptr("en-US")})
			}},
		{name: "speech with_timestamps", wantParam: "with_timestamps", sync: schemas.SpeechRequest, stream: schemas.SpeechStreamRequest,
			req: func(rt schemas.RequestType) *schemas.BifrostRequest {
				return speech(rt, &schemas.SpeechParameters{VoiceConfig: voice, WithTimestamps: schemas.Ptr(true)})
			}},
		{name: "transcription language", wantParam: "language", sync: schemas.TranscriptionRequest, stream: schemas.TranscriptionStreamRequest,
			req: func(rt schemas.RequestType) *schemas.BifrostRequest {
				return transcription(rt, &schemas.TranscriptionParameters{Language: schemas.Ptr("en")})
			}},
		{name: "transcription timestamp_granularities", wantParam: "timestamp_granularities", sync: schemas.TranscriptionRequest, stream: schemas.TranscriptionStreamRequest,
			req: func(rt schemas.RequestType) *schemas.BifrostRequest {
				return transcription(rt, &schemas.TranscriptionParameters{TimestampGranularities: []string{"word"}})
			}},
	}
	for _, tc := range rejected {
		for _, requestType := range []schemas.RequestType{tc.sync, tc.stream} {
			t.Run(tc.name+"/"+string(requestType), func(t *testing.T) {
				assertUnsupportedParameter(t, schemas.CheckDeclaredUnifiedFields(provider, tc.req(requestType)), tc.wantParam)
			})
		}
	}

	accepted := []*schemas.BifrostRequest{
		speech(schemas.SpeechRequest, nil),
		speech(schemas.SpeechRequest, &schemas.SpeechParameters{VoiceConfig: voice, ResponseFormat: "wav", MaxOutputTokens: schemas.Ptr(1024),
			ExtraParams: map[string]interface{}{"speed": 2}}),
		speech(schemas.SpeechStreamRequest, &schemas.SpeechParameters{VoiceConfig: &schemas.SpeechVoiceInput{
			MultiVoiceConfig: []schemas.VoiceConfig{{Speaker: "A", Voice: "Kore"}, {Speaker: "B", Voice: "Puck"}}}}),
		transcription(schemas.TranscriptionRequest, &schemas.TranscriptionParameters{Prompt: schemas.Ptr("Transcribe."),
			ResponseFormat: schemas.Ptr("json"), Temperature: schemas.Ptr(0.2), Format: schemas.Ptr("mp3")}),
		transcription(schemas.TranscriptionStreamRequest, &schemas.TranscriptionParameters{}),
	}
	for _, req := range accepted {
		assert.Nil(t, schemas.CheckDeclaredUnifiedFields(provider, req))
	}

	// Operations whose vendor sheet is not wired up stay undeclared, so core leaves them unchecked.
	for _, requestType := range []schemas.RequestType{schemas.ChatCompletionRequest, schemas.ImageGenerationRequest, schemas.EmbeddingRequest} {
		_, declared := provider.SupportedUnifiedFields(requestType)
		assert.False(t, declared, string(requestType))
	}
}

// The request path of every Gemini speech: the value check and the body conversion.
func BenchmarkToGeminiSpeechRequest(b *testing.B) {
	req := geminiSpeechTestRequest(&schemas.SpeechParameters{VoiceConfig: &schemas.SpeechVoiceInput{Voice: schemas.Ptr("Kore")}})
	b.ReportAllocs()
	for b.Loop() {
		if _, err := ToGeminiSpeechRequest(req); err != nil {
			b.Fatal(err)
		}
	}
}

// The response path of every Gemini speech chunk: the block check (zero allocations when the
// chunk is not blocked) and the usage conversion of the final chunk.
func BenchmarkGeminiSpeechChunkChecks(b *testing.B) {
	var chunk GenerateContentResponse
	if err := sonic.Unmarshal([]byte(geminiSpeechFinalChunk), &chunk); err != nil {
		b.Fatal(err)
	}
	b.Run("SpeechBlockError", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if chunk.SpeechBlockError() != nil {
				b.Fatal("unexpected block")
			}
		}
	})
	b.Run("usage", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if convertGeminiUsageMetadataToSpeechUsage(chunk.UsageMetadata) == nil {
				b.Fatal("no usage")
			}
		}
	})
}

// Core runs the unified-field check on every request before a key is chosen. For Gemini it is
// one switch for an operation the provider does not declare (chat and every other one), and a
// walk of the set params for speech and transcription.
func BenchmarkGeminiDeclaredUnifiedFieldsCheck(b *testing.B) {
	provider := NewGeminiProvider(&schemas.ProviderConfig{}, testNoopLogger{})
	chat := &schemas.BifrostRequest{RequestType: schemas.ChatCompletionRequest, ChatRequest: &schemas.BifrostChatRequest{
		Provider: schemas.Gemini, Model: "gemini-2.5-flash", Params: &schemas.ChatParameters{Temperature: schemas.Ptr(0.2)},
	}}
	speech := &schemas.BifrostRequest{RequestType: schemas.SpeechStreamRequest, SpeechRequest: geminiSpeechTestRequest(&schemas.SpeechParameters{
		VoiceConfig: &schemas.SpeechVoiceInput{Voice: schemas.Ptr("Kore")}, MaxOutputTokens: schemas.Ptr(1024),
	})}
	for name, req := range map[string]*schemas.BifrostRequest{"chat (undeclared)": chat, "speech (declared)": speech} {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if schemas.CheckDeclaredUnifiedFields(provider, req) != nil {
					b.Fatal("unexpected refusal")
				}
			}
		})
	}
}

// Gemini speech is returned as WAV unless pcm is named (TXE-6539). The transport labels the audio from
// BifrostSpeechResponse.ContentType, so the converter must record the format it returns
// (TXE-6530); without it a raw PCM body went out as audio/mpeg.
func TestToBifrostSpeechResponseRecordsContentType(t *testing.T) {
	pcm := []byte{0x01, 0x00, 0x02, 0x00}
	resp := &GenerateContentResponse{
		Candidates: []*Candidate{{Content: &Content{Parts: []*Part{{InlineData: &Blob{
			MIMEType: "audio/L16;codec=pcm;rate=24000",
			Data:     base64.StdEncoding.EncodeToString(pcm),
		}}}}}},
		UsageMetadata: &GenerateContentResponseUsageMetadata{PromptTokenCount: 5, CandidatesTokenCount: 30, TotalTokenCount: 35},
	}

	for _, tc := range []struct {
		format string
		want   string
	}{
		{"", "audio/wav"},
		{"pcm", "audio/L16;codec=pcm;rate=24000"},
		{"wav", "audio/wav"},
	} {
		ctx := context.WithValue(context.Background(), BifrostContextKeyResponseFormat, tc.format)
		out, err := resp.ToBifrostSpeechResponse(ctx)
		if err != nil {
			t.Fatalf("format %q: %v", tc.format, err)
		}
		if out.ContentType != tc.want {
			t.Errorf("format %q: ContentType = %q, want %q", tc.format, out.ContentType, tc.want)
		}
		if out.Usage == nil || out.Usage.InputTokens != 5 || out.Usage.OutputTokens != 30 {
			t.Errorf("format %q: usage = %+v, want Gemini's usageMetadata", tc.format, out.Usage)
		}
	}
}

// TXE-6540 moved the speech stream reader into HandleGeminiSpeechStream, which takes the
// request headers as a map so Vertex can pass its own. This measures the one piece of work
// that changed on the Gemini API path: setting the three headers directly (before) against
// building the map and applying it (after).
func BenchmarkGeminiSpeechStreamHeaders(b *testing.B) {
	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)
	const apiKey = "dummy-key"
	b.Run("before: set directly", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			req.Header.Reset()
			setGeminiAuthHeader(req, apiKey)
			req.Header.Set("Accept", "text/event-stream")
			req.Header.Set("Cache-Control", "no-cache")
		}
	})
	b.Run("after: header map", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			req.Header.Reset()
			headers := map[string]string{"Accept": "text/event-stream", "Cache-Control": "no-cache"}
			headers["x-goog-api-key"] = apiKey
			applySpeechStreamHeadersForBenchmark(req, headers)
		}
	})
}

// applySpeechStreamHeadersForBenchmark applies headers the way HandleGeminiSpeechStream does.
// It is not inlined, so the map escapes to the heap as it does on the real call.
//
//go:noinline
func applySpeechStreamHeadersForBenchmark(req *fasthttp.Request, headers map[string]string) {
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	setGeminiAuthHeader(req, "")
}

// The buffered step both providers share after decoding the answer: the block check and the
// conversion of the audio to the requested format.
func BenchmarkSpeechResponseFromGenerateContent(b *testing.B) {
	var answer GenerateContentResponse
	if err := sonic.Unmarshal([]byte(geminiSpeechFinalChunk), &answer); err != nil {
		b.Fatal(err)
	}
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	for name, format := range map[string]string{"wav": "", "pcm": SpeechResponseFormatPCM} {
		req := geminiSpeechTestRequest(&schemas.SpeechParameters{ResponseFormat: format})
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, bErr := SpeechResponseFromGenerateContent(ctx, req, &answer); bErr != nil {
					b.Fatal(bErr.Error.Message)
				}
			}
		})
	}
}
