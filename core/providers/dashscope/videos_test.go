package dashscope

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/capsohq/bifrost/core/network/proxytest"
	schemas "github.com/capsohq/bifrost/core/schemas"
)

// recordedRequest is one request the mock DashScope received.
type recordedRequest struct {
	Method string
	Path   string
	Query  string
	Async  string
	Auth   string
	Body   string
}

// mockDashScope serves canned DashScope responses by "METHOD /path" and records every
// request, so a test can assert both the wire shape and the number of upstream calls.
type mockDashScope struct {
	mu       sync.Mutex
	requests []recordedRequest
}

func (m *mockDashScope) recorded() []recordedRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]recordedRequest(nil), m.requests...)
}

type mockResponse struct {
	status int
	body   string
}

func newMockDashScope(t *testing.T, routes map[string]mockResponse) (*DashScopeProvider, *mockDashScope) {
	t.Helper()
	mock := &mockDashScope{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mock.mu.Lock()
		mock.requests = append(mock.requests, recordedRequest{
			Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery,
			Async: r.Header.Get("X-DashScope-Async"), Auth: r.Header.Get("Authorization"), Body: string(body),
		})
		mock.mu.Unlock()
		route, ok := routes[r.Method+" "+r.URL.Path]
		if !ok {
			http.Error(w, `{"code":"NotFound","message":"no such route"}`, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(route.status)
		_, _ = io.WriteString(w, route.body)
	}))
	t.Cleanup(server.Close)
	// The regional base points nowhere: a video call that used it would fail to connect, so
	// every passing test also proves the workspace base served the call.
	provider := newTestProvider(t, schemas.NetworkConfig{
		BaseURL: "http://127.0.0.1:1", WorkspaceBaseURL: server.URL,
		DefaultRequestTimeoutInSeconds: 10, AllowPrivateNetwork: true,
	})
	return provider, mock
}

func videoTestContext(t *testing.T) *schemas.BifrostContext {
	t.Helper()
	ctx, cancel := schemas.NewBifrostContextWithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func videoTestKey() schemas.Key {
	return schemas.Key{Value: *schemas.NewSecretVar("sk-dash"), Models: []string{"*"}}
}

const createdTaskBody = `{"output":{"task_status":"PENDING","task_id":"0385dc79-5ff8-4d82-bcb6-000000000001"},"request_id":"4909100c-7b5a-9f92-bfe5-000000000001"}`

// assertJSONEqual compares two JSON documents by value.
func assertJSONEqual(t *testing.T, got, want string) {
	t.Helper()
	var gotValue, wantValue any
	if err := json.Unmarshal([]byte(got), &gotValue); err != nil {
		t.Fatalf("request body is not JSON: %v\n%s", err, got)
	}
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatalf("expected body is not JSON: %v\n%s", err, want)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Errorf("request body\n got: %s\nwant: %s", got, want)
	}
}

