package byteplus_test

import (
	"net/http"
	"testing"

	"github.com/capsohq/bifrost/core/providers/byteplus"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/tidwall/gjson"
)

// Fork (TXE-6426): Seedance video against the Bifrost–Tokun contract (C4, C5, C6, C7, C9).

const seedanceTasksPath = "/api/v3/contents/generations/tasks"

func seedanceRequest(params *schemas.VideoGenerationParameters) *schemas.BifrostVideoGenerationRequest {
	return &schemas.BifrostVideoGenerationRequest{
		Provider: schemas.BytePlus,
		Model:    "dreamina-seedance-2-0-fast-260128",
		Input:    &schemas.VideoGenerationInput{Prompt: "a paper boat on a pond"},
		Params:   params,
	}
}

// The unified fields reach Ark under Ark's names with the values the caller sent. The HTTP handler
// keeps a copy of aspect_ratio, resolution, last_frame and reference_images in ExtraParams; none
// of those copies may reach Ark under its unified name.
func TestSeedanceCreateSendsUnifiedFieldsAsArkFields(t *testing.T) {
	provider, captured, _ := newMockBytePlus(t, map[string]func(http.ResponseWriter, *http.Request){
		"POST " + seedanceTasksPath: jsonResponse(http.StatusOK, `{"id":"cgt-1"}`),
	})

	withFrames := &schemas.VideoGenerationParameters{
		Seconds:     schemas.Ptr("5"),
		AspectRatio: "9:16",
		Resolution:  "1080p",
		LastFrame:   "https://example.com/last.png",
		Audio:       schemas.Ptr(true),
		TaskControlParameters: schemas.TaskControlParameters{
			CallbackURL:         schemas.Ptr("https://hooks.example.com/v/task-1"),
			ExpiresAfterSeconds: schemas.Ptr(7200),
		},
		ExtraParams: map[string]any{"aspect_ratio": "9:16", "resolution": "1080p", "last_frame": "https://example.com/last.png", "watermark": false},
	}
	if _, bifrostErr := provider.VideoGeneration(testContext(t), testKey(), seedanceRequest(withFrames)); bifrostErr != nil {
		t.Fatalf("create: %+v", bifrostErr.Error)
	}
	withReferences := &schemas.VideoGenerationParameters{
		Size:            "1280x720",
		ReferenceImages: schemas.VideoReferenceImages{"https://example.com/a.png", "https://example.com/b.png"},
		ExtraParams:     map[string]any{"reference_images": []string{"https://example.com/a.png", "https://example.com/b.png"}},
	}
	if _, bifrostErr := provider.VideoGeneration(testContext(t), testKey(), seedanceRequest(withReferences)); bifrostErr != nil {
		t.Fatalf("create with references: %+v", bifrostErr.Error)
	}

	calls := captured()
	if len(calls) != 2 {
		t.Fatalf("upstream calls = %d, want 2", len(calls))
	}
	body := calls[0].body
	for path, want := range map[string]string{
		"duration":                "5",
		"ratio":                   "9:16",
		"resolution":              "1080p",
		"generate_audio":          "true",
		"callback_url":            "https://hooks.example.com/v/task-1",
		"execution_expires_after": "7200",
		"watermark":               "false",
		"content.#":               "2",
		"content.1.role":          "last_frame",
		"content.1.image_url.url": "https://example.com/last.png",
	} {
		if got := gjson.GetBytes(body, path).String(); got != want {
			t.Errorf("%s = %q, want %q; body %s", path, got, want, body)
		}
	}
	for _, unified := range []string{"aspect_ratio", "last_frame", "expires_after_seconds", "seconds", "size"} {
		if gjson.GetBytes(body, unified).Exists() {
			t.Errorf("unified field %q reached Ark under its unified name: %s", unified, body)
		}
	}
	body = calls[1].body
	for path, want := range map[string]string{
		"ratio":                   "16:9",
		"resolution":              "720p",
		"content.#":               "3",
		"content.1.role":          "reference_image",
		"content.2.role":          "reference_image",
		"content.2.image_url.url": "https://example.com/b.png",
	} {
		if got := gjson.GetBytes(body, path).String(); got != want {
			t.Errorf("%s = %q, want %q; body %s", path, got, want, body)
		}
	}
	if gjson.GetBytes(body, "reference_images").Exists() {
		t.Errorf("reference_images reached Ark under its unified name: %s", body)
	}
}

