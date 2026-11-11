package xai

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bytedance/sonic"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

// The vendor bodies in these tests follow xAI's batch guide (docs.x.ai, "Batch API",
// updated 2026-10-20): create by name, POST /requests with batch_requests[].batch_request_id
// and batch_request.chat_get_completion, results under results[].batch_result.response with
// usage.cost_in_usd_ticks, and pagination_token paging (TXE-6664, contract C8).

// pinnedNow is the instant every test in this package runs at. The vendor bodies here are
// recorded around it (created 2026-11-10T04:00Z, expiring 2026-11-11T04:00Z), and the batch
// status mapping compares a batch's expire_time with nowFunc, so under the real clock a
// recorded in-progress batch reads as expired from the moment its recorded expiry passes
// (TXE-7090: TestBatchCreateInlineChatLines failed on main from 2026-11-11T04:00Z). TestMain
// pins nowFunc for the whole package; a test that needs another instant sets nowFunc itself
// and restores it. TestXAIReadsTheClockOnlyThroughNowFunc keeps the pin effective.
var pinnedNow = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

func TestMain(m *testing.M) {
	nowFunc = func() time.Time { return pinnedNow }
	os.Exit(m.Run())
}

// TestXAIReadsTheClockOnlyThroughNowFunc fails when this package's code reads the real clock
// other than through nowFunc: such a read would compare recorded vendor timestamps with
// today's date again, and the tests would start failing on a date nobody chose.
func TestXAIReadsTheClockOnlyThroughNowFunc(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	clockRead := regexp.MustCompile(`\btime\.(Now|Since|Until)\b`)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			if clockRead.MatchString(line) && strings.TrimSpace(line) != "var nowFunc = time.Now" {
				t.Errorf("%s:%d reads the real clock: %s; read it through nowFunc so the tests run at pinnedNow", name, i+1, strings.TrimSpace(line))
			}
		}
	}
}

type recordedRequest struct {
	Method string
	Path   string
	Query  string
	Auth   string
	Body   string
}

type batchStub struct {
	mu       sync.Mutex
	requests []recordedRequest
	handler  func(w http.ResponseWriter, r *http.Request, body string)
}

func (s *batchStub) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.requests = append(s.requests, recordedRequest{
			Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery,
			Auth: r.Header.Get("Authorization"), Body: string(raw),
		})
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		s.handler(w, r, string(raw))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (s *batchStub) recorded() []recordedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recordedRequest(nil), s.requests...)
}

func newBatchTestProvider(baseURL string) *XAIProvider {
	return &XAIProvider{
		client:        &fasthttp.Client{ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second},
		networkConfig: schemas.NetworkConfig{BaseURL: baseURL},
		logger:        streamCostTestLogger{},
	}
}

func batchTestCtx() *schemas.BifrostContext {
	return schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
}

func batchKey(name, value string) schemas.Key {
	return schemas.Key{Name: name, Value: *schemas.NewSecretVar(value)}
}

func decodeJSON(t *testing.T, raw string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := sonic.UnmarshalString(raw, &out); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	return out
}

