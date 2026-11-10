package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bytedance/sonic"
	bifrost "github.com/capsohq/bifrost/core"
	schemas "github.com/capsohq/bifrost/core/schemas"
	"github.com/capsohq/bifrost/framework/configstore"
	"github.com/capsohq/bifrost/transports/v2/bifrost-http/lib"
	"github.com/fasthttp/router"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

func jsonVideoEditCtx(body string) *fasthttp.RequestCtx {
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod("POST")
	ctx.Request.SetRequestURI("/v1/videos/edits")
	ctx.Request.Header.SetContentType("application/json")
	ctx.Request.SetBody([]byte(body))
	return ctx
}

func multipartVideoEditCtx(t *testing.T, fields map[string]string, file []byte) *fasthttp.RequestCtx {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for k, v := range fields {
		if err := writer.WriteField(k, v); err != nil {
			t.Fatalf("write field %s: %v", k, err)
		}
	}
	if file != nil {
		part, err := writer.CreateFormFile("video", "source.mp4")
		if err != nil {
			t.Fatalf("create file part: %v", err)
		}
		if _, err := part.Write(file); err != nil {
			t.Fatalf("write file part: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod("POST")
	ctx.Request.SetRequestURI("/v1/videos/edits")
	ctx.Request.Header.SetContentType(writer.FormDataContentType())
	ctx.Request.SetBody(body.Bytes())
	return ctx
}

// The documented JSON shape: prompt plus a source reference, with model naming the provider.
func TestPrepareVideoEditRequest_JSON(t *testing.T) {
	req, err := prepareVideoEditRequest(jsonVideoEditCtx(
		`{"model":"runware/runway:aleph@2.0","prompt":"teal palette","video":{"url":"https://assets.runware.ai/clip.mp4"},"type":"upscale","upscale_factor":2}`), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.Provider != schemas.Runware || req.Model != "runway:aleph@2.0" {
		t.Fatalf("provider/model = %q/%q", req.Provider, req.Model)
	}
	if req.Input.Prompt != "teal palette" || req.Input.Video.URL != "https://assets.runware.ai/clip.mp4" {
		t.Fatalf("input not carried: %+v", req.Input)
	}
	if req.Params == nil || req.Params.Type == nil || *req.Params.Type != "upscale" {
		t.Fatalf("type not carried: %+v", req.Params)
	}
	if req.Params.UpscaleFactor == nil || *req.Params.UpscaleFactor != 2 {
		t.Fatalf("upscale_factor = %v, want 2", req.Params.UpscaleFactor)
	}
}

// Model is optional when the source video ID already names its provider — the same id:provider
// convention the retrieve, download, delete and remix routes use.
func TestPrepareVideoEditRequest_ProviderFromVideoID(t *testing.T) {
	req, err := prepareVideoEditRequest(jsonVideoEditCtx(
		`{"prompt":"teal palette","video":{"id":"video_abc123:openai"}}`), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.Provider != schemas.OpenAI {
		t.Fatalf("provider = %q, want openai", req.Provider)
	}
	if req.Model != "" {
		t.Fatalf("model = %q, want empty so the provider infers it from the source", req.Model)
	}
	if req.Input.Video.ID != "video_abc123:openai" {
		t.Fatalf("video id = %q, want the suffixed id preserved for the provider to strip", req.Input.Video.ID)
	}
}

// With no model and a bare video ID, the provider falls back to the query param / header, then errors.
func TestPrepareVideoEditRequest_ProviderFallbacks(t *testing.T) {
	ctx := jsonVideoEditCtx(`{"prompt":"teal palette","video":{"id":"video_abc123"}}`)
	ctx.Request.SetRequestURI("/v1/videos/edits?provider=openai")
	req, err := prepareVideoEditRequest(ctx, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.Provider != schemas.OpenAI {
		t.Fatalf("provider = %q, want openai from the query param", req.Provider)
	}

	ctx = jsonVideoEditCtx(`{"prompt":"teal palette","video":{"id":"video_abc123"}}`)
	ctx.Request.Header.Set("x-model-provider", "openai")
	if req, err = prepareVideoEditRequest(ctx, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.Provider != schemas.OpenAI {
		t.Fatalf("provider = %q, want openai from the header", req.Provider)
	}

	if _, err = prepareVideoEditRequest(jsonVideoEditCtx(
		`{"prompt":"teal palette","video":{"id":"video_abc123"}}`), nil); err == nil {
		t.Fatal("expected an error when no model, id suffix, query param or header is present")
	}
}

// A model with no provider prefix must not short-circuit provider resolution: Runware AIR ids like
// "runway:aleph@2.0" carry no "/" so ParseModelString yields an empty provider, and the query
// param, header and video-id suffix all still have to be consulted.
func TestPrepareVideoEditRequest_BareModelStillResolvesProvider(t *testing.T) {
	ctx := jsonVideoEditCtx(`{"model":"runway:aleph@2.0","prompt":"teal","video":{"url":"https://x/clip.mp4"}}`)
	ctx.Request.SetRequestURI("/v1/videos/edits?provider=runware")
	req, err := prepareVideoEditRequest(ctx, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.Provider != schemas.Runware {
		t.Fatalf("provider = %q, want runware from the query param", req.Provider)
	}
	if req.Model != "runway:aleph@2.0" {
		t.Fatalf("model = %q, want the bare AIR id preserved", req.Model)
	}

	ctx = jsonVideoEditCtx(`{"model":"runway:aleph@2.0","prompt":"teal","video":{"id":"task-uuid:runware"}}`)
	if req, err = prepareVideoEditRequest(ctx, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.Provider != schemas.Runware || req.Model != "runway:aleph@2.0" {
		t.Fatalf("provider/model = %q/%q, want runware/runway:aleph@2.0 from the id suffix", req.Provider, req.Model)
	}
}

// A bare model with no provider hint anywhere is not an error — the model catalogue and routing
// plugins resolve it downstream, exactly as on chat, images and /v1/videos.
func TestPrepareVideoEditRequest_BareModelDefersToRouting(t *testing.T) {
	req, err := prepareVideoEditRequest(jsonVideoEditCtx(
		`{"model":"runway:aleph@2.0","prompt":"teal","video":{"url":"https://x/clip.mp4"}}`), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.Provider != "" {
		t.Fatalf("provider = %q, want empty so routing can resolve it", req.Provider)
	}
	if req.Model != "runway:aleph@2.0" {
		t.Fatalf("model = %q", req.Model)
	}
}

// Only a suffix that names a real provider counts, so an id that merely contains a colon is not
// mistaken for one.
func TestVideoIDProviderSuffix(t *testing.T) {
	cases := map[string]string{
		"video_abc123:openai":  "openai",
		"task-uuid:runware":    "runware",
		"video_abc123":         "",
		"weird:id:with:colons": "",
		"video_abc123:":        "",
		":openai":              "",
		"":                     "",
	}
	for id, want := range cases {
		if got := videoIDProviderSuffix(id); got != want {
			t.Fatalf("videoIDProviderSuffix(%q) = %q, want %q", id, got, want)
		}
	}
}

func TestPrepareVideoEditRequest_RequiresSource(t *testing.T) {
	if _, err := prepareVideoEditRequest(jsonVideoEditCtx(`{"model":"openai/sora-2-pro","prompt":"teal"}`), nil); err == nil {
		t.Fatal("expected an error when the source video is missing")
	}
}

// Unrecognised body fields flow through as extra params rather than being dropped.
func TestPrepareVideoEditRequest_ExtraParams(t *testing.T) {
	req, err := prepareVideoEditRequest(jsonVideoEditCtx(
		`{"model":"runware/bria:2@1","prompt":"cut it","video":{"url":"https://x/clip.mp4"},"providerSettings":{"bria":{"preserveAlpha":true}}}`), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.Params == nil || req.Params.ExtraParams["providerSettings"] == nil {
		t.Fatalf("providerSettings should reach extra params, got %+v", req.Params)
	}
}

// An uploaded source arrives as multipart, which is also what the official OpenAI SDKs always send.
func TestPrepareVideoEditRequest_MultipartUpload(t *testing.T) {
	ctx := multipartVideoEditCtx(t,
		map[string]string{"model": "openai/sora-2-pro", "prompt": "warm backlight"},
		[]byte("\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00mp42isom"))

	req, err := prepareVideoEditRequest(ctx, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.Provider != schemas.OpenAI || req.Model != "sora-2-pro" {
		t.Fatalf("provider/model = %q/%q", req.Provider, req.Model)
	}
	if len(req.Input.Video.Video) == 0 {
		t.Fatal("uploaded video bytes were not read")
	}
	if req.Input.Prompt != "warm backlight" {
		t.Fatalf("prompt = %q", req.Input.Prompt)
	}
}

// The OpenAI SDKs flatten a reference source into a bracketed multipart field.
func TestPrepareVideoEditRequest_MultipartBracketedVideoID(t *testing.T) {
	ctx := multipartVideoEditCtx(t,
		map[string]string{"prompt": "warm backlight", "video[id]": "video_abc123:openai"}, nil)

	req, err := prepareVideoEditRequest(ctx, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.Input.Video.ID != "video_abc123:openai" {
		t.Fatalf("video id = %q", req.Input.Video.ID)
	}
	if req.Provider != schemas.OpenAI {
		t.Fatalf("provider = %q, want openai resolved from the id suffix", req.Provider)
	}
}

// The video routes mix a static path with parameterised siblings under the same prefix, which the
// router rejects if the shapes conflict. Registration panics on conflict, so this asserts the set
// registers and that each path still dispatches to its own handler.
func TestVideoRouteShapesDoNotConflict(t *testing.T) {
	hit := ""
	mark := func(name string) fasthttp.RequestHandler {
		return func(ctx *fasthttp.RequestCtx) { hit = name }
	}

	r := router.New()
	r.POST("/v1/videos", mark("generation"))
	r.POST("/v1/videos/edits", mark("edit"))
	r.GET("/v1/videos", mark("list"))
	r.GET("/v1/videos/{video_id}", mark("retrieve"))
	r.GET("/v1/videos/{video_id}/content", mark("download"))
	r.DELETE("/v1/videos/{video_id}", mark("delete"))
	r.POST("/v1/videos/{video_id}/remix", mark("remix"))
	r.POST("/v1/videos/{video_id}/cancel", mark("cancel"))

	cases := []struct {
		method, path, want string
	}{
		{"POST", "/v1/videos/edits", "edit"},
		{"POST", "/v1/videos", "generation"},
		{"POST", "/v1/videos/video_abc123:openai/remix", "remix"},
		{"POST", "/v1/videos/video_abc123:openai/cancel", "cancel"},
		{"GET", "/v1/videos/video_abc123:openai", "retrieve"},
		{"GET", "/v1/videos/video_abc123:openai/content", "download"},
		{"DELETE", "/v1/videos/video_abc123:openai", "delete"},
		{"GET", "/v1/videos", "list"},
	}
	for _, tc := range cases {
		hit = ""
		ctx := &fasthttp.RequestCtx{}
		ctx.Request.Header.SetMethod(tc.method)
		ctx.Request.SetRequestURI(tc.path)
		r.Handler(ctx)
		if hit != tc.want {
			t.Fatalf("%s %s dispatched to %q (status %d), want %q", tc.method, tc.path, hit, ctx.Response.StatusCode(), tc.want)
		}
	}
}

// videoTaskAccount configures OpenAI against a local upstream, with two keys.
type videoTaskAccount struct{ baseURL string }

func (videoTaskAccount) GetConfiguredProviders() ([]schemas.ModelProvider, error) {
	return []schemas.ModelProvider{schemas.OpenAI}, nil
}

func (videoTaskAccount) GetKeysForProvider(context.Context, schemas.ModelProvider) ([]schemas.Key, error) {
	return []schemas.Key{
		{ID: "k1", Name: "K1", Value: *schemas.NewSecretVar("sk-test-1"), Models: schemas.WhiteList{"*"}, Weight: 1},
		{ID: "k2", Name: "K2", Value: *schemas.NewSecretVar("sk-test-2"), Models: schemas.WhiteList{"*"}, Weight: 1},
	}, nil
}

func (a videoTaskAccount) GetConfigForProvider(schemas.ModelProvider) (*schemas.ProviderConfig, error) {
	network := schemas.DefaultNetworkConfig
	network.BaseURL = a.baseURL
	return &schemas.ProviderConfig{NetworkConfig: network, ConcurrencyAndBufferSize: schemas.DefaultConcurrencyAndBufferSize}, nil
}

// videoTaskRoutes serves the video task routes from a real core client whose OpenAI provider
// points at a local upstream, and reports how many requests reached that upstream.
func videoTaskRoutes(t *testing.T) (fasthttp.RequestHandler, *atomic.Int64) {
	t.Helper()
	var upstream atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstream.Add(1)
		w.WriteHeader(http.StatusTeapot)
	}))
	t.Cleanup(server.Close)
	client, err := bifrost.Init(context.Background(), schemas.BifrostConfig{Account: videoTaskAccount{baseURL: server.URL}, Logger: bifrost.NewDefaultLogger(schemas.LogLevelError)})
	require.NoError(t, err)
	t.Cleanup(client.Shutdown)

	h := &CompletionHandler{client: client, config: &lib.Config{ClientConfig: &configstore.ClientConfig{}}}
	r := router.New()
	r.POST("/v1/videos", h.videoGeneration)
	r.POST("/v1/videos/{video_id}/cancel", h.videoCancel)
	r.GET("/v1/videos/{video_id}", h.videoRetrieve)
	return r.Handler, &upstream
}

// TXE-6521 (contract C7 BF12): the four task control fields parse on POST /v1/videos as typed
// fields and never as extra params, so none can reach a vendor unconverted.
func TestVideoGenerationRequestParsesTaskControlFields(t *testing.T) {
	body := []byte(`{"model":"byteplus/seedance","prompt":"a dog","callback_url":"https://hooks.tokun.sh/v/task_1",` +
		`"client_reference_id":"task_1","expires_after_seconds":7200,"output_storage_uri":"gs://bucket/task_1/","watermark":false}`)
	var req VideoGenerationRequest
	require.NoError(t, sonic.Unmarshal(body, &req))
	require.NotNil(t, req.VideoGenerationParameters)
	control := req.VideoGenerationParameters.TaskControlParameters
	require.NotNil(t, control.CallbackURL)
	assert.Equal(t, "https://hooks.tokun.sh/v/task_1", *control.CallbackURL)
	require.NotNil(t, control.ClientReferenceID)
	assert.Equal(t, "task_1", *control.ClientReferenceID)
	require.NotNil(t, control.ExpiresAfterSeconds)
	assert.Equal(t, 7200, *control.ExpiresAfterSeconds)
	require.NotNil(t, control.OutputStorageURI)
	assert.Equal(t, "gs://bucket/task_1/", *control.OutputStorageURI)

	extra, err := extractExtraParams(body, videoGenerationParamsKnownFields)
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"watermark": false}, extra, "only the vendor-specific switch is an extra param")
	for _, field := range schemas.TaskControlFields {
		assert.True(t, videoGenerationParamsKnownFields[field], "%s must be a known field", field)
	}
}

// TXE-6521 (contract C7 BF12, C4 convention three): POST /v1/videos with a task control field the
// provider does not take is 400 caller_unsupported_parameter naming the field, with no upstream
// call. OpenAI takes none of the four.
func TestVideoGenerationRefusesUnsupportedTaskControlField(t *testing.T) {
	handler, upstream := videoTaskRoutes(t)
	for field, value := range map[string]string{
		"callback_url":          `"https://hooks.tokun.sh/v/task_1"`,
		"client_reference_id":   `"task_1"`,
		"expires_after_seconds": `7200`,
		"output_storage_uri":    `"gs://bucket/task_1/"`,
	} {
		t.Run(field, func(t *testing.T) {
			ctx := &fasthttp.RequestCtx{}
			ctx.Request.Header.SetMethod("POST")
			ctx.Request.SetRequestURI("/v1/videos")
			ctx.Request.Header.SetContentType("application/json")
			ctx.Request.SetBody([]byte(`{"model":"openai/sora-2","prompt":"a dog","` + field + `":` + value + `}`))
			handler(ctx)

			body := ctx.Response.Body()
			require.Equal(t, fasthttp.StatusBadRequest, ctx.Response.StatusCode(), "body: %s", body)
			var got struct {
				Error       struct{ Param, Code string }
				ExtraFields struct {
					ErrorType string `json:"error_type"`
				} `json:"extra_fields"`
			}
			require.NoError(t, sonic.Unmarshal(body, &got), "body: %s", body)
			assert.Equal(t, "caller_unsupported_parameter", got.ExtraFields.ErrorType, "body: %s", body)
			assert.Equal(t, field, got.Error.Param, "body: %s", body)
		})
	}
	assert.Zero(t, upstream.Load(), "a refused control field must make no upstream call")
}

// TXE-6521 (contract C7 BF13): POST /v1/videos/{id}/cancel on a provider with no cancel API is
// 400 provider_cancel_unsupported with no upstream call, named key or not. The route validates the
// id like the other id-addressed video routes.
func TestVideoCancelRoute(t *testing.T) {
	handler, upstream := videoTaskRoutes(t)
	post := func(path, keyName string) *fasthttp.RequestCtx {
		ctx := &fasthttp.RequestCtx{}
		ctx.Request.Header.SetMethod("POST")
		ctx.Request.SetRequestURI(path)
		if keyName != "" {
			ctx.Request.Header.Set("x-bf-api-key", keyName)
		}
		handler(ctx)
		return ctx
	}

	for _, keyName := range []string{"", "K2"} {
		ctx := post("/v1/videos/video_abc123:openai/cancel", keyName)
		body := ctx.Response.Body()
		require.Equal(t, fasthttp.StatusBadRequest, ctx.Response.StatusCode(), "body: %s", body)
		var got struct {
			Error       struct{ Code string }
			ExtraFields struct {
				ErrorType   string `json:"error_type"`
				Provider    string `json:"provider"`
				RequestType string `json:"request_type"`
			} `json:"extra_fields"`
		}
		require.NoError(t, sonic.Unmarshal(body, &got), "body: %s", body)
		assert.Equal(t, "provider_cancel_unsupported", got.ExtraFields.ErrorType, "body: %s", body)
		assert.Equal(t, "provider_cancel_unsupported", got.Error.Code, "body: %s", body)
		assert.Equal(t, "openai", got.ExtraFields.Provider, "body: %s", body)
		assert.Equal(t, "video_cancel", got.ExtraFields.RequestType, "body: %s", body)
	}
	assert.Zero(t, upstream.Load(), "a provider with no cancel API must not be called")

	ctx := post("/v1/videos/video_abc123/cancel", "")
	assert.Equal(t, fasthttp.StatusBadRequest, ctx.Response.StatusCode())
	assert.Contains(t, string(ctx.Response.Body()), "video_id must be in id:provider format")
	assert.Zero(t, upstream.Load())
}

// imageTaskRoutes serves every route of the real RegisterRoutes from a core client whose OpenAI
// provider (which has no image tasks) points at a local upstream, and counts upstream requests.
func imageTaskRoutes(t *testing.T) (fasthttp.RequestHandler, *atomic.Int64) {
	t.Helper()
	var upstream atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstream.Add(1)
		w.WriteHeader(http.StatusTeapot)
	}))
	t.Cleanup(server.Close)
	client, err := bifrost.Init(context.Background(), schemas.BifrostConfig{Account: videoTaskAccount{baseURL: server.URL}, Logger: bifrost.NewDefaultLogger(schemas.LogLevelError)})
	require.NoError(t, err)
	t.Cleanup(client.Shutdown)

	h := &CompletionHandler{client: client, config: &lib.Config{ClientConfig: &configstore.ClientConfig{}}}
	r := router.New()
	h.RegisterRoutes(r) // panics if the task routes conflict with the sync image routes
	return r.Handler, &upstream
}

func serveImageTask(handler fasthttp.RequestHandler, method, path, body string) *fasthttp.RequestCtx {
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod(method)
	ctx.Request.SetRequestURI(path)
	if body != "" {
		ctx.Request.Header.SetContentType("application/json")
		ctx.Request.SetBodyString(body)
	}
	handler(ctx)
	return ctx
}

// TXE-6431 (contract C7 BF16): the four image task routes are registered beside the sync image
// routes and reach core. A provider without image tasks answers each without an upstream call:
// a create or query with unsupported_operation, a cancel with provider_cancel_unsupported.
func TestImageTaskRoutes(t *testing.T) {
	handler, upstream := imageTaskRoutes(t)
	cases := []struct {
		name, method, path, body string
		status                   int
		errorType                string
		code                     string
	}{
		{"generation task", "POST", "/v1/images/generations/tasks", `{"model":"openai/gpt-image-1","prompt":"a fox"}`, 400, "", "unsupported_operation"},
		{"edit task", "POST", "/v1/images/edits/tasks", `{"model":"openai/gpt-image-1","prompt":"a fox","images":[{"url":"https://example.com/a.png"}]}`, 400, "", "unsupported_operation"},
		{"query", "GET", "/v1/images/tasks/task_1:openai", "", 400, "", "unsupported_operation"},
		{"cancel", "POST", "/v1/images/tasks/task_1:openai/cancel", "", 400, string(schemas.ErrorTypeProviderCancelUnsupported), ""},
		{"a task id without a provider", "GET", "/v1/images/tasks/task_1", "", 400, "", ""},
		{"stream on a task route", "POST", "/v1/images/generations/tasks", `{"model":"openai/gpt-image-1","prompt":"a fox","stream":true}`, 400, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := serveImageTask(handler, tc.method, tc.path, tc.body)
			require.Equal(t, tc.status, ctx.Response.StatusCode(), "body: %s", ctx.Response.Body())
			var body struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
				ExtraFields struct {
					ErrorType string `json:"error_type"`
				} `json:"extra_fields"`
			}
			require.NoError(t, sonic.Unmarshal(ctx.Response.Body(), &body))
			if tc.code != "" {
				assert.Equal(t, tc.code, body.Error.Code)
			}
			if tc.errorType != "" {
				assert.Equal(t, tc.errorType, body.ExtraFields.ErrorType)
			}
		})
	}
	assert.Equal(t, int64(0), upstream.Load(), "no route may call the upstream")
}

// TXE-6431 (contract C7 BF12): a task create's control fields leave the extra params for their
// typed form, so none reaches a vendor unconverted; a wrongly typed one is the caller's error.
func TestTakeTaskControl(t *testing.T) {
	extra := map[string]any{
		"callback_url": "https://hooks.tokun.sh/v/t1", "client_reference_id": "t1", "expires_after_seconds": float64(7200),
		"output_storage_uri": "gs://b/t1/", "safety_tolerance": float64(2),
	}
	control, err := takeTaskControl(extra)
	require.NoError(t, err)
	require.NotNil(t, control)
	assert.Equal(t, "https://hooks.tokun.sh/v/t1", *control.CallbackURL)
	assert.Equal(t, "t1", *control.ClientReferenceID)
	assert.Equal(t, 7200, *control.ExpiresAfterSeconds)
	assert.Equal(t, "gs://b/t1/", *control.OutputStorageURI)
	assert.Equal(t, map[string]any{"safety_tolerance": float64(2)}, extra, "only the vendor field stays an extra param")

	multipart := map[string]any{"expires_after_seconds": "60"}
	control, err = takeTaskControl(multipart)
	require.NoError(t, err)
	assert.Equal(t, 60, *control.ExpiresAfterSeconds, "a multipart form carries numbers as strings")

	none, err := takeTaskControl(map[string]any{"safety_tolerance": float64(2)})
	require.NoError(t, err)
	assert.Nil(t, none)

	for _, bad := range []map[string]any{{"callback_url": float64(1)}, {"expires_after_seconds": 1.5}, {"expires_after_seconds": "soon"}} {
		_, err := takeTaskControl(bad)
		assert.Error(t, err, "%v", bad)
	}
}

// TXE-6431: the sync image route keeps a field named callback_url as an extra param, as before
// the task routes existed; only the task routes give it the task-control meaning.
func TestSyncImageRouteLeavesCallbackURLAnExtraParam(t *testing.T) {
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod("POST")
	ctx.Request.Header.SetContentType("application/json")
	ctx.Request.SetBodyString(`{"model":"openai/gpt-image-1","prompt":"a fox","callback_url":"https://example.com/cb"}`)
	_, bifrostReq, err := prepareImageGenerationRequest(ctx, &lib.Config{ClientConfig: &configstore.ClientConfig{}})
	require.NoError(t, err)
	assert.Nil(t, bifrostReq.TaskControl)
	assert.Equal(t, "https://example.com/cb", bifrostReq.Params.ExtraParams["callback_url"])
}

// videoUpstream is a vendor stand-in that records each call as "METHOD uri authorization".
type videoUpstream struct {
	mu    sync.Mutex
	calls []string
}

func (u *videoUpstream) record(r *http.Request) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.calls = append(u.calls, r.Method+" "+r.URL.RequestURI()+" "+r.Header.Get("Authorization"))
}

func (u *videoUpstream) seen() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.calls...)
}

