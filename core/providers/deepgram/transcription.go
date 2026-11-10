package deepgram

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/bytedance/sonic"
	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	schemas "github.com/capsohq/bifrost/core/schemas"
)

// deepgramSupportedTranscriptionFields are the unified transcription fields Deepgram
// takes: language as the language query parameter, and response_format when it is json
// (Deepgram answers JSON only; any other value is refused in the converter). prompt is
// exempt from the check (schemas.UnpricedUnifiedFields) and is not sent: Deepgram has no
// prompt. A request carrying any other unified field is refused before a key is chosen.
var deepgramSupportedTranscriptionFields = []string{"language", "response_format"}

// deepgramAudioTypesByExtension names the media type for the file extensions Deepgram
// documents. A filename outside this map falls back to detection from the file header.
var deepgramAudioTypesByExtension = map[string]string{
	".aac":  "audio/aac",
	".aiff": "audio/aiff",
	".amr":  "audio/amr",
	".flac": "audio/flac",
	".m4a":  "audio/mp4",
	".mp2":  "audio/mpeg",
	".mp3":  "audio/mpeg",
	".mp4":  "audio/mp4",
	".ogg":  "audio/ogg",
	".opus": "audio/opus",
	".wav":  "audio/wav",
	".webm": "audio/webm",
}

// ToDeepgramTranscriptionRequest converts a Bifrost transcription request into Deepgram's
// POST /v1/listen request.
//
// Query parameters: model, the unified language, and every extra param under its own name
// and value, unchanged (contract C4: Deepgram prices options such as diarize, redact and
// keyterm separately, and Tokun bills what was sent). A repeated field is sent once per
// value. Keys are sorted so the URL is stable.
//
// Body: the audio bytes, or {"url": ...} when the request carries an audio URL and no
// file; Bifrost does not download the URL.
//
// Refused with 400 caller_unsupported_parameter, never dropped or rewritten: a
// response_format other than json; callback and callback_method (Deepgram would answer
// with a request id and deliver the transcript elsewhere, so there would be no usage to
// report); model and language as extra params (they are unified fields); the other unified
// transcription fields when they arrive as extra params (temperature, include,
// timestamp_granularities, response_format, prompt), which is how the native route delivers
// the ones it does not parse; a url next to a file; an extra param whose value is not a
// string, number, boolean or a list of them.
func ToDeepgramTranscriptionRequest(request *schemas.BifrostTranscriptionRequest) (*DeepgramTranscriptionRequest, *schemas.BifrostError) {
	if request == nil {
		return nil, providerUtils.NewBifrostOperationError("transcription request is nil", nil)
	}
	if strings.TrimSpace(request.Model) == "" {
		return nil, providerUtils.NewBifrostOperationError("transcription model is required", nil)
	}

	query := url.Values{}
	query.Set("model", request.Model)

	var extraParams map[string]interface{}
	if params := request.Params; params != nil {
		if format := params.ResponseFormat; format != nil && *format != "" && *format != "json" {
			return nil, schemas.NewUnsupportedParameterError("response_format",
				fmt.Sprintf("deepgram answers JSON only; response_format %q cannot be delivered", *format))
		}
		if params.Language != nil && *params.Language != "" {
			query.Set("language", *params.Language)
		}
		extraParams = params.ExtraParams
	}

	hasFile := request.Input != nil && len(request.Input.File) > 0
	audioURL, hasURL := request.AudioURL()
	if hasFile && hasURL {
		return nil, schemas.NewUnsupportedParameterError(schemas.TranscriptionURLParam,
			"send the audio as file or as url, not both")
	}
	if !hasFile && !hasURL {
		return nil, providerUtils.NewBifrostOperationError("transcription input not provided: send file or url", nil)
	}

	for key, value := range extraParams {
		switch key {
		case schemas.TranscriptionURLParam:
			// Carried in the body, below.
			continue
		case "model", "language":
			return nil, schemas.NewUnsupportedParameterError(key,
				fmt.Sprintf("%s is a unified field; send it as %s, not as an extra param", key, key))
		case "temperature", "include", "include[]", "timestamp_granularities", "timestamp_granularities[]", "response_format", "prompt":
			// The native route reads only some unified fields from the form and leaves the
			// rest in the extra params; they are the same fields under the same names, and
			// Deepgram takes none of them, so they are refused here as the declaration
			// refuses their unified form.
			return nil, schemas.NewUnsupportedParameterError(strings.TrimSuffix(key, "[]"),
				fmt.Sprintf("%s is an OpenAI transcription field that deepgram does not take", strings.TrimSuffix(key, "[]")))
		case "callback", "callback_method":
			return nil, schemas.NewUnsupportedParameterError(key,
				"deepgram callbacks are not supported: the transcript must come back on this request")
		}
		values, ok := deepgramQueryValues(value)
		if !ok {
			return nil, schemas.NewUnsupportedParameterError(key,
				fmt.Sprintf("extra param %q cannot be sent as a query parameter: its value must be a string, a number, a boolean or a list of them", key))
		}
		for _, v := range values {
			query.Add(key, v)
		}
	}

	out := &DeepgramTranscriptionRequest{Query: query.Encode()}
	if hasURL {
		body, err := sonic.Marshal(DeepgramURLBody{URL: audioURL})
		if err != nil {
			return nil, providerUtils.NewBifrostOperationError(schemas.ErrProviderRequestMarshal, err)
		}
		out.Body, out.ContentType, out.IsURL = body, "application/json", true
		return out, nil
	}
	out.Body = request.Input.File
	out.ContentType = deepgramAudioContentType(request.Input.Filename, request.Input.File)
	return out, nil
}

