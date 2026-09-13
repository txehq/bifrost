// Package providers implements various LLM providers and their utility functions.
// This file contains the Volcengine provider implementation.
package volcengine

import (
	"bytes"
	"context"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/capsohq/bifrost/core/providers/openai"
	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	schemas "github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

const (
	volcenginePathModels               = "/models"
	volcenginePathCompletions          = "/completions"
	volcenginePathChatCompletions      = "/chat/completions"
	volcenginePathEmbeddings           = "/embeddings"
	volcenginePathMultiModalEmbeddings = "/embeddings/multimodal"
	volcenginePathImages               = "/images/generations"
	volcenginePathVideos               = "/contents/generations/tasks"
	volcenginePathFiles                = "/files"
	volcenginePathResponses            = "/responses"
)

// VolcengineProvider implements the Provider interface for Volcengine's API.
type VolcengineProvider struct {
	logger              schemas.Logger        // Logger for provider operations
	client              *fasthttp.Client      // HTTP client for API requests
	networkConfig       schemas.NetworkConfig // Network configuration including extra headers
	sendBackRawRequest  bool                  // Whether to include raw request in BifrostResponse
	sendBackRawResponse bool                  // Whether to include raw response in BifrostResponse
	providerKey         schemas.ModelProvider // Provider identifier for error/response metadata
}

// NewVolcengineProvider creates a new Volcengine provider instance.
// It initializes the HTTP client with the provided configuration and sets up response pools.
// The client is configured with timeouts, concurrency limits, and optional proxy settings.
func NewVolcengineProvider(config *schemas.ProviderConfig, logger schemas.Logger) (*VolcengineProvider, error) {
	return newVolcengineCompatibleProvider(config, logger, schemas.Volcengine, "https://ark.cn-beijing.volces.com/api/v3")
}

// NewModelArkProvider creates a new ModelArk provider instance.
// ModelArk uses the BytePlus-hosted international ARK endpoint but otherwise shares Volcengine behavior.
func NewModelArkProvider(config *schemas.ProviderConfig, logger schemas.Logger) (*VolcengineProvider, error) {
	return newVolcengineCompatibleProvider(config, logger, schemas.ModelArk, "https://ark.ap-southeast.bytepluses.com/api/v3")
}

func newVolcengineCompatibleProvider(config *schemas.ProviderConfig, logger schemas.Logger, providerKey schemas.ModelProvider, defaultBaseURL string) (*VolcengineProvider, error) {
	config.CheckAndSetDefaults()

	client := &fasthttp.Client{
		ReadTimeout:         time.Second * time.Duration(config.NetworkConfig.DefaultRequestTimeoutInSeconds),
		WriteTimeout:        time.Second * time.Duration(config.NetworkConfig.DefaultRequestTimeoutInSeconds),
		MaxConnsPerHost:     5000,
		MaxIdleConnDuration: 30 * time.Second,
		MaxConnWaitTimeout:  10 * time.Second,
	}

	// Configure proxy and retry policy
	client = providerUtils.ConfigureProxy(client, config.ProxyConfig, logger)
	client = providerUtils.ConfigureDialer(client, config.NetworkConfig.AllowPrivateNetwork)
	if config.NetworkConfig.BaseURL == "" {
		config.NetworkConfig.BaseURL = defaultBaseURL
	}
	config.NetworkConfig.BaseURL = strings.TrimRight(config.NetworkConfig.BaseURL, "/")

	return &VolcengineProvider{
		logger:              logger,
		client:              client,
		networkConfig:       config.NetworkConfig,
		sendBackRawRequest:  config.SendBackRawRequest,
		sendBackRawResponse: config.SendBackRawResponse,
		providerKey:         providerKey,
	}, nil
}

// GetProviderKey returns the provider identifier for Volcengine.
func (provider *VolcengineProvider) GetProviderKey() schemas.ModelProvider {
	return provider.providerKey
}

var volcengineVisionEmbeddingModelPrefixes = []string{
	"doubao-embedding-vision-",
	"skylark-embedding-vision-",
}

func isVolcengineVisionEmbeddingModel(model string) bool {
	normalized := strings.ToLower(strings.TrimSpace(model))
	for _, prefix := range volcengineVisionEmbeddingModelPrefixes {
		if strings.Contains(normalized, prefix) {
			return true
		}
	}
	return false
}

// ListModels performs a list models request to Volcengine's API.
func (provider *VolcengineProvider) ListModels(ctx *schemas.BifrostContext, keys []schemas.Key, request *schemas.BifrostListModelsRequest) (*schemas.BifrostListModelsResponse, *schemas.BifrostError) {
	return openai.HandleOpenAIListModelsRequest(
		ctx,
		provider.client,
		request,
		provider.networkConfig.BaseURL+providerUtils.GetPathFromContext(ctx, volcenginePathModels),
		keys,
		provider.networkConfig.ExtraHeaders,
		provider.GetProviderKey(),
		providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest),
		providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse),
	)
}

// TextCompletion performs a text completion request to the Volcengine API.
func (provider *VolcengineProvider) TextCompletion(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostTextCompletionRequest) (*schemas.BifrostTextCompletionResponse, *schemas.BifrostError) {
	return openai.HandleOpenAITextCompletionRequest(
		ctx,
		provider.client,
		provider.networkConfig.BaseURL+providerUtils.GetPathFromContext(ctx, volcenginePathCompletions),
		request,
		openai.BearerAuthHeader(key),
		provider.networkConfig.ExtraHeaders,
		provider.GetProviderKey(),
		providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest),
		providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse),
		nil,
		nil,
		provider.logger,
	)
}

// TextCompletionStream performs a streaming text completion request to Volcengine's API.
// It formats the request, sends it to Volcengine, and processes the response.
// Returns a channel of BifrostStreamChunk objects or an error if the request fails.
func (provider *VolcengineProvider) TextCompletionStream(ctx *schemas.BifrostContext, postHookRunner schemas.PostHookRunner, postHookSpanFinalizer func(context.Context), key schemas.Key, request *schemas.BifrostTextCompletionRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	var authHeader map[string]string
	if key.Value.GetValue() != "" {
		authHeader = map[string]string{"Authorization": "Bearer " + key.Value.GetValue()}
	}
	return openai.HandleOpenAITextCompletionStreaming(
		ctx,
		provider.client,
		provider.networkConfig.BaseURL+providerUtils.GetPathFromContext(ctx, volcenginePathCompletions),
		request,
		authHeader,
		provider.networkConfig.ExtraHeaders,
		provider.networkConfig.StreamIdleTimeoutInSeconds,
		providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest),
		providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse),
		provider.GetProviderKey(),
		nil,
		postHookRunner,
		nil,
		nil,
		provider.logger,
		postHookSpanFinalizer,
	)
}

// ChatCompletion performs a chat completion request to the Volcengine API.
func (provider *VolcengineProvider) ChatCompletion(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostChatRequest) (*schemas.BifrostChatResponse, *schemas.BifrostError) {
	return openai.HandleOpenAIChatCompletionRequest(
		ctx,
		provider.client,
		provider.networkConfig.BaseURL+providerUtils.GetPathFromContext(ctx, volcenginePathChatCompletions),
		request,
		openai.BearerAuthHeader(key),
		provider.networkConfig.ExtraHeaders,
		providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest),
		providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse),
		provider.GetProviderKey(),
		nil,
		nil,
		nil,
		provider.logger,
	)
}

