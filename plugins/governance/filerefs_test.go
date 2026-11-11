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

// fileReferenceLikeName matches the names a field that can name a file goes by, as something
// the provider reads or as the place it writes its output (TXE-7105). A string field
// of any request type whose name matches must be classified: walked (fileReferenceFields),
// read by the request's own rule (fileReferenceFieldsReadElsewhere), or excused with the reason
// (fileReferenceFieldsExcused).
var fileReferenceLikeName = regexp.MustCompile(`(?i)url|uri|file_?ids?$|fileid|reference|lastframe|inputimages|bucket|location|source$|^path$|^image$|^video$|^audio$|^document$|output|folder|blob|storage|destination|prefix`)

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
	// The bucket and folder of a file operation. A Bedrock upload writes the object they name
	// with the filename, and uploadLocations composes the three into the object that is checked
	// (TestUploadLocationIsChecked); a Vertex upload writes under a fresh folder and replaces
	// nothing; a list is narrowed item by item; the other file operations address a file by id.
	"GCSStorageConfig.Bucket": "the bucket a Vertex file operation runs in; its upload writes under a fresh folder",
	"S3StorageConfig.Bucket":  "a piece of the object a Bedrock upload writes, read by uploadLocations",
	"GCSStorageConfig.Prefix": "the folder a Vertex file operation runs in; its upload writes under a fresh folder",
	"S3StorageConfig.Prefix":  "a piece of the object a Bedrock upload writes, read by uploadLocations",
	// Output in name only.
	"ImageEditParameters.OutputFormat":                            "an image format",
	"ImageGenerationParameters.OutputFormat":                      "an image format",
	"ResponsesImageGenerationCall.OutputFormat":                   "an image format",
	"ResponsesToolImageGeneration.OutputFormat":                   "an image format",
	"VideoEditParameters.OutputFormat":                            "a video format",
	"VideoGenerationParameters.OutputFormat":                      "a video format",
	"ResponsesToolMessageOutputStruct.ResponsesToolCallOutputStr": "the text a tool call returned, replayed as history",
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
	// The https forms of a stored object are that object (TXE-7105).
	{"gcs path-style url", "https://storage.googleapis.com/vk-guard-bucket/uploads/a.pdf", "gs://vk-guard-bucket/uploads/a.pdf"},
	{"gcs url, host in upper case", "HTTPS://Storage.GoogleAPIs.com/vk-guard-bucket/uploads/a.pdf", "gs://vk-guard-bucket/uploads/a.pdf"},
	{"gcs signed url", "https://storage.googleapis.com/vk-guard-bucket/uploads/a.pdf?X-Goog-Signature=abc&X-Goog-Expires=60", "gs://vk-guard-bucket/uploads/a.pdf"},
	{"gcs bucket-host url", "https://vk-guard-bucket.storage.googleapis.com/uploads/a.pdf", "gs://vk-guard-bucket/uploads/a.pdf"},
	{"gcs browser url", "https://storage.cloud.google.com/vk-guard-bucket/uploads/a.pdf", "gs://vk-guard-bucket/uploads/a.pdf"},
	{"gcs json api url", "https://storage.googleapis.com/storage/v1/b/vk-guard-bucket/o/uploads%2Fa.pdf", "gs://vk-guard-bucket/uploads/a.pdf"},
	{"gcs json api download url", "https://storage.googleapis.com/download/storage/v1/b/vk-guard-bucket/o/uploads%2Fa.pdf?alt=media", "gs://vk-guard-bucket/uploads/a.pdf"},
	{"gcs json api on www.googleapis.com", "https://www.googleapis.com/storage/v1/b/vk-guard-bucket/o/uploads%2Fa.pdf", "gs://vk-guard-bucket/uploads/a.pdf"},
	{"gcs regional endpoint", "https://storage.europe-west3.rep.googleapis.com/vk-guard-bucket/uploads/a.pdf", "gs://vk-guard-bucket/uploads/a.pdf"},
	{"gcs url, fully qualified host", "https://storage.googleapis.com./vk-guard-bucket/uploads/a.pdf", "gs://vk-guard-bucket/uploads/a.pdf"},
	{"gcs legacy host", "https://commondatastorage.googleapis.com/vk-guard-bucket/uploads/a.pdf", "gs://vk-guard-bucket/uploads/a.pdf"},
	{"gcs legacy bucket host", "https://vk-guard-bucket.commondatastorage.googleapis.com/uploads/a.pdf", "gs://vk-guard-bucket/uploads/a.pdf"},
	{"gcs download host", "https://storage-download.googleapis.com/vk-guard-bucket/uploads/a.pdf", "gs://vk-guard-bucket/uploads/a.pdf"},
	{"firebase storage url", "https://firebasestorage.googleapis.com/v0/b/vk-guard-bucket/o/uploads%2Fa.pdf?alt=media", "gs://vk-guard-bucket/uploads/a.pdf"},
	{"s3 url, fully qualified host", "https://vk-guard-bucket.s3.us-east-1.amazonaws.com./uploads/a.pdf", "s3://vk-guard-bucket/uploads/a.pdf"},
	{"gcs url with an encoded name", "https://storage.googleapis.com/vk-guard-bucket/uploads/a%20b.pdf", "gs://vk-guard-bucket/uploads/a b.pdf"},
	{"s3 bucket-host url", "https://vk-guard-bucket.s3.us-east-1.amazonaws.com/uploads/a.pdf", "s3://vk-guard-bucket/uploads/a.pdf"},
	{"s3 bucket-host url, no region", "https://vk-guard-bucket.s3.amazonaws.com/uploads/a.pdf", "s3://vk-guard-bucket/uploads/a.pdf"},
	{"s3 bucket-host url, dashed region", "https://vk-guard-bucket.s3-us-west-2.amazonaws.com/uploads/a.pdf", "s3://vk-guard-bucket/uploads/a.pdf"},
	{"s3 dualstack url", "https://vk-guard-bucket.s3.dualstack.eu-west-1.amazonaws.com/uploads/a.pdf", "s3://vk-guard-bucket/uploads/a.pdf"},
	{"s3 accelerate url", "https://vk-guard-bucket.s3-accelerate.amazonaws.com/uploads/a.pdf", "s3://vk-guard-bucket/uploads/a.pdf"},
	{"s3 url of a dotted bucket", "https://vk.guard.bucket.s3.us-east-1.amazonaws.com/uploads/a.pdf", "s3://vk.guard.bucket/uploads/a.pdf"},
	{"s3 path-style url", "https://s3.us-east-1.amazonaws.com/vk-guard-bucket/uploads/a.pdf", "s3://vk-guard-bucket/uploads/a.pdf"},
	{"s3 path-style url, no region", "https://s3.amazonaws.com/vk-guard-bucket/uploads/a.pdf", "s3://vk-guard-bucket/uploads/a.pdf"},
	{"s3 url in china", "https://vk-guard-bucket.s3.cn-north-1.amazonaws.com.cn/uploads/a.pdf", "s3://vk-guard-bucket/uploads/a.pdf"},
	{"s3 presigned url", "https://vk-guard-bucket.s3.us-east-1.amazonaws.com/uploads/a.pdf?X-Amz-Signature=abc", "s3://vk-guard-bucket/uploads/a.pdf"},
	{"s3 arn", "arn:aws:s3:::vk-guard-bucket/uploads/a.pdf", "s3://vk-guard-bucket/uploads/a.pdf"},
	// A bucket may carry the name of an endpoint kind; only the host's S3 label says what the endpoint is.
	{"s3 url of a bucket named accesspoint", "https://accesspoint.s3.us-east-1.amazonaws.com/uploads/a.pdf", "s3://accesspoint/uploads/a.pdf"},
	{"s3 url of a bucket named s3-control", "https://s3-control.s3.us-east-1.amazonaws.com/uploads/a.pdf", "s3://s3-control/uploads/a.pdf"},
	{"s3 url of a bucket with an s3-global label", "https://data.s3-global.s3.amazonaws.com/uploads/a.pdf", "s3://data.s3-global/uploads/a.pdf"},
	{"s3 arn in another partition", "arn:aws-cn:s3:::vk-guard-bucket/uploads/a.pdf", "s3://vk-guard-bucket/uploads/a.pdf"},
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
				for _, plain := range []string{"https://example.com/files/vk-guard", "data:image/png;base64,aGVsbG8=", "gs:/not-a-uri", "files/vk-guard", "https://storage.example.com/vk-guard-bucket/a.pdf", "https://bedrock-runtime.us-east-1.amazonaws.com/model/m/invoke"} {
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
		t.Run(name, func(t *testing.T) { check(t, name, fileReferenceFields[name] != fileRefID) })
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
		if got, ok, unverifiable := recordedFileID(form.written); !ok || unverifiable || got != form.recorded {
			t.Errorf("%s: recordedFileID(%q) = %q, %v; want %q", form.name, form.written, got, ok, form.recorded)
		}
	}
	for _, raw := range []string{
		"", "gs://", "s3://", "gs:/bucket/object", "https://example.com/gs://bucket/object",
		"https://example.com/v1beta/files/abc", "https://generativelanguage.googleapis.com/v1beta/models/m",
		"https://generativelanguage.googleapis.com.example.com/v1beta/files/abc",
		"data:application/pdf;base64," + strings.Repeat("A", 4096), "file-abc", "files/abc",
		// Hosts that only look like storage, and AWS and Google hosts that are not storage.
		"https://storage.googleapis.com.example.com/bucket/object", "https://example.com/storage.googleapis.com/bucket/object",
		"https://storage.googleapis.com/", "https://www.googleapis.com/drive/v3/files/abc",
		"https://bedrock-runtime.us-east-1.amazonaws.com/model/m/invoke", "https://s3.amazonaws.com/", "https://bucket.s3.amazonaws.com.example.com/key",
		"storage.googleapis.com/bucket/object", "arn:aws:iam::123456789012:role/r", "arn:aws:s3:::",
	} {
		if got, ok, unverifiable := recordedFileID(raw); ok || unverifiable {
			t.Errorf("recordedFileID(%.60q) = %q, %v, %v, want none", raw, got, ok, unverifiable)
		}
	}
	// A name that reaches S3 by a route that cannot be followed to a bucket refuses the request.
	chat := func(uri string) *schemas.BifrostRequest {
		return &schemas.BifrostRequest{RequestType: schemas.ChatCompletionRequest, ChatRequest: &schemas.BifrostChatRequest{Provider: schemas.Bedrock, Model: "m",
			Params: &schemas.ChatParameters{ExtraParams: map[string]interface{}{"document": uri}}}}
	}
	for _, raw := range []string{
		"arn:aws:s3:us-east-1:123456789012:accesspoint/ap/object/uploads/a.pdf", "arn:aws:s3::123456789012:accesspoint/m.mrap",
		"arn:aws:s3-object-lambda:us-east-1:123456789012:accesspoint/ol", "arn:aws:s3-outposts:us-east-1:123456789012:outpost/op/bucket/b",
		"https://ap-123456789012.s3-accesspoint.us-east-1.amazonaws.com/uploads/a.pdf", "https://ol-123456789012.s3-object-lambda.us-east-1.amazonaws.com/a.pdf",
		"https://m.mrap.accesspoint.s3-global.amazonaws.com/uploads/a.pdf", "https://123456789012.s3-control.us-east-1.amazonaws.com/v20180820/accesspoint/ap",
		"https://accesspoint.s3-accesspoint.us-east-1.amazonaws.com/uploads/a.pdf",
		// A googleapis.com host that looks like storage and is not one this resolver knows.
		"https://storage-other.googleapis.com/bucket/uploads/a.pdf", "https://bucket.storage-x.googleapis.com/uploads/a.pdf",
	} {
		if got, ok, unverifiable := recordedFileID(raw); ok || !unverifiable {
			t.Errorf("recordedFileID(%q) = %q, %v, %v, want unverifiable", raw, got, ok, unverifiable)
		}
		if _, unreadable := requestFileIDs(chat(raw)); !unreadable {
			t.Errorf("a request naming %q in an extra param is let through", raw)
		}
		if _, verifiable := distinctObjectRefs(jsonObjectRefs(schemas.Bedrock, map[string]any{"document": raw})); verifiable {
			t.Errorf("a raw body naming %q is let through", raw)
		}
		if ref, _ := objectRef("file", schemas.Bedrock, raw); !ref.unreadable {
			t.Errorf("a file addressed as %q is let through", raw)
		}
	}
}

