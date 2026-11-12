package kling_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/network/proxytest"
	"github.com/capsohq/bifrost/core/providers/kling"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/tidwall/gjson"
)

// Response bodies follow the examples of Kling's API reference on the new design standard
// (https://kling.ai/document-api/api/video/3-0-omni/text-to-video, "Create Task" and "Query Task
// (By task ID)", read 2026-11-11), with the placeholder strings filled in. The reference shows the
// risk-control failure only as a description of message ("such as triggering the content risk
// control of the platform"), so that message is written from the description, not recorded.
const (
	klingTaskID      = "893605946402811985"
	klingReferenceID = "tk_task_01HZX"
	klingVideoURL    = "http://203.0.113.20/bs2/upload-kling-api/893605946402811985.mp4"

	createBody = `{"code":0,"message":"SUCCEED","request_id":"req-create",` +
		`"data":{"id":"` + klingTaskID + `","status":"submitted","create_time":1781080778802,"update_time":1781080778802,"external_id":"` + klingReferenceID + `"}}`
	succeededTask = `{"id":"` + klingTaskID + `","status":"succeeded","message":"","create_time":1781080778802,"update_time":1781080794151,` +
		`"external_id":"` + klingReferenceID + `",` +
		`"outputs":[{"type":"video","id":"vid_7781","url":"` + klingVideoURL + `","watermark_url":"","duration":"5.1"}],` +
		`"billing":[{"charge_type":"unit","amount":"3.5","package_type":"video"}]}`
	succeededBody = `{"code":0,"message":"SUCCEED","request_id":"req-query","data":[` + succeededTask + `]}`
	blockedBody   = `{"code":0,"message":"SUCCEED","request_id":"req-blocked","data":[{"id":"` + klingTaskID + `","status":"failed",` +
		`"message":"The task failed because it triggered the content risk control of the platform",` +
		`"create_time":1781080778802,"update_time":1781080794151,"external_id":"` + klingReferenceID + `","outputs":[],"billing":[]}]}`
	failedBody = `{"code":0,"message":"SUCCEED","request_id":"req-failed","data":[{"id":"` + klingTaskID + `","status":"failed",` +
		`"message":"Generation failed, please try again","create_time":1781080778802,"update_time":1781080794151,"outputs":[],"billing":[]}]}`
	emptyQueryBody = `{"code":0,"message":"SUCCEED","request_id":"req-none","data":[]}`
	// Kling's "Error Codes" table: HTTP 400 with service code 1301, "Trigger the platform's content
	// security policy".
	contentSecurityBody = `{"code":1301,"message":"The content you uploaded appears to violate the community guidelines","request_id":"req-1301"}`
)

type capturedRequest struct {
	method string
	path   string
	query  string
	auth   string
	body   []byte
}

func newMockKling(t testing.TB, config *schemas.ProviderConfig, status int, body string) (*kling.KlingProvider, func() []capturedRequest) {
	t.Helper()
	var mu sync.Mutex
	var captured []capturedRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqBody, _ := io.ReadAll(r.Body)
		mu.Lock()
		captured = append(captured, capturedRequest{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, auth: r.Header.Get("Authorization"), body: reqBody})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)

	if config == nil {
		config = &schemas.ProviderConfig{}
	}
	config.NetworkConfig.BaseURL = server.URL
	config.NetworkConfig.DefaultRequestTimeoutInSeconds = 10
	config.NetworkConfig.AllowPrivateNetwork = true
	provider, err := kling.NewKlingProvider(config, bifrost.NewDefaultLogger(schemas.LogLevelError))
	if err != nil {
		t.Fatalf("NewKlingProvider: %v", err)
	}
	return provider, func() []capturedRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]capturedRequest(nil), captured...)
	}
}

func testContext(t testing.TB) *schemas.BifrostContext {
	t.Helper()
	ctx, cancel := schemas.NewBifrostContextWithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func testKey() schemas.Key {
	return schemas.Key{Value: *schemas.NewSecretVar("test-key"), Models: []string{"*"}}
}

func videoRequest(model string, input *schemas.VideoGenerationInput, params *schemas.VideoGenerationParameters) *schemas.BifrostVideoGenerationRequest {
	if input == nil {
		input = &schemas.VideoGenerationInput{Prompt: "a girl on a train, looking out the window"}
	}
	return &schemas.BifrostVideoGenerationRequest{Provider: schemas.Kling, Model: model, Input: input, Params: params}
}

// tokunControl are the task control fields Tokun sends on every Kling create (contract C7).
func tokunControl() schemas.TaskControlParameters {
	return schemas.TaskControlParameters{
		CallbackURL:       schemas.Ptr("https://hooks.tokun.sh/v/" + klingReferenceID),
		ClientReferenceID: schemas.Ptr(klingReferenceID),
	}
}

func onlyRequest(t *testing.T, captured func() []capturedRequest) capturedRequest {
	t.Helper()
	reqs := captured()
	if len(reqs) != 1 {
		t.Fatalf("upstream calls = %d, want exactly 1", len(reqs))
	}
	return reqs[0]
}

// Text-to-video: the path is chosen by the model, the price-affecting fields reach Kling
// unchanged in settings, Tokun's task id and callback address reach options, and the create
// response is the task object.
func TestKlingVideoGenerationTextToVideo(t *testing.T) {
	provider, captured := newMockKling(t, nil, http.StatusOK, createBody)

	resp, bifrostErr := provider.VideoGeneration(testContext(t), testKey(), videoRequest("kling-3.0", nil, &schemas.VideoGenerationParameters{
		Seconds:               schemas.Ptr("15"),
		Resolution:            "4K",
		AspectRatio:           "9:16",
		Audio:                 schemas.Ptr(true),
		NegativePrompt:        schemas.Ptr("blurry"),
		TaskControlParameters: tokunControl(),
	}))
	if bifrostErr != nil {
		t.Fatalf("VideoGeneration error: %+v", bifrostErr.Error)
	}

	req := onlyRequest(t, captured)
	if req.method != http.MethodPost || req.path != "/text-to-video/kling-3.0" {
		t.Errorf("request = %s %s, want POST /text-to-video/kling-3.0", req.method, req.path)
	}
	if req.auth != "Bearer test-key" {
		t.Errorf("Authorization = %q", req.auth)
	}
	body := gjson.ParseBytes(req.body)
	if body.Get("prompt").String() != "a girl on a train, looking out the window" || body.Get("contents").Exists() {
		t.Errorf("text-to-video carries the prompt at the top level: %s", req.body)
	}
	if got := body.Get("settings").Raw; !jsonEqual(t, got, `{"duration":15,"resolution":"4k","aspect_ratio":"9:16","audio":"native"}`) {
		t.Errorf("settings = %s", got)
	}
	if got := body.Get("options").Raw; !jsonEqual(t, got, `{"callback_url":"https://hooks.tokun.sh/v/tk_task_01HZX","external_task_id":"tk_task_01HZX"}`) {
		t.Errorf("options = %s", got)
	}
	if body.Get("negative_prompt").Exists() || body.Get("settings.negative_prompt").Exists() || body.Get("model").Exists() {
		t.Errorf("body carries a field Kling does not define: %s", req.body)
	}

	if resp.ID != klingTaskID+":kling" || resp.Object != "video" || resp.Model != "kling-3.0" {
		t.Errorf("id/object/model = %q/%q/%q", resp.ID, resp.Object, resp.Model)
	}
	if resp.Status != schemas.VideoStatusQueued || resp.ProviderStatus != "submitted" {
		t.Errorf("status/provider_status = %q/%q, want queued/submitted", resp.Status, resp.ProviderStatus)
	}
	if resp.CreatedAt != 1781080778 || resp.CompletedAt != nil || resp.ExpiresAt != nil || resp.Usage != nil || len(resp.Videos) != 0 {
		t.Errorf("a queued task carries only created_at: %+v", resp)
	}
}

func jsonEqual(t *testing.T, got, want string) bool {
	t.Helper()
	var g, w interface{}
	if err := schemas.Unmarshal([]byte(got), &g); err != nil {
		return false
	}
	if err := schemas.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("bad want JSON: %v", err)
	}
	return reflect.DeepEqual(g, w)
}

