package schemas

import "testing"

func TestBifrostVideoGenerationResponseBackfillSeconds(t *testing.T) {
	request := func(provider ModelProvider, seconds *string) *BifrostRequest {
		req := &BifrostVideoGenerationRequest{Provider: provider, Model: "m"}
		if seconds != nil {
			req.Params = &VideoGenerationParameters{Seconds: seconds}
		}
		return &BifrostRequest{VideoGenerationRequest: req}
	}
	cases := []struct {
		name     string
		reported *string
		req      *BifrostRequest
		want     *string
	}{
		{name: "provider-reported duration is kept", reported: Ptr("5"), req: request(MiniMax, Ptr("2")), want: Ptr("5")},
		{name: "missing duration comes from the request", req: request(OpenAI, Ptr("8")), want: Ptr("8")},
		{name: "missing duration on Gemini gets the default", req: request(Gemini, nil), want: Ptr(DefaultVideoDuration)},
		{name: "nothing to backfill", req: request(OpenAI, nil), want: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := &BifrostVideoGenerationResponse{Seconds: tc.reported}
			resp.BackfillParams(tc.req)
			switch {
			case tc.want == nil && resp.Seconds != nil:
				t.Errorf("seconds = %q, want nil", *resp.Seconds)
			case tc.want != nil && (resp.Seconds == nil || *resp.Seconds != *tc.want):
				t.Errorf("seconds = %v, want %q", resp.Seconds, *tc.want)
			}
			if resp.Model != "m" {
				t.Errorf("model = %q, want backfilled m", resp.Model)
			}
		})
	}
}

// BenchmarkBifrostVideoGenerationResponseBackfillParams measures the backfill every video create
// and edit response gets in the core request path, with and without a provider-reported duration.
func BenchmarkBifrostVideoGenerationResponseBackfillParams(b *testing.B) {
	req := &BifrostRequest{VideoGenerationRequest: &BifrostVideoGenerationRequest{
		Provider: MiniMax,
		Model:    "MiniMax-H3",
		Params:   &VideoGenerationParameters{Seconds: Ptr("2")},
	}}
	for _, tc := range []struct {
		name     string
		reported *string
	}{
		{name: "provider_reported_none"},
		{name: "provider_reported_seconds", reported: Ptr("5")},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				resp := BifrostVideoGenerationResponse{Seconds: tc.reported, Model: "MiniMax-H3"}
				resp.BackfillParams(req)
				if resp.Seconds == nil {
					b.Fatal("no seconds")
				}
			}
		})
	}
}
