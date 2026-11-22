package bifrost

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/capsohq/bifrost/core/schemas"
)

// imageStreamCounters lists every provider that declares schemas.ImageStreamCounter for a streamed
// image request type, with the test that shows its stream relays every image and reports on the
// final frame a usage.output_images equal to the images delivered. A provider that declares it
// without a case here fails TestEveryImageStreamRefusesOrCountsSeveralImages.
var imageStreamCounters = map[schemas.ModelProvider]map[schemas.RequestType]string{
	schemas.BytePlus: {
		schemas.ImageGenerationStreamRequest: "providers/byteplus TestBytePlusImageGenerationStream",
		schemas.ImageEditStreamRequest:       "providers/byteplus TestBytePlusImageEditStreamUsesEditEvents (same handler as generation)",
	},
}

func imageStreamRequest(provider schemas.ModelProvider, requestType schemas.RequestType, model string, n *int, extra map[string]interface{}) *schemas.BifrostRequest {
	req := &schemas.BifrostRequest{RequestType: requestType}
	switch requestType {
	case schemas.ImageGenerationStreamRequest:
		req.ImageGenerationRequest = &schemas.BifrostImageGenerationRequest{
			Provider: provider,
			Model:    model,
			Input:    &schemas.ImageGenerationInput{Prompt: "a lighthouse"},
			Params:   &schemas.ImageGenerationParameters{N: n, ExtraParams: extra},
		}
	case schemas.ImageEditStreamRequest:
		req.ImageEditRequest = &schemas.BifrostImageEditRequest{
			Provider: provider,
			Model:    model,
			Input:    &schemas.ImageEditInput{Prompt: "make it blue", Images: []schemas.ImageInput{{Image: []byte("\x89PNG\r\n\x1a\n"), URL: "https://example.com/a.png"}}},
			Params:   &schemas.ImageEditParameters{N: n, ExtraParams: extra},
		}
	}
	return req
}

func assertImageStreamCountRefusal(t *testing.T, name string, err *schemas.BifrostError, param string) {
	t.Helper()
	if err == nil || err.Error == nil || err.Error.Code == nil || *err.Error.Code != string(schemas.ErrorTypeCallerUnsupportedParameter) ||
		err.Error.Param != param || err.StatusCode == nil || *err.StatusCode != 400 || err.ExtraFields.ErrorType != schemas.ErrorTypeCallerUnsupportedParameter {
		t.Errorf("%s: want 400 caller_unsupported_parameter naming %s, got %+v", name, param, err)
	}
}

// TestEveryImageStreamRefusesOrCountsSeveralImages is the class check (TXE-8077): on every
// standard provider, a streamed image generation or edit asking for two images is either refused
// before any upstream call, or reaches a provider that declares its stream relays and counts every
// image and has a test above showing it.
func TestEveryImageStreamRefusesOrCountsSeveralImages(t *testing.T) {
	client := &Bifrost{logger: NewNoOpLogger()}
	declared := map[schemas.ModelProvider]map[schemas.RequestType]bool{}
	for _, providerKey := range schemas.StandardProviders {
		provider, err := client.createBaseProvider(providerKey, &schemas.ProviderConfig{})
		if err != nil {
			t.Fatalf("construct %s: %v", providerKey, err)
		}
		for _, requestType := range schemas.ImageStreamRequestTypes {
			name := string(providerKey) + "/" + string(requestType)
			counter, ok := provider.(schemas.ImageStreamCounter)
			if ok && counter.StreamsEveryImage(requestType) {
				if declared[providerKey] == nil {
					declared[providerKey] = map[schemas.RequestType]bool{}
				}
				declared[providerKey][requestType] = true
				if imageStreamCounters[providerKey][requestType] == "" {
					t.Errorf("%s declares that its stream relays and counts every image, but imageStreamCounters has no test for it: add one showing the final usage.output_images equals the images delivered", name)
				}
				if refusal := decideImageStreamCountForAttempt(provider, requestType, imageStreamRequest(providerKey, requestType, "m", schemas.Ptr(2), nil)); refusal != nil {
					t.Errorf("%s: a provider that counts every image was refused n=2: %+v", name, refusal)
				}
				continue
			}
			refusal := decideImageStreamCountForAttempt(provider, requestType, imageStreamRequest(providerKey, requestType, "m", schemas.Ptr(2), nil))
			assertImageStreamCountRefusal(t, name, refusal, "n")
			for _, n := range []*int{nil, schemas.Ptr(0), schemas.Ptr(1)} {
				if refusal := decideImageStreamCountForAttempt(provider, requestType, imageStreamRequest(providerKey, requestType, "m", n, nil)); refusal != nil {
					t.Errorf("%s: a stream for one image must not be refused, got %+v", name, refusal)
				}
			}
		}
	}
	for providerKey, types := range imageStreamCounters {
		for requestType := range types {
			if !declared[providerKey][requestType] {
				t.Errorf("imageStreamCounters lists %s/%s, which does not declare schemas.ImageStreamCounter: remove the case", providerKey, requestType)
			}
		}
	}
}

