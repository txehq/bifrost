package openai

import (
	schemas "github.com/capsohq/bifrost/core/schemas"
)

// cacheControlTreatment is what the OpenAI-shape chat encoder does with a per-block
// cache_control marker (on a content block, a tool definition or an assistant tool call) for
// one provider (TXE-8179).
type cacheControlTreatment int

const (
	// cacheControlStripped: the marker is removed before the body is sent.
	cacheControlStripped cacheControlTreatment = iota
	// cacheControlSent: the marker goes to the vendor as sent, whatever the model.
	cacheControlSent
	// cacheControlSentForClaude: the marker goes as sent when the model is Anthropic's, and is
	// removed for every other model the vendor serves.
	cacheControlSentForClaude
)

// cacheControlRule is one provider's entry: the treatment, whether a marker's ttl is delivered
// with it, and the evidence for both.
//
// ttlSent only matters when the marker is sent. When it is false, a sent marker that carries a
// ttl is refused (cacheControlTTLRefusal): the ttl sets the price of the write, and contract C4
// has a price field delivered or refused, never dropped.
type cacheControlRule struct {
	treatment cacheControlTreatment
	ttlSent   bool
	why       string
}

const (
	whyOwnEncoder    = "chat is encoded by the provider's own converter, never by the OpenAI-shape encoder"
	whyNoChat        = "serves no chat completions"
	whyUndocumented  = "its OpenAI-compatible chat API documents no per-block cache_control; an unknown field risks a 400"
	whyOpenAIBreaker = "OpenAI chat has no cache_control; caching intent on gpt-5.6+ goes as prompt_cache_breakpoint (chatUsesPromptCacheBreakpoints), platform.openai.com/docs/guides/prompt-caching"
)

