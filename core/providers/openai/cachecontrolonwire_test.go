package openai

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/capsohq/bifrost/core/schemas"
)

// Every standard provider has a per-block cache_control rule with its evidence, so a provider
// added later is decided in perBlockCacheControlRules instead of inheriting the strip default
// (TXE-8179). A rule for a provider that no longer exists fails too.
func TestEveryProviderHasAPerBlockCacheControlRule(t *testing.T) {
	standard := map[schemas.ModelProvider]bool{}
	for _, p := range schemas.StandardProviders {
		standard[p] = true
		rule, ok := perBlockCacheControlRules[p]
		if !ok {
			t.Errorf("provider %q has no entry in perBlockCacheControlRules: decide whether its upstream takes per-block cache_control, with the vendor doc", p)
			continue
		}
		if strings.TrimSpace(rule.why) == "" {
			t.Errorf("provider %q has a rule with no evidence", p)
		}
		if rule.treatment == cacheControlStripped && rule.ttlSent {
			t.Errorf("provider %q strips the marker but claims to send its ttl", p)
		}
	}
	for p := range perBlockCacheControlRules {
		if !standard[p] {
			t.Errorf("perBlockCacheControlRules lists %q, which is not a standard provider", p)
		}
	}
}

func cacheControlChatRequest(provider schemas.ModelProvider, model string, ttl *string) *schemas.BifrostChatRequest {
	cc := &schemas.CacheControl{Type: schemas.CacheControlTypeEphemeral, TTL: ttl}
	return &schemas.BifrostChatRequest{
		Provider: provider,
		Model:    model,
		Input: []schemas.ChatMessage{
			{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentBlocks: []schemas.ChatContentBlock{
				{Type: schemas.ChatContentBlockTypeText, Text: schemas.Ptr("long shared prefix"), CacheControl: cc},
			}}},
			{Role: schemas.ChatMessageRoleAssistant, ChatAssistantMessage: &schemas.ChatAssistantMessage{ToolCalls: []schemas.ChatAssistantMessageToolCall{
				{ID: schemas.Ptr("call_1"), Type: schemas.Ptr("function"), Function: schemas.ChatAssistantMessageToolCallFunction{Name: schemas.Ptr("lookup"), Arguments: "{}"}, CacheControl: cc},
			}}},
			{Role: schemas.ChatMessageRoleTool, ChatToolMessage: &schemas.ChatToolMessage{ToolCallID: schemas.Ptr("call_1")}, Content: &schemas.ChatMessageContent{ContentStr: schemas.Ptr("ok")}},
		},
		Params: &schemas.ChatParameters{Tools: []schemas.ChatTool{
			{Type: schemas.ChatToolTypeFunction, Function: &schemas.ChatToolFunction{Name: "lookup"}, CacheControl: cc},
		}},
	}
}

// cacheControlCount is how many cache_control objects a serialized body carries.
func cacheControlCount(t *testing.T, ctx *schemas.BifrostContext, req *schemas.BifrostChatRequest) int {
	t.Helper()
	if ctx == nil {
		ctx = schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	}
	body, err := json.Marshal(ToOpenAIChatRequest(ctx, req))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return strings.Count(string(body), `"cache_control"`)
}

// The encoder sends the markers (content block, tool call, tool definition) exactly where the
// table says: OpenRouter for every model, Databricks for Claude only, a custom provider on an
// OpenRouter base like OpenRouter, and nobody else (TXE-8179).
func TestPerBlockCacheControlFollowsTheTable(t *testing.T) {
	custom := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	custom.SetValue(schemas.BifrostContextKeyBaseProviderType, schemas.OpenRouter)
	customDatabricks := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	customDatabricks.SetValue(schemas.BifrostContextKeyBaseProviderType, schemas.Databricks)
	cases := []struct {
		name     string
		ctx      *schemas.BifrostContext
		provider schemas.ModelProvider
		model    string
		want     int
	}{
		{"openrouter claude", nil, schemas.OpenRouter, "anthropic/claude-sonnet-4-5", 3},
		{"openrouter gpt", nil, schemas.OpenRouter, "openai/gpt-5", 3},
		{"databricks claude endpoint", nil, schemas.Databricks, "databricks-claude-sonnet-4-5", 3},
		{"databricks unity catalog claude", nil, schemas.Databricks, "system.ai.claude-sonnet-4-5", 3},
		{"databricks gemini endpoint", nil, schemas.Databricks, "databricks-gemini-3-pro", 0},
		{"databricks gpt endpoint", nil, schemas.Databricks, "databricks-gpt-5", 0},
		{"custom provider on openrouter", custom, "my_router", "anthropic/claude-sonnet-4-5", 3},
		{"custom provider on databricks, claude", customDatabricks, "my_dbx", "databricks-claude-sonnet-4-5", 3},
		{"openai", nil, schemas.OpenAI, "gpt-4.1", 0},
		{"qwen, held", nil, schemas.Qwen, "qwen-plus", 0},
		{"moonshot", nil, schemas.Moonshot, "kimi-k2", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cacheControlCount(t, tc.ctx, cacheControlChatRequest(tc.provider, tc.model, nil)); got != tc.want {
				t.Errorf("cache_control markers on the wire = %d, want %d", got, tc.want)
			}
		})
	}
}

