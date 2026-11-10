package openai

import (
	"bytes"
	"context"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	schemas "github.com/capsohq/bifrost/core/schemas"
)

// multipartFields reads a multipart body into its parts in order: name and value for a field,
// name and filename for a file.
type multipartPart struct {
	name, value, filename string
}

func readMultipartParts(t *testing.T, contentType string, body []byte) []multipartPart {
	t.Helper()
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		t.Fatalf("ParseMediaType(%q): %v", contentType, err)
	}
	reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	var parts []multipartPart
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			return parts
		}
		if err != nil {
			t.Fatalf("NextPart(): %v", err)
		}
		data, _ := io.ReadAll(part)
		p := multipartPart{name: part.FormName(), filename: part.FileName()}
		if p.filename == "" {
			p.value = string(data)
		}
		parts = append(parts, p)
	}
}

func imageEditWithExtras() *schemas.BifrostImageEditRequest {
	return &schemas.BifrostImageEditRequest{
		Provider: schemas.OpenAI,
		Model:    "gpt-image-1",
		Input: &schemas.ImageEditInput{
			Prompt: "add a hat",
			Images: []schemas.ImageInput{{Image: []byte("\x89PNG\r\n\x1a\nfake")}},
		},
		Params: &schemas.ImageEditParameters{
			Size:       schemas.Ptr("1024x1024"),
			Resolution: "2K",
			ExtraParams: map[string]interface{}{
				"resolution":  "2K",                                   // a unified field is still an extra until a provider converts it
				"watermark":   false,                                  // a JSON bool
				"strength":    0.35,                                   // a JSON number
				"style_ref":   map[string]interface{}{"b": 2, "a": 1}, // a JSON object
				"camera":      "fixed",                                // a multipart string, written verbatim
				"size":        "256x256",                              // collides with a typed field: the typed value wins
				"ignored_nil": nil,
			},
		},
	}
}

