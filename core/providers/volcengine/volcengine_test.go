package volcengine_test

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/internal/llmtests"
	"github.com/capsohq/bifrost/core/network/proxytest"
	"github.com/capsohq/bifrost/core/providers/byteplus"
	"github.com/capsohq/bifrost/core/providers/volcengine"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/tidwall/gjson"
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
		{"GET video", "GET /contents/generations/tasks/", "", func(id string) *schemas.BifrostError {
			_, bifrostErr := provider.VideoRetrieve(newCtx(), key, &schemas.BifrostVideoRetrieveRequest{ID: id})
			return bifrostErr
		}},
		// Ark has no content endpoint: a download reads the task, then fetches its content.video_url (TXE-7493).
		{"GET video content", "GET /contents/generations/tasks/", "", func(id string) *schemas.BifrostError {
			_, bifrostErr := provider.VideoDownload(newCtx(), key, &schemas.BifrostVideoDownloadRequest{ID: id})
			return bifrostErr
		}},
		{"DELETE video", "DELETE /contents/generations/tasks/", "", func(id string) *schemas.BifrostError {
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

// TestArkVideoIsTheSameTaskOnEveryArkProvider is TXE-7493: volcengine and modelark serve
// Seedance's task API, the one byteplus serves, so one Ark task reads back as the same task
// object on all three. The create body is Ark's (content[], duration), not OpenAI's multipart; a
// succeeded task is completed with its WxH size, seconds and usage; the download fetches the
// task's content.video_url through the hardened media fetcher with no credential; and cancel,
// list and delete go to the task path. Red before: volcengine and modelark sent an OpenAI
// multipart body, and the same succeeded task read back as in_progress with no seconds, size or
// usage.
func TestArkVideoIsTheSameTaskOnEveryArkProvider(t *testing.T) {
	const taskID = "cgt-ark-1"
	const videoURL = "http://203.0.113.10/files/v.mp4"
	videoProxy := proxytest.ForwardProxy(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host != "203.0.113.10" || r.URL.Path != "/files/v.mp4" || r.Header.Get("Authorization") != "" {
			http.Error(w, "unexpected download "+r.URL.String(), http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write([]byte("mp4data"))
	})
	type call struct{ method, path, contentType, body string }
	var mu sync.Mutex
	var calls []call
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls = append(calls, call{r.Method, r.URL.Path, r.Header.Get("Content-Type"), string(body)})
		mu.Unlock()
		tasks := strings.TrimPrefix(r.URL.Path, "/api/v3")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && tasks == "/contents/generations/tasks":
			_, _ = w.Write([]byte(`{"id":"` + taskID + `"}`))
		case r.Method == http.MethodGet && tasks == "/contents/generations/tasks/"+taskID:
			_, _ = w.Write([]byte(`{"id":"` + taskID + `","model":"doubao-seedance-1-0-pro-250528","status":"succeeded","content":{"video_url":"` + videoURL + `"},"usage":{"completion_tokens":108900,"total_tokens":108900},"resolution":"720p","ratio":"16:9","duration":5,"created_at":100,"updated_at":200}`))
		case r.Method == http.MethodGet && tasks == "/contents/generations/tasks":
			_, _ = w.Write([]byte(`{"items":[{"id":"` + taskID + `","model":"doubao-seedance-1-0-pro-250528","status":"queued","created_at":100}],"total":1}`))
		case r.Method == http.MethodDelete && tasks == "/contents/generations/tasks/"+taskID:
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, `{"error":{"code":"NotFound","message":"no route"}}`, http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	logger := bifrost.NewDefaultLogger(schemas.LogLevelError)
	config := func(base string) *schemas.ProviderConfig {
		return &schemas.ProviderConfig{NetworkConfig: schemas.NetworkConfig{BaseURL: base, AllowPrivateNetwork: true}}
	}
	bp, err := byteplus.NewBytePlusProvider(config(srv.URL), logger)
	if err != nil {
		t.Fatal(err)
	}
	ve, err := volcengine.NewVolcengineProvider(config(srv.URL+"/api/v3"), logger)
	if err != nil {
		t.Fatal(err)
	}
	ma, err := volcengine.NewModelArkProvider(config(srv.URL+"/api/v3"), logger)
	if err != nil {
		t.Fatal(err)
	}
	type videoProvider interface {
		GetProviderKey() schemas.ModelProvider
		VideoGeneration(*schemas.BifrostContext, schemas.Key, *schemas.BifrostVideoGenerationRequest) (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError)
		VideoRetrieve(*schemas.BifrostContext, schemas.Key, *schemas.BifrostVideoRetrieveRequest) (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError)
		VideoDownload(*schemas.BifrostContext, schemas.Key, *schemas.BifrostVideoDownloadRequest) (*schemas.BifrostVideoDownloadResponse, *schemas.BifrostError)
		VideoList(*schemas.BifrostContext, schemas.Key, *schemas.BifrostVideoListRequest) (*schemas.BifrostVideoListResponse, *schemas.BifrostError)
		VideoCancel(*schemas.BifrostContext, schemas.Key, *schemas.BifrostVideoCancelRequest) (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError)
		VideoDelete(*schemas.BifrostContext, schemas.Key, *schemas.BifrostVideoDeleteRequest) (*schemas.BifrostVideoDeleteResponse, *schemas.BifrostError)
	}
	key := schemas.Key{Value: *schemas.NewSecretVar("ark-key")}
	newCtx := func() *schemas.BifrostContext {
		ctx := schemas.NewBifrostContext(t.Context(), schemas.NoDeadline)
		ctx.SetValue(schemas.BifrostContextKeyProviderProxyConfig, &schemas.ProxyConfig{Type: schemas.HTTPProxy, URL: schemas.NewSecretVar(videoProxy.URL)})
		return ctx
	}
	// The task object as each provider reports it, with its own suffix taken off the ids.
	normalized := func(p videoProvider, resp *schemas.BifrostVideoGenerationResponse) string {
		copied := *resp
		copied.ID = strings.TrimSuffix(copied.ID, ":"+string(p.GetProviderKey()))
		copied.ExtraFields = schemas.BifrostResponseExtraFields{}
		out, _ := json.Marshal(copied)
		return string(out)
	}
	var want string
	for _, p := range []videoProvider{bp, ve, ma} {
		name := string(p.GetProviderKey())
		calls = nil
		created, bifrostErr := p.VideoGeneration(newCtx(), key, &schemas.BifrostVideoGenerationRequest{
			Provider: p.GetProviderKey(),
			Model:    "doubao-seedance-1-0-pro-250528",
			Input:    &schemas.VideoGenerationInput{Prompt: "a dog"},
			Params:   &schemas.VideoGenerationParameters{Seconds: schemas.Ptr("5")},
		})
		if bifrostErr != nil {
			t.Fatalf("%s create: %+v", name, bifrostErr.Error)
		}
		if created.ID != taskID+":"+name || created.Status != schemas.VideoStatusQueued {
			t.Fatalf("%s create = %+v", name, created)
		}
		if len(calls) != 1 || calls[0].contentType != "application/json" || gjson.Get(calls[0].body, "content.0.text").String() != "a dog" || gjson.Get(calls[0].body, "duration").Int() != 5 {
			t.Fatalf("%s create sent %+v, want Ark's JSON body with content[] and duration", name, calls)
		}

		retrieved, bifrostErr := p.VideoRetrieve(newCtx(), key, &schemas.BifrostVideoRetrieveRequest{Provider: p.GetProviderKey(), ID: created.ID})
		if bifrostErr != nil {
			t.Fatalf("%s retrieve: %+v", name, bifrostErr.Error)
		}
		if retrieved.Status != schemas.VideoStatusCompleted || retrieved.Size != "1280x720" || retrieved.Seconds == nil || *retrieved.Seconds != "5" {
			t.Fatalf("%s retrieve = status %q size %q seconds %v, want completed 1280x720 5", name, retrieved.Status, retrieved.Size, retrieved.Seconds)
		}
		got := normalized(p, retrieved)
		if want == "" {
			want = got
		} else if got != want {
			t.Fatalf("%s reads the task as\n%s\nbyteplus reads it as\n%s", name, got, want)
		}

		downloaded, bifrostErr := p.VideoDownload(newCtx(), key, &schemas.BifrostVideoDownloadRequest{Provider: p.GetProviderKey(), ID: created.ID})
		if bifrostErr != nil {
			t.Fatalf("%s download: %+v", name, bifrostErr.Error)
		}
		if string(downloaded.Content) != "mp4data" || downloaded.ContentType != "video/mp4" {
			t.Fatalf("%s download = %q %q", name, downloaded.Content, downloaded.ContentType)
		}

		listed, bifrostErr := p.VideoList(newCtx(), key, &schemas.BifrostVideoListRequest{Provider: p.GetProviderKey(), Limit: schemas.Ptr(2)})
		if bifrostErr != nil {
			t.Fatalf("%s list: %+v", name, bifrostErr.Error)
		}
		if len(listed.Data) != 1 || listed.Data[0].ID != taskID+":"+name || listed.Data[0].Status != schemas.VideoStatusQueued {
			t.Fatalf("%s list = %+v", name, listed.Data)
		}

		// The task reads completed, so cancel is refused before any DELETE is sent.
		calls = nil
		if _, bifrostErr := p.VideoCancel(newCtx(), key, &schemas.BifrostVideoCancelRequest{Provider: p.GetProviderKey(), ID: created.ID}); bifrostErr == nil {
			t.Fatalf("%s cancelled a completed task", name)
		}
		for _, c := range calls {
			if c.method == http.MethodDelete {
				t.Fatalf("%s cancel of a completed task sent DELETE", name)
			}
		}

		// The gateway hands the provider the bare task id; the answer names the task with the
		// provider's suffix either way, as the create did.
		for _, id := range []string{created.ID, taskID} {
			deleted, bifrostErr := p.VideoDelete(newCtx(), key, &schemas.BifrostVideoDeleteRequest{Provider: p.GetProviderKey(), ID: id})
			if bifrostErr != nil || !deleted.Deleted || deleted.ID != created.ID {
				t.Fatalf("%s delete of %q = %+v, %+v", name, id, deleted, bifrostErr)
			}
		}
	}
}

// TestArkVideoDraftTaskIDIsSentBare is the Codex P2 on #332 (TXE-7493): a caller finalizing a
// Seedance draft passes the id Bifrost returned, which carries the provider's suffix. Ark must
// receive the bare task id in content[].draft_task.id whichever suffix it carries: the built-in
// provider's, a custom provider's own name, or that custom provider's base. The caller's extra
// params are left as they were.
func TestArkVideoDraftTaskIDIsSentBare(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(body))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"cgt-final"}`))
	}))
	t.Cleanup(srv.Close)
	logger := bifrost.NewDefaultLogger(schemas.LogLevelError)
	newProvider := func(base schemas.ModelProvider, custom string) *volcengine.VolcengineProvider {
		config := &schemas.ProviderConfig{NetworkConfig: schemas.NetworkConfig{BaseURL: srv.URL + "/api/v3", AllowPrivateNetwork: true}}
		if custom != "" {
			config.CustomProviderConfig = &schemas.CustomProviderConfig{CustomProviderKey: custom, BaseProviderType: base}
		}
		var p *volcengine.VolcengineProvider
		var err error
		if base == schemas.ModelArk {
			p, err = volcengine.NewModelArkProvider(config, logger)
		} else {
			p, err = volcengine.NewVolcengineProvider(config, logger)
		}
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	cases := []struct {
		name     string
		provider *volcengine.VolcengineProvider
		draftID  string
	}{
		{"volcengine", newProvider(schemas.Volcengine, ""), "cgt-draft-1:volcengine"},
		{"modelark", newProvider(schemas.ModelArk, ""), "cgt-draft-1:modelark"},
		{"custom provider's own name", newProvider(schemas.Volcengine, "tkp_ark"), "cgt-draft-1:tkp_ark"},
		{"custom provider's base", newProvider(schemas.Volcengine, "tkp_ark"), "cgt-draft-1:volcengine"},
		{"bare", newProvider(schemas.Volcengine, ""), "cgt-draft-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mu.Lock()
			bodies = nil
			mu.Unlock()
			extra := map[string]interface{}{"draft_task_id": tc.draftID}
			_, bifrostErr := tc.provider.VideoGeneration(schemas.NewBifrostContext(t.Context(), schemas.NoDeadline), schemas.Key{Value: *schemas.NewSecretVar("k")}, &schemas.BifrostVideoGenerationRequest{
				Provider: tc.provider.GetProviderKey(),
				Model:    "doubao-seedance-1-0-pro-250528",
				Input:    &schemas.VideoGenerationInput{},
				Params:   &schemas.VideoGenerationParameters{ExtraParams: extra},
			})
			if bifrostErr != nil {
				t.Fatalf("create: %+v", bifrostErr.Error)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(bodies) != 1 {
				t.Fatalf("%d calls reached Ark, want 1", len(bodies))
			}
			if got := gjson.Get(bodies[0], `content.#(type=="draft_task").draft_task.id`).String(); got != "cgt-draft-1" {
				t.Fatalf("Ark received draft_task.id %q, want the bare cgt-draft-1: %s", got, bodies[0])
			}
			if extra["draft_task_id"] != tc.draftID {
				t.Fatalf("the caller's extra params were modified: %v", extra)
			}
		})
	}
}

