package gemini

import (
	"context"
	"testing"

	"github.com/capsohq/bifrost/core/schemas"
)

// A :streamGenerateContent call that another provider serves as an image stream answers with
// one Gemini chunk per completed image; partial images have no Gemini counterpart.
func TestToGeminiImageGenerationStreamResponse(t *testing.T) {
	partial, err := ToGeminiImageGenerationStreamResponse(context.Background(), &schemas.BifrostImageGenerationStreamResponse{
		Type: schemas.ImageGenerationEventTypePartial, PartialImageIndex: schemas.Ptr(0), B64JSON: "cGFydA==",
	})
	if err != nil || partial != nil {
		t.Fatalf("partial image = %+v, %v; want nothing", partial, err)
	}

	completed, err := ToGeminiImageGenerationStreamResponse(context.Background(), &schemas.BifrostImageGenerationStreamResponse{
		ID: "img_1", Type: schemas.ImageGenerationEventTypeCompleted, B64JSON: "ZnVsbA==", OutputFormat: "jpeg",
		Usage: &schemas.ImageUsage{InputTokens: 12, OutputTokens: 1290, TotalTokens: 1302},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(completed.Candidates) != 1 || len(completed.Candidates[0].Content.Parts) != 1 {
		t.Fatalf("chunk = %+v", completed)
	}
	blob := completed.Candidates[0].Content.Parts[0].InlineData
	if blob == nil || blob.Data != "ZnVsbA==" || blob.MIMEType != "image/jpeg" {
		t.Fatalf("inlineData = %+v", blob)
	}
	if completed.UsageMetadata == nil || completed.UsageMetadata.CandidatesTokenCount != 1290 || completed.UsageMetadata.PromptTokenCount != 12 {
		t.Fatalf("usage = %+v", completed.UsageMetadata)
	}
}
