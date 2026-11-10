package elevenlabs

import (
	"errors"
	"maps"
	"strings"

	"github.com/bytedance/sonic"
	"github.com/capsohq/bifrost/core/schemas"
)

func ToElevenlabsTranscriptionRequest(bifrostReq *schemas.BifrostTranscriptionRequest) *ElevenlabsTranscriptionRequest {
	if bifrostReq == nil {
		return nil
	}

	req := &ElevenlabsTranscriptionRequest{
		ModelID: bifrostReq.Model,
	}

	if bifrostReq.Input != nil && len(bifrostReq.Input.File) > 0 {
		req.File = bifrostReq.Input.File
		req.Filename = bifrostReq.Input.Filename
	}

	if bifrostReq.Params == nil {
		return req
	}

	params := bifrostReq.Params

	if params.Language != nil {
		req.LanguageCode = params.Language
	}

	if len(params.ExtraParams) > 0 {
		// Work on a copy: the keys mapped below are deleted from it, and what is left is
		// written to the form as it came (writeTranscriptionExtraFields). The caller's map
		// stays whole for any later use of the same request.
		extra := maps.Clone(params.ExtraParams)
		req.ExtraParams = extra
		if tagAudioEvents, ok := schemas.SafeExtractBoolPointer(extra["tag_audio_events"]); ok {
			delete(extra, "tag_audio_events")
			req.TagAudioEvents = tagAudioEvents
		}
		if numSpeakers, ok := schemas.SafeExtractIntPointer(extra["num_speakers"]); ok {
			delete(extra, "num_speakers")
			req.NumSpeakers = numSpeakers
		}
		if timestampsGranularity, ok := schemas.SafeExtractStringPointer(extra["timestamps_granularity"]); ok {
			granularity := ElevenlabsTimestampsGranularity(*timestampsGranularity)
			delete(extra, "timestamps_granularity")
			req.TimestampsGranularity = &granularity
		}
		if diarize, ok := schemas.SafeExtractBoolPointer(extra["diarize"]); ok {
			delete(extra, "diarize")
			req.Diarize = diarize
		}
		if diarizationThreshold, ok := schemas.SafeExtractFloat64Pointer(extra["diarization_threshold"]); ok {
			delete(extra, "diarization_threshold")
			req.DiarizationThreshold = diarizationThreshold
		}
		if fileFormat, ok := schemas.SafeExtractStringPointer(extra["file_format"]); ok {
			fileFormat := ElevenlabsFileFormat(*fileFormat)
			delete(extra, "file_format")
			req.FileFormat = &fileFormat
		}
		if cloudStorageURL, ok := schemas.SafeExtractStringPointer(extra["cloud_storage_url"]); ok {
			delete(extra, "cloud_storage_url")
			req.CloudStorageURL = cloudStorageURL
		}
		if webhook, ok := schemas.SafeExtractBoolPointer(extra["webhook"]); ok {
			delete(extra, "webhook")
			req.Webhook = webhook
		}
		if webhookID, ok := schemas.SafeExtractStringPointer(extra["webhook_id"]); ok {
			delete(extra, "webhook_id")
			req.WebhookID = webhookID
		}
		if temperature, ok := schemas.SafeExtractFloat64Pointer(extra["temperature"]); ok {
			delete(extra, "temperature")
			req.Temperature = temperature
		}
		if seed, ok := schemas.SafeExtractIntPointer(extra["seed"]); ok {
			delete(extra, "seed")
			req.Seed = seed
		}
		if useMultiChannel, ok := schemas.SafeExtractBoolPointer(extra["use_multi_channel"]); ok {
			delete(extra, "use_multi_channel")
			req.UseMultiChannel = useMultiChannel
		}
	}

	if len(params.AdditionalFormats) > 0 {
		additionalFormats := make([]ElevenlabsAdditionalFormat, 0, len(params.AdditionalFormats))
		for _, format := range params.AdditionalFormats {
			if converted, ok := convertAdditionalFormat(format); ok {
				additionalFormats = append(additionalFormats, converted)
			}
		}
		if len(additionalFormats) > 0 {
			req.AdditionalFormats = additionalFormats
		}
	}

	if params.WebhookMetadata != nil {
		if metadataMap, ok := params.WebhookMetadata.(map[string]interface{}); ok {
			if len(metadataMap) > 0 {
				req.WebhookMetadata = metadataMap
			}
		} else {
			req.WebhookMetadata = params.WebhookMetadata
		}
	}

	return req
}

