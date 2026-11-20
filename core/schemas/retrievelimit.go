package schemas

import "strings"

// A retrieve is read under a quarter of the response limit (fork, TXE-8031). A retrieve answers
// with one resource the vendor holds (a batch job, a file's or a container's metadata, a video or
// image task, a stored response), and the gateway converts it before answering: it parses the
// body, keeps the parts the route returns and encodes the route's shape. Each step holds another
// response-sized buffer, so a retrieve read under the whole of client.max_response_body_size_mb
// could hold several times that limit at once. A Gemini inline batch job carries every result
// inline (up to 200,000), which is how one caller could reach it. Downloads (file content, video
// content, batch results) are not retrieves: they keep the whole limit, and are streamed or paged.

// IsRetrieve reports whether a request of this type is a retrieve: every type named *_retrieve,
// including one added later. A streamed retrieve (responses_retrieve_stream) is not one: its body
// is relayed as it arrives, never held whole.
func (t RequestType) IsRetrieve() bool {
	return strings.HasSuffix(string(t), "_retrieve")
}
