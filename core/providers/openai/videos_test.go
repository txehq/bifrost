package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	schemas "github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

// An uploaded source is carried as bytes; a referenced one keeps only the ID, with the provider
// suffix Bifrost stamps onto every video ID stripped back off before it reaches OpenAI.
func TestToOpenAIVideoEditRequest_SourceForms(t *testing.T) {
	uploaded, err := ToOpenAIVideoEditRequest(&schemas.BifrostVideoEditRequest{
		Model: "sora-2-pro",
		Input: &schemas.VideoEditInput{
			Prompt: "warm backlight",
			Video:  schemas.VideoInput{Video: []byte("\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00mp42isom")},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(uploaded.Video.Bytes) == 0 || uploaded.Video.ID != "" {
		t.Fatalf("uploaded source should carry bytes only, got %+v", uploaded.Video)
	}
	if uploaded.Prompt != "warm backlight" || uploaded.Model != "sora-2-pro" {
		t.Fatalf("prompt/model not carried: %+v", uploaded)
	}

	referenced, err := ToOpenAIVideoEditRequest(&schemas.BifrostVideoEditRequest{
		Input: &schemas.VideoEditInput{
			Prompt: "warm backlight",
			Video:  schemas.VideoInput{ID: "video_abc123:openai"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if referenced.Video.ID != "video_abc123" {
		t.Fatalf("video id = %q, want the provider suffix stripped", referenced.Video.ID)
	}
	if referenced.Model != "" {
		t.Fatalf("model must stay empty so OpenAI infers it from the source video, got %q", referenced.Model)
	}
}

// OpenAI's edit endpoint takes an uploaded file or a video ID. A URL has no equivalent, and it is
// the sole input rather than a droppable parameter, so it is rejected instead of ignored.
func TestToOpenAIVideoEditRequest_RejectsURL(t *testing.T) {
	_, err := ToOpenAIVideoEditRequest(&schemas.BifrostVideoEditRequest{
		Model: "sora-2-pro",
		Input: &schemas.VideoEditInput{
			Prompt: "warm backlight",
			Video:  schemas.VideoInput{URL: "https://example.com/clip.mp4"},
		},
	})
	if err == nil {
		t.Fatal("expected an error for a url source")
	}
	if !strings.Contains(err.Error(), "url") {
		t.Fatalf("error should name the unsupported form, got %q", err.Error())
	}
}

func TestToOpenAIVideoEditRequest_RequiresPromptAndSource(t *testing.T) {
	if _, err := ToOpenAIVideoEditRequest(&schemas.BifrostVideoEditRequest{
		Model: "sora-2-pro",
		Input: &schemas.VideoEditInput{Video: schemas.VideoInput{ID: "video_abc123"}},
	}); err == nil {
		t.Fatal("expected an error when prompt is missing")
	}
	if _, err := ToOpenAIVideoEditRequest(&schemas.BifrostVideoEditRequest{
		Model: "sora-2-pro",
		Input: &schemas.VideoEditInput{Prompt: "warm backlight"},
	}); err == nil {
		t.Fatal("expected an error when the source video is missing")
	}
}

// The multipart body must name the file part "video" and give it a real video content type, since
// OpenAI reads the source from that part.
func TestParseVideoEditFormDataBodyFromRequest(t *testing.T) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	bifrostErr := parseVideoEditFormDataBodyFromRequest(writer, &OpenAIVideoEditRequest{
		Prompt: "warm backlight",
		Model:  "sora-2-pro",
		Video:  OpenAIVideoEditVideoInput{Bytes: []byte("\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00mp42isom")},
	})
	if bifrostErr != nil {
		t.Fatalf("unexpected error: %v", bifrostErr)
	}

	form, err := multipart.NewReader(&body, writer.Boundary()).ReadForm(1 << 20)
	if err != nil {
		t.Fatalf("produced an unreadable multipart body: %v", err)
	}
	if got := form.Value["prompt"]; len(got) != 1 || got[0] != "warm backlight" {
		t.Fatalf("prompt field = %v", got)
	}
	if got := form.Value["model"]; len(got) != 1 || got[0] != "sora-2-pro" {
		t.Fatalf("model field = %v", got)
	}
	videoParts := form.File["video"]
	if len(videoParts) != 1 {
		t.Fatalf("expected exactly one 'video' file part, got %d", len(videoParts))
	}
	if ct := videoParts[0].Header.Get("Content-Type"); ct != "video/mp4" {
		t.Fatalf("video part content type = %q, want video/mp4", ct)
	}
	if fn := videoParts[0].Filename; !strings.HasSuffix(fn, ".mp4") {
		t.Fatalf("video part filename = %q, want an .mp4 extension", fn)
	}
}

// A model omitted for a referenced source must not be written as an empty form field.
func TestParseVideoEditFormDataBodyFromRequest_OmitsEmptyModel(t *testing.T) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if bifrostErr := parseVideoEditFormDataBodyFromRequest(writer, &OpenAIVideoEditRequest{
		Prompt: "warm backlight",
		Video:  OpenAIVideoEditVideoInput{Bytes: []byte("\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00mp42isom")},
	}); bifrostErr != nil {
		t.Fatalf("unexpected error: %v", bifrostErr)
	}
	form, err := multipart.NewReader(&body, writer.Boundary()).ReadForm(1 << 20)
	if err != nil {
		t.Fatalf("produced an unreadable multipart body: %v", err)
	}
	if _, ok := form.Value["model"]; ok {
		t.Fatalf("empty model must be omitted, got %v", form.Value["model"])
	}
}

// Inbound OpenAI-format requests split "provider/model" the same way the other video routes do.
func TestToBifrostVideoEditRequest(t *testing.T) {
	out := ToBifrostVideoEditRequest(&OpenAIVideoEditRequest{
		Prompt: "warm backlight",
		Model:  "openai/sora-2-pro",
		Video:  OpenAIVideoEditVideoInput{ID: "video_abc123"},
	})
	if out == nil {
		t.Fatal("expected a converted request")
	}
	if out.Provider != schemas.OpenAI || out.Model != "sora-2-pro" {
		t.Fatalf("provider/model = %q/%q, want openai/sora-2-pro", out.Provider, out.Model)
	}
	if out.Input == nil || out.Input.Video.ID != "video_abc123" || out.Input.Prompt != "warm backlight" {
		t.Fatalf("input not carried: %+v", out.Input)
	}
}

// The official SDKs send no model on the video edit route, so the provider the transport resolved
// from the query parameter, header, or video ID suffix is the only routing signal available.
func TestToBifrostVideoEditRequest_ProviderFallback(t *testing.T) {
	out := ToBifrostVideoEditRequest(&OpenAIVideoEditRequest{
		Prompt:   "make it grayscale",
		Video:    OpenAIVideoEditVideoInput{ID: "vid_123"},
		Provider: schemas.Runware,
	})
	if out.Provider != schemas.Runware {
		t.Fatalf("provider = %q, want runware", out.Provider)
	}
	if out.Model != "" {
		t.Fatalf("model should stay empty when none was sent, got %q", out.Model)
	}

	// An explicit "provider/model" string still wins over the resolved fallback.
	explicit := ToBifrostVideoEditRequest(&OpenAIVideoEditRequest{
		Prompt:   "make it grayscale",
		Video:    OpenAIVideoEditVideoInput{ID: "vid_123"},
		Model:    "replicate/some-model",
		Provider: schemas.Runware,
	})
	if explicit.Provider != schemas.Replicate || explicit.Model != "some-model" {
		t.Fatalf("model string should win, got provider=%q model=%q", explicit.Provider, explicit.Model)
	}
}

// TXE-7018: a video create whose extra params name the model is refused, in any spelling,
// before the form is written; any other extra param leaves the request as it was.
func TestParseVideoGenerationFormDataBodyFromRequest_RefusesAModelAmongExtraParams(t *testing.T) {
	// The refusal holds whether or not extra params are passed through to the form (TXE-6532).
	for _, passthrough := range []bool{false, true} {
		for _, param := range []string{"model", "Model", "MODEL"} {
			var body bytes.Buffer
			bifrostErr := parseVideoGenerationFormDataBodyFromRequest(multipart.NewWriter(&body), &OpenAIVideoGenerationRequest{
				Model: "sora-2", Prompt: "a dog", ExtraParams: map[string]interface{}{param: "sora-2-pro"},
			}, schemas.OpenAI, passthrough)
			if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != 400 ||
				bifrostErr.Error.Param != param || bifrostErr.ExtraFields.ErrorType != schemas.ErrorTypeCallerInvalidRequest {
				t.Errorf("passthrough=%v extra param %q: want a 400 caller_invalid_request naming it, got %+v", passthrough, param, bifrostErr)
			}
			if body.Len() != 0 {
				t.Errorf("passthrough=%v extra param %q: the form was written before the refusal", passthrough, param)
			}
		}
		var body bytes.Buffer
		if bifrostErr := parseVideoGenerationFormDataBodyFromRequest(multipart.NewWriter(&body), &OpenAIVideoGenerationRequest{
			Model: "sora-2", Prompt: "a dog", ExtraParams: map[string]interface{}{"watermark": false},
		}, schemas.OpenAI, passthrough); bifrostErr != nil {
			t.Fatalf("passthrough=%v: an ordinary extra param was refused: %+v", passthrough, bifrostErr)
		}
	}
}

// twoPageVideoList serves an OpenAI-shaped video list recorded in two pages: the first page ends
// at video_b and says more follow, and the page after video_b is the last. It records each query.
func twoPageVideoList(t *testing.T, path string, queries *[]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			http.NotFound(w, r)
			return
		}
		*queries = append(*queries, r.URL.Query().Encode())
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("after") {
		case "":
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"video_a","object":"video","model":"sora-2","status":"completed","created_at":1762700000},{"id":"video_b","object":"video","model":"sora-2","status":"in_progress","created_at":1762700100}],"first_id":"video_a","last_id":"video_b","has_more":true}`))
		case "video_b":
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"video_c","object":"video","model":"sora-2","status":"failed","created_at":1762700200}],"first_id":"video_c","last_id":"video_c","has_more":false}`))
		default:
			http.Error(w, `{"error":{"message":"unknown cursor"}}`, http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestVideoListForwardsFiltersAndPagesToTheEnd is contract C7 / BF15 on the OpenAI-shaped list
// (OpenAI itself, and Azure and Volcengine through the same handler): the vendor's filters reach
// it by their own names on every page, and following last_id as the after cursor walks the
// recorded two pages to the end, with each id carrying the provider suffix Tokun stores.
func TestVideoListForwardsFiltersAndPagesToTheEnd(t *testing.T) {
	var queries []string
	srv := twoPageVideoList(t, "/v1/videos", &queries)
	provider := &OpenAIProvider{client: &fasthttp.Client{}, networkConfig: schemas.NetworkConfig{BaseURL: srv.URL}}
	key := schemas.Key{Value: *schemas.NewSecretVar("sk-test")}

	request := &schemas.BifrostVideoListRequest{
		Provider: schemas.OpenAI,
		Limit:    schemas.Ptr(2),
		Order:    schemas.Ptr("asc"),
		Filters:  map[string][]string{"status": {"completed", "failed"}, "created_after": {"1762700000"}},
	}
	var ids []string
	for page := 1; ; page++ {
		if page > 3 {
			t.Fatal("paging did not end after the recorded two pages")
		}
		resp, bifrostErr := provider.VideoList(schemas.NewBifrostContext(context.Background(), schemas.NoDeadline), key, request)
		if bifrostErr != nil {
			t.Fatalf("page %d: %v", page, bifrostErr.Error)
		}
		for _, video := range resp.Data {
			ids = append(ids, video.ID)
		}
		if resp.HasMore == nil || !*resp.HasMore {
			break
		}
		request.After = resp.LastID
	}

	if got := strings.Join(ids, ","); got != "video_a:openai,video_b:openai,video_c:openai" {
		t.Fatalf("ids = %s", got)
	}
	want := []string{
		"created_after=1762700000&limit=2&order=asc&status=completed&status=failed",
		"after=video_b&created_after=1762700000&limit=2&order=asc&status=completed&status=failed",
	}
	if strings.Join(queries, " | ") != strings.Join(want, " | ") {
		t.Fatalf("vendor queries = %q, want %q", queries, want)
	}
}

// TestVideoDownloadStreamsOnRequest is contract C9 / BF20 on OpenAI's /videos/{id}/content: a
// request that asks for a stream gets the vendor's body to read, with the variant and the key's
// credentials on the one upstream call; a request that does not still gets the bytes.
func TestVideoDownloadStreamsOnRequest(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.RequestURI()+" "+r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "image/webp")
		_, _ = w.Write([]byte("THUMBNAIL"))
	}))
	t.Cleanup(srv.Close)
	provider := &OpenAIProvider{client: &fasthttp.Client{}, streamingClient: &fasthttp.Client{}, networkConfig: schemas.NetworkConfig{BaseURL: srv.URL}}
	key := schemas.Key{Value: *schemas.NewSecretVar("sk-test")}
	variant := schemas.VideoDownloadVariantThumbnail

	streamed, bifrostErr := provider.VideoDownload(schemas.NewBifrostContext(context.Background(), schemas.NoDeadline), key,
		&schemas.BifrostVideoDownloadRequest{Provider: schemas.OpenAI, ID: "video_a:openai", Variant: &variant, Stream: true})
	if bifrostErr != nil {
		t.Fatalf("streamed download: %v", bifrostErr.Error)
	}
	if streamed.Body == nil || streamed.Content != nil {
		t.Fatalf("a streamed download returns Body only, got body=%v content=%q", streamed.Body != nil, streamed.Content)
	}
	body, err := io.ReadAll(streamed.Body)
	if err != nil || streamed.Body.Close() != nil || string(body) != "THUMBNAIL" {
		t.Fatalf("streamed body = %q, %v", body, err)
	}
	if streamed.ContentType != "image/webp" || streamed.ContentLength != int64(len("THUMBNAIL")) || streamed.VideoID != "video_a:openai" {
		t.Fatalf("streamed response = %+v", streamed)
	}

	buffered, bifrostErr := provider.VideoDownload(schemas.NewBifrostContext(context.Background(), schemas.NoDeadline), key,
		&schemas.BifrostVideoDownloadRequest{Provider: schemas.OpenAI, ID: "video_a:openai"})
	if bifrostErr != nil || string(buffered.Content) != "THUMBNAIL" || buffered.Body != nil {
		t.Fatalf("buffered download = %+v, %v", buffered, bifrostErr)
	}

	want := "GET /v1/videos/video_a/content?variant=thumbnail Bearer sk-test"
	if len(calls) != 2 || calls[0] != want || calls[1] != "GET /v1/videos/video_a/content Bearer sk-test" {
		t.Fatalf("upstream calls = %q", calls)
	}
}