// TXE-6518 (contract C4 BF5): with passthrough on (the x-bf-passthrough-extra-params header, or
// the /v1/images/edits handler, which turns it on for every request), the multipart image edit sent
// upstream carries every extra param as a form field, keys sorted, before the file parts. With
// passthrough off it carries none, as before.
func TestImageEditMultipartPassthroughExtraParams(t *testing.T) {
	for _, tc := range []struct {
		name        string
		passthrough bool
		want        []multipartPart
	}{
		{
			name:        "header set",
			passthrough: true,
			want: []multipartPart{
				{name: "model", value: "gpt-image-1"},
				{name: "prompt", value: "add a hat"},
				{name: "size", value: "1024x1024"},
				{name: "camera", value: "fixed"},
				{name: "resolution", value: "2K"},
				{name: "strength", value: "0.35"},
				{name: "style_ref", value: `{"a":1,"b":2}`},
				{name: "watermark", value: "false"},
				{name: "image[]", filename: "image0.png"},
			},
		},
		{
			name:        "header absent",
			passthrough: false,
			want: []multipartPart{
				{name: "model", value: "gpt-image-1"},
				{name: "prompt", value: "add a hat"},
				{name: "size", value: "1024x1024"},
				{name: "image[]", filename: "image0.png"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotType string
			var gotBody []byte
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/images/edits" {
					t.Errorf("path = %s, want /v1/images/edits", r.URL.Path)
				}
				gotType = r.Header.Get("Content-Type")
				gotBody, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"created":1,"data":[{"b64_json":"aGk="}]}`))
			}))
			defer server.Close()

			provider := NewOpenAIProvider(&schemas.ProviderConfig{
				NetworkConfig: schemas.NetworkConfig{BaseURL: server.URL},
			}, testNoopLogger{})
			ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
			if tc.passthrough {
				ctx.SetValue(schemas.BifrostContextKeyPassthroughExtraParams, true)
			}

			_, bifrostErr := provider.ImageEdit(ctx, schemas.Key{Value: *schemas.NewSecretVar("test-key")}, imageEditWithExtras())
			if bifrostErr != nil {
				t.Fatalf("ImageEdit: %+v", bifrostErr.Error)
			}
			got := readMultipartParts(t, gotType, gotBody)
			if len(got) != len(tc.want) {
				t.Fatalf("parts = %+v\nwant  %+v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("part %d = %+v, want %+v\nall parts %+v", i, got[i], tc.want[i], got)
				}
			}
		})
	}
}

// The OpenAI images API has no resolution field. Until an OpenAI-shaped provider converts the
// unified tier, the JSON body is the one sent before the field existed: no typed resolution, the
// raw extra param (sent only with the passthrough header) unchanged.
func TestToOpenAIImageGenerationRequestDropsUnifiedResolution(t *testing.T) {
	params := &schemas.ImageGenerationParameters{
		Resolution:  "2K",
		ExtraParams: map[string]interface{}{"resolution": "2K"},
	}
	req := ToOpenAIImageGenerationRequest(&schemas.BifrostImageGenerationRequest{
		Provider: schemas.XAI,
		Model:    "grok-imagine-image",
		Input:    &schemas.ImageGenerationInput{Prompt: "a fox"},
		Params:   params,
	})
	body, err := schemas.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(body, []byte("resolution")) {
		t.Fatalf("typed resolution leaked into the OpenAI body: %s", body)
	}
	if req.ExtraParams["resolution"] != "2K" {
		t.Fatalf("extra resolution = %v, want it kept for the passthrough channel", req.ExtraParams["resolution"])
	}
	if params.Resolution != "2K" {
		t.Fatalf("the caller's request was mutated")
	}
}

func benchImageEditRequest(extras map[string]interface{}) *OpenAIImageEditRequest {
	return &OpenAIImageEditRequest{
		Model: "gpt-image-1",
		Input: &schemas.ImageEditInput{
			Prompt: "add a hat",
			Images: []schemas.ImageInput{{Image: bytes.Repeat([]byte{0x89, 'P', 'N', 'G'}, 1024)}},
		},
		ImageEditParameters: schemas.ImageEditParameters{
			N:    schemas.Ptr(1),
			Size: schemas.Ptr("1024x1024"),
		},
		ExtraParams: extras,
	}
}

// BenchmarkImageEditMultipartWrite measures the multipart image edit body (TXE-6518 BF5). The
// /v1/images/edits handler turns passthrough on for every request, so "no extras" is the common
// case and must cost only the empty-map check.
func BenchmarkImageEditMultipartWrite(b *testing.B) {
	cases := []struct {
		name        string
		passthrough bool
		extras      map[string]interface{}
	}{
		{"passthrough off", false, map[string]interface{}{"watermark": "false", "camera": "fixed", "seed_mode": "fixed"}},
		{"no extras", true, nil},
		{"three string extras", true, map[string]interface{}{"watermark": "false", "camera": "fixed", "seed_mode": "fixed"}},
		{"string and object extras", true, map[string]interface{}{"camera": "fixed", "strength": 0.35, "style_ref": map[string]interface{}{"a": 1, "b": "x"}}},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			req := benchImageEditRequest(tc.extras)
			var body bytes.Buffer
			b.ReportAllocs()
			for b.Loop() {
				body.Reset()
				writer := multipart.NewWriter(&body)
				if err := parseImageEditFormDataBodyFromRequest(writer, req, schemas.OpenAI, tc.passthrough); err != nil {
					b.Fatal(err.Error)
				}
			}
		})
	}
}

// BenchmarkImageEditRequestBody measures the image edit body the handlers build (TXE-6528). "form,
// written inline" is what the handlers did before: a fresh buffer and multipart writer per request.
// "form" is the same edit through buildImageEditRequestBody, whose only added work for a bytes
// input is the scan for a URL-only image. "json, url inputs" is the new body for link inputs.
func BenchmarkImageEditRequestBody(b *testing.B) {
	urlReq := benchImageEditRequest(nil)
	urlReq.Input.Images = []schemas.ImageInput{{URL: "https://example.com/a.png"}, {URL: "https://example.com/b.png"}}
	b.Run("form, written inline", func(b *testing.B) {
		req := benchImageEditRequest(nil)
		b.ReportAllocs()
		for b.Loop() {
			var body bytes.Buffer
			writer := multipart.NewWriter(&body)
			if err := parseImageEditFormDataBodyFromRequest(writer, req, schemas.OpenAI, true); err != nil {
				b.Fatal(err.Error)
			}
			_, _ = body.Bytes(), writer.FormDataContentType()
		}
	})
	for _, tc := range []struct {
		name string
		req  *OpenAIImageEditRequest
	}{
		{"form", benchImageEditRequest(nil)},
		{"json, url inputs", urlReq},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, _, err := buildImageEditRequestBody(tc.req, schemas.OpenAI, true); err != nil {
					b.Fatal(err.Error)
				}
			}
		})
	}
}

// BenchmarkImageGenerationFieldCheck measures the check every OpenAI and Azure image generation
// request now passes before its body is built (TXE-6528): a few nil comparisons, no allocation.
func BenchmarkImageGenerationFieldCheck(b *testing.B) {
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	for _, tc := range []struct {
		name string
		req  *schemas.BifrostImageGenerationRequest
	}{
		{"no params", &schemas.BifrostImageGenerationRequest{Provider: schemas.OpenAI}},
		{"deliverable params", &schemas.BifrostImageGenerationRequest{Provider: schemas.OpenAI, Params: &schemas.ImageGenerationParameters{N: schemas.Ptr(2), Size: schemas.Ptr("1024x1024")}}},
		{"another provider", &schemas.BifrostImageGenerationRequest{Provider: schemas.XAI, Params: &schemas.ImageGenerationParameters{AspectRatio: schemas.Ptr("16:9")}}},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if err := checkOpenAIImageGenerationFields(ctx, tc.req); err != nil {
					b.Fatal(err.Error)
				}
			}
		})
	}
}

// imageTestProvider returns an OpenAI provider pointed at an upstream that records how many calls
// it got and the last request's Content-Type and body, and answers with status and body.
func imageTestProvider(t *testing.T, status int, responseBody string) (provider *OpenAIProvider, calls *int, gotType *string, gotBody *[]byte) {
	t.Helper()
	calls, gotType, gotBody = new(int), new(string), new([]byte)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls++
		*gotType = r.Header.Get("Content-Type")
		*gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(responseBody))
	}))
	t.Cleanup(server.Close)
	provider = NewOpenAIProvider(&schemas.ProviderConfig{
		NetworkConfig: schemas.NetworkConfig{BaseURL: server.URL},
	}, testNoopLogger{})
	return provider, calls, gotType, gotBody
}

func imageTestKey() schemas.Key {
	return schemas.Key{Value: *schemas.NewSecretVar("test-key")}
}

const imageTestOKBody = `{"created":1,"data":[{"b64_json":"aGk="}]}`

// TXE-6528 (contract C4): OpenAI's images API has no aspect_ratio, input_images, resolution or
// type. A generation request carrying one is answered 400 caller_unsupported_parameter naming the
// field, and no upstream call is made. Before, the field was sent to OpenAI under a name it does
// not know (or, for resolution, dropped).
func TestImageGenerationRejectsUndeliverableFields(t *testing.T) {
	for _, tc := range []struct {
		param  string
		params schemas.ImageGenerationParameters
	}{
		{"aspect_ratio", schemas.ImageGenerationParameters{AspectRatio: schemas.Ptr("16:9")}},
		{"input_images", schemas.ImageGenerationParameters{InputImages: []string{"https://example.com/a.png"}}},
		{"resolution", schemas.ImageGenerationParameters{Resolution: "2K", ExtraParams: map[string]interface{}{"resolution": "2K"}}},
		{"type", schemas.ImageGenerationParameters{Type: schemas.Ptr("vectorize")}},
		// Several set: the first in alphabetical order is named, as schemas.CheckUnifiedFields does.
		{"aspect_ratio", schemas.ImageGenerationParameters{Type: schemas.Ptr("vectorize"), AspectRatio: schemas.Ptr("1:1"), N: schemas.Ptr(2)}},
	} {
		t.Run(tc.param, func(t *testing.T) {
			provider, calls, _, _ := imageTestProvider(t, http.StatusOK, imageTestOKBody)
			ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
			params := tc.params
			resp, bifrostErr := provider.ImageGeneration(ctx, imageTestKey(), &schemas.BifrostImageGenerationRequest{
				Provider: schemas.OpenAI,
				Model:    "gpt-image-1.5",
				Input:    &schemas.ImageGenerationInput{Prompt: "a fox"},
				Params:   &params,
			})
			if bifrostErr == nil {
				t.Fatalf("ImageGeneration succeeded (%+v), want a 400 naming %s", resp, tc.param)
			}
			if *calls != 0 {
				t.Fatalf("upstream calls = %d, want 0", *calls)
			}
			if bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != 400 {
				t.Fatalf("status = %v, want 400", bifrostErr.StatusCode)
			}
			if got := bifrostErr.ResolveErrorType(); got != schemas.ErrorTypeCallerUnsupportedParameter {
				t.Fatalf("error_type = %q, want %q", got, schemas.ErrorTypeCallerUnsupportedParameter)
			}
			if bifrostErr.Error == nil || bifrostErr.Error.Param != tc.param {
				t.Fatalf("error.param = %+v, want %q", bifrostErr.Error, tc.param)
			}

			// The streaming route refuses the same request before it opens a stream.
			streamParams := tc.params
			stream, streamErr := provider.ImageGenerationStream(ctx, nil, nil, imageTestKey(), &schemas.BifrostImageGenerationRequest{
				Provider: schemas.OpenAI,
				Model:    "gpt-image-1.5",
				Input:    &schemas.ImageGenerationInput{Prompt: "a fox"},
				Params:   &streamParams,
			})
			if stream != nil || streamErr == nil {
				t.Fatalf("ImageGenerationStream opened a stream (err %+v), want a 400 naming %s", streamErr, tc.param)
			}
			if streamErr.ResolveErrorType() != schemas.ErrorTypeCallerUnsupportedParameter || streamErr.Error == nil || streamErr.Error.Param != tc.param {
				t.Fatalf("stream error = %+v (%s), want caller_unsupported_parameter naming %q", streamErr.Error, streamErr.ExtraFields.ErrorType, tc.param)
			}
			if *calls != 0 {
				t.Fatalf("upstream calls after the stream request = %d, want 0", *calls)
			}
		})
	}
}

// The check is for the two providers whose images API is OpenAI's own. A request with only fields
// OpenAI takes still makes its one call, the unpriced fields OpenAI lacks are still dropped, and
// the providers that share the converter, or a body Bifrost does not build, are left alone.
func TestImageGenerationFieldCheckScope(t *testing.T) {
	t.Run("deliverable fields make one call", func(t *testing.T) {
		provider, calls, _, gotBody := imageTestProvider(t, http.StatusOK, imageTestOKBody)
		ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
		_, bifrostErr := provider.ImageGeneration(ctx, imageTestKey(), &schemas.BifrostImageGenerationRequest{
			Provider: schemas.OpenAI,
			Model:    "gpt-image-1.5",
			Input:    &schemas.ImageGenerationInput{Prompt: "a fox"},
			Params: &schemas.ImageGenerationParameters{
				N: schemas.Ptr(2), Size: schemas.Ptr("1024x1024"), Quality: schemas.Ptr("low"),
				Background: schemas.Ptr("opaque"), OutputFormat: schemas.Ptr("png"),
				Seed: schemas.Ptr(7), NegativePrompt: schemas.Ptr("blur"),
			},
		})
		if bifrostErr != nil {
			t.Fatalf("ImageGeneration: %+v", bifrostErr.Error)
		}
		if *calls != 1 {
			t.Fatalf("upstream calls = %d, want 1", *calls)
		}
		want := `{"model":"gpt-image-1.5","prompt":"a fox","n":2,"background":"opaque","size":"1024x1024","quality":"low","output_format":"png"}`
		if string(*gotBody) != want {
			t.Fatalf("body = %s\nwant   %s", *gotBody, want)
		}
	})

	aspect := &schemas.ImageGenerationParameters{AspectRatio: schemas.Ptr("16:9")}
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	for _, provider := range []schemas.ModelProvider{schemas.OpenAI, schemas.Azure} {
		err := checkOpenAIImageGenerationFields(ctx, &schemas.BifrostImageGenerationRequest{Provider: provider, Params: aspect})
		if err == nil || err.Error.Param != "aspect_ratio" {
			t.Fatalf("%s: err = %+v, want aspect_ratio refused", provider, err)
		}
	}
	for _, provider := range []schemas.ModelProvider{schemas.XAI, "my-openai-compatible"} {
		if err := checkOpenAIImageGenerationFields(ctx, &schemas.BifrostImageGenerationRequest{Provider: provider, Params: aspect}); err != nil {
			t.Fatalf("%s: refused %+v, want it left to the provider", provider, err.Error)
		}
	}
	if err := checkOpenAIImageGenerationFields(ctx, &schemas.BifrostImageGenerationRequest{Provider: schemas.OpenAI}); err != nil {
		t.Fatalf("no params: refused %+v", err.Error)
	}
	rawCtx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	rawCtx.SetValue(schemas.BifrostContextKeyUseRawRequestBody, true)
	if err := checkOpenAIImageGenerationFields(rawCtx, &schemas.BifrostImageGenerationRequest{Provider: schemas.OpenAI, Params: aspect}); err != nil {
		t.Fatalf("raw request body: refused %+v, want the caller's body sent as is", err.Error)
	}
}

// openAIModerationBlockedBody is OpenAI's documented error for a gpt-image request its safety
// system blocked: HTTP 400, code moderation_blocked.
const openAIModerationBlockedBody = `{"error":{"message":"Your request was rejected by the safety system. If you believe this is an error, contact us at help.openai.com and include the request ID req_0123456789abcdef.","type":"image_generation_user_error","param":null,"code":"moderation_blocked"}}`

// TXE-6528 (contract C6): a moderation block on generation or edit comes back with OpenAI's
// status 400 and its code, classified caller_content_filtered (the classification is the shared
// one TXE-6517 added: moderation_blocked is one of schemas' content-filter codes). One upstream
// call each: the block is about the content, so nothing is retried.
func TestImageModerationBlockedIsCallerContentFiltered(t *testing.T) {
	check := func(t *testing.T, bifrostErr *schemas.BifrostError, calls int) {
		t.Helper()
		if bifrostErr == nil {
			t.Fatal("want the vendor's moderation error")
		}
		if calls != 1 {
			t.Fatalf("upstream calls = %d, want 1", calls)
		}
		if bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != 400 {
			t.Fatalf("status = %v, want 400", bifrostErr.StatusCode)
		}
		if bifrostErr.IsBifrostError {
			t.Fatal("is_bifrost_error = true, want the vendor's error")
		}
		if bifrostErr.Error == nil || bifrostErr.Error.Code == nil || *bifrostErr.Error.Code != "moderation_blocked" {
			t.Fatalf("error.code = %+v, want moderation_blocked", bifrostErr.Error)
		}
		if bifrostErr.Error.Type == nil || *bifrostErr.Error.Type != "image_generation_user_error" {
			t.Fatalf("error.type = %v, want image_generation_user_error", bifrostErr.Error.Type)
		}
		if got := bifrostErr.ResolveErrorType(); got != schemas.ErrorTypeCallerContentFiltered {
			t.Fatalf("error_type = %q, want %q", got, schemas.ErrorTypeCallerContentFiltered)
		}
	}
	t.Run("generation", func(t *testing.T) {
		provider, calls, _, _ := imageTestProvider(t, http.StatusBadRequest, openAIModerationBlockedBody)
		ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
		_, bifrostErr := provider.ImageGeneration(ctx, imageTestKey(), &schemas.BifrostImageGenerationRequest{
			Provider: schemas.OpenAI,
			Model:    "gpt-image-1.5",
			Input:    &schemas.ImageGenerationInput{Prompt: "blocked"},
		})
		check(t, bifrostErr, *calls)
	})
	t.Run("edit", func(t *testing.T) {
		provider, calls, _, _ := imageTestProvider(t, http.StatusBadRequest, openAIModerationBlockedBody)
		ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
		_, bifrostErr := provider.ImageEdit(ctx, imageTestKey(), &schemas.BifrostImageEditRequest{
			Provider: schemas.OpenAI,
			Model:    "gpt-image-1.5",
			Input: &schemas.ImageEditInput{
				Prompt: "blocked",
				Images: []schemas.ImageInput{{Image: []byte("\x89PNG\r\n\x1a\nfake")}},
			},
		})
		check(t, bifrostErr, *calls)
	})
}

// TXE-6528 (contract C5): a two-image gpt-image-1.5 response yields usage.output_images 2 next to
// the vendor's token usage, for generation and edit. OpenAI reports tokens, not an image count;
// the count is the length of data[] (schemas' BackfillParams, which core runs on every image
// response, TXE-6438).
func TestImageResponseReportsOutputImages(t *testing.T) {
	const twoImages = `{"created":1762770000,"background":"opaque","output_format":"png","quality":"low","size":"1024x1024","data":[{"b64_json":"aGk="},{"b64_json":"aG8="}],"usage":{"input_tokens":12,"input_tokens_details":{"image_tokens":0,"text_tokens":12},"output_tokens":544,"output_tokens_details":{"image_tokens":544,"text_tokens":0},"total_tokens":556}}`
	check := func(t *testing.T, resp *schemas.BifrostImageGenerationResponse, req *schemas.BifrostRequest) {
		t.Helper()
		resp.BackfillParams(req)
		if resp.Usage == nil || resp.Usage.OutputImages == nil || *resp.Usage.OutputImages != 2 {
			t.Fatalf("usage.output_images = %+v, want 2", resp.Usage)
		}
		if resp.Usage.InputTokens != 12 || resp.Usage.OutputTokens != 544 || resp.Usage.TotalTokens != 556 {
			t.Fatalf("token usage = %+v, want 12/544/556", resp.Usage)
		}
		if resp.Usage.OutputTokensDetails == nil || resp.Usage.OutputTokensDetails.ImageTokens != 544 {
			t.Fatalf("output_tokens_details = %+v, want image_tokens 544", resp.Usage.OutputTokensDetails)
		}
		wire, err := schemas.Marshal(resp)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if !bytes.Contains(wire, []byte(`"output_images":2`)) {
			t.Fatalf("response JSON has no usage.output_images: %s", wire)
		}
	}
	t.Run("generation", func(t *testing.T) {
		provider, _, _, _ := imageTestProvider(t, http.StatusOK, twoImages)
		ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
		req := &schemas.BifrostImageGenerationRequest{
			Provider: schemas.OpenAI,
			Model:    "gpt-image-1.5",
			Input:    &schemas.ImageGenerationInput{Prompt: "two foxes"},
			Params:   &schemas.ImageGenerationParameters{N: schemas.Ptr(2)},
		}
		resp, bifrostErr := provider.ImageGeneration(ctx, imageTestKey(), req)
		if bifrostErr != nil {
			t.Fatalf("ImageGeneration: %+v", bifrostErr.Error)
		}
		check(t, resp, &schemas.BifrostRequest{ImageGenerationRequest: req})
	})
	t.Run("edit", func(t *testing.T) {
		provider, _, _, _ := imageTestProvider(t, http.StatusOK, twoImages)
		ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
		req := &schemas.BifrostImageEditRequest{
			Provider: schemas.OpenAI,
			Model:    "gpt-image-1.5",
			Input: &schemas.ImageEditInput{
				Prompt: "two hats",
				Images: []schemas.ImageInput{{Image: []byte("\x89PNG\r\n\x1a\nfake")}},
			},
			Params: &schemas.ImageEditParameters{N: schemas.Ptr(2)},
		}
		resp, bifrostErr := provider.ImageEdit(ctx, imageTestKey(), req)
		if bifrostErr != nil {
			t.Fatalf("ImageEdit: %+v", bifrostErr.Error)
		}
		check(t, resp, &schemas.BifrostRequest{ImageEditRequest: req})
	})
}

// TXE-6528 (contract C4): an edit whose input images are URLs reaches OpenAI as its JSON edit
// body with images[].image_url equal to the URLs, in one upstream call, and Bifrost never fetches
// the URLs. Before, each URL was written as an empty multipart file part, which OpenAI rejects.
func TestImageEditURLInputsAreSentAsLinks(t *testing.T) {
	imageHostHits := 0
	imageHost := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		imageHostHits++
		w.WriteHeader(http.StatusOK)
	}))
	defer imageHost.Close()
	urlA, urlB := imageHost.URL+"/a.png?sig=1&x=y", imageHost.URL+"/b.webp"
	png := []byte("\x89PNG\r\n\x1a\nfake")
	const pngDataURL = "data:image/png;base64,iVBORw0KGgpmYWtl"

	for _, tc := range []struct {
		name        string
		passthrough bool
		input       []schemas.ImageInput
		params      *schemas.ImageEditParameters
		want        string
	}{
		{
			name:  "urls only",
			input: []schemas.ImageInput{{URL: urlA}, {URL: urlB}},
			want:  `{"model":"gpt-image-1.5","prompt":"add a hat","images":[{"image_url":"` + urlA + `"},{"image_url":"` + urlB + `"}]}`,
		},
		{
			name:  "scalar fields, and extras kept out without the passthrough switch",
			input: []schemas.ImageInput{{URL: urlA}},
			params: &schemas.ImageEditParameters{
				N: schemas.Ptr(2), Size: schemas.Ptr("1024x1024"), Quality: schemas.Ptr("low"),
				Background: schemas.Ptr("opaque"), InputFidelity: schemas.Ptr("high"),
				OutputFormat: schemas.Ptr("webp"), OutputCompression: schemas.Ptr(80), User: schemas.Ptr("u1"),
				ExtraParams: map[string]interface{}{"moderation": "low"},
			},
			want: `{"model":"gpt-image-1.5","prompt":"add a hat","images":[{"image_url":"` + urlA + `"}],"n":2,"size":"1024x1024","quality":"low","background":"opaque","input_fidelity":"high","output_format":"webp","output_compression":80,"user":"u1"}`,
		},
		{
			name:        "extras merged with the passthrough switch, a typed field never overridden",
			passthrough: true,
			input:       []schemas.ImageInput{{URL: urlA}},
			params: &schemas.ImageEditParameters{
				Size:        schemas.Ptr("1024x1024"),
				ExtraParams: map[string]interface{}{"moderation": "low", "size": "256x256", "images": "x"},
			},
			want: `{"model":"gpt-image-1.5","prompt":"add a hat","images":[{"image_url":"` + urlA + `"}],"size":"1024x1024","moderation":"low"}`,
		},
		{
			name:   "bytes beside a url, and a mask, go as data urls",
			input:  []schemas.ImageInput{{Image: png}, {URL: urlA}},
			params: &schemas.ImageEditParameters{Mask: png},
			want:   `{"model":"gpt-image-1.5","prompt":"add a hat","images":[{"image_url":"` + pngDataURL + `"},{"image_url":"` + urlA + `"}],"mask":{"image_url":"` + pngDataURL + `"}}`,
		},
		{
			name:  "a data url the caller sent is passed on unchanged",
			input: []schemas.ImageInput{{URL: pngDataURL}},
			want:  `{"model":"gpt-image-1.5","prompt":"add a hat","images":[{"image_url":"` + pngDataURL + `"}]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider, calls, gotType, gotBody := imageTestProvider(t, http.StatusOK, imageTestOKBody)
			ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
			if tc.passthrough {
				ctx.SetValue(schemas.BifrostContextKeyPassthroughExtraParams, true)
			}
			_, bifrostErr := provider.ImageEdit(ctx, imageTestKey(), &schemas.BifrostImageEditRequest{
				Provider: schemas.OpenAI,
				Model:    "gpt-image-1.5",
				Input:    &schemas.ImageEditInput{Prompt: "add a hat", Images: tc.input},
				Params:   tc.params,
			})
			if bifrostErr != nil {
				t.Fatalf("ImageEdit: %+v", bifrostErr.Error)
			}
			if *calls != 1 {
				t.Fatalf("upstream calls = %d, want 1", *calls)
			}
			if *gotType != "application/json" {
				t.Fatalf("Content-Type = %q, want application/json", *gotType)
			}
			if string(*gotBody) != tc.want {
				t.Fatalf("body = %s\nwant   %s", *gotBody, tc.want)
			}
		})
	}
	if imageHostHits != 0 {
		t.Fatalf("Bifrost fetched an input image URL %d times, want 0", imageHostHits)
	}
}

