package googlecloudtts

import (
	"encoding/xml"
	"fmt"
	"io"
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
	if !sameFamily(family, model) {
		return schemas.NewUnsupportedParameterError("voice", fmt.Sprintf("voice %q is a %s voice and the request's model is %q: the model must name the voice's family", voiceName, strings.ToLower(family), model))
	}
	return nil
}

// ssmlVoiceTag is the start of an SSML <voice> element, which reads its content in another
// voice (https://docs.cloud.google.com/text-to-speech/docs/ssml#select_a_voice).
const ssmlVoiceTag = "<voice"

// ssmlPrefixedVoiceTag is the tail of a <voice> element written with a namespace prefix
// (<s:voice>).
const ssmlPrefixedVoiceTag = ":voice"

// ssmlDoctype starts a document type declaration, which can define an entity that expands
// to markup: a <voice> element the text of the document never spells.
const ssmlDoctype = "<!doctype"

// checkSSMLVoices refuses an SSML document that selects a voice the request's model does not
// cover. Google lets a <voice name="..."> element switch family inside one request (a Studio
// voice inside a Standard request), so the voice parameter alone does not bind the family
// that is served.
//
// A document that spells none of "<voice", ":voice" and "<!doctype", in any letter case, has
// no <voice> element under any reading and passes without being parsed or allocating. Any other
// document is read with an XML tokenizer, so comments, CDATA sections and attribute values
// are not mistaken for elements or the other way round, and then:
//   - every <voice> element must carry a name of the model's family; one that selects its
//     voice another way (gender, language, variant) is refused, because its family cannot
//     be established;
//   - a document the tokenizer cannot read to the end, or one with a document type
//     declaration, is refused: what Google would make of it cannot be established either.
func checkSSMLVoices(model, ssml string) *schemas.BifrostError {
	if indexFoldASCII(ssml, ssmlVoiceTag) < 0 && indexFoldASCII(ssml, ssmlPrefixedVoiceTag) < 0 && indexFoldASCII(ssml, ssmlDoctype) < 0 {
		return nil
	}
	decoder := xml.NewDecoder(strings.NewReader(ssml))
	for {
		token, err := decoder.RawToken()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return ssmlVoiceError(model, "the SSML names a <voice> element or a document type and is not well-formed XML")
		}
		switch element := token.(type) {
		case xml.Directive:
			return ssmlVoiceError(model, "the SSML carries a document type declaration")
		case xml.StartElement:
			if !equalFoldASCII(element.Name.Local, "voice") {
				continue
			}
			named := false
			for _, attr := range element.Attr {
				if !equalFoldASCII(attr.Name.Local, "name") {
					continue
				}
				named = true
				family, ok := voiceFamily(attr.Value)
				if !ok {
					return ssmlVoiceError(model, fmt.Sprintf("<voice name=%q> is not a Cloud Text-to-Speech voice name of the form <language>-<REGION>-<family>-<variant>", attr.Value))
				}
				if !sameFamily(family, model) {
					return ssmlVoiceError(model, fmt.Sprintf("<voice name=%q> selects a %s voice", attr.Value, strings.ToLower(family)))
				}
			}
			if !named {
				return ssmlVoiceError(model, "a <voice> element selects its voice without a name")
			}
		}
	}
}

func ssmlVoiceError(model, what string) *schemas.BifrostError {
	return schemas.NewUnsupportedParameterError("input", fmt.Sprintf("%s and the request's model is %q: every <voice> element in the SSML must name a voice of the model's family", what, model))
}

// indexFoldASCII returns the index of the first occurrence of the lower-case ASCII needle in
// s, comparing ASCII letters without regard to case, or -1.
func indexFoldASCII(s, needle string) int {
	for i := 0; i+len(needle) <= len(s); i++ {
		if equalFoldASCII(s[i:i+len(needle)], needle) {
			return i
		}
	}
	return -1
}

// languageCodeField is the unified name of voice.languageCode. The speech route parses it
// into the typed parameter and also leaves it among the extra params under its own name.
const languageCodeField = "language_code"

// vendorExtraParams returns the extra params to merge into the text:synthesize body: all of
// them except language_code, which the provider has already mapped to voice.languageCode and
// which is not a text:synthesize field. The map is copied only when it carries that key.
func vendorExtraParams(extraParams map[string]interface{}) map[string]interface{} {
	if _, ok := extraParams[languageCodeField]; !ok {
		return extraParams
	}
	vendor := make(map[string]interface{}, len(extraParams)-1)
	for key, value := range extraParams {
		if key != languageCodeField {
			vendor[key] = value
		}
	}
	return vendor
}

// protectedBodyFields are the text:synthesize objects the provider checks before sending:
// voice (the family) and input (the SSML voices). The extra-params channel merges caller
// fields into the body after those checks, so it may not carry either.
var protectedBodyFields = [...]string{"voice", "input"}

// checkSpeechParams refuses the values text:synthesize cannot take as given, each with
// caller_unsupported_parameter naming the field. Nothing is dropped, rewritten or clamped.
// Fields the provider does not take at all are refused by core from SupportedUnifiedFields.
func checkSpeechParams(model string, params *schemas.SpeechParameters) *schemas.BifrostError {
	if params == nil {
		return checkVoiceFamily(model, nil)
	}
	for _, field := range protectedBodyFields {
		if _, ok := params.ExtraParams[field]; ok {
			return schemas.NewUnsupportedParameterError(field, fmt.Sprintf("%s cannot be set as a vendor-specific field on google-cloud-tts: the provider checks it against the request's model before sending, so it is taken only from the request's own %s", field, field))
		}
	}
	if params.VoiceConfig != nil && len(params.VoiceConfig.MultiVoiceConfig) > 0 {
		return schemas.NewUnsupportedParameterError("voice", "voice must be one voice name: a multi-speaker voice list is not supported by google-cloud-tts")
	}
	// audioConfig is the one object extra params may add to, but not its audioEncoding: the
	// response's Content-Type and file name follow response_format, and headerless PCM
	// returned under an mp3 request cannot be told from its bytes.
	if audioConfig, ok := params.ExtraParams["audioConfig"].(map[string]interface{}); ok {
		if _, ok := audioConfig["audioEncoding"]; ok {
			return schemas.NewUnsupportedParameterError("response_format", "audioConfig.audioEncoding cannot be set as a vendor-specific field on google-cloud-tts: the audio encoding is chosen with response_format, which also labels the response")
		}
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
	synthesizeReq.ExtraParams = vendorExtraParams(params.ExtraParams)

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

// sameFamily reports whether a voice's family is the request's model. Only ASCII letters are
// compared without regard to case, and a name holding any other byte is never the same:
// Unicode folding would take "ſtandard" (long s) for "standard", while the governance and
// pricing lookups, which lower-case the model, would not, so the request would be served
// outside the limits and the price of the model it is.
func sameFamily(family, model string) bool {
	return equalFoldASCII(family, model)
}

// equalFoldASCII reports whether a and b are the same ASCII string, comparing letters without
// regard to case. A string with a byte outside ASCII equals nothing.
func equalFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if x >= 0x80 || y >= 0x80 {
			return false
		}
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}
