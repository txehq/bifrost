package elevenlabs

import (
	"maps"

	"github.com/capsohq/bifrost/core/schemas"
)

func ToElevenlabsSpeechRequest(bifrostReq *schemas.BifrostSpeechRequest) *ElevenlabsSpeechRequest {
	if bifrostReq == nil || bifrostReq.Input == nil {
		return nil
	}

	elevenlabsReq := &ElevenlabsSpeechRequest{
		ModelID: bifrostReq.Model,
		Text:    bifrostReq.Input.Input,
	}

	if bifrostReq.Params != nil {
		// Only the voice settings the caller gave are sent; the rest stay absent so the
		// vendor's defaults apply (contract C4).
		voiceSettings := ElevenlabsVoiceSettings{Speed: bifrostReq.Params.Speed}
		hasVoiceSettings := voiceSettings.Speed != nil

		if len(bifrostReq.Params.ExtraParams) > 0 {
			// Work on a copy: the keys consumed below are deleted from it, and the caller's
			// map must stay whole for any later use of the same request.
			extra := maps.Clone(bifrostReq.Params.ExtraParams)
			elevenlabsReq.ExtraParams = extra

			if stability, ok := schemas.SafeExtractFloat64Pointer(extra["stability"]); ok {
				delete(extra, "stability")
				voiceSettings.Stability = stability
				hasVoiceSettings = true
			}
			if useSpeakerBoost, ok := schemas.SafeExtractBoolPointer(extra["use_speaker_boost"]); ok {
				delete(extra, "use_speaker_boost")
				voiceSettings.UseSpeakerBoost = useSpeakerBoost
				hasVoiceSettings = true
			}
			if similarityBoost, ok := schemas.SafeExtractFloat64Pointer(extra["similarity_boost"]); ok {
				delete(extra, "similarity_boost")
				voiceSettings.SimilarityBoost = similarityBoost
				hasVoiceSettings = true
			}
			if style, ok := schemas.SafeExtractFloat64Pointer(extra["style"]); ok {
				delete(extra, "style")
				voiceSettings.Style = style
				hasVoiceSettings = true
			}
			if seed, ok := schemas.SafeExtractIntPointer(extra["seed"]); ok {
				delete(extra, "seed")
				elevenlabsReq.Seed = seed
			}
			if previousText, ok := schemas.SafeExtractStringPointer(extra["previous_text"]); ok {
				delete(extra, "previous_text")
				elevenlabsReq.PreviousText = previousText
			}
			if nextText, ok := schemas.SafeExtractStringPointer(extra["next_text"]); ok {
				delete(extra, "next_text")
				elevenlabsReq.NextText = nextText
			}
			if previousRequestIDs, ok := schemas.SafeExtractStringSlice(extra["previous_request_ids"]); ok {
				delete(extra, "previous_request_ids")
				elevenlabsReq.PreviousRequestIDs = previousRequestIDs
			}
			if nextRequestIDs, ok := schemas.SafeExtractStringSlice(extra["next_request_ids"]); ok {
				delete(extra, "next_request_ids")
				elevenlabsReq.NextRequestIDs = nextRequestIDs
			}
			if applyTextNormalization, ok := schemas.SafeExtractStringPointer(extra["apply_text_normalization"]); ok {
				delete(extra, "apply_text_normalization")
				elevenlabsReq.ApplyTextNormalization = applyTextNormalization
			}
			if applyLanguageTextNormalization, ok := schemas.SafeExtractBoolPointer(extra["apply_language_text_normalization"]); ok {
				delete(extra, "apply_language_text_normalization")
				elevenlabsReq.ApplyLanguageTextNormalization = applyLanguageTextNormalization
			}
			if usePVCAsIVC, ok := schemas.SafeExtractBoolPointer(extra["use_pvc_as_ivc"]); ok {
				delete(extra, "use_pvc_as_ivc")
				elevenlabsReq.UsePVCAsIVC = usePVCAsIVC
			}
		}

		if hasVoiceSettings {
			elevenlabsReq.VoiceSettings = &voiceSettings
		}

		if bifrostReq.Params.LanguageCode != nil {
			elevenlabsReq.LanguageCode = bifrostReq.Params.LanguageCode
		}

		if len(bifrostReq.Params.PronunciationDictionaryLocators) > 0 {
			elevenlabsReq.PronunciationDictionaryLocators = make([]ElevenlabsPronunciationDictionaryLocator, len(bifrostReq.Params.PronunciationDictionaryLocators))
			for i, locator := range bifrostReq.Params.PronunciationDictionaryLocators {
				elevenlabsReq.PronunciationDictionaryLocators[i] = ElevenlabsPronunciationDictionaryLocator{
					PronunciationDictionaryID: locator.PronunciationDictionaryID,
					VersionID:                 locator.VersionID,
				}
			}
		}
	}

	return elevenlabsReq
}
