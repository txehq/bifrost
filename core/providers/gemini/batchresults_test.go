package gemini

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestGeminiFileResultToBatchResultItem(t *testing.T) {
	t.Run("NativeGenerateContentResponse", func(t *testing.T) {
		var line GeminiBatchFileResultLine
		require.NoError(t, sonic.Unmarshal([]byte(`{"key":"request-1","response":{"candidates":[{"content":{"parts":[{"text":"hello"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":2,"totalTokenCount":9}}}`), &line))

		item, err := geminiFileResultToBatchResultItem(line, "fallback")
		require.NoError(t, err)
		assert.Equal(t, "request-1", item.CustomID)
		require.NotNil(t, item.Response)
		assert.Equal(t, 200, item.Response.StatusCode)
		assert.Equal(t, schemas.BatchResultStatusSucceeded, item.Status)
		assert.Equal(t, "chat.completion", item.Response.Body["object"])
		assert.Equal(t, "hello", batchLineChoice(t, item)["message"].(map[string]interface{})["content"])
		usage, ok := item.Response.Body["usage"].(map[string]interface{})
		require.True(t, ok)
		assert.EqualValues(t, 7, usage["prompt_tokens"])
		assert.EqualValues(t, 2, usage["completion_tokens"])
		assert.EqualValues(t, 9, usage["total_tokens"])
	})

	t.Run("CompatibilityEnvelope", func(t *testing.T) {
		var line GeminiBatchFileResultLine
		require.NoError(t, sonic.Unmarshal([]byte(`{"custom_id":"request-2","response":{"status_code":200,"body":{"model":"gemini-test","usage":{"promptTokens":3,"completionTokens":1,"totalTokens":4}}}}`), &line))

		item, err := geminiFileResultToBatchResultItem(line, "fallback")
		require.NoError(t, err)
		require.NotNil(t, item.Response)
		assert.Equal(t, 200, item.Response.StatusCode)
		usage, ok := item.Response.Body["usage"].(map[string]interface{})
		require.True(t, ok)
		assert.EqualValues(t, 3, usage["prompt_tokens"])
		assert.EqualValues(t, 1, usage["completion_tokens"])
		assert.EqualValues(t, 4, usage["total_tokens"])
	})

	t.Run("RejectsMissingResponseAndError", func(t *testing.T) {
		item, err := geminiFileResultToBatchResultItem(GeminiBatchFileResultLine{Key: "request-3"}, "fallback")
		require.Error(t, err)
		assert.Empty(t, item)
		assert.Contains(t, err.Error(), "neither response nor error")

		var nullResponse GeminiBatchFileResultLine
		require.NoError(t, sonic.Unmarshal([]byte(`{"key":"request-4","response":null}`), &nullResponse))
		item, err = geminiFileResultToBatchResultItem(nullResponse, "fallback")
		require.Error(t, err)
		assert.Empty(t, item)
	})
}

func TestParseGeminiBatchResultsJSONLUsesSourceLineIndex(t *testing.T) {
	body := []byte("{malformed}\n" +
		`{"response":{"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":1,"totalTokenCount":3}}}` + "\n")

	results, parseErrors := parseGeminiBatchResultsJSONL(body, nil)
	require.Len(t, parseErrors, 1)
	require.Len(t, results, 1)
	assert.Equal(t, "request-1", results[0].CustomID)
}

// TestGeminiBatchOutput locks in the fix for issue #3951: the generativelanguage batch
// REST API reports output under the Operation's `response` field (mirrored in
// `metadata.output`), never under `dest`. Inline responses are nested one level deep
// (response.inlinedResponses.inlinedResponses). geminiBatchOutput must read the real
// fields so completed results are no longer silently dropped.
func TestGeminiBatchOutput(t *testing.T) {
	t.Run("InlineUnderResponse", func(t *testing.T) {
		raw := `{
  "name": "batches/abc123",
  "metadata": {
    "@type": "type.googleapis.com/google.ai.generativelanguage.v1beta.GenerateContentBatch",
    "name": "batches/abc123",
    "state": "BATCH_STATE_SUCCEEDED",
    "batchStats": {"requestCount": "2", "successfulRequestCount": "2", "pendingRequestCount": "0"}
  },
  "done": true,
  "response": {
    "@type": "type.googleapis.com/google.ai.generativelanguage.v1beta.GenerateContentBatchOutput",
    "inlinedResponses": {
      "inlinedResponses": [
        {"metadata": {"key": "req-1"}, "response": {"candidates": [{"content": {"parts": [{"text": "4"}]}, "finishReason": "STOP"}], "usageMetadata": {"promptTokenCount": 5, "candidatesTokenCount": 1, "totalTokenCount": 6}}},
        {"metadata": {"key": "req-2"}, "response": {"candidates": [{"content": {"parts": [{"text": "Paris"}]}, "finishReason": "STOP"}], "usageMetadata": {"promptTokenCount": 4, "candidatesTokenCount": 1, "totalTokenCount": 5}}}
      ]
    }
  }
}`
		var resp GeminiBatchJobResponse
		require.NoError(t, sonic.Unmarshal([]byte(raw), &resp))

		fileName, inlined := geminiBatchOutput(&resp)
		assert.Empty(t, fileName)
		require.Len(t, inlined, 2)

		require.NotNil(t, inlined[0].Metadata)
		assert.Equal(t, "req-1", inlined[0].Metadata.Key)
		require.NotNil(t, inlined[0].Response)
		require.Len(t, inlined[0].Response.Candidates, 1)
		require.NotNil(t, inlined[0].Response.Candidates[0].Content)
		require.Len(t, inlined[0].Response.Candidates[0].Content.Parts, 1)
		assert.Equal(t, "4", inlined[0].Response.Candidates[0].Content.Parts[0].Text)

		require.NotNil(t, inlined[1].Metadata)
		assert.Equal(t, "req-2", inlined[1].Metadata.Key)
	})

	t.Run("FileUnderResponse", func(t *testing.T) {
		raw := `{
  "name": "batches/file1",
  "metadata": {"name": "batches/file1", "state": "BATCH_STATE_SUCCEEDED", "batchStats": {"requestCount": "10", "successfulRequestCount": "10", "pendingRequestCount": "0"}},
  "done": true,
  "response": {"@type": "type.googleapis.com/google.ai.generativelanguage.v1beta.GenerateContentBatchOutput", "responsesFile": "files/batch-out-1"}
}`
		var resp GeminiBatchJobResponse
		require.NoError(t, sonic.Unmarshal([]byte(raw), &resp))

		fileName, inlined := geminiBatchOutput(&resp)
		assert.Equal(t, "files/batch-out-1", fileName)
		assert.Empty(t, inlined)
	})

	t.Run("MetadataOutputFallback", func(t *testing.T) {
		// No top-level response; output only present under metadata.output.
		raw := `{
  "name": "batches/meta1",
  "metadata": {
    "name": "batches/meta1",
    "state": "BATCH_STATE_SUCCEEDED",
    "batchStats": {"requestCount": "1", "successfulRequestCount": "1", "pendingRequestCount": "0"},
    "output": {"inlinedResponses": {"inlinedResponses": [{"metadata": {"key": "only-1"}, "response": {"candidates": [{"content": {"parts": [{"text": "hey"}]}, "finishReason": "STOP"}]}}]}}
  },
  "done": true
}`
		var resp GeminiBatchJobResponse
		require.NoError(t, sonic.Unmarshal([]byte(raw), &resp))

		fileName, inlined := geminiBatchOutput(&resp)
		assert.Empty(t, fileName)
		require.Len(t, inlined, 1)
		require.NotNil(t, inlined[0].Metadata)
		assert.Equal(t, "only-1", inlined[0].Metadata.Key)
	})

	t.Run("IgnoresDestField", func(t *testing.T) {
		// The legacy dest field must never be read: only response/metadata.output count.
		resp := &GeminiBatchJobResponse{
			Name: "batches/dest1",
			Dest: &GeminiBatchDest{FileName: "files/should-be-ignored"},
		}
		fileName, inlined := geminiBatchOutput(resp)
		assert.Empty(t, fileName)
		assert.Empty(t, inlined)
	})

	t.Run("NilResponse", func(t *testing.T) {
		fileName, inlined := geminiBatchOutput(nil)
		assert.Empty(t, fileName)
		assert.Empty(t, inlined)
	})
}