// perBlockCacheControlRules is the one table of which providers' upstreams receive per-block
// cache_control from the OpenAI-shape chat encoder. Every schemas.StandardProviders entry is
// listed, and TestEveryProviderHasAPerBlockCacheControlRule fails on one that is not, so a new
// provider is decided here rather than inheriting a default.
//
// A provider that receives cache_control and bills a cache write by its ttl must also be in the
// cache-control TTL pricing table (TXE-8086); one that strips the marker never writes a cache it
// was asked to.
var perBlockCacheControlRules = map[schemas.ModelProvider]cacheControlRule{
	// OpenRouter forwards Anthropic-style breakpoints to Claude (ttl "1h" included), Gemini and
	// Alibaba models: openrouter.ai/docs/features/prompt-caching.
	schemas.OpenRouter: {cacheControlSent, true, "openrouter.ai/docs/features/prompt-caching: cache_control on content blocks for Anthropic, Gemini and Alibaba models, ttl \"1h\" for Anthropic"},
	// Databricks accepts cache_control only on its Claude endpoints, on text, thinking and image
	// content, tool calls and tool definitions: docs.databricks.com/aws/en/machine-learning/
	// foundation-model-apis/api-reference ("This parameter is only accepted by Databricks-hosted
	// Claude models") and .../model-serving/score-foundation-models (prompt caching). Neither
	// page documents a ttl (only {"type": "ephemeral"}), so a marker with a ttl is refused until a
	// recorded response shows Databricks honours it (TXE-8102).
	schemas.Databricks: {cacheControlSentForClaude, false, "docs.databricks.com/aws/en/machine-learning/foundation-model-apis/api-reference: only accepted by Databricks-hosted Claude models; no ttl documented"},

	schemas.OpenAI: {cacheControlStripped, false, whyOpenAIBreaker},
	schemas.Azure:  {cacheControlStripped, false, whyOpenAIBreaker},
	// Claude on Bedrock and Vertex is encoded natively (Converse cachePoint, Anthropic
	// rawPredict); the models that reach the OpenAI-shape encoder there take no cache_control.
	schemas.Bedrock:       {cacheControlStripped, false, "Claude goes through Converse/Invoke with cachePoint; the OpenAI-compatible models take prompt_cache_breakpoint"},
	schemas.BedrockMantle: {cacheControlStripped, false, "OpenAI-compatible models take prompt_cache_breakpoint, not cache_control"},
	schemas.Vertex:        {cacheControlStripped, false, "Claude goes through Anthropic rawPredict; the OpenAI-compatible MaaS models take no cache_control"},
	// Automatic caching, documented as needing no marker.
	schemas.DeepSeek:  {cacheControlStripped, false, "api-docs.deepseek.com/guides/kv_cache: caching is on by default, no request change"},
	schemas.MiniMax:   {cacheControlStripped, false, "platform.minimax.io/docs/api-reference/text-prompt-caching: automatic caching"},
	schemas.GLM:       {cacheControlStripped, false, "docs.z.ai/guides/capabilities/cache: implicit caching, no manual configuration"},
	schemas.Fireworks: {cacheControlStripped, false, "docs.fireworks.ai/guides/prompt-caching: enabled by default; isolation via prompt_cache_isolation_key"},
	schemas.Moonshot:  {cacheControlStripped, false, "platform.kimi.ai/docs/api/chat: explicit cache breakpoints are not supported (400); caching via prompt_cache_options"},
	// Alibaba documents cache_control on messages (not tools), 5 minutes only, in compatible
	// mode (alibabacloud.com/help/en/model-studio/context-cache). Held stripped: Bifrost does not
	// decode Qwen's cache-write usage, so an enabled write would be priced as plain input.
	schemas.Qwen:      {cacheControlStripped, false, "vendor accepts it (alibabacloud.com/help/en/model-studio/context-cache) but its cache-write usage is not decoded yet; held until both land together"},
	schemas.DashScope: {cacheControlStripped, false, whyNoChat},

	schemas.XAI:           {cacheControlStripped, false, whyUndocumented},
	schemas.GithubCopilot: {cacheControlStripped, false, whyUndocumented},
	schemas.OpencodeZen:   {cacheControlStripped, false, whyUndocumented},
	schemas.OpencodeGo:    {cacheControlStripped, false, whyUndocumented},
	schemas.ModelArk:      {cacheControlStripped, false, whyUndocumented},
	schemas.Volcengine:    {cacheControlStripped, false, whyUndocumented},
	schemas.BytePlus:      {cacheControlStripped, false, whyUndocumented},
	schemas.Groq:          {cacheControlStripped, false, whyUndocumented},
	schemas.Cerebras:      {cacheControlStripped, false, whyUndocumented},
	schemas.Mistral:       {cacheControlStripped, false, whyUndocumented},
	schemas.Nebius:        {cacheControlStripped, false, whyUndocumented},
	schemas.Parasail:      {cacheControlStripped, false, whyUndocumented},
	schemas.Ollama:        {cacheControlStripped, false, whyUndocumented},
	schemas.SGL:           {cacheControlStripped, false, whyUndocumented},
	schemas.VLLM:          {cacheControlStripped, false, whyUndocumented},
	schemas.Wafer:         {cacheControlStripped, false, whyUndocumented},
	schemas.Sarvam:        {cacheControlStripped, false, whyUndocumented},
	schemas.Cloudflare:    {cacheControlStripped, false, whyUndocumented},
	schemas.Runware:       {cacheControlStripped, false, whyUndocumented},
	schemas.Tencent:       {cacheControlStripped, false, whyUndocumented},
	schemas.HuggingFace:   {cacheControlStripped, false, whyUndocumented},

	schemas.Anthropic:  {cacheControlStripped, false, whyOwnEncoder},
	schemas.Gemini:     {cacheControlStripped, false, whyOwnEncoder},
	schemas.Cohere:     {cacheControlStripped, false, whyOwnEncoder},
	schemas.Perplexity: {cacheControlStripped, false, whyOwnEncoder},

	schemas.Deepgram:       {cacheControlStripped, false, whyNoChat},
	schemas.Elevenlabs:     {cacheControlStripped, false, whyNoChat},
	schemas.Replicate:      {cacheControlStripped, false, whyNoChat},
	schemas.Runway:         {cacheControlStripped, false, whyNoChat},
	schemas.Typesafe:       {cacheControlStripped, false, whyNoChat},
	schemas.Voyage:         {cacheControlStripped, false, whyNoChat},
	schemas.BFL:            {cacheControlStripped, false, whyNoChat},
	schemas.GoogleCloudTTS: {cacheControlStripped, false, whyNoChat},
	schemas.AzureSpeech:    {cacheControlStripped, false, whyNoChat},
	schemas.AssemblyAI:     {cacheControlStripped, false, whyNoChat},
	schemas.Kling:          {cacheControlStripped, false, whyNoChat},
}