// Image-to-video and first-and-last-frame: input_reference selects the image-to-video path and
// becomes the first_frame item, last_frame the last_frame item. A URL goes unchanged; a data URL
// goes as its Base64 payload.
func TestKlingVideoGenerationImageToVideo(t *testing.T) {
	provider, captured := newMockKling(t, nil, http.StatusOK, createBody)

	_, bifrostErr := provider.VideoGeneration(testContext(t), testKey(), videoRequest("kling-3.0",
		&schemas.VideoGenerationInput{Prompt: "the girl turns her head", InputReference: schemas.Ptr("https://example.com/first.png")},
		&schemas.VideoGenerationParameters{
			Seconds:               schemas.Ptr("10"),
			Resolution:            "1080p",
			Audio:                 schemas.Ptr(false),
			LastFrame:             "data:image/png;base64,iVBORw0KGgo=",
			TaskControlParameters: tokunControl(),
			// The HTTP handler keeps a copy of each unified field in the extra params.
			ExtraParams: map[string]interface{}{"resolution": "1080p", "last_frame": "data:image/png;base64,iVBORw0KGgo="},
		}))
	if bifrostErr != nil {
		t.Fatalf("VideoGeneration error: %+v", bifrostErr.Error)
	}

	req := onlyRequest(t, captured)
	if req.path != "/image-to-video/kling-3.0" {
		t.Errorf("path = %q, want /image-to-video/kling-3.0", req.path)
	}
	body := gjson.ParseBytes(req.body)
	want := `[{"type":"prompt","text":"the girl turns her head"},{"type":"first_frame","url":"https://example.com/first.png"},{"type":"last_frame","url":"iVBORw0KGgo="}]`
	if got := body.Get("contents").Raw; !jsonEqual(t, got, want) {
		t.Errorf("contents = %s", got)
	}
	if body.Get("prompt").Exists() {
		t.Errorf("image-to-video carries the prompt in contents only: %s", req.body)
	}
	if got := body.Get("settings").Raw; !jsonEqual(t, got, `{"duration":10,"resolution":"1080p","audio":"off"}`) {
		t.Errorf("settings = %s", got)
	}
	if body.Get("options.external_task_id").String() != klingReferenceID {
		t.Errorf("options = %s", body.Get("options").Raw)
	}
}

// Each served model reaches its own path, and audio is "native" or "original" by model.
func TestKlingVideoGenerationPathAndAudioByModel(t *testing.T) {
	firstFrame := &schemas.VideoGenerationInput{Prompt: "p", InputReference: schemas.Ptr("https://example.com/first.png")}
	for name, tc := range map[string]struct {
		model     string
		input     *schemas.VideoGenerationInput
		params    *schemas.VideoGenerationParameters
		wantPath  string
		wantAudio string
		contents  bool
	}{
		"kling-2.5-turbo text":  {model: "kling-2.5-turbo", params: &schemas.VideoGenerationParameters{Seconds: schemas.Ptr("10"), Resolution: "1080p", AspectRatio: "1:1"}, wantPath: "/text-to-video/kling-2.5-turbo"},
		"kling-2.5-turbo image": {model: "kling-2.5-turbo", input: firstFrame, params: &schemas.VideoGenerationParameters{Seconds: schemas.Ptr("5")}, wantPath: "/image-to-video/kling-2.5-turbo", contents: true},
		"kling-3.0-omni text":   {model: "kling-3.0-omni", params: &schemas.VideoGenerationParameters{Audio: schemas.Ptr(true), AspectRatio: "16:9"}, wantPath: "/omni-video/kling-3.0-omni", wantAudio: "native", contents: true},
		"kling-3.0-omni image":  {model: "kling-3.0-omni", input: firstFrame, params: &schemas.VideoGenerationParameters{Resolution: "4k", LastFrame: "https://example.com/last.png"}, wantPath: "/omni-video/kling-3.0-omni", contents: true},
		"kling-o1 text":         {model: "kling-o1", params: &schemas.VideoGenerationParameters{Audio: schemas.Ptr(true), Seconds: schemas.Ptr("3")}, wantPath: "/omni-video/kling-o1", wantAudio: "original", contents: true},
		"kling-o1 audio off":    {model: "kling-o1", input: firstFrame, params: &schemas.VideoGenerationParameters{Audio: schemas.Ptr(false)}, wantPath: "/omni-video/kling-o1", wantAudio: "off", contents: true},
	} {
		t.Run(name, func(t *testing.T) {
			provider, captured := newMockKling(t, nil, http.StatusOK, createBody)
			if _, bifrostErr := provider.VideoGeneration(testContext(t), testKey(), videoRequest(tc.model, tc.input, tc.params)); bifrostErr != nil {
				t.Fatalf("VideoGeneration error: %+v", bifrostErr.Error)
			}
			req := onlyRequest(t, captured)
			if req.path != tc.wantPath {
				t.Errorf("path = %q, want %q", req.path, tc.wantPath)
			}
			body := gjson.ParseBytes(req.body)
			if got := body.Get("settings.audio").String(); got != tc.wantAudio {
				t.Errorf("settings.audio = %q, want %q", got, tc.wantAudio)
			}
			if body.Get("contents").Exists() != tc.contents || body.Get("prompt").Exists() == tc.contents {
				t.Errorf("prompt placement is wrong for %s: %s", tc.wantPath, req.body)
			}
			if tc.params.LastFrame != "" && body.Get("contents.2.type").String() != "last_frame" {
				t.Errorf("last_frame not sent: %s", req.body)
			}
		})
	}
}

// The extra field watermark becomes options.watermark_info; every other extra field is merged
// into settings, whether or not the caller sent x-bf-passthrough-extra-params.
func TestKlingVideoGenerationExtraFields(t *testing.T) {
	provider, captured := newMockKling(t, nil, http.StatusOK, createBody)
	_, bifrostErr := provider.VideoGeneration(testContext(t), testKey(), videoRequest("kling-3.0", nil, &schemas.VideoGenerationParameters{
		Seconds:     schemas.Ptr("5"),
		ExtraParams: map[string]interface{}{"watermark": true, "multi_shot": false},
	}))
	if bifrostErr != nil {
		t.Fatalf("VideoGeneration error: %+v", bifrostErr.Error)
	}
	body := gjson.ParseBytes(onlyRequest(t, captured).body)
	if got := body.Get("options").Raw; !jsonEqual(t, got, `{"watermark_info":{"enabled":true}}`) {
		t.Errorf("options = %s", got)
	}
	if got := body.Get("settings").Raw; !jsonEqual(t, got, `{"duration":5,"multi_shot":false}`) {
		t.Errorf("settings = %s", got)
	}
	if body.Get("watermark").Exists() || body.Get("multi_shot").Exists() || body.Get("settings.watermark").Exists() {
		t.Errorf("an extra field was left outside its Kling field: %s", body.Raw)
	}
}

