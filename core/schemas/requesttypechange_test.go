package schemas

import (
	"reflect"
	"testing"
)

// RequestTypeChange names the conversion core dispatches on BifrostContextKeyChangeRequestType
// for every pair of a request and a target that core converts between, and refuses the others.
func TestRequestTypeChange(t *testing.T) {
	cases := []struct {
		request, target, change RequestType
		ok                      bool
	}{
		{ResponsesRequest, ResponsesRequest, "", true},
		{CountTokensRequest, CountTokensRequest, "", true},
		{ChatCompletionStreamRequest, ChatCompletionStreamRequest, "", true},
		{ResponsesRequest, ChatCompletionRequest, ChatCompletionRequest, true},
		{ResponsesStreamRequest, ChatCompletionStreamRequest, ChatCompletionRequest, true},
		{ChatCompletionRequest, ResponsesRequest, ResponsesRequest, true},
		{ChatCompletionStreamRequest, ResponsesStreamRequest, ResponsesRequest, true},
		{TextCompletionRequest, ChatCompletionRequest, ChatCompletionRequest, true},
		{TextCompletionStreamRequest, ChatCompletionStreamRequest, ChatCompletionRequest, true},
		{DecisionRequest, ResponsesRequest, ResponsesRequest, true},
		// A streamed request is not sent to a target that does not stream, nor the reverse.
		{ResponsesStreamRequest, ChatCompletionRequest, "", false},
		{ResponsesRequest, ChatCompletionStreamRequest, "", false},
		{ResponsesStreamRequest, ResponsesRequest, "", false},
		// Pairs core does not convert.
		{CountTokensRequest, ResponsesRequest, "", false},
		{ResponsesRequest, CountTokensRequest, "", false},
		{ChatCompletionRequest, TextCompletionRequest, "", false},
		{EmbeddingRequest, ChatCompletionRequest, "", false},
		{DecisionRequest, ResponsesStreamRequest, "", false},
	}
	for _, tc := range cases {
		change, ok := RequestTypeChange(tc.request, tc.target)
		if change != tc.change || ok != tc.ok {
			t.Errorf("RequestTypeChange(%s, %s) = %q, %v; want %q, %v", tc.request, tc.target, change, ok, tc.change, tc.ok)
		}
	}
}

// Every usage field a chat completion reports reaches the Responses usage it is converted to:
// input, output and total, the text, audio and image parts, cache reads, cache writes with their
// 5-minute and 1-hour parts, reasoning, predictions, citations, search queries, server-side tool
// calls, duration-based audio and the cost, and a top-level reasoning_tokens not yet folded.
func TestChatUsageMapsWhollyToResponsesUsage(t *testing.T) {
	audioSeconds, citations, searches, outputImages := 2.5, 6, 2, 7
	ticks := int64(1234)
	cost := &BifrostCost{TotalCost: 0.01}
	chat := &BifrostLLMUsage{
		PromptTokens:     1200,
		CompletionTokens: 300,
		TotalTokens:      1500,
		PromptTokensDetails: &ChatPromptTokensDetails{
			TextTokens: 1100, AudioTokens: 50, ImageTokens: 50,
			CachedReadTokens: 400, CachedWriteTokens: 250,
			CachedWriteTokenDetails: &ChatCachedWriteTokenDetails{CachedWriteTokens5m: 150, CachedWriteTokens1h: 100},
		},
		CompletionTokensDetails: &ChatCompletionTokensDetails{
			TextTokens: 170, AcceptedPredictionTokens: 3, AudioTokens: 5, CitationTokens: &citations,
			NumSearchQueries: &searches, ReasoningTokens: 120, ImageTokens: &outputImages, RejectedPredictionTokens: 4,
		},
		AudioSeconds:   &audioSeconds,
		ToolUsage:      &ToolUsage{WebSearch: &WebSearchToolUsage{NumRequests: 2}},
		Cost:           cost,
		CostInUsdTicks: &ticks,
	}
	want := &ResponsesResponseUsage{
		InputTokens:  1200,
		OutputTokens: 300,
		TotalTokens:  1500,
		InputTokensDetails: &ResponsesResponseInputTokens{
			TextTokens: 1100, AudioTokens: 50, ImageTokens: 50,
			CachedReadTokens: 400, CachedWriteTokens: 250,
			CachedWriteTokenDetails: &ChatCachedWriteTokenDetails{CachedWriteTokens5m: 150, CachedWriteTokens1h: 100},
		},
		OutputTokensDetails: &ResponsesResponseOutputTokens{
			TextTokens: 170, AcceptedPredictionTokens: 3, AudioTokens: 5, ImageTokens: &outputImages, ReasoningTokens: 120,
			RejectedPredictionTokens: 4, CitationTokens: &citations, NumSearchQueries: &searches,
		},
		AudioSeconds:   &audioSeconds,
		Cost:           cost,
		ToolUsage:      &ToolUsage{WebSearch: &WebSearchToolUsage{NumRequests: 2}},
		CostInUsdTicks: &ticks,
	}
	if got := chat.ToResponsesResponseUsage(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Responses usage\n%+v\nwant\n%+v", got, want)
	}
	if back := want.ToBifrostLLMUsage(); !reflect.DeepEqual(back.PromptTokensDetails, chat.PromptTokensDetails) ||
		!reflect.DeepEqual(back.CompletionTokensDetails, chat.CompletionTokensDetails) || back.AudioSeconds != chat.AudioSeconds {
		t.Fatalf("the Responses usage does not convert back to the chat usage: %+v", back)
	}

	sglang := &BifrostLLMUsage{PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30, ReasoningTokens: 15}
	if got := sglang.ToResponsesResponseUsage().OutputTokensDetails; got == nil || got.ReasoningTokens != 15 {
		t.Fatalf("a top-level reasoning_tokens maps to output details %+v, want 15 reasoning tokens", got)
	}
}