// newVideoRouteHandler returns the inference handler over a real client whose OpenAI provider
// has two keys and points at upstream, so a route runs end to end and key selection has a choice.
func newVideoRouteHandler(t *testing.T, upstream string) *CompletionHandler {
	t.Helper()
	network := schemas.DefaultNetworkConfig
	network.BaseURL = upstream
	config := &lib.Config{
		ClientConfig: &configstore.ClientConfig{},
		Providers: map[schemas.ModelProvider]configstore.ProviderConfig{
			schemas.OpenAI: {
				NetworkConfig: &network,
				Keys: []schemas.Key{
					{ID: "id-a", Name: "key-a", Value: *schemas.NewSecretVar("sk-a"), Weight: 1, Models: []string{"*"}},
					{ID: "id-b", Name: "key-b", Value: *schemas.NewSecretVar("sk-b"), Weight: 1, Models: []string{"*"}},
				},
			},
		},
	}
	client, err := bifrost.Init(context.Background(), schemas.BifrostConfig{
		Account: lib.NewBaseAccount(config),
		Logger:  bifrost.NewDefaultLogger(schemas.LogLevelError),
	})
	require.NoError(t, err)
	t.Cleanup(client.Shutdown)
	config.SetBifrostClient(client)
	return NewInferenceHandler(nil, client, config)
}

