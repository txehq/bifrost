package schemas

import (
	"reflect"
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

// TXE-6521 (contract C7): the task object serialises provider_status, each of the six statuses,
// expires_at, the terminal usage and a videos[] entry of type "content" that carries no url.
func TestVideoTaskObjectSerialisesContractFields(t *testing.T) {
	want := []VideoStatus{"queued", "in_progress", "completed", "failed", "cancelled", "expired"}
	if !slices.Equal(VideoStatuses, want) {
		t.Fatalf("VideoStatuses = %v, want %v", VideoStatuses, want)
	}
	for _, status := range VideoStatuses {
		out, err := Marshal(BifrostVideoGenerationResponse{ID: "t1:byteplus", Status: status, ProviderStatus: "VENDOR_RAW"})
		if err != nil {
			t.Fatalf("%s: marshal: %v", status, err)
		}
		var back map[string]any
		if err := Unmarshal(out, &back); err != nil {
			t.Fatalf("%s: unmarshal: %v", status, err)
		}
		if back["status"] != string(status) || back["provider_status"] != "VENDOR_RAW" {
			t.Fatalf("%s: serialised as %s", status, out)
		}
		wantTerminal := status != VideoStatusQueued && status != VideoStatusInProgress
		if status.IsTerminal() != wantTerminal {
			t.Fatalf("%s: IsTerminal = %v, want %v", status, status.IsTerminal(), wantTerminal)
		}
	}

	// A task object as a vendor ticket fills it at the terminal state, decoded and encoded again:
	// every contract field survives the round trip, including the usage fields of TXE-6518.
	const task = `{"id":"cgt-1:byteplus","completed_at":1762700000,"created_at":1762699000,` +
		`"expires_at":1762786400,"status":"completed","provider_status":"succeeded",` +
		`"videos":[{"type":"content","content_type":"video/mp4"},{"type":"url","url":"https://cdn.example/v.mp4","content_type":"video/mp4"}],` +
		`"usage":{"output_tokens":108900,"output_seconds":5,"vendor_cost":{"amount":0.25,"unit":"usd"}}}`
	var resp BifrostVideoGenerationResponse
	if err := Unmarshal([]byte(task), &resp); err != nil {
		t.Fatalf("unmarshal task object: %v", err)
	}
	if resp.ProviderStatus != "succeeded" || resp.Status != VideoStatusCompleted {
		t.Fatalf("status/provider_status = %q/%q", resp.Status, resp.ProviderStatus)
	}
	if resp.ExpiresAt == nil || *resp.ExpiresAt != 1762786400 {
		t.Fatalf("expires_at = %v", resp.ExpiresAt)
	}
	if len(resp.Videos) != 2 || resp.Videos[0].Type != VideoOutputTypeContent || resp.Videos[0].URL != nil {
		t.Fatalf("videos = %+v, want a content entry with no url first", resp.Videos)
	}
	out, err := Marshal(resp)
	if err != nil {
		t.Fatalf("marshal task object: %v", err)
	}
	var got, wantObj map[string]any
	if err := Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal round trip: %v", err)
	}
	if err := Unmarshal([]byte(task), &wantObj); err != nil {
		t.Fatalf("unmarshal want: %v", err)
	}
	delete(got, "extra_fields")
	if !reflect.DeepEqual(got, wantObj) {
		t.Fatalf("task object round trip\n got: %v\nwant: %v", got, wantObj)
	}
	content := got["videos"].([]any)[0].(map[string]any)
	if _, hasURL := content["url"]; hasURL {
		t.Fatalf("a content artifact serialised a url: %v", content)
	}
}

