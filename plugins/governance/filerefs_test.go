package governance

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/capsohq/bifrost/core/schemas"
)

// fileReferenceLikeName matches the names a field that can name a file goes by. A string field
// of any request type whose name matches must be classified: walked (fileReferenceFields),
// read by the request's own rule (fileReferenceFieldsReadElsewhere), or excused with the reason
// (fileReferenceFieldsExcused).
var fileReferenceLikeName = regexp.MustCompile(`(?i)url|uri|file_?ids?$|fileid|reference|lastframe|inputimages|bucket|location|source$|^path$|^image$|^video$|^audio$|^document$`)

// fileReferenceFieldsReadElsewhere are the fields namedProviderObjects reads: the object a
// request addresses, and the files a container or batch create copies or runs on.
var fileReferenceFieldsReadElsewhere = map[string]string{
	"BifrostFileRetrieveRequest.FileID":          "the file the request addresses",
	"BifrostFileDeleteRequest.FileID":            "the file the request addresses",
	"BifrostFileContentRequest.FileID":           "the file the request addresses",
	"BifrostContainerFileRetrieveRequest.FileID": "the container file the request addresses",
	"BifrostContainerFileContentRequest.FileID":  "the container file the request addresses",
	"BifrostContainerFileDeleteRequest.FileID":   "the container file the request addresses",
	"BifrostContainerFileCreateRequest.FileID":   "the file a container-file create copies in",
	"BifrostContainerCreateRequest.FileIDs":      "the files a container create copies in",
	"BifrostBatchCreateRequest.InputFileID":      "the file a batch runs on",
}

// fileReferenceFieldsExcused are fields whose name looks like a file reference and that name no
// uploaded file the provider would read under the shared key. Each says why.
var fileReferenceFieldsExcused = map[string]string{
	// Not a location the provider reads.
	"BatchRequestItem.URL":                    "the endpoint path a batch item is sent to",
	"BifrostContainerFileCreateRequest.Path":  "the path the new file gets inside the container",
	"BifrostPassthroughRequest.Path":          "the provider path; passthroughObjectRefs reads the objects it addresses",
	"BifrostPassthroughRequest.UpstreamURL":   "the base URL the gateway sends to, set by the route and never by the caller",
	"ResponsesApplyPatchOperation.Path":       "a path inside the patch the tool applies",
	"ResponsesToolShellSkill.Path":            "a path inside the shell tool's container",
	"ResponsesToolSearchCall.ToolReferences":  "names of tools",
	"TaskControlParameters.ClientReferenceID": "the caller's own label for a task",
	"TaskControlParameters.CallbackURL":       "the caller's webhook, which the provider posts to",
	"ChatMCPServer.URL":                       "an MCP server address",
	"ResponsesToolMCP.ServerURL":              "an MCP server address",
	"GCSStorageConfig.Bucket":                 "the bucket a file operation runs in; the file it addresses is checked by its id",
	"S3StorageConfig.Bucket":                  "the bucket a file operation runs in; the file it addresses is checked by its id",
	// Where output is written, not a file that is read. Writing over another key's upload is a
	// different question from reading it and is not decided here (TXE-7032 covers reads).
	"BatchOutputFolder.URL":                  "where a batch writes its output",
	"TaskControlParameters.OutputStorageURI": "where a task writes its output",
	// Web addresses in conversation history: a citation in an earlier answer, or a page the
	// provider's own search or fetch tool read from the public web. None is read with the
	// shared credential.
	"ChatAssistantMessageAnnotationCitation.URL":            "a citation in an earlier answer",
	"ResponsesOutputMessageContentTextAnnotation.URL":       "a citation in an earlier answer",
	"ResponsesOutputMessageContentTextAnnotation.Source":    "a citation in an earlier answer",
	"ResponsesCodeInterpreterOutputImage.URL":               "an image an earlier answer returned",
	"ResponsesWebFetchCall.URL":                             "a web page the provider's fetch tool read",
	"ResponsesWebFetchToolCallAction.URL":                   "a web page the provider's fetch tool read",
	"ResponsesWebSearchToolCallAction.URL":                  "a web page the provider's search tool opened",
	"ResponsesWebSearchToolCallActionSearchSource.URL":      "a web page the provider's search tool found",
	"ResponsesWebSearchToolCallActionSearchSource.ImageURL": "an image the provider's search tool found",
}

