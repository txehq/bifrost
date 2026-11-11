package xai_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/network/proxytest"
	"github.com/capsohq/bifrost/core/providers/xai"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// xaiCapturedRequest is one request the mock xAI received.
type xaiCapturedRequest struct {
	method, path, auth, contentType string
	body                            []byte
}

// newMockXAI serves handlers keyed by "METHOD /path" and records every request it receives,
// answered or not. A path with no handler answers 404.
func newMockXAI(t *testing.T, handlers map[string]http.HandlerFunc) (*xai.XAIProvider, func() []xaiCapturedRequest) {
	t.Helper()
	var mu sync.Mutex
	var captured []xaiCapturedRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		captured = append(captured, xaiCapturedRequest{method: r.Method, path: r.URL.Path, auth: r.Header.Get("Authorization"), contentType: r.Header.Get("Content-Type"), body: body})
		mu.Unlock()
		if handler, ok := handlers[r.Method+" "+r.URL.Path]; ok {
			handler(w, r)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(server.Close)

	provider, err := xai.NewXAIProvider(&schemas.ProviderConfig{
		NetworkConfig: schemas.NetworkConfig{BaseURL: server.URL, DefaultRequestTimeoutInSeconds: 30, AllowPrivateNetwork: true},
	}, bifrost.NewNoOpLogger())
	require.NoError(t, err)
	return provider, func() []xaiCapturedRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]xaiCapturedRequest(nil), captured...)
	}
}