// TestGeminiInlineResponseToBatchResultItem verifies the per-response conversion used by
// the batch results path for inline batches.
func TestGeminiInlineResponseToBatchResultItem(t *testing.T) {
	t.Run("SuccessWithMetadataKey", func(t *testing.T) {
		inline := GeminiInlinedResponse{
			Metadata: &GeminiBatchMetadata{Key: "row-9"},
			Response: &GenerateContentResponse{
				Candidates: []*Candidate{{
					Content:      &Content{Parts: []*Part{{Text: "hi"}}},
					FinishReason: FinishReasonStop,
				}},
				UsageMetadata: &GenerateContentResponseUsageMetadata{
					PromptTokenCount:     2,
					CandidatesTokenCount: 3,
					TotalTokenCount:      5,
				},
			},
		}

		item := geminiInlineResponseToBatchResultItem(inline, "request-0")
		assert.Equal(t, "row-9", item.CustomID)
		assert.Nil(t, item.Error)
		require.NotNil(t, item.Response)
		assert.Equal(t, 200, item.Response.StatusCode)
		assert.Equal(t, schemas.BatchResultStatusSucceeded, item.Status)
		choice := batchLineChoice(t, item)
		assert.Equal(t, "hi", choice["message"].(map[string]interface{})["content"])
		assert.Equal(t, "stop", choice["finish_reason"])

		usage, ok := item.Response.Body["usage"].(map[string]interface{})
		require.True(t, ok)
		assert.EqualValues(t, 2, usage["prompt_tokens"])
		assert.EqualValues(t, 3, usage["completion_tokens"])
		assert.EqualValues(t, 5, usage["total_tokens"])
	})

	t.Run("ErrorUsesFallbackID", func(t *testing.T) {
		inline := GeminiInlinedResponse{
			Error: &GeminiBatchErrorInfo{Code: 429, Message: "rate limited"},
		}

		item := geminiInlineResponseToBatchResultItem(inline, "request-3")
		assert.Equal(t, "request-3", item.CustomID)
		assert.Equal(t, schemas.BatchResultStatusFailed, item.Status)
		assert.Nil(t, item.Response)
		require.NotNil(t, item.Error)
		assert.Equal(t, "429", item.Error.Code)
		assert.Equal(t, "rate limited", item.Error.Message)
	})
}

// batchLineChoice returns the first choice of a chat result line's body.
func batchLineChoice(t *testing.T, item schemas.BatchResultItem) map[string]interface{} {
	t.Helper()
	require.NotNil(t, item.Response)
	choices, ok := item.Response.Body["choices"].([]interface{})
	require.True(t, ok, "body has no choices: %v", item.Response.Body)
	require.NotEmpty(t, choices)
	return choices[0].(map[string]interface{})
}

