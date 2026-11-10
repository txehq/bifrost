package gemini

import (
	"context"
	"encoding/base64"
	"testing"
)

// Gemini returns raw PCM unless wav was asked for. The transport labels the audio from
// BifrostSpeechResponse.ContentType, so the converter must record the format it returns
// (TXE-6530); without it a raw PCM body went out as audio/mpeg.
func TestToBifrostSpeechResponseRecordsContentType(t *testing.T) {
	pcm := []byte{0x01, 0x00, 0x02, 0x00}
	resp := &GenerateContentResponse{
		Candidates: []*Candidate{{Content: &Content{Parts: []*Part{{InlineData: &Blob{
			MIMEType: "audio/L16;codec=pcm;rate=24000",
			Data:     base64.StdEncoding.EncodeToString(pcm),
		}}}}}},
		UsageMetadata: &GenerateContentResponseUsageMetadata{PromptTokenCount: 5, CandidatesTokenCount: 30, TotalTokenCount: 35},
	}

	for _, tc := range []struct {
		format string
		want   string
	}{
		{"", "audio/L16;codec=pcm;rate=24000"},
		{"pcm", "audio/L16;codec=pcm;rate=24000"},
		{"wav", "audio/wav"},
	} {
		ctx := context.WithValue(context.Background(), BifrostContextKeyResponseFormat, tc.format)
		out, err := resp.ToBifrostSpeechResponse(ctx)
		if err != nil {
			t.Fatalf("format %q: %v", tc.format, err)
		}
		if out.ContentType != tc.want {
			t.Errorf("format %q: ContentType = %q, want %q", tc.format, out.ContentType, tc.want)
		}
		if out.Usage == nil || out.Usage.InputTokens != 5 || out.Usage.OutputTokens != 30 {
			t.Errorf("format %q: usage = %+v, want Gemini's usageMetadata", tc.format, out.Usage)
		}
	}
}