// ToBifrostTranscriptionResponse converts the transcript chunks (one for a plain answer, one
// per channel for a multichannel one) and the answer's top-level audio_duration_secs, nil when
// ElevenLabs did not report it.
func ToBifrostTranscriptionResponse(chunks []ElevenlabsSpeechToTextChunkResponse, audioDurationSecs *float64) *schemas.BifrostTranscriptionResponse {
	if len(chunks) == 0 {
		return nil
	}

	textParts := make([]string, 0, len(chunks))
	allWords := make([]schemas.TranscriptionWord, 0)
	allLogProbs := make([]schemas.TranscriptionLogProb, 0)

	var language *string
	var overallDuration *float64
	for _, chunk := range chunks {
		textParts = append(textParts, chunk.Text)

		words, logProbs, chunkDuration := convertWords(chunk.Words)
		allWords = append(allWords, words...)
		allLogProbs = append(allLogProbs, logProbs...)

		if language == nil && chunk.LanguageCode != "" {
			lc := chunk.LanguageCode
			language = &lc
		}

		if chunkDuration != nil {
			if overallDuration == nil || *chunkDuration > *overallDuration {
				val := *chunkDuration
				overallDuration = &val
			}
		}
	}

	text := strings.Join(textParts, "\n")

	response := &schemas.BifrostTranscriptionResponse{
		Text:     text,
		Words:    allWords,
		LogProbs: allLogProbs,
	}

	if language != nil {
		response.Language = language
	}

	if overallDuration != nil {
		response.Duration = overallDuration
	}

	// usage is the duration ElevenLabs reported and bills on, never Bifrost's reading of the
	// word timings above; it stays empty when the vendor reported none (TXE-6524, contract C5).
	if audioDurationSecs != nil {
		seconds := *audioDurationSecs
		response.Usage = &schemas.TranscriptionUsage{Type: "duration", Seconds: &seconds}
	}

	return response

}

func convertAdditionalFormat(format schemas.TranscriptionAdditionalFormat) (ElevenlabsAdditionalFormat, bool) {
	if format.Format == "" {
		return ElevenlabsAdditionalFormat{}, false
	}

	converted := ElevenlabsAdditionalFormat{
		Format: ElevenlabsExportOptions(format.Format),
	}

	if format.IncludeSpeakers != nil {
		converted.IncludeSpeakers = format.IncludeSpeakers
	}

	if format.IncludeTimestamps != nil {
		converted.IncludeTimestamps = format.IncludeTimestamps
	}

	if format.SegmentOnSilenceLongerThanS != nil {
		converted.SegmentOnSilenceLongerThanS = format.SegmentOnSilenceLongerThanS
	}

	if format.MaxSegmentDurationS != nil {
		converted.MaxSegmentDurationS = format.MaxSegmentDurationS
	}

	if format.MaxSegmentChars != nil {
		converted.MaxSegmentChars = format.MaxSegmentChars
	}

	if format.MaxCharactersPerLine != nil {
		converted.MaxCharactersPerLine = format.MaxCharactersPerLine
	}

	return converted, true
}

