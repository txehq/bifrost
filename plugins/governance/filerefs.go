package governance

import (
	"bytes"
	"net/url"
	"reflect"
	"strings"
	"sync"

	"github.com/capsohq/bifrost/core/schemas"
	configstoreTables "github.com/capsohq/bifrost/framework/configstore/tables"
)

// File references in requests (TXE-7032).
//
// A request can name an uploaded file in many places: a file_id in a message block, a URL in a
// file, image, video or document input, a tool's environment, an extra param the provider
// forwards. The provider then reads that file under the shared key, so each of those places has
// to pass the same ownership check as a file addressed by id. Before this file each surface had
// its own extraction, written when the surface was added, and each knew one id form (a file_id,
// or a Gemini file URL). A surface nobody wrote an extraction for, or an id form the extraction
// did not know (the gs:// URI a Vertex upload is recorded under, Bedrock's s3:// URI), was
// simply not checked.
//
// There is now one path:
//
//   - fileReferenceFields names every typed request field that can carry a file reference, on
//     any request type. requestFileIDs walks a request through that table, so a field listed
//     there is checked wherever its type appears. TestEveryFileReferenceFieldIsClassified fails
//     when a request type gains a field that looks like a reference and is neither listed here
//     nor excused there with a reason.
//   - recordedFileID is the only place an id form is recognised. Every surface, typed, extra
//     param or raw passthrough body, resolves a URL through it.

// fileRefKind says how a field names a file.
type fileRefKind uint8

const (
	// fileRefID is a field holding a file id as the provider issued it; it names a file
	// whatever its form.
	fileRefID fileRefKind = iota + 1
	// fileRefURL is a field holding a URL. It names a recorded file only when the URL has one
	// of the forms a provider records an upload under (see recordedFileID).
	fileRefURL
)

// fileReferenceFields is the single list of typed request fields that can name an uploaded
// file, keyed "<struct type>.<field>". A field is walked wherever its struct appears in a
// request, so listing it once covers every request type that embeds it (a Responses input block
// is checked on responses, count-tokens and compaction requests alike).
//
// Fields that address the request's own object (FileRetrieveRequest.FileID) or that the
// container and batch rules already read are not listed: providerObjectReference and
// namedProviderObjects own them.
var fileReferenceFields = map[string]fileRefKind{
	// Chat content blocks.
	"ChatInputFile.FileID":  fileRefID,
	"ChatInputFile.FileURL": fileRefURL,
	"ChatInputImage.FileID": fileRefID,
	"ChatInputImage.URL":    fileRefURL,
	// Responses input blocks and the items a conversation replays.
	"ResponsesMessageContentBlock.FileID":                 fileRefID,
	"ResponsesInputMessageContentBlockFile.FileURL":       fileRefURL,
	"ResponsesInputMessageContentBlockImage.ImageURL":     fileRefURL,
	"ResponsesComputerToolCallOutputData.FileID":          fileRefID,
	"ResponsesComputerToolCallOutputData.ImageURL":        fileRefURL,
	"ResponsesToolImageGenerationInputImageMask.FileID":   fileRefID,
	"ResponsesToolImageGenerationInputImageMask.ImageURL": fileRefURL,
	"ResponsesToolShellEnvironment.FileIDs":               fileRefID,
	"ResponsesWebFetchSource.FileID":                      fileRefID,
	"ResponsesWebFetchSource.URL":                         fileRefURL,
	// File ids an earlier answer carried, replayed as conversation history. The caller
	// normally got them from its own answer; checking them costs nothing for that caller and
	// keeps a replayed item from naming another key's file.
	"ResponsesCodeExecutionFileOutput.FileID":            fileRefID,
	"ResponsesFileSearchToolCallResult.FileID":           fileRefID,
	"ResponsesOutputMessageContentTextAnnotation.FileID": fileRefID,
	// Media inputs of the other inference requests.
	"EmbeddingMediaPart.URL":                    fileRefURL,
	"ImageInput.URL":                            fileRefURL,
	"ImageGenerationParameters.InputImages":     fileRefURL,
	"OCRDocument.DocumentURL":                   fileRefURL,
	"OCRDocument.ImageURL":                      fileRefURL,
	"DecisionInputPart.ImageURL":                fileRefURL,
	"VideoGenerationInput.InputReference":       fileRefURL,
	"VideoGenerationInput.VideoURI":             fileRefURL,
	"VideoGenerationParameters.LastFrame":       fileRefURL,
	"VideoGenerationParameters.ReferenceImages": fileRefURL,
	"VideoInput.URL":                            fileRefURL,
}

