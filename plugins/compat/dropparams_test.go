package compat

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/capsohq/bifrost/core/schemas"
)

func newTestContext() *schemas.BifrostContext {
	return schemas.NewBifrostContext(context.Background(), time.Time{})
}

func newResponsesRequest(provider schemas.ModelProvider, model string, params *schemas.ResponsesParameters) *schemas.BifrostRequest {
	return &schemas.BifrostRequest{
		RequestType:      schemas.ResponsesRequest,
		ResponsesRequest: &schemas.BifrostResponsesRequest{Provider: provider, Model: model, Params: params},
	}
}

// TestDropUnsupportedParams_MaxOutputTokens verifies that the Responses-API
// max_output_tokens cap is never dropped, whatever the catalog lists: it bounds what
// the caller is billed for, so it goes on as sent.
func TestDropUnsupportedParams_MaxOutputTokens(t *testing.T) {
	tests := []struct {
		name          string
		provider      schemas.ModelProvider
		model         string
		supported     []string
		wantPreserved bool
	}{
		{
			name:          "openai gpt-4o-mini lists chat max_tokens only",
			provider:      schemas.OpenAI,
			model:         "gpt-4o-mini",
			supported:     []string{"temperature", "top_p", "max_tokens", "stop", "tools"},
			wantPreserved: true,
		},
		{
			name:          "openai o4-mini lists max_completion_tokens",
			provider:      schemas.OpenAI,
			model:         "o4-mini",
			supported:     []string{"temperature", "max_completion_tokens"},
			wantPreserved: true,
		},
		{
			name:          "model lists max_output_tokens explicitly",
			provider:      schemas.OpenAI,
			model:         "gpt-oss-120b",
			supported:     []string{"temperature", "max_output_tokens"},
			wantPreserved: true,
		},
		{
			name:          "anthropic claude lists max_tokens",
			provider:      schemas.Anthropic,
			model:         "claude-sonnet-4",
			supported:     []string{"temperature", "max_tokens", "top_p"},
			wantPreserved: true,
		},
		{
			name:          "vertex gemini lists max_tokens",
			provider:      schemas.Vertex,
			model:         "gemini-2.5-pro",
			supported:     []string{"temperature", "max_tokens"},
			wantPreserved: true,
		},
		{
			name:          "no token cap listed",
			provider:      schemas.OpenAI,
			model:         "no-token-cap-model",
			supported:     []string{"temperature", "top_p"},
			wantPreserved: true,
		},
		{
			name:          "empty list",
			provider:      schemas.OpenAI,
			model:         "unknown-model",
			supported:     []string{},
			wantPreserved: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := newResponsesRequest(tt.provider, tt.model, &schemas.ResponsesParameters{
				MaxOutputTokens: schemas.Ptr(16),
				Temperature:     schemas.Ptr(0.5),
			})

			dropped := dropUnsupportedParams(newTestContext(), req, tt.supported)
			got := req.ResponsesRequest.Params.MaxOutputTokens

			if tt.wantPreserved {
				if got == nil {
					t.Fatalf("max_output_tokens = dropped, want preserved (supported=%v)", tt.supported)
				}
				if *got != 16 {
					t.Errorf("max_output_tokens = %d, want 16", *got)
				}
				if slices.Contains(dropped, "max_output_tokens") {
					t.Errorf("max_output_tokens reported in dropped=%v, want absent", dropped)
				}
				return
			}

			if got != nil {
				t.Fatalf("max_output_tokens = %d, want dropped (supported=%v)", *got, tt.supported)
			}
			if !slices.Contains(dropped, "max_output_tokens") {
				t.Errorf("max_output_tokens not reported in dropped=%v, want present", dropped)
			}
		})
	}
}

// TestDropUnsupportedParams_MaxOutputTokensSurgical verifies the fix is
// surgical: within one Responses request, max_output_tokens is preserved (chat
// token cap supported) while an unsupported sibling (top_p) is still dropped.
func TestDropUnsupportedParams_MaxOutputTokensSurgical(t *testing.T) {
	req := newResponsesRequest(schemas.OpenAI, "gpt-4o-mini", &schemas.ResponsesParameters{
		MaxOutputTokens: schemas.Ptr(16),
		TopP:            schemas.Ptr(0.9),
		Temperature:     schemas.Ptr(0.5),
	})

	dropped := dropUnsupportedParams(newTestContext(), req, []string{"max_tokens", "temperature"})

	p := req.ResponsesRequest.Params
	if p.MaxOutputTokens == nil {
		t.Errorf("max_output_tokens = dropped, want preserved")
	}
	if p.TopP != nil {
		t.Errorf("top_p = preserved, want dropped")
	}
	if p.Temperature == nil {
		t.Errorf("temperature = dropped, want preserved")
	}
	if !slices.Contains(dropped, "top_p") {
		t.Errorf("top_p not reported in dropped=%v, want present", dropped)
	}
}

