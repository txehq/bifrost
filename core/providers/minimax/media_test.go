package minimax_test

// Image and video generation on MiniMax's native APIs (upstream d20cb6ace), against a
// recording server.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/providers/minimax"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/tidwall/gjson"
)

type capturedRequest struct {
	method string
	path   string
	auth   string
	body   []byte
}

// newMockMiniMax starts a server that records each request and replies with the response registered for its path.
func newMockMiniMax(t *testing.T, responses map[string]func(w http.ResponseWriter)) (*minimax.MinimaxProvider, func() []capturedRequest) {
	t.Helper()
	var mu sync.Mutex
	var captured []capturedRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		captured = append(captured, capturedRequest{method: r.Method, path: r.URL.Path, auth: r.Header.Get("Authorization"), body: body})
		mu.Unlock()
		if respond, ok := responses[r.Method+" "+r.URL.Path]; ok {
			respond(w)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(server.Close)

	provider, err := minimax.NewMinimaxProvider(&schemas.ProviderConfig{
		NetworkConfig: schemas.NetworkConfig{
			BaseURL:                        server.URL,
			DefaultRequestTimeoutInSeconds: 10,
			AllowPrivateNetwork:            true,
		},
	}, bifrost.NewDefaultLogger(schemas.LogLevelError))
	if err != nil {
		t.Fatalf("NewMinimaxProvider: %v", err)
	}
	return provider, func() []capturedRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]capturedRequest(nil), captured...)
	}
}

func jsonResponse(status int, body string) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

