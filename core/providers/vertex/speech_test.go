package vertex

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/capsohq/bifrost/core/providers/gemini"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"golang.org/x/oauth2"
)

// The bodies below are generateContent shapes for a Gemini TTS model, which Vertex returns
// in the same form as the Gemini API (GenerateContentResponse). No Vertex TTS response has
// been recorded: these are the shapes the fork's Gemini types decode.
const (
	// Six bytes of PCM, base64.
	vertexSpeechPCMBase64 = "AAECAwQF"

	vertexSpeechAudioChunk = `{"candidates":[{"content":{"role":"model","parts":[{"inlineData":{"mimeType":"audio/L16;codec=pcm;rate=24000","data":"` + vertexSpeechPCMBase64 + `"}}]}}]}`

	vertexSpeechUsage = `"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":50,"totalTokenCount":69,"thoughtsTokenCount":12,` +
		`"promptTokensDetails":[{"modality":"TEXT","tokenCount":7}],"candidatesTokensDetails":[{"modality":"AUDIO","tokenCount":50}]}`

	vertexSpeechFinalChunk = `{"candidates":[{"content":{"role":"model","parts":[{"inlineData":{"mimeType":"audio/L16;codec=pcm;rate=24000","data":"` +
		vertexSpeechPCMBase64 + `"}}]},"finishReason":"STOP"}],` + vertexSpeechUsage + `}`

	vertexSpeechPromptBlocked = `{"promptFeedback":{"blockReason":"SAFETY","blockReasonMessage":"The prompt is blocked due to safety"},"usageMetadata":{"promptTokenCount":7,"totalTokenCount":7}}`

	vertexSpeechSafetyFinishChunk = `{"candidates":[{"finishReason":"PROHIBITED_CONTENT"}],"usageMetadata":{"promptTokenCount":7,"totalTokenCount":7}}`

	vertexSpeechModel = "gemini-3.8-flash-tts"
)

// vertexSpeechUpstream records what the provider sent to the stand-in for Vertex.
type vertexSpeechUpstream struct {
	mu       sync.Mutex
	calls    int
	host     string
	path     string
	query    string
	auth     string
	body     string
	provider *VertexProvider
}

func (u *vertexSpeechUpstream) snapshot() vertexSpeechUpstream {
	u.mu.Lock()
	defer u.mu.Unlock()
	return vertexSpeechUpstream{calls: u.calls, host: u.host, path: u.path, query: u.query, auth: u.auth, body: u.body}
}

