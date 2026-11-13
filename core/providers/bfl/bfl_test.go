package bfl

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/capsohq/bifrost/core/network/proxytest"
	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	schemas "github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

func newTestProvider(t *testing.T, network schemas.NetworkConfig) *BFLProvider {
	t.Helper()
	provider, err := NewBFLProvider(&schemas.ProviderConfig{NetworkConfig: network}, nil)
	if err != nil {
		t.Fatalf("NewBFLProvider: %v", err)
	}
	return provider
}

// TestBFLBase pins the global endpoint as the default and that a regional base replaces it.
func TestBFLBase(t *testing.T) {
	ctx := schemas.NewBifrostContext(t.Context(), schemas.NoDeadline)
	if got := newTestProvider(t, schemas.NetworkConfig{}).requestURL(ctx, "/v1/flux-2-pro"); got != "https://api.bfl.ai/v1/flux-2-pro" {
		t.Errorf("default URL = %q, want the global endpoint", got)
	}
	if got := newTestProvider(t, schemas.NetworkConfig{BaseURL: "https://api.eu.bfl.ai/"}).requestURL(ctx, "/v1/flux-2-pro"); got != "https://api.eu.bfl.ai/v1/flux-2-pro" {
		t.Errorf("regional URL = %q, want the configured base", got)
	}
}

// TestBFLRequestSendsXKeyToConfiguredBase pins, against a recording server, that a request
// the provider builds goes to the configured base with the key in x-key, no Authorization
// header, and that an extra header named x-key cannot replace the key.
func TestBFLRequestSendsXKeyToConfiguredBase(t *testing.T) {
	var mu sync.Mutex
	var gotPath, gotKey, gotAuth, gotTrace string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		gotPath, gotKey, gotAuth, gotTrace = r.URL.Path, r.Header.Get("x-key"), r.Header.Get("Authorization"), r.Header.Get("X-Trace")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	provider := newTestProvider(t, schemas.NetworkConfig{
		BaseURL:             server.URL + "/",
		AllowPrivateNetwork: true,
		ExtraHeaders: map[string]schemas.SecretVar{
			"x-key":   *schemas.NewSecretVar("extra-key"),
			"X-Trace": *schemas.NewSecretVar("t-1"),
		},
	})
	ctx := schemas.NewBifrostContext(t.Context(), schemas.NoDeadline)
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)
	req.SetRequestURI(provider.requestURL(ctx, "/v1/flux-2-pro"))
	req.Header.SetMethod(http.MethodPost)
	provider.setRequestHeaders(ctx, req, schemas.Key{Value: *schemas.NewSecretVar("bfl-key")})

	if err := provider.client.Do(req, resp); err != nil {
		t.Fatalf("request to the recording server failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if gotPath != "/v1/flux-2-pro" {
		t.Errorf("path = %q, want /v1/flux-2-pro on the configured base", gotPath)
	}
	if gotKey != "bfl-key" {
		t.Errorf("x-key = %q, want the key", gotKey)
	}
	if gotAuth != "" {
		t.Errorf("Authorization = %q, want none", gotAuth)
	}
	if gotTrace != "t-1" {
		t.Errorf("X-Trace = %q, want the extra header", gotTrace)
	}
}

// TestBFLOperationsAreUnsupported pins that every operation other than FLUX 3 Video answers the
// unsupported-operation error, naming its own request type, without calling upstream.
func TestBFLOperationsAreUnsupported(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	provider := newTestProvider(t, schemas.NetworkConfig{BaseURL: server.URL, AllowPrivateNetwork: true})
	var _ schemas.Provider = provider

	iface := reflect.TypeOf((*schemas.Provider)(nil)).Elem()
	value := reflect.ValueOf(provider)
	for i := 0; i < iface.NumMethod(); i++ {
		method := iface.Method(i)
		// FLUX 3 Video (TXE-6617) is served; every other operation of the interface is not.
		switch method.Name {
		case "GetProviderKey", "VideoGeneration", "VideoRetrieve", "VideoDownload":
			continue
		}
		fn := value.MethodByName(method.Name)
		args := make([]reflect.Value, fn.Type().NumIn())
		for j := range args {
			args[j] = reflect.Zero(fn.Type().In(j))
		}
		out := fn.Call(args)
		bifrostErr, _ := out[len(out)-1].Interface().(*schemas.BifrostError)
		if bifrostErr == nil || bifrostErr.Error == nil || bifrostErr.Error.Code == nil || *bifrostErr.Error.Code != "unsupported_operation" {
			t.Errorf("%s: got %+v, want the unsupported-operation error", method.Name, bifrostErr)
			continue
		}
		if bifrostErr.ExtraFields.RequestType == "" || bifrostErr.ExtraFields.Provider != schemas.BFL {
			t.Errorf("%s: extra fields %+v, want provider bfl and the request type", method.Name, bifrostErr.ExtraFields)
		}
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("unsupported operations made %d upstream calls", n)
	}
}

// TestBFLErrorParsing pins that BFL's error bodies keep their status (a 429 stays 429),
// read FastAPI's detail in both shapes, and land in the right error_type family.
func TestBFLErrorParsing(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		body        string
		wantMessage string
		wantParam   string
		wantCode    string
		want        schemas.ErrorType
	}{
		{name: "too many active tasks", status: 429, body: `{"detail":"You have exceeded the number of active tasks."}`, wantMessage: "You have exceeded the number of active tasks.", want: schemas.ErrorTypeProviderRateLimited},
		{name: "insufficient credits", status: 402, body: `{"detail":"Insufficient credits"}`, wantMessage: "Insufficient credits", want: schemas.ErrorTypeProviderBilling},
		{name: "bad key", status: 403, body: `{"detail":"Not authenticated - Invalid Authentication"}`, wantMessage: "Not authenticated - Invalid Authentication", want: schemas.ErrorTypeProviderAuthFailed},
		{name: "validation list", status: 422, body: `{"detail":[{"loc":["body","width"],"msg":"Input should be a multiple of 16","type":"multiple_of"},{"loc":["body","steps"],"msg":"Input should be less than or equal to 50","type":"less_than_equal"}]}`,
			wantMessage: "width: Input should be a multiple of 16; steps: Input should be less than or equal to 50", wantParam: "width", wantCode: "multiple_of", want: schemas.ErrorTypeCallerInvalidRequest},
		{name: "server error", status: 500, body: `{"detail":"Internal Server Error"}`, wantMessage: "Internal Server Error", want: schemas.ErrorTypeProviderServerError},
		{name: "non-JSON body", status: 503, body: `upstream unavailable`, want: schemas.ErrorTypeProviderOverloaded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := fasthttp.AcquireResponse()
			defer fasthttp.ReleaseResponse(resp)
			resp.SetStatusCode(tc.status)
			resp.Header.SetContentType("application/json")
			resp.SetBodyString(tc.body)

			bifrostErr := parseBFLError(resp)

			if bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != tc.status {
				t.Fatalf("status = %v, want %d", bifrostErr.StatusCode, tc.status)
			}
			if bifrostErr.Error == nil || bifrostErr.Error.Message == "" {
				t.Fatalf("error = %+v, want a message", bifrostErr.Error)
			}
			if tc.wantMessage != "" && bifrostErr.Error.Message != tc.wantMessage {
				t.Errorf("message = %q, want %q", bifrostErr.Error.Message, tc.wantMessage)
			}
			if tc.wantParam != "" && bifrostErr.Error.Param != tc.wantParam {
				t.Errorf("param = %v, want %q", bifrostErr.Error.Param, tc.wantParam)
			}
			if tc.wantCode != "" && (bifrostErr.Error.Code == nil || *bifrostErr.Error.Code != tc.wantCode) {
				t.Errorf("code = %v, want %q", bifrostErr.Error.Code, tc.wantCode)
			}
			if got := schemas.ClassifyErrorType(bifrostErr, schemas.ImageGenerationRequest); got != tc.want {
				t.Errorf("error_type = %q, want %q", got, tc.want)
			}
		})
	}
}

// BenchmarkBFLUnsupportedOperation measures what a request routed to bfl costs while its
// operations are unsupported: one unsupported-operation error, no network call. The
// constructor runs once per provider config load and is not on the request path.
func BenchmarkBFLUnsupportedOperation(b *testing.B) {
	provider, err := NewBFLProvider(&schemas.ProviderConfig{}, nil)
	if err != nil {
		b.Fatal(err)
	}
	ctx := schemas.NewBifrostContext(b.Context(), schemas.NoDeadline)
	request := &schemas.BifrostImageGenerationRequest{Provider: schemas.BFL, Model: "flux-2-pro"}
	key := schemas.Key{Value: *schemas.NewSecretVar("bfl-key")}
	b.ReportAllocs()
	for b.Loop() {
		if _, bifrostErr := provider.ImageGeneration(ctx, key, request); bifrostErr == nil {
			b.Fatal("want the unsupported-operation error")
		}
	}
}

