package schemas

// RequestTypeChange returns the BifrostContextKeyChangeRequestType core needs to send a
// request of type requestType to a target that takes the type target, and whether core can
// do it at all.
//
// A target of requestType itself needs no change (change is empty). A target of another type
// must stream when requestType streams, and the pair must be one core converts on
// BifrostContextKeyChangeRequestType: a text completion sent as a chat completion, a chat
// completion as a Responses request and a Responses request as a chat completion (each
// streamed or not), and a decision answered through the Responses API. Every other pair is
// not convertible.
func RequestTypeChange(requestType, target RequestType) (change RequestType, ok bool) {
	if requestType == target {
		return "", true
	}
	from, fromStream := streamlessRequestType(requestType)
	to, toStream := streamlessRequestType(target)
	if fromStream != toStream {
		return "", false
	}
	switch {
	case from == TextCompletionRequest && to == ChatCompletionRequest:
		return ChatCompletionRequest, true
	case from == ChatCompletionRequest && to == ResponsesRequest:
		return ResponsesRequest, true
	case from == ResponsesRequest && to == ChatCompletionRequest:
		return ChatCompletionRequest, true
	case from == DecisionRequest && to == ResponsesRequest && !fromStream:
		return ResponsesRequest, true
	}
	return "", false
}

// streamlessRequestType returns the non-streaming type of a text completion, chat
// completion or Responses request type, and whether requestType is the streaming one. Any
// other type is returned as it is.
func streamlessRequestType(requestType RequestType) (RequestType, bool) {
	switch requestType {
	case TextCompletionStreamRequest:
		return TextCompletionRequest, true
	case ChatCompletionStreamRequest:
		return ChatCompletionRequest, true
	case ResponsesStreamRequest:
		return ResponsesRequest, true
	}
	return requestType, false
}
