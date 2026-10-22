package handlers

import (
	"testing"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

func TestPrepareEmbeddingRequest_MultimodalVolcengineFields(t *testing.T) {
	t.Parallel()

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.SetBodyString(`{
		"model": "volcengine/doubao-embedding-vision-251215",
		"input": [
			{
				"type": "text",
				"text": "the sky is blue"
			}
		],
		"instructions": "Target_modality: text and video.\nInstruction:Compress the text\\video into one word.\nQuery:",
		"encoding_format": "float",
		"dimensions": 1024,
		"sparse_embedding": {
			"type": "enabled"
		}
	}`)

	req, bifrostReq, err := prepareEmbeddingRequest(ctx, nil)
	if err != nil {
		t.Fatalf("prepareEmbeddingRequest returned error: %v", err)
	}
	if req == nil || bifrostReq == nil || bifrostReq.Params == nil {
		t.Fatal("expected non-nil request and params")
	}

	if bifrostReq.Model != "doubao-embedding-vision-251215" {
		t.Fatalf("expected model doubao-embedding-vision-251215, got %s", bifrostReq.Model)
	}
	if len(bifrostReq.Input) != 1 || len(bifrostReq.Input[0].Content) != 1 || bifrostReq.Input[0].Content[0].Type != schemas.EmbeddingContentPartTypeText {
		t.Fatalf("expected one text input item, got %+v", bifrostReq.Input)
	}

	if bifrostReq.Params.Instructions == nil || *bifrostReq.Params.Instructions == "" {
		t.Fatal("expected instructions to be parsed")
	}
	if bifrostReq.Params.EncodingFormat == nil || *bifrostReq.Params.EncodingFormat != "float" {
		t.Fatalf("expected encoding_format float, got %v", bifrostReq.Params.EncodingFormat)
	}
	if bifrostReq.Params.Dimensions == nil || *bifrostReq.Params.Dimensions != 1024 {
		t.Fatalf("expected dimensions 1024, got %v", bifrostReq.Params.Dimensions)
	}
	if bifrostReq.Params.SparseEmbedding == nil {
		t.Fatal("expected sparse_embedding to be parsed")
	}
	if sparseType, ok := bifrostReq.Params.SparseEmbedding["type"]; !ok || sparseType != "enabled" {
		t.Fatalf("expected sparse_embedding.type enabled, got %v", bifrostReq.Params.SparseEmbedding)
	}
}

func TestPrepareEmbeddingRequest_ExtractsVolcengineInstructionsConfig(t *testing.T) {
	t.Parallel()

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.SetBodyString(`{
		"model": "volcengine/doubao-embedding-vision-251215",
		"input": "hello",
		"encoding_format": "float",
		"volcengine_instructions_config": {
			"task_type": "retrieval/ranking",
			"role": "corpus",
			"target_modality": "text",
			"validate_template": true
		}
	}`)

	_, bifrostReq, err := prepareEmbeddingRequest(ctx, nil)
	if err != nil {
		t.Fatalf("prepareEmbeddingRequest returned error: %v", err)
	}
	if bifrostReq == nil || bifrostReq.Params == nil {
		t.Fatal("expected non-nil bifrost request and params")
	}
	if len(bifrostReq.Input) != 1 || len(bifrostReq.Input[0].Content) != 1 || bifrostReq.Input[0].Content[0].Text == nil || *bifrostReq.Input[0].Content[0].Text != "hello" {
		t.Fatalf("expected string input to be parsed, got %+v", bifrostReq.Input)
	}
	if bifrostReq.Params.ExtraParams == nil {
		t.Fatal("expected extra params to be populated")
	}
	rawConfig, ok := bifrostReq.Params.ExtraParams["volcengine_instructions_config"]
	if !ok {
		t.Fatalf("expected volcengine_instructions_config in extra params, got %v", bifrostReq.Params.ExtraParams)
	}
	configMap, ok := rawConfig.(map[string]any)
	if !ok {
		t.Fatalf("expected volcengine_instructions_config to be a map, got %T", rawConfig)
	}
	if taskType, ok := configMap["task_type"].(string); !ok || taskType != "retrieval/ranking" {
		t.Fatalf("expected task_type retrieval/ranking, got %#v", configMap["task_type"])
	}
	if role, ok := configMap["role"].(string); !ok || role != "corpus" {
		t.Fatalf("expected role corpus, got %#v", configMap["role"])
	}
	if validateTemplate, ok := configMap["validate_template"].(bool); !ok || !validateTemplate {
		t.Fatalf("expected validate_template true, got %#v", configMap["validate_template"])
	}
	if bifrostReq.Provider != schemas.Volcengine {
		t.Fatalf("expected provider volcengine, got %s", bifrostReq.Provider)
	}
}

// The flat Volcengine-native multimodal input is one input item: its parts are
// fused into a single vector rather than embedded one per part.
func TestPrepareEmbeddingRequest_FlatMultimodalPartsAreOneItem(t *testing.T) {
	t.Parallel()

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.SetBodyString(`{
		"model": "volcengine/doubao-embedding-vision-250615",
		"input": [
			{"type": "text", "text": "a photo of a cat"},
			{"type": "image_url", "image_url": {"url": "https://example.com/cat.jpg"}},
			{"type": "video_url", "video_url": {"url": "https://example.com/cat.mp4"}}
		]
	}`)

	_, bifrostReq, err := prepareEmbeddingRequest(ctx, nil)
	if err != nil {
		t.Fatalf("prepareEmbeddingRequest returned error: %v", err)
	}
	if len(bifrostReq.Input) != 1 {
		t.Fatalf("expected one input item, got %d", len(bifrostReq.Input))
	}
	content := bifrostReq.Input[0].Content
	if len(content) != 3 {
		t.Fatalf("expected three parts, got %+v", content)
	}
	if content[1].Type != schemas.EmbeddingContentPartTypeImage || content[1].Image == nil || content[1].Image.URL == nil || *content[1].Image.URL != "https://example.com/cat.jpg" {
		t.Fatalf("unexpected image part: %+v", content[1])
	}
	if content[2].Type != schemas.EmbeddingContentPartTypeVideo || content[2].Video == nil || content[2].Video.URL == nil || *content[2].Video.URL != "https://example.com/cat.mp4" {
		t.Fatalf("unexpected video part: %+v", content[2])
	}
}

// Upstream's item shapes still decode one item per entry next to the flat form.
func TestPrepareEmbeddingRequest_ItemArraysStayPerItem(t *testing.T) {
	t.Parallel()

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.SetBodyString(`{
		"model": "openai/text-embedding-3-small",
		"input": [
			[{"type": "text", "text": "first"}],
			{"content": [{"type": "text", "text": "second"}], "params": {"task_type": "RETRIEVAL_QUERY"}}
		]
	}`)

	_, bifrostReq, err := prepareEmbeddingRequest(ctx, nil)
	if err != nil {
		t.Fatalf("prepareEmbeddingRequest returned error: %v", err)
	}
	if len(bifrostReq.Input) != 2 {
		t.Fatalf("expected two input items, got %d", len(bifrostReq.Input))
	}
	if bifrostReq.Input[1].Params == nil || bifrostReq.Input[1].Params.TaskType == nil || *bifrostReq.Input[1].Params.TaskType != "RETRIEVAL_QUERY" {
		t.Fatalf("expected per-item params on the second item, got %+v", bifrostReq.Input[1].Params)
	}
}
