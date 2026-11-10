package schemas

import (
	"fmt"
	"unicode/utf8"
)

type BifrostSpeechRequest struct {
	Provider       ModelProvider     `json:"provider"`
	Model          string            `json:"model"`
	Input          *SpeechInput      `json:"input,omitempty"`
	Params         *SpeechParameters `json:"params,omitempty"`
	Fallbacks      []Fallback        `json:"fallbacks,omitempty"`
	RawRequestBody []byte            `json:"-"` // set bifrost-use-raw-request-body to true in ctx to use the raw request body. Bifrost will directly send this to the downstream provider.
}

func (r *BifrostSpeechRequest) GetRawRequestBody() []byte {
	return r.RawRequestBody
}

type BifrostSpeechResponse struct {
	Audio               []byte                     `json:"audio"`
	Usage               *SpeechUsage               `json:"usage"`
	Alignment           *SpeechAlignment           `json:"alignment,omitempty"`            // Character-level timing information
	NormalizedAlignment *SpeechAlignment           `json:"normalized_alignment,omitempty"` // Character-level timing information for normalized text
	AudioBase64         *string                    `json:"audio_base64,omitempty"`         // Base64-encoded audio (when timestamps are requested)
	ExtraFields         BifrostResponseExtraFields `json:"extra_fields"`

	// ContentType is the media type of Audio as the provider knows it: the vendor's own
	// Content-Type for a raw-audio response, or the format a provider converted to (Gemini's
	// raw PCM). Empty when unknown; the HTTP transport then derives the type from the bytes and
	// the requested format. On a raw-audio response it is the HTTP Content-Type; it is
	// serialized so a stored copy (the semantic cache) still knows the format of headerless
	// audio such as raw PCM (TXE-6530, contract C5).
	ContentType string `json:"content_type,omitempty"`
}

// BackfillParams fills Usage.InputChars with the code-point count of the input only when the
// provider left it empty. A character count the vendor reported (MiniMax usage_characters) is
// the vendor's figure and is never overwritten by Bifrost's own count (TXE-6530, contract C5).
func (r *BifrostSpeechResponse) BackfillParams(request *BifrostSpeechRequest) {
	if r == nil || request == nil || request.Input == nil {
		return
	}
	r.Usage = backfillSpeechInputChars(r.Usage, request.Input.Input)
}

// SpeechAlignment represents character-level timing information for audio-text synchronization
type SpeechAlignment struct {
	CharStartTimesMs []float64 `json:"char_start_times_ms"` // Start time in milliseconds for each character
	CharEndTimesMs   []float64 `json:"char_end_times_ms"`   // End time in milliseconds for each character
	Characters       []string  `json:"characters"`          // Characters corresponding to timing info
}

// SpeechInput represents the input for a speech request.
type SpeechInput struct {
	Input string `json:"input"`
}

type SpeechParameters struct {
	VoiceConfig    *SpeechVoiceInput `json:"voice"`
	Instructions   string            `json:"instructions,omitempty"`
	ResponseFormat string            `json:"response_format,omitempty"` // Default is "mp3"
	Speed          *float64          `json:"speed,omitempty"`

	LanguageCode                    *string                                `json:"language_code,omitempty"`
	PronunciationDictionaryLocators []SpeechPronunciationDictionaryLocator `json:"pronunciation_dictionary_locators,omitempty"`
	EnableLogging                   *bool                                  `json:"enable_logging,omitempty"`
	OptimizeStreamingLatency        *bool                                  `json:"optimize_streaming_latency,omitempty"`
	WithTimestamps                  *bool                                  `json:"with_timestamps,omitempty"` // Returns character-level timing information

	// Dynamic parameters that can be provider-specific, they are directly
	// added to the request as is.
	ExtraParams map[string]interface{} `json:"-"`
}

type SpeechPronunciationDictionaryLocator struct {
	PronunciationDictionaryID string  `json:"pronunciation_dictionary_id"`
	VersionID                 *string `json:"version_id,omitempty"`
}

type SpeechVoiceInput struct {
	Voice            *string
	MultiVoiceConfig []VoiceConfig
}

type VoiceConfig struct {
	Speaker string `json:"speaker"`
	Voice   string `json:"voice"`
}

