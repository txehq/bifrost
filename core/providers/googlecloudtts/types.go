package googlecloudtts

// Request and response bodies of text:synthesize, from
// https://docs.cloud.google.com/text-to-speech/docs/reference/rest/v1/text/synthesize.
// Only the fields the unified speech request maps to are typed; every other vendor field
// (pitch, volumeGainDb, sampleRateHertz, effectsProfileId, ssmlGender, customVoice, ...)
// reaches the vendor through the extra-params channel, merged into these objects.

// GoogleCloudTTSSynthesizeRequest is the SynthesizeSpeechRequest body.
type GoogleCloudTTSSynthesizeRequest struct {
	Input       GoogleCloudTTSSynthesisInput `json:"input"`
	Voice       GoogleCloudTTSVoiceSelection `json:"voice"`
	AudioConfig GoogleCloudTTSAudioConfig    `json:"audioConfig"`

	ExtraParams map[string]interface{} `json:"-"`
}

// GetExtraParams satisfies providerUtils.RequestBodyWithExtraParams.
func (r *GoogleCloudTTSSynthesizeRequest) GetExtraParams() map[string]interface{} {
	return r.ExtraParams
}

// GoogleCloudTTSSynthesisInput is SynthesisInput: exactly one of text and ssml.
type GoogleCloudTTSSynthesisInput struct {
	Text string `json:"text,omitempty"`
	SSML string `json:"ssml,omitempty"`
}

// GoogleCloudTTSVoiceSelection is VoiceSelectionParams.
type GoogleCloudTTSVoiceSelection struct {
	LanguageCode string `json:"languageCode,omitempty"`
	Name         string `json:"name,omitempty"`
}

// GoogleCloudTTSAudioConfig is AudioConfig.
type GoogleCloudTTSAudioConfig struct {
	AudioEncoding string   `json:"audioEncoding"`
	SpeakingRate  *float64 `json:"speakingRate,omitempty"`
}

// GoogleCloudTTSSynthesizeResponse is the SynthesizeSpeechResponse body. audioContent is
// the audio in base64, container header included.
type GoogleCloudTTSSynthesizeResponse struct {
	AudioContent string `json:"audioContent"`
}

// GoogleCloudTTSError is the error envelope Google APIs return with a non-2xx status
// (google.rpc.Status in JSON).
type GoogleCloudTTSError struct {
	Error *GoogleCloudTTSErrorBody `json:"error,omitempty"`
}

// GoogleCloudTTSErrorBody is the error object: the HTTP code, the message, and the
// google.rpc.Code name (INVALID_ARGUMENT, PERMISSION_DENIED, RESOURCE_EXHAUSTED, ...).
type GoogleCloudTTSErrorBody struct {
	Code    int                         `json:"code"`
	Message string                      `json:"message"`
	Status  string                      `json:"status"`
	Details []GoogleCloudTTSErrorDetail `json:"details,omitempty"`
}

// GoogleCloudTTSErrorDetail is one error detail. Only google.rpc.RetryInfo is read.
type GoogleCloudTTSErrorDetail struct {
	Type       string `json:"@type"`
	RetryDelay string `json:"retryDelay,omitempty"` // a protobuf duration such as "39s"
}