// A batch line's body is the sync chat response for the same native response: the batch path
// calls the sync converter, so text, finish reason and usage cannot drift from the sync route.
func TestBatchResultItemFromGeminiResponseIsTheSyncChatResponse(t *testing.T) {
	t.Run("TextAndUsage", func(t *testing.T) {
		resp := &GenerateContentResponse{
			ResponseID:   "resp-1",
			ModelVersion: "gemini-2.5-flash",
			Candidates: []*Candidate{{
				Content:      &Content{Parts: []*Part{{Text: "foo"}, {Text: "bar"}}},
				FinishReason: FinishReasonStop,
			}},
			UsageMetadata: &GenerateContentResponseUsageMetadata{
				PromptTokenCount:     1,
				CandidatesTokenCount: 2,
				TotalTokenCount:      3,
			},
		}

		item := BatchResultItemFromGeminiResponse("line-1", schemas.BatchEndpointChatCompletions, resp, nil, false)
		assert.Equal(t, schemas.BatchResultStatusSucceeded, item.Status)

		want, err := sonic.Marshal(resp.ToBifrostChatResponse())
		require.NoError(t, err)
		var wantBody map[string]interface{}
		require.NoError(t, sonic.Unmarshal(want, &wantBody))
		delete(wantBody, "extra_fields")
		assert.Equal(t, wantBody, item.Response.Body)
		assert.Equal(t, "gemini-2.5-flash", item.Response.Body["model"], "the model is on the line for per-line settlement")
		assert.Equal(t, "stop", batchLineChoice(t, item)["finish_reason"])
		_, hasExtraFields := item.Response.Body["extra_fields"]
		assert.False(t, hasExtraFields, "extra_fields describes a sync call and is absent unless the raw response is asked for")
	})

	t.Run("NoUsageNoText", func(t *testing.T) {
		resp := &GenerateContentResponse{
			Candidates: []*Candidate{{FinishReason: FinishReasonMaxTokens}},
		}

		item := BatchResultItemFromGeminiResponse("line-2", "", resp, nil, false)
		assert.Equal(t, schemas.BatchResultStatusSucceeded, item.Status)
		assert.Equal(t, "length", batchLineChoice(t, item)["finish_reason"])
		assert.Nil(t, item.Response.Body["usage"], "usage the vendor did not report stays empty")
	})

	t.Run("RawResponseKeepsSafetyRatings", func(t *testing.T) {
		var resp GenerateContentResponse
		require.NoError(t, sonic.Unmarshal([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP","safetyRatings":[{"category":"HARM_CATEGORY_HATE_SPEECH","probability":"NEGLIGIBLE"}]}],"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":1,"totalTokenCount":5},"modelVersion":"gemini-2.5-flash"}`), &resp))

		item := BatchResultItemFromGeminiResponse("line-3", schemas.BatchEndpointChatCompletions, &resp, nil, true)
		extraFields, ok := item.Response.Body["extra_fields"].(map[string]interface{})
		require.True(t, ok)
		raw, ok := extraFields["raw_response"].(map[string]interface{})
		require.True(t, ok)
		candidate := raw["candidates"].([]interface{})[0].(map[string]interface{})
		ratings, ok := candidate["safetyRatings"].([]interface{})
		require.True(t, ok, "the native response keeps safetyRatings: %v", candidate)
		assert.Equal(t, "HARM_CATEGORY_HATE_SPEECH", ratings[0].(map[string]interface{})["category"])
	})

	t.Run("NoOutcomeIsFailed", func(t *testing.T) {
		item := BatchResultItemFromGeminiResponse("line-4", "", nil, nil, false)
		assert.Equal(t, schemas.BatchResultStatusFailed, item.Status)
		assert.True(t, item.Failed())
	})
}

// TestGeminiBatchResult_ThinkingTokens verifies that thinking tokens (thoughtsTokenCount)
// are added to completion_tokens and surfaced under completion_tokens_details.reasoning_tokens
// in both file-based and inline batch result processing.
func TestGeminiBatchResult_ThinkingTokens(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                 string
		promptTokens         int32
		candidatesTokens     int32
		thoughtsTokens       int32
		totalTokens          int32
		cachedTokens         int32
		wantPromptTokens     int
		wantCompletionTokens int
		wantTotalTokens      int
		wantReasoningTokens  int
		wantCachedTokens     int
	}{
		{
			name:                 "WithThinkingTokens",
			promptTokens:         100,
			candidatesTokens:     50,
			thoughtsTokens:       400,
			totalTokens:          550,
			wantPromptTokens:     100,
			wantCompletionTokens: 450, // 50 candidates + 400 thoughts
			wantTotalTokens:      550,
			wantReasoningTokens:  400,
		},
		{
			name:                 "WithThinkingTokensAndCachedTokens",
			promptTokens:         120,
			candidatesTokens:     60,
			thoughtsTokens:       300,
			totalTokens:          480,
			cachedTokens:         40,
			wantPromptTokens:     120,
			wantCompletionTokens: 360, // 60 candidates + 300 thoughts
			wantTotalTokens:      480,
			wantReasoningTokens:  300,
			wantCachedTokens:     40,
		},
		{
			name:                 "ZeroThinkingTokensUnchanged",
			promptTokens:         10,
			candidatesTokens:     20,
			thoughtsTokens:       0,
			totalTokens:          30,
			wantPromptTokens:     10,
			wantCompletionTokens: 20,
			wantTotalTokens:      30,
			wantReasoningTokens:  0,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			resp := &GenerateContentResponse{
				Candidates: []*Candidate{{
					Content:      &Content{Parts: []*Part{{Text: "result"}}},
					FinishReason: FinishReasonStop,
				}},
				UsageMetadata: &GenerateContentResponseUsageMetadata{
					PromptTokenCount:        tt.promptTokens,
					CandidatesTokenCount:    tt.candidatesTokens,
					ThoughtsTokenCount:      tt.thoughtsTokens,
					TotalTokenCount:         tt.totalTokens,
					CachedContentTokenCount: tt.cachedTokens,
				},
			}

			// Test direct body builder
			body := BatchResultItemFromGeminiResponse("line", schemas.BatchEndpointChatCompletions, resp, nil, false).Response.Body
			usage, ok := body["usage"].(map[string]interface{})
			require.True(t, ok)
			assert.EqualValues(t, tt.wantPromptTokens, usage["prompt_tokens"])
			assert.EqualValues(t, tt.wantCompletionTokens, usage["completion_tokens"])
			assert.EqualValues(t, tt.wantTotalTokens, usage["total_tokens"])

			if tt.wantReasoningTokens > 0 {
				details, ok := usage["completion_tokens_details"].(map[string]interface{})
				require.True(t, ok, "completion_tokens_details should be present")
				assert.EqualValues(t, tt.wantReasoningTokens, details["reasoning_tokens"])
			} else {
				assert.Nil(t, usage["completion_tokens_details"])
			}

			if tt.wantCachedTokens > 0 {
				details, ok := usage["prompt_tokens_details"].(map[string]interface{})
				require.True(t, ok, "prompt_tokens_details should be present")
				assert.EqualValues(t, tt.wantCachedTokens, details["cached_tokens"])
				assert.EqualValues(t, tt.wantCachedTokens, details["cached_read_tokens"], "the unified name beside OpenAI's")
			}

			// Test inline batch response path
			inline := GeminiInlinedResponse{
				Metadata: &GeminiBatchMetadata{Key: "custom-key"},
				Response: resp,
			}
			inlineItem := geminiInlineResponseToBatchResultItem(inline, "fallback")
			require.NotNil(t, inlineItem.Response)
			inlineUsage, ok := inlineItem.Response.Body["usage"].(map[string]interface{})
			require.True(t, ok)
			assert.EqualValues(t, tt.wantCompletionTokens, inlineUsage["completion_tokens"])
		})
	}

	t.Run("NilUsageMetadata", func(t *testing.T) {
		resp := &GenerateContentResponse{
			Candidates: []*Candidate{{FinishReason: FinishReasonStop}},
		}
		body := BatchResultItemFromGeminiResponse("line", "", resp, nil, false).Response.Body
		assert.Nil(t, body["usage"])
	})
}

// TestGeminiResourcePath verifies URI resource path formatting for Gemini models and batch operations.
func TestGeminiResourcePath(t *testing.T) {
	t.Parallel()

	valid := []struct{ id, collection, want string }{
		{"abc123", "batches", "batches/abc123"},
		{"batches/abc123", "batches", "batches/abc123"},
		{"files/abc", "files", "files/abc"},
		{"cachedContents/abc", "cachedContents", "cachedContents/abc"},
	}
	for _, tc := range valid {
		got, bifrostErr := geminiResourcePath(tc.id, tc.collection, "id")
		if bifrostErr != nil || got != tc.want {
			t.Fatalf("geminiResourcePath(%q) = %q, %v; want %q", tc.id, got, bifrostErr, tc.want)
		}
	}

	for _, id := range []string{"", "..", "../files/abc", "batches/../files/abc", "abc?x=1", "abc#", "abc%2Fdef", "files/abc"} {
		if got, bifrostErr := geminiResourcePath(id, "batches", "batch_id"); bifrostErr == nil {
			t.Fatalf("geminiResourcePath(%q) = %q, want error", id, got)
		}
	}
}

// A 200 that is not a batch object (no metadata) must become an error, not a nil dereference.
func TestGeminiBatchResponsesWithoutMetadataDoNotPanic(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/batches/no-metadata"):
			_, _ = w.Write([]byte(`{"files":[{"name":"files/abc"}]}`))
		case strings.HasSuffix(r.URL.Path, "/batches/no-stats"):
			_, _ = w.Write([]byte(`{"name":"batches/no-stats","metadata":{"name":"batches/no-stats","state":"BATCH_STATE_PENDING","createTime":"2026-07-01T00:00:00Z"}}`))
		default:
			_, _ = w.Write([]byte(`{"operations":[{"name":"batches/a"},{"name":"batches/b","metadata":{"state":"BATCH_STATE_SUCCEEDED","createTime":"2026-07-01T00:00:00Z"}}]}`))
		}
	}))
	defer ts.Close()

	provider := NewGeminiProvider(&schemas.ProviderConfig{
		NetworkConfig: schemas.NetworkConfig{BaseURL: ts.URL + "/v1beta"},
	}, testNoopLogger{})
	keys := []schemas.Key{{Value: *schemas.NewSecretVar("dummy-key")}}
	newCtx := func() *schemas.BifrostContext {
		return schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	}

	_, bifrostErr := provider.BatchRetrieve(newCtx(), keys, &schemas.BifrostBatchRetrieveRequest{Provider: schemas.Gemini, BatchID: "no-metadata"})
	require.NotNil(t, bifrostErr)
	assert.Contains(t, bifrostErr.Error.Message, "missing metadata")

	_, bifrostErr = provider.BatchResults(newCtx(), keys, &schemas.BifrostBatchResultsRequest{Provider: schemas.Gemini, BatchID: "no-metadata"})
	require.NotNil(t, bifrostErr)
	assert.Contains(t, bifrostErr.Error.Message, "missing metadata")

	retrieved, bifrostErr := provider.BatchRetrieve(newCtx(), keys, &schemas.BifrostBatchRetrieveRequest{Provider: schemas.Gemini, BatchID: "no-stats"})
	require.Nil(t, bifrostErr)
	assert.Equal(t, "batches/no-stats", retrieved.ID)
	assert.Equal(t, 0, retrieved.RequestCounts.Total)

	listed, bifrostErr := provider.BatchList(newCtx(), keys, &schemas.BifrostBatchListRequest{Provider: schemas.Gemini})
	require.Nil(t, bifrostErr)
	require.Len(t, listed.Data, 2)
	assert.Equal(t, "batches/a", listed.Data[0].ID)
	assert.Equal(t, schemas.BatchStatusCompleted, listed.Data[1].Status)
}