// TestVideoContentStreamsWithTheNamedKey is contract C9 / BF20 on GET /v1/videos/{id}/content:
// the file is forwarded as it arrives, on one upstream call made with the key named by
// x-bf-api-key. The vendor sends half the file and then waits until the caller has read it, so a
// handler that read the whole body before answering would never return.
func TestVideoContentStreamsWithTheNamedKey(t *testing.T) {
	upstream := &videoUpstream{}
	firstHalfRead := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstream.record(r)
		if r.URL.Path != "/v1/videos/video_a/content" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"message":"Video not found","type":"invalid_request_error"}}`))
			return
		}
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write([]byte("FIRST-HALF|"))
		w.(http.Flusher).Flush()
		select {
		case <-firstHalfRead:
		case <-time.After(10 * time.Second):
			return
		}
		_, _ = w.Write([]byte("SECOND-HALF"))
	}))
	defer srv.Close()
	h := newVideoRouteHandler(t, srv.URL)

	var ctx fasthttp.RequestCtx
	ctx.Request.Header.SetMethod(fasthttp.MethodGet)
	ctx.Request.SetRequestURI("/v1/videos/video_a:openai/content")
	ctx.Request.Header.Set("x-bf-api-key", "key-b")
	ctx.SetUserValue("video_id", "video_a:openai")
	h.videoDownload(&ctx)

	// Response.Body() would read the stream to its end, so nothing here may call it.
	require.Equal(t, fasthttp.StatusOK, ctx.Response.StatusCode())
	require.True(t, ctx.Response.IsBodyStream(), "the file must be a body stream, not bytes held by the handler")
	require.Equal(t, "video/mp4", string(ctx.Response.Header.ContentType()))
	// The handler has returned and the vendor is still sending: the request context must still
	// be live, or the rest of the file could not be read.
	stream := ctx.Response.BodyStream()
	first := make([]byte, len("FIRST-HALF|"))
	_, err := io.ReadFull(stream, first)
	require.NoError(t, err)
	require.Equal(t, "FIRST-HALF|", string(first))
	close(firstHalfRead)
	rest, err := io.ReadAll(stream)
	require.NoError(t, err)
	require.Equal(t, "SECOND-HALF", string(rest))
	require.NoError(t, ctx.Response.CloseBodyStream())
	require.Equal(t, []string{"GET /v1/videos/video_a/content Bearer sk-b"}, upstream.seen())

	// The vendor's refusal is returned as an error, not as a stream.
	var missing fasthttp.RequestCtx
	missing.Request.Header.SetMethod(fasthttp.MethodGet)
	missing.Request.SetRequestURI("/v1/videos/video_gone:openai/content")
	missing.Request.Header.Set("x-bf-api-key", "key-a")
	missing.SetUserValue("video_id", "video_gone:openai")
	h.videoDownload(&missing)
	require.Equal(t, fasthttp.StatusNotFound, missing.Response.StatusCode(), "body: %s", missing.Response.Body())
	require.False(t, missing.Response.IsBodyStream())
	require.Contains(t, string(missing.Response.Body()), "Video not found")
	require.Equal(t, "GET /v1/videos/video_gone/content Bearer sk-a", upstream.seen()[1])
	require.Len(t, upstream.seen(), 2)
}

// TestVideoListForwardsVendorFiltersWithTheNamedKey is contract C7 / BF15 on GET /v1/videos: the
// query parameters Bifrost does not read itself reach the vendor unchanged, repeated values
// included; provider stays with Bifrost; the call is made once, with the named key; and the
// after cursor a caller takes from last_id reaches the vendor without the provider suffix.
func TestVideoListForwardsVendorFiltersWithTheNamedKey(t *testing.T) {
	upstream := &videoUpstream{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstream.record(r)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("after") == "video_b" {
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"video_c","object":"video","model":"sora-2","status":"failed","created_at":1762700200}],"first_id":"video_c","last_id":"video_c","has_more":false}`))
			return
		}
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"video_a","object":"video","model":"sora-2","status":"completed","created_at":1762700000},{"id":"video_b","object":"video","model":"sora-2","status":"in_progress","created_at":1762700100}],"first_id":"video_a","last_id":"video_b","has_more":true}`))
	}))
	defer srv.Close()
	h := newVideoRouteHandler(t, srv.URL)

	list := func(uri string) (ids []string, hasMore bool, lastID string) {
		var ctx fasthttp.RequestCtx
		ctx.Request.Header.SetMethod(fasthttp.MethodGet)
		ctx.Request.SetRequestURI(uri)
		ctx.Request.Header.Set("x-bf-api-key", "key-b")
		h.videoList(&ctx)
		require.Equal(t, fasthttp.StatusOK, ctx.Response.StatusCode(), "body: %s", ctx.Response.Body())
		var page struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
			HasMore bool   `json:"has_more"`
			LastID  string `json:"last_id"`
		}
		require.NoError(t, json.Unmarshal(ctx.Response.Body(), &page))
		for _, video := range page.Data {
			ids = append(ids, video.ID)
		}
		return ids, page.HasMore, page.LastID
	}

	const filters = "provider=openai&limit=2&status=completed&status=failed&created_after=1762700000"
	ids, hasMore, lastID := list("/v1/videos?" + filters)
	require.True(t, hasMore)
	require.Equal(t, "video_b:openai", lastID)
	next, hasMore, _ := list("/v1/videos?" + filters + "&after=" + lastID)
	require.False(t, hasMore)
	require.Equal(t, []string{"video_a:openai", "video_b:openai", "video_c:openai"}, append(ids, next...))

	require.Equal(t, []string{
		"GET /v1/videos?created_after=1762700000&limit=2&status=completed&status=failed Bearer sk-b",
		"GET /v1/videos?after=video_b&created_after=1762700000&limit=2&status=completed&status=failed Bearer sk-b",
	}, upstream.seen())

	// A parameter Bifrost reads itself is refused when repeated, before any upstream call.
	var repeated fasthttp.RequestCtx
	repeated.Request.Header.SetMethod(fasthttp.MethodGet)
	repeated.Request.SetRequestURI("/v1/videos?provider=openai&provider=azure")
	h.videoList(&repeated)
	require.Equal(t, fasthttp.StatusBadRequest, repeated.Response.StatusCode(), "body: %s", repeated.Response.Body())
	require.Contains(t, string(repeated.Response.Body()), "provider query parameter must be sent once")
	require.Len(t, upstream.seen(), 2)

	var args fasthttp.Args
	args.Parse("provider=azure&after=x&limit=1&order=asc")
	require.Nil(t, videoListFilters(&args), "the parameters Bifrost reads are not vendor filters")
	args.Parse("provider=dashscope&start_time=1&start_time=2&Status=SUCCEEDED")
	if got, want := videoListFilters(&args), map[string][]string{"start_time": {"1", "2"}, "Status": {"SUCCEEDED"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("filters = %v, want %v", got, want)
	}
}

// BenchmarkVideoListFilters is what collecting the vendor filters adds to GET /v1/videos: no
// allocation for a request that sends none.
func BenchmarkVideoListFilters(b *testing.B) {
	for name, query := range map[string]string{
		"none":  "provider=azure&limit=2&order=desc",
		"three": "provider=azure&limit=2&status=completed&status=failed&created_after=1762700000",
	} {
		b.Run(name, func(b *testing.B) {
			var args fasthttp.Args
			args.Parse(query)
			b.ReportAllocs()
			for b.Loop() {
				_ = videoListFilters(&args)
			}
		})
	}
}

// TXE-6430 (contract C7): GET /v1/videos/{id} names the task by the vendor's task id, or, with
// ?id_type=client_reference_id, by the caller's own id. A reference keeps every colon but the
// last, which separates the provider.
func TestPrepareVideoRetrieveRequest(t *testing.T) {
	for name, tc := range map[string]struct {
		videoID, idType string
		want            schemas.BifrostVideoRetrieveRequest
		wantErr         string
	}{
		"task id":                   {videoID: "893605946402811985:kling", want: schemas.BifrostVideoRetrieveRequest{Provider: "kling", ID: "893605946402811985"}},
		"task id, explicit":         {videoID: "893605946402811985:kling", idType: "task_id", want: schemas.BifrostVideoRetrieveRequest{Provider: "kling", ID: "893605946402811985"}},
		"task id, url-encoded":      {videoID: "a%2Fb:kling", want: schemas.BifrostVideoRetrieveRequest{Provider: "kling", ID: "a/b"}},
		"client reference":          {videoID: "tk_task_1:kling", idType: "client_reference_id", want: schemas.BifrostVideoRetrieveRequest{Provider: "kling", ID: "tk_task_1", IDType: schemas.VideoIDTypeClientReference}},
		"client reference, colons":  {videoID: "tk:task:1:tkp_kling", idType: "client_reference_id", want: schemas.BifrostVideoRetrieveRequest{Provider: "tkp_kling", ID: "tk:task:1", IDType: schemas.VideoIDTypeClientReference}},
		"client reference, encoded": {videoID: "tk%3Atask%201:kling", idType: "client_reference_id", want: schemas.BifrostVideoRetrieveRequest{Provider: "kling", ID: "tk:task 1", IDType: schemas.VideoIDTypeClientReference}},
		"no provider":               {videoID: "893605946402811985", wantErr: "video_id must be in id:provider format"},
		"reference, no provider":    {videoID: "tk_task_1", idType: "client_reference_id", wantErr: "client_reference_id:provider"},
		"reference, empty provider": {videoID: "tk_task_1:", idType: "client_reference_id", wantErr: "client_reference_id:provider"},
		"unknown id type":           {videoID: "x:kling", idType: "external_id", wantErr: `invalid id_type "external_id"`},
		"empty id":                  {videoID: "", wantErr: "video_id is required"},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := prepareVideoRetrieveRequest(tc.videoID, tc.idType)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, *got)
		})
	}
}

// TXE-6430 (contract C7): a provider whose vendor cannot be queried by the caller's own id
// answers a retrieve by client_reference_id 400 caller_unsupported_parameter naming id_type, with
// no upstream call. OpenAI has no such query.
func TestVideoRetrieveByClientReferenceRefusedWhenUnsupported(t *testing.T) {
	handler, upstream := videoTaskRoutes(t)
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod("GET")
	ctx.Request.SetRequestURI("/v1/videos/tk_task_1:openai?id_type=client_reference_id")
	handler(ctx)
	body := ctx.Response.Body()
	require.Equal(t, fasthttp.StatusBadRequest, ctx.Response.StatusCode(), "body: %s", body)
	var got struct {
		Error       struct{ Code, Param string }
		ExtraFields struct {
			ErrorType   string `json:"error_type"`
			Provider    string `json:"provider"`
			RequestType string `json:"request_type"`
		} `json:"extra_fields"`
	}
	require.NoError(t, sonic.Unmarshal(body, &got), "body: %s", body)
	assert.Equal(t, "caller_unsupported_parameter", got.ExtraFields.ErrorType, "body: %s", body)
	assert.Equal(t, "id_type", got.Error.Param, "body: %s", body)
	assert.Equal(t, "openai", got.ExtraFields.Provider, "body: %s", body)
	assert.Equal(t, "video_retrieve", got.ExtraFields.RequestType, "body: %s", body)

	ctx = &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod("GET")
	ctx.Request.SetRequestURI("/v1/videos/tk_task_1:openai?id_type=external_id")
	handler(ctx)
	assert.Equal(t, fasthttp.StatusBadRequest, ctx.Response.StatusCode())
	assert.Contains(t, string(ctx.Response.Body()), "invalid id_type")
	assert.Zero(t, upstream.Load(), "a refused retrieve must not reach the provider")
}

// klingUpstreamCall is one request the local Kling upstream received.
type klingUpstreamCall struct {
	method, path, query, auth string
	body                      []byte
}

// klingTaskAccount configures the built-in kling provider against a local upstream, with two keys.
type klingTaskAccount struct{ baseURL string }

func (klingTaskAccount) GetConfiguredProviders() ([]schemas.ModelProvider, error) {
	return []schemas.ModelProvider{schemas.Kling}, nil
}

func (klingTaskAccount) GetKeysForProvider(context.Context, schemas.ModelProvider) ([]schemas.Key, error) {
	return []schemas.Key{
		{ID: "k1", Name: "K1", Value: *schemas.NewSecretVar("kling-key-1"), Models: schemas.WhiteList{"*"}, Weight: 1},
		{ID: "k2", Name: "K2", Value: *schemas.NewSecretVar("kling-key-2"), Models: schemas.WhiteList{"*"}, Weight: 1},
	}, nil
}

func (a klingTaskAccount) GetConfigForProvider(schemas.ModelProvider) (*schemas.ProviderConfig, error) {
	network := schemas.DefaultNetworkConfig
	network.BaseURL = a.baseURL
	network.AllowPrivateNetwork = true
	return &schemas.ProviderConfig{NetworkConfig: network, ConcurrencyAndBufferSize: schemas.DefaultConcurrencyAndBufferSize}, nil
}

// TXE-6430 (contract C3, C4, C7): through the HTTP routes, a Kling create and a Kling query are
// each exactly one upstream call made with the key x-bf-api-key names; a query by
// client_reference_id returns the same task object as a query by the task id; an undeliverable
// field and a cancel are refused with no upstream call.
func TestKlingVideoRoutes(t *testing.T) {
	const task = `{"id":"893605946402811985","status":"succeeded","message":"","create_time":1781080778802,"update_time":1781080794151,` +
		`"external_id":"tk:task:1","outputs":[{"type":"video","id":"vid_1","url":"https://v.klingai.com/out.mp4","watermark_url":"","duration":"5"}],` +
		`"billing":[{"charge_type":"unit","amount":"3.5","package_type":"video"}]}`
	var mu sync.Mutex
	var calls []klingUpstreamCall
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls = append(calls, klingUpstreamCall{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, auth: r.Header.Get("Authorization"), body: body})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			_, _ = w.Write([]byte(`{"code":0,"message":"SUCCEED","request_id":"r1","data":{"id":"893605946402811985","status":"submitted","create_time":1781080778802,"update_time":1781080778802,"external_id":"tk:task:1"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"message":"SUCCEED","request_id":"r2","data":[` + task + `]}`))
	}))
	t.Cleanup(server.Close)
	upstreamCalls := func() []klingUpstreamCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]klingUpstreamCall(nil), calls...)
	}

	client, err := bifrost.Init(context.Background(), schemas.BifrostConfig{Account: klingTaskAccount{baseURL: server.URL}, Logger: bifrost.NewDefaultLogger(schemas.LogLevelError)})
	require.NoError(t, err)
	t.Cleanup(client.Shutdown)
	h := &CompletionHandler{client: client, config: &lib.Config{ClientConfig: &configstore.ClientConfig{}}}
	r := router.New()
	r.POST("/v1/videos", h.videoGeneration)
	r.GET("/v1/videos/{video_id}", h.videoRetrieve)
	r.POST("/v1/videos/{video_id}/cancel", h.videoCancel)

	do := func(method, uri, body string) *fasthttp.RequestCtx {
		ctx := &fasthttp.RequestCtx{}
		ctx.Request.Header.SetMethod(method)
		ctx.Request.SetRequestURI(uri)
		ctx.Request.Header.Set("x-bf-api-key", "K2")
		if body != "" {
			ctx.Request.Header.SetContentType("application/json")
			ctx.Request.SetBody([]byte(body))
		}
		r.Handler(ctx)
		return ctx
	}
	type taskObject struct {
		ID             string `json:"id"`
		Status         string `json:"status"`
		ProviderStatus string `json:"provider_status"`
		ExpiresAt      int64  `json:"expires_at"`
		Videos         []struct {
			Type string `json:"type"`
			URL  string `json:"url"`
		} `json:"videos"`
		Usage struct {
			OutputSeconds float64 `json:"output_seconds"`
			VendorCost    struct {
				Amount float64 `json:"amount"`
				Unit   string  `json:"unit"`
			} `json:"vendor_cost"`
		} `json:"usage"`
	}
	type refusal struct {
		Error       struct{ Code, Param string }
		ExtraFields struct {
			ErrorType string `json:"error_type"`
		} `json:"extra_fields"`
	}

	// Create: one call, with K2, the fields unchanged and Tokun's ids in options.
	created := do("POST", "/v1/videos", `{"model":"kling/kling-3.0","prompt":"a dog","seconds":"5","resolution":"1080p","aspect_ratio":"16:9","audio":true,`+
		`"callback_url":"https://hooks.tokun.sh/v/tk:task:1","client_reference_id":"tk:task:1","watermark":true}`)
	require.Equal(t, fasthttp.StatusOK, created.Response.StatusCode(), "body: %s", created.Response.Body())
	var createdTask taskObject
	require.NoError(t, sonic.Unmarshal(created.Response.Body(), &createdTask))
	assert.Equal(t, "893605946402811985:kling", createdTask.ID)
	assert.Equal(t, "queued", createdTask.Status)
	assert.Equal(t, "submitted", createdTask.ProviderStatus)
	got := upstreamCalls()
	require.Len(t, got, 1, "a create is exactly one upstream call")
	assert.Equal(t, "POST /text-to-video/kling-3.0", got[0].method+" "+got[0].path)
	assert.Equal(t, "Bearer kling-key-2", got[0].auth, "the key x-bf-api-key names, and no other")
	assert.JSONEq(t, `{"prompt":"a dog","settings":{"duration":5,"resolution":"1080p","aspect_ratio":"16:9","audio":"native"},`+
		`"options":{"callback_url":"https://hooks.tokun.sh/v/tk:task:1","external_task_id":"tk:task:1","watermark_info":{"enabled":true}}}`, string(got[0].body))

	// Query by the task id and by the client reference: one call each, the same task object.
	byID := do("GET", "/v1/videos/893605946402811985:kling", "")
	require.Equal(t, fasthttp.StatusOK, byID.Response.StatusCode(), "body: %s", byID.Response.Body())
	byReference := do("GET", "/v1/videos/tk:task:1:kling?id_type=client_reference_id", "")
	require.Equal(t, fasthttp.StatusOK, byReference.Response.StatusCode(), "body: %s", byReference.Response.Body())
	got = upstreamCalls()
	require.Len(t, got, 3, "each query is exactly one upstream call")
	assert.Equal(t, "GET /tasks?task_ids=893605946402811985", got[1].method+" "+got[1].path+"?"+got[1].query)
	assert.Equal(t, "GET /tasks?external_task_ids=tk%3Atask%3A1", got[2].method+" "+got[2].path+"?"+got[2].query)
	assert.Equal(t, "Bearer kling-key-2", got[1].auth)
	assert.Equal(t, "Bearer kling-key-2", got[2].auth)
	var taskByID, taskByReference taskObject
	require.NoError(t, sonic.Unmarshal(byID.Response.Body(), &taskByID))
	require.NoError(t, sonic.Unmarshal(byReference.Response.Body(), &taskByReference))
	assert.Equal(t, taskByID, taskByReference, "both queries return the same task object")
	assert.Equal(t, "893605946402811985:kling", taskByReference.ID, "the id is Kling's task id either way")
	assert.Equal(t, "completed", taskByID.Status)
	assert.Equal(t, "succeeded", taskByID.ProviderStatus)
	assert.Equal(t, int64(1781080794+30*24*3600), taskByID.ExpiresAt)
	require.Len(t, taskByID.Videos, 1)
	assert.Equal(t, "url", taskByID.Videos[0].Type)
	assert.Equal(t, "https://v.klingai.com/out.mp4", taskByID.Videos[0].URL)
	assert.Equal(t, 5.0, taskByID.Usage.OutputSeconds)
	assert.Equal(t, 3.5, taskByID.Usage.VendorCost.Amount)
	assert.Equal(t, "unit", taskByID.Usage.VendorCost.Unit)

	// Undeliverable fields and the cancel are refused before any upstream call.
	for param, field := range map[string]string{
		"seconds":               `"seconds":"20"`,
		"seed":                  `"seed":7`,
		"size":                  `"size":"1280x720"`,
		"expires_after_seconds": `"expires_after_seconds":7200`,
		"output_storage_uri":    `"output_storage_uri":"s3://bucket/task/"`,
	} {
		refused := do("POST", "/v1/videos", `{"model":"kling/kling-3.0","prompt":"a dog",`+field+`}`)
		var r refusal
		require.NoError(t, sonic.Unmarshal(refused.Response.Body(), &r), "body: %s", refused.Response.Body())
		assert.Equal(t, fasthttp.StatusBadRequest, refused.Response.StatusCode(), "%s body: %s", param, refused.Response.Body())
		assert.Equal(t, "caller_unsupported_parameter", r.ExtraFields.ErrorType, "%s body: %s", param, refused.Response.Body())
		assert.Equal(t, param, r.Error.Param, "body: %s", refused.Response.Body())
	}
	cancelled := do("POST", "/v1/videos/893605946402811985:kling/cancel", "")
	var cancelRefusal refusal
	require.NoError(t, sonic.Unmarshal(cancelled.Response.Body(), &cancelRefusal))
	assert.Equal(t, fasthttp.StatusBadRequest, cancelled.Response.StatusCode())
	assert.Equal(t, "provider_cancel_unsupported", cancelRefusal.ExtraFields.ErrorType)
	assert.Len(t, upstreamCalls(), 3, "a refused request must not reach Kling")
}