// ChatCompletionStream performs a streaming chat completion request to the Volcengine API.
// It supports real-time streaming of responses using Server-Sent Events (SSE).
// Uses Volcengine's OpenAI-compatible streaming format.
// Returns a channel containing BifrostStreamChunk objects representing the stream or an error if the request fails.
func (provider *VolcengineProvider) ChatCompletionStream(ctx *schemas.BifrostContext, postHookRunner schemas.PostHookRunner, postHookSpanFinalizer func(context.Context), key schemas.Key, request *schemas.BifrostChatRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	var authHeader map[string]string
	if key.Value.GetValue() != "" {
		authHeader = map[string]string{"Authorization": "Bearer " + key.Value.GetValue()}
	}
	// Use shared OpenAI-compatible streaming logic
	return openai.HandleOpenAIChatCompletionStreaming(
		ctx,
		provider.client,
		provider.networkConfig.BaseURL+providerUtils.GetPathFromContext(ctx, volcenginePathChatCompletions),
		request,
		authHeader,
		provider.networkConfig.ExtraHeaders,
		provider.networkConfig.StreamIdleTimeoutInSeconds,
		providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest),
		providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse),
		provider.GetProviderKey(),
		postHookRunner,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		provider.logger,
		postHookSpanFinalizer,
	)
}

// Responses performs a responses request to the Volcengine API.
func (provider *VolcengineProvider) Responses(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostResponsesRequest) (*schemas.BifrostResponsesResponse, *schemas.BifrostError) {
	return openai.HandleOpenAIResponsesRequest(
		ctx,
		provider.client,
		provider.networkConfig.BaseURL+providerUtils.GetPathFromContext(ctx, volcenginePathResponses),
		request,
		openai.BearerAuthHeader(key),
		provider.networkConfig.ExtraHeaders,
		providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest),
		providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse),
		provider.GetProviderKey(),
		nil,
		nil,
		nil,
		provider.logger,
	)
}

// ResponsesStream performs a streaming responses request to the Volcengine API.
func (provider *VolcengineProvider) ResponsesStream(ctx *schemas.BifrostContext, postHookRunner schemas.PostHookRunner, postHookSpanFinalizer func(context.Context), key schemas.Key, request *schemas.BifrostResponsesRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	var authHeader map[string]string
	if key.Value.GetValue() != "" {
		authHeader = map[string]string{"Authorization": "Bearer " + key.Value.GetValue()}
	}
	return openai.HandleOpenAIResponsesStreaming(
		ctx,
		provider.client,
		provider.networkConfig.BaseURL+providerUtils.GetPathFromContext(ctx, volcenginePathResponses),
		request,
		authHeader,
		provider.networkConfig.ExtraHeaders,
		provider.networkConfig.StreamIdleTimeoutInSeconds,
		providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest),
		providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse),
		provider.GetProviderKey(),
		postHookRunner,
		nil,
		nil,
		nil,
		nil,
		nil,
		provider.logger,
		postHookSpanFinalizer,
	)
}

func (provider *VolcengineProvider) Embedding(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostEmbeddingRequest) (*schemas.BifrostEmbeddingResponse, *schemas.BifrostError) {
	// Doubao vision embeddings use the multimodal endpoint even for text-only input.
	if request != nil && request.Input != nil && (request.Input.IsMultiModal() || (isVolcengineVisionEmbeddingModel(request.Model) && request.Input.Text != nil)) {
		return provider.multiModalEmbedding(ctx, key, request)
	}
	return openai.HandleOpenAIEmbeddingRequest(
		ctx,
		provider.client,
		provider.networkConfig.BaseURL+providerUtils.GetPathFromContext(ctx, volcenginePathEmbeddings),
		request,
		openai.BearerAuthHeader(key),
		provider.networkConfig.ExtraHeaders,
		provider.GetProviderKey(),
		providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest),
		providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse),
		nil,
		nil,
		provider.logger,
	)
}

// volcengineMultiModalEmbeddingRequest is the native Volcengine request for /embeddings/multimodal.
type volcengineMultiModalEmbeddingRequest struct {
	Model           string                             `json:"model"`
	Input           []schemas.MultiModalEmbeddingInput `json:"input"`
	Instructions    *string                            `json:"instructions,omitempty"`
	EncodingFormat  *string                            `json:"encoding_format,omitempty"`
	Dimensions      *int                               `json:"dimensions,omitempty"`
	SparseEmbedding map[string]interface{}             `json:"sparse_embedding,omitempty"`
}

// volcengineMultiModalEmbeddingResponse is the native Volcengine response from /embeddings/multimodal.
type volcengineMultiModalEmbeddingResponse struct {
	ID      string                            `json:"id,omitempty"`
	Created int64                             `json:"created,omitempty"`
	Data    volcengineMultiModalEmbeddingData `json:"data"`
	Model   string                            `json:"model"`
	Object  string                            `json:"object,omitempty"`
	Usage   *volcengineMultiModalUsage        `json:"usage,omitempty"`
}

type volcengineMultiModalEmbeddingData struct {
	Embedding       []float32                      `json:"embedding"`
	SparseEmbedding []schemas.EmbeddingSparseValue `json:"sparse_embedding,omitempty"`
	Object          string                         `json:"object,omitempty"`
}

type volcengineMultiModalUsage struct {
	PromptTokens        int                              `json:"prompt_tokens,omitempty"`
	PromptTokensDetails *schemas.ChatPromptTokensDetails `json:"prompt_tokens_details,omitempty"`
	TotalTokens         int                              `json:"total_tokens"`
}

const (
	volcengineInstructionsConfigKey = "volcengine_instructions_config"

	volcengineVisionVersionSparseSupported       = 250615
	volcengineVisionVersionInstructionsSupported = 251215
)

type volcengineInstructionsConfig struct {
	TaskType         string `json:"task_type,omitempty"`         // retrieval_ranking | clustering_classification_sts
	Role             string `json:"role,omitempty"`              // query | corpus (for retrieval/ranking task types)
	TargetModality   string `json:"target_modality,omitempty"`   // text | image | video | text and video | text/image/video ...
	CorpusModality   string `json:"corpus_modality,omitempty"`   // text | image | video | text and image | text and video ...
	Instruction      string `json:"instruction,omitempty"`       // user-defined instruction body
	ValidateTemplate *bool  `json:"validate_template,omitempty"` // optional strict template validation
}

func parseVolcengineVisionModelDate(model string) (int, bool) {
	normalized := strings.ToLower(strings.TrimSpace(model))
	for _, prefix := range volcengineVisionEmbeddingModelPrefixes {
		idx := strings.Index(normalized, prefix)
		if idx < 0 {
			continue
		}
		verPart := normalized[idx+len(prefix):]
		if len(verPart) < 6 {
			return 0, false
		}
		verPart = verPart[:6]
		for _, r := range verPart {
			if r < '0' || r > '9' {
				return 0, false
			}
		}
		version, err := strconv.Atoi(verPart)
		if err != nil {
			return 0, false
		}
		return version, true
	}
	return 0, false
}