// --- Image tasks (TXE-6431) ---

func imageTaskCtx(t *testing.T) *schemas.BifrostContext {
	t.Helper()
	return schemas.NewBifrostContext(t.Context(), schemas.NoDeadline)
}

// marshalBody encodes a converted body the way the request path does, extra fields merged.
func marshalBody(t *testing.T, body *bflImageBody) map[string]any {
	t.Helper()
	data, err := providerUtils.MarshalConvertedRequestBody(body, true)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal %s: %v", data, err)
	}
	return out
}

// refusedParam returns the error.param of a converter refusal, or "" when err is not one.
func refusedParam(err error) string {
	bifrostErr, ok := providerUtils.AsBifrostBadRequestError(err)
	if !ok || bifrostErr.ExtraFields.ErrorType != schemas.ErrorTypeCallerUnsupportedParameter || bifrostErr.Error == nil {
		return ""
	}
	param, _ := bifrostErr.Error.Param.(string)
	return param
}

// TestBFLImageTaskConversion pins, per model, how the unified fields become BFL's fields (contract
// C4, B4, B5, B10): converted where the endpoint has a counterpart, refused with
// caller_unsupported_parameter where it has none, and the caller's extra fields merged unchanged.
func TestBFLImageTaskConversion(t *testing.T) {
	str := schemas.Ptr[string]
	num := schemas.Ptr[int]
	cases := []struct {
		name       string
		in         imageTaskInput
		want       map[string]any
		absent     []string
		wantRefuse string
	}{
		{
			name: "flux-pro-1.1: size to width/height, seed, output_format, callback_url to webhook_url, extras merged",
			in: imageTaskInput{model: "flux-pro-1.1", prompt: "a fox", size: str("1024x768"), seed: num(7), outputFormat: str("png"),
				callbackURL: str("https://hooks.tokun.sh/v/t1"), extra: map[string]any{"safety_tolerance": float64(2), "webhook_secret": "s3cret"}},
			want: map[string]any{"prompt": "a fox", "width": float64(1024), "height": float64(768), "seed": float64(7), "output_format": "png",
				"webhook_url": "https://hooks.tokun.sh/v/t1", "safety_tolerance": float64(2), "webhook_secret": "s3cret"},
		},
		{
			name: "flux-3-image: images array, aspect_ratio, resolution in BFL's lower case; seed dropped",
			in: imageTaskInput{model: "flux-3-image", prompt: "a fox", aspectRatio: str("16:9"), resolution: "2K", seed: num(7),
				images: []string{"https://example.com/a.png", "aGVsbG8="}},
			want:   map[string]any{"aspect_ratio": "16:9", "resolution": "2k", "images": []any{"https://example.com/a.png", "aGVsbG8="}},
			absent: []string{"seed", "input_image"},
		},
		{
			name: "flux-kontext-pro: input_images to input_image, input_image_2",
			in:   imageTaskInput{model: "flux-kontext-pro", prompt: "edit", images: []string{"https://example.com/a.png", "https://example.com/b.png"}},
			want: map[string]any{"input_image": "https://example.com/a.png", "input_image_2": "https://example.com/b.png"},
		},
		{
			name: "flux-dev: num_inference_steps to steps",
			in:   imageTaskInput{model: "flux-dev", prompt: "a fox", numInferenceSteps: num(30)},
			want: map[string]any{"steps": float64(30)},
		},
		{
			name: "flux-pro-1.0-fill: one image and the mask as base64",
			in:   imageTaskInput{model: "flux-pro-1.0-fill", prompt: "fill", images: []string{"aW1n"}, mask: []byte("mask")},
			want: map[string]any{"image": "aW1n", "mask": "bWFzaw=="},
		},
		{name: "n 2 is refused", in: imageTaskInput{model: "flux-pro-1.1", prompt: "a", n: num(2)}, wantRefuse: "n"},
		{name: "response_format b64_json is refused", in: imageTaskInput{model: "flux-pro-1.1", prompt: "a", responseFormat: str("b64_json")}, wantRefuse: "response_format"},
		{name: "callback_url on flux-3-image, which has no webhook_url, is refused", in: imageTaskInput{model: "flux-3-image", prompt: "a", callbackURL: str("https://hooks.tokun.sh/v/t1")}, wantRefuse: "callback_url"},
		{name: "size on kontext, which takes no width/height, is refused", in: imageTaskInput{model: "flux-kontext-pro", prompt: "a", size: str("1024x1024")}, wantRefuse: "size"},
		{name: "a malformed size is refused", in: imageTaskInput{model: "flux-pro-1.1", prompt: "a", size: str("large")}, wantRefuse: "size"},
		{name: "aspect_ratio on flux-pro-1.1 is refused", in: imageTaskInput{model: "flux-pro-1.1", prompt: "a", aspectRatio: str("1:1")}, wantRefuse: "aspect_ratio"},
		{name: "resolution on flux-pro-1.1 is refused", in: imageTaskInput{model: "flux-pro-1.1", prompt: "a", resolution: "1K"}, wantRefuse: "resolution"},
		{name: "output_format on flux-3-image is refused", in: imageTaskInput{model: "flux-3-image", prompt: "a", outputFormat: str("png")}, wantRefuse: "output_format"},
		{name: "an input image on flux-pro-1.1 is refused", in: imageTaskInput{model: "flux-pro-1.1", prompt: "a", images: []string{"aW1n"}}, wantRefuse: "input_images"},
		{name: "more input images than the model takes are refused", in: imageTaskInput{model: "flux-kontext-pro", prompt: "a", images: []string{"1", "2", "3", "4", "5"}}, wantRefuse: "input_images"},
		{name: "a mask on a model other than fill is refused", in: imageTaskInput{model: "flux-kontext-pro", prompt: "a", images: []string{"aW1n"}, mask: []byte("m")}, wantRefuse: "mask"},
		{name: "a model outside the table takes no converted field", in: imageTaskInput{model: "flux-future", prompt: "a", size: str("512x512")}, wantRefuse: "size"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, err := toBFLImageBody(tc.in)
			if tc.wantRefuse != "" {
				if got := refusedParam(err); got != tc.wantRefuse {
					t.Fatalf("refusal param = %q (err %v), want %q", got, err, tc.wantRefuse)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			got := marshalBody(t, body)
			for key, want := range tc.want {
				if !reflect.DeepEqual(got[key], want) {
					t.Errorf("%s = %#v, want %#v", key, got[key], want)
				}
			}
			for _, key := range tc.absent {
				if _, ok := got[key]; ok {
					t.Errorf("%s is present, want absent", key)
				}
			}
		})
	}
}

// TestBFLImageTaskInputs pins the two creates' input handling: a data URL's base64 payload, edit
// image bytes as base64 and edit URLs unchanged (contract C4 agreement four, B10).
func TestBFLImageTaskInputs(t *testing.T) {
	gen := generationTaskInput(&schemas.BifrostImageGenerationRequest{
		Model:  "flux-kontext-pro",
		Input:  &schemas.ImageGenerationInput{Prompt: "p"},
		Params: &schemas.ImageGenerationParameters{InputImages: []string{"data:image/png;base64,aGVsbG8=", "https://example.com/a.png"}},
	})
	if !reflect.DeepEqual(gen.images, []string{"aGVsbG8=", "https://example.com/a.png"}) {
		t.Errorf("generation images = %v", gen.images)
	}
	edit := editTaskInput(&schemas.BifrostImageEditRequest{
		Model: "flux-kontext-pro",
		Input: &schemas.ImageEditInput{Prompt: "p", Images: []schemas.ImageInput{{Image: []byte("img")}, {URL: "https://example.com/b.png"}}},
	})
	if !reflect.DeepEqual(edit.images, []string{"aW1n", "https://example.com/b.png"}) {
		t.Errorf("edit images = %v", edit.images)
	}
}

// TestBFLTaskID pins the task id: it carries the model and a regional polling host, polls only
// BFL's own hosts, and refuses an id that names any other host (contract C7, B3).
func TestBFLTaskID(t *testing.T) {
	const base = "https://api.bfl.ai"
	cases := []struct {
		name, pollingURL, want string
	}{
		{"regional host is recorded", "https://api.us1.bfl.ai/v1/get_result?id=t1", "t1|flux-3-image|api.us1.bfl.ai"},
		{"the configured base is not recorded", "https://api.bfl.ai/v1/get_result?id=t1", "t1|flux-3-image"},
		{"a host that is not BFL's is not recorded", "https://evil.example/v1/get_result?id=t1", "t1|flux-3-image"},
		{"plain http is not recorded", "http://api.us1.bfl.ai/v1/get_result?id=t1", "t1|flux-3-image"},
		{"no polling_url", "", "t1|flux-3-image"},
	}
	for _, tc := range cases {
		if got := encodeTaskID("t1", "flux-3-image", tc.pollingURL, base); got != tc.want {
			t.Errorf("%s: encodeTaskID = %q, want %q", tc.name, got, tc.want)
		}
	}

	ref, ok := decodeTaskID("t1|flux-3-image|api.us1.bfl.ai")
	if !ok || ref.pollURL(base) != "https://api.us1.bfl.ai/v1/get_result?id=t1" || ref.model != "flux-3-image" {
		t.Errorf("regional id: %+v ok=%v poll=%q", ref, ok, ref.pollURL(base))
	}
	ref, ok = decodeTaskID("t1")
	if !ok || ref.pollURL(base) != "https://api.bfl.ai/v1/get_result?id=t1" || ref.model != "" {
		t.Errorf("bare id: %+v ok=%v", ref, ok)
	}
	for _, crafted := range []string{"t1|flux-3-image|evil.example", "t1|m|api.bfl.ai.evil.example", "t1|m|127.0.0.1", ""} {
		if _, ok := decodeTaskID(crafted); ok {
			t.Errorf("decodeTaskID(%q) accepted a host that is not BFL's", crafted)
		}
	}
	// The id keeps no ":" of its own, so the route's "{id}:{provider}" split finds the provider.
	full := providerUtils.AddVideoIDProviderSuffix("t1|flux-3-image|api.us1.bfl.ai", schemas.BFL)
	if full != "t1%7Cflux-3-image%7Capi.us1.bfl.ai:bfl" {
		t.Errorf("full task id = %q", full)
	}
}

// TestBFLImageTaskStatuses pins the nine BFL statuses one to one, provider_status verbatim, the
// two Moderated states' content_filter and the error of every failed state (contract C6, C7, B7).
func TestBFLImageTaskStatuses(t *testing.T) {
	cases := []struct {
		raw        string
		want       schemas.TaskStatus
		wantError  bool
		wantFilter bool
	}{
		{"Pending", schemas.VideoStatusQueued, false, false},
		{"Reasoning", schemas.VideoStatusInProgress, false, false},
		{"Generating", schemas.VideoStatusInProgress, false, false},
		{"Ready", schemas.VideoStatusCompleted, false, false},
		{"Request Moderated", schemas.VideoStatusFailed, true, true},
		{"Content Moderated", schemas.VideoStatusFailed, true, true},
		{"No Card Generated", schemas.VideoStatusFailed, true, false},
		{"Error", schemas.VideoStatusFailed, true, false},
		{"Task not found", schemas.VideoStatusFailed, true, false},
		{"Some Future State", schemas.VideoStatusInProgress, false, false},
	}
	if len(statusTable) != 9 {
		t.Fatalf("status table has %d entries, want BFL's nine", len(statusTable))
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			result := &BFLResultResponse{ID: "t1", Status: tc.raw, Details: map[string]any{"Moderation Reasons": []any{"Derivative Works Filter"}}}
			task := toImageTask(result, bflTask{id: "t1", model: "flux-pro-1.1"})
			if task.Status != tc.want || task.ProviderStatus != tc.raw {
				t.Fatalf("status = %q / provider_status %q, want %q / %q", task.Status, task.ProviderStatus, tc.want, tc.raw)
			}
			if tc.wantError != (task.Error != nil) || (task.Error != nil && task.Error.Code != tc.raw) {
				t.Errorf("error = %+v, want code %q: %v", task.Error, tc.raw, tc.wantError)
			}
			if tc.wantFilter {
				if task.ContentFilter == nil || task.ContentFilter.FilteredCount != 1 || !reflect.DeepEqual(task.ContentFilter.Reasons, []string{"Derivative Works Filter"}) {
					t.Errorf("content_filter = %+v, want one filtered image and the vendor's reasons", task.ContentFilter)
				}
			} else if task.ContentFilter != nil {
				t.Errorf("content_filter = %+v, want none", task.ContentFilter)
			}
			if len(task.Data) != 0 {
				t.Errorf("data = %+v, want empty", task.Data)
			}
		})
	}
}

