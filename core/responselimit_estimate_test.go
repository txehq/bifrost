package bifrost

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/capsohq/bifrost/core/network"
	"github.com/capsohq/bifrost/core/schemas"
)

// refusalRecorder records the error each post-hook sees, as governance does.
type refusalRecorder struct {
	fakeRoutingPlugin
	mu   sync.Mutex
	errs []*schemas.BifrostError
}

func (r *refusalRecorder) PostLLMHook(_ *schemas.BifrostContext, resp *schemas.BifrostResponse, bifrostErr *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errs = append(r.errs, bifrostErr)
	return resp, bifrostErr, nil
}

func (r *refusalRecorder) last() *schemas.BifrostError {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.errs) == 0 {
		return nil
	}
	return r.errs[len(r.errs)-1]
}

// TXE-7302: an image answer past client.max_response_body_size_mb is refused unread, but the
// vendor may have billed it. The refusal reaching the post-hooks carries Bifrost's estimate of
// that bill, from the request (n, size, quality), so governance can charge the virtual key's
// budget. A chat answer past the limit carries none: the request does not fix its price. Both
// stay bifrost_internal, which Tokun does not charge the caller for (contract C6). Before, the
// image refusal carried no usage at all.
func TestResponseLimitRefusalOfAnImageAnswerCarriesTheEstimate(t *testing.T) {
	network.SetMaxResponseBodySizeMB(1)
	t.Cleanup(func() { network.SetMaxResponseBodySizeMB(0) })

	padding := strings.Repeat("A", 1<<20+1024)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/images/generations") {
			_, _ = io.WriteString(w, `{"created":1,"data":[{"b64_json":"`+padding+`"}],"usage":{"input_tokens":10,"output_tokens":4000,"total_tokens":4010}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"`+padding+`"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`)
	}))
	t.Cleanup(server.Close)

	account := NewMockAccount()
	account.AddProviderWithBaseURL(schemas.OpenAI, 1, 4, server.URL)
	account.configs[schemas.OpenAI].NetworkConfig.AllowPrivateNetwork = true
	account.configs[schemas.OpenAI].NetworkConfig.MaxRetries = 0
	account.keys[schemas.OpenAI][0].Models = schemas.WhiteList{"*"}
	recorder := &refusalRecorder{fakeRoutingPlugin: fakeRoutingPlugin{name: "refusal-recorder"}}
	client, err := Init(context.Background(), schemas.BifrostConfig{Account: account, Logger: NewNoOpLogger(), LLMPlugins: []schemas.LLMPlugin{recorder}})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(client.Shutdown)

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	_, imageErr := client.ImageGenerationRequest(ctx, &schemas.BifrostImageGenerationRequest{
		Provider: schemas.OpenAI, Model: "gpt-image-1",
		Input:  &schemas.ImageGenerationInput{Prompt: "a lighthouse"},
		Params: &schemas.ImageGenerationParameters{N: schemas.Ptr(3), Size: schemas.Ptr("1024x1024"), Quality: schemas.Ptr("high")},
	})
	if imageErr == nil {
		t.Fatal("an image answer past the response limit was not refused")
	}
	if imageErr.ExtraFields.ErrorType != schemas.ErrorTypeBifrostInternal {
		t.Fatalf("error_type = %q, want %q: the caller is not charged (C6)", imageErr.ExtraFields.ErrorType, schemas.ErrorTypeBifrostInternal)
	}
	seen := recorder.last()
	if seen == nil || seen.ExtraFields.EstimatedImageUsage == nil {
		t.Fatalf("the post-hooks saw an image refusal with no estimate: %+v", seen)
	}
	want := schemas.EstimatedImageUsage{OutputImages: 3, Size: "1024x1024", Quality: "high"}
	if got := *seen.ExtraFields.EstimatedImageUsage; got != want {
		t.Fatalf("estimate = %+v, want %+v", got, want)
	}
	if seen.ExtraFields.BilledUsage != nil {
		t.Fatalf("the estimate must not pose as the vendor's usage: BilledUsage = %+v", seen.ExtraFields.BilledUsage)
	}

	_, chatErr := client.ChatCompletionRequest(schemas.NewBifrostContext(context.Background(), schemas.NoDeadline), &schemas.BifrostChatRequest{
		Provider: schemas.OpenAI, Model: "gpt-4o",
		Input: []schemas.ChatMessage{{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: schemas.Ptr("hi")}}},
	})
	if chatErr == nil {
		t.Fatal("a chat answer past the response limit was not refused")
	}
	if seen := recorder.last(); seen == nil || seen.ExtraFields.EstimatedImageUsage != nil {
		t.Fatalf("a chat refusal carried an image estimate: %+v", seen)
	}
}

