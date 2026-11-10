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

// voiceFamily returns the voice family a Cloud Text-to-Speech voice name carries, as written
// in the name, and false when the name has none. Every name in Google's voice list
// (https://docs.cloud.google.com/text-to-speech/docs/list-voices-and-types) is
// <language>-<REGION>-<family>-<variant>: en-US-Wavenet-D is Wavenet, en-US-Chirp3-HD-Achernar
// is Chirp3-HD. It allocates nothing: it runs on every request.
func voiceFamily(voiceName string) (string, bool) {
	if strings.HasPrefix(voiceName, "-") || strings.HasSuffix(voiceName, "-") || strings.Contains(voiceName, "--") {
		return "", false
	}
	language := strings.IndexByte(voiceName, '-')
	if language < 0 {
		return "", false
	}
	region := strings.IndexByte(voiceName[language+1:], '-')
	if region < 0 {
		return "", false
	}
	familyStart := language + 1 + region + 1
	variant := strings.LastIndexByte(voiceName, '-')
	if variant < familyStart {
		return "", false
	}
	return voiceName[familyStart:variant], true
}

// checkVoiceFamily refuses a voice that is not of the family the request names as its model.
// text:synthesize has no model field, so the voice name alone decides which voice family
// Google serves and bills, while the model is what governance admits and what the caller is
// priced on: a wavenet request carrying a Chirp3-HD voice would be served the dearer family
// under the cheaper name. A request with no voice name is refused for the same reason: Google
// would pick the voice, and its family, by itself.
func checkVoiceFamily(model string, params *schemas.SpeechParameters) *schemas.BifrostError {
	voiceName := ""
	if params != nil && params.VoiceConfig != nil && params.VoiceConfig.Voice != nil {
		voiceName = *params.VoiceConfig.Voice
	}
	if voiceName == "" {
		return schemas.NewUnsupportedParameterError("voice", fmt.Sprintf("voice is required by google-cloud-tts: name a voice of the %s family", model))
	}
	family, ok := voiceFamily(voiceName)
	if !ok {
		return schemas.NewUnsupportedParameterError("voice", fmt.Sprintf("voice %q is not a Cloud Text-to-Speech voice name of the form <language>-<REGION>-<family>-<variant>, so google-cloud-tts cannot tell that it belongs to model %q", voiceName, model))
	}
	if !strings.EqualFold(family, model) {
		return schemas.NewUnsupportedParameterError("voice", fmt.Sprintf("voice %q is a %s voice and the request's model is %q: the model must name the voice's family", voiceName, strings.ToLower(family), model))
	}
	return nil
}

// ssmlVoiceTag is the start of an SSML <voice> element, which reads its content in another
// voice (https://docs.cloud.google.com/text-to-speech/docs/ssml#select_a_voice).
const ssmlVoiceTag = "<voice"

