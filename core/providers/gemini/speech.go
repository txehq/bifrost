package gemini

import (
	"context"
	"fmt"
	"maps"
	"math"
	"strings"

	"github.com/capsohq/bifrost/core/providers/utils"
	"github.com/capsohq/bifrost/core/schemas"
)

// ToBifrostSpeechRequest converts a GeminiGenerationRequest to a BifrostSpeechRequest
func (request *GeminiGenerationRequest) ToBifrostSpeechRequest(ctx *schemas.BifrostContext) *schemas.BifrostSpeechRequest {
	provider, model := schemas.ParseModelString(request.Model, "")

	bifrostReq := &schemas.BifrostSpeechRequest{
		Provider: provider,
		Model:    model,
	}

	// Extract text input from contents
	var textInput string
	for _, content := range request.Contents {
		for _, part := range content.Parts {
			if part.Text != "" {
				textInput += part.Text
			}
		}
	}

	bifrostReq.Input = &schemas.SpeechInput{
		Input: textInput,
	}

	// Convert generation config to parameters
	if request.GenerationConfig.SpeechConfig != nil || len(request.GenerationConfig.ResponseModalities) > 0 {
		bifrostReq.Params = &schemas.SpeechParameters{}

		// Extract voice config from speech config
		if request.GenerationConfig.SpeechConfig != nil {
			// Handle single-speaker voice config
			if request.GenerationConfig.SpeechConfig.VoiceConfig != nil {
				bifrostReq.Params.VoiceConfig = &schemas.SpeechVoiceInput{}

				if request.GenerationConfig.SpeechConfig.VoiceConfig.PrebuiltVoiceConfig != nil {
					voiceName := request.GenerationConfig.SpeechConfig.VoiceConfig.PrebuiltVoiceConfig.VoiceName
					bifrostReq.Params.VoiceConfig.Voice = &voiceName
				}
			} else if request.GenerationConfig.SpeechConfig.MultiSpeakerVoiceConfig != nil {
				// Handle multi-speaker voice config
				// Convert to Bifrost's MultiVoiceConfig format
				if len(request.GenerationConfig.SpeechConfig.MultiSpeakerVoiceConfig.SpeakerVoiceConfigs) > 0 {
					bifrostReq.Params.VoiceConfig = &schemas.SpeechVoiceInput{}
					multiVoiceConfig := make([]schemas.VoiceConfig, 0, len(request.GenerationConfig.SpeechConfig.MultiSpeakerVoiceConfig.SpeakerVoiceConfigs))

					for _, speakerConfig := range request.GenerationConfig.SpeechConfig.MultiSpeakerVoiceConfig.SpeakerVoiceConfigs {
						if speakerConfig.VoiceConfig != nil && speakerConfig.VoiceConfig.PrebuiltVoiceConfig != nil {
							multiVoiceConfig = append(multiVoiceConfig, schemas.VoiceConfig{
								Speaker: speakerConfig.Speaker,
								Voice:   speakerConfig.VoiceConfig.PrebuiltVoiceConfig.VoiceName,
							})
						}
					}

					bifrostReq.Params.VoiceConfig.MultiVoiceConfig = multiVoiceConfig
				}
			}
		}

		// The native generationConfig.maxOutputTokens is the unified output-token bound.
		if request.GenerationConfig.MaxOutputTokens > 0 {
			bifrostReq.Params.MaxOutputTokens = schemas.Ptr(int(request.GenerationConfig.MaxOutputTokens))
		}

		// Store response modalities in extra params if needed
		if len(request.GenerationConfig.ResponseModalities) > 0 {
			if bifrostReq.Params.ExtraParams == nil {
				bifrostReq.Params.ExtraParams = make(map[string]interface{})
			}
			modalities := make([]string, len(request.GenerationConfig.ResponseModalities))
			for i, mod := range request.GenerationConfig.ResponseModalities {
				modalities[i] = string(mod)
			}
			bifrostReq.Params.ExtraParams["response_modalities"] = modalities
		}
	}

	return bifrostReq
}

// Speech response formats. Gemini's generateContent answers 24 kHz 16-bit mono PCM and
// nothing else: pcm returns those bytes as they are, wav wraps them in a RIFF header.
const (
	SpeechResponseFormatWAV = "wav"
	SpeechResponseFormatPCM = "pcm"
)