// geminiBatchVendor is a stand-in for the generativelanguage API: it records the body of every
// generateContent and batchGenerateContent call and serves one batch object and one file.
type geminiBatchVendor struct {
	*httptest.Server
	mu         sync.Mutex
	calls      []string
	syncBodies []string
	batchBody  string
	batch      string // the batch object GET /batches/{id} returns
	file       string // the responses file
	listBody   string
}

func newGeminiBatchVendor(t *testing.T) *geminiBatchVendor {
	t.Helper()
	v := &geminiBatchVendor{}
	v.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		v.mu.Lock()
		v.calls = append(v.calls, r.Method+" "+r.URL.Path)
		v.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, ":batchGenerateContent"):
			v.mu.Lock()
			v.batchBody = string(body)
			v.mu.Unlock()
			_, _ = io.WriteString(w, `{"name":"batches/abc123","metadata":{"@type":"type.googleapis.com/google.ai.generativelanguage.v1beta.GenerateContentBatch","name":"batches/abc123","displayName":"tokun-batch_tk_1","model":"models/gemini-2.5-flash","state":"BATCH_STATE_PENDING","createTime":"2026-11-10T08:00:00Z","updateTime":"2026-11-10T08:00:00Z","batchStats":{"requestCount":"2","pendingRequestCount":"2"}}}`)
		case strings.HasSuffix(r.URL.Path, ":generateContent"):
			v.mu.Lock()
			v.syncBodies = append(v.syncBodies, string(body))
			v.mu.Unlock()
			_, _ = io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"inlineData":{"mimeType":"image/png","data":"aW1hZ2U="}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`)
		case strings.Contains(r.URL.Path, "/download/"):
			if rangeHeader := r.Header.Get("Range"); rangeHeader != "" {
				start, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(rangeHeader, "bytes="), "-"))
				if err != nil || start > len(v.file) {
					w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
					return
				}
				w.WriteHeader(http.StatusPartialContent)
				_, _ = io.WriteString(w, v.file[start:])
				return
			}
			_, _ = io.WriteString(w, v.file)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/batches"):
			_, _ = io.WriteString(w, v.listBody)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/batches/"):
			_, _ = io.WriteString(w, v.batch)
		default:
			t.Errorf("unexpected upstream call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(v.Close)
	return v
}

func (v *geminiBatchVendor) provider(sendBackRawResponse bool) (*GeminiProvider, []schemas.Key) {
	provider := NewGeminiProvider(&schemas.ProviderConfig{
		NetworkConfig:       schemas.NetworkConfig{BaseURL: v.URL + "/v1beta", DefaultRequestTimeoutInSeconds: 30},
		SendBackRawResponse: sendBackRawResponse,
	}, testNoopLogger{})
	return provider, []schemas.Key{{ID: "key-1", Name: "tk-key-1", Value: *schemas.NewSecretVar("dummy-key")}}
}

func batchLineFromJSON(t *testing.T, line string) schemas.BatchRequestItem {
	t.Helper()
	var item schemas.BatchRequestItem
	require.NoError(t, sonic.UnmarshalString(line, &item))
	return item
}

// The same OpenAI-shape body reaches Gemini with identical parameters by the sync route and as
// an inline batch line (contract C8): the stand-in vendor records what each route sent, and
// the batch line's request equals the sync request, byte for byte as JSON. Before this, the
// batch path kept only messages and dropped max_tokens, temperature, response_format and
// OpenAI-shape tools.
func TestGeminiBatchCreateInlineLinesMatchTheSyncRoute(t *testing.T) {
	// A tools line and a structured-output line: the sync converter does not send a JSON
	// response format beside function declarations on models that cannot combine them, so
	// the two are checked on separate lines.
	const chatLine = `{"custom_id":"line-1","method":"POST","url":"/v1/chat/completions","body":{"model":"gemini-2.5-flash","messages":[{"role":"system","content":"Be brief."},{"role":"user","content":"What is the capital of France?"}],"max_tokens":256,"temperature":0.2,"top_p":0.9,"stop":["END"],"presence_penalty":0.1,"tools":[{"type":"function","function":{"name":"lookup","description":"Look a fact up","parameters":{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}}}],"tool_choice":"auto","vendor_only_switch":true}}`
	const structuredLine = `{"custom_id":"line-2","method":"POST","url":"/v1/chat/completions","body":{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"Name a city."}],"max_completion_tokens":64,"response_format":{"type":"json_schema","json_schema":{"name":"answer","schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}}}`
	const imageLine = `{"custom_id":"img-1","method":"POST","url":"/v1/images/generations","body":{"model":"gemini-2.5-flash-image","prompt":"A lighthouse at dusk","n":1,"aspect_ratio":"16:9","seed":3}}`

	for _, passthrough := range []bool{false, true} {
		t.Run(fmt.Sprintf("chat line, extra params passthrough=%v", passthrough), func(t *testing.T) {
			vendor := newGeminiBatchVendor(t)
			provider, keys := vendor.provider(false)
			newCtx := func() *schemas.BifrostContext {
				ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
				if passthrough {
					ctx.SetValue(schemas.BifrostContextKeyPassthroughExtraParams, true)
				}
				return ctx
			}
			item := batchLineFromJSON(t, chatLine)

			// The sync route: the request the transport builds from this body, sent as chat.
			chatRequest, err := item.ToBifrostChatRequest(schemas.Gemini, "gemini-2.5-flash")
			require.NoError(t, err)
			_, bifrostErr := provider.ChatCompletion(newCtx(), keys[0], chatRequest)
			require.Nil(t, bifrostErr)

			// The batch route: the same body as an inline line.
			resp, bifrostErr := provider.BatchCreate(newCtx(), keys[0], &schemas.BifrostBatchCreateRequest{
				Provider: schemas.Gemini,
				Model:    schemas.Ptr("gemini-2.5-flash"),
				Endpoint: schemas.BatchEndpointChatCompletions,
				Requests: []schemas.BatchRequestItem{item},
			})
			require.Nil(t, bifrostErr)
			assert.Equal(t, "batches/abc123", resp.ID, "the batch id is the vendor's, unchanged")
			assert.Equal(t, schemas.BatchStatusInProgress, resp.Status)
			require.NotNil(t, resp.DisplayName)
			assert.Equal(t, "tokun-batch_tk_1", *resp.DisplayName)

			assert.Equal(t, []string{"POST /v1beta/models/gemini-2.5-flash:generateContent", "POST /v1beta/models/gemini-2.5-flash:batchGenerateContent"}, vendor.calls)
			require.Len(t, vendor.syncBodies, 1)

			var batch struct {
				Batch struct {
					InputConfig struct {
						Requests struct {
							Requests []struct {
								Request  sonic.NoCopyRawMessage `json:"request"`
								Metadata map[string]interface{} `json:"metadata"`
							} `json:"requests"`
						} `json:"requests"`
					} `json:"inputConfig"`
				} `json:"batch"`
			}
			require.NoError(t, sonic.UnmarshalString(vendor.batchBody, &batch))
			lines := batch.Batch.InputConfig.Requests.Requests
			require.Len(t, lines, 1)
			assert.JSONEq(t, vendor.syncBodies[0], string(lines[0].Request), "the batch line and the sync request must be the same request")
			assert.Equal(t, map[string]interface{}{"key": "line-1", "bifrost_endpoint": "/v1/chat/completions"}, lines[0].Metadata)

			// Every parameter the old conversion dropped is in the line.
			var sent map[string]interface{}
			require.NoError(t, sonic.Unmarshal(lines[0].Request, &sent))
			generationConfig, ok := sent["generationConfig"].(map[string]interface{})
			require.True(t, ok, "generationConfig missing: %s", lines[0].Request)
			assert.EqualValues(t, 256, generationConfig["maxOutputTokens"])
			assert.EqualValues(t, 0.2, generationConfig["temperature"])
			assert.EqualValues(t, 0.9, generationConfig["topP"])
			assert.Equal(t, []interface{}{"END"}, generationConfig["stopSequences"])
			assert.EqualValues(t, 0.1, generationConfig["presencePenalty"])
			tools, ok := sent["tools"].([]interface{})
			require.True(t, ok, "tools missing: %s", lines[0].Request)
			declarations := tools[0].(map[string]interface{})["functionDeclarations"].([]interface{})
			assert.Equal(t, "lookup", declarations[0].(map[string]interface{})["name"])
			assert.NotNil(t, sent["systemInstruction"])
			assert.NotNil(t, sent["contents"])
			// A field outside the unified structure is merged only when the caller asked for it,
			// on both routes alike.
			_, merged := sent["vendor_only_switch"]
			assert.Equal(t, passthrough, merged)
		})
	}

	t.Run("structured output line", func(t *testing.T) {
		vendor := newGeminiBatchVendor(t)
		provider, keys := vendor.provider(false)
		item := batchLineFromJSON(t, structuredLine)

		chatRequest, err := item.ToBifrostChatRequest(schemas.Gemini, "gemini-2.5-flash")
		require.NoError(t, err)
		_, bifrostErr := provider.ChatCompletion(schemas.NewBifrostContext(context.Background(), schemas.NoDeadline), keys[0], chatRequest)
		require.Nil(t, bifrostErr)
		_, bifrostErr = provider.BatchCreate(schemas.NewBifrostContext(context.Background(), schemas.NoDeadline), keys[0], &schemas.BifrostBatchCreateRequest{
			Provider: schemas.Gemini,
			Model:    schemas.Ptr("gemini-2.5-flash"),
			Endpoint: schemas.BatchEndpointChatCompletions,
			Requests: []schemas.BatchRequestItem{item},
		})
		require.Nil(t, bifrostErr)

		lineRequest := gjsonGet(t, vendor.batchBody, "batch.inputConfig.requests.requests.0.request")
		assert.JSONEq(t, vendor.syncBodies[0], lineRequest)
		assert.EqualValues(t, 64, gjson.Get(lineRequest, "generationConfig.maxOutputTokens").Int())
		assert.Equal(t, "application/json", gjson.Get(lineRequest, "generationConfig.responseMimeType").String())
		assert.True(t, gjson.Get(lineRequest, "generationConfig.responseSchema").Exists() || gjson.Get(lineRequest, "generationConfig.responseJsonSchema").Exists(), "the response schema is missing: %s", lineRequest)
	})

	t.Run("image generation line", func(t *testing.T) {
		vendor := newGeminiBatchVendor(t)
		provider, keys := vendor.provider(false)
		item := batchLineFromJSON(t, imageLine)

		imageRequest, err := item.ToBifrostImageGenerationRequest(schemas.Gemini, "gemini-2.5-flash-image")
		require.NoError(t, err)
		// The sync image route always merges extra params.
		syncCtx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
		syncCtx.SetValue(schemas.BifrostContextKeyPassthroughExtraParams, true)
		_, bifrostErr := provider.ImageGeneration(syncCtx, keys[0], imageRequest)
		require.Nil(t, bifrostErr)

		_, bifrostErr = provider.BatchCreate(schemas.NewBifrostContext(context.Background(), schemas.NoDeadline), keys[0], &schemas.BifrostBatchCreateRequest{
			Provider: schemas.Gemini,
			Model:    schemas.Ptr("gemini-2.5-flash-image"),
			Endpoint: schemas.BatchEndpointImageGeneration,
			Requests: []schemas.BatchRequestItem{item},
		})
		require.Nil(t, bifrostErr)
		require.Len(t, vendor.syncBodies, 1)

		lineRequest := gjsonGet(t, vendor.batchBody, "batch.inputConfig.requests.requests.0.request")
		assert.JSONEq(t, vendor.syncBodies[0], lineRequest)
		assert.JSONEq(t, `{"key":"img-1","bifrost_endpoint":"/v1/images/generations"}`, gjsonGet(t, vendor.batchBody, "batch.inputConfig.requests.requests.0.metadata"))
	})

	t.Run("a body already in Gemini shape is sent as it is", func(t *testing.T) {
		vendor := newGeminiBatchVendor(t)
		provider, keys := vendor.provider(false)
		item := batchLineFromJSON(t, `{"custom_id":"native-1","body":{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":32}}}`)
		_, bifrostErr := provider.BatchCreate(schemas.NewBifrostContext(context.Background(), schemas.NoDeadline), keys[0], &schemas.BifrostBatchCreateRequest{
			Provider: schemas.Gemini,
			Model:    schemas.Ptr("gemini-2.5-flash"),
			Requests: []schemas.BatchRequestItem{item},
		})
		require.Nil(t, bifrostErr)
		assert.JSONEq(t, `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":32}}`, gjsonGet(t, vendor.batchBody, "batch.inputConfig.requests.requests.0.request"))
		assert.JSONEq(t, `{"key":"native-1"}`, gjsonGet(t, vendor.batchBody, "batch.inputConfig.requests.requests.0.metadata"))
	})
}

func gjsonGet(t *testing.T, document, path string) string {
	t.Helper()
	result := gjson.Get(document, path)
	require.True(t, result.Exists(), "%s is missing in %s", path, document)
	return result.Raw
}

// A batch the caller got wrong is a 400 before anything is sent: no model (never a default
// model), a url batchGenerateContent cannot run, a line naming another model.
func TestGeminiBatchCreateRefusesWhatItCannotRun(t *testing.T) {
	chat := `{"custom_id":"line-1","url":"/v1/chat/completions","body":{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"hi"}]}}`
	cases := []struct {
		name      string
		model     *string
		line      string
		wantParam string
		wantText  string
	}{
		{"no model", nil, chat, "model", "model is required"},
		{"empty model", schemas.Ptr(""), chat, "model", "model is required"},
		{"embeddings url", schemas.Ptr("gemini-2.5-flash"), `{"custom_id":"e-1","url":"/v1/embeddings","body":{"model":"gemini-2.5-flash","input":"hi"}}`, "requests[0].url", "/v1/embeddings"},
		{"line names another model", schemas.Ptr("gemini-2.5-flash"), `{"custom_id":"line-1","url":"/v1/chat/completions","body":{"model":"gemini-2.5-pro","messages":[{"role":"user","content":"hi"}]}}`, "requests[0].body.model", "gemini-2.5-pro"},
		{"chat line without messages", schemas.Ptr("gemini-2.5-flash"), `{"custom_id":"line-1","url":"/v1/chat/completions","body":{"model":"gemini-2.5-flash"}}`, "requests[0].body", "messages is required"},
		{"imagen image line", schemas.Ptr("imagen-4.0-generate-001"), `{"custom_id":"img-1","url":"/v1/images/generations","body":{"model":"imagen-4.0-generate-001","prompt":"a cat"}}`, "requests[0].url", "Imagen"},
		{"line without a body", schemas.Ptr("gemini-2.5-flash"), `{"custom_id":"line-1","url":"/v1/chat/completions"}`, "requests[0].body", "body is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vendor := newGeminiBatchVendor(t)
			provider, keys := vendor.provider(false)
			_, bifrostErr := provider.BatchCreate(schemas.NewBifrostContext(context.Background(), schemas.NoDeadline), keys[0], &schemas.BifrostBatchCreateRequest{
				Provider: schemas.Gemini,
				Model:    tc.model,
				Requests: []schemas.BatchRequestItem{batchLineFromJSON(t, tc.line)},
			})
			require.NotNil(t, bifrostErr)
			require.NotNil(t, bifrostErr.StatusCode, "error = %+v", bifrostErr.Error)
			assert.Equal(t, http.StatusBadRequest, *bifrostErr.StatusCode)
			assert.Equal(t, tc.wantParam, bifrostErr.Error.Param)
			assert.Contains(t, bifrostErr.Error.Message, tc.wantText)
			assert.Empty(t, vendor.calls, "nothing may be sent upstream")
		})
	}
}

