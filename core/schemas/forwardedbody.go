package schemas

import (
	"bytes"
	"io"
	"mime"
	"mime/multipart"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
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

// ForwardedBodyFormat is how a forwarded body is read: by the guard, the router and every gate
// alike (TXE-8012). ClassifyForwardedBody is the one place it is decided.
type ForwardedBodyFormat int

const (
	// ForwardedBodyOpaque is content: a declared non-JSON, non-multipart type (text/plain,
	// application/octet-stream, audio), an empty body, or an undeclared body that opens like
	// neither. It is forwarded unchanged and no reader reads a member from it.
	ForwardedBodyOpaque ForwardedBodyFormat = iota
	// ForwardedBodyJSON is one JSON document: declared application/json or a +json type, or
	// undeclared and opening with "{" or "[".
	ForwardedBodyJSON
	// ForwardedBodyJSONLines is a declared JSON Lines file (jsonLinesMediaTypes), one JSON value
	// per line, as a batch input file is uploaded.
	ForwardedBodyJSONLines
	// ForwardedBodyMultipart is declared multipart (read by its boundary), or undeclared and
	// opening with a boundary line.
	ForwardedBodyMultipart
	// ForwardedBodyUnreadable is a body whose declared type does not parse, or multipart that
	// names no boundary: a vendor may still read it, so the guard refuses it.
	ForwardedBodyUnreadable
)

// ForwardedBodyClass is the result of ClassifyForwardedBody.
type ForwardedBodyClass struct {
	Format   ForwardedBodyFormat
	Declared bool   // a Content-Type was given
	Boundary string // ForwardedBodyMultipart
	Reason   string // ForwardedBodyUnreadable: what the guard reports
}

// ClassifyForwardedBody decides how a forwarded body is read from its declared Content-Type,
// never from what the body looks like under a declaration: a text/plain or octet-stream upload
// that opens with "{" is content, not JSON (TXE-8012). Only an undeclared body (a raw request
// body Bifrost decoded itself, or a passthrough caller that sent no type) is read by its first
// bytes, since there is nothing else to read it by. Every reader of a forwarded body classifies
// through here, so a body the guard does not check as JSON is a body no gate reads as JSON:
// TestEveryForwardedBodyReaderUsesTheSharedClassifier fails when one does not.
func ClassifyForwardedBody(contentType string, body []byte) ForwardedBodyClass {
	trimmed := trimForwardedBody(body)
	if len(trimmed) == 0 {
		return ForwardedBodyClass{Format: ForwardedBodyOpaque, Declared: contentType != ""}
	}
	if contentType == "" {
		switch {
		case trimmed[0] == '{' || trimmed[0] == '[':
			return ForwardedBodyClass{Format: ForwardedBodyJSON}
		case bytes.HasPrefix(trimmed, []byte("--")):
			line, _, _ := bytes.Cut(trimmed, []byte("\n"))
			if line = bytes.TrimRight(line, "\r"); len(line) > 2 {
				return ForwardedBodyClass{Format: ForwardedBodyMultipart, Boundary: string(line[2:])}
			}
		}
		return ForwardedBodyClass{Format: ForwardedBodyOpaque}
	}
	mediaType, params, err := mime.ParseMediaType(contentType)
	switch {
	case err != nil:
		// Go rejects a type a vendor may still read (two boundary parameters, say), so a body
		// under one is not judged by a guess at what the vendor makes of it.
		return ForwardedBodyClass{Format: ForwardedBodyUnreadable, Declared: true, Reason: "a Content-Type that does not parse"}
	case strings.HasPrefix(mediaType, "multipart/"):
		if params["boundary"] == "" {
			return ForwardedBodyClass{Format: ForwardedBodyUnreadable, Declared: true, Reason: "multipart body with no boundary"}
		}
		return ForwardedBodyClass{Format: ForwardedBodyMultipart, Declared: true, Boundary: params["boundary"]}
	case IsJSONMediaType(mediaType):
		return ForwardedBodyClass{Format: ForwardedBodyJSON, Declared: true}
	case jsonLinesMediaTypes[mediaType]:
		return ForwardedBodyClass{Format: ForwardedBodyJSONLines, Declared: true}
	}
	return ForwardedBodyClass{Format: ForwardedBodyOpaque, Declared: true}
}

// IsJSONMediaType reports a parsed media type that declares one JSON document.
func IsJSONMediaType(mediaType string) bool {
	return mediaType == "application/json" || strings.HasSuffix(mediaType, "+json")
}

// trimForwardedBody drops leading whitespace and a UTF-8 byte order mark.
func trimForwardedBody(body []byte) []byte {
	trimmed := bytes.TrimPrefix(bytes.TrimLeft(body, " \t\r\n"), utf8ByteOrderMark)
	return bytes.TrimLeft(trimmed, " \t\r\n")
}

// AmbiguousBodyMember reports the first member of a forwarded body that two parsers can read
// differently, as a path ("model", "tasks[0].model", "multipart field size"), and true; or "" and
// false for a body every parser reads the same way. contentType is the caller's declared
// Content-Type, "" when none is known. How the body is read is ClassifyForwardedBody's call:
//
//   - JSON: an object anywhere in the document that gives one key twice, compared as decoded,
//     so "model" and "mod\u0065l" are the same key. In the root object and the objects of a
//     root array, a gated key (GatedBodyKeys) is also compared without case, since
//     encoding/json and sonic fill a struct field from any casing. A document nested deeper
//     than maxForwardedBodyDepth is refused, and so is one that does not parse strictly: a
//     lenient vendor parser might read members gjson does not see.
//   - JSON Lines: each non-blank line as a JSON document.
//   - multipart: a name given twice when any of its parts is a text field, unless the name ends
//     in "[]", the form encoding of a list. A name only ever given as a file may repeat: a list
//     of images is sent that way. A part that declares a JSON type is checked as JSON. A body
//     that does not parse as multipart is refused.
//   - unreadable (a declared type that does not parse, multipart with no boundary): refused.
//
// An opaque body (empty, or declared as another type: text/plain, octet-stream, audio) is not
// ambiguous: no reader reads a member from it.
func AmbiguousBodyMember(body []byte, contentType string) (string, bool) {
	class := ClassifyForwardedBody(contentType, body)
	trimmed := trimForwardedBody(body)
	switch class.Format {
	case ForwardedBodyUnreadable:
		return class.Reason, true
	case ForwardedBodyJSON:
		return ambiguousJSONDocument(trimmed)
	case ForwardedBodyJSONLines:
		return ambiguousJSONLinesMember(trimmed)
	case ForwardedBodyMultipart:
		return ambiguousMultipartField(trimmed, class.Boundary)
	}
	return "", false
}

// MislabeledJSONBody reports a forwarded body that ClassifyForwardedBody calls content (declared
// as a non-JSON type, or undeclared and not opening like JSON) but that a vendor could still parse
// as JSON naming a gated key (GatedBodyKeys, compared without case): the media type it declares
// ("" when none) and the key. No reader reads such a body as JSON, so the gates see no model,
// while a vendor that parses it would run the one it names, past a model's own budget or rate
// limit. Core refuses it and asks for UTF-8 application/json (TXE-8012).
//
// What a lenient parser might accept is open-ended, so the check is broad rather than a model of
// one parser. The body is read under every encoding a JSON parser might pick (UTF-8, UTF-16 and
// UTF-32 in both byte orders, whatever its charset or byte order mark claims). Under any of them
// where it opens like JSON ("{" or "[" after Unicode whitespace, byte order marks and // or /* */
// comments), it is reported when:
//   - it is strict JSON whose root object, or an object of a root array, names a gated key; or
//   - it is not strict JSON (a trailing comma, quotes, bare keys, comments, nesting past
//     maxForwardedBodyDepth) and mentions a gated key anywhere, or holds a backslash, which could
//     spell one through an escape.
//
// A body declared JSON Lines is reported when a line names a gated key at its root: it is no
// request document, so no gate reads a model from it either.
//
// Content that opens like JSON under none of them (audio, images, prose) is not reported, nor is
// a text upload opening with "{" that never mentions a gated key and holds no backslash. File
// upload routes are exempt in core (IsFileUploadPath): the vendor stores those bytes and runs no
// model.
func MislabeledJSONBody(body []byte, contentType string) (mediaType, key string, mislabeled bool) {
	format := ClassifyForwardedBody(contentType, body).Format
	mediaType, _, _ = mime.ParseMediaType(contentType)
	if format == ForwardedBodyJSONLines {
		// A JSON Lines file is no request document either, so no gate reads a model from it; a
		// line naming one at its root is what a vendor parsing the body as JSON would run.
		for _, line := range bytes.Split(body, []byte("\n")) {
			line = trimForwardedBody(bytes.TrimRight(line, " \t\r"))
			if len(line) == 0 {
				continue
			}
			if key = gatedKeyInJSONText(line); key != "" {
				return mediaType, key, true
			}
		}
		return "", "", false
	}
	if format != ForwardedBodyOpaque {
		return "", "", false
	}
	for _, decode := range jsonTextDecodings {
		// Most content opens like JSON under no encoding: decide from a short prefix before
		// decoding a large body. A prefix holding only space and comments reads on.
		if opens, decided := opensLikeJSON(decode(body[:min(len(body), 1024)])); decided && !opens {
			continue
		}
		doc := decode(body)
		start, opens := jsonTextStart(doc)
		if !opens {
			continue
		}
		if key = gatedKeyInJSONText(doc[start:]); key != "" {
			return mediaType, key, true
		}
	}
	return "", "", false
}

// jsonTextDecodings are the encodings a JSON parser might read a body in: as it is (UTF-8), and
// UTF-16 and UTF-32 in both byte orders. A leading byte order mark decodes to U+FEFF, which
// jsonTextStart skips.
var jsonTextDecodings = []func([]byte) []byte{
	func(b []byte) []byte { return b },
	func(b []byte) []byte { return decodeUTF16(b, false) },
	func(b []byte) []byte { return decodeUTF16(b, true) },
	func(b []byte) []byte { return decodeUTF32(b, false) },
	func(b []byte) []byte { return decodeUTF32(b, true) },
}

// opensLikeJSON reports whether text opens like JSON, and whether that was decided: a text of
// only space and comments is undecided.
func opensLikeJSON(text []byte) (opens, decided bool) {
	start, opens := jsonTextStart(text)
	return opens, start < len(text)
}

// jsonTextStart skips Unicode whitespace, byte order marks and // or /* */ comments, and reports
// where the text starts and whether it starts with "{" or "[".
func jsonTextStart(text []byte) (int, bool) {
	i := 0
	for i < len(text) {
		r, size := utf8.DecodeRune(text[i:])
		switch {
		case r == utf8.RuneError && size <= 1:
			return i, false
		case unicode.IsSpace(r) || r == 0xfeff || r == 0x200b:
			i += size
		case r == '/' && i+1 < len(text) && text[i+1] == '/':
			end := bytes.IndexByte(text[i:], '\n')
			if end < 0 {
				return len(text), false
			}
			i += end + 1
		case r == '/' && i+1 < len(text) && text[i+1] == '*':
			end := bytes.Index(text[i+2:], []byte("*/"))
			if end < 0 {
				return len(text), false
			}
			i += end + 4
		default:
			return i, r == '{' || r == '['
		}
	}
	return i, false
}

// gatedKeyInJSONText returns the gated key a JSON text opening with "{" or "[" names: by its
// root members when it is strict JSON, by any mention or backslash when it is not.
func gatedKeyInJSONText(doc []byte) string {
	if _, deep := jsonNestedTooDeep(doc); !deep && gjson.ValidBytes(doc) {
		var key string
		named := func(object gjson.Result) bool {
			object.ForEach(func(member, _ gjson.Result) bool {
				if gated := gatedBodyKey(member.Str); gated != "" {
					key = gated
					return false
				}
				return true
			})
			return key != ""
		}
		root := gjson.ParseBytes(doc)
		if root.IsObject() {
			named(root)
		} else {
			root.ForEach(func(_, element gjson.Result) bool {
				return !(element.IsObject() && named(element))
			})
		}
		return key
	}
	lower := bytes.ToLower(doc)
	for _, key := range GatedBodyKeys {
		if bytes.Contains(lower, []byte(key)) {
			return key
		}
	}
	if bytes.IndexByte(doc, '\\') >= 0 {
		return GatedBodyKeys[0]
	}
	return ""
}

// decodeUTF16 reads b as UTF-16 in the given byte order and returns it as UTF-8.
func decodeUTF16(b []byte, bigEndian bool) []byte {
	units := make([]uint16, 0, len(b)/2)
	for ; len(b) >= 2; b = b[2:] {
		if bigEndian {
			units = append(units, uint16(b[0])<<8|uint16(b[1]))
		} else {
			units = append(units, uint16(b[1])<<8|uint16(b[0]))
		}
	}
	out := make([]byte, 0, len(units))
	for _, r := range utf16.Decode(units) {
		out = utf8.AppendRune(out, r)
	}
	return out
}

// decodeUTF32 reads b as UTF-32 in the given byte order and returns it as UTF-8.
func decodeUTF32(b []byte, bigEndian bool) []byte {
	out := make([]byte, 0, len(b)/4)
	for ; len(b) >= 4; b = b[4:] {
		var r uint32
		if bigEndian {
			r = uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
		} else {
			r = uint32(b[3])<<24 | uint32(b[2])<<16 | uint32(b[1])<<8 | uint32(b[0])
		}
		out = utf8.AppendRune(out, rune(r))
	}
	return out
}

// IsFileUploadPath reports a passthrough route that stores the request body as a file and runs no
// model: OpenAI's and Anthropic's POST .../files, OpenAI's POST .../uploads/{id}/parts, and
// Gemini's POST /upload/... media upload. A file is content, so MislabeledJSONBody does not apply
// there: a batch input file is JSON Lines naming a model on every line, uploaded under whatever
// type its SDK guesses (Python's mimetypes knows no .jsonl, so often application/octet-stream).
func IsFileUploadPath(method, path string) bool {
	if method != "POST" {
		return false
	}
	path = strings.TrimSuffix(path, "/")
	switch {
	case strings.HasSuffix(path, "/files"):
		return true
	case strings.HasPrefix(path, "/upload/"):
		return true
	}
	if i := strings.Index(path, "/uploads/"); i >= 0 {
		id, tail, _ := strings.Cut(path[i+len("/uploads/"):], "/")
		return id != "" && tail == "parts"
	}
	return false
}

// ambiguousJSONDocument checks one JSON document: nested no deeper than maxForwardedBodyDepth
// (counted before gjson's validator, which recurses once per level, sees it), strictly valid,
// and repeating no member.
func ambiguousJSONDocument(doc []byte) (string, bool) {
	if member, deep := jsonNestedTooDeep(doc); deep {
		return member, true
	}
	if !gjson.ValidBytes(doc) {
		return "a JSON body that does not parse strictly", true
	}
	return ambiguousJSONMember(doc)
}

// jsonLinesMediaTypes are the declared types under which a body is read as JSON Lines: one JSON
// value per line, as a batch input file is uploaded.
var jsonLinesMediaTypes = map[string]bool{
	"application/jsonl":       true,
	"application/x-jsonl":     true,
	"application/jsonlines":   true,
	"application/x-jsonlines": true,
	"application/json-lines":  true,
	"application/ndjson":      true,
	"application/x-ndjson":    true,
}

// ambiguousJSONLinesMember checks each non-blank line of a JSON Lines body as a JSON document:
// a line that does not parse strictly, or that repeats a member, makes the body ambiguous.
func ambiguousJSONLinesMember(body []byte) (string, bool) {
	for number := 1; len(body) > 0; number++ {
		var line []byte
		line, body, _ = bytes.Cut(body, []byte("\n"))
		line = bytes.Trim(line, " \t\r")
		if len(line) == 0 {
			continue
		}
		if member, ambiguous := ambiguousJSONDocument(line); ambiguous {
			return "line " + strconv.Itoa(number) + ": " + member, true
		}
	}
	return "", false
}

// multipartUnparsable is the member reported for a multipart body that does not parse.
const multipartUnparsable = "multipart body that does not parse"

// multipartUnreadableDisposition is the member reported for a part whose name Go cannot read
// but a lenient vendor parser might: a Content-Disposition given twice, one that does not
// parse (such as one with two name parameters), or one that is not form-data.
const multipartUnreadableDisposition = "multipart part with an unreadable Content-Disposition"

// multipartPartName returns the form name of a part, and false when the part's
// Content-Disposition cannot be read one way only. part.FormName returns "" for every such
// part, which would let a part a vendor reads as a field slip past the repeat check. A part
// with no Content-Disposition names no field and is readable, with an empty name.
func multipartPartName(part *multipart.Part) (string, bool) {
	dispositions := part.Header.Values("Content-Disposition")
	switch len(dispositions) {
	case 0:
		return "", true
	case 1:
	default:
		return "", false
	}
	if !plainHeaderValue(dispositions[0]) || extendedNameParameter.MatchString(dispositions[0]) {
		return "", false
	}
	disposition, params, err := mime.ParseMediaType(dispositions[0])
	if err != nil || disposition != "form-data" || params["name"] == "" {
		return "", false
	}
	return params["name"], true
}

// extendedNameParameter matches an RFC 2231 extended or continued name parameter (name*=,
// name*0=, name*0*=). Go decodes it into "name" and lets it win over a plain name; a vendor
// that ignores RFC 2231 reads the plain one. Form names are plain in every API this forwards
// to, so a part carrying one is refused rather than read one way. It may also match inside a
// quoted value, which refuses a body that is merely odd: the check fails closed.
var extendedNameParameter = regexp.MustCompile(`(?i);\s*name\*`)

// plainHeaderValue reports whether a header value's syntax, outside its quoted strings, holds
// only printable ASCII, space and tab. mime.ParseMediaType trims any Unicode space around a
// parameter, which extendedNameParameter's \s does not match; with only space and tab allowed
// between parameters, the two agree on where a parameter starts. A quoted string (a filename
// such as "résumé.pdf") may hold any byte but a control character; a backslash escapes the byte
// after it, as in Go's parser.
func plainHeaderValue(value string) bool {
	quoted := false
	for i := 0; i < len(value); i++ {
		c := value[i]
		if (c < 0x20 && c != '\t') || c == 0x7f {
			return false
		}
		switch {
		case quoted && c == '\\':
			i++
		case c == '"':
			quoted = !quoted
		case !quoted && c > 0x7e:
			return false
		}
	}
	return true
}

// utf8ByteOrderMark opens some JSON documents; Python's json.loads, among others, skips it.
var utf8ByteOrderMark = []byte{0xef, 0xbb, 0xbf}

// GatedBodyKeys are the members an access gate or the router reads from a forwarded body by
// name. Two of them that differ only in case are ambiguous too: a typed decoder fills the field
// from either, gjson and the vendor read the exact spelling. They are read only from the root
// object and from the objects of a root array (governance's passthrough models, a typed decoder
// of the request), so only those objects are compared without case: elsewhere, such as a tool's
// input_schema.properties, keys are user-defined and case-sensitive.
var GatedBodyKeys = []string{BodyModelKey}

// BodyModelKey is the member the gates (governance's model access and limits) and the router read
// a forwarded body's model from. Readers use this constant, never a literal:
// TestBodyReadersGateOnlyOnTheSharedKeyList fails otherwise.
const BodyModelKey = "model"

// BodyStreamKey is the member the passthrough router reads a stream flag from. It is not gated: a
// misread changes how the answer is framed, not what the caller may run.
const BodyStreamKey = "stream"

// gatedBodyKey returns the gated key name folds to, or "".
func gatedBodyKey(name string) string {
	for _, key := range GatedBodyKeys {
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

// jsonNestedTooDeep reports a document that opens more than maxForwardedBodyDepth containers at
// once. It is one pass over the bytes with no recursion, so it runs before anything that
// recurses per level (gjson's validator) sees the body.
func jsonNestedTooDeep(doc []byte) (string, bool) {
	depth, inString := 0, false
	for i := 0; i < len(doc); i++ {
		switch c := doc[i]; {
		case inString && c == '\\':
			i++
		case c == '"':
			inString = !inString
		case inString:
		case c == '{' || c == '[':
			if depth++; depth > maxForwardedBodyDepth {
				return "a JSON body nested more than " + strconv.Itoa(maxForwardedBodyDepth) + " levels", true
			}
		case c == '}' || c == ']':
			depth--
		}
	}
	return "", false
}

// jsonFrame is one open container while ambiguousJSONMember scans.
type jsonFrame struct {
	object    bool
	key       string // the member being read, in an object
	index     int    // the element being read, in an array
	foldGated bool   // gated keys are compared without case in this object
	keys      []string
	large     map[string]struct{}
	gated     []string // gated keys (folded) this object has given
}

// seen records name in the frame and reports whether the object already gave it.
func (f *jsonFrame) seen(name string) bool {
	if folded := gatedBodyKey(name); f.foldGated && folded != "" {
		if slices.Contains(f.gated, folded) {
			return true
		}
		f.gated = append(f.gated, folded)
	}
	if f.large != nil {
		if _, ok := f.large[name]; ok {
			return true
		}
		f.large[name] = struct{}{}
		return false
	}
	if slices.Contains(f.keys, name) {
		return true
	}
	// Most objects carry a handful of keys: a slice is cheaper than a map until they do not.
	if len(f.keys) == 16 {
		f.large = make(map[string]struct{}, 32)
		for _, key := range f.keys {
			f.large[key] = struct{}{}
		}
		f.large[name] = struct{}{}
		return false
	}
	f.keys = append(f.keys, name)
	return false
}

// ambiguousJSONMember scans a strictly valid document, nested no deeper than
// maxForwardedBodyDepth, in one pass with an explicit stack: each byte is visited once, however
// deep the nesting (a recursive walk re-reads every child at each level). It reports the first
// object that gives a key twice, as a path.
func ambiguousJSONMember(doc []byte) (string, bool) {
	stack := make([]jsonFrame, 0, 8)
	i := skipJSONSpace(doc, 0)
	for i < len(doc) {
		// i is at the start of a value.
		switch doc[i] {
		case '{', '[':
			frame := jsonFrame{object: doc[i] == '{'}
			if len(stack) < cap(stack) {
				// Reuse the key buffers a closed sibling at this depth left behind.
				previous := stack[: len(stack)+1 : cap(stack)][len(stack)]
				frame.keys, frame.gated = previous.keys[:0], previous.gated[:0]
			}
			if frame.object {
				depth := len(stack)
				frame.foldGated = depth == 0 || (depth == 1 && !stack[0].object)
			}
			stack = append(stack, frame)
			i = skipJSONSpace(doc, i+1)
			if doc[i] == '}' || doc[i] == ']' {
				stack = stack[:len(stack)-1]
				i++
				break
			}
			if frame.object {
				var member string
				var ambiguous bool
				if i, member, ambiguous = readJSONKey(doc, i, stack); ambiguous {
					return member, true
				}
			}
			continue
		case '"':
			i = skipJSONString(doc, i)
		default:
			for i < len(doc) && !strings.ContainsRune(",]} \t\r\n", rune(doc[i])) {
				i++
			}
		}
		// A value ended: move past separators and closing brackets to the next value.
		for {
			i = skipJSONSpace(doc, i)
			if len(stack) == 0 || i >= len(doc) {
				return "", false
			}
			if doc[i] != ',' {
				stack = stack[:len(stack)-1]
				i++
				continue
			}
			i = skipJSONSpace(doc, i+1)
			top := &stack[len(stack)-1]
			if !top.object {
				top.index++
				break
			}
			var member string
			var ambiguous bool
			if i, member, ambiguous = readJSONKey(doc, i, stack); ambiguous {
				return member, true
			}
			break
		}
	}
	return "", false
}

// readJSONKey reads the member name at doc[i] for the object on top of stack, records it, and
// returns the index of the member's value. A name the object already gave is reported as a path.
func readJSONKey(doc []byte, i int, stack []jsonFrame) (int, string, bool) {
	end := skipJSONString(doc, i)
	raw := doc[i+1 : end-1]
	var name string
	if bytes.IndexByte(raw, '\\') >= 0 {
		name = gjson.ParseBytes(doc[i:end]).Str // decoded: "mod\u0065l" is "model"
	} else {
		// The body is not written while it is read, so the name is viewed, not copied.
		name = unsafe.String(unsafe.SliceData(raw), len(raw))
	}
	top := &stack[len(stack)-1]
	if top.seen(name) {
		path := make([]bodyPathSegment, 0, len(stack)-1)
		for _, frame := range stack[:len(stack)-1] {
			if frame.object {
				path = append(path, bodyPathSegment{key: frame.key, index: -1})
			} else {
				path = append(path, bodyPathSegment{index: frame.index})
			}
		}
		return 0, renderBodyPath(path, name), true
	}
	top.key = name
	i = skipJSONSpace(doc, end) // at ':'
	return skipJSONSpace(doc, i+1), "", false
}

// skipJSONString returns the index just past the string that opens at doc[i].
func skipJSONString(doc []byte, i int) int {
	for i++; i < len(doc); i++ {
		switch doc[i] {
		case '\\':
			i++
		case '"':
			return i + 1
		}
	}
	return len(doc)
}

// skipJSONSpace returns the first index at or after i that is not JSON whitespace.
func skipJSONSpace(doc []byte, i int) int {
	for i < len(doc) && (doc[i] == ' ' || doc[i] == '\t' || doc[i] == '\r' || doc[i] == '\n') {
		i++
	}
	return i
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
			return multipartUnparsable, true
		}
		name, readable := multipartPartName(part)
		text := part.FileName() == ""
		if !readable {
			part.Close()
			return multipartUnreadableDisposition, true
		}
		// A part is read as JSON only when it is a field (no filename) that declares a JSON type.
		// A part with a filename is a file, whatever type its SDK guessed: a batch input .jsonl
		// part names a model on every line and runs none. A part with no type is a form field.
		if member, ambiguous := ambiguousMultipartPartContent(part, text); ambiguous {
			part.Close()
			label := name
			if label == "" {
				label = "(unnamed part)"
			}
			return "multipart field " + label + ": " + member, true
		}
		part.Close()
		if name == "" || strings.HasSuffix(name, "[]") {
			continue
		}
		if gated := gatedBodyKey(name); gated != "" {
			name = gated
		}
		if previous, repeated := seen[name]; repeated && (text || previous.text) {
			return "multipart field " + name, true
		}
		seen[name] = seenName{text: text || seen[name].text}
	}
}

// ambiguousMultipartPartContent checks a field part's content by the part's own declared type: a
// field declaring a JSON or JSON Lines type is checked as one, any other part is content. A file
// part (field false) is content whatever its type. A part with
// two Content-Type headers, or one that does not parse, is refused like such a body.
func ambiguousMultipartPartContent(part *multipart.Part, field bool) (string, bool) {
	if !field {
		return "", false
	}
	types := part.Header.Values("Content-Type")
	switch len(types) {
	case 0:
		return "", false
	case 1:
	default:
		return "a part with two Content-Type headers", true
	}
	class := ClassifyForwardedBody(types[0], []byte{'{'}) // the format is the declared type's alone
	switch class.Format {
	case ForwardedBodyUnreadable:
		return class.Reason, true
	case ForwardedBodyJSON, ForwardedBodyJSONLines:
	default:
		return "", false
	}
	content, err := io.ReadAll(part)
	if err != nil {
		return multipartUnparsable, true
	}
	return AmbiguousBodyMember(content, types[0])
}
