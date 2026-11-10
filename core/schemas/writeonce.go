package schemas

// One request, one upstream call (fork, TXE-7044, contract C3). For every request other than
// chat, Bifrost writes the request to the vendor at most once: a second copy of an image, a
// speech request, a batch or a task create is a second charge.
//
// The HTTP transport every provider client shares can resend a request on its own. When a
// request goes out on a keep-alive connection reused from the pool and that connection fails
// before the first response byte, the request is written again on another connection
// (network.StaleConnectionRetryIfErr). That is right when the vendor closed the connection
// before reading the request, and a duplicate when it read the request and then the
// connection died: the two look the same from here.
//
// Chat keeps that resend, because its behaviour must not change (contract C2). Every other
// request type is write-once: the transport checks that a reused connection is still open
// before it writes anything, replaces one that is not, and after the first byte is written
// never sends the request again.

// KeepsStaleConnectionRetry reports whether a request of this type may be written to the vendor
// again when a reused connection fails before the first response byte. Only the chat family
// does: text completion, chat completion and Responses, streamed or not. Every other type,
// including one added later, is write-once.
func (t RequestType) KeepsStaleConnectionRetry() bool {
	switch t {
	case TextCompletionRequest, TextCompletionStreamRequest,
		ChatCompletionRequest, ChatCompletionStreamRequest,
		ResponsesRequest, ResponsesStreamRequest:
		return true
	}
	return false
}
