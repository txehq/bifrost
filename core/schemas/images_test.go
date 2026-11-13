package schemas

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// An image source arrives either as the object form or as a bare string, so images: ["https://..."]
// reads the way input_images does on /v1/images/generations.
func TestImageInputUnmarshalJSON(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantURL string
		wantImg string
		wantErr bool
	}{
		{name: "bare string url", body: `"https://example.com/teapot.jpg"`, wantURL: "https://example.com/teapot.jpg"},
		{name: "bare string data uri", body: `"data:image/png;base64,aGVsbG8="`, wantURL: "data:image/png;base64,aGVsbG8="},
		{name: "object url", body: `{"url":"https://example.com/teapot.jpg"}`, wantURL: "https://example.com/teapot.jpg"},
		{name: "object base64 bytes", body: `{"image":"aGVsbG8="}`, wantImg: "hello"},
		{name: "null clears both arms", body: `null`},
		{name: "number is neither form", body: `42`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got ImageInput
			err := Unmarshal([]byte(tc.body), &got)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.URL != tc.wantURL {
				t.Fatalf("url = %q, want %q", got.URL, tc.wantURL)
			}
			if string(got.Image) != tc.wantImg {
				t.Fatalf("image = %q, want %q", got.Image, tc.wantImg)
			}
		})
	}
}

// The two forms mix freely inside one images array, and decoding into a reused value must not
// leave the other arm populated.
func TestImageEditInputUnmarshalMixedImageForms(t *testing.T) {
	var input ImageEditInput
	if err := Unmarshal([]byte(
		`{"prompt":"blue","images":["https://example.com/a.jpg",{"image":"aGVsbG8="},{"url":"https://example.com/b.jpg"}]}`,
	), &input); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(input.Images) != 3 {
		t.Fatalf("images = %+v, want 3 entries", input.Images)
	}
	if input.Images[0].URL != "https://example.com/a.jpg" || input.Images[0].Image != nil {
		t.Fatalf("string form = %+v", input.Images[0])
	}
	if string(input.Images[1].Image) != "hello" || input.Images[1].URL != "" {
		t.Fatalf("bytes form = %+v", input.Images[1])
	}
	if input.Images[2].URL != "https://example.com/b.jpg" {
		t.Fatalf("object url form = %+v", input.Images[2])
	}

	// A second decode into the same value replaces both arms rather than merging them.
	reused := &input.Images[1]
	if err := Unmarshal([]byte(`"https://example.com/c.jpg"`), reused); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reused.URL != "https://example.com/c.jpg" || reused.Image != nil {
		t.Fatalf("reused decode = %+v, want only the url arm set", reused)
	}
}

