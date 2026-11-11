package azurespeech

import (
	"bytes"
	"encoding/xml"
	"regexp"
	"strconv"
	"strings"

	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	schemas "github.com/capsohq/bifrost/core/schemas"
)

const (
	// fastTranscriptionAPIVersion is the api-version of the fast transcription API that
	// carries enhancedMode (MAI-Transcribe).
	fastTranscriptionAPIVersion = "2025-10-15"

	// speechUserAgent is sent on every request: the text to speech REST API lists
	// User-Agent as a required header.
	speechUserAgent = "bifrost"

	ssmlContentType       = "application/ssml+xml"
	outputFormatHeader    = "X-Microsoft-OutputFormat"
	subscriptionKeyHeader = "Ocp-Apim-Subscription-Key"

	// defaultSpeechLocale is the xml:lang of the SSML document when the caller gave no
	// language_code and the voice name does not start with a locale.
	defaultSpeechLocale = "en-US"

	// minSpeechRate and maxSpeechRate bound <prosody rate>: the SSML reference allows a
	// relative rate of 0.5 to 2 times the original audio.
	minSpeechRate = 0.5
	maxSpeechRate = 2.0
)

// regionPattern matches an Azure region name (eastus, swedencentral, westus3). A location
// that matches is a region; anything else is a resource endpoint.
var regionPattern = regexp.MustCompile(`^[a-z][a-z0-9]*$`)

// speechLocation returns where the key's Speech resource lives: the key's
// azure_key_config.endpoint when set, else the provider's network_config.base_url. Either
// holds a region (eastus) or the resource endpoint
// (https://<resource>.cognitiveservices.azure.com).
func (provider *AzureSpeechProvider) speechLocation(key schemas.Key) (string, *schemas.BifrostError) {
	location := ""
	if key.AzureKeyConfig != nil {
		location = strings.TrimSpace(key.AzureKeyConfig.Endpoint.GetValue())
	}
	if location == "" {
		location = strings.TrimSpace(provider.networkConfig.BaseURL)
	}
	if location == "" {
		return "", providerUtils.NewConfigurationError("azurespeech needs a region or a resource endpoint: set azure_key_config.endpoint on the key, or network_config.base_url on the provider")
	}
	return strings.TrimRight(location, "/"), nil
}

// isRegion reports whether location names an Azure region instead of an endpoint URL.
func isRegion(location string) bool {
	return regionPattern.MatchString(strings.ToLower(location))
}

// endpointBase returns location as a URL prefix, adding https:// to a bare host name.
func endpointBase(location string) string {
	if strings.Contains(location, "://") {
		return location
	}
	return "https://" + location
}

// speechSynthesisURL is the text to speech endpoint for location. A region uses the
// regional host; a resource endpoint (custom domain) serves the same API under /tts.
func speechSynthesisURL(location string) string {
	if isRegion(location) {
		return "https://" + strings.ToLower(location) + ".tts.speech.microsoft.com/cognitiveservices/v1"
	}
	return endpointBase(location) + "/tts/cognitiveservices/v1"
}

// transcriptionURL is the fast transcription endpoint for location.
func transcriptionURL(location string) string {
	base := endpointBase(location)
	if isRegion(location) {
		base = "https://" + strings.ToLower(location) + ".api.cognitive.microsoft.com"
	}
	return base + "/speechtotext/transcriptions:transcribe?api-version=" + fastTranscriptionAPIVersion
}

// unifiedOutputFormats maps the unified response_format values to the
// X-Microsoft-OutputFormat value sent for them. Azure AI Speech has no aac or flac output.
var unifiedOutputFormats = map[string]string{
	"mp3":  "audio-24khz-48kbitrate-mono-mp3",
	"wav":  "riff-24khz-16bit-mono-pcm",
	"pcm":  "raw-24khz-16bit-mono-pcm",
	"opus": "ogg-24khz-16bit-mono-opus",
}

// nativeOutputFormats are the X-Microsoft-OutputFormat values the text to speech REST API
// documents. A caller that names one in response_format gets exactly that format.
var nativeOutputFormats = map[string]struct{}{
	"amr-wb-16000hz":                     {},
	"audio-16khz-16bit-32kbps-mono-opus": {},
	"audio-16khz-32kbitrate-mono-mp3":    {},
	"audio-16khz-64kbitrate-mono-mp3":    {},
	"audio-16khz-128kbitrate-mono-mp3":   {},
	"audio-24khz-16bit-24kbps-mono-opus": {},
	"audio-24khz-16bit-48kbps-mono-opus": {},
	"audio-24khz-48kbitrate-mono-mp3":    {},
	"audio-24khz-96kbitrate-mono-mp3":    {},
	"audio-24khz-160kbitrate-mono-mp3":   {},
	"audio-48khz-96kbitrate-mono-mp3":    {},
	"audio-48khz-192kbitrate-mono-mp3":   {},
	"g722-16khz-64kbps":                  {},
	"ogg-16khz-16bit-mono-opus":          {},
	"ogg-24khz-16bit-mono-opus":          {},
	"ogg-48khz-16bit-mono-opus":          {},
	"raw-8khz-8bit-mono-alaw":            {},
	"raw-8khz-8bit-mono-mulaw":           {},
	"raw-8khz-16bit-mono-pcm":            {},
	"raw-16khz-16bit-mono-pcm":           {},
	"raw-16khz-16bit-mono-truesilk":      {},
	"raw-22050hz-16bit-mono-pcm":         {},
	"raw-24khz-16bit-mono-pcm":           {},
	"raw-24khz-16bit-mono-truesilk":      {},
	"raw-44100hz-16bit-mono-pcm":         {},
	"raw-48khz-16bit-mono-pcm":           {},
	"webm-16khz-16bit-mono-opus":         {},
	"webm-24khz-16bit-24kbps-mono-opus":  {},
	"webm-24khz-16bit-mono-opus":         {},
	"riff-8khz-8bit-mono-alaw":           {},
	"riff-8khz-8bit-mono-mulaw":          {},
	"riff-8khz-16bit-mono-pcm":           {},
	"riff-22050hz-16bit-mono-pcm":        {},
	"riff-24khz-16bit-mono-pcm":          {},
	"riff-44100hz-16bit-mono-pcm":        {},
	"riff-48khz-16bit-mono-pcm":          {},
}

