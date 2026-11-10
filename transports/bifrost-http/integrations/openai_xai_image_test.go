package integrations

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/providers/xai"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

// TXE-6606 (contract C4): /openai/v1/images/generations and /openai/v1/images/edits with an xai/
// model reach the same provider methods as /v1/images/*. These tests drive each route's own
// parser and converter into the xAI provider and pin what reaches xAI's wire.

var openAIXAIPNG = append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 24)...)

type openAIXAIEgress struct {
	mu     sync.Mutex
	paths  []string
	ctypes []string
	bodies []map[string]any
}

func (e *openAIXAIEgress) provider(t *testing.T) *xai.XAIProvider {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		e.mu.Lock()
		e.paths = append(e.paths, r.URL.Path)
		e.ctypes = append(e.ctypes, r.Header.Get("Content-Type"))
		e.bodies = append(e.bodies, body)
		e.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":1762761600,"data":[{"url":"https://imgen.x.ai/a.jpg"}],"usage":{"cost_in_usd_ticks":400000000}}`))
	}))
	t.Cleanup(server.Close)
	provider, err := xai.NewXAIProvider(&schemas.ProviderConfig{
		NetworkConfig: schemas.NetworkConfig{BaseURL: server.URL, DefaultRequestTimeoutInSeconds: 30},
	}, bifrost.NewNoOpLogger())
	require.NoError(t, err)
	return provider
}

func (e *openAIXAIEgress) only(t *testing.T) (string, string, map[string]any) {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	require.Len(t, e.paths, 1, "exactly one upstream call")
	return e.paths[0], e.ctypes[0], e.bodies[0]
}

func openAIImageRoute(t *testing.T, path string) RouteConfig {
	t.Helper()
	for _, route := range CreateOpenAIRouteConfigs("/openai", nil) {
		if route.Path == path {
			return route
		}
	}
	t.Fatalf("route %s not registered", path)
	return RouteConfig{}
}

var openAIXAIKey = schemas.Key{Value: *schemas.NewSecretVar("xai-test")}

func TestOpenAIRouteXAIImageGenerationIngressToEgress(t *testing.T) {
	route := openAIImageRoute(t, "/openai/v1/images/generations")
	bifrostCtx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	req := route.GetRequestTypeInstance(bifrostCtx)
	require.NoError(t, parseJSONRequestBody([]byte(
		`{"model":"xai/grok-imagine-image-2.0","prompt":"a lighthouse","n":2,"quality":"medium","resolution":"2K","aspect_ratio":"16:9","response_format":"url"}`), req))
	converted, err := route.RequestConverter(bifrostCtx, req)
	require.NoError(t, err)
	require.NotNil(t, converted.ImageGenerationRequest)
	require.Equal(t, schemas.XAI, converted.ImageGenerationRequest.Provider)

	egress := &openAIXAIEgress{}
	response, bifrostErr := egress.provider(t).ImageGeneration(bifrostCtx, openAIXAIKey, converted.ImageGenerationRequest)
	require.Nil(t, bifrostErr)
	require.Len(t, response.Data, 1)

	path, ctype, body := egress.only(t)
	require.Equal(t, "/v1/images/generations", path)
	require.Equal(t, "application/json", ctype)
	require.Equal(t, "grok-imagine-image-2.0", body["model"])
	require.Equal(t, "medium", body["quality"])
	require.Equal(t, "2k", body["resolution"])
	require.Equal(t, "16:9", body["aspect_ratio"])
	require.Equal(t, "url", body["response_format"])
	require.EqualValues(t, 2, body["n"])
}

func TestOpenAIRouteXAIImageEditIngressToEgress(t *testing.T) {
	route := openAIImageRoute(t, "/openai/v1/images/edits")

	var form bytes.Buffer
	writer := multipart.NewWriter(&form)
	for k, v := range map[string]string{
		"model":        "xai/grok-imagine-image-2.0",
		"prompt":       "make it night",
		"quality":      "low",
		"resolution":   "1K",
		"aspect_ratio": "4:3",
		"n":            "1",
	} {
		require.NoError(t, writer.WriteField(k, v))
	}
	part, err := writer.CreateFormFile("image", "day.png")
	require.NoError(t, err)
	_, err = part.Write(openAIXAIPNG)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	httpCtx := &fasthttp.RequestCtx{}
	httpCtx.Request.Header.SetMethod(http.MethodPost)
	httpCtx.Request.SetRequestURI("/openai/v1/images/edits")
	httpCtx.Request.Header.SetContentType(writer.FormDataContentType())
	httpCtx.Request.SetBody(form.Bytes())

	bifrostCtx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	req := route.GetRequestTypeInstance(bifrostCtx)
	require.NotNil(t, route.RequestParser, "the edit route parses multipart")
	require.NoError(t, route.RequestParser(httpCtx, req))
	converted, err := route.RequestConverter(bifrostCtx, req)
	require.NoError(t, err)
	require.NotNil(t, converted.ImageEditRequest)
	require.Equal(t, schemas.XAI, converted.ImageEditRequest.Provider)

	egress := &openAIXAIEgress{}
	_, bifrostErr := egress.provider(t).ImageEdit(bifrostCtx, openAIXAIKey, converted.ImageEditRequest)
	require.Nil(t, bifrostErr)

	path, ctype, body := egress.only(t)
	require.Equal(t, "/v1/images/edits", path)
	require.Equal(t, "application/json", ctype, "xAI's edit takes JSON, never multipart")
	require.Equal(t, map[string]any{
		"url":  "data:image/png;base64," + base64.StdEncoding.EncodeToString(openAIXAIPNG),
		"type": "image_url",
	}, body["image"])
	require.Equal(t, "low", body["quality"])
	require.Equal(t, "1k", body["resolution"], "the route must not drop the price-affecting tier")
	require.Equal(t, "4:3", body["aspect_ratio"])
	require.EqualValues(t, 1, body["n"])
}