// TestBFLImageTaskReadyResult pins a completed task: the sample link as a url artifact,
// completed_at from end_time, expires_at one hour later for FLUX 3 and ten minutes later
// otherwise, nothing guessed without the model or end_time, and the settled cost in credits
// (contract C5, C9, B6, B8).
func TestBFLImageTaskReadyResult(t *testing.T) {
	result := &BFLResultResponse{
		ID: "t1", Status: "Ready", Cost: schemas.Ptr(4.5),
		Result: json.RawMessage(`{"sample":"https://delivery.bfl.ai/r/sample.jpeg?se=1","end_time":1700000000.25,"seed":7}`),
	}
	task := toImageTask(result, bflTask{id: "t1", model: "flux-3-image"})
	if len(task.Data) != 1 || task.Data[0].Type != schemas.VideoOutputTypeURL || *task.Data[0].URL != "https://delivery.bfl.ai/r/sample.jpeg?se=1" || task.Data[0].ContentType != "image/jpeg" {
		t.Fatalf("data = %+v", task.Data)
	}
	if task.CompletedAt == nil || *task.CompletedAt != 1700000000 {
		t.Fatalf("completed_at = %v", task.CompletedAt)
	}
	if task.ExpiresAt == nil || *task.ExpiresAt != 1700000000+3600 {
		t.Errorf("FLUX 3 expires_at = %v, want completed + 1h", task.ExpiresAt)
	}
	if task.Usage == nil || task.Usage.VendorCost == nil || task.Usage.VendorCost.Amount != 4.5 || task.Usage.VendorCost.Unit != schemas.VendorCostUnitCredit {
		t.Errorf("usage = %+v, want vendor_cost 4.5 credit", task.Usage)
	}
	if other := toImageTask(result, bflTask{id: "t1", model: "flux-pro-1.1"}); other.ExpiresAt == nil || *other.ExpiresAt != 1700000000+600 {
		t.Errorf("other model expires_at = %v, want completed + 10m", other.ExpiresAt)
	}
	if bare := toImageTask(result, bflTask{id: "t1"}); bare.ExpiresAt != nil {
		t.Errorf("bare id expires_at = %v, want none (the model is unknown)", bare.ExpiresAt)
	}
	noEnd := &BFLResultResponse{ID: "t1", Status: "Ready", Result: json.RawMessage(`{"sample":"https://x/s.png"}`)}
	if task := toImageTask(noEnd, bflTask{id: "t1", model: "flux-3-image"}); task.ExpiresAt != nil || task.CompletedAt != nil {
		t.Errorf("no end_time: completed_at %v expires_at %v, want none", task.CompletedAt, task.ExpiresAt)
	}
}

// bflRecorder is a recording BFL: it answers the submit and the get_result with canned bodies.
type bflRecorder struct {
	mu       sync.Mutex
	paths    []string
	bodies   [][]byte
	keys     []string
	submit   string
	result   string
	calls    atomic.Int32
	statusOn int
}

func (r *bflRecorder) serve(w http.ResponseWriter, req *http.Request) {
	r.calls.Add(1)
	body, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	r.paths = append(r.paths, req.URL.RequestURI())
	r.bodies = append(r.bodies, body)
	r.keys = append(r.keys, req.Header.Get("x-key"))
	r.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.statusOn != 0 {
		w.WriteHeader(r.statusOn)
		_, _ = w.Write([]byte(`{"detail":"You have exceeded the number of active tasks."}`))
		return
	}
	if req.Method == http.MethodGet {
		_, _ = w.Write([]byte(r.result))
		return
	}
	_, _ = w.Write([]byte(r.submit))
}

