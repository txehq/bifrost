package bifrost

import (
	"context"
	"slices"
	"testing"

	schemas "github.com/capsohq/bifrost/core/schemas"
)

// The table states what the drivers of this build do: facts read from their converters.
func TestCapabilityTableMeasuresTheDrivers(t *testing.T) {
	entries, err := CapabilityTable(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	row := func(provider schemas.ModelProvider, request schemas.RequestType, prefix string) CapabilityEntry {
		t.Helper()
		for _, e := range entries {
			if e.ProviderType == provider && e.RequestType == request && e.ModelPrefix == prefix {
				return e
			}
		}
		t.Fatalf("no row for %s %s %q", provider, request, prefix)
		return CapabilityEntry{}
	}
	// OpenAI's image API takes n, size and quality.
	if e := row(schemas.OpenAI, schemas.ImageGenerationRequest, ""); !e.Implemented || len(e.IgnoredParams) != 0 {
		t.Errorf("openai image_generation %+v", e)
	}
	// Gemini's generateContent returns one image and has no quality; Imagen reads n.
	if e := row(schemas.Gemini, schemas.ImageGenerationRequest, ""); !slices.Equal(e.IgnoredParams, []string{"n", "quality"}) {
		t.Errorf("gemini image_generation %+v", e)
	}
	if e := row(schemas.Gemini, schemas.ImageGenerationRequest, "imagen-"); slices.Contains(e.IgnoredParams, "n") {
		t.Errorf("gemini imagen image_generation %+v", e)
	}
	// Cohere serves embeddings and no images; it reads dimensions.
	if e := row(schemas.Cohere, schemas.EmbeddingRequest, ""); !e.Implemented || len(e.IgnoredParams) != 0 {
		t.Errorf("cohere embedding %+v", e)
	}
	if e := row(schemas.Cohere, schemas.ImageGenerationRequest, ""); e.Implemented {
		t.Errorf("cohere image_generation %+v", e)
	}
	// OpenAI serves decisions natively only on its decision models.
	if e := row(schemas.OpenAI, schemas.DecisionRequest, ""); e.Implemented {
		t.Errorf("openai decisions on a chat model %+v", e)
	}
	if e := row(schemas.OpenAI, schemas.DecisionRequest, "gpt-6-luna"); !e.Implemented {
		t.Errorf("openai decisions on gpt-6-luna %+v", e)
	}
	// Every video driver reads the duration.
	for _, e := range entries {
		if e.RequestType == schemas.VideoGenerationRequest && e.Implemented && slices.Contains(e.IgnoredParams, "seconds") {
			t.Errorf("%s video_generation %q ignores seconds", e.ProviderType, e.ModelPrefix)
		}
	}
}

// Two requests that differ only in a field the driver randomises are the same request.
func TestRequestFactsMaskVolatileFields(t *testing.T) {
	a := requestFacts([]recordedRequest{{Method: "POST", Host: "api.example", Path: "/v1", ContentType: "application/json",
		Body: []byte(`[{"taskType":"imageInference","taskUUID":"1111","width":1024}]`)}})
	b := requestFacts([]recordedRequest{{Method: "POST", Host: "api.example", Path: "/v1", ContentType: "application/json",
		Body: []byte(`[{"taskType":"imageInference","taskUUID":"2222","width":1024}]`)}})
	c := requestFacts([]recordedRequest{{Method: "POST", Host: "api.example", Path: "/v1", ContentType: "application/json",
		Body: []byte(`[{"taskType":"imageInference","taskUUID":"3333","width":1536}]`)}})
	volatile := map[string]bool{}
	for k, v := range b {
		if a[k] != v {
			volatile[k] = true
		}
	}
	if !volatile["0.body.0.taskUUID"] || len(volatile) != 1 {
		t.Fatalf("volatile %v", volatile)
	}
	if !sameFacts(a, b, volatile) || sameFacts(a, c, volatile) {
		t.Fatal("the comparison does not mask only the volatile field")
	}
}