func assertRefusal(t *testing.T, bifrostErr *schemas.BifrostError, errorType schemas.ErrorType, param string) {
	t.Helper()
	if bifrostErr == nil {
		t.Fatal("the request was not refused")
	}
	if bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %v, want 400", bifrostErr.StatusCode)
	}
	if bifrostErr.ExtraFields.ErrorType != errorType {
		t.Errorf("error_type = %q, want %q", bifrostErr.ExtraFields.ErrorType, errorType)
	}
	if param != "" {
		if got, _ := bifrostErr.Error.Param.(string); got != param {
			t.Errorf("error.param = %v, want %q", bifrostErr.Error.Param, param)
		}
	}
}

// A value outside the model's tiers, and a field Kling has no parameter for, is refused with 400
// caller_unsupported_parameter and no upstream call: never dropped, changed or clamped.
func TestKlingVideoGenerationRefusesUndeliverableValues(t *testing.T) {
	firstFrame := &schemas.VideoGenerationInput{Prompt: "p", InputReference: schemas.Ptr("https://example.com/first.png")}
	for name, tc := range map[string]struct {
		model  string
		input  *schemas.VideoGenerationInput
		params *schemas.VideoGenerationParameters
		param  string
	}{
		"seconds above the tier":           {model: "kling-3.0", params: &schemas.VideoGenerationParameters{Seconds: schemas.Ptr("20")}, param: "seconds"},
		"seconds below the tier":           {model: "kling-3.0", params: &schemas.VideoGenerationParameters{Seconds: schemas.Ptr("2")}, param: "seconds"},
		"seconds between 2.5 turbo tiers":  {model: "kling-2.5-turbo", params: &schemas.VideoGenerationParameters{Seconds: schemas.Ptr("7")}, param: "seconds"},
		"seconds above the O1 tier":        {model: "kling-o1", params: &schemas.VideoGenerationParameters{Seconds: schemas.Ptr("12")}, param: "seconds"},
		"fractional seconds":               {model: "kling-3.0", params: &schemas.VideoGenerationParameters{Seconds: schemas.Ptr("5.5")}, param: "seconds"},
		"resolution the model lacks":       {model: "kling-2.5-turbo", params: &schemas.VideoGenerationParameters{Resolution: "4k"}, param: "resolution"},
		"resolution Kling lacks":           {model: "kling-3.0", params: &schemas.VideoGenerationParameters{Resolution: "480p"}, param: "resolution"},
		"aspect ratio Kling lacks":         {model: "kling-3.0", params: &schemas.VideoGenerationParameters{AspectRatio: "4:3"}, param: "aspect_ratio"},
		"aspect ratio on image-to-video":   {model: "kling-3.0", input: firstFrame, params: &schemas.VideoGenerationParameters{AspectRatio: "16:9"}, param: "aspect_ratio"},
		"audio on a model without audio":   {model: "kling-2.5-turbo", params: &schemas.VideoGenerationParameters{Audio: schemas.Ptr(true)}, param: "audio"},
		"audio off on a model without it":  {model: "kling-2.5-turbo", input: firstFrame, params: &schemas.VideoGenerationParameters{Audio: schemas.Ptr(false)}, param: "audio"},
		"seed":                             {model: "kling-3.0", params: &schemas.VideoGenerationParameters{Seed: schemas.Ptr(7)}, param: "seed"},
		"last frame without a first frame": {model: "kling-3.0", params: &schemas.VideoGenerationParameters{LastFrame: "https://example.com/last.png"}, param: "last_frame"},
		"last frame only on omni":          {model: "kling-o1", params: &schemas.VideoGenerationParameters{LastFrame: "https://example.com/last.png"}, param: "last_frame"},
		"source video":                     {model: "kling-3.0-omni", input: &schemas.VideoGenerationInput{Prompt: "p", VideoURI: schemas.Ptr("https://example.com/in.mp4")}, param: "video_uri"},
		"non-string resolution":            {model: "kling-3.0", params: &schemas.VideoGenerationParameters{ExtraParams: map[string]interface{}{"resolution": float64(1080)}}, param: "resolution"},
		"reference images as objects":      {model: "kling-3.0-omni", params: &schemas.VideoGenerationParameters{ExtraParams: map[string]interface{}{"reference_images": []interface{}{map[string]interface{}{"uri": "https://example.com/r.png"}}}}, param: "reference_images"},
		"watermark that is not a boolean":  {model: "kling-3.0", params: &schemas.VideoGenerationParameters{ExtraParams: map[string]interface{}{"watermark": "yes"}}, param: "watermark"},
	} {
		t.Run(name, func(t *testing.T) {
			provider, captured := newMockKling(t, nil, http.StatusOK, createBody)
			_, bifrostErr := provider.VideoGeneration(testContext(t), testKey(), videoRequest(tc.model, tc.input, tc.params))
			assertRefusal(t, bifrostErr, schemas.ErrorTypeCallerUnsupportedParameter, tc.param)
			if n := len(captured()); n != 0 {
				t.Errorf("upstream calls = %d, want 0", n)
			}
		})
	}
}

// An extra field named after a settings field the converter writes, or after a container of the
// body, is refused before any call, in any letter case: it would overwrite the converted value
// after governance and pricing read the request.
func TestKlingVideoGenerationRefusesExtraParamsOverridingTheBody(t *testing.T) {
	for _, name := range []string{"duration", "Duration", "audio", "settings", "options", "contents", "Prompt", "model", "MODEL", "stream", "Stream", "model_id", "modelId", "deployment", "engine", "version", "aspectRatio"} {
		t.Run(name, func(t *testing.T) {
			provider, captured := newMockKling(t, nil, http.StatusOK, createBody)
			_, bifrostErr := provider.VideoGeneration(testContext(t), testKey(), videoRequest("kling-3.0", nil, &schemas.VideoGenerationParameters{
				Seconds:     schemas.Ptr("5"),
				ExtraParams: map[string]interface{}{name: "x"},
			}))
			assertRefusal(t, bifrostErr, schemas.ErrorTypeCallerInvalidRequest, name)
			if n := len(captured()); n != 0 {
				t.Errorf("upstream calls = %d, want 0", n)
			}
		})
	}
}

// A model with no tier table is refused: an out-of-tier value could not be recognised for it.
// So is another spelling of a served name: governance and pricing read the name as written, so
// a variant they do not know must not be folded onto a served model here.
func TestKlingVideoGenerationUnknownModel(t *testing.T) {
	provider, captured := newMockKling(t, nil, http.StatusOK, createBody)
	for _, model := range []string{"kling-2.1", "kling-v3-pro", "", "Kling-3.0", "KLING-3.0", " kling-3.0", "kling-3.0 "} {
		_, bifrostErr := provider.VideoGeneration(testContext(t), testKey(), videoRequest(model, nil, nil))
		assertRefusal(t, bifrostErr, schemas.ErrorTypeCallerModelUnknown, "")
	}
	if n := len(captured()); n != 0 {
		t.Errorf("upstream calls = %d, want 0", n)
	}
}

func bifrostRequest(requestType schemas.RequestType, params *schemas.VideoGenerationParameters) *schemas.BifrostRequest {
	return &schemas.BifrostRequest{RequestType: requestType, VideoGenerationRequest: videoRequest("kling-3.0", nil, params)}
}