// TestBFLImageTaskSubmitAndQuery pins the two vendor calls against a recording server: the submit
// is one POST /v1/{model} with x-key and the converted body, and its task object carries the
// cost and megapixels; the query is one GET of get_result and maps the result (contract C7, B2,
// B3, B6).
func TestBFLImageTaskSubmitAndQuery(t *testing.T) {
	rec := &bflRecorder{}
	server := httptest.NewServer(http.HandlerFunc(rec.serve))
	defer server.Close()
	rec.submit = `{"id":"t1","polling_url":"` + server.URL + `/v1/get_result?id=t1","cost":4,"input_mp":0,"output_mp":1.05}`
	rec.result = `{"id":"t1","status":"Ready","result":{"sample":"https://delivery.bfl.ai/r/s.png","end_time":1700000000},"cost":4}`
	provider := newTestProvider(t, schemas.NetworkConfig{BaseURL: server.URL, AllowPrivateNetwork: true})
	key := schemas.Key{Value: *schemas.NewSecretVar("bfl-key")}

	task, bifrostErr := provider.ImageGenerationTask(imageTaskCtx(t), key, &schemas.BifrostImageGenerationRequest{
		Provider:    schemas.BFL,
		Model:       "flux-pro-1.1",
		Input:       &schemas.ImageGenerationInput{Prompt: "a fox"},
		Params:      &schemas.ImageGenerationParameters{Size: schemas.Ptr("1024x768"), ExtraParams: map[string]any{"webhook_secret": "s3cret"}},
		TaskControl: &schemas.TaskControlParameters{CallbackURL: schemas.Ptr("https://hooks.tokun.sh/v/t1")},
	})
	if bifrostErr != nil {
		t.Fatalf("submit: %+v", bifrostErr.Error)
	}
	if rec.paths[0] != "/v1/flux-pro-1.1" || rec.keys[0] != "bfl-key" {
		t.Fatalf("submit went to %q with x-key %q", rec.paths[0], rec.keys[0])
	}
	var sent map[string]any
	if err := json.Unmarshal(rec.bodies[0], &sent); err != nil {
		t.Fatalf("submit body: %v", err)
	}
	if sent["width"] != float64(1024) || sent["height"] != float64(768) || sent["webhook_url"] != "https://hooks.tokun.sh/v/t1" || sent["webhook_secret"] != "s3cret" || sent["prompt"] != "a fox" {
		t.Fatalf("submit body = %v", sent)
	}
	if task.ID != "t1%7Cflux-pro-1.1:bfl" || task.Status != schemas.VideoStatusQueued || task.Object != "image.task" || task.Model != "flux-pro-1.1" {
		t.Fatalf("created task = %+v", task)
	}
	if task.Usage == nil || task.Usage.VendorCost == nil || task.Usage.VendorCost.Amount != 4 || task.Usage.OutputMegapixels == nil || *task.Usage.OutputMegapixels != 1.05 || task.Usage.InputMegapixels == nil {
		t.Fatalf("created usage = %+v", task.Usage)
	}

	// The route hands the provider the vendor part, decoded, as GET /v1/images/tasks/{id} does.
	got, bifrostErr := provider.ImageTaskRetrieve(imageTaskCtx(t), key, &schemas.BifrostImageTaskRetrieveRequest{Provider: schemas.BFL, ID: "t1|flux-pro-1.1"})
	if bifrostErr != nil {
		t.Fatalf("query: %+v", bifrostErr.Error)
	}
	if rec.paths[1] != "/v1/get_result?id=t1" || rec.keys[1] != "bfl-key" {
		t.Fatalf("query went to %q with x-key %q", rec.paths[1], rec.keys[1])
	}
	if got.ID != task.ID || got.Status != schemas.VideoStatusCompleted || len(got.Data) != 1 || got.ExpiresAt == nil || *got.ExpiresAt != 1700000600 {
		t.Fatalf("queried task = %+v", got)
	}
	if n := rec.calls.Load(); n != 2 {
		t.Fatalf("upstream calls = %d, want one submit and one query", n)
	}
}

// TestBFLImageTaskRefusalsCallNothing pins that a refused field, a crafted task id and a model
// name that would add path segments make no upstream call, and that BFL's 429 keeps its status
// (contract C4, C6, B9).
func TestBFLImageTaskRefusalsCallNothing(t *testing.T) {
	rec := &bflRecorder{}
	server := httptest.NewServer(http.HandlerFunc(rec.serve))
	defer server.Close()
	provider := newTestProvider(t, schemas.NetworkConfig{BaseURL: server.URL, AllowPrivateNetwork: true})
	key := schemas.Key{Value: *schemas.NewSecretVar("bfl-key")}

	_, bifrostErr := provider.ImageGenerationTask(imageTaskCtx(t), key, &schemas.BifrostImageGenerationRequest{
		Model: "flux-pro-1.1", Input: &schemas.ImageGenerationInput{Prompt: "a"}, Params: &schemas.ImageGenerationParameters{N: schemas.Ptr(2)},
	})
	if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != 400 || bifrostErr.Error.Param != "n" || bifrostErr.ExtraFields.ErrorType != schemas.ErrorTypeCallerUnsupportedParameter {
		t.Fatalf("n 2: got %+v, want 400 caller_unsupported_parameter on n", bifrostErr)
	}
	_, bifrostErr = provider.ImageGenerationTask(imageTaskCtx(t), key, &schemas.BifrostImageGenerationRequest{
		Model: "../v1/credits", Input: &schemas.ImageGenerationInput{Prompt: "a"},
	})
	if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != 400 {
		t.Fatalf("model with a path: got %+v, want 400", bifrostErr)
	}
	_, bifrostErr = provider.ImageTaskRetrieve(imageTaskCtx(t), key, &schemas.BifrostImageTaskRetrieveRequest{ID: "t1|flux-pro-1.1|evil.example"})
	if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != 400 {
		t.Fatalf("crafted host: got %+v, want 400", bifrostErr)
	}
	if n := rec.calls.Load(); n != 0 {
		t.Fatalf("refusals made %d upstream calls", n)
	}

	rec.statusOn = http.StatusTooManyRequests
	_, bifrostErr = provider.ImageEditTask(imageTaskCtx(t), key, &schemas.BifrostImageEditRequest{
		Model: "flux-kontext-pro", Input: &schemas.ImageEditInput{Prompt: "a", Images: []schemas.ImageInput{{URL: "https://example.com/a.png"}}},
	})
	if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != 429 {
		t.Fatalf("429: got %+v, want the vendor's 429", bifrostErr)
	}
	if n := rec.calls.Load(); n != 1 {
		t.Fatalf("the 429 create made %d upstream calls, want exactly one", n)
	}
}

// BenchmarkBFLImageTaskConversion measures the request-body conversion of an image task create,
// the provider's own work on the request path (AGENTS.md cost rule).
func BenchmarkBFLImageTaskConversion(b *testing.B) {
	in := imageTaskInput{model: "flux-kontext-pro", prompt: "a fox", aspectRatio: schemas.Ptr("16:9"), seed: schemas.Ptr(7),
		images: []string{"https://example.com/a.png"}, callbackURL: schemas.Ptr("https://hooks.tokun.sh/v/t1")}
	b.ReportAllocs()
	for b.Loop() {
		body, err := toBFLImageBody(in)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := providerUtils.MarshalConvertedRequestBody(body, true); err != nil {
			b.Fatal(err)
		}
	}
}

// TXE-6431 review: the handlers keep resolution in the extras as well as in the typed field. The
// converted value is the one sent; the raw copy is dropped, or the merge would refuse "2K" against
// "2k" and a valid request would never reach BFL. The caller's extras are not modified.
func TestBFLImageTaskSendsTheConvertedResolutionOnly(t *testing.T) {
	rec := &bflRecorder{submit: `{"id":"t1","polling_url":"https://api.bfl.ai/v1/get_result?id=t1"}`}
	server := httptest.NewServer(http.HandlerFunc(rec.serve))
	defer server.Close()
	provider := newTestProvider(t, schemas.NetworkConfig{BaseURL: server.URL, AllowPrivateNetwork: true})
	extra := map[string]any{"resolution": "2K", "safety_tolerance": float64(3)}
	_, bifrostErr := provider.ImageGenerationTask(imageTaskCtx(t), schemas.Key{Value: *schemas.NewSecretVar("k")}, &schemas.BifrostImageGenerationRequest{
		Model: "flux-3-image", Input: &schemas.ImageGenerationInput{Prompt: "a fox"},
		Params: &schemas.ImageGenerationParameters{Resolution: "2K", ExtraParams: extra},
	})
	if bifrostErr != nil {
		t.Fatalf("submit: %+v", bifrostErr.Error)
	}
	var sent map[string]any
	if err := json.Unmarshal(rec.bodies[0], &sent); err != nil {
		t.Fatal(err)
	}
	if sent["resolution"] != "2k" || sent["safety_tolerance"] != float64(3) {
		t.Fatalf("sent %v, want resolution 2k and the extra field", sent)
	}
	if extra["resolution"] != "2K" {
		t.Fatalf("the caller's extras were modified: %v", extra)
	}
}

