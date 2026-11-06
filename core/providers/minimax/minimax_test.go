package minimax_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/internal/llmtests"
	"github.com/capsohq/bifrost/core/providers/minimax"
	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/tidwall/gjson"
)

func TestMinimax(t *testing.T) {
	t.Parallel()
	if strings.TrimSpace(os.Getenv("MINIMAX_API_KEY")) == "" {
		t.Skip("Skipping MiniMax tests because MINIMAX_API_KEY is not set")
	}

	client, ctx, cancel, err := llmtests.SetupTest()
	if err != nil {
		t.Fatalf("Error initializing test setup: %v", err)
	}
	defer cancel()
	defer client.Shutdown()

	testConfig := llmtests.ComprehensiveTestConfig{
		Provider:  schemas.MiniMax,
		ChatModel: "MiniMax-M3",
		// Fork: text completions and prompt caching run on the Anthropic-compatible text endpoint.
		TextModel:          "MiniMax-M2.5",
		PromptCachingModel: "MiniMax-M2.5",
		Fallbacks: []schemas.Fallback{
			{Provider: schemas.MiniMax, Model: "MiniMax-M3"},
		},
		ReasoningModel:       "MiniMax-M3",
		VisionModel:          "MiniMax-M3",
		ImageGenerationModel: "image-01",
		VideoGenerationModel: "MiniMax-H3-Max",
		SpeechSynthesisModel: "speech-2.8-turbo",
		TranscriptionModel:   "asr-1.0",
		Scenarios: llmtests.TestScenarios{
			TextCompletion:        true,
			TextCompletionStream:  true,
			PromptCaching:         true,
			SimpleChat:            true,
			CompletionStream:      true,
			MultiTurnConversation: true,
			ToolCalls:             true,
			ToolCallsStreaming:    true,
			MultipleToolCalls:     true,
			End2EndToolCalling:    true,
			AutomaticFunctionCall: true,
			ImageURL:              true,
			ImageBase64:           true,
			CompleteEnd2End:       true,
			ListModels:            true,
			Reasoning:             true,
			ImageGeneration:       true,
			SpeechSynthesis:       true,
			SpeechSynthesisStream: true,
			Transcription:         true,
			TranscriptionStream:   true,
			VideoGeneration:       os.Getenv("MINIMAX_RUN_VIDEO_TESTS") == "1", // H3 billing is per output second
		},
		DisableParallelFor: []string{"PromptCaching"},
	}

	t.Run("MinimaxTests", func(t *testing.T) {
		llmtests.RunAllComprehensiveTests(t, client, ctx, testConfig)
	})
}

type capturedRequest struct {
	method string
	path   string
	auth   string
	header http.Header
	body   []byte
}