func xaiJSON(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

func xaiTestContext(t *testing.T) *schemas.BifrostContext {
	t.Helper()
	ctx, cancel := schemas.NewBifrostContextWithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func xaiTestKey() schemas.Key {
	return schemas.Key{Value: *schemas.NewSecretVar("xai-test"), Models: []string{"*"}}
}

// A video create maps every unified field onto xAI's own field and merges the caller's extra
// fields unchanged, without the extra-params header (TXE-6429, contract C4/C7).
func TestXAIVideoGenerationMapsUnifiedFields(t *testing.T) {
	t.Parallel()
	provider, captured := newMockXAI(t, map[string]http.HandlerFunc{
		"POST /v1/videos/generations": xaiJSON(http.StatusOK, `{"request_id":"d97415a1-5796-b7ec-379f-4e6819e08fdf"}`),
	})

	created, bifrostErr := provider.VideoGeneration(xaiTestContext(t), xaiTestKey(), &schemas.BifrostVideoGenerationRequest{
		Provider: schemas.XAI,
		Model:    "grok-imagine-video-1.5",
		Input: &schemas.VideoGenerationInput{
			Prompt:         "a serene lake at sunrise",
			InputReference: schemas.Ptr("https://example.com/first.jpg"),
		},
		Params: &schemas.VideoGenerationParameters{
			Seconds:         schemas.Ptr("8"),
			AspectRatio:     "16:9",
			Resolution:      "720p",
			LastFrame:       "data:image/png;base64,AAAA",
			ReferenceImages: schemas.VideoReferenceImages{"https://example.com/ref1.jpg", "https://example.com/ref2.jpg"},
			NegativePrompt:  schemas.Ptr("blur"),
			Seed:            schemas.Ptr(7),
			TaskControlParameters: schemas.TaskControlParameters{
				OutputStorageURI: schemas.Ptr("https://storage.example.com/signed-put-url"),
			},
			ExtraParams: map[string]interface{}{
				// The handlers keep the unified fields in the extra params too.
				"aspect_ratio":     "16:9",
				"resolution":       "720p",
				"last_frame":       "data:image/png;base64,AAAA",
				"reference_images": []interface{}{"https://example.com/ref1.jpg", "https://example.com/ref2.jpg"},
				"keyframes":        []interface{}{map[string]interface{}{"image": map[string]interface{}{"url": "https://example.com/k.jpg"}, "timestamp_s": 2.0}},
				"reference_audios": []interface{}{map[string]interface{}{"voice_id": "eve"}},
				"storage_options":  map[string]interface{}{"filename": "lake.mp4", "expires_after": 86400},
				"user":             "user-123",
				"generate_audio":   false,
			},
		},
	})
	require.Nil(t, bifrostErr)
	require.Equal(t, "d97415a1-5796-b7ec-379f-4e6819e08fdf:xai", created.ID)
	require.Equal(t, schemas.VideoStatusQueued, created.Status)
	require.Empty(t, created.ProviderStatus, "xAI reports no status on a create")
	require.Zero(t, created.CreatedAt, "xAI reports no creation time")

	requests := captured()
	require.Len(t, requests, 1)
	require.Equal(t, "Bearer xai-test", requests[0].auth)
	body := gjson.ParseBytes(requests[0].body)
	assert.Equal(t, "grok-imagine-video-1.5", body.Get("model").String())
	assert.Equal(t, "a serene lake at sunrise", body.Get("prompt").String())
	assert.Equal(t, gjson.Number, body.Get("duration").Type)
	assert.Equal(t, int64(8), body.Get("duration").Int())
	assert.Equal(t, "16:9", body.Get("aspect_ratio").String())
	assert.Equal(t, "720p", body.Get("resolution").String())
	assert.Equal(t, "https://example.com/first.jpg", body.Get("image.url").String())
	assert.Equal(t, "data:image/png;base64,AAAA", body.Get("last_frame.url").String())
	assert.Equal(t, "https://example.com/ref1.jpg", body.Get("reference_images.0.url").String())
	assert.Equal(t, "https://example.com/ref2.jpg", body.Get("reference_images.1.url").String())
	assert.Equal(t, "https://storage.example.com/signed-put-url", body.Get("output.upload_url").String())
	assert.Equal(t, "https://example.com/k.jpg", body.Get("keyframes.0.image.url").String())
	assert.Equal(t, 2.0, body.Get("keyframes.0.timestamp_s").Float())
	assert.Equal(t, "eve", body.Get("reference_audios.0.voice_id").String())
	assert.Equal(t, "lake.mp4", body.Get("storage_options.filename").String())
	assert.Equal(t, int64(86400), body.Get("storage_options.expires_after").Int())
	assert.Equal(t, "user-123", body.Get("user").String())
	assert.Equal(t, gjson.False, body.Get("generate_audio").Type)
	for _, absent := range []string{"seconds", "size", "input_reference", "output_storage_uri", "negative_prompt", "seed"} {
		assert.False(t, body.Get(absent).Exists(), "%s must not reach xAI under its unified name", absent)
	}
}

// size has no xAI field: it is sent as the aspect ratio and tier it names exactly, and
// refused when it names none or contradicts the ones sent with it.
func TestXAIVideoGenerationSize(t *testing.T) {
	t.Parallel()
	provider, captured := newMockXAI(t, map[string]http.HandlerFunc{
		"POST /v1/videos/generations": xaiJSON(http.StatusOK, `{"request_id":"req-size"}`),
	})
	create := func(params *schemas.VideoGenerationParameters) *schemas.BifrostError {
		_, bifrostErr := provider.VideoGeneration(xaiTestContext(t), xaiTestKey(), &schemas.BifrostVideoGenerationRequest{
			Provider: schemas.XAI, Model: "grok-imagine-video", Input: &schemas.VideoGenerationInput{Prompt: "a dog"}, Params: params,
		})
		return bifrostErr
	}

	require.Nil(t, create(&schemas.VideoGenerationParameters{Size: "1280x720"}))
	require.Nil(t, create(&schemas.VideoGenerationParameters{Size: "1080x1920", AspectRatio: "9:16"}))
	requests := captured()
	require.Len(t, requests, 2)
	assert.Equal(t, "16:9", gjson.GetBytes(requests[0].body, "aspect_ratio").String())
	assert.Equal(t, "720p", gjson.GetBytes(requests[0].body, "resolution").String())
	assert.Equal(t, "9:16", gjson.GetBytes(requests[1].body, "aspect_ratio").String())
	assert.Equal(t, "1080p", gjson.GetBytes(requests[1].body, "resolution").String())

	// Every ratio xAI documents is reachable from a size, the one whose name is not in
	// lowest terms included (21:9 is 7:3).
	for size, want := range map[string][2]string{
		"720x720":   {"1:1", "720p"},
		"960x720":   {"4:3", "720p"},
		"480x640":   {"3:4", "480p"},
		"1620x1080": {"3:2", "1080p"},
		"720x1080":  {"2:3", "720p"},
		"2520x1080": {"21:9", "1080p"},
		"1680x720":  {"21:9", "720p"},
		"1800x720":  {"5:2", "720p"},
	} {
		converted, err := xai.ToXAIVideoGenerationRequest(&schemas.BifrostVideoGenerationRequest{
			Model: "grok-imagine-video", Input: &schemas.VideoGenerationInput{Prompt: "a dog"},
			Params: &schemas.VideoGenerationParameters{Size: size},
		})
		require.NoError(t, err, size)
		assert.Equal(t, want, [2]string{converted.AspectRatio, converted.Resolution}, size)
	}

	for name, params := range map[string]*schemas.VideoGenerationParameters{
		"not a ratio xAI generates": {Size: "1000x700"},
		"not a tier xAI generates":  {Size: "1920x1200"},
		"near 720p is not 720p":     {Size: "1280x704"},
		"not WIDTHxHEIGHT":          {Size: "720p"},
		"contradicts aspect_ratio":  {Size: "1280x720", AspectRatio: "1:1"},
		"contradicts resolution":    {Size: "1280x720", Resolution: "480p"},
	} {
		t.Run(name, func(t *testing.T) {
			requireUnsupportedParameter(t, create(params), "size")
		})
	}
	requireUnsupportedParameter(t, create(&schemas.VideoGenerationParameters{Seconds: schemas.Ptr("5.5")}), "seconds")
	_, bifrostErr := provider.VideoGeneration(xaiTestContext(t), xaiTestKey(), &schemas.BifrostVideoGenerationRequest{
		Provider: schemas.XAI, Model: "grok-imagine-video",
		Input: &schemas.VideoGenerationInput{Prompt: "a dog", VideoURI: schemas.Ptr("https://example.com/src.mp4")},
	})
	requireUnsupportedParameter(t, bifrostErr, "video_uri")
	require.Len(t, captured(), 2, "a refused create must make no upstream call")
}

// BenchmarkToXAIVideoGenerationRequest measures the conversion a video create pays before it
// is sent: the unified fields, a size split into ratio and tier, and the extra-params copy.
func BenchmarkToXAIVideoGenerationRequest(b *testing.B) {
	minimal := &schemas.BifrostVideoGenerationRequest{
		Model: "grok-imagine-video-1.5", Input: &schemas.VideoGenerationInput{Prompt: "a paper boat on a pond"},
	}
	full := &schemas.BifrostVideoGenerationRequest{
		Model: "grok-imagine-video-1.5",
		Input: &schemas.VideoGenerationInput{Prompt: "a paper boat on a pond", InputReference: schemas.Ptr("https://media.example.invalid/first.jpg")},
		Params: &schemas.VideoGenerationParameters{
			Seconds: schemas.Ptr("8"), Size: "1280x720", LastFrame: "https://media.example.invalid/last.jpg",
			ReferenceImages:       schemas.VideoReferenceImages{"https://media.example.invalid/ref1.jpg", "https://media.example.invalid/ref2.jpg"},
			TaskControlParameters: schemas.TaskControlParameters{OutputStorageURI: schemas.Ptr("https://storage.example.invalid/put")},
			ExtraParams: map[string]interface{}{
				"last_frame": "https://media.example.invalid/last.jpg", "user": "user-123",
				"reference_images": []interface{}{"https://media.example.invalid/ref1.jpg", "https://media.example.invalid/ref2.jpg"},
			},
		},
	}
	for name, request := range map[string]*schemas.BifrostVideoGenerationRequest{"prompt only": minimal, "every unified field": full} {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := xai.ToXAIVideoGenerationRequest(request); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkToBifrostVideoGenerationResponse measures the conversion of a finished task, the
// largest answer a query returns.
func BenchmarkToBifrostVideoGenerationResponse(b *testing.B) {
	task := &xai.XAIVideoTask{
		Status: "done", Progress: schemas.Ptr(100.0), Model: "grok-imagine-video-1.5",
		Video: &xai.XAIVideoResult{URL: schemas.Ptr("https://vidgen.fixture.invalid/xai-video-req-1.mp4"), Duration: schemas.Ptr(6.0), RespectModeration: schemas.Ptr(true)},
		Usage: &xai.XAIVideoUsage{CostInUsdTicks: schemas.Ptr(int64(3000000000)), OutputTokens: schemas.Ptr(4800)},
	}
	b.ReportAllocs()
	for b.Loop() {
		_ = xai.ToBifrostVideoGenerationResponse(task)
	}
}

// BenchmarkXAISupportedUnifiedFields measures what core's field check asks of the provider on
// every request: a declared operation returns a package-level list, an undeclared one nothing.
func BenchmarkXAISupportedUnifiedFields(b *testing.B) {
	provider, err := xai.NewXAIProvider(&schemas.ProviderConfig{NetworkConfig: schemas.NetworkConfig{BaseURL: "http://127.0.0.1:1"}}, bifrost.NewNoOpLogger())
	if err != nil {
		b.Fatal(err)
	}
	for name, requestType := range map[string]schemas.RequestType{"video create": schemas.VideoGenerationRequest, "chat (undeclared)": schemas.ChatCompletionRequest} {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_, _ = provider.SupportedUnifiedFields(requestType)
			}
		})
	}
}

// A last_frame the caller sends in xAI's own object form is not a unified string, so the
// typed field is empty and the object is merged as it was sent.
func TestXAIVideoGenerationKeepsVendorShapedExtra(t *testing.T) {
	t.Parallel()
	provider, captured := newMockXAI(t, map[string]http.HandlerFunc{
		"POST /v1/videos/generations": xaiJSON(http.StatusOK, `{"request_id":"req-extra"}`),
	})
	_, bifrostErr := provider.VideoGeneration(xaiTestContext(t), xaiTestKey(), &schemas.BifrostVideoGenerationRequest{
		Provider: schemas.XAI, Model: "grok-imagine-video-1.5", Input: &schemas.VideoGenerationInput{Prompt: "a dog"},
		Params: &schemas.VideoGenerationParameters{ExtraParams: map[string]interface{}{
			"last_frame": map[string]interface{}{"file_id": "file-123"},
		}},
	})
	require.Nil(t, bifrostErr)
	requests := captured()
	require.Len(t, requests, 1)
	assert.Equal(t, "file-123", gjson.GetBytes(requests[0].body, "last_frame.file_id").String())
}

// xAI cannot find a task again (recovery method 5), so a create is written to xAI exactly
// once, also when the connection fails. Every other xAI call shares a pooled client that
// resends a request whose reused socket dies before the first response byte; the create must
// not, because the socket can die after xAI read the request. The query below leaves a
// keep-alive socket in that pool, which a create sent through it would reuse and then resend.
func TestXAIVideoGenerationIsSentOnceWhenTheConnectionFails(t *testing.T) {
	t.Parallel()
	provider, captured := newMockXAI(t, map[string]http.HandlerFunc{
		"GET /v1/videos/warm": xaiJSON(http.StatusOK, `{"status":"pending","progress":1}`),
		"POST /v1/videos/generations": func(w http.ResponseWriter, _ *http.Request) {
			// The request was read in full; the connection dies before any response byte.
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
		},
	})
	ctx := xaiTestContext(t)
	_, bifrostErr := provider.VideoRetrieve(ctx, xaiTestKey(), &schemas.BifrostVideoRetrieveRequest{Provider: schemas.XAI, ID: "warm:xai"})
	require.Nil(t, bifrostErr)

	_, bifrostErr = provider.VideoGeneration(ctx, xaiTestKey(), &schemas.BifrostVideoGenerationRequest{
		Provider: schemas.XAI, Model: "grok-imagine-video", Input: &schemas.VideoGenerationInput{Prompt: "a dog"},
	})
	require.NotNil(t, bifrostErr, "a create whose connection died must fail")
	require.False(t, bifrostErr.IsBifrostError, "the upstream connection failed, not Bifrost")

	creates := 0
	for _, request := range captured() {
		if request.method == http.MethodPost && request.path == "/v1/videos/generations" {
			creates++
		}
	}
	require.Equal(t, 1, creates, "the create must reach xAI exactly once")
}

// A create xAI accepted with a 2xx whose answer cannot be used (no request_id, or not JSON)
// may have started a task whose id is lost, so it is marked like a create that got no
// response: core neither retries it nor sends it to a fallback. An xAI error response is an
// answer that no task was created, and is not marked.
func TestXAIVideoGenerationUnusableAnswerIsNotRepeatable(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		handler     http.HandlerFunc
		wantUnknown bool
	}{
		"2xx with no request_id": {xaiJSON(http.StatusOK, `{}`), true},
		"2xx that is not JSON":   {xaiJSON(http.StatusOK, `<html>gateway</html>`), true},
		"an xAI error response":  {xaiJSON(http.StatusServiceUnavailable, `{"code":"service_unavailable","error":"overloaded"}`), false},
	} {
		t.Run(name, func(t *testing.T) {
			provider, captured := newMockXAI(t, map[string]http.HandlerFunc{"POST /v1/videos/generations": tc.handler})
			ctx := xaiTestContext(t)
			_, bifrostErr := provider.VideoGeneration(ctx, xaiTestKey(), &schemas.BifrostVideoGenerationRequest{
				Provider: schemas.XAI, Model: "grok-imagine-video", Input: &schemas.VideoGenerationInput{Prompt: "a dog"},
			})
			require.NotNil(t, bifrostErr)
			unknown, _ := ctx.Value(schemas.BifrostContextKeyUpstreamWriteUnknown).(bool)
			require.Equal(t, tc.wantUnknown, unknown)
			require.Len(t, captured(), 1)
		})
	}
}

