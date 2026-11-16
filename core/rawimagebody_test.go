package bifrost

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/capsohq/bifrost/core/schemas"
)

// rawImageBodyPricingCase is a raw body asking for a larger size or tier than the typed request
// it rides on, and what it must be billed at.
type rawImageBodyPricingCase struct {
	model string
	body  string
	want  schemas.RawImageBilling
	// duplicate names a pricing field twice in two spellings; the reader must refuse it.
	duplicate string
}

// rawImageBodyPricingCases has a case for every provider and request type whose provider reads raw
// image bodies (schemas.RawImageBodyReader, TXE-7502). TestEveryProviderReadsOrRefusesRawImageBodies
// fails for a provider that reads one with no case here, and for a case whose provider does not.
var rawImageBodyPricingCases = map[schemas.ModelProvider]map[schemas.RequestType]rawImageBodyPricingCase{
	schemas.DashScope: {
		schemas.ImageGenerationTaskRequest: {
			model:     "wan2.7-image-pro",
			body:      `{"model":"wan2.7-image-pro","input":{"messages":[]},"parameters":{"size":"4K","n":3}}`,
			want:      schemas.RawImageBilling{Images: 3, Parameters: &schemas.ImageGenerationResponseParameters{Resolution: "4K"}},
			duplicate: `{"model":"wan2.7-image-pro","parameters":{"size":"1K","SIZE":"4K"}}`,
		},
		schemas.ImageEditTaskRequest: {
			model:     "wan2.6-image",
			body:      `{"model":"wan2.6-image","input":{"messages":[]},"parameters":{"size":"2048*2048","n":2}}`,
			want:      schemas.RawImageBilling{Images: 2, Parameters: &schemas.ImageGenerationResponseParameters{Size: "2048x2048"}},
			duplicate: `{"model":"wan2.6-image","parameters":{"n":1,"N":4}}`,
		},
	},
	schemas.BFL: {
		schemas.ImageGenerationTaskRequest: {
			model:     "flux-pro-1.1",
			body:      `{"prompt":"a lighthouse","width":2048,"height":2048,"steps":50}`,
			want:      schemas.RawImageBilling{Parameters: &schemas.ImageGenerationResponseParameters{Size: "2048x2048", NumInferenceSteps: schemas.Ptr(50)}},
			duplicate: `{"prompt":"a lighthouse","width":1024,"Width":2048,"height":2048}`,
		},
		schemas.ImageEditTaskRequest: {
			model:     "flux-3-image",
			body:      `{"prompt":"make it blue","images":["https://example.com/a.png","https://example.com/b.png"],"resolution":"4k"}`,
			want:      schemas.RawImageBilling{InputImages: 2, Parameters: &schemas.ImageGenerationResponseParameters{Resolution: "4K"}},
			duplicate: `{"prompt":"make it blue","resolution":"1k","re_solution":"4k"}`,
		},
	},
	schemas.Gemini: {
		schemas.ImageGenerationRequest: {
			model:     "gemini-3-pro-image-preview",
			body:      `{"contents":[{"parts":[{"text":"a lighthouse"}]}],"generationConfig":{"imageConfig":{"imageSize":"4K","aspectRatio":"1:1"}}}`,
			want:      schemas.RawImageBilling{Parameters: &schemas.ImageGenerationResponseParameters{Size: "4096x4096", AspectRatio: "1:1"}},
			duplicate: `{"generationConfig":{"imageConfig":{"imageSize":"1K","image_size":"4K"}}}`,
		},
		schemas.ImageEditRequest: {
			model:     "imagen-3.0-capability-001",
			body:      `{"instances":[{"prompt":"make it blue","referenceImages":[{"referenceType":"REFERENCE_TYPE_RAW","referenceImage":{"bytesBase64Encoded":"aW1n"}},{"referenceType":"REFERENCE_TYPE_MASK","referenceImage":{"bytesBase64Encoded":"bWFzaw=="}}]}],"parameters":{"sampleImageSize":"2K","sampleCount":1}}`,
			want:      schemas.RawImageBilling{InputImages: 1, Parameters: &schemas.ImageGenerationResponseParameters{Size: "2048x2048"}},
			duplicate: `{"parameters":{"sampleImageSize":"1K"},"Parameters":{"sampleImageSize":"2K"}}`,
		},
	},
	schemas.Vertex: {
		schemas.ImageGenerationRequest: {
			model:     "imagen-4.0-generate-001",
			body:      `{"instances":[{"prompt":"a lighthouse"}],"parameters":{"sampleImageSize":"2K","aspectRatio":"16:9"}}`,
			want:      schemas.RawImageBilling{Parameters: &schemas.ImageGenerationResponseParameters{Size: "2048x1152"}},
			duplicate: `{"parameters":{"aspectRatio":"1:1","aspect_ratio":"16:9"}}`,
		},
		schemas.ImageEditRequest: {
			model:     "gemini-2.5-flash-image",
			body:      `{"contents":[{"parts":[{"text":"make it blue"},{"inline_data":{"mime_type":"image/png","data":"aW1n"}},{"fileData":{"fileUri":"gs://b/a.png"}}]}],"generation_config":{"image_config":{"image_size":"2K"}}}`,
			want:      schemas.RawImageBilling{InputImages: 2, Parameters: &schemas.ImageGenerationResponseParameters{Size: "2048x2048"}},
			duplicate: `{"generationConfig":{"imageConfig":{"imageSize":"1K"}},"generation_config":{"image_config":{"image_size":"4K"}}}`,
		},
	},
}