// The provider's declaration makes core refuse, before a key is chosen, every unified field Kling
// has no parameter for and the two task control fields it cannot take, and accept the ones the
// converter delivers (contract C4, C7).
func TestKlingDeclaresDeliverableFields(t *testing.T) {
	provider, _ := newMockKling(t, nil, http.StatusOK, createBody)

	delivered := &schemas.VideoGenerationParameters{
		Seconds: schemas.Ptr("5"), Resolution: "720p", AspectRatio: "16:9", Audio: schemas.Ptr(true), LastFrame: "https://example.com/l.png",
		NegativePrompt: schemas.Ptr("blurry"), TaskControlParameters: tokunControl(),
	}
	req := bifrostRequest(schemas.VideoGenerationRequest, delivered)
	if bifrostErr := schemas.CheckDeclaredUnifiedFields(provider, req); bifrostErr != nil {
		t.Errorf("a deliverable field was refused: %+v", bifrostErr.Error)
	}
	if bifrostErr := schemas.CheckTaskControlFields(provider, req); bifrostErr != nil {
		t.Errorf("callback_url or client_reference_id was refused: %+v", bifrostErr.Error)
	}

	for param, params := range map[string]*schemas.VideoGenerationParameters{
		"size":              {Size: "1280x720"},
		"output_format":     {OutputFormat: schemas.Ptr("mov")},
		"reference_images":  {ReferenceImages: schemas.VideoReferenceImages{"https://example.com/r.png"}},
		"type":              {Type: schemas.Ptr("upscale")},
		"upscale_factor":    {UpscaleFactor: schemas.Ptr(2)},
		"target_megapixels": {TargetMegapixels: schemas.Ptr(8)},
	} {
		assertRefusal(t, schemas.CheckDeclaredUnifiedFields(provider, bifrostRequest(schemas.VideoGenerationRequest, params)), schemas.ErrorTypeCallerUnsupportedParameter, param)
	}
	for param, control := range map[string]schemas.TaskControlParameters{
		schemas.TaskControlFieldExpiresAfterSeconds: {ExpiresAfterSeconds: schemas.Ptr(7200)},
		schemas.TaskControlFieldOutputStorageURI:    {OutputStorageURI: schemas.Ptr("s3://bucket/task/")},
	} {
		assertRefusal(t, schemas.CheckTaskControlFields(provider, bifrostRequest(schemas.VideoGenerationRequest, &schemas.VideoGenerationParameters{TaskControlParameters: control})), schemas.ErrorTypeCallerUnsupportedParameter, param)
	}
}

func assertSucceededTask(t *testing.T, resp *schemas.BifrostVideoGenerationResponse) {
	t.Helper()
	if resp.ID != klingTaskID+":kling" || resp.Object != "video" {
		t.Errorf("id/object = %q/%q", resp.ID, resp.Object)
	}
	if resp.Status != schemas.VideoStatusCompleted || resp.ProviderStatus != "succeeded" {
		t.Errorf("status/provider_status = %q/%q, want completed/succeeded", resp.Status, resp.ProviderStatus)
	}
	if resp.CreatedAt != 1781080778 || resp.CompletedAt == nil || *resp.CompletedAt != 1781080794 {
		t.Errorf("created_at/completed_at = %d/%v", resp.CreatedAt, resp.CompletedAt)
	}
	// Kling keeps a result for 30 days: expires_at is the completion time plus 30 days.
	if resp.ExpiresAt == nil || *resp.ExpiresAt != 1781080794+30*24*3600 {
		t.Errorf("expires_at = %v, want completion + 30 days", resp.ExpiresAt)
	}
	if len(resp.Videos) != 1 {
		t.Fatalf("videos = %+v, want one", resp.Videos)
	}
	video := resp.Videos[0]
	if video.Type != schemas.VideoOutputTypeURL || video.URL == nil || *video.URL != klingVideoURL || video.ContentType != "video/mp4" || video.ID != "vid_7781" {
		t.Errorf("videos[0] = %+v", video)
	}
	if resp.Seconds == nil || *resp.Seconds != "5.1" {
		t.Errorf("seconds = %v, want Kling's 5.1", resp.Seconds)
	}
	if resp.Usage == nil || resp.Usage.OutputSeconds == nil || *resp.Usage.OutputSeconds != 5.1 {
		t.Fatalf("usage.output_seconds = %+v, want 5.1", resp.Usage)
	}
	if cost := resp.Usage.VendorCost; cost == nil || cost.Amount != 3.5 || cost.Unit != schemas.VendorCostUnitUnit {
		t.Errorf("usage.vendor_cost = %+v, want 3.5 unit", cost)
	}
	if resp.Usage.Cost != nil || resp.Usage.OutputTokens != nil || resp.Usage.InputSeconds != nil {
		t.Errorf("usage carries a quantity Kling did not report: %+v", resp.Usage)
	}
	if resp.Error != nil || resp.ContentFilter != nil {
		t.Errorf("a succeeded task carries error/content_filter: %+v / %+v", resp.Error, resp.ContentFilter)
	}
}

// A query by Kling's task id and a query by Tokun's client_reference_id are each one call, to
// task_ids and external_task_ids, and return the same task object.
func TestKlingVideoRetrieveByEitherID(t *testing.T) {
	provider, captured := newMockKling(t, &schemas.ProviderConfig{SendBackRawResponse: true}, http.StatusOK, succeededBody)
	ctx := testContext(t)

	byTaskID, bifrostErr := provider.VideoRetrieve(ctx, testKey(), &schemas.BifrostVideoRetrieveRequest{Provider: schemas.Kling, ID: klingTaskID + ":kling"})
	if bifrostErr != nil {
		t.Fatalf("VideoRetrieve error: %+v", bifrostErr.Error)
	}
	assertSucceededTask(t, byTaskID)
	if byTaskID.ExtraFields.RawResponse == nil {
		t.Error("the vendor's raw response was not kept")
	}

	byReference, bifrostErr := provider.VideoRetrieveByClientReferenceID(ctx, testKey(), &schemas.BifrostVideoRetrieveRequest{
		Provider: schemas.Kling, ID: klingReferenceID, IDType: schemas.VideoIDTypeClientReference,
	})
	if bifrostErr != nil {
		t.Fatalf("VideoRetrieveByClientReferenceID error: %+v", bifrostErr.Error)
	}
	assertSucceededTask(t, byReference)

	// The id_type also selects the query on VideoRetrieve itself, for a caller of the provider.
	viaIDType, bifrostErr := provider.VideoRetrieve(ctx, testKey(), &schemas.BifrostVideoRetrieveRequest{
		Provider: schemas.Kling, ID: klingReferenceID, IDType: schemas.VideoIDTypeClientReference,
	})
	if bifrostErr != nil {
		t.Fatalf("VideoRetrieve by reference error: %+v", bifrostErr.Error)
	}

	byTaskID.ExtraFields.Latency, byReference.ExtraFields.Latency, viaIDType.ExtraFields.Latency = 0, 0, 0
	if !reflect.DeepEqual(byTaskID, byReference) || !reflect.DeepEqual(byReference, viaIDType) {
		t.Errorf("the two queries returned different task objects:\n%+v\n%+v\n%+v", byTaskID, byReference, viaIDType)
	}

	reqs := captured()
	if len(reqs) != 3 {
		t.Fatalf("upstream calls = %d, want one per query", len(reqs))
	}
	for i, wantQuery := range []string{"task_ids=" + klingTaskID, "external_task_ids=" + klingReferenceID, "external_task_ids=" + klingReferenceID} {
		if reqs[i].method != http.MethodGet || reqs[i].path != "/tasks" || reqs[i].query != wantQuery {
			t.Errorf("query %d = %s %s?%s, want GET /tasks?%s", i, reqs[i].method, reqs[i].path, reqs[i].query, wantQuery)
		}
		if reqs[i].auth != "Bearer test-key" || len(reqs[i].body) != 0 {
			t.Errorf("query %d auth/body = %q/%q", i, reqs[i].auth, reqs[i].body)
		}
	}

	if _, ok := interface{}(provider).(schemas.VideoClientReferenceRetriever); !ok {
		t.Error("kling must implement schemas.VideoClientReferenceRetriever")
	}
}