// A stored object's name that cannot be resolved to the one name it is recorded under refuses
// the request: the object it reaches may be another key's.
func TestUnresolvableStorageNamesRefuseTheRequest(t *testing.T) {
	for _, name := range []string{
		// A "." or ".." segment is literal to the storage and resolved by an https client, so
		// which object is read depends on who reads it.
		"gs://bucket/mine/../out-other/a.jsonl", "s3://bucket/./out-other/a.jsonl", "gs://bucket/out/..",
		"https://storage.googleapis.com/bucket/mine/../out-other/a.jsonl", "https://storage.googleapis.com/bucket/mine/%2e%2e/out-other/a.jsonl",
		"https://bucket.s3.us-east-1.amazonaws.com/mine/../out-other/a.jsonl",
		// The bucket position: an https client resolves "/../bucket/a" to "/bucket/a".
		"https://storage.googleapis.com/../bucket/out-other/a.jsonl", "https://s3.amazonaws.com/./bucket/out-other/a.jsonl", "gs://../bucket/a.jsonl",
		// An S3 access point's alias, or an ARN, where the bucket goes: the same objects under
		// a name nothing is recorded under.
		"s3://ap-abcdefgh1234567890-s3alias/out-other/a.jsonl", "S3://AP-abcdefgh1234567890-S3Alias/out-other/a.jsonl", "s3://ol-abcdefgh--ol-s3/a.jsonl", "s3://op-abcdefgh--op-s3/a.jsonl",
		"s3://mfzwi23gnjvgw.mrap/out-other/a.jsonl", "s3://arn:aws:s3:us-east-1:123456789012:accesspoint/ap/out-other/a.jsonl",
		"https://ap-abcdefgh1234567890-s3alias.s3.us-east-1.amazonaws.com/out-other/a.jsonl", "https://s3.us-east-1.amazonaws.com/ap-abcdefgh1234567890-s3alias/out-other/a.jsonl",
		"arn:aws:s3:::ap-abcdefgh1234567890-s3alias/out-other/a.jsonl",
		// A wildcard names every object it matches.
		"gs://bucket/uploads/*", "gs://bucket/uploads/*.jsonl", "gs://bucket/up*/a.jsonl", "s3://bucket/uploads/a?.jsonl", "gs://bucket/uploads/[ab].jsonl",
		"https://storage.googleapis.com/bucket/uploads/*",
	} {
		ref, _ := objectRef("file", schemas.Vertex, name)
		if !ref.unreadable {
			t.Errorf("a file named %q is not refused", name)
		}
		if _, verifiable := distinctObjectRefs([]providerObjectRef{ref}); verifiable {
			t.Errorf("a request naming %q can be verified", name)
		}
	}
	for _, plain := range []string{
		"gs://bucket/out/a..b/c.jsonl", "gs://bucket/out/.hidden/c.jsonl", "s3://bucket/out/v1.2/c.jsonl", "s3://my-s3alias-data/c.jsonl", "gs://bucket-s3alias/c.jsonl", "s3://bucket--x-s3/c.jsonl",
		"s3://bucket/out//c.jsonl", "gs://bucket", "file-abc", "files/abc", "https://example.com/a/../b",
	} {
		if ref, _ := objectRef("file", schemas.Vertex, plain); ref.unreadable {
			t.Errorf("a file named %q is refused", plain)
		}
	}
	// An object is looked up under the name it is recorded under, whatever form named it, and
	// under the id as written.
	for written, recorded := range map[string]string{
		"https://storage.googleapis.com/bucket/out/a.jsonl":    "gs://bucket/out/a.jsonl",
		"GS://bucket/out/a.jsonl ":                             "gs://bucket/out/a.jsonl",
		"arn:aws:s3:::bucket/out/a.jsonl":                      "s3://bucket/out/a.jsonl",
		"https://generativelanguage.googleapis.com/v1/files/x": "files/x",
	} {
		ref, _ := objectRef("file", schemas.Vertex, written)
		if !slices.Contains(ref.ids, recorded) || !slices.Contains(ref.ids, written) || ref.unreadable {
			t.Errorf("a file named %q is looked up under %q, want %q among them", written, ref.ids, recorded)
		}
	}
}

