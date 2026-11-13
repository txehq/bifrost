package voyage

import "github.com/capsohq/bifrost/core/schemas"

type VoyageEmbeddingRequest struct {
	Model           string                 `json:"model"`
	Input           []string               `json:"input"`
	InputType       *string                `json:"input_type,omitempty"`
	OutputDimension *int                   `json:"output_dimension,omitempty"`
	OutputDType     *string                `json:"output_dtype,omitempty"`
	EncodingFormat  *string                `json:"encoding_format,omitempty"`
	Truncation      *bool                  `json:"truncation,omitempty"`
	ExtraParams     map[string]interface{} `json:"-"`
}

func (r *VoyageEmbeddingRequest) GetExtraParams() map[string]interface{} { return r.ExtraParams }

type VoyageEmbeddingResponse struct {
	Object string                  `json:"object"`
	Data   []schemas.EmbeddingData `json:"data"`
	Model  string                  `json:"model"`
	Usage  *VoyageUsage            `json:"usage"`
}

type VoyageUsage struct {
	TotalTokens *int `json:"total_tokens"`
}

type VoyageError struct {
	Detail string `json:"detail"`
}
