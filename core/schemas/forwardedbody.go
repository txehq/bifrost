package schemas

import (
	"bytes"
	"mime/multipart"
	"strconv"
	"strings"
	"unsafe"

	"github.com/tidwall/gjson"
)

// Forwarded bodies and the parser differential (fork, TXE-7994).
//
// A forwarded body is sent to the provider as the caller wrote it: a passthrough request's Body,
// or a RawRequestBody in raw request body mode (BifrostContextKeyUseRawRequestBody). Bifrost
// still reads such a body before it leaves: the access gates read the models, storage URIs and
// topics it names, the router reads its model for key selection, and providers read pricing
// dimensions from it. Each reader uses whatever parser suits it (gjson keeps the first of two
// equal keys, encoding/json and sonic keep the last), and the vendor uses its own. A body that
// gives one key twice in an object is therefore read one way by a gate and another way by the
// vendor: a key restricted to model A sends {"model":"A","model":"B"} and runs B.
//
// The rule: a forwarded body that repeats a member is refused before any plugin reads it
// (core's RunLLMPreHooks), so every reader, present or future, sees a body that every parser
// reads the same way. AmbiguousBodyMember is the one check; ForwardedBody is the one accessor
// that finds the body on whichever request variant carries it.

// maxForwardedBodyDepth bounds the JSON nesting AmbiguousBodyMember follows. A body nested
// deeper is refused as ambiguous: no API nests this far, and an unbounded walk is a stack to
// exhaust.
const maxForwardedBodyDepth = 512

// ForwardedBody returns the body the active request variant forwards to the provider as it was
// written: the passthrough body, or the raw request body set in raw request body mode. It is nil
// for a request that forwards no body. TestForwardedBodyCoversEveryRawBodyField fails when a
// request variant gains a raw body this does not return.
func (br *BifrostRequest) ForwardedBody() []byte {
	switch {
	case br.PassthroughRequest != nil:
		return br.PassthroughRequest.Body
	case br.TextCompletionRequest != nil:
		return br.TextCompletionRequest.RawRequestBody
	case br.ChatRequest != nil:
		return br.ChatRequest.RawRequestBody
	case br.ResponsesRequest != nil:
		return br.ResponsesRequest.RawRequestBody
	case br.ResponsesRetrieveRequest != nil:
		return br.ResponsesRetrieveRequest.RawRequestBody
	case br.ResponsesDeleteRequest != nil:
		return br.ResponsesDeleteRequest.RawRequestBody
	case br.ResponsesCancelRequest != nil:
		return br.ResponsesCancelRequest.RawRequestBody
	case br.ResponsesInputItemsRequest != nil:
		return br.ResponsesInputItemsRequest.RawRequestBody
	case br.CountTokensRequest != nil:
		return br.CountTokensRequest.RawRequestBody
	case br.CompactionRequest != nil:
		return br.CompactionRequest.RawRequestBody
	case br.EmbeddingRequest != nil:
		return br.EmbeddingRequest.RawRequestBody
	case br.RerankRequest != nil:
		return br.RerankRequest.RawRequestBody
	case br.DecisionRequest != nil:
		return br.DecisionRequest.RawRequestBody
	case br.OCRRequest != nil:
		return br.OCRRequest.RawRequestBody
	case br.SpeechRequest != nil:
		return br.SpeechRequest.RawRequestBody
	case br.TranscriptionRequest != nil:
		return br.TranscriptionRequest.RawRequestBody
	case br.ImageGenerationRequest != nil:
		return br.ImageGenerationRequest.RawRequestBody
	case br.ImageEditRequest != nil:
		return br.ImageEditRequest.RawRequestBody
	case br.ImageVariationRequest != nil:
		return br.ImageVariationRequest.RawRequestBody
	case br.VideoGenerationRequest != nil:
		return br.VideoGenerationRequest.RawRequestBody
	case br.VideoEditRequest != nil:
		return br.VideoEditRequest.RawRequestBody
	case br.VideoRemixRequest != nil:
		return br.VideoRemixRequest.RawRequestBody
	case br.FileListRequest != nil:
		return br.FileListRequest.RawRequestBody
	case br.FileRetrieveRequest != nil:
		return br.FileRetrieveRequest.RawRequestBody
	case br.FileDeleteRequest != nil:
		return br.FileDeleteRequest.RawRequestBody
	case br.FileContentRequest != nil:
		return br.FileContentRequest.RawRequestBody
	case br.CachedContentCreateRequest != nil:
		return br.CachedContentCreateRequest.RawRequestBody
	case br.CachedContentListRequest != nil:
		return br.CachedContentListRequest.RawRequestBody
	case br.CachedContentRetrieveRequest != nil:
		return br.CachedContentRetrieveRequest.RawRequestBody
	case br.CachedContentUpdateRequest != nil:
		return br.CachedContentUpdateRequest.RawRequestBody
	case br.CachedContentDeleteRequest != nil:
		return br.CachedContentDeleteRequest.RawRequestBody
	case br.BatchCreateRequest != nil:
		return br.BatchCreateRequest.RawRequestBody
	case br.BatchRetrieveRequest != nil:
		return br.BatchRetrieveRequest.RawRequestBody
	case br.BatchCancelRequest != nil:
		return br.BatchCancelRequest.RawRequestBody
	case br.BatchResultsRequest != nil:
		return br.BatchResultsRequest.RawRequestBody
	case br.BatchDeleteRequest != nil:
		return br.BatchDeleteRequest.RawRequestBody
	}
	return nil
}

