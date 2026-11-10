package azurespeech

import (
	"mime/multipart"
	"strconv"
	"strings"

	"github.com/bytedance/sonic"
	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	schemas "github.com/capsohq/bifrost/core/schemas"
)

// maiTranscribePrefix is the lower-case start of the MAI-Transcribe model names.
const maiTranscribePrefix = "mai-transcribe"

// transcriptionModelName returns the name sent in definition.enhancedMode.model. The
// unified model ids are lower case (mai-transcribe-2); the vendor documents them as
// MAI-Transcribe-2, so the documented casing is restored. Any other model is sent as given.
func transcriptionModelName(model string) string {
	model = strings.TrimSpace(model)
	if len(model) >= len(maiTranscribePrefix) && strings.EqualFold(model[:len(maiTranscribePrefix)], maiTranscribePrefix) {
		return "MAI-Transcribe" + model[len(maiTranscribePrefix):]
	}
	return model
}

// transcriptionTimestamps maps the unified timestamp_granularities to the one value
// enhancedMode.modelOptions.timestamps takes. "word" carries every word's timing inside
// its phrase, so a request for both word and segment is sent as "word".
func transcriptionTimestamps(granularities []string) (string, *schemas.BifrostError) {
	timestamps := ""
	for _, granularity := range granularities {
		switch strings.ToLower(strings.TrimSpace(granularity)) {
		case "word":
			timestamps = "word"
		case "segment":
			if timestamps == "" {
				timestamps = "segment"
			}
		default:
			return "", schemas.NewUnsupportedParameterError("timestamp_granularities",
				"timestamp_granularities value "+strconv.Quote(granularity)+" is not supported by azurespeech: use word or segment")
		}
	}
	return timestamps, nil
}

// decodeExtraParam returns an extra parameter as the JSON value it stands for. A multipart
// form carries every field as a string, so an object or array arrives JSON-encoded
// ({"enabled":true}); it is decoded so it lands in the definition as an object, not as a
// string. Anything else is returned unchanged.
func decodeExtraParam(value interface{}) interface{} {
	text, ok := value.(string)
	if !ok {
		return value
	}
	trimmed := strings.TrimSpace(text)
	if len(trimmed) < 2 || (trimmed[0] != '{' && trimmed[0] != '[') {
		return value
	}
	var decoded interface{}
	if err := sonic.UnmarshalString(trimmed, &decoded); err != nil {
		return value
	}
	return decoded
}

// mergeDefinition merges src into dst: an object is merged key by key, any other value
// replaces what dst holds.
func mergeDefinition(dst, src map[string]interface{}) {
	for key, value := range src {
		if srcMap, ok := value.(map[string]interface{}); ok {
			if dstMap, ok := dst[key].(map[string]interface{}); ok {
				mergeDefinition(dstMap, srcMap)
				continue
			}
		}
		dst[key] = value
	}
}

// ToAzureSpeechTranscriptionRequest converts a unified transcription request into a fast
// transcription call. The model goes to definition.enhancedMode, language to
// definition.locales, timestamp_granularities to enhancedMode.modelOptions.timestamps, and
// response_format diarized_json turns on diarization. When passthroughExtraParams is set
// (x-bf-passthrough-extra-params), fields outside the unified structure are merged into
// the definition under their own names (diarization, phraseList, enhancedMode, channels,
// profanityFilterMode), objects recursively. A value the API cannot take as given returns
// a caller_unsupported_parameter error.
func ToAzureSpeechTranscriptionRequest(request *schemas.BifrostTranscriptionRequest, passthroughExtraParams bool) (*AzureSpeechTranscriptionRequest, *schemas.BifrostError) {
	if request == nil || request.Input == nil || len(request.Input.File) == 0 {
		return nil, providerUtils.NewBifrostBadRequestError("transcription file is required")
	}

	enhancedMode := map[string]interface{}{
		"enabled": true,
		"model":   transcriptionModelName(request.Model),
	}
	definition := map[string]interface{}{"enhancedMode": enhancedMode}

	if params := request.Params; params != nil {
		if params.ResponseFormat != nil {
			switch strings.ToLower(strings.TrimSpace(*params.ResponseFormat)) {
			case "", "json", "verbose_json":
			case "diarized_json":
				definition["diarization"] = map[string]interface{}{"enabled": true}
			default:
				// The API answers JSON only; Bifrost does not render text, srt or vtt from it.
				return nil, schemas.NewUnsupportedParameterError("response_format",
					"response_format "+strconv.Quote(*params.ResponseFormat)+" is not supported by azurespeech: use json, verbose_json or diarized_json")
			}
		}
		if params.Language != nil && strings.TrimSpace(*params.Language) != "" {
			definition["locales"] = []interface{}{strings.TrimSpace(*params.Language)}
		}
		timestamps, bifrostErr := transcriptionTimestamps(params.TimestampGranularities)
		if bifrostErr != nil {
			return nil, bifrostErr
		}
		if timestamps != "" {
			enhancedMode["modelOptions"] = map[string]interface{}{"timestamps": timestamps}
		}
		if passthroughExtraParams && len(params.ExtraParams) > 0 {
			extra := make(map[string]interface{}, len(params.ExtraParams))
			for key, value := range params.ExtraParams {
				extra[key] = decodeExtraParam(value)
			}
			mergeDefinition(definition, extra)
		}
	}

	return &AzureSpeechTranscriptionRequest{
		File:       request.Input.File,
		Filename:   request.Input.Filename,
		Definition: definition,
	}, nil
}