// A query returns the task object: status mapped one to one with xAI's own word in
// provider_status, xAI's usage and video duration in the unified usage, the public link as a
// url artifact.
func TestXAIVideoRetrieveMapsTheTaskObject(t *testing.T) {
	t.Parallel()
	const done = `{"status":"done","progress":100,"model":"grok-imagine-video","video":{"url":"https://vidgen.x.ai/xai-vidgen-bucket/xai-video-req-1.mp4","duration":6,"respect_moderation":true},"usage":{"cost_in_usd_ticks":3000000000,"input_tokens":12,"output_tokens":4800,"total_tokens":4812}}`
	provider, _ := newMockXAI(t, map[string]http.HandlerFunc{
		"GET /v1/videos/req-1":       xaiJSON(http.StatusOK, done),
		"GET /v1/videos/req-pending": xaiJSON(http.StatusOK, `{"status":"pending","progress":42,"model":"grok-imagine-video"}`),
		"GET /v1/videos/req-failed":  xaiJSON(http.StatusOK, `{"status":"failed","error":{"code":"invalid_argument","message":"unsupported duration"}}`),
		"GET /v1/videos/req-expired": xaiJSON(http.StatusOK, `{"status":"expired"}`),
		"GET /v1/videos/req-new":     xaiJSON(http.StatusOK, `{"status":"archived"}`),
	})
	ctx := xaiTestContext(t)
	ctx.SetValue(schemas.BifrostContextKeyCaptureRawResponse, true)
	retrieve := func(id string) *schemas.BifrostVideoGenerationResponse {
		t.Helper()
		resp, bifrostErr := provider.VideoRetrieve(ctx, xaiTestKey(), &schemas.BifrostVideoRetrieveRequest{Provider: schemas.XAI, ID: id + ":xai"})
		require.Nil(t, bifrostErr)
		require.Equal(t, id+":xai", resp.ID)
		return resp
	}

	completed := retrieve("req-1")
	require.Equal(t, schemas.VideoStatusCompleted, completed.Status)
	require.Equal(t, "done", completed.ProviderStatus)
	require.Equal(t, "grok-imagine-video", completed.Model)
	require.Equal(t, schemas.Ptr("6"), completed.Seconds)
	require.Nil(t, completed.ContentFilter)
	require.Nil(t, completed.ExpiresAt, "xAI states no link lifetime")
	require.NotNil(t, completed.ExtraFields.RawResponse)

	// The wire form, as the caller reads it.
	encoded, err := schemas.Marshal(completed)
	require.NoError(t, err)
	wire := gjson.ParseBytes(encoded)
	assert.Equal(t, "completed", wire.Get("status").String())
	assert.Equal(t, "done", wire.Get("provider_status").String())
	assert.Equal(t, int64(4800), wire.Get("usage.output_tokens").Int())
	assert.Equal(t, 6.0, wire.Get("usage.output_seconds").Float())
	assert.Equal(t, 3000000000.0, wire.Get("usage.vendor_cost.amount").Float())
	assert.Equal(t, "usd_ticks", wire.Get("usage.vendor_cost.unit").String())
	assert.Equal(t, 0.3, wire.Get("usage.cost.total_cost").Float())
	assert.Equal(t, int64(1), wire.Get("videos.#").Int())
	assert.Equal(t, "url", wire.Get("videos.0.type").String())
	assert.Equal(t, "https://vidgen.x.ai/xai-vidgen-bucket/xai-video-req-1.mp4", wire.Get("videos.0.url").String())
	assert.Equal(t, "video/mp4", wire.Get("videos.0.content_type").String())
	for _, absent := range []string{"usage.input_seconds", "usage.input_images", "usage.compute_seconds", "created_at", "expires_at"} {
		assert.False(t, wire.Get(absent).Exists(), "%s was not reported by xAI and must stay empty", absent)
	}

	pending := retrieve("req-pending")
	require.Equal(t, schemas.VideoStatusInProgress, pending.Status)
	require.Equal(t, "pending", pending.ProviderStatus)
	require.Equal(t, schemas.Ptr(42.0), pending.Progress)
	require.Nil(t, pending.Usage, "a task xAI reports no usage for carries none")
	require.Empty(t, pending.Videos)

	failed := retrieve("req-failed")
	require.Equal(t, schemas.VideoStatusFailed, failed.Status)
	require.Equal(t, "failed", failed.ProviderStatus)
	require.Equal(t, &schemas.VideoCreateError{Code: "invalid_argument", Message: "unsupported duration"}, failed.Error)
	require.Nil(t, failed.ContentFilter)

	expired := retrieve("req-expired")
	require.Equal(t, schemas.VideoStatusExpired, expired.Status)
	require.Equal(t, "expired", expired.ProviderStatus)

	// A status xAI adds later is not known to be terminal, so it is not reported as one.
	unknown := retrieve("req-new")
	require.Equal(t, schemas.VideoStatusInProgress, unknown.Status)
	require.Equal(t, "archived", unknown.ProviderStatus)
}