// A count sent under a vendor's own name of n is the same request as n.
func TestImageStreamCountReadsVendorNamesOfN(t *testing.T) {
	client := &Bifrost{logger: NewNoOpLogger()}
	provider, err := client.createBaseProvider(schemas.HuggingFace, &schemas.ProviderConfig{})
	if err != nil {
		t.Fatal(err)
	}
	for _, param := range []string{"num_images", "number_of_images", "num_outputs"} {
		for _, value := range []interface{}{3, float64(3), "3"} {
			req := imageStreamRequest(schemas.HuggingFace, schemas.ImageGenerationStreamRequest, "fal-ai/flux/dev", nil, map[string]interface{}{param: value})
			assertImageStreamCountRefusal(t, param, decideImageStreamCountForAttempt(provider, schemas.ImageGenerationStreamRequest, req), param)
		}
		req := imageStreamRequest(schemas.HuggingFace, schemas.ImageGenerationStreamRequest, "fal-ai/flux/dev", nil, map[string]interface{}{param: 1})
		if refusal := decideImageStreamCountForAttempt(provider, schemas.ImageGenerationStreamRequest, req); refusal != nil {
			t.Errorf("%s=1 must not be refused, got %+v", param, refusal)
		}
	}
	// The non-streaming routes take any n.
	req := imageStreamRequest(schemas.HuggingFace, schemas.ImageGenerationStreamRequest, "fal-ai/flux/dev", schemas.Ptr(4), nil)
	if refusal := decideImageStreamCountForAttempt(provider, schemas.ImageGenerationRequest, req); refusal != nil {
		t.Errorf("a non-streaming request must not be refused, got %+v", refusal)
	}
}

// TestImageStreamSeveralImagesRefusedBeforeUpstream drives the public stream entry points of the two
// providers TXE-8077 found (replicate, huggingface). Their streams relay every image under index 0
// or by position in one event and end on a frame that carries one image and no
// usage.output_images, so a streamed n above 1 must be answered 400 with no request sent upstream.
// A stream for one image still reaches the vendor.
func TestImageStreamSeveralImagesRefusedBeforeUpstream(t *testing.T) {
	cases := []struct {
		provider schemas.ModelProvider
		model    string
	}{
		{schemas.Replicate, "black-forest-labs/flux-schnell"},
		{schemas.HuggingFace, "fal-ai/flux/dev"},
	}
	for _, tc := range cases {
		for _, requestType := range []schemas.RequestType{schemas.ImageGenerationStreamRequest, schemas.ImageEditStreamRequest} {
			t.Run(string(tc.provider)+"/"+string(requestType), func(t *testing.T) {
				var hits atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					hits.Add(1)
					w.WriteHeader(http.StatusInternalServerError)
				}))
				defer server.Close()
				account := NewMockAccount()
				account.AddProviderWithBaseURL(tc.provider, 1, 1, server.URL)
				account.configs[tc.provider].NetworkConfig.MaxRetries = 0
				account.SetKeysForProvider(tc.provider, []schemas.Key{
					{ID: "test-key", Value: *schemas.NewSecretVar("test-key"), Models: schemas.WhiteList{"*"}, Weight: 100},
				})
				client := newStreamTestClient(t, account)

				send := func(n *int) *schemas.BifrostError {
					ctx := schemas.NewBifrostContext(context.Background(), time.Now().Add(30*time.Second))
					req := imageStreamRequest(tc.provider, requestType, tc.model, n, nil)
					var stream chan *schemas.BifrostStreamChunk
					var bifrostErr *schemas.BifrostError
					if requestType == schemas.ImageGenerationStreamRequest {
						stream, bifrostErr = client.ImageGenerationStreamRequest(ctx, req.ImageGenerationRequest)
					} else {
						stream, bifrostErr = client.ImageEditStreamRequest(ctx, req.ImageEditRequest)
					}
					if stream != nil {
						for range stream {
						}
					}
					return bifrostErr
				}

				assertImageStreamCountRefusal(t, "n=2", send(schemas.Ptr(2)), "n")
				if got := hits.Load(); got != 0 {
					t.Fatalf("a refused stream reached the vendor %d time(s)", got)
				}
				send(schemas.Ptr(1))
				if hits.Load() == 0 {
					t.Errorf("a stream for one image should still reach the vendor")
				}
			})
		}
	}
}
