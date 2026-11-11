package openai

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	"github.com/capsohq/bifrost/core/schemas"
)

// resourceVendor records the method and path of every call and answers each with an empty
// JSON object. The tests here assert where a call went, not what came back.
type resourceVendor struct {
	*httptest.Server
	mu    sync.Mutex
	calls []string
}

func newResourceVendor(t *testing.T) *resourceVendor {
	t.Helper()
	v := &resourceVendor{}
	v.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v.mu.Lock()
		v.calls = append(v.calls, r.Method+" "+r.URL.Path)
		v.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{}`)
	}))
	t.Cleanup(v.Close)
	return v
}

func (v *resourceVendor) takeCalls() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	calls := v.calls
	v.calls = nil
	return calls
}

func newResourceTestProvider(v *resourceVendor, custom bool, overrides map[schemas.RequestType]string) (*OpenAIProvider, *schemas.BifrostContext, schemas.Key) {
	config := &schemas.ProviderConfig{NetworkConfig: schemas.NetworkConfig{BaseURL: v.URL, DefaultRequestTimeoutInSeconds: 30}}
	if custom {
		config.CustomProviderConfig = &schemas.CustomProviderConfig{
			CustomProviderKey:    "tkp_video_vendor",
			BaseProviderType:     schemas.OpenAI,
			RequestPathOverrides: overrides,
		}
	}
	provider := NewOpenAIProvider(config, testNoopLogger{})
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	return provider, ctx, schemas.Key{ID: "key-1", Name: "tk-key-1", Value: *schemas.NewSecretVar("sk-test")}
}

// resourceCall is one OpenAI call whose default path names an id, outside the batch and
// file calls (batch_test.go covers those).
type resourceCall struct {
	// defaultCall is the upstream call of the built-in provider.
	defaultCall string
	// template is a request_path_overrides entry off /v1, and templatedCall the upstream
	// call it must produce. The two-id templates name the file before the container, so an
	// id substituted by position instead of by placeholder lands in the wrong segment.
	template      string
	templatedCall string
	call          func(p *OpenAIProvider, ctx *schemas.BifrostContext, key schemas.Key) *schemas.BifrostError
}

var resourceCalls = map[schemas.RequestType]resourceCall{
	schemas.ModelRetrieveRequest: {"GET /v1/models/glm-video-1", "/api/v3/models/{model}", "GET /api/v3/models/glm-video-1",
		func(p *OpenAIProvider, ctx *schemas.BifrostContext, key schemas.Key) *schemas.BifrostError {
			_, err := p.ModelRetrieve(ctx, key, &schemas.BifrostModelRetrieveRequest{Model: "glm-video-1"})
			return err
		}},
	schemas.VideoRetrieveRequest: {"GET /v1/videos/video_1", "/api/v3/tasks/{video_id}", "GET /api/v3/tasks/video_1",
		func(p *OpenAIProvider, ctx *schemas.BifrostContext, key schemas.Key) *schemas.BifrostError {
			_, err := p.VideoRetrieve(ctx, key, &schemas.BifrostVideoRetrieveRequest{ID: "video_1"})
			return err
		}},
	schemas.VideoDownloadRequest: {"GET /v1/videos/video_1/content", "/api/v3/tasks/{video_id}/file", "GET /api/v3/tasks/video_1/file",
		func(p *OpenAIProvider, ctx *schemas.BifrostContext, key schemas.Key) *schemas.BifrostError {
			_, err := p.VideoDownload(ctx, key, &schemas.BifrostVideoDownloadRequest{ID: "video_1"})
			return err
		}},
	schemas.VideoDeleteRequest: {"DELETE /v1/videos/video_1", "/api/v3/tasks/{video_id}", "DELETE /api/v3/tasks/video_1",
		func(p *OpenAIProvider, ctx *schemas.BifrostContext, key schemas.Key) *schemas.BifrostError {
			_, err := p.VideoDelete(ctx, key, &schemas.BifrostVideoDeleteRequest{ID: "video_1"})
			return err
		}},
	schemas.VideoRemixRequest: {"POST /v1/videos/video_1/remix", "/api/v3/tasks/{video_id}/remix", "POST /api/v3/tasks/video_1/remix",
		func(p *OpenAIProvider, ctx *schemas.BifrostContext, key schemas.Key) *schemas.BifrostError {
			_, err := p.VideoRemix(ctx, key, &schemas.BifrostVideoRemixRequest{ID: "video_1", Input: &schemas.VideoGenerationInput{Prompt: "again"}})
			return err
		}},
	schemas.ContainerRetrieveRequest: {"GET /v1/containers/cntr_1", "/api/v3/boxes/{container_id}", "GET /api/v3/boxes/cntr_1",
		func(p *OpenAIProvider, ctx *schemas.BifrostContext, key schemas.Key) *schemas.BifrostError {
			_, err := p.ContainerRetrieve(ctx, []schemas.Key{key}, &schemas.BifrostContainerRetrieveRequest{ContainerID: "cntr_1"})
			return err
		}},
	schemas.ContainerDeleteRequest: {"DELETE /v1/containers/cntr_1", "/api/v3/boxes/{container_id}", "DELETE /api/v3/boxes/cntr_1",
		func(p *OpenAIProvider, ctx *schemas.BifrostContext, key schemas.Key) *schemas.BifrostError {
			_, err := p.ContainerDelete(ctx, []schemas.Key{key}, &schemas.BifrostContainerDeleteRequest{ContainerID: "cntr_1"})
			return err
		}},
	schemas.ContainerFileCreateRequest: {"POST /v1/containers/cntr_1/files", "/api/v3/boxes/{container_id}/items", "POST /api/v3/boxes/cntr_1/items",
		func(p *OpenAIProvider, ctx *schemas.BifrostContext, key schemas.Key) *schemas.BifrostError {
			_, err := p.ContainerFileCreate(ctx, key, &schemas.BifrostContainerFileCreateRequest{ContainerID: "cntr_1", File: []byte("bytes")})
			return err
		}},
	schemas.ContainerFileListRequest: {"GET /v1/containers/cntr_1/files", "/api/v3/boxes/{container_id}/items", "GET /api/v3/boxes/cntr_1/items",
		func(p *OpenAIProvider, ctx *schemas.BifrostContext, key schemas.Key) *schemas.BifrostError {
			_, err := p.ContainerFileList(ctx, []schemas.Key{key}, &schemas.BifrostContainerFileListRequest{ContainerID: "cntr_1"})
			return err
		}},
	schemas.ContainerFileRetrieveRequest: {"GET /v1/containers/cntr_1/files/cfile_2", "/api/v3/items/{file_id}/in/{container_id}", "GET /api/v3/items/cfile_2/in/cntr_1",
		func(p *OpenAIProvider, ctx *schemas.BifrostContext, key schemas.Key) *schemas.BifrostError {
			_, err := p.ContainerFileRetrieve(ctx, []schemas.Key{key}, &schemas.BifrostContainerFileRetrieveRequest{ContainerID: "cntr_1", FileID: "cfile_2"})
			return err
		}},
	schemas.ContainerFileContentRequest: {"GET /v1/containers/cntr_1/files/cfile_2/content", "/api/v3/items/{file_id}/in/{container_id}/bytes", "GET /api/v3/items/cfile_2/in/cntr_1/bytes",
		func(p *OpenAIProvider, ctx *schemas.BifrostContext, key schemas.Key) *schemas.BifrostError {
			_, err := p.ContainerFileContent(ctx, []schemas.Key{key}, &schemas.BifrostContainerFileContentRequest{ContainerID: "cntr_1", FileID: "cfile_2"})
			return err
		}},
	schemas.ContainerFileDeleteRequest: {"DELETE /v1/containers/cntr_1/files/cfile_2", "/api/v3/items/{file_id}/in/{container_id}", "DELETE /api/v3/items/cfile_2/in/cntr_1",
		func(p *OpenAIProvider, ctx *schemas.BifrostContext, key schemas.Key) *schemas.BifrostError {
			_, err := p.ContainerFileDelete(ctx, []schemas.Key{key}, &schemas.BifrostContainerFileDeleteRequest{ContainerID: "cntr_1", FileID: "cfile_2"})
			return err
		}},
	schemas.ResponsesRetrieveRequest: {"GET /v1/responses/resp_1", "/api/v3/answers/{response_id}", "GET /api/v3/answers/resp_1",
		func(p *OpenAIProvider, ctx *schemas.BifrostContext, key schemas.Key) *schemas.BifrostError {
			_, err := p.ResponsesRetrieve(ctx, key, &schemas.BifrostResponsesRetrieveRequest{ResponseID: "resp_1"})
			return err
		}},
	schemas.ResponsesRetrieveStreamRequest: {"GET /v1/responses/resp_1", "/api/v3/answers/{response_id}/events", "GET /api/v3/answers/resp_1/events",
		func(p *OpenAIProvider, ctx *schemas.BifrostContext, key schemas.Key) *schemas.BifrostError {
			runner := func(_ *schemas.BifrostContext, result *schemas.BifrostResponse, err *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError) {
				return result, err
			}
			stream, err := p.ResponsesRetrieveStream(ctx, runner, func(context.Context) {}, key, &schemas.BifrostResponsesRetrieveRequest{ResponseID: "resp_1"})
			if stream != nil {
				deadline := time.After(5 * time.Second)
				for open := true; open; {
					select {
					case _, open = <-stream:
					case <-deadline:
						open = false
					}
				}
			}
			return err
		}},
	schemas.ResponsesDeleteRequest: {"DELETE /v1/responses/resp_1", "/api/v3/answers/{response_id}", "DELETE /api/v3/answers/resp_1",
		func(p *OpenAIProvider, ctx *schemas.BifrostContext, key schemas.Key) *schemas.BifrostError {
			_, err := p.ResponsesDelete(ctx, key, &schemas.BifrostResponsesDeleteRequest{ResponseID: "resp_1"})
			return err
		}},
	schemas.ResponsesCancelRequest: {"POST /v1/responses/resp_1/cancel", "/api/v3/answers/{response_id}/stop", "POST /api/v3/answers/resp_1/stop",
		func(p *OpenAIProvider, ctx *schemas.BifrostContext, key schemas.Key) *schemas.BifrostError {
			_, err := p.ResponsesCancel(ctx, key, &schemas.BifrostResponsesCancelRequest{ResponseID: "resp_1"})
			return err
		}},
	schemas.ResponsesInputItemsRequest: {"GET /v1/responses/resp_1/input_items", "/api/v3/answers/{response_id}/inputs", "GET /api/v3/answers/resp_1/inputs",
		func(p *OpenAIProvider, ctx *schemas.BifrostContext, key schemas.Key) *schemas.BifrostError {
			_, err := p.ResponsesInputItems(ctx, key, &schemas.BifrostResponsesInputItemsRequest{ResponseID: "resp_1"})
			return err
		}},
}

// batchAndFileResourceTypes are the listed request types that batch_test.go covers.
var batchAndFileResourceTypes = []schemas.RequestType{
	schemas.BatchRetrieveRequest, schemas.BatchCancelRequest, schemas.BatchResultsRequest,
	schemas.FileRetrieveRequest, schemas.FileDeleteRequest, schemas.FileContentRequest,
}

// Every request type listed in ResourcePathPlaceholders has a row here or in the batch and
// file tests, so a request type added to the list without a call that honours its template
// fails this test instead of silently losing the id.
func TestEveryResourceRequestTypeHasACall(t *testing.T) {
	covered := map[schemas.RequestType]bool{}
	for _, requestType := range batchAndFileResourceTypes {
		covered[requestType] = true
	}
	for requestType := range resourceCalls {
		covered[requestType] = true
	}
	for requestType := range providerUtils.ResourcePathPlaceholders {
		assert.True(t, covered[requestType], "%s is listed in ResourcePathPlaceholders and has no call in resourceCalls", requestType)
	}
	for requestType := range covered {
		assert.Contains(t, providerUtils.ResourcePathPlaceholders, requestType, "%s has a call here and no placeholder", requestType)
	}
}

func TestResourceCallsHonourPathOverrides(t *testing.T) {
	allTemplates := map[schemas.RequestType]string{}
	for requestType, rc := range resourceCalls {
		allTemplates[requestType] = rc.template
	}

	for requestType, rc := range resourceCalls {
		t.Run(string(requestType), func(t *testing.T) {
			v := newResourceVendor(t)

			provider, ctx, key := newResourceTestProvider(v, false, nil)
			_ = rc.call(provider, ctx, key)
			assert.Equal(t, []string{rc.defaultCall}, v.takeCalls(), "built-in openai provider")

			provider, ctx, key = newResourceTestProvider(v, true, nil)
			_ = rc.call(provider, ctx, key)
			assert.Equal(t, []string{rc.defaultCall}, v.takeCalls(), "custom provider without overrides")

			// An override for another request type leaves this one on its default path.
			other := map[schemas.RequestType]string{schemas.ChatCompletionRequest: "/api/v3/chat/completions"}
			provider, ctx, key = newResourceTestProvider(v, true, other)
			_ = rc.call(provider, ctx, key)
			assert.Equal(t, []string{rc.defaultCall}, v.takeCalls(), "custom provider with an override for another request type")

			provider, ctx, key = newResourceTestProvider(v, true, allTemplates)
			_ = rc.call(provider, ctx, key)
			assert.Equal(t, []string{rc.templatedCall}, v.takeCalls(), "custom provider with a path template")

			// A full URL template replaces the base URL.
			provider, ctx, key = newResourceTestProvider(v, true, map[schemas.RequestType]string{requestType: v.URL + rc.template})
			provider.networkConfig.BaseURL = "http://127.0.0.1:1"
			_ = rc.call(provider, ctx, key)
			assert.Equal(t, []string{rc.templatedCall}, v.takeCalls(), "custom provider with a full URL template")

			// The override the UI used to accept: a path with no id. Refused, nothing sent.
			provider, ctx, key = newResourceTestProvider(v, true, map[schemas.RequestType]string{requestType: "/api/v3/things"})
			bifrostErr := rc.call(provider, ctx, key)
			require.NotNil(t, bifrostErr, "an override without its placeholder must be refused")
			require.NotNil(t, bifrostErr.Error)
			assert.Contains(t, bifrostErr.Error.Message, "request_path_overrides."+string(requestType))
			assert.Contains(t, bifrostErr.Error.Message, providerUtils.ResourcePathPlaceholders[requestType][0])
			assert.Empty(t, v.takeCalls(), "no upstream call for a template that lost the id")
		})
	}
}

// A path the caller's own request put on the context still wins over a template, as it
// does for every other OpenAI call.
func TestResourceCallContextPathWinsOverATemplate(t *testing.T) {
	v := newResourceVendor(t)
	provider, ctx, key := newResourceTestProvider(v, true, map[schemas.RequestType]string{schemas.ModelRetrieveRequest: "/api/v3/models/{model}"})
	ctx.SetValue(schemas.BifrostContextKeyURLPath, "/models/glm-video-1")
	_, _ = provider.ModelRetrieve(ctx, key, &schemas.BifrostModelRetrieveRequest{Model: "glm-video-1"})
	assert.Equal(t, []string{"GET /models/glm-video-1"}, v.takeCalls())
}

// BenchmarkResourceURLForIDs measures what building the URL of a call that names an id costs
// per request: the built-in provider and a custom provider without an override for the
// request type take the path they took before this helper existed.
func BenchmarkResourceURLForIDs(b *testing.B) {
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	newProvider := func(custom bool, overrides map[schemas.RequestType]string) *OpenAIProvider {
		config := &schemas.ProviderConfig{NetworkConfig: schemas.NetworkConfig{BaseURL: "https://api.openai.com", DefaultRequestTimeoutInSeconds: 30}}
		if custom {
			config.CustomProviderConfig = &schemas.CustomProviderConfig{CustomProviderKey: "tkp_video_vendor", BaseProviderType: schemas.OpenAI, RequestPathOverrides: overrides}
		}
		return NewOpenAIProvider(config, testNoopLogger{})
	}
	cases := []struct {
		name     string
		provider *OpenAIProvider
	}{
		{"built-in", newProvider(false, nil)},
		{"custom without overrides", newProvider(true, nil)},
		{"custom with another override", newProvider(true, map[schemas.RequestType]string{schemas.ChatCompletionRequest: "/api/v3/chat/completions"})},
		{"custom with a template", newProvider(true, map[schemas.RequestType]string{schemas.ContainerFileRetrieveRequest: "/api/v3/boxes/{container_id}/items/{file_id}"})},
	}
	for _, bc := range cases {
		b.Run("before/"+bc.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = bc.provider.buildRequestURL(ctx, "/v1/containers/"+"cntr_1"+"/files/"+"cfile_2", schemas.ContainerFileRetrieveRequest)
			}
		})
		b.Run("after/"+bc.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := bc.provider.buildResourceURLForIDs(ctx, "/v1/containers/"+"cntr_1"+"/files/"+"cfile_2", schemas.ContainerFileRetrieveRequest, "cntr_1", "cfile_2"); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
