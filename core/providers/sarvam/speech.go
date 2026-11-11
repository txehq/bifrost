package sarvam

import (
	schemas "github.com/capsohq/bifrost/core/schemas"
)

// speechMappedUnifiedFields are the unified speech fields ToSarvamSpeechRequest maps to
// Sarvam's own names: voice as speaker, speed as pace, language_code as target_language_code.
var speechMappedUnifiedFields = []string{"voice", "speed", "language_code"}

// ToSarvamSpeechRequest maps a Bifrost speech request onto Sarvam's text-to-speech request.
func ToSarvamSpeechRequest(bifrostReq *schemas.BifrostSpeechRequest) *SarvamSpeechRequest {
	if bifrostReq == nil || bifrostReq.Input == nil {
		return nil
	}

	sarvamReq := &SarvamSpeechRequest{
		Text:  bifrostReq.Input.Input,
		Model: bifrostReq.Model,
	}

	if bifrostReq.Params == nil {
		return sarvamReq
	}

	// Copy so consuming keys below doesn't mutate the caller's request map.
	// language_code is target_language_code here; the speech route's copy of it among the
	// extra params is not forwarded under the unified name (TXE-7077).
	if unmapped := bifrostReq.Params.UnmappedExtraParams(speechMappedUnifiedFields...); unmapped != nil {
		sarvamReq.ExtraParams = make(map[string]interface{}, len(unmapped))
		for k, v := range unmapped {
			sarvamReq.ExtraParams[k] = v
		}
	}

	if bifrostReq.Params.LanguageCode != nil {
		sarvamReq.TargetLanguageCode = *bifrostReq.Params.LanguageCode
	}
	if bifrostReq.Params.VoiceConfig != nil && bifrostReq.Params.VoiceConfig.Voice != nil {
		sarvamReq.Speaker = bifrostReq.Params.VoiceConfig.Voice
	}
	if bifrostReq.Params.Speed != nil {
		sarvamReq.Pace = bifrostReq.Params.Speed
	}

	if ep := bifrostReq.Params.ExtraParams; ep != nil {
		if v, ok := schemas.SafeExtractStringPointer(ep["target_language_code"]); ok {
			delete(sarvamReq.ExtraParams, "target_language_code")
			sarvamReq.TargetLanguageCode = *v
		}
		if v, ok := schemas.SafeExtractStringPointer(ep["speaker"]); ok {
			delete(sarvamReq.ExtraParams, "speaker")
			sarvamReq.Speaker = v
		}
		if v, ok := schemas.SafeExtractFloat64Pointer(ep["pace"]); ok {
			delete(sarvamReq.ExtraParams, "pace")
			sarvamReq.Pace = v
		}
		if v, ok := schemas.SafeExtractFloat64Pointer(ep["pitch"]); ok {
			delete(sarvamReq.ExtraParams, "pitch")
			sarvamReq.Pitch = v
		}
		if v, ok := schemas.SafeExtractFloat64Pointer(ep["loudness"]); ok {
			delete(sarvamReq.ExtraParams, "loudness")
			sarvamReq.Loudness = v
		}
		if v, ok := schemas.SafeExtractFloat64Pointer(ep["temperature"]); ok {
			delete(sarvamReq.ExtraParams, "temperature")
			sarvamReq.Temperature = v
		}
		if v, ok := schemas.SafeExtractIntPointer(ep["speech_sample_rate"]); ok {
			delete(sarvamReq.ExtraParams, "speech_sample_rate")
			sarvamReq.SpeechSampleRate = v
		}
		if v, ok := schemas.SafeExtractStringPointer(ep["output_audio_codec"]); ok {
			delete(sarvamReq.ExtraParams, "output_audio_codec")
			sarvamReq.OutputAudioCodec = v
		}
		if v, ok := schemas.SafeExtractBoolPointer(ep["enable_preprocessing"]); ok {
			delete(sarvamReq.ExtraParams, "enable_preprocessing")
			sarvamReq.EnablePreprocessing = v
		}
		if v, ok := schemas.SafeExtractStringPointer(ep["dict_id"]); ok {
			delete(sarvamReq.ExtraParams, "dict_id")
			sarvamReq.DictID = v
		}
		if v, ok := schemas.SafeExtractBoolPointer(ep["enable_cached_responses"]); ok {
			delete(sarvamReq.ExtraParams, "enable_cached_responses")
			sarvamReq.EnableCachedResponses = v
		}
	}

	return sarvamReq
}