// A streaming edit whose input images are links takes the same JSON body with stream set, through
// ImageEditStream to the upstream and back: one call, application/json, the links unchanged, and
// the partial and completed events delivered with OpenAI's usage on the completed one.
func TestImageEditStreamURLInputsAreSentAsLinks(t *testing.T) {
	imageHostHits := 0
	imageHost := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		imageHostHits++
	}))
	defer imageHost.Close()
	urlA, urlB := imageHost.URL+"/a.png?sig=1&x=y", imageHost.URL+"/b.webp"

	const sse = "event: image_edit.partial_image\ndata: " + `{"type":"image_edit.partial_image","b64_json":"aGk=","partial_image_index":0,"sequence_number":1}` + "\n\n" +
		"event: image_edit.completed\ndata: " + `{"type":"image_edit.completed","b64_json":"aGk=","sequence_number":2,"usage":{"input_tokens":12,"output_tokens":272,"total_tokens":284}}` + "\n\n"
	calls, gotType, gotAccept, gotPath := 0, "", "", ""
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		gotType, gotAccept, gotPath = r.Header.Get("Content-Type"), r.Header.Get("Accept"), r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sse))
	}))
	defer server.Close()

	provider := NewOpenAIProvider(&schemas.ProviderConfig{
		NetworkConfig: schemas.NetworkConfig{BaseURL: server.URL},
	}, testNoopLogger{})
	stream, bifrostErr := provider.ImageEditStream(
		schemas.NewBifrostContext(context.Background(), schemas.NoDeadline), passthroughPostHook, nil, imageTestKey(),
		&schemas.BifrostImageEditRequest{
			Provider: schemas.OpenAI,
			Model:    "gpt-image-1.5",
			Input:    &schemas.ImageEditInput{Prompt: "add a hat", Images: []schemas.ImageInput{{URL: urlA}, {URL: urlB}}},
			Params:   &schemas.ImageEditParameters{PartialImages: schemas.Ptr(1)},
		})
	if bifrostErr != nil {
		t.Fatalf("ImageEditStream: %+v", bifrostErr.Error)
	}
	chunks := collectChunks(t, stream)

	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls)
	}
	if gotPath != "/v1/images/edits" {
		t.Fatalf("path = %q, want /v1/images/edits", gotPath)
	}
	if gotType != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", gotType)
	}
	if gotAccept != "text/event-stream" {
		t.Fatalf("Accept = %q, want text/event-stream", gotAccept)
	}
	want := `{"model":"gpt-image-1.5","prompt":"add a hat","images":[{"image_url":"` + urlA + `"},{"image_url":"` + urlB + `"}],"stream":true,"partial_images":1}`
	if string(gotBody) != want {
		t.Fatalf("body = %s\nwant   %s", gotBody, want)
	}
	if imageHostHits != 0 {
		t.Fatalf("Bifrost fetched an input image URL %d times, want 0", imageHostHits)
	}

	if len(chunks) != 2 {
		t.Fatalf("chunks = %d, want 2 (partial, completed)", len(chunks))
	}
	for i, wantType := range []schemas.ImageEventType{schemas.ImageEditEventTypePartial, schemas.ImageEditEventTypeCompleted} {
		if chunks[i].BifrostError != nil {
			t.Fatalf("chunk %d is an error: %+v", i, chunks[i].BifrostError.Error)
		}
		got := chunks[i].BifrostImageGenerationStreamResponse
		if got == nil || got.Type != wantType || got.B64JSON != "aGk=" {
			t.Fatalf("chunk %d = %+v, want type %s with the image", i, got, wantType)
		}
	}
	usage := chunks[1].BifrostImageGenerationStreamResponse.Usage
	if usage == nil || usage.InputTokens != 12 || usage.OutputTokens != 272 || usage.TotalTokens != 284 {
		t.Fatalf("completed usage = %+v, want OpenAI's 12/272/284", usage)
	}
}