// TXE-6431 review (AGENTS.md): the converted body encodes with sorted keys, so identical requests
// are byte-identical.
func TestBFLImageBodyIsSorted(t *testing.T) {
	body, err := toBFLImageBody(imageTaskInput{model: "flux-pro-1.1", prompt: "p", size: schemas.Ptr("1024x768"), seed: schemas.Ptr(1), callbackURL: schemas.Ptr("https://h/x")})
	if err != nil {
		t.Fatal(err)
	}
	data, err := body.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"height":768,"prompt":"p","seed":1,"webhook_url":"https://h/x","width":1024}`; string(data) != want {
		t.Fatalf("body = %s, want %s", data, want)
	}
}

// --- FLUX 3 Video (TXE-6617) ---

func videoRequest(input *schemas.VideoGenerationInput, params *schemas.VideoGenerationParameters) *schemas.BifrostVideoGenerationRequest {
	if input == nil {
		input = &schemas.VideoGenerationInput{Prompt: "a paper boat"}
	}
	return &schemas.BifrostVideoGenerationRequest{Provider: schemas.BFL, Model: "flux-3-video", Input: input, Params: params}
}

// TestBFLVideoConversion pins the /v1/flux-3-video body (contract C4, B15): seconds as duration,
// the unified resolution tiers as BFL's, audio as generate_audio, the mode from the inputs unless
// the caller names one, extras merged unchanged and the raw copies of converted fields dropped.
func TestBFLVideoConversion(t *testing.T) {
	str := schemas.Ptr[string]
	ref := "https://example.test/first.png"
	cases := []struct {
		name       string
		req        *schemas.BifrostVideoGenerationRequest
		want       map[string]any
		absent     []string
		wantRefuse string
	}{
		{
			name: "text to video",
			req: videoRequest(nil, &schemas.VideoGenerationParameters{Seconds: str("5"), AspectRatio: "16:9", Resolution: "1080p", Audio: schemas.Ptr(false),
				ExtraParams: map[string]any{"draft": true, "safety_tolerance": float64(2), "resolution": "1080p", "aspect_ratio": "16:9"}}),
			want: map[string]any{"prompt": "a paper boat", "mode": "t2v", "duration": float64(5), "aspect_ratio": "16:9", "resolution": "fhd",
				"generate_audio": false, "draft": true, "safety_tolerance": float64(2)},
		},
		{
			name: "input_reference is the first keyframe, image to video",
			req:  videoRequest(&schemas.VideoGenerationInput{Prompt: "p", InputReference: &ref}, nil),
			want: map[string]any{"mode": "i2v", "keyframes": ref},
		},
		{
			name: "video_uri is the start video, video to video",
			req:  videoRequest(&schemas.VideoGenerationInput{Prompt: "p", VideoURI: str("https://example.test/in.mp4")}, nil),
			want: map[string]any{"mode": "v2v", "start_video": "https://example.test/in.mp4"},
		},
		{
			name:   "a mode the caller names is BFL's and wins",
			req:    videoRequest(nil, &schemas.VideoGenerationParameters{ExtraParams: map[string]any{"mode": "draft_enhance", "draft_cache": "c1"}}),
			want:   map[string]any{"mode": "draft_enhance", "draft_cache": "c1"},
			absent: []string{"keyframes"},
		},
		{name: "a fractional duration is refused", req: videoRequest(nil, &schemas.VideoGenerationParameters{Seconds: str("4.5")}), wantRefuse: "seconds"},
		{name: "a resolution BFL has no tier for is refused", req: videoRequest(nil, &schemas.VideoGenerationParameters{Resolution: "480p"}), wantRefuse: "resolution"},
		{
			name:       "input_reference and keyframes together are refused",
			req:        videoRequest(&schemas.VideoGenerationInput{Prompt: "p", InputReference: &ref}, &schemas.VideoGenerationParameters{ExtraParams: map[string]any{"keyframes": []any{"a", "b"}}}),
			wantRefuse: "input_reference",
		},
		// BFL's own names for seconds and audio are billed (TXE-6617): never sent without the
		// unified field, and with it the body is written from the unified field only.
		{name: "duration as an extra param without seconds is refused", req: videoRequest(nil, &schemas.VideoGenerationParameters{ExtraParams: map[string]any{"duration": float64(10)}}), wantRefuse: "duration"},
		{name: "generate_audio as an extra param without audio is refused", req: videoRequest(nil, &schemas.VideoGenerationParameters{ExtraParams: map[string]any{"generate_audio": true}}), wantRefuse: "generate_audio"},
		{
			name: "duration and generate_audio beside seconds and audio are written from the unified fields",
			req: videoRequest(nil, &schemas.VideoGenerationParameters{Seconds: str("5"), Audio: schemas.Ptr(true),
				ExtraParams: map[string]any{"duration": float64(5), "generate_audio": true}}),
			want: map[string]any{"duration": float64(5), "generate_audio": true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, err := toBFLVideoBody(tc.req)
			if tc.wantRefuse != "" {
				if got := refusedParam(err); got != tc.wantRefuse {
					t.Fatalf("refusal param = %q (err %v), want %q", got, err, tc.wantRefuse)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			got := marshalBody(t, body)
			for key, want := range tc.want {
				if !reflect.DeepEqual(got[key], want) {
					t.Errorf("%s = %#v, want %#v", key, got[key], want)
				}
			}
			for _, key := range tc.absent {
				if _, ok := got[key]; ok {
					t.Errorf("%s present, want absent", key)
				}
			}
		})
	}
}

// TestBFLVideoTask pins the query mapping (contract C6, C7, C9, B17): the nine statuses one to
// one with provider_status verbatim, content_filter for both Moderated states, the sample link,
// expires_at two hours after BFL's end time, the settled cost in credits.
func TestBFLVideoTask(t *testing.T) {
	ready := toVideoTask(&BFLResultResponse{ID: "v1", Status: "Ready", Cost: schemas.Ptr(30.0),
		Result: json.RawMessage(`{"sample":"https://delivery.bfl.ai/v/sample.mp4?se=1","end_time":1700000000}`)})
	if ready.Status != schemas.VideoStatusCompleted || ready.ProviderStatus != "Ready" {
		t.Fatalf("ready status = %q / %q", ready.Status, ready.ProviderStatus)
	}
	if len(ready.Videos) != 1 || *ready.Videos[0].URL != "https://delivery.bfl.ai/v/sample.mp4?se=1" || ready.Videos[0].ContentType != "video/mp4" || ready.Videos[0].Type != schemas.VideoOutputTypeURL {
		t.Fatalf("videos = %+v", ready.Videos)
	}
	if ready.ExpiresAt == nil || *ready.ExpiresAt != 1700000000+7200 {
		t.Errorf("expires_at = %v, want end_time + 2h", ready.ExpiresAt)
	}
	if ready.Usage == nil || ready.Usage.VendorCost == nil || ready.Usage.VendorCost.Amount != 30 || ready.Usage.VendorCost.Unit != schemas.VendorCostUnitCredit {
		t.Errorf("usage = %+v", ready.Usage)
	}
	for raw, want := range map[string]schemas.VideoStatus{
		"Pending": schemas.VideoStatusQueued, "Reasoning": schemas.VideoStatusInProgress, "Generating": schemas.VideoStatusInProgress,
		"Request Moderated": schemas.VideoStatusFailed, "Content Moderated": schemas.VideoStatusFailed, "No Card Generated": schemas.VideoStatusFailed,
		"Error": schemas.VideoStatusFailed, "Task not found": schemas.VideoStatusFailed,
	} {
		task := toVideoTask(&BFLResultResponse{ID: "v1", Status: raw, Details: map[string]any{"Moderation Reasons": []any{"r1"}}})
		if task.Status != want || task.ProviderStatus != raw || len(task.Videos) != 0 || task.ExpiresAt != nil {
			t.Errorf("%s: %+v", raw, task)
		}
		if moderated := moderatedStatuses[raw]; moderated != (task.ContentFilter != nil) {
			t.Errorf("%s: content_filter = %+v", raw, task.ContentFilter)
		}
		if want == schemas.VideoStatusFailed && (task.Error == nil || task.Error.Code != raw) {
			t.Errorf("%s: error = %+v", raw, task.Error)
		}
	}
	noEnd := toVideoTask(&BFLResultResponse{ID: "v1", Status: "Ready", Result: json.RawMessage(`{"sample":"https://x/s.mp4"}`)})
	if noEnd.ExpiresAt != nil {
		t.Errorf("no end_time: expires_at %v, want none", noEnd.ExpiresAt)
	}
}

// TestBFLVideoSubmitAndQuery pins the two calls against a recording server (B15 to B17): one POST
// /v1/flux-3-video, a task id that carries the model, then one GET of get_result.
func TestBFLVideoSubmitAndQuery(t *testing.T) {
	rec := &bflRecorder{}
	server := httptest.NewServer(http.HandlerFunc(rec.serve))
	defer server.Close()
	rec.submit = `{"id":"v1","polling_url":"` + server.URL + `/v1/get_result?id=v1","cost":30}`
	rec.result = `{"id":"v1","status":"Generating","progress":0.4}`
	provider := newTestProvider(t, schemas.NetworkConfig{BaseURL: server.URL, AllowPrivateNetwork: true})
	key := schemas.Key{Value: *schemas.NewSecretVar("bfl-key")}

	task, bifrostErr := provider.VideoGeneration(imageTaskCtx(t), key, videoRequest(nil, &schemas.VideoGenerationParameters{Seconds: schemas.Ptr("5")}))
	if bifrostErr != nil {
		t.Fatalf("create: %+v", bifrostErr.Error)
	}
	if rec.paths[0] != "/v1/flux-3-video" || rec.keys[0] != "bfl-key" {
		t.Fatalf("create went to %q", rec.paths[0])
	}
	if task.ID != "v1%7Cflux-3-video:bfl" || task.Status != schemas.VideoStatusQueued || task.Seconds == nil || *task.Seconds != "5" {
		t.Fatalf("created task = %+v", task)
	}
	if task.Usage == nil || task.Usage.VendorCost == nil || task.Usage.VendorCost.Amount != 30 {
		t.Fatalf("created usage = %+v", task.Usage)
	}
	got, bifrostErr := provider.VideoRetrieve(imageTaskCtx(t), key, &schemas.BifrostVideoRetrieveRequest{Provider: schemas.BFL, ID: "v1|flux-3-video"})
	if bifrostErr != nil {
		t.Fatalf("query: %+v", bifrostErr.Error)
	}
	if rec.paths[1] != "/v1/get_result?id=v1" || got.Status != schemas.VideoStatusInProgress || got.ProviderStatus != "Generating" || got.ID != task.ID {
		t.Fatalf("query %q -> %+v", rec.paths[1], got)
	}
	// Only the id the create handed out is a video id: a bare BFL id, or one naming an image
	// model, is refused before the key is sent anywhere (TXE-6617 review).
	for _, id := range []string{"v1", "v1:bfl", "v1|flux-pro-1.1", "v1|flux-3-image|api.us1.bfl.ai", "v1|FLUX-3-VIDEO"} {
		if _, bifrostErr := provider.VideoRetrieve(imageTaskCtx(t), key, &schemas.BifrostVideoRetrieveRequest{Provider: schemas.BFL, ID: id}); bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != 400 {
			t.Errorf("query of %q: got %+v, want 400", id, bifrostErr)
		}
		if _, bifrostErr := provider.VideoDownload(imageTaskCtx(t), key, &schemas.BifrostVideoDownloadRequest{Provider: schemas.BFL, ID: id}); bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != 400 {
			t.Errorf("download of %q: got %+v, want 400", id, bifrostErr)
		}
	}
	if len(rec.paths) != 2 {
		t.Fatalf("a refused id reached BFL: %q", rec.paths)
	}
}

// TestBFLVideoCreateIsSentOnce pins that the create reaches BFL exactly once, also when BFL's
// connection fails: the failure is returned, never retried by the provider. A model other than
// flux-3-video is refused with no call, and BFL has no cancel API, so no VideoCanceller.
func TestBFLVideoCreateIsSentOnce(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close() // the connection drops before any answer
		}
	}))
	defer server.Close()
	provider := newTestProvider(t, schemas.NetworkConfig{BaseURL: server.URL, AllowPrivateNetwork: true})
	key := schemas.Key{Value: *schemas.NewSecretVar("bfl-key")}

	if _, bifrostErr := provider.VideoGeneration(imageTaskCtx(t), key, videoRequest(nil, nil)); bifrostErr == nil {
		t.Fatal("a dropped connection must be an error")
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("the create reached BFL %d times, want exactly once", n)
	}
	other := videoRequest(nil, nil)
	other.Model = "flux-pro-1.1"
	if _, bifrostErr := provider.VideoGeneration(imageTaskCtx(t), key, other); bifrostErr == nil || *bifrostErr.StatusCode != 400 {
		t.Fatalf("non-video model: got %+v, want 400", bifrostErr)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("the refused model reached BFL")
	}
	if _, ok := any(provider).(schemas.VideoCanceller); ok {
		t.Fatal("BFL has no cancel API; the provider must not implement VideoCanceller")
	}
}

// TestBFLVideoRefusesTaskControlFields pins that FLUX 3 Video declares none of the four task
// control fields, so core refuses each before a key is chosen (contract C7, B15), and refuses
// the unified fields BFL has no counterpart for.
func TestBFLVideoRefusesTaskControlFields(t *testing.T) {
	provider := newTestProvider(t, schemas.NetworkConfig{})
	for _, field := range schemas.TaskControlFields {
		params := &schemas.VideoGenerationParameters{}
		switch field {
		case schemas.TaskControlFieldCallbackURL:
			params.CallbackURL = schemas.Ptr("https://hooks.example.test/v/1")
		case schemas.TaskControlFieldClientReferenceID:
			params.ClientReferenceID = schemas.Ptr("t1")
		case schemas.TaskControlFieldExpiresAfterSeconds:
			params.ExpiresAfterSeconds = schemas.Ptr(600)
		case schemas.TaskControlFieldOutputStorageURI:
			params.OutputStorageURI = schemas.Ptr("gs://b/t1")
		}
		req := &schemas.BifrostRequest{RequestType: schemas.VideoGenerationRequest, VideoGenerationRequest: videoRequest(nil, params)}
		if err := schemas.CheckTaskControlFields(provider, req); err == nil || err.Error.Param != field {
			t.Errorf("%s: got %+v, want caller_unsupported_parameter naming it", field, err)
		}
	}
	for _, params := range []*schemas.VideoGenerationParameters{{Size: "1280x720"}, {OutputFormat: schemas.Ptr("mp4")}, {LastFrame: "https://x/l.png"}} {
		req := &schemas.BifrostRequest{RequestType: schemas.VideoGenerationRequest, VideoGenerationRequest: videoRequest(nil, params)}
		if err := schemas.CheckDeclaredUnifiedFields(provider, req); err == nil {
			t.Errorf("%+v: want caller_unsupported_parameter", params)
		}
	}
}

// TestBFLVideoDownload pins B18: the download reads the completed task's sample link and fetches
// it through the SSRF-hardened media fetcher, here through a forward proxy, as bifrost sets it on
// each attempt. The served bytes keep a video type even when the host says otherwise, and a sample
// link on a loopback address is refused without being dialed.
func TestBFLVideoDownload(t *testing.T) {
	proxy := proxytest.ForwardProxy(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Host == "203.0.113.20" && r.URL.Path == "/v1/get_result":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"v1","status":"Ready","result":{"sample":"http://203.0.113.10/v/sample.mp4","end_time":1700000000}}`))
		case r.URL.Host == "203.0.113.10" && r.URL.Path == "/v/sample.mp4":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("MP4 BYTES"))
		default:
			http.NotFound(w, r)
		}
	})
	proxyConfig := &schemas.ProxyConfig{Type: schemas.HTTPProxy, URL: schemas.NewSecretVar(proxy.URL)}
	provider, err := NewBFLProvider(&schemas.ProviderConfig{
		NetworkConfig: schemas.NetworkConfig{BaseURL: "http://203.0.113.20", DefaultRequestTimeoutInSeconds: 30},
		ProxyConfig:   proxyConfig,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := imageTaskCtx(t)
	ctx.SetValue(schemas.BifrostContextKeyProviderProxyConfig, proxyConfig)
	got, bifrostErr := provider.VideoDownload(ctx, schemas.Key{Value: *schemas.NewSecretVar("k")}, &schemas.BifrostVideoDownloadRequest{Provider: schemas.BFL, ID: "v1|flux-3-video"})
	if bifrostErr != nil {
		t.Fatalf("download: %+v", bifrostErr.Error)
	}
	if string(got.Content) != "MP4 BYTES" || got.ContentType != "video/mp4" {
		t.Fatalf("download = %q %q, want the bytes as video/mp4", got.Content, got.ContentType)
	}
	// A caller that asks for a stream gets the open response, with the same bytes and type.
	streamed, bifrostErr := provider.VideoDownload(ctx, schemas.Key{Value: *schemas.NewSecretVar("k")}, &schemas.BifrostVideoDownloadRequest{Provider: schemas.BFL, ID: "v1|flux-3-video", Stream: true})
	if bifrostErr != nil {
		t.Fatalf("streamed download: %+v", bifrostErr.Error)
	}
	if streamed.Body == nil {
		t.Fatalf("streamed download has no body: %+v", streamed)
	}
	streamedBytes, err := io.ReadAll(streamed.Body)
	_ = streamed.Body.Close()
	if err != nil || string(streamedBytes) != "MP4 BYTES" || streamed.ContentType != "video/mp4" {
		t.Fatalf("streamed download = %q %q (%v), want the bytes as video/mp4", streamedBytes, streamed.ContentType, err)
	}

	var internalHits atomic.Int32
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		internalHits.Add(1)
		_, _ = w.Write([]byte("internal"))
	}))
	defer internal.Close()
	rec := &bflRecorder{result: `{"id":"v2","status":"Ready","result":{"sample":"` + internal.URL + `/s.mp4"}}`}
	bfl := httptest.NewServer(http.HandlerFunc(rec.serve))
	defer bfl.Close()
	local := newTestProvider(t, schemas.NetworkConfig{BaseURL: bfl.URL, AllowPrivateNetwork: true})
	if _, bifrostErr := local.VideoDownload(imageTaskCtx(t), schemas.Key{Value: *schemas.NewSecretVar("k")}, &schemas.BifrostVideoDownloadRequest{Provider: schemas.BFL, ID: "v2|flux-3-video"}); bifrostErr == nil {
		t.Fatal("a loopback sample link must be refused")
	}
	if n := internalHits.Load(); n != 0 {
		t.Fatalf("the download dialed the loopback link %d times", n)
	}
}