// Marshalling stays on the object form; the string form is an input convenience only, so logged
// and replayed requests keep one shape.
func TestImageInputMarshalsObjectForm(t *testing.T) {
	out, err := Marshal(ImageInput{URL: "https://example.com/teapot.jpg"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != `{"image":null,"url":"https://example.com/teapot.jpg"}` {
		t.Fatalf("marshalled = %s", out)
	}
}

// Image edit requests carry aspect_ratio too, so edit responses backfill it like generation responses.
func TestImageEditBackfillsAspectRatio(t *testing.T) {
	req := &BifrostRequest{ImageEditRequest: &BifrostImageEditRequest{
		Input:  &ImageEditInput{Images: []ImageInput{{Image: []byte("png")}}},
		Params: &ImageEditParameters{AspectRatio: new("3:2")},
	}}

	resp := &BifrostImageGenerationResponse{}
	resp.BackfillParams(req)
	if resp.ImageGenerationResponseParameters == nil || resp.ImageGenerationResponseParameters.AspectRatio != "3:2" {
		t.Fatalf("response params = %+v, want aspect_ratio 3:2", resp.ImageGenerationResponseParameters)
	}

	streamResp := &BifrostImageGenerationStreamResponse{}
	streamResp.BackfillParams(req)
	if streamResp.AspectRatio != "3:2" {
		t.Fatalf("stream aspect_ratio = %q, want 3:2", streamResp.AspectRatio)
	}
}

// TXE-7356: the resolution tier, the inference steps and the dall-e-3 qualities a model is priced
// by reach the response, where the cost path reads them. hd and standard used to be dropped here.
func TestImageBackfillsPricedRequestShape(t *testing.T) {
	req := &BifrostRequest{ImageGenerationRequest: &BifrostImageGenerationRequest{
		Params: &ImageGenerationParameters{Quality: new("hd"), Resolution: "2k", NumInferenceSteps: new(60)},
	}}
	resp := &BifrostImageGenerationResponse{}
	resp.BackfillParams(req)
	p := resp.ImageGenerationResponseParameters
	if p == nil || p.Quality != "hd" || p.Resolution != "2k" || p.NumInferenceSteps == nil || *p.NumInferenceSteps != 60 {
		t.Fatalf("response params = %+v, want quality hd, resolution 2k, 60 steps", p)
	}
	// The response's copy is its own: a later change to the request does not move it.
	*req.ImageGenerationRequest.Params.NumInferenceSteps = 10
	if *p.NumInferenceSteps != 60 {
		t.Fatalf("steps aliased the request: %d", *p.NumInferenceSteps)
	}

	streamResp := &BifrostImageGenerationStreamResponse{}
	streamResp.BackfillParams(req)
	if streamResp.Resolution != "2k" || streamResp.NumInferenceSteps == nil {
		t.Fatalf("stream = resolution %q steps %v", streamResp.Resolution, streamResp.NumInferenceSteps)
	}

	// Stability's native "steps" extra param counts when num_inference_steps is unset.
	edit := &BifrostRequest{ImageEditRequest: &BifrostImageEditRequest{
		Params: &ImageEditParameters{Quality: new("standard"), ExtraParams: map[string]interface{}{"steps": float64(40)}},
	}}
	editResp := &BifrostImageGenerationResponse{}
	editResp.BackfillParams(edit)
	ep := editResp.ImageGenerationResponseParameters
	if ep == nil || ep.Quality != "standard" || ep.NumInferenceSteps == nil || *ep.NumInferenceSteps != 40 {
		t.Fatalf("edit response params = %+v, want quality standard and 40 steps", ep)
	}

	// A value the vendor reported wins over the request's.
	vendor := &BifrostImageGenerationResponse{ImageGenerationResponseParameters: &ImageGenerationResponseParameters{Resolution: "1K"}}
	vendor.BackfillParams(req)
	if vendor.ImageGenerationResponseParameters.Resolution != "1K" {
		t.Fatalf("resolution = %q, want the vendor's 1K", vendor.ImageGenerationResponseParameters.Resolution)
	}
}

// TXE-6518 (contract C4 BF3): resolution decodes on both image generation and image edit.
func TestImageParametersResolution(t *testing.T) {
	var gen ImageGenerationParameters
	if err := Unmarshal([]byte(`{"resolution":"2K","aspect_ratio":"1:1"}`), &gen); err != nil {
		t.Fatalf("unmarshal generation: %v", err)
	}
	if gen.Resolution != "2K" {
		t.Fatalf("generation resolution = %v, want 2K", gen.Resolution)
	}
	var edit ImageEditParameters
	if err := Unmarshal([]byte(`{"resolution":"4K"}`), &edit); err != nil {
		t.Fatalf("unmarshal edit: %v", err)
	}
	if edit.Resolution != "4K" {
		t.Fatalf("edit resolution = %v, want 4K", edit.Resolution)
	}
}

// TXE-6518 (contract C6 BF10): a response with blocked images serialises content_filter, and a
// response whose every image was blocked carries "data": [] after the pipeline's BackfillParams,
// never "data": null.
func TestImageResponseContentFilter(t *testing.T) {
	t.Run("all blocked", func(t *testing.T) {
		resp := &BifrostImageGenerationResponse{
			ContentFilter: &ContentFilterInfo{FilteredCount: 2, Reasons: []string{"PROHIBITED_CONTENT", "58061214"}},
		}
		resp.BackfillParams(&BifrostRequest{ImageGenerationRequest: &BifrostImageGenerationRequest{Model: "m"}})
		out, err := Marshal(resp)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		for _, want := range []string{`"data":[]`, `"content_filter":{"filtered_count":2,"reasons":["PROHIBITED_CONTENT","58061214"]}`} {
			if !strings.Contains(string(out), want) {
				t.Fatalf("%s missing from %s", want, out)
			}
		}
	})
	t.Run("nil request still keeps data an array", func(t *testing.T) {
		resp := &BifrostImageGenerationResponse{}
		resp.BackfillParams(nil)
		if resp.Data == nil || len(resp.Data) != 0 {
			t.Fatalf("data = %#v, want an empty array", resp.Data)
		}
	})
	t.Run("partly blocked keeps delivered images", func(t *testing.T) {
		resp := &BifrostImageGenerationResponse{
			Data:          []ImageData{{URL: "https://example.com/1.png"}},
			ContentFilter: &ContentFilterInfo{FilteredCount: 1, Reasons: []string{"SAFETY"}},
		}
		resp.BackfillParams(&BifrostRequest{ImageGenerationRequest: &BifrostImageGenerationRequest{Model: "m"}})
		if len(resp.Data) != 1 {
			t.Fatalf("data = %#v, want the one delivered image", resp.Data)
		}
	})
	t.Run("no filter, no key", func(t *testing.T) {
		out, err := Marshal(&BifrostImageGenerationResponse{Data: []ImageData{{URL: "u"}}})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if strings.Contains(string(out), "content_filter") {
			t.Fatalf("content_filter present on an unfiltered response: %s", out)
		}
	})
}

// TXE-6438 (contract C5): every sync image response carries usage.output_images equal to the number
// of images the vendor delivered in data, also when the vendor sent no usage at all (Vercel AI
// Gateway's OpenAI-compatible images endpoint), and a count the provider decoded from the vendor's
// own usage report is kept.
func TestImageResponseOutputImages(t *testing.T) {
	genReq := &BifrostRequest{ImageGenerationRequest: &BifrostImageGenerationRequest{Model: "meta/muse-image-1.0"}}
	cases := []struct {
		name string
		resp *BifrostImageGenerationResponse
		req  *BifrostRequest
		want int
	}{
		{
			name: "vendor sent no usage",
			resp: &BifrostImageGenerationResponse{Data: []ImageData{{B64JSON: "aQ=="}, {B64JSON: "aQ==", Index: 1}}},
			req:  genReq,
			want: 2,
		},
		{
			name: "vendor sent token usage without a count",
			resp: &BifrostImageGenerationResponse{
				Data:  []ImageData{{URL: "https://example.com/1.png"}},
				Usage: &ImageUsage{InputTokens: 12, OutputTokens: 4160, TotalTokens: 4172},
			},
			req:  genReq,
			want: 1,
		},
		{
			name: "vendor-reported count is kept",
			resp: &BifrostImageGenerationResponse{
				Data:  []ImageData{{URL: "https://example.com/1.png"}},
				Usage: &ImageUsage{OutputImages: new(3)},
			},
			req:  genReq,
			want: 3,
		},
		{
			name: "every image blocked",
			resp: &BifrostImageGenerationResponse{ContentFilter: &ContentFilterInfo{FilteredCount: 1}},
			req:  genReq,
			want: 0,
		},
		{
			name: "image edit",
			resp: &BifrostImageGenerationResponse{Data: []ImageData{{URL: "u"}}},
			req: &BifrostRequest{ImageEditRequest: &BifrostImageEditRequest{
				Input: &ImageEditInput{Images: []ImageInput{{Image: []byte("png")}}},
			}},
			want: 1,
		},
		{
			name: "nil request",
			resp: &BifrostImageGenerationResponse{Data: []ImageData{{URL: "u"}, {URL: "v"}}},
			want: 2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.resp.BackfillParams(tc.req)
			out, err := Marshal(tc.resp)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var decoded struct {
				Data  []ImageData `json:"data"`
				Usage *struct {
					OutputImages *int `json:"output_images"`
				} `json:"usage"`
			}
			if err := Unmarshal(out, &decoded); err != nil {
				t.Fatalf("unmarshal %s: %v", out, err)
			}
			if decoded.Usage == nil || decoded.Usage.OutputImages == nil {
				t.Fatalf("usage.output_images missing from %s", out)
			}
			if got := *decoded.Usage.OutputImages; got != tc.want {
				t.Fatalf("usage.output_images = %d, want %d in %s", got, tc.want, out)
			}
		})
	}
}

// TXE-6438: the request-path cost of BackfillParams on a sync image response, which now writes
// usage.output_images. Each iteration builds a fresh response, as the provider does per request.
func BenchmarkImageResponseBackfillParams(b *testing.B) {
	req := &BifrostRequest{ImageGenerationRequest: &BifrostImageGenerationRequest{Model: "meta/muse-image-1.0"}}
	data := []ImageData{{B64JSON: "aQ=="}, {B64JSON: "aQ==", Index: 1}}
	b.Run("no usage", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			resp := &BifrostImageGenerationResponse{Data: data}
			resp.BackfillParams(req)
		}
	})
	b.Run("token usage", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			resp := &BifrostImageGenerationResponse{Data: data, Usage: &ImageUsage{InputTokens: 12, OutputTokens: 4160}}
			resp.BackfillParams(req)
		}
	})
	b.Run("vendor count", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			resp := &BifrostImageGenerationResponse{Data: data, Usage: &ImageUsage{OutputImages: new(2)}}
			resp.BackfillParams(req)
		}
	})
}