// Recorded inline responses of a finished batch: a chat line with thinking and cached tokens,
// an image line with its inlineData artifact and per-modality usage, and a line the vendor
// failed. REST reports the state here as JOB_STATE_SUCCEEDED.
const geminiFinishedInlineBatch = `{"name":"batches/abc123","done":true,"metadata":{"@type":"type.googleapis.com/google.ai.generativelanguage.v1beta.GenerateContentBatch","name":"batches/abc123","displayName":"tokun-batch_tk_1","model":"models/gemini-2.5-flash-image","state":"JOB_STATE_SUCCEEDED","createTime":"2026-11-10T08:00:00Z","endTime":"2026-11-10T09:30:00Z","updateTime":"2026-11-10T09:30:00Z","batchStats":{"requestCount":"3","successfulRequestCount":"2"}},"response":{"@type":"type.googleapis.com/google.ai.generativelanguage.v1beta.GenerateContentBatchOutput","inlinedResponses":{"inlinedResponses":[
{"metadata":{"key":"line-1","bifrost_endpoint":"/v1/chat/completions"},"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"Paris."}]},"finishReason":"STOP","index":0,"safetyRatings":[{"category":"HARM_CATEGORY_DANGEROUS_CONTENT","probability":"NEGLIGIBLE"}]}],"usageMetadata":{"promptTokenCount":120,"candidatesTokenCount":60,"totalTokenCount":480,"cachedContentTokenCount":40,"thoughtsTokenCount":300,"promptTokensDetails":[{"modality":"TEXT","tokenCount":120}],"candidatesTokensDetails":[{"modality":"TEXT","tokenCount":60}]},"modelVersion":"gemini-2.5-flash","responseId":"resp-chat-1"}},
{"metadata":{"key":"img-1","bifrost_endpoint":"/v1/images/generations"},"response":{"candidates":[{"content":{"role":"model","parts":[{"inlineData":{"mimeType":"image/png","data":"iVBORw0KGgoAAAANSUhEUg=="}}]},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":1290,"totalTokenCount":1298,"promptTokensDetails":[{"modality":"TEXT","tokenCount":8}],"candidatesTokensDetails":[{"modality":"IMAGE","tokenCount":1290}]},"modelVersion":"gemini-2.5-flash-image","responseId":"resp-img-1"}},
{"metadata":{"key":"line-3","bifrost_endpoint":"/v1/chat/completions"},"error":{"code":400,"message":"Request contains an invalid argument.","status":"INVALID_ARGUMENT"}}
]}}}`