func parseVolcengineInstructionsConfig(params *schemas.EmbeddingParameters) (*volcengineInstructionsConfig, error) {
	if params == nil || params.ExtraParams == nil {
		return nil, nil
	}
	raw, ok := params.ExtraParams[volcengineInstructionsConfigKey]
	if !ok || raw == nil {
		return nil, nil
	}
	payload, err := schemas.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("failed to parse %s: %w", volcengineInstructionsConfigKey, err)
	}
	var cfg volcengineInstructionsConfig
	if err := schemas.Unmarshal(payload, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse %s: %w", volcengineInstructionsConfigKey, err)
	}
	return &cfg, nil
}

func normalizeVolcengineTaskType(taskType string) string {
	normalized := strings.ToLower(strings.TrimSpace(taskType))
	if normalized == "" {
		return ""
	}
	normalized = strings.NewReplacer("/", "_", "-", "_", " ", "_").Replace(normalized)

	switch normalized {
	case "retrieval", "ranking", "retrieval_ranking", "retrieval_query", "retrieval_document", "recall", "rerank", "召回", "排序", "召回排序":
		return "retrieval_ranking"
	case "clustering", "classification", "sts", "clustering_classification_sts", "semantic_similarity", "semantic_text_similarity", "聚类", "分类", "语义文本相似度":
		return "clustering_classification_sts"
	}

	if strings.Contains(normalized, "retrieval") || strings.Contains(normalized, "ranking") || strings.Contains(normalized, "recall") || strings.Contains(normalized, "rerank") || strings.Contains(normalized, "query") || strings.Contains(normalized, "document") {
		return "retrieval_ranking"
	}
	if strings.Contains(normalized, "clustering") || strings.Contains(normalized, "classification") || strings.Contains(normalized, "sts") || strings.Contains(normalized, "semantic") || strings.Contains(normalized, "similarity") {
		return "clustering_classification_sts"
	}

	return ""
}

func normalizeVolcengineRole(role string) string {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "query", "查询":
		return "query"
	case "corpus", "语料":
		return "corpus"
	default:
		return ""
	}
}

func buildVolcengineInstructionsFromConfig(cfg *volcengineInstructionsConfig) (string, error) {
	taskType := normalizeVolcengineTaskType(cfg.TaskType)
	if taskType == "" {
		return "", fmt.Errorf("%s.task_type must be one of retrieval/ranking or clustering/classification/sts", volcengineInstructionsConfigKey)
	}

	targetModality := strings.TrimSpace(cfg.TargetModality)
	instructionBody := strings.TrimSpace(cfg.Instruction)
	role := normalizeVolcengineRole(cfg.Role)

	if taskType == "retrieval_ranking" {
		if role == "" {
			return "", fmt.Errorf("%s.role is required for retrieval/ranking and must be query or corpus", volcengineInstructionsConfigKey)
		}
		if role == "query" {
			if targetModality == "" || instructionBody == "" {
				return "", fmt.Errorf("%s.query requires target_modality and instruction", volcengineInstructionsConfigKey)
			}
			return fmt.Sprintf("Target_modality: %s.\nInstruction:%s\nQuery:", targetModality, instructionBody), nil
		}

		corpusModality := strings.TrimSpace(cfg.CorpusModality)
		if corpusModality == "" {
			corpusModality = targetModality
		}
		if corpusModality == "" {
			return "", fmt.Errorf("%s.corpus requires corpus_modality (or target_modality)", volcengineInstructionsConfigKey)
		}
		return fmt.Sprintf("Instruction:Compress the %s into one word.\nQuery:", corpusModality), nil
	}

	if targetModality == "" || instructionBody == "" {
		return "", fmt.Errorf("%s for clustering/classification/sts requires target_modality and instruction", volcengineInstructionsConfigKey)
	}
	return fmt.Sprintf("Target_modality: %s.\nInstruction:%s\nQuery:", targetModality, instructionBody), nil
}

func validateVolcengineInstructionTemplate(instructions string) error {
	instructions = strings.TrimSpace(instructions)
	if instructions == "" {
		return fmt.Errorf("instructions must not be empty")
	}

	const queryPrefix = "Target_modality: "
	const querySplit = ".\nInstruction:"
	const querySuffix = "\nQuery:"
	if strings.HasPrefix(instructions, queryPrefix) && strings.HasSuffix(instructions, querySuffix) {
		rest := strings.TrimSuffix(strings.TrimPrefix(instructions, queryPrefix), querySuffix)
		parts := strings.SplitN(rest, querySplit, 2)
		if len(parts) != 2 {
			return fmt.Errorf("instructions must follow template: Target_modality: {}.\\nInstruction:{}\\nQuery:")
		}
		targetModality := strings.TrimSpace(parts[0])
		instructionBody := strings.TrimSpace(parts[1])
		if targetModality == "" || instructionBody == "" {
			return fmt.Errorf("instructions must follow template: Target_modality: {}.\\nInstruction:{}\\nQuery:")
		}
		return nil
	}

	const corpusPrefix = "Instruction:Compress the "
	const corpusSuffix = " into one word.\nQuery:"
	if strings.HasPrefix(instructions, corpusPrefix) && strings.HasSuffix(instructions, corpusSuffix) {
		modality := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(instructions, corpusPrefix), corpusSuffix))
		if modality == "" {
			return fmt.Errorf("instructions must follow template: Instruction:Compress the {} into one word.\\nQuery:")
		}
		return nil
	}

	return fmt.Errorf("instructions must follow one of templates: Target_modality: {}.\\nInstruction:{}\\nQuery: OR Instruction:Compress the {} into one word.\\nQuery:")
}

func isDisallowedDefaultInstruction(instructions string) bool {
	defaultBody := "compress the text into one word"
	normalized := strings.ToLower(strings.TrimSpace(instructions))
	normalized = strings.TrimSuffix(normalized, ".")
	if normalized == defaultBody {
		return true
	}

	const queryPrefix = "Target_modality: "
	const querySplit = ".\nInstruction:"
	const querySuffix = "\nQuery:"
	if strings.HasPrefix(instructions, queryPrefix) && strings.HasSuffix(instructions, querySuffix) {
		rest := strings.TrimSuffix(strings.TrimPrefix(instructions, queryPrefix), querySuffix)
		parts := strings.SplitN(rest, querySplit, 2)
		if len(parts) == 2 {
			body := strings.ToLower(strings.TrimSpace(parts[1]))
			body = strings.TrimSuffix(body, ".")
			return body == defaultBody
		}
	}
	return false
}

func isSparseEmbeddingEnabled(sparseEmbedding map[string]interface{}) bool {
	if sparseEmbedding == nil {
		return false
	}
	rawType, ok := sparseEmbedding["type"]
	if !ok {
		return false
	}
	typeValue, ok := rawType.(string)
	if !ok {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(typeValue), "enabled")
}

func isTextOnlyMultiModalInputs(inputs []schemas.MultiModalEmbeddingInput) bool {
	if len(inputs) == 0 {
		return false
	}
	for _, input := range inputs {
		if input.Type != schemas.MultiModalEmbeddingText {
			return false
		}
	}
	return true
}