// TXE-7051 (contract C9): a sync image response of a vendor with a documented link lifetime
// carries expires_at = creation + lifetime, on generation and edit alike; a response without a
// link, or of a vendor with no documented lifetime, carries none.
func TestImageResponseExpiresAt(t *testing.T) {
	now := func() time.Time { return time.Unix(1_800_000_000, 0) }
	link := []ImageData{{URL: "https://example.com/1.png"}}
	cases := []struct {
		name     string
		provider ModelProvider
		resp     *BifrostImageGenerationResponse
		want     *int64
	}{
		{"vendor created plus 24 hours", BytePlus, &BifrostImageGenerationResponse{Created: 1_762_000_000, Data: link}, new(int64(1_762_000_000 + 86400))},
		{"no vendor created: the time the answer was read", DashScope, &BifrostImageGenerationResponse{Data: link}, new(int64(1_800_000_000 + 86400))},
		{"12 hour lifetime", Tencent, &BifrostImageGenerationResponse{Created: 1_762_000_000, Data: link}, new(int64(1_762_000_000 + 43200))},
		{"one link among bytes", MiniMax, &BifrostImageGenerationResponse{Created: 1_762_000_000, Data: []ImageData{{B64JSON: "aQ=="}, {URL: "https://example.com/2.png", Index: 1}}}, new(int64(1_762_000_000 + 86400))},
		{"bytes only", BytePlus, &BifrostImageGenerationResponse{Created: 1_762_000_000, Data: []ImageData{{B64JSON: "aQ=="}}}, nil},
		{"every image blocked", BytePlus, &BifrostImageGenerationResponse{Created: 1_762_000_000, Data: []ImageData{}}, nil},
		{"vendor with no documented lifetime", OpenAI, &BifrostImageGenerationResponse{Created: 1_762_000_000, Data: link}, nil},
		{"custom provider", ModelProvider("tkp_42"), &BifrostImageGenerationResponse{Created: 1_762_000_000, Data: link}, nil},
		{"an expiry the converter set is kept", BytePlus, &BifrostImageGenerationResponse{Created: 1_762_000_000, Data: link, ExpiresAt: new(int64(1_762_000_600))}, new(int64(1_762_000_600))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.resp.fillExpiresAt(tc.provider, now)
			switch {
			case tc.want == nil && tc.resp.ExpiresAt != nil:
				t.Fatalf("expires_at = %d, want none", *tc.resp.ExpiresAt)
			case tc.want != nil && (tc.resp.ExpiresAt == nil || *tc.resp.ExpiresAt != *tc.want):
				t.Fatalf("expires_at = %v, want %d", tc.resp.ExpiresAt, *tc.want)
			}
		})
	}

	t.Run("BackfillParams fills it on generation and edit, from the routed provider", func(t *testing.T) {
		for _, req := range []*BifrostRequest{
			{ImageGenerationRequest: &BifrostImageGenerationRequest{Provider: Tencent, Model: "hy-image-v3"}},
			{ImageEditRequest: &BifrostImageEditRequest{Provider: Tencent, Model: "hy-image-v3"}},
		} {
			resp := &BifrostImageGenerationResponse{Created: 1_762_000_000, Data: []ImageData{{URL: "https://example.com/1.png"}}}
			resp.BackfillParams(req)
			if resp.ExpiresAt == nil || *resp.ExpiresAt != 1_762_000_000+43200 {
				t.Fatalf("expires_at = %v, want created + 12 hours", resp.ExpiresAt)
			}
			out, err := MarshalSorted(resp)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(out), `"expires_at":1762043200`) {
				t.Fatalf("expires_at missing from the wire: %s", out)
			}
		}
	})
	t.Run("absent from the wire when unknown", func(t *testing.T) {
		resp := &BifrostImageGenerationResponse{Created: 1_762_000_000, Data: []ImageData{{URL: "https://example.com/1.png"}}}
		resp.BackfillParams(&BifrostRequest{ImageGenerationRequest: &BifrostImageGenerationRequest{Provider: OpenAI, Model: "gpt-image-1.5"}})
		out, err := MarshalSorted(resp)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(out), "expires_at") {
			t.Fatalf("expires_at present for a vendor with no documented lifetime: %s", out)
		}
	})
}