// A Bedrock file upload names the object it writes in pieces: a bucket, a folder and a
// filename. The object they compose is checked like any other the request names.
func TestUploadLocationIsChecked(t *testing.T) {
	upload := func(config *schemas.FileStorageConfig, extra map[string]interface{}, filename string) *schemas.BifrostRequest {
		return &schemas.BifrostRequest{RequestType: schemas.FileUploadRequest, FileUploadRequest: &schemas.BifrostFileUploadRequest{Provider: schemas.Bedrock, Filename: filename, StorageConfig: config, ExtraParams: extra}}
	}
	s3 := func(bucket, prefix string) *schemas.FileStorageConfig {
		return &schemas.FileStorageConfig{S3: &schemas.S3StorageConfig{Bucket: bucket, Prefix: prefix}}
	}
	for name, tc := range map[string]struct {
		req  *schemas.BifrostRequest
		want string
	}{
		"s3 bucket, prefix and filename":       {upload(s3("bucket", "uploads/"), nil, "a.jsonl"), "s3://bucket/uploads/a.jsonl"},
		"s3 prefix with slashes around it":     {upload(s3("bucket", "/uploads/in/"), nil, "a.jsonl"), "s3://bucket/uploads/in/a.jsonl"},
		"s3 bucket and filename":               {upload(s3("bucket", ""), nil, "a.jsonl"), "s3://bucket/a.jsonl"},
		"s3 bucket written as a uri":           {upload(s3("s3://bucket/tenant/", "uploads"), nil, "a.jsonl"), "s3://bucket/tenant/uploads/a.jsonl"},
		"s3 bucket and prefix in extra params": {upload(nil, map[string]interface{}{"s3_bucket": "bucket", "s3_prefix": "uploads"}, "a.jsonl"), "s3://bucket/uploads/a.jsonl"},
	} {
		if !namesFile(tc.req, tc.want) {
			t.Errorf("%s: the upload is not checked at %q; references: %+v", name, tc.want, providerObjectReferences(tc.req))
		}
	}
	// Both S3 sources are checked when both are present: which one the provider reads is its
	// choice.
	both := upload(s3("bucket", "uploads"), map[string]interface{}{"s3_bucket": "s3://other-bucket/in/", "s3_prefix": "x"}, "a.jsonl")
	for _, want := range []string{"s3://bucket/uploads/a.jsonl", "s3://other-bucket/in/x/a.jsonl"} {
		if !namesFile(both, want) {
			t.Errorf("an upload carrying an S3 config and extra params is not checked at %q; references: %+v", want, providerObjectReferences(both))
		}
	}
	// A Vertex upload writes under a fresh folder and replaces nothing, and an upload with no
	// bucket or no filename names no object.
	for name, req := range map[string]*schemas.BifrostRequest{
		"a gcs config": upload(&schemas.FileStorageConfig{GCS: &schemas.GCSStorageConfig{Bucket: "bucket", Prefix: "uploads"}}, nil, "a.jsonl"),
		"no bucket":    upload(nil, nil, "a.jsonl"),
		"no filename":  upload(s3("bucket", "uploads"), nil, ""),
	} {
		if refs := requestFileRefs(req); len(refs) != 0 {
			t.Errorf("an upload with %s was read as %+v", name, refs)
		}
	}
	// A filename that climbs out of the folder cannot be verified.
	if _, verifiable := distinctObjectRefs(providerObjectReferences(upload(s3("bucket", "mine"), nil, "../out-other/a.jsonl"))); verifiable {
		t.Errorf("an upload named ../out-other/a.jsonl is let through")
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
		if id, ok, _ := recordedFileID(padded); !ok || id != "files/vk-guard" {
			t.Errorf("a padded Gemini file URI resolved to %q, %v", id, ok)
		}
		if req := chatWith(map[string]interface{}{"video": padded}); !namesFile(req, "files/vk-guard") {
			t.Errorf("a padded Gemini file URI is not checked")
		}
	})

	t.Run("more files than the bound refuses the request", func(t *testing.T) {
		list := func(n int) []any {
			uris := make([]any, 0, n)
			for i := 0; i < n; i++ {
				uris = append(uris, fmt.Sprintf("s3://vk-guard-bucket/uploads/%d.pdf", i))
			}
			return uris
		}
		if ids, unreadable := requestFileIDs(chatWith(map[string]interface{}{"vendor_field": list(maxFileReferences)})); unreadable || len(ids) != maxFileReferences {
			t.Errorf("%d files: got %d ids, unreadable %v", maxFileReferences, len(ids), unreadable)
		}
		ids, unreadable := requestFileIDs(chatWith(map[string]interface{}{"vendor_field": list(10000)}))
		if !unreadable || len(ids) > maxFileReferences {
			t.Errorf("10000 files: %d ids would each be looked up, unreadable %v", len(ids), unreadable)
		}
		// A raw body (passthrough, an inline batch item) is bounded the same way.
		bodyRefs := jsonObjectRefs(schemas.Bedrock, map[string]any{"documents": list(10000)})
		if len(bodyRefs) > maxFileReferences+1 || !bodyRefs[len(bodyRefs)-1].unreadable {
			t.Errorf("a raw body naming 10000 files yields %d references and no refusal", len(bodyRefs))
		}
		// One file repeated in a raw body is one reference, under a file-id key or not.
		var history []any
		for i := 0; i < 500; i++ {
			history = append(history, map[string]any{"file_id": uri}, map[string]any{"fileUri": uri})
		}
		if refs := jsonObjectRefs(schemas.Vertex, map[string]any{"contents": history}); len(refs) != 1 || refs[0].unreadable {
			t.Errorf("one file named 1000 times in a raw body yields %d references", len(refs))
		}
		// The bound is over the whole request: a batch whose inline items each name a few files
		// is counted across its items, and refused before any lookup.
		items := make([]schemas.BatchRequestItem, 0, 400)
		for i := 0; i < 400; i++ {
			items = append(items, schemas.BatchRequestItem{CustomID: fmt.Sprint(i), Body: map[string]any{"file_id": fmt.Sprintf("file-%d", i)}})
		}
		batch := &schemas.BifrostRequest{RequestType: schemas.BatchCreateRequest, BatchCreateRequest: &schemas.BifrostBatchCreateRequest{Provider: schemas.OpenAI, Requests: items}}
		if _, verifiable := distinctObjectRefs(providerObjectReferences(batch)); verifiable {
			t.Errorf("a batch naming 400 distinct files across its items is within the bound")
		}
		same := make([]schemas.BatchRequestItem, 0, 400)
		for i := 0; i < 400; i++ {
			same = append(same, schemas.BatchRequestItem{CustomID: fmt.Sprint(i), Body: map[string]any{"file_id": "file-shared"}})
		}
		batch.BatchCreateRequest.Requests = same
		if refs, verifiable := distinctObjectRefs(providerObjectReferences(batch)); !verifiable || len(refs) != 1 {
			t.Errorf("a batch naming one file 400 times: %d lookups, verifiable %v", len(refs), verifiable)
		}
		// The same file repeated is one lookup, however often it is named.
		repeated := make([]any, 10000)
		for i := range repeated {
			repeated[i] = uri
		}
		if ids, unreadable := requestFileIDs(chatWith(map[string]interface{}{"vendor_field": repeated})); unreadable || len(ids) != 1 {
			t.Errorf("one file named 10000 times: got %d ids, unreadable %v", len(ids), unreadable)
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
		`{"image":"https://Storage.GoogleAPIs.com/b/o"}`, `{"image":"https:\/\/storage.cloud.google.com\/b\/o"}`,
		`{"image":"https://b.s3.us-east-1.AmazonAWS.com/k"}`, `{"document":"ARN:aws:s3:::b/k"}`,
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
		`{"instances":[{"image":"https:\/\/storage.googleapis.com\/b\/o"}]}`:                     "gs://b/o",
		`{"parameters":{"storageUri":"https://b.s3.us-east-1.amazonaws.com/out/"}}`:              "s3://b/out/",
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