// speechMaxOutputTokensParam is the unified speech field for the output-token bound.
const speechMaxOutputTokensParam = "max_output_tokens"

// SpeechUnifiedFields are the unified speech fields (the JSON names of
// schemas.SpeechParameters) the generateContent conversion delivers: voice as
// speechConfig, response_format by converting the PCM, max_output_tokens as
// generationConfig.maxOutputTokens. Every other speech field has no generateContent
// counterpart, so a provider that sends speech through ToGeminiSpeechRequest declares
// exactly these (schemas.UnifiedFieldDeclarer) and a request carrying another one is
// refused with caller_unsupported_parameter (contract C4).
var SpeechUnifiedFields = []string{"voice", "response_format", speechMaxOutputTokensParam}

// TranscriptionUnifiedFields are the unified transcription fields the generateContent
// conversion delivers: prompt as the text part, temperature as
// generationConfig.temperature, response_format when it is json (the only shape the
// transcript is returned in), and file_format, which describes the upload and is sent as
// the audio part's detected MIME type. generateContent has no language, timestamp or
// output-format field, so those are refused (contract C4).
var TranscriptionUnifiedFields = []string{"prompt", "temperature", "response_format", "file_format"}

// ResolveSpeechResponseFormat returns the format a speech response is returned in: the
// requested one, or wav when none is named.
func ResolveSpeechResponseFormat(params *schemas.SpeechParameters) string {
	if params == nil || params.ResponseFormat == "" {
		return SpeechResponseFormatWAV
	}
	return params.ResponseFormat
}

// ValidateSpeechRequest returns the caller_unsupported_parameter error for a value of a
// declared speech field that generateContent cannot take as given, and nil otherwise. The
// fields themselves are checked against SpeechUnifiedFields by core; this is the half of
// contract C4 that needs the value. It allocates only when it refuses.
func ValidateSpeechRequest(bifrostReq *schemas.BifrostSpeechRequest) *schemas.BifrostError {
	if bifrostReq == nil || bifrostReq.Params == nil {
		return nil
	}
	switch bifrostReq.Params.ResponseFormat {
	case "", SpeechResponseFormatWAV, SpeechResponseFormatPCM:
	default:
		return schemas.NewUnsupportedParameterError("response_format",
			fmt.Sprintf("response_format %q is not supported for Gemini speech; use wav or pcm (default wav)", bifrostReq.Params.ResponseFormat))
	}
	if maxTokens := bifrostReq.Params.MaxOutputTokens; maxTokens != nil && (*maxTokens < 1 || *maxTokens > math.MaxInt32) {
		return schemas.NewUnsupportedParameterError(speechMaxOutputTokensParam,
			fmt.Sprintf("max_output_tokens %d cannot be sent to Gemini; it must be between 1 and %d", *maxTokens, math.MaxInt32))
	}
	return nil
}

// ToGeminiSpeechRequest converts a BifrostSpeechRequest to a GeminiGenerationRequest
func ToGeminiSpeechRequest(bifrostReq *schemas.BifrostSpeechRequest) (*GeminiGenerationRequest, error) {
	if bifrostReq == nil {
		return nil, fmt.Errorf("bifrostReq is nil")
	}
	// Callers on the provider path have run ValidateSpeechRequest; this keeps a direct
	// caller of the converter from sending a value Gemini cannot take.
	if bifrostErr := ValidateSpeechRequest(bifrostReq); bifrostErr != nil {
		return nil, fmt.Errorf("%s", bifrostErr.Error.Message)
	}
	// Create the base Gemini generation request
	geminiReq := &GeminiGenerationRequest{
		Model: bifrostReq.Model,
	}
	// Convert parameters to generation config
	geminiReq.GenerationConfig.ResponseModalities = []Modality{ModalityAudio}
	// The output-token bound, sent as given (never clamped).
	if bifrostReq.Params != nil && bifrostReq.Params.MaxOutputTokens != nil {
		geminiReq.GenerationConfig.MaxOutputTokens = int32(*bifrostReq.Params.MaxOutputTokens)
	}
	// Convert speech input to Gemini format
	if bifrostReq.Input != nil && bifrostReq.Input.Input != "" {
		geminiReq.Contents = []Content{
			{
				Parts: []*Part{
					{
						Text: bifrostReq.Input.Input,
					},
				},
			},
		}
		// Add speech config to generation config if voice config is provided
		if bifrostReq.Params != nil && bifrostReq.Params.VoiceConfig != nil {
			// Handle both single voice and multi-voice configurations
			if bifrostReq.Params.VoiceConfig.Voice != nil || len(bifrostReq.Params.VoiceConfig.MultiVoiceConfig) > 0 {
				addSpeechConfigToGenerationConfig(&geminiReq.GenerationConfig, bifrostReq.Params.VoiceConfig)
			}
			geminiReq.ExtraParams = bifrostReq.Params.ExtraParams
			// The HTTP transport also leaves max_output_tokens in the extra params. It was
			// delivered above; merged into the body under that name Gemini would refuse it.
			if _, ok := geminiReq.ExtraParams[speechMaxOutputTokensParam]; ok {
				geminiReq.ExtraParams = maps.Clone(geminiReq.ExtraParams)
				delete(geminiReq.ExtraParams, speechMaxOutputTokensParam)
			}
		}
	}
	return geminiReq, nil
}