// A chat request keeps max_completion_tokens whatever the catalog lists: it bounds
// what the caller is billed for, so it is delivered as sent and never dropped.
func TestDropUnsupportedParams_ChatMaxCompletionTokensIsNeverDropped(t *testing.T) {
	for name, supported := range map[string][]string{
		"max_tokens listed":            {"max_tokens"},
		"max_completion_tokens listed": {"max_completion_tokens"},
		"no token cap listed":          {"temperature"},
		"empty list":                   {},
	} {
		t.Run(name, func(t *testing.T) {
			req := &schemas.BifrostRequest{
				RequestType: schemas.ChatCompletionRequest,
				ChatRequest: &schemas.BifrostChatRequest{
					Provider: schemas.OpenAI,
					Model:    "gpt-4o-mini",
					Params:   &schemas.ChatParameters{MaxCompletionTokens: schemas.Ptr(1), TopP: schemas.Ptr(0.5)},
				},
			}
			dropped := dropUnsupportedParams(newTestContext(), req, supported)
			if got := req.ChatRequest.Params.MaxCompletionTokens; got == nil || *got != 1 {
				t.Fatalf("max_completion_tokens = %v, want 1 as sent", got)
			}
			if slices.Contains(dropped, "max_completion_tokens") {
				t.Fatalf("dropped = %v, must not name max_completion_tokens", dropped)
			}
			if !slices.Contains(dropped, "top_p") {
				t.Fatalf("dropped = %v, want top_p: other unsupported params are still dropped", dropped)
			}
		})
	}
}

// TestDropUnsupportedParams_ServiceTier pins service_tier on both request paths.
// service_tier decides what the caller is billed, so it is never dropped: the provider
// delivers it or refuses it with 400 caller_unsupported_parameter.
func TestDropUnsupportedParams_ServiceTier(t *testing.T) {
	tests := []struct {
		name      string
		supported []string
	}{
		{
			name:      "catalog lists service_tier",
			supported: []string{"temperature", "service_tier"},
		},
		{
			name:      "catalog omits service_tier",
			supported: []string{"temperature"},
		},
		{
			name:      "empty catalog",
			supported: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name+"/chat", func(t *testing.T) {
			req := &schemas.BifrostRequest{
				RequestType: schemas.ChatCompletionRequest,
				ChatRequest: &schemas.BifrostChatRequest{
					Provider: schemas.OpenAI,
					Model:    "gpt-5.4",
					Params: &schemas.ChatParameters{
						ServiceTier: schemas.Ptr(schemas.BifrostServiceTierPriority),
						Temperature: schemas.Ptr(0.5),
					},
				},
			}

			dropped := dropUnsupportedParams(newTestContext(), req, tt.supported)
			assertServiceTier(t, req.ChatRequest.Params.ServiceTier, dropped, true, tt.supported)
		})

		t.Run(tt.name+"/responses", func(t *testing.T) {
			req := newResponsesRequest(schemas.OpenAI, "gpt-5.4", &schemas.ResponsesParameters{
				ServiceTier: schemas.Ptr(schemas.BifrostServiceTierPriority),
				Temperature: schemas.Ptr(0.5),
			})

			dropped := dropUnsupportedParams(newTestContext(), req, tt.supported)
			assertServiceTier(t, req.ResponsesRequest.Params.ServiceTier, dropped, true, tt.supported)
		})
	}
}

func assertServiceTier(t *testing.T, got *schemas.BifrostServiceTier, dropped []string, wantPreserved bool, supported []string) {
	t.Helper()

	if wantPreserved {
		if got == nil {
			t.Fatalf("service_tier = dropped, want preserved (supported=%v)", supported)
		}
		if *got != schemas.BifrostServiceTierPriority {
			t.Errorf("service_tier = %q, want %q", *got, schemas.BifrostServiceTierPriority)
		}
		if slices.Contains(dropped, "service_tier") {
			t.Errorf("service_tier reported in dropped=%v, want absent", dropped)
		}
		return
	}

	if got != nil {
		t.Fatalf("service_tier = %q, want dropped (supported=%v)", *got, supported)
	}
	if !slices.Contains(dropped, "service_tier") {
		t.Errorf("service_tier not reported in dropped=%v, want present", dropped)
	}
}