// newVertexSpeechUpstream starts a TLS server that answers every request with body and a
// Vertex provider whose clients dial it whatever host the URL names.
func newVertexSpeechUpstream(t *testing.T, body string, sse bool) *vertexSpeechUpstream {
	t.Helper()
	upstream := &vertexSpeechUpstream{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqBody, _ := io.ReadAll(r.Body)
		upstream.mu.Lock()
		upstream.calls++
		upstream.host, upstream.path, upstream.query = r.Host, r.URL.Path, r.URL.RawQuery
		upstream.auth, upstream.body = r.Header.Get("Authorization"), string(reqBody)
		upstream.mu.Unlock()
		if sse {
			w.Header().Set("Content-Type", "text/event-stream")
		} else {
			w.Header().Set("Content-Type", "application/json")
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)

	config := &schemas.ProviderConfig{NetworkConfig: schemas.NetworkConfig{DefaultRequestTimeoutInSeconds: 10}}
	config.CheckAndSetDefaults()
	provider, err := NewVertexProvider(config, nil)
	require.NoError(t, err)
	for _, client := range []*fasthttp.Client{provider.client, provider.streamingClient} {
		client.Dial = func(string) (net.Conn, error) { return net.Dial("tcp", server.Listener.Addr().String()) }
		client.TLSConfig = &tls.Config{InsecureSkipVerify: true}
	}
	upstream.provider = provider
	return upstream
}

func vertexSpeechAPIKey() schemas.Key {
	return schemas.Key{
		Value:           *schemas.NewSecretVar("api-key"),
		VertexKeyConfig: &schemas.VertexKeyConfig{ProjectID: *schemas.NewSecretVar("proj"), Region: *schemas.NewSecretVar("us-central1")},
	}
}

func vertexSpeechRequest(params *schemas.SpeechParameters) *schemas.BifrostSpeechRequest {
	return &schemas.BifrostSpeechRequest{
		Provider: schemas.Vertex,
		Model:    vertexSpeechModel,
		Input:    &schemas.SpeechInput{Input: "Say hello."},
		Params:   params,
	}
}

func vertexSpeechVoice(params schemas.SpeechParameters) *schemas.SpeechParameters {
	params.VoiceConfig = &schemas.SpeechVoiceInput{Voice: schemas.Ptr("Kore")}
	return &params
}

func vertexSSE(events ...string) string {
	body := ""
	for _, event := range events {
		body += "data: " + event + "\n\n"
	}
	return body
}

// collectVertexSpeechStream runs SpeechStream against an SSE body and returns every chunk.
func collectVertexSpeechStream(t *testing.T, upstream *vertexSpeechUpstream, params *schemas.SpeechParameters) []*schemas.BifrostStreamChunk {
	t.Helper()
	postHook := func(_ *schemas.BifrostContext, result *schemas.BifrostResponse, err *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError) {
		return result, err
	}
	stream, bErr := upstream.provider.SpeechStream(schemas.NewBifrostContext(context.Background(), schemas.NoDeadline),
		postHook, func(context.Context) {}, vertexSpeechAPIKey(), vertexSpeechRequest(params))
	require.Nil(t, bErr, "stream setup failed")
	var chunks []*schemas.BifrostStreamChunk
	for chunk := range stream {
		chunks = append(chunks, chunk)
	}
	return chunks
}

// A Vertex speech request reaches the model's :generateContent with the body the Gemini API
// path builds, in one upstream call, and the buffered answer comes back as audio with the
// vendor's full usage (contract C3, C4, C5).
func TestVertexSpeechSendsTheGeminiBodyAndReturnsAudioAndUsage(t *testing.T) {
	upstream := newVertexSpeechUpstream(t, vertexSpeechFinalChunk, false)
	params := vertexSpeechVoice(schemas.SpeechParameters{MaxOutputTokens: schemas.Ptr(2048)})
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)

	resp, bErr := upstream.provider.Speech(ctx, vertexSpeechAPIKey(), vertexSpeechRequest(params))
	require.Nil(t, bErr)
	sent := upstream.snapshot()
	assert.Equal(t, 1, sent.calls, "one upstream call per request")
	assert.Equal(t, "us-central1-aiplatform.googleapis.com", sent.host)
	assert.Equal(t, "/v1/projects/proj/locations/us-central1/publishers/google/models/"+vertexSpeechModel+":generateContent", sent.path)
	assert.Equal(t, "key=api-key", sent.query)
	assert.Empty(t, sent.auth, "an API key is the only credential sent")

	geminiBody, err := gemini.ToGeminiSpeechRequest(vertexSpeechRequest(params))
	require.NoError(t, err)
	want, err := sonic.Marshal(geminiBody)
	require.NoError(t, err)
	assert.JSONEq(t, string(want), sent.body, "Vertex takes the body the Gemini API path builds")
	assert.Contains(t, sent.body, `"maxOutputTokens":2048`)

	// An unnamed response_format is WAV, as on the Gemini API.
	assert.Equal(t, "audio/wav", resp.ContentType)
	assert.Equal(t, "RIFF", string(resp.Audio[:4]))
	assert.Equal(t, []byte{0, 1, 2, 3, 4, 5}, resp.Audio[len(resp.Audio)-6:])
	require.NotNil(t, resp.Usage)
	assert.Equal(t, 7, resp.Usage.InputTokens)
	assert.Equal(t, 50, resp.Usage.OutputTokens)
	assert.Equal(t, 69, resp.Usage.TotalTokens)
	require.NotNil(t, resp.Usage.OutputTokenDetails)
	assert.Equal(t, 50, resp.Usage.OutputTokenDetails.AudioTokens)
	assert.Equal(t, 12, resp.Usage.OutputTokenDetails.ReasoningTokens)
}

// pcm names the bare PCM Vertex sends, labeled with the vendor's media type.
func TestVertexSpeechPCMIsReturnedAsSent(t *testing.T) {
	upstream := newVertexSpeechUpstream(t, vertexSpeechFinalChunk, false)
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	resp, bErr := upstream.provider.Speech(ctx, vertexSpeechAPIKey(), vertexSpeechRequest(vertexSpeechVoice(schemas.SpeechParameters{ResponseFormat: "pcm"})))
	require.Nil(t, bErr)
	assert.Equal(t, []byte{0, 1, 2, 3, 4, 5}, resp.Audio)
	assert.Equal(t, "audio/L16;codec=pcm;rate=24000", resp.ContentType)
}