// A body built without ToOpenAIChatRequest (no CacheControlOnWire set) reads the same table.
func TestPerBlockCacheControlWithoutTheFlagReadsTheTable(t *testing.T) {
	for provider, want := range map[schemas.ModelProvider]bool{schemas.OpenRouter: true, schemas.OpenAI: false} {
		req := ToOpenAIChatRequest(schemas.NewBifrostContext(context.Background(), schemas.NoDeadline), cacheControlChatRequest(provider, "anthropic/claude-sonnet-4-5", nil))
		req.CacheControlOnWire = nil
		body, err := json.Marshal(req)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if got := strings.Contains(string(body), `"cache_control"`); got != want {
			t.Errorf("%s: cache_control on the wire = %v, want %v", provider, got, want)
		}
	}
}

// A ttl on a marker Databricks receives is refused with 400 caller_unsupported_parameter, since
// Databricks documents none and dropping it would change the price (contract C4). OpenRouter
// sends it; a provider that strips the marker refuses nothing; a marker without a ttl passes.
func TestCacheControlTTLRefusal(t *testing.T) {
	oneHour := schemas.Ptr("1h")
	cases := []struct {
		name     string
		provider schemas.ModelProvider
		model    string
		ttl      *string
		refused  bool
	}{
		{"databricks claude with 1h", schemas.Databricks, "databricks-claude-sonnet-4-5", oneHour, true},
		{"databricks claude with 5m", schemas.Databricks, "databricks-claude-sonnet-4-5", schemas.Ptr("5m"), true},
		{"databricks claude without ttl", schemas.Databricks, "databricks-claude-sonnet-4-5", nil, false},
		{"databricks gemini with 1h (marker stripped)", schemas.Databricks, "databricks-gemini-3-pro", oneHour, false},
		{"openrouter claude with 1h", schemas.OpenRouter, "anthropic/claude-sonnet-4-5", oneHour, false},
		{"openai with 1h (marker stripped)", schemas.OpenAI, "gpt-4.1", oneHour, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ChatRequestRefusal(schemas.NewBifrostContext(context.Background(), schemas.NoDeadline), cacheControlChatRequest(tc.provider, tc.model, tc.ttl))
			if !tc.refused {
				if err != nil {
					t.Fatalf("refused: %+v", err.Error)
				}
				return
			}
			if err == nil {
				t.Fatal("not refused")
			}
			if err.StatusCode == nil || *err.StatusCode != http.StatusBadRequest || err.ExtraFields.ErrorType != schemas.ErrorTypeCallerUnsupportedParameter {
				t.Errorf("refusal = status %v type %q, want 400 caller_unsupported_parameter", err.StatusCode, err.ExtraFields.ErrorType)
			}
			if err.Error == nil || err.Error.Param != "cache_control.ttl" {
				t.Errorf("refusal param = %+v, want cache_control.ttl", err.Error)
			}
		})
	}
}

