package volcengine_test

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
	"github.com/capsohq/bifrost/core/providers/volcengine"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/tidwall/gjson"
)

// A Seedance task on a ModelArk custom provider is created with Ark's JSON task body under the
// base URL's API root, and its video ID carries the custom provider's name, which its query
// and its cancellation accept.
func TestModelArkSeedanceUsesArkTaskBodyUnderItsOwnName(t *testing.T) {
	var mu sync.Mutex
	type seen struct {
		method, path, contentType string
		body                      []byte
	}
	var requests []seen
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		requests = append(requests, seen{r.Method, r.URL.Path, r.Header.Get("Content-Type"), body})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodPost:
			_, _ = w.Write([]byte(`{"id":"cgt-20261104-abcd"}`))
		case http.MethodGet:
			_, _ = w.Write([]byte(`{"id":"cgt-20261104-abcd","model":"seedance-1-5-pro-251215","status":"succeeded",
				"content":{"video_url":"https://ark-content.example/v.mp4"},"usage":{"completion_tokens":108900,"total_tokens":108900},
				"created_at":1743414619,"updated_at":1743414673,"resolution":"720p","ratio":"16:9","duration":5}`))
		case http.MethodDelete:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer server.Close()

	provider, err := volcengine.NewModelArkProvider(&schemas.ProviderConfig{
		NetworkConfig:        schemas.NetworkConfig{BaseURL: server.URL + "/api/v3", AllowPrivateNetwork: true, DefaultRequestTimeoutInSeconds: 10},
		CustomProviderConfig: &schemas.CustomProviderConfig{BaseProviderType: schemas.ModelArk, CustomProviderKey: "tkp_9"},
	}, bifrost.NewDefaultLogger(schemas.LogLevelError))
	if err != nil {
		t.Fatal(err)
	}
	if provider.GetProviderKey() != "tkp_9" {
		t.Fatalf("provider key %q", provider.GetProviderKey())
	}
	ctx, cancel := schemas.NewBifrostContextWithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	key := schemas.Key{Value: *schemas.NewSecretVar("ark-key"), Models: []string{"*"}}

	created, bifrostErr := provider.VideoGeneration(ctx, key, &schemas.BifrostVideoGenerationRequest{
		Model:  "seedance-1-5-pro-251215",
		Input:  &schemas.VideoGenerationInput{Prompt: "The kitten is yawning at the camera.", InputReference: schemas.Ptr("https://example.com/kitten.png")},
		Params: &schemas.VideoGenerationParameters{Seconds: schemas.Ptr("5")},
	})
	if bifrostErr != nil {
		t.Fatalf("create: %+v", bifrostErr.Error)
	}
	if created.ID != "cgt-20261104-abcd:tkp_9" {
		t.Fatalf("video id %q", created.ID)
	}
	task, bifrostErr := provider.VideoRetrieve(ctx, key, &schemas.BifrostVideoRetrieveRequest{ID: created.ID})
	if bifrostErr != nil || task.Status != schemas.VideoStatusCompleted || task.ID != created.ID {
		t.Fatalf("retrieve: %+v %+v", task, bifrostErr)
	}
	if _, bifrostErr := provider.VideoDelete(ctx, key, &schemas.BifrostVideoDeleteRequest{ID: created.ID}); bifrostErr != nil {
		t.Fatalf("delete: %+v", bifrostErr.Error)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 3 {
		t.Fatalf("requests %+v", requests)
	}
	create := requests[0]
	if create.method != http.MethodPost || create.path != "/api/v3/contents/generations/tasks" || !strings.HasPrefix(create.contentType, "application/json") {
		t.Fatalf("create went as %s %s %s", create.method, create.path, create.contentType)
	}
	if gjson.GetBytes(create.body, "content.0.type").Str != "text" || gjson.GetBytes(create.body, "content.0.text").Str != "The kitten is yawning at the camera." ||
		gjson.GetBytes(create.body, "content.1.type").Str != "image_url" || gjson.GetBytes(create.body, "content.1.image_url.url").Str != "https://example.com/kitten.png" ||
		gjson.GetBytes(create.body, "content.1.role").Str != "first_frame" || gjson.GetBytes(create.body, "duration").Int() != 5 {
		t.Fatalf("create body %s", create.body)
	}
	for i, want := range []string{"GET /api/v3/contents/generations/tasks/cgt-20261104-abcd", "DELETE /api/v3/contents/generations/tasks/cgt-20261104-abcd"} {
		if got := requests[i+1].method + " " + requests[i+1].path; got != want {
			t.Fatalf("request %d: %s, want %s", i+1, got, want)
		}
	}
}
