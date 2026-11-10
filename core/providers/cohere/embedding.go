package cohere

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"

	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/google/uuid"
)

// cohereImageURL returns the image as a data URI, since Cohere rejects bare base64.
func cohereImageURL(media *schemas.EmbeddingMediaPart) (string, bool) {
	if media.URL != nil {
		return *media.URL, true
	}
	if media.Data == nil {
		return "", false
	}
	if media.MIMEType != nil && !strings.HasPrefix(*media.Data, "data:") {
		return "data:" + *media.MIMEType + ";base64," + *media.Data, true
	}
	return *media.Data, true
}

func cohereContentBlockFromEmbeddingPart(part schemas.EmbeddingContentPart) (*CohereContentBlock, error) {
	if err := part.Validate(); err != nil {
		return nil, err
	}
	switch part.Type {
	case schemas.EmbeddingContentPartTypeText:
		text := *part.Text
		return &CohereContentBlock{Type: CohereContentBlockTypeText, Text: &text}, nil
	case schemas.EmbeddingContentPartTypeImage:
		if url, ok := cohereImageURL(part.Image); ok {
			return &CohereContentBlock{
				Type:     CohereContentBlockTypeImage,
				ImageURL: &CohereImageURL{URL: url},
			}, nil
		}
		return nil, providerUtils.InvalidRequestErrorf("cohere image part missing data")
	default:
		return nil, providerUtils.InvalidRequestErrorf("this provider supports only text and image embedding parts")
	}
}

func embeddingContentFromCohereBlocks(blocks []CohereContentBlock) (schemas.EmbeddingContent, error) {
	result := make(schemas.EmbeddingContent, 0, len(blocks))
	for _, block := range blocks {
		switch block.Type {
		case CohereContentBlockTypeText:
			if block.Text == nil {
				return nil, providerUtils.InvalidRequestErrorf("cohere text block missing text")
			}
			text := *block.Text
			result = append(result, schemas.EmbeddingContentPart{
				Type: schemas.EmbeddingContentPartTypeText,
				Text: &text,
			})
		case CohereContentBlockTypeImage:
			if block.ImageURL == nil {
				return nil, providerUtils.InvalidRequestErrorf("cohere image block missing image_url")
			}
			url := block.ImageURL.URL
			result = append(result, schemas.EmbeddingContentPart{
				Type:  schemas.EmbeddingContentPartTypeImage,
				Image: &schemas.EmbeddingMediaPart{URL: &url},
			})
		default:
			return nil, providerUtils.InvalidRequestErrorf("unsupported cohere embedding block type %q", block.Type)
		}
	}
	return result, nil
}

// allSingleImages returns every item's image when each item is exactly one image part.
func allSingleImages(contents []schemas.EmbeddingContent) ([]string, bool) {
	images := make([]string, 0, len(contents))
	for _, content := range contents {
		url, ok := isSingleImageContent(content)
		if !ok {
			return nil, false
		}
		images = append(images, url)
	}
	return images, true
}

func isSingleImageContent(content schemas.EmbeddingContent) (string, bool) {
	if len(content) != 1 || content[0].Type != schemas.EmbeddingContentPartTypeImage || content[0].Image == nil {
		return "", false
	}
	return cohereImageURL(content[0].Image)
}

// cohereInputTypes maps a unified task_type onto Cohere's input_type. Cohere's own values
// pass through; the task types of the Gemini dialect (which callers on the /genai and
// LangChain routes send) map onto their Cohere equivalent. Matching ignores case.
var cohereInputTypes = map[string]string{
	"search_document":    "search_document",
	"search_query":       "search_query",
	"classification":     "classification",
	"clustering":         "clustering",
	"image":              "image",
	"retrieval_document": "search_document",
	"retrieval_query":    "search_query",
}

