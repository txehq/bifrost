package moonshot_test

import (
	"context"
	"net/http"
	"testing"

	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/tidwall/gjson"
)

// pricedTTLKey is BifrostContextKeyRequestedPromptCacheTTL spelled out, so this test also reads the
// key pricing reads on a tree where the constant does not exist.
const pricedTTLKey = schemas.BifrostContextKey("bifrost-requested-prompt-cache-ttl")

// TestPricedPromptCacheTTLIsTheTTLKimiReceives: Kimi bills a cache write by the
// prompt_cache_options.ttl of the body it receives, and Bifrost prices the write from the TTL it
// stamped on the context (fork, TXE-7069). The two must be the same value on every request path:
// typed, through the extra-params merge with any spelling it accepts, and a raw body sent as is
// whatever the typed field says (fork, TXE-7996). Each case runs on chat, chat stream, Responses and
// Responses stream, and compares the context against the body the upstream received.
//
// Before TXE-7996 the context kept the typed or merged value: a raw body asking for 1h was priced
// at the 5-minute rate, and a PromptCacheOptions extra param was not read at all.
func TestPricedPromptCacheTTLIsTheTTLKimiReceives(t *testing.T) {
	type call func(t *testing.T, ctx *schemas.BifrostContext, params ttlCase)
	provider, _, requests := newMoonshotTestProvider(t, func(w http.ResponseWriter, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"recorded","type":"invalid_request_error"}}`))
	})
	key := moonshotTestKey()
	input := []schemas.ChatMessage{{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: bifrost.Ptr("hi")}}}
	responsesInput := []schemas.ResponsesMessage{{
		Role:    bifrost.Ptr(schemas.ResponsesInputMessageRoleUser),
		Content: &schemas.ResponsesMessageContent{ContentStr: bifrost.Ptr("hi")},
	}}
	calls := map[string]call{
		"chat": func(t *testing.T, ctx *schemas.BifrostContext, c ttlCase) {
			_, _ = provider.ChatCompletion(ctx, key, &schemas.BifrostChatRequest{
				Provider: schemas.Moonshot, Model: "kimi-k3", Input: input, RawRequestBody: c.rawBody,
				Params: &schemas.ChatParameters{PromptCacheOptions: c.typed, ExtraParams: c.extra},
			})
		},
		"chat stream": func(t *testing.T, ctx *schemas.BifrostContext, c ttlCase) {
			stream, _ := provider.ChatCompletionStream(ctx, moonshotPassthroughPostHook, nil, key, &schemas.BifrostChatRequest{
				Provider: schemas.Moonshot, Model: "kimi-k3", Input: input, RawRequestBody: c.rawBody,
				Params: &schemas.ChatParameters{PromptCacheOptions: c.typed, ExtraParams: c.extra},
			})
			drainStream(stream)
		},
		"responses": func(t *testing.T, ctx *schemas.BifrostContext, c ttlCase) {
			_, _ = provider.Responses(ctx, key, &schemas.BifrostResponsesRequest{
				Provider: schemas.Moonshot, Model: "kimi-k3", Input: responsesInput, RawRequestBody: c.rawBody,
				Params: &schemas.ResponsesParameters{PromptCacheOptions: c.typed, ExtraParams: c.extra},
			})
		},
		"responses stream": func(t *testing.T, ctx *schemas.BifrostContext, c ttlCase) {
			stream, _ := provider.ResponsesStream(ctx, moonshotPassthroughPostHook, nil, key, &schemas.BifrostResponsesRequest{
				Provider: schemas.Moonshot, Model: "kimi-k3", Input: responsesInput, RawRequestBody: c.rawBody,
				Params: &schemas.ResponsesParameters{PromptCacheOptions: c.typed, ExtraParams: c.extra},
			})
			drainStream(stream)
		},
	}
	for _, c := range ttlCases() {
		for name, run := range calls {
			t.Run(c.name+"/"+name, func(t *testing.T) {
				ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
				// What Bifrost stamps before the attempt: the typed request's TTL, and the attempt's
				// base provider (a custom provider on Kimi has the same one).
				ctx.SetValue(pricedTTLKey, c.stampedBefore)
				ctx.SetValue(schemas.BifrostContextKeyBaseProviderType, schemas.Moonshot)
				if c.rawBody != nil {
					ctx.SetValue(schemas.BifrostContextKeyUseRawRequestBody, true)
				}
				if c.extra != nil {
					ctx.SetValue(schemas.BifrostContextKeyPassthroughExtraParams, true)
				}
				run(t, ctx, c)
				sent := <-requests
				onWire := gjson.GetBytes(sent.body, "prompt_cache_options.ttl").String()
				want := c.wantOnWire
				if c.extra != nil {
					// The merge decides the spelling the body carries; Kimi reads the body.
					want = onWire
				} else if c.rawBody == nil && onWire != want {
					t.Fatalf("Kimi received ttl %q, want %q; body %s", onWire, want, sent.body)
				}
				priced, _ := ctx.Value(pricedTTLKey).(string)
				if priced != want {
					t.Fatalf("priced ttl %q, but Kimi received a body asking for %q: %s", priced, want, sent.body)
				}
			})
		}
	}
}

// drainStream reads a stream to its end; a refused stream comes back nil.
func drainStream(stream chan *schemas.BifrostStreamChunk) {
	if stream == nil {
		return
	}
	for range stream {
	}
}

type ttlCase struct {
	name          string
	typed         *schemas.PromptCacheOptions
	extra         map[string]interface{}
	rawBody       []byte
	stampedBefore string
	wantOnWire    string
}

func ttlCases() []ttlCase {
	return []ttlCase{
		{name: "typed 1h", typed: &schemas.PromptCacheOptions{TTL: bifrost.Ptr("1h")}, stampedBefore: "1h", wantOnWire: "1h"},
		{name: "no ttl", stampedBefore: "", wantOnWire: ""},
		{
			// The merge sends this spelling as is when the body has no such field.
			name:          "extra param PromptCacheOptions",
			extra:         map[string]interface{}{"PromptCacheOptions": map[string]interface{}{"ttl": "1h"}},
			stampedBefore: "",
		},
		{
			// Codex on PR #403: the merge treats both names as one field.
			name: "extra params prompt_cache_options and PromptCacheOptions",
			extra: map[string]interface{}{
				"prompt_cache_options": map[string]interface{}{},
				"PromptCacheOptions":   map[string]interface{}{"ttl": "1h"},
			},
			stampedBefore: "",
		},
		{
			name:          "extra param prompt_cache_options",
			extra:         map[string]interface{}{"prompt_cache_options": map[string]interface{}{"ttl": "1h"}},
			stampedBefore: "1h",
		},
		{
			name:          "raw body 5m under a typed 1h",
			typed:         &schemas.PromptCacheOptions{TTL: bifrost.Ptr("1h")},
			rawBody:       []byte(`{"model":"kimi-k3","messages":[{"role":"user","content":"hi"}],"input":"hi","prompt_cache_options":{"ttl":"5m"}}`),
			stampedBefore: "1h", wantOnWire: "5m",
		},
		{
			name:          "raw body 1h with nothing typed",
			rawBody:       []byte(`{"model":"kimi-k3","messages":[{"role":"user","content":"hi"}],"input":"hi","prompt_cache_options":{"ttl":"1h"}}`),
			stampedBefore: "", wantOnWire: "1h",
		},
		{
			name:          "raw body with an escaped field name",
			rawBody:       []byte(`{"model":"kimi-k3","messages":[{"role":"user","content":"hi"}],"input":"hi","prompt_cache_options":{"ttl":"1h"}}`),
			stampedBefore: "", wantOnWire: "1h",
		},
	}
}