// xAI reports a moderated task as done with respect_moderation=false and no link. The task
// object reports it as failed with content_filter, keeps xAI's status word, and still carries
// the usage xAI billed (xAI charges a moderated request).
func TestXAIVideoRetrieveReportsModeration(t *testing.T) {
	t.Parallel()
	provider, _ := newMockXAI(t, map[string]http.HandlerFunc{
		"GET /v1/videos/req-mod": xaiJSON(http.StatusOK, `{"status":"done","progress":100,"model":"grok-imagine-video","video":{"url":"","duration":6,"respect_moderation":false},"usage":{"cost_in_usd_ticks":3000000000}}`),
	})
	resp, bifrostErr := provider.VideoRetrieve(xaiTestContext(t), xaiTestKey(), &schemas.BifrostVideoRetrieveRequest{Provider: schemas.XAI, ID: "req-mod:xai"})
	require.Nil(t, bifrostErr)
	require.Equal(t, schemas.VideoStatusFailed, resp.Status)
	require.Equal(t, "done", resp.ProviderStatus)
	require.Equal(t, &schemas.ContentFilterInfo{FilteredCount: 1, Reasons: []string{"respect_moderation"}}, resp.ContentFilter)
	require.NotNil(t, resp.Error)
	require.Empty(t, resp.Videos)
	require.NotNil(t, resp.Usage)
	require.Equal(t, &schemas.VendorCost{Amount: 3000000000, Unit: schemas.VendorCostUnitUSDTicks}, resp.Usage.VendorCost)
}

