package xai_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/internal/llmtests"
	"github.com/capsohq/bifrost/core/providers/xai"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/stretchr/testify/require"
)

func TestXAI(t *testing.T) {
	t.Parallel()
	if strings.TrimSpace(os.Getenv("XAI_API_KEY")) == "" {
		t.Skip("Skipping XAI tests because XAI_API_KEY is not set")
	}

	client, ctx, cancel, err := llmtests.SetupTest()
	if err != nil {
		t.Fatalf("Error initializing test setup: %v", err)
	}
	defer cancel()
	defer client.Shutdown()

	testConfig := llmtests.ComprehensiveTestConfig{
		Provider:                schemas.XAI,
		ChatModel:               "grok-4-0709",
		ReasoningModel:          "grok-3-mini",
		TextModel:               "", // xAI dropped raw sampling (/v1/completions); all current models are reasoning models
		VisionModel:             "grok-4-1-fast-reasoning",
		EmbeddingModel:          "", // XAI doesn't support embedding
		ImageGenerationModel:    "grok-imagine-image",
		ExternalCompactionModel: "grok-4.3",
		Scenarios: llmtests.TestScenarios{
			TextCompletion:             true,
			SimpleChat:                 true,
			CompletionStream:           true,
			MultiTurnConversation:      true,
			ToolCalls:                  true,
			ToolCallsStreaming:         true,
			MultipleToolCalls:          true,
			MultipleToolCallsStreaming: true,
			End2EndToolCalling:         true,
			AutomaticFunctionCall:      true,
			ImageURL:                   true,
			ImageBase64:                true,
			ImageGeneration:            true,
			ImageGenerationStream:      false,
			FileBase64:                 false,
			FileURL:                    false,
			MultipleImages:             true,
			CompleteEnd2End:            true,
			Reasoning:                  true,
			Embedding:                  false,
			ListModels:                 true,
			ExternalCompaction:         true,
		},
	}

	t.Run("XAITests", func(t *testing.T) {
		llmtests.RunAllComprehensiveTests(t, client, ctx, testConfig)
	})
}

// A retrieve must land on /v1/models/{model} with the key's bearer token, map the
// provider's model object onto the Bifrost shape, and refuse a path-shaping model id
// before anything is dispatched.
func TestXAIModelRetrieve(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var gotPath, gotMethod, gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotPath, gotMethod, gotAuth = r.URL.Path, r.Method, r.Header.Get("Authorization")
		mu.Unlock()
		_, _ = w.Write([]byte(`{"id":"grok-4","object":"model","created":1743724800,"owned_by":"xai"}`))
	}))
	defer server.Close()

	provider, err := xai.NewXAIProvider(&schemas.ProviderConfig{
		NetworkConfig: schemas.NetworkConfig{BaseURL: server.URL, DefaultRequestTimeoutInSeconds: 30},
	}, bifrost.NewNoOpLogger())
	require.NoError(t, err)

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	key := schemas.Key{Value: *schemas.NewSecretVar("xai-test")}

	response, bifrostErr := provider.ModelRetrieve(ctx, key, &schemas.BifrostModelRetrieveRequest{Model: "grok-4"})
	require.Nil(t, bifrostErr)

	mu.Lock()
	path, method, auth := gotPath, gotMethod, gotAuth
	mu.Unlock()
	require.Equal(t, "/v1/models/grok-4", path)
	require.Equal(t, http.MethodGet, method)
	require.Equal(t, "Bearer xai-test", auth)

	require.Equal(t, "xai/grok-4", response.ID)
	require.Equal(t, schemas.Ptr("xai"), response.OwnedBy)
	require.Equal(t, schemas.Ptr(int64(1743724800)), response.Created)

	_, bifrostErr = provider.ModelRetrieve(ctx, key, &schemas.BifrostModelRetrieveRequest{Model: "../models"})
	require.NotNil(t, bifrostErr)
	require.Equal(t, schemas.Ptr(http.StatusBadRequest), bifrostErr.StatusCode)
}

