package bifrost

import (
	schemas "github.com/capsohq/bifrost/core/schemas"
)

// capabilityModel is one model family of a driver: the upstream model ids starting with
// prefix, probed with model. editType is the edit an image edit probe asks for, for a
// driver that requires one.
type capabilityModel struct {
	prefix   string
	model    string
	editType string
}

// capabilityCase is one driver and request type with the model families its dispatch
// tells apart. A request type a driver refuses is probed with one family covering every
// model.
type capabilityCase struct {
	provider schemas.ModelProvider
	request  schemas.RequestType
	models   []capabilityModel
}

var allModels = []capabilityModel{{prefix: "", model: "capability-probe"}}

func one(model string) []capabilityModel { return []capabilityModel{{prefix: "", model: model}} }

// capabilityDrivers lists, per provider type, the model families of each request type
// whose driver dispatches on the model id (a family the driver refuses before sending is
// absent: no row is the table's answer for it). Request types not listed are probed with
// allModels.
var capabilityDrivers = map[schemas.ModelProvider]map[schemas.RequestType][]capabilityModel{
	schemas.OpenAI: {
		schemas.EmbeddingRequest:       one("text-embedding-3-small"),
		schemas.ImageGenerationRequest: one("gpt-image-2"),
		schemas.ImageEditRequest:       one("gpt-image-2"),
		schemas.ImageVariationRequest:  one("dall-e-2"),
		schemas.VideoGenerationRequest: one("sora-2"),
		schemas.VideoRemixRequest:      one("sora-2"),
		// The OpenAI driver serves decisions natively only on its decision models.
		schemas.DecisionRequest: {{prefix: "", model: "gpt-5.5"}, {prefix: "gpt-6-luna", model: "gpt-6-luna"}},
	},
	schemas.Azure: {
		schemas.EmbeddingRequest:       one("fx-embedding-deployment"),
		schemas.ImageGenerationRequest: one("fx-image-deployment"),
		schemas.ImageEditRequest:       one("fx-image-deployment"),
		schemas.VideoGenerationRequest: one("fx-video-deployment"),
		schemas.DecisionRequest:        one("fx-decision-deployment"),
	},
	schemas.Gemini: {
		schemas.EmbeddingRequest:       one("gemini-embedding-001"),
		schemas.ImageGenerationRequest: {{prefix: "", model: "gemini-3.1-flash-image"}, {prefix: "imagen-", model: "imagen-4.0-generate-001"}},
		schemas.ImageEditRequest:       {{prefix: "", model: "gemini-3.1-flash-image"}, {prefix: "imagen-", model: "imagen-3.0-capability-001"}},
		schemas.VideoGenerationRequest: one("veo-3.1-generate-preview"),
	},
	schemas.Vertex: {
		schemas.EmbeddingRequest: {{prefix: "", model: "text-embedding-005"}, {prefix: "gemini-embedding-2", model: "gemini-embedding-2"},
			{prefix: "multimodalembedding", model: "multimodalembedding@001"}},
		schemas.ImageGenerationRequest: {{prefix: "gemini-", model: "gemini-3.1-flash-image"}, {prefix: "imagen-", model: "imagen-4.0-generate-001"}},
		schemas.ImageEditRequest:       {{prefix: "gemini-", model: "gemini-3.1-flash-image"}, {prefix: "imagen-", model: "imagen-3.0-capability-001"}},
		schemas.VideoGenerationRequest: {{prefix: "veo-", model: "veo-3.1-generate-001"}},
	},
	schemas.Bedrock: {
		schemas.EmbeddingRequest: {{prefix: "amazon.titan-embed-text", model: "amazon.titan-embed-text-v2:0"},
			{prefix: "amazon.titan-embed-image", model: "amazon.titan-embed-image-v1"},
			{prefix: "cohere.embed", model: "cohere.embed-v4:0"},
			{prefix: "amazon.nova", model: "amazon.nova-2-multimodal-embeddings-v1:0"}},
		schemas.ImageGenerationRequest: {{prefix: "", model: "amazon.titan-image-generator-v2:0"},
			{prefix: "amazon.nova-canvas", model: "amazon.nova-canvas-v1:0"},
			{prefix: "stability.", model: "stability.sd3-5-large-v1:0"}},
		// Titan and Nova Canvas edits name their task; inpainting stands for them.
		schemas.ImageEditRequest: {{prefix: "", model: "amazon.titan-image-generator-v2:0", editType: "inpainting"},
			{prefix: "amazon.nova-canvas", model: "amazon.nova-canvas-v1:0", editType: "inpainting"},
			{prefix: "stability.", model: "stability.stable-image-inpaint-v1:0"}},
		schemas.ImageVariationRequest: one("amazon.titan-image-generator-v2:0"),
	},
	schemas.Cohere:  {schemas.EmbeddingRequest: one("embed-v4.0")},
	schemas.Mistral: {schemas.EmbeddingRequest: one("mistral-embed")},
	schemas.HuggingFace: {
		// The model id is "<inference route>/<model>"; embeddings and hf-inference images
		// look the model up on the Hub first, which the proxy answers for "capability/<task>".
		schemas.EmbeddingRequest: {{prefix: "hf-inference/", model: "hf-inference/capability/feature-extraction"},
			{prefix: "nebius/", model: "nebius/capability/feature-extraction"},
			{prefix: "sambanova/", model: "sambanova/capability/feature-extraction"},
			{prefix: "scaleway/", model: "scaleway/capability/feature-extraction"}},
		schemas.ImageGenerationRequest: {{prefix: "fal-ai/", model: "fal-ai/flux/dev"},
			{prefix: "hf-inference/", model: "hf-inference/capability/text-to-image"},
			{prefix: "nebius/", model: "nebius/black-forest-labs/flux-schnell"},
			{prefix: "together/", model: "together/black-forest-labs/FLUX.1-schnell"}},
		schemas.ImageEditRequest:       {{prefix: "fal-ai/", model: "fal-ai/flux-kontext/dev"}},
		schemas.ImageVariationRequest:  {{prefix: "fal-ai/", model: "fal-ai/flux/dev"}},
		schemas.VideoGenerationRequest: {{prefix: "fal-ai/", model: "fal-ai/veo3"}},
		schemas.VideoRemixRequest:      {{prefix: "fal-ai/", model: "fal-ai/veo3"}},
		schemas.DecisionRequest:        {{prefix: "fal-ai/", model: "fal-ai/flux/dev"}},
	},
	schemas.Volcengine: {
		schemas.EmbeddingRequest:       one("doubao-embedding-large-text-250515"),
		schemas.ImageGenerationRequest: one("doubao-seedream-4-0-250828"),
		schemas.VideoGenerationRequest: one("doubao-seedance-1-0-pro-250528"),
	},
	schemas.ModelArk: {
		schemas.EmbeddingRequest:       one("skylark-embedding-large-text"),
		schemas.ImageGenerationRequest: one("seedream-4-0-250828"),
		schemas.VideoGenerationRequest: one("seedance-1-0-pro-250528"),
	},
	schemas.Cloudflare: {
		schemas.EmbeddingRequest: one("@cf/baai/bge-m3"),
		schemas.DecisionRequest:  one("@cf/cloudflare/clef"),
	},
	schemas.Databricks: {
		schemas.EmbeddingRequest: one("databricks-gte-large-en"),
		schemas.DecisionRequest:  {{prefix: "", model: "databricks-claude-sonnet-4"}, {prefix: "ai_decide", model: "ai_decide"}},
	},
	schemas.Fireworks:  {schemas.EmbeddingRequest: one("nomic-ai/nomic-embed-text-v1.5")},
	schemas.Nebius:     {schemas.EmbeddingRequest: one("BAAI/bge-en-icl"), schemas.ImageGenerationRequest: one("black-forest-labs/flux-schnell")},
	schemas.Ollama:     {schemas.EmbeddingRequest: one("nomic-embed-text"), schemas.DecisionRequest: one("qwen3")},
	schemas.OpenRouter: {schemas.EmbeddingRequest: one("openai/text-embedding-3-small"), schemas.DecisionRequest: one("openai/gpt-5.5")},
	schemas.Parasail:   {schemas.EmbeddingRequest: one("parasail-bge-m3")},
	schemas.SGL:        {schemas.EmbeddingRequest: one("Alibaba-NLP/gte-Qwen2-7B-instruct")},
	schemas.VLLM:       {schemas.EmbeddingRequest: one("BAAI/bge-m3")},
	schemas.XAI:        {schemas.ImageGenerationRequest: one("grok-2-image")},
	schemas.Replicate: {
		schemas.ImageGenerationRequest: one("black-forest-labs/flux-1.1-pro"),
		schemas.ImageEditRequest:       one("black-forest-labs/flux-kontext-pro"),
		schemas.VideoGenerationRequest: one("google/veo-3"),
	},
	schemas.Runware: {
		schemas.ImageGenerationRequest: one("runware:101@1"),
		schemas.ImageEditRequest:       one("runware:101@1"),
		schemas.VideoGenerationRequest: one("klingai:5@3"),
	},
	schemas.Runway: {
		schemas.ImageGenerationRequest: {{prefix: "", model: "gen4_image"}, {prefix: "gpt_image", model: "gpt_image_2"},
			{prefix: "gemini_image3", model: "gemini_image3_pro"}, {prefix: "gemini_2.5", model: "gemini_2.5_flash"}},
		schemas.ImageEditRequest: {{prefix: "", model: "gen4_image"}, {prefix: "gpt_image", model: "gpt_image_2"},
			{prefix: "gemini_image3", model: "gemini_image3_pro"}, {prefix: "gemini_2.5", model: "gemini_2.5_flash"}},
		schemas.VideoGenerationRequest: {{prefix: "", model: "gen4_turbo"}, {prefix: "veo", model: "veo3.1"}},
	},
	schemas.Minimax:    {schemas.ImageGenerationRequest: one("image-01")},
	schemas.Typesafe:   {schemas.DecisionRequest: one("system-one-2")},
	schemas.Perplexity: {schemas.DecisionRequest: one("sonar-decide")},
	schemas.Anthropic:  {},
	schemas.Elevenlabs: {},
}

