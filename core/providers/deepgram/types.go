package deepgram

// DeepgramTranscriptionRequest is what Bifrost sends to POST /v1/listen: the options as
// query parameters and the audio as the body, either the bytes themselves or a JSON
// object naming a URL Deepgram fetches.
type DeepgramTranscriptionRequest struct {
	Query       string // Encoded query string, keys sorted; never empty (model is always set)
	Body        []byte // Audio bytes, or the JSON body {"url": "..."}
	ContentType string // The audio's media type, or application/json for a URL
	IsURL       bool   // Whether Body is the JSON URL body
}

// DeepgramURLBody is the JSON body of a request whose audio Deepgram fetches itself.
type DeepgramURLBody struct {
	URL string `json:"url"`
}

// DeepgramTranscriptionResponse is the 200 body of POST /v1/listen. Only the fields Bifrost
// converts are decoded; the whole body reaches the caller as raw_response.
type DeepgramTranscriptionResponse struct {
	Metadata *DeepgramMetadata `json:"metadata,omitempty"`
	Results  *DeepgramResults  `json:"results,omitempty"`
}

// DeepgramMetadata carries the request id and the quantities Deepgram bills on.
type DeepgramMetadata struct {
	RequestID string   `json:"request_id,omitempty"`
	Duration  *float64 `json:"duration,omitempty"` // Seconds of audio, per channel
	Channels  *int     `json:"channels,omitempty"` // Channels transcribed; above 1 only with multichannel=true
}

type DeepgramResults struct {
	Channels []DeepgramChannel `json:"channels"`
}

type DeepgramChannel struct {
	Alternatives     []DeepgramAlternative `json:"alternatives"`
	DetectedLanguage *string               `json:"detected_language,omitempty"`
}

type DeepgramAlternative struct {
	Transcript string `json:"transcript"`
}

// DeepgramError is Deepgram's error body. Its documentation shows three shapes:
// {err_code, err_msg, request_id}, {category, message, details, request_id} for a body it
// cannot parse, and {error_code, err_msg, request_id} on some 5xx answers.
type DeepgramError struct {
	ErrCode   string `json:"err_code,omitempty"`
	ErrorCode string `json:"error_code,omitempty"`
	ErrMsg    string `json:"err_msg,omitempty"`
	Category  string `json:"category,omitempty"`
	Message   string `json:"message,omitempty"`
	Details   string `json:"details,omitempty"`
	RequestID string `json:"request_id,omitempty"`
}
