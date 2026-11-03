package bedrock

import (
	"testing"

	"github.com/capsohq/bifrost/core/schemas"
)

// The request a converter reads is the one the next attempt (fallback or retry) converts
// again, so converting must leave the caller's ExtraParams untouched and give the same
// upstream parameters both times.
func TestImageConvertersLeaveCallerExtraParamsIntact(t *testing.T) {
	generation := &schemas.BifrostImageGenerationRequest{
		Model: "amazon.titan-image-generator-v2:0",
		Input: &schemas.ImageGenerationInput{Prompt: "a lighthouse"},
		Params: &schemas.ImageGenerationParameters{
			ExtraParams: map[string]interface{}{"cfgScale": 7.5, "aspect_ratio": "16:9"},
		},
	}
	for attempt := 1; attempt <= 2; attempt++ {
		req, err := ToBedrockImageGenerationRequest(generation)
		if err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
		if req.ImageGenerationConfig == nil || req.ImageGenerationConfig.CfgScale == nil || *req.ImageGenerationConfig.CfgScale != 7.5 {
			t.Fatalf("attempt %d: cfgScale not applied: %+v", attempt, req.ImageGenerationConfig)
		}
		stability, err := ToStabilityAIImageGenerationRequest(generation)
		if err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
		if stability.AspectRatio == nil || *stability.AspectRatio != "16:9" {
			t.Fatalf("attempt %d: aspect_ratio not applied", attempt)
		}
	}
	if len(generation.Params.ExtraParams) != 2 {
		t.Fatalf("caller ExtraParams changed: %v", generation.Params.ExtraParams)
	}

	edit := &schemas.BifrostImageEditRequest{
		Model: "amazon.nova-canvas-v1:0",
		Input: &schemas.ImageEditInput{Prompt: "a hat", Images: []schemas.ImageInput{{Image: []byte("\x89PNG\r\n\x1a\n")}}},
		Params: &schemas.ImageEditParameters{
			Type:        schemas.Ptr("inpainting"),
			ExtraParams: map[string]interface{}{"mask_prompt": "the head"},
		},
	}
	for attempt := 1; attempt <= 2; attempt++ {
		req, err := ToBedrockImageEditRequest(edit)
		if err != nil {
			t.Fatalf("edit attempt %d: %v", attempt, err)
		}
		if req.InPaintingParams == nil || req.InPaintingParams.MaskPrompt == nil || *req.InPaintingParams.MaskPrompt != "the head" {
			t.Fatalf("edit attempt %d: mask_prompt not applied: %+v", attempt, req.InPaintingParams)
		}
	}
	if edit.Params.ExtraParams["mask_prompt"] != "the head" {
		t.Fatalf("caller ExtraParams changed: %v", edit.Params.ExtraParams)
	}
}