func resolveAndValidateVolcengineInstructions(request *schemas.BifrostEmbeddingRequest) (*string, bool, *schemas.BifrostError) {
	var params *schemas.EmbeddingParameters
	if request != nil {
		params = request.Params
	}

	var instructions *string
	if params != nil {
		instructions = params.Instructions
	}

	config, err := parseVolcengineInstructionsConfig(params)
	if err != nil {
		return nil, false, providerUtils.NewBifrostOperationError(err.Error(), err)
	}

	shouldValidateTemplate := false
	if config != nil && config.ValidateTemplate != nil {
		shouldValidateTemplate = *config.ValidateTemplate
	}

	if (instructions == nil || strings.TrimSpace(*instructions) == "") && config != nil {
		generated, genErr := buildVolcengineInstructionsFromConfig(config)
		if genErr != nil {
			return nil, false, providerUtils.NewBifrostOperationError(genErr.Error(), genErr)
		}
		instructions = &generated
		shouldValidateTemplate = true
	}

	modelVersion, hasModelVersion := parseVolcengineVisionModelDate(request.Model)
	hasInstructions := instructions != nil && strings.TrimSpace(*instructions) != ""

	if hasModelVersion && hasInstructions && modelVersion < volcengineVisionVersionInstructionsSupported {
		return nil, false, providerUtils.NewBifrostOperationError(
			fmt.Sprintf("instructions are supported only for doubao/skylark-embedding-vision-%d and later models", volcengineVisionVersionInstructionsSupported),
			nil,
		)
	}
	if hasModelVersion && modelVersion >= volcengineVisionVersionInstructionsSupported && !hasInstructions {
		return nil, false, providerUtils.NewBifrostOperationError("instructions are required for doubao/skylark-embedding-vision-251215 and later models", nil)
	}
	if hasInstructions && isDisallowedDefaultInstruction(*instructions) {
		return nil, false, providerUtils.NewBifrostOperationError("instructions must be customized for your business scenario; default instruction is not allowed", nil)
	}
	if hasInstructions && shouldValidateTemplate {
		if err := validateVolcengineInstructionTemplate(*instructions); err != nil {
			return nil, false, providerUtils.NewBifrostOperationError(err.Error(), err)
		}
	}

	return instructions, shouldValidateTemplate, nil
}

func validateVolcengineSparseEmbeddingConstraints(request *schemas.BifrostEmbeddingRequest, sparseEmbedding map[string]interface{}) *schemas.BifrostError {
	if !isSparseEmbeddingEnabled(sparseEmbedding) {
		return nil
	}

	modelVersion, hasModelVersion := parseVolcengineVisionModelDate(request.Model)
	if hasModelVersion && modelVersion < volcengineVisionVersionSparseSupported {
		return providerUtils.NewBifrostOperationError(
			fmt.Sprintf("sparse_embedding is supported only for doubao/skylark-embedding-vision-%d and later models", volcengineVisionVersionSparseSupported),
			nil,
		)
	}

	if request.Input == nil || !isTextOnlyMultiModalInputs(request.Input.MultiModalInputs) {
		return providerUtils.NewBifrostOperationError("sparse_embedding supports text-only multimodal input", nil)
	}

	return nil
}

func (provider *VolcengineProvider) multiModalEmbedding(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostEmbeddingRequest) (*schemas.BifrostEmbeddingResponse, *schemas.BifrostError) {
	if request == nil || request.Input == nil {
		return nil, providerUtils.NewBifrostOperationError("invalid request: multimodal embedding input is required", nil)
	}

	resolvedInstructions, _, bifrostErr := resolveAndValidateVolcengineInstructions(request)
	if bifrostErr != nil {
		return nil, bifrostErr
	}

	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)
	req.SetRequestURI(provider.networkConfig.BaseURL + providerUtils.GetPathFromContext(ctx, volcenginePathMultiModalEmbeddings))
	req.Header.SetMethod(http.MethodPost)
	req.Header.SetContentType("application/json")

	if key.Value.GetValue() != "" {
		req.Header.Set("Authorization", "Bearer "+key.Value.GetValue())
	}

	nativeReq := volcengineMultiModalEmbeddingRequest{
		Model: request.Model,
		Input: request.Input.MultiModalInputs,
	}
	if len(nativeReq.Input) == 0 && request.Input.Text != nil {
		nativeReq.Input = []schemas.MultiModalEmbeddingInput{
			{
				Type: schemas.MultiModalEmbeddingText,
				Text: request.Input.Text,
			},
		}
	}
	if len(nativeReq.Input) == 0 {
		return nil, providerUtils.NewBifrostOperationError("volcengine multimodal embedding requires multimodal input or a single text input", nil)
	}
	if request.Params != nil {
		nativeReq.Instructions = resolvedInstructions
		nativeReq.EncodingFormat = request.Params.EncodingFormat
		nativeReq.Dimensions = request.Params.Dimensions
		nativeReq.SparseEmbedding = request.Params.SparseEmbedding
	}
	if bifrostErr := validateVolcengineSparseEmbeddingConstraints(request, nativeReq.SparseEmbedding); bifrostErr != nil {
		return nil, bifrostErr
	}

	jsonData, err := schemas.Marshal(nativeReq)
	if err != nil {
		return nil, providerUtils.NewBifrostOperationError("failed to marshal multimodal embedding request", err)
	}
	req.SetBody(jsonData)

	latency, bifrostErr, wait := providerUtils.MakeRequestWithContext(ctx, provider.client, req, resp)
	wait()
	if bifrostErr != nil {
		return nil, bifrostErr
	}

	if resp.StatusCode() != fasthttp.StatusOK {
		provider.logger.Debug(fmt.Sprintf("error from volcengine multimodal embedding: %s", string(resp.Body())))
		return nil, openai.ParseOpenAIError(resp)
	}

	body, err := providerUtils.CheckAndDecodeBody(resp)
	if err != nil {
		return nil, providerUtils.NewBifrostOperationError(schemas.ErrProviderResponseDecode, err)
	}

	var nativeResp volcengineMultiModalEmbeddingResponse
	if err := schemas.Unmarshal(body, &nativeResp); err != nil {
		return nil, providerUtils.NewBifrostOperationError(schemas.ErrProviderResponseDecode, err)
	}

	// Convert to standard bifrost embedding response
	object := nativeResp.Data.Object
	if object == "" {
		object = "embedding"
	}
	responseObject := nativeResp.Object
	if responseObject == "" {
		responseObject = "list"
	}
	response := &schemas.BifrostEmbeddingResponse{
		Data: []schemas.EmbeddingData{
			{
				Index:           0,
				Object:          object,
				SparseEmbedding: nativeResp.Data.SparseEmbedding,
				Embedding: schemas.EmbeddingStruct{
					EmbeddingArray: convertFloat32SliceToFloat64(nativeResp.Data.Embedding),
				},
			},
		},
		Model:  nativeResp.Model,
		Object: responseObject,
		ExtraFields: schemas.BifrostResponseExtraFields{
			Provider:               provider.GetProviderKey(),
			OriginalModelRequested: request.Model,
			RequestType:            schemas.EmbeddingRequest,
			Latency:                latency.Milliseconds(),
		},
	}

	if nativeResp.Usage != nil {
		response.Usage = &schemas.BifrostLLMUsage{
			PromptTokens:        nativeResp.Usage.PromptTokens,
			PromptTokensDetails: nativeResp.Usage.PromptTokensDetails,
			TotalTokens:         nativeResp.Usage.TotalTokens,
		}
	}

	if providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest) {
		response.ExtraFields.RawRequest = jsonData
	}
	if providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse) {
		response.ExtraFields.RawResponse = body
	}

	return response, nil
}