// An inline batch reaches xAI as a batch named from its metadata, then one request per line
// whose batch_request_id is the line's custom_id and whose chat_get_completion is the line's
// body as the sync chat route would send it. Both calls carry the key's bearer token.
func TestBatchCreateInlineChatLines(t *testing.T) {
	stub := &batchStub{}
	stub.handler = func(w http.ResponseWriter, r *http.Request, _ string) {
		switch r.URL.Path {
		case "/v1/batches":
			_, _ = io.WriteString(w, `{"batch_id":"batch_7f3a","name":"tokun_batch_id=batch_01JX","create_time":"2026-11-10T04:00:00Z","expire_time":"2026-11-11T04:00:00Z","state":{"num_requests":0,"num_pending":0,"num_success":0,"num_error":0,"num_cancelled":0}}`)
		case "/v1/batches/batch_7f3a/requests":
			_, _ = io.WriteString(w, `{}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
	srv := stub.serve(t)
	provider := newBatchTestProvider(srv.URL)

	resp, bErr := provider.BatchCreate(batchTestCtx(), batchKey("xai-a", "sk-xai-a"), &schemas.BifrostBatchCreateRequest{
		Provider:         schemas.XAI,
		Model:            schemas.Ptr("xai/grok-4.3"),
		Endpoint:         schemas.BatchEndpointChatCompletions,
		CompletionWindow: "24h",
		Metadata:         map[string]string{"tokun_batch_id": "batch_01JX"},
		Requests: []schemas.BatchRequestItem{
			{CustomID: "line-1", Method: "POST", URL: "/v1/chat/completions", Body: map[string]any{
				"model": "grok-4.3", "temperature": 0.2, "stream": true,
				"messages": []any{map[string]any{"role": "user", "content": "Classify: great product"}},
			}},
			{CustomID: "line-2", Method: "POST", URL: "/v1/chat/completions", Body: map[string]any{
				"model":    "xai/grok-4.3",
				"messages": []any{map[string]any{"role": "system", "content": "Be terse."}, map[string]any{"role": "user", "content": "Classify: late delivery"}},
			}},
		},
	})
	if bErr != nil {
		t.Fatalf("BatchCreate: %+v", bErr.Error)
	}

	calls := stub.recorded()
	if len(calls) != 2 {
		t.Fatalf("upstream calls = %d, want 2 (create, add requests): %+v", len(calls), calls)
	}
	create, add := calls[0], calls[1]
	if create.Method != http.MethodPost || create.Path != "/v1/batches" || create.Auth != "Bearer sk-xai-a" {
		t.Fatalf("create call = %+v", create)
	}
	if got := decodeJSON(t, create.Body); len(got) != 1 || got["name"] != "tokun_batch_id=batch_01JX" {
		t.Fatalf("create body = %s, want only the metadata-carrying name", create.Body)
	}
	if add.Method != http.MethodPost || add.Path != "/v1/batches/batch_7f3a/requests" || add.Auth != "Bearer sk-xai-a" {
		t.Fatalf("add call = %+v", add)
	}
	lines, _ := decodeJSON(t, add.Body)["batch_requests"].([]any)
	if len(lines) != 2 {
		t.Fatalf("batch_requests = %s, want 2 lines", add.Body)
	}
	for i, want := range []string{"line-1", "line-2"} {
		line := lines[i].(map[string]any)
		if line["batch_request_id"] != want {
			t.Fatalf("line %d batch_request_id = %v, want %s", i, line["batch_request_id"], want)
		}
		chat, ok := line["batch_request"].(map[string]any)["chat_get_completion"].(map[string]any)
		if !ok {
			t.Fatalf("line %d has no chat_get_completion: %v", i, line)
		}
		if chat["model"] != "grok-4.3" {
			t.Fatalf("line %d model = %v, want grok-4.3 (provider prefix stripped)", i, chat["model"])
		}
		if _, streamed := chat["stream"]; streamed {
			t.Fatalf("line %d still carries stream: %v", i, chat)
		}
		if msgs, _ := chat["messages"].([]any); len(msgs) == 0 {
			t.Fatalf("line %d lost its messages: %v", i, chat)
		}
	}
	if first := lines[0].(map[string]any)["batch_request"].(map[string]any)["chat_get_completion"].(map[string]any); first["temperature"] != 0.2 {
		t.Fatalf("line-1 temperature = %v, want 0.2", first["temperature"])
	}

	if resp.ID != "batch_7f3a" || resp.Status != schemas.BatchStatusInProgress {
		t.Fatalf("id/status = %s/%s, want batch_7f3a/in_progress", resp.ID, resp.Status)
	}
	if resp.RequestCounts.Total != 2 || resp.RequestCounts.Pending != 2 {
		t.Fatalf("request_counts = %+v, want total 2 pending 2", resp.RequestCounts)
	}
	if resp.Metadata["tokun_batch_id"] != "batch_01JX" {
		t.Fatalf("metadata = %v, want tokun_batch_id round-tripped through the name", resp.Metadata)
	}
	if resp.CreatedAt != time.Date(2026, 11, 10, 4, 0, 0, 0, time.UTC).Unix() || resp.ExpiresAt == nil {
		t.Fatalf("created_at=%d expires_at=%v", resp.CreatedAt, resp.ExpiresAt)
	}
}

// A line's unmodelled fields are extra params, as on the sync chat route: they reach xAI
// only when passthrough extra params is enabled, and are dropped otherwise, the same gate
// the sync route applies (review finding on PR 151).
func TestBatchLineExtraParamsFollowPassthrough(t *testing.T) {
	item := schemas.BatchRequestItem{CustomID: "line-1", URL: "/v1/chat/completions", Body: map[string]any{
		"model":             "grok-4.3",
		"messages":          []any{map[string]any{"role": "user", "content": "hi"}},
		"search_parameters": map[string]any{"mode": "off"},
	}}
	chatBody := func(passthrough bool) map[string]any {
		ctx := batchTestCtx()
		if passthrough {
			ctx.SetValue(schemas.BifrostContextKeyPassthroughExtraParams, true)
		}
		lines, bErr := ToXAIBatchRequests(ctx, schemas.BatchEndpointChatCompletions, "grok-4.3", nil, []schemas.BatchRequestItem{item})
		if bErr != nil {
			t.Fatalf("ToXAIBatchRequests: %+v", bErr.Error)
		}
		return decodeJSON(t, string(lines[0].BatchRequest[xaiBatchChatKey]))
	}
	if got := chatBody(true); got["search_parameters"] == nil {
		t.Fatalf("passthrough on: body = %v, want search_parameters forwarded", got)
	}
	if got := chatBody(false); got["search_parameters"] != nil {
		t.Fatalf("passthrough off: body = %v, want search_parameters dropped as on the sync route", got)
	}
}

// A line the sync route would refuse is refused with the sync route's error, its param
// the line's path to the field: a service_tier outside the model's catalog list
// (TXE-6527), a prompt-cache field the model cannot take (TXE-7043), both through
// openai.ChatRequestRefusal, and an extra param only the gateway may set
// (the shared merge, TXE-7031). Before, the line's tier went to xAI as sent and the
// merge's refusal lost its error_type and param.
func TestBatchLineRefusalsMatchTheSyncRoute(t *testing.T) {
	schemas.SetCapabilityResolver(func(_ schemas.ModelProvider, model string) *schemas.ModelCapabilities {
		if model == "grok-4.3" {
			return &schemas.ModelCapabilities{ServiceTiers: []string{"default"}}
		}
		return nil
	})
	t.Cleanup(func() { schemas.SetCapabilityResolver(nil) })
	line := func(extra map[string]any) schemas.BatchRequestItem {
		body := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}}
		for k, v := range extra {
			body[k] = v
		}
		return schemas.BatchRequestItem{CustomID: "line-1", URL: "/v1/chat/completions", Body: body}
	}
	cases := []struct {
		name      string
		item      schemas.BatchRequestItem
		errorType schemas.ErrorType
		param     string
	}{
		{"service_tier outside the catalog", line(map[string]any{"service_tier": "flex"}), schemas.ErrorTypeCallerUnsupportedParameter, "requests[1].body.service_tier"},
		{"prompt_cache_key the model cannot take", line(map[string]any{"prompt_cache_key": "conversation-1"}), schemas.ErrorTypeCallerUnsupportedParameter, "requests[1].body.prompt_cache_key"},
		{"prompt_cache_retention the model cannot take", line(map[string]any{"prompt_cache_retention": "24h"}), schemas.ErrorTypeCallerUnsupportedParameter, "requests[1].body.prompt_cache_retention"},
		{"extra param naming the deployment", line(map[string]any{"deployment": "other"}), schemas.ErrorTypeCallerInvalidRequest, "requests[1].body.deployment"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := batchTestCtx()
			ctx.SetValue(schemas.BifrostContextKeyPassthroughExtraParams, true)
			items := []schemas.BatchRequestItem{line(nil), tc.item}
			items[0].CustomID = "line-0"
			_, bErr := ToXAIBatchRequests(ctx, schemas.BatchEndpointChatCompletions, "grok-4.3", nil, items)
			if bErr == nil {
				t.Fatal("want the line refused")
			}
			if bErr.StatusCode == nil || *bErr.StatusCode != 400 || bErr.ExtraFields.ErrorType != tc.errorType {
				t.Fatalf("status=%v error_type=%q, want 400 %q", bErr.StatusCode, bErr.ExtraFields.ErrorType, tc.errorType)
			}
			if bErr.Error == nil || bErr.Error.Param != tc.param || !strings.HasPrefix(bErr.Error.Message, "requests[1].body: ") {
				t.Fatalf("error = %+v, want param %s and a message naming the line", bErr.Error, tc.param)
			}
		})
	}
	if _, bErr := ToXAIBatchRequests(batchTestCtx(), schemas.BatchEndpointChatCompletions, "grok-4.3", nil,
		[]schemas.BatchRequestItem{line(map[string]any{"service_tier": "default"})}); bErr != nil {
		t.Fatalf("a tier in the catalog list must pass: %+v", bErr.Error)
	}
}

// A batch runs one model (review finding on PR 151): governance picks the batch's provider
// key from the grants for the request's model, so every line runs as that model. A line
// without a model, or with the batch's model under a provider prefix or a key alias, runs as
// the batch's model; a line naming another model is refused before any call.
func TestBatchLinesRunAsTheBatchModel(t *testing.T) {
	chat := func(model any) map[string]any {
		body := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}}
		if model != nil {
			body["model"] = model
		}
		return body
	}
	aliases := schemas.KeyAliases{"cheap": {ModelID: "grok-4.3"}}
	items := []schemas.BatchRequestItem{
		{CustomID: "no-model", Body: chat(nil)},
		{CustomID: "prefixed", Body: chat("xai/grok-4.3")},
		{CustomID: "alias", Body: chat("cheap")},
	}
	lines, bErr := ToXAIBatchRequests(batchTestCtx(), schemas.BatchEndpointChatCompletions, "grok-4.3", aliases.Resolve, items)
	if bErr != nil {
		t.Fatalf("ToXAIBatchRequests: %+v", bErr.Error)
	}
	for _, line := range lines {
		if got := decodeJSON(t, string(line.BatchRequest[xaiBatchChatKey]))["model"]; got != "grok-4.3" {
			t.Fatalf("%s runs as %v, want grok-4.3", line.BatchRequestID, got)
		}
	}
	_, bErr = ToXAIBatchRequests(batchTestCtx(), schemas.BatchEndpointChatCompletions, "grok-4.3", aliases.Resolve,
		[]schemas.BatchRequestItem{{CustomID: "other", Body: chat("grok-4.6")}})
	if bErr == nil || bErr.Error.Param != "requests[0].body.model" {
		t.Fatalf("a line naming another model = %+v, want a 400 on requests[0].body.model", bErr)
	}
}

// A file batch is one call naming the uploaded JSONL file.
func TestBatchCreateWithInputFile(t *testing.T) {
	stub := &batchStub{}
	stub.handler = func(w http.ResponseWriter, _ *http.Request, _ string) {
		_, _ = io.WriteString(w, `{"batch_id":"batch_f1","name":"sentiment","state":{"num_requests":0}}`)
	}
	provider := newBatchTestProvider(stub.serve(t).URL)
	resp, bErr := provider.BatchCreate(batchTestCtx(), batchKey("k", "sk"), &schemas.BifrostBatchCreateRequest{
		Provider: schemas.XAI, InputFileID: "file-abc123", DisplayName: schemas.Ptr("sentiment"),
	})
	if bErr != nil {
		t.Fatalf("BatchCreate: %+v", bErr.Error)
	}
	calls := stub.recorded()
	if len(calls) != 1 {
		t.Fatalf("upstream calls = %d, want 1", len(calls))
	}
	body := decodeJSON(t, calls[0].Body)
	if body["name"] != "sentiment" || body["input_file_id"] != "file-abc123" {
		t.Fatalf("create body = %s", calls[0].Body)
	}
	if resp.Status != schemas.BatchStatusValidating || resp.Metadata != nil {
		t.Fatalf("status=%s metadata=%v, want validating and no metadata", resp.Status, resp.Metadata)
	}
}

// A line xAI batches cannot take, or a request whose name cannot hold both its display name
// and its metadata, is refused before any upstream call.
func TestBatchCreateRefusesBeforeDispatch(t *testing.T) {
	stub := &batchStub{}
	stub.handler = func(w http.ResponseWriter, _ *http.Request, _ string) { w.WriteHeader(http.StatusInternalServerError) }
	provider := newBatchTestProvider(stub.serve(t).URL)
	line := func(id, url string) schemas.BatchRequestItem {
		return schemas.BatchRequestItem{CustomID: id, URL: url, Body: map[string]any{
			"model": "grok-4.3", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}}
	}
	m := schemas.Ptr("grok-4.3")
	cases := []struct {
		name      string
		req       *schemas.BifrostBatchCreateRequest
		errorType schemas.ErrorType
		param     string
	}{
		{"embeddings line", &schemas.BifrostBatchCreateRequest{Model: m, Requests: []schemas.BatchRequestItem{line("a", "/v1/embeddings")}}, schemas.ErrorTypeCallerUnsupportedParameter, "url"},
		{"display name with metadata", &schemas.BifrostBatchCreateRequest{Model: m, DisplayName: schemas.Ptr("x"), Metadata: map[string]string{"k": "v"}, Requests: []schemas.BatchRequestItem{line("a", "/v1/chat/completions")}}, schemas.ErrorTypeCallerUnsupportedParameter, "display_name"},
		{"output_expires_after", &schemas.BifrostBatchCreateRequest{Model: m, OutputExpiresAfter: &schemas.BatchExpiresAfter{Anchor: "created_at", Seconds: 3600}, Requests: []schemas.BatchRequestItem{line("a", "/v1/chat/completions")}}, schemas.ErrorTypeCallerUnsupportedParameter, "output_expires_after"},
		{"duplicate custom_id", &schemas.BifrostBatchCreateRequest{Model: m, Requests: []schemas.BatchRequestItem{line("a", "/v1/chat/completions"), line("a", "/v1/chat/completions")}}, schemas.ErrorTypeCallerInvalidRequest, ""},
		{"missing custom_id", &schemas.BifrostBatchCreateRequest{Model: m, Requests: []schemas.BatchRequestItem{line("", "/v1/chat/completions")}}, schemas.ErrorTypeCallerInvalidRequest, ""},
		{"no batch model", &schemas.BifrostBatchCreateRequest{Requests: []schemas.BatchRequestItem{line("a", "/v1/chat/completions")}}, schemas.ErrorTypeCallerInvalidRequest, "model"},
		{"line names another model", &schemas.BifrostBatchCreateRequest{Model: schemas.Ptr("grok-4.6"), Requests: []schemas.BatchRequestItem{line("a", "/v1/chat/completions")}}, schemas.ErrorTypeCallerInvalidRequest, "requests[0].body.model"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, bErr := provider.BatchCreate(batchTestCtx(), batchKey("k", "sk"), tc.req)
			if bErr == nil {
				t.Fatal("want an error")
			}
			if bErr.StatusCode == nil || *bErr.StatusCode != 400 || bErr.ExtraFields.ErrorType != tc.errorType {
				t.Fatalf("status=%v error_type=%q, want 400 %q", bErr.StatusCode, bErr.ExtraFields.ErrorType, tc.errorType)
			}
			if tc.param != "" && bErr.Error.Param != tc.param {
				t.Fatalf("param = %v, want %s", bErr.Error.Param, tc.param)
			}
		})
	}
	if n := len(stub.recorded()); n != 0 {
		t.Fatalf("upstream calls = %d, want 0", n)
	}
}

// When xAI refuses the requests after creating the batch, its error comes back with the
// empty batch's id in the message, and Bifrost makes no further call.
func TestBatchCreateAddRequestsRefused(t *testing.T) {
	stub := &batchStub{}
	stub.handler = func(w http.ResponseWriter, r *http.Request, _ string) {
		if r.URL.Path == "/v1/batches" {
			_, _ = io.WriteString(w, `{"batch_id":"batch_9","name":"bifrost-batch","state":{}}`)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"code":"Client specified an invalid argument","error":"Model grok-2 is not supported for batch processing"}`)
	}
	provider := newBatchTestProvider(stub.serve(t).URL)
	_, bErr := provider.BatchCreate(batchTestCtx(), batchKey("k", "sk"), &schemas.BifrostBatchCreateRequest{
		Model: schemas.Ptr("grok-2"),
		Requests: []schemas.BatchRequestItem{{CustomID: "a", URL: "/v1/chat/completions", Body: map[string]any{
			"model": "grok-2", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}}},
	})
	if bErr == nil || bErr.StatusCode == nil || *bErr.StatusCode != 400 {
		t.Fatalf("want the vendor's 400, got %+v", bErr)
	}
	if !strings.Contains(bErr.Error.Message, "batch_9") || !strings.Contains(bErr.Error.Message, "not supported for batch processing") {
		t.Fatalf("message = %q, want the batch id and the vendor's text", bErr.Error.Message)
	}
	if n := len(stub.recorded()); n != 2 {
		t.Fatalf("upstream calls = %d, want 2", n)
	}
}

// xAI reports counters, not a status: retrieve derives the unified status from them and maps
// them to request_counts.
func TestBatchRetrieveMapsStateToStatus(t *testing.T) {
	prev := nowFunc
	nowFunc = func() time.Time { return time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { nowFunc = prev })

	cases := []struct {
		name string
		body string
		want schemas.BatchStatus
	}{
		{"running", `{"batch_id":"b","state":{"num_requests":100,"num_pending":25,"num_success":70,"num_error":5}}`, schemas.BatchStatusInProgress},
		{"done", `{"batch_id":"b","state":{"num_requests":100,"num_pending":0,"num_success":95,"num_error":5}}`, schemas.BatchStatusCompleted},
		{"empty", `{"batch_id":"b","state":{"num_requests":0,"num_pending":0}}`, schemas.BatchStatusValidating},
		{"cancelling", `{"batch_id":"b","cancel_time":"2026-11-10T11:00:00Z","state":{"num_requests":10,"num_pending":3,"num_success":7}}`, schemas.BatchStatusCancelling},
		{"cancelled", `{"batch_id":"b","cancel_time":"2026-11-10T11:00:00Z","state":{"num_requests":10,"num_pending":0,"num_success":7,"num_cancelled":3}}`, schemas.BatchStatusCancelled},
		{"expired", `{"batch_id":"b","expires_at":1762732800,"state":{"num_requests":10,"num_pending":4,"num_success":6}}`, schemas.BatchStatusExpired},
		{"not yet expired", `{"batch_id":"b","expire_time":"2026-11-11T04:00:00Z","state":{"num_requests":10,"num_pending":4}}`, schemas.BatchStatusInProgress},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &batchStub{}
			stub.handler = func(w http.ResponseWriter, _ *http.Request, _ string) { _, _ = io.WriteString(w, tc.body) }
			provider := newBatchTestProvider(stub.serve(t).URL)
			resp, bErr := provider.BatchRetrieve(batchTestCtx(), []schemas.Key{batchKey("k", "sk")}, &schemas.BifrostBatchRetrieveRequest{BatchID: "b"})
			if bErr != nil {
				t.Fatalf("BatchRetrieve: %+v", bErr.Error)
			}
			if resp.Status != tc.want {
				t.Fatalf("status = %s, want %s", resp.Status, tc.want)
			}
			if calls := stub.recorded(); len(calls) != 1 || calls[0].Method != http.MethodGet || calls[0].Path != "/v1/batches/b" {
				t.Fatalf("calls = %+v", calls)
			}
		})
	}

	stub := &batchStub{}
	stub.handler = func(w http.ResponseWriter, _ *http.Request, _ string) {
		_, _ = io.WriteString(w, `{"batch_id":"b","state":{"num_requests":100,"num_pending":20,"num_success":70,"num_error":5,"num_cancelled":5}}`)
	}
	provider := newBatchTestProvider(stub.serve(t).URL)
	resp, _ := provider.BatchRetrieve(batchTestCtx(), []schemas.Key{batchKey("k", "sk")}, &schemas.BifrostBatchRetrieveRequest{BatchID: "b"})
	want := schemas.BatchRequestCounts{Total: 100, Completed: 70, Failed: 5, Succeeded: 70, Canceled: 5, Pending: 20}
	if resp.RequestCounts != want {
		t.Fatalf("request_counts = %+v, want %+v", resp.RequestCounts, want)
	}
}