// cohereEmbeddingInputType resolves Cohere's input_type, which every v3+ embed model
// requires (contract C4). The unified field task_type is converted through
// cohereInputTypes; a caller that already speaks Cohere may send input_type itself as an
// extra field (the native /cohere/v2/embed route arrives that way), and it is sent as
// given. Bifrost adds no value the caller did not send: with neither, or with a task_type
// Cohere has no equivalent for, the request is refused as caller_unsupported_parameter on
// task_type, before any upstream call, instead of being embedded under an input type the
// caller never asked for (the input type changes the vectors returned).
func cohereEmbeddingInputType(params *schemas.EmbeddingParameters) (string, error) {
	var taskType, extraInputType string
	if params != nil {
		if params.TaskType != nil {
			taskType = *params.TaskType
		}
		if v, ok := schemas.SafeExtractString(params.ExtraParams["input_type"]); ok {
			extraInputType = v
		}
	}
	if taskType == "" {
		if extraInputType != "" {
			return extraInputType, nil
		}
		return "", providerUtils.ConverterBifrostError(schemas.NewUnsupportedParameterError("task_type",
			"task_type is required for Cohere embeddings: it is sent as Cohere's input_type (search_document, search_query, classification, clustering or image), and Bifrost does not choose one for the caller"))
	}
	inputType, ok := cohereInputTypes[strings.ToLower(taskType)]
	if !ok {
		return "", providerUtils.ConverterBifrostError(schemas.NewUnsupportedParameterError("task_type",
			fmt.Sprintf("task_type %q has no Cohere input_type equivalent; use search_document, search_query, classification, clustering or image", taskType)))
	}
	if extraInputType != "" && extraInputType != inputType {
		return "", providerUtils.InvalidRequestErrorf("task_type %q (Cohere input_type %q) and input_type %q disagree; send one of them", taskType, inputType, extraInputType)
	}
	return inputType, nil
}

// cohereInternalEmbeddingInputType is the input_type Bifrost's own embedding requests
// (semantic cache, routing, Warp index) get when they name no task: search_document, the
// value Bifrost sent for every request before callers had to choose, so vectors stored
// before this change stay comparable with new ones.
const cohereInternalEmbeddingInputType = "search_document"

// withInternalEmbeddingInputType gives an internal embedding request that names no task
// Cohere's documented default. A caller's request is returned unchanged and is refused by
// ToCohereEmbeddingRequest when it names none (contract C4); see
// bifrost.MarkInternalEmbeddingRequest.
func withInternalEmbeddingInputType(ctx *schemas.BifrostContext, req *schemas.BifrostEmbeddingRequest) *schemas.BifrostEmbeddingRequest {
	if ctx == nil || req == nil {
		return req
	}
	if internal, _ := ctx.Value(schemas.BifrostContextKeyInternalEmbeddingRequest).(bool); !internal {
		return req
	}
	if req.Params != nil && (req.Params.TaskType != nil || req.Params.ExtraParams["input_type"] != nil) {
		return req
	}
	params := &schemas.EmbeddingParameters{}
	if req.Params != nil {
		params = req.Params.Clone()
	}
	params.TaskType = schemas.Ptr(cohereInternalEmbeddingInputType)
	withDefault := *req
	withDefault.Params = params
	return &withDefault
}