func convertFloat32SliceToFloat64(values []float32) []float64 {
	if len(values) == 0 {
		return nil
	}
	out := make([]float64, len(values))
	for i, v := range values {
		out[i] = float64(v)
	}
	return out
}

// Speech is not supported by the Volcengine provider.
func (provider *VolcengineProvider) Speech(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostSpeechRequest) (*schemas.BifrostSpeechResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.SpeechRequest, provider.GetProviderKey())
}

// SpeechStream is not supported by the Volcengine provider.
func (provider *VolcengineProvider) SpeechStream(ctx *schemas.BifrostContext, postHookRunner schemas.PostHookRunner, postHookSpanFinalizer func(context.Context), key schemas.Key, request *schemas.BifrostSpeechRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.SpeechStreamRequest, provider.GetProviderKey())
}

// Transcription is not supported by the Volcengine provider.
func (provider *VolcengineProvider) Transcription(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostTranscriptionRequest) (*schemas.BifrostTranscriptionResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.TranscriptionRequest, provider.GetProviderKey())
}

// TranscriptionStream is not supported by the Volcengine provider.
func (provider *VolcengineProvider) TranscriptionStream(ctx *schemas.BifrostContext, postHookRunner schemas.PostHookRunner, postHookSpanFinalizer func(context.Context), key schemas.Key, request *schemas.BifrostTranscriptionRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.TranscriptionStreamRequest, provider.GetProviderKey())
}

// Rerank is not supported by the Volcengine provider.
func (provider *VolcengineProvider) Rerank(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostRerankRequest) (*schemas.BifrostRerankResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.RerankRequest, provider.GetProviderKey())
}

// OCR is not supported by the Volcengine provider.
func (provider *VolcengineProvider) OCR(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostOCRRequest) (*schemas.BifrostOCRResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.OCRRequest, provider.GetProviderKey())
}

func (provider *VolcengineProvider) ImageGeneration(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostImageGenerationRequest) (*schemas.BifrostImageGenerationResponse, *schemas.BifrostError) {
	return openai.HandleOpenAIImageGenerationRequest(
		ctx,
		provider.client,
		provider.networkConfig.BaseURL+providerUtils.GetPathFromContext(ctx, volcenginePathImages),
		request,
		key,
		provider.networkConfig.ExtraHeaders,
		nil,
		provider.GetProviderKey(),
		providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest),
		providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse),
		provider.logger,
	)
}

// ImageGenerationStream is not supported by the Volcengine provider.
func (provider *VolcengineProvider) ImageGenerationStream(ctx *schemas.BifrostContext, postHookRunner schemas.PostHookRunner, postHookSpanFinalizer func(context.Context), key schemas.Key, request *schemas.BifrostImageGenerationRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ImageGenerationStreamRequest, provider.GetProviderKey())
}

// ImageEdit is not supported by the Volcengine provider.
func (provider *VolcengineProvider) ImageEdit(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostImageEditRequest) (*schemas.BifrostImageGenerationResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ImageEditRequest, provider.GetProviderKey())
}

// ImageEditStream is not supported by the Volcengine provider.
func (provider *VolcengineProvider) ImageEditStream(ctx *schemas.BifrostContext, postHookRunner schemas.PostHookRunner, postHookSpanFinalizer func(context.Context), key schemas.Key, request *schemas.BifrostImageEditRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ImageEditStreamRequest, provider.GetProviderKey())
}

// ImageVariation is not supported by the Volcengine provider.
func (provider *VolcengineProvider) ImageVariation(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostImageVariationRequest) (*schemas.BifrostImageGenerationResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ImageVariationRequest, provider.GetProviderKey())
}

func (provider *VolcengineProvider) VideoGeneration(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostVideoGenerationRequest) (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError) {
	return openai.HandleOpenAIVideoGenerationRequest(
		ctx,
		provider.client,
		provider.networkConfig.BaseURL+providerUtils.GetPathFromContext(ctx, volcenginePathVideos),
		request,
		key,
		provider.networkConfig.ExtraHeaders,
		nil,
		provider.GetProviderKey(),
		providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest),
		providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse),
		provider.logger,
	)
}

func (provider *VolcengineProvider) VideoRetrieve(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostVideoRetrieveRequest) (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError) {
	videoID := providerUtils.StripVideoIDProviderSuffix(request.ID, provider.GetProviderKey())
	return openai.HandleOpenAIVideoRetrieveRequest(
		ctx,
		provider.client,
		provider.networkConfig.BaseURL+providerUtils.GetPathFromContext(ctx, fmt.Sprintf("%s/%s", volcenginePathVideos, videoID)),
		request,
		key,
		provider.networkConfig.ExtraHeaders,
		nil,
		provider.GetProviderKey(),
		providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest),
		providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse),
		provider.VideoDownload,
		provider.logger,
	)
}

func (provider *VolcengineProvider) VideoDownload(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostVideoDownloadRequest) (*schemas.BifrostVideoDownloadResponse, *schemas.BifrostError) {
	videoID := providerUtils.StripVideoIDProviderSuffix(request.ID, provider.GetProviderKey())
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)
	req.SetRequestURI(provider.networkConfig.BaseURL + providerUtils.GetPathFromContext(ctx, fmt.Sprintf("%s/%s/content", volcenginePathVideos, videoID)))
	req.Header.SetMethod(http.MethodGet)
	if key.Value.GetValue() != "" {
		req.Header.Set("Authorization", "Bearer "+key.Value.GetValue())
	}

	latency, bifrostErr, wait := providerUtils.MakeRequestWithContext(ctx, provider.client, req, resp)
	wait()
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	if resp.StatusCode() != fasthttp.StatusOK {
		return nil, openai.ParseOpenAIError(resp)
	}

	body, err := providerUtils.CheckAndDecodeBody(resp)
	if err != nil {
		return nil, providerUtils.NewBifrostOperationError(schemas.ErrProviderResponseDecode, err)
	}

	contentType := string(resp.Header.ContentType())
	if contentType == "" {
		contentType = "video/mp4"
	}

	return &schemas.BifrostVideoDownloadResponse{
		VideoID:     providerUtils.AddVideoIDProviderSuffix(videoID, provider.GetProviderKey()),
		Content:     append([]byte(nil), body...),
		ContentType: contentType,
		ExtraFields: schemas.BifrostResponseExtraFields{
			RequestType: schemas.VideoDownloadRequest,
			Provider:    provider.GetProviderKey(),
			Latency:     latency.Milliseconds(),
		},
	}, nil
}

func (provider *VolcengineProvider) VideoDelete(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostVideoDeleteRequest) (*schemas.BifrostVideoDeleteResponse, *schemas.BifrostError) {
	videoID := providerUtils.StripVideoIDProviderSuffix(request.ID, provider.GetProviderKey())
	return openai.HandleOpenAIVideoDeleteRequest(
		ctx,
		provider.client,
		provider.networkConfig.BaseURL+providerUtils.GetPathFromContext(ctx, fmt.Sprintf("%s/%s", volcenginePathVideos, videoID)),
		videoID,
		key,
		provider.networkConfig.ExtraHeaders,
		nil,
		provider.GetProviderKey(),
		providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest),
		providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse),
		provider.logger,
	)
}