// Query, cancel and results with a named key (keys holds only that key, contract C3) make
// exactly one call, with that key.
func TestBatchFollowUpsUseOnlyTheGivenKey(t *testing.T) {
	stub := &batchStub{}
	stub.handler = func(w http.ResponseWriter, r *http.Request, _ string) {
		if strings.HasSuffix(r.URL.Path, "/results") {
			_, _ = io.WriteString(w, `{"results":[]}`)
			return
		}
		_, _ = io.WriteString(w, `{"batch_id":"b","state":{"num_requests":1,"num_pending":1}}`)
	}
	provider := newBatchTestProvider(stub.serve(t).URL)
	ctx := batchTestCtx()
	keys := []schemas.Key{batchKey("xai-b", "sk-xai-b")}
	if _, err := provider.BatchRetrieve(ctx, keys, &schemas.BifrostBatchRetrieveRequest{BatchID: "b"}); err != nil {
		t.Fatalf("retrieve: %+v", err.Error)
	}
	if _, err := provider.BatchCancel(ctx, keys, &schemas.BifrostBatchCancelRequest{BatchID: "b"}); err != nil {
		t.Fatalf("cancel: %+v", err.Error)
	}
	if _, err := provider.BatchResults(ctx, keys, &schemas.BifrostBatchResultsRequest{BatchID: "b"}); err != nil {
		t.Fatalf("results: %+v", err.Error)
	}
	calls := stub.recorded()
	if len(calls) != 3 {
		t.Fatalf("calls = %d, want 3", len(calls))
	}
	for _, c := range calls {
		if c.Auth != "Bearer sk-xai-b" {
			t.Fatalf("call %s %s used %q, want only the named key", c.Method, c.Path, c.Auth)
		}
	}
	if calls[1].Method != http.MethodPost || calls[1].Path != "/v1/batches/b:cancel" {
		t.Fatalf("cancel call = %+v, want POST /v1/batches/b:cancel", calls[1])
	}
}

