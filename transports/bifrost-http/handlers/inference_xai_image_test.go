package handlers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/providers/xai"
	schemas "github.com/capsohq/bifrost/core/schemas"
	"github.com/stretchr/testify/require"
)

// TXE-6606 (contract C4): the /v1/images/generations and /v1/images/edits ingress parsers feed the
// xAI provider, and these tests pin what reaches xAI's wire from each body shape, JSON and
// multipart. The provider-level tests in core/providers/xai pin the provider alone.

const xaiEgressImageResponse = `{"created":1762761600,"data":[{"url":"https://imgen.x.ai/a.jpg"}],"usage":{"cost_in_usd_ticks":400000000}}`

// xaiEgressPNG is a PNG signature plus padding: enough for the media-type sniffing that turns
// uploaded bytes into a data URI.
var xaiEgressPNG = append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 24)...)

type xaiEgress struct {
	mu     sync.Mutex
	paths  []string
	ctypes []string
	bodies []map[string]any
}

func (e *xaiEgress) provider(t *testing.T) *xai.XAIProvider {
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
		_, _ = w.Write([]byte(xaiEgressImageResponse))
	}))
	t.Cleanup(server.Close)
	provider, err := xai.NewXAIProvider(&schemas.ProviderConfig{
		NetworkConfig: schemas.NetworkConfig{BaseURL: server.URL, DefaultRequestTimeoutInSeconds: 30},
	}, bifrost.NewNoOpLogger())
	require.NoError(t, err)
	return provider
}

func (e *xaiEgress) only(t *testing.T) (string, string, map[string]any) {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	require.Len(t, e.paths, 1, "exactly one upstream call")
	return e.paths[0], e.ctypes[0], e.bodies[0]
}

func (e *xaiEgress) calls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.paths)
}

func xaiEgressCtx() *schemas.BifrostContext {
	return schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
}

var xaiEgressKey = schemas.Key{Value: *schemas.NewSecretVar("xai-test")}

func TestXAIImageGenerationIngressToEgress(t *testing.T) {
	t.Run("JSON body: quality, resolution, aspect_ratio and n reach xAI", func(t *testing.T) {
		egress := &xaiEgress{}
		provider := egress.provider(t)
		_, req, err := prepareImageGenerationRequest(jsonRequestCtx("/v1/images/generations",
			`{"model":"xai/grok-imagine-image-2.0","prompt":"a lighthouse","n":2,"quality":"medium","resolution":"2K","aspect_ratio":"16:9"}`), nil)
		require.NoError(t, err)
		require.Equal(t, schemas.XAI, req.Provider)

		response, bifrostErr := provider.ImageGeneration(xaiEgressCtx(), xaiEgressKey, req)
		require.Nil(t, bifrostErr)
		require.Len(t, response.Data, 1)

		path, ctype, body := egress.only(t)
		require.Equal(t, "/v1/images/generations", path)
		require.Equal(t, "application/json", ctype)
		require.Equal(t, "grok-imagine-image-2.0", body["model"])
		require.Equal(t, "medium", body["quality"])
		require.Equal(t, "2k", body["resolution"], "the unified tier is sent in xAI's spelling, once")
		require.Equal(t, "16:9", body["aspect_ratio"])
		require.EqualValues(t, 2, body["n"])
	})

	t.Run("JSON body: deferred is refused before any upstream call", func(t *testing.T) {
		egress := &xaiEgress{}
		provider := egress.provider(t)
		_, req, err := prepareImageGenerationRequest(jsonRequestCtx("/v1/images/generations",
			`{"model":"xai/grok-imagine-image-2.0","prompt":"a lighthouse","deferred":true}`), nil)
		require.NoError(t, err)

		_, bifrostErr := provider.ImageGeneration(xaiEgressCtx(), xaiEgressKey, req)
		require.NotNil(t, bifrostErr)
		require.Equal(t, schemas.ErrorTypeCallerUnsupportedParameter, bifrostErr.ExtraFields.ErrorType)
		require.Equal(t, "deferred", bifrostErr.Error.Param)
		require.Zero(t, egress.calls())
	})
}

func TestXAIImageEditIngressToEgress(t *testing.T) {
	t.Run("JSON body: a URL source, quality and resolution reach xAI's JSON edit", func(t *testing.T) {
		egress := &xaiEgress{}
		provider := egress.provider(t)
		_, req, err := prepareImageEditRequest(jsonImageEditCtx(
			`{"model":"xai/grok-imagine-image-2.0","prompt":"make it blue","images":[{"url":"https://example.com/day.png"}],"quality":"low","resolution":"1K"}`), nil)
		require.NoError(t, err)
		require.Equal(t, schemas.XAI, req.Provider)

		_, bifrostErr := provider.ImageEdit(xaiEgressCtx(), xaiEgressKey, req)
		require.Nil(t, bifrostErr)

		path, ctype, body := egress.only(t)
		require.Equal(t, "/v1/images/edits", path)
		require.Equal(t, "application/json", ctype)
		require.Equal(t, map[string]any{"url": "https://example.com/day.png", "type": "image_url"}, body["image"])
		require.Equal(t, "low", body["quality"])
		require.Equal(t, "1k", body["resolution"])
	})

	t.Run("multipart body: an upload becomes a data URI next to an image_url source", func(t *testing.T) {
		egress := &xaiEgress{}
		provider := egress.provider(t)
		_, req, err := prepareImageEditRequest(multipartImageEditCtx(t,
			map[string]string{
				"model":      "xai/grok-imagine-image-2.0",
				"prompt":     "put the two side by side",
				"image_url":  "https://example.com/night.png",
				"quality":    "medium",
				"resolution": "2K",
				"n":          "2",
			},
			map[string][]byte{"image": xaiEgressPNG}), nil)
		require.NoError(t, err)

		_, bifrostErr := provider.ImageEdit(xaiEgressCtx(), xaiEgressKey, req)
		require.Nil(t, bifrostErr)

		path, ctype, body := egress.only(t)
		require.Equal(t, "/v1/images/edits", path)
		require.Equal(t, "application/json", ctype, "xAI's edit takes JSON, never multipart")
		require.Equal(t, []any{
			map[string]any{"url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(xaiEgressPNG), "type": "image_url"},
			map[string]any{"url": "https://example.com/night.png", "type": "image_url"},
		}, body["images"])
		require.Equal(t, "medium", body["quality"])
		require.Equal(t, "2k", body["resolution"])
		require.EqualValues(t, 2, body["n"])
	})

	t.Run("multipart body: size is refused before any upstream call", func(t *testing.T) {
		egress := &xaiEgress{}
		provider := egress.provider(t)
		_, req, err := prepareImageEditRequest(multipartImageEditCtx(t,
			map[string]string{"model": "xai/grok-imagine-image-2.0", "prompt": "make it blue", "size": "1024x1024"},
			map[string][]byte{"image": xaiEgressPNG}), nil)
		require.NoError(t, err)

		_, bifrostErr := provider.ImageEdit(xaiEgressCtx(), xaiEgressKey, req)
		require.NotNil(t, bifrostErr)
		require.Equal(t, schemas.ErrorTypeCallerUnsupportedParameter, bifrostErr.ExtraFields.ErrorType)
		require.Equal(t, "size", bifrostErr.Error.Param)
		require.Zero(t, egress.calls())
	})
}