// capabilityRequestTypes are the request types the table covers, in its order.
var capabilityRequestTypes = []schemas.RequestType{
	schemas.EmbeddingRequest, schemas.ImageGenerationRequest, schemas.ImageEditRequest, schemas.ImageVariationRequest,
	schemas.VideoGenerationRequest, schemas.VideoRemixRequest, schemas.DecisionRequest,
}

// capabilityCases are every (driver, request type) the table probes.
var capabilityCases = func() []capabilityCase {
	var out []capabilityCase
	for provider, byRequest := range capabilityDrivers {
		for _, request := range capabilityRequestTypes {
			models, ok := byRequest[request]
			if !ok {
				models = allModels
			}
			out = append(out, capabilityCase{provider: provider, request: request, models: models})
		}
	}
	return out
}()

// capabilityKey is the key a driver needs before it sends a request; serviceAccount is the
// Vertex credentials, whose token endpoint is the recording proxy.
func capabilityKey(provider schemas.ModelProvider, serviceAccount string) schemas.Key {
	key := schemas.Key{ID: "capability-table", Name: "capability-table", Value: *schemas.NewSecretVar("capability-table-key"), Weight: 1}
	switch provider {
	case schemas.Azure:
		key.AzureKeyConfig = &schemas.AzureKeyConfig{Endpoint: *schemas.NewSecretVar("https://capability-table.openai.azure.com")}
	case schemas.Vertex:
		key.Value = *schemas.NewSecretVar("")
		key.VertexKeyConfig = &schemas.VertexKeyConfig{ProjectID: *schemas.NewSecretVar("capability-table"),
			ProjectNumber: *schemas.NewSecretVar("123456789012"), Region: *schemas.NewSecretVar("us-central1"),
			AuthCredentials: *schemas.NewSecretVar(serviceAccount)}
	case schemas.Bedrock:
		key.Value = *schemas.NewSecretVar("")
		key.BedrockKeyConfig = &schemas.BedrockKeyConfig{AccessKey: *schemas.NewSecretVar("AKIACAPABILITYTABLE0"),
			SecretKey: *schemas.NewSecretVar("capability-table-secret"), Region: schemas.NewSecretVar("us-east-1")}
	case schemas.Cloudflare:
		key.CloudflareKeyConfig = &schemas.CloudflareKeyConfig{AccountID: *schemas.NewSecretVar("capability-table")}
	case schemas.Databricks:
		key.DatabricksKeyConfig = &schemas.DatabricksKeyConfig{WorkspaceURL: *schemas.NewSecretVar("https://capability-table.cloud.databricks.com")}
	case schemas.Ollama:
		key.OllamaKeyConfig = &schemas.OllamaKeyConfig{URL: *schemas.NewSecretVar("https://ollama.capability-table.example")}
	case schemas.SGL:
		key.SGLKeyConfig = &schemas.SGLKeyConfig{URL: *schemas.NewSecretVar("https://sgl.capability-table.example")}
	case schemas.VLLM:
		key.VLLMKeyConfig = &schemas.VLLMKeyConfig{URL: *schemas.NewSecretVar("https://vllm.capability-table.example"), ModelName: "BAAI/bge-m3"}
	}
	return key
}