// perBlockCacheControlOnWire reports whether the OpenAI-shape chat encoder sends per-block
// cache_control for this provider and model. A custom provider is looked up by its base
// provider, so one built on OpenRouter keeps its markers like OpenRouter itself. A provider
// missing from the table strips them.
func perBlockCacheControlOnWire(ctx *schemas.BifrostContext, provider schemas.ModelProvider, model string) bool {
	switch perBlockCacheControlRules[schemas.ResolveBaseProvider(ctx, provider)].treatment {
	case cacheControlSent:
		return true
	case cacheControlSentForClaude:
		return schemas.IsAnthropicModelFamily(ctx, model)
	default:
		return false
	}
}

// cacheControlTTLRefusal refuses a chat request whose per-block cache_control carries a ttl
// when the provider sends the marker but not its ttl: 400 caller_unsupported_parameter naming
// cache_control.ttl. Dropping the ttl would write a 5-minute cache while the caller asked, and
// expects to pay, for another lifetime.
func cacheControlTTLRefusal(ctx *schemas.BifrostContext, bifrostReq *schemas.BifrostChatRequest) *schemas.BifrostError {
	if bifrostReq == nil {
		return nil
	}
	// Only a provider that sends the marker without its ttl can refuse; every other request
	// returns after one map lookup, before the request is walked. For those that can, the walk
	// (no allocation) runs before the model-family check, which resolves the canonical model.
	rule := perBlockCacheControlRules[schemas.ResolveBaseProvider(ctx, bifrostReq.Provider)]
	if rule.ttlSent || rule.treatment == cacheControlStripped {
		return nil
	}
	if !chatCarriesCacheControlTTL(bifrostReq) {
		return nil
	}
	if !perBlockCacheControlOnWire(ctx, bifrostReq.Provider, schemas.ResolveCanonicalModel(ctx, bifrostReq.Model)) {
		return nil
	}
	return schemas.NewUnsupportedParameterError("cache_control.ttl",
		"cache_control.ttl is not supported on provider "+string(bifrostReq.Provider)+
			": it documents cache_control as {\"type\": \"ephemeral\"} only; omit ttl for its default cache lifetime")
}

// chatCarriesCacheControlTTL reports whether any per-block cache_control in the request (on a
// content block, an assistant tool call or a tool definition) sets a ttl.
func chatCarriesCacheControlTTL(req *schemas.BifrostChatRequest) bool {
	hasTTL := func(cc *schemas.CacheControl) bool { return cc != nil && cc.TTL != nil && *cc.TTL != "" }
	for _, msg := range req.Input {
		if msg.Content != nil {
			for _, block := range msg.Content.ContentBlocks {
				if hasTTL(block.CacheControl) {
					return true
				}
			}
		}
		if msg.ChatAssistantMessage != nil {
			for _, call := range msg.ChatAssistantMessage.ToolCalls {
				if hasTTL(call.CacheControl) {
					return true
				}
			}
		}
	}
	if req.Params != nil {
		for _, tool := range req.Params.Tools {
			if hasTTL(tool.CacheControl) {
				return true
			}
		}
	}
	return false
}
