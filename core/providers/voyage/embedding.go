package voyage

import (
	"fmt"
	"math"
	"slices"

	"github.com/capsohq/bifrost/core/schemas"
)

// ToVoyageEmbeddingRequest preserves the batch as one request. Per-item options and
// non-text inputs cannot be represented by this endpoint and must not be split or dropped.
func ToVoyageEmbeddingRequest(request *schemas.BifrostEmbeddingRequest) (*VoyageEmbeddingRequest, *schemas.BifrostError) {
	if request == nil || len(request.Input) == 0 {
		return nil, schemas.NewUnsupportedParameterError("input", "voyage requires at least one text input")
	}
	if err := schemas.CheckUnifiedFields(&schemas.BifrostRequest{RequestType: schemas.EmbeddingRequest, EmbeddingRequest: request}, voyageEmbeddingFields); err != nil {
		return nil, err
	}
	input := schemas.EmbeddingInput(request.Input)
	if err := input.RejectPerItemParams(); err != nil {
		return nil, schemas.NewUnsupportedParameterError("input", err.Error())
	}
	if !input.AllSingleText() {
		return nil, schemas.NewUnsupportedParameterError("input", "voyage text embeddings require one text part per input item")
	}
	out := &VoyageEmbeddingRequest{Model: request.Model, Input: input.Texts()}
	if p := request.Params; p != nil {
		// Extra parameters are merged after governance has authorized the model
		// and after conversion. They must not replace either decision's fields.
		for field := range p.ExtraParams {
			collision := false
			switch field {
			case "model", "input":
				collision = true
			case "output_dimension":
				collision = p.Dimensions != nil
			case "output_dtype", "encoding_format":
				collision = p.EncodingFormat != nil
			case "input_type":
				collision = p.TaskType != nil
			case "truncation":
				collision = p.AutoTruncate != nil
			}
			if collision {
				return nil, schemas.NewUnsupportedParameterError("extra_params."+field, "extra parameters cannot replace authorized or unified request fields")
			}
		}
		if p.Dimensions != nil {
			switch *p.Dimensions {
			case 256, 512, 1024, 2048:
			default:
				return nil, schemas.NewUnsupportedParameterError("dimensions", "voyage supports 256, 512, 1024 and 2048 dimensions")
			}
		}
		out.OutputDimension = p.Dimensions
		out.InputType = p.TaskType
		out.Truncation = p.AutoTruncate
		out.ExtraParams = p.ExtraParams
		if p.TaskType != nil && *p.TaskType != "query" && *p.TaskType != "document" {
			return nil, schemas.NewUnsupportedParameterError("task_type", "voyage supports query and document task types")
		}
		if p.EncodingFormat != nil {
			switch *p.EncodingFormat {
			case "float", "int8", "uint8", "binary", "ubinary":
				out.OutputDType = p.EncodingFormat
			case "base64":
				out.EncodingFormat = p.EncodingFormat
			default:
				return nil, schemas.NewUnsupportedParameterError("encoding_format", fmt.Sprintf("unsupported voyage encoding format %q", *p.EncodingFormat))
			}
		}
	}
	return out, nil
}

func (r *VoyageEmbeddingResponse) ToBifrostEmbeddingResponse(model string) *schemas.BifrostEmbeddingResponse {
	if r.Model != "" {
		model = r.Model
	}
	out := &schemas.BifrostEmbeddingResponse{Object: r.Object, Model: model, Data: r.Data}
	if r.Usage != nil {
		out.Usage = &schemas.BifrostLLMUsage{PromptTokens: r.Usage.TotalTokens, TotalTokens: r.Usage.TotalTokens}
	}
	return out
}

// ToBifrostEmbeddingResponseWithEncoding restores the representation selected in
// the upstream request. Voyage does not repeat it on individual response rows.
// JSON integer values in these byte-sized formats are exact in float64; validate
// their range and integrality before converting, never silently truncate them.
func (r *VoyageEmbeddingResponse) ToBifrostEmbeddingResponseWithEncoding(model, encoding string) (*schemas.BifrostEmbeddingResponse, error) {
	out := r.ToBifrostEmbeddingResponse(model)
	if encoding == "" || encoding == schemas.EmbeddingEncodingFloat {
		return out, nil
	}
	switch encoding {
	case "int8":
		encoding = schemas.EmbeddingEncodingInt8
	case "binary":
		encoding = schemas.EmbeddingEncodingBinary
	case "uint8":
		encoding = schemas.EmbeddingEncodingUint8
	case "ubinary":
		encoding = schemas.EmbeddingEncodingUbinary
	case "base64":
		encoding = schemas.EmbeddingEncodingBase64
	default:
		return nil, fmt.Errorf("unsupported voyage response encoding %q", encoding)
	}
	out.Data = slices.Clone(r.Data)
	for i, item := range r.Data {
		out.Data[i].EncodingFormat = encoding
		if encoding == schemas.EmbeddingEncodingBase64 {
			if item.Embedding.EmbeddingStr == nil {
				return nil, fmt.Errorf("voyage embedding %d is not base64 text", i)
			}
			continue
		}
		values := item.Embedding.EmbeddingArray
		if encoding == schemas.EmbeddingEncodingInt8 || encoding == schemas.EmbeddingEncodingBinary {
			if item.Embedding.EmbeddingInt8Array != nil {
				continue
			}
			if values == nil {
				return nil, fmt.Errorf("voyage embedding %d is not a numeric vector", i)
			}
			vector := make([]int8, len(values))
			for j, value := range values {
				if value < -128 || value > 127 || math.Trunc(value) != value {
					return nil, fmt.Errorf("voyage embedding %d contains an invalid signed byte", i)
				}
				vector[j] = int8(value)
			}
			out.Data[i].Embedding = schemas.EmbeddingStruct{EmbeddingInt8Array: vector}
		} else {
			if item.Embedding.EmbeddingInt32Array != nil {
				continue
			}
			if values == nil {
				return nil, fmt.Errorf("voyage embedding %d is not a numeric vector", i)
			}
			vector := make([]int32, len(values))
			for j, value := range values {
				if value < 0 || value > 255 || math.Trunc(value) != value {
					return nil, fmt.Errorf("voyage embedding %d contains an invalid unsigned byte", i)
				}
				vector[j] = int32(value)
			}
			out.Data[i].Embedding = schemas.EmbeddingStruct{EmbeddingInt32Array: vector}
		}
	}
	return out, nil
}
