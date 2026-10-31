package handlers

import (
	"testing"

	"github.com/bytedance/sonic"
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

	// The fork's flat multimodal list is one input item whose parts are its entries.
	if len(bifrostReq.Input) != 1 || len(bifrostReq.Input[0].Content) != 1 ||
		bifrostReq.Input[0].Content[0].Type != schemas.EmbeddingContentPartTypeText ||
		*bifrostReq.Input[0].Content[0].Text != "the sky is blue" {
		t.Fatalf("expected one text item from the flat list, got %+v", bifrostReq.Input)
	}
	if bifrostReq.Model != "doubao-embedding-vision-251215" {
		t.Fatalf("expected model doubao-embedding-vision-251215, got %s", bifrostReq.Model)
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
	if len(bifrostReq.Input) != 1 || !schemas.EmbeddingInput(bifrostReq.Input).AllSingleText() || *bifrostReq.Input[0].Content[0].Text != "hello" {
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

// TestPrepareEmbeddingRequest_FlatMultiModalList: the fork's flat list keeps every entry, in
// order, as the parts of one item, so Volcengine embeds them into one vector as before.
func TestPrepareEmbeddingRequest_FlatMultiModalList(t *testing.T) {
	t.Parallel()

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.SetBodyString(`{
		"model": "volcengine/doubao-embedding-vision-251215",
		"input": [
			{"type":"text","text":"a photo of a cat"},
			{"type":"image_url","image_url":{"url":"https://example.com/cat.jpg"}},
			{"type":"video_url","video_url":{"url":"https://example.com/cat.mp4"}}
		]
	}`)

	_, bifrostReq, err := prepareEmbeddingRequest(ctx, nil)
	if err != nil {
		t.Fatalf("prepareEmbeddingRequest returned error: %v", err)
	}
	if len(bifrostReq.Input) != 1 {
		t.Fatalf("expected one item, got %d", len(bifrostReq.Input))
	}
	parts := bifrostReq.Input[0].Content
	if len(parts) != 3 {
		t.Fatalf("expected three parts, got %+v", parts)
	}
	if parts[0].Type != schemas.EmbeddingContentPartTypeText || *parts[0].Text != "a photo of a cat" {
		t.Fatalf("part 0 = %+v, want the text", parts[0])
	}
	if parts[1].Type != schemas.EmbeddingContentPartTypeImage || *parts[1].Image.URL != "https://example.com/cat.jpg" {
		t.Fatalf("part 1 = %+v, want the image url", parts[1])
	}
	if parts[2].Type != schemas.EmbeddingContentPartTypeVideo || *parts[2].Video.URL != "https://example.com/cat.mp4" {
		t.Fatalf("part 2 = %+v, want the video url", parts[2])
	}
}

// Upstream's item shapes must not be read as the flat list: an array of part arrays and
// items carrying content each stay one item per entry.
func TestPrepareEmbeddingRequest_UpstreamItemShapesUnchanged(t *testing.T) {
	t.Parallel()

	for _, body := range []string{
		`{"model":"openai/text-embedding-3-small","input":[[{"type":"text","text":"a"}],[{"type":"text","text":"b"}]]}`,
		`{"model":"openai/text-embedding-3-small","input":[{"content":[{"type":"text","text":"a"}]},{"content":[{"type":"text","text":"b"}],"params":{"task_type":"RETRIEVAL_QUERY"}}]}`,
	} {
		ctx := &fasthttp.RequestCtx{}
		ctx.Request.SetBodyString(body)
		_, bifrostReq, err := prepareEmbeddingRequest(ctx, nil)
		if err != nil {
			t.Fatalf("%s: prepareEmbeddingRequest returned error: %v", body, err)
		}
		if len(bifrostReq.Input) != 2 || *bifrostReq.Input[0].Content[0].Text != "a" || *bifrostReq.Input[1].Content[0].Text != "b" {
			t.Fatalf("%s: got %+v, want two items", body, bifrostReq.Input)
		}
	}
}

func TestPrepareEmbeddingRequest_FlatMultiModalListRejectsBadEntries(t *testing.T) {
	t.Parallel()

	for _, input := range []string{
		`[{"type":"image_url"}]`,
		`[{"type":"text","text":"a"},{"type":"audio_url","audio_url":{"url":"https://example.com/a.mp3"}}]`,
	} {
		ctx := &fasthttp.RequestCtx{}
		ctx.Request.SetBodyString(`{"model":"volcengine/doubao-embedding-vision-251215","input":` + input + `}`)
		if _, _, err := prepareEmbeddingRequest(ctx, nil); err == nil {
			t.Fatalf("%s: expected an error", input)
		}
	}
}

// TestEmbeddingRequestInputLastDuplicateWins pins that a body repeating "input"
// embeds the last value, as JSON decoding reads every other field, instead of an
// earlier one of a different shape the decoder left set.
func TestEmbeddingRequestInputLastDuplicateWins(t *testing.T) {
	var req EmbeddingRequest
	if err := sonic.Unmarshal([]byte(`{"input":"first","input":[15339,1917]}`), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	items := req.Input.toEmbeddingInput()
	if len(items) != 1 || len(items[0].Content) != 1 || items[0].Content[0].Text != nil || len(items[0].Content[0].Tokens) != 2 {
		t.Fatalf("embedded %+v, want the last input's two tokens", items)
	}
}

func BenchmarkEmbeddingRequestInputUnmarshal(b *testing.B) {
	data := []byte(`["first text","second text"]`)
	b.ReportAllocs()
	for b.Loop() {
		var input EmbeddingRequestInput
		if err := input.UnmarshalJSON(data); err != nil {
			b.Fatal(err)
		}
	}
}
