package gemini

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Interaction bodies in the shape the vendor documents: the Interaction schema of
// https://ai.google.dev/static/api/interactions.openapi.json and the "Raw REST JSON structure"
// samples of https://ai.google.dev/gemini-api/docs/omni.
const (
	omniInteractionID = "v1_ChdPU0F4YWFtNkFwS2kxZThQZ05lbXdROBIXT1NBeGFhbTZBcEtpMWU4UGdOZW13UTg"

	omniInteractionInProgress = `{"id":"` + omniInteractionID + `","object":"interaction","status":"in_progress","model":"gemini-omni-1.1-flash","created":"2026-11-10T12:22:47Z","updated":"2026-11-10T12:22:47Z"}`

	omniInteractionCancelled = `{"id":"` + omniInteractionID + `","object":"interaction","status":"cancelled","model":"gemini-omni-1.1-flash","created":"2026-11-10T12:22:47Z","updated":"2026-11-10T12:23:10Z"}`

	// A completed interaction created with delivery=uri: the video is a Gemini Files file.
	omniInteractionCompletedURITemplate = `{
  "steps": [
    {"type": "user_input", "content": [{"type": "text", "text": "a marble on a track"}]},
    {"type": "thought", "content": [{"text": "planning the shot", "type": "thought"}]},
    {"type": "function_call", "name": "unused", "arguments": {"a": 1}, "id": "x"},
    {"type": "model_output", "content": [{"type": "video", "mime_type": "video/mp4", "uri": "FILE_URI"}]}
  ],
  "id": "` + omniInteractionID + `",
  "status": "completed",
  "model": "gemini-omni-1.1-flash",
  "object": "interaction",
  "created": "2026-11-10T12:22:47Z",
  "updated": "2026-11-10T12:25:15Z",
  "usage": {
    "input_tokens_by_modality": [{"modality": "text", "tokens": 12}, {"modality": "image", "tokens": 258}],
    "output_tokens_by_modality": [{"modality": "text", "tokens": 7}, {"modality": "video", "tokens": 46336}],
    "total_input_tokens": 270,
    "total_output_tokens": 46343,
    "total_tokens": 46613
  }
}`
)

func omniVideoRequest(model string, input *schemas.VideoGenerationInput, params *schemas.VideoGenerationParameters) *schemas.BifrostVideoGenerationRequest {
	if input == nil {
		input = &schemas.VideoGenerationInput{Prompt: "a marble on a track"}
	}
	return &schemas.BifrostVideoGenerationRequest{Provider: schemas.Gemini, Model: model, Input: input, Params: params}
}

func marshalInteractionRequest(t *testing.T, req *GeminiInteractionRequest, mergeExtraParams bool) string {
	t.Helper()
	body, err := providerUtils.MarshalConvertedRequestBody(req, mergeExtraParams)
	require.NoError(t, err)
	return string(body)
}

func requireUnsupportedParameter(t *testing.T, err error, param string) {
	t.Helper()
	require.Error(t, err)
	bifrostErr, ok := providerUtils.AsBifrostBadRequestError(err)
	require.True(t, ok, "want a 400 converter refusal, got %v", err)
	requireUnsupportedParameterError(t, bifrostErr, param)
}

func requireUnsupportedParameterError(t *testing.T, bifrostErr *schemas.BifrostError, param string) {
	t.Helper()
	require.NotNil(t, bifrostErr)
	require.NotNil(t, bifrostErr.StatusCode)
	assert.Equal(t, http.StatusBadRequest, *bifrostErr.StatusCode)
	assert.Equal(t, schemas.ErrorTypeCallerUnsupportedParameter, bifrostErr.ExtraFields.ErrorType)
	require.NotNil(t, bifrostErr.Error)
	assert.Equal(t, param, bifrostErr.Error.Param)
}

func TestIsOmniVideoModel(t *testing.T) {
	for model, want := range map[string]bool{
		"gemini-omni-1.1-flash":         true,
		"gemini-omni-1.2-flash":         true,
		"gemini-omni-flash-preview":     true,
		"gemini-omni-1.1-flash-preview": true,
		"google/gemini-omni-1.1-flash":  true,
		"veo-3.1-generate-001":          false,
		"gemini-3.8-flash":              false,
		"":                              false,
	} {
		assert.Equal(t, want, IsOmniVideoModel(nil, model), model)
	}
}