// Reasoning the caller sent decides what it is billed, so the plugin never drops it or
// turns it off: not when the model's list has no reasoning, and not when the model
// rejects reasoning alongside tools (the provider refuses that request, or core serves
// it on the Responses API). Tools the model does not list are still dropped.
func TestDropUnsupportedParams_ChatReasoningIsNeverDroppedOrRewritten(t *testing.T) {
	for name, supported := range map[string][]string{
		"reasoning not listed":               {"tools"},
		"tools not listed":                   {"reasoning"},
		"reasoning with tools not supported": {"reasoning", "tools"},
		"same, model takes effort none":      {"reasoning", "tools", "supports_none_reasoning_effort"},
		"reasoning with tools supported":     {"reasoning", "tools", "reasoning_with_tool_calls"},
		"empty list":                         {},
	} {
		for _, provider := range []schemas.ModelProvider{schemas.OpenAI, schemas.Azure, schemas.Groq, "my-openai"} {
			t.Run(name+"/"+string(provider), func(t *testing.T) {
				req := &schemas.BifrostRequest{
					RequestType: schemas.ChatCompletionRequest,
					ChatRequest: &schemas.BifrostChatRequest{
						Provider: provider,
						Model:    "reasoning-model",
						Params: &schemas.ChatParameters{
							Reasoning: &schemas.ChatReasoning{Effort: new("high"), Mode: new("pro"), MaxTokens: schemas.Ptr(2048)},
							Tools: []schemas.ChatTool{{
								Type:     schemas.ChatToolTypeFunction,
								Function: &schemas.ChatToolFunction{Name: "get_weather"},
							}},
						},
					},
				}
				dropped := dropUnsupportedParams(newTestContext(), req, supported)
				got := req.ChatRequest.Params.Reasoning
				if got == nil || got.Effort == nil || *got.Effort != "high" || got.Mode == nil || *got.Mode != "pro" || got.MaxTokens == nil || *got.MaxTokens != 2048 {
					t.Fatalf("reasoning = %+v, want effort=high mode=pro max_tokens=2048 as sent", got)
				}
				if slices.Contains(dropped, "reasoning") {
					t.Fatalf("dropped = %v, must not name reasoning", dropped)
				}
				if wantTools := slices.Contains(supported, "tools"); (req.ChatRequest.Params.Tools != nil) != wantTools {
					t.Fatalf("tools kept = %v, want %v", req.ChatRequest.Params.Tools != nil, wantTools)
				}
			})
		}
	}
}

// The other price-affecting fields are never dropped either, on any request shape,
// while a field that does not affect the price is still dropped next to them.
func TestDropUnsupportedParams_PriceAffectingFieldsAreNeverDropped(t *testing.T) {
	for name, supported := range map[string][]string{"nothing listed": {}, "only temperature listed": {"temperature"}} {
		t.Run(name+"/chat", func(t *testing.T) {
			req := &schemas.BifrostRequest{
				RequestType: schemas.ChatCompletionRequest,
				ChatRequest: &schemas.BifrostChatRequest{Provider: schemas.OpenAI, Model: "m", Params: &schemas.ChatParameters{
					MaxCompletionTokens:  schemas.Ptr(1),
					ServiceTier:          schemas.Ptr(schemas.BifrostServiceTierFlex),
					PromptCacheKey:       schemas.Ptr("tenant-42"),
					PromptCacheRetention: schemas.Ptr("24h"),
					Reasoning:            &schemas.ChatReasoning{Effort: new("high")},
					TopP:                 schemas.Ptr(0.5),
				}},
			}
			dropped := dropUnsupportedParams(newTestContext(), req, supported)
			p := req.ChatRequest.Params
			if p.MaxCompletionTokens == nil || p.ServiceTier == nil || p.PromptCacheKey == nil || p.PromptCacheRetention == nil || p.Reasoning == nil {
				t.Fatalf("a price-affecting chat field was dropped: %+v", p)
			}
			if !slices.Equal(dropped, []string{"top_p"}) {
				t.Fatalf("dropped = %v, want [top_p]", dropped)
			}
		})
		t.Run(name+"/responses", func(t *testing.T) {
			req := newResponsesRequest(schemas.OpenAI, "m", &schemas.ResponsesParameters{
				MaxOutputTokens: schemas.Ptr(1),
				ServiceTier:     schemas.Ptr(schemas.BifrostServiceTierFlex),
				PromptCacheKey:  schemas.Ptr("tenant-42"),
				Reasoning:       &schemas.ResponsesParametersReasoning{Effort: new("high")},
				TopP:            schemas.Ptr(0.5),
			})
			dropped := dropUnsupportedParams(newTestContext(), req, supported)
			p := req.ResponsesRequest.Params
			if p.MaxOutputTokens == nil || p.ServiceTier == nil || p.PromptCacheKey == nil || p.Reasoning == nil || p.Reasoning.Effort == nil {
				t.Fatalf("a price-affecting Responses field was dropped: %+v", p)
			}
			if !slices.Equal(dropped, []string{"top_p"}) {
				t.Fatalf("dropped = %v, want [top_p]", dropped)
			}
		})
		t.Run(name+"/text", func(t *testing.T) {
			req := &schemas.BifrostRequest{
				RequestType: schemas.TextCompletionRequest,
				TextCompletionRequest: &schemas.BifrostTextCompletionRequest{Provider: schemas.OpenAI, Model: "m", Params: &schemas.TextCompletionParameters{
					MaxTokens: schemas.Ptr(1),
					TopP:      schemas.Ptr(0.5),
				}},
			}
			dropped := dropUnsupportedParams(newTestContext(), req, supported)
			if req.TextCompletionRequest.Params.MaxTokens == nil {
				t.Fatal("max_tokens was dropped from a text completion")
			}
			if !slices.Equal(dropped, []string{"top_p"}) {
				t.Fatalf("dropped = %v, want [top_p]", dropped)
			}
		})
	}
}