func TestEstimateImageUsageFromRequest(t *testing.T) {
	for name, tc := range map[string]struct {
		req  *schemas.BifrostRequest
		want *schemas.EstimatedImageUsage
	}{
		"generation with n, size, quality and resolution": {
			&schemas.BifrostRequest{ImageGenerationRequest: &schemas.BifrostImageGenerationRequest{Params: &schemas.ImageGenerationParameters{N: schemas.Ptr(4), Size: schemas.Ptr("1536x1024"), Quality: schemas.Ptr("low"), Resolution: "2K"}}},
			&schemas.EstimatedImageUsage{OutputImages: 4, Size: "1536x1024", Quality: "low", Resolution: "2K"},
		},
		"generation with no params is one image": {
			&schemas.BifrostRequest{ImageGenerationRequest: &schemas.BifrostImageGenerationRequest{}},
			&schemas.EstimatedImageUsage{OutputImages: 1},
		},
		"edit": {
			&schemas.BifrostRequest{ImageEditRequest: &schemas.BifrostImageEditRequest{Params: &schemas.ImageEditParameters{N: schemas.Ptr(2), Size: schemas.Ptr("1024x1024")}}},
			&schemas.EstimatedImageUsage{OutputImages: 2, Size: "1024x1024"},
		},
		"variation": {
			&schemas.BifrostRequest{ImageVariationRequest: &schemas.BifrostImageVariationRequest{Params: &schemas.ImageVariationParameters{N: schemas.Ptr(2), Size: schemas.Ptr("512x512")}}},
			&schemas.EstimatedImageUsage{OutputImages: 2, Size: "512x512"},
		},
		"chat has no estimate": {
			&schemas.BifrostRequest{ChatRequest: &schemas.BifrostChatRequest{}},
			nil,
		},
	} {
		got := schemas.EstimateImageUsageFromRequest(tc.req)
		if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
			t.Errorf("%s: estimate = %+v, want %+v", name, got, tc.want)
		}
	}
}

// BenchmarkAttachResponseLimitImageEstimate is what TXE-7302 adds after every provider dispatch:
// a successful request pays a nil check, an ordinary error an errors.Is, and only an image refusal
// for the response limit builds the estimate.
func BenchmarkAttachResponseLimitImageEstimate(b *testing.B) {
	imageReq := &schemas.BifrostRequest{ImageGenerationRequest: &schemas.BifrostImageGenerationRequest{Params: &schemas.ImageGenerationParameters{N: schemas.Ptr(3), Size: schemas.Ptr("1024x1024"), Quality: schemas.Ptr("high")}}}
	b.Run("success", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			attachResponseLimitImageEstimate(imageReq, nil)
		}
	})
	b.Run("other_error", func(b *testing.B) {
		other := &schemas.BifrostError{Error: &schemas.ErrorField{Message: "upstream 500", Error: io.ErrUnexpectedEOF}}
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			attachResponseLimitImageEstimate(imageReq, other)
		}
	})
	b.Run("image_refusal", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			refusal := &schemas.BifrostError{Error: &schemas.ErrorField{Error: &network.ResponseBodyTooLargeError{Limit: 1 << 20}}}
			attachResponseLimitImageEstimate(imageReq, refusal)
		}
	})
}