// requestStructTypes returns every struct type reachable from schemas.BifrostRequest through
// typed fields, and for each the request fields (ChatRequest, ResponsesRequest, ...) it is
// reachable from.
func requestStructTypes() map[reflect.Type][]string {
	reached := map[reflect.Type][]string{}
	var walk func(rt reflect.Type, root string, seen map[reflect.Type]bool)
	walk = func(rt reflect.Type, root string, seen map[reflect.Type]bool) {
		rt = structTypeOf(rt)
		if rt == nil || seen[rt] {
			return
		}
		seen[rt] = true
		reached[rt] = append(reached[rt], root)
		for i := 0; i < rt.NumField(); i++ {
			if rt.Field(i).IsExported() {
				walk(rt.Field(i).Type, root, seen)
			}
		}
	}
	root := reflect.TypeOf(schemas.BifrostRequest{})
	for i := 0; i < root.NumField(); i++ {
		walk(root.Field(i).Type, root.Field(i).Name, map[reflect.Type]bool{})
	}
	return reached
}

// Every string field of every request type whose name looks like a file reference is
// classified. This is the test that fails when a request type gains a URL or file-id field and
// nobody decides whether the ownership check reads it.
func TestEveryFileReferenceFieldIsClassified(t *testing.T) {
	types := requestStructTypes()
	present := map[string]bool{}
	var unclassified []string
	for rt := range types {
		for i := 0; i < rt.NumField(); i++ {
			field := rt.Field(i)
			if !field.IsExported() || !isStringType(field.Type) {
				continue
			}
			name := rt.Name() + "." + field.Name
			present[name] = true
			if !fileReferenceLikeName.MatchString(field.Name) {
				continue
			}
			_, walked := fileReferenceFields[name]
			_, elsewhere := fileReferenceFieldsReadElsewhere[name]
			_, excused := fileReferenceFieldsExcused[name]
			if n := btoi(walked) + btoi(elsewhere) + btoi(excused); n == 0 {
				unclassified = append(unclassified, fmt.Sprintf("%s (%s, in %s)", name, field.Type, strings.Join(types[rt], ", ")))
			} else if n > 1 {
				t.Errorf("%s is classified more than once", name)
			}
		}
	}
	sort.Strings(unclassified)
	for _, name := range unclassified {
		t.Errorf("unclassified file-reference-like field %s: list it in fileReferenceFields (filerefs.go) so the ownership check reads it, or in fileReferenceFieldsExcused with the reason it names no uploaded file", name)
	}
	// A stale entry would claim coverage of a field that no longer exists.
	for _, table := range []map[string]string{fileReferenceFieldsReadElsewhere, fileReferenceFieldsExcused} {
		for name := range table {
			if !present[name] {
				t.Errorf("%s is classified but is not a string field of any request type", name)
			}
		}
	}
	for name := range fileReferenceFields {
		if !present[name] {
			t.Errorf("fileReferenceFields lists %s, which is not a string field of any request type", name)
		}
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// plantInRequest builds a request of the given BifrostRequest field whose struct field named
// target ("<type>.<field>") holds value, with the shortest chain of containers around it. ok is
// false when target is not reachable from that request field.
func plantInRequest(requestField, target, value string) (*schemas.BifrostRequest, bool) {
	req := &schemas.BifrostRequest{}
	field := reflect.ValueOf(req).Elem().FieldByName(requestField)
	built, ok := plantInType(field.Type(), target, value, map[reflect.Type]bool{})
	if !ok {
		return nil, false
	}
	field.Set(built)
	return req, true
}

// plantInType returns a value of type rt that holds value at target, or false.
func plantInType(rt reflect.Type, target, value string, onPath map[reflect.Type]bool) (reflect.Value, bool) {
	switch rt.Kind() {
	case reflect.Pointer:
		inner, ok := plantInType(rt.Elem(), target, value, onPath)
		if !ok {
			return reflect.Value{}, false
		}
		ptr := reflect.New(rt.Elem())
		ptr.Elem().Set(inner)
		return ptr, true
	case reflect.Slice:
		inner, ok := plantInType(rt.Elem(), target, value, onPath)
		if !ok {
			return reflect.Value{}, false
		}
		return reflect.Append(reflect.MakeSlice(rt, 0, 1), inner), true
	case reflect.Map:
		if rt.Key().Kind() != reflect.String {
			return reflect.Value{}, false
		}
		inner, ok := plantInType(rt.Elem(), target, value, onPath)
		if !ok {
			return reflect.Value{}, false
		}
		m := reflect.MakeMap(rt)
		m.SetMapIndex(reflect.ValueOf("k").Convert(rt.Key()), inner)
		return m, true
	case reflect.Struct:
		if onPath[rt] {
			return reflect.Value{}, false
		}
		onPath[rt] = true
		defer delete(onPath, rt)
		for i := 0; i < rt.NumField(); i++ {
			field := rt.Field(i)
			if !field.IsExported() {
				continue
			}
			if rt.Name()+"."+field.Name == target {
				out := reflect.New(rt).Elem()
				out.Field(i).Set(stringValueOf(field.Type, value))
				return out, true
			}
		}
		for i := 0; i < rt.NumField(); i++ {
			field := rt.Field(i)
			if !field.IsExported() || structTypeOf(field.Type) == nil {
				continue
			}
			if inner, ok := plantInType(field.Type, target, value, onPath); ok {
				out := reflect.New(rt).Elem()
				out.Field(i).Set(inner)
				return out, true
			}
		}
	}
	return reflect.Value{}, false
}

// stringValueOf wraps a string as the field type holds it: a string, a pointer or a list.
func stringValueOf(rt reflect.Type, value string) reflect.Value {
	switch rt.Kind() {
	case reflect.Pointer:
		ptr := reflect.New(rt.Elem())
		ptr.Elem().Set(stringValueOf(rt.Elem(), value))
		return ptr
	case reflect.Slice:
		return reflect.Append(reflect.MakeSlice(rt, 0, 1), stringValueOf(rt.Elem(), value))
	default:
		return reflect.ValueOf(value).Convert(rt)
	}
}

// namesFile reports whether the references of a request include the file id.
func namesFile(req *schemas.BifrostRequest, id string) bool {
	for _, ref := range providerObjectReferences(req) {
		if ref.requestID == id && !ref.isList {
			return true
		}
	}
	return false
}

// The forms an upload is recorded under, and what a reference in each form must resolve to.
var recordedFileForms = []struct {
	name, written, recorded string
}{
	{"vertex gs uri", "gs://vk-guard-bucket/uploads/a.pdf", "gs://vk-guard-bucket/uploads/a.pdf"},
	{"gs uri in upper case", "GS://vk-guard-bucket/uploads/a.pdf", "gs://vk-guard-bucket/uploads/a.pdf"},
	{"gs uri with surrounding space", "  gs://vk-guard-bucket/uploads/a.pdf ", "gs://vk-guard-bucket/uploads/a.pdf"},
	{"bedrock s3 uri", "s3://vk-guard-bucket/uploads/a.pdf", "s3://vk-guard-bucket/uploads/a.pdf"},
	{"s3 uri in upper case", "S3://vk-guard-bucket/uploads/a.pdf", "s3://vk-guard-bucket/uploads/a.pdf"},
	{"gemini file uri", "https://generativelanguage.googleapis.com/v1beta/files/vk-guard", "files/vk-guard"},
	{"gemini file uri, host in upper case", "https://GenerativeLanguage.GoogleAPIs.com/v1beta/files/vk-guard", "files/vk-guard"},
}

// Every field that can name a file is read by the ownership check on every request type it can
// appear in, for every provider and in every form an upload is recorded under. A reference that
// skips the check fails here: a field added to fileReferenceFields that the walk does not
// reach, a request type whose rule returns before the files are read, an id form
// recordedFileID does not know.
func TestEveryFileReferenceIsChecked(t *testing.T) {
	types := requestStructTypes()
	rootsOf := map[string][]string{}
	for rt, roots := range types {
		for i := 0; i < rt.NumField(); i++ {
			rootsOf[rt.Name()+"."+rt.Field(i).Name] = roots
		}
	}
	providers := append([]schemas.ModelProvider{"custom-on-vertex"}, schemas.StandardProviders...)

	check := func(t *testing.T, target string, urlField bool) {
		roots := rootsOf[target]
		if len(roots) == 0 {
			t.Fatalf("%s is not reachable from any request type", target)
		}
		for _, root := range roots {
			if urlField {
				for _, form := range recordedFileForms {
					req, ok := plantInRequest(root, target, form.written)
					if !ok {
						t.Fatalf("%s: could not build a %s holding it", target, root)
					}
					for _, provider := range providers {
						req.SetProvider(provider)
						if !namesFile(req, form.recorded) {
							t.Errorf("%s in %s on %s: %s %q is not checked as file %q; references: %+v", target, root, provider, form.name, form.written, form.recorded, providerObjectReferences(req))
						}
					}
				}
				// A URL in no recorded form names no file, and must not be looked up.
				for _, plain := range []string{"https://example.com/files/vk-guard", "data:image/png;base64,aGVsbG8=", "gs:/not-a-uri", "files/vk-guard"} {
					req, _ := plantInRequest(root, target, plain)
					if refs := requestFileRefs(req); len(refs) != 0 {
						t.Errorf("%s in %s: %q is not a recorded file form but was read as %+v", target, root, plain, refs)
					}
				}
				continue
			}
			// An id field names a file whatever its form.
			for _, id := range []string{"file-vk-guard", "files/vk-guard", "gs://vk-guard-bucket/uploads/a.pdf"} {
				req, ok := plantInRequest(root, target, id)
				if !ok {
					t.Fatalf("%s: could not build a %s holding it", target, root)
				}
				for _, provider := range providers {
					req.SetProvider(provider)
					if !namesFile(req, id) {
						t.Errorf("%s in %s on %s: file id %q is not checked; references: %+v", target, root, provider, id, providerObjectReferences(req))
					}
				}
			}
		}
	}

	names := make([]string, 0, len(fileReferenceFields))
	for name := range fileReferenceFields {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		t.Run(name, func(t *testing.T) { check(t, name, fileReferenceFields[name] == fileRefURL) })
	}
	elsewhere := make([]string, 0, len(fileReferenceFieldsReadElsewhere))
	for name := range fileReferenceFieldsReadElsewhere {
		elsewhere = append(elsewhere, name)
	}
	sort.Strings(elsewhere)
	for _, name := range elsewhere {
		t.Run(name, func(t *testing.T) { check(t, name, false) })
	}
}

// Extra params and a cached content's contents hold provider-shaped data. A file named there,
// under whatever key and in whatever Go type the route stored it, is checked on every request
// type that has the field.
func TestFileReferencesInOpenFieldsAreChecked(t *testing.T) {
	type vendorImage struct {
		GCSURI *string `json:"gcsUri,omitempty"`
	}
	uri := "gs://vk-guard-bucket/uploads/a.pdf"
	shapes := map[string]func() any{
		"a map under the vendor's key": func() any { return map[string]any{"image": map[string]any{"gcsUri": uri}} },
		"a list of parts":              func() any { return []any{map[string]any{"fileData": map[string]any{"fileUri": uri}}} },
		"a typed vendor struct":        func() any { return []vendorImage{{GCSURI: &uri}} },
		"a list of strings":            func() any { return []string{"https://example.com/a.png", uri} },
		"a file id under file_id":      func() any { return map[string]any{"file_id": uri} },
		"file ids under file_ids":      func() any { return map[string]any{"file_ids": []any{uri}} },
		"an s3 location": func() any {
			return map[string]any{"s3Location": map[string]any{"uri": "s3://vk-guard-bucket/uploads/a.pdf"}}
		},
		"a gemini file uri under a key": func() any {
			return map[string]any{"video": "https://generativelanguage.googleapis.com/v1beta/files/vk-guard"}
		},
	}
	want := map[string]string{
		"an s3 location":                "s3://vk-guard-bucket/uploads/a.pdf",
		"a gemini file uri under a key": "files/vk-guard",
	}
	types := requestStructTypes()
	checked := 0
	for rt, roots := range types {
		for i := 0; i < rt.NumField(); i++ {
			field := rt.Field(i)
			if _, open := fileReferenceOpenFields[field.Name]; !open || !isOpenType(field.Type) {
				continue
			}
			for _, root := range roots {
				for name, shape := range shapes {
					req := &schemas.BifrostRequest{}
					requestField := reflect.ValueOf(req).Elem().FieldByName(root)
					built, ok := plantOpenValue(requestField.Type(), rt, i, shape(), map[reflect.Type]bool{})
					if !ok {
						continue
					}
					requestField.Set(built)
					req.SetProvider(schemas.Vertex)
					id := uri
					if other, ok := want[name]; ok {
						id = other
					}
					checked++
					if !namesFile(req, id) {
						t.Errorf("%s.%s in %s: %s is not checked as file %q; references: %+v", rt.Name(), field.Name, root, name, id, providerObjectReferences(req))
					}
				}
			}
		}
	}
	if checked < 50 {
		t.Fatalf("only %d open-field cases ran; the walk over request types found too few ExtraParams fields", checked)
	}
}

// plantOpenValue is plantInType for an open field: it builds a value of type rt in which field
// index of struct owner holds value.
func plantOpenValue(rt, owner reflect.Type, index int, value any, onPath map[reflect.Type]bool) (reflect.Value, bool) {
	switch rt.Kind() {
	case reflect.Pointer:
		inner, ok := plantOpenValue(rt.Elem(), owner, index, value, onPath)
		if !ok {
			return reflect.Value{}, false
		}
		ptr := reflect.New(rt.Elem())
		ptr.Elem().Set(inner)
		return ptr, true
	case reflect.Slice:
		inner, ok := plantOpenValue(rt.Elem(), owner, index, value, onPath)
		if !ok {
			return reflect.Value{}, false
		}
		return reflect.Append(reflect.MakeSlice(rt, 0, 1), inner), true
	case reflect.Struct:
		if rt == owner {
			out := reflect.New(rt).Elem()
			field := out.Field(index)
			switch field.Kind() {
			case reflect.Map:
				m := reflect.MakeMap(field.Type())
				m.SetMapIndex(reflect.ValueOf("vendor_field"), reflect.ValueOf(value))
				field.Set(m)
			case reflect.Slice:
				field.Set(reflect.Append(reflect.MakeSlice(field.Type(), 0, 1), reflect.ValueOf(value)))
			case reflect.Interface:
				field.Set(reflect.ValueOf(value))
			default:
				return reflect.Value{}, false
			}
			return out, true
		}
		if onPath[rt] {
			return reflect.Value{}, false
		}
		onPath[rt] = true
		defer delete(onPath, rt)
		for i := 0; i < rt.NumField(); i++ {
			field := rt.Field(i)
			if !field.IsExported() || structTypeOf(field.Type) == nil {
				continue
			}
			if inner, ok := plantOpenValue(field.Type, owner, index, value, onPath); ok {
				out := reflect.New(rt).Elem()
				out.Field(i).Set(inner)
				return out, true
			}
		}
	}
	return reflect.Value{}, false
}

func TestRecordedFileID(t *testing.T) {
	for _, form := range recordedFileForms {
		if got, ok := recordedFileID(form.written); !ok || got != form.recorded {
			t.Errorf("%s: recordedFileID(%q) = %q, %v; want %q", form.name, form.written, got, ok, form.recorded)
		}
	}
	for _, raw := range []string{
		"", "gs://", "s3://", "gs:/bucket/object", "https://example.com/gs://bucket/object",
		"https://example.com/v1beta/files/abc", "https://generativelanguage.googleapis.com/v1beta/models/m",
		"https://generativelanguage.googleapis.com.example.com/v1beta/files/abc",
		"data:application/pdf;base64," + strings.Repeat("A", 4096), "file-abc", "files/abc",
	} {
		if got, ok := recordedFileID(raw); ok {
			t.Errorf("recordedFileID(%.60q) = %q, want none", raw, got)
		}
	}
}

// Three ways a reference could slip past the walk of untyped data: kept as raw JSON bytes, padded
// past a length shortcut, or nested below the depth the walk reads to.
func TestFileReferencesCannotHideInOpenFields(t *testing.T) {
	chatWith := func(extra map[string]interface{}) *schemas.BifrostRequest {
		return &schemas.BifrostRequest{RequestType: schemas.ChatCompletionRequest, ChatRequest: &schemas.BifrostChatRequest{Provider: schemas.Vertex, Model: "m",
			Params: &schemas.ChatParameters{ExtraParams: extra}}}
	}
	uri := "gs://vk-guard-bucket/uploads/a.pdf"

	t.Run("raw JSON bytes are decoded", func(t *testing.T) {
		for name, value := range map[string]any{
			"json.RawMessage":      json.RawMessage(`{"image":{"gcsUri":"` + uri + `"}}`),
			"bytes":                []byte(`[{"fileData":{"fileUri":"` + uri + `"}}]`),
			"escaped slashes":      json.RawMessage(`{"image":{"gcsUri":"gs:\/\/vk-guard-bucket\/uploads\/a.pdf"}}`),
			"bytes that are a uri": []byte(uri),
		} {
			if req := chatWith(map[string]interface{}{"vendor_field": value}); !namesFile(req, uri) {
				t.Errorf("%s: the file named in raw bytes is not checked; references: %+v", name, providerObjectReferences(req))
			}
		}
		// JSON-shaped bytes this decoder cannot read may be read by a more lenient one.
		for _, broken := range []string{`{"image":{"gcsUri":"` + uri + `"},}`, `[{"fileUri":"` + uri + `"}`, `"` + uri} {
			if _, unreadable := requestFileIDs(chatWith(map[string]interface{}{"vendor_field": json.RawMessage(broken)})); !unreadable {
				t.Errorf("undecodable JSON %s was let through", broken)
			}
		}
		if refs := requestFileRefs(chatWith(map[string]interface{}{"audio": []byte{0x00, 0x01, 0x02, 0xff}})); len(refs) != 0 {
			t.Errorf("media bytes were read as %+v", refs)
		}
	})

	t.Run("a long url is still read", func(t *testing.T) {
		padded := "https://generativelanguage.googleapis.com/v1beta/files/vk-guard?pad=" + strings.Repeat("x", 8192)
		if id, ok := recordedFileID(padded); !ok || id != "files/vk-guard" {
			t.Errorf("a padded Gemini file URI resolved to %q, %v", id, ok)
		}
		if req := chatWith(map[string]interface{}{"video": padded}); !namesFile(req, "files/vk-guard") {
			t.Errorf("a padded Gemini file URI is not checked")
		}
	})

	t.Run("data nested past the bound refuses the request", func(t *testing.T) {
		nest := func(levels int) any {
			var value any = uri
			for i := 0; i < levels; i++ {
				value = map[string]any{"k": value}
			}
			return value
		}
		shallow := chatWith(map[string]interface{}{"vendor_field": nest(8)})
		if !namesFile(shallow, uri) {
			t.Errorf("a reference eight levels down is not checked")
		}
		if _, unreadable := requestFileIDs(shallow); unreadable {
			t.Errorf("eight levels were reported unreadable")
		}
		deep := chatWith(map[string]interface{}{"vendor_field": nest(openValueDepth + 8)})
		if _, unreadable := requestFileIDs(deep); !unreadable {
			t.Fatalf("a reference below the bound was neither read nor reported")
		}
		found := false
		for _, ref := range providerObjectReferences(deep) {
			found = found || ref.unreadable
		}
		if !found {
			t.Errorf("the unreadable request carries no reference that refuses it")
		}
	})
}

// A raw body is parsed only when it could name an object; a storage URI or a Gemini file host,
// in any case and with JSON-escaped slashes, must not slip past that shortcut.
func TestMayNameProviderObjectsSeesRecordedFileForms(t *testing.T) {
	for _, body := range []string{
		`{"image":{"gcsUri":"gs://b/o"}}`, `{"image":{"gcsUri":"GS://b/o"}}`, `{"image":{"gcsUri":"gs:\/\/b\/o"}}`,
		`{"source":{"s3Location":{"uri":"s3://b/k"}}}`, `{"source":{"s3Location":{"uri":"S3://b/k"}}}`,
		`{"video":"https://GENERATIVELANGUAGE.googleapis.com/v1beta/files/x"}`,
	} {
		if !mayNameProviderObjects([]byte(body)) {
			t.Errorf("body %s would be forwarded without a look", body)
		}
	}
	if mayNameProviderObjects([]byte(`{"model":"m","messages":[{"role":"user","content":"hello, world"}]}`)) {
		t.Errorf("a body naming nothing is parsed")
	}
	for body, want := range map[string]string{
		`{"instances":[{"image":{"gcsUri":"gs:\/\/b\/o"}}]}`:                                     "gs://b/o",
		`{"messages":[{"content":[{"document":{"source":{"s3Location":{"uri":"S3://b/k"}}}}]}]}`: "s3://b/k",
	} {
		var ids []string
		for _, ref := range passthroughObjectRefs(&schemas.BifrostPassthroughRequest{Provider: schemas.Vertex, Method: "POST", Path: "/v1/x", Body: []byte(body)}) {
			ids = append(ids, ref.requestID)
		}
		if !slices.Contains(ids, want) {
			t.Errorf("passthrough body %s: references %v, want %q among them", body, ids, want)
		}
	}
}

// The file extraction runs on every request that presents a virtual key.
func BenchmarkRequestFileIDs(b *testing.B) {
	text := strings.Repeat("The quick brown fox. ", 40)
	image := "https://example.com/images/cat.png"
	messages := make([]schemas.ChatMessage, 0, 20)
	for i := 0; i < 20; i++ {
		messages = append(messages, schemas.ChatMessage{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentBlocks: []schemas.ChatContentBlock{
			{Type: schemas.ChatContentBlockTypeText, Text: &text},
			{Type: schemas.ChatContentBlockTypeImage, ImageURLStruct: &schemas.ChatInputImage{URL: image}},
		}}})
	}
	req := &schemas.BifrostRequest{RequestType: schemas.ChatCompletionRequest, ChatRequest: &schemas.BifrostChatRequest{Provider: schemas.Vertex, Model: "m", Input: messages}}
	_, _ = requestFileIDs(req)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = requestFileIDs(req)
	}
}
