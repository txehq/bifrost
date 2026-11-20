package schemas

import (
	"bytes"
	"mime/multipart"
	"reflect"
	"strings"
	"testing"
	"unicode/utf16"
)

// TestAmbiguousBodyMember (TXE-7994): a forwarded body that gives one member twice is read
// differently by gjson (first) and by encoding/json, sonic and the vendor (last), so it is
// reported with the member's path; every other body is not.
func TestAmbiguousBodyMember(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   string
		member string // "" when the body is not ambiguous
	}{
		{"empty", ``, ""},
		{"plain text", `hello`, ""},
		{"invalid JSON", `{"model":"a","model"`, "a JSON body that does not parse strictly"},
		{"lenient JSON a vendor might accept", `{"model":"a",}`, "a JSON body that does not parse strictly"},
		{"byte order mark", "\xef\xbb\xbf{\"model\":\"a\",\"model\":\"b\"}", "model"},
		{"model in two casings", `{"model":"a","Model":"b"}`, "Model"},
		{"nested model in two casings", `[{"MODEL":"a","model":"b"}]`, "[0].model"},
		{"object naming its model once", `{"model":"a","messages":[{"role":"user","content":"x"}]}`, ""},
		{"same key in sibling objects", `{"messages":[{"role":"user"},{"role":"assistant"}]}`, ""},
		{"keys differing in case", `{"properties":{"Name":{},"name":{}}}`, ""},
		{"tool schema properties model and Model", `{"model":"a","tools":[{"input_schema":{"properties":{"model":{},"Model":{}}}}]}`, ""},
		{"nested object model and Model", `{"metadata":{"model":"a","Model":"b"}}`, ""},
		{"nested object model repeated exactly", `{"metadata":{"model":"a","model":"b"}}`, "metadata.model"},
		{"top-level model repeated", `{"model":"a","stream":true,"model":"b"}`, "model"},
		{"escaped repeat", `{"model":"a","model":"b"}`, "model"},
		{"leading whitespace", " \n\t{\"model\":\"a\",\"model\":\"b\"}", "model"},
		{"task array", `[{"model":"a"},{"taskType":"x","model":"a","model":"b"}]`, "[1].model"},
		{"nested object", `{"batch":{"input_uri":"gs://a","input_uri":"gs://b"}}`, "batch.input_uri"},
		{"nested in a list", `{"requests":[{"x":1},{"content":{"parts":[{"text":"a","text":"b"}]}}]}`, "requests[1].content.parts[0].text"},
		{"repeat with object values", `{"metadata":{},"metadata":{"k":"v"}}`, "metadata"},
		{"many keys then a repeat", `{"a":1,"b":1,"c":1,"d":1,"e":1,"f":1,"g":1,"h":1,"i":1,"j":1,"k":1,"l":1,"m":1,"n":1,"o":1,"p":1,"q":1,"r":1,"s":1,"a":2}`, "a"},
		{"many distinct keys", `{"a":1,"b":1,"c":1,"d":1,"e":1,"f":1,"g":1,"h":1,"i":1,"j":1,"k":1,"l":1,"m":1,"n":1,"o":1,"p":1,"q":1,"r":1,"s":1}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			member, ambiguous := AmbiguousBodyMember([]byte(tc.body), "")
			if ambiguous != (tc.member != "") || member != tc.member {
				t.Fatalf("AmbiguousBodyMember(%s) = %q, %t; want %q", tc.body, member, ambiguous, tc.member)
			}
		})
	}

	deep := strings.Repeat("[", maxForwardedBodyDepth+1) + strings.Repeat("]", maxForwardedBodyDepth+1)
	if member, ambiguous := AmbiguousBodyMember([]byte(deep), ""); !ambiguous || !strings.Contains(member, "nested more than") {
		t.Fatalf("a body nested past the bound = %q, %t; want it refused", member, ambiguous)
	}
	// A million levels is refused before anything recurses per level (gjson's validator would
	// grow the stack once per level), and brackets inside a string are not nesting.
	million := strings.Repeat("[", 1_000_000) + strings.Repeat("]", 1_000_000)
	if member, ambiguous := AmbiguousBodyMember([]byte(million), "application/json"); !ambiguous || !strings.Contains(member, "nested more than") {
		t.Fatalf("a body nested a million levels = %q, %t; want it refused", member, ambiguous)
	}
	inString := `{"text":"` + strings.Repeat("[", 2*maxForwardedBodyDepth) + `\"{"}`
	if member, ambiguous := AmbiguousBodyMember([]byte(inString), ""); ambiguous {
		t.Fatalf("brackets inside a string = %q, refused", member)
	}
	shallow := strings.Repeat("[", maxForwardedBodyDepth-1) + strings.Repeat("]", maxForwardedBodyDepth-1)
	if member, ambiguous := AmbiguousBodyMember([]byte(shallow), ""); ambiguous {
		t.Fatalf("a body nested within the bound = %q, refused", member)
	}

	// The declared type decides how a body is read, never what it opens with (TXE-8012): a
	// declared JSON body is read strictly; a text/plain or octet-stream upload is content, even
	// when it opens with "{" or a boundary line. Only an undeclared body is read by its first bytes.
	for _, tc := range []struct {
		body, contentType, member string
	}{
		{`not json`, "application/json; charset=utf-8", "a JSON body that does not parse strictly"},
		{`{"model":"a","model":"b"}`, "application/vnd.api+json", "model"},
		{`not json`, "text/plain", ""},
		{`{"model":"a","model":"b"}`, "text/plain", ""},
		{"{hello", "text/plain", ""},
		{"{hello", "text/plain; charset=utf-8", ""},
		{"[1, 2", "application/octet-stream", ""},
		{`{"model":"a","model":"b",}`, "text/plain", ""},
		{"{\"model\":\"a\"}\n{\"model\":\"b\"}\n", "application/octet-stream", ""},
		{`{"model":"a","model":"b"}`, "application/problem+json", "model"},
		{"{\"model\":\"a\"}\r\n\n{\"model\":\"b\"}\n", "application/jsonl", ""},
		{"{\"model\":\"a\"}\n{\"model\":\"b\",\"model\":\"c\"}\n", "application/x-ndjson", "line 2: model"},
		{"{\"model\":\"a\"}\n[1,\n", "application/jsonl; charset=utf-8", "line 2: a JSON body that does not parse strictly"},
		{"{hello", "", "a JSON body that does not parse strictly"},
		{"{hello", "application/json", "a JSON body that does not parse strictly"},
		{`{"model":"a"}`, "multipart/form-data; boundary=a; boundary=b", "a Content-Type that does not parse"},
		{`{"model":"a"}`, "not a type", "a Content-Type that does not parse"},
		{``, "not a type", ""},
		{"--b\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\na\r\n--b\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\nb\r\n--b--\r\n", "text/plain", ""},
		{"--b\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\na\r\n--b\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\nb\r\n--b--\r\n", "", "multipart field model"},
		{"-- a line of text that is not multipart", "text/plain", ""},
		{"-- a line of text that is not multipart", "", "multipart body that does not parse"},
	} {
		if member, ambiguous := AmbiguousBodyMember([]byte(tc.body), tc.contentType); ambiguous != (tc.member != "") || member != tc.member {
			t.Errorf("AmbiguousBodyMember(%s, %s) = %q, %t; want %q", tc.body, tc.contentType, member, ambiguous, tc.member)
		}
	}
}

// TestAmbiguousMultipartField: a multipart body that gives a text field twice is ambiguous
// (the router keeps the last model field, a vendor may keep the first); repeated files and
// "name[]" list fields are how lists are sent, and are not.
func TestAmbiguousMultipartField(t *testing.T) {
	var contentType string
	build := func(parts ...[2]string) []byte {
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		w.SetBoundary("txe-7994-boundary")
		contentType = w.FormDataContentType()
		for _, p := range parts {
			if strings.HasPrefix(p[0], "file:") {
				fw, _ := w.CreateFormFile(strings.TrimPrefix(p[0], "file:"), "a.png")
				fw.Write([]byte(p[1]))
				continue
			}
			w.WriteField(p[0], p[1])
		}
		w.Close()
		return buf.Bytes()
	}
	for _, tc := range []struct {
		name   string
		body   []byte
		member string
	}{
		{"fields once", build([2]string{"model", "a"}, [2]string{"prompt", "p"}), ""},
		{"repeated images", build([2]string{"model", "a"}, [2]string{"file:image", "x"}, [2]string{"file:image", "y"}), ""},
		{"list field", build([2]string{"timestamp_granularities[]", "word"}, [2]string{"timestamp_granularities[]", "segment"}), ""},
		{"model repeated", build([2]string{"model", "a"}, [2]string{"prompt", "p"}, [2]string{"model", "b"}), "multipart field model"},
		{"size repeated", build([2]string{"size", "256x256"}, [2]string{"size", "4096x4096"}), "multipart field size"},
		{"a file and a text field of one name", build([2]string{"file:model", "x"}, [2]string{"model", "b"}), "multipart field model"},
		{"a text field then a file of one name", build([2]string{"model", "a"}, [2]string{"file:model", "x"}), "multipart field model"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Read by the declared boundary, and by the boundary line when no type is declared.
			for _, declared := range []string{contentType, ""} {
				member, ambiguous := AmbiguousBodyMember(tc.body, declared)
				if ambiguous != (tc.member != "") || member != tc.member {
					t.Fatalf("AmbiguousBodyMember(declared %q) = %q, %t; want %q", declared, member, ambiguous, tc.member)
				}
			}
		})
	}

	// A preamble before the first boundary hides nothing from the declared boundary.
	body := build([2]string{"model", "a"}, [2]string{"model", "b"})
	withPreamble := append([]byte("preamble text\r\n"), body...)
	if member, ambiguous := AmbiguousBodyMember(withPreamble, contentType); !ambiguous || member != "multipart field model" {
		t.Errorf("a repeat behind a preamble = %q, %t; want it refused", member, ambiguous)
	}
	// A declared multipart body that does not parse, or names no boundary, is refused.
	if member, ambiguous := AmbiguousBodyMember(body[:len(body)/2], contentType); !ambiguous || member != "multipart body that does not parse" {
		t.Errorf("a truncated multipart body = %q, %t; want it refused", member, ambiguous)
	}
	if member, ambiguous := AmbiguousBodyMember(body, "multipart/form-data"); !ambiguous || member != "multipart body with no boundary" {
		t.Errorf("a multipart body with no boundary = %q, %t; want it refused", member, ambiguous)
	}
	// A part whose Content-Disposition Go cannot read one way only is refused: Go would skip it
	// as nameless, and a lenient vendor parser might read it as a second model field.
	for _, disposition := range []string{
		"Content-Disposition: form-data; name=\"prompt\"; name=\"model\"",
		"Content-Disposition: attachment; name=\"model\"",
		"Content-Disposition: form-data; name=\"model\"\r\nContent-Disposition: form-data; name=\"prompt\"",
		"Content-Disposition: form-data; name=\"model",
		"Content-Disposition: form-data",
		"Content-Disposition: form-data; name=\"prompt\"; name*=utf-8''model",
		"Content-Disposition: form-data; name=\"prompt\"; NAME*0=\"model\"",
		"Content-Disposition: form-data; name=\"prompt\";\u00a0name*=utf-8''model",
		"Content-Disposition: form-data; name=\"prompt\";\u2003name*=utf-8''model",
	} {
		raw := []byte("--b\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\na\r\n--b\r\n" + disposition + "\r\n\r\nb\r\n--b--\r\n")
		if member, ambiguous := AmbiguousBodyMember(raw, "multipart/form-data; boundary=b"); !ambiguous || member != "multipart part with an unreadable Content-Disposition" {
			t.Errorf("a part with %q = %q, %t; want it refused", disposition, member, ambiguous)
		}
	}
	// A gated name repeated in another case is a repeat: a vendor may fold form names.
	folded := []byte("--b\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\na\r\n--b\r\nContent-Disposition: form-data; name=\"Model\"\r\n\r\nb\r\n--b--\r\n")
	if member, ambiguous := AmbiguousBodyMember(folded, "multipart/form-data; boundary=b"); !ambiguous || member != "multipart field model" {
		t.Errorf("model then Model = %q, %t; want it refused", member, ambiguous)
	}
	// A non-ASCII quoted filename stays admitted; non-ASCII outside a quoted string does not.
	unicodeFilename := []byte("--b\r\nContent-Disposition: form-data; name=\"file\"; filename=\"r\u00e9sum\u00e9.pdf\"\r\n\r\nx\r\n--b--\r\n")
	if member, ambiguous := AmbiguousBodyMember(unicodeFilename, "multipart/form-data; boundary=b"); ambiguous {
		t.Errorf("a non-ASCII quoted filename = %q, %t; want it admitted", member, ambiguous)
	}
	// A filename* parameter is not a name parameter and stays admitted.
	filenameStar := []byte("--b\r\nContent-Disposition: form-data; name=\"image\"; filename*=utf-8''a.png\r\n\r\nx\r\n--b--\r\n")
	if member, ambiguous := AmbiguousBodyMember(filenameStar, "multipart/form-data; boundary=b"); ambiguous {
		t.Errorf("a filename* part = %q, %t; want it admitted", member, ambiguous)
	}
	// A part with no Content-Disposition names no field and is not refused.
	noDisposition := []byte("--b\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\na\r\n--b\r\nContent-Type: text/plain\r\n\r\nb\r\n--b--\r\n")
	if member, ambiguous := AmbiguousBodyMember(noDisposition, "multipart/form-data; boundary=b"); ambiguous {
		t.Errorf("a part with no Content-Disposition = %q, %t; want it admitted", member, ambiguous)
	}
}

// TestForwardedBodyContentType: a passthrough request's declared type is read from its forwarded
// headers in any casing; a raw request body declares none.
func TestForwardedBodyContentType(t *testing.T) {
	req := BifrostRequest{PassthroughRequest: &BifrostPassthroughRequest{SafeHeaders: map[string]string{"Content-Type": "multipart/form-data; boundary=x"}}}
	if got := req.ForwardedBodyContentType(); got != "multipart/form-data; boundary=x" {
		t.Fatalf("ForwardedBodyContentType = %q", got)
	}
	if got := (&BifrostRequest{ChatRequest: &BifrostChatRequest{}}).ForwardedBodyContentType(); got != "" {
		t.Fatalf("a raw request body declared %q", got)
	}
}

// TestForwardedBodyCoversEveryRawBodyField fails when a request variant carries a body
// forwarded as written (a RawRequestBody, or the passthrough Body) that ForwardedBody does not
// return: core's guard would not see it, and every gate reading it would be open to the parser
// differential again (TXE-7994). Other byte fields (an uploaded file) are content, not a body.
func TestForwardedBodyCoversEveryRawBodyField(t *testing.T) {
	marker := []byte(`{"model":"a","model":"b"}`)
	requestType := reflect.TypeOf(BifrostRequest{})
	covered := 0
	for i := 0; i < requestType.NumField(); i++ {
		variant := requestType.Field(i)
		if variant.Type.Kind() != reflect.Pointer || variant.Type.Elem().Kind() != reflect.Struct {
			continue
		}
		for j := 0; j < variant.Type.Elem().NumField(); j++ {
			field := variant.Type.Elem().Field(j)
			if field.Type != reflect.TypeOf([]byte(nil)) || (field.Name != "RawRequestBody" && field.Name != "Body") {
				continue
			}
			var req BifrostRequest
			value := reflect.New(variant.Type.Elem())
			value.Elem().Field(j).SetBytes(marker)
			reflect.ValueOf(&req).Elem().Field(i).Set(value)
			if got := req.ForwardedBody(); !bytes.Equal(got, marker) {
				t.Errorf("BifrostRequest.%s.%s is a forwarded body that ForwardedBody does not return; add it to the switch", variant.Name, field.Name)
			}
			covered++
		}
	}
	if covered < 36 {
		t.Fatalf("found %d forwarded body fields, fewer than the 36 known: the scan is broken", covered)
	}
}

func BenchmarkAmbiguousBodyMember(b *testing.B) {
	var chat strings.Builder
	chat.WriteString(`{"model":"gpt-4o","stream":true,"messages":[`)
	for i := 0; i < 2000; i++ {
		if i > 0 {
			chat.WriteString(",")
		}
		chat.WriteString(`{"role":"user","content":[{"type":"text","text":"` + strings.Repeat("lorem ipsum ", 40) + `"}]}`)
	}
	chat.WriteString(`],"temperature":0.2}`)
	for _, tc := range []struct {
		name string
		body []byte
	}{
		{"chat_1MB", []byte(chat.String())},
		{"small", []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)},
		{"not_json", bytes.Repeat([]byte{0xff}, 1<<20)},
		// Nested to the bound around a large string: one pass, not one pass per level.
		{"nested_511_around_4MB", []byte(strings.Repeat("[", maxForwardedBodyDepth-1) + `"` + strings.Repeat("x", 4<<20) + `"` + strings.Repeat("]", maxForwardedBodyDepth-1))},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.SetBytes(int64(len(tc.body)))
			b.ReportAllocs()
			for b.Loop() {
				if _, ambiguous := AmbiguousBodyMember(tc.body, ""); ambiguous {
					b.Fatal("ambiguous")
				}
			}
		})
	}
}

// TestClassifyForwardedBody: the declared type alone decides the format (TXE-8012); an
// undeclared body is read by its first bytes.
func TestClassifyForwardedBody(t *testing.T) {
	for _, tc := range []struct {
		contentType, body string
		want              ForwardedBodyFormat
	}{
		{"application/json", "not json", ForwardedBodyJSON},
		{"application/json; charset=utf-8", "{}", ForwardedBodyJSON},
		{"application/vnd.api+json", "{}", ForwardedBodyJSON},
		{"text/plain", `{"model":"a"}`, ForwardedBodyOpaque},
		{"application/octet-stream", "[1]", ForwardedBodyOpaque},
		{"audio/wav", "{", ForwardedBodyOpaque},
		{"application/x-ndjson", "{}\n{}", ForwardedBodyJSONLines},
		{"multipart/form-data; boundary=x", "--x", ForwardedBodyMultipart},
		{"multipart/form-data", "--x", ForwardedBodyUnreadable},
		{"not a type", "{}", ForwardedBodyUnreadable},
		{"", " \ufeff{\"model\":\"a\"}", ForwardedBodyJSON},
		{"", "[1]", ForwardedBodyJSON},
		{"", "--x\r\n", ForwardedBodyMultipart},
		{"", "hello", ForwardedBodyOpaque},
		{"application/json", "", ForwardedBodyOpaque},
	} {
		if got := ClassifyForwardedBody(tc.contentType, []byte(tc.body)).Format; got != tc.want {
			t.Errorf("ClassifyForwardedBody(%q, %q) = %d; want %d", tc.contentType, tc.body, got, tc.want)
		}
	}
}

// TestAmbiguousMultipartJSONPart: a multipart part is read as JSON only when it declares a JSON
// type (TXE-8012); a part with no type, or another type, is content whatever it looks like.
func TestAmbiguousMultipartJSONPart(t *testing.T) {
	part := func(header, content string) []byte {
		return []byte("--b\r\nContent-Disposition: form-data; name=\"metadata\"\r\n" + header + "\r\n" + content + "\r\n--b--\r\n")
	}
	const contentType = "multipart/form-data; boundary=b"
	for _, tc := range []struct {
		name, header, content, member string
	}{
		{"JSON part repeating model", "Content-Type: application/json\r\n", `{"model":"a","model":"b"}`, "multipart field metadata: model"},
		{"JSON part that does not parse", "Content-Type: application/json; charset=utf-8\r\n", `{"model":"a",}`, "multipart field metadata: a JSON body that does not parse strictly"},
		{"JSON part naming each member once", "Content-Type: application/json\r\n", `{"model":"a"}`, ""},
		{"untyped part that looks like JSON", "", `{"model":"a","model":"b"}`, ""},
		{"text part that looks like JSON", "Content-Type: text/plain\r\n", `{hello`, ""},
		{"two Content-Type headers", "Content-Type: text/plain\r\nContent-Type: application/json\r\n", `{}`, "multipart field metadata: a part with two Content-Type headers"},
		{"part type that does not parse", "Content-Type: not a type\r\n", `{}`, "multipart field metadata: a Content-Type that does not parse"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			member, ambiguous := AmbiguousBodyMember(part(tc.header, tc.content), contentType)
			if ambiguous != (tc.member != "") || member != tc.member {
				t.Fatalf("AmbiguousBodyMember = %q, %t; want %q", member, ambiguous, tc.member)
			}
		})
	}
}

// TestMislabeledJSONBody: a body declared as a non-JSON type that is strict JSON naming a gated
// key at its root is reported (TXE-8012); uploads that only look like JSON are not.
func TestMislabeledJSONBody(t *testing.T) {
	for _, tc := range []struct {
		contentType, body, mediaType, key string
	}{
		{"text/plain", `{"model":"gpt-4o"}`, "text/plain", "model"},
		{"application/octet-stream", ` [{"task":1},{"Model":"x"}]`, "application/octet-stream", "model"},
		{"text/plain; charset=utf-8", "\ufeff{\"MODEL\":\"x\"}", "text/plain", "model"},
		{"text/plain", `{hello`, "", ""},
		{"text/plain", `{"model":"a",}`, "text/plain", "model"},
		// JSON Lines naming a model must be declared as JSON Lines; as octet-stream it is refused.
		{"application/octet-stream", "{\"model\":\"a\"}\n{\"model\":\"b\"}\n", "application/octet-stream", "model"},
		{"application/octet-stream", "{\"key\":\"a\"}\n{\"key\":\"b\"}\n", "", ""},
		{"text/plain", `{"metadata":{"model":"a"}}`, "", ""},
		{"text/plain", `["model"]`, "", ""},
		{"application/json", `{"model":"a"}`, "", ""},
		{"", `{"model":"a"}`, "", ""},
		// Declared JSON Lines is no request document: a line naming a model at its root is refused
		// (core exempts file upload routes, where batch files live).
		{"application/x-ndjson", `{"model":"a"}`, "application/x-ndjson", "model"},
		{"application/jsonl", "{\"custom_id\":\"1\",\"body\":{\"model\":\"a\"}}\n", "", ""},
		// Every encoding is read, whatever the charset claims.
		{"text/plain; charset=utf-16le", `{"model":"X"}`, "text/plain", "model"},
		// Unicode space and comments before the opener.
		{"text/plain", "\u00a0\u2028/* x */ // y\n{\"model\":\"X\"}", "text/plain", "model"},
		// Any backslash in non-strict JSON could spell a key.
		{"text/plain", "{\"mo\\\nde\\154\":\"X\",}", "text/plain", "model"},
		{"text/plain", `{"a":"\\",}`, "text/plain", "model"},
		{"audio/wav", "RIFF\x00\x00\x00\x00WAVEfmt {\"model\"", "", ""},
		// Content that may name a provider object: governance's object gate reads only JSON.
		{"text/plain", `{"file_id":"file-foreign"}`, "text/plain", "a provider object"},
		{"text/plain", `{"File_ID":"file-foreign",}`, "text/plain", "a provider object"},
		{"application/octet-stream", `{"input":[{"fileUri":"gs://bucket/o"}]}`, "application/octet-stream", "a provider object"},
		{"text/plain", `{"tools":[{"type":"code_interpreter"}]}`, "text/plain", "a provider object"},
		{"text/plain", `{"note":"see s3://bucket/key"}`, "text/plain", "a provider object"},
		{"application/x-ndjson", "{\"custom_id\":\"1\",\"file_id\":\"file-x\"}\n", "application/x-ndjson", "a provider object"},
		{"text/plain", `{hello, nothing named here`, "", ""},
		// The 1 KB prefix must not decide on a rune or a "/" it cut: ideographic spaces (3 bytes)
		// to byte 1025, and spaces to a "/" at byte 1024 that opens a comment.
		{"text/plain", strings.Repeat("\u3000", 342) + `{"model":"X"}`, "text/plain", "model"},
		{"text/plain", strings.Repeat(" ", 1023) + "/* c */" + `{"model":"X"}`, "text/plain", "model"},
		// JSON a lenient parser accepts: no strict reading places the key, so a mention is enough.
		{"text/plain", `{"model":"X",}`, "text/plain", "model"},
		{"text/plain", `{model: 'X'}`, "text/plain", "model"},
		{"text/plain", `{"mod\u0065l":"X",}`, "text/plain", "model"},
		{"text/plain", `{"m\x6fdel":"X"`, "text/plain", "model"},
		{"text/plain", strings.Repeat("[", maxForwardedBodyDepth+1) + `{"MODEL":"x"}`, "text/plain", "model"},
		{"text/plain", `{hello, a text file with no key`, "", ""},
	} {
		mediaType, key, mislabeled := MislabeledJSONBody([]byte(tc.body), tc.contentType)
		if mislabeled != (tc.key != "") || mediaType != tc.mediaType || key != tc.key {
			t.Errorf("MislabeledJSONBody(%q, %q) = %q, %q, %t; want %q, %q", tc.body, tc.contentType, mediaType, key, mislabeled, tc.mediaType, tc.key)
		}
	}
}

// TestMislabeledJSONBodyInAnotherEncoding: a UTF-16 or UTF-32 JSON body opens with no "{" byte,
// but a vendor that detects the encoding (RFC 4627, Python's json.loads on bytes) parses it, so
// it is read after transcoding: by its byte order mark, its declared charset, or the NUL pattern
// of its first bytes, declared as another type or undeclared (TXE-8012).
func TestMislabeledJSONBodyInAnotherEncoding(t *testing.T) {
	encode := func(text string, width int, bigEndian, bom bool) []byte {
		var out []byte
		runes := []rune(text)
		if bom {
			runes = append([]rune{0xfeff}, runes...)
		}
		for _, r := range runes {
			units := []uint32{uint32(r)}
			if width == 2 {
				units = units[:0]
				for _, u := range utf16.Encode([]rune{r}) {
					units = append(units, uint32(u))
				}
			}
			for _, u := range units {
				for i := 0; i < width; i++ {
					shift := 8 * i
					if bigEndian {
						shift = 8 * (width - 1 - i)
					}
					out = append(out, byte(u>>shift))
				}
			}
		}
		return out
	}
	const naming = `{"model":"o1"}`
	for _, tc := range []struct {
		name, contentType string
		body              []byte
		key               string
	}{
		{"UTF-16LE with a byte order mark", "text/plain", encode(naming, 2, false, true), "model"},
		{"UTF-16BE with a byte order mark", "application/octet-stream", encode(naming, 2, true, true), "model"},
		// UTF-16LE with no mark opens with the byte "{", so undeclared it is read as JSON and
		// refused for not parsing strictly; big-endian opens with NUL and is content until read here.
		{"UTF-16BE by its NUL pattern, undeclared", "", encode(naming, 2, true, false), "model"},
		{"UTF-16BE by its declared charset", "text/plain; charset=utf-16be", encode(naming, 2, true, false), "model"},
		{"UTF-32BE with a byte order mark", "text/plain", encode(naming, 4, true, true), "model"},
		{"UTF-32LE by its NUL pattern", "text/plain", encode(naming, 4, false, false), "model"},
		{"UTF-16 JSON naming no gated key", "text/plain", encode(`{"a":1}`, 2, false, true), ""},
		{"UTF-16 text", "text/plain", encode("hello", 2, false, true), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, key, mislabeled := MislabeledJSONBody(tc.body, tc.contentType)
			if mislabeled != (tc.key != "") || key != tc.key {
				t.Fatalf("MislabeledJSONBody = %q, %t; want %q", key, mislabeled, tc.key)
			}
		})
	}
}

// TestIsFileUploadPath: the routes that store a body as a file and run no model (TXE-8012).
func TestIsFileUploadPath(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		want         bool
	}{
		{"POST", "/v1/files", true},
		{"POST", "/v1/files/", true},
		{"POST", "/upload/v1beta/files", true},
		{"POST", "/v1/uploads/upload_abc/parts", true},
		{"GET", "/v1/files", false},
		{"POST", "/v1/files/file-1/content", false},
		{"POST", "/v1/chat/completions", false},
		{"POST", "/v1/uploads", false},
		{"POST", "/v1/uploads/upload_abc/complete", false},
	} {
		if got := IsFileUploadPath(tc.method, tc.path); got != tc.want {
			t.Errorf("IsFileUploadPath(%s %s) = %t; want %t", tc.method, tc.path, got, tc.want)
		}
	}
}