// seconds is whole seconds or "-1" (the model chooses); "5.0" is a whole number.
func TestSeedanceDurationForms(t *testing.T) {
	for seconds, want := range map[string]int64{"5": 5, "5.0": 5, "-1": -1, "2": 2, "15": 15} {
		req, err := byteplus.ToBytePlusVideoGenerationRequest(seedanceRequest(&schemas.VideoGenerationParameters{Seconds: schemas.Ptr(seconds)}))
		if err != nil {
			t.Fatalf("seconds %q: %v", seconds, err)
		}
		if req.Duration == nil || int64(*req.Duration) != want {
			t.Errorf("seconds %q: duration = %v, want %d", seconds, req.Duration, want)
		}
	}
}

// A value Ark cannot take as given is refused with caller_unsupported_parameter naming the field,
// before any upstream call: nothing is rounded, clamped or replaced by a nearest value.
func TestSeedanceCreateRefusesWhatArkCannotTakeAsGiven(t *testing.T) {
	provider, captured, _ := newMockBytePlus(t, map[string]func(http.ResponseWriter, *http.Request){
		"POST " + seedanceTasksPath: jsonResponse(http.StatusOK, `{"id":"cgt-1"}`),
	})
	cases := []struct {
		name   string
		param  string
		params *schemas.VideoGenerationParameters
	}{
		{"fractional seconds", "seconds", &schemas.VideoGenerationParameters{Seconds: schemas.Ptr("5.5")}},
		{"seconds below every model's range", "seconds", &schemas.VideoGenerationParameters{Seconds: schemas.Ptr("1")}},
		{"seconds above every model's range", "seconds", &schemas.VideoGenerationParameters{Seconds: schemas.Ptr("16")}},
		{"seconds not a number", "seconds", &schemas.VideoGenerationParameters{Seconds: schemas.Ptr("five")}},
		{"size with no exact ratio", "size", &schemas.VideoGenerationParameters{Size: "1000x720"}},
		{"size with no tier", "size", &schemas.VideoGenerationParameters{Size: "1600x900"}},
		{"size that is not WIDTHxHEIGHT", "size", &schemas.VideoGenerationParameters{Size: "720p"}},
		{"size disagreeing with resolution", "size", &schemas.VideoGenerationParameters{Size: "1280x720", Resolution: "1080p"}},
		{"size disagreeing with aspect_ratio", "size", &schemas.VideoGenerationParameters{Size: "1280x720", AspectRatio: "1:1"}},
		{"deadline below Ark's range", "expires_after_seconds", &schemas.VideoGenerationParameters{TaskControlParameters: schemas.TaskControlParameters{ExpiresAfterSeconds: schemas.Ptr(3599)}}},
		{"deadline above Ark's range", "expires_after_seconds", &schemas.VideoGenerationParameters{TaskControlParameters: schemas.TaskControlParameters{ExpiresAfterSeconds: schemas.Ptr(259201)}}},
	}
	for _, tc := range cases {
		_, bifrostErr := provider.VideoGeneration(testContext(t), testKey(), seedanceRequest(tc.params))
		if bifrostErr == nil {
			t.Errorf("%s: accepted", tc.name)
			continue
		}
		if bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusBadRequest || bifrostErr.ExtraFields.ErrorType != schemas.ErrorTypeCallerUnsupportedParameter || bifrostErr.Error == nil || bifrostErr.Error.Param != tc.param {
			t.Errorf("%s: status=%v error_type=%q error=%+v, want 400 caller_unsupported_parameter param %q", tc.name, bifrostErr.StatusCode, bifrostErr.ExtraFields.ErrorType, bifrostErr.Error, tc.param)
		}
	}
	if calls := captured(); len(calls) != 0 {
		t.Errorf("refused requests made %d upstream calls", len(calls))
	}
}