// The create body carries every unified field in the vendor's own field, and nothing else.
func TestToGeminiInteractionVideoRequest_MapsUnifiedFields(t *testing.T) {
	req, err := ToGeminiInteractionVideoRequest(omniVideoRequest("google/gemini-omni-1.1-flash", nil, &schemas.VideoGenerationParameters{
		Seconds:        schemas.Ptr("8"),
		AspectRatio:    "9:16",
		Resolution:     "1080p",
		Seed:           schemas.Ptr(7),
		NegativePrompt: schemas.Ptr("no rain"),
		TaskControlParameters: schemas.TaskControlParameters{
			CallbackURL:       schemas.Ptr("https://hooks.tokun.sh/v/task-1"),
			ClientReferenceID: schemas.Ptr("task-1"),
		},
		// The handlers keep the unified fields in the extra params under their own name too.
		ExtraParams: map[string]any{"aspect_ratio": "9:16", "resolution": "1080p", "store": true},
	}), GeminiInteractionVideoOptions)
	require.NoError(t, err)

	assert.JSONEq(t, `{
		"model": "gemini-omni-1.1-flash",
		"input": [{"type": "text", "text": "a marble on a track"}],
		"response_format": {"type": "video", "delivery": "uri", "aspect_ratio": "9:16", "duration": "8s", "resolution": "1080p"},
		"generation_config": {"seed": 7},
		"background": true,
		"webhook_config": {"uris": ["https://hooks.tokun.sh/v/task-1"], "user_metadata": {"client_reference_id": "task-1"}}
	}`, marshalInteractionRequest(t, req, false))

	// With x-bf-passthrough-extra-params the vendor-only extras are merged; the unified
	// fields, already converted, are not merged a second time under their unified names.
	assert.JSONEq(t, `{
		"model": "gemini-omni-1.1-flash",
		"input": [{"type": "text", "text": "a marble on a track"}],
		"response_format": {"type": "video", "delivery": "uri", "aspect_ratio": "9:16", "duration": "8s", "resolution": "1080p"},
		"generation_config": {"seed": 7},
		"background": true,
		"store": true,
		"webhook_config": {"uris": ["https://hooks.tokun.sh/v/task-1"], "user_metadata": {"client_reference_id": "task-1"}}
	}`, marshalInteractionRequest(t, req, true))
}

func TestToGeminiInteractionVideoRequest_DefaultsAndDelivery(t *testing.T) {
	// No params: a text input, background true, and the Gemini API's default delivery.
	req, err := ToGeminiInteractionVideoRequest(omniVideoRequest("gemini-omni-1.1-flash", nil, nil), GeminiInteractionVideoOptions)
	require.NoError(t, err)
	assert.JSONEq(t, `{"model":"gemini-omni-1.1-flash","input":[{"type":"text","text":"a marble on a track"}],"response_format":{"type":"video","delivery":"uri"},"background":true}`,
		marshalInteractionRequest(t, req, true))

	// The extra param delivery picks the mode; it is consumed, not merged at the top level.
	req, err = ToGeminiInteractionVideoRequest(omniVideoRequest("gemini-omni-1.1-flash", nil, &schemas.VideoGenerationParameters{
		ExtraParams: map[string]any{"delivery": "inline"},
	}), GeminiInteractionVideoOptions)
	require.NoError(t, err)
	assert.JSONEq(t, `{"model":"gemini-omni-1.1-flash","input":[{"type":"text","text":"a marble on a track"}],"response_format":{"type":"video","delivery":"inline"},"background":true}`,
		marshalInteractionRequest(t, req, true))

	// Options without a default delivery (Vertex) send none.
	req, err = ToGeminiInteractionVideoRequest(omniVideoRequest("gemini-omni-1.1-flash-preview", nil, nil), InteractionVideoOptions{})
	require.NoError(t, err)
	assert.JSONEq(t, `{"model":"gemini-omni-1.1-flash-preview","input":[{"type":"text","text":"a marble on a track"}],"response_format":{"type":"video"},"background":true}`,
		marshalInteractionRequest(t, req, true))

	_, err = ToGeminiInteractionVideoRequest(omniVideoRequest("gemini-omni-1.1-flash", nil, &schemas.VideoGenerationParameters{
		ExtraParams: map[string]any{"delivery": "gcs"},
	}), GeminiInteractionVideoOptions)
	requireUnsupportedParameter(t, err, "delivery")
}

// Images keep the form the caller sent: a URL goes in uri unchanged, a data URL is decoded.
func TestToGeminiInteractionVideoRequest_Images(t *testing.T) {
	const dataURL = "data:image/jpeg;base64,Zmlyc3Q="

	req, err := ToGeminiInteractionVideoRequest(omniVideoRequest("gemini-omni-1.1-flash",
		&schemas.VideoGenerationInput{Prompt: "forest to snow", InputReference: schemas.Ptr(dataURL)},
		&schemas.VideoGenerationParameters{LastFrame: "https://cdn.example.com/last.png"},
	), GeminiInteractionVideoOptions)
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"model": "gemini-omni-1.1-flash",
		"input": [
			{"type": "image", "data": "Zmlyc3Q=", "mime_type": "image/jpeg"},
			{"type": "image", "uri": "https://cdn.example.com/last.png"},
			{"type": "text", "text": "forest to snow"}
		],
		"response_format": {"type": "video", "delivery": "uri"},
		"generation_config": {"video_config": {"task": "image_to_video"}},
		"background": true
	}`, marshalInteractionRequest(t, req, false))

	req, err = ToGeminiInteractionVideoRequest(omniVideoRequest("gemini-omni-1.1-flash",
		&schemas.VideoGenerationInput{Prompt: "the cat bats the yarn", VideoURI: schemas.Ptr("https://generativelanguage.googleapis.com/v1beta/files/src")},
		&schemas.VideoGenerationParameters{ReferenceImages: schemas.VideoReferenceImages{"https://cdn.example.com/cat.png", dataURL}},
	), GeminiInteractionVideoOptions)
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"model": "gemini-omni-1.1-flash",
		"input": [
			{"type": "image", "uri": "https://cdn.example.com/cat.png"},
			{"type": "image", "data": "Zmlyc3Q=", "mime_type": "image/jpeg"},
			{"type": "video", "uri": "https://generativelanguage.googleapis.com/v1beta/files/src"},
			{"type": "text", "text": "the cat bats the yarn"}
		],
		"response_format": {"type": "video", "delivery": "uri"},
		"generation_config": {"video_config": {"task": "reference_to_video"}},
		"background": true
	}`, marshalInteractionRequest(t, req, false))

	// A last frame is only the second of two images; references and frames are two tasks.
	_, err = ToGeminiInteractionVideoRequest(omniVideoRequest("gemini-omni-1.1-flash", nil,
		&schemas.VideoGenerationParameters{LastFrame: dataURL}), GeminiInteractionVideoOptions)
	requireUnsupportedParameter(t, err, "last_frame")
	_, err = ToGeminiInteractionVideoRequest(omniVideoRequest("gemini-omni-1.1-flash",
		&schemas.VideoGenerationInput{Prompt: "p", InputReference: schemas.Ptr(dataURL)},
		&schemas.VideoGenerationParameters{ReferenceImages: schemas.VideoReferenceImages{dataURL}}), GeminiInteractionVideoOptions)
	requireUnsupportedParameter(t, err, "reference_images")
	_, err = ToGeminiInteractionVideoRequest(omniVideoRequest("gemini-omni-1.1-flash",
		&schemas.VideoGenerationInput{Prompt: "p", InputReference: schemas.Ptr("data:;base64,")}, nil), GeminiInteractionVideoOptions)
	requireUnsupportedParameter(t, err, "input_reference")
}