// rawImageRequest is a typed request of requestType that asks for the smallest size, carrying body
// as its raw body.
func rawImageRequest(provider schemas.ModelProvider, requestType schemas.RequestType, model, body string) *schemas.BifrostRequest {
	req := &schemas.BifrostRequest{RequestType: requestType}
	switch requestType {
	case schemas.ImageEditRequest, schemas.ImageEditStreamRequest, schemas.ImageEditTaskRequest:
		req.ImageEditRequest = &schemas.BifrostImageEditRequest{
			Provider: provider, Model: model,
			Params:         &schemas.ImageEditParameters{Size: schemas.Ptr("256x256")},
			RawRequestBody: []byte(body),
		}
	case schemas.ImageVariationRequest:
		req.ImageVariationRequest = &schemas.BifrostImageVariationRequest{
			Provider: provider, Model: model,
			Params:         &schemas.ImageVariationParameters{Size: schemas.Ptr("256x256")},
			RawRequestBody: []byte(body),
		}
	default:
		req.ImageGenerationRequest = &schemas.BifrostImageGenerationRequest{
			Provider: provider, Model: model,
			Input:          &schemas.ImageGenerationInput{Prompt: "a lighthouse"},
			Params:         &schemas.ImageGenerationParameters{Size: schemas.Ptr("256x256")},
			RawRequestBody: []byte(body),
		}
	}
	return req
}

func rawBodyCtx() *schemas.BifrostContext {
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	ctx.SetValue(schemas.BifrostContextKeyUseRawRequestBody, true)
	return ctx
}