// The provider declares the unified fields it delivers, so core refuses the two task control
// fields Ark has no field for, and takes the two it has.
func TestBytePlusDeclaresSeedanceUnifiedFields(t *testing.T) {
	provider, _, _ := newMockBytePlus(t, nil)
	if _, declared := provider.SupportedUnifiedFields(schemas.ImageGenerationRequest); declared {
		t.Error("video's declaration must not cover image generation")
	}
	check := func(params *schemas.VideoGenerationParameters) *schemas.BifrostError {
		req := &schemas.BifrostRequest{RequestType: schemas.VideoGenerationRequest, VideoGenerationRequest: seedanceRequest(params)}
		if bifrostErr := schemas.CheckDeclaredUnifiedFields(provider, req); bifrostErr != nil {
			return bifrostErr
		}
		return schemas.CheckTaskControlFields(provider, req)
	}
	taken := &schemas.VideoGenerationParameters{
		Seconds: schemas.Ptr("5"), Size: "1280x720", Seed: schemas.Ptr(7), Audio: schemas.Ptr(true), OutputFormat: schemas.Ptr("mp4"),
		AspectRatio: "16:9", Resolution: "720p", LastFrame: "https://example.com/l.png", ReferenceImages: schemas.VideoReferenceImages{"https://example.com/r.png"},
		TaskControlParameters: schemas.TaskControlParameters{CallbackURL: schemas.Ptr("https://hooks.example.com/v/1"), ExpiresAfterSeconds: schemas.Ptr(3600)},
	}
	if bifrostErr := check(taken); bifrostErr != nil {
		t.Fatalf("a field the provider delivers was refused: %+v", bifrostErr.Error)
	}
	for field, params := range map[string]*schemas.VideoGenerationParameters{
		"client_reference_id": {TaskControlParameters: schemas.TaskControlParameters{ClientReferenceID: schemas.Ptr("task-1")}},
		"output_storage_uri":  {TaskControlParameters: schemas.TaskControlParameters{OutputStorageURI: schemas.Ptr("s3://bucket/task-1")}},
		"upscale_factor":      {UpscaleFactor: schemas.Ptr(2)},
	} {
		bifrostErr := check(params)
		if bifrostErr == nil || bifrostErr.ExtraFields.ErrorType != schemas.ErrorTypeCallerUnsupportedParameter || bifrostErr.Error.Param != field {
			t.Errorf("%s: got %+v, want caller_unsupported_parameter naming it", field, bifrostErr)
		}
	}
}