// newMockMiniMax starts a server that records each request and replies with the response registered for its path.
func newMockMiniMax(t *testing.T, responses map[string]func(w http.ResponseWriter)) (*minimax.MiniMaxProvider, func() []capturedRequest) {
	t.Helper()
	var mu sync.Mutex
	var captured []capturedRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		captured = append(captured, capturedRequest{method: r.Method, path: r.URL.Path, auth: r.Header.Get("Authorization"), header: r.Header.Clone(), body: body})
		mu.Unlock()
		if respond, ok := responses[r.Method+" "+r.URL.Path]; ok {
			respond(w)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(server.Close)

	provider, err := minimax.NewMiniMaxProvider(&schemas.ProviderConfig{
		NetworkConfig: schemas.NetworkConfig{
			BaseURL:                        server.URL,
			DefaultRequestTimeoutInSeconds: 10,
			AllowPrivateNetwork:            true,
		},
	}, bifrost.NewDefaultLogger(schemas.LogLevelError))
	if err != nil {
		t.Fatalf("NewMiniMaxProvider: %v", err)
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

func userChatRequest(params *schemas.ChatParameters) *schemas.BifrostChatRequest {
	return &schemas.BifrostChatRequest{
		Provider: schemas.MiniMax,
		Model:    "MiniMax-M3",
		Input:    []schemas.ChatMessage{{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: schemas.Ptr("hi")}}},
		Params:   params,
	}
}

const chatSuccessBody = `{"id":"c1","object":"chat.completion","created":1,"model":"MiniMax-M3","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hello","reasoning_content":"thinking"}}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5},"base_resp":{"status_code":0,"status_msg":""}}`

func TestMiniMaxChatSendsReasoningSplitAndReadsReasoningContent(t *testing.T) {
	provider, captured := newMockMiniMax(t, map[string]func(http.ResponseWriter){
		"POST /v1/chat/completions": jsonResponse(http.StatusOK, chatSuccessBody),
	})

	resp, bifrostErr := provider.ChatCompletion(testContext(t), testKey(), userChatRequest(nil))
	if bifrostErr != nil {
		t.Fatalf("ChatCompletion error: %+v", bifrostErr.Error)
	}
	reqs := captured()
	if len(reqs) != 1 {
		t.Fatalf("expected 1 upstream request, got %d", len(reqs))
	}
	if reqs[0].auth != "Bearer test-key" {
		t.Errorf("Authorization = %q, want Bearer test-key", reqs[0].auth)
	}
	if !gjson.GetBytes(reqs[0].body, "reasoning_split").Bool() {
		t.Errorf("reasoning_split not sent as true: %s", reqs[0].body)
	}
	msg := resp.Choices[0].ChatNonStreamResponseChoice.Message
	if msg.ChatAssistantMessage == nil || msg.ChatAssistantMessage.Reasoning == nil || *msg.ChatAssistantMessage.Reasoning != "thinking" {
		t.Errorf("reasoning_content not mapped to Reasoning: %+v", msg.ChatAssistantMessage)
	}
}

func TestMiniMaxChatKeepsCallerReasoningSplit(t *testing.T) {
	provider, captured := newMockMiniMax(t, map[string]func(http.ResponseWriter){
		"POST /v1/chat/completions": jsonResponse(http.StatusOK, chatSuccessBody),
	})
	params := &schemas.ChatParameters{ExtraParams: map[string]interface{}{"reasoning_split": false}}

	if _, bifrostErr := provider.ChatCompletion(testContext(t), testKey(), userChatRequest(params)); bifrostErr != nil {
		t.Fatalf("ChatCompletion error: %+v", bifrostErr.Error)
	}
	split := gjson.GetBytes(captured()[0].body, "reasoning_split")
	if !split.Exists() || split.Bool() {
		t.Errorf("caller's reasoning_split=false was not preserved: %s", captured()[0].body)
	}
}

func TestMiniMaxChatReasoningEffortNone(t *testing.T) {
	tests := []struct {
		name         string
		model        string
		wantThinking bool
	}{
		{name: "M3 disables thinking", model: "MiniMax-M3", wantThinking: true},
		{name: "M3.1 drops effort without disabling", model: "MiniMax-M3.1-Flash-Preview", wantThinking: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider, captured := newMockMiniMax(t, map[string]func(http.ResponseWriter){
				"POST /v1/chat/completions": jsonResponse(http.StatusOK, chatSuccessBody),
			})
			reasoning := &schemas.ChatReasoning{Effort: schemas.Ptr("none")}
			req := userChatRequest(&schemas.ChatParameters{Reasoning: reasoning})
			req.Model = tt.model

			if _, bifrostErr := provider.ChatCompletion(testContext(t), testKey(), req); bifrostErr != nil {
				t.Fatalf("ChatCompletion error: %+v", bifrostErr.Error)
			}
			body := captured()[0].body
			if gjson.GetBytes(body, "reasoning_effort").Exists() {
				t.Errorf("reasoning_effort none should not be sent: %s", body)
			}
			if got := gjson.GetBytes(body, "thinking.type").String() == "disabled"; got != tt.wantThinking {
				t.Errorf("thinking disabled = %v, want %v: %s", got, tt.wantThinking, body)
			}
			if reasoning.Effort == nil || *reasoning.Effort != "none" {
				t.Errorf("caller's reasoning params were mutated: %+v", reasoning)
			}
		})
	}
}

func TestMiniMaxChatReasoningEffortLadder(t *testing.T) {
	tests := []struct{ effort, want string }{
		{effort: "max", want: "max"},
		{effort: "xhigh", want: "xhigh"},
		{effort: "high", want: "high"},
		{effort: "minimal", want: "low"},
	}
	for _, tt := range tests {
		t.Run(tt.effort, func(t *testing.T) {
			provider, captured := newMockMiniMax(t, map[string]func(http.ResponseWriter){
				"POST /v1/chat/completions": jsonResponse(http.StatusOK, chatSuccessBody),
			})
			req := userChatRequest(&schemas.ChatParameters{Reasoning: &schemas.ChatReasoning{Effort: schemas.Ptr(tt.effort)}})

			if _, bifrostErr := provider.ChatCompletion(testContext(t), testKey(), req); bifrostErr != nil {
				t.Fatalf("ChatCompletion error: %+v", bifrostErr.Error)
			}
			if got := gjson.GetBytes(captured()[0].body, "reasoning_effort").String(); got != tt.want {
				t.Errorf("reasoning_effort = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMiniMaxChatBaseRespErrorOn200(t *testing.T) {
	provider, _ := newMockMiniMax(t, map[string]func(http.ResponseWriter){
		"POST /v1/chat/completions": jsonResponse(http.StatusOK, `{"id":"","choices":null,"base_resp":{"status_code":1008,"status_msg":"insufficient balance"}}`),
	})

	_, bifrostErr := provider.ChatCompletion(testContext(t), testKey(), userChatRequest(nil))
	if bifrostErr == nil {
		t.Fatal("expected an error for non-zero base_resp")
	}
	if bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusPaymentRequired {
		t.Errorf("status code = %v, want 402", bifrostErr.StatusCode)
	}
	if bifrostErr.Error == nil || bifrostErr.Error.Message != "insufficient balance" {
		t.Errorf("message = %+v, want insufficient balance", bifrostErr.Error)
	}
}

func TestMiniMaxChatStreamBaseRespErrorOn200(t *testing.T) {
	provider, _ := newMockMiniMax(t, map[string]func(http.ResponseWriter){
		"POST /v1/chat/completions": func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("data: {\"id\":\"\",\"choices\":null,\"base_resp\":{\"status_code\":1008,\"status_msg\":\"insufficient balance\"}}\n\ndata: [DONE]\n\n"))
		},
	})

	stream, bifrostErr := provider.ChatCompletionStream(testContext(t), func(_ *schemas.BifrostContext, resp *schemas.BifrostResponse, err *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError) {
		return resp, err
	}, nil, testKey(), userChatRequest(nil))
	if bifrostErr != nil {
		t.Fatalf("unexpected setup error: %+v", bifrostErr)
	}

	var streamErr *schemas.BifrostError
	for chunk := range stream {
		if chunk != nil && chunk.BifrostError != nil {
			streamErr = chunk.BifrostError
		}
	}
	if streamErr == nil {
		t.Fatal("expected a stream error for non-zero base_resp")
	}
	if streamErr.StatusCode == nil || *streamErr.StatusCode != http.StatusPaymentRequired {
		t.Errorf("status code = %v, want 402", streamErr.StatusCode)
	}
	if streamErr.Error == nil || streamErr.Error.Message != "insufficient balance" {
		t.Errorf("message = %+v, want insufficient balance", streamErr.Error)
	}
}

func TestMiniMaxHTTPErrorUsesErrorBody(t *testing.T) {
	provider, _ := newMockMiniMax(t, map[string]func(http.ResponseWriter){
		"POST /v1/chat/completions": jsonResponse(http.StatusUnauthorized, `{"type":"error","error":{"type":"authentication_error","message":"invalid api key","http_code":"401"},"request_id":"r1"}`),
	})

	_, bifrostErr := provider.ChatCompletion(testContext(t), testKey(), userChatRequest(nil))
	if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected a 401 error, got %+v", bifrostErr)
	}
	if bifrostErr.Error.Message != "invalid api key" {
		t.Errorf("message = %q, want invalid api key", bifrostErr.Error.Message)
	}
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
		Provider: schemas.MiniMax,
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
	if got := gjson.GetBytes(captured()[0].body, "n").Int(); got != 2 {
		t.Errorf("n = %d, want 2", got)
	}
}

func TestMiniMaxImageGenerationBaseRespError(t *testing.T) {
	provider, _ := newMockMiniMax(t, map[string]func(http.ResponseWriter){
		"POST /v1/image_generation": jsonResponse(http.StatusOK, `{"id":"img1","data":{},"base_resp":{"status_code":1026,"status_msg":"input new_sensitive"}}`),
	})

	_, bifrostErr := provider.ImageGeneration(testContext(t), testKey(), &schemas.BifrostImageGenerationRequest{
		Provider: schemas.MiniMax,
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
		Provider: schemas.MiniMax,
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

	retrieved, bifrostErr := provider.VideoRetrieve(ctx, testKey(), &schemas.BifrostVideoRetrieveRequest{Provider: schemas.MiniMax, ID: created.ID})
	if bifrostErr != nil {
		t.Fatalf("VideoRetrieve error: %+v", bifrostErr.Error)
	}
	if retrieved.Status != schemas.VideoStatusCompleted || retrieved.ID != created.ID {
		t.Errorf("unexpected retrieve response: id=%s status=%s", retrieved.ID, retrieved.Status)
	}
	if len(retrieved.Videos) != 1 || retrieved.Videos[0].URL == nil || *retrieved.Videos[0].URL != "https://cdn.example/v.mp4" {
		t.Errorf("unexpected videos: %+v", retrieved.Videos)
	}

	deleted, bifrostErr := provider.VideoDelete(ctx, testKey(), &schemas.BifrostVideoDeleteRequest{Provider: schemas.MiniMax, ID: created.ID})
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

// The settled size must be "WxH": video pricing bands read the short edge of that string, so a tier
// label such as "480P" would miss the 480p rate.
func TestToBifrostVideoGenerationResponseSize(t *testing.T) {
	for _, tc := range []struct {
		resolution, ratio, want string
	}{
		{"480P", "9:16", "480x853"},
		{"480P", "16:9", "853x480"},
		{"768P", "16:9", "1365x768"},
		{"768P", "1:1", "768x768"},
		{"1080P", "21:9", "2520x1080"},
		{"2K", "16:9", "2560x1440"},
		{"768p", "4:3", "1024x768"},
		{"768P", "adaptive", ""},
		{"768P", "", ""},
		{"", "16:9", ""},
		{"4K", "16:9", ""},
	} {
		resp, bifrostErr := minimax.ToBifrostVideoGenerationResponse(&minimax.MiniMaxVideoTask{ID: "t1", Status: minimax.MiniMaxVideoTaskStatusSucceeded, Resolution: tc.resolution, Ratio: tc.ratio})
		if bifrostErr != nil {
			t.Fatalf("%s %s: %+v", tc.resolution, tc.ratio, bifrostErr.Error)
		}
		if resp.Size != tc.want {
			t.Errorf("resolution %q ratio %q: size = %q, want %q", tc.resolution, tc.ratio, resp.Size, tc.want)
		}
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

func passthroughPostHook(_ *schemas.BifrostContext, result *schemas.BifrostResponse, err *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError) {
	return result, err
}

func sseResponse(events ...string) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, e := range events {
			_, _ = w.Write([]byte("data: " + e + "\n\n"))
		}
	}
}

func collectStream(t *testing.T, stream chan *schemas.BifrostStreamChunk) []*schemas.BifrostStreamChunk {
	t.Helper()
	var chunks []*schemas.BifrostStreamChunk
	for chunk := range stream {
		chunks = append(chunks, chunk)
	}
	return chunks
}

// multipartFields parses a captured multipart request into its text fields and file bytes.
func multipartFields(t *testing.T, req capturedRequest) (map[string]string, []byte) {
	t.Helper()
	_, params, err := mime.ParseMediaType(req.header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("content type: %v", err)
	}
	reader := multipart.NewReader(bytes.NewReader(req.body), params["boundary"])
	fields := map[string]string{}
	var file []byte
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("multipart: %v", err)
		}
		data, _ := io.ReadAll(part)
		if part.FormName() == "file" {
			file = data
			continue
		}
		fields[part.FormName()] = string(data)
	}
	return fields, file
}

func speechRequest(voice string, params *schemas.SpeechParameters) *schemas.BifrostSpeechRequest {
	if params == nil {
		params = &schemas.SpeechParameters{}
	}
	if voice != "" {
		params.VoiceConfig = &schemas.SpeechVoiceInput{Voice: schemas.Ptr(voice)}
	}
	return &schemas.BifrostSpeechRequest{
		Provider: schemas.MiniMax,
		Model:    "speech-2.8-turbo",
		Input:    &schemas.SpeechInput{Input: "Hello there"},
		Params:   params,
	}
}

func TestToMiniMaxSpeechRequest(t *testing.T) {
	t.Run("voice is required", func(t *testing.T) {
		if _, err := minimax.ToMiniMaxSpeechRequest(speechRequest("", nil), false); err == nil || !strings.Contains(err.Error(), "voice is required") {
			t.Fatalf("expected a voice error, got %v", err)
		}
	})
	t.Run("unsupported format is rejected", func(t *testing.T) {
		if _, err := minimax.ToMiniMaxSpeechRequest(speechRequest("English_Graceful_Lady", &schemas.SpeechParameters{ResponseFormat: "aac"}), false); err == nil {
			t.Fatal("expected aac to be rejected")
		}
	})
	t.Run("tuning extra params move into voice and audio settings", func(t *testing.T) {
		req, err := minimax.ToMiniMaxSpeechRequest(speechRequest("English_Graceful_Lady", &schemas.SpeechParameters{
			ResponseFormat: "wav",
			Speed:          schemas.Ptr(1.25),
			ExtraParams:    map[string]interface{}{"vol": 2.0, "pitch": 3.0, "emotion": "happy", "sample_rate": 24000.0, "language_boost": "English", "subtitle_enable": true},
		}), false)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		vs := req.VoiceSetting
		if vs.VoiceID != "English_Graceful_Lady" || *vs.Speed != 1.25 || *vs.Vol != 2 || *vs.Pitch != 3 || *vs.Emotion != "happy" {
			t.Errorf("unexpected voice_setting: %+v", vs)
		}
		if req.AudioSetting == nil || *req.AudioSetting.Format != "wav" || *req.AudioSetting.SampleRate != 24000 {
			t.Errorf("unexpected audio_setting: %+v", req.AudioSetting)
		}
		if req.LanguageBoost == nil || *req.LanguageBoost != "English" {
			t.Errorf("language_boost = %v", req.LanguageBoost)
		}
		if _, ok := req.ExtraParams["subtitle_enable"]; !ok || len(req.ExtraParams) != 1 {
			t.Errorf("only unknown keys should stay in extra params, got %v", req.ExtraParams)
		}
		if req.OutputFormat == nil || *req.OutputFormat != "hex" || req.Stream {
			t.Errorf("non-streaming request should ask for hex output: %+v", req)
		}
	})
	t.Run("stream excludes the aggregated final audio", func(t *testing.T) {
		req, err := minimax.ToMiniMaxSpeechRequest(speechRequest("English_Graceful_Lady", nil), true)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !req.Stream || req.StreamOptions == nil || !req.StreamOptions.ExcludeAggregatedAudio || req.OutputFormat != nil {
			t.Errorf("unexpected stream request: %+v", req)
		}
	})
}

func TestMiniMaxSpeech(t *testing.T) {
	audio := []byte("ID3fake-mp3")
	provider, captured := newMockMiniMax(t, map[string]func(http.ResponseWriter){
		"POST /v1/t2a_v2": jsonResponse(http.StatusOK, `{"data":{"audio":"`+hex.EncodeToString(audio)+`","status":2},"extra_info":{"audio_length":900,"usage_characters":11},"trace_id":"t1","base_resp":{"status_code":0,"status_msg":"success"}}`),
	})

	resp, bifrostErr := provider.Speech(testContext(t), testKey(), speechRequest("English_Graceful_Lady", nil))
	if bifrostErr != nil {
		t.Fatalf("Speech error: %+v", bifrostErr.Error)
	}
	if !bytes.Equal(resp.Audio, audio) {
		t.Errorf("audio = %q, want %q", resp.Audio, audio)
	}
	if resp.Usage == nil || resp.Usage.InputChars != 11 {
		t.Errorf("usage = %+v, want 11 input chars", resp.Usage)
	}
	body := captured()[0].body
	if gjson.GetBytes(body, "voice_setting.voice_id").String() != "English_Graceful_Lady" || gjson.GetBytes(body, "output_format").String() != "hex" {
		t.Errorf("unexpected request body: %s", body)
	}
}

func TestMiniMaxSpeechBaseRespErrorOn200(t *testing.T) {
	provider, _ := newMockMiniMax(t, map[string]func(http.ResponseWriter){
		"POST /v1/t2a_v2": jsonResponse(http.StatusOK, `{"base_resp":{"status_code":2054,"status_msg":"voice id not exist"}}`),
	})

	_, bifrostErr := provider.Speech(testContext(t), testKey(), speechRequest("not_a_voice", nil))
	if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected a 400, got %+v", bifrostErr)
	}
	if bifrostErr.Error.Message != "voice id not exist" {
		t.Errorf("message = %q", bifrostErr.Error.Message)
	}
}

func TestMiniMaxSpeechStream(t *testing.T) {
	provider, captured := newMockMiniMax(t, map[string]func(http.ResponseWriter){
		"POST /v1/t2a_v2": sseResponse(
			`{"data":{"audio":"aa","status":1},"trace_id":"t1","base_resp":{"status_code":0,"status_msg":""}}`,
			`{"data":{"audio":"bbcc","status":1},"trace_id":"t1","base_resp":{"status_code":0,"status_msg":""}}`,
			`{"data":{"audio":"","status":1},"trace_id":"t1","base_resp":{"status_code":0,"status_msg":""}}`,
			`{"data":{"audio":"","status":2},"extra_info":{"audio_length":900,"usage_characters":11},"trace_id":"t1","base_resp":{"status_code":0,"status_msg":"success"}}`,
		),
	})

	stream, bifrostErr := provider.SpeechStream(testContext(t), passthroughPostHook, func(context.Context) {}, testKey(), speechRequest("English_Graceful_Lady", nil))
	if bifrostErr != nil {
		t.Fatalf("SpeechStream error: %+v", bifrostErr.Error)
	}
	chunks := collectStream(t, stream)
	var audio []byte
	var types []schemas.SpeechStreamResponseType
	var usage *schemas.SpeechUsage
	for _, c := range chunks {
		if c.BifrostError != nil {
			t.Fatalf("unexpected stream error: %+v", c.BifrostError.Error)
		}
		types = append(types, c.BifrostSpeechStreamResponse.Type)
		audio = append(audio, c.BifrostSpeechStreamResponse.Audio...)
		if c.BifrostSpeechStreamResponse.Usage != nil {
			usage = c.BifrostSpeechStreamResponse.Usage
		}
	}
	want := []schemas.SpeechStreamResponseType{schemas.SpeechStreamResponseTypeDelta, schemas.SpeechStreamResponseTypeDelta, schemas.SpeechStreamResponseTypeDone}
	if len(types) != len(want) || types[0] != want[0] || types[1] != want[1] || types[2] != want[2] {
		t.Errorf("event types = %v, want %v", types, want)
	}
	if !bytes.Equal(audio, []byte{0xaa, 0xbb, 0xcc}) {
		t.Errorf("audio = %x, want aabbcc", audio)
	}
	if usage == nil || usage.InputChars != 11 {
		t.Errorf("usage = %+v, want 11 input chars", usage)
	}
	if !gjson.GetBytes(captured()[0].body, "stream_options.exclude_aggregated_audio").Bool() {
		t.Errorf("exclude_aggregated_audio not sent: %s", captured()[0].body)
	}
}

func TestMiniMaxSpeechStreamBaseRespErrorOn200(t *testing.T) {
	provider, _ := newMockMiniMax(t, map[string]func(http.ResponseWriter){
		"POST /v1/t2a_v2": jsonResponse(http.StatusOK, `{"base_resp":{"status_code":2054,"status_msg":"voice id not exist"}}`),
	})

	stream, bifrostErr := provider.SpeechStream(testContext(t), passthroughPostHook, func(context.Context) {}, testKey(), speechRequest("not_a_voice", nil))
	if bifrostErr != nil {
		t.Fatalf("SpeechStream setup error: %+v", bifrostErr.Error)
	}
	chunks := collectStream(t, stream)
	if len(chunks) != 1 || chunks[0].BifrostError == nil {
		t.Fatalf("expected exactly one error chunk, got %d chunks", len(chunks))
	}
	got := chunks[0].BifrostError
	if got.StatusCode == nil || *got.StatusCode != http.StatusBadRequest || got.Error.Message != "voice id not exist" {
		t.Errorf("unexpected error: status=%v message=%q", got.StatusCode, got.Error.Message)
	}
}

func TestMiniMaxSpeechStreamTruncated(t *testing.T) {
	provider, _ := newMockMiniMax(t, map[string]func(http.ResponseWriter){
		"POST /v1/t2a_v2": sseResponse(`{"data":{"audio":"aa","status":1},"base_resp":{"status_code":0,"status_msg":""}}`),
	})

	stream, bifrostErr := provider.SpeechStream(testContext(t), passthroughPostHook, func(context.Context) {}, testKey(), speechRequest("English_Graceful_Lady", nil))
	if bifrostErr != nil {
		t.Fatalf("SpeechStream setup error: %+v", bifrostErr.Error)
	}
	chunks := collectStream(t, stream)
	if len(chunks) == 0 || chunks[len(chunks)-1].BifrostError == nil {
		t.Fatalf("a stream cut before its status 2 chunk must end with an error, got %d chunks", len(chunks))
	}
}

func transcriptionRequest(params *schemas.TranscriptionParameters) *schemas.BifrostTranscriptionRequest {
	return &schemas.BifrostTranscriptionRequest{
		Provider: schemas.MiniMax,
		Model:    "asr-1.0",
		Input:    &schemas.TranscriptionInput{File: []byte("ID3fake-mp3"), Filename: "clip.mp3"},
		Params:   params,
	}
}

func TestMiniMaxTranscription(t *testing.T) {
	tests := []struct {
		name         string
		params       *schemas.TranscriptionParameters
		response     string
		wantFields   map[string]string
		wantLanguage string
		wantText     string
		wantWords    int
		wantSegments int
		wantPlainFmt string
		wantSeconds  float64
	}{
		{
			name:       "json with language header",
			params:     &schemas.TranscriptionParameters{Language: schemas.Ptr("en")},
			response:   `{"text":"The quick brown fox.","duration":3.312,"trace_id":"t1"}`,
			wantFields: map[string]string{"model": "asr-1.0"}, wantLanguage: "en",
			wantText: "The quick brown fox.", wantSeconds: 3.312,
		},
		{
			name:       "word granularity maps segments to words",
			params:     &schemas.TranscriptionParameters{TimestampGranularities: []string{"word"}},
			response:   `{"text":"The quick","duration":1,"n_speakers":1,"segments":[{"id":0,"start":0.14,"end":0.26,"speaker":"S1","text":"The "},{"id":1,"start":0.26,"end":0.6,"speaker":"S1","text":"quick "}]}`,
			wantFields: map[string]string{"response_format": "verbose_json", "timestamp_level": "word"},
			wantText:   "The quick", wantWords: 2, wantSeconds: 1,
		},
		{
			name:       "verbose_json keeps sentence segments",
			params:     &schemas.TranscriptionParameters{ResponseFormat: schemas.Ptr("verbose_json")},
			response:   `{"text":"Hello. Bye.","duration":2,"segments":[{"id":0,"start":0,"end":1,"speaker":"S1","text":"Hello."},{"id":1,"start":1,"end":2,"speaker":"S2","text":"Bye."}]}`,
			wantFields: map[string]string{"response_format": "verbose_json"},
			wantText:   "Hello. Bye.", wantSegments: 2, wantSeconds: 2,
		},
		{
			name:       "srt comes back as plain text",
			params:     &schemas.TranscriptionParameters{ResponseFormat: schemas.Ptr("srt")},
			response:   "1\n00:00:00,140 --> 00:00:03,120\nThe quick brown fox.\n",
			wantFields: map[string]string{"response_format": "srt"},
			wantText:   "1\n00:00:00,140 --> 00:00:03,120\nThe quick brown fox.\n", wantPlainFmt: "srt",
		},
		{
			name:       "text is served from json",
			params:     &schemas.TranscriptionParameters{ResponseFormat: schemas.Ptr("text")},
			response:   `{"text":"Hi.","duration":0.5}`,
			wantFields: map[string]string{"response_format": ""},
			wantText:   "Hi.", wantPlainFmt: "text", wantSeconds: 0.5,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider, captured := newMockMiniMax(t, map[string]func(http.ResponseWriter){
				"POST /v1/speech_to_text": func(w http.ResponseWriter) {
					if strings.HasPrefix(tt.response, "{") {
						w.Header().Set("Content-Type", "application/json")
					} else {
						w.Header().Set("Content-Type", "text/plain")
					}
					_, _ = w.Write([]byte(tt.response))
				},
			})

			resp, bifrostErr := provider.Transcription(testContext(t), testKey(), transcriptionRequest(tt.params))
			if bifrostErr != nil {
				t.Fatalf("Transcription error: %+v", bifrostErr.Error)
			}
			req := captured()[0]
			fields, file := multipartFields(t, req)
			if string(file) != "ID3fake-mp3" {
				t.Errorf("file = %q", file)
			}
			for k, v := range tt.wantFields {
				if fields[k] != v {
					t.Errorf("field %s = %q, want %q (all: %v)", k, fields[k], v, fields)
				}
			}
			if got := req.header.Get("language"); got != tt.wantLanguage {
				t.Errorf("language header = %q, want %q", got, tt.wantLanguage)
			}
			if resp.Text != tt.wantText {
				t.Errorf("text = %q, want %q", resp.Text, tt.wantText)
			}
			if len(resp.Words) != tt.wantWords || len(resp.Segments) != tt.wantSegments {
				t.Errorf("words = %d, segments = %d, want %d and %d", len(resp.Words), len(resp.Segments), tt.wantWords, tt.wantSegments)
			}
			if tt.wantWords > 0 && (resp.Words[1].Word != "quick" || resp.Words[1].Speaker == nil || *resp.Words[1].Speaker != "S1") {
				t.Errorf("unexpected word: %+v", resp.Words[1])
			}
			gotFmt := ""
			if resp.ResponseFormat != nil {
				gotFmt = *resp.ResponseFormat
			}
			if tt.wantPlainFmt != "" && gotFmt != tt.wantPlainFmt {
				t.Errorf("response format = %q, want %q", gotFmt, tt.wantPlainFmt)
			}
			if tt.wantSeconds > 0 && (resp.Usage == nil || resp.Usage.Seconds == nil || *resp.Usage.Seconds != tt.wantSeconds) {
				t.Errorf("usage = %+v, want %v seconds", resp.Usage, tt.wantSeconds)
			}
		})
	}
}

func TestMiniMaxTranscriptionHTTPError(t *testing.T) {
	provider, _ := newMockMiniMax(t, map[string]func(http.ResponseWriter){
		"POST /v1/speech_to_text": jsonResponse(http.StatusBadRequest, `{"type":"error","error":{"type":"bad_request_error","message":"invalid params, audio duration 623.4s exceeds the limit of 500s (2013)","http_code":"400"},"request_id":"r1"}`),
	})

	_, bifrostErr := provider.Transcription(testContext(t), testKey(), transcriptionRequest(nil))
	if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected a 400, got %+v", bifrostErr)
	}
	if !strings.Contains(bifrostErr.Error.Message, "exceeds the limit of 500s") {
		t.Errorf("message = %q", bifrostErr.Error.Message)
	}
}

func TestMiniMaxTranscriptionStream(t *testing.T) {
	provider, captured := newMockMiniMax(t, map[string]func(http.ResponseWriter){
		"POST /v1/speech_to_text": sseResponse(
			`{"index":0,"delta":"The","finish":false}`,
			`{"index":1,"delta":" quick brown fox jumps o","finish":false}`,
			`{"index":2,"delta":"ver the lazy dog.","finish":true,"duration":3.312}`,
		),
	})

	stream, bifrostErr := provider.TranscriptionStream(testContext(t), passthroughPostHook, func(context.Context) {}, testKey(), transcriptionRequest(&schemas.TranscriptionParameters{Language: schemas.Ptr("en")}))
	if bifrostErr != nil {
		t.Fatalf("TranscriptionStream error: %+v", bifrostErr.Error)
	}
	chunks := collectStream(t, stream)
	// Fork: the finish event's text arrives as a third delta, then a done event without a delta.
	if len(chunks) != 4 {
		t.Fatalf("expected 3 deltas and a done event, got %d chunks", len(chunks))
	}
	var joined strings.Builder
	for i, c := range chunks {
		if c.BifrostError != nil {
			t.Fatalf("unexpected stream error: %+v", c.BifrostError.Error)
		}
		ts := c.BifrostTranscriptionStreamResponse
		if ts.ExtraFields.ChunkIndex != i {
			t.Errorf("chunk %d has index %d", i, ts.ExtraFields.ChunkIndex)
		}
		if ts.Type == schemas.TranscriptionStreamResponseTypeDelta && ts.Delta != nil {
			joined.WriteString(*ts.Delta)
		}
	}
	// The streaming accumulator builds the logged transcript from delta events alone.
	if joined.String() != "The quick brown fox jumps over the lazy dog." {
		t.Errorf("deltas join to %q, want the full transcript", joined.String())
	}
	done := chunks[3].BifrostTranscriptionStreamResponse
	if done.Type != schemas.TranscriptionStreamResponseTypeDone || done.Text != "The quick brown fox jumps over the lazy dog." || done.Delta != nil {
		t.Errorf("unexpected done event: type=%s text=%q delta=%v", done.Type, done.Text, done.Delta)
	}
	if done.Usage == nil || done.Usage.Seconds == nil || *done.Usage.Seconds != 3.312 {
		t.Errorf("usage = %+v, want 3.312 seconds", done.Usage)
	}
	if d := chunks[1].BifrostTranscriptionStreamResponse; d.Type != schemas.TranscriptionStreamResponseTypeDelta || *d.Delta != " quick brown fox jumps o" {
		t.Errorf("unexpected delta: %+v", d)
	}
	req := captured()[0]
	fields, _ := multipartFields(t, req)
	if fields["stream"] != "true" || req.header.Get("language") != "en" {
		t.Errorf("stream field = %q, language header = %q", fields["stream"], req.header.Get("language"))
	}
}

func TestMiniMaxTranscriptionStreamBaseRespError(t *testing.T) {
	provider, _ := newMockMiniMax(t, map[string]func(http.ResponseWriter){
		"POST /v1/speech_to_text": sseResponse(
			`{"index":0,"delta":"The","finish":false}`,
			`{"base_resp":{"status_code":1004,"status_msg":"authentication failed"}}`,
		),
	})

	stream, bifrostErr := provider.TranscriptionStream(testContext(t), passthroughPostHook, func(context.Context) {}, testKey(), transcriptionRequest(nil))
	if bifrostErr != nil {
		t.Fatalf("TranscriptionStream error: %+v", bifrostErr.Error)
	}
	var streamErr *schemas.BifrostError
	for _, c := range collectStream(t, stream) {
		if c.BifrostError != nil {
			streamErr = c.BifrostError
		}
	}
	if streamErr == nil || streamErr.Error == nil || streamErr.Error.Message != "authentication failed" {
		t.Fatalf("expected the MiniMax base_resp error, got %+v", streamErr)
	}
	if streamErr.StatusCode == nil || *streamErr.StatusCode != http.StatusUnauthorized {
		t.Errorf("status code = %v, want 401", streamErr.StatusCode)
	}
}

func TestMiniMaxTranscriptionStreamRejectsTimestampGranularities(t *testing.T) {
	for name, params := range map[string]*schemas.TranscriptionParameters{
		"timestamp_granularities": {TimestampGranularities: []string{"word"}},
		"extra_params":            {ExtraParams: map[string]any{"timestamp_level": "sentence"}},
	} {
		t.Run(name, func(t *testing.T) {
			provider, captured := newMockMiniMax(t, nil)
			_, bifrostErr := provider.TranscriptionStream(testContext(t), passthroughPostHook, func(context.Context) {}, testKey(), transcriptionRequest(params))
			if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusBadRequest {
				t.Fatalf("expected a 400, got %+v", bifrostErr)
			}
			if len(captured()) != 0 {
				t.Errorf("no request should reach MiniMax, got %d", len(captured()))
			}
		})
	}
}

func TestMiniMaxTranscriptionStreamRejectsNonJSONFormat(t *testing.T) {
	provider, captured := newMockMiniMax(t, nil)

	_, bifrostErr := provider.TranscriptionStream(testContext(t), passthroughPostHook, func(context.Context) {}, testKey(), transcriptionRequest(&schemas.TranscriptionParameters{ResponseFormat: schemas.Ptr("verbose_json")}))
	if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected a 400, got %+v", bifrostErr)
	}
	if len(captured()) != 0 {
		t.Errorf("no request should reach MiniMax, got %d", len(captured()))
	}
}

