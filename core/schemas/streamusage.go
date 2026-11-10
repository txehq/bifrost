package schemas

// MergeStreamFrame folds the usage reported on one streamed frame into u, the usage a stream
// accumulator attaches to its final frame (fork, TXE-6522, contract C10 BF21).
//
// The final frame's usage has to equal the usage of the same request made without streaming,
// field for field: Tokun settles on it. Accumulators used to copy only the token counts, the token
// details and cost, so tool_usage (billed web search calls) and cost_in_usd_ticks (xAI's own
// charge) never reached the final frame. This method is the one place that knows every field:
//
//   - the token counts take the largest value seen, because some upstreams (vLLM's
//     stream_continuous_usage_stats) report running totals and the last frame is not always the
//     largest; total_tokens is never less than prompt plus completion;
//   - every other field is taken from the latest frame that reports it, because vendors that
//     report it send it once, on the trailing usage frame.
//
// BillingTurns is not merged: only AddBillingTurn sets it, and no vendor frame carries it.
// TestMergeStreamFrameCarriesEveryField fails when a field is added to BifrostLLMUsage and not
// handled here.
func (u *BifrostLLMUsage) MergeStreamFrame(frame *BifrostLLMUsage) {
	if u == nil || frame == nil {
		return
	}
	u.PromptTokens = max(u.PromptTokens, frame.PromptTokens)
	u.CompletionTokens = max(u.CompletionTokens, frame.CompletionTokens)
	u.ReasoningTokens = max(u.ReasoningTokens, frame.ReasoningTokens)
	u.TotalTokens = max(u.TotalTokens, frame.TotalTokens, u.PromptTokens+u.CompletionTokens)

	if frame.PromptTokensDetails != nil {
		u.PromptTokensDetails = frame.PromptTokensDetails
	}
	if frame.CompletionTokensDetails != nil {
		u.CompletionTokensDetails = frame.CompletionTokensDetails
	}
	if frame.AudioSeconds != nil {
		u.AudioSeconds = frame.AudioSeconds
	}
	if frame.SearchUnits != nil {
		u.SearchUnits = frame.SearchUnits
	}
	if frame.ToolUsage != nil {
		u.ToolUsage = frame.ToolUsage
	}
	if frame.Cost != nil {
		u.Cost = frame.Cost
	}
	if frame.CostInUsdTicks != nil {
		u.CostInUsdTicks = frame.CostInUsdTicks
	}
	if frame.StateTokens != nil {
		u.StateTokens = frame.StateTokens
	}
	if frame.StateTokensDropped != nil {
		u.StateTokensDropped = frame.StateTokensDropped
	}
	if frame.Truncated != nil {
		u.Truncated = frame.Truncated
	}
	if frame.TruncatedQuestions != nil {
		u.TruncatedQuestions = frame.TruncatedQuestions
	}
	if frame.Speed != nil {
		u.Speed = frame.Speed
	}
	if frame.InferenceGeo != nil {
		u.InferenceGeo = frame.InferenceGeo
	}
	if frame.ServiceTier != nil {
		u.ServiceTier = frame.ServiceTier
	}
	if frame.ServerSideFallbackModel != nil {
		u.ServerSideFallbackModel = frame.ServerSideFallbackModel
	}
}