func testContext(t *testing.T) *schemas.BifrostContext {
	t.Helper()
	ctx, cancel := schemas.NewBifrostContextWithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func testKey() schemas.Key {
	return schemas.Key{Value: *schemas.NewSecretVar("test-key"), Models: []string{"*"}}
}

func TestToMiniMaxImageGenerationRequest(t *testing.T) {
	tests := []struct {
		name            string
		params          *schemas.ImageGenerationParameters
		wantWidth       int64
		wantAspectRatio string
		wantFormat      string
	}{
		{name: "valid size becomes width and height", params: &schemas.ImageGenerationParameters{Size: schemas.Ptr("1024x768")}, wantWidth: 1024},
		{name: "out of range size snaps to aspect ratio", params: &schemas.ImageGenerationParameters{Size: schemas.Ptr("2560x1440")}, wantAspectRatio: "16:9"},
		{name: "explicit aspect ratio wins", params: &schemas.ImageGenerationParameters{Size: schemas.Ptr("1024x1024"), AspectRatio: schemas.Ptr("9:16")}, wantAspectRatio: "9:16"},
		{name: "b64_json maps to base64", params: &schemas.ImageGenerationParameters{ResponseFormat: schemas.Ptr("b64_json")}, wantFormat: "base64"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := minimax.ToMiniMaxImageGenerationRequest(&schemas.BifrostImageGenerationRequest{
				Model:  "image-01",
				Input:  &schemas.ImageGenerationInput{Prompt: "a cat"},
				Params: tt.params,
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantWidth != 0 && (req.Width == nil || int64(*req.Width) != tt.wantWidth) {
				t.Errorf("width = %v, want %d", req.Width, tt.wantWidth)
			}
			if tt.wantAspectRatio != "" && (req.AspectRatio == nil || *req.AspectRatio != tt.wantAspectRatio) {
				t.Errorf("aspect_ratio = %v, want %s", req.AspectRatio, tt.wantAspectRatio)
			}
			if tt.wantAspectRatio != "" && req.Width != nil {
				t.Errorf("width should be unset when aspect_ratio is used, got %d", *req.Width)
			}
			if tt.wantFormat != "" && (req.ResponseFormat == nil || *req.ResponseFormat != tt.wantFormat) {
				t.Errorf("response_format = %v, want %s", req.ResponseFormat, tt.wantFormat)
			}
		})
	}
}

func TestMiniMaxImageGeneration(t *testing.T) {
	provider, captured := newMockMiniMax(t, map[string]func(http.ResponseWriter){
		"POST /v1/image_generation": jsonResponse(http.StatusOK, `{"id":"img1","data":{"image_urls":["https://cdn.example/a.png","https://cdn.example/b.png"]},"metadata":{"success_count":2,"failed_count":0},"base_resp":{"status_code":0,"status_msg":"success"}}`),
	})

	resp, bifrostErr := provider.ImageGeneration(testContext(t), testKey(), &schemas.BifrostImageGenerationRequest{
		Provider: schemas.Minimax,
		Model:    "image-01",
		Input:    &schemas.ImageGenerationInput{Prompt: "a cat"},
		Params:   &schemas.ImageGenerationParameters{N: schemas.Ptr(2)},
	})
	if bifrostErr != nil {
		t.Fatalf("ImageGeneration error: %+v", bifrostErr.Error)
	}
	if len(resp.Data) != 2 || resp.Data[1].URL != "https://cdn.example/b.png" || resp.Data[1].Index != 1 {
		t.Errorf("unexpected data: %+v", resp.Data)
	}
	reqs := captured()
	if got := gjson.GetBytes(reqs[0].body, "n").Int(); got != 2 {
		t.Errorf("n = %d, want 2", got)
	}
	if reqs[0].auth != "Bearer test-key" {
		t.Errorf("authorization = %q", reqs[0].auth)
	}
}

func TestMiniMaxImageGenerationBaseRespError(t *testing.T) {
	provider, _ := newMockMiniMax(t, map[string]func(http.ResponseWriter){
		"POST /v1/image_generation": jsonResponse(http.StatusOK, `{"id":"img1","data":{},"base_resp":{"status_code":1026,"status_msg":"input new_sensitive"}}`),
	})

	_, bifrostErr := provider.ImageGeneration(testContext(t), testKey(), &schemas.BifrostImageGenerationRequest{
		Provider: schemas.Minimax,
		Model:    "image-01",
		Input:    &schemas.ImageGenerationInput{Prompt: "a cat"},
	})
	if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected a 400 content filter error, got %+v", bifrostErr)
	}
	if bifrostErr.Error.Code == nil || *bifrostErr.Error.Code != "1026" {
		t.Errorf("code = %v, want 1026", bifrostErr.Error.Code)
	}
}

func TestToMiniMaxVideoGenerationRequest(t *testing.T) {
	tests := []struct {
		name           string
		model          string
		input          *schemas.VideoGenerationInput
		params         *schemas.VideoGenerationParameters
		wantDuration   int
		wantResolution string
		wantRatio      string
		wantContent    int
	}{
		{
			name:  "text to video defaults",
			model: "MiniMax-H3", input: &schemas.VideoGenerationInput{Prompt: "a dog"},
			wantDuration: 6, wantResolution: "768P", wantRatio: "16:9", wantContent: 1,
		},
		{
			name:  "image to video uses adaptive ratio",
			model: "MiniMax-H3", input: &schemas.VideoGenerationInput{Prompt: "a dog", InputReference: schemas.Ptr("https://example.com/a.png")},
			wantDuration: 6, wantResolution: "768P", wantRatio: "adaptive", wantContent: 2,
		},
		{
			name:  "size and seconds map with clamping on H3-Max",
			model: "MiniMax-H3-Max", input: &schemas.VideoGenerationInput{Prompt: "a dog"},
			params:       &schemas.VideoGenerationParameters{Seconds: schemas.Ptr("4"), Size: "480x854"},
			wantDuration: 5, wantResolution: "480P", wantRatio: "9:16", wantContent: 1,
		},
		{
			name:  "large size on H3 picks 2K",
			model: "MiniMax-H3", input: &schemas.VideoGenerationInput{Prompt: "a dog"},
			params:       &schemas.VideoGenerationParameters{Seconds: schemas.Ptr("20"), Size: "2560x1440"},
			wantDuration: 15, wantResolution: "2K", wantRatio: "16:9", wantContent: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := minimax.ToMiniMaxVideoGenerationRequest(&schemas.BifrostVideoGenerationRequest{Model: tt.model, Input: tt.input, Params: tt.params})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if req.Duration != tt.wantDuration {
				t.Errorf("duration = %d, want %d", req.Duration, tt.wantDuration)
			}
			if req.Resolution != tt.wantResolution {
				t.Errorf("resolution = %s, want %s", req.Resolution, tt.wantResolution)
			}
			if req.Ratio == nil || *req.Ratio != tt.wantRatio {
				t.Errorf("ratio = %v, want %s", req.Ratio, tt.wantRatio)
			}
			if len(req.Content) != tt.wantContent {
				t.Errorf("content items = %d, want %d", len(req.Content), tt.wantContent)
			}
		})
	}
}