// BenchmarkVolcengineVideoRequestPath measures a VolcEngine video create and query end to end
// through the provider against a loopback Ark server (TXE-7493): request conversion, the HTTP
// call and the response conversion. The same benchmark runs on main, where the calls went
// through the OpenAI video handlers, for the before figures in the PR.
func BenchmarkVolcengineVideoRequestPath(b *testing.B) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			_, _ = w.Write([]byte(`{"id":"cgt-bench"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"cgt-bench","model":"doubao-seedance-1-0-pro-250528","status":"succeeded","content":{"video_url":"https://x.invalid/v.mp4"},"usage":{"completion_tokens":108900,"total_tokens":108900},"resolution":"720p","ratio":"16:9","duration":5,"created_at":100,"updated_at":200}`))
	}))
	b.Cleanup(srv.Close)
	p, err := volcengine.NewVolcengineProvider(&schemas.ProviderConfig{NetworkConfig: schemas.NetworkConfig{BaseURL: srv.URL + "/api/v3", AllowPrivateNetwork: true}}, bifrost.NewDefaultLogger(schemas.LogLevelError))
	if err != nil {
		b.Fatal(err)
	}
	key := schemas.Key{Value: *schemas.NewSecretVar("k")}
	create := &schemas.BifrostVideoGenerationRequest{
		Provider: schemas.Volcengine,
		Model:    "doubao-seedance-1-0-pro-250528",
		Input:    &schemas.VideoGenerationInput{Prompt: "a paper boat drifting down a rain gutter"},
		Params:   &schemas.VideoGenerationParameters{Seconds: schemas.Ptr("5")},
	}
	retrieve := &schemas.BifrostVideoRetrieveRequest{Provider: schemas.Volcengine, ID: "cgt-bench:volcengine"}
	b.Run("create", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, bifrostErr := p.VideoGeneration(schemas.NewBifrostContext(context.Background(), schemas.NoDeadline), key, create); bifrostErr != nil {
				b.Fatal(bifrostErr.Error)
			}
		}
	})
	b.Run("retrieve", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, bifrostErr := p.VideoRetrieve(schemas.NewBifrostContext(context.Background(), schemas.NoDeadline), key, retrieve); bifrostErr != nil {
				b.Fatal(bifrostErr.Error)
			}
		}
	})
}
