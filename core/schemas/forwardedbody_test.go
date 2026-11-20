package schemas

import (
	"bytes"
	"mime/multipart"
	"reflect"
	"strings"
	"testing"
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
	shallow := strings.Repeat("[", maxForwardedBodyDepth-1) + strings.Repeat("]", maxForwardedBodyDepth-1)
	if member, ambiguous := AmbiguousBodyMember([]byte(shallow), ""); ambiguous {
		t.Fatalf("a body nested within the bound = %q, refused", member)
	}

	// A declared JSON body is read strictly whatever it opens with; a non-JSON type is not read.
	for _, tc := range []struct {
		body, contentType, member string
	}{
		{`not json`, "application/json; charset=utf-8", "a JSON body that does not parse strictly"},
		{`{"model":"a","model":"b"}`, "application/vnd.api+json", "model"},
		{`not json`, "text/plain", ""},
		{`{"model":"a","model":"b"}`, "text/plain", "model"},
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
