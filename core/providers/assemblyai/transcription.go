package assemblyai

import (
	"bytes"
	"fmt"
	"mime/multipart"
	"net/textproto"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/bytedance/sonic"
	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	schemas "github.com/capsohq/bifrost/core/schemas"
)

// assemblyAITranscriptionUnifiedFields are the unified transcription fields the Sync API
// takes (contract C4). A request carrying any other unified field is answered
// 400 caller_unsupported_parameter before a key is chosen.
var assemblyAITranscriptionUnifiedFields = []string{"file_format", "language", "prompt", "response_format", "timestamp_granularities"}

// assemblyAIConfigExtraParam is the extra field that carries a whole vendor config object,
// for a caller that wants its members sent exactly as written.
const assemblyAIConfigExtraParam = "config"

// ToAssemblyAITranscriptionRequest resolves a Bifrost transcription request into the Sync
// API's model header, audio part and config part.
//
// Unified fields: language becomes language_codes, prompt becomes prompt, a "word"
// timestamp granularity becomes timestamps, and file_format names the audio part's
// Content-Type when the audio has no signature. The Sync API returns JSON only and has no
// segment timings, so any other response_format or granularity is refused rather than
// dropped (contract C4).
//
// Extra fields are merged into the config unchanged. One that names a config member the
// unified fields already set is refused, never applied over it (the shared extra-params
// rule, TXE-7018). A multipart form can only carry strings, so a string is sent as the JSON value it spells
// (see assemblyAIConfigValue); the `config` extra field carries a whole config object.
func ToAssemblyAITranscriptionRequest(bifrostReq *schemas.BifrostTranscriptionRequest) (*AssemblyAITranscriptionRequest, *schemas.BifrostError) {
	if bifrostReq == nil || bifrostReq.Input == nil || len(bifrostReq.Input.File) == 0 {
		return nil, providerUtils.NewBifrostBadRequestError("transcription file is required")
	}
	req := &AssemblyAITranscriptionRequest{
		Model:    bifrostReq.Model,
		Audio:    bifrostReq.Input.File,
		Filename: bifrostReq.Input.Filename,
	}
	params := bifrostReq.Params
	fileFormat := ""
	if params != nil && params.Format != nil {
		fileFormat = *params.Format
	}
	req.AudioContentType = assemblyAIAudioContentType(bifrostReq.Input.File, bifrostReq.Input.Filename, fileFormat)
	if params == nil {
		return req, nil
	}
	if params.ResponseFormat != nil {
		switch *params.ResponseFormat {
		case "", "json", "verbose_json", "text":
		default:
			return nil, schemas.NewUnsupportedParameterError("response_format",
				fmt.Sprintf("response_format %q is not supported by assemblyai transcription: the Sync API returns JSON only", *params.ResponseFormat))
		}
	}
	for _, granularity := range params.TimestampGranularities {
		if granularity != "word" {
			return nil, schemas.NewUnsupportedParameterError("timestamp_granularities",
				fmt.Sprintf("timestamp granularity %q is not supported by assemblyai transcription: the Sync API reports word timings only", granularity))
		}
		req.Config.Timestamps = schemas.Ptr(true)
	}
	if params.Language != nil {
		if language := strings.TrimSpace(*params.Language); language != "" {
			req.Config.LanguageCodes = []string{language}
		}
	}
	if params.Prompt != nil && *params.Prompt != "" {
		req.Config.Prompt = params.Prompt
	}

	extraParams := make(map[string]interface{}, len(params.ExtraParams))
	if rawConfig, ok := params.ExtraParams[assemblyAIConfigExtraParam]; ok {
		configObject, err := assemblyAIConfigObject(rawConfig)
		if err != nil {
			return nil, providerUtils.NewBifrostBadRequestError(err.Error())
		}
		for name, value := range configObject {
			extraParams[name] = value
		}
	}
	for name, value := range params.ExtraParams {
		if name == assemblyAIConfigExtraParam {
			continue
		}
		extraParams[name] = assemblyAIConfigValue(name, value)
	}
	if len(extraParams) > 0 {
		req.Config.ExtraParams = extraParams
	}
	return req, nil
}