// BenchmarkBFLVideoConversion measures the FLUX 3 Video create's body conversion and encode, the
// provider's own work on the request path (AGENTS.md cost rule).
func BenchmarkBFLVideoConversion(b *testing.B) {
	req := videoRequest(nil, &schemas.VideoGenerationParameters{Seconds: schemas.Ptr("5"), AspectRatio: "16:9", Resolution: "1080p",
		ExtraParams: map[string]any{"safety_tolerance": float64(2), "resolution": "1080p"}})
	b.ReportAllocs()
	for b.Loop() {
		body, err := toBFLVideoBody(req)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := providerUtils.MarshalConvertedRequestBody(body, true); err != nil {
			b.Fatal(err)
		}
	}
}

// trustedHooks is the destination the test keys trust with their webhook secret (TXE-7310).
var trustedHooks = []string{"https://hooks.example.test/v/"}

// TXE-7062: a key's bfl_key_config.webhook_secret is sent as BFL's webhook_secret on an image task
// that carries callback_url; a webhook_secret on the request wins, and without callback_url
// nothing is sent. TXE-7310: only a callback_url under one of the key's webhook_url_prefixes gets
// the key's secret; the caller picks callback_url, so any other destination gets none.
func TestBFLKeyWebhookSecret(t *testing.T) {
	keyWith := schemas.Key{Value: *schemas.NewSecretVar("k"), BFLKeyConfig: &schemas.BFLKeyConfig{WebhookSecret: schemas.NewSecretVar("key-secret"), WebhookURLPrefixes: trustedHooks}}
	keyNoPrefixes := schemas.Key{Value: *schemas.NewSecretVar("k"), BFLKeyConfig: &schemas.BFLKeyConfig{WebhookSecret: schemas.NewSecretVar("key-secret")}}
	cases := []struct {
		name   string
		key    schemas.Key
		params *schemas.ImageGenerationParameters
		cb     *string
		want   any // the webhook_secret BFL receives; nil for none
	}{
		{"the key's secret signs a trusted callback", keyWith, nil, schemas.Ptr("https://hooks.example.test/v/1"), "key-secret"},
		{"the request's own secret wins", keyWith, &schemas.ImageGenerationParameters{ExtraParams: map[string]any{"webhook_secret": "request-secret"}}, schemas.Ptr("https://hooks.example.test/v/1"), "request-secret"},
		{"no callback_url, no secret", keyWith, nil, nil, nil},
		{"a key without the config sends none", schemas.Key{Value: *schemas.NewSecretVar("k")}, nil, schemas.Ptr("https://hooks.example.test/v/1"), nil},
		{"a caller-chosen host gets no key secret", keyWith, nil, schemas.Ptr("https://attacker.example/v/1"), nil},
		{"a path outside the prefix gets no key secret", keyWith, nil, schemas.Ptr("https://hooks.example.test/admin"), nil},
		{"a key with no prefixes signs for nobody", keyNoPrefixes, nil, schemas.Ptr("https://hooks.example.test/v/1"), nil},
		{"the caller's own secret still goes to its own host", keyWith, &schemas.ImageGenerationParameters{ExtraParams: map[string]any{"webhook_secret": "request-secret"}}, schemas.Ptr("https://attacker.example/v/1"), "request-secret"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &bflRecorder{submit: `{"id":"t1"}`}
			server := httptest.NewServer(http.HandlerFunc(rec.serve))
			defer server.Close()
			provider := newTestProvider(t, schemas.NetworkConfig{BaseURL: server.URL, AllowPrivateNetwork: true})
			req := &schemas.BifrostImageGenerationRequest{Model: "flux-pro-1.1", Input: &schemas.ImageGenerationInput{Prompt: "a fox"}, Params: tc.params}
			if tc.cb != nil {
				req.TaskControl = &schemas.TaskControlParameters{CallbackURL: tc.cb}
			}
			if _, bifrostErr := provider.ImageGenerationTask(imageTaskCtx(t), tc.key, req); bifrostErr != nil {
				t.Fatalf("submit: %+v", bifrostErr.Error)
			}
			var sent map[string]any
			if err := json.Unmarshal(rec.bodies[0], &sent); err != nil {
				t.Fatal(err)
			}
			if got, ok := sent["webhook_secret"]; tc.want == nil && ok || tc.want != nil && got != tc.want {
				t.Fatalf("webhook_secret = %v (present %v), want %v; body %s", got, ok, tc.want, rec.bodies[0])
			}
		})
	}
}