// A client reference is the caller's own string: it reaches Kling exactly as given, even when it
// looks like a provider-suffixed or URL-escaped task id.
func TestKlingVideoRetrieveSendsTheClientReferenceAsGiven(t *testing.T) {
	provider, captured := newMockKling(t, nil, http.StatusOK, emptyQueryBody)
	for reference, wantQuery := range map[string]string{
		"order-7:kling": "external_task_ids=order-7%3Akling",
		"100%25":        "external_task_ids=100%2525",
		"a b/c":         "external_task_ids=a+b%2Fc",
	} {
		_, _ = provider.VideoRetrieveByClientReferenceID(testContext(t), testKey(), &schemas.BifrostVideoRetrieveRequest{
			Provider: schemas.Kling, ID: reference, IDType: schemas.VideoIDTypeClientReference,
		})
		reqs := captured()
		if got := reqs[len(reqs)-1].query; got != wantQuery {
			t.Errorf("reference %q: query = %q, want %q", reference, got, wantQuery)
		}
	}
}

// Kling's four statuses map one to one, with the vendor's string in provider_status. A string
// the table does not know is in_progress, never a terminal state.
func TestKlingVideoStatusMapping(t *testing.T) {
	for raw, want := range map[string]schemas.VideoStatus{
		"submitted":  schemas.VideoStatusQueued,
		"processing": schemas.VideoStatusInProgress,
		"succeeded":  schemas.VideoStatusCompleted,
		"failed":     schemas.VideoStatusFailed,
		"paused":     schemas.VideoStatusInProgress,
	} {
		resp, bifrostErr := kling.ToBifrostVideoGenerationResponse(&kling.KlingTask{ID: "1", Status: raw, CreateTime: 1781080778802, UpdateTime: 1781080794151})
		if bifrostErr != nil {
			t.Fatalf("%s: %+v", raw, bifrostErr.Error)
		}
		if resp.Status != want || resp.ProviderStatus != raw {
			t.Errorf("%s: status/provider_status = %q/%q, want %q/%q", raw, resp.Status, resp.ProviderStatus, want, raw)
		}
		if (resp.CompletedAt != nil) != (want == schemas.VideoStatusCompleted) || (resp.ExpiresAt != nil) != (want == schemas.VideoStatusCompleted) {
			t.Errorf("%s: completed_at/expires_at = %v/%v", raw, resp.CompletedAt, resp.ExpiresAt)
		}
	}
}

// A task Kling's risk control blocked is failed with content_filter filled; any other failure is
// failed without it. Kling's task object has no failure code, so error.code stays empty.
func TestKlingVideoRetrieveFailures(t *testing.T) {
	provider, _ := newMockKling(t, nil, http.StatusOK, blockedBody)
	blocked, bifrostErr := provider.VideoRetrieve(testContext(t), testKey(), &schemas.BifrostVideoRetrieveRequest{Provider: schemas.Kling, ID: klingTaskID})
	if bifrostErr != nil {
		t.Fatalf("VideoRetrieve error: %+v", bifrostErr.Error)
	}
	const reason = "The task failed because it triggered the content risk control of the platform"
	if blocked.Status != schemas.VideoStatusFailed || blocked.ProviderStatus != "failed" {
		t.Errorf("status/provider_status = %q/%q", blocked.Status, blocked.ProviderStatus)
	}
	if blocked.Error == nil || blocked.Error.Message != reason || blocked.Error.Code != "" {
		t.Errorf("error = %+v", blocked.Error)
	}
	if blocked.ContentFilter == nil || blocked.ContentFilter.FilteredCount != 1 || !reflect.DeepEqual(blocked.ContentFilter.Reasons, []string{reason}) {
		t.Errorf("content_filter = %+v, want one filtered item with Kling's message", blocked.ContentFilter)
	}
	if len(blocked.Videos) != 0 || blocked.Usage != nil || blocked.ExpiresAt != nil {
		t.Errorf("a blocked task carries an artifact, usage or expiry: %+v", blocked)
	}

	provider, _ = newMockKling(t, nil, http.StatusOK, failedBody)
	failed, bifrostErr := provider.VideoRetrieve(testContext(t), testKey(), &schemas.BifrostVideoRetrieveRequest{Provider: schemas.Kling, ID: klingTaskID})
	if bifrostErr != nil {
		t.Fatalf("VideoRetrieve error: %+v", bifrostErr.Error)
	}
	if failed.Status != schemas.VideoStatusFailed || failed.Error == nil || failed.Error.Message != "Generation failed, please try again" {
		t.Errorf("failed task = %+v / %+v", failed.Status, failed.Error)
	}
	if failed.ContentFilter != nil {
		t.Errorf("a failure that is not a content block carries content_filter: %+v", failed.ContentFilter)
	}
}

// A query Kling answers with no task is 404, so a caller recovering by its own reference can
// tell "submit again" from a vendor failure. An id with a comma would be a batch query.
func TestKlingVideoRetrieveNotFound(t *testing.T) {
	provider, captured := newMockKling(t, nil, http.StatusOK, emptyQueryBody)
	_, bifrostErr := provider.VideoRetrieveByClientReferenceID(testContext(t), testKey(), &schemas.BifrostVideoRetrieveRequest{
		Provider: schemas.Kling, ID: klingReferenceID, IDType: schemas.VideoIDTypeClientReference,
	})
	if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusNotFound {
		t.Fatalf("err = %+v, want 404", bifrostErr)
	}
	if bifrostErr.Error.Code == nil || *bifrostErr.Error.Code != "task_not_found" || bifrostErr.IsBifrostError {
		t.Errorf("error = %+v", bifrostErr.Error)
	}
	if n := len(captured()); n != 1 {
		t.Errorf("upstream calls = %d, want 1", n)
	}

	for _, id := range []string{"a,b", " "} {
		if _, bifrostErr := provider.VideoRetrieve(testContext(t), testKey(), &schemas.BifrostVideoRetrieveRequest{Provider: schemas.Kling, ID: id}); bifrostErr == nil || *bifrostErr.StatusCode != http.StatusBadRequest {
			t.Errorf("id %q: err = %+v, want 400", id, bifrostErr)
		}
	}
	if n := len(captured()); n != 1 {
		t.Errorf("a refused id reached Kling: %d calls", n)
	}
}

