package elevenlabs

import (
	"bytes"
	"mime/multipart"
	"reflect"
	"testing"

	"github.com/capsohq/bifrost/core/schemas"
)

// TestConvertWords_PropagatesSpeakerID confirms that ElevenLabs' diarization
// output (speaker_id per word, per https://elevenlabs.io/docs/api-reference/speech-to-text/convert)
// is carried through into Bifrost's canonical TranscriptionWord.Speaker field
// instead of being silently dropped.
func TestConvertWords_PropagatesSpeakerID(t *testing.T) {
	speakerA := "speaker_0"
	speakerB := "speaker_1"
	start1, end1 := 0.0, 1.2
	start2, end2 := 1.2, 2.5

	words := []ElevenlabsSpeechToTextWord{
		{Text: "Hello", Type: "word", Start: &start1, End: &end1, SpeakerID: &speakerA, LogProb: -0.1},
		{Text: " ", Type: "spacing", Start: &end1, End: &end1, SpeakerID: &speakerA},
		{Text: "there", Type: "word", Start: &start2, End: &end2, SpeakerID: &speakerB, LogProb: -0.2},
	}

	converted, logProbs, duration := convertWords(words)

	if len(converted) != 2 {
		t.Fatalf("expected 2 words (spacing dropped), got %d", len(converted))
	}
	if converted[0].Speaker == nil || *converted[0].Speaker != "speaker_0" {
		t.Fatalf("expected first word speaker %q, got %+v", "speaker_0", converted[0].Speaker)
	}
	if converted[1].Speaker == nil || *converted[1].Speaker != "speaker_1" {
		t.Fatalf("expected second word speaker %q, got %+v", "speaker_1", converted[1].Speaker)
	}
	if len(logProbs) != 2 {
		t.Fatalf("expected 2 logprobs, got %d", len(logProbs))
	}
	if duration == nil || *duration != 2.5 {
		t.Fatalf("expected duration 2.5, got %v", duration)
	}
}

// TestToBifrostTranscriptionResponse_RoleBasedSpeakers confirms role-based
// diarization (detect_speaker_roles=true, speaker_id values "agent"/"customer")
// also flows through unchanged, since Speaker is a plain string passthrough.
func TestToBifrostTranscriptionResponse_RoleBasedSpeakers(t *testing.T) {
	agent := "agent"
	start, end := 0.0, 1.0

	chunks := []ElevenlabsSpeechToTextChunkResponse{
		{
			LanguageCode: "en",
			Text:         "Hello",
			Words: []ElevenlabsSpeechToTextWord{
				{Text: "Hello", Type: "word", Start: &start, End: &end, SpeakerID: &agent, LogProb: -0.05},
			},
		},
	}

	resp := ToBifrostTranscriptionResponse(chunks, nil)
	if resp == nil {
		t.Fatal("expected non-nil response")
	}
	if len(resp.Words) != 1 {
		t.Fatalf("expected 1 word, got %d", len(resp.Words))
	}
	if resp.Words[0].Speaker == nil || *resp.Words[0].Speaker != "agent" {
		t.Fatalf("expected speaker %q, got %+v", "agent", resp.Words[0].Speaker)
	}
}

// ElevenLabs answers in the shape of its SpeechToTextChunkResponseModel. The single-channel
// sample is the document Tokun's upstream stand-in serves (tokun/internal/upstreamfixture);
// the multichannel one follows the measurement in TXE-4862 (credit-run-013): a two-channel
// 0.1 s file answered a top-level audio_duration_secs of 0.2, the sum over the channels.
const (
	sttSingleChannelAnswer = `{"language_code":"en","language_probability":0.98,"text":"hello world","words":[{"text":"hello","type":"word","logprob":-0.1,"start":0.0,"end":0.5}],"transcription_id":"tr_1","audio_duration_secs":12.34}`
	sttMultichannelAnswer  = `{"transcripts":[{"language_code":"en","language_probability":0.99,"text":"left","channel_index":0,"words":[{"text":"left","type":"word","logprob":-0.1,"start":0.0,"end":0.08}]},{"language_code":"en","language_probability":0.99,"text":"right","channel_index":1,"words":[{"text":"right","type":"word","logprob":-0.1,"start":0.0,"end":0.09}]}],"transcription_id":"tr_2","audio_duration_secs":0.2}`
	sttAnswerNoDuration    = `{"language_code":"en","text":"hello","words":[{"text":"hello","type":"word","logprob":-0.1,"start":0.0,"end":0.5}]}`
)

