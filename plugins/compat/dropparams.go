package compat

import (
	"fmt"
	"strings"

	"github.com/capsohq/bifrost/core/schemas"
)

// dropUnsupportedParams removes unsupported model parameters from a request in place.
//
// A price-affecting field the caller sent is never dropped or rewritten, whatever the
// catalog lists: the output cap (max_completion_tokens, max_output_tokens, max_tokens),
// service_tier, prompt_cache_key, prompt_cache_retention and reasoning (effort and
// budget). They decide what the caller is billed, so the request goes on with them as
// sent and the provider delivers or refuses them (contract C4). Dropping one would serve
// and bill a different request than the caller sent, with only
// extra_fields.dropped_compat_plugin_params to say so. reasoning.summary is a
// presentation option, not one of them, and is still fitted.
func dropUnsupportedParams(ctx *schemas.BifrostContext, req *schemas.BifrostRequest, supportedParams []string) []string {
	if req == nil {
		return nil
	}

	isSupported := make(map[string]bool, len(supportedParams))
	for _, param := range supportedParams {
		isSupported[param] = true
	}

	var dropped []string

	if req.ChatRequest != nil && req.ChatRequest.Params != nil {
		params := req.ChatRequest.Params
		hasSupportedTools := len(params.Tools) > 0 && isSupported["tools"]

		if params.Audio != nil && !isSupported["audio"] {
			params.Audio = nil
			dropped = append(dropped, "audio")
		}
		if params.FrequencyPenalty != nil && !isSupported["frequency_penalty"] {
			params.FrequencyPenalty = nil
			dropped = append(dropped, "frequency_penalty")
		}
		if params.LogitBias != nil && !isSupported["logit_bias"] {
			params.LogitBias = nil
			dropped = append(dropped, "logit_bias")
		}
		if params.LogProbs != nil && !isSupported["logprobs"] {
			params.LogProbs = nil
			dropped = append(dropped, "logprobs")
		}
		if params.Metadata != nil && !isSupported["metadata"] {
			params.Metadata = nil
			dropped = append(dropped, "metadata")
		}
		if params.ParallelToolCalls != nil && !isSupported["parallel_tool_calls"] {
			params.ParallelToolCalls = nil
			dropped = append(dropped, "parallel_tool_calls")
		}
		if params.Prediction != nil && !isSupported["prediction"] {
			params.Prediction = nil
			dropped = append(dropped, "prediction")
		}
		if params.PresencePenalty != nil && !isSupported["presence_penalty"] {
			params.PresencePenalty = nil
			dropped = append(dropped, "presence_penalty")
		}
		// Reasoning the caller sent stays as sent. When the caller sent none, a model that
		// reasons by default but rejects reasoning alongside tools gets effort "none", so
		// the tools request is accepted; nothing the caller sent is changed by that.
		if params.Reasoning == nil && isSupported["reasoning"] && isSupported["supports_none_reasoning_effort"] && hasSupportedTools && !isSupported["reasoning_with_tool_calls"] && !isConvertedToResponses(ctx) {
			params.Reasoning = &schemas.ChatReasoning{Effort: new("none")}
			dropped = append(dropped, "reasoning")
		}
		if params.Reasoning != nil && params.Reasoning.Summary != nil {
			var droppedSummary bool
			params.Reasoning.Summary, droppedSummary = fitReasoningSummary(ctx, params.Reasoning.Summary, isSupported)
			if droppedSummary {
				dropped = append(dropped, "reasoning.summary")
			}
		}
		if params.ResponseFormat != nil && !isSupported["response_format"] {
			params.ResponseFormat = nil
			dropped = append(dropped, "response_format")
		}
		if params.Seed != nil && !isSupported["seed"] {
			params.Seed = nil
			dropped = append(dropped, "seed")
		}
		if len(params.Stop) > 0 && !isSupported["stop"] {
			params.Stop = nil
			dropped = append(dropped, "stop")
		}
		if params.Temperature != nil && !isSupported["temperature"] {
			params.Temperature = nil
			dropped = append(dropped, "temperature")
		}
		if params.TopLogProbs != nil && !isSupported["top_logprobs"] {
			params.TopLogProbs = nil
			dropped = append(dropped, "top_logprobs")
		}
		if params.TopP != nil && !isSupported["top_p"] {
			params.TopP = nil
			dropped = append(dropped, "top_p")
		}
		if params.ToolChoice != nil && !isSupported["tool_choice"] {
			params.ToolChoice = nil
			dropped = append(dropped, "tool_choice")
		}
		if len(params.Tools) > 0 && !isSupported["tools"] {
			params.Tools = nil
			dropped = append(dropped, "tools")
		}
		if params.Verbosity != nil && !isSupported["verbosity"] {
			params.Verbosity = nil
			dropped = append(dropped, "verbosity")
		}
		if params.WebSearchOptions != nil && !isSupported["web_search_options"] {
			params.WebSearchOptions = nil
			dropped = append(dropped, "web_search_options")
		}
	}

	if req.ResponsesRequest != nil && req.ResponsesRequest.Params != nil {
		params := req.ResponsesRequest.Params

		if params.MaxToolCalls != nil && !isSupported["max_tool_calls"] {
			params.MaxToolCalls = nil
			dropped = append(dropped, "max_tool_calls")
		}
		if params.Metadata != nil && !isSupported["metadata"] {
			params.Metadata = nil
			dropped = append(dropped, "metadata")
		}
		if params.ParallelToolCalls != nil && !isSupported["parallel_tool_calls"] {
			params.ParallelToolCalls = nil
			dropped = append(dropped, "parallel_tool_calls")
		}
		if params.Reasoning != nil {
			var droppedSummary, droppedGenerate bool
			params.Reasoning.Summary, droppedSummary = fitReasoningSummary(ctx, params.Reasoning.Summary, isSupported)
			params.Reasoning.GenerateSummary, droppedGenerate = fitReasoningSummary(ctx, params.Reasoning.GenerateSummary, isSupported)
			if droppedSummary || droppedGenerate {
				dropped = append(dropped, "reasoning.summary")
			}
		}
		if params.Temperature != nil && !isSupported["temperature"] {
			params.Temperature = nil
			dropped = append(dropped, "temperature")
		}
		if params.Text != nil && !isSupported["text"] {
			params.Text = nil
			dropped = append(dropped, "text")
		}
		if params.TopLogProbs != nil && !isSupported["top_logprobs"] {
			params.TopLogProbs = nil
			dropped = append(dropped, "top_logprobs")
		}
		if params.TopP != nil && !isSupported["top_p"] {
			params.TopP = nil
			dropped = append(dropped, "top_p")
		}
		if params.ToolChoice != nil && !isSupported["tool_choice"] {
			params.ToolChoice = nil
			dropped = append(dropped, "tool_choice")
		}
		if len(params.Tools) > 0 && !isSupported["tools"] {
			params.Tools = nil
			dropped = append(dropped, "tools")
		}
		if !isSupported["web_search"] {
			droppedKeys := dropWebsearchToolCalls(req)
			if len(droppedKeys) > 0 {
				ctx.Log(schemas.LogLevelWarn, fmt.Sprintf("dropped %d web search tool(s) - the model does not support web_search: %s", len(droppedKeys), strings.Join(droppedKeys, ", ")))
			}
			dropped = append(dropped, droppedKeys...)
		}
	}

	if req.TextCompletionRequest != nil && req.TextCompletionRequest.Params != nil {
		params := req.TextCompletionRequest.Params

		if params.FrequencyPenalty != nil && !isSupported["frequency_penalty"] {
			params.FrequencyPenalty = nil
			dropped = append(dropped, "frequency_penalty")
		}
		if params.LogitBias != nil && !isSupported["logit_bias"] {
			params.LogitBias = nil
			dropped = append(dropped, "logit_bias")
		}
		if params.LogProbs != nil && !isSupported["logprobs"] {
			params.LogProbs = nil
			dropped = append(dropped, "logprobs")
		}
		if params.N != nil && !isSupported["n"] {
			params.N = nil
			dropped = append(dropped, "n")
		}
		if params.PresencePenalty != nil && !isSupported["presence_penalty"] {
			params.PresencePenalty = nil
			dropped = append(dropped, "presence_penalty")
		}
		if params.Seed != nil && !isSupported["seed"] {
			params.Seed = nil
			dropped = append(dropped, "seed")
		}
		if len(params.Stop) > 0 && !isSupported["stop"] {
			params.Stop = nil
			dropped = append(dropped, "stop")
		}
		if params.Temperature != nil && !isSupported["temperature"] {
			params.Temperature = nil
			dropped = append(dropped, "temperature")
		}
		if params.TopP != nil && !isSupported["top_p"] {
			params.TopP = nil
			dropped = append(dropped, "top_p")
		}
	}

	if !isSupported["assistant_prefill"] {
		ctx.Log(schemas.LogLevelDebug, "model does not support assistant prefill, assistant messages will be trimmed")
	}
	ctx.SetValue(schemas.BifrostContextKeySupportsAssistantPrefill, isSupported["assistant_prefill"])

	return dropped
}