// ToCohereEmbeddingRequest converts a Bifrost embedding request to Cohere format.
func ToCohereEmbeddingRequest(bifrostReq *schemas.BifrostEmbeddingRequest) (*CohereEmbeddingRequest, error) {
	if bifrostReq == nil || len(bifrostReq.Input) == 0 {
		return nil, providerUtils.InvalidRequestErrorf("embedding input is not provided")
	}

	inputType, inputTypeErr := cohereEmbeddingInputType(bifrostReq.Params)
	if inputTypeErr != nil {
		return nil, inputTypeErr
	}
	cohereReq := &CohereEmbeddingRequest{
		Model:     bifrostReq.Model,
		InputType: inputType,
	}
	if bifrostReq.Params != nil {
		cohereReq.OutputDimension = bifrostReq.Params.Dimensions

		if bifrostReq.Params.ExtraParams != nil {
			// A copy: the native fields are removed from it below, and a retry or
			// fallback converts the same request again.
			cohereReq.ExtraParams = maps.Clone(bifrostReq.Params.ExtraParams)

			if embeddingTypes, ok := schemas.SafeExtractStringSlice(bifrostReq.Params.ExtraParams["embedding_types"]); ok {
				delete(cohereReq.ExtraParams, "embedding_types")
				cohereReq.EmbeddingTypes = embeddingTypes
			}
			// Already resolved into InputType above; never sent twice.
			delete(cohereReq.ExtraParams, "input_type")
			if priority, ok := schemas.SafeExtractIntPointer(bifrostReq.Params.ExtraParams["priority"]); ok {
				delete(cohereReq.ExtraParams, "priority")
				cohereReq.Priority = priority
			}
			if maxTokens, ok := schemas.SafeExtractIntPointer(bifrostReq.Params.ExtraParams["max_tokens"]); ok {
				delete(cohereReq.ExtraParams, "max_tokens")
				cohereReq.MaxTokens = maxTokens
			}
			if truncate, ok := schemas.SafeExtractStringPointer(bifrostReq.Params.ExtraParams["truncate"]); ok {
				delete(cohereReq.ExtraParams, "truncate")
				cohereReq.Truncate = truncate
			}
		}
	}

	if len(cohereReq.EmbeddingTypes) == 0 && bifrostReq.Params != nil && bifrostReq.Params.EncodingFormat != nil {
		cohereReq.EmbeddingTypes = []string{*bifrostReq.Params.EncodingFormat}
	}

	if err := schemas.EmbeddingInput(bifrostReq.Input).RejectPerItemParams(); err != nil {
		return nil, providerUtils.InvalidRequestErrorf("%s", err)
	}
	contents := schemas.EmbeddingInput(bifrostReq.Input).Contents()

	// Pick the shape first: all single-text → texts[], all lone images → images[], else inputs[].
	if schemas.EmbeddingInput(bifrostReq.Input).AllSingleText() {
		cohereReq.Texts = make([]string, len(contents))
		for i, content := range contents {
			cohereReq.Texts[i] = *content[0].Text
		}
	} else if images, ok := allSingleImages(contents); ok {
		cohereReq.Images = images
	} else {
		inputs := make([]CohereEmbeddingInput, 0, len(contents))
		for _, content := range contents {
			blocks := make([]CohereContentBlock, 0, len(content))
			for _, part := range content {
				block, err := cohereContentBlockFromEmbeddingPart(part)
				if err != nil {
					return nil, err
				}
				blocks = append(blocks, *block)
			}
			inputs = append(inputs, CohereEmbeddingInput{Content: blocks})
		}
		cohereReq.Inputs = inputs
	}

	return cohereReq, nil
}

// ToBifrostEmbeddingRequest converts a Cohere embedding request to Bifrost format.
// Each Cohere input entry maps to one element in Contents (one output embedding).
func (req *CohereEmbeddingRequest) ToBifrostEmbeddingRequest(ctx *schemas.BifrostContext) (*schemas.BifrostEmbeddingRequest, error) {
	if req == nil {
		return nil, nil
	}

	provider, model := schemas.ParseModelString(req.Model, "")

	bifrostReq := &schemas.BifrostEmbeddingRequest{
		Provider: provider,
		Model:    model,

		Params: &schemas.EmbeddingParameters{},
	}

	switch {
	case len(req.Texts) > 0:
		items := make([]schemas.EmbeddingInputItem, len(req.Texts))
		for i, text := range req.Texts {
			t := text
			items[i] = schemas.EmbeddingInputItem{Content: schemas.EmbeddingContent{{
				Type: schemas.EmbeddingContentPartTypeText,
				Text: &t,
			}}}
		}
		bifrostReq.Input = items
	case len(req.Images) > 0:
		items := make([]schemas.EmbeddingInputItem, len(req.Images))
		for i, imgURL := range req.Images {
			u := imgURL
			items[i] = schemas.EmbeddingInputItem{Content: schemas.EmbeddingContent{{
				Type:  schemas.EmbeddingContentPartTypeImage,
				Image: &schemas.EmbeddingMediaPart{URL: &u},
			}}}
		}
		bifrostReq.Input = items
	case len(req.Inputs) > 0:
		items := make([]schemas.EmbeddingInputItem, 0, len(req.Inputs))
		for _, input := range req.Inputs {
			content, err := embeddingContentFromCohereBlocks(input.Content)
			if err != nil {
				return nil, fmt.Errorf("cohere embedding input conversion failed: %w", err)
			}
			items = append(items, schemas.EmbeddingInputItem{Content: content})
		}
		bifrostReq.Input = items
	}

	bifrostReq.Params.Dimensions = req.OutputDimension

	extraParams := req.ExtraParams
	if extraParams == nil {
		extraParams = make(map[string]interface{})
	}
	if req.InputType != "" {
		extraParams["input_type"] = req.InputType
	}
	if len(req.EmbeddingTypes) > 0 {
		extraParams["embedding_types"] = req.EmbeddingTypes
	}
	if req.Priority != nil {
		extraParams["priority"] = *req.Priority
	}
	if req.MaxTokens != nil {
		extraParams["max_tokens"] = *req.MaxTokens
	}
	if req.Truncate != nil {
		extraParams["truncate"] = req.Truncate
	}
	bifrostReq.Params.ExtraParams = extraParams

	return bifrostReq, nil
}