func benchmarkFillOpenAIVideoTaskObject(b *testing.B, base schemas.BifrostVideoGenerationResponse) {
	b.ReportAllocs()
	for b.Loop() {
		response := base
		fillOpenAIVideoTaskObject(&response)
	}
}

// BenchmarkFillOpenAIVideoTaskObject measures the task object step on create and query: a task
// still running is the common poll and allocates nothing.
func BenchmarkFillOpenAIVideoTaskObject(b *testing.B) {
	seconds := "8"
	b.Run("in_progress", func(b *testing.B) {
		benchmarkFillOpenAIVideoTaskObject(b, schemas.BifrostVideoGenerationResponse{Status: schemas.VideoStatusInProgress, Seconds: &seconds})
	})
	b.Run("completed", func(b *testing.B) {
		benchmarkFillOpenAIVideoTaskObject(b, schemas.BifrostVideoGenerationResponse{Status: schemas.VideoStatusCompleted, Seconds: &seconds})
	})
}

func benchmarkVideoGenerationForm(b *testing.B, extraParams map[string]any, passthrough bool) {
	request := &schemas.BifrostVideoGenerationRequest{
		Model:  "sora-2",
		Input:  &schemas.VideoGenerationInput{Prompt: "a fox"},
		Params: &schemas.VideoGenerationParameters{Seconds: schemas.Ptr("8"), Size: "1280x720", ExtraParams: extraParams},
	}
	b.ReportAllocs()
	for b.Loop() {
		openaiReq, err := ToOpenAIVideoGenerationRequest(request)
		if err != nil {
			b.Fatal(err)
		}
		var body bytes.Buffer
		if bifrostErr := parseVideoGenerationFormDataBodyFromRequest(multipart.NewWriter(&body), openaiReq, schemas.Azure, passthrough); bifrostErr != nil {
			b.Fatal(bifrostErr.Error.Message)
		}
	}
}

