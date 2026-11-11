package vertex_test

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/capsohq/bifrost/core/internal/llmtests"
	"github.com/capsohq/bifrost/core/providers/gemini"
	"github.com/capsohq/bifrost/core/providers/vertex"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVertex(t *testing.T) {
	t.Parallel()
	if strings.TrimSpace(os.Getenv("VERTEX_API_KEY")) == "" && (strings.TrimSpace(os.Getenv("VERTEX_PROJECT_ID")) == "" || strings.TrimSpace(os.Getenv("VERTEX_CREDENTIALS")) == "") {
		t.Skip("Skipping Vertex tests because VERTEX_API_KEY is not set and VERTEX_PROJECT_ID or VERTEX_CREDENTIALS is not set")
	}

	client, ctx, cancel, err := llmtests.SetupTest()
	if err != nil {
		t.Fatalf("Error initializing test setup: %v", err)
	}
	defer cancel()
	defer client.Shutdown()

	rerankModel := strings.TrimSpace(os.Getenv("VERTEX_RERANK_MODEL"))

	// Vertex file operations are GCS-backed: the bucket/prefix are passed via the typed
	// StorageConfig (VERTEX_GCS_BUCKET, optional VERTEX_GCS_PREFIX), not extra_params.
	var fileStorageConfig *schemas.FileStorageConfig
	var batchOutputFolder *schemas.BatchOutputFolder
	if gcsBucket := strings.TrimSpace(os.Getenv("VERTEX_GCS_BUCKET")); gcsBucket != "" {
		gcsPrefix := strings.TrimSpace(os.Getenv("VERTEX_GCS_PREFIX"))
		fileStorageConfig = &schemas.FileStorageConfig{
			GCS: &schemas.GCSStorageConfig{
				Bucket: gcsBucket,
				Prefix: gcsPrefix,
			},
		}
		// Batch output is a gs:// prefix; Vertex writes results into its own subdirectory under it.
		outputURI := "gs://" + gcsBucket
		if gcsPrefix != "" {
			outputURI += "/" + strings.Trim(gcsPrefix, "/")
		}
		outputURI += "/batch-output"
		batchOutputFolder = &schemas.BatchOutputFolder{URL: outputURI}
	}

	testConfig := llmtests.ComprehensiveTestConfig{
		Provider:                 schemas.Vertex,
		ChatModel:                "gemini-2.5-pro",
		PromptCachingModel:       "claude-sonnet-4-5",
		WebSearchChatModel:       "claude-sonnet-4-5",
		VisionModel:              "claude-sonnet-4-5",
		TextModel:                "", // Vertex doesn't support text completion in newer models
		EmbeddingModel:           "text-multilingual-embedding-002",
		MultimodalEmbeddingModel: "gemini-embedding-2-preview",
		RerankModel:              rerankModel,
		ReasoningModel:           "claude-4.5-haiku",
		ImageGenerationModel:     "gemini-2.5-flash-image",
		ImageEditModel:           "gemini-2.5-flash-image",
		VideoGenerationModel:     "veo-3.1-generate-preview",
		FileStorageConfig:        fileStorageConfig,
		BatchOutputFolder:        batchOutputFolder,
		Scenarios: llmtests.TestScenarios{
			TextCompletion:               false, // Not supported
			SimpleChat:                   true,
			CompletionStream:             true,
			MultiTurnConversation:        true,
			ToolCalls:                    true,
			ToolCallsStreaming:           true,
			MultipleToolCalls:            true,
			MultipleToolCallsStreaming:   true,
			End2EndToolCalling:           true,
			AutomaticFunctionCall:        true,
			ImageURL:                     false,
			ImageBase64:                  true,
			ImageGeneration:              true,
			ImageGenerationStream:        false,
			ImageEdit:                    true,
			VideoGeneration:              false, // disabled for now because of long running operations
			VideoRetrieve:                false,
			VideoRemix:                   false,
			VideoDownload:                false,
			VideoList:                    false,
			VideoDelete:                  false,
			MultipleImages:               true,
			CompleteEnd2End:              true,
			FileBase64:                   true,
			Embedding:                    true,
			MultimodalEmbedding:          true,
			Rerank:                       rerankModel != "",
			Reasoning:                    true,
			PromptCaching:                true,
			ListModels:                   false,
			CountTokens:                  true,
			StructuredOutputs:            true, // Structured outputs with nullable enum support
			InterleavedThinking:          true,
			EagerInputStreaming:          true, // fine-grained-tool-streaming-2025-05-14 (GA on Vertex)
			ServerToolsViaOpenAIEndpoint: true, // web_search only on Vertex per Table 20 (web_fetch/code_execution skip)
			FileUpload:                   true,
			FileList:                     true,
			FileRetrieve:                 true,
			FileDelete:                   true,
			FileContent:                  true,
			FileBatchInput:               true,
			BatchCreate:                  true,
			BatchList:                    true,
			BatchRetrieve:                true,
			BatchCancel:                  true,
			BatchResults:                 true,
		},
	}

	t.Run("VertexTests", func(t *testing.T) {
		llmtests.RunAllComprehensiveTests(t, client, ctx, testConfig)
	})
}