// A field the Interactions API has no field for is refused, never dropped (contract C4).
func TestToGeminiInteractionVideoRequest_RefusesUndeliverableFields(t *testing.T) {
	for param, params := range map[string]*schemas.VideoGenerationParameters{
		"size":                  {Size: "1280x720"},
		"audio":                 {Audio: schemas.Ptr(true)},
		"output_format":         {OutputFormat: schemas.Ptr("webm")},
		"type":                  {Type: schemas.Ptr("upscale")},
		"upscale_factor":        {UpscaleFactor: schemas.Ptr(2)},
		"target_megapixels":     {TargetMegapixels: schemas.Ptr(8)},
		"expires_after_seconds": {TaskControlParameters: schemas.TaskControlParameters{ExpiresAfterSeconds: schemas.Ptr(600)}},
		"output_storage_uri":    {TaskControlParameters: schemas.TaskControlParameters{OutputStorageURI: schemas.Ptr("gs://b/p")}},
	} {
		_, err := ToGeminiInteractionVideoRequest(omniVideoRequest("gemini-omni-1.1-flash", nil, params), GeminiInteractionVideoOptions)
		requireUnsupportedParameter(t, err, param)
	}

	for _, seconds := range []string{"8.5", "eight", "", "0", "-3"} {
		_, err := ToGeminiInteractionVideoRequest(omniVideoRequest("gemini-omni-1.1-flash", nil,
			&schemas.VideoGenerationParameters{Seconds: schemas.Ptr(seconds)}), GeminiInteractionVideoOptions)
		requireUnsupportedParameter(t, err, "seconds")
	}

	// Where webhook_config is not documented (Vertex), the two control fields are refused.
	for param, control := range map[string]schemas.TaskControlParameters{
		"callback_url":        {CallbackURL: schemas.Ptr("https://hooks.tokun.sh/v/task-1")},
		"client_reference_id": {ClientReferenceID: schemas.Ptr("task-1")},
	} {
		_, err := ToGeminiInteractionVideoRequest(omniVideoRequest("gemini-omni-1.1-flash-preview", nil,
			&schemas.VideoGenerationParameters{TaskControlParameters: control}), InteractionVideoOptions{})
		requireUnsupportedParameter(t, err, param)
	}
}

// The provider's declaration must name every create parameter the Veo path took before the
// declaration existed: core refuses any field that is set and not declared, for every model.
func TestGeminiVideoGenerationUnifiedFields_CoverEveryCreateParameter(t *testing.T) {
	provider := NewGeminiProvider(&schemas.ProviderConfig{}, testNoopLogger{})
	declared, ok := provider.SupportedUnifiedFields(schemas.VideoGenerationRequest)
	require.True(t, ok)
	assert.True(t, slices.IsSorted(declared[:len(declared)-2]), "keep the list sorted")

	var want []string
	var collect func(typ reflect.Type)
	collect = func(typ reflect.Type) {
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			if field.Anonymous {
				collect(field.Type)
				continue
			}
			name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
			if name != "" && name != "-" {
				want = append(want, name)
			}
		}
	}
	collect(reflect.TypeOf(schemas.VideoGenerationParameters{}))
	// The two control fields no gemini video API takes stay undeclared, so core refuses them.
	want = slices.DeleteFunc(want, func(name string) bool {
		return name == schemas.TaskControlFieldExpiresAfterSeconds || name == schemas.TaskControlFieldOutputStorageURI
	})
	assert.ElementsMatch(t, want, declared)

	for _, requestType := range []schemas.RequestType{schemas.ChatCompletionRequest, schemas.ImageGenerationRequest, schemas.VideoRetrieveRequest} {
		_, ok := provider.SupportedUnifiedFields(requestType)
		assert.False(t, ok, requestType)
	}
	for _, field := range OmniVideoUnifiedFields {
		assert.Contains(t, declared, field)
	}
}