// Create error 1301 is the vendor blocking the caller's content: caller_content_filtered, with
// Kling's status, code and body kept. The same code inside a 200 is the same error.
func TestKlingVideoGenerationContentBlocked(t *testing.T) {
	for name, status := range map[string]int{"HTTP 400": http.StatusBadRequest, "inside a 200": http.StatusOK} {
		t.Run(name, func(t *testing.T) {
			provider, captured := newMockKling(t, &schemas.ProviderConfig{SendBackRawResponse: true}, status, contentSecurityBody)
			ctx := testContext(t)
			_, bifrostErr := provider.VideoGeneration(ctx, testKey(), videoRequest("kling-3.0", nil, &schemas.VideoGenerationParameters{TaskControlParameters: tokunControl()}))
			assertRefusal(t, bifrostErr, schemas.ErrorTypeCallerContentFiltered, "")
			if bifrostErr.Error.Code == nil || *bifrostErr.Error.Code != "1301" {
				t.Errorf("error.code = %v, want Kling's 1301", bifrostErr.Error.Code)
			}
			if bifrostErr.Error.Message != "The content you uploaded appears to violate the community guidelines" {
				t.Errorf("error.message = %q", bifrostErr.Error.Message)
			}
			if bifrostErr.Error.EventID == nil || *bifrostErr.Error.EventID != "req-1301" || bifrostErr.IsBifrostError {
				t.Errorf("event_id/is_bifrost_error = %v/%v", bifrostErr.Error.EventID, bifrostErr.IsBifrostError)
			}
			if !strings.Contains(string(mustJSON(t, bifrostErr.ExtraFields.RawResponse)), `"code":1301`) {
				t.Errorf("raw_response = %s", mustJSON(t, bifrostErr.ExtraFields.RawResponse))
			}
			if n := len(captured()); n != 1 {
				t.Errorf("upstream calls = %d, want 1: a block is not retried", n)
			}
		})
	}
}

func mustJSON(t *testing.T, v interface{}) []byte {
	t.Helper()
	data, err := schemas.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return data
}

// Every other vendor error keeps Kling's HTTP status and service code. The two billing codes Kling
// answers with 429 are declared provider_billing; the rest are classified from the status.
func TestKlingVendorErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		status    int
		body      string
		wantCode  string
		errorType schemas.ErrorType
	}{
		"invalid key":             {status: 401, body: `{"code":1002,"message":"Authorization is invalid","request_id":"r"}`, wantCode: "1002", errorType: schemas.ErrorTypeProviderAuthFailed},
		"resource pack exhausted": {status: 429, body: `{"code":1102,"message":"Resource pack exhausted","request_id":"r"}`, wantCode: "1102", errorType: schemas.ErrorTypeProviderBilling},
		"rate limit":              {status: 429, body: `{"code":1302,"message":"Too many requests","request_id":"r"}`, wantCode: "1302", errorType: schemas.ErrorTypeProviderRateLimited},
		"invalid parameter":       {status: 400, body: `{"code":1201,"message":"settings.duration is invalid","request_id":"r"}`, wantCode: "1201", errorType: schemas.ErrorTypeCallerInvalidRequest},
		"server error":            {status: 500, body: `{"code":5000,"message":"Server internal error","request_id":"r"}`, wantCode: "5000", errorType: schemas.ErrorTypeProviderServerError},
	} {
		t.Run(name, func(t *testing.T) {
			provider, _ := newMockKling(t, nil, tc.status, tc.body)
			_, bifrostErr := provider.VideoGeneration(testContext(t), testKey(), videoRequest("kling-3.0", nil, nil))
			if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != tc.status {
				t.Fatalf("err = %+v, want status %d", bifrostErr, tc.status)
			}
			if bifrostErr.Error.Code == nil || *bifrostErr.Error.Code != tc.wantCode {
				t.Errorf("error.code = %v, want %s", bifrostErr.Error.Code, tc.wantCode)
			}
			bifrostErr.ExtraFields.RequestType = schemas.VideoGenerationRequest
			if got := bifrostErr.ResolveErrorType(); got != tc.errorType {
				t.Errorf("error_type = %q, want %q", got, tc.errorType)
			}
		})
	}

	// A 200 whose envelope carries no task is the vendor answering with nothing.
	provider, _ := newMockKling(t, nil, http.StatusOK, `{"code":0,"message":"SUCCEED","request_id":"r","data":{}}`)
	_, bifrostErr := provider.VideoGeneration(testContext(t), testKey(), videoRequest("kling-3.0", nil, nil))
	if bifrostErr == nil || *bifrostErr.StatusCode != http.StatusBadGateway || bifrostErr.ExtraFields.ErrorType != schemas.ErrorTypeProviderEmptyResult {
		t.Errorf("err = %+v, want 502 provider_empty_result", bifrostErr)
	}
}

// vendor_cost is the Units charged to a resource package. A balance deduction is money, not
// Units, and a task with no billing record reports nothing.
func TestKlingUsageFromBilling(t *testing.T) {
	video := []kling.KlingOutput{{Type: "video", URL: klingVideoURL, Duration: "10"}}
	for name, tc := range map[string]struct {
		billing   []kling.KlingBilling
		wantUnits *float64
	}{
		"one package":         {billing: []kling.KlingBilling{{ChargeType: "unit", Amount: "7", PackageType: "video"}}, wantUnits: schemas.Ptr(7.0)},
		"two packages":        {billing: []kling.KlingBilling{{ChargeType: "unit", Amount: "1.5"}, {ChargeType: "unit", Amount: "2.25"}}, wantUnits: schemas.Ptr(3.75)},
		"units and cash":      {billing: []kling.KlingBilling{{ChargeType: "unit", Amount: "2"}, {ChargeType: "cash", Amount: "0.28", Currency: "USD"}}, wantUnits: schemas.Ptr(2.0)},
		"cash only":           {billing: []kling.KlingBilling{{ChargeType: "cash", CashType: "balance", Amount: "0.7", Currency: "USD", ListPrice: "0.7"}}},
		"no record":           {},
		"amount not a number": {billing: []kling.KlingBilling{{ChargeType: "unit", Amount: "2"}, {ChargeType: "unit", Amount: "n/a"}}},
	} {
		t.Run(name, func(t *testing.T) {
			resp, bifrostErr := kling.ToBifrostVideoGenerationResponse(&kling.KlingTask{ID: "1", Status: "succeeded", UpdateTime: 1781080794151, Outputs: video, Billing: tc.billing})
			if bifrostErr != nil {
				t.Fatalf("convert: %+v", bifrostErr.Error)
			}
			if resp.Usage == nil || resp.Usage.OutputSeconds == nil || *resp.Usage.OutputSeconds != 10 {
				t.Fatalf("usage.output_seconds = %+v, want 10", resp.Usage)
			}
			cost := resp.Usage.VendorCost
			if (cost == nil) != (tc.wantUnits == nil) {
				t.Fatalf("usage.vendor_cost = %+v, want %v", cost, tc.wantUnits)
			}
			if cost != nil && (cost.Amount != *tc.wantUnits || cost.Unit != schemas.VendorCostUnitUnit) {
				t.Errorf("usage.vendor_cost = %+v, want %v unit", cost, *tc.wantUnits)
			}
		})
	}

	// A task that reports neither a duration nor a deduction has no usage at all.
	resp, _ := kling.ToBifrostVideoGenerationResponse(&kling.KlingTask{ID: "1", Status: "succeeded", Outputs: []kling.KlingOutput{{Type: "video", URL: klingVideoURL}}})
	if resp.Usage != nil || resp.Seconds != nil {
		t.Errorf("usage/seconds = %+v/%v, want none: Kling reported none", resp.Usage, resp.Seconds)
	}
}