func TestDropUnsupportedParams_ChatReasoningNilForcedToNoneWithUnsupportedTools(t *testing.T) {
	newChatNoReasoning := func() *schemas.BifrostRequest {
		return &schemas.BifrostRequest{
			RequestType: schemas.ChatCompletionRequest,
			ChatRequest: &schemas.BifrostChatRequest{
				Provider: schemas.OpenAI,
				Model:    "reasoning-no-tools-model",
				Params: &schemas.ChatParameters{
					Tools: []schemas.ChatTool{{
						Type: schemas.ChatToolTypeFunction,
						Function: &schemas.ChatToolFunction{
							Name:        "get_weather",
							Description: schemas.Ptr("Returns weather"),
						},
					}},
				},
			},
		}
	}

	forceNone := newChatNoReasoning()
	dropped := dropUnsupportedParams(newTestContext(), forceNone, []string{"reasoning", "tools", "supports_none_reasoning_effort"})
	if forceNone.ChatRequest.Params.Reasoning == nil {
		t.Fatalf("reasoning = nil, want forced to effort=none when model reasons by default and doesn't support reasoning with tools")
	}
	if got := forceNone.ChatRequest.Params.Reasoning.Effort; got == nil || *got != "none" {
		t.Fatalf("reasoning.effort = %v, want \"none\"", got)
	}
	if forceNone.ChatRequest.Params.Tools == nil {
		t.Fatalf("tools = dropped, want preserved")
	}
	if !slices.Contains(dropped, "reasoning") {
		t.Errorf("reasoning not reported in dropped=%v, want present", dropped)
	}

	noNoneSupport := newChatNoReasoning()
	dropped = dropUnsupportedParams(newTestContext(), noNoneSupport, []string{"reasoning", "tools"})
	if noNoneSupport.ChatRequest.Params.Reasoning != nil {
		t.Fatalf("reasoning = %v, want left nil when model doesn't support effort=none", noNoneSupport.ChatRequest.Params.Reasoning)
	}
	if slices.Contains(dropped, "reasoning") {
		t.Errorf("reasoning reported in dropped=%v, want absent since it was never set", dropped)
	}

	reasoningWithToolsSupported := newChatNoReasoning()
	dropped = dropUnsupportedParams(newTestContext(), reasoningWithToolsSupported, []string{"reasoning", "tools", "reasoning_with_tool_calls", "supports_none_reasoning_effort"})
	if reasoningWithToolsSupported.ChatRequest.Params.Reasoning != nil {
		t.Fatalf("reasoning = %v, want left nil when reasoning_with_tool_calls is supported", reasoningWithToolsSupported.ChatRequest.Params.Reasoning)
	}
	if slices.Contains(dropped, "reasoning") {
		t.Errorf("reasoning reported in dropped=%v, want absent since it was never set", dropped)
	}
}