// xaiImageRecorder is a fake xAI image API: it records every request and answers with the
// status and body the test sets.
type xaiImageRecorder struct {
	mu       sync.Mutex
	paths    []string
	bodies   []map[string]any
	ctypes   []string
	status   int
	response string
}

func (r *xaiImageRecorder) serve(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		raw, _ := io.ReadAll(req.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		r.mu.Lock()
		r.paths = append(r.paths, req.URL.Path)
		r.bodies = append(r.bodies, body)
		r.ctypes = append(r.ctypes, req.Header.Get("Content-Type"))
		status, response := r.status, r.response
		r.mu.Unlock()
		if status == 0 {
			status = http.StatusOK
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(response))
	}))
	t.Cleanup(server.Close)
	return server
}

func (r *xaiImageRecorder) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.paths)
}

func (r *xaiImageRecorder) last(t *testing.T) (string, map[string]any, string) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	require.NotEmpty(t, r.paths, "no upstream call was made")
	n := len(r.paths) - 1
	return r.paths[n], r.bodies[n], r.ctypes[n]
}

func newXAIImageProvider(t *testing.T, baseURL string) *xai.XAIProvider {
	t.Helper()
	provider, err := xai.NewXAIProvider(&schemas.ProviderConfig{
		NetworkConfig: schemas.NetworkConfig{BaseURL: baseURL, DefaultRequestTimeoutInSeconds: 30},
	}, bifrost.NewNoOpLogger())
	require.NoError(t, err)
	return provider
}

func xaiImageCtx() *schemas.BifrostContext {
	return schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
}

var xaiImageKey = schemas.Key{Value: *schemas.NewSecretVar("xai-test")}

// A recorded two-image generation: the shape of xAI's REST reference, with the usage
// fields it lists (cost_in_usd_ticks is required; one USD cent is 100,000,000 ticks).
const xaiRecordedImageResponse = `{"created":1762761600,"data":[{"url":"https://imgen.x.ai/a.jpg","mime_type":"image/jpeg"},{"url":"https://imgen.x.ai/b.jpg","mime_type":"image/jpeg"}],"usage":{"cost_in_usd_ticks":800000000,"input_tokens":12,"output_tokens":0,"total_tokens":12}}`

func requireUnsupportedParameter(t *testing.T, bifrostErr *schemas.BifrostError, param string) {
	t.Helper()
	require.NotNil(t, bifrostErr)
	require.Equal(t, schemas.Ptr(http.StatusBadRequest), bifrostErr.StatusCode)
	require.Equal(t, schemas.ErrorTypeCallerUnsupportedParameter, bifrostErr.ExtraFields.ErrorType)
	require.NotNil(t, bifrostErr.Error)
	require.Equal(t, param, bifrostErr.Error.Param)
}

// requireVendorUsage runs BackfillParams, as core does on every image response, because
// usage.output_images is written there (TXE-6438) from the images the provider left in data.
func requireVendorUsage(t *testing.T, response *schemas.BifrostImageGenerationResponse, outputImages int) {
	t.Helper()
	require.NotNil(t, response)
	response.BackfillParams(nil)
	usage := response.Usage
	require.NotNil(t, usage)
	require.Equal(t, schemas.Ptr(outputImages), usage.OutputImages)
	require.Equal(t, &schemas.VendorCost{Amount: 800000000, Unit: schemas.VendorCostUnitUSDTicks}, usage.VendorCost)
	require.NotNil(t, usage.Cost, "the provider cost is still derived from cost_in_usd_ticks")
	require.InDelta(t, 0.08, usage.Cost.TotalCost, 1e-12)
}