// TXE-7310 (Codex review): the image edit task route takes its own conversion, so it pins the same
// rule: a trusted callback_url gets the key's secret, any other gets none, and the request's own
// secret still wins.
func TestBFLKeyWebhookSecretOnEditTasks(t *testing.T) {
	key := schemas.Key{Value: *schemas.NewSecretVar("k"), BFLKeyConfig: &schemas.BFLKeyConfig{WebhookSecret: schemas.NewSecretVar("key-secret"), WebhookURLPrefixes: trustedHooks}}
	cases := []struct {
		name  string
		cb    string
		extra map[string]any
		want  any // the webhook_secret BFL receives; nil for none
	}{
		{"a trusted callback gets the key's secret", "https://hooks.example.test/v/edit_1", nil, "key-secret"},
		{"an untrusted host gets none", "https://attacker.example/v/edit_1", nil, nil},
		{"a path climbing out of the prefix gets none", "https://hooks.example.test/v/../admin", nil, nil},
		{"the request's own secret wins", "https://attacker.example/v/edit_1", map[string]any{"webhook_secret": "request-secret"}, "request-secret"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &bflRecorder{submit: `{"id":"t1"}`}
			server := httptest.NewServer(http.HandlerFunc(rec.serve))
			defer server.Close()
			provider := newTestProvider(t, schemas.NetworkConfig{BaseURL: server.URL, AllowPrivateNetwork: true})
			req := &schemas.BifrostImageEditRequest{
				Model:       "flux-kontext-pro",
				Input:       &schemas.ImageEditInput{Prompt: "make it night", Images: []schemas.ImageInput{{URL: "https://example.com/a.png"}}},
				TaskControl: &schemas.TaskControlParameters{CallbackURL: schemas.Ptr(tc.cb)},
			}
			if tc.extra != nil {
				req.Params = &schemas.ImageEditParameters{ExtraParams: tc.extra}
			}
			if _, bifrostErr := provider.ImageEditTask(imageTaskCtx(t), key, req); bifrostErr != nil {
				t.Fatalf("submit: %+v", bifrostErr.Error)
			}
			var sent map[string]any
			if err := json.Unmarshal(rec.bodies[0], &sent); err != nil {
				t.Fatal(err)
			}
			if sent["webhook_url"] != tc.cb {
				t.Fatalf("webhook_url = %v, want %s", sent["webhook_url"], tc.cb)
			}
			if got, ok := sent["webhook_secret"]; tc.want == nil && ok || tc.want != nil && got != tc.want {
				t.Fatalf("webhook_secret = %v (present %v), want %v", got, ok, tc.want)
			}
		})
	}
}

