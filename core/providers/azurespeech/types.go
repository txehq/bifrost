package azurespeech

// AzureSpeechTranscriptionRequest is one fast transcription call: the audio part and the
// definition JSON of the multipart form.
type AzureSpeechTranscriptionRequest struct {
	File       []byte
	Filename   string
	Definition map[string]interface{}
}

// AzureSpeechTranscriptionResponse is the fast transcription API's response
// (Transcriptions - Transcribe).
type AzureSpeechTranscriptionResponse struct {
	// DurationMilliseconds is the audio duration Azure AI Speech bills. Nil when the
	// response did not carry it.
	DurationMilliseconds *float64                         `json:"durationMilliseconds,omitempty"`
	CombinedPhrases      []AzureSpeechCombinedPhrase      `json:"combinedPhrases,omitempty"`
	Phrases              []AzureSpeechTranscriptionPhrase `json:"phrases,omitempty"`
}

// AzureSpeechCombinedPhrase is the full transcript of one channel.
type AzureSpeechCombinedPhrase struct {
	Channel *int   `json:"channel,omitempty"`
	Text    string `json:"text"`
}

// AzureSpeechTranscriptionPhrase is one recognized phrase with its timing.
type AzureSpeechTranscriptionPhrase struct {
	Channel              *int                           `json:"channel,omitempty"`
	Speaker              *int                           `json:"speaker,omitempty"`
	OffsetMilliseconds   float64                        `json:"offsetMilliseconds"`
	DurationMilliseconds float64                        `json:"durationMilliseconds"`
	Text                 string                         `json:"text"`
	Words                []AzureSpeechTranscriptionWord `json:"words,omitempty"`
	Locale               string                         `json:"locale,omitempty"`
	Confidence           *float64                       `json:"confidence,omitempty"`
}

// AzureSpeechTranscriptionWord is one word of a phrase with its timing.
type AzureSpeechTranscriptionWord struct {
	Text                 string  `json:"text"`
	OffsetMilliseconds   float64 `json:"offsetMilliseconds"`
	DurationMilliseconds float64 `json:"durationMilliseconds"`
}

// AzureSpeechError is the error body of the Speech REST APIs. The fast transcription API
// answers {"code","message","innerError"}; other Azure AI services nest the same object
// under "error". The text to speech API often answers with no body at all.
type AzureSpeechError struct {
	Code       string                 `json:"code,omitempty"`
	Message    string                 `json:"message,omitempty"`
	InnerError *AzureSpeechInnerError `json:"innerError,omitempty"`
	Error      *AzureSpeechErrorBody  `json:"error,omitempty"`
}

// AzureSpeechErrorBody is the nested form of AzureSpeechError.
type AzureSpeechErrorBody struct {
	Code       string                 `json:"code,omitempty"`
	Message    string                 `json:"message,omitempty"`
	InnerError *AzureSpeechInnerError `json:"innerError,omitempty"`
}

// AzureSpeechInnerError is the more specific error inside an AzureSpeechError.
type AzureSpeechInnerError struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}