// B4, B6 (contract C4, C5): quality and the unified resolution tier reach xAI on
// grok-imagine-image-2.0, and the recorded usage yields vendor_cost and output_images.
func TestXAIImageGenerationDeliversQualityAndVendorUsage(t *testing.T) {
	t.Parallel()
	recorder := &xaiImageRecorder{response: xaiRecordedImageResponse}
	provider := newXAIImageProvider(t, recorder.serve(t).URL)

	response, bifrostErr := provider.ImageGeneration(xaiImageCtx(), xaiImageKey, &schemas.BifrostImageGenerationRequest{
		Provider: schemas.XAI,
		Model:    "grok-imagine-image-2.0",
		Input:    &schemas.ImageGenerationInput{Prompt: "a lighthouse"},
		Params: &schemas.ImageGenerationParameters{
			N:           schemas.Ptr(2),
			Quality:     schemas.Ptr("medium"),
			Resolution:  "2K",
			AspectRatio: schemas.Ptr("16:9"),
			ExtraParams: map[string]interface{}{"resolution": "2K"},
		},
	})
	require.Nil(t, bifrostErr)

	path, body, ctype := recorder.last(t)
	require.Equal(t, "/v1/images/generations", path)
	require.Equal(t, "application/json", ctype)
	require.Equal(t, "medium", body["quality"])
	require.Equal(t, "2k", body["resolution"])
	require.Equal(t, "16:9", body["aspect_ratio"])
	require.EqualValues(t, 2, body["n"])

	require.Len(t, response.Data, 2)
	require.Nil(t, response.ContentFilter)
	requireVendorUsage(t, response, 2)
}

// B4, B5 (contract C2–C4): quality on a model without it, deferred, and a resolution tier
// xAI does not offer are refused with caller_unsupported_parameter before any upstream call.
func TestXAIImageGenerationRefusesUndeliverableFields(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		model  string
		params *schemas.ImageGenerationParameters
		param  string
	}{
		{"quality on grok-imagine-image", "grok-imagine-image", &schemas.ImageGenerationParameters{Quality: schemas.Ptr("low")}, "quality"},
		{"deferred", "grok-imagine-image-2.0", &schemas.ImageGenerationParameters{ExtraParams: map[string]interface{}{"deferred": true}}, "deferred"},
		{"resolution 4K", "grok-imagine-image-2.0", &schemas.ImageGenerationParameters{Resolution: "4K"}, "resolution"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			recorder := &xaiImageRecorder{response: xaiRecordedImageResponse}
			provider := newXAIImageProvider(t, recorder.serve(t).URL)
			_, bifrostErr := provider.ImageGeneration(xaiImageCtx(), xaiImageKey, &schemas.BifrostImageGenerationRequest{
				Provider: schemas.XAI,
				Model:    tc.model,
				Input:    &schemas.ImageGenerationInput{Prompt: "a lighthouse"},
				Params:   tc.params,
			})
			requireUnsupportedParameter(t, bifrostErr, tc.param)
			require.Zero(t, recorder.calls(), "a refused field must make no upstream call")
		})
	}
}