// The three task control fields that cannot reach xAI are refused before any upstream call,
// output_storage_uri is taken, and a cancel is refused because xAI has no cancel API.
func TestXAIVideoTaskControlAndCancel(t *testing.T) {
	t.Parallel()
	provider, captured := newMockXAI(t, nil)
	request := func(control schemas.TaskControlParameters) *schemas.BifrostRequest {
		return &schemas.BifrostRequest{
			RequestType: schemas.VideoGenerationRequest,
			VideoGenerationRequest: &schemas.BifrostVideoGenerationRequest{
				Provider: schemas.XAI, Model: "grok-imagine-video", Input: &schemas.VideoGenerationInput{Prompt: "a dog"},
				Params: &schemas.VideoGenerationParameters{TaskControlParameters: control},
			},
		}
	}
	requireUnsupportedParameter(t, schemas.CheckTaskControlFields(provider, request(schemas.TaskControlParameters{ClientReferenceID: schemas.Ptr("task-1")})), "client_reference_id")
	requireUnsupportedParameter(t, schemas.CheckTaskControlFields(provider, request(schemas.TaskControlParameters{CallbackURL: schemas.Ptr("https://hooks.example.test/v/task-1")})), "callback_url")
	requireUnsupportedParameter(t, schemas.CheckTaskControlFields(provider, request(schemas.TaskControlParameters{ExpiresAfterSeconds: schemas.Ptr(600)})), "expires_after_seconds")
	require.Nil(t, schemas.CheckTaskControlFields(provider, request(schemas.TaskControlParameters{OutputStorageURI: schemas.Ptr("https://storage.example.com/put")})))

	// A unified field xAI has no specified field for is refused the same way.
	audio := request(schemas.TaskControlParameters{})
	audio.VideoGenerationRequest.Params.Audio = schemas.Ptr(true)
	requireUnsupportedParameter(t, schemas.CheckDeclaredUnifiedFields(provider, audio), "audio")

	cancelErr := schemas.CheckVideoCancelSupported(provider, &schemas.BifrostRequest{
		RequestType:        schemas.VideoCancelRequest,
		VideoCancelRequest: &schemas.BifrostVideoCancelRequest{Provider: schemas.XAI, ID: "req-1:xai"},
	})
	require.NotNil(t, cancelErr)
	require.Equal(t, schemas.Ptr(http.StatusBadRequest), cancelErr.StatusCode)
	require.Equal(t, schemas.ErrorTypeProviderCancelUnsupported, cancelErr.ExtraFields.ErrorType)

	_, deleteErr := provider.VideoDelete(xaiTestContext(t), xaiTestKey(), &schemas.BifrostVideoDeleteRequest{Provider: schemas.XAI, ID: "req-1:xai"})
	require.NotNil(t, deleteErr)
	_, listErr := provider.VideoList(xaiTestContext(t), xaiTestKey(), &schemas.BifrostVideoListRequest{Provider: schemas.XAI})
	require.NotNil(t, listErr)
	require.Empty(t, captured(), "none of these may call xAI")
}