func (provider *VolcengineProvider) VideoList(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostVideoListRequest) (*schemas.BifrostVideoListResponse, *schemas.BifrostError) {
	return openai.HandleOpenAIVideoListRequest(
		ctx,
		provider.client,
		provider.networkConfig.BaseURL+providerUtils.GetPathFromContext(ctx, volcenginePathVideos),
		request,
		key,
		provider.networkConfig.ExtraHeaders,
		nil,
		provider.GetProviderKey(),
		providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest),
		providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse),
		provider.logger,
	)
}

// VideoRemix is not supported by Volcengine provider.
func (provider *VolcengineProvider) VideoEdit(_ *schemas.BifrostContext, _ schemas.Key, _ *schemas.BifrostVideoEditRequest) (*schemas.BifrostVideoEditResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.VideoEditRequest, provider.GetProviderKey())
}

func (provider *VolcengineProvider) VideoRemix(_ *schemas.BifrostContext, _ schemas.Key, _ *schemas.BifrostVideoRemixRequest) (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.VideoRemixRequest, provider.GetProviderKey())
}

func (provider *VolcengineProvider) FileUpload(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostFileUploadRequest) (*schemas.BifrostFileUploadResponse, *schemas.BifrostError) {
	if len(request.File) == 0 {
		return nil, providerUtils.NewBifrostOperationError("file content is required", nil)
	}
	if request.Purpose == "" {
		return nil, providerUtils.NewBifrostOperationError("purpose is required", nil)
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	if err := writer.WriteField("purpose", string(request.Purpose)); err != nil {
		return nil, providerUtils.NewBifrostOperationError("failed to write purpose field", err)
	}
	if request.ExpiresAfter != nil {
		if err := writer.WriteField("expires_after[anchor]", request.ExpiresAfter.Anchor); err != nil {
			return nil, providerUtils.NewBifrostOperationError("failed to write expires_after[anchor] field", err)
		}
		if err := writer.WriteField("expires_after[seconds]", fmt.Sprintf("%d", request.ExpiresAfter.Seconds)); err != nil {
			return nil, providerUtils.NewBifrostOperationError("failed to write expires_after[seconds] field", err)
		}
	}

	filename := request.Filename
	if filename == "" {
		filename = "file.jsonl"
	}
	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		return nil, providerUtils.NewBifrostOperationError("failed to create form file", err)
	}
	if _, err := part.Write(request.File); err != nil {
		return nil, providerUtils.NewBifrostOperationError("failed to write file content", err)
	}
	if err := writer.Close(); err != nil {
		return nil, providerUtils.NewBifrostOperationError("failed to close multipart writer", err)
	}

	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)
	req.SetRequestURI(provider.networkConfig.BaseURL + providerUtils.GetPathFromContext(ctx, volcenginePathFiles))
	req.Header.SetMethod(http.MethodPost)
	req.Header.SetContentType(writer.FormDataContentType())
	if key.Value.GetValue() != "" {
		req.Header.Set("Authorization", "Bearer "+key.Value.GetValue())
	}
	req.SetBody(body.Bytes())

	latency, bifrostErr, wait := providerUtils.MakeRequestWithContext(ctx, provider.client, req, resp)
	wait()
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	if resp.StatusCode() != fasthttp.StatusOK {
		return nil, openai.ParseOpenAIError(resp)
	}

	responseBody, err := providerUtils.CheckAndDecodeBody(resp)
	if err != nil {
		return nil, providerUtils.NewBifrostOperationError(schemas.ErrProviderResponseDecode, err)
	}

	var parsed openai.OpenAIFileResponse
	rawRequest, rawResponse, bifrostErr := providerUtils.HandleProviderResponse(
		responseBody,
		&parsed,
		nil,
		providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest),
		providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse),
	)
	if bifrostErr != nil {
		return nil, bifrostErr
	}

	return parsed.ToBifrostFileUploadResponse(
		latency,
		providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest),
		providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse),
		rawRequest,
		rawResponse,
	), nil
}

func (provider *VolcengineProvider) FileList(ctx *schemas.BifrostContext, keys []schemas.Key, request *schemas.BifrostFileListRequest) (*schemas.BifrostFileListResponse, *schemas.BifrostError) {
	if len(keys) == 0 {
		return nil, providerUtils.NewBifrostOperationError("no keys provided", nil)
	}

	helper, err := providerUtils.NewSerialListHelper(keys, request.After, provider.logger, true)
	if err != nil {
		return nil, providerUtils.NewBifrostOperationError("invalid pagination cursor", err)
	}

	key, nativeCursor, ok := helper.GetCurrentKey()
	if !ok {
		return &schemas.BifrostFileListResponse{
			Object:  "list",
			Data:    []schemas.FileObject{},
			HasMore: false,
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType: schemas.FileListRequest,
				Provider:    provider.GetProviderKey(),
			},
		}, nil
	}

	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	requestURL := provider.networkConfig.BaseURL + providerUtils.GetPathFromContext(ctx, volcenginePathFiles)
	values := url.Values{}
	if request.Purpose != "" {
		values.Set("purpose", string(request.Purpose))
	}
	if request.Limit > 0 {
		values.Set("limit", fmt.Sprintf("%d", request.Limit))
	}
	if nativeCursor != "" {
		values.Set("after", nativeCursor)
	}
	if request.Order != nil && *request.Order != "" {
		values.Set("order", *request.Order)
	}
	if encoded := values.Encode(); encoded != "" {
		requestURL += "?" + encoded
	}

	providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)
	req.SetRequestURI(requestURL)
	req.Header.SetMethod(http.MethodGet)
	req.Header.SetContentType("application/json")
	if key.Value.GetValue() != "" {
		req.Header.Set("Authorization", "Bearer "+key.Value.GetValue())
	}

	latency, bifrostErr, wait := providerUtils.MakeRequestWithContext(ctx, provider.client, req, resp)
	wait()
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	if resp.StatusCode() != fasthttp.StatusOK {
		return nil, openai.ParseOpenAIError(resp)
	}

	responseBody, err := providerUtils.CheckAndDecodeBody(resp)
	if err != nil {
		return nil, providerUtils.NewBifrostOperationError(schemas.ErrProviderResponseDecode, err)
	}

	var parsed openai.OpenAIFileListResponse
	_, _, bifrostErr = providerUtils.HandleProviderResponse(
		responseBody,
		&parsed,
		nil,
		providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest),
		providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse),
	)
	if bifrostErr != nil {
		return nil, bifrostErr
	}

	files := make([]schemas.FileObject, 0, len(parsed.Data))
	var lastFileID string
	for _, file := range parsed.Data {
		files = append(files, schemas.FileObject{
			ID:            file.ID,
			Object:        file.Object,
			Bytes:         file.Bytes,
			CreatedAt:     file.CreatedAt,
			Filename:      file.Filename,
			Purpose:       schemas.FilePurpose(file.Purpose),
			Status:        openai.ToBifrostFileStatus(file.Status),
			StatusDetails: file.StatusDetails,
		})
		lastFileID = file.ID
	}

	nextCursor, hasMore := helper.BuildNextCursor(parsed.HasMore, lastFileID)
	result := &schemas.BifrostFileListResponse{
		Object:  "list",
		Data:    files,
		HasMore: hasMore,
		ExtraFields: schemas.BifrostResponseExtraFields{
			RequestType: schemas.FileListRequest,
			Provider:    provider.GetProviderKey(),
			Latency:     latency.Milliseconds(),
		},
	}
	if nextCursor != "" {
		result.After = &nextCursor
	}

	return result, nil
}

