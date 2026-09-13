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