// Each of the eight statuses the API documents: the four with a task-object status of the same
// meaning map by name, the other four stay in provider_status and report in_progress.
func TestToBifrostInteractionVideoResponse_Statuses(t *testing.T) {
	for raw, want := range map[string]schemas.VideoStatus{
		"in_progress":           schemas.VideoStatusInProgress,
		"completed":             schemas.VideoStatusCompleted,
		"failed":                schemas.VideoStatusFailed,
		"cancelled":             schemas.VideoStatusCancelled,
		"requires_action":       schemas.VideoStatusInProgress,
		"incomplete":            schemas.VideoStatusInProgress,
		"budget_exceeded":       schemas.VideoStatusInProgress,
		"requires_continuation": schemas.VideoStatusInProgress,
	} {
		resp, bifrostErr := ToBifrostInteractionVideoResponse(&GeminiInteraction{
			ID: omniInteractionID, Status: raw, Created: "2026-11-10T12:22:47Z", Updated: "2026-11-10T12:25:15Z",
		}, GeminiInteractionVideoOptions)
		require.Nil(t, bifrostErr, raw)
		assert.Equal(t, want, resp.Status, raw)
		assert.Equal(t, raw, resp.ProviderStatus, raw)
		assert.Equal(t, int64(1794313367), resp.CreatedAt, raw)
		if want.IsTerminal() {
			require.NotNil(t, resp.CompletedAt, raw)
			assert.Equal(t, int64(1794313515), *resp.CompletedAt, raw)
		} else {
			assert.Nil(t, resp.CompletedAt, raw)
		}
		assert.Nil(t, resp.ContentFilter, raw)
		assert.Nil(t, resp.ExpiresAt, raw)
	}

	_, bifrostErr := ToBifrostInteractionVideoResponse(&GeminiInteraction{Status: "completed"}, GeminiInteractionVideoOptions)
	require.NotNil(t, bifrostErr, "an interaction without an id is not a task")
}

func decodeInteraction(t *testing.T, body string) *GeminiInteraction {
	t.Helper()
	var interaction GeminiInteraction
	require.NoError(t, schemas.Unmarshal([]byte(body), &interaction))
	return &interaction
}

func TestToBifrostInteractionVideoResponse_ArtifactUsageAndExpiry(t *testing.T) {
	uriBody := strings.Replace(omniInteractionCompletedURITemplate, "FILE_URI", "https://generativelanguage.googleapis.com/v1beta/files/abc123:download?alt=media", 1)

	// delivery=uri: a "content" entry with no url and no bytes, usage from the video modality
	// only, and expires_at 48 hours after completion.
	resp, bifrostErr := ToBifrostInteractionVideoResponse(decodeInteraction(t, uriBody), GeminiInteractionVideoOptions)
	require.Nil(t, bifrostErr)
	assert.Equal(t, schemas.VideoStatusCompleted, resp.Status)
	assert.Equal(t, "gemini-omni-1.1-flash", resp.Model)
	require.Len(t, resp.Videos, 1)
	assert.Equal(t, schemas.VideoOutput{Type: schemas.VideoOutputTypeContent, ContentType: "video/mp4"}, resp.Videos[0])
	require.NotNil(t, resp.Usage)
	assert.Equal(t, schemas.VideoUsage{OutputTokens: schemas.Ptr(46336)}, *resp.Usage)
	require.NotNil(t, resp.CompletedAt)
	require.NotNil(t, resp.ExpiresAt)
	assert.Equal(t, *resp.CompletedAt+48*3600, *resp.ExpiresAt)

	// Inline data: the same "content" entry; the bytes are not copied into the task object.
	// The Gemini API keeps an interaction for a tier-dependent time, so no expiry is reported.
	inlineBody := strings.Replace(uriBody, `"uri": "https://generativelanguage.googleapis.com/v1beta/files/abc123:download?alt=media"`, `"data": "AAAAIGZ0eXBpc29t"`, 1)
	resp, bifrostErr = ToBifrostInteractionVideoResponse(decodeInteraction(t, inlineBody), GeminiInteractionVideoOptions)
	require.Nil(t, bifrostErr)
	require.Len(t, resp.Videos, 1)
	assert.Equal(t, schemas.VideoOutput{Type: schemas.VideoOutputTypeContent, ContentType: "video/mp4"}, resp.Videos[0])
	assert.Nil(t, resp.ExpiresAt)

	// A vendor with a fixed retention (Vertex, 7 days) reports creation plus that retention.
	resp, bifrostErr = ToBifrostInteractionVideoResponse(decodeInteraction(t, inlineBody), InteractionVideoOptions{StoredRetention: 7 * 24 * time.Hour})
	require.Nil(t, bifrostErr)
	require.NotNil(t, resp.ExpiresAt)
	assert.Equal(t, resp.CreatedAt+7*24*3600, *resp.ExpiresAt)

	// Usage the vendor did not break down by a video modality fills nothing.
	resp, bifrostErr = ToBifrostInteractionVideoResponse(decodeInteraction(t,
		`{"id":"i","status":"completed","usage":{"total_output_tokens":500,"output_tokens_by_modality":[{"modality":"text","tokens":500}]}}`), GeminiInteractionVideoOptions)
	require.Nil(t, bifrostErr)
	assert.Nil(t, resp.Usage)
	assert.Empty(t, resp.Videos)

	// A failed interaction carries the vendor's first error verbatim and no content_filter:
	// how a filtered interaction looks is not documented, so it is not inferred.
	resp, bifrostErr = ToBifrostInteractionVideoResponse(decodeInteraction(t,
		`{"id":"i","status":"failed","updated":"2026-11-10T12:25:15Z","errors":[{"code":"https://example.invalid/errors/x","message":"vendor text"},{"code":"second","message":"ignored"}]}`), GeminiInteractionVideoOptions)
	require.Nil(t, bifrostErr)
	assert.Equal(t, schemas.VideoStatusFailed, resp.Status)
	require.NotNil(t, resp.Error)
	assert.Equal(t, schemas.VideoCreateError{Code: "https://example.invalid/errors/x", Message: "vendor text"}, *resp.Error)
	assert.Nil(t, resp.ContentFilter)
}

