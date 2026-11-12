package azure

import (
	"context"
	"testing"

	"github.com/capsohq/bifrost/core/schemas"
)

// The request-path cost of the jobs API (TXE-6533). Every Azure video create now asks
// whether its model is first-generation sora, and every query, download and delete asks
// whether its id is a jobs-API job; the Sora 2 benchmarks measure what that adds to the
// requests that do not use the jobs API.

func BenchmarkAzureVideoCreateDispatchSora2(b *testing.B) {
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	b.ReportAllocs()
	for b.Loop() {
		if isAzureSoraJobsModel(ctx, "sora-2") {
			b.Fatal("sora-2 dispatched to the jobs API")
		}
	}
}

func BenchmarkAzureVideoTaskIDDispatchSora2(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		if _, ok := azureSoraJobID("video_68f0a1b2c3:azure", schemas.Azure); ok {
			b.Fatal("a Sora 2 id dispatched to the jobs API")
		}
	}
}

func BenchmarkAzureSoraJobRequest(b *testing.B) {
	request := &schemas.BifrostVideoGenerationRequest{
		Provider: schemas.Azure,
		Model:    "sora",
		Input:    &schemas.VideoGenerationInput{Prompt: "A cat playing piano in a jazz bar."},
		Params:   &schemas.VideoGenerationParameters{Seconds: schemas.Ptr("5"), Size: "480x480"},
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, bifrostErr := toAzureSoraJobRequest(request); bifrostErr != nil {
			b.Fatal(bifrostErr.Error.Message)
		}
	}
}