// speechOutputFormat returns the X-Microsoft-OutputFormat value for a unified
// response_format: mp3 when none is given (the unified default), the mapped format for
// mp3, wav, pcm and opus, and a documented Azure format name as given. Any other value is
// refused, never replaced by another format.
func speechOutputFormat(responseFormat string) (string, *schemas.BifrostError) {
	format := strings.ToLower(strings.TrimSpace(responseFormat))
	if format == "" {
		format = "mp3"
	}
	if mapped, ok := unifiedOutputFormats[format]; ok {
		return mapped, nil
	}
	if _, ok := nativeOutputFormats[format]; ok {
		return format, nil
	}
	return "", schemas.NewUnsupportedParameterError("response_format",
		"response_format "+strconv.Quote(responseFormat)+" is not an Azure AI Speech output format: use mp3, wav, pcm, opus or an X-Microsoft-OutputFormat value")
}

// voiceLocale returns the locale a voice name starts with (en-US for
// en-US-Harper:MAI-Voice-2.1-Flash), or "" when the name does not start with one.
func voiceLocale(voice string) string {
	parts := strings.SplitN(voice, "-", 3)
	if len(parts) < 3 {
		return ""
	}
	language, region := parts[0], parts[1]
	if len(language) < 2 || len(language) > 3 || len(region) < 2 || len(region) > 4 {
		return ""
	}
	return language + "-" + region
}

// writeXMLEscaped writes s with the XML special characters escaped, for use as text or as
// an attribute value. xml.EscapeText only fails when the writer does, and a bytes.Buffer
// never does.
func writeXMLEscaped(buf *bytes.Buffer, s string) {
	_ = xml.EscapeText(buf, []byte(s))
}

// ToAzureSpeechSSML builds the SSML document Azure AI Speech synthesizes for a unified
// speech request, and the X-Microsoft-OutputFormat value that goes with it. The voice name
// selects the model (Azure AI Speech has no model field): it is sent exactly as given.
// input is always sent as text, escaped; speed becomes <prosody rate>; language_code
// becomes the document's xml:lang. A value Azure AI Speech cannot take as given returns a
// caller_unsupported_parameter error.
func ToAzureSpeechSSML(request *schemas.BifrostSpeechRequest) (ssml []byte, outputFormat string, bifrostErr *schemas.BifrostError) {
	if request == nil || request.Input == nil || request.Input.Input == "" {
		return nil, "", providerUtils.NewBifrostBadRequestError("speech input text is required")
	}
	params := request.Params
	if params == nil || params.VoiceConfig == nil {
		return nil, "", providerUtils.NewBifrostBadRequestError("voice parameter is required: an Azure AI Speech voice name such as en-US-Harper:MAI-Voice-2.1-Flash")
	}
	if len(params.VoiceConfig.MultiVoiceConfig) > 0 {
		return nil, "", schemas.NewUnsupportedParameterError("voice", "azurespeech takes one voice name; a multi-speaker voice configuration is not supported")
	}
	if params.VoiceConfig.Voice == nil || strings.TrimSpace(*params.VoiceConfig.Voice) == "" {
		return nil, "", providerUtils.NewBifrostBadRequestError("voice parameter is required: an Azure AI Speech voice name such as en-US-Harper:MAI-Voice-2.1-Flash")
	}
	voice := strings.TrimSpace(*params.VoiceConfig.Voice)

	outputFormat, bifrostErr = speechOutputFormat(params.ResponseFormat)
	if bifrostErr != nil {
		return nil, "", bifrostErr
	}

	rate := ""
	if params.Speed != nil {
		speed := *params.Speed
		// The negated form also refuses NaN.
		if !(speed >= minSpeechRate && speed <= maxSpeechRate) {
			return nil, "", schemas.NewUnsupportedParameterError("speed",
				"speed "+strconv.FormatFloat(speed, 'f', -1, 64)+" is outside the range Azure AI Speech accepts for <prosody rate> (0.5 to 2)")
		}
		rate = strconv.FormatFloat(speed, 'f', -1, 64)
	}

	locale := ""
	if params.LanguageCode != nil {
		locale = strings.TrimSpace(*params.LanguageCode)
	}
	if locale == "" {
		locale = voiceLocale(voice)
	}
	if locale == "" {
		locale = defaultSpeechLocale
	}

	var buf bytes.Buffer
	buf.Grow(len(request.Input.Input) + len(voice) + 192)
	buf.WriteString(`<speak version="1.0" xmlns="http://www.w3.org/2001/10/synthesis" xml:lang="`)
	writeXMLEscaped(&buf, locale)
	buf.WriteString(`"><voice name="`)
	writeXMLEscaped(&buf, voice)
	buf.WriteString(`">`)
	if rate != "" {
		buf.WriteString(`<prosody rate="`)
		buf.WriteString(rate)
		buf.WriteString(`">`)
	}
	writeXMLEscaped(&buf, request.Input.Input)
	if rate != "" {
		buf.WriteString(`</prosody>`)
	}
	buf.WriteString(`</voice></speak>`)
	return buf.Bytes(), outputFormat, nil
}