// Large-response passthrough hands the caller the vendor's bytes. A speech answer is base64
// audio inside JSON, so it is always read whole and converted, whatever the threshold.
func TestVertexSpeechConvertsTheAnswerWhenLargeResponseModeIsOn(t *testing.T) {
	upstream := newVertexSpeechUpstream(t, vertexSpeechFinalChunk, false)
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	ctx.SetValue(schemas.BifrostContextKeyLargeResponseThreshold, int64(1))

	resp, bErr := upstream.provider.Speech(ctx, vertexSpeechAPIKey(), vertexSpeechRequest(vertexSpeechVoice(schemas.SpeechParameters{ResponseFormat: "pcm"})))
	require.Nil(t, bErr)
	assert.Equal(t, []byte{0, 1, 2, 3, 4, 5}, resp.Audio)
	require.NotNil(t, resp.Usage)
	assert.Equal(t, 50, resp.Usage.OutputTokens)
	isLarge, _ := ctx.Value(schemas.BifrostContextKeyLargeResponseMode).(bool)
	assert.False(t, isLarge, "the vendor envelope is never passed through as the speech response")
}

// A key with service-account credentials authenticates with its OAuth2 token and no key
// query parameter.
func TestVertexSpeechAuthenticatesWithTheKeyCredentials(t *testing.T) {
	upstream := newVertexSpeechUpstream(t, vertexSpeechFinalChunk, false)
	key := schemas.Key{VertexKeyConfig: &schemas.VertexKeyConfig{
		ProjectID:       *schemas.NewSecretVar("proj"),
		Region:          *schemas.NewSecretVar("global"),
		AuthCredentials: *schemas.NewSecretVar("test-credentials"),
	}}
	upstream.provider.tokenSources.Store(getClientKey(vertexCredentialIdentity(key)), oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "test-token"}))
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)

	_, bErr := upstream.provider.Speech(ctx, key, vertexSpeechRequest(vertexSpeechVoice(schemas.SpeechParameters{})))
	require.Nil(t, bErr)
	sent := upstream.snapshot()
	assert.Equal(t, 1, sent.calls)
	assert.Equal(t, "Bearer test-token", sent.auth)
	assert.Empty(t, sent.query)
	assert.Equal(t, "/v1/projects/proj/locations/global/publishers/google/models/"+vertexSpeechModel+":generateContent", sent.path)
}

// Contract C6: a speech Vertex blocked with a 200 is a 400 caller_content_filtered carrying
// the vendor's reason and the usage it counted, never an empty audio.
func TestVertexSpeechBlockedIsContentFilteredError(t *testing.T) {
	for name, tc := range map[string]struct {
		body     string
		wantCode string
	}{
		"prompt blocked":      {vertexSpeechPromptBlocked, "SAFETY"},
		"safety finishReason": {vertexSpeechSafetyFinishChunk, "PROHIBITED_CONTENT"},
	} {
		t.Run(name, func(t *testing.T) {
			upstream := newVertexSpeechUpstream(t, tc.body, false)
			ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
			resp, bErr := upstream.provider.Speech(ctx, vertexSpeechAPIKey(), vertexSpeechRequest(vertexSpeechVoice(schemas.SpeechParameters{})))
			require.Nil(t, resp)
			require.NotNil(t, bErr)
			require.NotNil(t, bErr.StatusCode)
			assert.Equal(t, 400, *bErr.StatusCode)
			assert.Equal(t, schemas.ErrorTypeCallerContentFiltered, bErr.ExtraFields.ErrorType)
			require.NotNil(t, bErr.Error.Code)
			assert.Equal(t, tc.wantCode, *bErr.Error.Code)
			require.NotNil(t, bErr.ExtraFields.BilledUsage, "tokens the vendor counted before blocking are billed")
			assert.Equal(t, 7, bErr.ExtraFields.BilledUsage.PromptTokens)
			assert.Equal(t, 1, upstream.snapshot().calls, "one upstream call per request")
		})
	}
}