func (provider *VolcengineProvider) FileRetrieve(ctx *schemas.BifrostContext, keys []schemas.Key, request *schemas.BifrostFileRetrieveRequest) (*schemas.BifrostFileRetrieveResponse, *schemas.BifrostError) {
	if request.FileID == "" {
		return nil, providerUtils.NewBifrostOperationError("file_id is required", nil)
	}
	if len(keys) == 0 {
		return nil, providerUtils.NewBifrostOperationError("no keys provided", nil)
	}

	sendBackRawRequest := providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest)
	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)

	var lastErr *schemas.BifrostError
	for _, key := range keys {
		req := fasthttp.AcquireRequest()
		resp := fasthttp.AcquireResponse()

		providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)
		req.SetRequestURI(provider.networkConfig.BaseURL + providerUtils.GetPathFromContext(ctx, fmt.Sprintf("%s/%s", volcenginePathFiles, request.FileID)))
		req.Header.SetMethod(http.MethodGet)
		req.Header.SetContentType("application/json")
		if key.Value.GetValue() != "" {
			req.Header.Set("Authorization", "Bearer "+key.Value.GetValue())
		}

		latency, bifrostErr, wait := providerUtils.MakeRequestWithContext(ctx, provider.client, req, resp)
		wait()
		if bifrostErr != nil {
			fasthttp.ReleaseRequest(req)
			fasthttp.ReleaseResponse(resp)
			lastErr = bifrostErr
			continue
		}
		if resp.StatusCode() != fasthttp.StatusOK {
			lastErr = openai.ParseOpenAIError(resp)
			fasthttp.ReleaseRequest(req)
			fasthttp.ReleaseResponse(resp)
			continue
		}

		responseBody, err := providerUtils.CheckAndDecodeBody(resp)
		if err != nil {
			fasthttp.ReleaseRequest(req)
			fasthttp.ReleaseResponse(resp)
			lastErr = providerUtils.NewBifrostOperationError(schemas.ErrProviderResponseDecode, err)
			continue
		}

		var parsed openai.OpenAIFileResponse
		rawRequest, rawResponse, bifrostErr := providerUtils.HandleProviderResponse(responseBody, &parsed, nil, sendBackRawRequest, sendBackRawResponse)
		if bifrostErr != nil {
			fasthttp.ReleaseRequest(req)
			fasthttp.ReleaseResponse(resp)
			lastErr = bifrostErr
			continue
		}

		fasthttp.ReleaseRequest(req)
		fasthttp.ReleaseResponse(resp)

		return parsed.ToBifrostFileRetrieveResponse(provider.GetProviderKey(), latency, sendBackRawRequest, sendBackRawResponse, rawRequest, rawResponse), nil
	}

	return nil, lastErr
}

func (provider *VolcengineProvider) FileDelete(ctx *schemas.BifrostContext, keys []schemas.Key, request *schemas.BifrostFileDeleteRequest) (*schemas.BifrostFileDeleteResponse, *schemas.BifrostError) {
	if request.FileID == "" {
		return nil, providerUtils.NewBifrostOperationError("file_id is required", nil)
	}
	if len(keys) == 0 {
		return nil, providerUtils.NewBifrostOperationError("no keys provided", nil)
	}

	sendBackRawRequest := providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest)
	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)

	var lastErr *schemas.BifrostError
	for _, key := range keys {
		req := fasthttp.AcquireRequest()
		resp := fasthttp.AcquireResponse()

		providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)
		req.SetRequestURI(provider.networkConfig.BaseURL + providerUtils.GetPathFromContext(ctx, fmt.Sprintf("%s/%s", volcenginePathFiles, request.FileID)))
		req.Header.SetMethod(http.MethodDelete)
		req.Header.SetContentType("application/json")
		if key.Value.GetValue() != "" {
			req.Header.Set("Authorization", "Bearer "+key.Value.GetValue())
		}

		latency, bifrostErr, wait := providerUtils.MakeRequestWithContext(ctx, provider.client, req, resp)
		wait()
		if bifrostErr != nil {
			fasthttp.ReleaseRequest(req)
			fasthttp.ReleaseResponse(resp)
			lastErr = bifrostErr
			continue
		}
		if resp.StatusCode() != fasthttp.StatusOK {
			lastErr = openai.ParseOpenAIError(resp)
			fasthttp.ReleaseRequest(req)
			fasthttp.ReleaseResponse(resp)
			continue
		}

		responseBody, err := providerUtils.CheckAndDecodeBody(resp)
		if err != nil {
			fasthttp.ReleaseRequest(req)
			fasthttp.ReleaseResponse(resp)
			lastErr = providerUtils.NewBifrostOperationError(schemas.ErrProviderResponseDecode, err)
			continue
		}

		var parsed openai.OpenAIFileDeleteResponse
		rawRequest, rawResponse, bifrostErr := providerUtils.HandleProviderResponse(responseBody, &parsed, nil, sendBackRawRequest, sendBackRawResponse)
		if bifrostErr != nil {
			fasthttp.ReleaseRequest(req)
			fasthttp.ReleaseResponse(resp)
			lastErr = bifrostErr
			continue
		}

		fasthttp.ReleaseRequest(req)
		fasthttp.ReleaseResponse(resp)

		result := &schemas.BifrostFileDeleteResponse{
			ID:      parsed.ID,
			Object:  parsed.Object,
			Deleted: parsed.Deleted,
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType: schemas.FileDeleteRequest,
				Provider:    provider.GetProviderKey(),
				Latency:     latency.Milliseconds(),
			},
		}
		if sendBackRawRequest {
			result.ExtraFields.RawRequest = rawRequest
		}
		if sendBackRawResponse {
			result.ExtraFields.RawResponse = rawResponse
		}
		return result, nil
	}

	return nil, lastErr
}

func (provider *VolcengineProvider) FileContent(ctx *schemas.BifrostContext, keys []schemas.Key, request *schemas.BifrostFileContentRequest) (*schemas.BifrostFileContentResponse, *schemas.BifrostError) {
	if request.FileID == "" {
		return nil, providerUtils.NewBifrostOperationError("file_id is required", nil)
	}
	if len(keys) == 0 {
		return nil, providerUtils.NewBifrostOperationError("no keys provided", nil)
	}

	var lastErr *schemas.BifrostError
	for _, key := range keys {
		req := fasthttp.AcquireRequest()
		resp := fasthttp.AcquireResponse()

		providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)
		req.SetRequestURI(provider.networkConfig.BaseURL + providerUtils.GetPathFromContext(ctx, fmt.Sprintf("%s/%s/content", volcenginePathFiles, request.FileID)))
		req.Header.SetMethod(http.MethodGet)
		if key.Value.GetValue() != "" {
			req.Header.Set("Authorization", "Bearer "+key.Value.GetValue())
		}

		latency, bifrostErr, wait := providerUtils.MakeRequestWithContext(ctx, provider.client, req, resp)
		wait()
		if bifrostErr != nil {
			fasthttp.ReleaseRequest(req)
			fasthttp.ReleaseResponse(resp)
			lastErr = bifrostErr
			continue
		}
		if resp.StatusCode() != fasthttp.StatusOK {
			lastErr = openai.ParseOpenAIError(resp)
			fasthttp.ReleaseRequest(req)
			fasthttp.ReleaseResponse(resp)
			continue
		}

		responseBody, err := providerUtils.CheckAndDecodeBody(resp)
		if err != nil {
			fasthttp.ReleaseRequest(req)
			fasthttp.ReleaseResponse(resp)
			lastErr = providerUtils.NewBifrostOperationError(schemas.ErrProviderResponseDecode, err)
			continue
		}

		contentType := string(resp.Header.ContentType())
		if contentType == "" {
			contentType = "application/octet-stream"
		}

		fasthttp.ReleaseRequest(req)
		fasthttp.ReleaseResponse(resp)

		return &schemas.BifrostFileContentResponse{
			FileID:      request.FileID,
			Content:     append([]byte(nil), responseBody...),
			ContentType: contentType,
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType: schemas.FileContentRequest,
				Provider:    provider.GetProviderKey(),
				Latency:     latency.Milliseconds(),
			},
		}, nil
	}

	return nil, lastErr
}