// fileReferenceOpenFields are the fields that hold provider-shaped data with no fixed type: the
// extra params a provider forwards, and a cached content's contents. Every string in them is
// read as a possible file reference (see scanOpenValue).
var fileReferenceOpenFields = map[string]struct{}{
	"ExtraParams":       {},
	"Contents":          {},
	"SystemInstruction": {},
}

// geminiFileHost is the host of a Gemini file URI, "https://generativelanguage.googleapis.com/<version>/files/<id>".
const geminiFileHost = "generativelanguage.googleapis.com"

// storageURISchemes are the object-storage schemes a provider reads with its own credential,
// and records an upload under: Vertex's file upload returns "gs://<bucket>/<object>" as the
// file id, Bedrock's "s3://<bucket>/<key>".
var storageURISchemes = [...]string{"gs://", "s3://"}

// recordedFileID returns the id a file named by a URL is recorded under, or false when the URL
// has none of the forms a provider records an upload under. It is the one place those forms are
// known:
//
//   - a Gemini file URI, "https://generativelanguage.googleapis.com/<version>/files/<id>", is the
//     file recorded as "files/<id>", the name the upload response carries;
//   - a storage URI, "gs://<bucket>/<object>" or "s3://<bucket>/<key>", is recorded as written
//     by the Vertex and Bedrock uploads. The scheme is matched in any case and returned in
//     lower case, which can only refuse more: the rest of the URI is the object's own name and
//     is compared exactly.
//
// Any other URL (an https page, a data: URL) names no recorded file. It costs one length check
// and a prefix compare for those, and allocates only when the scheme was written in upper case
// or the URL is a Gemini file URI.
func recordedFileID(rawURL string) (string, bool) {
	raw := strings.TrimSpace(rawURL)
	if len(raw) < len("s3://x") {
		return "", false
	}
	for _, scheme := range storageURISchemes {
		if len(raw) > len(scheme) && strings.EqualFold(raw[:len(scheme)], scheme) {
			if raw[:len(scheme)] == scheme {
				return raw, true
			}
			return scheme + raw[len(scheme):], true
		}
	}
	// A data: URL can be megabytes long and is never a file URI; neither is anything that does
	// not mention the Gemini host. Length alone excuses nothing: a file URI padded with a long
	// query is still that file.
	if len(raw) >= len("data:") && strings.EqualFold(raw[:len("data:")], "data:") {
		return "", false
	}
	if !containsFold(raw, geminiFileHost) {
		return "", false
	}
	parsed, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(parsed.Hostname(), geminiFileHost) {
		return "", false
	}
	segments := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	for i := 0; i+1 < len(segments); i++ {
		if segments[i] == "files" && segments[i+1] != "" {
			return "files/" + strings.SplitN(segments[i+1], ":", 2)[0], true
		}
	}
	return "", false
}

// containsFold reports whether s contains substr, ignoring ASCII case, without allocating.
// substr is lower-case ASCII. Only positions that hold its first letter are compared, so a long
// string costs one pass.
func containsFold(s, substr string) bool {
	first := substr[0]
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i]|0x20 == first && strings.EqualFold(s[i:i+len(substr)], substr) {
			return true
		}
	}
	return false
}

// fileRefStep is one field of a struct type that leads to a file reference: a reference field
// itself (kind set), an open field (open set), or a field to descend into (next set).
type fileRefStep struct {
	index int
	kind  fileRefKind
	open  bool
	next  *fileRefPlan
}

// fileRefPlan is the list of fields of one struct type worth visiting. A type none of whose
// fields can reach a reference has an empty plan and is never entered.
type fileRefPlan struct {
	steps []fileRefStep
}

var (
	fileRefPlansOnce sync.Once
	fileRefRootPlan  *fileRefPlan
)