// Fork (txehq/bifrost): text completions go to MiniMax's Anthropic-compatible text generation endpoint.

func textCompletionRequest(extraParams map[string]interface{}) *schemas.BifrostTextCompletionRequest {
	req := &schemas.BifrostTextCompletionRequest{
		Provider: schemas.MiniMax,
		Model:    "MiniMax-M2.5",
		Input:    &schemas.TextCompletionInput{PromptStr: schemas.Ptr("Say hello")},
	}
	if extraParams != nil {
		req.Params = &schemas.TextCompletionParameters{ExtraParams: extraParams}
	}
	return req
}

const anthropicTextSuccessBody = `{"id":"msg_1","type":"message","role":"assistant","model":"MiniMax-M2.5","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":4,"output_tokens":1}}`

func TestMiniMaxTextCompletionUsesAnthropicTextEndpoint(t *testing.T) {
	provider, captured := newMockMiniMax(t, map[string]func(http.ResponseWriter){
		"POST /text/v1/messages": jsonResponse(http.StatusOK, anthropicTextSuccessBody),
	})

	resp, bifrostErr := provider.TextCompletion(testContext(t), testKey(), textCompletionRequest(map[string]interface{}{"anthropic_beta": "prompt-caching-2024-07-31"}))
	if bifrostErr != nil {
		t.Fatalf("TextCompletion error: %+v", bifrostErr.Error)
	}
	reqs := captured()
	if len(reqs) != 1 {
		t.Fatalf("expected 1 upstream request, got %d", len(reqs))
	}
	got := reqs[0]
	if got.auth != "Bearer test-key" {
		t.Errorf("Authorization = %q, want Bearer test-key", got.auth)
	}
	if v := got.header.Get("anthropic-version"); v != "2023-06-01" {
		t.Errorf("anthropic-version = %q", v)
	}
	if v := got.header.Get("anthropic-beta"); v != "prompt-caching-2024-07-31" {
		t.Errorf("anthropic-beta = %q", v)
	}
	if gjson.GetBytes(got.body, "stream").Exists() {
		t.Errorf("non-streamed request carries stream: %s", got.body)
	}
	if !strings.Contains(gjson.GetBytes(got.body, "messages.0.content").Raw, "Say hello") {
		t.Errorf("prompt not sent as the user message: %s", got.body)
	}
	if resp.ExtraFields.RequestType != schemas.TextCompletionRequest || resp.ExtraFields.Provider != schemas.MiniMax {
		t.Errorf("extra fields = %+v", resp.ExtraFields)
	}
	if len(resp.Choices) != 1 || resp.Choices[0].TextCompletionResponseChoice == nil || resp.Choices[0].TextCompletionResponseChoice.Text == nil || *resp.Choices[0].TextCompletionResponseChoice.Text != "hello" {
		t.Fatalf("unexpected choices: %+v", resp.Choices)
	}
	if resp.Usage == nil || resp.Usage.PromptTokens != 4 || resp.Usage.CompletionTokens != 1 {
		t.Errorf("usage = %+v", resp.Usage)
	}
}