// The JSON edit body is OpenAI's. Another provider on the shared handler keeps the multipart form,
// and so does an OpenAI edit whose every image is bytes; a streaming edit sets stream in the JSON.
func TestImageEditBodyChoice(t *testing.T) {
	urlOnly := func() *OpenAIImageEditRequest {
		return &OpenAIImageEditRequest{
			Model: "gpt-image-1.5",
			Input: &schemas.ImageEditInput{Prompt: "add a hat", Images: []schemas.ImageInput{{URL: "https://example.com/a.png"}}},
		}
	}
	_, contentType, bifrostErr := buildImageEditRequestBody(urlOnly(), schemas.Azure, false)
	if bifrostErr != nil {
		t.Fatalf("azure: %+v", bifrostErr.Error)
	}
	if mediaType, _, _ := mime.ParseMediaType(contentType); mediaType != "multipart/form-data" {
		t.Fatalf("azure Content-Type = %q, want multipart/form-data", contentType)
	}

	bytesOnly := urlOnly()
	bytesOnly.Input.Images = []schemas.ImageInput{{Image: []byte("\x89PNG\r\n\x1a\nfake"), URL: "https://example.com/a.png"}}
	_, contentType, bifrostErr = buildImageEditRequestBody(bytesOnly, schemas.OpenAI, false)
	if bifrostErr != nil {
		t.Fatalf("bytes: %+v", bifrostErr.Error)
	}
	if mediaType, _, _ := mime.ParseMediaType(contentType); mediaType != "multipart/form-data" {
		t.Fatalf("bytes Content-Type = %q, want multipart/form-data", contentType)
	}

	stream := urlOnly()
	stream.Stream = schemas.Ptr(true)
	body, contentType, bifrostErr := buildImageEditRequestBody(stream, schemas.OpenAI, false)
	if bifrostErr != nil {
		t.Fatalf("stream: %+v", bifrostErr.Error)
	}
	want := `{"model":"gpt-image-1.5","prompt":"add a hat","images":[{"image_url":"https://example.com/a.png"}],"stream":true}`
	if contentType != "application/json" || string(body) != want {
		t.Fatalf("stream body (%s) = %s\nwant %s", contentType, body, want)
	}
}