// requestFileRefPlan returns the plan for schemas.BifrostRequest, built once.
func requestFileRefPlan() *fileRefPlan {
	fileRefPlansOnce.Do(func() {
		fileRefRootPlan = buildFileRefPlans(reflect.TypeOf(schemas.BifrostRequest{}))
	})
	return fileRefRootPlan
}

// structTypeOf strips pointers, slices, arrays and map values down to the struct type a field
// holds, or returns nil when it holds none.
func structTypeOf(rt reflect.Type) reflect.Type {
	for {
		switch rt.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
			rt = rt.Elem()
		case reflect.Struct:
			return rt
		default:
			return nil
		}
	}
}

// isOpenType reports whether a field's type holds untyped provider data: an interface, or a map
// or slice of them.
func isOpenType(rt reflect.Type) bool {
	for {
		switch rt.Kind() {
		case reflect.Interface:
			return true
		case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
			rt = rt.Elem()
		default:
			return false
		}
	}
}

// isStringType reports whether a field holds a string, a pointer to one or a list of them.
func isStringType(rt reflect.Type) bool {
	for rt.Kind() == reflect.Pointer || rt.Kind() == reflect.Slice || rt.Kind() == reflect.Array {
		rt = rt.Elem()
	}
	return rt.Kind() == reflect.String
}

// buildFileRefPlans builds the plan of every struct type reachable from root and returns
// root's. Request types are recursive (a message holds tool calls that hold messages), so the
// types are collected first and "can reach a reference" is settled by iterating to a fixed
// point; a plan then keeps only the fields that can.
func buildFileRefPlans(root reflect.Type) *fileRefPlan {
	plans := map[reflect.Type]*fileRefPlan{}
	var order []reflect.Type
	var collect func(rt reflect.Type)
	collect = func(rt reflect.Type) {
		if _, seen := plans[rt]; seen {
			return
		}
		plans[rt] = &fileRefPlan{}
		order = append(order, rt)
		for i := 0; i < rt.NumField(); i++ {
			if next := structTypeOf(rt.Field(i).Type); next != nil {
				collect(next)
			}
		}
	}
	collect(root)

	direct := func(rt reflect.Type, field reflect.StructField) (fileRefKind, bool) {
		if !field.IsExported() {
			return 0, false
		}
		if kind, ok := fileReferenceFields[rt.Name()+"."+field.Name]; ok && isStringType(field.Type) {
			return kind, false
		}
		if _, ok := fileReferenceOpenFields[field.Name]; ok && isOpenType(field.Type) {
			return 0, true
		}
		return 0, false
	}
	reaches := map[reflect.Type]bool{}
	for changed := true; changed; {
		changed = false
		for _, rt := range order {
			if reaches[rt] {
				continue
			}
			for i := 0; i < rt.NumField(); i++ {
				field := rt.Field(i)
				kind, open := direct(rt, field)
				next := structTypeOf(field.Type)
				if kind != 0 || open || (field.IsExported() && next != nil && reaches[next]) {
					reaches[rt] = true
					changed = true
					break
				}
			}
		}
	}
	for _, rt := range order {
		plan := plans[rt]
		for i := 0; i < rt.NumField(); i++ {
			field := rt.Field(i)
			kind, open := direct(rt, field)
			switch {
			case kind != 0:
				plan.steps = append(plan.steps, fileRefStep{index: i, kind: kind})
			case open:
				plan.steps = append(plan.steps, fileRefStep{index: i, open: true})
			default:
				if next := structTypeOf(field.Type); field.IsExported() && next != nil && reaches[next] {
					plan.steps = append(plan.steps, fileRefStep{index: i, next: plans[next]})
				}
			}
		}
	}
	return plans[root]
}

// fileIDCollector gathers the file ids a request names, each once, in the order found.
//
// unreadable is set when the request cannot be verified from the ids gathered: untyped data
// nested past openValueDepth, raw JSON that does not parse, or more than maxFileReferences
// distinct files. What was not read, or not kept, may name another key's file.
type fileIDCollector struct {
	ids        []string
	seen       map[string]struct{}
	unreadable bool // the ids are not the whole answer; the request is refused
}