// BatchCreate is not supported by Volcengine provider.
func (provider *VolcengineProvider) BatchCreate(_ *schemas.BifrostContext, _ schemas.Key, _ *schemas.BifrostBatchCreateRequest) (*schemas.BifrostBatchCreateResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.BatchCreateRequest, provider.GetProviderKey())
}

// BatchList is not supported by Volcengine provider.
func (provider *VolcengineProvider) BatchList(_ *schemas.BifrostContext, _ []schemas.Key, _ *schemas.BifrostBatchListRequest) (*schemas.BifrostBatchListResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.BatchListRequest, provider.GetProviderKey())
}

// BatchRetrieve is not supported by Volcengine provider.
func (provider *VolcengineProvider) BatchRetrieve(_ *schemas.BifrostContext, _ []schemas.Key, _ *schemas.BifrostBatchRetrieveRequest) (*schemas.BifrostBatchRetrieveResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.BatchRetrieveRequest, provider.GetProviderKey())
}

// BatchCancel is not supported by Volcengine provider.
func (provider *VolcengineProvider) BatchCancel(_ *schemas.BifrostContext, _ []schemas.Key, _ *schemas.BifrostBatchCancelRequest) (*schemas.BifrostBatchCancelResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.BatchCancelRequest, provider.GetProviderKey())
}

// BatchResults is not supported by Volcengine provider.
func (provider *VolcengineProvider) BatchResults(_ *schemas.BifrostContext, _ []schemas.Key, _ *schemas.BifrostBatchResultsRequest) (*schemas.BifrostBatchResultsResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.BatchResultsRequest, provider.GetProviderKey())
}

// BatchDelete is not supported by Volcengine provider.
func (provider *VolcengineProvider) BatchDelete(_ *schemas.BifrostContext, _ []schemas.Key, _ *schemas.BifrostBatchDeleteRequest) (*schemas.BifrostBatchDeleteResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.BatchDeleteRequest, provider.GetProviderKey())
}

// CountTokens is not supported by the Volcengine provider.
func (provider *VolcengineProvider) CountTokens(_ *schemas.BifrostContext, _ schemas.Key, _ *schemas.BifrostResponsesRequest) (*schemas.BifrostCountTokensResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.CountTokensRequest, provider.GetProviderKey())
}

// Compaction is not supported by the Volcengine provider.
func (provider *VolcengineProvider) Compaction(_ *schemas.BifrostContext, _ schemas.Key, _ *schemas.BifrostCompactionRequest) (*schemas.BifrostCompactionResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.CompactionRequest, provider.GetProviderKey())
}

// ContainerCreate is not supported by the Volcengine provider.
func (provider *VolcengineProvider) ContainerCreate(_ *schemas.BifrostContext, _ schemas.Key, _ *schemas.BifrostContainerCreateRequest) (*schemas.BifrostContainerCreateResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerCreateRequest, provider.GetProviderKey())
}

// ContainerList is not supported by the Volcengine provider.
func (provider *VolcengineProvider) ContainerList(_ *schemas.BifrostContext, _ []schemas.Key, _ *schemas.BifrostContainerListRequest) (*schemas.BifrostContainerListResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerListRequest, provider.GetProviderKey())
}

// ContainerRetrieve is not supported by the Volcengine provider.
func (provider *VolcengineProvider) ContainerRetrieve(_ *schemas.BifrostContext, _ []schemas.Key, _ *schemas.BifrostContainerRetrieveRequest) (*schemas.BifrostContainerRetrieveResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerRetrieveRequest, provider.GetProviderKey())
}

// ContainerDelete is not supported by the Volcengine provider.
func (provider *VolcengineProvider) ContainerDelete(_ *schemas.BifrostContext, _ []schemas.Key, _ *schemas.BifrostContainerDeleteRequest) (*schemas.BifrostContainerDeleteResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerDeleteRequest, provider.GetProviderKey())
}

// ContainerFileCreate is not supported by the Volcengine provider.
func (provider *VolcengineProvider) ContainerFileCreate(_ *schemas.BifrostContext, _ schemas.Key, _ *schemas.BifrostContainerFileCreateRequest) (*schemas.BifrostContainerFileCreateResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerFileCreateRequest, provider.GetProviderKey())
}

// ContainerFileList is not supported by the Volcengine provider.
func (provider *VolcengineProvider) ContainerFileList(_ *schemas.BifrostContext, _ []schemas.Key, _ *schemas.BifrostContainerFileListRequest) (*schemas.BifrostContainerFileListResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerFileListRequest, provider.GetProviderKey())
}

// ContainerFileRetrieve is not supported by the Volcengine provider.
func (provider *VolcengineProvider) ContainerFileRetrieve(_ *schemas.BifrostContext, _ []schemas.Key, _ *schemas.BifrostContainerFileRetrieveRequest) (*schemas.BifrostContainerFileRetrieveResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerFileRetrieveRequest, provider.GetProviderKey())
}

// ContainerFileContent is not supported by the Volcengine provider.
func (provider *VolcengineProvider) ContainerFileContent(_ *schemas.BifrostContext, _ []schemas.Key, _ *schemas.BifrostContainerFileContentRequest) (*schemas.BifrostContainerFileContentResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerFileContentRequest, provider.GetProviderKey())
}

// ContainerFileDelete is not supported by the Volcengine provider.
func (provider *VolcengineProvider) ContainerFileDelete(_ *schemas.BifrostContext, _ []schemas.Key, _ *schemas.BifrostContainerFileDeleteRequest) (*schemas.BifrostContainerFileDeleteResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerFileDeleteRequest, provider.GetProviderKey())
}

// Passthrough is not supported by the Volcengine provider.
func (provider *VolcengineProvider) Passthrough(_ *schemas.BifrostContext, _ schemas.Key, _ *schemas.BifrostPassthroughRequest) (*schemas.BifrostPassthroughResponse, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.PassthroughRequest, provider.GetProviderKey())
}

// PassthroughStream is not supported by the Volcengine provider.
func (provider *VolcengineProvider) PassthroughStream(_ *schemas.BifrostContext, _ schemas.PostHookRunner, _ func(context.Context), _ schemas.Key, _ *schemas.BifrostPassthroughRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.PassthroughStreamRequest, provider.GetProviderKey())
}