// TXE-6521 (contract C7 BF12): the four task control fields decode at the top level of the create
// parameters, and an unset one is neither serialised nor reported as present.
func TestTaskControlParametersDecode(t *testing.T) {
	var p VideoGenerationParameters
	body := `{"seconds":"5","callback_url":"https://hooks.tokun.sh/v/task_1","client_reference_id":"task_1",` +
		`"expires_after_seconds":7200,"output_storage_uri":"gs://bucket/task_1/"}`
	if err := Unmarshal([]byte(body), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.CallbackURL == nil || *p.CallbackURL != "https://hooks.tokun.sh/v/task_1" {
		t.Fatalf("callback_url = %v", p.CallbackURL)
	}
	if p.ClientReferenceID == nil || *p.ClientReferenceID != "task_1" {
		t.Fatalf("client_reference_id = %v", p.ClientReferenceID)
	}
	if p.ExpiresAfterSeconds == nil || *p.ExpiresAfterSeconds != 7200 {
		t.Fatalf("expires_after_seconds = %v", p.ExpiresAfterSeconds)
	}
	if p.OutputStorageURI == nil || *p.OutputStorageURI != "gs://bucket/task_1/" {
		t.Fatalf("output_storage_uri = %v", p.OutputStorageURI)
	}
	if got := p.Present(); !slices.Equal(got, TaskControlFields) {
		t.Fatalf("Present() = %v, want all four %v", got, TaskControlFields)
	}
	if !slices.IsSorted(TaskControlFields) {
		t.Fatalf("TaskControlFields is not sorted: %v", TaskControlFields)
	}

	only := VideoGenerationParameters{TaskControlParameters: TaskControlParameters{ClientReferenceID: Ptr("task_2")}}
	if got := only.Present(); !slices.Equal(got, []string{"client_reference_id"}) {
		t.Fatalf("Present() = %v, want only client_reference_id", got)
	}
	out, err := Marshal(only)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(out) != `{"client_reference_id":"task_2"}` {
		t.Fatalf("parameters marshal to %s", out)
	}
	if got := (*TaskControlParameters)(nil).Present(); got != nil {
		t.Fatalf("nil Present() = %v", got)
	}
}

// taskFieldDeclarer is a provider that declares which unified fields it takes for video create.
type taskFieldDeclarer struct {
	fields   []string
	declared bool
}

func (d taskFieldDeclarer) SupportedUnifiedFields(requestType RequestType) ([]string, bool) {
	if requestType != VideoGenerationRequest {
		return nil, false
	}
	return d.fields, d.declared
}

// TXE-6521 (contract C7, C4 convention three): a task control field is refused unless the provider
// declares it. A provider that declares nothing takes none.
func TestCheckTaskControlFields(t *testing.T) {
	request := func(control TaskControlParameters) *BifrostRequest {
		return &BifrostRequest{
			RequestType: VideoGenerationRequest,
			VideoGenerationRequest: &BifrostVideoGenerationRequest{
				Provider: BytePlus,
				Model:    "seedance",
				Params:   &VideoGenerationParameters{Seconds: Ptr("5"), TaskControlParameters: control},
			},
		}
	}
	all := TaskControlParameters{
		CallbackURL:         Ptr("https://hooks.tokun.sh/v/t"),
		ClientReferenceID:   Ptr("t"),
		ExpiresAfterSeconds: Ptr(3600),
		OutputStorageURI:    Ptr("gs://b/t/"),
	}
	cases := []struct {
		name      string
		provider  any
		control   TaskControlParameters
		wantParam string // "" = accepted
	}{
		{"no control field, provider declares nothing", struct{}{}, TaskControlParameters{}, ""},
		{"provider is not a declarer", struct{}{}, TaskControlParameters{CallbackURL: all.CallbackURL}, "callback_url"},
		{"provider declares nothing for the operation", taskFieldDeclarer{}, TaskControlParameters{OutputStorageURI: all.OutputStorageURI}, "output_storage_uri"},
		{"declared field is accepted", taskFieldDeclarer{fields: []string{"callback_url"}, declared: true}, TaskControlParameters{CallbackURL: all.CallbackURL}, ""},
		{"undeclared field next to a declared one", taskFieldDeclarer{fields: []string{"callback_url"}, declared: true}, TaskControlParameters{CallbackURL: all.CallbackURL, ExpiresAfterSeconds: all.ExpiresAfterSeconds}, "expires_after_seconds"},
		{"first unsupported field alphabetically", struct{}{}, all, "callback_url"},
		{"all four declared", taskFieldDeclarer{fields: TaskControlFields, declared: true}, all, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckTaskControlFields(tc.provider, request(tc.control))
			if tc.wantParam == "" {
				if err != nil {
					t.Fatalf("refused: %+v", err.Error)
				}
				return
			}
			if err == nil {
				t.Fatalf("accepted, want caller_unsupported_parameter for %s", tc.wantParam)
			}
			if err.StatusCode == nil || *err.StatusCode != 400 {
				t.Fatalf("status = %v, want 400", err.StatusCode)
			}
			if err.ExtraFields.ErrorType != ErrorTypeCallerUnsupportedParameter {
				t.Fatalf("error_type = %q", err.ExtraFields.ErrorType)
			}
			if err.Error == nil || err.Error.Param != tc.wantParam {
				t.Fatalf("error.param = %v, want %s", err.Error.Param, tc.wantParam)
			}
		})
	}

	// A request that is not a task create has no control fields to check.
	if err := CheckTaskControlFields(struct{}{}, &BifrostRequest{RequestType: VideoRetrieveRequest, VideoRetrieveRequest: &BifrostVideoRetrieveRequest{Provider: BytePlus, ID: "t"}}); err != nil {
		t.Fatalf("a retrieve was refused: %+v", err.Error)
	}
	if err := CheckTaskControlFields(struct{}{}, nil); err != nil {
		t.Fatalf("a nil request was refused: %+v", err.Error)
	}
}