func TestGeminiFileDownloadURL(t *testing.T) {
	const base = "https://generativelanguage.googleapis.com/v1beta"
	for uri, want := range map[string]string{
		"https://generativelanguage.googleapis.com/v1beta/files/abc123:download?alt=media": "https://generativelanguage.googleapis.com/v1beta/files/abc123:download?alt=media",
		"https://generativelanguage.googleapis.com/v1beta/files/abc123":                    "https://generativelanguage.googleapis.com/v1beta/files/abc123:download?alt=media",
		"files/abc123": "https://generativelanguage.googleapis.com/v1beta/files/abc123:download?alt=media",
	} {
		got, bifrostErr := geminiFileDownloadURL(base, uri)
		require.Nil(t, bifrostErr, uri)
		assert.Equal(t, want, got, uri)
	}
	// The key is never sent to another host, over another scheme, or to a non-file path.
	for _, uri := range []string{
		"https://attacker.example/v1beta/files/abc123:download?alt=media",
		"http://generativelanguage.googleapis.com/v1beta/files/abc123",
		"https://generativelanguage.googleapis.com.attacker.example/v1beta/files/abc123",
		"https://generativelanguage.googleapis.com/v1beta/models/x",
		"gs://bucket/video.mp4",
		"",
	} {
		got, bifrostErr := geminiFileDownloadURL(base, uri)
		require.NotNil(t, bifrostErr, "%q resolved to %q", uri, got)
	}
}

// omniRecorder is a fake Gemini API that records every call it receives.
type omniRecorder struct {
	mu     sync.Mutex
	calls  []string
	bodies []string
	keys   []string
}

func (r *omniRecorder) record(req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, req.Method+" "+req.URL.RequestURI())
	r.bodies = append(r.bodies, string(body))
	r.keys = append(r.keys, req.Header.Get("x-goog-api-key"))
}

func (r *omniRecorder) snapshot() ([]string, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.calls), slices.Clone(r.bodies)
}

func newOmniTestProvider(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, serverURL string)) (*GeminiProvider, *omniRecorder) {
	t.Helper()
	recorder := &omniRecorder{}
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder.record(r)
		handler(w, r, server.URL)
	}))
	t.Cleanup(server.Close)
	provider := NewGeminiProvider(&schemas.ProviderConfig{
		NetworkConfig: schemas.NetworkConfig{BaseURL: server.URL + "/v1beta"},
	}, testNoopLogger{})
	return provider, recorder
}

func omniTestContext() *schemas.BifrostContext {
	return schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
}

var omniTestKey = schemas.Key{Value: *schemas.NewSecretVar("gemini-test")}

const omniTaskID = "interactions%2F" + omniInteractionID + ":gemini"

// An Omni create goes to POST /interactions with the unified fields converted, and the task id
// it returns names the interaction kind.
func TestGeminiOmniVideoGeneration_CreatesBackgroundInteraction(t *testing.T) {
	provider, recorder := newOmniTestProvider(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(omniInteractionInProgress))
	})

	resp, bifrostErr := provider.VideoGeneration(omniTestContext(), omniTestKey, omniVideoRequest("gemini-omni-1.1-flash", nil, &schemas.VideoGenerationParameters{
		Seconds:     schemas.Ptr("8"),
		AspectRatio: "16:9",
		Resolution:  "720p",
		TaskControlParameters: schemas.TaskControlParameters{
			CallbackURL:       schemas.Ptr("https://hooks.tokun.sh/v/task-1"),
			ClientReferenceID: schemas.Ptr("task-1"),
		},
	}))
	require.Nil(t, bifrostErr)

	calls, bodies := recorder.snapshot()
	require.Equal(t, []string{"POST /v1beta/interactions"}, calls)
	assert.JSONEq(t, `{
		"model": "gemini-omni-1.1-flash",
		"input": [{"type": "text", "text": "a marble on a track"}],
		"response_format": {"type": "video", "delivery": "uri", "aspect_ratio": "16:9", "duration": "8s", "resolution": "720p"},
		"background": true,
		"webhook_config": {"uris": ["https://hooks.tokun.sh/v/task-1"], "user_metadata": {"client_reference_id": "task-1"}}
	}`, bodies[0])
	assert.Equal(t, []string{"gemini-test"}, recorder.keys)

	assert.Equal(t, omniTaskID, resp.ID)
	assert.Equal(t, schemas.VideoStatusInProgress, resp.Status)
	assert.Equal(t, "in_progress", resp.ProviderStatus)
	assert.Equal(t, "gemini-omni-1.1-flash", resp.Model)
	assert.Empty(t, resp.Videos)
}

