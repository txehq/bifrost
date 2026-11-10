package openai

import (
	"github.com/capsohq/bifrost/core/schemas"
)

// ToBifrostSpeechRequest converts an OpenAI speech request to Bifrost format
func (request *OpenAISpeechRequest) ToBifrostSpeechRequest(ctx *schemas.BifrostContext) *schemas.BifrostSpeechRequest {
	provider, model := schemas.ParseModelString(request.Model, "")

	return &schemas.BifrostSpeechRequest{
		Provider:  provider,
		Model:     model,
		Input:     &schemas.SpeechInput{Input: request.Input},
		Params:    &request.SpeechParameters,
		Fallbacks: schemas.ParseFallbacks(request.Fallbacks),
	}
}

// ToOpenAISpeechRequest converts a Bifrost speech request to OpenAI format
func ToOpenAISpeechRequest(bifrostReq *schemas.BifrostSpeechRequest) *OpenAISpeechRequest {
	if bifrostReq == nil || bifrostReq.Input.Input == "" {
		return nil
	}

	speechInput := bifrostReq.Input
	params := bifrostReq.Params

	openaiReq := &OpenAISpeechRequest{
		Model: bifrostReq.Model,
		Input: speechInput.Input,
	}

	if params != nil {
		openaiReq.SpeechParameters = *params
		// SpeechParameters is shared with ElevenLabs. OpenAI's /audio/speech schema has none of
		// these fields, so they are not sent to OpenAI-shaped endpoints (OpenAI, Azure, Groq,
		// OpenRouter) even when the caller set them (TXE-6530, contract C4).
		openaiReq.LanguageCode = nil
		openaiReq.PronunciationDictionaryLocators = nil
		openaiReq.EnableLogging = nil
		openaiReq.OptimizeStreamingLatency = nil
		openaiReq.WithTimestamps = nil
	}

	if bifrostReq.Params != nil {
		openaiReq.ExtraParams = bifrostReq.Params.ExtraParams
	}
	return openaiReq
}
