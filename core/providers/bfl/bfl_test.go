package bfl

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

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

// TestBFLOperationsAreUnsupported pins that every operation answers the
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
		if method.Name == "GetProviderKey" {
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