// A refused field makes no upstream call, on either video API of the provider.
func TestGeminiVideoGeneration_RefusesUndeliverableFieldsWithoutUpstreamCall(t *testing.T) {
	provider, recorder := newOmniTestProvider(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(omniInteractionInProgress))
	})

	_, bifrostErr := provider.VideoGeneration(omniTestContext(), omniTestKey, omniVideoRequest("gemini-omni-1.1-flash", nil,
		&schemas.VideoGenerationParameters{Size: "1280x720"}))
	requireUnsupportedParameterError(t, bifrostErr, "size")

	// predictLongRunning (Veo) has no webhook field: the control fields the provider declares
	// for the Interactions API are refused on that path, not dropped.
	_, bifrostErr = provider.VideoGeneration(omniTestContext(), omniTestKey, omniVideoRequest("veo-3.1-generate-001", nil,
		&schemas.VideoGenerationParameters{TaskControlParameters: schemas.TaskControlParameters{CallbackURL: schemas.Ptr("https://hooks.tokun.sh/v/task-1")}}))
	requireUnsupportedParameterError(t, bifrostErr, "callback_url")
	_, bifrostErr = provider.VideoGeneration(omniTestContext(), omniTestKey, omniVideoRequest("veo-3.1-generate-001", nil,
		&schemas.VideoGenerationParameters{TaskControlParameters: schemas.TaskControlParameters{ClientReferenceID: schemas.Ptr("task-1")}}))
	requireUnsupportedParameterError(t, bifrostErr, "client_reference_id")

	calls, _ := recorder.snapshot()
	assert.Empty(t, calls)
}

// The task id decides the API: an interaction id is queried on /interactions, a Veo operation
// name on /operations, and a Veo create still goes to predictLongRunning.
func TestGeminiVideo_TaskIDKindReachesItsOwnAPI(t *testing.T) {
	provider, recorder := newOmniTestProvider(t, func(w http.ResponseWriter, r *http.Request, serverURL string) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1beta/interactions/"):
			_, _ = w.Write([]byte(strings.Replace(omniInteractionCompletedURITemplate, "FILE_URI", serverURL+"/v1beta/files/abc123:download?alt=media", 1)))
		default:
			_, _ = w.Write([]byte(`{"name":"models/veo-3.1-generate-001/operations/op-1","done":false}`))
		}
	})

	omni, bifrostErr := provider.VideoRetrieve(omniTestContext(), omniTestKey, &schemas.BifrostVideoRetrieveRequest{Provider: schemas.Gemini, ID: omniTaskID})
	require.Nil(t, bifrostErr)
	assert.Equal(t, omniTaskID, omni.ID)
	assert.Equal(t, schemas.VideoStatusCompleted, omni.Status)
	assert.Equal(t, "completed", omni.ProviderStatus)
	require.NotNil(t, omni.Usage)
	assert.Equal(t, schemas.Ptr(46336), omni.Usage.OutputTokens)
	require.Len(t, omni.Videos, 1)
	assert.Equal(t, schemas.VideoOutputTypeContent, omni.Videos[0].Type)
	assert.Nil(t, omni.Videos[0].URL)

	veoID := providerUtils.AddVideoIDProviderSuffix("models/veo-3.1-generate-001/operations/op-1", schemas.Gemini)
	veo, bifrostErr := provider.VideoRetrieve(omniTestContext(), omniTestKey, &schemas.BifrostVideoRetrieveRequest{Provider: schemas.Gemini, ID: veoID})
	require.Nil(t, bifrostErr)
	assert.Equal(t, veoID, veo.ID)

	_, bifrostErr = provider.VideoGeneration(omniTestContext(), omniTestKey, omniVideoRequest("veo-3.1-generate-001", nil, nil))
	require.Nil(t, bifrostErr)

	calls, _ := recorder.snapshot()
	assert.Equal(t, []string{
		"GET /v1beta/interactions/" + omniInteractionID,
		"GET /v1beta/models/veo-3.1-generate-001/operations/op-1",
		"POST /v1beta/models/veo-3.1-generate-001:predictLongRunning",
	}, calls)

	// An id that only looks like an interaction path cannot leave /interactions/{id}.
	for _, id := range []string{"interactions%2F..%2Ffiles%2Fx:gemini", "interactions%2Fa%2Fcancel:gemini", "interactions%2Fa%3Fx=1:gemini"} {
		_, bifrostErr := provider.VideoRetrieve(omniTestContext(), omniTestKey, &schemas.BifrostVideoRetrieveRequest{Provider: schemas.Gemini, ID: id})
		require.NotNil(t, bifrostErr, id)
	}
	after, _ := recorder.snapshot()
	assert.Len(t, after, len(calls), "a malformed interaction id made an upstream call")
}