// imageLinkLifetimeInDocs matches a provider page line that documents how long an image result
// link works.
var imageLinkLifetimeInDocs = regexp.MustCompile(`(?i)image (urls?|links?)[^.|]*(expire|valid for)`)

// TXE-7051: the check that makes the next provider automatic. A provider page that documents an
// image link lifetime must have a row in ImageLinkLifetimes, or its responses would carry links
// with no expires_at.
func TestImageLinkLifetimesMatchProviderDocs(t *testing.T) {
	pages, err := filepath.Glob(filepath.Join("..", "..", "docs", "providers", "supported-providers", "*.mdx"))
	if err != nil || len(pages) == 0 {
		t.Skipf("provider pages not found from %s (err %v)", "core/schemas", err)
	}
	documented := 0
	for _, page := range pages {
		body, err := os.ReadFile(page)
		if err != nil {
			t.Fatal(err)
		}
		line := imageLinkLifetimeInDocs.Find(body)
		if line == nil {
			continue
		}
		documented++
		provider := ModelProvider(strings.TrimSuffix(filepath.Base(page), ".mdx"))
		if _, ok := ImageLinkLifetimes[provider]; !ok {
			t.Errorf("%s documents an image link lifetime (%q) but %s has no row in ImageLinkLifetimes", filepath.Base(page), line, provider)
		}
	}
	if documented == 0 {
		t.Error("no provider page documents an image link lifetime; the pattern no longer matches the pages it was written for")
	}
}

// BenchmarkImageResponseBackfillExpiresAt measures what fillExpiresAt adds to an image response:
// one map lookup for a vendor without a documented lifetime, and one allocation for one with it.
func BenchmarkImageResponseBackfillExpiresAt(b *testing.B) {
	for _, provider := range []ModelProvider{OpenAI, BytePlus} {
		b.Run(string(provider), func(b *testing.B) {
			resp := &BifrostImageGenerationResponse{Created: 1_762_000_000, Data: []ImageData{{URL: "https://example.com/1.png"}}}
			b.ReportAllocs()
			for b.Loop() {
				resp.ExpiresAt = nil
				resp.fillExpiresAt(provider, time.Now)
			}
		})
	}
}