// The encoder table and the cache-control TTL pricing table (schemas.CacheControlTTLPricedCacheWrites,
// TXE-8086) cannot drift: a provider priced at its markers' ttl must send the marker with its ttl,
// or the vendor bills a 5-minute write that pricing costs at the asked tier; and a provider that
// sends a ttl must have a pricing decision, or the vendor bills a 1-hour write that pricing costs
// at the base rate (TXE-8179).
func TestCacheControlTTLPricingAgreesWithTheEncoder(t *testing.T) {
	for provider := range schemas.CacheControlTTLPricedCacheWrites {
		rule, ok := perBlockCacheControlRules[provider]
		if !ok || rule.treatment == cacheControlStripped || !rule.ttlSent {
			t.Errorf("%s is priced at its cache_control ttl (CacheControlTTLPricedCacheWrites) but perBlockCacheControlRules does not send its markers with their ttl: %+v", provider, rule)
		}
	}
	for provider, rule := range perBlockCacheControlRules {
		if rule.treatment == cacheControlStripped || !rule.ttlSent {
			continue
		}
		if _, priced := schemas.CacheControlTTLPricedCacheWrites[provider]; !priced {
			t.Errorf("%s sends cache_control with its ttl but has no entry in schemas.CacheControlTTLPricedCacheWrites: decide how its ttl is priced", provider)
		}
	}
}

// BenchmarkCacheControlTTLRefusal measures the TTL check ChatRequestRefusal runs on every chat
// request the shared handlers send (TXE-8179). A provider that strips the marker, or sends it with
// its ttl, returns after one map lookup; only Databricks Claude walks the request.
func BenchmarkCacheControlTTLRefusal(b *testing.B) {
	conversation := func(provider schemas.ModelProvider, model string, ttl *string) *schemas.BifrostChatRequest {
		req := &schemas.BifrostChatRequest{Provider: provider, Model: model, Params: &schemas.ChatParameters{}}
		for i := 0; i < 40; i++ {
			req.Input = append(req.Input, schemas.ChatMessage{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentBlocks: []schemas.ChatContentBlock{
				{Type: schemas.ChatContentBlockTypeText, Text: schemas.Ptr("turn")},
				{Type: schemas.ChatContentBlockTypeText, Text: schemas.Ptr("more")},
			}}})
		}
		for i := 0; i < 10; i++ {
			req.Params.Tools = append(req.Params.Tools, schemas.ChatTool{Type: schemas.ChatToolTypeFunction, Function: &schemas.ChatToolFunction{Name: "t"}})
		}
		if ttl != nil {
			last := &req.Input[len(req.Input)-1].Content.ContentBlocks[1]
			last.CacheControl = &schemas.CacheControl{Type: schemas.CacheControlTypeEphemeral, TTL: ttl}
		}
		return req
	}
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	for _, tc := range []struct {
		name string
		req  *schemas.BifrostChatRequest
	}{
		{"openai-40-turns", conversation(schemas.OpenAI, "gpt-4.1", nil)},
		{"openrouter-40-turns", conversation(schemas.OpenRouter, "anthropic/claude-sonnet-4-5", nil)},
		{"databricks-claude-40-turns", conversation(schemas.Databricks, "databricks-claude-sonnet-4-5", nil)},
		{"databricks-claude-40-turns-ttl", conversation(schemas.Databricks, "databricks-claude-sonnet-4-5", schemas.Ptr("1h"))},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = cacheControlTTLRefusal(ctx, tc.req)
			}
		})
	}
}

// The Responses→chat conversion now keeps a block's marker, so a lone marked text block stays a
// block. A provider that strips the marker still gets the plain string it got before, and a
// provider that sends it gets the block with the marker (TXE-8179).
func TestConvertedLoneMarkedTextBlockKeepsItsWireShape(t *testing.T) {
	var in OpenAIResponsesRequest
	body := `{"model":"m","input":[{"role":"user","content":[{"type":"input_text","text":"long shared prefix","cache_control":{"type":"ephemeral"}}]}]}`
	if err := schemas.Unmarshal([]byte(body), &in); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for provider, want := range map[schemas.ModelProvider]string{
		schemas.Moonshot:   `"content":"long shared prefix"`,
		schemas.OpenRouter: `"content":[{"type":"text","text":"long shared prefix","cache_control":{"type":"ephemeral"}}]`,
	} {
		ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
		req := in.ToBifrostResponsesRequest(ctx)
		req.Provider = provider
		req.Model = "anthropic/claude-sonnet-4-5"
		wire, err := json.Marshal(ToOpenAIChatRequest(ctx, req.ToChatRequest()))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if !strings.Contains(string(wire), want) {
			t.Errorf("%s: wire %s, want it to contain %s", provider, wire, want)
		}
	}
}