// MarshalJSON implements custom JSON marshalling for SpeechVoiceInput.
// It marshals either Voice or MultiVoiceConfig directly without wrapping.
func (vi *SpeechVoiceInput) MarshalJSON() ([]byte, error) {
	// Validation: ensure only one field is set at a time
	if vi.Voice != nil && len(vi.MultiVoiceConfig) > 0 {
		return nil, fmt.Errorf("both Voice and MultiVoiceConfig are set; only one should be non-nil")
	}

	if vi.Voice != nil {
		return MarshalSorted(*vi.Voice)
	}
	if len(vi.MultiVoiceConfig) > 0 {
		return MarshalSorted(vi.MultiVoiceConfig)
	}
	// If both are nil, return null
	return MarshalSorted(nil)
}

// UnmarshalJSON implements custom JSON unmarshalling for SpeechVoiceInput.
// It determines whether "voice" is a string or a VoiceConfig object/array and assigns to the appropriate field.
// It also handles direct string/array content without a wrapper object.
func (vi *SpeechVoiceInput) UnmarshalJSON(data []byte) error {
	// Reset receiver state before attempting any decode to avoid stale data
	vi.Voice = nil
	vi.MultiVoiceConfig = nil

	// First, try to unmarshal as a direct string
	var stringContent string
	if err := Unmarshal(data, &stringContent); err == nil {
		vi.Voice = &stringContent
		return nil
	}

	// Try to unmarshal as an array of VoiceConfig objects
	var voiceConfigs []VoiceConfig
	if err := Unmarshal(data, &voiceConfigs); err == nil {
		// Validate each VoiceConfig and build a new slice deterministically
		validConfigs := make([]VoiceConfig, 0, len(voiceConfigs))
		for _, config := range voiceConfigs {
			if config.Voice == "" {
				return fmt.Errorf("voice config has empty voice field")
			}
			validConfigs = append(validConfigs, config)
		}
		vi.MultiVoiceConfig = validConfigs
		return nil
	}

	return fmt.Errorf("voice field is neither a string, nor an array of VoiceConfig objects")
}

type SpeechStreamResponseType string

const (
	SpeechStreamResponseTypeDelta SpeechStreamResponseType = "speech.audio.delta"
	SpeechStreamResponseTypeDone  SpeechStreamResponseType = "speech.audio.done"
)

type BifrostSpeechStreamResponse struct {
	Type        SpeechStreamResponseType   `json:"type"`
	Audio       []byte                     `json:"audio"`
	Usage       *SpeechUsage               `json:"usage"`
	ExtraFields BifrostResponseExtraFields `json:"extra_fields"`
}

// BackfillParams applies the same rule as BifrostSpeechResponse.BackfillParams to a stream chunk.
func (r *BifrostSpeechStreamResponse) BackfillParams(request *BifrostSpeechRequest) {
	if r == nil || request == nil || request.Input == nil {
		return
	}
	r.Usage = backfillSpeechInputChars(r.Usage, request.Input.Input)
}

// backfillSpeechInputChars returns usage with InputChars set to the code-point count of input
// when the provider reported none. Bifrost's per-character pricing reads InputChars, so it is
// still filled for vendors that report nothing; a vendor-reported count is kept as is.
func backfillSpeechInputChars(usage *SpeechUsage, input string) *SpeechUsage {
	if usage == nil {
		usage = &SpeechUsage{}
	}
	if usage.InputChars == 0 {
		usage.InputChars = utf8.RuneCountInString(input)
	}
	return usage
}

type SpeechUsageInputTokenDetails struct {
	TextTokens  int `json:"text_tokens,omitempty"`
	AudioTokens int `json:"audio_tokens,omitempty"`
}
type SpeechUsage struct {
	InputTokens       int                           `json:"input_tokens"`
	InputChars        int                           `json:"input_chars,omitempty"`
	InputTokenDetails *SpeechUsageInputTokenDetails `json:"input_token_details,omitempty"`
	OutputTokens      int                           `json:"output_tokens"`
	TotalTokens       int                           `json:"total_tokens"`
	// AudioSeconds is the generated audio duration in seconds. Populated for
	// duration-based audio models (e.g. ElevenLabs sound effects) so per-second
	// pricing can be applied; zero for token/character-billed TTS.
	AudioSeconds int `json:"audio_seconds,omitempty"`
	// Cost is the per-category cost breakdown, populated when pricing is available.
	Cost *BifrostCost `json:"cost,omitempty"`

	// Fork (TXE-6518, contract C5 BF6): quantities the vendor reported. BilledCharacters is the
	// character count the vendor billed (MiniMax usage_characters, ElevenLabs character-count); it
	// is never Bifrost's own count of the input, which stays in InputChars. Nil when the vendor did
	// not report it.
	BilledCharacters *int        `json:"billed_characters,omitempty"`
	VendorCost       *VendorCost `json:"vendor_cost,omitempty"`
}