// A usage-policy refusal keeps xAI's status and body and is declared caller_content_filtered,
// so it is never mistaken for a dead key or a parameter error. xAI documents no code for one;
// the codes recognised are the ones every vendor shares.
func TestXAIVideoPolicyRefusalIsContentFiltered(t *testing.T) {
	t.Parallel()
	const refusal = `{"code":"content_policy_violation","error":"The prompt violates the usage policy."}`
	provider, _ := newMockXAI(t, map[string]http.HandlerFunc{
		"POST /v1/videos/generations": xaiJSON(http.StatusForbidden, refusal),
		"GET /v1/videos/req-bad":      xaiJSON(http.StatusBadRequest, `{"code":"invalid_argument","error":"unknown request"}`),
	})
	ctx := xaiTestContext(t)
	ctx.SetValue(schemas.BifrostContextKeyCaptureRawResponse, true)
	_, bifrostErr := provider.VideoGeneration(ctx, xaiTestKey(), &schemas.BifrostVideoGenerationRequest{
		Provider: schemas.XAI, Model: "grok-imagine-video", Input: &schemas.VideoGenerationInput{Prompt: "blocked"},
	})
	require.NotNil(t, bifrostErr)
	require.Equal(t, schemas.Ptr(http.StatusForbidden), bifrostErr.StatusCode)
	require.Equal(t, schemas.ErrorTypeCallerContentFiltered, bifrostErr.ExtraFields.ErrorType)
	require.Equal(t, schemas.Ptr("content_policy_violation"), bifrostErr.Error.Code)
	require.Equal(t, "The prompt violates the usage policy.", bifrostErr.Error.Message)
	rawBody, err := schemas.Marshal(bifrostErr.ExtraFields.RawResponse)
	require.NoError(t, err)
	require.JSONEq(t, refusal, string(rawBody))

	// Any other xAI error is left for the status-based classification.
	_, bifrostErr = provider.VideoRetrieve(ctx, xaiTestKey(), &schemas.BifrostVideoRetrieveRequest{Provider: schemas.XAI, ID: "req-bad:xai"})
	require.NotNil(t, bifrostErr)
	require.Equal(t, schemas.Ptr(http.StatusBadRequest), bifrostErr.StatusCode)
	require.Empty(t, bifrostErr.ExtraFields.ErrorType)
}

