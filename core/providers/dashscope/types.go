package dashscope

// DashScopeError is the body DashScope's native API returns with an error status:
// {"request_id": "...", "code": "InvalidApiKey", "message": "Invalid API-key provided."}.
type DashScopeError struct {
	RequestID string `json:"request_id,omitempty"`
	Code      string `json:"code,omitempty"`
	Message   string `json:"message,omitempty"`
}

// DashScopeVideoRequest is the body of POST /api/v1/services/aigc/video-generation/video-synthesis.
// Every Wan family shares the envelope; which input and parameters fields a family takes is
// decided in ToDashScopeVideoGenerationRequest.
type DashScopeVideoRequest struct {
	Model       string                    `json:"model"`
	Input       DashScopeVideoInput       `json:"input"`
	Parameters  *DashScopeVideoParameters `json:"parameters,omitempty"`
	ExtraParams map[string]interface{}    `json:"-"`
}

// GetExtraParams implements providerUtils.RequestBodyWithExtraParams.
func (r *DashScopeVideoRequest) GetExtraParams() map[string]interface{} {
	if r == nil {
		return nil
	}
	return r.ExtraParams
}

// DashScopeVideoInput is the request's input object. img_url is the first frame of the
// Wan 2.6-and-earlier image-to-video models; media is the input list of Wan 2.7 and Wan 3.0.
type DashScopeVideoInput struct {
	Prompt         string                `json:"prompt,omitempty"`
	NegativePrompt *string               `json:"negative_prompt,omitempty"`
	ImgURL         *string               `json:"img_url,omitempty"`
	Media          []DashScopeVideoMedia `json:"media,omitempty"`
}

// DashScopeVideoMedia is one entry of input.media: a type (first_frame, last_frame,
// reference_image, reference_video, reference_audio) and a URL or data URL.
type DashScopeVideoMedia struct {
	Type string `json:"type"`
	URL  string `json:"url"`
}

// DashScopeVideoParameters is the request's parameters object.
type DashScopeVideoParameters struct {
	Size         string `json:"size,omitempty"`       // "1280*720"; Wan 2.6-and-earlier text-to-video
	Resolution   string `json:"resolution,omitempty"` // "720P"; Wan 2.7, Wan 3.0, and image-to-video of the earlier models
	Ratio        string `json:"ratio,omitempty"`      // "16:9"; Wan 2.7 text-to-video and Wan 3.0
	Duration     *int   `json:"duration,omitempty"`
	Audio        *bool  `json:"audio,omitempty"`
	Seed         *int   `json:"seed,omitempty"`
	Watermark    *bool  `json:"watermark,omitempty"`
	PromptExtend *bool  `json:"prompt_extend,omitempty"`
}

