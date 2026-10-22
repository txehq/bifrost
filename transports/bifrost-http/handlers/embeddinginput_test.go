package handlers

import (
	"testing"

	"github.com/bytedance/sonic"
)

// TestEmbeddingRequestInputLastDuplicateWins pins that a body repeating "input"
// embeds the last value, as JSON decoding reads every other field, instead of an
// earlier one of a different shape the decoder left set.
func TestEmbeddingRequestInputLastDuplicateWins(t *testing.T) {
	var req EmbeddingRequest
	if err := sonic.Unmarshal([]byte(`{"input":"first","input":[15339,1917]}`), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	items := req.Input.toEmbeddingInput()
	if len(items) != 1 || len(items[0].Content) != 1 || items[0].Content[0].Text != nil || len(items[0].Content[0].Tokens) != 2 {
		t.Fatalf("embedded %+v, want the last input's two tokens", items)
	}
}