// cohereEmbeddingUsage fills the unified usage from what Cohere bills (contract C5).
// meta.billed_units is the settlement source: prompt_tokens is the billed input_tokens, the
// image counters land in prompt_tokens_details.image_tokens and input_images. meta.tokens
// (processed counts, which can differ from what is billed) is read only when Cohere sent no
// billed_units at all. Nil when Cohere reported neither.
func cohereEmbeddingUsage(meta *CohereEmbeddingMeta) *schemas.BifrostLLMUsage {
	var usage *schemas.BifrostLLMUsage
	if billed := meta.BilledUnits; billed != nil {
		usage = &schemas.BifrostLLMUsage{}
		if billed.InputTokens != nil {
			usage.PromptTokens = *billed.InputTokens
		}
		if billed.OutputTokens != nil {
			usage.CompletionTokens = *billed.OutputTokens
		}
		// Images bill on their own counters, outside input_tokens. Kept out of
		// PromptTokens so text pricing never counts them.
		if billed.ImageTokens != nil && *billed.ImageTokens > 0 {
			usage.PromptTokensDetails = &schemas.ChatPromptTokensDetails{ImageTokens: *billed.ImageTokens}
		}
		if billed.Images != nil {
			images := *billed.Images
			usage.InputImages = &images
		}
	} else if tokens := meta.Tokens; tokens != nil {
		usage = &schemas.BifrostLLMUsage{}
		if tokens.InputTokens != nil {
			usage.PromptTokens = *tokens.InputTokens
		}
		if tokens.OutputTokens != nil {
			usage.CompletionTokens = *tokens.OutputTokens
		}
	}
	if usage == nil {
		return nil
	}
	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	if usage.PromptTokensDetails != nil {
		usage.TotalTokens += usage.PromptTokensDetails.ImageTokens
	}
	return usage
}

// ToBifrostEmbeddingResponse converts a Cohere embedding response to Bifrost format
func (response *CohereEmbeddingResponse) ToBifrostEmbeddingResponse() *schemas.BifrostEmbeddingResponse {
	if response == nil {
		return nil
	}

	bifrostResponse := &schemas.BifrostEmbeddingResponse{
		Object: "list",
	}

	if emb := response.Embeddings; emb != nil {
		// One entry per representation, labelled with the encoding it arrived as.
		for i, e := range emb.Float {
			bifrostResponse.Data = append(bifrostResponse.Data, schemas.EmbeddingData{
				Object:         "embedding",
				Index:          i,
				Embedding:      schemas.EmbeddingStruct{EmbeddingArray: e},
				EncodingFormat: schemas.EmbeddingEncodingFloat,
			})
		}
		for i, e := range emb.Base64 {
			v := e
			bifrostResponse.Data = append(bifrostResponse.Data, schemas.EmbeddingData{
				Object:         "embedding",
				Index:          i,
				Embedding:      schemas.EmbeddingStruct{EmbeddingStr: &v},
				EncodingFormat: schemas.EmbeddingEncodingBase64,
			})
		}
		for i, e := range emb.Int8 {
			bifrostResponse.Data = append(bifrostResponse.Data, schemas.EmbeddingData{
				Object:         "embedding",
				Index:          i,
				Embedding:      schemas.EmbeddingStruct{EmbeddingInt8Array: e},
				EncodingFormat: schemas.EmbeddingEncodingInt8,
			})
		}
		for i, e := range emb.Binary {
			bifrostResponse.Data = append(bifrostResponse.Data, schemas.EmbeddingData{
				Object:         "embedding",
				Index:          i,
				Embedding:      schemas.EmbeddingStruct{EmbeddingInt8Array: e},
				EncodingFormat: schemas.EmbeddingEncodingBinary,
			})
		}
		for i, e := range emb.Uint8 {
			bifrostResponse.Data = append(bifrostResponse.Data, schemas.EmbeddingData{
				Object:         "embedding",
				Index:          i,
				Embedding:      schemas.EmbeddingStruct{EmbeddingInt32Array: e},
				EncodingFormat: schemas.EmbeddingEncodingUint8,
			})
		}
		for i, e := range emb.Ubinary {
			bifrostResponse.Data = append(bifrostResponse.Data, schemas.EmbeddingData{
				Object:         "embedding",
				Index:          i,
				Embedding:      schemas.EmbeddingStruct{EmbeddingInt32Array: e},
				EncodingFormat: schemas.EmbeddingEncodingUbinary,
			})
		}
	}

	if response.Meta != nil {
		bifrostResponse.Usage = cohereEmbeddingUsage(response.Meta)
	}

	return bifrostResponse
}

