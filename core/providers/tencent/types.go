package tencent

// Wire types of Tencent Cloud TokenHub's Hy image generation API (大模型服务平台 TokenHub,
// "Hy 生图调用指南", https://cloud.tencent.com/document/product/1823/135745, updated
// 2026-10-12). Both models are synchronous: the server finishes generation before it
// answers, so one request is one vendor call and there is no task to poll.

// TencentImageV3Request is the body of POST /v1/wand/hunyuan-image/v3-generation (model
// hy-image-v3, HunyuanImage 3.0).
type TencentImageV3Request struct {
	Model       string                 `json:"model"`
	Prompt      string                 `json:"prompt"`
	Images      []string               `json:"images,omitempty"` // 0-3 reference images, URL or base64
	Size        *string                `json:"size,omitempty"`   // "WxH"
	Seed        *int                   `json:"seed,omitempty"`
	ExtraParams map[string]interface{} `json:"-"` // footnote, revise: sent with x-bf-passthrough-extra-params
}

// GetExtraParams implements providerUtils.RequestBodyWithExtraParams.
func (r *TencentImageV3Request) GetExtraParams() map[string]interface{} {
	return r.ExtraParams
}

// TencentImageV35Request is the body of POST /v1/wand/hunyuan-image/v35-generation (model
// hy-image-v3.5-preview). The instruction is the last user message of an OpenAI chat-style
// messages array; reference images are image_url parts of that message.
type TencentImageV35Request struct {
	Model       string                 `json:"model"`
	Messages    []TencentImageMessage  `json:"messages"`
	Size        *string                `json:"size,omitempty"` // "WxH"
	Seed        *int                   `json:"seed,omitempty"`
	ExtraParams map[string]interface{} `json:"-"` // generate_max_pixels, footnote, session, use_search_tool, ...
}

// GetExtraParams implements providerUtils.RequestBodyWithExtraParams.
func (r *TencentImageV35Request) GetExtraParams() map[string]interface{} {
	return r.ExtraParams
}

// TencentImageMessage is one entry of the v3.5 messages array.
type TencentImageMessage struct {
	Role    string                `json:"role"`
	Content []TencentImageContent `json:"content"`
}

// TencentImageContent is one part of a v3.5 message: text, or an image by URL or data URI.
type TencentImageContent struct {
	Type     string               `json:"type"` // "text" or "image_url"
	Text     string               `json:"text,omitempty"`
	ImageURL *TencentImageURLPart `json:"image_url,omitempty"`
}

// TencentImageURLPart carries an http(s) URL or a base64 data URI.
type TencentImageURLPart struct {
	URL string `json:"url"`
}

// TencentImageResponse is the response of both endpoints. v3 fills Data; v3.5 fills Choices
// (always one) and reports a failure, including a moderation block, as finish_reason "error"
// with an Error object.
type TencentImageResponse struct {
	ID            string               `json:"id"`
	Object        string               `json:"object,omitempty"`
	Created       int64                `json:"created"`
	Model         string               `json:"model,omitempty"`
	RequestID     string               `json:"request_id,omitempty"`
	Data          []TencentImageData   `json:"data,omitempty"`
	Choices       []TencentImageChoice `json:"choices,omitempty"`
	Usage         *TencentUsage        `json:"usage,omitempty"`
	TokenhubUsage *TencentUsage        `json:"tokenhub_usage,omitempty"`
	Error         *TencentError        `json:"error,omitempty"`
}

// TencentImageData is one v3 image. The URL is a signed temporary address valid for 12 hours.
type TencentImageData struct {
	URL           string `json:"url"`
	RevisedPrompt string `json:"revised_prompt,omitempty"`
}

// TencentImageChoice is the single v3.5 choice.
type TencentImageChoice struct {
	Index        int               `json:"index"`
	FinishReason *string           `json:"finish_reason"`
	Delta        TencentImageDelta `json:"delta"`
}

// TencentImageDelta holds the generated image of a v3.5 choice.
type TencentImageDelta struct {
	Type  string             `json:"type,omitempty"`
	Image *TencentDeltaImage `json:"image,omitempty"`
}

// TencentDeltaImage is the v3.5 image: a signed temporary URL and its pixel size.
type TencentDeltaImage struct {
	URL    string `json:"url"`
	Width  int    `json:"width,omitempty"`
	Height int    `json:"height,omitempty"`
}

// TencentUsage is TokenHub's usage block. The documentation reports only total_tokens, and says
// v3.5 does not split prompt and completion tokens.
type TencentUsage struct {
	TotalTokens *int `json:"total_tokens,omitempty"`
}

// TencentError is TokenHub's OpenAI-style error object.
type TencentError struct {
	Type      string      `json:"type,omitempty"`
	Code      interface{} `json:"code,omitempty"` // documented as a string; a number is kept too
	Message   string      `json:"message,omitempty"`
	RequestID string      `json:"request_id,omitempty"`
}
