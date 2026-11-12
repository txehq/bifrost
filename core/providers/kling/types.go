package kling

// Wire types of the Kling API on the new design standard (kling.ai/document-api, read
// 2026-11-11): Bearer auth, one create path per model and input kind, and one task object shared
// by the create response, the two queries and the callback.

// KlingVideoRequest is the body of a create call. Text-to-video carries the prompt at the top
// level; image-to-video and omni-video carry it as a contents item beside the frame images.
type KlingVideoRequest struct {
	Prompt   string                 `json:"prompt,omitempty"`
	Contents []KlingContent         `json:"contents,omitempty"`
	Settings map[string]interface{} `json:"settings,omitempty"`
	Options  *KlingOptions          `json:"options,omitempty"`
}

// GetExtraParams implements providerUtils.RequestBodyWithExtraParams. It is always empty: the
// converter has already placed every extra field (in settings, or as options.watermark_info), so
// nothing is merged into the top level of the body.
func (r *KlingVideoRequest) GetExtraParams() map[string]interface{} {
	return nil
}

// Kling contents item types this provider writes.
const (
	klingContentPrompt     = "prompt"
	klingContentFirstFrame = "first_frame"
	klingContentLastFrame  = "last_frame"
)

// KlingContent is one item of contents[]: the prompt, or a frame image given as a URL or as
// Base64.
type KlingContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	URL  string `json:"url,omitempty"`
}

// KlingOptions are the general options of a create call.
type KlingOptions struct {
	CallbackURL    *string             `json:"callback_url,omitempty"`
	ExternalTaskID *string             `json:"external_task_id,omitempty"`
	WatermarkInfo  *KlingWatermarkInfo `json:"watermark_info,omitempty"`
}

// KlingWatermarkInfo asks Kling to also produce a watermarked result.
type KlingWatermarkInfo struct {
	Enabled bool `json:"enabled"`
}

// KlingEnvelope is the part of every Kling response outside data: the service code (0 on
// success), its message and the request id.
type KlingEnvelope struct {
	Code      int    `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

// KlingCreateResponse is the response of a create call.
type KlingCreateResponse struct {
	KlingEnvelope
	Data *KlingTask `json:"data"`
}

// KlingQueryResponse is the response of GET /tasks.
type KlingQueryResponse struct {
	KlingEnvelope
	Data []KlingTask `json:"data"`
}

// KlingListRequest is the body of POST /tasks, the query by cursor. Times are Unix milliseconds
// as strings, as Kling documents them.
type KlingListRequest struct {
	StartTime string            `json:"start_time,omitempty"`
	EndTime   string            `json:"end_time,omitempty"`
	Cursor    string            `json:"cursor,omitempty"`
	Limit     *int              `json:"limit,omitempty"`
	Filters   []KlingListFilter `json:"filters,omitempty"`
}

// KlingListFilter is one filter of POST /tasks: key is "status" or "product_type".
type KlingListFilter struct {
	Key    string   `json:"key"`
	Values []string `json:"values"`
}

// KlingListResponse is the response of POST /tasks.
type KlingListResponse struct {
	KlingEnvelope
	Data *KlingListData `json:"data"`
}

// KlingListData is one page of tasks.
type KlingListData struct {
	Result     []KlingTask `json:"result"`
	Count      int         `json:"count"`
	NextCursor string      `json:"next_cursor"`
	HasMore    bool        `json:"has_more"`
}

// KlingTask is Kling's task object. Times are Unix milliseconds.
type KlingTask struct {
	ID         string         `json:"id"`
	Status     string         `json:"status"`
	Message    string         `json:"message"`
	CreateTime int64          `json:"create_time"`
	UpdateTime int64          `json:"update_time"`
	ExternalID string         `json:"external_id"`
	Outputs    []KlingOutput  `json:"outputs"`
	Billing    []KlingBilling `json:"billing"`
}

// klingOutputVideo is the outputs[].type of a generated video.
const klingOutputVideo = "video"

// KlingOutput is one generated result. Only the fields of a video result are decoded.
type KlingOutput struct {
	Type         string `json:"type"`
	ID           string `json:"id"`
	URL          string `json:"url"`
	WatermarkURL string `json:"watermark_url"`
	Duration     string `json:"duration"` // seconds, as a decimal string
}

// klingChargeTypeUnit is the billing[].charge_type of a deduction from a resource package.
const klingChargeTypeUnit = "unit"

// KlingBilling is one deduction a task caused. A task that drew on several accounts has several.
type KlingBilling struct {
	ChargeType  string `json:"charge_type"`  // "cash" (balance) or "unit" (resource package)
	CashType    string `json:"cash_type"`    // cash only: balance, test_balance, reward_balance
	Amount      string `json:"amount"`       // decimal: units for "unit", money for "cash"
	Currency    string `json:"currency"`     // cash only: CNY or USD
	PackageType string `json:"package_type"` // unit only: video, image, audio
	ListPrice   string `json:"list_price"`   // cash only
}
