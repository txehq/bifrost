package bedrock

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/capsohq/bifrost/core/schemas"
)

// A Bedrock-format image request keeps a Bedrock provider's raw body, and gets another
// provider's images in Bedrock's shape: that provider's raw body is its own format.
func TestToBedrockInvokeImagesResponseRawOnlyFromBedrock(t *testing.T) {
	bedrockRaw := json.RawMessage(`{"images":["AAA"],"seeds":[7]}`)
	served := &schemas.BifrostImageGenerationResponse{
		Data:        []schemas.ImageData{{B64JSON: "AAA"}},
		ExtraFields: schemas.BifrostResponseExtraFields{Provider: schemas.Bedrock, RawResponse: bedrockRaw},
	}
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	if out, err := ToBedrockInvokeImagesResponse(ctx, served); err != nil || string(out.(json.RawMessage)) != string(bedrockRaw) {
		t.Fatalf("bedrock raw not relayed: %#v (%v)", out, err)
	}

	custom := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	custom.SetValue(schemas.BifrostContextKeyBaseProviderType, schemas.Bedrock)
	served.ExtraFields.Provider = "my-bedrock"
	if out, err := ToBedrockInvokeImagesResponse(custom, served); err != nil || string(out.(json.RawMessage)) != string(bedrockRaw) {
		t.Fatalf("custom bedrock raw not relayed: %#v (%v)", out, err)
	}

	openai := &schemas.BifrostImageGenerationResponse{
		Data: []schemas.ImageData{{B64JSON: "AAA"}, {B64JSON: "BBB"}},
		ExtraFields: schemas.BifrostResponseExtraFields{Provider: schemas.OpenAI,
			RawResponse: json.RawMessage(`{"data":[{"b64_json":"AAA"},{"b64_json":"BBB"}],"usage":{"output_tokens":2112}}`)},
	}
	out, err := ToBedrockInvokeImagesResponse(ctx, openai)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := out.(*BedrockImageGenerationResponse)
	if !ok || len(got.Images) != 2 || got.Images[1] != "BBB" {
		t.Fatalf("another provider's images not in Bedrock's shape: %#v", out)
	}
}
