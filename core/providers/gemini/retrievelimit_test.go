package gemini

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/capsohq/bifrost/core/network"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// geminiInlineBatchJob is a finished inline batch job of about size bytes, in the shape the
// generativelanguage API returns: every result inline under metadata.output.
func geminiInlineBatchJob(size int) []byte {
	var b strings.Builder
	b.Grow(size + 1024)
	b.WriteString(`{"name":"batches/big","metadata":{"@type":"type.googleapis.com/google.ai.generativelanguage.v1main.GenerateContentBatch","name":"batches/big","model":"models/gemini-2.5-flash","displayName":"big","state":"BATCH_STATE_SUCCEEDED","createTime":"2026-11-01T00:00:00Z","endTime":"2026-11-01T01:00:00Z","batchStats":{"requestCount":"0","successfulRequestCount":"0"},"output":{"inlinedResponses":{"inlinedResponses":[`)
	text := strings.Repeat("lorem ipsum ", 80)
	for i := 0; b.Len() < size; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"response":{"candidates":[{"content":{"parts":[{"text":%q}],"role":"model"},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":200,"totalTokenCount":203}},"metadata":{"key":"k%d"}}`, text, i)
	}
	b.WriteString(`]}}},"done":true,"response":{"@type":"type.googleapis.com/google.ai.generativelanguage.v1main.GenerateContentBatchOutput"}}`)
	return []byte(b.String())
}

func newRetrieveCtx() *schemas.BifrostContext {
	ctx, _ := schemas.NewBifrostContextWithCancel(context.Background())
	// What core stamps on every retrieve (markRetrieve).
	ctx.SetValue(schemas.BifrostContextKeyRetrieveRequest, true)
	return ctx
}

// TXE-8031: a Gemini batch job past the retrieve limit is refused naming that limit, and the
// walk over keys stops at the refusal, so the job is downloaded once, not once per key. Before,
// the job was read under the whole response limit and every key downloaded it again (the batch
// retrieve loop named its error lastError, which TestEveryKeyWalkStopsAtAResponseLimitRefusal
// did not see).
func TestGeminiBatchRetrievePastTheRetrieveLimitIsRefusedOnce(t *testing.T) {
	network.SetMaxResponseBodySizeMB(4) // retrieve limit 1 MiB
	t.Cleanup(func() { network.SetMaxResponseBodySizeMB(0) })

	job := geminiInlineBatchJob(2 << 20)
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("x-goog-api-key") != "first" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"code":403,"message":"denied","status":"PERMISSION_DENIED"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(job)
	}))
	defer ts.Close()

	provider := NewGeminiProvider(&schemas.ProviderConfig{NetworkConfig: schemas.NetworkConfig{BaseURL: ts.URL + "/v1beta"}}, testNoopLogger{})
	keys := []schemas.Key{{Value: *schemas.NewSecretVar("first")}, {Value: *schemas.NewSecretVar("second")}}
	_, bifrostErr := provider.BatchRetrieve(newRetrieveCtx(), keys, &schemas.BifrostBatchRetrieveRequest{Provider: schemas.Gemini, BatchID: "big"})
	require.NotNil(t, bifrostErr)
	assert.Equal(t, schemas.ErrorTypeBifrostInternal, bifrostErr.ExtraFields.ErrorType, "the refusal, not the second key's 403: %+v", bifrostErr.Error)
	assert.Contains(t, bifrostErr.Error.Message, "limit 1 MiB, for a retrieve")
	assert.Equal(t, int32(1), calls.Load(), "the job is downloaded once")
}

// TXE-8031: reading and converting a Gemini batch retrieve for the /genai route allocates at
// most network.RetrieveResponseShare bytes per byte of the job, so a retrieve at its limit (a
// RetrieveResponseShare-th of client.max_response_body_size_mb) allocates no more than the
// response limit. The job is just over 8 MiB, the worst case for the read buffer, whose capacity
// fasthttp rounds up to the next power of two. Measured 3.3: the read buffer (2), the route's body
// with the vendor's output (1) and the decoders' and encoders' own buffers. Before, decoding every
// inline result into Go values cost about twenty times the job, and copying the body, copying its
// output and encoding the route's body again cost four more.
func TestGeminiBatchRetrieveAllocatesWithinTheRetrieveShare(t *testing.T) {
	job := geminiInlineBatchJob(8 << 20)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", fmt.Sprint(len(job)))
		_, _ = w.Write(job)
	}))
	defer ts.Close()
	provider := NewGeminiProvider(&schemas.ProviderConfig{NetworkConfig: schemas.NetworkConfig{BaseURL: ts.URL + "/v1beta"}}, testNoopLogger{})
	keys := []schemas.Key{{Value: *schemas.NewSecretVar("k")}}

	retrieve := func() int {
		resp, bifrostErr := provider.BatchRetrieve(newRetrieveCtx(), keys, &schemas.BifrostBatchRetrieveRequest{Provider: schemas.Gemini, BatchID: "big"})
		require.Nil(t, bifrostErr)
		body, err := ToGeminiBatchRetrieveBody(resp)
		require.NoError(t, err)
		// What the router writes (sendSuccess): a json.RawMessage as it is (SetBodyRaw), anything
		// else encoded and copied into the response buffer.
		if raw, ok := body.(json.RawMessage); ok && raw != nil {
			return len(raw)
		}
		out, err := sonic.Marshal(body)
		require.NoError(t, err)
		return len(append([]byte(nil), out...))
	}
	n := retrieve() // warms the client's pooled read buffer, as a running gateway's is
	require.Greater(t, n, len(job)-1024, "the route returns the inline results")

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	retrieve()
	runtime.ReadMemStats(&after)
	perByte := float64(after.TotalAlloc-before.TotalAlloc) / float64(len(job))
	t.Logf("%.2f bytes allocated per job byte (%d MiB job)", perByte, len(job)>>20)
	assert.LessOrEqual(t, perByte, float64(network.RetrieveResponseShare),
		"a retrieve at its limit would hold more than client.max_response_body_size_mb")
}