// Cancel calls :cancel and returns the batch; a refusal keeps the vendor's status and is
// marked provider_cancel_unsupported, while a missing batch is not.
func TestBatchCancel(t *testing.T) {
	stub := &batchStub{}
	stub.handler = func(w http.ResponseWriter, _ *http.Request, _ string) {
		_, _ = io.WriteString(w, `{"batch_id":"b","cancel_time":"2026-11-10T11:00:00Z","state":{"num_requests":10,"num_pending":0,"num_success":4,"num_cancelled":6}}`)
	}
	provider := newBatchTestProvider(stub.serve(t).URL)
	resp, bErr := provider.BatchCancel(batchTestCtx(), []schemas.Key{batchKey("k", "sk")}, &schemas.BifrostBatchCancelRequest{BatchID: "b"})
	if bErr != nil {
		t.Fatalf("BatchCancel: %+v", bErr.Error)
	}
	if resp.Status != schemas.BatchStatusCancelled || resp.CancelledAt == nil || resp.RequestCounts.Canceled != 6 || resp.RequestCounts.Completed != 4 {
		t.Fatalf("cancel response = %+v", resp)
	}

	for _, tc := range []struct {
		status int
		marked bool
	}{{http.StatusBadRequest, true}, {http.StatusConflict, true}, {http.StatusPreconditionFailed, true}, {http.StatusNotFound, false}, {http.StatusInternalServerError, false}} {
		stub := &batchStub{}
		stub.handler = func(w http.ResponseWriter, _ *http.Request, _ string) {
			w.WriteHeader(tc.status)
			_, _ = io.WriteString(w, `{"code":"Failed precondition","error":"batch is already complete"}`)
		}
		provider := newBatchTestProvider(stub.serve(t).URL)
		_, bErr := provider.BatchCancel(batchTestCtx(), []schemas.Key{batchKey("k", "sk")}, &schemas.BifrostBatchCancelRequest{BatchID: "b"})
		if bErr == nil || bErr.StatusCode == nil || *bErr.StatusCode != tc.status {
			t.Fatalf("status %d: got %+v", tc.status, bErr)
		}
		if marked := bErr.ExtraFields.ErrorType == schemas.ErrorTypeProviderCancelUnsupported; marked != tc.marked {
			t.Fatalf("status %d: error_type = %q, marked=%v want %v", tc.status, bErr.ExtraFields.ErrorType, marked, tc.marked)
		}
		if bErr.Error == nil || bErr.Error.Message != "batch is already complete" {
			t.Fatalf("status %d: vendor message lost: %+v", tc.status, bErr.Error)
		}
	}
}