// TXE-7502, the class check: in raw request body mode, every standard provider either reads a
// priced image operation's raw body itself, and has a case above showing a body asking for a larger
// size or tier is billed at it, or core refuses the attempt before any upstream call. A provider
// that starts reading raw image bodies without a case, or a case left behind for one that stopped,
// fails here; a provider added later is refused by default.
func TestEveryProviderReadsOrRefusesRawImageBodies(t *testing.T) {
	client := &Bifrost{logger: NewNoOpLogger()}
	readers := map[schemas.ModelProvider]map[schemas.RequestType]bool{}
	for _, providerKey := range schemas.StandardProviders {
		provider, err := client.createBaseProvider(providerKey, &schemas.ProviderConfig{})
		if err != nil {
			t.Fatalf("construct %s: %v", providerKey, err)
		}
		for _, requestType := range schemas.PricedImageRequestTypes {
			reader, ok := provider.(schemas.RawImageBodyReader)
			reads := ok && reader.ReadsRawImageBody(requestType)
			name := string(providerKey) + "/" + string(requestType)
			if !reads {
				ctx := rawBodyCtx()
				refusal := decideRawImageBodyForAttempt(ctx, provider, requestType, rawImageRequest(providerKey, requestType, "m", `{"size":"4096x4096"}`))
				if refusal == nil || refusal.Error == nil || refusal.Error.Code == nil || *refusal.Error.Code != "raw_request_body_unsupported" ||
					refusal.StatusCode == nil || *refusal.StatusCode != 400 {
					t.Errorf("%s: a raw body on a priced image operation whose provider does not read it must be refused 400 raw_request_body_unsupported, got %+v", name, refusal)
				}
				if schemas.RawImageBillingFromContext(ctx) != nil {
					t.Errorf("%s: a refused attempt left a raw image billing on the context", name)
				}
				continue
			}
			if readers[providerKey] == nil {
				readers[providerKey] = map[schemas.RequestType]bool{}
			}
			readers[providerKey][requestType] = true
			tc, ok := rawImageBodyPricingCases[providerKey][requestType]
			if !ok {
				t.Errorf("%s reads raw image bodies but rawImageBodyPricingCases has no case for it: add one showing a body asking for a larger size or tier is billed at it", name)
				continue
			}
			ctx := rawBodyCtx()
			if refusal := decideRawImageBodyForAttempt(ctx, provider, requestType, rawImageRequest(providerKey, requestType, tc.model, tc.body)); refusal != nil {
				t.Errorf("%s: refused a raw body it reads: %+v", name, refusal.Error)
				continue
			}
			got := schemas.RawImageBillingFromContext(ctx)
			if got == nil || !reflect.DeepEqual(*got.Parameters, *tc.want.Parameters) || got.Images != tc.want.Images || got.InputImages != tc.want.InputImages {
				t.Errorf("%s: raw body %s billed at %+v, want %+v (images %d, input images %d)", name, tc.body, got, tc.want.Parameters, tc.want.Images, tc.want.InputImages)
			}
			dupCtx := rawBodyCtx()
			if refusal := decideRawImageBodyForAttempt(dupCtx, provider, requestType, rawImageRequest(providerKey, requestType, tc.model, tc.duplicate)); refusal == nil {
				t.Errorf("%s: a raw body naming a pricing field twice (%s) must be refused", name, tc.duplicate)
			}
			if refusal := decideRawImageBodyForAttempt(rawBodyCtx(), provider, requestType, rawImageRequest(providerKey, requestType, tc.model, `[`+tc.body+`]`)); refusal == nil {
				t.Errorf("%s: a raw body that is not one JSON object must be refused", name)
			}
		}
	}
	for providerKey, cases := range rawImageBodyPricingCases {
		for requestType := range cases {
			if !readers[providerKey][requestType] {
				t.Errorf("rawImageBodyPricingCases has a case for %s/%s, whose provider does not read raw image bodies", providerKey, requestType)
			}
		}
	}
}