// The typed model is what governance and key selection authorized. An extra field named
// model, in any letter case, would replace it on the wire after that, so each operation
// refuses it before any upstream call.
func TestXAIMediaRefusesModelInExtraParams(t *testing.T) {
	t.Parallel()
	provider, captured := newMockXAI(t, nil)
	for _, name := range []string{"model", "Model", "MODEL"} {
		extra := map[string]interface{}{name: "grok-imagine-video-1.5-pro"}
		_, bifrostErr := provider.VideoGeneration(xaiTestContext(t), xaiTestKey(), &schemas.BifrostVideoGenerationRequest{
			Provider: schemas.XAI, Model: "grok-imagine-video", Input: &schemas.VideoGenerationInput{Prompt: "a dog"},
			Params: &schemas.VideoGenerationParameters{ExtraParams: extra},
		})
		require.NotNil(t, bifrostErr, name)
		require.Equal(t, schemas.Ptr(http.StatusBadRequest), bifrostErr.StatusCode, name)

		_, bifrostErr = provider.Speech(xaiTestContext(t), xaiTestKey(), xaiSpeechRequest(&schemas.SpeechParameters{
			VoiceConfig: &schemas.SpeechVoiceInput{Voice: schemas.Ptr("eve")}, ExtraParams: extra,
		}))
		require.NotNil(t, bifrostErr, name)
		require.Equal(t, schemas.Ptr(http.StatusBadRequest), bifrostErr.StatusCode, name)

		_, bifrostErr = provider.Transcription(xaiTestContext(t), xaiTestKey(), &schemas.BifrostTranscriptionRequest{
			Provider: schemas.XAI, Model: "grok-voice-transcribe-2.0", Input: &schemas.TranscriptionInput{File: []byte("a")},
			Params: &schemas.TranscriptionParameters{ExtraParams: extra},
		})
		require.NotNil(t, bifrostErr, name)
		require.Equal(t, schemas.Ptr(http.StatusBadRequest), bifrostErr.StatusCode, name)
	}
	require.Empty(t, captured(), "a refused request must not call xAI")
}

// Large-payload mode streams the caller's body upstream unparsed, which no xAI media endpoint
// can take: each of the three operations is refused with a 400 before any upstream call
// instead of sending xAI an empty or unconverted body.
func TestXAIMediaRefusesLargePayloadPassthrough(t *testing.T) {
	t.Parallel()
	provider, captured := newMockXAI(t, nil)
	largePayload := func() *schemas.BifrostContext {
		ctx := xaiTestContext(t)
		ctx.SetValue(schemas.BifrostContextKeyLargePayloadMode, true)
		ctx.SetValue(schemas.BifrostContextKeyLargePayloadReader, io.Reader(strings.NewReader("--boundary\r\n")))
		return ctx
	}
	requireRefused := func(bifrostErr *schemas.BifrostError, operation string) {
		t.Helper()
		require.NotNil(t, bifrostErr)
		require.Equal(t, schemas.Ptr(http.StatusBadRequest), bifrostErr.StatusCode)
		require.Contains(t, bifrostErr.Error.Message, "large payload passthrough is not supported for xai "+operation)
	}

	// The large-payload hook fills only routing metadata, so the inputs are empty.
	_, bifrostErr := provider.VideoGeneration(largePayload(), xaiTestKey(), &schemas.BifrostVideoGenerationRequest{Provider: schemas.XAI, Model: "grok-imagine-video"})
	requireRefused(bifrostErr, "video generation")
	_, bifrostErr = provider.Speech(largePayload(), xaiTestKey(), &schemas.BifrostSpeechRequest{Provider: schemas.XAI, Model: "grok-voice-tts"})
	requireRefused(bifrostErr, "speech")
	_, bifrostErr = provider.Transcription(largePayload(), xaiTestKey(), &schemas.BifrostTranscriptionRequest{Provider: schemas.XAI, Model: "grok-voice-transcribe-2.0"})
	requireRefused(bifrostErr, "transcription")
	require.Empty(t, captured(), "a refused request must not call xAI")
}

// A custom provider on the xAI base serves only the operations its allowed_requests lists
// (contract C12): each of the five new operations checks its own permission before anything
// is sent, and a download is not gated by the query permission.
func TestXAIMediaHonoursAllowedRequests(t *testing.T) {
	t.Parallel()
	var upstream atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstream.Add(1)
		xaiJSON(http.StatusOK, `{"status":"pending"}`)(w, nil)
	}))
	t.Cleanup(server.Close)
	newProvider := func(allowed *schemas.AllowedRequests) *xai.XAIProvider {
		provider, err := xai.NewXAIProvider(&schemas.ProviderConfig{
			NetworkConfig:        schemas.NetworkConfig{BaseURL: server.URL, DefaultRequestTimeoutInSeconds: 30, AllowPrivateNetwork: true},
			CustomProviderConfig: &schemas.CustomProviderConfig{CustomProviderKey: "tkp_1", BaseProviderType: schemas.XAI, AllowedRequests: allowed},
		}, bifrost.NewNoOpLogger())
		require.NoError(t, err)
		return provider
	}
	ctx, key := xaiTestContext(t), xaiTestKey()

	chatOnly := newProvider(&schemas.AllowedRequests{ChatCompletion: true})
	_, bifrostErr := chatOnly.VideoGeneration(ctx, key, &schemas.BifrostVideoGenerationRequest{Provider: "tkp_1", Model: "grok-imagine-video", Input: &schemas.VideoGenerationInput{Prompt: "a dog"}})
	require.NotNil(t, bifrostErr)
	_, bifrostErr = chatOnly.VideoRetrieve(ctx, key, &schemas.BifrostVideoRetrieveRequest{Provider: "tkp_1", ID: "req-1:tkp_1"})
	require.NotNil(t, bifrostErr)
	_, bifrostErr = chatOnly.VideoDownload(ctx, key, &schemas.BifrostVideoDownloadRequest{Provider: "tkp_1", ID: "req-1:tkp_1"})
	require.NotNil(t, bifrostErr)
	_, bifrostErr = chatOnly.Speech(ctx, key, xaiSpeechRequest(&schemas.SpeechParameters{VoiceConfig: &schemas.SpeechVoiceInput{Voice: schemas.Ptr("eve")}}))
	require.NotNil(t, bifrostErr)
	_, bifrostErr = chatOnly.Transcription(ctx, key, &schemas.BifrostTranscriptionRequest{Provider: "tkp_1", Model: "grok-voice-transcribe-2.0", Input: &schemas.TranscriptionInput{File: []byte("a")}})
	require.NotNil(t, bifrostErr)
	require.Zero(t, upstream.Load(), "an operation the custom provider does not allow must not call xAI")

	// The task id carries the custom provider's name, and a query is answered under it.
	queryOnly := newProvider(&schemas.AllowedRequests{VideoRetrieve: true})
	task, bifrostErr := queryOnly.VideoRetrieve(ctx, key, &schemas.BifrostVideoRetrieveRequest{Provider: "tkp_1", ID: "req-1:tkp_1"})
	require.Nil(t, bifrostErr)
	require.Equal(t, "req-1:tkp_1", task.ID)
	require.Equal(t, int32(1), upstream.Load())

	// Download allowed, query not: the download still reads the task it needs.
	downloadOnly := newProvider(&schemas.AllowedRequests{VideoDownload: true})
	_, bifrostErr = downloadOnly.VideoDownload(ctx, key, &schemas.BifrostVideoDownloadRequest{Provider: "tkp_1", ID: "req-1:tkp_1"})
	require.NotNil(t, bifrostErr, "the task is pending, so there is nothing to download yet")
	require.Contains(t, bifrostErr.Error.Message, "video not ready")
	require.Equal(t, int32(2), upstream.Load())
}

