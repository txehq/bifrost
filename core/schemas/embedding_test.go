package schemas

import (
	"testing"
)

// The Volcengine-native multimodal part shapes keep decoding after the move to
// EmbeddingContent: image_url/video_url parts become image/video parts with a URL.
func TestEmbeddingContent_UnmarshalJSON_LegacyMultiModalParts(t *testing.T) {
	data := []byte(`[
		{"type":"text","text":"a photo of a cat"},
		{"type":"image_url","image_url":{"url":"https://example.com/cat.jpg"}},
		{"type":"video_url","video_url":{"url":"https://example.com/cat.mp4"}}
	]`)
	var content EmbeddingContent
	if err := Unmarshal(data, &content); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(content) != 3 {
		t.Fatalf("expected 3 parts, got %d", len(content))
	}
	if content[0].Type != EmbeddingContentPartTypeText || content[0].Text == nil || *content[0].Text != "a photo of a cat" {
		t.Fatalf("unexpected text part: %+v", content[0])
	}
	if content[1].Type != EmbeddingContentPartTypeImage || content[1].Image == nil || content[1].Image.URL == nil || *content[1].Image.URL != "https://example.com/cat.jpg" {
		t.Fatalf("unexpected image part: %+v", content[1])
	}
	if content[2].Type != EmbeddingContentPartTypeVideo || content[2].Video == nil || content[2].Video.URL == nil || *content[2].Video.URL != "https://example.com/cat.mp4" {
		t.Fatalf("unexpected video part: %+v", content[2])
	}
	if err := content.Validate(); err != nil {
		t.Fatalf("normalized legacy parts must validate: %v", err)
	}
}

func TestEmbeddingContent_UnmarshalJSON_CurrentPartShapes(t *testing.T) {
	data := []byte(`[
		{"type":"image","image":{"data":"aGVsbG8=","mime_type":"image/png"}},
		{"type":"tokens","tokens":[1,2,3]}
	]`)
	var content EmbeddingContent
	if err := Unmarshal(data, &content); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if content[0].Type != EmbeddingContentPartTypeImage || content[0].Image == nil || content[0].Image.Data == nil || *content[0].Image.Data != "aGVsbG8=" {
		t.Fatalf("unexpected image part: %+v", content[0])
	}
	if content[1].Type != EmbeddingContentPartTypeTokens || len(content[1].Tokens) != 3 {
		t.Fatalf("unexpected tokens part: %+v", content[1])
	}
}

func TestEmbeddingContent_UnmarshalJSON_LegacyPartWithoutURLObjectFailsValidation(t *testing.T) {
	var content EmbeddingContent
	if err := Unmarshal([]byte(`[{"type":"image_url"}]`), &content); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := content.Validate(); err == nil {
		t.Fatal("expected validation error for an image_url part with no image_url object")
	}
}

func TestEmbeddingParameters_ForkFieldsRoundTrip(t *testing.T) {
	data := []byte(`{"encoding_format":"float","instructions":"Instruction:x\nQuery:","sparse_embedding":{"type":"enabled"}}`)
	var params EmbeddingParameters
	if err := Unmarshal(data, &params); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if params.Instructions == nil || *params.Instructions != "Instruction:x\nQuery:" {
		t.Fatalf("expected instructions to decode, got %v", params.Instructions)
	}
	if params.SparseEmbedding["type"] != "enabled" {
		t.Fatalf("expected sparse_embedding.type enabled, got %v", params.SparseEmbedding)
	}
	clone := params.Clone()
	if clone.Instructions != params.Instructions {
		t.Fatal("Clone must keep the instructions pointer")
	}
}