// The list is POST /tasks with Kling's cursor and page size, kept to video tasks. While there is
// another page, last_id is the cursor to send back as after.
func TestKlingVideoList(t *testing.T) {
	listBody := `{"code":0,"message":"SUCCEED","request_id":"req-list","data":{"result":[` + succeededTask + `,` +
		`{"id":"893605946402811986","status":"processing","message":"","create_time":1781080779000,"update_time":1781080780000,"outputs":[],"billing":[]}],` +
		`"count":2,"next_cursor":"cursor-2","has_more":true}}`
	provider, captured := newMockKling(t, nil, http.StatusOK, listBody)

	listed, bifrostErr := provider.VideoList(testContext(t), testKey(), &schemas.BifrostVideoListRequest{Provider: schemas.Kling, After: schemas.Ptr("cursor-1"), Limit: schemas.Ptr(2)})
	if bifrostErr != nil {
		t.Fatalf("VideoList error: %+v", bifrostErr.Error)
	}
	req := onlyRequest(t, captured)
	if req.method != http.MethodPost || req.path != "/tasks" {
		t.Errorf("request = %s %s, want POST /tasks", req.method, req.path)
	}
	if !jsonEqual(t, string(req.body), `{"cursor":"cursor-1","limit":2,"filters":[{"key":"product_type","values":["video"]}]}`) {
		t.Errorf("body = %s", req.body)
	}
	if len(listed.Data) != 2 || listed.Data[0].ID != klingTaskID+":kling" || listed.Data[0].Status != schemas.VideoStatusCompleted || listed.Data[1].Status != schemas.VideoStatusInProgress {
		t.Fatalf("data = %+v", listed.Data)
	}
	if first := listed.Data[0]; first.CreatedAt != 1781080778 || first.CompletedAt == nil || first.ExpiresAt == nil || first.Seconds == nil || *first.Seconds != "5.1" {
		t.Errorf("data[0] = %+v", first)
	}
	if listed.HasMore == nil || !*listed.HasMore || listed.LastID == nil || *listed.LastID != "cursor-2" || listed.FirstID == nil || *listed.FirstID != klingTaskID+":kling" {
		t.Errorf("has_more/last_id/first_id = %v/%v/%v", listed.HasMore, listed.LastID, listed.FirstID)
	}

	// The last page has no cursor: last_id is the last task.
	provider, _ = newMockKling(t, nil, http.StatusOK, `{"code":0,"message":"SUCCEED","request_id":"r","data":{"result":[`+succeededTask+`],"count":1,"next_cursor":"","has_more":false}}`)
	last, bifrostErr := provider.VideoList(testContext(t), testKey(), &schemas.BifrostVideoListRequest{Provider: schemas.Kling})
	if bifrostErr != nil {
		t.Fatalf("VideoList error: %+v", bifrostErr.Error)
	}
	if *last.HasMore || *last.LastID != klingTaskID+":kling" {
		t.Errorf("has_more/last_id = %v/%v", *last.HasMore, *last.LastID)
	}

	// Kling has no order parameter and caps the page at 500: neither is dropped or clamped.
	provider, captured = newMockKling(t, nil, http.StatusOK, listBody)
	_, bifrostErr = provider.VideoList(testContext(t), testKey(), &schemas.BifrostVideoListRequest{Provider: schemas.Kling, Order: schemas.Ptr("asc")})
	assertRefusal(t, bifrostErr, schemas.ErrorTypeCallerUnsupportedParameter, "order")
	_, bifrostErr = provider.VideoList(testContext(t), testKey(), &schemas.BifrostVideoListRequest{Provider: schemas.Kling, Limit: schemas.Ptr(501)})
	assertRefusal(t, bifrostErr, schemas.ErrorTypeCallerUnsupportedParameter, "limit")
	// A vendor filter is refused by name (the first in sorted order), not dropped: Kling's list
	// takes its filters in the POST body, which this provider does not write yet.
	_, bifrostErr = provider.VideoList(testContext(t), testKey(), &schemas.BifrostVideoListRequest{Provider: schemas.Kling, Filters: map[string][]string{"status": {"succeeded"}, "start_time": {"1781080000000"}}})
	assertRefusal(t, bifrostErr, schemas.ErrorTypeCallerUnsupportedParameter, "start_time")
	if n := len(captured()); n != 0 {
		t.Errorf("a refused list reached Kling: %d calls", n)
	}
}

// The download is one query to Kling and one fetch of outputs[].url without the key. A task that
// has not succeeded has nothing to download.
func TestKlingVideoDownload(t *testing.T) {
	// The fetch goes through the SSRF-checked media fetcher, which refuses the loopback mock, so
	// the file is served for a public address by a forward proxy.
	videoProxy := proxytest.ForwardProxy(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host != "203.0.113.20" {
			http.NotFound(w, r)
			return
		}
		if auth := r.Header.Get("Authorization"); auth != "" {
			t.Errorf("the download sent credentials to the file host: %q", auth)
		}
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write([]byte("mp4data"))
	})
	provider, captured := newMockKling(t, nil, http.StatusOK, succeededBody)
	ctx := testContext(t)
	ctx.SetValue(schemas.BifrostContextKeyProviderProxyConfig, &schemas.ProxyConfig{Type: schemas.HTTPProxy, URL: schemas.NewSecretVar(videoProxy.URL)})

	downloaded, bifrostErr := provider.VideoDownload(ctx, testKey(), &schemas.BifrostVideoDownloadRequest{Provider: schemas.Kling, ID: klingTaskID + ":kling"})
	if bifrostErr != nil {
		t.Fatalf("VideoDownload error: %+v", bifrostErr.Error)
	}
	if string(downloaded.Content) != "mp4data" || downloaded.ContentType != "video/mp4" || downloaded.VideoID != klingTaskID+":kling" {
		t.Errorf("download = %q %s %s", downloaded.Content, downloaded.ContentType, downloaded.VideoID)
	}
	if req := onlyRequest(t, captured); req.query != "task_ids="+klingTaskID {
		t.Errorf("query = %q", req.query)
	}

	// A caller that asked for a stream gets the same bytes as Body, with their length.
	streamed, bifrostErr := provider.VideoDownload(ctx, testKey(), &schemas.BifrostVideoDownloadRequest{Provider: schemas.Kling, ID: klingTaskID + ":kling", Stream: true})
	if bifrostErr != nil {
		t.Fatalf("streamed VideoDownload error: %+v", bifrostErr.Error)
	}
	if streamed.Body == nil || streamed.Content != nil || streamed.ContentLength != int64(len("mp4data")) || streamed.ContentType != "video/mp4" {
		t.Fatalf("streamed download = %+v", streamed)
	}
	streamedBytes, err := io.ReadAll(streamed.Body)
	_ = streamed.Body.Close()
	if err != nil || string(streamedBytes) != "mp4data" {
		t.Errorf("streamed body = %q, %v", streamedBytes, err)
	}

	provider, _ = newMockKling(t, nil, http.StatusOK, strings.Replace(succeededBody, `"status":"succeeded"`, `"status":"processing"`, 1))
	if _, bifrostErr := provider.VideoDownload(testContext(t), testKey(), &schemas.BifrostVideoDownloadRequest{Provider: schemas.Kling, ID: klingTaskID}); bifrostErr == nil || *bifrostErr.StatusCode != http.StatusBadRequest {
		t.Errorf("err = %+v, want 400 for a task that is still running", bifrostErr)
	}
}