// Contract C5 (TXE-6524): usage is {type: "duration", seconds: audio_duration_secs}, the
// figure ElevenLabs bills on. Before, audio_duration_secs was not parsed and usage was empty.
func TestTranscriptionUsageIsTheVendorAudioDuration(t *testing.T) {
	cases := []struct {
		name         string
		answer       string
		wantChunks   int
		wantSeconds  *float64 // nil: no usage at all
		wantDuration float64  // the unified duration field, still the last word's end
	}{
		{"single channel", sttSingleChannelAnswer, 1, schemas.Ptr(12.34), 0.5},
		// The top-level value already is the sum over the two channels; it is taken as it is.
		{"multichannel", sttMultichannelAnswer, 2, schemas.Ptr(0.2), 0.09},
		// Not reported: usage stays empty, never filled from the word timings.
		{"vendor reported no duration", sttAnswerNoDuration, 1, nil, 0.5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chunks, audioDurationSecs, err := parseTranscriptionResponse([]byte(tc.answer))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if len(chunks) != tc.wantChunks {
				t.Fatalf("chunks = %d, want %d", len(chunks), tc.wantChunks)
			}
			resp := ToBifrostTranscriptionResponse(chunks, audioDurationSecs)
			if resp.Duration == nil || *resp.Duration != tc.wantDuration {
				t.Errorf("duration = %v, want %v", resp.Duration, tc.wantDuration)
			}
			if tc.wantSeconds == nil {
				if resp.Usage != nil {
					t.Fatalf("usage = %+v, want none when the vendor reported no duration", resp.Usage)
				}
				return
			}
			want := &schemas.TranscriptionUsage{Type: "duration", Seconds: tc.wantSeconds}
			if !reflect.DeepEqual(resp.Usage, want) {
				t.Fatalf("usage = %+v, want {type: duration, seconds: %v}", resp.Usage, *tc.wantSeconds)
			}
		})
	}
}

// A transcript of silence has no text and no words but is still a billed answer: the reported
// duration alone makes it a transcript, not an unexpected format.
func TestParseTranscriptionResponse_SilenceWithDurationIsATranscript(t *testing.T) {
	chunks, audioDurationSecs, err := parseTranscriptionResponse([]byte(`{"language_code":"","text":"","words":[],"audio_duration_secs":3.5}`))
	if err != nil || len(chunks) != 1 {
		t.Fatalf("chunks = %d, err = %v, want one chunk", len(chunks), err)
	}
	if audioDurationSecs == nil || *audioDurationSecs != 3.5 {
		t.Fatalf("audio_duration_secs = %v, want 3.5", audioDurationSecs)
	}
}

func TestIsEmptyTranscriptionBody(t *testing.T) {
	for body, want := range map[string]bool{
		"":                       true,
		"  \n ":                  true,
		"null":                   true,
		"{}":                     true,
		" { } ":                  true,
		`{"text":""}`:            false,
		sttSingleChannelAnswer:   false,
		`{"message":"accepted"}`: false,
		"[]":                     false,
	} {
		if got := isEmptyTranscriptionBody([]byte(body)); got != want {
			t.Errorf("isEmptyTranscriptionBody(%q) = %v, want %v", body, got, want)
		}
	}
}