func TestVertexGeminiChatCompletionPreservesGCSImageURL(t *testing.T) {
	result, err := gemini.ToGeminiChatCompletionRequestWithImageURLSchemes(nil, &schemas.BifrostChatRequest{
		Model: "gemini-3-flash-preview",
		Input: []schemas.ChatMessage{
			{
				Role: schemas.ChatMessageRoleUser,
				Content: &schemas.ChatMessageContent{
					ContentBlocks: []schemas.ChatContentBlock{
						{
							Type: schemas.ChatContentBlockTypeText,
							Text: schemas.Ptr("Describe this image."),
						},
						{
							Type: schemas.ChatContentBlockTypeImage,
							ImageURLStruct: &schemas.ChatInputImage{
								URL: "gs://my-bucket/xxx.png",
							},
						},
					},
				},
			},
		},
	}, "http", "https", "gs")

	require.NoError(t, err)
	require.Len(t, result.Contents, 1)
	require.Len(t, result.Contents[0].Parts, 2)
	require.NotNil(t, result.Contents[0].Parts[1].FileData)
	assert.Equal(t, "gs://my-bucket/xxx.png", result.Contents[0].Parts[1].FileData.FileURI)
	assert.Equal(t, "image/png", result.Contents[0].Parts[1].FileData.MIMEType)
}

func TestVertexGeminiResponsesPreservesGCSImageURL(t *testing.T) {
	result, err := gemini.ToGeminiResponsesRequestWithImageURLSchemes(nil, &schemas.BifrostResponsesRequest{
		Provider: schemas.Vertex,
		Model:    "gemini-3-flash-preview",
		Input: []schemas.ResponsesMessage{
			{
				Role: schemas.Ptr(schemas.ResponsesInputMessageRoleUser),
				Content: &schemas.ResponsesMessageContent{
					ContentBlocks: []schemas.ResponsesMessageContentBlock{
						{
							Type: schemas.ResponsesInputMessageContentBlockTypeText,
							Text: schemas.Ptr("Describe this image."),
						},
						{
							Type: schemas.ResponsesInputMessageContentBlockTypeImage,
							ResponsesInputMessageContentBlockImage: &schemas.ResponsesInputMessageContentBlockImage{
								ImageURL: schemas.Ptr("gs://my-bucket/xxx.png"),
							},
						},
					},
				},
			},
		},
	}, "http", "https", "gs")

	require.NoError(t, err)
	require.Len(t, result.Contents, 1)
	require.Len(t, result.Contents[0].Parts, 2)
	require.NotNil(t, result.Contents[0].Parts[1].FileData)
	assert.Equal(t, "gs://my-bucket/xxx.png", result.Contents[0].Parts[1].FileData.FileURI)
	assert.Equal(t, "image/png", result.Contents[0].Parts[1].FileData.MIMEType)
}

// TestVertexGeminiRejectsUnsupportedScheme guards the upper bound of the Vertex
// scheme allowlist: even though Vertex extends Gemini's defaults with "gs", schemes
// outside that set (file://, ftp://, ...) must still be rejected.
func TestVertexGeminiRejectsUnsupportedScheme(t *testing.T) {
	_, err := gemini.ToGeminiChatCompletionRequestWithImageURLSchemes(nil, &schemas.BifrostChatRequest{
		Model: "gemini-3-flash-preview",
		Input: []schemas.ChatMessage{
			{
				Role: schemas.ChatMessageRoleUser,
				Content: &schemas.ChatMessageContent{
					ContentBlocks: []schemas.ChatContentBlock{
						{
							Type: schemas.ChatContentBlockTypeImage,
							ImageURLStruct: &schemas.ChatInputImage{
								URL: "file:///etc/passwd",
							},
						},
					},
				},
			},
		},
	}, "http", "https", "gs")

	require.Error(t, err)
	assert.Contains(t, err.Error(), `URL scheme "file" is not allowed`)
}