// AmbiguousBodyMember reports the first member of a forwarded body that two parsers can read
// differently, as a path ("model", "tasks[0].model", "multipart field size"), and true; or "" and
// false for a body every parser reads the same way.
//
//   - JSON: an object anywhere in the document that gives one key twice. Keys are compared as
//     decoded, so "model" and "model" are the same key, as they are to every parser. A
//     document nested deeper than maxForwardedBodyDepth is refused too.
//   - multipart/form-data (recognised by its opening boundary line): a text field given twice,
//     unless its name ends in "[]", the form encoding of a list. A file part may repeat: a list
//     of images is sent that way.
//
// Anything else (an empty body, plain text, binary audio, JSON that does not parse) is not
// ambiguous: either nothing reads it, or the route has already refused it as malformed.
func AmbiguousBodyMember(body []byte) (string, bool) {
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	if len(trimmed) == 0 {
		return "", false
	}
	switch trimmed[0] {
	case '{', '[':
		if !gjson.ValidBytes(trimmed) {
			return "", false
		}
		// The body is not written while it is read, so it is viewed as a string, not copied.
		path := make([]bodyPathSegment, 0, 8)
		return ambiguousJSONMember(gjson.Parse(unsafe.String(unsafe.SliceData(trimmed), len(trimmed))), &path, 0)
	case '-':
		return ambiguousMultipartField(trimmed)
	}
	return "", false
}

// bodyPathSegment is one step from the document root: an object member or a list index.
type bodyPathSegment struct {
	key   string
	index int // -1 for an object member
}

// ambiguousJSONMember walks value depth first; path holds the steps leading to it, and is
// rendered only when a repeat is found, so a clean body costs no string building.
func ambiguousJSONMember(value gjson.Result, path *[]bodyPathSegment, depth int) (string, bool) {
	if !value.IsObject() && !value.IsArray() {
		return "", false
	}
	if depth >= maxForwardedBodyDepth {
		return renderBodyPath(*path, "") + " (nested more than " + strconv.Itoa(maxForwardedBodyDepth) + " levels)", true
	}
	var found string
	var ambiguous bool
	if value.IsArray() {
		index := 0
		value.ForEach(func(_, element gjson.Result) bool {
			if element.IsObject() || element.IsArray() {
				*path = append(*path, bodyPathSegment{index: index})
				found, ambiguous = ambiguousJSONMember(element, path, depth+1)
				*path = (*path)[:len(*path)-1]
			}
			index++
			return !ambiguous
		})
		return found, ambiguous
	}
	// Most objects carry a handful of keys: a slice is cheaper than a map until they do not.
	var keys [16]string
	small := keys[:0]
	var large map[string]struct{}
	value.ForEach(func(key, member gjson.Result) bool {
		name := key.Str
		if large != nil {
			if _, seen := large[name]; seen {
				found, ambiguous = renderBodyPath(*path, name), true
				return false
			}
			large[name] = struct{}{}
		} else {
			for _, seen := range small {
				if seen == name {
					found, ambiguous = renderBodyPath(*path, name), true
					return false
				}
			}
			if len(small) == len(keys) {
				large = make(map[string]struct{}, 2*len(keys))
				for _, seen := range small {
					large[seen] = struct{}{}
				}
				large[name] = struct{}{}
			} else {
				small = append(small, name)
			}
		}
		if member.IsObject() || member.IsArray() {
			*path = append(*path, bodyPathSegment{key: name, index: -1})
			found, ambiguous = ambiguousJSONMember(member, path, depth+1)
			*path = (*path)[:len(*path)-1]
		}
		return !ambiguous
	})
	return found, ambiguous
}

// renderBodyPath writes the steps leading to an object and, when key is set, the key it
// repeats: "model", "tasks[0].model", "[1].model".
func renderBodyPath(path []bodyPathSegment, key string) string {
	var b strings.Builder
	for _, step := range path {
		if step.index >= 0 {
			b.WriteString("[" + strconv.Itoa(step.index) + "]")
			continue
		}
		if b.Len() > 0 {
			b.WriteString(".")
		}
		b.WriteString(step.key)
	}
	if key != "" {
		if b.Len() > 0 {
			b.WriteString(".")
		}
		b.WriteString(key)
	}
	return b.String()
}

// ambiguousMultipartField reads a multipart/form-data body by the boundary its first line opens
// with and reports a text field given twice.
func ambiguousMultipartField(body []byte) (string, bool) {
	line, _, _ := bytes.Cut(body, []byte("\n"))
	line = bytes.TrimRight(line, "\r")
	if !bytes.HasPrefix(line, []byte("--")) || len(line) <= 2 {
		return "", false
	}
	reader := multipart.NewReader(bytes.NewReader(body), string(line[2:]))
	seen := map[string]struct{}{}
	for {
		part, err := reader.NextPart()
		if err != nil {
			return "", false
		}
		name, file := part.FormName(), part.FileName() != ""
		part.Close()
		if name == "" || file || strings.HasSuffix(name, "[]") {
			continue
		}
		if _, repeated := seen[name]; repeated {
			return "multipart field " + name, true
		}
		seen[name] = struct{}{}
	}
}