// The task object carries Ark's state under status and provider_status, Ark's token count,
// url artifacts and the link expiry of completion plus 24 hours.
func TestSeedanceTaskObject(t *testing.T) {
	const succeeded = `{"id":"cgt-1","model":"dreamina-seedance-2-0-fast-260128","status":"succeeded","content":{"video_url":"https://ark.example.com/v.mp4"},"usage":{"completion_tokens":108900,"total_tokens":108900},"created_at":1762700000,"updated_at":1762700090,"resolution":"720p","ratio":"16:9","duration":5}`
	tasks := map[string]string{
		"cgt-1":         succeeded,
		"cgt-queued":    `{"id":"cgt-queued","model":"m","status":"queued","created_at":1762700000,"updated_at":1762700000}`,
		"cgt-running":   `{"id":"cgt-running","model":"m","status":"running","created_at":1762700000,"updated_at":1762700010}`,
		"cgt-cancelled": `{"id":"cgt-cancelled","model":"m","status":"cancelled","created_at":1762700000,"updated_at":1762700020}`,
		"cgt-expired":   `{"id":"cgt-expired","model":"m","status":"expired","created_at":1762700000,"updated_at":1762786400}`,
		"cgt-blocked":   `{"id":"cgt-blocked","model":"m","status":"failed","error":{"code":"OutputVideoSensitiveContentDetected","message":"The request failed because the output video may contain sensitive information."},"created_at":1762700000,"updated_at":1762700050}`,
		"cgt-failed":    `{"id":"cgt-failed","model":"m","status":"failed","error":{"code":"InternalServiceError","message":"internal error"},"created_at":1762700000,"updated_at":1762700050}`,
		"cgt-new-state": `{"id":"cgt-new-state","model":"m","status":"paused","created_at":1762700000,"updated_at":1762700050}`,
	}
	routes := map[string]func(http.ResponseWriter, *http.Request){}
	for id, body := range tasks {
		routes["GET "+seedanceTasksPath+"/"+id] = jsonResponse(http.StatusOK, body)
	}
	provider, _, _ := newMockBytePlus(t, routes)
	get := func(id string) *schemas.BifrostVideoGenerationResponse {
		t.Helper()
		resp, bifrostErr := provider.VideoRetrieve(testContext(t), testKey(), &schemas.BifrostVideoRetrieveRequest{Provider: schemas.BytePlus, ID: id})
		if bifrostErr != nil {
			t.Fatalf("%s: %+v", id, bifrostErr.Error)
		}
		return resp
	}

	done := get("cgt-1")
	if done.Status != schemas.VideoStatusCompleted || done.ProviderStatus != "succeeded" {
		t.Errorf("status=%s provider_status=%q", done.Status, done.ProviderStatus)
	}
	if done.Usage == nil || done.Usage.OutputTokens == nil || *done.Usage.OutputTokens != 108900 {
		t.Errorf("usage.output_tokens = %+v, want Ark's completion_tokens 108900", done.Usage)
	}
	if done.CompletedAt == nil || *done.CompletedAt != 1762700090 || done.ExpiresAt == nil || *done.ExpiresAt != 1762700090+86400 {
		t.Errorf("completed_at=%v expires_at=%v, want updated_at and updated_at + 24 h", done.CompletedAt, done.ExpiresAt)
	}
	if len(done.Videos) != 1 || done.Videos[0].Type != schemas.VideoOutputTypeURL || done.Videos[0].URL == nil || *done.Videos[0].URL != "https://ark.example.com/v.mp4" {
		t.Errorf("videos = %+v", done.Videos)
	}
	if done.Seconds == nil || *done.Seconds != "5" || done.Size != "1280x720" {
		t.Errorf("seconds=%v size=%q", done.Seconds, done.Size)
	}
	if again := get("cgt-1"); *again.ExpiresAt != *done.ExpiresAt {
		t.Errorf("two queries of one task disagree on expires_at: %d and %d", *done.ExpiresAt, *again.ExpiresAt)
	}

	for id, want := range map[string]struct {
		status   schemas.VideoStatus
		provider string
	}{
		"cgt-queued":    {schemas.VideoStatusQueued, "queued"},
		"cgt-running":   {schemas.VideoStatusInProgress, "running"},
		"cgt-cancelled": {schemas.VideoStatusCancelled, "cancelled"},
		"cgt-expired":   {schemas.VideoStatusExpired, "expired"},
		"cgt-failed":    {schemas.VideoStatusFailed, "failed"},
		"cgt-new-state": {schemas.VideoStatusInProgress, "paused"},
	} {
		resp := get(id)
		if resp.Status != want.status || resp.ProviderStatus != want.provider {
			t.Errorf("%s: status=%s provider_status=%q, want %s and %q", id, resp.Status, resp.ProviderStatus, want.status, want.provider)
		}
		if resp.ExpiresAt != nil || resp.Usage != nil || resp.ContentFilter != nil {
			t.Errorf("%s: expires_at=%v usage=%+v content_filter=%+v, want none", id, resp.ExpiresAt, resp.Usage, resp.ContentFilter)
		}
	}

	blocked := get("cgt-blocked")
	if blocked.Status != schemas.VideoStatusFailed || blocked.Error == nil || blocked.Error.Code != "OutputVideoSensitiveContentDetected" {
		t.Errorf("blocked task: status=%s error=%+v", blocked.Status, blocked.Error)
	}
	if cf := blocked.ContentFilter; cf == nil || cf.FilteredCount != 1 || len(cf.Reasons) != 1 || cf.Reasons[0] != "OutputVideoSensitiveContentDetected" {
		t.Errorf("content_filter = %+v, want one item with Ark's code as the reason", blocked.ContentFilter)
	}
}