// List returns the team's batches with their names and metadata, paged by pagination_token.
func TestBatchList(t *testing.T) {
	stub := &batchStub{}
	stub.handler = func(w http.ResponseWriter, _ *http.Request, _ string) {
		_, _ = io.WriteString(w, `{"batches":[{"batch_id":"b1","name":"tokun_batch_id=batch_01JX","state":{"num_requests":2,"num_pending":0,"num_success":2}},{"batch_id":"b2","name":"customer_feedback_analysis","state":{"num_requests":5,"num_pending":5}}],"pagination_token":"page-2"}`)
	}
	provider := newBatchTestProvider(stub.serve(t).URL)
	resp, bErr := provider.BatchList(batchTestCtx(), []schemas.Key{batchKey("k", "sk")}, &schemas.BifrostBatchListRequest{Limit: 20})
	if bErr != nil {
		t.Fatalf("BatchList: %+v", bErr.Error)
	}
	calls := stub.recorded()
	if len(calls) != 1 || calls[0].Path != "/v1/batches" || calls[0].Query != "limit=20" {
		t.Fatalf("calls = %+v", calls)
	}
	if len(resp.Data) != 2 || resp.Data[0].ID != "b1" || resp.Data[0].Metadata["tokun_batch_id"] != "batch_01JX" || resp.Data[0].Status != schemas.BatchStatusCompleted {
		t.Fatalf("first batch = %+v", resp.Data[0])
	}
	if resp.Data[1].DisplayName == nil || *resp.Data[1].DisplayName != "customer_feedback_analysis" || resp.Data[1].Metadata != nil {
		t.Fatalf("second batch = %+v", resp.Data[1])
	}
	if !resp.HasMore || resp.NextCursor == nil || *resp.NextCursor == "" {
		t.Fatalf("has_more=%v next_cursor=%v, want a cursor for the vendor's token", resp.HasMore, resp.NextCursor)
	}

	_, _ = provider.BatchList(batchTestCtx(), []schemas.Key{batchKey("k", "sk")}, &schemas.BifrostBatchListRequest{After: resp.NextCursor})
	if calls := stub.recorded(); len(calls) != 2 || calls[1].Query != "pagination_token=page-2" {
		t.Fatalf("second page call = %+v", calls[len(calls)-1])
	}
}