// DashScopeTaskResponse is the body of a task create (output carries task_id and task_status
// only) and of GET /api/v1/tasks/{id}.
type DashScopeTaskResponse struct {
	RequestID string               `json:"request_id,omitempty"`
	Output    *DashScopeTaskOutput `json:"output,omitempty"`
	Usage     *DashScopeVideoUsage `json:"usage,omitempty"`
	// Code and Message are set when DashScope answers a 2xx with an error object and no output.
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// DashScopeTaskOutput is a task's output object. The three times are "2006-01-02 15:04:05.000"
// in UTC+8. code and message are set on a FAILED task.
type DashScopeTaskOutput struct {
	TaskID        string `json:"task_id,omitempty"`
	TaskStatus    string `json:"task_status,omitempty"`
	SubmitTime    string `json:"submit_time,omitempty"`
	ScheduledTime string `json:"scheduled_time,omitempty"`
	EndTime       string `json:"end_time,omitempty"`
	OrigPrompt    string `json:"orig_prompt,omitempty"`
	VideoURL      string `json:"video_url,omitempty"`
	Code          string `json:"code,omitempty"`
	Message       string `json:"message,omitempty"`
}

// DashScopeVideoUsage is the usage object of a finished task. Wan 2.6 and later report
// output_video_duration and input_video_duration. The older families report the clip's length
// as video_duration (Wan 2.1 and earlier) or duration alone (Wan 2.2 and 2.5), and take no
// input video. The rest (SR, ratio, video_count, fps) stays readable in the raw response.
type DashScopeVideoUsage struct {
	InputVideoDuration  *float64 `json:"input_video_duration,omitempty"`
	OutputVideoDuration *float64 `json:"output_video_duration,omitempty"`
	VideoDuration       *float64 `json:"video_duration,omitempty"`
	Duration            *float64 `json:"duration,omitempty"`
}

// outputSeconds is the length of the generated clip: output_video_duration where DashScope
// reports it, else the older families' video_duration or duration. Where output_video_duration
// is present, duration is the billed total with the input video and is not read.
func (u *DashScopeVideoUsage) outputSeconds() *float64 {
	switch {
	case u == nil:
		return nil
	case u.OutputVideoDuration != nil:
		return u.OutputVideoDuration
	case u.VideoDuration != nil:
		return u.VideoDuration
	case u.InputVideoDuration == nil:
		return u.Duration
	}
	return nil
}

// DashScopeTaskListResponse is the body of GET /api/v1/tasks: one page of the account's
// asynchronous tasks, with the page it is and how many pages there are.
type DashScopeTaskListResponse struct {
	RequestID string                  `json:"request_id,omitempty"`
	Data      []DashScopeTaskListItem `json:"data"`
	Total     int                     `json:"total"`
	TotalPage int                     `json:"total_page"`
	PageNo    int                     `json:"page_no"`
	PageSize  int                     `json:"page_size"`
	// Code and Message are set when DashScope answers a 2xx with an error object.
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// DashScopeTaskListItem is one listed task. The times are Unix milliseconds, unlike the
// strings of a task query. The rest of the entry (api_key_id, caller ids, region) stays
// readable in the raw response.
type DashScopeTaskListItem struct {
	TaskID    string `json:"task_id,omitempty"`
	Status    string `json:"status,omitempty"`
	ModelName string `json:"model_name,omitempty"`
	GmtCreate int64  `json:"gmt_create,omitempty"`
	StartTime int64  `json:"start_time,omitempty"`
	EndTime   int64  `json:"end_time,omitempty"`
}

// DashScopeTTSRequest is the body of a non-streamed Qwen3-TTS call to
// /api/v1/services/aigc/multimodal-generation/generation.
type DashScopeTTSRequest struct {
	Model string            `json:"model"`
	Input DashScopeTTSInput `json:"input"`

	ExtraParams map[string]interface{} `json:"-"`
}

// GetExtraParams returns the caller's extra fields, merged into the body only under
// x-bf-passthrough-extra-params (contract C4, agreement two).
func (r *DashScopeTTSRequest) GetExtraParams() map[string]interface{} {
	return r.ExtraParams
}

// DashScopeTTSInput carries the text, the voice and the language. language_type is omitted
// when the caller gave no language_code, which leaves the vendor default (Auto).
type DashScopeTTSInput struct {
	Text         string `json:"text"`
	Voice        string `json:"voice"`
	LanguageType string `json:"language_type,omitempty"`
}

// DashScopeTTSResponse is a non-streamed Qwen3-TTS answer. output.audio.data is empty for a
// non-streamed call: the audio is the signed link in output.audio.url, valid until
// output.audio.expires_at (Unix seconds).
type DashScopeTTSResponse struct {
	RequestID string             `json:"request_id,omitempty"`
	Code      string             `json:"code,omitempty"`
	Message   string             `json:"message,omitempty"`
	Output    DashScopeTTSOutput `json:"output"`
	Usage     *DashScopeTTSUsage `json:"usage,omitempty"`
}

// DashScopeTTSOutput is the output object of a Qwen3-TTS answer.
type DashScopeTTSOutput struct {
	FinishReason string             `json:"finish_reason,omitempty"`
	Audio        *DashScopeTTSAudio `json:"audio,omitempty"`
}

// DashScopeTTSAudio is the synthesized audio: a link with its expiry, or base64 data.
type DashScopeTTSAudio struct {
	ID        string `json:"id,omitempty"`
	URL       string `json:"url,omitempty"`
	Data      string `json:"data,omitempty"`
	ExpiresAt *int64 `json:"expires_at,omitempty"`
}

// DashScopeTTSUsage is the usage Qwen3-TTS reports. characters is the vendor's billed
// character count (Qwen3-TTS-Flash only); the token counts are 0 for that model and are
// kept as reported.
type DashScopeTTSUsage struct {
	Characters   *int `json:"characters,omitempty"`
	InputTokens  *int `json:"input_tokens,omitempty"`
	OutputTokens *int `json:"output_tokens,omitempty"`
	TotalTokens  *int `json:"total_tokens,omitempty"`
}

// DashScopeASRRequest is the body of a synchronous Qwen3-ASR call to
// /api/v1/services/aigc/multimodal-generation/generation.
type DashScopeASRRequest struct {
	Model      string                  `json:"model"`
	Input      DashScopeASRInput       `json:"input"`
	Parameters *DashScopeASRParameters `json:"parameters,omitempty"`

	ExtraParams map[string]interface{} `json:"-"`
}

// GetExtraParams returns the caller's extra fields, merged into the body only under
// x-bf-passthrough-extra-params (contract C4, agreement two).
func (r *DashScopeASRRequest) GetExtraParams() map[string]interface{} {
	return r.ExtraParams
}

// DashScopeASRInput holds the messages: an optional system message carrying the context
// text, then the user message carrying the audio.
type DashScopeASRInput struct {
	Messages []DashScopeASRMessage `json:"messages"`
}

// DashScopeASRMessage is one message of a Qwen3-ASR request or answer.
type DashScopeASRMessage struct {
	Role        string                   `json:"role"`
	Content     []DashScopeASRContent    `json:"content"`
	Annotations []DashScopeASRAnnotation `json:"annotations,omitempty"`
}

// DashScopeASRContent is one content item: {"audio": <URL or data URL>} or {"text": ...}.
type DashScopeASRContent struct {
	Audio string  `json:"audio,omitempty"`
	Text  *string `json:"text,omitempty"`
}

// DashScopeASRAnnotation is the audio_info annotation of a Qwen3-ASR answer.
type DashScopeASRAnnotation struct {
	Type     string `json:"type,omitempty"`
	Language string `json:"language,omitempty"`
	Emotion  string `json:"emotion,omitempty"`
}

// DashScopeASRParameters carries asr_options.
type DashScopeASRParameters struct {
	ASROptions *DashScopeASROptions `json:"asr_options,omitempty"`
}

// DashScopeASROptions are the recognition options Bifrost converts unified fields into.
type DashScopeASROptions struct {
	Language string `json:"language,omitempty"`
}

// DashScopeASRResponse is a synchronous Qwen3-ASR answer.
type DashScopeASRResponse struct {
	RequestID string             `json:"request_id,omitempty"`
	Code      string             `json:"code,omitempty"`
	Message   string             `json:"message,omitempty"`
	Output    DashScopeASROutput `json:"output"`
	Usage     *DashScopeASRUsage `json:"usage,omitempty"`
}

// DashScopeASROutput is the output object of a Qwen3-ASR answer.
type DashScopeASROutput struct {
	Choices []DashScopeASRChoice `json:"choices,omitempty"`
}

// DashScopeASRChoice is one choice of a Qwen3-ASR answer.
type DashScopeASRChoice struct {
	FinishReason string              `json:"finish_reason,omitempty"`
	Message      DashScopeASRMessage `json:"message"`
}

// DashScopeASRUsage is the usage Qwen3-ASR reports: the billed audio seconds and the token
// details. input_tokens and output_tokens are read when the vendor sends them.
type DashScopeASRUsage struct {
	Seconds             *float64                   `json:"seconds,omitempty"`
	InputTokens         *int                       `json:"input_tokens,omitempty"`
	OutputTokens        *int                       `json:"output_tokens,omitempty"`
	TotalTokens         *int                       `json:"total_tokens,omitempty"`
	InputTokensDetails  *DashScopeASRTokensDetails `json:"input_tokens_details,omitempty"`
	OutputTokensDetails *DashScopeASRTokensDetails `json:"output_tokens_details,omitempty"`
}

// DashScopeASRTokensDetails splits a token count by modality.
type DashScopeASRTokensDetails struct {
	TextTokens  *int `json:"text_tokens,omitempty"`
	AudioTokens *int `json:"audio_tokens,omitempty"`
}