// assemblyAIConfigObject reads the `config` extra field: a JSON object, given as a map by a
// Go caller or as text by a multipart form.
func assemblyAIConfigObject(raw interface{}) (map[string]interface{}, error) {
	switch value := raw.(type) {
	case map[string]interface{}:
		return value, nil
	case string:
		var object map[string]interface{}
		if err := sonic.Unmarshal([]byte(value), &object); err != nil || object == nil {
			return nil, fmt.Errorf("the config field must be a JSON object")
		}
		return object, nil
	default:
		return nil, fmt.Errorf("the config field must be a JSON object")
	}
}

// assemblyAIStringConfigFields are the config members the vendor types as a string (or, for
// the last two, a string or an array of strings): their text is never read as a boolean or
// a number.
var assemblyAIStringConfigFields = map[string]bool{"prompt": true, "conversation_context": true, "language_codes": true}

// assemblyAIConfigValue is the JSON value sent for one extra field. Values that are not
// strings are sent as given. A string comes from a multipart form field, which cannot say
// whether "true" is a boolean, so it is sent as the JSON value it spells: true and false as
// booleans, an integer as a number, and text opening with [ or { as that array or object
// when it parses. Everything else, and every scalar of a string-typed member, stays a string.
func assemblyAIConfigValue(name string, value interface{}) interface{} {
	text, ok := value.(string)
	if !ok {
		return value
	}
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "[") || strings.HasPrefix(trimmed, "{") {
		var decoded interface{}
		if err := sonic.Unmarshal([]byte(trimmed), &decoded); err == nil {
			return decoded
		}
		return text
	}
	if assemblyAIStringConfigFields[name] {
		return text
	}
	switch trimmed {
	case "true":
		return true
	case "false":
		return false
	}
	if number, err := strconv.ParseInt(trimmed, 10, 64); err == nil {
		return number
	}
	return text
}

// assemblyAIConfigJSON encodes the config part, or nil when the config has no member: the
// part is optional and is left out rather than sent empty.
func assemblyAIConfigJSON(config *AssemblyAIConfig) ([]byte, error) {
	body, err := providerUtils.MarshalProviderRequest(config)
	if err != nil {
		return nil, err
	}
	if len(config.ExtraParams) > 0 {
		if body, err = providerUtils.MergeExtraParamsIntoJSON(body, config.ExtraParams); err != nil {
			return nil, err
		}
	}
	if bytes.Equal(bytes.TrimSpace(body), []byte("{}")) {
		return nil, nil
	}
	return body, nil
}

// writeAssemblyAITranscriptionMultipart writes the Sync API's multipart form: the `audio`
// part with the audio's own Content-Type, then the `config` part as application/json.
func writeAssemblyAITranscriptionMultipart(writer *multipart.Writer, req *AssemblyAITranscriptionRequest) *schemas.BifrostError {
	filename := req.Filename
	if filename == "" {
		filename = "audio"
	}
	// The config is built first: an extra field the shared rule refuses ends the request
	// before the audio is copied.
	configJSON, err := assemblyAIConfigJSON(&req.Config)
	if err != nil {
		if badRequest, ok := providerUtils.AsBifrostBadRequestError(err); ok {
			return badRequest
		}
		return providerUtils.NewBifrostOperationError(schemas.ErrProviderRequestMarshal, err)
	}

	audioHeader := textproto.MIMEHeader{}
	audioHeader.Set("Content-Disposition", fmt.Sprintf(`form-data; name="audio"; filename=%q`, filename))
	audioHeader.Set("Content-Type", req.AudioContentType)
	audioPart, err := writer.CreatePart(audioHeader)
	if err != nil {
		return providerUtils.NewBifrostOperationError("failed to create audio part", err)
	}
	if _, err := audioPart.Write(req.Audio); err != nil {
		return providerUtils.NewBifrostOperationError("failed to write audio data", err)
	}

	if configJSON == nil {
		return nil
	}
	configHeader := textproto.MIMEHeader{}
	configHeader.Set("Content-Disposition", `form-data; name="config"`)
	configHeader.Set("Content-Type", "application/json")
	configPart, err := writer.CreatePart(configHeader)
	if err != nil {
		return providerUtils.NewBifrostOperationError("failed to create config part", err)
	}
	if _, err := configPart.Write(configJSON); err != nil {
		return providerUtils.NewBifrostOperationError("failed to write config data", err)
	}
	return nil
}