// Without raw request body mode, or on an operation that is not a priced image one, the gate does
// nothing, and it clears an earlier attempt's billing so a fallback that converts is priced from
// its typed request.
func TestRawImageBodyGateLeavesOtherAttemptsAlone(t *testing.T) {
	client := &Bifrost{logger: NewNoOpLogger()}
	openai, err := client.createBaseProvider(schemas.OpenAI, &schemas.ProviderConfig{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	ctx.SetValue(schemas.BifrostContextKeyRawImageBilling, &schemas.RawImageBilling{Parameters: &schemas.ImageGenerationResponseParameters{Size: "4096x4096"}})
	if refusal := decideRawImageBodyForAttempt(ctx, openai, schemas.ImageGenerationRequest, rawImageRequest(schemas.OpenAI, schemas.ImageGenerationRequest, "gpt-image-1", `{}`)); refusal != nil {
		t.Fatalf("raw mode is off: refused %+v", refusal.Error)
	}
	if schemas.RawImageBillingFromContext(ctx) != nil {
		t.Fatal("an earlier attempt's raw image billing survived into an attempt that converts")
	}
	raw := rawBodyCtx()
	if refusal := decideRawImageBodyForAttempt(raw, openai, schemas.ImageTaskRetrieveRequest, &schemas.BifrostRequest{}); refusal != nil {
		t.Fatalf("a task query is not billed: refused %+v", refusal.Error)
	}
	if refusal := decideRawImageBodyForAttempt(raw, openai, schemas.ChatCompletionRequest, &schemas.BifrostRequest{}); refusal != nil {
		t.Fatalf("chat is not an image operation: refused %+v", refusal.Error)
	}
}

// TXE-7502 through the client: a raw Gemini body asking for 4K, on a typed request asking for
// 1024x1024, is billed at 4096x4096 (what the post-hooks, and so cost, read). A raw OpenAI body,
// on the sync and the streamed route, is refused with no upstream call. A raw BFL task create
// asking for 2048x2048 is billed at that size.
func TestRawImageBodyIsBilledAtItsOwnSizeThroughTheClient(t *testing.T) {
	var upstreamCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/flux-pro-1.1":
			_, _ = io.WriteString(w, `{"id":"task-1","polling_url":"`+"http://"+r.Host+`/v1/get_result?id=task-1"}`)
		default:
			_, _ = io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"inlineData":{"mimeType":"image/png","data":"iVBORw0KGgo="}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":2000,"totalTokenCount":2005}}`)
		}
	}))
	t.Cleanup(server.Close)

	account := NewMockAccount()
	for _, provider := range []schemas.ModelProvider{schemas.Gemini, schemas.OpenAI, schemas.BFL} {
		account.AddProviderWithBaseURL(provider, 1, 4, server.URL)
		account.configs[provider].NetworkConfig.AllowPrivateNetwork = true
		account.configs[provider].NetworkConfig.MaxRetries = 0
		account.keys[provider][0].Models = schemas.WhiteList{"*"}
	}
	recorder := &responseRecorder{fakeRoutingPlugin: fakeRoutingPlugin{name: "response-recorder"}}
	client, err := Init(context.Background(), schemas.BifrostConfig{Account: account, Logger: NewNoOpLogger(), LLMPlugins: []schemas.LLMPlugin{recorder}})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(client.Shutdown)

	_, bifrostErr := client.ImageGenerationRequest(rawBodyCtx(), &schemas.BifrostImageGenerationRequest{
		Provider: schemas.Gemini, Model: "gemini-3-pro-image-preview",
		Input:          &schemas.ImageGenerationInput{Prompt: "a lighthouse"},
		Params:         &schemas.ImageGenerationParameters{Size: schemas.Ptr("1024x1024")},
		RawRequestBody: []byte(`{"contents":[{"parts":[{"text":"a lighthouse"}]}],"generationConfig":{"imageConfig":{"imageSize":"4K"}}}`),
	})
	if bifrostErr != nil {
		t.Fatalf("gemini raw image generation failed: %+v", bifrostErr.Error)
	}
	recorder.mu.Lock()
	last := recorder.resps[len(recorder.resps)-1]
	recorder.mu.Unlock()
	if last == nil || last.ImageGenerationResponse == nil || last.ImageGenerationResponse.ImageGenerationResponseParameters == nil {
		t.Fatalf("the post-hooks saw no image response parameters: %+v", last)
	}
	if size := last.ImageGenerationResponse.Size; size != "4096x4096" {
		t.Fatalf("the raw body asked for 4K and is billed at size %q, want 4096x4096 (the typed request asked for 1024x1024)", size)
	}

	before := upstreamCalls.Load()
	_, bifrostErr = client.ImageGenerationRequest(rawBodyCtx(), &schemas.BifrostImageGenerationRequest{
		Provider: schemas.OpenAI, Model: "dall-e-3",
		Input:          &schemas.ImageGenerationInput{Prompt: "a lighthouse"},
		RawRequestBody: []byte(`{"model":"dall-e-3","prompt":"a lighthouse","size":"1792x1024","quality":"hd"}`),
	})
	if bifrostErr == nil || bifrostErr.Error == nil || bifrostErr.Error.Code == nil || *bifrostErr.Error.Code != "raw_request_body_unsupported" {
		t.Fatalf("a raw OpenAI image body must be refused raw_request_body_unsupported, got %+v", bifrostErr)
	}
	_, bifrostErr = client.ImageGenerationStreamRequest(rawBodyCtx(), &schemas.BifrostImageGenerationRequest{
		Provider: schemas.OpenAI, Model: "gpt-image-1",
		Input:          &schemas.ImageGenerationInput{Prompt: "a lighthouse"},
		RawRequestBody: []byte(`{"model":"gpt-image-1","prompt":"a lighthouse","size":"1536x1024","quality":"high","stream":true}`),
	})
	if bifrostErr == nil || bifrostErr.Error == nil || bifrostErr.Error.Code == nil || *bifrostErr.Error.Code != "raw_request_body_unsupported" {
		t.Fatalf("a raw OpenAI streamed image body must be refused raw_request_body_unsupported, got %+v", bifrostErr)
	}
	if got := upstreamCalls.Load(); got != before {
		t.Fatalf("refused raw image bodies made %d upstream calls", got-before)
	}

	task, bifrostErr := client.ImageGenerationTaskRequest(rawBodyCtx(), &schemas.BifrostImageGenerationRequest{
		Provider: schemas.BFL, Model: "flux-pro-1.1",
		Input:          &schemas.ImageGenerationInput{Prompt: "a lighthouse"},
		Params:         &schemas.ImageGenerationParameters{Size: schemas.Ptr("512x512")},
		RawRequestBody: []byte(`{"prompt":"a lighthouse","width":2048,"height":2048}`),
	})
	if bifrostErr != nil {
		t.Fatalf("bfl raw image task failed: %+v", bifrostErr.Error)
	}
	if task.BilledParameters == nil || task.BilledParameters.Size != "2048x2048" {
		t.Fatalf("the raw BFL body asked for 2048x2048 and is billed at %+v (the typed request asked for 512x512)", task.BilledParameters)
	}
}