func TestMiniMaxTextCompletionResolvesAgainstBaseURLOrigin(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		jsonResponse(http.StatusOK, anthropicTextSuccessBody)(w)
	}))
	t.Cleanup(server.Close)
	provider, err := minimax.NewMiniMaxProvider(&schemas.ProviderConfig{
		NetworkConfig: schemas.NetworkConfig{BaseURL: server.URL + "/v1/", DefaultRequestTimeoutInSeconds: 10, AllowPrivateNetwork: true},
	}, bifrost.NewDefaultLogger(schemas.LogLevelError))
	if err != nil {
		t.Fatalf("NewMiniMaxProvider: %v", err)
	}
	if _, bifrostErr := provider.TextCompletion(testContext(t), testKey(), textCompletionRequest(nil)); bifrostErr != nil {
		t.Fatalf("TextCompletion error: %+v", bifrostErr.Error)
	}
	if gotPath != "/text/v1/messages" {
		t.Errorf("path = %q, want /text/v1/messages on the base URL's origin", gotPath)
	}
}

func TestMiniMaxTextCompletionErrorUsesAnthropicErrorBody(t *testing.T) {
	provider, _ := newMockMiniMax(t, map[string]func(http.ResponseWriter){
		"POST /text/v1/messages": jsonResponse(http.StatusBadRequest, `{"type":"error","error":{"type":"invalid_request_error","message":"prompt too long"}}`),
	})

	_, bifrostErr := provider.TextCompletion(testContext(t), testKey(), textCompletionRequest(nil))
	if bifrostErr == nil {
		t.Fatal("expected an error")
	}
	if bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %v, want 400", bifrostErr.StatusCode)
	}
	if bifrostErr.Error == nil || bifrostErr.Error.Message != "prompt too long" || bifrostErr.Error.Type == nil || *bifrostErr.Error.Type != "invalid_request_error" {
		t.Errorf("error = %+v", bifrostErr.Error)
	}
	if bifrostErr.ExtraFields.RequestType != schemas.TextCompletionRequest || bifrostErr.ExtraFields.Provider != schemas.MiniMax {
		t.Errorf("extra fields = %+v", bifrostErr.ExtraFields)
	}
}