// TestDashScopeVideoCreateReachesEachFamilyInItsShape pins the body each Wan family receives,
// against the request shapes DashScope documents for it, and that every create carries
// X-DashScope-Async: enable on the workspace base.
func TestDashScopeVideoCreateReachesEachFamilyInItsShape(t *testing.T) {
	const firstFrame = "https://cdn.example.com/first.png"
	const lastFrame = "https://cdn.example.com/last.png"
	cases := []struct {
		name        string
		model       string
		input       schemas.VideoGenerationInput
		params      *schemas.VideoGenerationParameters
		passthrough bool
		want        string
	}{
		{
			name:  "wan3.0 text-to-video takes resolution and ratio",
			model: "wan3.0-video",
			input: schemas.VideoGenerationInput{Prompt: "A kitten on a rooftop"},
			params: &schemas.VideoGenerationParameters{
				Seconds: schemas.Ptr("5"), Resolution: "720p", AspectRatio: "16:9",
				Audio: schemas.Ptr(false), Seed: schemas.Ptr(7), NegativePrompt: schemas.Ptr("blurry"),
				ExtraParams: map[string]any{"resolution": "720p", "aspect_ratio": "16:9", "watermark": true, "prompt_extend": false},
			},
			want: `{"model":"wan3.0-video","input":{"prompt":"A kitten on a rooftop","negative_prompt":"blurry"},"parameters":{"resolution":"720P","ratio":"16:9","duration":5,"audio":false,"seed":7,"watermark":true,"prompt_extend":false}}`,
		},
		{
			name:  "wan3.0 first and last frame go to input.media",
			model: "wan3.0-video-prime",
			input: schemas.VideoGenerationInput{Prompt: "The cat looks up", InputReference: schemas.Ptr(firstFrame)},
			params: &schemas.VideoGenerationParameters{
				LastFrame:   schemas.TolerantString(lastFrame),
				ExtraParams: map[string]any{"last_frame": lastFrame},
			},
			want: `{"model":"wan3.0-video-prime","input":{"prompt":"The cat looks up","media":[{"type":"first_frame","url":"` + firstFrame + `"},{"type":"last_frame","url":"` + lastFrame + `"}]}}`,
		},
		{
			name:  "wan3.0 reference images, videos and audios go to input.media in order",
			model: "wan3.0-video",
			input: schemas.VideoGenerationInput{Prompt: "Image 1 dances to Audio 1 as in Video 1"},
			params: &schemas.VideoGenerationParameters{
				ReferenceImages: schemas.VideoReferenceImages{"https://cdn.example.com/a.png", "https://cdn.example.com/b.png"},
				ExtraParams: map[string]any{
					"reference_images": []any{"https://cdn.example.com/a.png", "https://cdn.example.com/b.png"},
					"reference_videos": []any{"https://cdn.example.com/v.mp4"},
					"reference_audios": []any{"https://cdn.example.com/s.mp3"},
				},
			},
			want: `{"model":"wan3.0-video","input":{"prompt":"Image 1 dances to Audio 1 as in Video 1","media":[{"type":"reference_image","url":"https://cdn.example.com/a.png"},{"type":"reference_image","url":"https://cdn.example.com/b.png"},{"type":"reference_video","url":"https://cdn.example.com/v.mp4"},{"type":"reference_audio","url":"https://cdn.example.com/s.mp3"}]}}`,
		},
		{
			name:   "wan2.7 text-to-video takes resolution and ratio",
			model:  "wan2.7-t2v",
			input:  schemas.VideoGenerationInput{Prompt: "A paper boat"},
			params: &schemas.VideoGenerationParameters{Seconds: schemas.Ptr("10"), Resolution: "1080P", AspectRatio: "9:16"},
			want:   `{"model":"wan2.7-t2v","input":{"prompt":"A paper boat"},"parameters":{"resolution":"1080P","ratio":"9:16","duration":10}}`,
		},
		{
			name:   "wan2.7 image-to-video sends the frames as input.media",
			model:  "wan2.7-i2v-2026-04-25",
			input:  schemas.VideoGenerationInput{Prompt: "The cat looks up", InputReference: schemas.Ptr(firstFrame)},
			params: &schemas.VideoGenerationParameters{Resolution: "720P", LastFrame: schemas.TolerantString(lastFrame)},
			want:   `{"model":"wan2.7-i2v-2026-04-25","input":{"prompt":"The cat looks up","media":[{"type":"first_frame","url":"` + firstFrame + `"},{"type":"last_frame","url":"` + lastFrame + `"}]},"parameters":{"resolution":"720P"}}`,
		},
		{
			name:   "wan2.6 text-to-video takes parameters.size",
			model:  "wan2.6-t2v",
			input:  schemas.VideoGenerationInput{Prompt: "A paper boat"},
			params: &schemas.VideoGenerationParameters{Seconds: schemas.Ptr("5"), Size: "1280x720"},
			want:   `{"model":"wan2.6-t2v","input":{"prompt":"A paper boat"},"parameters":{"size":"1280*720","duration":5}}`,
		},
		{
			name:   "wan2.6 image-to-video takes img_url and resolution",
			model:  "wan2.6-i2v-flash",
			input:  schemas.VideoGenerationInput{Prompt: "A cat running", InputReference: schemas.Ptr(firstFrame)},
			params: &schemas.VideoGenerationParameters{Resolution: "720P", Audio: schemas.Ptr(true)},
			want:   `{"model":"wan2.6-i2v-flash","input":{"prompt":"A cat running","img_url":"` + firstFrame + `"},"parameters":{"resolution":"720P","audio":true}}`,
		},
		{
			name:  "other extra fields are merged into the body, objects recursively",
			model: "wan2.6-t2v",
			input: schemas.VideoGenerationInput{Prompt: "A paper boat"},
			params: &schemas.VideoGenerationParameters{
				Size:        "1920x1080",
				ExtraParams: map[string]any{"parameters": map[string]any{"shot_type": "multi"}, "input": map[string]any{"audio_url": "https://cdn.example.com/s.mp3"}},
			},
			passthrough: true,
			want:        `{"model":"wan2.6-t2v","input":{"prompt":"A paper boat","audio_url":"https://cdn.example.com/s.mp3"},"parameters":{"size":"1920*1080","shot_type":"multi"}}`,
		},
		{
			name:   "no parameters object when the caller sets none",
			model:  "wan2.2-t2v-plus",
			input:  schemas.VideoGenerationInput{Prompt: "A paper boat"},
			params: nil,
			want:   `{"model":"wan2.2-t2v-plus","input":{"prompt":"A paper boat"}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider, mock := newMockDashScope(t, map[string]mockResponse{
				"POST " + dashScopeVideoSynthesisPath: {http.StatusOK, createdTaskBody},
			})
			ctx := videoTestContext(t)
			if tc.passthrough {
				ctx.SetValue(schemas.BifrostContextKeyPassthroughExtraParams, true)
			}
			input := tc.input
			resp, bifrostErr := provider.VideoGeneration(ctx, videoTestKey(), &schemas.BifrostVideoGenerationRequest{
				Provider: schemas.DashScope, Model: tc.model, Input: &input, Params: tc.params,
			})
			if bifrostErr != nil {
				t.Fatalf("VideoGeneration: %+v", bifrostErr.Error)
			}
			requests := mock.recorded()
			if len(requests) != 1 {
				t.Fatalf("upstream calls = %d, want exactly 1", len(requests))
			}
			got := requests[0]
			if got.Method != http.MethodPost || got.Path != dashScopeVideoSynthesisPath {
				t.Errorf("request = %s %s, want POST %s", got.Method, got.Path, dashScopeVideoSynthesisPath)
			}
			if got.Async != "enable" {
				t.Errorf("X-DashScope-Async = %q, want enable", got.Async)
			}
			if got.Auth != "Bearer sk-dash" {
				t.Errorf("Authorization = %q, want the key as a bearer token", got.Auth)
			}
			assertJSONEqual(t, got.Body, tc.want)

			if resp.ID != "0385dc79-5ff8-4d82-bcb6-000000000001:dashscope" {
				t.Errorf("id = %q, want the vendor task id with the provider suffix", resp.ID)
			}
			if resp.Status != schemas.VideoStatusQueued || resp.ProviderStatus != "PENDING" {
				t.Errorf("status = %q / provider_status = %q, want queued / PENDING", resp.Status, resp.ProviderStatus)
			}
			if resp.Object != "video" || resp.Model != tc.model || resp.CreatedAt == 0 {
				t.Errorf("object = %q, model = %q, created_at = %d", resp.Object, resp.Model, resp.CreatedAt)
			}
		})
	}
}

// TestDashScopeVideoCreateRefusesUndeliverableFields pins contract C4 agreement three: a field
// the model's family cannot take is a 400 caller_unsupported_parameter naming the field, and
// DashScope is never called.
func TestDashScopeVideoCreateRefusesUndeliverableFields(t *testing.T) {
	const frame = "https://cdn.example.com/frame.png"
	cases := []struct {
		name      string
		model     string
		input     schemas.VideoGenerationInput
		params    *schemas.VideoGenerationParameters
		wantType  schemas.ErrorType
		wantParam string
	}{
		{"wan2.6 size outside its tiers", "wan2.6-t2v", schemas.VideoGenerationInput{Prompt: "p"}, &schemas.VideoGenerationParameters{Size: "1024x1024"}, schemas.ErrorTypeCallerUnsupportedParameter, "size"},
		{"wan2.6 size that is not a size", "wan2.6-t2v", schemas.VideoGenerationInput{Prompt: "p"}, &schemas.VideoGenerationParameters{Size: "720p"}, schemas.ErrorTypeCallerUnsupportedParameter, "size"},
		{"wan2.6 text-to-video resolution", "wan2.6-t2v", schemas.VideoGenerationInput{Prompt: "p"}, &schemas.VideoGenerationParameters{Resolution: "720P"}, schemas.ErrorTypeCallerUnsupportedParameter, "resolution"},
		{"wan2.6 image-to-video size", "wan2.6-i2v", schemas.VideoGenerationInput{Prompt: "p", InputReference: schemas.Ptr(frame)}, &schemas.VideoGenerationParameters{Size: "1280x720"}, schemas.ErrorTypeCallerUnsupportedParameter, "size"},
		{"wan2.6 aspect_ratio", "wan2.6-t2v", schemas.VideoGenerationInput{Prompt: "p"}, &schemas.VideoGenerationParameters{AspectRatio: "16:9"}, schemas.ErrorTypeCallerUnsupportedParameter, "aspect_ratio"},
		{"wan2.6 last_frame", "wan2.6-i2v", schemas.VideoGenerationInput{Prompt: "p", InputReference: schemas.Ptr(frame)}, &schemas.VideoGenerationParameters{LastFrame: frame}, schemas.ErrorTypeCallerUnsupportedParameter, "last_frame"},
		{"wan2.6 reference_images", "wan2.6-t2v", schemas.VideoGenerationInput{Prompt: "p"}, &schemas.VideoGenerationParameters{ReferenceImages: schemas.VideoReferenceImages{frame}}, schemas.ErrorTypeCallerUnsupportedParameter, "reference_images"},
		{"wan2.7 size", "wan2.7-t2v", schemas.VideoGenerationInput{Prompt: "p"}, &schemas.VideoGenerationParameters{Size: "1280x720"}, schemas.ErrorTypeCallerUnsupportedParameter, "size"},
		{"wan2.7 reference_images", "wan2.7-i2v", schemas.VideoGenerationInput{Prompt: "p"}, &schemas.VideoGenerationParameters{ReferenceImages: schemas.VideoReferenceImages{frame}}, schemas.ErrorTypeCallerUnsupportedParameter, "reference_images"},
		{"wan2.7 image-to-video aspect_ratio", "wan2.7-i2v", schemas.VideoGenerationInput{Prompt: "p", InputReference: schemas.Ptr(frame)}, &schemas.VideoGenerationParameters{AspectRatio: "16:9"}, schemas.ErrorTypeCallerUnsupportedParameter, "aspect_ratio"},
		{"wan2.7 reference_videos extra", "wan2.7-t2v", schemas.VideoGenerationInput{Prompt: "p"}, &schemas.VideoGenerationParameters{ExtraParams: map[string]any{"reference_videos": []any{"https://cdn.example.com/v.mp4"}}}, schemas.ErrorTypeCallerUnsupportedParameter, "reference_videos"},
		{"wan3.0 size", "wan3.0-video", schemas.VideoGenerationInput{Prompt: "p"}, &schemas.VideoGenerationParameters{Size: "1280x720"}, schemas.ErrorTypeCallerUnsupportedParameter, "size"},
		{"fractional seconds are not rounded", "wan3.0-video", schemas.VideoGenerationInput{Prompt: "p"}, &schemas.VideoGenerationParameters{Seconds: schemas.Ptr("5.5")}, schemas.ErrorTypeCallerUnsupportedParameter, "seconds"},
		{"a resolution that is not a string", "wan3.0-video", schemas.VideoGenerationInput{Prompt: "p"}, &schemas.VideoGenerationParameters{ExtraParams: map[string]any{"resolution": 1024}}, schemas.ErrorTypeCallerUnsupportedParameter, "resolution"},
		{"video_uri", "wan3.0-video", schemas.VideoGenerationInput{Prompt: "p", VideoURI: schemas.Ptr("https://cdn.example.com/v.mp4")}, nil, schemas.ErrorTypeCallerUnsupportedParameter, "video_uri"},
		{"a model whose request fields are not known", "happyhorse-1.0", schemas.VideoGenerationInput{Prompt: "p"}, nil, schemas.ErrorTypeCallerModelUnknown, ""},
		{"a watermark that is not a boolean", "wan3.0-video", schemas.VideoGenerationInput{Prompt: "p"}, &schemas.VideoGenerationParameters{ExtraParams: map[string]any{"watermark": "yes"}}, schemas.ErrorTypeCallerInvalidRequest, ""},
		{"no prompt and no input media", "wan3.0-video", schemas.VideoGenerationInput{}, nil, schemas.ErrorTypeCallerInvalidRequest, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider, mock := newMockDashScope(t, map[string]mockResponse{
				"POST " + dashScopeVideoSynthesisPath: {http.StatusOK, createdTaskBody},
			})
			input := tc.input
			_, bifrostErr := provider.VideoGeneration(videoTestContext(t), videoTestKey(), &schemas.BifrostVideoGenerationRequest{
				Provider: schemas.DashScope, Model: tc.model, Input: &input, Params: tc.params,
			})
			if bifrostErr == nil {
				t.Fatal("expected a refusal")
			}
			if bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %v, want 400", bifrostErr.StatusCode)
			}
			if got := schemas.ClassifyErrorType(bifrostErr, schemas.VideoGenerationRequest); got != tc.wantType {
				t.Errorf("error_type = %q, want %q", got, tc.wantType)
			}
			gotParam, _ := bifrostErr.Error.Param.(string)
			if gotParam != tc.wantParam {
				t.Errorf("error.param = %v, want %q", bifrostErr.Error.Param, tc.wantParam)
			}
			if n := len(mock.recorded()); n != 0 {
				t.Errorf("a refused request made %d upstream calls", n)
			}
		})
	}
}

// TestDashScopeVideoCreateRefusesTaskControlFields pins that DashScope declares its video
// create fields and none of the four task control fields, so core's checks refuse each one,
// and a priced field DashScope has no parameter for, before a key is chosen (contract C7).
func TestDashScopeVideoCreateRefusesTaskControlFields(t *testing.T) {
	provider := newTestProvider(t, schemas.NetworkConfig{})
	if _, declared := provider.SupportedUnifiedFields(schemas.VideoGenerationRequest); !declared {
		t.Fatal("dashscope declares no unified fields for video generation")
	}
	if _, declared := provider.SupportedUnifiedFields(schemas.ImageGenerationRequest); declared {
		t.Error("dashscope declares unified fields for an operation it does not implement")
	}
	controls := map[string]schemas.TaskControlParameters{
		schemas.TaskControlFieldCallbackURL:         {CallbackURL: schemas.Ptr("https://hooks.example.com/v/1")},
		schemas.TaskControlFieldClientReferenceID:   {ClientReferenceID: schemas.Ptr("task-1")},
		schemas.TaskControlFieldExpiresAfterSeconds: {ExpiresAfterSeconds: schemas.Ptr(3600)},
		schemas.TaskControlFieldOutputStorageURI:    {OutputStorageURI: schemas.Ptr("oss://bucket/task-1")},
	}
	for field, control := range controls {
		t.Run(field, func(t *testing.T) {
			req := &schemas.BifrostRequest{RequestType: schemas.VideoGenerationRequest, VideoGenerationRequest: &schemas.BifrostVideoGenerationRequest{
				Provider: schemas.DashScope, Model: "wan3.0-video",
				Input:  &schemas.VideoGenerationInput{Prompt: "p"},
				Params: &schemas.VideoGenerationParameters{TaskControlParameters: control},
			}}
			bifrostErr := schemas.CheckTaskControlFields(provider, req)
			if bifrostErr == nil || bifrostErr.ExtraFields.ErrorType != schemas.ErrorTypeCallerUnsupportedParameter || bifrostErr.Error.Param != field {
				t.Fatalf("got %+v, want caller_unsupported_parameter naming %s", bifrostErr, field)
			}
		})
	}
	t.Run("output_format", func(t *testing.T) {
		req := &schemas.BifrostRequest{RequestType: schemas.VideoGenerationRequest, VideoGenerationRequest: &schemas.BifrostVideoGenerationRequest{
			Provider: schemas.DashScope, Model: "wan3.0-video",
			Input:  &schemas.VideoGenerationInput{Prompt: "p"},
			Params: &schemas.VideoGenerationParameters{OutputFormat: schemas.Ptr("webm")},
		}}
		bifrostErr := schemas.CheckDeclaredUnifiedFields(provider, req)
		if bifrostErr == nil || bifrostErr.Error.Param != "output_format" {
			t.Fatalf("got %+v, want caller_unsupported_parameter naming output_format", bifrostErr)
		}
	})
	t.Run("the delivered fields pass", func(t *testing.T) {
		req := &schemas.BifrostRequest{RequestType: schemas.VideoGenerationRequest, VideoGenerationRequest: &schemas.BifrostVideoGenerationRequest{
			Provider: schemas.DashScope, Model: "wan3.0-video",
			Input: &schemas.VideoGenerationInput{Prompt: "p"},
			Params: &schemas.VideoGenerationParameters{
				Seconds: schemas.Ptr("5"), Size: "1280x720", Audio: schemas.Ptr(true), Seed: schemas.Ptr(1), NegativePrompt: schemas.Ptr("n"),
				AspectRatio: "16:9", Resolution: "720P", LastFrame: "https://cdn.example.com/l.png",
				ReferenceImages: schemas.VideoReferenceImages{"https://cdn.example.com/a.png"},
			},
		}}
		if bifrostErr := schemas.CheckDeclaredUnifiedFields(provider, req); bifrostErr != nil {
			t.Fatalf("a delivered field was refused: %+v", bifrostErr.Error)
		}
	})
}

// TestDashScopeVideoRetrieveMapsTheTaskObject pins the task object built from DashScope's
// documented query responses: status one to one with provider_status, the two usage
// durations, the link with expires_at 24 hours after submission, and content_filter for the
// two moderation codes.
func TestDashScopeVideoRetrieveMapsTheTaskObject(t *testing.T) {
	const taskID = "eff1443c-ccab-4676-aad3-000000000002"
	const videoURL = "https://dashscope-result-sh.oss-accelerate.aliyuncs.com/1d/xx.mp4?Expires=1759213419&Signature=abc"
	// 2025-09-29 14:18:52 UTC+8 is 06:18:52 UTC.
	submitted := time.Date(2025, 9, 29, 6, 18, 52, 0, time.UTC).Unix()
	ended := time.Date(2025, 9, 29, 6, 23, 39, 0, time.UTC).Unix()

	t.Run("a succeeded task", func(t *testing.T) {
		provider, mock := newMockDashScope(t, map[string]mockResponse{
			"GET " + dashScopeTasksPath + "/" + taskID: {http.StatusOK, `{"request_id":"caa62a12-8841-41a6-8af2-000000000002","output":{"task_id":"` + taskID + `","task_status":"SUCCEEDED","submit_time":"2025-09-29 14:18:52.331","scheduled_time":"2025-09-29 14:18:59.290","end_time":"2025-09-29 14:23:39.407","orig_prompt":"A kitten on a rooftop","video_url":"` + videoURL + `"},"usage":{"duration":10,"input_video_duration":0,"output_video_duration":10,"video_count":1,"ratio":"16:9","SR":720}}`},
		})
		ctx := videoTestContext(t)
		ctx.SetValue(schemas.BifrostContextKeyCaptureRawResponse, true)
		resp, bifrostErr := provider.VideoRetrieve(ctx, videoTestKey(), &schemas.BifrostVideoRetrieveRequest{Provider: schemas.DashScope, ID: taskID + ":dashscope"})
		if bifrostErr != nil {
			t.Fatalf("VideoRetrieve: %+v", bifrostErr.Error)
		}
		requests := mock.recorded()
		if len(requests) != 1 || requests[0].Method != http.MethodGet || requests[0].Async != "" || requests[0].Auth != "Bearer sk-dash" {
			t.Fatalf("requests = %+v, want one keyed GET without X-DashScope-Async", requests)
		}
		if resp.ID != taskID+":dashscope" || resp.Object != "video" {
			t.Errorf("id = %q, object = %q", resp.ID, resp.Object)
		}
		if resp.Status != schemas.VideoStatusCompleted || resp.ProviderStatus != "SUCCEEDED" {
			t.Errorf("status = %q / provider_status = %q, want completed / SUCCEEDED", resp.Status, resp.ProviderStatus)
		}
		if resp.CreatedAt != submitted {
			t.Errorf("created_at = %d, want %d (submit_time read as UTC+8)", resp.CreatedAt, submitted)
		}
		if resp.CompletedAt == nil || *resp.CompletedAt != ended {
			t.Errorf("completed_at = %v, want %d", resp.CompletedAt, ended)
		}
		if resp.ExpiresAt == nil || *resp.ExpiresAt != submitted+24*60*60 {
			t.Errorf("expires_at = %v, want submission + 24 h = %d", resp.ExpiresAt, submitted+24*60*60)
		}
		if len(resp.Videos) != 1 || resp.Videos[0].Type != schemas.VideoOutputTypeURL || resp.Videos[0].URL == nil || *resp.Videos[0].URL != videoURL || resp.Videos[0].ContentType != "video/mp4" {
			t.Errorf("videos = %+v, want the vendor's link unchanged as a url item", resp.Videos)
		}
		if resp.Usage == nil || resp.Usage.OutputSeconds == nil || *resp.Usage.OutputSeconds != 10 || resp.Usage.InputSeconds == nil || *resp.Usage.InputSeconds != 0 {
			t.Errorf("usage = %+v, want output_seconds 10 and input_seconds 0", resp.Usage)
		}
		if resp.Seconds == nil || *resp.Seconds != "10" || resp.Prompt != "A kitten on a rooftop" {
			t.Errorf("seconds = %v, prompt = %q", resp.Seconds, resp.Prompt)
		}
		if resp.Error != nil || resp.ContentFilter != nil {
			t.Errorf("error = %+v, content_filter = %+v on a succeeded task", resp.Error, resp.ContentFilter)
		}
		if resp.ExtraFields.RawResponse == nil {
			t.Error("raw_response is empty though the caller asked for it")
		}
	})

	t.Run("a task whose usage carries no durations reports none", func(t *testing.T) {
		provider, _ := newMockDashScope(t, map[string]mockResponse{
			"GET " + dashScopeTasksPath + "/" + taskID: {http.StatusOK, `{"request_id":"r","output":{"task_id":"` + taskID + `","task_status":"SUCCEEDED","submit_time":"2025-09-29 14:18:52.331","end_time":"2025-09-29 14:23:39.407","video_url":"` + videoURL + `"},"usage":{"duration":5,"SR":480,"video_count":1}}`},
		})
		resp, bifrostErr := provider.VideoRetrieve(videoTestContext(t), videoTestKey(), &schemas.BifrostVideoRetrieveRequest{Provider: schemas.DashScope, ID: taskID})
		if bifrostErr != nil {
			t.Fatalf("VideoRetrieve: %+v", bifrostErr.Error)
		}
		if resp.Usage != nil || resp.Seconds != nil {
			t.Errorf("usage = %+v, seconds = %v; want neither filled from a quantity the vendor did not report", resp.Usage, resp.Seconds)
		}
	})

	states := []struct {
		output        string
		wantStatus    schemas.VideoStatus
		wantProvider  string
		wantCode      string
		wantFiltered  bool
		wantCompleted bool
	}{
		{`"task_status":"PENDING","submit_time":"2025-09-29 14:18:52.331"`, schemas.VideoStatusQueued, "PENDING", "", false, false},
		{`"task_status":"RUNNING","submit_time":"2025-09-29 14:18:52.331","scheduled_time":"2025-09-29 14:18:59.290"`, schemas.VideoStatusInProgress, "RUNNING", "", false, false},
		{`"task_status":"CANCELED","submit_time":"2025-09-29 14:18:52.331","end_time":"2025-09-29 14:23:39.407"`, schemas.VideoStatusCancelled, "CANCELED", "", false, true},
		{`"task_status":"UNKNOWN"`, schemas.VideoStatusExpired, "UNKNOWN", "", false, false},
		{`"task_status":"FAILED","submit_time":"2025-09-29 14:18:52.331","end_time":"2025-09-29 14:23:39.407","code":"InvalidParameter","message":"The size is not match"`, schemas.VideoStatusFailed, "FAILED", "InvalidParameter", false, true},
		{`"task_status":"FAILED","submit_time":"2025-09-29 14:18:52.331","end_time":"2025-09-29 14:23:39.407","code":"DataInspectionFailed","message":"Output data may contain inappropriate content."`, schemas.VideoStatusFailed, "FAILED", "DataInspectionFailed", true, true},
		{`"task_status":"FAILED","submit_time":"2025-09-29 14:18:52.331","end_time":"2025-09-29 14:23:39.407","code":"IPInfringementSuspect","message":"Input data is suspected of being involved in IP infringement."`, schemas.VideoStatusFailed, "FAILED", "IPInfringementSuspect", true, true},
		{`"task_status":"SUSPENDED"`, schemas.VideoStatusInProgress, "SUSPENDED", "", false, false},
	}
	for _, tc := range states {
		name := tc.wantProvider
		if tc.wantCode != "" {
			name += " " + tc.wantCode
		}
		t.Run(name, func(t *testing.T) {
			provider, _ := newMockDashScope(t, map[string]mockResponse{
				"GET " + dashScopeTasksPath + "/" + taskID: {http.StatusOK, `{"request_id":"r","output":{"task_id":"` + taskID + `",` + tc.output + `}}`},
			})
			resp, bifrostErr := provider.VideoRetrieve(videoTestContext(t), videoTestKey(), &schemas.BifrostVideoRetrieveRequest{Provider: schemas.DashScope, ID: taskID})
			if bifrostErr != nil {
				t.Fatalf("VideoRetrieve: %+v", bifrostErr.Error)
			}
			if resp.Status != tc.wantStatus || resp.ProviderStatus != tc.wantProvider {
				t.Errorf("status = %q / provider_status = %q, want %q / %q", resp.Status, resp.ProviderStatus, tc.wantStatus, tc.wantProvider)
			}
			if (resp.CompletedAt != nil) != tc.wantCompleted {
				t.Errorf("completed_at = %v, want set: %v", resp.CompletedAt, tc.wantCompleted)
			}
			if resp.ExpiresAt != nil || len(resp.Videos) != 0 {
				t.Errorf("expires_at = %v, videos = %+v on a task with no link", resp.ExpiresAt, resp.Videos)
			}
			if tc.wantCode == "" {
				if resp.Error != nil {
					t.Errorf("error = %+v, want none", resp.Error)
				}
			} else if resp.Error == nil || resp.Error.Code != tc.wantCode || resp.Error.Message == "" {
				t.Errorf("error = %+v, want the vendor's code %q and message", resp.Error, tc.wantCode)
			}
			if tc.wantFiltered {
				if resp.ContentFilter == nil || resp.ContentFilter.FilteredCount != 1 || !reflect.DeepEqual(resp.ContentFilter.Reasons, []string{tc.wantCode}) {
					t.Errorf("content_filter = %+v, want one filtered item with reason %q", resp.ContentFilter, tc.wantCode)
				}
			} else if resp.ContentFilter != nil {
				t.Errorf("content_filter = %+v, want none", resp.ContentFilter)
			}
		})
	}

	t.Run("a vendor error keeps its status and code", func(t *testing.T) {
		provider, _ := newMockDashScope(t, map[string]mockResponse{
			"GET " + dashScopeTasksPath + "/" + taskID: {http.StatusUnauthorized, `{"code":"InvalidApiKey","message":"Invalid API-key provided.","request_id":"r"}`},
		})
		_, bifrostErr := provider.VideoRetrieve(videoTestContext(t), videoTestKey(), &schemas.BifrostVideoRetrieveRequest{Provider: schemas.DashScope, ID: taskID})
		if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusUnauthorized || bifrostErr.Error.Code == nil || *bifrostErr.Error.Code != "InvalidApiKey" {
			t.Fatalf("got %+v, want the vendor's 401 InvalidApiKey", bifrostErr)
		}
		if got := schemas.ClassifyErrorType(bifrostErr, schemas.VideoRetrieveRequest); got != schemas.ErrorTypeProviderAuthFailed {
			t.Errorf("error_type = %q, want provider_auth_failed", got)
		}
	})

	t.Run("a task id that could leave the tasks path is refused", func(t *testing.T) {
		provider, mock := newMockDashScope(t, nil)
		_, bifrostErr := provider.VideoRetrieve(videoTestContext(t), videoTestKey(), &schemas.BifrostVideoRetrieveRequest{Provider: schemas.DashScope, ID: "../services"})
		if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusBadRequest {
			t.Fatalf("got %+v, want a 400", bifrostErr)
		}
		if n := len(mock.recorded()); n != 0 {
			t.Errorf("a refused id made %d upstream calls", n)
		}
	})
}

// TestDashScopeVideoCreateErrors pins the two ways a create fails at DashScope: an error
// status, and a 2xx that carries the error object instead of a task.
func TestDashScopeVideoCreateErrors(t *testing.T) {
	cases := []struct {
		name       string
		response   mockResponse
		wantStatus int
		wantCode   string
		wantType   schemas.ErrorType
	}{
		{"moderation refuses the prompt", mockResponse{http.StatusBadRequest, `{"code":"DataInspectionFailed","message":"Input data may contain inappropriate content.","request_id":"r"}`}, 400, "DataInspectionFailed", schemas.ErrorTypeCallerContentFiltered},
		{"moderation suspects infringement", mockResponse{http.StatusBadRequest, `{"code":"IPInfringementSuspect","message":"Input data is suspected of being involved in IP infringement.","request_id":"r"}`}, 400, "IPInfringementSuspect", schemas.ErrorTypeCallerContentFiltered},
		{"a rate limit", mockResponse{http.StatusTooManyRequests, `{"code":"Throttling.RateQuota","message":"Requests rate limit exceeded.","request_id":"r"}`}, 429, "Throttling.RateQuota", schemas.ErrorTypeProviderRateLimited},
		{"an error object inside a 200", mockResponse{http.StatusOK, `{"code":"InternalError","message":"internal error","request_id":"r"}`}, 502, "InternalError", schemas.ErrorTypeProviderServerError},
		{"a moderation error object inside a 200", mockResponse{http.StatusOK, `{"code":"DataInspectionFailed","message":"blocked","request_id":"r"}`}, 400, "DataInspectionFailed", schemas.ErrorTypeCallerContentFiltered},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider, mock := newMockDashScope(t, map[string]mockResponse{"POST " + dashScopeVideoSynthesisPath: tc.response})
			_, bifrostErr := provider.VideoGeneration(videoTestContext(t), videoTestKey(), &schemas.BifrostVideoGenerationRequest{
				Provider: schemas.DashScope, Model: "wan3.0-video", Input: &schemas.VideoGenerationInput{Prompt: "p"},
			})
			if bifrostErr == nil {
				t.Fatal("expected an error")
			}
			if bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != tc.wantStatus {
				t.Errorf("status = %v, want %d", bifrostErr.StatusCode, tc.wantStatus)
			}
			if bifrostErr.Error == nil || bifrostErr.Error.Code == nil || *bifrostErr.Error.Code != tc.wantCode {
				t.Errorf("error.code = %+v, want %q verbatim", bifrostErr.Error, tc.wantCode)
			}
			if got := schemas.ClassifyErrorType(bifrostErr, schemas.VideoGenerationRequest); got != tc.wantType {
				t.Errorf("error_type = %q, want %q", got, tc.wantType)
			}
			if n := len(mock.recorded()); n != 1 {
				t.Errorf("upstream calls = %d, want exactly 1 (no retry)", n)
			}
		})
	}
}

// TestDashScopeVideoCancel pins the cancel route: one POST to the vendor's cancel API, a
// cancelled task object for a PENDING task, and provider_cancel_unsupported for any other
// state, which DashScope reports as 400 UnsupportedOperation.
func TestDashScopeVideoCancel(t *testing.T) {
	const taskID = "0385dc79-5ff8-4d82-bcb6-000000000003"
	const cancelPath = dashScopeTasksPath + "/" + taskID + "/cancel"
	var _ schemas.VideoCanceller = (*DashScopeProvider)(nil)

	t.Run("a pending task is cancelled with one call", func(t *testing.T) {
		provider, mock := newMockDashScope(t, map[string]mockResponse{"POST " + cancelPath: {http.StatusOK, `{"request_id":"7574ee8f-38a3-4b1e-9280-000000000003"}`}})
		ctx := videoTestContext(t)
		ctx.SetValue(schemas.BifrostContextKeyCaptureRawResponse, true)
		resp, bifrostErr := provider.VideoCancel(ctx, videoTestKey(), &schemas.BifrostVideoCancelRequest{Provider: schemas.DashScope, ID: taskID + ":dashscope"})
		if bifrostErr != nil {
			t.Fatalf("VideoCancel: %+v", bifrostErr.Error)
		}
		requests := mock.recorded()
		if len(requests) != 1 || requests[0].Method != http.MethodPost || requests[0].Path != cancelPath || requests[0].Body != "" || requests[0].Async != "" {
			t.Fatalf("requests = %+v, want exactly one bodyless POST %s", requests, cancelPath)
		}
		if resp.ID != taskID+":dashscope" || resp.Object != "video" || resp.Status != schemas.VideoStatusCancelled {
			t.Errorf("task object = %+v, want the id with status cancelled", resp)
		}
		if resp.ProviderStatus != "" {
			t.Errorf("provider_status = %q, want none: the cancel answer reports no state", resp.ProviderStatus)
		}
		if resp.ExtraFields.RawResponse == nil {
			t.Error("raw_response is empty though the caller asked for it")
		}
	})

	t.Run("a task that is not pending is provider_cancel_unsupported", func(t *testing.T) {
		provider, mock := newMockDashScope(t, map[string]mockResponse{"POST " + cancelPath: {http.StatusBadRequest, `{"code":"UnsupportedOperation","message":"Failed to cancel the task, please confirm if the task is in PENDING status.","request_id":"r"}`}})
		_, bifrostErr := provider.VideoCancel(videoTestContext(t), videoTestKey(), &schemas.BifrostVideoCancelRequest{Provider: schemas.DashScope, ID: taskID})
		if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusBadRequest {
			t.Fatalf("got %+v, want a 400", bifrostErr)
		}
		if bifrostErr.ExtraFields.ErrorType != schemas.ErrorTypeProviderCancelUnsupported {
			t.Errorf("error_type = %q, want provider_cancel_unsupported", bifrostErr.ExtraFields.ErrorType)
		}
		if !strings.Contains(bifrostErr.Error.Message, "PENDING") {
			t.Errorf("message = %q, want the vendor's reason", bifrostErr.Error.Message)
		}
		if n := len(mock.recorded()); n != 1 {
			t.Errorf("upstream calls = %d, want exactly 1", n)
		}
	})

	t.Run("any other vendor error is returned as it is", func(t *testing.T) {
		provider, _ := newMockDashScope(t, map[string]mockResponse{"POST " + cancelPath: {http.StatusUnauthorized, `{"code":"InvalidApiKey","message":"Invalid API-key provided.","request_id":"r"}`}})
		_, bifrostErr := provider.VideoCancel(videoTestContext(t), videoTestKey(), &schemas.BifrostVideoCancelRequest{Provider: schemas.DashScope, ID: taskID})
		if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusUnauthorized {
			t.Fatalf("got %+v, want the vendor's 401", bifrostErr)
		}
		if got := schemas.ClassifyErrorType(bifrostErr, schemas.VideoCancelRequest); got != schemas.ErrorTypeProviderAuthFailed {
			t.Errorf("error_type = %q, want provider_auth_failed", got)
		}
	})
}

// TestDashScopeVideoDownload pins the download: the task is queried once for its video_url,
// the link is fetched without the key, the served type is pinned to a video type, and a task
// with no link or a link to a private address is not fetched.
func TestDashScopeVideoDownload(t *testing.T) {
	const taskID = "eff1443c-ccab-4676-aad3-000000000004"
	taskBody := func(status, videoURL string) string {
		return `{"request_id":"r","output":{"task_id":"` + taskID + `","task_status":"` + status + `","submit_time":"2025-09-29 14:18:52.331","end_time":"2025-09-29 14:23:39.407","video_url":"` + videoURL + `"}}`
	}

	t.Run("a finished task's link is fetched without the key", func(t *testing.T) {
		var fetchAuth string
		proxy := proxytest.ForwardProxy(t, func(w http.ResponseWriter, r *http.Request) {
			fetchAuth = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, "video bytes")
		})
		provider, mock := newMockDashScope(t, map[string]mockResponse{
			"GET " + dashScopeTasksPath + "/" + taskID: {http.StatusOK, taskBody("SUCCEEDED", "http://203.0.113.10/xx.mp4?Signature=abc")},
		})
		ctx := videoTestContext(t)
		ctx.SetValue(schemas.BifrostContextKeyProviderProxyConfig, &schemas.ProxyConfig{Type: schemas.HTTPProxy, URL: schemas.NewSecretVar(proxy.URL)})
		resp, bifrostErr := provider.VideoDownload(ctx, videoTestKey(), &schemas.BifrostVideoDownloadRequest{Provider: schemas.DashScope, ID: taskID + ":dashscope"})
		if bifrostErr != nil {
			t.Fatalf("VideoDownload: %+v", bifrostErr.Error)
		}
		if string(resp.Content) != "video bytes" || resp.ContentType != "video/mp4" || resp.VideoID != taskID+":dashscope" {
			t.Errorf("download = %q as %q for %q", resp.Content, resp.ContentType, resp.VideoID)
		}
		if fetchAuth != "" {
			t.Errorf("the link was fetched with Authorization %q; the key must not leave DashScope's API", fetchAuth)
		}
		if n := len(mock.recorded()); n != 1 {
			t.Errorf("task queries = %d, want exactly 1", n)
		}
	})

	t.Run("a task that is still running has nothing to download", func(t *testing.T) {
		provider, _ := newMockDashScope(t, map[string]mockResponse{
			"GET " + dashScopeTasksPath + "/" + taskID: {http.StatusOK, taskBody("RUNNING", "")},
		})
		_, bifrostErr := provider.VideoDownload(videoTestContext(t), videoTestKey(), &schemas.BifrostVideoDownloadRequest{Provider: schemas.DashScope, ID: taskID})
		if bifrostErr == nil || !strings.Contains(bifrostErr.Error.Message, "in_progress") {
			t.Fatalf("got %+v, want a not-ready error naming the status", bifrostErr)
		}
	})

	t.Run("a link to a private address is never dialed", func(t *testing.T) {
		var hits int
		internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits++ }))
		t.Cleanup(internal.Close)
		provider, _ := newMockDashScope(t, map[string]mockResponse{
			"GET " + dashScopeTasksPath + "/" + taskID: {http.StatusOK, taskBody("SUCCEEDED", internal.URL+"/xx.mp4?Signature=abc123")},
		})
		_, bifrostErr := provider.VideoDownload(videoTestContext(t), videoTestKey(), &schemas.BifrostVideoDownloadRequest{Provider: schemas.DashScope, ID: taskID})
		if bifrostErr == nil {
			t.Fatal("expected the loopback link to be refused")
		}
		if hits != 0 {
			t.Errorf("the loopback link was dialed %d times", hits)
		}
		message := bifrostErr.Error.Message
		if bifrostErr.Error.Error != nil {
			message += " " + bifrostErr.Error.Error.Error()
		}
		if strings.Contains(message, "abc123") {
			t.Errorf("the error leaks the signed query: %s", message)
		}
	})
}

// BenchmarkDashScopeVideoGenerationRequest measures the request-path cost this provider adds
// to a video create: the conversion to the Wan 3.0 body and its JSON encoding.
func BenchmarkDashScopeVideoGenerationRequest(b *testing.B) {
	request := &schemas.BifrostVideoGenerationRequest{
		Provider: schemas.DashScope, Model: "wan3.0-video",
		Input: &schemas.VideoGenerationInput{Prompt: "A kitten on a rooftop", InputReference: schemas.Ptr("https://cdn.example.com/first.png")},
		Params: &schemas.VideoGenerationParameters{
			Seconds: schemas.Ptr("5"), Resolution: "720p", AspectRatio: "16:9", LastFrame: "https://cdn.example.com/last.png",
			Audio: schemas.Ptr(true), Seed: schemas.Ptr(7),
			ExtraParams: map[string]any{"resolution": "720p", "aspect_ratio": "16:9", "last_frame": "https://cdn.example.com/last.png", "watermark": false},
		},
	}
	b.ReportAllocs()
	for b.Loop() {
		converted, err := ToDashScopeVideoGenerationRequest(request)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := schemas.MarshalSorted(converted); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkDashScopeVideoTaskResponse measures the conversion of a finished task to the task
// object, the work a task query adds per call.
func BenchmarkDashScopeVideoTaskResponse(b *testing.B) {
	body := []byte(`{"request_id":"r","output":{"task_id":"eff1443c-ccab-4676-aad3-000000000002","task_status":"SUCCEEDED","submit_time":"2025-09-29 14:18:52.331","scheduled_time":"2025-09-29 14:18:59.290","end_time":"2025-09-29 14:23:39.407","orig_prompt":"A kitten on a rooftop","video_url":"https://dashscope-result-sh.oss-accelerate.aliyuncs.com/1d/xx.mp4?Expires=1759213419"},"usage":{"duration":10,"input_video_duration":0,"output_video_duration":10,"video_count":1,"SR":720}}`)
	b.ReportAllocs()
	for b.Loop() {
		var task DashScopeTaskResponse
		if err := schemas.Unmarshal(body, &task); err != nil {
			b.Fatal(err)
		}
		if _, bifrostErr := ToBifrostVideoGenerationResponse(&task); bifrostErr != nil {
			b.Fatal(bifrostErr.Error.Message)
		}
	}
}
