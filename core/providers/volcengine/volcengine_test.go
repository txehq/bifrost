package volcengine_test

import (
	"net"
	"os"
	"strings"
	"sync"
	"testing"

	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/internal/llmtests"
	"github.com/capsohq/bifrost/core/providers/volcengine"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

func envOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func TestVolcengine(t *testing.T) {
	t.Parallel()

	if strings.TrimSpace(os.Getenv("VOLCENGINE_API_KEY")) == "" {
		t.Skip("Skipping Volcengine tests because VOLCENGINE_API_KEY is not set")
	}

	client, ctx, cancel, err := llmtests.SetupTest()
	if err != nil {
		t.Fatalf("Error initializing test setup: %v", err)
	}
	defer cancel()

	testConfig := llmtests.ComprehensiveTestConfig{
		Provider:             schemas.Volcengine,
		ChatModel:            envOrDefault("VOLCENGINE_CHAT_MODEL", "doubao-seed-1-6-250615"),
		TextModel:            envOrDefault("VOLCENGINE_TEXT_MODEL", "doubao-seed-1-6-250615"),
		VisionModel:          envOrDefault("VOLCENGINE_VISION_MODEL", "doubao-1.5-vision-pro-250328"),
		EmbeddingModel:       envOrDefault("VOLCENGINE_EMBEDDING_MODEL", "doubao-embedding-large-text-240915"),
		ImageGenerationModel: envOrDefault("VOLCENGINE_IMAGE_MODEL", "doubao-seedream-4-5-251128"),
		VideoGenerationModel: envOrDefault("VOLCENGINE_VIDEO_MODEL", "doubao-seedance-1-0-lite-i2v-250428"),
		Scenarios: llmtests.TestScenarios{
			TextCompletion:        true,
			TextCompletionStream:  true,
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
			MultipleImages:        true,
			Embedding:             true,
			ListModels:            true,
			ImageGeneration:       true,
			FileUpload:            true,
			FileList:              true,
			FileRetrieve:          true,
			FileDelete:            true,
			FileContent:           true,
			VideoGeneration:       true,
			VideoRetrieve:         true,
			VideoDownload:         true,
			VideoList:             true,
			VideoDelete:           true,
		},
		DisableParallelFor: []string{"VideoGeneration", "VideoRetrieve", "VideoDownload", "VideoList", "VideoDelete"},
	}

	t.Run("VolcengineTests", func(t *testing.T) {
		llmtests.RunAllComprehensiveTests(t, client, ctx, testConfig)
	})
	client.Shutdown()
}

// TestVolcengineResourceIDsStayOnExpectedEndpoint applies upstream #7307's contract to the
// fork's Volcengine/ModelArk file and video handlers: a path-shaping ID is refused with a 400
// before anything is dispatched, and a valid ID reaches the upstream as one escaped segment.
func TestVolcengineResourceIDsStayOnExpectedEndpoint(t *testing.T) {
	t.Parallel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var mu sync.Mutex
	var received []string
	go fasthttp.Serve(ln, func(ctx *fasthttp.RequestCtx) { //nolint:errcheck
		mu.Lock()
		received = append(received, string(ctx.Method())+" "+string(ctx.Request.Header.RequestURI()))
		mu.Unlock()
		ctx.SetBodyString(`{"id":"x"}`)
	})

	config := &schemas.ProviderConfig{NetworkConfig: schemas.NetworkConfig{BaseURL: "http://" + ln.Addr().String(), AllowPrivateNetwork: true}}
	provider, err := volcengine.NewVolcengineProvider(config, bifrost.NewDefaultLogger(schemas.LogLevelError))
	if err != nil {
		t.Fatal(err)
	}
	key := schemas.Key{Value: *schemas.NewSecretVar("ark-test")}
	keys := []schemas.Key{key}
	newCtx := func() *schemas.BifrostContext { return schemas.NewBifrostContext(t.Context(), schemas.NoDeadline) }

	operations := []struct {
		name   string
		prefix string
		suffix string
		call   func(string) *schemas.BifrostError
	}{
		{"GET file", "GET /files/", "", func(id string) *schemas.BifrostError {
			_, bifrostErr := provider.FileRetrieve(newCtx(), keys, &schemas.BifrostFileRetrieveRequest{FileID: id})
			return bifrostErr
		}},
		{"DELETE file", "DELETE /files/", "", func(id string) *schemas.BifrostError {
			_, bifrostErr := provider.FileDelete(newCtx(), keys, &schemas.BifrostFileDeleteRequest{FileID: id})
			return bifrostErr
		}},
		{"GET file content", "GET /files/", "/content", func(id string) *schemas.BifrostError {
			_, bifrostErr := provider.FileContent(newCtx(), keys, &schemas.BifrostFileContentRequest{FileID: id})
			return bifrostErr
		}},
		// Video goes to Ark's task API under the host's API root; a download reads the task
		// first, whose URL it would then fetch.
		{"GET video", "GET /api/v3/contents/generations/tasks/", "", func(id string) *schemas.BifrostError {
			_, bifrostErr := provider.VideoRetrieve(newCtx(), key, &schemas.BifrostVideoRetrieveRequest{ID: id})
			return bifrostErr
		}},
		{"GET video content", "GET /api/v3/contents/generations/tasks/", "", func(id string) *schemas.BifrostError {
			_, bifrostErr := provider.VideoDownload(newCtx(), key, &schemas.BifrostVideoDownloadRequest{ID: id})
			return bifrostErr
		}},
		{"DELETE video", "DELETE /api/v3/contents/generations/tasks/", "", func(id string) *schemas.BifrostError {
			_, bifrostErr := provider.VideoDelete(newCtx(), key, &schemas.BifrostVideoDeleteRequest{ID: id})
			return bifrostErr
		}},
	}
	invalidIDs := []string{"../models?#", "%2e%2e%2fmodels", "../models", "/models", "..", "?limit=100", "#fragment", "\\models"}
	validIDs := map[string]string{"obj-123_ABC": "obj-123_ABC", "obj 123": "obj%20123"}

	for _, operation := range operations {
		for _, id := range invalidIDs {
			bifrostErr := operation.call(id)
			if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != 400 {
				t.Fatalf("%s: ID %q returned %+v, want a 400", operation.name, id, bifrostErr)
			}
		}
		mu.Lock()
		if len(received) != 0 {
			t.Fatalf("%s: invalid IDs were dispatched upstream: %v", operation.name, received)
		}
		mu.Unlock()

		for id, segment := range validIDs {
			operation.call(id) //nolint:errcheck
			mu.Lock()
			want := operation.prefix + segment + operation.suffix
			if len(received) != 1 || received[0] != want {
				t.Fatalf("%s: ID %q reached upstream as %v, want %q", operation.name, id, received, want)
			}
			received = nil
			mu.Unlock()
		}
	}
}
