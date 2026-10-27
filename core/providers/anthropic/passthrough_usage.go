package anthropic

import (
	"strings"

	"github.com/capsohq/bifrost/core/providers/anthropicmsg"
	"github.com/capsohq/bifrost/core/schemas"
)

// ClassifyPassthroughPath maps an Anthropic passthrough path to the Bifrost request type it
// addresses: /messages is a Responses-shaped request (Bifrost routes Anthropic Messages through
// the Responses schema), /messages/count_tokens is a token count and /complete a legacy text
// completion. A query string is ignored; anything else returns "". Usage extraction and passthrough
// log parsing both dispatch on this.
func ClassifyPassthroughPath(path string) schemas.RequestType {
	if idx := strings.IndexByte(path, '?'); idx >= 0 {
		path = path[:idx]
	}

	switch {
	case strings.HasSuffix(path, "/messages"):
		return schemas.ResponsesRequest
	case strings.HasSuffix(path, "/messages/count_tokens"):
		return schemas.CountTokensRequest
	case strings.HasSuffix(path, "/complete"):
		return schemas.TextCompletionRequest
	}
	return ""
}

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
