package bifrost

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/capsohq/bifrost/core/schemas"
)

// responseRecorder records the response each post-hook sees, as governance and logging do.
type responseRecorder struct {
	fakeRoutingPlugin
	mu    sync.Mutex
	resps []*schemas.BifrostResponse
}

func (r *responseRecorder) PostLLMHook(_ *schemas.BifrostContext, resp *schemas.BifrostResponse, bifrostErr *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resps = append(r.resps, resp)
	return resp, bifrostErr, nil
}

// TXE-7017: an image answer over the large-response threshold on OpenAI's native route is
// streamed to the caller unread. The response the post-hooks see has no data and, before, a
// usage.output_images of 0 that BackfillParams wrote from the empty placeholder, so governance and
// logging recorded a paid answer of several images as none. It now carries no output_images (the
// count is unknown) and the requested n for pricing. The same request under the threshold is read
// and reports the vendor's count, so the test is not vacuous.
func TestLargeResponseImageAnswerIsNotSettledAsZeroImages(t *testing.T) {
	padding := strings.Repeat("A", 64*1024)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"created":1,"data":[{"b64_json":"`+padding+`"},{"b64_json":"`+padding+`"},{"b64_json":"`+padding+`"}],"usage":{"input_tokens":10,"output_tokens":4000,"total_tokens":4010}}`)
	}))
	t.Cleanup(server.Close)

	account := NewMockAccount()
	account.AddProviderWithBaseURL(schemas.OpenAI, 1, 4, server.URL)
	account.configs[schemas.OpenAI].NetworkConfig.AllowPrivateNetwork = true
	account.configs[schemas.OpenAI].NetworkConfig.MaxRetries = 0
	account.keys[schemas.OpenAI][0].Models = schemas.WhiteList{"*"}
	recorder := &responseRecorder{fakeRoutingPlugin: fakeRoutingPlugin{name: "response-recorder"}}
	client, err := Init(context.Background(), schemas.BifrostConfig{Account: account, Logger: NewNoOpLogger(), LLMPlugins: []schemas.LLMPlugin{recorder}})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(client.Shutdown)

	run := func(t *testing.T, threshold int64) (*schemas.BifrostContext, *schemas.BifrostImageGenerationResponse) {
		t.Helper()
		ctx := routeCtx("openai")
		if threshold > 0 {
			ctx.SetValue(schemas.BifrostContextKeyLargeResponseThreshold, threshold)
		}
		_, bifrostErr := client.ImageGenerationRequest(ctx, &schemas.BifrostImageGenerationRequest{
			Provider: schemas.OpenAI, Model: "gpt-image-1",
			Input:  &schemas.ImageGenerationInput{Prompt: "a lighthouse"},
			Params: &schemas.ImageGenerationParameters{N: schemas.Ptr(3), Size: schemas.Ptr("1024x1024"), Quality: schemas.Ptr("high")},
		})
		if bifrostErr != nil {
			t.Fatalf("image generation failed: %+v", bifrostErr)
		}
		if reader, ok := ctx.Value(schemas.BifrostContextKeyLargeResponseReader).(io.ReadCloser); ok {
			_, _ = io.Copy(io.Discard, reader)
			_ = reader.Close()
		}
		recorder.mu.Lock()
		defer recorder.mu.Unlock()
		last := recorder.resps[len(recorder.resps)-1]
		if last == nil || last.ImageGenerationResponse == nil {
			t.Fatalf("the post-hooks saw no image response: %+v", last)
		}
		return ctx, last.ImageGenerationResponse
	}

	t.Run("over the threshold", func(t *testing.T) {
		ctx, seen := run(t, 1024)
		if large, _ := ctx.Value(schemas.BifrostContextKeyLargeResponseMode).(bool); !large {
			t.Fatal("the answer was not streamed in large-response mode; the test needs it to be")
		}
		if seen.Usage == nil {
			t.Fatal("the unread response carries no usage for pricing")
		}
		if seen.Usage.OutputImages != nil {
			t.Fatalf("the post-hooks saw output_images %d for an answer Bifrost never read", *seen.Usage.OutputImages)
		}
		if !seen.LargeResponseUnread {
			t.Error("the response is not marked unread")
		}
		if seen.Usage.OutputTokensDetails == nil || seen.Usage.OutputTokensDetails.NImages != 3 {
			t.Fatalf("pricing count = %+v, want the requested 3", seen.Usage.OutputTokensDetails)
		}
	})

	t.Run("under the threshold", func(t *testing.T) {
		ctx, seen := run(t, 0)
		if ctx.Value(schemas.BifrostContextKeyLargeResponseMode) != nil {
			t.Fatal("a response with no threshold was streamed")
		}
		if seen.LargeResponseUnread || seen.Usage == nil || seen.Usage.OutputImages == nil || *seen.Usage.OutputImages != 3 {
			t.Fatalf("a read response must report the vendor's 3 images: %+v", seen.Usage)
		}
	})
}

// Every image request type returns through backfillImageResponse, whichever provider served it:
// generation, edit and variation are all marked when the context is in large-response mode, and
// none is without it.
func TestBackfillImageResponseMarksEveryImageTypeInLargeResponseMode(t *testing.T) {
	for name, req := range map[string]*schemas.BifrostRequest{
		"generation": {RequestType: schemas.ImageGenerationRequest, ImageGenerationRequest: &schemas.BifrostImageGenerationRequest{Provider: schemas.XAI, Params: &schemas.ImageGenerationParameters{N: schemas.Ptr(2)}}},
		"edit":       {RequestType: schemas.ImageEditRequest, ImageEditRequest: &schemas.BifrostImageEditRequest{Provider: schemas.OpenAI, Params: &schemas.ImageEditParameters{N: schemas.Ptr(2)}}},
		"variation":  {RequestType: schemas.ImageVariationRequest, ImageVariationRequest: &schemas.BifrostImageVariationRequest{Provider: schemas.OpenAI, Params: &schemas.ImageVariationParameters{N: schemas.Ptr(2)}}},
	} {
		large := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
		large.SetValue(schemas.BifrostContextKeyLargeResponseMode, true)
		response := &schemas.BifrostImageGenerationResponse{}
		backfillImageResponse(large, response, req)
		if response.Usage == nil || response.Usage.OutputImages != nil || response.Usage.OutputTokensDetails == nil || response.Usage.OutputTokensDetails.NImages != 2 {
			t.Errorf("%s in large-response mode: usage = %+v, want no output_images and NImages 2", name, response.Usage)
		}

		read := &schemas.BifrostImageGenerationResponse{Data: []schemas.ImageData{{B64JSON: "aW1n"}}}
		backfillImageResponse(schemas.NewBifrostContext(context.Background(), schemas.NoDeadline), read, req)
		if read.LargeResponseUnread || read.Usage == nil || read.Usage.OutputImages == nil || *read.Usage.OutputImages != 1 {
			t.Errorf("%s read in full: usage = %+v, want output_images 1", name, read.Usage)
		}
	}
}

// BenchmarkBackfillImageResponse is TXE-7017's cost on an image response: one context lookup when
// the answer was read, and the mark when it was streamed unread.
func BenchmarkBackfillImageResponse(b *testing.B) {
	req := &schemas.BifrostRequest{RequestType: schemas.ImageGenerationRequest, ImageGenerationRequest: &schemas.BifrostImageGenerationRequest{Provider: schemas.OpenAI, Model: "gpt-image-1", Params: &schemas.ImageGenerationParameters{N: schemas.Ptr(2), Size: schemas.Ptr("1024x1024")}}}
	read := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	large := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	large.SetValue(schemas.BifrostContextKeyLargeResponseMode, true)
	b.Run("read", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			backfillImageResponse(read, &schemas.BifrostImageGenerationResponse{Data: []schemas.ImageData{{B64JSON: "aW1n"}}}, req)
		}
	})
	b.Run("read_before", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			(&schemas.BifrostImageGenerationResponse{Data: []schemas.ImageData{{B64JSON: "aW1n"}}}).BackfillParams(req)
		}
	})
	b.Run("large_unread", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			backfillImageResponse(large, &schemas.BifrostImageGenerationResponse{}, req)
		}
	})
}
