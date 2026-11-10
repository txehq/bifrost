package googlecloudtts

import (
	"fmt"
	"strings"

	schemas "github.com/capsohq/bifrost/core/schemas"
)

// AudioConfig.speakingRate is documented as "in the range [0.25, 2.0] ... Any other
// values < 0.25 or > 2.0 will return an error".
const (
	minSpeakingRate = 0.25
	maxSpeakingRate = 2.0
)

// defaultAudioEncoding is sent when the request names no response_format: the unified
// speech route defaults to mp3, and audioConfig.audioEncoding is required by the vendor.
const defaultAudioEncoding = "MP3"

// audioEncodings maps a unified response_format onto the vendor's AudioEncoding. The
// first four are the unified names the vendor can produce; the rest are the vendor's own
// encodings, taken under their own names. aac and flac have no AudioEncoding.
var audioEncodings = map[string]string{
	"mp3":      "MP3",
	"opus":     "OGG_OPUS", // Opus in an Ogg container
	"wav":      "LINEAR16", // 16-bit linear PCM with a WAV header
	"pcm":      "PCM",      // 16-bit linear PCM, no header
	"ogg_opus": "OGG_OPUS",
	"linear16": "LINEAR16",
	"mulaw":    "MULAW",
	"alaw":     "ALAW",
	"m4a":      "M4A",
}

// audioEncodingFor returns the vendor AudioEncoding for a unified response_format, and
// false when the vendor has none for it.
func audioEncodingFor(responseFormat string) (string, bool) {
	if responseFormat == "" {
		return defaultAudioEncoding, true
	}
	encoding, ok := audioEncodings[strings.ToLower(responseFormat)]
	return encoding, ok
}

// checkSpeechParams refuses the values text:synthesize cannot take as given, each with
// caller_unsupported_parameter naming the field. Nothing is dropped, rewritten or clamped.
// Fields the provider does not take at all are refused by core from SupportedUnifiedFields.
func checkSpeechParams(params *schemas.SpeechParameters) *schemas.BifrostError {
	if params == nil {
		return nil
	}
	if params.VoiceConfig != nil && len(params.VoiceConfig.MultiVoiceConfig) > 0 {
		return schemas.NewUnsupportedParameterError("voice", "voice must be one voice name: a multi-speaker voice list is not supported by google-cloud-tts")
	}
	if _, ok := audioEncodingFor(params.ResponseFormat); !ok {
		return schemas.NewUnsupportedParameterError("response_format", fmt.Sprintf("response_format %q is not supported by google-cloud-tts: use mp3, opus, wav, pcm, mulaw, alaw or m4a", params.ResponseFormat))
	}
	if params.Speed != nil && (*params.Speed < minSpeakingRate || *params.Speed > maxSpeakingRate) {
		return schemas.NewUnsupportedParameterError("speed", fmt.Sprintf("speed %v is outside the range google-cloud-tts takes (%v to %v)", *params.Speed, minSpeakingRate, maxSpeakingRate))
	}
	return nil
}

// isSSML reports whether the input is an SSML document: its root element is <speak>
// (https://docs.cloud.google.com/text-to-speech/docs/ssml).
func isSSML(input string) bool {
	rest, ok := strings.CutPrefix(strings.TrimSpace(input), "<speak")
	if !ok || rest == "" {
		return false
	}
	switch rest[0] {
	case '>', ' ', '\t', '\r', '\n', '/':
		return true
	}
	return false
}

// ToGoogleCloudTTSSpeechRequest maps a unified speech request onto the text:synthesize
// body: input → input.text (input.ssml for an SSML document), voice → voice.name,
// language_code → voice.languageCode, response_format → audioConfig.audioEncoding,
// speed → audioConfig.speakingRate. The request's model is the caller's price tier
// (wavenet, neural2, ...) and has no vendor field: the voice name selects the voice.
// The caller is expected to have run checkSpeechParams.
func ToGoogleCloudTTSSpeechRequest(bifrostReq *schemas.BifrostSpeechRequest) *GoogleCloudTTSSynthesizeRequest {
	if bifrostReq == nil || bifrostReq.Input == nil {
		return nil
	}

	synthesizeReq := &GoogleCloudTTSSynthesizeRequest{
		AudioConfig: GoogleCloudTTSAudioConfig{AudioEncoding: defaultAudioEncoding},
	}
	if isSSML(bifrostReq.Input.Input) {
		synthesizeReq.Input.SSML = bifrostReq.Input.Input
	} else {
		synthesizeReq.Input.Text = bifrostReq.Input.Input
	}

	params := bifrostReq.Params
	if params == nil {
		return synthesizeReq
	}
	synthesizeReq.ExtraParams = params.ExtraParams

	if params.VoiceConfig != nil && params.VoiceConfig.Voice != nil {
		synthesizeReq.Voice.Name = *params.VoiceConfig.Voice
	}
	if params.LanguageCode != nil {
		synthesizeReq.Voice.LanguageCode = *params.LanguageCode
	}
	if encoding, ok := audioEncodingFor(params.ResponseFormat); ok {
		synthesizeReq.AudioConfig.AudioEncoding = encoding
	}
	synthesizeReq.AudioConfig.SpeakingRate = params.Speed

	return synthesizeReq
}