// B9 (contract C4, C5): an edit is a JSON POST to /v1/images/edits; a URL source is sent
// unchanged, bytes become a data URI, and the response carries the same usage as generation.
func TestXAIImageEditSendsJSONSources(t *testing.T) {
	t.Parallel()
	pngBytes := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00")

	t.Run("one URL source goes in image", func(t *testing.T) {
		t.Parallel()
		recorder := &xaiImageRecorder{response: xaiRecordedImageResponse}
		provider := newXAIImageProvider(t, recorder.serve(t).URL)
		response, bifrostErr := provider.ImageEdit(xaiImageCtx(), xaiImageKey, &schemas.BifrostImageEditRequest{
			Provider: schemas.XAI,
			Model:    "grok-imagine-image-2.0",
			Input: &schemas.ImageEditInput{
				Prompt: "make it night",
				Images: []schemas.ImageInput{{URL: "https://example.com/day.png"}},
			},
			Params: &schemas.ImageEditParameters{Quality: schemas.Ptr("medium"), N: schemas.Ptr(2)},
		})
		require.Nil(t, bifrostErr)

		path, body, ctype := recorder.last(t)
		require.Equal(t, "/v1/images/edits", path)
		require.Equal(t, "application/json", ctype)
		require.Equal(t, "grok-imagine-image-2.0", body["model"])
		require.Equal(t, "make it night", body["prompt"])
		require.Equal(t, map[string]any{"url": "https://example.com/day.png", "type": "image_url"}, body["image"])
		require.NotContains(t, body, "images")
		require.Equal(t, "medium", body["quality"])

		require.Len(t, response.Data, 2)
		requireVendorUsage(t, response, 2)
	})

	t.Run("several sources, bytes as a data URI, go in images", func(t *testing.T) {
		t.Parallel()
		recorder := &xaiImageRecorder{response: xaiRecordedImageResponse}
		provider := newXAIImageProvider(t, recorder.serve(t).URL)
		dataURI := "data:image/jpeg;base64,/9j/4AAQ"
		_, bifrostErr := provider.ImageEdit(xaiImageCtx(), xaiImageKey, &schemas.BifrostImageEditRequest{
			Provider: schemas.XAI,
			Model:    "grok-imagine-image",
			Input: &schemas.ImageEditInput{
				Prompt: "put <IMAGE_0> beside <IMAGE_1>",
				Images: []schemas.ImageInput{{Image: pngBytes}, {URL: dataURI}},
			},
		})
		require.Nil(t, bifrostErr)

		_, body, _ := recorder.last(t)
		require.NotContains(t, body, "image")
		require.Equal(t, []any{
			map[string]any{"url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(pngBytes), "type": "image_url"},
			map[string]any{"url": dataURI, "type": "image_url"},
		}, body["images"])
	})
}

// B9, B5 (contract C4): an edit refuses, before any upstream call, the price-affecting
// fields xAI's edit endpoint has no counterpart for, quality on a model without it, and
// deferred.
func TestXAIImageEditRefusesUndeliverableFields(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		model  string
		params *schemas.ImageEditParameters
		param  string
	}{
		{"size", "grok-imagine-image-2.0", &schemas.ImageEditParameters{Size: schemas.Ptr("1024x1024")}, "size"},
		{"mask", "grok-imagine-image-2.0", &schemas.ImageEditParameters{Mask: []byte("mask")}, "mask"},
		{"output_format", "grok-imagine-image-2.0", &schemas.ImageEditParameters{OutputFormat: schemas.Ptr("png")}, "output_format"},
		{"quality on grok-imagine-image", "grok-imagine-image", &schemas.ImageEditParameters{Quality: schemas.Ptr("low")}, "quality"},
		{"deferred", "grok-imagine-image-2.0", &schemas.ImageEditParameters{ExtraParams: map[string]interface{}{"deferred": true}}, "deferred"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			recorder := &xaiImageRecorder{response: xaiRecordedImageResponse}
			provider := newXAIImageProvider(t, recorder.serve(t).URL)
			_, bifrostErr := provider.ImageEdit(xaiImageCtx(), xaiImageKey, &schemas.BifrostImageEditRequest{
				Provider: schemas.XAI,
				Model:    tc.model,
				Input: &schemas.ImageEditInput{
					Prompt: "make it night",
					Images: []schemas.ImageInput{{URL: "https://example.com/day.png"}},
				},
				Params: tc.params,
			})
			requireUnsupportedParameter(t, bifrostErr, tc.param)
			require.Zero(t, recorder.calls(), "a refused field must make no upstream call")
		})
	}
}

