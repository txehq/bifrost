package xai

import schemas "github.com/capsohq/bifrost/core/schemas"

// SupportedUnifiedFields declares, per operation, the unified request fields xAI takes, so
// core refuses any other one with 400 caller_unsupported_parameter before a key is chosen and
// without an upstream call (fork, TXE-6429, contract C4). An operation not listed here makes
// no declaration and is not checked.
func (provider *XAIProvider) SupportedUnifiedFields(requestType schemas.RequestType) ([]string, bool) {
	switch requestType {
	case schemas.VideoGenerationRequest:
		return xaiVideoUnifiedFields, true
	case schemas.SpeechRequest:
		return xaiSpeechUnifiedFields, true
	case schemas.TranscriptionRequest:
		return xaiTranscriptionUnifiedFields, true
	}
	return nil, false
}