// SpeechBlockError returns the error for a speech generation Gemini blocked, and nil when
// it was not blocked: a prompt refused before generating (promptFeedback.blockReason with
// no candidates) or a candidate that ended on a safety finishReason. A speech response is
// audio bytes with nowhere to carry a content_filter marker, so the vendor's 200 becomes a
// 400 caller_content_filtered whose error.code is the vendor's reason verbatim (contract
// C6). It allocates only when the response is blocked.
func (response *GenerateContentResponse) SpeechBlockError() *schemas.BifrostError {
	if response == nil {
		return nil
	}
	if len(response.Candidates) == 0 {
		feedback := response.PromptFeedback
		if feedback == nil || feedback.BlockReason == "" {
			return nil
		}
		message := feedback.BlockReasonMessage
		if message == "" {
			message = "Gemini blocked the prompt: " + feedback.BlockReason
		}
		return schemas.NewVendorErrorObjectError(schemas.VendorErrorCauseContentFiltered, feedback.BlockReason, message)
	}
	for _, candidate := range response.Candidates {
		if candidate == nil || !isSpeechSafetyFinishReason(candidate.FinishReason) {
			continue
		}
		message := candidate.FinishMessage
		if message == "" {
			message = "Gemini blocked the speech: finishReason " + string(candidate.FinishReason)
		}
		return schemas.NewVendorErrorObjectError(schemas.VendorErrorCauseContentFiltered, string(candidate.FinishReason), message)
	}
	return nil
}

// isSpeechSafetyFinishReason reports whether a candidate's finishReason says Gemini stopped
// the speech over its content. LANGUAGE (an unsupported language) and the reasons that only
// image or tool generation produce are not content blocks.
func isSpeechSafetyFinishReason(reason FinishReason) bool {
	switch reason {
	case FinishReasonSafety, FinishReasonRecitation, FinishReasonBlocklist, FinishReasonProhibitedContent, FinishReasonSPII:
		return true
	}
	return false
}