func convertWords(words []ElevenlabsSpeechToTextWord) ([]schemas.TranscriptionWord, []schemas.TranscriptionLogProb, *float64) {
	if len(words) == 0 {
		return nil, nil, nil
	}

	convertedWords := make([]schemas.TranscriptionWord, 0, len(words))
	logProbs := make([]schemas.TranscriptionLogProb, 0, len(words))

	var maxEnd float64
	var hasEnd bool

	for _, word := range words {
		trimmed := strings.TrimSpace(word.Text)
		if word.Type == "spacing" && trimmed == "" {
			continue
		}

		transcriptionWord := schemas.TranscriptionWord{
			Word:    word.Text,
			Speaker: word.SpeakerID,
		}

		if word.Start != nil {
			transcriptionWord.Start = *word.Start
		}

		if word.End != nil {
			transcriptionWord.End = *word.End
			if !hasEnd || *word.End > maxEnd {
				maxEnd = *word.End
				hasEnd = true
			}
		}

		convertedWords = append(convertedWords, transcriptionWord)
		logProbs = append(logProbs, schemas.TranscriptionLogProb{
			Token:   word.Text,
			LogProb: word.LogProb,
		})
	}

	if !hasEnd {
		return convertedWords, logProbs, nil
	}

	duration := maxEnd
	return convertedWords, logProbs, &duration
}

// isEmptyTranscriptionBody reports whether a 200 body carries nothing at all: no bytes, JSON
// null, or an empty JSON object.
func isEmptyTranscriptionBody(body []byte) bool {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" || trimmed == "null" {
		return true
	}
	if strings.HasPrefix(trimmed, "{") && strings.HasSuffix(trimmed, "}") {
		return strings.TrimSpace(trimmed[1:len(trimmed)-1]) == ""
	}
	return false
}

// rawJSONOrNil returns body when it is valid JSON and nil otherwise: the raw response of an
// error is serialized as JSON, so a body that is not JSON cannot be attached as it is.
func rawJSONOrNil(body []byte) []byte {
	if sonic.Valid(body) {
		return body
	}
	return nil
}

// parseTranscriptionResponse returns the transcript chunks and the top-level
// audio_duration_secs of the answer, the audio length ElevenLabs reports and bills on: the
// chunk's own on a plain answer, the multichannel answer's on a multichannel one. ElevenLabs
// reports the multichannel value as the sum over the channels (measured in TXE-4862: a
// two-channel 0.1 s file answered 0.2), so it already is the billed duration and per-channel
// values are not summed. It is nil when the answer does not carry the field. No chunks and no error means the answer has no transcript in it.
func parseTranscriptionResponse(body []byte) ([]ElevenlabsSpeechToTextChunkResponse, *float64, error) {
	var multichannel ElevenlabsMultichannelSpeechToTextResponse
	if err := sonic.Unmarshal(body, &multichannel); err == nil && len(multichannel.Transcripts) > 0 {
		return multichannel.Transcripts, multichannel.AudioDurationSecs, nil
	}

	// A multichannel answer whose transcripts list is there but empty has no output: no
	// chunks and no error, which the caller reports as provider_empty_result.
	var listed struct {
		Transcripts *[]struct{} `json:"transcripts"`
	}
	if err := sonic.Unmarshal(body, &listed); err == nil && listed.Transcripts != nil {
		return nil, nil, nil
	}

	var single ElevenlabsSpeechToTextChunkResponse
	if err := sonic.Unmarshal(body, &single); err == nil {
		if single.LanguageCode != "" || single.Text != "" || len(single.Words) > 0 || single.AudioDurationSecs != nil {
			return []ElevenlabsSpeechToTextChunkResponse{single}, single.AudioDurationSecs, nil
		}
	}

	var webhook ElevenlabsSpeechToTextWebhookResponse
	if err := sonic.Unmarshal(body, &webhook); err == nil && strings.TrimSpace(webhook.Message) != "" {
		return nil, nil, errors.New(webhook.Message)
	}

	return nil, nil, errors.New("unexpected Elevenlabs transcription response format")
}