// Contract C4: a field or value generateContent cannot take is refused before anything is
// sent. Fields are refused by core against what the provider declares; values by the provider.
func TestVertexSpeechRefusesWhatItCannotSend(t *testing.T) {
	upstream := newVertexSpeechUpstream(t, vertexSpeechFinalChunk, false)
	declared := func(req *schemas.BifrostSpeechRequest, requestType schemas.RequestType) *schemas.BifrostError {
		fields, ok := upstream.provider.SupportedUnifiedFields(requestType)
		require.True(t, ok, "Vertex declares its speech fields")
		return schemas.CheckUnifiedFields(&schemas.BifrostRequest{RequestType: requestType, SpeechRequest: req}, fields)
	}
	assertRefused := func(bErr *schemas.BifrostError, param string) {
		t.Helper()
		require.NotNil(t, bErr, "%s was not refused", param)
		require.NotNil(t, bErr.StatusCode)
		assert.Equal(t, 400, *bErr.StatusCode)
		assert.Equal(t, schemas.ErrorTypeCallerUnsupportedParameter, bErr.ExtraFields.ErrorType)
		require.NotNil(t, bErr.Error.Param)
		assert.Equal(t, param, bErr.Error.Param)
	}

	for _, requestType := range []schemas.RequestType{schemas.SpeechRequest, schemas.SpeechStreamRequest} {
		assertRefused(declared(vertexSpeechRequest(vertexSpeechVoice(schemas.SpeechParameters{Speed: schemas.Ptr(1.5)})), requestType), "speed")
		assertRefused(declared(vertexSpeechRequest(vertexSpeechVoice(schemas.SpeechParameters{Instructions: "Speak slowly."})), requestType), "instructions")
		assert.Nil(t, declared(vertexSpeechRequest(vertexSpeechVoice(schemas.SpeechParameters{ResponseFormat: "wav", MaxOutputTokens: schemas.Ptr(64)})), requestType))
	}

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	mp3 := vertexSpeechRequest(vertexSpeechVoice(schemas.SpeechParameters{ResponseFormat: "mp3"}))
	_, bErr := upstream.provider.Speech(ctx, vertexSpeechAPIKey(), mp3)
	assertRefused(bErr, "response_format")
	_, bErr = upstream.provider.SpeechStream(ctx, nil, func(context.Context) {}, vertexSpeechAPIKey(), mp3)
	assertRefused(bErr, "response_format")
	_, bErr = upstream.provider.Speech(ctx, vertexSpeechAPIKey(), vertexSpeechRequest(vertexSpeechVoice(schemas.SpeechParameters{MaxOutputTokens: schemas.Ptr(0)})))
	assertRefused(bErr, "max_output_tokens")
	assert.Zero(t, upstream.snapshot().calls, "a refused request makes no upstream call")
}

// Vertex serves speech for Gemini models only; any other model keeps the unsupported error.
func TestVertexSpeechIsGeminiOnly(t *testing.T) {
	upstream := newVertexSpeechUpstream(t, vertexSpeechFinalChunk, false)
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	req := vertexSpeechRequest(vertexSpeechVoice(schemas.SpeechParameters{}))
	req.Model = "claude-sonnet-4-6"
	_, bErr := upstream.provider.Speech(ctx, vertexSpeechAPIKey(), req)
	require.NotNil(t, bErr)
	_, bErr = upstream.provider.SpeechStream(ctx, nil, func(context.Context) {}, vertexSpeechAPIKey(), req)
	require.NotNil(t, bErr)
	assert.Zero(t, upstream.snapshot().calls)
}

// Contract C10: a streamed request reaches :streamGenerateContent as SSE in one call, each
// vendor frame with audio is a delta, and the done frame carries the vendor's usage.
func TestVertexSpeechStreamSendsDeltasAndADoneFrameWithUsage(t *testing.T) {
	upstream := newVertexSpeechUpstream(t, vertexSSE(vertexSpeechAudioChunk, vertexSpeechFinalChunk), true)
	chunks := collectVertexSpeechStream(t, upstream, vertexSpeechVoice(schemas.SpeechParameters{}))

	sent := upstream.snapshot()
	assert.Equal(t, 1, sent.calls, "one upstream call per request")
	assert.Equal(t, "/v1/projects/proj/locations/us-central1/publishers/google/models/"+vertexSpeechModel+":streamGenerateContent", sent.path)
	assert.Equal(t, "alt=sse&key=api-key", sent.query)

	require.Len(t, chunks, 3)
	for _, chunk := range chunks[:2] {
		require.NotNil(t, chunk.BifrostSpeechStreamResponse)
		assert.Equal(t, schemas.SpeechStreamResponseTypeDelta, chunk.BifrostSpeechStreamResponse.Type)
		assert.Equal(t, []byte{0, 1, 2, 3, 4, 5}, chunk.BifrostSpeechStreamResponse.Audio)
	}
	done := chunks[2].BifrostSpeechStreamResponse
	require.NotNil(t, done)
	assert.Equal(t, schemas.SpeechStreamResponseTypeDone, done.Type)
	require.NotNil(t, done.Usage)
	assert.Equal(t, 7, done.Usage.InputTokens)
	assert.Equal(t, 50, done.Usage.OutputTokens)
	assert.Equal(t, 69, done.Usage.TotalTokens)
	require.NotNil(t, done.Usage.OutputTokenDetails)
	assert.Equal(t, 50, done.Usage.OutputTokenDetails.AudioTokens)
	assert.Equal(t, 12, done.Usage.OutputTokenDetails.ReasoningTokens)
}