// Cancel queries once, calls the vendor cancel only for a running interaction, and refuses
// every other state with provider_cancel_unsupported. DELETE is never called.
func TestGeminiOmniVideoCancel(t *testing.T) {
	for _, tc := range []struct {
		status      string
		wantCancel  bool
		interaction string
	}{
		{"in_progress", true, omniInteractionInProgress},
		{"completed", false, `{"id":"` + omniInteractionID + `","status":"completed"}`},
		{"failed", false, `{"id":"` + omniInteractionID + `","status":"failed"}`},
		{"cancelled", false, omniInteractionCancelled},
		{"requires_action", false, `{"id":"` + omniInteractionID + `","status":"requires_action"}`},
		{"incomplete", false, `{"id":"` + omniInteractionID + `","status":"incomplete"}`},
		{"budget_exceeded", false, `{"id":"` + omniInteractionID + `","status":"budget_exceeded"}`},
		{"requires_continuation", false, `{"id":"` + omniInteractionID + `","status":"requires_continuation"}`},
	} {
		t.Run(tc.status, func(t *testing.T) {
			provider, recorder := newOmniTestProvider(t, func(w http.ResponseWriter, r *http.Request, _ string) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodPost {
					_, _ = w.Write([]byte(omniInteractionCancelled))
					return
				}
				_, _ = w.Write([]byte(tc.interaction))
			})
			ctx := omniTestContext()
			ctx.SetValue(schemas.BifrostContextKeySendBackRawResponse, true)

			resp, bifrostErr := provider.VideoCancel(ctx, omniTestKey, &schemas.BifrostVideoCancelRequest{Provider: schemas.Gemini, ID: omniTaskID})
			calls, _ := recorder.snapshot()
			if tc.wantCancel {
				require.Nil(t, bifrostErr)
				assert.Equal(t, []string{
					"GET /v1beta/interactions/" + omniInteractionID,
					"POST /v1beta/interactions/" + omniInteractionID + "/cancel",
				}, calls)
				assert.Equal(t, omniTaskID, resp.ID)
				assert.Equal(t, schemas.VideoStatusCancelled, resp.Status)
				assert.Equal(t, "cancelled", resp.ProviderStatus)
				return
			}
			require.NotNil(t, bifrostErr)
			require.NotNil(t, bifrostErr.StatusCode)
			assert.Equal(t, http.StatusBadRequest, *bifrostErr.StatusCode)
			assert.Equal(t, schemas.ErrorTypeProviderCancelUnsupported, bifrostErr.ExtraFields.ErrorType)
			assert.Contains(t, bifrostErr.Error.Message, tc.status)
			assert.Equal(t, []string{"GET /v1beta/interactions/" + omniInteractionID}, calls)
		})
	}

	// A Veo operation has no cancel API: refused without any call.
	provider, recorder := newOmniTestProvider(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	veoID := providerUtils.AddVideoIDProviderSuffix("models/veo-3.1-generate-001/operations/op-1", schemas.Gemini)
	_, bifrostErr := provider.VideoCancel(omniTestContext(), omniTestKey, &schemas.BifrostVideoCancelRequest{Provider: schemas.Gemini, ID: veoID})
	require.NotNil(t, bifrostErr)
	assert.Equal(t, schemas.ErrorTypeProviderCancelUnsupported, bifrostErr.ExtraFields.ErrorType)
	calls, _ := recorder.snapshot()
	assert.Empty(t, calls)

	// A vendor error on the query is passed through with its status, not turned into a refusal.
	provider, _ = newOmniTestProvider(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":404,"message":"interaction not found","status":"NOT_FOUND"}}`))
	})
	_, bifrostErr = provider.VideoCancel(omniTestContext(), omniTestKey, &schemas.BifrostVideoCancelRequest{Provider: schemas.Gemini, ID: omniTaskID})
	require.NotNil(t, bifrostErr)
	require.NotNil(t, bifrostErr.StatusCode)
	assert.Equal(t, http.StatusNotFound, *bifrostErr.StatusCode)
	assert.NotEqual(t, schemas.ErrorTypeProviderCancelUnsupported, bifrostErr.ExtraFields.ErrorType)
}

// The provider implements the cancel interface core dispatches POST /v1/videos/{id}/cancel to.
func TestGeminiProviderImplementsVideoCanceller(t *testing.T) {
	var provider any = NewGeminiProvider(&schemas.ProviderConfig{}, testNoopLogger{})
	_, ok := provider.(schemas.VideoCanceller)
	require.True(t, ok)
	assert.Nil(t, schemas.CheckVideoCancelSupported(provider, &schemas.BifrostRequest{RequestType: schemas.VideoCancelRequest}))
}