// B7 (contract C6 convention three): an image xAI marks respect_moderation: false takes no
// slot in data and is counted in content_filter; output_images counts only what was
// delivered. xAI gives no reason code, so reasons stays empty.
func TestXAIImageBlockedImagesFillContentFilter(t *testing.T) {
	t.Parallel()

	t.Run("partly blocked", func(t *testing.T) {
		t.Parallel()
		recorder := &xaiImageRecorder{response: `{"data":[{"url":"https://imgen.x.ai/a.jpg","respect_moderation":true},{"url":"https://imgen.x.ai/b.jpg","respect_moderation":false}],"usage":{"cost_in_usd_ticks":800000000}}`}
		provider := newXAIImageProvider(t, recorder.serve(t).URL)
		response, bifrostErr := provider.ImageGeneration(xaiImageCtx(), xaiImageKey, &schemas.BifrostImageGenerationRequest{
			Provider: schemas.XAI, Model: "grok-imagine-image",
			Input:  &schemas.ImageGenerationInput{Prompt: "p"},
			Params: &schemas.ImageGenerationParameters{N: schemas.Ptr(2)},
		})
		require.Nil(t, bifrostErr)
		require.Len(t, response.Data, 1)
		require.Equal(t, "https://imgen.x.ai/a.jpg", response.Data[0].URL)
		require.Equal(t, &schemas.ContentFilterInfo{FilteredCount: 1}, response.ContentFilter)
		requireVendorUsage(t, response, 1)
	})

	t.Run("all blocked on an edit", func(t *testing.T) {
		t.Parallel()
		recorder := &xaiImageRecorder{response: `{"data":[{"respect_moderation":false},{"respect_moderation":false}],"usage":{"cost_in_usd_ticks":800000000}}`}
		provider := newXAIImageProvider(t, recorder.serve(t).URL)
		response, bifrostErr := provider.ImageEdit(xaiImageCtx(), xaiImageKey, &schemas.BifrostImageEditRequest{
			Provider: schemas.XAI, Model: "grok-imagine-image",
			Input:  &schemas.ImageEditInput{Prompt: "p", Images: []schemas.ImageInput{{URL: "https://example.com/a.png"}}},
			Params: &schemas.ImageEditParameters{N: schemas.Ptr(2)},
		})
		require.Nil(t, bifrostErr)
		require.NotNil(t, response.Data)
		require.Empty(t, response.Data)
		require.Equal(t, &schemas.ContentFilterInfo{FilteredCount: 2}, response.ContentFilter)
		requireVendorUsage(t, response, 0)

		out, err := json.Marshal(response)
		require.NoError(t, err)
		require.Contains(t, string(out), `"data":[]`)
		require.Contains(t, string(out), `"content_filter":{"filtered_count":2}`)
	})
}

// B8, B10 (contract C6): a vendor error recognised as a content block keeps the vendor's
// status and is declared caller_content_filtered, on generation and edit, in xAI's own
// error shape and in the OpenAI envelope; any other vendor error is not.
func TestXAIImageErrorsMarkContentBlocks(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		status   int
		body     string
		edit     bool
		filtered bool
		message  string
	}{
		{"xAI shape 400 on generation", http.StatusBadRequest, `{"code":"content_policy_violation","error":"Generated image rejected by content moderation."}`, false, true, "Generated image rejected by content moderation."},
		{"xAI shape 403 on edit", http.StatusForbidden, `{"code":"content_policy_violation","error":"Generated image rejected by content moderation."}`, true, true, "Generated image rejected by content moderation."},
		{"OpenAI envelope on edit", http.StatusBadRequest, `{"error":{"message":"blocked","type":"invalid_request_error","code":"moderation_blocked","param":null}}`, true, true, "blocked"},
		{"other error", http.StatusUnprocessableEntity, `{"code":"Client specified an invalid argument","error":"prompt is required"}`, false, false, "prompt is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			recorder := &xaiImageRecorder{status: tc.status, response: tc.body}
			provider := newXAIImageProvider(t, recorder.serve(t).URL)
			var bifrostErr *schemas.BifrostError
			if tc.edit {
				_, bifrostErr = provider.ImageEdit(xaiImageCtx(), xaiImageKey, &schemas.BifrostImageEditRequest{
					Provider: schemas.XAI, Model: "grok-imagine-image",
					Input: &schemas.ImageEditInput{Prompt: "p", Images: []schemas.ImageInput{{URL: "https://example.com/a.png"}}},
				})
			} else {
				_, bifrostErr = provider.ImageGeneration(xaiImageCtx(), xaiImageKey, &schemas.BifrostImageGenerationRequest{
					Provider: schemas.XAI, Model: "grok-imagine-image",
					Input: &schemas.ImageGenerationInput{Prompt: "p"},
				})
			}
			require.NotNil(t, bifrostErr)
			require.Equal(t, 1, recorder.calls())
			require.Equal(t, schemas.Ptr(tc.status), bifrostErr.StatusCode, "the vendor status is kept")
			require.Equal(t, tc.message, bifrostErr.Error.Message)
			if tc.filtered {
				require.Equal(t, schemas.ErrorTypeCallerContentFiltered, bifrostErr.ExtraFields.ErrorType)
			} else {
				require.NotEqual(t, schemas.ErrorTypeCallerContentFiltered, bifrostErr.ExtraFields.ErrorType)
			}
		})
	}
}