// Contract C6 and C10 on the stream: a block ends it on the content-filter error and a body
// without the vendor's end marker on the truncation error, never on a done frame; both carry
// the usage the vendor counted.
func TestVertexSpeechStreamEndsOnAnErrorWhenBlockedOrTruncated(t *testing.T) {
	for name, tc := range map[string]struct {
		events     []string
		wantDeltas int
		wantStatus int
		wantType   schemas.ErrorType
		wantBilled int
	}{
		"prompt blocked":                  {[]string{vertexSpeechPromptBlocked}, 0, 400, schemas.ErrorTypeCallerContentFiltered, 7},
		"safety finishReason after audio": {[]string{vertexSpeechAudioChunk, vertexSpeechSafetyFinishChunk}, 1, 400, schemas.ErrorTypeCallerContentFiltered, 7},
		// Core classifies the truncation error (provider_connection_failed) after the provider.
		"usage but no finishReason": {[]string{vertexSpeechAudioChunk, `{` + vertexSpeechUsage + `}`}, 1, 502, "", 7},
	} {
		t.Run(name, func(t *testing.T) {
			upstream := newVertexSpeechUpstream(t, vertexSSE(tc.events...), true)
			chunks := collectVertexSpeechStream(t, upstream, vertexSpeechVoice(schemas.SpeechParameters{}))
			require.Len(t, chunks, tc.wantDeltas+1)
			for _, chunk := range chunks[:tc.wantDeltas] {
				require.NotNil(t, chunk.BifrostSpeechStreamResponse)
				assert.Equal(t, schemas.SpeechStreamResponseTypeDelta, chunk.BifrostSpeechStreamResponse.Type)
			}
			last := chunks[len(chunks)-1]
			require.NotNil(t, last.BifrostError, "the stream must end on the error")
			assert.Nil(t, last.BifrostSpeechStreamResponse, "no done frame")
			require.NotNil(t, last.BifrostError.StatusCode)
			assert.Equal(t, tc.wantStatus, *last.BifrostError.StatusCode)
			if tc.wantType == "" {
				assert.Equal(t, schemas.ErrProviderStreamTruncated, last.BifrostError.Error.Message)
			} else {
				assert.Equal(t, tc.wantType, last.BifrostError.ExtraFields.ErrorType)
			}
			require.NotNil(t, last.BifrostError.ExtraFields.BilledUsage)
			assert.Equal(t, tc.wantBilled, last.BifrostError.ExtraFields.BilledUsage.PromptTokens)
			assert.Equal(t, 1, upstream.snapshot().calls, "one upstream call per request")
		})
	}
}

// The request-path work a Vertex speech request adds before the upstream call: building the
// body (the Gemini conversion plus the Vertex marshal) and resolving the URL and credentials
// for an API key. "gemini body" is the Gemini API path's conversion and marshal, for scale.
func BenchmarkVertexGeminiSpeechRequest(b *testing.B) {
	config := &schemas.ProviderConfig{NetworkConfig: schemas.NetworkConfig{DefaultRequestTimeoutInSeconds: 10}}
	config.CheckAndSetDefaults()
	provider, err := NewVertexProvider(config, nil)
	if err != nil {
		b.Fatal(err)
	}
	req := vertexSpeechRequest(vertexSpeechVoice(schemas.SpeechParameters{MaxOutputTokens: schemas.Ptr(2048)}))
	key := vertexSpeechAPIKey()
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)

	b.Run("body", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := vertexGeminiSpeechRequestBody(req); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("gemini body", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			body, err := gemini.ToGeminiSpeechRequest(req)
			if err != nil {
				b.Fatal(err)
			}
			if _, err := sonic.Marshal(body); err != nil {
				b.Fatal(err)
			}
		}
	})
	for name, stream := range map[string]bool{"target": false, "target stream": true} {
		method := ":generateContent"
		if stream {
			method = ":streamGenerateContent"
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, bErr := provider.geminiSpeechTarget(ctx, key, req.Model, method, stream); bErr != nil {
					b.Fatal(bErr.Error.Message)
				}
			}
		})
	}
	b.Run("declared fields check", func(b *testing.B) {
		fields, _ := provider.SupportedUnifiedFields(schemas.SpeechRequest)
		bifrostReq := &schemas.BifrostRequest{RequestType: schemas.SpeechRequest, SpeechRequest: req}
		b.ReportAllocs()
		for b.Loop() {
			if bErr := schemas.CheckUnifiedFields(bifrostReq, fields); bErr != nil {
				b.Fatal(bErr.Error.Message)
			}
		}
	})
}
