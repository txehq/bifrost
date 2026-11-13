package bedrock

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// normalizeCachedUsage must fold cached read/write tokens into PromptTokens only.
// Bedrock's stream reports TotalTokens directly, so the total must NOT be
// re-summed here. This is the value billed when a stream is cancelled mid-flight.
func TestNormalizeCachedUsage_FoldsCacheIntoPromptOnly(t *testing.T) {
	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     10,
		CompletionTokens: 5,
		TotalTokens:      18, // provider-reported, already includes cache
		PromptTokensDetails: &schemas.ChatPromptTokensDetails{
			CachedReadTokens:  3,
			CachedWriteTokens: 7,
		},
	}
	normalizeCachedUsage(usage)
	if usage.PromptTokens != 20 {
		t.Fatalf("PromptTokens = %d, want 20 (10 + 3 + 7)", usage.PromptTokens)
	}
	if usage.TotalTokens != 18 {
		t.Fatalf("TotalTokens = %d, want 18 (provider-reported, unchanged)", usage.TotalTokens)
	}
}

func TestNormalizeCachedUsage_NoCacheDetailsIsNoOp(t *testing.T) {
	usage := &schemas.BifrostLLMUsage{PromptTokens: 10, TotalTokens: 15}
	normalizeCachedUsage(usage)
	if usage.PromptTokens != 10 || usage.TotalTokens != 15 {
		t.Fatalf("unexpected mutation: prompt=%d total=%d", usage.PromptTokens, usage.TotalTokens)
	}
}

func TestNormalizeCachedUsage_NilSafe(t *testing.T) {
	normalizeCachedUsage(nil) // must not panic
}

func TestAccumulateBedrockResponsesUsage_MirrorsCacheIntoBilledUsage(t *testing.T) {
	responseUsage := &schemas.ResponsesResponseUsage{}
	billedUsage := &schemas.BifrostLLMUsage{}
	cacheDetails := []BedrockCacheWriteDetails{
		{TTL: BedrockCacheWriteTTL1h, InputTokens: 700},
	}
	upstreamUsage := &BedrockTokenUsage{
		InputTokens:           10,
		OutputTokens:          5,
		TotalTokens:           1515,
		CacheReadInputTokens:  800,
		CacheWriteInputTokens: 700,
		CacheDetails:          &cacheDetails,
	}

	accumulateBedrockResponsesUsage(responseUsage, billedUsage, upstreamUsage)

	if responseUsage.InputTokens != 10 || responseUsage.OutputTokens != 5 || responseUsage.TotalTokens != 1515 {
		t.Fatalf("unexpected response usage: %+v", responseUsage)
	}
	if responseUsage.InputTokensDetails == nil {
		t.Fatal("expected response cache details")
	}
	if responseUsage.InputTokensDetails.CachedReadTokens != 800 {
		t.Fatalf("response cached read = %d, want 800", responseUsage.InputTokensDetails.CachedReadTokens)
	}
	if responseUsage.InputTokensDetails.CachedWriteTokens != 700 {
		t.Fatalf("response cached write = %d, want 700", responseUsage.InputTokensDetails.CachedWriteTokens)
	}
	if responseUsage.InputTokensDetails.CachedWriteTokenDetails == nil ||
		responseUsage.InputTokensDetails.CachedWriteTokenDetails.CachedWriteTokens1h != 700 {
		t.Fatalf("response cached write details = %+v, want 1h=700", responseUsage.InputTokensDetails.CachedWriteTokenDetails)
	}

	if billedUsage.PromptTokens != 10 || billedUsage.CompletionTokens != 5 || billedUsage.TotalTokens != 1515 {
		t.Fatalf("unexpected billed usage before normalization: %+v", billedUsage)
	}
	if billedUsage.PromptTokensDetails == nil {
		t.Fatal("expected billed cache details")
	}
	if billedUsage.PromptTokensDetails.CachedReadTokens != 800 {
		t.Fatalf("billed cached read = %d, want 800", billedUsage.PromptTokensDetails.CachedReadTokens)
	}
	if billedUsage.PromptTokensDetails.CachedWriteTokens != 700 {
		t.Fatalf("billed cached write = %d, want 700", billedUsage.PromptTokensDetails.CachedWriteTokens)
	}
	if billedUsage.PromptTokensDetails.CachedWriteTokenDetails == nil ||
		billedUsage.PromptTokensDetails.CachedWriteTokenDetails.CachedWriteTokens1h != 700 {
		t.Fatalf("billed cached write details = %+v, want 1h=700", billedUsage.PromptTokensDetails.CachedWriteTokenDetails)
	}

	normalizeCachedUsage(billedUsage)
	if billedUsage.PromptTokens != 1510 {
		t.Fatalf("normalized billed prompt = %d, want 1510", billedUsage.PromptTokens)
	}
	if billedUsage.TotalTokens != 1515 {
		t.Fatalf("normalized billed total = %d, want 1515", billedUsage.TotalTokens)
	}
}

