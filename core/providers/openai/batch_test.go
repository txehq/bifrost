package openai

import (
	"context"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bytedance/sonic"
	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Lines of an OpenAI batch output file and error file, in the shape the Batch API documents:
// the output file holds the answered lines, the error file the lines that failed with a
// request error or that the batch never reached before its window ended.
const (
	batchOutputLineChat   = `{"id": "batch_req_out1", "custom_id": "line-1", "response": {"status_code": 200, "request_id": "req_out1", "body": {"id": "chatcmpl-1", "object": "chat.completion", "created": 1711652795, "model": "gpt-4o-mini-2024-07-18", "choices": [{"index": 0, "message": {"role": "assistant", "content": "Hello."}, "logprobs": null, "finish_reason": "stop"}], "usage": {"prompt_tokens": 2048, "completion_tokens": 12, "total_tokens": 2060, "prompt_tokens_details": {"cached_tokens": 1024, "audio_tokens": 0}, "completion_tokens_details": {"reasoning_tokens": 4, "audio_tokens": 0, "accepted_prediction_tokens": 0, "rejected_prediction_tokens": 0}}, "system_fingerprint": "fp_123"}}, "error": null}`
	batchOutputLineChat2  = `{"id": "batch_req_out2", "custom_id": "line-2", "response": {"status_code": 200, "request_id": "req_out2", "body": {"id": "chatcmpl-2", "object": "chat.completion", "created": 1711652796, "model": "gpt-4o-mini-2024-07-18", "choices": [{"index": 0, "message": {"role": "assistant", "content": "Hi."}, "logprobs": null, "finish_reason": "stop"}], "usage": {"prompt_tokens": 20, "completion_tokens": 2, "total_tokens": 22}, "system_fingerprint": "fp_123"}}, "error": null}`
	batchErrorLineRequest = `{"id": "batch_req_err1", "custom_id": "line-3", "response": {"status_code": 400, "request_id": "req_err1", "body": {"error": {"message": "The model ` + "`gpt-nope`" + ` does not exist or you do not have access to it.", "type": "invalid_request_error", "param": null, "code": "model_not_found"}}}, "error": null}`
	batchErrorLineExpired = `{"id": "batch_req_err2", "custom_id": "line-4", "response": null, "error": {"code": "batch_expired", "message": "This request could not be executed before the completion window expired."}}`
)

func TestParseBatchResultLine(t *testing.T) {
	t.Run("succeeded chat line keeps the vendor id and carries the unified usage", func(t *testing.T) {
		item, err := ParseBatchResultLine([]byte(batchOutputLineChat), schemas.BatchEndpointChatCompletions)
		require.NoError(t, err)
		assert.Equal(t, "batch_req_out1", item.ID)
		assert.Equal(t, "line-1", item.CustomID)
		assert.Equal(t, schemas.BatchResultStatusSucceeded, item.Status)
		assert.Nil(t, item.Error)
		assert.False(t, item.Failed())
		require.NotNil(t, item.Response)
		assert.Equal(t, 200, item.Response.StatusCode)
		assert.Equal(t, "req_out1", item.Response.RequestID)
		assert.Equal(t, "chat.completion", item.Response.Body["object"])
		assert.Equal(t, "fp_123", item.Response.Body["system_fingerprint"], "the body outside usage is the vendor's, unchanged")

		// The usage is what the sync route returns for the same response: the vendor body
		// decoded into the unified chat usage.
		var vendor struct {
			Response struct {
				Body struct {
					Usage schemas.BifrostLLMUsage `json:"usage"`
				} `json:"body"`
			} `json:"response"`
		}
		require.NoError(t, sonic.Unmarshal([]byte(batchOutputLineChat), &vendor))
		wantUsage, err := sonic.Marshal(vendor.Response.Body.Usage)
		require.NoError(t, err)
		gotUsage, err := sonic.Marshal(item.Response.Body["usage"])
		require.NoError(t, err)
		assert.JSONEq(t, string(wantUsage), string(gotUsage))

		usage := item.Response.Body["usage"].(map[string]any)
		promptDetails := usage["prompt_tokens_details"].(map[string]any)
		assert.EqualValues(t, 1024, promptDetails["cached_read_tokens"], "OpenAI's cached_tokens is the unified cached_read_tokens")
		assert.EqualValues(t, 1024, promptDetails["cached_tokens"], "and stays readable under OpenAI's own name")
		assert.EqualValues(t, 4, usage["completion_tokens_details"].(map[string]any)["reasoning_tokens"])
		assert.EqualValues(t, 2048, usage["prompt_tokens"])
		assert.EqualValues(t, 12, usage["completion_tokens"])
		assert.EqualValues(t, 2060, usage["total_tokens"])
	})

	t.Run("a request error is failed and carries the vendor's code and message", func(t *testing.T) {
		item, err := ParseBatchResultLine([]byte(batchErrorLineRequest), schemas.BatchEndpointChatCompletions)
		require.NoError(t, err)
		assert.Equal(t, "batch_req_err1", item.ID)
		assert.Equal(t, "line-3", item.CustomID)
		assert.Equal(t, schemas.BatchResultStatusFailed, item.Status)
		assert.True(t, item.Failed())
		require.NotNil(t, item.Error)
		assert.Equal(t, "model_not_found", item.Error.Code)
		assert.Contains(t, item.Error.Message, "gpt-nope")
		require.NotNil(t, item.Response, "the vendor's error response stays on the line")
		assert.Equal(t, 400, item.Response.StatusCode)
	})

	t.Run("a request error without a code falls back to the error type", func(t *testing.T) {
		line := `{"id":"batch_req_e","custom_id":"line-9","response":{"status_code":429,"request_id":"r","body":{"error":{"message":"Rate limit reached","type":"rate_limit_error","code":null}}},"error":null}`
		item, err := ParseBatchResultLine([]byte(line), schemas.BatchEndpointChatCompletions)
		require.NoError(t, err)
		assert.Equal(t, schemas.BatchResultStatusFailed, item.Status)
		require.NotNil(t, item.Error)
		assert.Equal(t, "rate_limit_error", item.Error.Code)
		assert.Equal(t, "Rate limit reached", item.Error.Message)
	})

	t.Run("a line the batch never reached is expired", func(t *testing.T) {
		item, err := ParseBatchResultLine([]byte(batchErrorLineExpired), schemas.BatchEndpointChatCompletions)
		require.NoError(t, err)
		assert.Equal(t, "batch_req_err2", item.ID)
		assert.Equal(t, schemas.BatchResultStatusExpired, item.Status)
		assert.True(t, item.Failed())
		assert.Nil(t, item.Response)
		require.NotNil(t, item.Error)
		assert.Equal(t, "batch_expired", item.Error.Code)
		assert.Contains(t, item.Error.Message, "completion window")
	})

	t.Run("a line with neither response nor error is failed", func(t *testing.T) {
		item, err := ParseBatchResultLine([]byte(`{"id":"batch_req_x","custom_id":"line-x","response":null,"error":null}`), schemas.BatchEndpointChatCompletions)
		require.NoError(t, err)
		assert.Equal(t, schemas.BatchResultStatusFailed, item.Status)
		assert.True(t, item.Failed())
	})

	t.Run("embeddings and image lines carry their operation's unified usage", func(t *testing.T) {
		embedding := `{"id":"batch_req_em","custom_id":"e-1","response":{"status_code":200,"request_id":"r","body":{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1]}],"model":"text-embedding-3-small","usage":{"prompt_tokens":8,"total_tokens":8}}},"error":null}`
		item, err := ParseBatchResultLine([]byte(embedding), schemas.BatchEndpointEmbeddings)
		require.NoError(t, err)
		assert.Equal(t, schemas.BatchResultStatusSucceeded, item.Status)
		usage := item.Response.Body["usage"].(map[string]any)
		assert.EqualValues(t, 8, usage["prompt_tokens"])
		assert.EqualValues(t, 8, usage["total_tokens"])

		image := `{"id":"batch_req_im","custom_id":"i-1","response":{"status_code":200,"request_id":"r","body":{"created":1,"data":[{"b64_json":"aGk="}],"usage":{"input_tokens":50,"output_tokens":4160,"total_tokens":4210,"input_tokens_details":{"text_tokens":50,"image_tokens":0},"output_tokens_details":{"image_tokens":4160,"text_tokens":0}}}},"error":null}`
		item, err = ParseBatchResultLine([]byte(image), schemas.BatchEndpointImageGeneration)
		require.NoError(t, err)
		usage = item.Response.Body["usage"].(map[string]any)
		assert.EqualValues(t, 50, usage["input_tokens"])
		assert.EqualValues(t, 4160, usage["output_tokens"])
		assert.EqualValues(t, 4160, usage["output_tokens_details"].(map[string]any)["image_tokens"])
		assert.Equal(t, "aGk=", item.Response.Body["data"].([]any)[0].(map[string]any)["b64_json"], "the image artifact stays on the line")
	})

	t.Run("a malformed line is an error for the caller to count", func(t *testing.T) {
		_, err := ParseBatchResultLine([]byte(`{"id":`), schemas.BatchEndpointChatCompletions)
		require.Error(t, err)
	})
}

// batchVendor is a stand-in for the OpenAI files and batches API. It serves one batch whose
// output and error files are given, and records every upstream call.
type batchVendor struct {
	*httptest.Server
	mu          sync.Mutex
	calls       []string // "METHOD path" in arrival order
	auth        map[string]int
	uploads     []string            // uploaded file bodies
	uploadForms []map[string]string // the other form fields of each upload
	creates     []string            // batch create bodies
	prefix      string              // the API root the vendor answers on, "/v1" for OpenAI
	batch       string              // the batch object the retrieve returns
	files       map[string]string   // file id -> content
	bytesServed atomic.Int64
}

func newBatchVendor(t *testing.T, batch string, files map[string]string) *batchVendor {
	t.Helper()
	return newBatchVendorAt(t, "/v1", batch, files)
}

// newBatchVendorAt is newBatchVendor for a vendor whose files and batches API is rooted at
// prefix. A call to any other path fails the test.
func newBatchVendorAt(t *testing.T, prefix string, batch string, files map[string]string) *batchVendor {
	t.Helper()
	v := &batchVendor{auth: map[string]int{}, prefix: prefix, batch: batch, files: files}
	v.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v.mu.Lock()
		v.calls = append(v.calls, r.Method+" "+r.URL.Path)
		v.auth[r.Header.Get("Authorization")]++
		v.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == prefix+"/files":
			_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil {
				t.Errorf("upload content type: %v", err)
			}
			reader := multipart.NewReader(r.Body, params["boundary"])
			fields := map[string]string{}
			for {
				part, err := reader.NextPart()
				if err != nil {
					break
				}
				content, _ := io.ReadAll(part)
				if part.FormName() == "file" {
					v.mu.Lock()
					v.uploads = append(v.uploads, string(content))
					v.mu.Unlock()
				} else {
					fields[part.FormName()] = string(content)
				}
			}
			v.mu.Lock()
			v.uploadForms = append(v.uploadForms, fields)
			v.mu.Unlock()
			fmt.Fprint(w, `{"id":"file-in","object":"file","bytes":1,"created_at":1,"filename":"batch_requests.jsonl","purpose":"batch"}`)
		case r.Method == http.MethodPost && r.URL.Path == prefix+"/batches":
			body, _ := io.ReadAll(r.Body)
			v.mu.Lock()
			v.creates = append(v.creates, string(body))
			v.mu.Unlock()
			fmt.Fprint(w, `{"id":"batch_1","object":"batch","endpoint":"/v1/chat/completions","input_file_id":"file-in","completion_window":"24h","status":"validating","created_at":1711471533,"expires_at":1711557933,"request_counts":{"total":0,"completed":0,"failed":0},"metadata":{"tokun_batch_id":"batch_tk_1"}}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, prefix+"/batches/"):
			fmt.Fprint(w, v.batch)
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, prefix+"/batches/") && strings.HasSuffix(r.URL.Path, "/cancel"):
			fmt.Fprint(w, `{"id":"batch_1","object":"batch","status":"cancelling","cancelling_at":1711475133,"request_counts":{"total":2,"completed":1,"failed":0}}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, prefix+"/files/") && !strings.HasSuffix(r.URL.Path, "/content"):
			fmt.Fprint(w, `{"id":"file-out","object":"file","bytes":120,"created_at":1711471533,"filename":"output.jsonl","purpose":"batch_output"}`)
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, prefix+"/files/"):
			fmt.Fprint(w, `{"id":"file-out","object":"file","deleted":true}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, prefix+"/files/") && strings.HasSuffix(r.URL.Path, "/content"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, prefix+"/files/"), "/content")
			content, ok := v.files[id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, `{"error":{"message":"No such File object: `+id+`","type":"invalid_request_error","code":null}}`)
				return
			}
			start := 0
			if rangeHeader := r.Header.Get("Range"); rangeHeader != "" {
				parsed, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(rangeHeader, "bytes="), "-"))
				if err != nil {
					t.Errorf("unparseable Range %q", rangeHeader)
				}
				if parsed >= len(content) {
					w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
					return
				}
				start = parsed
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(content)-1, len(content)))
				w.WriteHeader(http.StatusPartialContent)
			}
			// Written in small pieces so a client that stops reading stops the transfer.
			for rest := content[start:]; len(rest) > 0; {
				n := min(len(rest), 32*1024)
				written, err := io.WriteString(w, rest[:n])
				v.bytesServed.Add(int64(written))
				if err != nil {
					return
				}
				rest = rest[n:]
			}
		default:
			t.Errorf("unexpected upstream call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(v.Close)
	return v
}

func (v *batchVendor) snapshotCalls() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]string(nil), v.calls...)
}

func (v *batchVendor) resetCalls() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.calls = nil
}

func newBatchTestProvider(v *batchVendor) (*OpenAIProvider, *schemas.BifrostContext, []schemas.Key) {
	provider := NewOpenAIProvider(&schemas.ProviderConfig{NetworkConfig: schemas.NetworkConfig{BaseURL: v.URL, DefaultRequestTimeoutInSeconds: 30}}, testNoopLogger{})
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	keys := []schemas.Key{{ID: "key-1", Name: "tk-key-1", Value: *schemas.NewSecretVar("sk-test")}}
	return provider, ctx, keys
}

const completedBatch = `{"id":"batch_1","object":"batch","endpoint":"/v1/chat/completions","input_file_id":"file-in","completion_window":"24h","status":"completed","output_file_id":"file-out","error_file_id":"file-err","created_at":1711471533,"request_counts":{"total":4,"completed":2,"failed":2}}`

// Results merge the error file into the output file's lines: every line the vendor wrote
// comes back once, with its status, the vendor's line id and the unified usage.
func TestBatchResultsMergesTheErrorFile(t *testing.T) {
	vendor := newBatchVendor(t, completedBatch, map[string]string{
		"file-out": batchOutputLineChat + "\n" + batchOutputLineChat2 + "\n",
		"file-err": batchErrorLineRequest + "\n" + batchErrorLineExpired + "\n",
	})
	provider, ctx, keys := newBatchTestProvider(vendor)

	resp, bifrostErr := provider.BatchResults(ctx, keys, &schemas.BifrostBatchResultsRequest{Provider: schemas.OpenAI, BatchID: "batch_1"})
	require.Nil(t, bifrostErr)
	assert.Equal(t, "batch_1", resp.BatchID)
	assert.Equal(t, schemas.BatchEndpointChatCompletions, resp.Endpoint)
	assert.False(t, resp.HasMore)
	assert.Nil(t, resp.NextCursor)

	type line struct {
		id, customID string
		status       schemas.BatchResultStatus
	}
	var got []line
	for _, item := range resp.Results {
		got = append(got, line{item.ID, item.CustomID, item.Status})
	}
	assert.Equal(t, []line{
		{"batch_req_out1", "line-1", schemas.BatchResultStatusSucceeded},
		{"batch_req_out2", "line-2", schemas.BatchResultStatusSucceeded},
		{"batch_req_err1", "line-3", schemas.BatchResultStatusFailed},
		{"batch_req_err2", "line-4", schemas.BatchResultStatusExpired},
	}, got)
	assert.Equal(t, "model_not_found", resp.Results[2].Error.Code)
	assert.Equal(t, "batch_expired", resp.Results[3].Error.Code)
	assert.EqualValues(t, 1024, resp.Results[0].Response.Body["usage"].(map[string]any)["prompt_tokens_details"].(map[string]any)["cached_read_tokens"])
	assert.Equal(t, schemas.BatchRequestCounts{Total: 4, Completed: 2, Failed: 2}, schemas.BatchRequestCountsFromResults(resp.Results))

	// One retrieve for the file ids, then one download per file.
	assert.Equal(t, []string{"GET /v1/batches/batch_1", "GET /v1/files/file-out/content", "GET /v1/files/file-err/content"}, vendor.snapshotCalls())

	// The JSON a caller receives is the contract's line shape.
	encoded, err := sonic.Marshal(resp.Results[3])
	require.NoError(t, err)
	assert.JSONEq(t, `{"id":"batch_req_err2","custom_id":"line-4","status":"expired","error":{"code":"batch_expired","message":"This request could not be executed before the completion window expired."}}`, string(encoded))
}

func TestBatchResultsWithOnlyOneFile(t *testing.T) {
	t.Run("every line failed: only an error file", func(t *testing.T) {
		batch := `{"id":"batch_1","object":"batch","endpoint":"/v1/chat/completions","status":"completed","error_file_id":"file-err"}`
		vendor := newBatchVendor(t, batch, map[string]string{"file-err": batchErrorLineRequest + "\n" + batchErrorLineExpired + "\n"})
		provider, ctx, keys := newBatchTestProvider(vendor)
		resp, bifrostErr := provider.BatchResults(ctx, keys, &schemas.BifrostBatchResultsRequest{Provider: schemas.OpenAI, BatchID: "batch_1"})
		require.Nil(t, bifrostErr)
		require.Len(t, resp.Results, 2)
		assert.Equal(t, schemas.BatchResultStatusFailed, resp.Results[0].Status)
		assert.Equal(t, schemas.BatchResultStatusExpired, resp.Results[1].Status)
		assert.Equal(t, []string{"GET /v1/batches/batch_1", "GET /v1/files/file-err/content"}, vendor.snapshotCalls())
	})
	t.Run("every line answered: only an output file", func(t *testing.T) {
		batch := `{"id":"batch_1","object":"batch","endpoint":"/v1/chat/completions","status":"completed","output_file_id":"file-out"}`
		vendor := newBatchVendor(t, batch, map[string]string{"file-out": batchOutputLineChat + "\n"})
		provider, ctx, keys := newBatchTestProvider(vendor)
		resp, bifrostErr := provider.BatchResults(ctx, keys, &schemas.BifrostBatchResultsRequest{Provider: schemas.OpenAI, BatchID: "batch_1", Limit: 10})
		require.Nil(t, bifrostErr)
		require.Len(t, resp.Results, 1)
		assert.False(t, resp.HasMore)
		assert.Equal(t, []string{"GET /v1/batches/batch_1", "GET /v1/files/file-out/content"}, vendor.snapshotCalls())
	})
	t.Run("no file yet is an error, not an empty result", func(t *testing.T) {
		vendor := newBatchVendor(t, `{"id":"batch_1","object":"batch","endpoint":"/v1/chat/completions","status":"in_progress"}`, nil)
		provider, ctx, keys := newBatchTestProvider(vendor)
		_, bifrostErr := provider.BatchResults(ctx, keys, &schemas.BifrostBatchResultsRequest{Provider: schemas.OpenAI, BatchID: "batch_1"})
		require.NotNil(t, bifrostErr)
		assert.Contains(t, bifrostErr.Error.Message, "batch results not available")
	})
	t.Run("a file the vendor refuses keeps the vendor's status", func(t *testing.T) {
		vendor := newBatchVendor(t, completedBatch, map[string]string{"file-out": batchOutputLineChat + "\n"})
		provider, ctx, keys := newBatchTestProvider(vendor)
		_, bifrostErr := provider.BatchResults(ctx, keys, &schemas.BifrostBatchResultsRequest{Provider: schemas.OpenAI, BatchID: "batch_1"})
		require.NotNil(t, bifrostErr)
		require.NotNil(t, bifrostErr.StatusCode)
		assert.Equal(t, http.StatusNotFound, *bifrostErr.StatusCode)
		assert.Contains(t, bifrostErr.Error.Message, "file-err")
	})
	t.Run("a malformed line is counted in parse_errors and the rest still returned", func(t *testing.T) {
		batch := `{"id":"batch_1","object":"batch","endpoint":"/v1/chat/completions","status":"completed","output_file_id":"file-out"}`
		vendor := newBatchVendor(t, batch, map[string]string{"file-out": batchOutputLineChat + "\n{\"id\": \n" + batchOutputLineChat2 + "\n"})
		provider, ctx, keys := newBatchTestProvider(vendor)
		resp, bifrostErr := provider.BatchResults(ctx, keys, &schemas.BifrostBatchResultsRequest{Provider: schemas.OpenAI, BatchID: "batch_1"})
		require.Nil(t, bifrostErr)
		require.Len(t, resp.Results, 2)
		require.Len(t, resp.ExtraFields.ParseErrors, 1)
		assert.Equal(t, "parse_error", resp.ExtraFields.ParseErrors[0].Code)
	})
}

// Paging walks the output file and then the error file by cursor, returning every line once
// whatever the page size, including a page that ends exactly where the output file does.
func TestBatchResultsPagesAcrossBothFiles(t *testing.T) {
	var output, errorFile strings.Builder
	var want []string
	for i := range 7 {
		id := fmt.Sprintf("ok-%d", i)
		want = append(want, id+":succeeded")
		output.WriteString(strings.Replace(batchOutputLineChat2, `"line-2"`, `"`+id+`"`, 1) + "\n")
	}
	for i := range 3 {
		id := fmt.Sprintf("bad-%d", i)
		want = append(want, id+":expired")
		errorFile.WriteString(strings.Replace(batchErrorLineExpired, `"line-4"`, `"`+id+`"`, 1) + "\n")
	}
	vendor := newBatchVendor(t, completedBatch, map[string]string{"file-out": output.String(), "file-err": errorFile.String()})
	provider, ctx, keys := newBatchTestProvider(vendor)

	for _, limit := range []int{1, 3, 7, 9, 10, 50} {
		t.Run(fmt.Sprintf("limit %d", limit), func(t *testing.T) {
			var got []string
			var after *string
			for page := 0; ; page++ {
				require.Less(t, page, 20, "paging did not terminate")
				resp, bifrostErr := provider.BatchResults(ctx, keys, &schemas.BifrostBatchResultsRequest{Provider: schemas.OpenAI, BatchID: "batch_1", Limit: limit, After: after})
				require.Nil(t, bifrostErr)
				require.LessOrEqual(t, len(resp.Results), limit)
				for _, item := range resp.Results {
					got = append(got, item.CustomID+":"+string(item.Status))
				}
				require.Equal(t, resp.HasMore, resp.NextCursor != nil)
				if !resp.HasMore {
					break
				}
				after = resp.NextCursor
			}
			assert.Equal(t, want, got)
		})
	}

	t.Run("a cursor this gateway did not issue is a 400", func(t *testing.T) {
		_, bifrostErr := provider.BatchResults(ctx, keys, &schemas.BifrostBatchResultsRequest{Provider: schemas.OpenAI, BatchID: "batch_1", Limit: 2, After: schemas.Ptr("batch_req_out1")})
		require.NotNil(t, bifrostErr)
		require.NotNil(t, bifrostErr.StatusCode)
		assert.Equal(t, http.StatusBadRequest, *bifrostErr.StatusCode)
	})
}

// A page of a large output file is read from a stream: the vendor sends about one page of a
// 64 MB file, not the file, on the first page and on a page deep inside it.
func TestBatchResultsPageOfALargeFileIsNotReadWhole(t *testing.T) {
	const lineCount = 64_000
	line := strings.Replace(batchOutputLineChat2, `"content": "Hi."`, `"content": "`+strings.Repeat("x", 1024-len(batchOutputLineChat2)-1+len(`Hi.`))+`"`, 1) + "\n"
	require.Len(t, line, 1024)
	vendor := newBatchVendor(t, completedBatch, map[string]string{"file-out": strings.Repeat(line, lineCount), "file-err": batchErrorLineExpired + "\n"})
	provider, ctx, keys := newBatchTestProvider(vendor)
	const fileSize = lineCount * 1024

	first, bifrostErr := provider.BatchResults(ctx, keys, &schemas.BifrostBatchResultsRequest{Provider: schemas.OpenAI, BatchID: "batch_1", Limit: 100})
	require.Nil(t, bifrostErr)
	require.Len(t, first.Results, 100)
	require.True(t, first.HasMore)
	served := vendor.bytesServed.Load()
	assert.Less(t, served, int64(fileSize/4), "the first page pulled %d of the file's %d bytes", served, fileSize)

	second, bifrostErr := provider.BatchResults(ctx, keys, &schemas.BifrostBatchResultsRequest{Provider: schemas.OpenAI, BatchID: "batch_1", Limit: 100, After: first.NextCursor})
	require.Nil(t, bifrostErr)
	require.Len(t, second.Results, 100)
	served = vendor.bytesServed.Load() - served
	assert.Less(t, served, int64(fileSize/4), "the second page pulled %d of the file's %d bytes", served, fileSize)
	assert.Equal(t, schemas.BatchResultStatusSucceeded, second.Results[99].Status)
}

// Inline requests are one file upload plus one batch create (contract C3 allows the two
// calls for one create). The JSONL holds the caller's lines unchanged, and metadata and
// output_expires_after reach the vendor's batch create.
func TestBatchCreateInlineUploadsTheLinesUnchanged(t *testing.T) {
	vendor := newBatchVendor(t, completedBatch, nil)
	provider, ctx, keys := newBatchTestProvider(vendor)

	lines := []string{
		`{"custom_id":"line-1","method":"POST","url":"/v1/chat/completions","body":{"model":"gpt-4o-mini","messages":[{"role":"system","content":"Be brief."},{"role":"user","content":"Hello"}],"max_completion_tokens":64,"temperature":0.2,"response_format":{"type":"json_object"},"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}}],"service_tier":"flex"}}`,
		`{"custom_id":"line-2","method":"POST","url":"/v1/chat/completions","body":{"model":"gpt-4o-mini","messages":[{"role":"user","content":"Bye"}],"seed":7}}`,
	}
	var requests []schemas.BatchRequestItem
	for _, line := range lines {
		var item schemas.BatchRequestItem
		require.NoError(t, sonic.UnmarshalString(line, &item))
		requests = append(requests, item)
	}

	resp, bifrostErr := provider.BatchCreate(ctx, keys[0], &schemas.BifrostBatchCreateRequest{
		Provider:           schemas.OpenAI,
		Model:              schemas.Ptr("gpt-4o-mini"),
		Endpoint:           schemas.BatchEndpointChatCompletions,
		CompletionWindow:   "24h",
		Metadata:           map[string]string{"tokun_batch_id": "batch_tk_1"},
		OutputExpiresAfter: &schemas.BatchExpiresAfter{Anchor: "created_at", Seconds: 86400},
		Requests:           requests,
	})
	require.Nil(t, bifrostErr)
	assert.Equal(t, "batch_1", resp.ID, "the batch id is the vendor's, unchanged")
	assert.Equal(t, schemas.BatchStatusValidating, resp.Status)
	assert.Equal(t, "batch_tk_1", resp.Metadata["tokun_batch_id"])
	require.NotNil(t, resp.ExpiresAt)

	assert.Equal(t, []string{"POST /v1/files", "POST /v1/batches"}, vendor.snapshotCalls(), "one upload, then one batch create")

	require.Len(t, vendor.uploads, 1)
	uploaded := strings.Split(strings.TrimSpace(vendor.uploads[0]), "\n")
	require.Len(t, uploaded, len(lines))
	for i, line := range lines {
		assert.JSONEq(t, line, uploaded[i], "line %d reaches the vendor as the caller sent it", i+1)
	}
	assert.Equal(t, "batch", vendor.uploadForms[0]["purpose"])

	require.Len(t, vendor.creates, 1)
	assert.JSONEq(t, `{"input_file_id":"file-in","endpoint":"/v1/chat/completions","completion_window":"24h","metadata":{"tokun_batch_id":"batch_tk_1"},"output_expires_after":{"anchor":"created_at","seconds":86400}}`, vendor.creates[0])
}

// Zhipu bigmodel (the z.ai domestic site) serves OpenAI-shaped files and batches under
// /api/paas/v4. Tokun reaches it through a custom provider on the openai base whose
// request_path_overrides name those paths; for a call that names a batch or a file, the
// override is a template with {batch_id} or {file_id} where the id goes.
var bigmodelPathOverrides = map[schemas.RequestType]string{
	schemas.BatchCreateRequest:   "/api/paas/v4/batches",
	schemas.BatchListRequest:     "/api/paas/v4/batches",
	schemas.BatchRetrieveRequest: "/api/paas/v4/batches/{batch_id}",
	schemas.BatchCancelRequest:   "/api/paas/v4/batches/{batch_id}/cancel",
	schemas.BatchResultsRequest:  "/api/paas/v4/batches/{batch_id}",
	schemas.FileUploadRequest:    "/api/paas/v4/files",
	schemas.FileListRequest:      "/api/paas/v4/files",
	schemas.FileRetrieveRequest:  "/api/paas/v4/files/{file_id}",
	schemas.FileDeleteRequest:    "/api/paas/v4/files/{file_id}",
	schemas.FileContentRequest:   "/api/paas/v4/files/{file_id}/content",
}

// The two lines below are the shape bigmodel documents for a batch output file
// (docs.bigmodel.cn/cn/guide/tools/batch.md, read 2026-11-10): no "error" member, the
// batch's id as the line id, and the request id inside the body. The error-file line is
// in OpenAI's shape, which is all the documentation says about that file.
const (
	bigmodelOutputLine1 = `{"response":{"status_code":200,"body":{"created":1715959701,"usage":{"completion_tokens":26,"prompt_tokens":89,"total_tokens":115},"model":"glm-4","id":"8668357533850320547","choices":[{"finish_reason":"stop","index":0,"message":{"role":"assistant","content":"ok"}}],"request_id":"615-request-1"}},"custom_id":"request-1","id":"batch_1791490810192076800"}`
	bigmodelOutputLine2 = `{"response":{"status_code":200,"body":{"created":1715959701,"usage":{"completion_tokens":22,"prompt_tokens":94,"total_tokens":116,"prompt_tokens_details":{"cached_tokens":64}},"model":"glm-4","id":"8668368425887509080","choices":[{"finish_reason":"stop","index":0,"message":{"role":"assistant","content":"ok"}}],"request_id":"616-request-2"}},"custom_id":"request-2","id":"batch_1791490810192076800"}`
	bigmodelErrorLine   = `{"response":{"status_code":400,"body":{"error":{"code":"1210","message":"API 调用参数有误，请检查文档。"}}},"custom_id":"request-3","id":"batch_1791490810192076800"}`
	bigmodelBatch       = `{"id":"batch_1791490810192076800","object":"batch","endpoint":"/v4/chat/completions","input_file_id":"file-in","completion_window":"24h","status":"completed","output_file_id":"file-out","error_file_id":"file-err","created_at":1715959000,"request_counts":{"total":3,"completed":2,"failed":1}}`
)

func newBigmodelTestProvider(v *batchVendor, overrides map[schemas.RequestType]string) (*OpenAIProvider, *schemas.BifrostContext, []schemas.Key) {
	provider := NewOpenAIProvider(&schemas.ProviderConfig{
		NetworkConfig: schemas.NetworkConfig{BaseURL: v.URL, DefaultRequestTimeoutInSeconds: 30},
		CustomProviderConfig: &schemas.CustomProviderConfig{
			CustomProviderKey:    "tkp_bigmodel",
			BaseProviderType:     schemas.OpenAI,
			RequestPathOverrides: overrides,
		},
	}, testNoopLogger{})
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	keys := []schemas.Key{{ID: "key-1", Name: "tk-key-1", Value: *schemas.NewSecretVar("sk-test")}}
	return provider, ctx, keys
}

// batchAndFileCalls runs every batch and file call that names its target in the URL and
// returns the upstream calls the vendor saw, in order.
func batchAndFileCalls(t *testing.T, v *batchVendor, provider *OpenAIProvider, ctx *schemas.BifrostContext, keys []schemas.Key) []string {
	t.Helper()
	_, bifrostErr := provider.BatchCreate(ctx, keys[0], &schemas.BifrostBatchCreateRequest{
		Endpoint: "/v4/chat/completions",
		Requests: []schemas.BatchRequestItem{{CustomID: "request-1", Method: "POST", URL: "/v4/chat/completions", Body: map[string]any{"model": "glm-5.3-flash"}}},
	})
	require.Nil(t, bifrostErr, "create: %v", bifrostErr)
	_, bifrostErr = provider.BatchRetrieve(ctx, keys, &schemas.BifrostBatchRetrieveRequest{BatchID: "batch_1"})
	require.Nil(t, bifrostErr, "retrieve: %v", bifrostErr)
	_, bifrostErr = provider.BatchCancel(ctx, keys, &schemas.BifrostBatchCancelRequest{BatchID: "batch_1"})
	require.Nil(t, bifrostErr, "cancel: %v", bifrostErr)
	_, bifrostErr = provider.BatchResults(ctx, keys, &schemas.BifrostBatchResultsRequest{BatchID: "batch_1"})
	require.Nil(t, bifrostErr, "results: %v", bifrostErr)
	_, bifrostErr = provider.FileRetrieve(ctx, keys, &schemas.BifrostFileRetrieveRequest{FileID: "file-out"})
	require.Nil(t, bifrostErr, "file retrieve: %v", bifrostErr)
	_, bifrostErr = provider.FileContent(ctx, keys, &schemas.BifrostFileContentRequest{FileID: "file-out"})
	require.Nil(t, bifrostErr, "file content: %v", bifrostErr)
	_, bifrostErr = provider.FileDelete(ctx, keys, &schemas.BifrostFileDeleteRequest{FileID: "file-out"})
	require.Nil(t, bifrostErr, "file delete: %v", bifrostErr)
	return v.snapshotCalls()
}

// Every batch and file call of a custom provider reaches the path its override names, the
// id in the placeholder's place, on a vendor that answers nowhere else; the built-in openai
// provider keeps the /v1 paths it had.
func TestBatchAndFileCallsHonourPathOverrides(t *testing.T) {
	files := map[string]string{"file-out": bigmodelOutputLine1 + "\n", "file-err": bigmodelErrorLine + "\n"}
	callsUnder := func(root string) []string {
		return []string{
			"POST " + root + "/files",
			"POST " + root + "/batches",
			"GET " + root + "/batches/batch_1",
			"POST " + root + "/batches/batch_1/cancel",
			"GET " + root + "/batches/batch_1",
			"GET " + root + "/files/file-out/content",
			"GET " + root + "/files/file-err/content",
			"GET " + root + "/files/file-out",
			"GET " + root + "/files/file-out/content",
			"DELETE " + root + "/files/file-out",
		}
	}

	t.Run("custom provider with overrides reaches /api/paas/v4", func(t *testing.T) {
		v := newBatchVendorAt(t, "/api/paas/v4", bigmodelBatch, files)
		provider, ctx, keys := newBigmodelTestProvider(v, bigmodelPathOverrides)
		assert.Equal(t, callsUnder("/api/paas/v4"), batchAndFileCalls(t, v, provider, ctx, keys))
		assert.Equal(t, map[string]int{"Bearer sk-test": 10}, v.auth, "every call carries the one key")
	})

	t.Run("built-in openai provider keeps /v1", func(t *testing.T) {
		v := newBatchVendor(t, bigmodelBatch, files)
		provider, ctx, keys := newBatchTestProvider(v)
		assert.Equal(t, callsUnder("/v1"), batchAndFileCalls(t, v, provider, ctx, keys))
	})

	t.Run("custom provider without overrides keeps /v1", func(t *testing.T) {
		v := newBatchVendor(t, bigmodelBatch, files)
		provider, ctx, keys := newBigmodelTestProvider(v, nil)
		assert.Equal(t, callsUnder("/v1"), batchAndFileCalls(t, v, provider, ctx, keys))
	})

	t.Run("an absolute override replaces the base URL", func(t *testing.T) {
		v := newBatchVendorAt(t, "/api/paas/v4", bigmodelBatch, files)
		provider, ctx, keys := newBigmodelTestProvider(v, map[schemas.RequestType]string{
			schemas.BatchRetrieveRequest: v.URL + "/api/paas/v4/batches/{batch_id}",
		})
		provider.networkConfig.BaseURL = "http://127.0.0.1:1"
		_, bifrostErr := provider.BatchRetrieve(ctx, keys, &schemas.BifrostBatchRetrieveRequest{BatchID: "batch_1"})
		require.Nil(t, bifrostErr, "retrieve: %v", bifrostErr)
		assert.Equal(t, []string{"GET /api/paas/v4/batches/batch_1"}, v.snapshotCalls())
	})

	t.Run("results fall back to the batch_retrieve override for the retrieve leg", func(t *testing.T) {
		v := newBatchVendorAt(t, "/api/paas/v4", bigmodelBatch, files)
		overrides := map[schemas.RequestType]string{}
		for requestType, override := range bigmodelPathOverrides {
			if requestType != schemas.BatchResultsRequest {
				overrides[requestType] = override
			}
		}
		provider, ctx, keys := newBigmodelTestProvider(v, overrides)
		_, bifrostErr := provider.BatchResults(ctx, keys, &schemas.BifrostBatchResultsRequest{BatchID: "batch_1"})
		require.Nil(t, bifrostErr, "results: %v", bifrostErr)
		assert.Equal(t, []string{"GET /api/paas/v4/batches/batch_1", "GET /api/paas/v4/files/file-out/content", "GET /api/paas/v4/files/file-err/content"}, v.snapshotCalls())
	})

	t.Run("an override that lost its placeholder is refused before any upstream call", func(t *testing.T) {
		v := newBatchVendorAt(t, "/api/paas/v4", bigmodelBatch, files)
		for requestType, call := range map[schemas.RequestType]func(*OpenAIProvider, *schemas.BifrostContext, []schemas.Key) *schemas.BifrostError{
			schemas.BatchRetrieveRequest: func(p *OpenAIProvider, ctx *schemas.BifrostContext, keys []schemas.Key) *schemas.BifrostError {
				_, err := p.BatchRetrieve(ctx, keys, &schemas.BifrostBatchRetrieveRequest{BatchID: "batch_1"})
				return err
			},
			schemas.BatchCancelRequest: func(p *OpenAIProvider, ctx *schemas.BifrostContext, keys []schemas.Key) *schemas.BifrostError {
				_, err := p.BatchCancel(ctx, keys, &schemas.BifrostBatchCancelRequest{BatchID: "batch_1"})
				return err
			},
			schemas.BatchResultsRequest: func(p *OpenAIProvider, ctx *schemas.BifrostContext, keys []schemas.Key) *schemas.BifrostError {
				_, err := p.BatchResults(ctx, keys, &schemas.BifrostBatchResultsRequest{BatchID: "batch_1"})
				return err
			},
			schemas.FileRetrieveRequest: func(p *OpenAIProvider, ctx *schemas.BifrostContext, keys []schemas.Key) *schemas.BifrostError {
				_, err := p.FileRetrieve(ctx, keys, &schemas.BifrostFileRetrieveRequest{FileID: "file-out"})
				return err
			},
			schemas.FileDeleteRequest: func(p *OpenAIProvider, ctx *schemas.BifrostContext, keys []schemas.Key) *schemas.BifrostError {
				_, err := p.FileDelete(ctx, keys, &schemas.BifrostFileDeleteRequest{FileID: "file-out"})
				return err
			},
			schemas.FileContentRequest: func(p *OpenAIProvider, ctx *schemas.BifrostContext, keys []schemas.Key) *schemas.BifrostError {
				_, err := p.FileContent(ctx, keys, &schemas.BifrostFileContentRequest{FileID: "file-out"})
				return err
			},
		} {
			provider, ctx, keys := newBigmodelTestProvider(v, map[schemas.RequestType]string{requestType: "/api/paas/v4/batches"})
			bifrostErr := call(provider, ctx, keys)
			require.NotNil(t, bifrostErr, "%s", requestType)
			assert.Contains(t, bifrostErr.Error.Message, "request_path_overrides."+string(requestType), "%s", requestType)
		}
		assert.Empty(t, v.snapshotCalls(), "a path without the id never reaches the vendor")
	})
}

// A bigmodel batch's results come back per line in the contract's shape: the output file's
// lines succeeded with the OpenAI chat body and its usage, the error file's line failed
// with the vendor's error. The batch names its endpoint "/v4/chat/completions", which is
// read as the chat operation.
func TestBatchResultsOfABigmodelBatch(t *testing.T) {
	v := newBatchVendorAt(t, "/api/paas/v4", bigmodelBatch, map[string]string{
		"file-out": bigmodelOutputLine1 + "\n" + bigmodelOutputLine2 + "\n",
		"file-err": bigmodelErrorLine + "\n",
	})
	provider, ctx, keys := newBigmodelTestProvider(v, bigmodelPathOverrides)

	resp, bifrostErr := provider.BatchResults(ctx, keys, &schemas.BifrostBatchResultsRequest{BatchID: "batch_1791490810192076800"})
	require.Nil(t, bifrostErr, "results: %v", bifrostErr)
	require.Len(t, resp.Results, 3)

	first := resp.Results[0]
	assert.Equal(t, "request-1", first.CustomID)
	assert.Equal(t, schemas.BatchResultStatusSucceeded, first.Status)
	assert.Nil(t, first.Error)
	require.NotNil(t, first.Response)
	assert.Equal(t, 200, first.Response.StatusCode)
	assert.Equal(t, "615-request-1", first.Response.Body["request_id"])
	assert.Len(t, first.Response.Body["choices"], 1)
	usage := first.Response.Body["usage"].(map[string]any)
	assert.EqualValues(t, 89, usage["prompt_tokens"])
	assert.EqualValues(t, 26, usage["completion_tokens"])
	assert.EqualValues(t, 115, usage["total_tokens"])

	second := resp.Results[1]
	assert.Equal(t, "request-2", second.CustomID)
	usage = second.Response.Body["usage"].(map[string]any)
	assert.EqualValues(t, 94, usage["prompt_tokens"])
	assert.EqualValues(t, 64, usage["prompt_tokens_details"].(map[string]any)["cached_read_tokens"], "the unified usage of the chat operation, as on the sync route")

	failed := resp.Results[2]
	assert.Equal(t, "request-3", failed.CustomID)
	assert.Equal(t, schemas.BatchResultStatusFailed, failed.Status)
	require.NotNil(t, failed.Error)
	assert.Equal(t, "1210", failed.Error.Code)
	assert.Equal(t, "API 调用参数有误，请检查文档。", failed.Error.Message)
}

// BenchmarkBatchAndFileResourceURL measures building the URL of a batch or file call that
// names its target. "before" is what the six call sites did until they read
// request_path_overrides: one concatenation onto the base URL. The built-in provider and a
// custom provider without overrides must cost the same; only an overridden path pays for
// the template.
func BenchmarkBatchAndFileResourceURL(b *testing.B) {
	const id = "batch_1791490810192076800"
	newProvider := func(custom *schemas.CustomProviderConfig) *OpenAIProvider {
		return NewOpenAIProvider(&schemas.ProviderConfig{
			NetworkConfig:        schemas.NetworkConfig{BaseURL: "https://open.bigmodel.cn", DefaultRequestTimeoutInSeconds: 30},
			CustomProviderConfig: custom,
		}, testNoopLogger{})
	}
	var sink string
	b.Run("before: base URL + /v1 path", func(b *testing.B) {
		provider := newProvider(nil)
		b.ReportAllocs()
		for b.Loop() {
			sink = provider.networkConfig.BaseURL + "/v1/batches/" + id + "/cancel"
		}
	})
	for _, bc := range []struct {
		name   string
		custom *schemas.CustomProviderConfig
	}{
		{"built-in provider", nil},
		{"custom provider without overrides", &schemas.CustomProviderConfig{CustomProviderKey: "tkp_plain", BaseProviderType: schemas.OpenAI}},
		{"custom provider, path override", &schemas.CustomProviderConfig{CustomProviderKey: "tkp_bigmodel", BaseProviderType: schemas.OpenAI, RequestPathOverrides: bigmodelPathOverrides}},
		{"custom provider, full URL override", &schemas.CustomProviderConfig{CustomProviderKey: "tkp_bigmodel", BaseProviderType: schemas.OpenAI, RequestPathOverrides: map[schemas.RequestType]string{schemas.BatchCancelRequest: "https://open.bigmodel.cn/api/paas/v4/batches/{batch_id}/cancel"}}},
	} {
		b.Run(bc.name, func(b *testing.B) {
			provider := newProvider(bc.custom)
			b.ReportAllocs()
			for b.Loop() {
				url, err := provider.buildResourceURL("/v1/batches/", id, "/cancel", schemas.BatchCancelRequest)
				if err != nil {
					b.Fatal(err.Error.Message)
				}
				sink = url
			}
		})
	}
	_ = sink
}

// resourceVendor records the method and path of every call and answers each with an empty
// JSON object. The tests here assert where a call went, not what came back.
type resourceVendor struct {
	*httptest.Server
	mu    sync.Mutex
	calls []string
}

func newResourceVendor(t *testing.T) *resourceVendor {
	t.Helper()
	v := &resourceVendor{}
	v.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v.mu.Lock()
		v.calls = append(v.calls, r.Method+" "+r.URL.Path)
		v.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{}`)
	}))
	t.Cleanup(v.Close)
	return v
}

func (v *resourceVendor) takeCalls() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	calls := v.calls
	v.calls = nil
	return calls
}

func newResourceTestProvider(v *resourceVendor, custom bool, overrides map[schemas.RequestType]string) (*OpenAIProvider, *schemas.BifrostContext, schemas.Key) {
	config := &schemas.ProviderConfig{NetworkConfig: schemas.NetworkConfig{BaseURL: v.URL, DefaultRequestTimeoutInSeconds: 30}}
	if custom {
		config.CustomProviderConfig = &schemas.CustomProviderConfig{
			CustomProviderKey:    "tkp_video_vendor",
			BaseProviderType:     schemas.OpenAI,
			RequestPathOverrides: overrides,
		}
	}
	provider := NewOpenAIProvider(config, testNoopLogger{})
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	return provider, ctx, schemas.Key{ID: "key-1", Name: "tk-key-1", Value: *schemas.NewSecretVar("sk-test")}
}

// resourceCall is one OpenAI call whose default path names an id, outside the batch and
// file calls (batch_test.go covers those).
type resourceCall struct {
	// defaultCall is the upstream call of the built-in provider.
	defaultCall string
	// template is a request_path_overrides entry off /v1, and templatedCall the upstream
	// call it must produce. The two-id templates name the file before the container, so an
	// id substituted by position instead of by placeholder lands in the wrong segment.
	template      string
	templatedCall string
	call          func(p *OpenAIProvider, ctx *schemas.BifrostContext, key schemas.Key) *schemas.BifrostError
}

var resourceCalls = map[schemas.RequestType]resourceCall{
	schemas.ModelRetrieveRequest: {"GET /v1/models/glm-video-1", "/api/v3/models/{model}", "GET /api/v3/models/glm-video-1",
		func(p *OpenAIProvider, ctx *schemas.BifrostContext, key schemas.Key) *schemas.BifrostError {
			_, err := p.ModelRetrieve(ctx, key, &schemas.BifrostModelRetrieveRequest{Model: "glm-video-1"})
			return err
		}},
	schemas.VideoRetrieveRequest: {"GET /v1/videos/video_1", "/api/v3/tasks/{video_id}", "GET /api/v3/tasks/video_1",
		func(p *OpenAIProvider, ctx *schemas.BifrostContext, key schemas.Key) *schemas.BifrostError {
			_, err := p.VideoRetrieve(ctx, key, &schemas.BifrostVideoRetrieveRequest{ID: "video_1"})
			return err
		}},
	schemas.VideoDownloadRequest: {"GET /v1/videos/video_1/content", "/api/v3/tasks/{video_id}/file", "GET /api/v3/tasks/video_1/file",
		func(p *OpenAIProvider, ctx *schemas.BifrostContext, key schemas.Key) *schemas.BifrostError {
			_, err := p.VideoDownload(ctx, key, &schemas.BifrostVideoDownloadRequest{ID: "video_1"})
			return err
		}},
	schemas.VideoDeleteRequest: {"DELETE /v1/videos/video_1", "/api/v3/tasks/{video_id}", "DELETE /api/v3/tasks/video_1",
		func(p *OpenAIProvider, ctx *schemas.BifrostContext, key schemas.Key) *schemas.BifrostError {
			_, err := p.VideoDelete(ctx, key, &schemas.BifrostVideoDeleteRequest{ID: "video_1"})
			return err
		}},
	schemas.VideoRemixRequest: {"POST /v1/videos/video_1/remix", "/api/v3/tasks/{video_id}/remix", "POST /api/v3/tasks/video_1/remix",
		func(p *OpenAIProvider, ctx *schemas.BifrostContext, key schemas.Key) *schemas.BifrostError {
			_, err := p.VideoRemix(ctx, key, &schemas.BifrostVideoRemixRequest{ID: "video_1", Input: &schemas.VideoGenerationInput{Prompt: "again"}})
			return err
		}},
	schemas.ContainerRetrieveRequest: {"GET /v1/containers/cntr_1", "/api/v3/boxes/{container_id}", "GET /api/v3/boxes/cntr_1",
		func(p *OpenAIProvider, ctx *schemas.BifrostContext, key schemas.Key) *schemas.BifrostError {
			_, err := p.ContainerRetrieve(ctx, []schemas.Key{key}, &schemas.BifrostContainerRetrieveRequest{ContainerID: "cntr_1"})
			return err
		}},
	schemas.ContainerDeleteRequest: {"DELETE /v1/containers/cntr_1", "/api/v3/boxes/{container_id}", "DELETE /api/v3/boxes/cntr_1",
		func(p *OpenAIProvider, ctx *schemas.BifrostContext, key schemas.Key) *schemas.BifrostError {
			_, err := p.ContainerDelete(ctx, []schemas.Key{key}, &schemas.BifrostContainerDeleteRequest{ContainerID: "cntr_1"})
			return err
		}},
	schemas.ContainerFileCreateRequest: {"POST /v1/containers/cntr_1/files", "/api/v3/boxes/{container_id}/items", "POST /api/v3/boxes/cntr_1/items",
		func(p *OpenAIProvider, ctx *schemas.BifrostContext, key schemas.Key) *schemas.BifrostError {
			_, err := p.ContainerFileCreate(ctx, key, &schemas.BifrostContainerFileCreateRequest{ContainerID: "cntr_1", File: []byte("bytes")})
			return err
		}},
	schemas.ContainerFileListRequest: {"GET /v1/containers/cntr_1/files", "/api/v3/boxes/{container_id}/items", "GET /api/v3/boxes/cntr_1/items",
		func(p *OpenAIProvider, ctx *schemas.BifrostContext, key schemas.Key) *schemas.BifrostError {
			_, err := p.ContainerFileList(ctx, []schemas.Key{key}, &schemas.BifrostContainerFileListRequest{ContainerID: "cntr_1"})
			return err
		}},
	schemas.ContainerFileRetrieveRequest: {"GET /v1/containers/cntr_1/files/cfile_2", "/api/v3/items/{file_id}/in/{container_id}", "GET /api/v3/items/cfile_2/in/cntr_1",
		func(p *OpenAIProvider, ctx *schemas.BifrostContext, key schemas.Key) *schemas.BifrostError {
			_, err := p.ContainerFileRetrieve(ctx, []schemas.Key{key}, &schemas.BifrostContainerFileRetrieveRequest{ContainerID: "cntr_1", FileID: "cfile_2"})
			return err
		}},
	schemas.ContainerFileContentRequest: {"GET /v1/containers/cntr_1/files/cfile_2/content", "/api/v3/items/{file_id}/in/{container_id}/bytes", "GET /api/v3/items/cfile_2/in/cntr_1/bytes",
		func(p *OpenAIProvider, ctx *schemas.BifrostContext, key schemas.Key) *schemas.BifrostError {
			_, err := p.ContainerFileContent(ctx, []schemas.Key{key}, &schemas.BifrostContainerFileContentRequest{ContainerID: "cntr_1", FileID: "cfile_2"})
			return err
		}},
	schemas.ContainerFileDeleteRequest: {"DELETE /v1/containers/cntr_1/files/cfile_2", "/api/v3/items/{file_id}/in/{container_id}", "DELETE /api/v3/items/cfile_2/in/cntr_1",
		func(p *OpenAIProvider, ctx *schemas.BifrostContext, key schemas.Key) *schemas.BifrostError {
			_, err := p.ContainerFileDelete(ctx, []schemas.Key{key}, &schemas.BifrostContainerFileDeleteRequest{ContainerID: "cntr_1", FileID: "cfile_2"})
			return err
		}},
	schemas.ResponsesRetrieveRequest: {"GET /v1/responses/resp_1", "/api/v3/answers/{response_id}", "GET /api/v3/answers/resp_1",
		func(p *OpenAIProvider, ctx *schemas.BifrostContext, key schemas.Key) *schemas.BifrostError {
			_, err := p.ResponsesRetrieve(ctx, key, &schemas.BifrostResponsesRetrieveRequest{ResponseID: "resp_1"})
			return err
		}},
	schemas.ResponsesRetrieveStreamRequest: {"GET /v1/responses/resp_1", "/api/v3/answers/{response_id}/events", "GET /api/v3/answers/resp_1/events",
		func(p *OpenAIProvider, ctx *schemas.BifrostContext, key schemas.Key) *schemas.BifrostError {
			runner := func(_ *schemas.BifrostContext, result *schemas.BifrostResponse, err *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError) {
				return result, err
			}
			stream, err := p.ResponsesRetrieveStream(ctx, runner, func(context.Context) {}, key, &schemas.BifrostResponsesRetrieveRequest{ResponseID: "resp_1"})
			if stream != nil {
				deadline := time.After(5 * time.Second)
				for open := true; open; {
					select {
					case _, open = <-stream:
					case <-deadline:
						open = false
					}
				}
			}
			return err
		}},
	schemas.ResponsesDeleteRequest: {"DELETE /v1/responses/resp_1", "/api/v3/answers/{response_id}", "DELETE /api/v3/answers/resp_1",
		func(p *OpenAIProvider, ctx *schemas.BifrostContext, key schemas.Key) *schemas.BifrostError {
			_, err := p.ResponsesDelete(ctx, key, &schemas.BifrostResponsesDeleteRequest{ResponseID: "resp_1"})
			return err
		}},
	schemas.ResponsesCancelRequest: {"POST /v1/responses/resp_1/cancel", "/api/v3/answers/{response_id}/stop", "POST /api/v3/answers/resp_1/stop",
		func(p *OpenAIProvider, ctx *schemas.BifrostContext, key schemas.Key) *schemas.BifrostError {
			_, err := p.ResponsesCancel(ctx, key, &schemas.BifrostResponsesCancelRequest{ResponseID: "resp_1"})
			return err
		}},
	schemas.ResponsesInputItemsRequest: {"GET /v1/responses/resp_1/input_items", "/api/v3/answers/{response_id}/inputs", "GET /api/v3/answers/resp_1/inputs",
		func(p *OpenAIProvider, ctx *schemas.BifrostContext, key schemas.Key) *schemas.BifrostError {
			_, err := p.ResponsesInputItems(ctx, key, &schemas.BifrostResponsesInputItemsRequest{ResponseID: "resp_1"})
			return err
		}},
}

// batchAndFileResourceTypes are the listed request types that batch_test.go covers.
var batchAndFileResourceTypes = []schemas.RequestType{
	schemas.BatchRetrieveRequest, schemas.BatchCancelRequest, schemas.BatchResultsRequest,
	schemas.FileRetrieveRequest, schemas.FileDeleteRequest, schemas.FileContentRequest,
}

// Every request type listed in ResourcePathPlaceholders has a row here or in the batch and
// file tests, so a request type added to the list without a call that honours its template
// fails this test instead of silently losing the id.
func TestEveryResourceRequestTypeHasACall(t *testing.T) {
	covered := map[schemas.RequestType]bool{}
	for _, requestType := range batchAndFileResourceTypes {
		covered[requestType] = true
	}
	for requestType := range resourceCalls {
		covered[requestType] = true
	}
	for requestType := range providerUtils.ResourcePathPlaceholders {
		assert.True(t, covered[requestType], "%s is listed in ResourcePathPlaceholders and has no call in resourceCalls", requestType)
	}
	for requestType := range covered {
		assert.Contains(t, providerUtils.ResourcePathPlaceholders, requestType, "%s has a call here and no placeholder", requestType)
	}
}

func TestResourceCallsHonourPathOverrides(t *testing.T) {
	allTemplates := map[schemas.RequestType]string{}
	for requestType, rc := range resourceCalls {
		allTemplates[requestType] = rc.template
	}

	for requestType, rc := range resourceCalls {
		t.Run(string(requestType), func(t *testing.T) {
			v := newResourceVendor(t)

			provider, ctx, key := newResourceTestProvider(v, false, nil)
			_ = rc.call(provider, ctx, key)
			assert.Equal(t, []string{rc.defaultCall}, v.takeCalls(), "built-in openai provider")

			provider, ctx, key = newResourceTestProvider(v, true, nil)
			_ = rc.call(provider, ctx, key)
			assert.Equal(t, []string{rc.defaultCall}, v.takeCalls(), "custom provider without overrides")

			// An override for another request type leaves this one on its default path.
			other := map[schemas.RequestType]string{schemas.ChatCompletionRequest: "/api/v3/chat/completions"}
			provider, ctx, key = newResourceTestProvider(v, true, other)
			_ = rc.call(provider, ctx, key)
			assert.Equal(t, []string{rc.defaultCall}, v.takeCalls(), "custom provider with an override for another request type")

			provider, ctx, key = newResourceTestProvider(v, true, allTemplates)
			_ = rc.call(provider, ctx, key)
			assert.Equal(t, []string{rc.templatedCall}, v.takeCalls(), "custom provider with a path template")

			// A full URL template replaces the base URL.
			provider, ctx, key = newResourceTestProvider(v, true, map[schemas.RequestType]string{requestType: v.URL + rc.template})
			provider.networkConfig.BaseURL = "http://127.0.0.1:1"
			_ = rc.call(provider, ctx, key)
			assert.Equal(t, []string{rc.templatedCall}, v.takeCalls(), "custom provider with a full URL template")

			// The override the UI used to accept: a path with no id. Refused, nothing sent.
			provider, ctx, key = newResourceTestProvider(v, true, map[schemas.RequestType]string{requestType: "/api/v3/things"})
			bifrostErr := rc.call(provider, ctx, key)
			require.NotNil(t, bifrostErr, "an override without its placeholder must be refused")
			require.NotNil(t, bifrostErr.Error)
			assert.Contains(t, bifrostErr.Error.Message, "request_path_overrides."+string(requestType))
			assert.Contains(t, bifrostErr.Error.Message, providerUtils.ResourcePathPlaceholders[requestType][0])
			assert.NotContains(t, bifrostErr.Error.Message, "/api/v3/things", "the stored override stays out of an error returned to the caller")
			assert.Empty(t, v.takeCalls(), "no upstream call for a template that lost the id")
		})
	}
}

// A path the caller's own request put on the context still wins over a template, as it
// does for every other OpenAI call.
func TestResourceCallContextPathWinsOverATemplate(t *testing.T) {
	v := newResourceVendor(t)
	provider, ctx, key := newResourceTestProvider(v, true, map[schemas.RequestType]string{schemas.ModelRetrieveRequest: "/api/v3/models/{model}"})
	ctx.SetValue(schemas.BifrostContextKeyURLPath, "/models/glm-video-1")
	_, _ = provider.ModelRetrieve(ctx, key, &schemas.BifrostModelRetrieveRequest{Model: "glm-video-1"})
	assert.Equal(t, []string{"GET /models/glm-video-1"}, v.takeCalls())
}

// BenchmarkResourceURLForIDs measures what building the URL of a call that names an id costs
// per request: the built-in provider and a custom provider without an override for the
// request type take the path they took before this helper existed.
func BenchmarkResourceURLForIDs(b *testing.B) {
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	newProvider := func(custom bool, overrides map[schemas.RequestType]string) *OpenAIProvider {
		config := &schemas.ProviderConfig{NetworkConfig: schemas.NetworkConfig{BaseURL: "https://api.openai.com", DefaultRequestTimeoutInSeconds: 30}}
		if custom {
			config.CustomProviderConfig = &schemas.CustomProviderConfig{CustomProviderKey: "tkp_video_vendor", BaseProviderType: schemas.OpenAI, RequestPathOverrides: overrides}
		}
		return NewOpenAIProvider(config, testNoopLogger{})
	}
	cases := []struct {
		name     string
		provider *OpenAIProvider
	}{
		{"built-in", newProvider(false, nil)},
		{"custom without overrides", newProvider(true, nil)},
		{"custom with another override", newProvider(true, map[schemas.RequestType]string{schemas.ChatCompletionRequest: "/api/v3/chat/completions"})},
		{"custom with a template", newProvider(true, map[schemas.RequestType]string{schemas.ContainerFileRetrieveRequest: "/api/v3/boxes/{container_id}/items/{file_id}"})},
	}
	for _, bc := range cases {
		b.Run("before/"+bc.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = bc.provider.buildRequestURL(ctx, "/v1/containers/"+"cntr_1"+"/files/"+"cfile_2", schemas.ContainerFileRetrieveRequest)
			}
		})
		b.Run("after/"+bc.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := bc.provider.buildResourceURLForIDs(ctx, "/v1/containers/"+"cntr_1"+"/files/"+"cfile_2", schemas.ContainerFileRetrieveRequest, "cntr_1", "cfile_2"); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