// TestDropUnsupportedParams_ReasoningSummary pins the summary flags, with effort always
// kept: a detailed/concise summary the model does not support becomes "auto" when
// supports_reasoning_summary_auto holds and is dropped otherwise (Fireworks models on
// Azure Responses accept "auto" and 400 on "concise"/"detailed"), and an unsupported
// "auto" is dropped.
func TestDropUnsupportedParams_ReasoningSummary(t *testing.T) {
	all := []string{"reasoning", "reasoning_summary_detailed", "reasoning_summary_concise", "reasoning_summary_auto"}
	autoOnly := []string{"reasoning", "reasoning_summary_auto"}
	tests := []struct {
		name        string
		supported   []string
		summary     string
		want        string
		wantDropped bool
	}{
		{name: "all supported keeps detailed", supported: all, summary: "detailed", want: "detailed"},
		{name: "all supported keeps concise", supported: all, summary: "concise", want: "concise"},
		{name: "auto only converts detailed", supported: autoOnly, summary: "detailed", want: "auto"},
		{name: "auto only converts concise", supported: autoOnly, summary: "concise", want: "auto"},
		{name: "auto only keeps auto", supported: autoOnly, summary: "auto", want: "auto"},
		{name: "concise unsupported keeps detailed", supported: []string{"reasoning", "reasoning_summary_detailed", "reasoning_summary_auto"}, summary: "detailed", want: "detailed"},
		{name: "concise unsupported converts concise", supported: []string{"reasoning", "reasoning_summary_detailed", "reasoning_summary_auto"}, summary: "concise", want: "auto"},
		{name: "no auto drops detailed", supported: []string{"reasoning"}, summary: "detailed", wantDropped: true},
		{name: "no auto drops auto", supported: []string{"reasoning", "reasoning_summary_detailed", "reasoning_summary_concise"}, summary: "auto", wantDropped: true},
	}

	for _, tt := range tests {
		t.Run(tt.name+"/responses", func(t *testing.T) {
			req := newResponsesRequest(schemas.Azure, "FW-GLM-5.3", &schemas.ResponsesParameters{
				Reasoning: &schemas.ResponsesParametersReasoning{Effort: schemas.Ptr("high"), Summary: schemas.Ptr(tt.summary)},
			})

			dropped := dropUnsupportedParams(newTestContext(), req, tt.supported)
			reasoning := req.ResponsesRequest.Params.Reasoning
			if reasoning == nil || reasoning.Effort == nil || *reasoning.Effort != "high" {
				t.Fatalf("reasoning = %+v, want effort=high kept", reasoning)
			}
			assertReasoningSummary(t, reasoning.Summary, dropped, tt.want, tt.wantDropped)
		})

		t.Run(tt.name+"/chat", func(t *testing.T) {
			req := &schemas.BifrostRequest{
				RequestType: schemas.ChatCompletionRequest,
				ChatRequest: &schemas.BifrostChatRequest{
					Provider: schemas.Azure,
					Model:    "FW-GLM-5.3",
					Params: &schemas.ChatParameters{
						Reasoning: &schemas.ChatReasoning{Effort: schemas.Ptr("high"), Summary: schemas.Ptr(tt.summary)},
					},
				},
			}

			dropped := dropUnsupportedParams(newTestContext(), req, tt.supported)
			reasoning := req.ChatRequest.Params.Reasoning
			if reasoning == nil || reasoning.Effort == nil || *reasoning.Effort != "high" {
				t.Fatalf("reasoning = %+v, want effort=high kept", reasoning)
			}
			assertReasoningSummary(t, reasoning.Summary, dropped, tt.want, tt.wantDropped)
		})
	}
}

func assertReasoningSummary(t *testing.T, got *string, dropped []string, want string, wantDropped bool) {
	t.Helper()
	if wantDropped {
		if got != nil {
			t.Fatalf("reasoning.summary = %q, want dropped", *got)
		}
		if !slices.Contains(dropped, "reasoning.summary") {
			t.Errorf("reasoning.summary not reported in dropped=%v, want present", dropped)
		}
		return
	}
	if got == nil || *got != want {
		gotValue := "<nil>"
		if got != nil {
			gotValue = *got
		}
		t.Fatalf("reasoning.summary = %q, want %q", gotValue, want)
	}
	if slices.Contains(dropped, "reasoning.summary") {
		t.Errorf("reasoning.summary reported in dropped=%v, want absent", dropped)
	}
}