// TestInterruptedStreamBillsGuardrailUnits (TXE-7365): a Converse stream whose metadata event
// carried the guardrail trace and is then cancelled bills the guardrail units through the
// error's BilledUsage, on both the chat and the Responses stream. Before the fix the Responses
// stream put the units only on its response usage, which a cancelled stream never returns.
func TestInterruptedStreamBillsGuardrailUnits(t *testing.T) {
	metadata := []byte(`{"usage":{"inputTokens":10,"outputTokens":2,"totalTokens":12},"metrics":{"latencyMs":5},` +
		`"trace":{"guardrail":{"inputAssessment":{"gr-1":{"invocationMetrics":{"usage":{"contentPolicyUnits":1,"topicPolicyUnits":1}}}}}}}`)
	want := &schemas.GuardrailUnits{ContentPolicyUnits: 1, TopicPolicyUnits: 1}

	for _, route := range []string{"chat", "responses"} {
		t.Run(route, func(t *testing.T) {
			release := make(chan struct{})
			var releaseOnce sync.Once
			closeRelease := func() { releaseOnce.Do(func() { close(release) }) }
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				flusher, ok := w.(http.Flusher)
				require.True(t, ok)
				w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
				w.WriteHeader(http.StatusOK)
				writeEventStreamEvent(t, w, "messageStart", []byte(`{"role":"assistant"}`))
				writeEventStreamEvent(t, w, "metadata", metadata)
				// A delta after the metadata tells the test the metadata was read.
				writeEventStreamEvent(t, w, "contentBlockDelta", []byte(`{"delta":{"text":"Hi"},"contentBlockIndex":0}`))
				flusher.Flush()
				select {
				case <-release:
				case <-r.Context().Done():
				}
			}))
			defer ts.Close()
			defer closeRelease()

			var mu sync.Mutex
			var billed *schemas.BifrostError
			postHook := func(_ *schemas.BifrostContext, result *schemas.BifrostResponse, err *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError) {
				if err != nil {
					mu.Lock()
					billed = err
					mu.Unlock()
				}
				return result, err
			}

			provider := newTestProviderWithServer(t, ts)
			ctx, cancel := schemas.NewBifrostContextWithCancel(context.Background())
			defer cancel()
			var stream chan *schemas.BifrostStreamChunk
			var bifrostErr *schemas.BifrostError
			if route == "chat" {
				req := testChatRequest()
				req.Model = testConverseStreamModel
				stream, bifrostErr = provider.ChatCompletionStream(ctx, postHook, nil, testBedrockKey(), req)
			} else {
				req := testResponsesRequest()
				req.Model = testConverseStreamModel
				stream, bifrostErr = provider.ResponsesStream(ctx, postHook, nil, testBedrockKey(), req)
			}
			require.Nil(t, bifrostErr)

			deadline := time.After(5 * time.Second)
		read:
			for {
				select {
				case chunk, ok := <-stream:
					require.True(t, ok, "stream closed before the text delta")
					if chunk.BifrostError == nil && (chunk.BifrostChatResponse != nil || chunk.BifrostResponsesStreamResponse != nil) &&
						hasTextDelta(chunk) {
						break read
					}
				case <-deadline:
					t.Fatal("no text delta arrived")
				}
			}
			cancel()
			for range stream {
			}

			mu.Lock()
			defer mu.Unlock()
			require.NotNil(t, billed, "the cancelled stream ran no error post-hook")
			require.NotNil(t, billed.ExtraFields.BilledUsage, "the cancelled stream billed nothing")
			assert.Equal(t, want, billed.ExtraFields.BilledUsage.GuardrailUnits)
		})
	}
}

func hasTextDelta(chunk *schemas.BifrostStreamChunk) bool {
	if r := chunk.BifrostResponsesStreamResponse; r != nil {
		return r.Type == schemas.ResponsesStreamResponseTypeOutputTextDelta
	}
	if c := chunk.BifrostChatResponse; c != nil {
		for _, choice := range c.Choices {
			if choice.ChatStreamResponseChoice != nil && choice.ChatStreamResponseChoice.Delta != nil &&
				choice.ChatStreamResponseChoice.Delta.Content != nil {
				return true
			}
		}
	}
	return false
}