// BenchmarkVideoGenerationForm measures building the multipart create body. A request with no
// passthrough header skips the extra-param writer behind one bool check.
func BenchmarkVideoGenerationForm(b *testing.B) {
	b.Run("no_passthrough", func(b *testing.B) { benchmarkVideoGenerationForm(b, map[string]any{"vendor_hint": "high"}, false) })
	b.Run("passthrough_one_extra", func(b *testing.B) { benchmarkVideoGenerationForm(b, map[string]any{"vendor_hint": "high"}, true) })
}

// Every unified video field an /openai/v1/videos request sets is also kept in ExtraParams under
// its own name, as the native /v1/videos handler keeps it, for the converters that still read it
// there (fork, TXE-7126). The loop runs over schemas.UnifiedVideoGenerationFields, so a unified
// field added later fails here until it has a sample below and a copy in addUnifiedVideoFieldCopies.
func TestOpenAIVideoRequestKeepsUnifiedFieldCopies(t *testing.T) {
	samples := map[string]any{
		"aspect_ratio":     "9:16",
		"resolution":       "1080p",
		"last_frame":       "https://img.example.test/last.png",
		"reference_images": []string{"https://img.example.test/a.png"},
	}
	body := map[string]any{"model": "minimax/MiniMax-H3", "prompt": "a dog"}
	for _, name := range schemas.UnifiedVideoGenerationFields {
		sample, ok := samples[name]
		if !ok {
			t.Fatalf("unified video field %q has no sample here; add one, and a copy in addUnifiedVideoFieldCopies", name)
		}
		body[name] = sample
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	var req OpenAIVideoGenerationRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatal(err)
	}
	// A caller's own extra param under a unified name is kept, not replaced by the copy.
	req.ExtraParams = map[string]any{"resolution": "1080p", "prompt_optimizer": false}
	req.VideoGenerationParameters.ExtraParams = req.ExtraParams

	bifrostReq := req.ToBifrostVideoGenerationRequest(nil)
	extra := bifrostReq.Params.ExtraParams
	for _, name := range schemas.UnifiedVideoGenerationFields {
		got, err := json.Marshal(extra[name])
		if err != nil {
			t.Fatal(err)
		}
		want, _ := json.Marshal(samples[name])
		if string(got) != string(want) {
			t.Errorf("ExtraParams[%q] = %s, want the compatibility copy %s", name, got, want)
		}
	}
	if extra["prompt_optimizer"] != false {
		t.Errorf("the caller's own extra param was lost: %v", extra)
	}

	// A request that sets no unified field gets no ExtraParams at all.
	plain := OpenAIVideoGenerationRequest{Model: "openai/sora-2", Prompt: "a dog"}
	if got := plain.ToBifrostVideoGenerationRequest(nil).Params.ExtraParams; got != nil {
		t.Errorf("ExtraParams = %v, want nil when no unified field is set", got)
	}
}