// videoCancellerStub is a provider whose vendor has a cancel API.
type videoCancellerStub struct{}

func (videoCancellerStub) VideoCancel(*BifrostContext, Key, *BifrostVideoCancelRequest) (*BifrostVideoGenerationResponse, *BifrostError) {
	return &BifrostVideoGenerationResponse{Status: VideoStatusCancelled}, nil
}

// TXE-6521 (contract C7 BF13): a cancel on a provider with no cancel API is 400
// provider_cancel_unsupported; the video cancel request type resolves its provider like the other
// id-addressed video requests and follows the delete permission of a custom provider.
func TestVideoCancelRequestType(t *testing.T) {
	req := &BifrostRequest{RequestType: VideoCancelRequest, VideoCancelRequest: &BifrostVideoCancelRequest{Provider: BytePlus, ID: "cgt-1"}}

	err := CheckVideoCancelSupported(struct{}{}, req)
	if err == nil {
		t.Fatal("a provider with no cancel API was accepted")
	}
	if err.StatusCode == nil || *err.StatusCode != 400 || err.ExtraFields.ErrorType != ErrorTypeProviderCancelUnsupported {
		t.Fatalf("status/error_type = %v/%q, want 400/provider_cancel_unsupported", err.StatusCode, err.ExtraFields.ErrorType)
	}
	if err.IsBifrostError {
		t.Fatal("provider_cancel_unsupported is the vendor's limit, not a Bifrost failure")
	}
	if err := CheckVideoCancelSupported(videoCancellerStub{}, req); err != nil {
		t.Fatalf("a provider with a cancel API was refused: %+v", err.Error)
	}
	if err := CheckVideoCancelSupported(struct{}{}, &BifrostRequest{RequestType: VideoRetrieveRequest}); err != nil {
		t.Fatalf("a retrieve was refused: %+v", err.Error)
	}

	provider, model, fallbacks := req.GetRequestFields()
	if provider != BytePlus || model != "" || fallbacks != nil {
		t.Fatalf("GetRequestFields = %q/%q/%v", provider, model, fallbacks)
	}
	req.SetProvider(MiniMax)
	if req.VideoCancelRequest.Provider != MiniMax {
		t.Fatalf("SetProvider left %q", req.VideoCancelRequest.Provider)
	}

	if !(&AllowedRequests{VideoDelete: true}).IsOperationAllowed(VideoCancelRequest) {
		t.Fatal("cancel is refused on a custom provider that allows delete")
	}
	if (&AllowedRequests{VideoRetrieve: true}).IsOperationAllowed(VideoCancelRequest) {
		t.Fatal("cancel is allowed on a custom provider that does not allow delete")
	}
}