// The rejection has to come before any key or endpoint work: the empty key below would otherwise
// fail with "project ID is not set".
func TestVertexImagen_UnsupportedParamsReturn400(t *testing.T) {
	config := &schemas.ProviderConfig{}
	config.CheckAndSetDefaults()
	provider, err := vertex.NewVertexProvider(config, nil)
	require.NoError(t, err)
	newCtx := func() *schemas.BifrostContext {
		return schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	}
	assertRejected := func(t *testing.T, bifrostErr *schemas.BifrostError, param string) {
		t.Helper()
		require.NotNil(t, bifrostErr)
		require.NotNil(t, bifrostErr.StatusCode)
		assert.Equal(t, http.StatusBadRequest, *bifrostErr.StatusCode)
		require.NotNil(t, bifrostErr.Error)
		assert.Equal(t, param, bifrostErr.Error.Param)
		assert.Equal(t, schemas.ErrorTypeCallerUnsupportedParameter, bifrostErr.ExtraFields.ErrorType)
	}
	generation := func(model string, params *schemas.ImageGenerationParameters) *schemas.BifrostImageGenerationRequest {
		return &schemas.BifrostImageGenerationRequest{Provider: schemas.Vertex, Model: model, Input: &schemas.ImageGenerationInput{Prompt: "a red apple"}, Params: params}
	}
	edit := func(params *schemas.ImageEditParameters) *schemas.BifrostImageEditRequest {
		return &schemas.BifrostImageEditRequest{Provider: schemas.Vertex, Model: "imagen-3.0-capability-001", Input: &schemas.ImageEditInput{Prompt: "make it green", Images: []schemas.ImageInput{{Image: []byte("png")}}}, Params: params}
	}

	_, bifrostErr := provider.ImageGeneration(newCtx(), schemas.Key{}, generation("imagen-4.0-generate-001", &schemas.ImageGenerationParameters{Quality: schemas.Ptr("high")}))
	assertRejected(t, bifrostErr, "quality")
	_, bifrostErr = provider.ImageGeneration(newCtx(), schemas.Key{}, generation("imagen-4.0-generate-001", &schemas.ImageGenerationParameters{Background: schemas.Ptr("transparent")}))
	assertRejected(t, bifrostErr, "background")
	_, bifrostErr = provider.ImageEdit(newCtx(), schemas.Key{}, edit(&schemas.ImageEditParameters{Quality: schemas.Ptr("high")}))
	assertRejected(t, bifrostErr, "quality")
	_, bifrostErr = provider.ImageEdit(newCtx(), schemas.Key{}, edit(&schemas.ImageEditParameters{Background: schemas.Ptr("opaque")}))
	assertRejected(t, bifrostErr, "background")

	// A Gemini image model on the same route is not an Imagen request: it passes this check and
	// stops at the missing project instead.
	_, bifrostErr = provider.ImageGeneration(newCtx(), schemas.Key{}, generation("gemini-2.5-flash-image", &schemas.ImageGenerationParameters{Quality: schemas.Ptr("high")}))
	require.NotNil(t, bifrostErr)
	assert.NotEqual(t, schemas.ErrorTypeCallerUnsupportedParameter, bifrostErr.ExtraFields.ErrorType)
}

// TXE-6537 (contract C4): a Veo create whose size or first frame cannot be sent is refused with
// 400 caller_unsupported_parameter before any key or endpoint work (the empty key below would
// otherwise fail with "project ID is not set"). A Cloud Storage first frame is deliverable.
func TestVertexVideoGeneration_UndeliverableFieldsReturn400(t *testing.T) {
	config := &schemas.ProviderConfig{}
	config.CheckAndSetDefaults()
	provider, err := vertex.NewVertexProvider(config, nil)
	require.NoError(t, err)
	create := func(input schemas.VideoGenerationInput, params schemas.VideoGenerationParameters) *schemas.BifrostError {
		input.Prompt = "a fox"
		_, bifrostErr := provider.VideoGeneration(schemas.NewBifrostContext(context.Background(), schemas.NoDeadline), schemas.Key{},
			&schemas.BifrostVideoGenerationRequest{Provider: schemas.Vertex, Model: "veo-3.1-generate-001", Input: &input, Params: &params})
		require.NotNil(t, bifrostErr)
		return bifrostErr
	}
	assertRejected := func(t *testing.T, bifrostErr *schemas.BifrostError, param string) {
		t.Helper()
		require.NotNil(t, bifrostErr.StatusCode, "error: %+v", bifrostErr.Error)
		assert.Equal(t, http.StatusBadRequest, *bifrostErr.StatusCode)
		require.NotNil(t, bifrostErr.Error)
		assert.Equal(t, param, bifrostErr.Error.Param)
		assert.Equal(t, schemas.ErrorTypeCallerUnsupportedParameter, bifrostErr.ExtraFields.ErrorType)
	}

	assertRejected(t, create(schemas.VideoGenerationInput{}, schemas.VideoGenerationParameters{Size: "1024x1024"}), "size")
	assertRejected(t, create(schemas.VideoGenerationInput{InputReference: schemas.Ptr("https://example.com/first.png")}, schemas.VideoGenerationParameters{}), "input_reference")
	assertRejected(t, create(schemas.VideoGenerationInput{}, schemas.VideoGenerationParameters{LastFrame: "https://example.com/last.png"}), "last_frame")
	assertRejected(t, create(schemas.VideoGenerationInput{}, schemas.VideoGenerationParameters{ReferenceImages: schemas.VideoReferenceImages{"https://example.com/ref.png"}}), "reference_images")
	assertRejected(t, create(schemas.VideoGenerationInput{}, schemas.VideoGenerationParameters{Seconds: schemas.Ptr("7.5")}), "seconds")

	deliverable := create(schemas.VideoGenerationInput{InputReference: schemas.Ptr("gs://frames/first.png")},
		schemas.VideoGenerationParameters{Size: "1920x1080", TaskControlParameters: schemas.TaskControlParameters{OutputStorageURI: schemas.Ptr("gs://out/task_1/")}})
	assert.NotEqual(t, schemas.ErrorTypeCallerUnsupportedParameter, deliverable.ExtraFields.ErrorType, "error: %+v", deliverable.Error)
}