func TestMiniMaxVideoLifecycle(t *testing.T) {
	provider, captured := newMockMiniMax(t, map[string]func(http.ResponseWriter){
		"POST /v2/video_generation":                      jsonResponse(http.StatusOK, `{"task_id":"424010985738629"}`),
		"GET /v2/query/video_generation/424010985738629": jsonResponse(http.StatusOK, `{"task":{"id":"424010985738629","model":"MiniMax-H3","status":"succeeded","created_at":100,"updated_at":200,"content":{"url":"https://cdn.example/v.mp4"},"resolution":"768P","duration":6}}`),
		"DELETE /v2/video_generation/424010985738629":    jsonResponse(http.StatusOK, `{"task_id":"424010985738629","action":"deleted","status":"deleted"}`),
	})
	ctx := testContext(t)

	created, bifrostErr := provider.VideoGeneration(ctx, testKey(), &schemas.BifrostVideoGenerationRequest{
		Provider: schemas.Minimax,
		Model:    "MiniMax-H3",
		Input:    &schemas.VideoGenerationInput{Prompt: "a dog"},
	})
	if bifrostErr != nil {
		t.Fatalf("VideoGeneration error: %+v", bifrostErr.Error)
	}
	if created.Status != schemas.VideoStatusQueued || !strings.HasPrefix(created.ID, "424010985738629") || created.ID == "424010985738629" {
		t.Errorf("unexpected create response: id=%s status=%s", created.ID, created.Status)
	}
	if created.Seconds == nil || *created.Seconds != "6" {
		t.Errorf("seconds = %v, want 6", created.Seconds)
	}

	retrieved, bifrostErr := provider.VideoRetrieve(ctx, testKey(), &schemas.BifrostVideoRetrieveRequest{Provider: schemas.Minimax, ID: created.ID})
	if bifrostErr != nil {
		t.Fatalf("VideoRetrieve error: %+v", bifrostErr.Error)
	}
	if retrieved.Status != schemas.VideoStatusCompleted || retrieved.ID != created.ID {
		t.Errorf("unexpected retrieve response: id=%s status=%s", retrieved.ID, retrieved.Status)
	}
	if len(retrieved.Videos) != 1 || retrieved.Videos[0].URL == nil || *retrieved.Videos[0].URL != "https://cdn.example/v.mp4" {
		t.Errorf("unexpected videos: %+v", retrieved.Videos)
	}

	deleted, bifrostErr := provider.VideoDelete(ctx, testKey(), &schemas.BifrostVideoDeleteRequest{Provider: schemas.Minimax, ID: created.ID})
	if bifrostErr != nil {
		t.Fatalf("VideoDelete error: %+v", bifrostErr.Error)
	}
	if !deleted.Deleted || deleted.ID != created.ID {
		t.Errorf("unexpected delete response: %+v", deleted)
	}

	reqs := captured()
	if len(reqs) != 3 {
		t.Fatalf("expected 3 upstream requests, got %d", len(reqs))
	}
	if got := gjson.GetBytes(reqs[0].body, "content.0.text").String(); got != "a dog" {
		t.Errorf("content[0].text = %q, want a dog", got)
	}
}

func TestToBifrostVideoGenerationResponseFailedTask(t *testing.T) {
	resp, bifrostErr := minimax.ToBifrostVideoGenerationResponse(&minimax.MiniMaxVideoTask{
		ID:     "t1",
		Status: minimax.MiniMaxVideoTaskStatusFailed,
		Error:  &minimax.MiniMaxTaskError{Code: "content_policy", Message: "blocked"},
	})
	if bifrostErr != nil {
		t.Fatalf("unexpected error: %+v", bifrostErr.Error)
	}
	if resp.Status != schemas.VideoStatusFailed || resp.Error == nil || resp.Error.Code != "content_policy" || resp.Error.Message != "blocked" {
		t.Errorf("unexpected failed task mapping: %+v", resp)
	}
}