// transcriptionForm converts the request, writes the multipart body as it goes upstream and
// reads it back, so a test sees the form fields ElevenLabs receives.
func transcriptionForm(t *testing.T, req *schemas.BifrostTranscriptionRequest) map[string][]string {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if bifrostErr := writeTranscriptionMultipart(writer, ToElevenlabsTranscriptionRequest(req)); bifrostErr != nil {
		t.Fatalf("writeTranscriptionMultipart: %+v", bifrostErr.Error)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	form, err := multipart.NewReader(&body, writer.Boundary()).ReadForm(1 << 20)
	if err != nil {
		t.Fatalf("read form: %v", err)
	}
	return form.Value
}

// Contract C4 (TXE-6524): a caller field the converter does not map is written to the form
// unchanged. Before, only the mapped fields were written, so the priced addons keyterms,
// transcript_edit and detect_speaker_roles never reached ElevenLabs.
func TestTranscriptionMultipartCarriesUnmappedExtraFields(t *testing.T) {
	extra := map[string]interface{}{
		// Addons, in the shapes the multipart parsers store them: a repeated field as a
		// []string, a single field as a string.
		"keyterms":             []string{"Bifrost", "Tokun", "ElevenLabs"},
		"transcript_edit":      "true",
		"detect_speaker_roles": "true",
		// The same addons can arrive typed from the Go SDK.
		"entity_detection": []interface{}{"pii", "phi"},
		"no_verbatim":      true,
		// Mapped fields still go through their own writer, once.
		"diarize":      true,
		"num_speakers": 2,
		// Bifrost's own fields are not the caller's to write twice.
		"model_id": "not-the-model",
		"unset":    nil,
	}
	req := &schemas.BifrostTranscriptionRequest{
		Provider: schemas.Elevenlabs,
		Model:    "scribe_v2",
		Input:    &schemas.TranscriptionInput{File: []byte("RIFF....WAVE"), Filename: "a.wav"},
		Params:   &schemas.TranscriptionParameters{Language: schemas.Ptr("en"), ExtraParams: extra},
	}

	form := transcriptionForm(t, req)

	want := map[string][]string{
		"model_id":             {"scribe_v2"},
		"language_code":        {"en"},
		"diarize":              {"true"},
		"num_speakers":         {"2"},
		"keyterms":             {"Bifrost", "Tokun", "ElevenLabs"},
		"transcript_edit":      {"true"},
		"detect_speaker_roles": {"true"},
		"entity_detection":     {`["pii","phi"]`},
		"no_verbatim":          {"true"},
	}
	if !reflect.DeepEqual(form, want) {
		t.Fatalf("form fields = %v\nwant        %v", form, want)
	}

	// The conversion consumed keys from a copy: the caller's map is whole, so a second
	// conversion of the same request sends the same form.
	if len(extra) != 9 {
		t.Fatalf("caller's ExtraParams has %d keys, want all 9 left in place", len(extra))
	}
	if again := transcriptionForm(t, req); !reflect.DeepEqual(again, want) {
		t.Fatalf("second conversion sent %v, want the same form", again)
	}
}

// language_code from the extra fields is the caller's only when the unified language did not
// already write it; otherwise the field would be sent twice with two values.
func TestTranscriptionMultipartLanguageCodeWrittenOnce(t *testing.T) {
	input := &schemas.TranscriptionInput{File: []byte("x"), Filename: "a.wav"}
	both := transcriptionForm(t, &schemas.BifrostTranscriptionRequest{Model: "scribe_v2", Input: input,
		Params: &schemas.TranscriptionParameters{Language: schemas.Ptr("en"), ExtraParams: map[string]interface{}{"language_code": "de"}}})
	if got := both["language_code"]; !reflect.DeepEqual(got, []string{"en"}) {
		t.Errorf("language and language_code both given: sent %v, want only the unified en", got)
	}
	onlyExtra := transcriptionForm(t, &schemas.BifrostTranscriptionRequest{Model: "scribe_v2", Input: input,
		Params: &schemas.TranscriptionParameters{ExtraParams: map[string]interface{}{"language_code": "de"}}})
	if got := onlyExtra["language_code"]; !reflect.DeepEqual(got, []string{"de"}) {
		t.Errorf("only language_code given: sent %v, want de", got)
	}
}

// Cost on the request path: one multipart build per transcription. The plain request has no
// extra params and pays nothing new; the addon request pays the copy of the caller's map and
// the sorted write of what is left.
func BenchmarkWriteTranscriptionMultipart(b *testing.B) {
	input := &schemas.TranscriptionInput{File: bytes.Repeat([]byte("a"), 32<<10), Filename: "a.wav"}
	run := func(b *testing.B, params *schemas.TranscriptionParameters) {
		req := &schemas.BifrostTranscriptionRequest{Model: "scribe_v2", Input: input, Params: params}
		var body bytes.Buffer
		b.ReportAllocs()
		for b.Loop() {
			body.Reset()
			writer := multipart.NewWriter(&body)
			if bifrostErr := writeTranscriptionMultipart(writer, ToElevenlabsTranscriptionRequest(req)); bifrostErr != nil {
				b.Fatal(bifrostErr.Error.Message)
			}
			_ = writer.Close()
		}
	}
	b.Run("plain", func(b *testing.B) { run(b, &schemas.TranscriptionParameters{Language: schemas.Ptr("en")}) })
	b.Run("addons", func(b *testing.B) {
		run(b, &schemas.TranscriptionParameters{Language: schemas.Ptr("en"), ExtraParams: map[string]interface{}{
			"diarize": true, "keyterms": []string{"Bifrost", "Tokun"}, "transcript_edit": "true", "detect_speaker_roles": "true",
		}})
	})
}
