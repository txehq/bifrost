package anthropicmsg

import (
	"bytes"
	"testing"
)

func TestExtractUsageBillsCompactionWithoutDeclinedFallback(t *testing.T) {
	body := []byte(`{"usage":{"input_tokens":10,"output_tokens":20,"cache_read_input_tokens":30,"cache_creation_input_tokens":40,"iterations":[{"type":"message","input_tokens":999,"output_tokens":999},{"type":"fallback_message","input_tokens":10,"output_tokens":20},{"type":"compaction","input_tokens":100,"output_tokens":200,"cache_read_input_tokens":300,"cache_creation_input_tokens":400}]}}`)
	original := bytes.Clone(body)
	got := ExtractUsage("/v1/messages", nil, body)
	if got == nil || got.LLMUsage == nil {
		t.Fatal("missing usage")
	}
	u := got.LLMUsage
	if u.PromptTokens != 880 || u.CompletionTokens != 220 || u.TotalTokens != 1100 {
		t.Fatalf("billable usage = %+v, want 880 input and 220 output", u)
	}
	if u.PromptTokensDetails == nil || u.PromptTokensDetails.CachedReadTokens != 330 || u.PromptTokensDetails.CachedWriteTokens != 440 {
		t.Fatalf("cache usage = %+v", u.PromptTokensDetails)
	}
	if !bytes.Equal(body, original) {
		t.Fatal("usage extraction changed the native response")
	}
}

func TestExtractUsageCarriesServerToolCounts(t *testing.T) {
	body := []byte(`{"usage":{"input_tokens":10,"output_tokens":20,"server_tool_use":{"web_search_requests":0,"web_fetch_requests":2,"code_execution_requests":1}}}`)
	got := ExtractUsage("/v1/messages", nil, body)
	if got == nil || got.LLMUsage == nil || got.LLMUsage.ToolUsage == nil {
		t.Fatal("missing tool usage")
	}
	tu := got.LLMUsage.ToolUsage
	if tu.WebSearch != nil || tu.WebFetch == nil || tu.WebFetch.NumRequests != 2 || tu.CodeExecution == nil || tu.CodeExecution.NumRequests != 1 {
		t.Fatalf("tool usage = %+v, want web_fetch 2 and code_execution 1", tu)
	}

	var s StreamUsage
	s.ObserveEvent([]byte(`{"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":1,"server_tool_use":{"web_fetch_requests":1}}}}`))
	final := s.ObserveEvent([]byte(`{"type":"message_delta","usage":{"output_tokens":20,"server_tool_use":{"web_fetch_requests":2,"code_execution_requests":1}}}`))
	if final == nil || final.LLMUsage.ToolUsage == nil || final.LLMUsage.ToolUsage.WebFetch.NumRequests != 2 || final.LLMUsage.ToolUsage.CodeExecution.NumRequests != 1 {
		t.Fatalf("stream tool usage = %+v", final)
	}
}