// The large-response threshold still bounds memory on xAI image responses: a body over it is
// handed to the transport as a stream, unchanged, and is never materialised; a body under it
// is normalised as usual; an error body is read and parsed with xAI's error shape.
func TestXAIImageLargeResponseThreshold(t *testing.T) {
	t.Parallel()
	request := &schemas.BifrostImageGenerationRequest{
		Provider: schemas.XAI,
		Model:    "grok-imagine-image-2.0",
		Input:    &schemas.ImageGenerationInput{Prompt: "a lighthouse"},
	}

	t.Run("over the threshold streams xAI's body", func(t *testing.T) {
		t.Parallel()
		recorder := &xaiImageRecorder{response: xaiRecordedImageResponse}
		provider := newXAIImageProvider(t, recorder.serve(t).URL)
		ctx := xaiImageCtx()
		ctx.SetValue(schemas.BifrostContextKeyLargeResponseThreshold, int64(64))

		response, bifrostErr := provider.ImageGeneration(ctx, xaiImageKey, request)
		require.Nil(t, bifrostErr)
		require.Nil(t, response.Data, "a large body is streamed, not decoded")
		require.Equal(t, true, ctx.Value(schemas.BifrostContextKeyLargeResponseMode))
		reader, ok := ctx.Value(schemas.BifrostContextKeyLargeResponseReader).(io.ReadCloser)
		require.True(t, ok, "the transport gets a reader over xAI's body")
		streamed, err := io.ReadAll(reader)
		require.NoError(t, err)
		require.NoError(t, reader.Close())
		require.JSONEq(t, xaiRecordedImageResponse, string(streamed))
	})

	t.Run("under the threshold is normalised", func(t *testing.T) {
		t.Parallel()
		recorder := &xaiImageRecorder{response: xaiRecordedImageResponse}
		provider := newXAIImageProvider(t, recorder.serve(t).URL)
		ctx := xaiImageCtx()
		ctx.SetValue(schemas.BifrostContextKeyLargeResponseThreshold, int64(1<<20))

		response, bifrostErr := provider.ImageGeneration(ctx, xaiImageKey, request)
		require.Nil(t, bifrostErr)
		require.Nil(t, ctx.Value(schemas.BifrostContextKeyLargeResponseReader))
		require.Len(t, response.Data, 2)
		requireVendorUsage(t, response, 2)
	})

	t.Run("an error body is parsed while streaming is on", func(t *testing.T) {
		t.Parallel()
		recorder := &xaiImageRecorder{status: http.StatusForbidden, response: `{"code":"content_policy_violation","error":"blocked"}`}
		provider := newXAIImageProvider(t, recorder.serve(t).URL)
		ctx := xaiImageCtx()
		ctx.SetValue(schemas.BifrostContextKeyLargeResponseThreshold, int64(16))

		_, bifrostErr := provider.ImageGeneration(ctx, xaiImageKey, request)
		require.NotNil(t, bifrostErr)
		require.Equal(t, schemas.Ptr(http.StatusForbidden), bifrostErr.StatusCode)
		require.Equal(t, schemas.ErrorTypeCallerContentFiltered, bifrostErr.ExtraFields.ErrorType)
		require.Equal(t, "blocked", bifrostErr.Error.Message)
	})
}

