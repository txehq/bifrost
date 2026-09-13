package anthropic

import (
	"github.com/capsohq/bifrost/core/providers/anthropicmsg"
	"github.com/capsohq/bifrost/core/schemas"
)

// ExtractAnthropicPassthroughUsage is retained for provider compatibility. The
// shared native Messages component owns the implementation.
func ExtractAnthropicPassthroughUsage(path string, requestBody, body []byte) *schemas.BifrostPassthroughUsage {
	return anthropicmsg.ExtractUsage(path, requestBody, body)
}

// ExtractAnthropicMessagesUsage supports native endpoints such as Vertex rawPredict.
func ExtractAnthropicMessagesUsage(body []byte) *schemas.BifrostPassthroughUsage {
	return anthropicmsg.ExtractUsage("/v1/messages", nil, body)
}

// HasAnthropicPassthroughUsage is retained for existing passthrough callers.
func HasAnthropicPassthroughUsage(event []byte) bool {
	return anthropicmsg.HasUsage(event)
}

// AnthropicPassthroughStreamUsage aliases the shared incremental usage observer.
type AnthropicPassthroughStreamUsage = anthropicmsg.StreamUsage