// A download's task query is part of the download: a custom provider that allows only
// video_download can download, and its video_download path override, not the retrieve one,
// replaces the query's path.
func TestKlingVideoDownloadIsGatedAndRoutedAsADownload(t *testing.T) {
	videoProxy := proxytest.ForwardProxy(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write([]byte("mp4data"))
	})
	provider, captured := newMockKling(t, &schemas.ProviderConfig{CustomProviderConfig: &schemas.CustomProviderConfig{
		CustomProviderKey: "tkp_kling_dl",
		BaseProviderType:  schemas.Kling,
		AllowedRequests:   &schemas.AllowedRequests{VideoDownload: true},
		RequestPathOverrides: map[schemas.RequestType]string{
			schemas.VideoDownloadRequest: "/download-tasks",
			schemas.VideoRetrieveRequest: "/retrieve-tasks",
		},
	}}, http.StatusOK, succeededBody)
	ctx := testContext(t)
	ctx.SetValue(schemas.BifrostContextKeyProviderProxyConfig, &schemas.ProxyConfig{Type: schemas.HTTPProxy, URL: schemas.NewSecretVar(videoProxy.URL)})

	downloaded, bifrostErr := provider.VideoDownload(ctx, testKey(), &schemas.BifrostVideoDownloadRequest{Provider: "tkp_kling_dl", ID: klingTaskID + ":tkp_kling_dl"})
	if bifrostErr != nil {
		t.Fatalf("VideoDownload error: %+v", bifrostErr.Error)
	}
	if string(downloaded.Content) != "mp4data" {
		t.Errorf("content = %q", downloaded.Content)
	}
	if req := onlyRequest(t, captured); req.path != "/download-tasks" || req.query != "task_ids="+klingTaskID {
		t.Errorf("query = %s?%s, want the video_download override", req.path, req.query)
	}

	if _, bifrostErr := provider.VideoRetrieve(testContext(t), testKey(), &schemas.BifrostVideoRetrieveRequest{Provider: "tkp_kling_dl", ID: klingTaskID}); bifrostErr == nil {
		t.Error("video retrieve ran on a custom provider that allows only downloads")
	}
	if n := len(captured()); n != 1 {
		t.Errorf("upstream calls = %d, want 1", n)
	}
}

// Kling has no cancel API and no delete API: the provider does not implement VideoCanceller, so
// core answers the cancel route 400 provider_cancel_unsupported before a key is chosen, and the
// delete is unsupported. Neither reaches Kling.
func TestKlingCancelAndDeleteAreUnsupported(t *testing.T) {
	provider, captured := newMockKling(t, nil, http.StatusOK, succeededBody)
	if _, ok := interface{}(provider).(schemas.VideoCanceller); ok {
		t.Fatal("kling must not implement schemas.VideoCanceller: Kling has no cancel API")
	}
	bifrostErr := schemas.CheckVideoCancelSupported(provider, &schemas.BifrostRequest{
		RequestType:        schemas.VideoCancelRequest,
		VideoCancelRequest: &schemas.BifrostVideoCancelRequest{Provider: schemas.Kling, ID: klingTaskID},
	})
	assertRefusal(t, bifrostErr, schemas.ErrorTypeProviderCancelUnsupported, "")

	if _, bifrostErr := provider.VideoDelete(testContext(t), testKey(), &schemas.BifrostVideoDeleteRequest{Provider: schemas.Kling, ID: klingTaskID}); bifrostErr == nil {
		t.Error("video delete must be unsupported")
	}
	if _, bifrostErr := provider.ChatCompletion(testContext(t), testKey(), &schemas.BifrostChatRequest{Model: "kling-3.0"}); bifrostErr == nil {
		t.Error("chat must be unsupported")
	}
	if n := len(captured()); n != 0 {
		t.Errorf("upstream calls = %d, want 0", n)
	}
}

// A custom provider built on kling (Tokun's tkp_<id>) goes by its own name in the task id, uses
// its request_path_overrides, and is gated by its allowed_requests.
func TestKlingAsCustomProviderBase(t *testing.T) {
	if !bifrost.IsSupportedBaseProvider(schemas.Kling) {
		t.Fatal("kling must be accepted as base_provider_type")
	}

	provider, captured := newMockKling(t, &schemas.ProviderConfig{CustomProviderConfig: &schemas.CustomProviderConfig{
		CustomProviderKey:    "tkp_kling",
		BaseProviderType:     schemas.Kling,
		AllowedRequests:      &schemas.AllowedRequests{VideoGeneration: true, VideoRetrieve: true},
		RequestPathOverrides: map[schemas.RequestType]string{schemas.VideoGenerationRequest: "/text-to-video/kling-3.0-canary"},
	}}, http.StatusOK, createBody)
	if got := provider.GetProviderKey(); got != "tkp_kling" {
		t.Errorf("provider key = %q, want tkp_kling", got)
	}
	resp, bifrostErr := provider.VideoGeneration(testContext(t), testKey(), videoRequest("kling-3.0", nil, nil))
	if bifrostErr != nil {
		t.Fatalf("VideoGeneration error: %+v", bifrostErr.Error)
	}
	if resp.ID != klingTaskID+":tkp_kling" {
		t.Errorf("id = %q, want the custom provider's suffix", resp.ID)
	}
	if got := onlyRequest(t, captured).path; got != "/text-to-video/kling-3.0-canary" {
		t.Errorf("path = %q, want the override", got)
	}
	if _, bifrostErr := provider.VideoList(testContext(t), testKey(), &schemas.BifrostVideoListRequest{Provider: "tkp_kling"}); bifrostErr == nil {
		t.Error("video list ran on a custom provider that does not allow it")
	}
	if n := len(captured()); n != 1 {
		t.Errorf("upstream calls = %d, want 1", n)
	}
}

func BenchmarkToKlingVideoGenerationRequest(b *testing.B) {
	params := &schemas.VideoGenerationParameters{
		Seconds: schemas.Ptr("10"), Resolution: "1080p", Audio: schemas.Ptr(true), LastFrame: "https://example.com/last.png",
		TaskControlParameters: tokunControl(),
		ExtraParams:           map[string]interface{}{"watermark": false, "multi_shot": false},
	}
	text := videoRequest("kling-3.0", nil, &schemas.VideoGenerationParameters{Seconds: schemas.Ptr("5"), AspectRatio: "16:9", TaskControlParameters: tokunControl()})
	image := videoRequest("kling-3.0", &schemas.VideoGenerationInput{Prompt: "p", InputReference: schemas.Ptr("https://example.com/first.png")}, params)
	for name, req := range map[string]*schemas.BifrostVideoGenerationRequest{"text-to-video": text, "image-to-video": image} {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := kling.ToKlingVideoGenerationRequest(req); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkToBifrostKlingVideoGenerationResponse(b *testing.B) {
	var task kling.KlingTask
	if err := schemas.Unmarshal([]byte(succeededTask), &task); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, bifrostErr := kling.ToBifrostVideoGenerationResponse(&task); bifrostErr != nil {
			b.Fatal(bifrostErr.Error.Message)
		}
	}
}

func BenchmarkKlingVideoRoundTrip(b *testing.B) {
	create, _ := newMockKling(b, nil, http.StatusOK, createBody)
	query, _ := newMockKling(b, nil, http.StatusOK, succeededBody)
	req := videoRequest("kling-3.0", nil, &schemas.VideoGenerationParameters{Seconds: schemas.Ptr("5"), TaskControlParameters: tokunControl()})
	retrieve := &schemas.BifrostVideoRetrieveRequest{Provider: schemas.Kling, ID: klingTaskID + ":kling"}
	ctx, cancel := schemas.NewBifrostContextWithTimeout(context.Background(), time.Minute)
	b.Cleanup(cancel)
	b.Run("create", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, bifrostErr := create.VideoGeneration(ctx, testKey(), req); bifrostErr != nil {
				b.Fatal(bifrostErr.Error.Message)
			}
		}
	})
	b.Run("retrieve", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, bifrostErr := query.VideoRetrieve(ctx, testKey(), retrieve); bifrostErr != nil {
				b.Fatal(bifrostErr.Error.Message)
			}
		}
	})
}