// maxFileReferences bounds the distinct files one request may name. Each is one ownership
// lookup against the config store before the provider is called, so without a bound a single
// request listing thousands of made-up URIs would run thousands of queries. No provider takes
// anywhere near this many files in one request.
const maxFileReferences = 128

func (c *fileIDCollector) add(id string) {
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	if _, dup := c.seen[id]; dup {
		return
	}
	if len(c.ids) >= maxFileReferences {
		c.unreadable = true
		return
	}
	if c.seen == nil {
		c.seen = make(map[string]struct{}, 4)
	}
	c.seen[id] = struct{}{}
	c.ids = append(c.ids, id)
}

// addReference adds what a reference field of the given kind names.
func (c *fileIDCollector) addReference(kind fileRefKind, value string) {
	if kind == fileRefID {
		c.add(value)
		return
	}
	if id, ok := recordedFileID(value); ok {
		c.add(id)
	}
}

// requestFileIDs returns the uploaded files a request names anywhere in it, under the id each
// is recorded under: every field of fileReferenceFields on whatever request type is set, and
// every reference inside its extra params. It is the one extraction every inference request
// goes through. unreadable is true when part of the request could not be read or it names more
// than maxFileReferences files, in which case the list is not the whole answer and the request
// must not be let through on it.
func requestFileIDs(req *schemas.BifrostRequest) (ids []string, unreadable bool) {
	if req == nil {
		return nil, false
	}
	var collector fileIDCollector
	walkFileRefs(reflect.ValueOf(req).Elem(), requestFileRefPlan(), &collector)
	return collector.ids, collector.unreadable
}

// walkFileRefs visits the planned fields of a struct value.
func walkFileRefs(value reflect.Value, plan *fileRefPlan, collector *fileIDCollector) {
	for i := range plan.steps {
		step := &plan.steps[i]
		field := value.Field(step.index)
		switch {
		case step.kind != 0:
			walkStrings(field, step.kind, collector)
		case step.open:
			scanOpenValue(field, collector, 0)
		default:
			walkStructs(field, step.next, collector)
		}
	}
}

// walkStructs follows a field down to the structs it holds.
func walkStructs(value reflect.Value, plan *fileRefPlan, collector *fileIDCollector) {
	switch value.Kind() {
	case reflect.Pointer:
		if !value.IsNil() {
			walkStructs(value.Elem(), plan, collector)
		}
	case reflect.Struct:
		walkFileRefs(value, plan, collector)
	case reflect.Slice, reflect.Array:
		for i := 0; i < value.Len(); i++ {
			walkStructs(value.Index(i), plan, collector)
		}
	case reflect.Map:
		iter := value.MapRange()
		for iter.Next() {
			walkStructs(iter.Value(), plan, collector)
		}
	}
}