// TXE-7062 review: BFL receives the key's webhook secret, but no copy Bifrost keeps does: the
// raw_request of a create and of an error carry it redacted. The caller never sent that secret,
// so echoing it back would expose a credential from the key configuration.
func TestBFLKeyWebhookSecretIsRedactedFromCapturedBodies(t *testing.T) {
	key := schemas.Key{Value: *schemas.NewSecretVar("k"), BFLKeyConfig: &schemas.BFLKeyConfig{WebhookSecret: schemas.NewSecretVar("key-secret-7062"), WebhookURLPrefixes: trustedHooks}}
	req := func() *schemas.BifrostImageGenerationRequest {
		return &schemas.BifrostImageGenerationRequest{Model: "flux-pro-1.1", Input: &schemas.ImageGenerationInput{Prompt: "a fox"},
			TaskControl: &schemas.TaskControlParameters{CallbackURL: schemas.Ptr("https://hooks.example.test/v/1")}}
	}
	for _, status := range []int{0, http.StatusTooManyRequests} {
		rec := &bflRecorder{submit: `{"id":"t1"}`, statusOn: status}
		server := httptest.NewServer(http.HandlerFunc(rec.serve))
		provider, err := NewBFLProvider(&schemas.ProviderConfig{
			NetworkConfig:      schemas.NetworkConfig{BaseURL: server.URL, AllowPrivateNetwork: true},
			SendBackRawRequest: true,
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		task, bifrostErr := provider.ImageGenerationTask(imageTaskCtx(t), key, req())
		server.Close()
		if !bytes.Contains(rec.bodies[0], []byte(`"webhook_secret":"key-secret-7062"`)) {
			t.Fatalf("status %d: BFL did not receive the key's secret: %s", status, rec.bodies[0])
		}
		var captured any
		if status == 0 {
			if bifrostErr != nil {
				t.Fatalf("submit: %+v", bifrostErr.Error)
			}
			captured = task.ExtraFields.RawRequest
		} else {
			if bifrostErr == nil {
				t.Fatal("want the 429")
			}
			captured = bifrostErr.ExtraFields.RawRequest
		}
		data, _ := json.Marshal(captured)
		if captured == nil || len(data) < 10 {
			t.Fatalf("status %d: no raw_request captured", status)
		}
		if bytes.Contains(data, []byte("key-secret-7062")) {
			t.Fatalf("status %d: raw_request carries the key's secret: %s", status, data)
		}
		if !bytes.Contains(data, []byte(redactedSecret)) {
			t.Fatalf("status %d: raw_request lost the redaction marker: %s", status, data)
		}
	}
}

// TXE-7062 security review: only the field the provider filled is redacted, so the captured copy
// is no oracle. A caller who plants a guess of the key's secret in the prompt gets the prompt back
// unchanged whether or not the guess is right, and a caller's own webhook_secret is its own input,
// captured as sent.
func TestBFLCapturedBodyIsNoSecretOracle(t *testing.T) {
	const secret = "key-secret-7062"
	key := schemas.Key{Value: *schemas.NewSecretVar("k"), BFLKeyConfig: &schemas.BFLKeyConfig{WebhookSecret: schemas.NewSecretVar(secret), WebhookURLPrefixes: trustedHooks}}
	capture := func(req *schemas.BifrostImageGenerationRequest) map[string]any {
		rec := &bflRecorder{submit: `{"id":"t1"}`}
		server := httptest.NewServer(http.HandlerFunc(rec.serve))
		defer server.Close()
		provider, err := NewBFLProvider(&schemas.ProviderConfig{
			NetworkConfig:      schemas.NetworkConfig{BaseURL: server.URL, AllowPrivateNetwork: true},
			SendBackRawRequest: true,
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		task, bifrostErr := provider.ImageGenerationTask(imageTaskCtx(t), key, req)
		if bifrostErr != nil {
			t.Fatalf("submit: %+v", bifrostErr.Error)
		}
		data, _ := json.Marshal(task.ExtraFields.RawRequest)
		var raw string
		_ = json.Unmarshal(data, &raw)
		if raw != "" {
			data = []byte(raw)
		}
		var body map[string]any
		if err := json.Unmarshal(data, &body); err != nil {
			t.Fatalf("raw_request %s: %v", data, err)
		}
		return body
	}
	cb := &schemas.TaskControlParameters{CallbackURL: schemas.Ptr("https://hooks.example.test/v/1")}

	guessed := capture(&schemas.BifrostImageGenerationRequest{Model: "flux-pro-1.1", Input: &schemas.ImageGenerationInput{Prompt: secret}, TaskControl: cb})
	if guessed["prompt"] != secret {
		t.Fatalf("the caller's own prompt was rewritten, which confirms the guess: %v", guessed["prompt"])
	}
	if guessed["webhook_secret"] != redactedSecret {
		t.Fatalf("the injected secret was not redacted: %v", guessed["webhook_secret"])
	}

	own := capture(&schemas.BifrostImageGenerationRequest{Model: "flux-pro-1.1", Input: &schemas.ImageGenerationInput{Prompt: "p"}, TaskControl: cb,
		Params: &schemas.ImageGenerationParameters{ExtraParams: map[string]any{"webhook_secret": "caller-secret"}}})
	if own["webhook_secret"] != "caller-secret" {
		t.Fatalf("the caller's own webhook_secret should be captured as sent: %v", own["webhook_secret"])
	}
}

// BenchmarkBFLImageTaskKeySecret measures the credential path of TXE-7062 on a create with
// callback_url: resolving the key's webhook secret, converting and encoding the body, and
// redacting the captured copy. "unconfigured" is the path every key without bfl_key_config takes.
// TXE-7310: "trusted" signs and redacts (the callback is under the key's prefix); "untrusted"
// pays for the destination check and sends no key secret.
func BenchmarkBFLImageTaskKeySecret(b *testing.B) {
	in := imageTaskInput{model: "flux-kontext-pro", prompt: "a fox", aspectRatio: schemas.Ptr("16:9"), seed: schemas.Ptr(7),
		images: []string{"https://example.com/a.png"}}
	configured := schemas.Key{Value: *schemas.NewSecretVar("k"), BFLKeyConfig: &schemas.BFLKeyConfig{WebhookSecret: schemas.NewSecretVar("whsec-0123456789"), WebhookURLPrefixes: trustedHooks}}
	for _, tc := range []struct {
		name     string
		key      schemas.Key
		callback string
		signed   bool
	}{
		{"unconfigured", schemas.Key{Value: *schemas.NewSecretVar("k")}, "https://hooks.example.test/v/t1", false},
		{"configured/trusted", configured, "https://hooks.example.test/v/t1", true},
		{"configured/untrusted", configured, "https://hooks.tokun.sh/v/t1", false},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			probe, err := toBFLImageBody(withKeyWebhook(imageTaskInput{model: in.model, prompt: in.prompt, callbackURL: &tc.callback}, tc.key))
			if err != nil || probe.injectedSecret != tc.signed {
				b.Fatalf("case %s: injectedSecret = %v, want %v (err %v)", tc.name, probe != nil && probe.injectedSecret, tc.signed, err)
			}
			for b.Loop() {
				run := in
				run.callbackURL = &tc.callback
				run = withKeyWebhook(run, tc.key)
				body, err := toBFLImageBody(run)
				if err != nil {
					b.Fatal(err)
				}
				data, err := providerUtils.MarshalConvertedRequestBody(body, true)
				if err != nil {
					b.Fatal(err)
				}
				if body.injectedSecret {
					_ = redactInjectedSecret(data)
				}
			}
		})
	}
}

// TXE-7062 security review: a caller who names webhook_secret in any spelling the merge folds to
// it (Webhook_Secret, WebhookSecret) keeps its own value, and the key's secret is never written
// beside it. Otherwise the merge would accept a matching guess and refuse any other.
func TestBFLKeyWebhookSecretIsNotWrittenBesideACallerSpelling(t *testing.T) {
	cb := schemas.Ptr("https://hooks.example.test/v/1")
	for _, spelling := range []string{"webhook_secret", "Webhook_Secret", "WebhookSecret", "WEBHOOKSECRET"} {
		body, err := toBFLImageBody(imageTaskInput{model: "flux-pro-1.1", prompt: "p", callbackURL: cb,
			keyWebhookSecret: "key-secret-7062", extra: map[string]any{spelling: "caller-guess"}})
		if err != nil {
			t.Fatalf("%s: %v", spelling, err)
		}
		if body.injectedSecret {
			t.Errorf("%s: the key's secret was written beside the caller's own", spelling)
		}
		if _, ok := body.fields["webhook_secret"]; ok {
			t.Errorf("%s: fields carry the key's secret", spelling)
		}
		data, err := providerUtils.MarshalConvertedRequestBody(body, true)
		if err != nil {
			t.Fatalf("%s: the merge refused the caller's own field: %v", spelling, err)
		}
		if bytes.Contains(data, []byte("key-secret-7062")) {
			t.Errorf("%s: the key's secret reached the body: %s", spelling, data)
		}
	}
}
