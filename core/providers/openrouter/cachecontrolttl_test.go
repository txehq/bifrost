package openrouter_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/tidwall/gjson"
)

// pricedCacheControlTTLKey is schemas.BifrostContextKeyRequestedCacheControlTTL spelled out, so the
// test builds on a tree without the key and fails there by assertion.
const pricedCacheControlTTLKey = schemas.BifrostContextKey("bifrost-requested-cache-control-ttl")

// TestPricedCacheControlTTLIsTheTTLOpenRouterReceives: OpenRouter bills a Claude cache write at the
// cache_control.ttl of the blocks it receives and reports one cache_write_tokens figure, and
// Bifrost prices the write from the TTL it stamped on the context (fork, TXE-8086). The stamp must
// be the longest TTL the body actually carries:
//   - the chat encoder sends cache_control as given, so a 1h block is priced at 1h;
//   - the Responses encoder turns a marker into prompt_cache_breakpoint, which has no TTL, so the
//     request is billed, and priced, at the base rate;
//   - a raw body is read as sent.
func TestPricedCacheControlTTLIsTheTTLOpenRouterReceives(t *testing.T) {
	bodies := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies <- body
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"recorded","code":400}}`))
	}))
	defer server.Close()
	provider := newDecisionTestProvider(server.URL + "/api")
	key := schemas.Key{Value: *schemas.NewSecretVar("or-test"), Models: []string{"*"}}
	const model = "anthropic/claude-fable-5.2"
	ttl := func(s string) *schemas.CacheControl {
		return &schemas.CacheControl{Type: schemas.CacheControlTypeEphemeral, TTL: &s}
	}
	chatBlocks := func(markers ...*schemas.CacheControl) []schemas.ChatMessage {
		blocks := make([]schemas.ChatContentBlock, len(markers))
		for i, m := range markers {
			blocks[i] = schemas.ChatContentBlock{Type: schemas.ChatContentBlockTypeText, Text: schemas.Ptr(strings.Repeat("context ", 8)), CacheControl: m}
		}
		return []schemas.ChatMessage{{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentBlocks: blocks}}}
	}

	for _, c := range []struct {
		name string
		req  *schemas.BifrostRequest
		raw  []byte
		// onWire is the longest cache_control.ttl the body sent must carry; priced must equal it.
		onWire string
	}{
		{name: "chat 1h block", req: &schemas.BifrostRequest{ChatRequest: &schemas.BifrostChatRequest{
			Provider: schemas.OpenRouter, Model: model, Input: chatBlocks(ttl("1h")),
		}}, onWire: "1h"},
		{name: "chat 5m and 1h blocks", req: &schemas.BifrostRequest{ChatRequest: &schemas.BifrostChatRequest{
			Provider: schemas.OpenRouter, Model: model, Input: chatBlocks(ttl("5m"), ttl("1h")),
		}}, onWire: "1h"},
		{name: "chat untimed block", req: &schemas.BifrostRequest{ChatRequest: &schemas.BifrostChatRequest{
			Provider: schemas.OpenRouter, Model: model, Input: chatBlocks(&schemas.CacheControl{Type: schemas.CacheControlTypeEphemeral}),
		}}, onWire: ""},
		{name: "responses 1h block", req: &schemas.BifrostRequest{ResponsesRequest: &schemas.BifrostResponsesRequest{
			Provider: schemas.OpenRouter, Model: model, Input: []schemas.ResponsesMessage{{
				Role: schemas.Ptr(schemas.ResponsesInputMessageRoleUser),
				Content: &schemas.ResponsesMessageContent{ContentBlocks: []schemas.ResponsesMessageContentBlock{{
					Type: schemas.ResponsesInputMessageContentBlockTypeText, Text: schemas.Ptr("context"), CacheControl: ttl("1h"),
				}}},
			}},
		}}, onWire: ""},
		{name: "chat raw body", req: &schemas.BifrostRequest{ChatRequest: &schemas.BifrostChatRequest{
			Provider: schemas.OpenRouter, Model: model, Input: chatBlocks(nil),
		}}, raw: []byte(`{"model":"` + model + `","messages":[{"role":"user","content":[{"type":"text","text":"x","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}`), onWire: "1h"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
			// What Bifrost stamps before the attempt: the base provider, and the typed request's
			// longest cache_control.ttl.
			ctx.SetValue(schemas.BifrostContextKeyBaseProviderType, schemas.OpenRouter)
			ctx.SetValue(pricedCacheControlTTLKey, c.req.RequestedCacheControlTTL())
			if c.raw != nil {
				ctx.SetValue(schemas.BifrostContextKeyUseRawRequestBody, true)
			}
			switch {
			case c.req.ChatRequest != nil:
				c.req.ChatRequest.RawRequestBody = c.raw
				_, _ = provider.ChatCompletion(ctx, key, c.req.ChatRequest)
			default:
				_, _ = provider.Responses(ctx, key, c.req.ResponsesRequest)
			}
			sent := <-bodies
			longest := ""
			for _, path := range []string{"messages.#.content.#.cache_control.ttl", "input.#.content.#.cache_control.ttl", "input.#.cache_control.ttl"} {
				gjson.GetBytes(sent, path).ForEach(func(_, v gjson.Result) bool {
					v.ForEach(func(_, ttl gjson.Result) bool {
						longest = schemas.LongerCacheTTL(longest, ttl.String())
						return true
					})
					if v.Type == gjson.String {
						longest = schemas.LongerCacheTTL(longest, v.String())
					}
					return true
				})
			}
			if longest != c.onWire {
				t.Fatalf("OpenRouter received a longest cache_control.ttl of %q, want %q: %s", longest, c.onWire, sent)
			}
			if priced, _ := ctx.Value(pricedCacheControlTTLKey).(string); priced != c.onWire {
				t.Fatalf("priced cache_control ttl %q, but OpenRouter received a body asking for %q: %s", priced, c.onWire, sent)
			}
		})
	}
}
