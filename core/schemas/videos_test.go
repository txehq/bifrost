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
