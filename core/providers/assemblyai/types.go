package assemblyai

// AssemblyAIConfig is the JSON `config` part of a Sync API request, holding the members
// Bifrost fills from the unified transcription fields. Every other member of the vendor's
// config (keyterms_prompt, conversation_context, sample_rate, channels, and any feature the
// vendor adds later) travels in ExtraParams and is merged into the part unchanged.
type AssemblyAIConfig struct {
	Prompt        *string  `json:"prompt,omitempty"`
	LanguageCodes []string `json:"language_codes,omitempty"`
	Timestamps    *bool    `json:"timestamps,omitempty"`

	ExtraParams map[string]interface{} `json:"-"`
}

// AssemblyAITranscriptionRequest is the resolved form of POST /v1/transcribe: the model sent
// in the X-AAI-Model header, the `audio` part and the `config` part.
type AssemblyAITranscriptionRequest struct {
	Model            string
	Audio            []byte
	Filename         string
	AudioContentType string
	Config           AssemblyAIConfig
}

// AssemblyAITranscriptionResponse is the Sync API's 200 body (SyncTranscriptResponse).
type AssemblyAITranscriptionResponse struct {
	Text            string           `json:"text"`
	Words           []AssemblyAIWord `json:"words"`
	Confidence      *float64         `json:"confidence,omitempty"`
	AudioDurationMS *float64         `json:"audio_duration_ms,omitempty"`
	SessionID       string           `json:"session_id,omitempty"`
	RequestTimeMS   *float64         `json:"request_time_ms,omitempty"`
}

// AssemblyAIWord is one entry of the response's words. Start and End are milliseconds and
// are present only when the request set timestamps and the word could be aligned.
type AssemblyAIWord struct {
	Text       string   `json:"text"`
	Confidence *float64 `json:"confidence,omitempty"`
	Start      *float64 `json:"start,omitempty"`
	End        *float64 `json:"end,omitempty"`
}

// AssemblyAIError is the Sync API's error body: RFC 9457 problem details
// (application/problem+json).
type AssemblyAIError struct {
	Status int    `json:"status"`
	Title  string `json:"title"`
	Detail string `json:"detail"`
}