// deepgramQueryValues renders an extra param as query values: one for a scalar, one per
// element for a list. ok is false for a value with no query form (an object, a nested
// list, null).
func deepgramQueryValues(value interface{}) (values []string, ok bool) {
	switch v := value.(type) {
	case []string:
		return v, true
	case []interface{}:
		values = make([]string, 0, len(v))
		for _, element := range v {
			s, ok := deepgramQueryScalar(element)
			if !ok {
				return nil, false
			}
			values = append(values, s)
		}
		return values, true
	}
	s, ok := deepgramQueryScalar(value)
	if !ok {
		return nil, false
	}
	return []string{s}, true
}

// deepgramQueryScalar renders a string, boolean or number as it is written in a query.
func deepgramQueryScalar(value interface{}) (string, bool) {
	switch v := value.(type) {
	case string:
		return v, true
	case bool:
		return strconv.FormatBool(v), true
	case json.Number:
		return v.String(), true
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), true
	case float32:
		return strconv.FormatFloat(float64(v), 'f', -1, 32), true
	case int:
		return strconv.Itoa(v), true
	case int64:
		return strconv.FormatInt(v, 10), true
	case int32:
		return strconv.FormatInt(int64(v), 10), true
	}
	return "", false
}

// deepgramAudioContentType names the audio's media type from the filename's extension,
// else from the file header. Deepgram reads the container itself; the header only has to
// say the body is audio and not the JSON URL form.
func deepgramAudioContentType(filename string, audio []byte) string {
	if contentType, ok := deepgramAudioTypesByExtension[strings.ToLower(filepath.Ext(filename))]; ok {
		return contentType
	}
	return providerUtils.DetectAudioMimeType(audio)
}

// ToBifrostTranscriptionResponse converts Deepgram's response into the unified one.
//
// text is the first alternative's transcript of each channel, joined with a newline (one
// channel unless the request set multichannel=true).
//
// usage is {type: "duration", seconds: metadata.duration × metadata.channels}: Deepgram
// bills every channel of a multichannel request for the audio's full duration, and the
// unified usage has one seconds field that settlement reads. When Deepgram reports no
// duration or no channel count, usage stays empty (contract C5: a quantity the vendor did
// not report is not filled in).
//
// It returns nil when the body carries no results, which a 200 from a sync request never
// does; the caller answers provider_empty_result.
func ToBifrostTranscriptionResponse(response *DeepgramTranscriptionResponse) *schemas.BifrostTranscriptionResponse {
	if response == nil || response.Results == nil {
		return nil
	}
	out := &schemas.BifrostTranscriptionResponse{}

	channels := response.Results.Channels
	switch len(channels) {
	case 0:
	case 1:
		out.Text = channels[0].transcript()
	default:
		transcripts := make([]string, 0, len(channels))
		for i := range channels {
			transcripts = append(transcripts, channels[i].transcript())
		}
		out.Text = strings.Join(transcripts, "\n")
	}
	if len(channels) > 0 && channels[0].DetectedLanguage != nil && *channels[0].DetectedLanguage != "" {
		out.Language = channels[0].DetectedLanguage
	}

	if metadata := response.Metadata; metadata != nil && metadata.Duration != nil && metadata.Channels != nil && *metadata.Channels > 0 {
		seconds := *metadata.Duration * float64(*metadata.Channels)
		out.Usage = &schemas.TranscriptionUsage{Type: "duration", Seconds: &seconds}
	}
	return out
}

// transcript returns the channel's first alternative, the one Deepgram ranks highest.
func (c *DeepgramChannel) transcript() string {
	if len(c.Alternatives) == 0 {
		return ""
	}
	return c.Alternatives[0].Transcript
}
