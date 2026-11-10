package dashscope

// DashScopeError is the body DashScope's native API returns with an error status:
// {"request_id": "...", "code": "InvalidApiKey", "message": "Invalid API-key provided."}.
type DashScopeError struct {
	RequestID string `json:"request_id,omitempty"`
	Code      string `json:"code,omitempty"`
	Message   string `json:"message,omitempty"`
}