// Results page through /results: after and limit go to xAI as pagination_token and limit,
// each line comes back by custom_id with its status, a succeeded line as an OpenAI chat body
// with the unified usage and vendor_cost in usd_ticks, a failed line with the vendor's error.
func TestBatchResultsTwoPages(t *testing.T) {
	pages := map[string]string{
		"": `{"results":[
			{"batch_request_id":"line-1","batch_result":{"response":{"chat_get_completion":{
				"id":"chatcmpl-1","object":"chat.completion","created":1794283200,"model":"grok-4.3",
				"choices":[{"index":0,"message":{"role":"assistant","content":"positive"},"finish_reason":"stop"}],
				"usage":{"prompt_tokens":32,"completion_tokens":1,"total_tokens":60,
					"prompt_tokens_details":{"cached_tokens":16},
					"completion_tokens_details":{"reasoning_tokens":27},
					"cost_in_usd_ticks":123450}}}}},
			{"batch_request_id":"line-2","error_message":"Model output exceeded the context window"}
		],"pagination_token":"tok-2"}`,
		"tok-2": `{"results":[
			{"batch_request_id":"line-3","batch_result":{"error":{"code":"invalid_argument","message":"messages must not be empty"}}},
			{"batch_request_id":"line-4","state":"cancelled","error_message":"batch was cancelled"}
		]}`,
	}
	stub := &batchStub{}
	stub.handler = func(w http.ResponseWriter, r *http.Request, _ string) {
		_, _ = io.WriteString(w, pages[r.URL.Query().Get("pagination_token")])
	}
	provider := newBatchTestProvider(stub.serve(t).URL)
	keys := []schemas.Key{batchKey("k", "sk")}

	first, bErr := provider.BatchResults(batchTestCtx(), keys, &schemas.BifrostBatchResultsRequest{BatchID: "batch_7f3a", Limit: 2})
	if bErr != nil {
		t.Fatalf("page 1: %+v", bErr.Error)
	}
	if calls := stub.recorded(); calls[0].Path != "/v1/batches/batch_7f3a/results" || calls[0].Query != "limit=2" {
		t.Fatalf("page 1 call = %+v", calls[0])
	}
	if !first.HasMore || first.NextCursor == nil || *first.NextCursor != "tok-2" || len(first.Results) != 2 {
		t.Fatalf("page 1 = has_more %v next %v results %d", first.HasMore, first.NextCursor, len(first.Results))
	}

	ok := first.Results[0]
	if ok.CustomID != "line-1" || ok.Status != schemas.BatchResultStatusSucceeded || ok.Error != nil || ok.Response == nil || ok.Response.StatusCode != 200 {
		t.Fatalf("line-1 = %+v", ok)
	}
	body := ok.Response.Body
	if _, has := body["extra_fields"]; has {
		t.Fatalf("response.body carries Bifrost's extra_fields: %v", body)
	}
	if choices, _ := body["choices"].([]any); len(choices) != 1 {
		t.Fatalf("response.body choices = %v", body["choices"])
	}
	usage, _ := body["usage"].(map[string]any)
	if usage["prompt_tokens"] != float64(32) {
		t.Fatalf("usage.prompt_tokens = %v, want 32", usage["prompt_tokens"])
	}
	// The sync route's normalization: reasoning tokens are part of completion_tokens.
	if usage["completion_tokens"] != float64(28) {
		t.Fatalf("usage.completion_tokens = %v, want 28 (1 visible + 27 reasoning)", usage["completion_tokens"])
	}
	// Contract C5: the money the vendor reported, in its own unit, is usage.vendor_cost
	// (cost_in_usd_ticks keeps its meaning beside it). Compared field by field: the decoded
	// map has no key order, so a string comparison of its encoding failed at random.
	vendorCost, _ := usage["vendor_cost"].(map[string]any)
	if vendorCost["amount"] != float64(123450) || vendorCost["unit"] != string(schemas.VendorCostUnitUSDTicks) {
		t.Fatalf("usage.vendor_cost = %v, want {amount: 123450, unit: usd_ticks} from cost_in_usd_ticks", usage["vendor_cost"])
	}

	failed := first.Results[1]
	if failed.CustomID != "line-2" || failed.Status != schemas.BatchResultStatusFailed || failed.Response != nil ||
		failed.Error == nil || failed.Error.Message != "Model output exceeded the context window" || !failed.Failed() {
		t.Fatalf("line-2 = %+v", failed)
	}

	second, bErr := provider.BatchResults(batchTestCtx(), keys, &schemas.BifrostBatchResultsRequest{BatchID: "batch_7f3a", After: first.NextCursor, Limit: 2})
	if bErr != nil {
		t.Fatalf("page 2: %+v", bErr.Error)
	}
	if calls := stub.recorded(); calls[1].Query != "limit=2&pagination_token=tok-2" {
		t.Fatalf("page 2 query = %q", calls[1].Query)
	}
	if second.HasMore || second.NextCursor != nil || len(second.Results) != 2 {
		t.Fatalf("page 2 = has_more %v next %v results %d", second.HasMore, second.NextCursor, len(second.Results))
	}
	if r := second.Results[0]; r.Status != schemas.BatchResultStatusFailed || r.Error == nil || r.Error.Code != "invalid_argument" || r.Error.Message != "messages must not be empty" {
		t.Fatalf("line-3 = %+v", r)
	}
	if r := second.Results[1]; r.Status != schemas.BatchResultStatusCancelled || r.Error == nil || r.Error.Message != "batch was cancelled" {
		t.Fatalf("line-4 = %+v", r)
	}

	encoded, err := sonic.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	wire := decodeJSON(t, string(encoded))
	if wire["has_more"] != true || wire["next_cursor"] != "tok-2" {
		t.Fatalf("wire paging = %v / %v", wire["has_more"], wire["next_cursor"])
	}
	if line := wire["results"].([]any)[1].(map[string]any); line["status"] != "failed" || line["custom_id"] != "line-2" {
		t.Fatalf("wire line = %v", line)
	}
}

