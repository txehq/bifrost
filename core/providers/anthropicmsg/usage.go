// Package anthropicmsg implements native Anthropic Messages passthrough for
// providers that expose an Anthropic-compatible /v1/messages endpoint.
package anthropicmsg

import (
	"strings"

	"github.com/bytedance/sonic"
	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	"github.com/capsohq/bifrost/core/schemas"
)

type cacheCreationUsage struct {
	Ephemeral5mInputTokens int `json:"ephemeral_5m_input_tokens"`
	Ephemeral1hInputTokens int `json:"ephemeral_1h_input_tokens"`
}

type serverToolUseUsage struct {
	WebSearchRequests int `json:"web_search_requests"`
}

type outputTokensDetails struct {
	ThinkingTokens int `json:"thinking_tokens"`
}

type messagesUsage struct {
	Type                     *string              `json:"type,omitempty"`
	Iterations               []messagesUsage      `json:"iterations,omitempty"`
	InputTokens              int                  `json:"input_tokens"`
	OutputTokens             int                  `json:"output_tokens"`
	CacheReadInputTokens     int                  `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int                  `json:"cache_creation_input_tokens"`
	CacheCreation            cacheCreationUsage   `json:"cache_creation"`
	ServerToolUse            *serverToolUseUsage  `json:"server_tool_use,omitempty"`
	OutputTokensDetails      *outputTokensDetails `json:"output_tokens_details,omitempty"`
	ServiceTier              *string              `json:"service_tier,omitempty"`
	Speed                    *string              `json:"speed,omitempty"`
	InferenceGeo             *string              `json:"inference_geo,omitempty"`
}

type messagesResponse struct {
	Usage *messagesUsage `json:"usage,omitempty"`
}

type streamMessage struct {
	Usage *messagesUsage `json:"usage,omitempty"`
}

type streamEvent struct {
	Usage   *messagesUsage `json:"usage,omitempty"`
	Message *streamMessage `json:"message,omitempty"`
}

// ExtractUsage extracts billing usage from a native Anthropic response.
func ExtractUsage(path string, _, body []byte) *schemas.BifrostPassthroughUsage {
	if idx := strings.IndexByte(path, '?'); idx >= 0 {
		path = path[:idx]
	}

	switch {
	case strings.HasSuffix(path, "/messages"):
		return extractMessagesUsage(body)
	case strings.HasSuffix(path, "/complete"):
		return extractCompleteUsage(body)
	default:
		return nil
	}
}

// HasUsage cheaply identifies Anthropic SSE events that carry usage.
func HasUsage(event []byte) bool {
	return providerUtils.GetJSONField(event, "usage").Exists() ||
		providerUtils.GetJSONField(event, "message.usage").Exists()
}

func buildUsage(au *messagesUsage) *schemas.BifrostPassthroughUsage {
	if au == nil {
		return nil
	}
	au = billableUsage(au)
	totalInput := au.InputTokens + au.CacheReadInputTokens + au.CacheCreationInputTokens
	total := totalInput + au.OutputTokens
	if total == 0 {
		return nil
	}

	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     totalInput,
		CompletionTokens: au.OutputTokens,
		TotalTokens:      total,
	}
	if au.CacheReadInputTokens > 0 || au.CacheCreationInputTokens > 0 {
		details := &schemas.ChatPromptTokensDetails{
			CachedReadTokens:  au.CacheReadInputTokens,
			CachedWriteTokens: au.CacheCreationInputTokens,
		}
		if au.CacheCreation.Ephemeral5mInputTokens > 0 || au.CacheCreation.Ephemeral1hInputTokens > 0 {
			details.CachedWriteTokenDetails = &schemas.ChatCachedWriteTokenDetails{
				CachedWriteTokens5m: au.CacheCreation.Ephemeral5mInputTokens,
				CachedWriteTokens1h: au.CacheCreation.Ephemeral1hInputTokens,
			}
		}
		usage.PromptTokensDetails = details
	}
	if au.ServerToolUse != nil && au.ServerToolUse.WebSearchRequests > 0 {
		n := au.ServerToolUse.WebSearchRequests
		usage.CompletionTokensDetails = &schemas.ChatCompletionTokensDetails{NumSearchQueries: &n}
	}
	if au.OutputTokensDetails != nil && au.OutputTokensDetails.ThinkingTokens > 0 {
		if usage.CompletionTokensDetails == nil {
			usage.CompletionTokensDetails = &schemas.ChatCompletionTokensDetails{}
		}
		usage.CompletionTokensDetails.ReasoningTokens = au.OutputTokensDetails.ThinkingTokens
	}

	result := &schemas.BifrostPassthroughUsage{
		LLMUsage:     usage,
		Speed:        au.Speed,
		InferenceGeo: au.InferenceGeo,
	}
	if au.ServiceTier != nil {
		tier := schemas.BifrostServiceTier(*au.ServiceTier)
		switch *au.ServiceTier {
		case "standard":
			tier = schemas.BifrostServiceTierDefault
		case "priority":
			tier = schemas.BifrostServiceTierPriority
		}
		result.ServiceTier = &tier
	}
	return result
}

// StreamUsage incrementally merges usage from Anthropic message_start and
// message_delta events without retaining the stream body.
type StreamUsage struct {
	combined messagesUsage
	seen     bool
}

// ObserveEvent merges one complete SSE data payload into the running usage.
func (a *StreamUsage) ObserveEvent(event []byte) *schemas.BifrostPassthroughUsage {
	var evt streamEvent
	if err := sonic.Unmarshal(event, &evt); err != nil {
		return a.usage()
	}
	u := evt.Usage
	if u == nil && evt.Message != nil {
		u = evt.Message.Usage
	}
	if u == nil {
		return a.usage()
	}
	u = billableUsage(u)

	a.seen = true
	c := &a.combined
	c.InputTokens = max(c.InputTokens, u.InputTokens)
	c.OutputTokens = max(c.OutputTokens, u.OutputTokens)
	c.CacheReadInputTokens = max(c.CacheReadInputTokens, u.CacheReadInputTokens)
	c.CacheCreationInputTokens = max(c.CacheCreationInputTokens, u.CacheCreationInputTokens)
	c.CacheCreation.Ephemeral5mInputTokens = max(c.CacheCreation.Ephemeral5mInputTokens, u.CacheCreation.Ephemeral5mInputTokens)
	c.CacheCreation.Ephemeral1hInputTokens = max(c.CacheCreation.Ephemeral1hInputTokens, u.CacheCreation.Ephemeral1hInputTokens)
	if u.ServerToolUse != nil {
		if c.ServerToolUse == nil {
			c.ServerToolUse = &serverToolUseUsage{}
		}
		c.ServerToolUse.WebSearchRequests = max(c.ServerToolUse.WebSearchRequests, u.ServerToolUse.WebSearchRequests)
	}
	if u.OutputTokensDetails != nil {
		if c.OutputTokensDetails == nil {
			c.OutputTokensDetails = &outputTokensDetails{}
		}
		c.OutputTokensDetails.ThinkingTokens = max(c.OutputTokensDetails.ThinkingTokens, u.OutputTokensDetails.ThinkingTokens)
	}
	if u.ServiceTier != nil {
		c.ServiceTier = u.ServiceTier
	}
	if u.Speed != nil {
		c.Speed = u.Speed
	}
	if u.InferenceGeo != nil {
		c.InferenceGeo = u.InferenceGeo
	}
	return a.usage()
}

func (a *StreamUsage) usage() *schemas.BifrostPassthroughUsage {
	if !a.seen {
		return nil
	}
	return buildUsage(&a.combined)
}

// Only compaction iterations add charges to the top-level serving attempt.
// Declined server-side fallback attempts are not billable.
func billableUsage(u *messagesUsage) *messagesUsage {
	if len(u.Iterations) == 0 {
		return u
	}
	out := *u
	out.Iterations = nil
	out.Type = nil
	if u.OutputTokensDetails != nil {
		details := *u.OutputTokensDetails
		out.OutputTokensDetails = &details
	}
	if u.ServerToolUse != nil {
		tools := *u.ServerToolUse
		out.ServerToolUse = &tools
	}
	for _, it := range u.Iterations {
		if it.Type == nil || *it.Type != "compaction" {
			continue
		}
		out.InputTokens += it.InputTokens
		out.OutputTokens += it.OutputTokens
		out.CacheReadInputTokens += it.CacheReadInputTokens
		out.CacheCreationInputTokens += it.CacheCreationInputTokens
		out.CacheCreation.Ephemeral5mInputTokens += it.CacheCreation.Ephemeral5mInputTokens
		out.CacheCreation.Ephemeral1hInputTokens += it.CacheCreation.Ephemeral1hInputTokens
		if it.OutputTokensDetails != nil && it.OutputTokensDetails.ThinkingTokens > 0 {
			if out.OutputTokensDetails == nil {
				out.OutputTokensDetails = &outputTokensDetails{}
			}
			out.OutputTokensDetails.ThinkingTokens = max(out.OutputTokensDetails.ThinkingTokens, it.OutputTokensDetails.ThinkingTokens)
		}
		if it.ServerToolUse != nil {
			if out.ServerToolUse == nil {
				out.ServerToolUse = &serverToolUseUsage{}
			}
			out.ServerToolUse.WebSearchRequests = max(out.ServerToolUse.WebSearchRequests, it.ServerToolUse.WebSearchRequests)
		}
	}
	return &out
}

func extractMessagesUsage(body []byte) *schemas.BifrostPassthroughUsage {
	if len(body) == 0 {
		return nil
	}
	var resp messagesResponse
	if err := sonic.Unmarshal(body, &resp); err != nil {
		return nil
	}
	return buildUsage(resp.Usage)
}

func extractCompleteUsage(body []byte) *schemas.BifrostPassthroughUsage {
	var resp struct {
		Usage *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if len(body) == 0 || sonic.Unmarshal(body, &resp) != nil || resp.Usage == nil {
		return nil
	}
	total := resp.Usage.InputTokens + resp.Usage.OutputTokens
	if total == 0 {
		return nil
	}
	return &schemas.BifrostPassthroughUsage{LLMUsage: &schemas.BifrostLLMUsage{
		PromptTokens: resp.Usage.InputTokens, CompletionTokens: resp.Usage.OutputTokens, TotalTokens: total,
	}}
}
