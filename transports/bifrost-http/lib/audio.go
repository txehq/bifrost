package lib

import (
	"bytes"
	"strconv"
	"strings"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

// HeaderBifrostUsage carries the unified usage of a response whose body is not Bifrost JSON:
// a non-streaming /v1/audio/speech response is the audio bytes themselves, so its
// schemas.SpeechUsage travels here as JSON (TXE-6530, contract C5). It is gateway-owned
// (x-bifrost-*), so ForwardProviderResponseHeaders never lets a vendor header set it.
const HeaderBifrostUsage = "x-bifrost-usage"

// SetSpeechUsageHeader writes usage as compact JSON into HeaderBifrostUsage. Nothing is written
// when usage is nil or cannot be encoded: the header carries what the provider reported, never
// a placeholder.
func SetSpeechUsageHeader(ctx *fasthttp.RequestCtx, usage *schemas.SpeechUsage) {
	if ctx == nil || usage == nil {
		return
	}
	encoded, err := schemas.MarshalSorted(usage)
	if err != nil {
		return
	}
	ctx.Response.Header.SetBytesV(HeaderBifrostUsage, encoded)
}

// WriteSpeechAudioResponse writes a non-streaming speech response whose body is the raw audio:
// Content-Type and the attachment filename follow the format of the audio actually returned
// (the provider's resp.ContentType, else SpeechContentType). Every OpenAI-shaped speech route
// writes its audio through here so they cannot drift apart. It does not write HeaderBifrostUsage:
// callers set that once with SetSpeechUsageHeader before their large-response shortcut, so the
// shortcut carries it too and the usage is serialized only once per response.
func WriteSpeechAudioResponse(ctx *fasthttp.RequestCtx, resp *schemas.BifrostSpeechResponse, responseFormat string) {
	contentType := resp.ContentType
	if contentType == "" {
		contentType = SpeechContentType(resp.Audio, responseFormat)
	}
	ctx.Response.Header.Set("Content-Type", contentType)
	ctx.Response.Header.Set("Content-Disposition", "attachment; filename="+speechFilenameForContentType(contentType, responseFormat))
	ctx.Response.Header.Set("Content-Length", strconv.Itoa(len(resp.Audio)))
	ctx.Response.SetBody(resp.Audio)
}

var (
	magicRIFF = []byte("RIFF")
	magicWAVE = []byte("WAVE")
	magicID3  = []byte("ID3")
	magicFLAC = []byte("fLaC")
	magicOGG  = []byte("OggS")
	magicFORM = []byte("FORM")
	magicAIFF = []byte("AIFF")
	magicAIFC = []byte("AIFC")
	magicADIF = []byte("ADIF")
	magicFTYP = []byte("ftyp")
)

// SpeechContentType returns the media type of audio. A container signature in the bytes wins
// (a provider such as Gemini may return WAV whatever format was asked for); raw formats with no
// signature (pcm, ulaw, alaw) take the type of the requested responseFormat, which may be
// OpenAI-style ("wav") or ElevenLabs-style ("pcm_16000"); a bare MPEG or ADTS frame sync is
// checked only after that, because raw PCM samples can look like one. With nothing to go on,
// an empty format means the API default mp3 and any other format is application/octet-stream.
func SpeechContentType(audio []byte, responseFormat string) string {
	if ct := audioContainerContentType(audio); ct != "" {
		return ct
	}
	if ct := speechFormatContentType(responseFormat); ct != "" {
		return ct
	}
	if len(audio) >= 2 && audio[0] == 0xFF {
		switch {
		case audio[1]&0xF6 == 0xF0: // ADTS: 12-bit sync, layer bits 00
			return "audio/aac"
		case audio[1]&0xE0 == 0xE0 && audio[1]&0x06 != 0: // MPEG audio frame, layer I-III
			return "audio/mpeg"
		}
	}
	if responseFormat == "" {
		return "audio/mpeg"
	}
	return "application/octet-stream"
}

func audioContainerContentType(audio []byte) string {
	switch {
	case len(audio) >= 12 && bytes.Equal(audio[:4], magicRIFF) && bytes.Equal(audio[8:12], magicWAVE):
		return "audio/wav"
	case bytes.HasPrefix(audio, magicID3):
		return "audio/mpeg"
	case bytes.HasPrefix(audio, magicFLAC):
		return "audio/flac"
	case bytes.HasPrefix(audio, magicOGG):
		return "audio/ogg"
	case len(audio) >= 12 && bytes.Equal(audio[:4], magicFORM) &&
		(bytes.Equal(audio[8:12], magicAIFF) || bytes.Equal(audio[8:12], magicAIFC)):
		return "audio/aiff"
	case bytes.HasPrefix(audio, magicADIF):
		return "audio/aac"
	case len(audio) >= 8 && bytes.Equal(audio[4:8], magicFTYP):
		return "audio/mp4"
	}
	return ""
}

func speechFormatContentType(responseFormat string) string {
	switch {
	case responseFormat == "":
		return ""
	case strings.HasPrefix(responseFormat, "mp3"):
		return "audio/mpeg"
	case strings.HasPrefix(responseFormat, "opus"):
		return "audio/ogg"
	case strings.HasPrefix(responseFormat, "aac"):
		return "audio/aac"
	case strings.HasPrefix(responseFormat, "flac"):
		return "audio/flac"
	case strings.HasPrefix(responseFormat, "wav"):
		return "audio/wav"
	case strings.HasPrefix(responseFormat, "pcmu"):
		// MiniMax pcmu_raw is headerless G.711 mu-law, not linear PCM (pcmu_wav is a WAV
		// container and is caught by the signature check before this).
		return "audio/basic"
	case strings.HasPrefix(responseFormat, "pcm"):
		return "audio/pcm"
	case strings.HasPrefix(responseFormat, "ulaw"):
		return "audio/basic"
	case strings.HasPrefix(responseFormat, "alaw"):
		return "audio/x-alaw-basic"
	}
	return ""
}

// SpeechAttachmentFilename derives the download filename from the requested audio format so
// non-MP3 responses aren't mislabeled as "speech.mp3". The format may be OpenAI-style ("opus",
// "wav", "flac", ...) or ElevenLabs-style with a codec/rate prefix ("mp3_22050_32",
// "pcm_16000", "ulaw_8000"). Unknown or empty formats fall back to mp3 (the API default).
func SpeechAttachmentFilename(responseFormat string) string {
	ext := "mp3"
	switch {
	case strings.HasPrefix(responseFormat, "opus"):
		ext = "opus"
	case strings.HasPrefix(responseFormat, "aac"):
		ext = "aac"
	case strings.HasPrefix(responseFormat, "flac"):
		ext = "flac"
	case strings.HasPrefix(responseFormat, "wav"):
		ext = "wav"
	case strings.HasPrefix(responseFormat, "pcm"):
		ext = "pcm"
	case strings.HasPrefix(responseFormat, "ulaw"):
		ext = "ulaw"
	case strings.HasPrefix(responseFormat, "alaw"):
		ext = "alaw"
	}
	return "speech." + ext
}

// speechFilenameForContentType names the attachment after the audio actually returned, so a WAV
// answer to an mp3 request is not called speech.mp3; types with no fixed extension fall back to
// the requested format.
func speechFilenameForContentType(contentType, responseFormat string) string {
	mediaType, _, _ := strings.Cut(contentType, ";")
	switch strings.ToLower(strings.TrimSpace(mediaType)) {
	case "audio/wav":
		return "speech.wav"
	case "audio/mpeg":
		return "speech.mp3"
	case "audio/flac":
		return "speech.flac"
	case "audio/aac":
		return "speech.aac"
	case "audio/aiff":
		return "speech.aiff"
	case "audio/mp4":
		return "speech.m4a"
	case "audio/ogg", "audio/opus":
		return "speech.opus"
	case "audio/pcm", "audio/l16":
		return "speech.pcm"
	case "audio/basic":
		return "speech.ulaw"
	}
	return SpeechAttachmentFilename(responseFormat)
}

// NonEmptyFormValues returns the non-empty values of a multipart form field, in the order the
// caller sent them. The transcription parsers store a field with one value as that value and a
// field the caller repeated (include[]=a&include[]=b) as the []string this returns, which
// openai.ParseTranscriptionFormDataBodyFromRequest writes back as repeated form fields, so no
// value is lost on the way to the vendor (TXE-6530, contract C4).
//
// The common case, a field with one non-empty value, allocates nothing: it returns a one-element
// sub-slice of values, which callers only read. A copy is made only for a field the caller
// repeated, the result a caller may keep.
func NonEmptyFormValues(values []string) []string {
	n, last := 0, -1
	for i, v := range values {
		if v != "" {
			n, last = n+1, i
		}
	}
	switch n {
	case 0:
		return nil
	case 1:
		return values[last : last+1 : last+1]
	}
	out := make([]string, 0, n)
	for _, v := range values {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}