// Metadata survives the trip through the batch name; a name that is not a query string
// carries none.
func TestXAIBatchNameCarriesMetadata(t *testing.T) {
	name, err := encodeXAIBatchName(nil, map[string]string{"tokun_batch_id": "batch_01JX", "team": "a b&c"})
	if err != nil {
		t.Fatalf("encode: %+v", err)
	}
	if got := decodeXAIBatchMetadata(name); got["tokun_batch_id"] != "batch_01JX" || got["team"] != "a b&c" || len(got) != 2 {
		t.Fatalf("round trip of %q = %v", name, got)
	}
	if name, _ := encodeXAIBatchName(schemas.Ptr("nightly eval"), nil); name != "nightly eval" {
		t.Fatalf("display name = %q", name)
	}
	if name, _ := encodeXAIBatchName(nil, nil); name != xaiDefaultBatchName {
		t.Fatalf("default name = %q", name)
	}
	for _, plain := range []string{"customer_feedback_analysis", "", "a=1&a=2", "=v"} {
		if got := decodeXAIBatchMetadata(plain); got != nil {
			t.Fatalf("decode(%q) = %v, want nil", plain, got)
		}
	}
}

// Times are read as RFC 3339 strings, Unix seconds or milliseconds, numeric strings or
// protobuf {seconds} objects.
func TestXAITimestampForms(t *testing.T) {
	want := time.Date(2026, 11, 10, 4, 0, 0, 0, time.UTC).Unix()
	for _, raw := range []string{`"2026-11-10T04:00:00Z"`, `1794283200`, `1794283200000`, `"1794283200"`, `{"seconds":1794283200}`, `{"seconds":"1794283200","nanos":5}`} {
		var ts xaiTimestamp
		if err := sonic.UnmarshalString(raw, &ts); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if ts.unix == nil || *ts.unix != want {
			t.Fatalf("%s = %v, want %d", raw, ts.unix, want)
		}
	}
	for _, raw := range []string{`null`, `""`, `"soon"`, `0`} {
		var ts xaiTimestamp
		if err := sonic.UnmarshalString(raw, &ts); err != nil || ts.unix != nil {
			t.Fatalf("%s: unix=%v err=%v, want unset", raw, ts.unix, err)
		}
	}
}