// assemblyAIAudioExtensions maps a filename extension to the Content-Type the Sync API
// names for that format.
var assemblyAIAudioExtensions = map[string]string{
	".wav":  "audio/wav",
	".pcm":  "audio/pcm",
	".raw":  "audio/pcm",
	".aac":  "audio/aac",
	".mp3":  "audio/mpeg",
	".mpeg": "audio/mpeg",
	".m4a":  "audio/mp4",
	".mp4":  "audio/mp4",
	".ogg":  "audio/ogg",
	".oga":  "audio/ogg",
	".opus": "audio/ogg",
	".flac": "audio/flac",
	".webm": "audio/webm",
}

// assemblyAIAudioContentType is the Content-Type of the audio part, which the Sync API
// requires to match the format. A container signature decides first, then the request's
// file_format (a format name such as "wav" or "pcm", or a MIME type), then the filename's
// extension, then an MPEG or ADTS frame header. Headerless PCM can only be told by
// file_format or the extension. Audio that none of them identifies is sent as
// application/octet-stream, so the vendor answers for a format it does not take instead of
// Bifrost guessing one.
func assemblyAIAudioContentType(audio []byte, filename, fileFormat string) string {
	switch {
	case len(audio) >= 12 && bytes.Equal(audio[:4], []byte("RIFF")) && bytes.Equal(audio[8:12], []byte("WAVE")):
		return "audio/wav"
	case bytes.HasPrefix(audio, []byte("fLaC")):
		return "audio/flac"
	case bytes.HasPrefix(audio, []byte("OggS")):
		return "audio/ogg"
	case bytes.HasPrefix(audio, []byte{0x1A, 0x45, 0xDF, 0xA3}):
		return "audio/webm"
	case len(audio) >= 8 && bytes.Equal(audio[4:8], []byte("ftyp")):
		return "audio/mp4"
	case bytes.HasPrefix(audio, []byte("ID3")):
		return "audio/mpeg"
	case bytes.HasPrefix(audio, []byte("ADIF")):
		return "audio/aac"
	}
	if format := strings.ToLower(strings.TrimSpace(fileFormat)); format != "" {
		// "audio/wav" and "wav" name the same format; "audio/mp3" is the common spelling of audio/mpeg.
		if contentType, ok := assemblyAIAudioExtensions["."+strings.TrimPrefix(strings.TrimPrefix(format, "audio/"), ".")]; ok {
			return contentType
		}
		for _, contentType := range assemblyAIAudioExtensions {
			if contentType == format {
				return contentType
			}
		}
	}
	if contentType, ok := assemblyAIAudioExtensions[strings.ToLower(filepath.Ext(filename))]; ok {
		return contentType
	}
	if len(audio) >= 2 && audio[0] == 0xFF {
		switch {
		case audio[1]&0xF6 == 0xF0:
			return "audio/aac"
		case audio[1]&0xE0 == 0xE0:
			return "audio/mpeg"
		}
	}
	return "application/octet-stream"
}

// ToBifrostTranscriptionResponse converts a Sync API response to Bifrost format.
//
// audio_duration_ms / 1000 is the billable duration: it fills duration and
// usage{type: "duration", seconds}, and both stay empty when the vendor did not report it
// (contract C5). Word times are milliseconds on the wire and seconds in the unified words.
// The unified words carry timings, so they are filled only when the vendor returned
// timings; a word the vendor could not align keeps its place with zero times. The vendor's
// own word list, confidences included, is in the raw response.
func ToBifrostTranscriptionResponse(resp *AssemblyAITranscriptionResponse) *schemas.BifrostTranscriptionResponse {
	out := &schemas.BifrostTranscriptionResponse{
		Text: resp.Text,
		Task: schemas.Ptr("transcribe"),
	}
	if resp.AudioDurationMS != nil {
		seconds := *resp.AudioDurationMS / 1000
		out.Duration = schemas.Ptr(seconds)
		out.Usage = &schemas.TranscriptionUsage{Type: "duration", Seconds: schemas.Ptr(seconds)}
	}
	timed := false
	for _, word := range resp.Words {
		if word.Start != nil && word.End != nil {
			timed = true
			break
		}
	}
	if !timed {
		return out
	}
	out.Words = make([]schemas.TranscriptionWord, 0, len(resp.Words))
	for _, word := range resp.Words {
		unified := schemas.TranscriptionWord{Word: word.Text}
		if word.Start != nil && word.End != nil {
			unified.Start = *word.Start / 1000
			unified.End = *word.End / 1000
		}
		out.Words = append(out.Words, unified)
	}
	return out
}