// writeTranscriptionMultipart writes the two parts of a fast transcription request: the
// audio file and the definition JSON (keys sorted, so the body is deterministic).
func writeTranscriptionMultipart(writer *multipart.Writer, reqBody *AzureSpeechTranscriptionRequest) *schemas.BifrostError {
	filename := reqBody.Filename
	if filename == "" {
		filename = providerUtils.AudioFilenameFromBytes(reqBody.File)
	}
	fileWriter, err := writer.CreateFormFile("audio", filename)
	if err != nil {
		return providerUtils.NewBifrostOperationError("failed to create audio field", err)
	}
	if _, err := fileWriter.Write(reqBody.File); err != nil {
		return providerUtils.NewBifrostOperationError("failed to write audio data", err)
	}
	definition, err := providerUtils.MarshalSorted(reqBody.Definition)
	if err != nil {
		return providerUtils.NewBifrostOperationError("failed to marshal the transcription definition", err)
	}
	if err := writer.WriteField("definition", string(definition)); err != nil {
		return providerUtils.NewBifrostOperationError("failed to write definition field", err)
	}
	return nil
}

// ToBifrostTranscriptionResponse converts a fast transcription response into the unified
// one. text joins combinedPhrases (one per channel). usage is the vendor's
// durationMilliseconds in seconds and is left nil when the vendor did not report it: the
// duration is never measured from the file. phrases become segments (diarized segments
// when diarized is set) and their words become words.
func ToBifrostTranscriptionResponse(response *AzureSpeechTranscriptionResponse, diarized bool) *schemas.BifrostTranscriptionResponse {
	bifrostResponse := &schemas.BifrostTranscriptionResponse{}
	if response == nil {
		return bifrostResponse
	}

	texts := make([]string, 0, len(response.CombinedPhrases))
	for _, combined := range response.CombinedPhrases {
		if combined.Text != "" {
			texts = append(texts, combined.Text)
		}
	}
	bifrostResponse.Text = strings.Join(texts, " ")

	if response.DurationMilliseconds != nil {
		seconds := *response.DurationMilliseconds / 1000
		bifrostResponse.Duration = schemas.Ptr(seconds)
		bifrostResponse.Usage = &schemas.TranscriptionUsage{
			Type:    "duration",
			Seconds: schemas.Ptr(seconds),
		}
	}

	if diarized {
		bifrostResponse.DiarizedSegments = make([]schemas.TranscriptionDiarizedSegment, 0, len(response.Phrases))
	}
	for i, phrase := range response.Phrases {
		start := phrase.OffsetMilliseconds / 1000
		end := (phrase.OffsetMilliseconds + phrase.DurationMilliseconds) / 1000
		if bifrostResponse.Language == nil && phrase.Locale != "" {
			bifrostResponse.Language = schemas.Ptr(phrase.Locale)
		}
		var speaker *string
		if phrase.Speaker != nil {
			speaker = schemas.Ptr(strconv.Itoa(*phrase.Speaker))
		}
		if diarized {
			segment := schemas.TranscriptionDiarizedSegment{
				ID:    "seg_" + strconv.Itoa(i),
				Type:  "transcript.text.segment",
				Start: start,
				End:   end,
				Text:  phrase.Text,
			}
			if speaker != nil {
				segment.Speaker = *speaker
			}
			bifrostResponse.DiarizedSegments = append(bifrostResponse.DiarizedSegments, segment)
		} else {
			bifrostResponse.Segments = append(bifrostResponse.Segments, schemas.TranscriptionSegment{
				ID:     i,
				Start:  start,
				End:    end,
				Text:   phrase.Text,
				Tokens: []int{},
			})
		}
		for _, word := range phrase.Words {
			bifrostResponse.Words = append(bifrostResponse.Words, schemas.TranscriptionWord{
				Word:    word.Text,
				Start:   word.OffsetMilliseconds / 1000,
				End:     (word.OffsetMilliseconds + word.DurationMilliseconds) / 1000,
				Speaker: speaker,
			})
		}
	}

	return bifrostResponse
}
