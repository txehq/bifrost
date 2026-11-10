package schemas

import (
	"encoding/json"
	"strings"
	"testing"
)

// BackfillParams used to overwrite usage.input_chars with Bifrost's code-point count on every
// response, replacing a count the vendor reported (MiniMax usage_characters). The vendor's
// figure now stays; Bifrost's count fills the field only when the vendor reported none.
func TestSpeechBackfillParamsKeepsVendorCharacterCount(t *testing.T) {
	req := &BifrostSpeechRequest{Input: &SpeechInput{Input: "你好, world"}} // 9 code points

	resp := &BifrostSpeechResponse{Usage: &SpeechUsage{InputChars: 13}}
	resp.BackfillParams(req)
	if resp.Usage.InputChars != 13 {
		t.Fatalf("input_chars = %d, want the vendor's 13", resp.Usage.InputChars)
	}

	stream := &BifrostSpeechStreamResponse{Usage: &SpeechUsage{InputChars: 13}}
	stream.BackfillParams(req)
	if stream.Usage.InputChars != 13 {
		t.Fatalf("stream input_chars = %d, want the vendor's 13", stream.Usage.InputChars)
	}
}

func TestSpeechBackfillParamsFillsUnreportedCharacterCount(t *testing.T) {
	req := &BifrostSpeechRequest{Input: &SpeechInput{Input: "你好, world"}}

	resp := &BifrostSpeechResponse{}
	resp.BackfillParams(req)
	if resp.Usage == nil || resp.Usage.InputChars != 9 {
		t.Fatalf("usage = %+v, want input_chars 9", resp.Usage)
	}

	stream := &BifrostSpeechStreamResponse{Usage: &SpeechUsage{OutputTokens: 4}}
	stream.BackfillParams(req)
	if stream.Usage.InputChars != 9 || stream.Usage.OutputTokens != 4 {
		t.Fatalf("stream usage = %+v, want input_chars 9 and the vendor's output_tokens", stream.Usage)
	}
}

// The semantic cache stores a response as JSON and replays it on a hit; the provider-recorded
// media type of headerless audio (Gemini raw PCM) must survive that, or a cached hit is labeled
// audio/mpeg.
func TestSpeechResponseContentTypeSurvivesJSONRoundTrip(t *testing.T) {
	in := BifrostSpeechResponse{Audio: []byte{1, 0, 2, 0}, ContentType: "audio/L16;codec=pcm;rate=24000"}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out BifrostSpeechResponse
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if out.ContentType != in.ContentType {
		t.Fatalf("content type after round trip = %q, want %q (json %s)", out.ContentType, in.ContentType, data)
	}
	empty, _ := json.Marshal(BifrostSpeechResponse{Audio: []byte{1}})
	if strings.Contains(string(empty), "content_type") {
		t.Fatalf("an unknown media type must be omitted: %s", empty)
	}
}
