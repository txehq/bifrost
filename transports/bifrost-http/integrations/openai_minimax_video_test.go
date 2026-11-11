package integrations

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/fasthttp/router"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

// miniMaxVideoUpstream records what reaches MiniMax and answers a v1 or v2 create.
type miniMaxVideoUpstream struct {
	mu    sync.Mutex
	calls []miniMaxVideoCall
}

type miniMaxVideoCall struct {
	method, path string
	body         map[string]any
}

func (u *miniMaxVideoUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	u.mu.Lock()
	u.calls = append(u.calls, miniMaxVideoCall{method: r.Method, path: r.URL.Path, body: body})
	u.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/v2/video_generation":
		_, _ = io.WriteString(w, `{"task_id":"424010985738629"}`)
	case "/v1/video_generation":
		_, _ = io.WriteString(w, `{"task_id":"106916112212032","base_resp":{"status_code":0,"status_msg":"success"}}`)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (u *miniMaxVideoUpstream) take() []miniMaxVideoCall {
	u.mu.Lock()
	defer u.mu.Unlock()
	calls := u.calls
	u.calls = nil
	return calls
}

type miniMaxVideoAccount struct{ url string }

func (a miniMaxVideoAccount) GetConfiguredProviders() ([]schemas.ModelProvider, error) {
	return []schemas.ModelProvider{schemas.MiniMax}, nil
}

func (a miniMaxVideoAccount) GetKeysForProvider(_ context.Context, _ schemas.ModelProvider) ([]schemas.Key, error) {
	return []schemas.Key{{ID: "mm-key", Name: "mm-key", Value: *schemas.NewSecretVar("placeholder"), Models: schemas.WhiteList{"*"}, Weight: 1.0}}, nil
}

func (a miniMaxVideoAccount) GetConfigForProvider(_ schemas.ModelProvider) (*schemas.ProviderConfig, error) {
	nc := schemas.DefaultNetworkConfig
	nc.BaseURL = a.url
	nc.MaxRetries = 0
	nc.DefaultRequestTimeoutInSeconds = 5
	nc.AllowPrivateNetwork = true
	return &schemas.ProviderConfig{NetworkConfig: nc, ConcurrencyAndBufferSize: schemas.DefaultConcurrencyAndBufferSize}, nil
}

// TestOpenAIVideoRouteReachesMiniMax drives the OpenAI SDK video route, /openai/v1/videos, into
// MiniMax's egress (TXE-6428). The route has its own parser (parseOpenAIVideoGenerationMultipartRequest)
// and converter (OpenAIVideoGenerationRequest.ToBifrostVideoGenerationRequest), so this proves that
// the unified fields and callback_url a JSON request carries, and the vendor fields it nests under
// extra_params (x-bf-passthrough-extra-params), reach MiniMax's v1 and v2 bodies in MiniMax's own
// fields, and that a multipart input_reference becomes the first frame.
func TestOpenAIVideoRouteReachesMiniMax(t *testing.T) {
	upstream := &miniMaxVideoUpstream{}
	server := httptest.NewServer(upstream)
	defer server.Close()

	client, err := bifrost.Init(context.Background(), schemas.BifrostConfig{
		Account: miniMaxVideoAccount{url: server.URL},
		Logger:  bifrost.NewDefaultLogger(schemas.LogLevelError),
	})
	require.NoError(t, err)
	defer client.Shutdown()

	r := router.New()
	identity := func(next fasthttp.RequestHandler) fasthttp.RequestHandler { return next }
	NewOpenAIRouter(client, &mockHandlerStore{}, nil, &testLogger{}).RegisterRoutes(r, identity)

	post := func(t *testing.T, contentType string, body []byte) (int, map[string]any) {
		t.Helper()
		var ctx fasthttp.RequestCtx
		ctx.Request.Header.SetMethod(fasthttp.MethodPost)
		ctx.Request.SetRequestURI("/openai/v1/videos")
		ctx.Request.Header.Set("x-bf-passthrough-extra-params", "true")
		ctx.Request.Header.SetContentType(contentType)
		ctx.Request.SetBody(body)
		r.Handler(&ctx)
		var resp map[string]any
		require.NoError(t, json.Unmarshal(ctx.Response.Body(), &resp), "response: %s", ctx.Response.Body())
		return ctx.Response.StatusCode(), resp
	}
	onlyCall := func(t *testing.T, path string) map[string]any {
		t.Helper()
		calls := upstream.take()
		require.Len(t, calls, 1, "upstream calls: %+v", calls)
		require.Equal(t, http.MethodPost, calls[0].method)
		require.Equal(t, path, calls[0].path)
		return calls[0].body
	}

	t.Run("v2 H3 JSON", func(t *testing.T) {
		status, resp := post(t, "application/json", []byte(`{"model":"minimax/MiniMax-H3","prompt":"a dog","seconds":"6","resolution":"768P","aspect_ratio":"9:16","callback_url":"https://cb.example/hook","extra_params":{"extra":{"prompt_expansion_mode":"quality"}}}`))
		require.Equal(t, http.StatusOK, status, "response: %v", resp)
		assert.Equal(t, "queued", resp["status"])
		body := onlyCall(t, "/v2/video_generation")
		assert.Equal(t, "MiniMax-H3", body["model"])
		assert.EqualValues(t, 6, body["duration"])
		assert.Equal(t, "768P", body["resolution"])
		assert.Equal(t, "9:16", body["ratio"])
		assert.Equal(t, "https://cb.example/hook", body["callback_url"])
		assert.Equal(t, map[string]any{"prompt_expansion_mode": "quality"}, body["extra"])
		for _, unified := range []string{"seconds", "aspect_ratio", "size"} {
			assert.NotContains(t, body, unified, "the unified %s is sent only in MiniMax's own field", unified)
		}
	})

	t.Run("v1 Hailuo JSON", func(t *testing.T) {
		status, resp := post(t, "application/json", []byte(`{"model":"minimax/MiniMax-Hailuo-2.3","prompt":"a dog","seconds":"10","resolution":"768P","last_frame":"https://img.example/last.png","callback_url":"https://cb.example/hook","extra_params":{"prompt_optimizer":false}}`))
		require.Equal(t, http.StatusOK, status, "response: %v", resp)
		assert.Contains(t, resp["id"], "v1-106916112212032")
		body := onlyCall(t, "/v1/video_generation")
		assert.Equal(t, "MiniMax-Hailuo-2.3", body["model"])
		assert.Equal(t, "a dog", body["prompt"])
		assert.EqualValues(t, 10, body["duration"])
		assert.Equal(t, "768P", body["resolution"])
		assert.Equal(t, "https://img.example/last.png", body["last_frame_image"])
		assert.Equal(t, "https://cb.example/hook", body["callback_url"])
		assert.Equal(t, false, body["prompt_optimizer"])
		assert.NotContains(t, body, "last_frame")
		assert.NotContains(t, body, "seconds")
	})

	t.Run("v1 Hailuo multipart with input_reference", func(t *testing.T) {
		var form bytes.Buffer
		writer := multipart.NewWriter(&form)
		require.NoError(t, writer.WriteField("model", "minimax/MiniMax-Hailuo-02"))
		require.NoError(t, writer.WriteField("prompt", "a dog"))
		require.NoError(t, writer.WriteField("seconds", "6"))
		part, err := writer.CreateFormFile("input_reference", "first.png")
		require.NoError(t, err)
		_, err = part.Write([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"))
		require.NoError(t, err)
		require.NoError(t, writer.Close())

		status, resp := post(t, writer.FormDataContentType(), form.Bytes())
		require.Equal(t, http.StatusOK, status, "response: %v", resp)
		body := onlyCall(t, "/v1/video_generation")
		assert.Equal(t, "MiniMax-Hailuo-02", body["model"])
		assert.EqualValues(t, 6, body["duration"])
		first, _ := body["first_frame_image"].(string)
		assert.Contains(t, first, "data:image/png;base64,", "the uploaded input_reference is the first frame")
	})

	t.Run("a field MiniMax cannot take is refused before any call", func(t *testing.T) {
		status, resp := post(t, "application/json", []byte(`{"model":"minimax/MiniMax-H3","prompt":"a dog","seed":7}`))
		assert.Equal(t, http.StatusBadRequest, status, "response: %v", resp)
		assert.Empty(t, upstream.take())
	})
}