// checkSSMLVoices refuses an SSML document that selects a voice the request's model does not
// cover. Google lets a <voice name="..."> element switch family inside one request (a Studio
// voice inside a Standard request), so the voice parameter alone does not bind the family
// that is served. Every <voice> element must carry a name of the model's family; an element
// that selects its voice any other way (gender, language, variant), or whose name cannot be
// read as written (an entity reference, a malformed tag), is refused, because its family
// cannot be established. The scan matches the tag name without regard to case and does not
// skip comments or CDATA, so it can only refuse more than Google would read, never less.
// It allocates nothing unless it refuses.
func checkSSMLVoices(model, ssml string) *schemas.BifrostError {
	rest := ssml
	for {
		at := indexFoldASCII(rest, ssmlVoiceTag)
		if at < 0 {
			return nil
		}
		rest = rest[at+len(ssmlVoiceTag):]
		if rest == "" {
			return ssmlVoiceError(model, "a <voice> element is not closed")
		}
		if !isXMLSpace(rest[0]) && rest[0] != '>' && rest[0] != '/' {
			continue // another element whose name starts with "voice"
		}
		named := false
		for {
			rest = strings.TrimLeft(rest, " \t\r\n")
			if rest == "" {
				return ssmlVoiceError(model, "a <voice> element is not closed")
			}
			if rest[0] == '>' || rest[0] == '/' {
				break
			}
			nameEnd := strings.IndexAny(rest, "= \t\r\n>/")
			if nameEnd <= 0 {
				return ssmlVoiceError(model, "a <voice> element has a malformed attribute")
			}
			attr := rest[:nameEnd]
			rest = strings.TrimLeft(rest[nameEnd:], " \t\r\n")
			if rest == "" || rest[0] != '=' {
				return ssmlVoiceError(model, "a <voice> element has a malformed attribute")
			}
			rest = strings.TrimLeft(rest[1:], " \t\r\n")
			if rest == "" || (rest[0] != '"' && rest[0] != '\'') {
				return ssmlVoiceError(model, "a <voice> element has a malformed attribute")
			}
			valueEnd := strings.IndexByte(rest[1:], rest[0])
			if valueEnd < 0 {
				return ssmlVoiceError(model, "a <voice> element has a malformed attribute")
			}
			value := rest[1 : 1+valueEnd]
			rest = rest[valueEnd+2:]
			if !strings.EqualFold(attr, "name") {
				continue
			}
			named = true
			family, ok := voiceFamily(value)
			if !ok || strings.ContainsRune(value, '&') {
				return ssmlVoiceError(model, fmt.Sprintf("<voice name=%q> is not a Cloud Text-to-Speech voice name of the form <language>-<REGION>-<family>-<variant>", value))
			}
			if !strings.EqualFold(family, model) {
				return ssmlVoiceError(model, fmt.Sprintf("<voice name=%q> selects a %s voice", value, strings.ToLower(family)))
			}
		}
		if !named {
			return ssmlVoiceError(model, "a <voice> element selects its voice without a name")
		}
	}
}

func ssmlVoiceError(model, what string) *schemas.BifrostError {
	return schemas.NewUnsupportedParameterError("input", fmt.Sprintf("%s and the request's model is %q: every <voice> element in the SSML must name a voice of the model's family", what, model))
}

func isXMLSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n'
}

// indexFoldASCII returns the index of the first occurrence of the lower-case ASCII needle in
// s, comparing ASCII letters without regard to case, or -1.
func indexFoldASCII(s, needle string) int {
	for i := 0; i+len(needle) <= len(s); i++ {
		if strings.EqualFold(s[i:i+len(needle)], needle) {
			return i
		}
	}
	return -1
}

// checkSpeechParams refuses the values text:synthesize cannot take as given, each with
// caller_unsupported_parameter naming the field. Nothing is dropped, rewritten or clamped.
// Fields the provider does not take at all are refused by core from SupportedUnifiedFields.
func checkSpeechParams(model string, params *schemas.SpeechParameters) *schemas.BifrostError {
	if params == nil {
		return checkVoiceFamily(model, nil)
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
	return checkVoiceFamily(model, params)
}

// isSSML reports whether the input is an SSML document: its root element is <speak>
// (https://docs.cloud.google.com/text-to-speech/docs/ssml). An XML declaration, processing
// instructions and comments may come before the root element.
func isSSML(input string) bool {
	rest := strings.TrimSpace(input)
	for {
		var end string
		switch {
		case strings.HasPrefix(rest, "<?"):
			end = "?>"
		case strings.HasPrefix(rest, "<!--"):
			end = "-->"
		default:
			after, ok := strings.CutPrefix(rest, "<speak")
			if !ok || after == "" {
				return false
			}
			switch after[0] {
			case '>', ' ', '\t', '\r', '\n', '/':
				return true
			}
			return false
		}
		_, after, found := strings.Cut(rest, end)
		if !found {
			return false
		}
		rest = strings.TrimSpace(after)
	}
}

// ToGoogleCloudTTSSpeechRequest maps a unified speech request onto the text:synthesize
// body: input → input.text (input.ssml for an SSML document), voice → voice.name,
// language_code → voice.languageCode, response_format → audioConfig.audioEncoding,
// speed → audioConfig.speakingRate. The request's model is the caller's price tier
// (wavenet, neural2, ...) and has no vendor field: the voice name selects the voice, and
// checkSpeechParams has checked that the voice is of that family.
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