// TXE-7502: a staged large image body on OpenAI's native route was streamed unparsed and priced at
// the default size. Nothing can read a streamed body's size, so it is refused 413 before the
// upstream write, on the routes where it was raw; the same body on a chat request is still raw.
func TestStagedLargeImageRequestIsRefused(t *testing.T) {
	for _, tt := range []struct {
		wire        string
		provider    schemas.ModelProvider
		requestType schemas.RequestType
		model       string
	}{
		{"openai", schemas.OpenAI, schemas.ImageGenerationRequest, "gpt-image-1"},
		{"openai", schemas.OpenAI, schemas.ImageEditRequest, "gpt-image-1"},
		{"openai", schemas.Azure, schemas.ImageGenerationRequest, "gpt-image-1"},
		{"genai", schemas.Gemini, schemas.ImageGenerationRequest, "gemini-3-pro-image-preview"},
		{"genai", schemas.Vertex, schemas.ImageGenerationRequest, "imagen-4.0-generate-001"},
	} {
		ctx := routeCtx(tt.wire)
		stageLargeRequest(ctx, `{"model":"`+tt.model+`","prompt":"a lighthouse","size":"4096x4096"}`, tt.model)
		refusal := decideLargePayloadForAttempt(ctx, tt.provider, tt.provider, tt.requestType, tt.model, tt.model)
		if refusal == nil || refusal.StatusCode == nil || *refusal.StatusCode != 413 || !strings.Contains(refusal.Error.Message, "priced by the size") {
			t.Errorf("%s %s on %s: a staged large image body must be refused 413, got %+v", tt.provider, tt.requestType, tt.wire, refusal)
		}
	}
	chat := routeCtx("openai")
	stageLargeRequest(chat, `{"model":"gpt-5","messages":[]}`, "gpt-5")
	if refusal := decideLargePayloadForAttempt(chat, schemas.OpenAI, schemas.OpenAI, schemas.ChatCompletionRequest, "gpt-5", "gpt-5"); refusal != nil {
		t.Fatalf("a staged chat body is still forwarded raw: refused %s", refusal.Error.Message)
	}
}

// The gate runs on every provider attempt; on every request that is not a raw priced image one it
// returns after a context write and a read.
func BenchmarkDecideRawImageBodyForAttempt(b *testing.B) {
	provider, err := (&Bifrost{logger: NewNoOpLogger()}).createBaseProvider(schemas.OpenAI, &schemas.ProviderConfig{})
	if err != nil {
		b.Fatal(err)
	}
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	req := rawImageRequest(schemas.OpenAI, schemas.ImageGenerationRequest, "gpt-image-1", `{}`)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = decideRawImageBodyForAttempt(ctx, provider, schemas.ImageGenerationRequest, req)
	}
}