// walkStrings reads a reference field: a string, a pointer to one or a list of them.
func walkStrings(value reflect.Value, kind fileRefKind, collector *fileIDCollector) {
	switch value.Kind() {
	case reflect.String:
		if value.Len() > 0 {
			collector.addReference(kind, value.String())
		}
	case reflect.Pointer:
		if !value.IsNil() {
			walkStrings(value.Elem(), kind, collector)
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < value.Len(); i++ {
			walkStrings(value.Index(i), kind, collector)
		}
	}
}

// openValueDepth bounds the walk of untyped data; provider bodies are a few levels deep. Data
// nested deeper (or a typed value that points back at itself) is not read further and marks
// the request unreadable, which refuses it: stopping quietly would let a reference placed below
// the bound through.
const openValueDepth = 32

// fileIDKeys are the keys untyped data names a file id under, as jsonObjectRefs reads them.
func isFileIDKey(key string) bool {
	switch key {
	case "file_id", "fileId", "input_file_id", "file_ids", "fileIds":
		return true
	}
	return false
}

// scanOpenValue reads untyped provider data (extra params, a cached content's contents, a
// typed provider struct stored in an extra param) for file references. The key a provider
// reads a location under differs by provider and operation (fileUri, gcsUri, uri, url, image),
// so the key is not consulted: every string that has a recorded-file form names that file. A
// string under a file-id key names a file whatever its form.
func scanOpenValue(value reflect.Value, collector *fileIDCollector, depth int) {
	if depth > openValueDepth {
		switch value.Kind() {
		case reflect.Interface, reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map, reflect.Struct, reflect.String:
			collector.unreadable = true
		}
		return
	}
	switch value.Kind() {
	case reflect.Interface, reflect.Pointer:
		if !value.IsNil() {
			scanOpenValue(value.Elem(), collector, depth+1)
		}
	case reflect.String:
		if value.Len() > 0 {
			collector.addReference(fileRefURL, value.String())
		}
	case reflect.Slice, reflect.Array:
		if value.Type().Elem().Kind() == reflect.Uint8 {
			scanOpenBytes(value, collector, depth)
			return
		}
		for i := 0; i < value.Len(); i++ {
			scanOpenValue(value.Index(i), collector, depth+1)
		}
	case reflect.Map:
		iter := value.MapRange()
		for iter.Next() {
			item := iter.Value()
			if key := iter.Key(); key.Kind() == reflect.String && isFileIDKey(key.String()) {
				scanFileIDs(item, collector, depth+1)
				continue
			}
			scanOpenValue(item, collector, depth+1)
		}
	case reflect.Struct:
		for i := 0; i < value.NumField(); i++ {
			if value.Type().Field(i).IsExported() {
				scanOpenValue(value.Field(i), collector, depth+1)
			}
		}
	}
}

// scanOpenBytes reads a byte slice stored in untyped data. A route can keep a vendor member as
// raw JSON (json.RawMessage), which the provider forwards as written, so bytes that could name
// an object are decoded and read like any other value; bytes with no JSON shape are read as one
// string, and JSON-shaped bytes that do not decode mark the request unreadable. Media bytes carry none of the markers and are not touched.
func scanOpenBytes(value reflect.Value, collector *fileIDCollector, depth int) {
	if value.Kind() != reflect.Slice || value.Len() == 0 {
		return
	}
	raw := value.Bytes()
	if !mayNameProviderObjects(raw) {
		return
	}
	var decoded any
	if err := schemas.Unmarshal(raw, &decoded); err != nil {
		// Bytes shaped like JSON that this decoder cannot read may still be read by the
		// provider's, so what they name is unknown and the request is refused. Bytes with no
		// JSON shape at all are a plain string.
		switch trimmed := bytes.TrimSpace(raw); {
		case len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[' || trimmed[0] == '"'):
			collector.unreadable = true
		default:
			collector.addReference(fileRefURL, string(raw))
		}
		return
	}
	scanOpenValue(reflect.ValueOf(decoded), collector, depth+1)
}

// scanFileIDs adds the strings under a file-id key as ids.
func scanFileIDs(value reflect.Value, collector *fileIDCollector, depth int) {
	if depth > openValueDepth {
		collector.unreadable = true
		return
	}
	switch value.Kind() {
	case reflect.Interface, reflect.Pointer:
		if !value.IsNil() {
			scanFileIDs(value.Elem(), collector, depth+1)
		}
	case reflect.String:
		collector.add(value.String())
	case reflect.Slice, reflect.Array:
		for i := 0; i < value.Len(); i++ {
			scanFileIDs(value.Index(i), collector, depth+1)
		}
	}
}

// requestFileRefs returns requestFileIDs as indirect file references on the request's provider.
// A request part of which could not be read yields one more reference, marked unreadable, which
// enforceProviderObjectOwnership refuses.
func requestFileRefs(req *schemas.BifrostRequest) []providerObjectRef {
	ids, unreadable := requestFileIDs(req)
	if len(ids) == 0 && !unreadable {
		return nil
	}
	provider, _, _ := req.GetRequestFields()
	refs := make([]providerObjectRef, 0, len(ids)+1)
	if unreadable {
		refs = append(refs, providerObjectRef{kind: configstoreTables.ProviderObjectKindFile, provider: string(provider), indirect: true, unreadable: true})
	}
	for _, id := range ids {
		if ref, ok := objectRef(configstoreTables.ProviderObjectKindFile, provider, id); ok {
			ref.indirect = true
			refs = append(refs, ref)
		}
	}
	return refs
}