// ToBifrostSpeechResponse converts a GenerateContentResponse to a BifrostSpeechResponse.
// A blocked response is the caller's to turn into an error first (SpeechBlockError).
func (response *GenerateContentResponse) ToBifrostSpeechResponse(ctx context.Context) (*schemas.BifrostSpeechResponse, error) {
	bifrostResp := &schemas.BifrostSpeechResponse{}

	// Process candidates to extract audio content
	if len(response.Candidates) > 0 {
		candidate := response.Candidates[0]
		if candidate.Content != nil && len(candidate.Content.Parts) > 0 {
			var audioData []byte
			audioMIMEType := ""
			// Extract audio data from all parts
			for _, part := range candidate.Content.Parts {
				if part.InlineData != nil && len(part.InlineData.Data) > 0 {
					// Check if this is audio data
					if strings.HasPrefix(part.InlineData.MIMEType, "audio/") {
						if audioMIMEType == "" {
							audioMIMEType = part.InlineData.MIMEType
						}
						decodedData, err := decodeBase64StringToBytes(part.InlineData.Data)
						if err != nil {
							return nil, fmt.Errorf("failed to decode base64 audio data: %v", err)
						}
						audioData = append(audioData, decodedData...)
					}
				}
			}
			if len(audioData) > 0 {
				// Gemini returns PCM audio (s16le, 24000 Hz, mono). It is converted to WAV, a
				// standard playable format, unless the request asked for the bare PCM; a context
				// that names no format gets WAV, the default.
				responseFormat, _ := ctx.Value(BifrostContextKeyResponseFormat).(string)
				if responseFormat != SpeechResponseFormatPCM {
					wavData, err := utils.ConvertPCMToWAV(audioData, utils.DefaultGeminiPCMConfig())
					if err != nil {
						return nil, fmt.Errorf("failed to convert PCM to WAV: %v", err)
					}
					bifrostResp.Audio = wavData
					bifrostResp.ContentType = "audio/wav"
				} else {
					// Raw PCM as Gemini sent it; its mimeType (audio/L16;codec=pcm;rate=24000)
					// is the only record of the format (TXE-6530).
					bifrostResp.Audio = audioData
					bifrostResp.ContentType = audioMIMEType
				}
			}

			// Set usage information
			if response.UsageMetadata != nil {
				bifrostResp.Usage = convertGeminiUsageMetadataToSpeechUsage(response.UsageMetadata)
			}
		}
	}
	return bifrostResp, nil
}

// ToGeminiSpeechResponse converts a BifrostSpeechResponse to Gemini's GenerateContentResponse
func ToGeminiSpeechResponse(bifrostResp *schemas.BifrostSpeechResponse) *GenerateContentResponse {
	if bifrostResp == nil {
		return nil
	}

	genaiResp := &GenerateContentResponse{}

	candidate := &Candidate{
		Content: &Content{
			Parts: []*Part{
				{
					InlineData: &Blob{
						Data:     encodeBytesToBase64String(bifrostResp.Audio),
						MIMEType: utils.DetectAudioMimeType(bifrostResp.Audio),
					},
				},
			},
			Role: string(RoleModel),
		},
	}

	// Set usage metadata if present
	if bifrostResp.Usage != nil {
		genaiResp.UsageMetadata = convertBifrostSpeechUsageToGeminiUsageMetadata(bifrostResp.Usage)
	}

	genaiResp.Candidates = []*Candidate{candidate}
	return genaiResp
}

// ToGeminiSpeechStreamResponse converts a Bifrost speech stream chunk to the
// GenerateContentResponse shape used by Gemini's streamGenerateContent SSE API.
func ToGeminiSpeechStreamResponse(bifrostResp *schemas.BifrostSpeechStreamResponse) *GenerateContentResponse {
	if bifrostResp == nil {
		return nil
	}

	genaiResp := &GenerateContentResponse{}
	if len(bifrostResp.Audio) > 0 {
		mimeType := utils.DetectAudioMimeType(bifrostResp.Audio)
		provider := bifrostResp.ExtraFields.RoutingInfo.Provider
		if provider == schemas.Gemini || provider == schemas.Vertex {
			// Gemini TTS stream chunks are headerless signed 16-bit PCM. Generic
			// header detection falls back to audio/mp3 for headerless data, which
			// makes native GenAI clients decode valid PCM as the wrong format.
			mimeType = "audio/L16;codec=pcm;rate=24000"
		}
		genaiResp.Candidates = []*Candidate{{
			Content: &Content{
				Parts: []*Part{{
					InlineData: &Blob{
						Data:     encodeBytesToBase64String(bifrostResp.Audio),
						MIMEType: mimeType,
					},
				}},
				Role: string(RoleModel),
			},
		}}
	}

	if bifrostResp.Type == schemas.SpeechStreamResponseTypeDone {
		if len(genaiResp.Candidates) == 0 {
			genaiResp.Candidates = []*Candidate{{FinishReason: FinishReasonStop}}
		} else {
			genaiResp.Candidates[0].FinishReason = FinishReasonStop
		}
	}
	if bifrostResp.Usage != nil {
		genaiResp.UsageMetadata = convertBifrostSpeechUsageToGeminiUsageMetadata(bifrostResp.Usage)
	}

	return genaiResp
}
