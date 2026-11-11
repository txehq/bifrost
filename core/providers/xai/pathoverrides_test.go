package xai

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/capsohq/bifrost/core/schemas"
)

// newOverrideTestProvider is an xAI-based custom provider whose request_path_overrides move
// every batch and file call, as a vendor serving xAI's API under another prefix needs.
func newOverrideTestProvider(baseURL string, overrides map[schemas.RequestType]string) *XAIProvider {
	provider := newBatchTestProvider(baseURL)
	provider.customProviderConfig = &schemas.CustomProviderConfig{
		CustomProviderKey: "xai-compatible", BaseProviderType: schemas.XAI, RequestPathOverrides: overrides,
	}
	return provider
}

// Every xAI batch and file call on an xAI-based custom provider reaches the path its
// request_path_overrides entry names, with the id in place of the placeholder. Both calls
// of an inline create follow the batch_create entry: the second adds /{batch_id}/requests
// to its path and keeps its query. Before TXE-7084 every one of these calls went to xAI's
// own /v1 path.
func TestCustomProviderOverridesReachEveryXAIBatchAndFilePath(t *testing.T) {
	stub := &batchStub{}
	stub.handler = func(w http.ResponseWriter, _ *http.Request, _ string) {
		_, _ = io.WriteString(w, `{"batch_id":"batch_9","name":"bifrost-batch","state":{}}`)
	}
	provider := newOverrideTestProvider(stub.serve(t).URL, map[schemas.RequestType]string{
		schemas.BatchCreateRequest:   "/api/v2/batches?team=t1",
		schemas.BatchListRequest:     "/api/v2/batches",
		schemas.BatchRetrieveRequest: "/api/v2/batches/{batch_id}/view",
		schemas.BatchCancelRequest:   "/api/v2/batches/{batch_id}/stop",
		schemas.BatchResultsRequest:  "/api/v2/batches/{batch_id}/lines",
		schemas.FileUploadRequest:    "/api/v2/files",
		schemas.FileListRequest:      "/api/v2/files?scope=team",
		schemas.FileRetrieveRequest:  "/api/v2/files/{file_id}/meta",
		schemas.FileDeleteRequest:    "/api/v2/files/{file_id}/rm",
		schemas.FileContentRequest:   "/api/v2/files/{file_id}/raw",
	})
	ctx := batchTestCtx()
	key := batchKey("k", "sk")
	keys := []schemas.Key{key}
	chatLine := schemas.BatchRequestItem{CustomID: "a", URL: "/v1/chat/completions", Body: map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}}

	_, _ = provider.BatchCreate(ctx, key, &schemas.BifrostBatchCreateRequest{Model: schemas.Ptr("grok-4.3"), Requests: []schemas.BatchRequestItem{chatLine}})
	_, _ = provider.BatchList(ctx, keys, &schemas.BifrostBatchListRequest{Limit: 5})
	_, _ = provider.BatchRetrieve(ctx, keys, &schemas.BifrostBatchRetrieveRequest{BatchID: "batch_9"})
	_, _ = provider.BatchCancel(ctx, keys, &schemas.BifrostBatchCancelRequest{BatchID: "batch_9"})
	_, _ = provider.BatchResults(ctx, keys, &schemas.BifrostBatchResultsRequest{BatchID: "batch_9", Limit: 2})
	_, _ = provider.FileUpload(ctx, key, &schemas.BifrostFileUploadRequest{File: []byte("{}\n"), Filename: "in.jsonl", Purpose: schemas.FilePurposeBatch})
	_, _ = provider.FileList(ctx, keys, &schemas.BifrostFileListRequest{Limit: 3})
	_, _ = provider.FileRetrieve(ctx, keys, &schemas.BifrostFileRetrieveRequest{FileID: "file_1"})
	_, _ = provider.FileDelete(ctx, keys, &schemas.BifrostFileDeleteRequest{FileID: "file_1"})
	_, _ = provider.FileContent(ctx, keys, &schemas.BifrostFileContentRequest{FileID: "file_1"})

	want := []string{
		"POST /api/v2/batches?team=t1",
		"POST /api/v2/batches/batch_9/requests?team=t1",
		"GET /api/v2/batches?limit=5",
		"GET /api/v2/batches/batch_9/view",
		"POST /api/v2/batches/batch_9/stop",
		"GET /api/v2/batches/batch_9/lines?limit=2",
		"POST /api/v2/files",
		"GET /api/v2/files?scope=team&limit=3",
		"GET /api/v2/files/file_1/meta",
		"DELETE /api/v2/files/file_1/rm",
		"GET /api/v2/files/file_1/raw",
	}
	got := []string{}
	for _, r := range stub.recorded() {
		line := r.Method + " " + r.Path
		if r.Query != "" {
			line += "?" + r.Query
		}
		got = append(got, line)
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// An override for a call that names a resource but carries no placeholder would send the
// call to a path that lost the id. It is a configuration error and nothing is sent.
func TestXAIOverrideWithoutPlaceholderSendsNothing(t *testing.T) {
	stub := &batchStub{}
	stub.handler = func(w http.ResponseWriter, _ *http.Request, _ string) { _, _ = io.WriteString(w, `{}`) }
	provider := newOverrideTestProvider(stub.serve(t).URL, map[schemas.RequestType]string{
		schemas.BatchRetrieveRequest: "/api/v2/batches",
		schemas.FileContentRequest:   "/api/v2/files/content",
	})
	keys := []schemas.Key{batchKey("k", "sk")}
	_, retrieveErr := provider.BatchRetrieve(batchTestCtx(), keys, &schemas.BifrostBatchRetrieveRequest{BatchID: "batch_9"})
	_, contentErr := provider.FileContent(batchTestCtx(), keys, &schemas.BifrostFileContentRequest{FileID: "file_1"})
	for name, bErr := range map[string]*schemas.BifrostError{"batch_retrieve": retrieveErr, "file_content": contentErr} {
		if bErr == nil || bErr.Error == nil || !strings.Contains(bErr.Error.Message, "request_path_overrides."+name) {
			t.Fatalf("%s: error = %+v, want the configuration error naming the override", name, bErr)
		}
	}
	if n := len(stub.recorded()); n != 0 {
		t.Fatalf("upstream calls = %d, want 0", n)
	}
}

// A full-URL override moves the call to another host; both calls of the create follow it.
func TestBatchRequestsURLFollowsTheCreateURL(t *testing.T) {
	cases := map[string]string{
		"https://api.example.com/v1/batches":          "https://api.example.com/v1/batches/b1/requests",
		"https://api.example.com/v1/batches/":         "https://api.example.com/v1/batches/b1/requests",
		"https://api.example.com/v1/batches?team=t#x": "https://api.example.com/v1/batches/b1/requests?team=t#x",
		"http://127.0.0.1:9/pp/xai/v1/batches":        "http://127.0.0.1:9/pp/xai/v1/batches/b1/requests",
	}
	for createURL, want := range cases {
		if got := batchRequestsURL(createURL, "b1"); got != want {
			t.Errorf("batchRequestsURL(%q) = %q, want %q", createURL, got, want)
		}
	}
}

// BenchmarkXAIResourceURL is the cost of building a batch or file URL: the built-in provider
// (no overrides, one concatenation) and an xai-based custom provider (template substitution).
func BenchmarkXAIResourceURL(b *testing.B) {
	builtIn := newBatchTestProvider("https://api.x.ai")
	custom := newOverrideTestProvider("https://vendor.example.com", map[schemas.RequestType]string{
		schemas.BatchRetrieveRequest: "/api/v2/batches/{batch_id}",
	})
	b.Run("built-in", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := builtIn.resourceURL("/v1/batches/", "batch_9", "", schemas.BatchRetrieveRequest); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("override", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := custom.resourceURL("/v1/batches/", "batch_9", "", schemas.BatchRetrieveRequest); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("create second call", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_ = batchRequestsURL("https://vendor.example.com/api/v2/batches?team=t1", "batch_9")
		}
	})
}