// Results come back one line per custom_id in the OpenAI shape of the line's url, with status
// and the sync operation's unified usage; an image line keeps its artifact; the native
// response is in raw_response when the caller asks for it (contract C8, BF18).
func TestGeminiBatchResultsReturnOpenAIShapeLines(t *testing.T) {
	vendor := newGeminiBatchVendor(t)
	vendor.batch = geminiFinishedInlineBatch
	provider, keys := vendor.provider(true)
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)

	resp, bifrostErr := provider.BatchResults(ctx, keys, &schemas.BifrostBatchResultsRequest{Provider: schemas.Gemini, BatchID: "batches/abc123"})
	require.Nil(t, bifrostErr, "a JOB_STATE_SUCCEEDED batch is finished")
	require.Len(t, resp.Results, 3)
	assert.False(t, resp.HasMore)
	assert.Equal(t, []string{"GET /v1beta/batches/abc123"}, vendor.calls, "results are one upstream call")

	// The chat line is the sync chat response of the same native response.
	chat := resp.Results[0]
	assert.Equal(t, "line-1", chat.CustomID)
	assert.Equal(t, schemas.BatchResultStatusSucceeded, chat.Status)
	assert.Equal(t, 200, chat.Response.StatusCode)
	assert.Equal(t, "chat.completion", chat.Response.Body["object"])
	assert.Equal(t, "gemini-2.5-flash", chat.Response.Body["model"])
	assert.Equal(t, "Paris.", batchLineChoice(t, chat)["message"].(map[string]interface{})["content"])
	usage := chat.Response.Body["usage"].(map[string]interface{})
	assert.EqualValues(t, 120, usage["prompt_tokens"])
	assert.EqualValues(t, 360, usage["completion_tokens"], "candidates plus thoughts, as on the sync route")
	assert.EqualValues(t, 480, usage["total_tokens"])
	assert.EqualValues(t, 300, usage["completion_tokens_details"].(map[string]interface{})["reasoning_tokens"])
	assert.EqualValues(t, 60, usage["completion_tokens_details"].(map[string]interface{})["text_tokens"])
	assert.EqualValues(t, 40, usage["prompt_tokens_details"].(map[string]interface{})["cached_read_tokens"])
	assert.EqualValues(t, 120, usage["prompt_tokens_details"].(map[string]interface{})["text_tokens"])
	raw := chat.Response.Body["extra_fields"].(map[string]interface{})["raw_response"].(map[string]interface{})
	assert.NotNil(t, raw["candidates"].([]interface{})[0].(map[string]interface{})["safetyRatings"], "the native response keeps safetyRatings")

	// The image line is an image generation response with the artifact and per-modality usage.
	image := resp.Results[1]
	assert.Equal(t, "img-1", image.CustomID)
	assert.Equal(t, schemas.BatchResultStatusSucceeded, image.Status)
	data, ok := image.Response.Body["data"].([]interface{})
	require.True(t, ok, "an image line has data[]: %v", image.Response.Body)
	require.Len(t, data, 1)
	assert.Equal(t, "iVBORw0KGgoAAAANSUhEUg==", data[0].(map[string]interface{})["b64_json"])
	imageUsage := image.Response.Body["usage"].(map[string]interface{})
	assert.EqualValues(t, 8, imageUsage["input_tokens"])
	assert.EqualValues(t, 1290, imageUsage["output_tokens"])
	assert.EqualValues(t, 1290, imageUsage["output_tokens_details"].(map[string]interface{})["image_tokens"])
	assert.EqualValues(t, 8, imageUsage["input_tokens_details"].(map[string]interface{})["text_tokens"])
	_, isChat := image.Response.Body["choices"]
	assert.False(t, isChat, "an image line must not be a chat completion")

	// The failed line carries the vendor's error.
	failed := resp.Results[2]
	assert.Equal(t, "line-3", failed.CustomID)
	assert.Equal(t, schemas.BatchResultStatusFailed, failed.Status)
	assert.Nil(t, failed.Response)
	assert.Equal(t, "400", failed.Error.Code)
	assert.Equal(t, "Request contains an invalid argument.", failed.Error.Message)

	// The batch object: the display name, the terminal phase time, JOB_STATE_* read as completed.
	retrieved, bifrostErr := provider.BatchRetrieve(ctx, keys, &schemas.BifrostBatchRetrieveRequest{Provider: schemas.Gemini, BatchID: "batches/abc123"})
	require.Nil(t, bifrostErr)
	assert.Equal(t, "batches/abc123", retrieved.ID)
	assert.Equal(t, schemas.BatchStatusCompleted, retrieved.Status)
	require.NotNil(t, retrieved.Done)
	assert.True(t, *retrieved.Done)
	require.NotNil(t, retrieved.DisplayName)
	assert.Equal(t, "tokun-batch_tk_1", *retrieved.DisplayName)
	require.NotNil(t, retrieved.CompletedAt)
	assert.EqualValues(t, 1794303000, *retrieved.CompletedAt)
	assert.Equal(t, 3, retrieved.RequestCounts.Total)

	t.Run("pages over the inline responses", func(t *testing.T) {
		var got []string
		var after *string
		for page := 0; ; page++ {
			require.Less(t, page, 10, "paging did not terminate")
			pageResp, pageErr := provider.BatchResults(ctx, keys, &schemas.BifrostBatchResultsRequest{Provider: schemas.Gemini, BatchID: "batches/abc123", Limit: 2, After: after})
			require.Nil(t, pageErr)
			require.LessOrEqual(t, len(pageResp.Results), 2)
			for _, item := range pageResp.Results {
				got = append(got, item.CustomID+":"+string(item.Status))
			}
			if !pageResp.HasMore {
				break
			}
			after = pageResp.NextCursor
		}
		assert.Equal(t, []string{"line-1:succeeded", "img-1:succeeded", "line-3:failed"}, got)
	})

	t.Run("without the raw-response switch the line has no extra_fields", func(t *testing.T) {
		quiet, quietKeys := vendor.provider(false)
		quietResp, quietErr := quiet.BatchResults(ctx, quietKeys, &schemas.BifrostBatchResultsRequest{Provider: schemas.Gemini, BatchID: "batches/abc123"})
		require.Nil(t, quietErr)
		_, hasExtraFields := quietResp.Results[0].Response.Body["extra_fields"]
		assert.False(t, hasExtraFields)
	})

	t.Run("a finished batch with no output has no invented line", func(t *testing.T) {
		empty := newGeminiBatchVendor(t)
		empty.batch = `{"name":"batches/empty","done":true,"metadata":{"name":"batches/empty","state":"BATCH_STATE_FAILED","createTime":"2026-11-10T08:00:00Z","batchStats":{"requestCount":"2"}}}`
		emptyProvider, emptyKeys := empty.provider(false)
		emptyResp, emptyErr := emptyProvider.BatchResults(ctx, emptyKeys, &schemas.BifrostBatchResultsRequest{Provider: schemas.Gemini, BatchID: "batches/empty"})
		require.Nil(t, emptyErr)
		assert.Empty(t, emptyResp.Results, "a line with custom_id \"info\" would be settled as a customer's line")
	})

	t.Run("a batch still running has no results, whichever state spelling", func(t *testing.T) {
		for _, state := range []string{"BATCH_STATE_RUNNING", "JOB_STATE_RUNNING", "JOB_STATE_PENDING", "JOB_STATE_QUEUED"} {
			running := newGeminiBatchVendor(t)
			running.batch = `{"name":"batches/run","metadata":{"name":"batches/run","state":"` + state + `","createTime":"2026-11-10T08:00:00Z","batchStats":{"requestCount":"2","pendingRequestCount":"2"}}}`
			runningProvider, runningKeys := running.provider(false)
			_, runningErr := runningProvider.BatchResults(ctx, runningKeys, &schemas.BifrostBatchResultsRequest{Provider: schemas.Gemini, BatchID: "batches/run"})
			require.NotNil(t, runningErr, state)
			assert.Contains(t, runningErr.Error.Message, "still processing")
		}
	})
}

