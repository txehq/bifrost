package schemas

import (
	"slices"
	"testing"
)

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

// TXE-6518 (contract C4 BF3): the four unified video create fields decode from a request body.
func TestVideoGenerationParametersUnifiedFields(t *testing.T) {
	var p VideoGenerationParameters
	body := `{"aspect_ratio":"9:16","resolution":"1080p","last_frame":"data:image/png;base64,AAAA",` +
		`"reference_images":["https://example.com/a.png","https://example.com/b.png"]}`
	if err := Unmarshal([]byte(body), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.AspectRatio != "9:16" || p.Resolution != "1080p" {
		t.Fatalf("aspect_ratio/resolution = %q/%q", p.AspectRatio, p.Resolution)
	}
	if p.LastFrame != "data:image/png;base64,AAAA" {
		t.Fatalf("last_frame = %v", p.LastFrame)
	}
	if !slices.Equal([]string(p.ReferenceImages), []string{"https://example.com/a.png", "https://example.com/b.png"}) {
		t.Fatalf("reference_images = %v", p.ReferenceImages)
	}

	out, err := Marshal(VideoGenerationParameters{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(out) != "{}" {
		t.Fatalf("empty parameters marshal to %s, want {}", out)
	}
}

// The unified scalar fields decode only a JSON string. Another type (Runware 3D's numeric
// resolution, an object) leaves the typed field empty instead of failing the request; the raw value
// stays an extra param.
func TestTolerantStringDecode(t *testing.T) {
	for _, body := range []string{
		`{"resolution":1024,"aspect_ratio":{"w":16,"h":9},"last_frame":["a"]}`,
		`{"resolution":null,"aspect_ratio":true,"last_frame":3.5}`,
	} {
		p := VideoGenerationParameters{Resolution: "stale", AspectRatio: "stale", LastFrame: "stale"}
		if err := Unmarshal([]byte(body), &p); err != nil {
			t.Fatalf("%s: unmarshal failed: %v", body, err)
		}
		if p.Resolution != "" || p.AspectRatio != "" || p.LastFrame != "" {
			t.Fatalf("%s: typed fields = %q/%q/%q, want empty", body, p.Resolution, p.AspectRatio, p.LastFrame)
		}
		out, err := Marshal(p)
		if err != nil || string(out) != "{}" {
			t.Fatalf("%s: re-marshal = %s, %v; want {}", body, out, err)
		}
	}
	var img ImageGenerationParameters
	if err := Unmarshal([]byte(`{"resolution":2048}`), &img); err != nil || img.Resolution != "" {
		t.Fatalf("image numeric resolution: %q, %v", img.Resolution, err)
	}
}

// reference_images in any shape other than an array of strings (Runway's objects) leaves the typed
// list empty instead of failing the request; the raw value stays an extra param.
func TestVideoReferenceImagesTolerantDecode(t *testing.T) {
	for _, body := range []string{
		`{"reference_images":[{"uri":"https://example.com/a.png","tag":"fox"}]}`,
		`{"reference_images":"https://example.com/a.png"}`,
		`{"reference_images":null}`,
	} {
		p := VideoGenerationParameters{ReferenceImages: VideoReferenceImages{"stale"}}
		if err := Unmarshal([]byte(body), &p); err != nil {
			t.Fatalf("%s: unmarshal failed: %v", body, err)
		}
		if p.ReferenceImages != nil {
			t.Fatalf("%s: reference_images = %v, want nil", body, p.ReferenceImages)
		}
	}
}
