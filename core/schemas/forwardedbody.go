package schemas

import (
	"bytes"
	"io"
	"mime"
	"mime/multipart"
	"slices"
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

// ForwardedBodyContentType returns the media type the caller declared for the forwarded body: the
// Content-Type a passthrough request forwards. A raw request body is always JSON (the native
// routes that set one decode it as JSON first), so it declares none and is read as JSON.
func (br *BifrostRequest) ForwardedBodyContentType() string {
	if br.PassthroughRequest == nil {
		return ""
	}
	for name, value := range br.PassthroughRequest.SafeHeaders {
		if strings.EqualFold(name, "content-type") {
			return value
		}
	}
	return ""
}

// AmbiguousBodyMember reports the first member of a forwarded body that two parsers can read
// differently, as a path ("model", "tasks[0].model", "multipart field size"), and true; or "" and
// false for a body every parser reads the same way. contentType is the caller's declared
// Content-Type, "" when none is known.
//
//   - JSON (declared, or a body opening with "{" or "[" after a UTF-8 byte order mark and
//     whitespace): an object anywhere in the document that gives one key twice, compared as
//     decoded, so "model" and "mod\u0065l" are the same key. A gated key (gatedBodyKeys) is also
//     compared without case, since encoding/json and sonic fill a struct field from any casing.
//     A document nested deeper than maxForwardedBodyDepth is refused, and so is one that does not
//     parse strictly: a lenient vendor parser might read members gjson does not see.
//   - multipart/form-data (declared, read by the declared boundary; or, with no declared type, a
//     body opening with a boundary line): a name given twice when any of its parts is a text
//     field, unless the name ends in "[]", the form encoding of a list. A name only ever given as
//     a file may repeat: a list of images is sent that way. A body that does not parse as
//     multipart is refused.
//
// Anything else (an empty body, plain text, binary audio) is not ambiguous: nothing reads it as
// structured data.
func AmbiguousBodyMember(body []byte, contentType string) (string, bool) {
	trimmed := bytes.TrimPrefix(bytes.TrimLeft(body, " \t\r\n"), utf8ByteOrderMark)
	trimmed = bytes.TrimLeft(trimmed, " \t\r\n")
	mediaType, params, mimeErr := mime.ParseMediaType(contentType)
	if mimeErr == nil && strings.HasPrefix(mediaType, "multipart/") {
		boundary := params["boundary"]
		if boundary == "" {
			return "multipart body with no boundary", true
		}
		return ambiguousMultipartField(trimmed, boundary)
	}
	declaredJSON := mimeErr == nil && (mediaType == "application/json" || strings.HasSuffix(mediaType, "+json"))
	if len(trimmed) == 0 {
		return "", false
	}
	switch {
	case declaredJSON || trimmed[0] == '{' || trimmed[0] == '[':
		if !gjson.ValidBytes(trimmed) {
			if !declaredJSON && trimmed[0] != '{' && trimmed[0] != '[' {
				return "", false
			}
			return "a JSON body that does not parse strictly", true
		}
		// The body is not written while it is read, so it is viewed as a string, not copied.
		path := make([]bodyPathSegment, 0, 8)
		return ambiguousJSONMember(gjson.Parse(unsafe.String(unsafe.SliceData(trimmed), len(trimmed))), &path, 0)
	case contentType == "" && bytes.HasPrefix(trimmed, []byte("--")):
		line, _, _ := bytes.Cut(trimmed, []byte("\n"))
		line = bytes.TrimRight(line, "\r")
		if len(line) <= 2 {
			return "", false
		}
		return ambiguousMultipartField(trimmed, string(line[2:]))
	}
	return "", false
}

// utf8ByteOrderMark opens some JSON documents; Python's json.loads, among others, skips it.
var utf8ByteOrderMark = []byte{0xef, 0xbb, 0xbf}

// gatedBodyKeys are the members an access gate or the router reads from a forwarded body by
// name. Two of them that differ only in case are ambiguous too: a typed decoder fills the field
// from either, gjson and the vendor read the exact spelling.
var gatedBodyKeys = []string{"model"}

// gatedBodyKey returns the gated key name folds to, or "".
func gatedBodyKey(name string) string {
	for _, key := range gatedBodyKeys {
		if strings.EqualFold(name, key) {
			return key
		}
	}
	return ""
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
	var gated []string // gated keys (folded) this object has given
	value.ForEach(func(key, member gjson.Result) bool {
		name := key.Str
		if folded := gatedBodyKey(name); folded != "" {
			if slices.Contains(gated, folded) {
				found, ambiguous = renderBodyPath(*path, name), true
				return false
			}
			gated = append(gated, folded)
		}
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

// ambiguousMultipartField reads a multipart body by its boundary and reports a name given twice
// when any of its parts is a text field. A body that does not parse is refused.
func ambiguousMultipartField(body []byte, boundary string) (string, bool) {
	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	type seenName struct{ text bool }
	seen := map[string]seenName{}
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			return "", false
		}
		if err != nil {
			return "multipart body that does not parse", true
		}
		name, text := part.FormName(), part.FileName() == ""
		part.Close()
		if name == "" || strings.HasSuffix(name, "[]") {
			continue
		}
		if previous, repeated := seen[name]; repeated && (text || previous.text) {
			return "multipart field " + name, true
		}
		seen[name] = seenName{text: text || seen[name].text}
	}
}