// ToCohereEmbeddingResponse converts a BifrostEmbeddingResponse to Cohere's native embedding response format.
func ToCohereEmbeddingResponse(bifrostResp *schemas.BifrostEmbeddingResponse) *CohereEmbeddingResponse {
	if bifrostResp == nil || len(bifrostResp.Data) == 0 {
		return nil
	}

	cohereResp := &CohereEmbeddingResponse{
		ID:         uuid.New().String(),
		Embeddings: &CohereEmbeddingData{},
	}

	// Cohere arrays carry no index, so position must follow the input index.
	data := slices.Clone(bifrostResp.Data)
	slices.SortStableFunc(data, func(a, b schemas.EmbeddingData) int { return cmp.Compare(a.Index, b.Index) })

	// Unlabelled entries are plain float vectors.
	for _, item := range data {
		emb := item.Embedding
		switch item.EncodingFormat {
		case schemas.EmbeddingEncodingFloat:
			cohereResp.Embeddings.Float = append(cohereResp.Embeddings.Float, emb.EmbeddingArray)
		case schemas.EmbeddingEncodingBase64:
			if emb.EmbeddingStr != nil {
				cohereResp.Embeddings.Base64 = append(cohereResp.Embeddings.Base64, *emb.EmbeddingStr)
			}
		case schemas.EmbeddingEncodingInt8:
			cohereResp.Embeddings.Int8 = append(cohereResp.Embeddings.Int8, emb.EmbeddingInt8Array)
		case schemas.EmbeddingEncodingBinary:
			cohereResp.Embeddings.Binary = append(cohereResp.Embeddings.Binary, emb.EmbeddingInt8Array)
		case schemas.EmbeddingEncodingUint8:
			cohereResp.Embeddings.Uint8 = append(cohereResp.Embeddings.Uint8, emb.EmbeddingInt32Array)
		case schemas.EmbeddingEncodingUbinary:
			cohereResp.Embeddings.Ubinary = append(cohereResp.Embeddings.Ubinary, emb.EmbeddingInt32Array)
		default:
			if emb.EmbeddingArray != nil {
				cohereResp.Embeddings.Float = append(cohereResp.Embeddings.Float, emb.EmbeddingArray)
			}
		}
	}

	// The object form above is what Cohere's v2 API labels embeddings_by_type.
	cohereResp.ResponseType = schemas.Ptr("embeddings_by_type")

	// /v2/embed reports usage as billed_units only.
	if bifrostResp.Usage != nil {
		inputTokens := bifrostResp.Usage.PromptTokens
		imageTokens := 0
		if details := bifrostResp.Usage.PromptTokensDetails; details != nil {
			imageTokens = details.ImageTokens
		}
		cohereResp.Meta = &CohereEmbeddingMeta{
			BilledUnits: &CohereBilledUnits{
				InputTokens: &inputTokens,
				ImageTokens: &imageTokens,
				Images:      copyIntPointer(bifrostResp.Usage.InputImages),
			},
		}
	}

	return cohereResp
}

// copyIntPointer returns a copy of v, nil when v is nil.
func copyIntPointer(v *int) *int {
	if v == nil {
		return nil
	}
	c := *v
	return &c
}