// TXE-6537 (contract C4 BF4, C7): what core's pre-key checks decide for Vertex. Veo takes the
// unified video fields below and output_storage_uri; it has no callback, external task id or
// deadline parameter, no output format, and no cancel API.
func TestVertexVideoFieldAndCancelDeclarations(t *testing.T) {
	config := &schemas.ProviderConfig{}
	config.CheckAndSetDefaults()
	provider, err := vertex.NewVertexProvider(config, nil)
	require.NoError(t, err)
	check := func(params schemas.VideoGenerationParameters) *schemas.BifrostError {
		req := &schemas.BifrostRequest{RequestType: schemas.VideoGenerationRequest, VideoGenerationRequest: &schemas.BifrostVideoGenerationRequest{
			Provider: schemas.Vertex, Model: "veo-3.1-generate-001", Input: &schemas.VideoGenerationInput{Prompt: "a fox"}, Params: &params,
		}}
		if bifrostErr := schemas.CheckDeclaredUnifiedFields(provider, req); bifrostErr != nil {
			return bifrostErr
		}
		return schemas.CheckTaskControlFields(provider, req)
	}

	require.Nil(t, check(schemas.VideoGenerationParameters{
		Seconds: schemas.Ptr("8"), Size: "1280x720", Audio: schemas.Ptr(true), Seed: schemas.Ptr(7), NegativePrompt: schemas.Ptr("rain"),
		AspectRatio: "16:9", Resolution: "720p", LastFrame: "gs://f/last.png", ReferenceImages: schemas.VideoReferenceImages{"gs://f/ref.png"},
		TaskControlParameters: schemas.TaskControlParameters{OutputStorageURI: schemas.Ptr("gs://out/task_1/")},
	}))

	for param, params := range map[string]schemas.VideoGenerationParameters{
		"output_format":         {OutputFormat: schemas.Ptr("webm")},
		"type":                  {Type: schemas.Ptr("upscale")},
		"upscale_factor":        {UpscaleFactor: schemas.Ptr(2)},
		"callback_url":          {TaskControlParameters: schemas.TaskControlParameters{CallbackURL: schemas.Ptr("https://hooks.tokun.sh/v/task_1")}},
		"client_reference_id":   {TaskControlParameters: schemas.TaskControlParameters{ClientReferenceID: schemas.Ptr("task_1")}},
		"expires_after_seconds": {TaskControlParameters: schemas.TaskControlParameters{ExpiresAfterSeconds: schemas.Ptr(3600)}},
	} {
		bifrostErr := check(params)
		require.NotNil(t, bifrostErr, "%s was accepted", param)
		assert.Equal(t, schemas.ErrorTypeCallerUnsupportedParameter, bifrostErr.ExtraFields.ErrorType, param)
		assert.Equal(t, param, bifrostErr.Error.Param)
	}

	cancelErr := schemas.CheckVideoCancelSupported(provider, &schemas.BifrostRequest{RequestType: schemas.VideoCancelRequest})
	require.NotNil(t, cancelErr)
	assert.Equal(t, http.StatusBadRequest, *cancelErr.StatusCode)
	assert.Equal(t, schemas.ErrorTypeProviderCancelUnsupported, cancelErr.ExtraFields.ErrorType)
}