// Request-path cost of an inline batch: converting one chat line, and mapping one result
// line with usage. Run: go test ./providers/xai/ -run '^$' -bench 'XAIBatch' -benchmem
func BenchmarkXAIBatchLineConversion(b *testing.B) {
	ctx := batchTestCtx()
	items := []schemas.BatchRequestItem{{CustomID: "line-1", URL: "/v1/chat/completions", Body: map[string]any{
		"model": "grok-4.3", "temperature": 0.2, "max_tokens": 64,
		"messages": []any{map[string]any{"role": "system", "content": "Be terse."}, map[string]any{"role": "user", "content": "Classify: late delivery"}},
	}}}
	b.ReportAllocs()
	for b.Loop() {
		if _, bErr := ToXAIBatchRequests(ctx, schemas.BatchEndpointChatCompletions, "grok-4.3", nil, items); bErr != nil {
			b.Fatal(bErr.Error.Message)
		}
	}
}

func BenchmarkXAIBatchResultItem(b *testing.B) {
	var result XAIBatchResult
	raw := `{"batch_request_id":"line-1","batch_result":{"response":{"chat_get_completion":{
		"id":"chatcmpl-1","object":"chat.completion","created":1794283200,"model":"grok-4.3",
		"choices":[{"index":0,"message":{"role":"assistant","content":"positive"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":32,"completion_tokens":1,"total_tokens":60,
			"prompt_tokens_details":{"cached_tokens":16},"completion_tokens_details":{"reasoning_tokens":27},
			"cost_in_usd_ticks":123450}}}}}`
	if err := sonic.UnmarshalString(raw, &result); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if item := result.ToBifrostBatchResultItem(); item.Response == nil {
			b.Fatal("no response")
		}
	}
}