// A file-based batch's responses file is read as a stream and paged by cursor.
func TestGeminiBatchResultsPageTheResponsesFile(t *testing.T) {
	vendor := newGeminiBatchVendor(t)
	vendor.batch = `{"name":"batches/file1","done":true,"metadata":{"name":"batches/file1","state":"BATCH_STATE_SUCCEEDED","createTime":"2026-11-10T08:00:00Z","batchStats":{"requestCount":"5","successfulRequestCount":"4"}},"response":{"responsesFile":"files/batch-out-1"}}`
	var file strings.Builder
	for i := range 4 {
		fmt.Fprintf(&file, `{"key":"k-%d","response":{"candidates":[{"content":{"role":"model","parts":[{"text":"answer %d"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":2,"totalTokenCount":7},"modelVersion":"gemini-2.5-flash"}}`+"\n", i, i)
	}
	file.WriteString(`{"key":"k-4","error":{"code":500,"message":"Internal error","status":"INTERNAL"}}` + "\n")
	vendor.file = file.String()
	provider, keys := vendor.provider(false)
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)

	var got []string
	var after *string
	for page := 0; ; page++ {
		require.Less(t, page, 10, "paging did not terminate")
		resp, bifrostErr := provider.BatchResults(ctx, keys, &schemas.BifrostBatchResultsRequest{Provider: schemas.Gemini, BatchID: "batches/file1", Limit: 2, After: after})
		require.Nil(t, bifrostErr)
		require.LessOrEqual(t, len(resp.Results), 2)
		for _, item := range resp.Results {
			got = append(got, item.CustomID+":"+string(item.Status))
			if item.Status == schemas.BatchResultStatusSucceeded {
				assert.Equal(t, "chat.completion", item.Response.Body["object"])
				assert.EqualValues(t, 7, item.Response.Body["usage"].(map[string]interface{})["total_tokens"])
			}
		}
		if !resp.HasMore {
			break
		}
		after = resp.NextCursor
	}
	assert.Equal(t, []string{"k-0:succeeded", "k-1:succeeded", "k-2:succeeded", "k-3:succeeded", "k-4:failed"}, got)
}