// The download fetches the public link xAI gave, without the xAI key, and only for a
// completed task.
func TestXAIVideoDownload(t *testing.T) {
	t.Parallel()
	// The media fetcher refuses the loopback mock, so the file is served for a public address
	// by a forward proxy.
	const videoURL = "http://203.0.113.10/xai-vidgen-bucket/xai-video-req-1.mp4"
	videoProxy := proxytest.ForwardProxy(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host != "203.0.113.10" || !strings.HasSuffix(r.URL.Path, "/xai-video-req-1.mp4") {
			http.NotFound(w, r)
			return
		}
		if auth := r.Header.Get("Authorization"); auth != "" {
			t.Errorf("the download sent credentials to the file host: %q", auth)
		}
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write([]byte("mp4data"))
	})
	provider, captured := newMockXAI(t, map[string]http.HandlerFunc{
		"GET /v1/videos/req-1":       xaiJSON(http.StatusOK, `{"status":"done","video":{"url":"`+videoURL+`","duration":6,"respect_moderation":true}}`),
		"GET /v1/videos/req-pending": xaiJSON(http.StatusOK, `{"status":"pending"}`),
	})
	ctx := xaiTestContext(t)
	ctx.SetValue(schemas.BifrostContextKeyProviderProxyConfig, &schemas.ProxyConfig{Type: schemas.HTTPProxy, URL: schemas.NewSecretVar(videoProxy.URL)})

	downloaded, bifrostErr := provider.VideoDownload(ctx, xaiTestKey(), &schemas.BifrostVideoDownloadRequest{Provider: schemas.XAI, ID: "req-1:xai"})
	require.Nil(t, bifrostErr)
	require.Equal(t, []byte("mp4data"), downloaded.Content)
	require.Equal(t, "video/mp4", downloaded.ContentType)

	// The video is the only artifact xAI produces: naming it is fine, any other variant is
	// refused before the task is even queried instead of answered with the video.
	downloaded, bifrostErr = provider.VideoDownload(ctx, xaiTestKey(), &schemas.BifrostVideoDownloadRequest{Provider: schemas.XAI, ID: "req-1:xai", Variant: schemas.Ptr(schemas.VideoDownloadVariantVideo)})
	require.Nil(t, bifrostErr)
	require.Equal(t, []byte("mp4data"), downloaded.Content)
	before := len(captured())
	for _, variant := range []schemas.VideoDownloadVariant{schemas.VideoDownloadVariantThumbnail, schemas.VideoDownloadVariantSpriteSheet} {
		_, bifrostErr = provider.VideoDownload(ctx, xaiTestKey(), &schemas.BifrostVideoDownloadRequest{Provider: schemas.XAI, ID: "req-1:xai", Variant: schemas.Ptr(variant)})
		requireUnsupportedParameter(t, bifrostErr, "variant")
	}
	require.Len(t, captured(), before, "a refused variant must not call xAI")

	// A task with nothing to download is the caller's request, not a Bifrost fault.
	_, bifrostErr = provider.VideoDownload(ctx, xaiTestKey(), &schemas.BifrostVideoDownloadRequest{Provider: schemas.XAI, ID: "req-pending:xai"})
	require.NotNil(t, bifrostErr)
	require.Equal(t, schemas.Ptr(http.StatusBadRequest), bifrostErr.StatusCode)
	require.Contains(t, bifrostErr.Error.Message, "video not ready, current status: in_progress")
}