// A create Ark refuses for its input content is caller_content_filtered, with Ark's status and code kept.
func TestSeedanceCreateSensitiveInputIsContentFiltered(t *testing.T) {
	for code, want := range map[string]schemas.ErrorType{
		"InputTextSensitiveContentDetected":  schemas.ErrorTypeCallerContentFiltered,
		"InputImageSensitiveContentDetected": schemas.ErrorTypeCallerContentFiltered,
		"InvalidParameter":                   "",
	} {
		provider, _, _ := newMockBytePlus(t, map[string]func(http.ResponseWriter, *http.Request){
			"POST " + seedanceTasksPath: jsonResponse(http.StatusBadRequest, `{"error":{"code":"`+code+`","message":"refused","type":"BadRequest"}}`),
		})
		_, bifrostErr := provider.VideoGeneration(testContext(t), testKey(), seedanceRequest(nil))
		if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusBadRequest || bifrostErr.Error.Code == nil || *bifrostErr.Error.Code != code {
			t.Fatalf("%s: %+v", code, bifrostErr)
		}
		if bifrostErr.ExtraFields.ErrorType != want {
			t.Errorf("%s: error_type = %q, want %q", code, bifrostErr.ExtraFields.ErrorType, want)
		}
	}
}

// Cancel queries the task once and sends Ark's DELETE only while the task is queued: on a
// finished task DELETE would remove the record, so it is never sent.
func TestSeedanceCancel(t *testing.T) {
	task := func(id, status string) func(http.ResponseWriter, *http.Request) {
		return jsonResponse(http.StatusOK, `{"id":"`+id+`","model":"m","status":"`+status+`","created_at":1762700000,"updated_at":1762700010}`)
	}
	ok := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }
	provider, captured, _ := newMockBytePlus(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET " + seedanceTasksPath + "/cgt-queued":     task("cgt-queued", "queued"),
		"DELETE " + seedanceTasksPath + "/cgt-queued":  ok,
		"GET " + seedanceTasksPath + "/cgt-done":       task("cgt-done", "succeeded"),
		"DELETE " + seedanceTasksPath + "/cgt-done":    ok,
		"GET " + seedanceTasksPath + "/cgt-running":    task("cgt-running", "running"),
		"DELETE " + seedanceTasksPath + "/cgt-running": ok,
		"GET " + seedanceTasksPath + "/cgt-raced":      task("cgt-raced", "queued"),
		"DELETE " + seedanceTasksPath + "/cgt-raced":   jsonResponse(http.StatusConflict, `{"error":{"code":"InvalidAction","message":"the task is running"}}`),
	})
	var canceller schemas.VideoCanceller = provider
	count := func(method, id string) (n int) {
		for _, call := range captured() {
			if call.method == method && call.path == seedanceTasksPath+"/"+id {
				n++
			}
		}
		return n
	}

	cancelled, bifrostErr := canceller.VideoCancel(testContext(t), testKey(), &schemas.BifrostVideoCancelRequest{Provider: schemas.BytePlus, ID: "cgt-queued:byteplus"})
	if bifrostErr != nil {
		t.Fatalf("cancel of a queued task: %+v", bifrostErr.Error)
	}
	if cancelled.Status != schemas.VideoStatusCancelled || cancelled.ProviderStatus != "queued" || cancelled.ID != "cgt-queued:byteplus" {
		t.Errorf("cancelled task = id %q status %s provider_status %q", cancelled.ID, cancelled.Status, cancelled.ProviderStatus)
	}
	if count(http.MethodGet, "cgt-queued") != 1 || count(http.MethodDelete, "cgt-queued") != 1 {
		t.Errorf("queued task: %d queries and %d deletes, want one of each", count(http.MethodGet, "cgt-queued"), count(http.MethodDelete, "cgt-queued"))
	}

	for _, id := range []string{"cgt-done", "cgt-running", "cgt-raced"} {
		_, bifrostErr := canceller.VideoCancel(testContext(t), testKey(), &schemas.BifrostVideoCancelRequest{Provider: schemas.BytePlus, ID: id})
		if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusBadRequest || bifrostErr.ExtraFields.ErrorType != schemas.ErrorTypeProviderCancelUnsupported {
			t.Errorf("%s: got %+v, want 400 provider_cancel_unsupported", id, bifrostErr)
		}
	}
	if count(http.MethodDelete, "cgt-done") != 0 || count(http.MethodDelete, "cgt-running") != 0 {
		t.Error("DELETE was sent for a task that is not queued")
	}
	if count(http.MethodGet, "cgt-done") != 1 || count(http.MethodDelete, "cgt-raced") != 1 {
		t.Error("cancel must query once, and send DELETE once for a task the query reports queued")
	}
}
