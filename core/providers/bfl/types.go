package bfl

import "encoding/json"

// BFLError is the body BFL returns with an error status. BFL's API is FastAPI, so detail
// is either a string ({"detail": "Insufficient credits"}) or a list of validation
// errors ({"detail": [{"loc": ["body", "width"], "msg": "...", "type": "..."}]}).
type BFLError struct {
	Detail json.RawMessage `json:"detail,omitempty"`
}

// BFLValidationError is one entry of a 422 detail list.
type BFLValidationError struct {
	Loc  []any  `json:"loc,omitempty"`
	Msg  string `json:"msg,omitempty"`
	Type string `json:"type,omitempty"`
}

// BFLAsyncResponse is the answer of a task submit: AsyncResponse ({id, polling_url}), or, when
// the request carried webhook_url, AsyncWebhookResponse ({id, status, webhook_url}). Both carry
// the credit cost and the megapixels the task is billed for.
type BFLAsyncResponse struct {
	ID         string   `json:"id"`
	PollingURL string   `json:"polling_url,omitempty"`
	Status     string   `json:"status,omitempty"`
	WebhookURL string   `json:"webhook_url,omitempty"`
	Cost       *float64 `json:"cost,omitempty"`      // credits
	InputMP    *float64 `json:"input_mp,omitempty"`  // input megapixels
	OutputMP   *float64 `json:"output_mp,omitempty"` // output megapixels
}

// BFLResultResponse is the answer of GET /v1/get_result: ResultResponse, or
// SettledCostResultResponse, which adds the settled cost in credits. BFL's OpenAPI leaves result
// unconstrained; Bifrost reads sample and end_time from it.
type BFLResultResponse struct {
	ID       string          `json:"id"`
	Status   string          `json:"status"`
	Result   json.RawMessage `json:"result,omitempty"`
	Progress *float64        `json:"progress,omitempty"`
	Details  map[string]any  `json:"details,omitempty"`
	Cost     *float64        `json:"cost,omitempty"` // settled credits
}