// The list returns each batch's display name, which is what a caller finds a batch by after a
// lost create response (Gemini has no batch metadata), and reads either state spelling.
func TestGeminiBatchListReturnsDisplayNameAndBothStateSpellings(t *testing.T) {
	vendor := newGeminiBatchVendor(t)
	vendor.listBody = `{"operations":[{"name":"batches/a","metadata":{"name":"batches/a","displayName":"tokun-batch_tk_1","state":"BATCH_STATE_RUNNING","createTime":"2026-11-10T08:00:00Z"}},{"name":"batches/b","metadata":{"name":"batches/b","displayName":"tokun-batch_tk_2","state":"JOB_STATE_CANCELLED","createTime":"2026-11-10T08:05:00Z"}},{"name":"batches/c"}]}`
	provider, keys := vendor.provider(false)
	listed, bifrostErr := provider.BatchList(schemas.NewBifrostContext(context.Background(), schemas.NoDeadline), keys, &schemas.BifrostBatchListRequest{Provider: schemas.Gemini})
	require.Nil(t, bifrostErr)
	require.Len(t, listed.Data, 3)
	require.NotNil(t, listed.Data[0].DisplayName)
	assert.Equal(t, "tokun-batch_tk_1", *listed.Data[0].DisplayName)
	assert.Equal(t, schemas.BatchStatusInProgress, listed.Data[0].Status)
	require.NotNil(t, listed.Data[1].DisplayName)
	assert.Equal(t, "tokun-batch_tk_2", *listed.Data[1].DisplayName)
	assert.Equal(t, schemas.BatchStatusCancelled, listed.Data[1].Status)
	assert.Nil(t, listed.Data[2].DisplayName)

	for state, want := range map[string]schemas.BatchStatus{
		"BATCH_STATE_PENDING":           schemas.BatchStatusInProgress,
		"JOB_STATE_PENDING":             schemas.BatchStatusInProgress,
		"JOB_STATE_QUEUED":              schemas.BatchStatusInProgress,
		"JOB_STATE_RUNNING":             schemas.BatchStatusInProgress,
		"BATCH_STATE_SUCCEEDED":         schemas.BatchStatusCompleted,
		"JOB_STATE_SUCCEEDED":           schemas.BatchStatusCompleted,
		"JOB_STATE_PARTIALLY_SUCCEEDED": schemas.BatchStatusCompleted,
		"JOB_STATE_FAILED":              schemas.BatchStatusFailed,
		"JOB_STATE_CANCELLING":          schemas.BatchStatusCancelling,
		"JOB_STATE_CANCELLED":           schemas.BatchStatusCancelled,
		"JOB_STATE_EXPIRED":             schemas.BatchStatusExpired,
		"JOB_STATE_PAUSED":              schemas.BatchStatus("JOB_STATE_PAUSED"),
	} {
		assert.Equal(t, want, ToBifrostBatchStatus(state), state)
	}
}