// fitReasoningSummary fits a reasoning.summary to the summary values the model
// accepts: an unsupported "detailed"/"concise" becomes "auto" when the model accepts
// "auto", and is dropped otherwise. Reports whether the summary was dropped.
func fitReasoningSummary(ctx *schemas.BifrostContext, summary *string, isSupported map[string]bool) (*string, bool) {
	if summary == nil {
		return nil, false
	}
	switch *summary {
	case "auto":
		if !isSupported["reasoning_summary_auto"] {
			return nil, true
		}
	case "detailed", "concise":
		if isSupported["reasoning_summary_"+*summary] {
			return summary, false
		}
		if !isSupported["reasoning_summary_auto"] {
			return nil, true
		}
		ctx.Log(schemas.LogLevelWarn, fmt.Sprintf("converted reasoning.summary %q to \"auto\" - the model does not accept %q", *summary, *summary))
		return schemas.Ptr("auto"), false
	}
	return summary, false
}

// dropWebsearchToolCalls drops web search tool calls from the request
func dropWebsearchToolCalls(req *schemas.BifrostRequest) []string {
	dropped := []string{}
	tools := req.ResponsesRequest.Params.Tools
	kept := tools[:0]
	for i, tool := range tools {
		if tool.Type == schemas.ResponsesToolTypeWebSearch || tool.Type == schemas.ResponsesToolTypeWebSearchPreview {
			dropped = append(dropped, fmt.Sprintf("tools[%d].%s", i, tool.Type))
		} else {
			kept = append(kept, tool)
		}
	}
	req.ResponsesRequest.Params.Tools = kept
	return dropped
}

// isConvertedToResponses reports whether an earlier hook already marked the request
// for conversion to Responses.
func isConvertedToResponses(ctx *schemas.BifrostContext) bool {
	if ctx == nil {
		return false
	}
	changeType, ok := ctx.Value(schemas.BifrostContextKeyChangeRequestType).(schemas.RequestType)
	return ok && changeType == schemas.ResponsesRequest
}