// anthropicTextStream writes an Anthropic messages stream, pausing before the message_delta event.
func anthropicTextStream(pause time.Duration) func(w http.ResponseWriter) {
	events := [][2]string{
		{"message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"MiniMax-M2.5","content":[],"usage":{"input_tokens":4,"output_tokens":0}}}`},
		{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hel"}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}`},
		{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`},
		{"message_stop", `{"type":"message_stop"}`},
	}
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, e := range events {
			if e[0] == "message_delta" && pause > 0 {
				time.Sleep(pause)
			}
			_, _ = w.Write([]byte("event: " + e[0] + "\ndata: " + e[1] + "\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}

func TestMiniMaxTextCompletionStream(t *testing.T) {
	provider, captured := newMockMiniMax(t, map[string]func(http.ResponseWriter){
		"POST /text/v1/messages": anthropicTextStream(0),
	})

	stream, bifrostErr := provider.TextCompletionStream(testContext(t), passthroughPostHook, nil, testKey(), textCompletionRequest(nil))
	if bifrostErr != nil {
		t.Fatalf("TextCompletionStream error: %+v", bifrostErr.Error)
	}
	var text strings.Builder
	var sawUsage bool
	for _, chunk := range collectStream(t, stream) {
		if chunk.BifrostError != nil {
			t.Fatalf("stream error: %+v", chunk.BifrostError.Error)
		}
		resp := chunk.BifrostTextCompletionResponse
		if resp == nil {
			t.Fatalf("chunk carries no text completion response: %+v", chunk)
		}
		if resp.ExtraFields.RequestType != schemas.TextCompletionStreamRequest {
			t.Errorf("request type = %s", resp.ExtraFields.RequestType)
		}
		for _, choice := range resp.Choices {
			if choice.TextCompletionResponseChoice != nil && choice.TextCompletionResponseChoice.Text != nil {
				text.WriteString(*choice.TextCompletionResponseChoice.Text)
			}
		}
		if resp.Usage != nil && resp.Usage.CompletionTokens == 2 {
			sawUsage = true
		}
	}
	if text.String() != "hello" {
		t.Errorf("streamed text = %q, want hello", text.String())
	}
	if !sawUsage {
		t.Error("no chunk carried the final usage")
	}
	reqs := captured()
	if len(reqs) != 1 {
		t.Fatalf("expected 1 upstream request, got %d", len(reqs))
	}
	if !gjson.GetBytes(reqs[0].body, "stream").Bool() {
		t.Errorf("streamed request does not set stream: %s", reqs[0].body)
	}
	if v := reqs[0].header.Get("Accept"); v != "text/event-stream" {
		t.Errorf("Accept = %q", v)
	}
	if reqs[0].auth != "Bearer test-key" {
		t.Errorf("Authorization = %q", reqs[0].auth)
	}
}

// TestMiniMaxTextCompletionStreamOutlivesRequestTimeout pins that a text stream lasting longer than
// the provider's request timeout runs to its final event. It passes on either client in this fork
// (both stream the body); TextCompletionStream uses the streaming client like every other stream.
func TestMiniMaxTextCompletionStreamOutlivesRequestTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		anthropicTextStream(1500 * time.Millisecond)(w)
	}))
	t.Cleanup(server.Close)
	provider, err := minimax.NewMiniMaxProvider(&schemas.ProviderConfig{
		NetworkConfig: schemas.NetworkConfig{BaseURL: server.URL, DefaultRequestTimeoutInSeconds: 1, AllowPrivateNetwork: true},
	}, bifrost.NewDefaultLogger(schemas.LogLevelError))
	if err != nil {
		t.Fatalf("NewMiniMaxProvider: %v", err)
	}

	stream, bifrostErr := provider.TextCompletionStream(testContext(t), passthroughPostHook, nil, testKey(), textCompletionRequest(nil))
	if bifrostErr != nil {
		t.Fatalf("TextCompletionStream error: %+v", bifrostErr.Error)
	}
	var sawUsage bool
	for _, chunk := range collectStream(t, stream) {
		if chunk.BifrostError != nil {
			t.Fatalf("stream cut off: %+v", chunk.BifrostError.Error)
		}
		if resp := chunk.BifrostTextCompletionResponse; resp != nil && resp.Usage != nil && resp.Usage.CompletionTokens == 2 {
			sawUsage = true
		}
	}
	if !sawUsage {
		t.Error("stream ended before its final usage event")
	}
}

// BenchmarkMiniMaxChatCompletion is the new provider's chat round trip against a local server:
// reasoning_split injection, the OpenAI converter, base_resp check and response decode.
func BenchmarkMiniMaxChatCompletion(b *testing.B) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		jsonResponse(http.StatusOK, chatSuccessBody)(w)
	}))
	b.Cleanup(server.Close)
	provider, err := minimax.NewMiniMaxProvider(&schemas.ProviderConfig{
		NetworkConfig: schemas.NetworkConfig{BaseURL: server.URL, DefaultRequestTimeoutInSeconds: 10, AllowPrivateNetwork: true},
	}, bifrost.NewDefaultLogger(schemas.LogLevelError))
	if err != nil {
		b.Fatalf("NewMiniMaxProvider: %v", err)
	}
	key := testKey()
	req := userChatRequest(nil)
	b.ReportAllocs()
	for b.Loop() {
		ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
		if _, bifrostErr := provider.ChatCompletion(ctx, key, req); bifrostErr != nil {
			b.Fatalf("ChatCompletion: %+v", bifrostErr.Error)
		}
		ctx.Cancel()
	}
}

// BenchmarkMiniMaxTextCompletion is the fork's text completion round trip against a local server.
func BenchmarkMiniMaxTextCompletion(b *testing.B) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		jsonResponse(http.StatusOK, anthropicTextSuccessBody)(w)
	}))
	b.Cleanup(server.Close)
	provider, err := minimax.NewMiniMaxProvider(&schemas.ProviderConfig{
		NetworkConfig: schemas.NetworkConfig{BaseURL: server.URL, DefaultRequestTimeoutInSeconds: 10, AllowPrivateNetwork: true},
	}, bifrost.NewDefaultLogger(schemas.LogLevelError))
	if err != nil {
		b.Fatalf("NewMiniMaxProvider: %v", err)
	}
	key := testKey()
	req := textCompletionRequest(nil)
	b.ReportAllocs()
	for b.Loop() {
		ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
		if _, bifrostErr := provider.TextCompletion(ctx, key, req); bifrostErr != nil {
			b.Fatalf("TextCompletion: %+v", bifrostErr.Error)
		}
		ctx.Cancel()
	}
}

// Caller mistakes caught by the converters are 400s that never reach MiniMax, not 500s a client
// would retry.
func TestMiniMaxConverterValidationIsBadRequest(t *testing.T) {
	provider, captured := newMockMiniMax(t, map[string]func(http.ResponseWriter){})
	ctx := testContext(t)
	key := testKey()

	cases := map[string]func() *schemas.BifrostError{
		"speech unsupported format": func() *schemas.BifrostError {
			_, err := provider.Speech(ctx, key, speechRequest("English_Graceful_Lady", &schemas.SpeechParameters{ResponseFormat: "aac"}))
			return err
		},
		"speech stream unsupported format": func() *schemas.BifrostError {
			_, err := provider.SpeechStream(ctx, passthroughPostHook, nil, key, speechRequest("English_Graceful_Lady", &schemas.SpeechParameters{ResponseFormat: "aac"}))
			return err
		},
		"image without prompt": func() *schemas.BifrostError {
			_, err := provider.ImageGeneration(ctx, key, &schemas.BifrostImageGenerationRequest{Provider: schemas.MiniMax, Model: "image-01", Input: &schemas.ImageGenerationInput{}})
			return err
		},
		"image malformed size": func() *schemas.BifrostError {
			_, err := provider.ImageGeneration(ctx, key, &schemas.BifrostImageGenerationRequest{Provider: schemas.MiniMax, Model: "image-01", Input: &schemas.ImageGenerationInput{Prompt: "a cat"}, Params: &schemas.ImageGenerationParameters{Size: schemas.Ptr("big")}})
			return err
		},
		"video without prompt": func() *schemas.BifrostError {
			_, err := provider.VideoGeneration(ctx, key, &schemas.BifrostVideoGenerationRequest{Provider: schemas.MiniMax, Model: "MiniMax-H3", Input: &schemas.VideoGenerationInput{}})
			return err
		},
		"video bad seconds": func() *schemas.BifrostError {
			_, err := provider.VideoGeneration(ctx, key, &schemas.BifrostVideoGenerationRequest{Provider: schemas.MiniMax, Model: "MiniMax-H3", Input: &schemas.VideoGenerationInput{Prompt: "a dog"}, Params: &schemas.VideoGenerationParameters{Seconds: schemas.Ptr("six")}})
			return err
		},
	}
	for name, call := range cases {
		bifrostErr := call()
		if bifrostErr == nil {
			t.Errorf("%s: expected an error", name)
			continue
		}
		if bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status = %v, want 400 (%+v)", name, bifrostErr.StatusCode, bifrostErr.Error)
		}
	}
	if reqs := captured(); len(reqs) != 0 {
		t.Errorf("invalid requests reached MiniMax: %d", len(reqs))
	}
}

// newParkedMiniMax serves events on path and then keeps the body open, sending SSE heartbeats until
// the client goes away (or 15s pass), the way an upstream can park a connection after its final
// event. handlerDone closes when the handler returns.
func newParkedMiniMax(t *testing.T, path string, events ...string) (*minimax.MiniMaxProvider, <-chan struct{}) {
	t.Helper()
	handlerDone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		defer close(handlerDone)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		for _, e := range events {
			_, _ = w.Write([]byte("data: " + e + "\n\n"))
		}
		flusher.Flush()
		deadline := time.After(15 * time.Second)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-deadline:
				return
			case <-ticker.C:
				if _, err := w.Write([]byte(": ping\n\n")); err != nil {
					return
				}
				flusher.Flush()
			}
		}
	}))
	t.Cleanup(server.Close)
	provider, err := minimax.NewMiniMaxProvider(&schemas.ProviderConfig{
		NetworkConfig: schemas.NetworkConfig{BaseURL: server.URL, DefaultRequestTimeoutInSeconds: 10, AllowPrivateNetwork: true},
	}, bifrost.NewDefaultLogger(schemas.LogLevelError))
	if err != nil {
		t.Fatalf("NewMiniMaxProvider: %v", err)
	}
	return provider, handlerDone
}

// collectStreamWithin drains stream, failing the test if it does not close within limit.
func collectStreamWithin(t *testing.T, stream chan *schemas.BifrostStreamChunk, limit time.Duration) []*schemas.BifrostStreamChunk {
	t.Helper()
	var chunks []*schemas.BifrostStreamChunk
	timeout := time.After(limit)
	for {
		select {
		case chunk, ok := <-stream:
			if !ok {
				return chunks
			}
			chunks = append(chunks, chunk)
		case <-timeout:
			t.Fatalf("stream did not close within %s after its terminal event (cleanup is draining a parked body); got %d chunks", limit, len(chunks))
			return chunks
		}
	}
}

// A terminal MiniMax event ends the stream even when the upstream keeps the connection open:
// the channel closes promptly, the finalizer runs once, the terminal usage or error is delivered
// once, and the parked connection is abandoned rather than drained.
func TestMiniMaxStreamTerminalEventWithParkedBody(t *testing.T) {
	type outcome struct {
		wantErrStatus int
		check         func(t *testing.T, last *schemas.BifrostStreamChunk)
	}
	speechUsage := func(t *testing.T, last *schemas.BifrostStreamChunk) {
		done := last.BifrostSpeechStreamResponse
		if done == nil || done.Type != schemas.SpeechStreamResponseTypeDone || done.Usage == nil || done.Usage.InputChars != 11 {
			t.Errorf("final chunk is not speech done with usage: %+v", last)
		}
	}
	transcriptionUsage := func(t *testing.T, last *schemas.BifrostStreamChunk) {
		done := last.BifrostTranscriptionStreamResponse
		if done == nil || done.Type != schemas.TranscriptionStreamResponseTypeDone || done.Usage == nil || done.Usage.Seconds == nil || *done.Usage.Seconds != 1.5 {
			t.Errorf("final chunk is not transcription done with usage: %+v", last)
		}
	}
	cases := []struct {
		name   string
		path   string
		events []string
		start  func(p *minimax.MiniMaxProvider, ctx *schemas.BifrostContext, fin func(context.Context)) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError)
		want   outcome
	}{
		{
			name: "speech done",
			path: "/v1/t2a_v2",
			events: []string{
				`{"data":{"audio":"aa","status":1},"base_resp":{"status_code":0,"status_msg":""}}`,
				`{"data":{"audio":"","status":2},"extra_info":{"audio_length":900,"usage_characters":11},"base_resp":{"status_code":0,"status_msg":"success"}}`,
			},
			start: func(p *minimax.MiniMaxProvider, ctx *schemas.BifrostContext, fin func(context.Context)) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
				return p.SpeechStream(ctx, passthroughPostHook, fin, testKey(), speechRequest("English_Graceful_Lady", nil))
			},
			want: outcome{check: speechUsage},
		},
		{
			name: "speech terminal base_resp error",
			path: "/v1/t2a_v2",
			events: []string{
				`{"data":{"audio":"aa","status":1},"base_resp":{"status_code":0,"status_msg":""}}`,
				`{"base_resp":{"status_code":1002,"status_msg":"rate limited"}}`,
			},
			start: func(p *minimax.MiniMaxProvider, ctx *schemas.BifrostContext, fin func(context.Context)) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
				return p.SpeechStream(ctx, passthroughPostHook, fin, testKey(), speechRequest("English_Graceful_Lady", nil))
			},
			want: outcome{wantErrStatus: http.StatusTooManyRequests},
		},
		{
			name: "transcription finish",
			path: "/v1/speech_to_text",
			events: []string{
				`{"index":0,"delta":"Hello","finish":false}`,
				`{"index":1,"delta":" world.","finish":true,"duration":1.5}`,
			},
			start: func(p *minimax.MiniMaxProvider, ctx *schemas.BifrostContext, fin func(context.Context)) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
				return p.TranscriptionStream(ctx, passthroughPostHook, fin, testKey(), transcriptionRequest(nil))
			},
			want: outcome{check: transcriptionUsage},
		},
		{
			name: "transcription terminal base_resp error",
			path: "/v1/speech_to_text",
			events: []string{
				`{"index":0,"delta":"Hello","finish":false}`,
				`{"base_resp":{"status_code":1004,"status_msg":"invalid api key"}}`,
			},
			start: func(p *minimax.MiniMaxProvider, ctx *schemas.BifrostContext, fin func(context.Context)) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
				return p.TranscriptionStream(ctx, passthroughPostHook, fin, testKey(), transcriptionRequest(nil))
			},
			want: outcome{wantErrStatus: http.StatusUnauthorized},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider, handlerDone := newParkedMiniMax(t, tc.path, tc.events...)
			var finalized atomic.Int32
			stream, bifrostErr := tc.start(provider, testContext(t), func(context.Context) { finalized.Add(1) })
			if bifrostErr != nil {
				t.Fatalf("stream setup error: %+v", bifrostErr.Error)
			}
			chunks := collectStreamWithin(t, stream, 3*time.Second)
			if len(chunks) == 0 {
				t.Fatal("no chunks")
			}
			var errs []*schemas.BifrostError
			for _, c := range chunks {
				if c.BifrostError != nil {
					errs = append(errs, c.BifrostError)
				}
			}
			if tc.want.wantErrStatus != 0 {
				if len(errs) != 1 {
					t.Fatalf("got %d error chunks, want exactly 1: %+v", len(errs), errs)
				}
				if errs[0].StatusCode == nil || *errs[0].StatusCode != tc.want.wantErrStatus {
					t.Errorf("error status = %v, want %d (%+v)", errs[0].StatusCode, tc.want.wantErrStatus, errs[0].Error)
				}
			} else {
				if len(errs) != 0 {
					t.Fatalf("unexpected error chunks (a 499 or truncation would show here): %+v", errs[0].Error)
				}
				tc.want.check(t, chunks[len(chunks)-1])
			}
			// The finalizer runs in the stream goroutine's deferred cleanup, just after the
			// channel closes, so wait for it rather than reading the count at close.
			deadline := time.Now().Add(2 * time.Second)
			for finalized.Load() == 0 && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			time.Sleep(50 * time.Millisecond)
			if got := finalized.Load(); got != 1 {
				t.Errorf("finalizer ran %d times, want 1", got)
			}
			select {
			case <-handlerDone:
			case <-time.After(3 * time.Second):
				t.Error("upstream handler still running 3s after the stream closed: the parked connection was not abandoned")
			}
		})
	}
}

// The fork's earlier exported names must keep compiling with their original signatures.
var (
	_ func(*schemas.ProviderConfig, schemas.Logger) (*minimax.MinimaxProvider, error) = minimax.NewMinimaxProvider
	_ schemas.Provider                                                                = (*minimax.MinimaxProvider)(nil)
	_ *minimax.MiniMaxProvider                                                        = (*minimax.MinimaxProvider)(nil)
)

func TestMiniMaxDeprecatedForkNames(t *testing.T) {
	if schemas.Minimax != schemas.MiniMax {
		t.Fatalf("schemas.Minimax = %q, want it to alias schemas.MiniMax (%q)", schemas.Minimax, schemas.MiniMax)
	}
	provider, err := minimax.NewMinimaxProvider(&schemas.ProviderConfig{}, bifrost.NewDefaultLogger(schemas.LogLevelError))
	if err != nil {
		t.Fatalf("NewMinimaxProvider: %v", err)
	}
	if provider.GetProviderKey() != schemas.Minimax {
		t.Errorf("provider key = %q, want %q", provider.GetProviderKey(), schemas.Minimax)
	}
}

// A custom provider built on minimax with allowed_requests must refuse every operation it does not
// allow before any conversion or upstream call: video, speech and transcription are billed.
func TestMiniMaxCustomProviderAllowedRequestsGateEveryOperation(t *testing.T) {
	var upstreamCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		jsonResponse(http.StatusOK, chatSuccessBody)(w)
	}))
	t.Cleanup(server.Close)
	provider, err := minimax.NewMiniMaxProvider(&schemas.ProviderConfig{
		NetworkConfig: schemas.NetworkConfig{BaseURL: server.URL, DefaultRequestTimeoutInSeconds: 10, AllowPrivateNetwork: true},
		CustomProviderConfig: &schemas.CustomProviderConfig{
			CustomProviderKey: "my-minimax",
			BaseProviderType:  schemas.MiniMax,
			AllowedRequests:   &schemas.AllowedRequests{ChatCompletion: true},
		},
	}, bifrost.NewDefaultLogger(schemas.LogLevelError))
	if err != nil {
		t.Fatalf("NewMiniMaxProvider: %v", err)
	}
	ctx := testContext(t)
	key := testKey()
	videoReq := &schemas.BifrostVideoGenerationRequest{Provider: schemas.MiniMax, Model: "MiniMax-H3", Input: &schemas.VideoGenerationInput{Prompt: "a dog"}}

	blocked := map[schemas.RequestType]func() *schemas.BifrostError{
		schemas.ListModelsRequest: func() *schemas.BifrostError {
			_, e := provider.ListModels(ctx, []schemas.Key{key}, &schemas.BifrostListModelsRequest{Provider: schemas.MiniMax})
			return e
		},
		schemas.TextCompletionRequest: func() *schemas.BifrostError {
			_, e := provider.TextCompletion(ctx, key, textCompletionRequest(nil))
			return e
		},
		schemas.TextCompletionStreamRequest: func() *schemas.BifrostError {
			_, e := provider.TextCompletionStream(ctx, passthroughPostHook, nil, key, textCompletionRequest(nil))
			return e
		},
		schemas.ChatCompletionStreamRequest: func() *schemas.BifrostError {
			_, e := provider.ChatCompletionStream(ctx, passthroughPostHook, nil, key, userChatRequest(nil))
			return e
		},
		schemas.ResponsesRequest: func() *schemas.BifrostError {
			_, e := provider.Responses(ctx, key, &schemas.BifrostResponsesRequest{Provider: schemas.MiniMax, Model: "MiniMax-M3"})
			return e
		},
		schemas.ResponsesStreamRequest: func() *schemas.BifrostError {
			_, e := provider.ResponsesStream(ctx, passthroughPostHook, nil, key, &schemas.BifrostResponsesRequest{Provider: schemas.MiniMax, Model: "MiniMax-M3"})
			return e
		},
		schemas.ImageGenerationRequest: func() *schemas.BifrostError {
			_, e := provider.ImageGeneration(ctx, key, &schemas.BifrostImageGenerationRequest{Provider: schemas.MiniMax, Model: "image-01", Input: &schemas.ImageGenerationInput{Prompt: "a cat"}})
			return e
		},
		schemas.VideoGenerationRequest: func() *schemas.BifrostError {
			_, e := provider.VideoGeneration(ctx, key, videoReq)
			return e
		},
		schemas.VideoRetrieveRequest: func() *schemas.BifrostError {
			_, e := provider.VideoRetrieve(ctx, key, &schemas.BifrostVideoRetrieveRequest{Provider: schemas.MiniMax, ID: "t1"})
			return e
		},
		schemas.VideoDownloadRequest: func() *schemas.BifrostError {
			_, e := provider.VideoDownload(ctx, key, &schemas.BifrostVideoDownloadRequest{Provider: schemas.MiniMax, ID: "t1"})
			return e
		},
		schemas.VideoDeleteRequest: func() *schemas.BifrostError {
			_, e := provider.VideoDelete(ctx, key, &schemas.BifrostVideoDeleteRequest{Provider: schemas.MiniMax, ID: "t1"})
			return e
		},
		schemas.SpeechRequest: func() *schemas.BifrostError {
			_, e := provider.Speech(ctx, key, speechRequest("English_Graceful_Lady", nil))
			return e
		},
		schemas.SpeechStreamRequest: func() *schemas.BifrostError {
			_, e := provider.SpeechStream(ctx, passthroughPostHook, nil, key, speechRequest("English_Graceful_Lady", nil))
			return e
		},
		schemas.TranscriptionRequest: func() *schemas.BifrostError {
			_, e := provider.Transcription(ctx, key, transcriptionRequest(nil))
			return e
		},
		schemas.TranscriptionStreamRequest: func() *schemas.BifrostError {
			_, e := provider.TranscriptionStream(ctx, passthroughPostHook, nil, key, transcriptionRequest(nil))
			return e
		},
	}
	for op, call := range blocked {
		e := call()
		if e == nil || e.Error == nil || e.Error.Code == nil || *e.Error.Code != "unsupported_operation" {
			t.Errorf("%s: want unsupported_operation, got %+v", op, e)
			continue
		}
		if !strings.Contains(e.Error.Message, "my-minimax") {
			t.Errorf("%s: error should name the custom provider: %q", op, e.Error.Message)
		}
	}
	if got := upstreamCalls.Load(); got != 0 {
		t.Fatalf("blocked operations reached the upstream %d times", got)
	}

	if _, e := provider.ChatCompletion(ctx, key, userChatRequest(nil)); e != nil {
		t.Fatalf("allowed chat completion failed: %+v", e.Error)
	}
	if got := upstreamCalls.Load(); got != 1 {
		t.Errorf("allowed chat completion made %d upstream calls, want 1", got)
	}
}

// The download URL comes from MiniMax's task response, so a task pointing at a loopback or private
// address must be refused without that address ever being dialed.
func TestMiniMaxVideoDownloadRefusesNonPublicURL(t *testing.T) {
	var internalHits atomic.Int32
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		internalHits.Add(1)
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write([]byte("internal secret"))
	}))
	t.Cleanup(internal.Close)
	provider, _ := newMockMiniMax(t, map[string]func(http.ResponseWriter){
		"GET /v2/query/video_generation/424010985738629": jsonResponse(http.StatusOK, `{"task":{"id":"424010985738629","model":"MiniMax-H3","status":"succeeded","content":{"url":"`+internal.URL+`/v.mp4?sig=abc123"},"resolution":"768P","ratio":"16:9","duration":6}}`),
	})

	_, bifrostErr := provider.VideoDownload(testContext(t), testKey(), &schemas.BifrostVideoDownloadRequest{Provider: schemas.MiniMax, ID: "424010985738629"})
	if bifrostErr == nil {
		t.Fatal("expected the loopback download URL to be refused")
	}
	if got := internalHits.Load(); got != 0 {
		t.Fatalf("download dialed the loopback URL %d times", got)
	}
	msg := bifrostErr.Error.Message
	if bifrostErr.Error.Error != nil {
		msg += " " + bifrostErr.Error.Error.Error()
	}
	if strings.Contains(msg, "abc123") {
		t.Errorf("error leaks the signed query: %s", msg)
	}
}

// A custom provider built on minimax reports its own name, so the video IDs it issues carry the
// custom suffix and the ownership rows governance records use the same scope as later lookups.
func TestMiniMaxCustomProviderReportsItsOwnKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		switch r.Method + " " + r.URL.Path {
		case "POST /v2/video_generation":
			jsonResponse(http.StatusOK, `{"task_id":"424010985738629","base_resp":{"status_code":0}}`)(w)
		case "GET /v2/query/video_generation/424010985738629":
			jsonResponse(http.StatusOK, `{"task":{"id":"424010985738629","model":"MiniMax-H3","status":"processing"}}`)(w)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	newProvider := func(custom *schemas.CustomProviderConfig) *minimax.MiniMaxProvider {
		p, err := minimax.NewMiniMaxProvider(&schemas.ProviderConfig{
			NetworkConfig:        schemas.NetworkConfig{BaseURL: server.URL, DefaultRequestTimeoutInSeconds: 10, AllowPrivateNetwork: true},
			CustomProviderConfig: custom,
		}, bifrost.NewDefaultLogger(schemas.LogLevelError))
		if err != nil {
			t.Fatalf("NewMiniMaxProvider: %v", err)
		}
		return p
	}

	if got := newProvider(nil).GetProviderKey(); got != schemas.MiniMax {
		t.Errorf("built-in provider key = %q, want minimax", got)
	}

	custom := newProvider(&schemas.CustomProviderConfig{CustomProviderKey: "my-minimax", BaseProviderType: schemas.MiniMax})
	if got := custom.GetProviderKey(); got != "my-minimax" {
		t.Fatalf("custom provider key = %q, want my-minimax", got)
	}
	ctx := testContext(t)
	created, bifrostErr := custom.VideoGeneration(ctx, testKey(), &schemas.BifrostVideoGenerationRequest{Provider: "my-minimax", Model: "MiniMax-H3", Input: &schemas.VideoGenerationInput{Prompt: "a dog"}})
	if bifrostErr != nil {
		t.Fatalf("VideoGeneration: %+v", bifrostErr.Error)
	}
	if want := providerUtils.AddVideoIDProviderSuffix("424010985738629", "my-minimax"); created.ID != want {
		t.Errorf("created id = %q, want %q (custom suffix)", created.ID, want)
	}
	retrieved, bifrostErr := custom.VideoRetrieve(ctx, testKey(), &schemas.BifrostVideoRetrieveRequest{Provider: "my-minimax", ID: created.ID})
	if bifrostErr != nil {
		t.Fatalf("VideoRetrieve: %+v", bifrostErr.Error)
	}
	if retrieved.ID != created.ID {
		t.Errorf("retrieved id = %q, want the created id %q", retrieved.ID, created.ID)
	}
}