// /content returns the bytes of a delivery=uri artifact, fetched from Gemini Files with the
// key, and the inline bytes of an interaction that carries them.
func TestGeminiOmniVideoDownload(t *testing.T) {
	video := []byte("\x00\x00\x00 ftypisom-not-a-real-video")

	t.Run("uri", func(t *testing.T) {
		provider, recorder := newOmniTestProvider(t, func(w http.ResponseWriter, r *http.Request, serverURL string) {
			if strings.HasPrefix(r.URL.Path, "/v1beta/files/") {
				w.Header().Set("Content-Type", "video/mp4")
				_, _ = w.Write(video)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(strings.Replace(omniInteractionCompletedURITemplate, "FILE_URI", serverURL+"/v1beta/files/abc123:download?alt=media", 1)))
		})
		resp, bifrostErr := provider.VideoDownload(omniTestContext(), omniTestKey, &schemas.BifrostVideoDownloadRequest{Provider: schemas.Gemini, ID: omniTaskID})
		require.Nil(t, bifrostErr)
		assert.Equal(t, video, resp.Content)
		assert.Equal(t, "video/mp4", resp.ContentType)
		assert.Equal(t, omniTaskID, resp.VideoID)
		calls, _ := recorder.snapshot()
		assert.Equal(t, []string{
			"GET /v1beta/interactions/" + omniInteractionID,
			"GET /v1beta/files/abc123:download?alt=media",
		}, calls)
		assert.Equal(t, []string{"gemini-test", "gemini-test"}, recorder.keys)
	})

	t.Run("inline", func(t *testing.T) {
		provider, recorder := newOmniTestProvider(t, func(w http.ResponseWriter, r *http.Request, _ string) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"` + omniInteractionID + `","status":"completed","steps":[{"type":"model_output","content":[{"type":"video","mime_type":"video/webm","data":"` + base64.StdEncoding.EncodeToString(video) + `"}]}]}`))
		})
		resp, bifrostErr := provider.VideoDownload(omniTestContext(), omniTestKey, &schemas.BifrostVideoDownloadRequest{Provider: schemas.Gemini, ID: omniTaskID})
		require.Nil(t, bifrostErr)
		assert.Equal(t, video, resp.Content)
		assert.Equal(t, "video/webm", resp.ContentType)
		calls, _ := recorder.snapshot()
		assert.Equal(t, []string{"GET /v1beta/interactions/" + omniInteractionID}, calls)
	})

	// Gemini Files may answer a download with a redirect to a storage host: the bytes are
	// fetched there, without the key.
	t.Run("uri redirected off the provider host", func(t *testing.T) {
		var storageKeys []string
		var storageMu sync.Mutex
		storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			storageMu.Lock()
			storageKeys = append(storageKeys, r.Header.Get("x-goog-api-key")+"|"+r.Header.Get("Authorization"))
			storageMu.Unlock()
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write(video)
		}))
		defer storage.Close()
		provider, recorder := newOmniTestProvider(t, func(w http.ResponseWriter, r *http.Request, serverURL string) {
			switch {
			case r.URL.Path == "/v1beta/files/abc123:download":
				http.Redirect(w, r, "/v1beta/files/abc123:hop", http.StatusFound)
			case r.URL.Path == "/v1beta/files/abc123:hop":
				http.Redirect(w, r, storage.URL+"/signed/abc123.mp4?sig=1", http.StatusFound)
			default:
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(strings.Replace(omniInteractionCompletedURITemplate, "FILE_URI", serverURL+"/v1beta/files/abc123:download?alt=media", 1)))
			}
		})
		resp, bifrostErr := provider.VideoDownload(omniTestContext(), omniTestKey, &schemas.BifrostVideoDownloadRequest{Provider: schemas.Gemini, ID: omniTaskID})
		require.Nil(t, bifrostErr)
		assert.Equal(t, video, resp.Content)
		assert.Equal(t, "video/mp4", resp.ContentType)
		// The two hops on the provider's own host carry the key; the storage host gets none.
		assert.Equal(t, []string{"gemini-test", "gemini-test", "gemini-test"}, recorder.keys)
		storageMu.Lock()
		defer storageMu.Unlock()
		assert.Equal(t, []string{"|"}, storageKeys)
	})

	t.Run("a redirect loop ends in an error", func(t *testing.T) {
		provider, recorder := newOmniTestProvider(t, func(w http.ResponseWriter, r *http.Request, serverURL string) {
			if strings.HasPrefix(r.URL.Path, "/v1beta/files/") {
				http.Redirect(w, r, r.URL.RequestURI(), http.StatusFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(strings.Replace(omniInteractionCompletedURITemplate, "FILE_URI", serverURL+"/v1beta/files/abc123:download?alt=media", 1)))
		})
		_, bifrostErr := provider.VideoDownload(omniTestContext(), omniTestKey, &schemas.BifrostVideoDownloadRequest{Provider: schemas.Gemini, ID: omniTaskID})
		require.NotNil(t, bifrostErr)
		calls, _ := recorder.snapshot()
		assert.Len(t, calls, 2+geminiFileMaxRedirects, "the query, the download and %d redirects", geminiFileMaxRedirects)
	})

	t.Run("uri on another host is not fetched", func(t *testing.T) {
		provider, recorder := newOmniTestProvider(t, func(w http.ResponseWriter, r *http.Request, _ string) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(strings.Replace(omniInteractionCompletedURITemplate, "FILE_URI", "https://attacker.example/v1beta/files/abc123:download?alt=media", 1)))
		})
		_, bifrostErr := provider.VideoDownload(omniTestContext(), omniTestKey, &schemas.BifrostVideoDownloadRequest{Provider: schemas.Gemini, ID: omniTaskID})
		require.NotNil(t, bifrostErr)
		calls, _ := recorder.snapshot()
		assert.Equal(t, []string{"GET /v1beta/interactions/" + omniInteractionID}, calls)
	})

	t.Run("not completed", func(t *testing.T) {
		provider, _ := newOmniTestProvider(t, func(w http.ResponseWriter, r *http.Request, _ string) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(omniInteractionInProgress))
		})
		_, bifrostErr := provider.VideoDownload(omniTestContext(), omniTestKey, &schemas.BifrostVideoDownloadRequest{Provider: schemas.Gemini, ID: omniTaskID})
		require.NotNil(t, bifrostErr)
		assert.Contains(t, bifrostErr.Error.Message, "in_progress")
	})
}
