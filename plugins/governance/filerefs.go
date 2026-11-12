package governance

import (
	"bytes"
	"net/url"
	"reflect"
	"slices"
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
//
// A request also names storage it writes to (TXE-7105): a task's output_storage_uri, a batch's
// output folder, the object a file upload stores. Those are in the same table and go through
// the same resolver, so a location recorded for another key is refused as an output like it is
// as an input. A stored object is looked up under the one name it is recorded under; whether a
// recorded folder should also answer for what is inside it is not decided here (TXE-7121).

// fileRefKind says how a field names a file.
type fileRefKind uint8

const (
	// fileRefID is a field holding a file id as the provider issued it; it names a file
	// whatever its form.
	fileRefID fileRefKind = iota + 1
	// fileRefURL is a field holding a URL. It names a recorded file only when the URL has one
	// of the forms a provider records an upload under (see recordedFileID).
	fileRefURL
	// fileRefLocation is a field holding the storage location a request writes its output to.
	// It resolves like a URL: the location is a recorded object only in the forms
	// recordedFileID knows.
	fileRefLocation
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
	// A batch's input named by storage URL instead of by file id.
	"BifrostBatchCreateRequest.InputBlob": fileRefURL,
	// Where a request writes: the provider writes there with the shared credential, so a
	// location recorded for another key is as closed as that key's file.
	"BatchOutputFolder.URL":                  fileRefLocation,
	"TaskControlParameters.OutputStorageURI": fileRefLocation,
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

// The hosts an object in Cloud Storage or S3 is addressed under over https.
const (
	gcsHost            = "storage.googleapis.com"
	gcsBrowserHost     = "storage.cloud.google.com"
	googleAPIsDomain   = "googleapis.com"
	awsDomain          = "amazonaws.com"
	awsChinaDomain     = "amazonaws.com.cn"
	s3ARNPrefix        = "arn:"
	gcsJSONAPIPath     = "storage/v1/b/"
	gcsJSONObjectsPart = "/o/"
)

// recordedFileID returns the id a file named by a URL is recorded under, or false when the URL
// has none of the forms a provider records an upload under. It is the one place those forms are
// known, and every form of one object resolves to one id:
//
//   - a Gemini file URI, "https://generativelanguage.googleapis.com/<version>/files/<id>", is the
//     file recorded as "files/<id>", the name the upload response carries;
//   - a storage URI, "gs://<bucket>/<object>" or "s3://<bucket>/<key>", is recorded as written
//     by the Vertex and Bedrock uploads. The scheme is matched in any case and returned in
//     lower case, which can only refuse more: the rest of the URI is the object's own name and
//     is compared exactly;
//   - the https forms of a Cloud Storage object resolve to its gs:// URI: path style
//     ("https://storage.googleapis.com/<bucket>/<object>", also on storage.cloud.google.com and
//     the regional and mTLS hosts), bucket-host style ("https://<bucket>.storage.googleapis.com/<object>")
//     and the JSON API ("https://storage.googleapis.com/[download/|upload/]storage/v1/b/<bucket>/o/<object>",
//     also on www.googleapis.com);
//   - the https forms of an S3 object resolve to its s3:// URI: bucket-host style
//     ("https://<bucket>.s3.<region>.amazonaws.com/<key>", with or without the region, with
//     "s3-<region>", dualstack, FIPS or accelerate labels, and under amazonaws.com.cn) and path
//     style ("https://s3.<region>.amazonaws.com/<bucket>/<key>"), and so does its ARN
//     ("arn:<partition>:s3:::<bucket>/<key>").
//
// A query string (a signed URL's signature, alt=media) is not part of the object's name. Any
// other URL (an https page, a data: URL) names no recorded file. It costs one length check, a
// prefix compare and one pass for those, and allocates only when the scheme was written in
// upper case or the URL is on one of the hosts above.
//
// unverifiable is true for a name that reaches S3 by a route this resolver cannot follow to a
// bucket and key: an access point, a multi-region access point, Object Lambda or Outposts, by
// ARN or by host. Such a name may be another key's object under a name nothing is recorded
// under, so the request that carries it is refused rather than let through as naming no file.
func recordedFileID(rawURL string) (id string, ok, unverifiable bool) {
	raw := strings.TrimSpace(rawURL)
	if len(raw) < len("s3://x") {
		return "", false, false
	}
	for _, scheme := range storageURISchemes {
		if len(raw) > len(scheme) && strings.EqualFold(raw[:len(scheme)], scheme) {
			if raw[:len(scheme)] == scheme {
				return raw, true, false
			}
			return scheme + raw[len(scheme):], true, false
		}
	}
	// A data: URL can be megabytes long and is never a file URI; neither is anything that does
	// not mention a storage host. Length alone excuses nothing: a file URI padded with a long
	// query is still that file.
	if len(raw) >= len("data:") && strings.EqualFold(raw[:len("data:")], "data:") {
		return "", false, false
	}
	if strings.EqualFold(raw[:len(s3ARNPrefix)], s3ARNPrefix) {
		return s3ARNObject(raw)
	}
	if !namesStorageHost(raw) {
		return "", false, false
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", false, false
	}
	// A fully qualified name ("storage.googleapis.com.") is the same host.
	host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	if host == geminiFileHost {
		segments := strings.Split(strings.Trim(parsed.Path, "/"), "/")
		for i := 0; i+1 < len(segments); i++ {
			if segments[i] == "files" && segments[i+1] != "" {
				return "files/" + strings.SplitN(segments[i+1], ":", 2)[0], true, false
			}
		}
		return "", false, false
	}
	// parsed.Path is the decoded path: an object's name in a URL is percent-encoded, in a
	// gs:// or s3:// URI it is written as it is.
	if id, ok, unverifiable := gcsObjectURL(host, parsed.Path); ok || unverifiable {
		return id, ok, unverifiable
	}
	return s3ObjectURL(host, parsed.Path)
}

// gcsPathStyleHosts are the hosts that address a Cloud Storage object as "/<bucket>/<object>",
// current and legacy; each but the browser host also takes "<bucket>." in front (see
// gcsBucketHostSuffixes).
var gcsPathStyleHosts = [...]string{gcsHost, gcsBrowserHost, "storage.mtls." + googleAPIsDomain, "commondatastorage." + googleAPIsDomain, "storage-download." + googleAPIsDomain, "storage-upload." + googleAPIsDomain}

var gcsBucketHostSuffixes = [...]string{"." + gcsHost, ".commondatastorage." + googleAPIsDomain}

// gcsObjectURL resolves the host and decoded path of an https URL on a Cloud Storage host to
// the object's gs:// URI. A googleapis.com host that looks like storage and is none of the
// known ones is unverifiable: it may address a bucket in a way this resolver does not know.
func gcsObjectURL(host, urlPath string) (id string, ok, unverifiable bool) {
	objectPath := strings.TrimPrefix(urlPath, "/")
	var bucket, object string
	switch {
	case slices.Contains(gcsPathStyleHosts[:], host), host == "www."+googleAPIsDomain,
		strings.HasPrefix(host, "storage.") && strings.HasSuffix(host, ".rep."+googleAPIsDomain):
		api := strings.TrimPrefix(strings.TrimPrefix(objectPath, "download/"), "upload/")
		if rest, isAPI := strings.CutPrefix(api, gcsJSONAPIPath); isAPI {
			// The JSON API: "b/<bucket>/o/<object>". A path that stops at the bucket, or
			// goes on to something other than an object, names the bucket.
			if b, o, found := strings.Cut(rest, gcsJSONObjectsPart); found {
				bucket, object = b, o
			} else {
				bucket, _, _ = strings.Cut(rest, "/")
			}
		} else if host == "www."+googleAPIsDomain {
			return "", false, false
		} else {
			bucket, object, _ = strings.Cut(objectPath, "/")
		}
	case host == "firebasestorage."+googleAPIsDomain:
		// Firebase's address of the same bucket: "/v0/b/<bucket>/o/<object>".
		rest, isAPI := strings.CutPrefix(objectPath, "v0/b/")
		if !isAPI {
			return "", false, false
		}
		if b, o, found := strings.Cut(rest, gcsJSONObjectsPart); found {
			bucket, object = b, o
		} else {
			bucket, _, _ = strings.Cut(rest, "/")
		}
	default:
		for _, suffix := range gcsBucketHostSuffixes {
			if strings.HasSuffix(host, suffix) {
				bucket, object = strings.TrimSuffix(host, suffix), objectPath
			}
		}
		if bucket == "" {
			return "", false, strings.HasSuffix(host, "."+googleAPIsDomain) && strings.Contains(host, "storage")
		}
	}
	if bucket == "" || strings.Contains(bucket, "/") {
		return "", false, false
	}
	return "gs://" + bucket + "/" + object, true, false
}

// s3ObjectURL resolves the host and decoded path of an https URL on an S3 host to the object's
// s3:// URI. The S3 label of the host ("s3", "s3-<region>", "s3-accelerate", "s3-fips") is the
// last one of that shape: what follows it is a region or an endpoint kind, what precedes it is
// the bucket, which may itself hold dots, and with nothing before it the bucket leads the path.
// A host whose S3 label is an access point's, a multi-region access point's ("s3-global"),
// Object Lambda's, Outposts' or the control plane's names no bucket and is unverifiable.
func s3ObjectURL(host, urlPath string) (id string, ok, unverifiable bool) {
	var name string
	switch {
	case strings.HasSuffix(host, "."+awsDomain):
		name = strings.TrimSuffix(host, "."+awsDomain)
	case strings.HasSuffix(host, "."+awsChinaDomain):
		name = strings.TrimSuffix(host, "."+awsChinaDomain)
	default:
		return "", false, false
	}
	labels := strings.Split(name, ".")
	service := -1
	for i, label := range labels {
		if label == "s3" || strings.HasPrefix(label, "s3-") {
			service = i
		}
	}
	if service < 0 {
		return "", false, false
	}
	// Only the service label says what kind of endpoint this is. The labels before it are the
	// bucket's own name, and a bucket may be called "accesspoint" or "s3-control".
	switch labels[service] {
	case "s3-accesspoint", "s3-accesspoint-fips", "s3-object-lambda", "s3-object-lambda-fips", "s3-outposts", "s3-outposts-fips", "s3-control", "s3-control-fips", "s3-global":
		return "", false, true
	}
	bucket, key := strings.Join(labels[:service], "."), strings.TrimPrefix(urlPath, "/")
	if bucket == "" {
		bucket, key, _ = strings.Cut(key, "/")
	}
	if bucket == "" {
		return "", false, false
	}
	return "s3://" + bucket + "/" + key, true, false
}

// s3ARNObject resolves "arn:<partition>:s3:::<bucket>[/<key>]" to the object's s3:// URI. Any
// other ARN of an S3 service (an access point "arn:aws:s3:<region>:<account>:accesspoint/<name>",
// a multi-region access point, s3-object-lambda, s3-outposts) is unverifiable.
func s3ARNObject(arn string) (id string, ok, unverifiable bool) {
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) != 6 {
		return "", false, false
	}
	service := strings.ToLower(parts[2])
	if service != "s3" && !strings.HasPrefix(service, "s3-") {
		return "", false, false
	}
	if service != "s3" || parts[3] != "" || parts[4] != "" {
		return "", false, true
	}
	if parts[5] == "" || parts[5][0] == '/' {
		return "", false, false
	}
	if !strings.Contains(parts[5], "/") {
		return "s3://" + parts[5] + "/", true, false
	}
	return "s3://" + parts[5], true, false
}

// namesStorageHost reports whether s mentions a host an upload can be addressed under: any
// googleapis.com host (the Gemini file host, Cloud Storage), storage.cloud.google.com, or an
// amazonaws.com host. One pass, no allocation.
func namesStorageHost(s string) bool {
	// The second letter is compared before the whole name: most letters g, s and a in a URL
	// start none of the hosts.
	for i := 0; i+1 < len(s); i++ {
		switch next := s[i+1] | 0x20; s[i] | 0x20 {
		case 'g':
			if next == 'o' && hasPrefixFold(s[i:], googleAPIsDomain) {
				return true
			}
		case 's':
			if next == 't' && hasPrefixFold(s[i:], gcsBrowserHost) {
				return true
			}
		case 'a':
			if next == 'm' && hasPrefixFold(s[i:], awsDomain) {
				return true
			}
		}
	}
	return false
}

// hasPrefixFold reports whether s starts with prefix, ignoring ASCII case.
func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

// storageGlobCharacters are the characters a storage wildcard is written with ("gs://b/in/*",
// which a Vertex batch reads every match of). A pattern names every object it matches, and
// those are not looked up one by one, so a URI holding one cannot be verified.
const storageGlobCharacters = "*?["

// unresolvableStorageURI reports whether id, a storage URI as recordedFileID returns it, names
// an object under a name that cannot be compared with what is recorded, so the request that
// carries it is refused rather than let through as naming nothing recorded:
//
//   - its bucket is no bucket's own name (see unresolvableBucket);
//   - it holds a wildcard (storageGlobCharacters);
//   - its path holds a "." or ".." segment. Object storage takes such a segment literally, but
//     a client that fetches the object over https may resolve it first, so which object is
//     read depends on who reads it.
//
// It is false for an id that is no storage URI.
func unresolvableStorageURI(id string) bool {
	var scheme string
	for _, known := range storageURISchemes {
		if strings.HasPrefix(id, known) {
			scheme = known
		}
	}
	if scheme == "" {
		return false
	}
	bucket, objectPath, _ := strings.Cut(id[len(scheme):], "/")
	if bucket == "" {
		return false
	}
	if unresolvableBucket(scheme, bucket) || strings.ContainsAny(id[len(scheme):], storageGlobCharacters) {
		return true
	}
	for rest := objectPath; rest != ""; {
		var segment string
		segment, rest, _ = strings.Cut(rest, "/")
		if segment == "." || segment == ".." {
			return true
		}
	}
	return false
}

// unresolvableBucket reports whether the bucket part of a storage URI is not a bucket's own
// name, so the object it leads to may be recorded under another name: "." or ".." (an https
// client resolves it against the path), and on S3 an access point's alias ("<name>-<hash>-s3alias",
// Object Lambda's "--ol-s3", Outposts' "--op-s3", a multi-region access point's "<alias>.mrap")
// or an ARN written where the bucket goes. An alias reaches the same objects as the bucket it
// fronts, and which bucket that is cannot be told without asking AWS.
func unresolvableBucket(scheme, bucket string) bool {
	if bucket == "." || bucket == ".." {
		return true
	}
	if scheme != "s3://" {
		return false
	}
	bucket = strings.ToLower(bucket)
	for _, suffix := range [...]string{"-s3alias", "--ol-s3", "--op-s3", ".mrap"} {
		if strings.HasSuffix(bucket, suffix) {
			return true
		}
	}
	return strings.HasPrefix(bucket, s3ARNPrefix)
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

// rawBodyFieldNames are the typed byte fields that hold a request body forwarded to the
// provider as it was received: the raw body a native route keeps beside the fields it parsed
// (RawRequestBody), and a passthrough request's Body. What such a body names reaches the
// provider whether or not a typed field repeats it (a native Vertex batch keeps only the first
// input URI in its typed field and sends the body with all of them), so it is read like any
// other untyped data. Every other byte field is content: an uploaded file, audio, an image.
var rawBodyFieldNames = map[string]struct{}{"RawRequestBody": {}, "Body": {}}

// isRawBodyField reports whether a struct field is a forwarded request body.
func isRawBodyField(field reflect.StructField) bool {
	_, named := rawBodyFieldNames[field.Name]
	return named && field.Type.Kind() == reflect.Slice && field.Type.Elem().Kind() == reflect.Uint8
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
		// A forwarded raw body is untyped data too. A passthrough request's body is read by
		// passthroughObjectRefs, with the path it belongs to.
		if isRawBodyField(field) && rt.Name() != "BifrostPassthroughRequest" {
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
// nested past openValueDepth, raw JSON that does not parse, more than maxFileReferences
// distinct files, or a name recordedFileID cannot follow to an object. What was not read, or not kept, may name another key's file.
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

// addReference adds what a reference field of the given kind names: an id as written, a URL
// or an output location under the id recordedFileID resolves it to.
func (c *fileIDCollector) addReference(kind fileRefKind, value string) {
	if kind == fileRefID {
		c.add(value)
		return
	}
	id, ok, unverifiable := recordedFileID(value)
	if unverifiable {
		c.unreadable = true
	}
	if ok {
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
	if r := req.FileUploadRequest; r != nil {
		for _, location := range uploadLocations(r) {
			collector.add(location)
		}
	}
	return collector.ids, collector.unreadable
}

// uploadLocations returns the storage objects a file upload writes, which it names in pieces:
// a bucket and folder (storage_config.s3, or Bedrock's s3_bucket and s3_prefix extra params) and
// a filename. Bedrock stores the file under exactly that name, "s3://<bucket>/<prefix>/<filename>",
// where the bucket may itself be written "s3://<bucket>/<prefix>/", and its PUT replaces what is
// there. So the object is checked like any other the request names: an upload over a file
// recorded for another key is refused.
//
// A Vertex upload is not read here: it stores every file under a fresh
// "<prefix>/vertex-files/<uuid>/" (core/providers/vertex, gcsObjectKey), so it never writes an
// object that exists.
//
// Bedrock takes the extra params only when the request has no S3 config; both are returned,
// since which one a provider reads is the provider's choice: an object the provider does not
// write costs a lookup, one this check ignored would be written unchecked.
func uploadLocations(r *schemas.BifrostFileUploadRequest) []string {
	var locations []string
	add := func(bucket, prefix string) {
		const scheme = "s3://"
		bucket = strings.TrimSpace(bucket)
		if hasPrefixFold(bucket, scheme) {
			bucket = bucket[len(scheme):]
		}
		name, inBucket, _ := strings.Cut(bucket, "/")
		if name == "" || r.Filename == "" {
			return
		}
		location := scheme + name + "/" + strings.Trim(inBucket+prefix, "/")
		locations = append(locations, strings.TrimRight(location, "/")+"/"+r.Filename)
	}
	if config := r.StorageConfig; config != nil && config.S3 != nil {
		add(config.S3.Bucket, config.S3.Prefix)
	}
	bucket, _ := r.ExtraParams["s3_bucket"].(string)
	prefix, _ := r.ExtraParams["s3_prefix"].(string)
	add(bucket, prefix)
	return locations
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