// benchXAIImageServer answers every request with body, for the request-path benchmarks.
func benchXAIImageServer(b *testing.B, body string) string {
	b.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = io.Copy(io.Discard, req.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	b.Cleanup(server.Close)
	return server.URL
}

func benchXAIImageProvider(b *testing.B, body string) *xai.XAIProvider {
	b.Helper()
	provider, err := xai.NewXAIProvider(&schemas.ProviderConfig{
		NetworkConfig: schemas.NetworkConfig{BaseURL: benchXAIImageServer(b, body), DefaultRequestTimeoutInSeconds: 30},
	}, bifrost.NewNoOpLogger())
	if err != nil {
		b.Fatal(err)
	}
	return provider
}

// BenchmarkXAIImageGeneration measures one generation through the provider against a loopback
// server: the ordinary response (no moderation flags) and one carrying respect_moderation.
func BenchmarkXAIImageGeneration(b *testing.B) {
	request := &schemas.BifrostImageGenerationRequest{
		Provider: schemas.XAI,
		Model:    "grok-imagine-image",
		Input:    &schemas.ImageGenerationInput{Prompt: "a lighthouse"},
		Params:   &schemas.ImageGenerationParameters{N: schemas.Ptr(2), AspectRatio: schemas.Ptr("16:9")},
	}
	flagged := strings.Replace(xaiRecordedImageResponse, `"mime_type":"image/jpeg"}`, `"mime_type":"image/jpeg","respect_moderation":true}`, -1)
	for _, bc := range []struct{ name, body string }{
		{"ordinary", xaiRecordedImageResponse},
		{"with_moderation_flags", flagged},
	} {
		b.Run(bc.name, func(b *testing.B) {
			provider := benchXAIImageProvider(b, bc.body)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
				if _, bifrostErr := provider.ImageGeneration(ctx, xaiImageKey, request); bifrostErr != nil {
					b.Fatal(bifrostErr.Error.Message)
				}
			}
		})
	}
}

// BenchmarkXAIImageEdit measures one JSON edit from a URL source (no before: xAI image edit was
// an unsupported operation).
func BenchmarkXAIImageEdit(b *testing.B) {
	provider := benchXAIImageProvider(b, xaiRecordedImageResponse)
	request := &schemas.BifrostImageEditRequest{
		Provider: schemas.XAI,
		Model:    "grok-imagine-image-2.0",
		Input: &schemas.ImageEditInput{
			Prompt: "make it blue",
			Images: []schemas.ImageInput{{URL: "https://example.com/source.png"}},
		},
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
		if _, bifrostErr := provider.ImageEdit(ctx, xaiImageKey, request); bifrostErr != nil {
			b.Fatal(bifrostErr.Error.Message)
		}
	}
}

// Large-payload mode streams the caller's multipart body unparsed, which xAI's JSON edit cannot
// take: the edit is refused with a 400 before any upstream call, not failed on the empty input.
func TestXAIImageEditRefusesLargePayloadPassthrough(t *testing.T) {
	t.Parallel()
	recorder := &xaiImageRecorder{response: xaiRecordedImageResponse}
	provider := newXAIImageProvider(t, recorder.serve(t).URL)
	ctx := xaiImageCtx()
	ctx.SetValue(schemas.BifrostContextKeyLargePayloadMode, true)
	ctx.SetValue(schemas.BifrostContextKeyLargePayloadReader, io.Reader(strings.NewReader("--boundary\r\n")))

	// The large-payload hook fills only routing metadata, so the input is empty.
	_, bifrostErr := provider.ImageEdit(ctx, xaiImageKey, &schemas.BifrostImageEditRequest{
		Provider: schemas.XAI,
		Model:    "grok-imagine-image-2.0",
	})
	require.NotNil(t, bifrostErr)
	require.Equal(t, schemas.Ptr(http.StatusBadRequest), bifrostErr.StatusCode)
	require.Contains(t, bifrostErr.Error.Message, "large payload passthrough is not supported for xai image edit")
	require.Zero(t, recorder.calls())
}
