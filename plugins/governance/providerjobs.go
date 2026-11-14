package governance

import (
	"bytes"
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"

	bifrost "github.com/capsohq/bifrost/core"
	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/capsohq/bifrost/framework/configstore"
	configstoreTables "github.com/capsohq/bifrost/framework/configstore/tables"
)

// Provider objects (batches, videos, files, containers and the files inside them, cached contents) are created through the
// operator's provider key, which every virtual key allowed on the provider shares, and are then
// addressed by the provider-side id alone. This file makes them belong to the virtual key that
// created them: the create response records the key, the read, download, delete, remix and
// container-file paths honour it, and list pages are narrowed to what the key may see.
//
// Who created an object is read from the ownership row written here, and, for batches and
// videos, from the accounting job row the logging plugin writes, which predates ownership rows.
// A batch row also names the batch's input, output and error files, so those files are bound to
// the batch's key even when they were never recorded on their own.
//
// A request that presented no virtual key is unrestricted, as it is everywhere else in
// governance. A row that names a different virtual key is refused, and it is refused as not
// found, so the other tenant's object is not confirmed to exist.
//
// An object with no row (created outside the gateway, before its kind was recorded, or whose row
// was lost) depends on its kind. A task (a batch, a video, an image task; see
// ownedOnlyByRecord) fails closed: a virtual key reaches a task only under an id recorded for
// it, so an unrecorded task is not found on retrieve, results, download, cancel, delete, edit and
// remix, and is left out of list pages (TXE-7119). Files, containers and cached contents with no
// row cannot be bound to anyone and stay reachable.
//
// A file in object storage is recorded under its gs:// or s3:// URI and may be named by any https
// or ARN form of it; every form is resolved to that URI before the lookup (TXE-7105; see
// recordedFileID).

// providerObjectRef names the provider object a request addresses, or the kind a list request
// pages through.
type providerObjectRef struct {
	kind      string
	provider  string
	requestID string   // the id as the caller wrote it, for messages
	ids       []string // the forms the id may be recorded under
	isList    bool
	indirect  bool // named inside a request that creates or runs something else, not addressed by id
	// passthroughList is a list through a passthrough route: its raw page cannot be narrowed.
	passthroughList bool
	// destination marks a storage location the request writes its output beneath, which is
	// checked and claimed as a folder (see storagefolders.go).
	destination bool
	// open marks a stored object named in untyped data (an extra param, a passthrough body,
	// an inline batch item), where an input and an output location look alike: it is checked
	// as a folder too, and never claimed.
	open       bool
	unreadable bool // the request's files could not all be read (requestFileIDs), or this id cannot be resolved to one recorded name, so it is refused
}

// governanceAutoContainerContextKey marks a request whose code_interpreter tool asked the provider
// for an automatic container, so the container named in its response is recorded for the key. Its
// value is the ids of the containers the same request named outright, which are never claimed.
const governanceAutoContainerContextKey schemas.BifrostContextKey = "bf-governance-auto-container"

// governanceVideoByReferenceContextKey marks a video retrieve that names the task by the
// client_reference_id it was created with (schemas.VideoIDTypeClientReference). The id in such a
// request is the caller's own, not an object id, so no creator is recorded under it and the
// pre-hook cannot check it; the post-hook checks the task the vendor returned instead.
const governanceVideoByReferenceContextKey schemas.BifrostContextKey = "bf-governance-video-by-client-reference"

// providerObjectLabel is the noun a kind goes by in client-facing messages.
func providerObjectLabel(kind string) string {
	switch kind {
	case configstoreTables.ProviderObjectKindBatch:
		return "batch"
	case configstoreTables.ProviderObjectKindVideo:
		return "video"
	case configstoreTables.ProviderObjectKindFile:
		return "file"
	case configstoreTables.ProviderObjectKindContainer:
		return "container"
	case configstoreTables.ProviderObjectKindCachedContent:
		return "cached content"
	case configstoreTables.ProviderObjectKindContainerFile:
		return "container file"
	case configstoreTables.ProviderObjectKindImageTask:
		return "image task"
	}
	return "object"
}

// requestVirtualKeyID is the virtual key a request is attributed to, read from its Grant, or
// empty when the request presented no key or nothing has resolved one yet.
func requestVirtualKeyID(ctx *schemas.BifrostContext) string {
	if ctx == nil {
		return ""
	}
	grant := ctx.Grant()
	if grant == nil {
		return ""
	}
	identity := grant.Identity()
	if identity == nil {
		return ""
	}
	virtualKey := identity.VirtualKey()
	if virtualKey == nil {
		return ""
	}
	return virtualKey.ID
}

// ownedOnlyByRecord reports whether a kind fails closed for virtual keys: an object of that kind
// with no creator on record is not found, instead of staying reachable. It is the one list of
// those kinds, read by the refusal of a single object and by the narrowing of a list page alike,
// so a kind added here is closed on every route that addresses it. These are the tasks a
// provider runs for whoever holds its id: a batch, a video, an image task.
func ownedOnlyByRecord(kind string) bool {
	switch kind {
	case configstoreTables.ProviderObjectKindBatch, configstoreTables.ProviderObjectKindVideo, configstoreTables.ProviderObjectKindImageTask:
		return true
	}
	return false
}

// videoIDForms returns the forms a video id may be recorded under: as written, bare, and scoped
// with its ":<provider>" suffix the way providers hand it back.
func videoIDForms(provider schemas.ModelProvider, id string) []string {
	if id == "" {
		return nil
	}
	bare := providerUtils.StripVideoIDProviderSuffix(id, provider)
	forms := []string{id}
	for _, form := range []string{bare, providerUtils.AddVideoIDProviderSuffix(bare, provider)} {
		if form != "" && form != id && (len(forms) < 2 || form != forms[1]) {
			forms = append(forms, form)
		}
	}
	return forms
}

// resourceCollections is, per kind, the collection a Google resource name puts in front of the
// id: a Gemini batch is "batches/<id>", a file "files/<id>", a cached content "cachedContents/<id>".
var resourceCollections = map[string]string{
	configstoreTables.ProviderObjectKindBatch:         "batches",
	configstoreTables.ProviderObjectKindFile:          "files",
	configstoreTables.ProviderObjectKindCachedContent: "cachedContents",
}

// providerObjectIDForms returns the forms an id may be recorded under, as written first. A video
// id is taken with and without its ":<provider>" suffix. Gemini addresses an object by a resource
// name and accepts its short form too: a batch as "batches/<id>" or "<id>", a cached content as
// "cachedContents/<n>" or "<n>", so a short id is also taken bare and under its collection. The
// provider is not consulted for that: a custom provider built on Gemini goes by its own name, and
// an id of another shape gains forms that nothing is recorded under. A full resource name
// ("projects/<p>/locations/<l>/batchPredictionJobs/<n>") is exact and is the only form it is
// recorded under; its short forms are resolved against full names by suffix instead (see
// resolveShortResourceNames), since a job number is one project's and cannot be a row of its own.
func providerObjectIDForms(kind string, provider schemas.ModelProvider, id string) []string {
	if id == "" {
		return nil
	}
	// An image task id has the video id's shape, "{URL-encoded vendor id}:{provider}".
	if kind == configstoreTables.ProviderObjectKindVideo || kind == configstoreTables.ProviderObjectKindImageTask {
		return videoIDForms(provider, id)
	}
	forms := []string{id}
	collection, ok := resourceCollections[kind]
	if !ok || strings.HasPrefix(id, "projects/") {
		return forms
	}
	bare := strings.TrimPrefix(id, collection+"/")
	prefixed := collection + "/" + bare
	for _, form := range []string{bare, prefixed} {
		if form != "" && !slices.Contains(forms, form) {
			forms = append(forms, form)
		}
	}
	return forms
}

// providerObjectLookupForms returns the ids an object named by id is looked up under. For
// every kind but a file these are providerObjectIDForms. A file can be named by any URL form of
// the id it is recorded under, so the id is resolved first (recordedFileID) and looked up under
// the recorded id as well as the id as written. unverifiable is true when the id names storage
// in a way that cannot be resolved to one recorded name (an S3 access point, a wildcard, a "."
// or ".." in the path; see unresolvableStorageURI), so the forms are not the whole answer.
func providerObjectLookupForms(kind string, provider schemas.ModelProvider, id string) (forms []string, unverifiable bool) {
	forms = providerObjectIDForms(kind, provider, id)
	if kind != configstoreTables.ProviderObjectKindFile {
		return forms, false
	}
	recorded, ok, unverifiable := recordedFileID(id)
	if !ok {
		return forms, unverifiable
	}
	for _, form := range providerObjectIDForms(kind, provider, recorded) {
		if !slices.Contains(forms, form) {
			forms = append(forms, form)
		}
	}
	return forms, unresolvableStorageURI(recorded)
}

func objectRef(kind string, provider schemas.ModelProvider, id string) (providerObjectRef, bool) {
	if id == "" {
		return providerObjectRef{}, false
	}
	forms, unverifiable := providerObjectLookupForms(kind, provider, id)
	return providerObjectRef{kind: kind, provider: string(provider), requestID: id, ids: forms, unreadable: unverifiable}, true
}

func listRef(kind string, provider schemas.ModelProvider) (providerObjectRef, bool) {
	return providerObjectRef{kind: kind, provider: string(provider), isList: true}, true
}

// providerObjectReference returns the provider object a request addresses, or false when the
// request addresses none.
func providerObjectReference(req *schemas.BifrostRequest) (providerObjectRef, bool) {
	if req == nil {
		return providerObjectRef{}, false
	}
	batch := configstoreTables.ProviderObjectKindBatch
	video := configstoreTables.ProviderObjectKindVideo
	file := configstoreTables.ProviderObjectKindFile
	container := configstoreTables.ProviderObjectKindContainer
	cached := configstoreTables.ProviderObjectKindCachedContent
	switch {
	case req.BatchRetrieveRequest != nil:
		return objectRef(batch, req.BatchRetrieveRequest.Provider, req.BatchRetrieveRequest.BatchID)
	case req.BatchCancelRequest != nil:
		return objectRef(batch, req.BatchCancelRequest.Provider, req.BatchCancelRequest.BatchID)
	case req.BatchResultsRequest != nil:
		return objectRef(batch, req.BatchResultsRequest.Provider, req.BatchResultsRequest.BatchID)
	case req.BatchDeleteRequest != nil:
		return objectRef(batch, req.BatchDeleteRequest.Provider, req.BatchDeleteRequest.BatchID)
	case req.BatchListRequest != nil:
		return listRef(batch, req.BatchListRequest.Provider)
	case req.BatchCreateRequest != nil:
		// A batch runs on its input file; another key's file is as closed to it as the file itself.
		return objectRef(file, req.BatchCreateRequest.Provider, req.BatchCreateRequest.InputFileID)
	case req.FileRetrieveRequest != nil:
		return objectRef(file, req.FileRetrieveRequest.Provider, req.FileRetrieveRequest.FileID)
	case req.FileContentRequest != nil:
		return objectRef(file, req.FileContentRequest.Provider, req.FileContentRequest.FileID)
	case req.FileDeleteRequest != nil:
		return objectRef(file, req.FileDeleteRequest.Provider, req.FileDeleteRequest.FileID)
	case req.FileListRequest != nil:
		return listRef(file, req.FileListRequest.Provider)
	case req.VideoRetrieveRequest != nil:
		return objectRef(video, req.VideoRetrieveRequest.Provider, req.VideoRetrieveRequest.ID)
	case req.VideoDownloadRequest != nil:
		return objectRef(video, req.VideoDownloadRequest.Provider, req.VideoDownloadRequest.ID)
	case req.VideoDeleteRequest != nil:
		return objectRef(video, req.VideoDeleteRequest.Provider, req.VideoDeleteRequest.ID)
	case req.VideoCancelRequest != nil:
		return objectRef(video, req.VideoCancelRequest.Provider, req.VideoCancelRequest.ID)
	case req.ImageTaskRetrieveRequest != nil:
		return objectRef(configstoreTables.ProviderObjectKindImageTask, req.ImageTaskRetrieveRequest.Provider, req.ImageTaskRetrieveRequest.ID)
	case req.ImageTaskCancelRequest != nil:
		return objectRef(configstoreTables.ProviderObjectKindImageTask, req.ImageTaskCancelRequest.Provider, req.ImageTaskCancelRequest.ID)
	case req.VideoRemixRequest != nil:
		return objectRef(video, req.VideoRemixRequest.Provider, req.VideoRemixRequest.ID)
	case req.VideoListRequest != nil:
		return listRef(video, req.VideoListRequest.Provider)
	case req.ContainerRetrieveRequest != nil:
		return objectRef(container, req.ContainerRetrieveRequest.Provider, req.ContainerRetrieveRequest.ContainerID)
	case req.ContainerDeleteRequest != nil:
		return objectRef(container, req.ContainerDeleteRequest.Provider, req.ContainerDeleteRequest.ContainerID)
	case req.ContainerFileCreateRequest != nil:
		return objectRef(container, req.ContainerFileCreateRequest.Provider, req.ContainerFileCreateRequest.ContainerID)
	case req.VideoEditRequest != nil:
		// An edit runs on its source video; another key's video is as closed to it as the video itself.
		if req.VideoEditRequest.Input == nil {
			return providerObjectRef{}, false
		}
		ref, ok := objectRef(video, req.VideoEditRequest.Provider, req.VideoEditRequest.Input.Video.ID)
		ref.indirect = true
		return ref, ok
	case req.ContainerFileListRequest != nil:
		return objectRef(container, req.ContainerFileListRequest.Provider, req.ContainerFileListRequest.ContainerID)
	case req.ContainerFileRetrieveRequest != nil:
		return objectRef(container, req.ContainerFileRetrieveRequest.Provider, req.ContainerFileRetrieveRequest.ContainerID)
	case req.ContainerFileContentRequest != nil:
		return objectRef(container, req.ContainerFileContentRequest.Provider, req.ContainerFileContentRequest.ContainerID)
	case req.ContainerFileDeleteRequest != nil:
		return objectRef(container, req.ContainerFileDeleteRequest.Provider, req.ContainerFileDeleteRequest.ContainerID)
	case req.ContainerListRequest != nil:
		return listRef(container, req.ContainerListRequest.Provider)
	case req.CachedContentRetrieveRequest != nil:
		return objectRef(cached, req.CachedContentRetrieveRequest.Provider, req.CachedContentRetrieveRequest.Name)
	case req.CachedContentUpdateRequest != nil:
		return objectRef(cached, req.CachedContentUpdateRequest.Provider, req.CachedContentUpdateRequest.Name)
	case req.CachedContentDeleteRequest != nil:
		return objectRef(cached, req.CachedContentDeleteRequest.Provider, req.CachedContentDeleteRequest.Name)
	case req.CachedContentListRequest != nil:
		return listRef(cached, req.CachedContentListRequest.Provider)
	}
	return providerObjectRef{}, false
}

// providerObjectReferences returns every provider object a request names. Most requests address
// one object by id (providerObjectReference). Several shapes name objects indirectly: a container
// create copies the files in file_ids into the new container, a container-file create can copy an
// existing file in by file_id, a video edit runs on its source video, a chat or responses request
// can carry uploaded files by file_id in its messages, and a chat, responses, image or
// transcription request can run on a cached content (Gemini's cached_content, in either key
// spelling). Those are checked like a direct reference, since the provider would otherwise hand
// another key's file, video or cache to the caller through the object it is creating or the
// answer. A file inside a container is an object of its own: a request that addresses one names
// both the container and the file.
//
// The files a request names as inputs are read by one extraction for every request type
// (requestFileRefs, see filerefs.go) and added to whatever the request's own shape names.
func providerObjectReferences(req *schemas.BifrostRequest) []providerObjectRef {
	if req == nil {
		return nil
	}
	refs := namedProviderObjects(req)
	fileRefs := requestFileRefs(req)
	if len(fileRefs) == 0 {
		return refs
	}
	if len(refs) == 0 {
		return fileRefs
	}
	for _, fileRef := range fileRefs {
		known := false
		for i := range refs {
			if refs[i].kind == fileRef.kind && refs[i].requestID == fileRef.requestID && !refs[i].isList {
				known = true
				refs[i].destination = refs[i].destination || fileRef.destination
				refs[i].open = refs[i].open || fileRef.open
				break
			}
		}
		if !known {
			refs = append(refs, fileRef)
		}
	}
	return refs
}

// namedProviderObjects returns the provider objects a request names through its own shape: the
// object it addresses, and the objects it copies from, edits or runs on. Files named as
// inference inputs are not read here (see providerObjectReferences).
func namedProviderObjects(req *schemas.BifrostRequest) []providerObjectRef {
	file := configstoreTables.ProviderObjectKindFile
	containerFile := configstoreTables.ProviderObjectKindContainerFile
	if req.ContainerCreateRequest != nil {
		var refs []providerObjectRef
		for _, fileID := range req.ContainerCreateRequest.FileIDs {
			if ref, ok := objectRef(file, req.ContainerCreateRequest.Provider, fileID); ok {
				ref.indirect = true
				refs = append(refs, ref)
			}
		}
		return refs
	}
	if r := req.ContainerFileCreateRequest; r != nil {
		var refs []providerObjectRef
		if ref, ok := objectRef(configstoreTables.ProviderObjectKindContainer, r.Provider, r.ContainerID); ok {
			refs = append(refs, ref)
		}
		if r.FileID != nil {
			if ref, ok := objectRef(file, r.Provider, *r.FileID); ok {
				ref.indirect = true
				refs = append(refs, ref)
			}
		}
		return refs
	}
	if r := req.PassthroughRequest; r != nil {
		return passthroughObjectRefs(r)
	}
	if r := req.BatchCreateRequest; r != nil {
		// A batch runs on its input file, or on inline items whose bodies the provider reads with
		// the files and caches named there.
		var refs []providerObjectRef
		if ref, ok := objectRef(file, r.Provider, r.InputFileID); ok {
			refs = append(refs, ref)
		}
		for _, item := range r.Requests {
			refs = append(refs, jsonObjectRefs(r.Provider, item.Body)...)
			refs = append(refs, jsonObjectRefs(r.Provider, item.Params)...)
		}
		return refs
	}
	if provider, containerID, fileID, ok := containerFileAddress(req); ok {
		var refs []providerObjectRef
		if ref, ok := objectRef(configstoreTables.ProviderObjectKindContainer, provider, containerID); ok {
			refs = append(refs, ref)
		}
		if ref, ok := objectRef(containerFile, provider, fileID); ok {
			refs = append(refs, ref)
		}
		return refs
	}
	if provider, extra, ok := cachedContentCarrier(req); ok {
		var refs []providerObjectRef
		// Both spellings are checked when both are present: providers disagree on which one wins
		// (Gemini's image converters read cachedContent first, its chat converter cached_content),
		// so checking only one would let the other carry a different cache through.
		var seenCache string
		for _, key := range []string{"cached_content", "cachedContent"} {
			if name, ok := schemas.SafeExtractString(extra[key]); ok && name != "" && name != seenCache {
				ref, _ := objectRef(configstoreTables.ProviderObjectKindCachedContent, provider, name)
				ref.indirect = true
				refs = append(refs, ref)
				seenCache = name
			}
		}
		refs = append(refs, toolObjectRefs(req, provider)...)
		return refs
	}
	if r := req.VideoGenerationRequest; r != nil && r.Params != nil {
		// A video can be generated from a draft task named in the request's extra params (BytePlus
		// draft_task_id, in bare or provider-suffixed form). The draft is a video another key may
		// have created, so it is checked like a video edit's source.
		if id, ok := schemas.SafeExtractString(r.Params.ExtraParams["draft_task_id"]); ok && id != "" {
			ref, _ := objectRef(configstoreTables.ProviderObjectKindVideo, r.Provider, id)
			ref.indirect = true
			return []providerObjectRef{ref}
		}
	}
	if ref, ok := providerObjectReference(req); ok {
		return []providerObjectRef{ref}
	}
	return nil
}

// containerFileAddress returns the container and file a container-file retrieve, content or
// delete request addresses, or false for any other request.
func containerFileAddress(req *schemas.BifrostRequest) (schemas.ModelProvider, string, string, bool) {
	switch {
	case req.ContainerFileRetrieveRequest != nil:
		r := req.ContainerFileRetrieveRequest
		return r.Provider, r.ContainerID, r.FileID, true
	case req.ContainerFileContentRequest != nil:
		r := req.ContainerFileContentRequest
		return r.Provider, r.ContainerID, r.FileID, true
	case req.ContainerFileDeleteRequest != nil:
		r := req.ContainerFileDeleteRequest
		return r.Provider, r.ContainerID, r.FileID, true
	}
	return "", "", "", false
}

// toolObjectRefs returns the containers and files a Responses request's code_interpreter tools
// name: the container they run in when it is an existing one named by id, or the file_ids an
// automatic container is seeded with. The provider runs the tool against them under the shared
// key, so each is checked like any other object the request names.
func toolObjectRefs(req *schemas.BifrostRequest, provider schemas.ModelProvider) []providerObjectRef {
	if req.ResponsesRequest == nil || req.ResponsesRequest.Params == nil {
		return nil
	}
	var refs []providerObjectRef
	add := func(kind, id string) {
		if ref, ok := objectRef(kind, provider, strings.TrimSpace(id)); ok {
			ref.indirect = true
			refs = append(refs, ref)
		}
	}
	for _, tool := range req.ResponsesRequest.Params.Tools {
		if tool.ResponsesToolCodeInterpreter == nil {
			continue
		}
		switch container := tool.ResponsesToolCodeInterpreter.Container.(type) {
		case string:
			add(configstoreTables.ProviderObjectKindContainer, container)
		case *string:
			if container != nil {
				add(configstoreTables.ProviderObjectKindContainer, *container)
			}
		case map[string]interface{}:
			if fileIDs, ok := container["file_ids"].([]interface{}); ok {
				for _, fileID := range fileIDs {
					if id, ok := fileID.(string); ok {
						add(configstoreTables.ProviderObjectKindFile, id)
					}
				}
			} else if fileIDs, ok := container["file_ids"].([]string); ok {
				for _, id := range fileIDs {
					add(configstoreTables.ProviderObjectKindFile, id)
				}
			}
		}
	}
	return refs
}

// passthroughCollections maps a passthrough path segment that names a collection to its object kind.
var passthroughCollections = map[string]string{
	"files":          configstoreTables.ProviderObjectKindFile,
	"batches":        configstoreTables.ProviderObjectKindBatch,
	"containers":     configstoreTables.ProviderObjectKindContainer,
	"videos":         configstoreTables.ProviderObjectKindVideo,
	"cachedContents": configstoreTables.ProviderObjectKindCachedContent,
}

// passthroughTaskCollections are the collections a POST creates a task in (Vertex's batch
// create posts to "batchPredictionJobs"), passthroughTaskOperations the model operations that
// create one (Gemini's batch create is "models/<m>:batchGenerateContent", its and Vertex's
// video create "models/<m>:predictLongRunning"), and passthroughVideoDerivations the routes under videos
// that create a new video from an existing one or from an upload ("videos/<id>/remix",
// "videos/edits", "videos/extensions"). The task such a request creates is addressed afterwards
// through the gateway's batch or video routes, which fail closed (ownedOnlyByRecord), and a passthrough
// answer is the provider's raw body, from which no creator is recorded. So a virtual key that
// created a task this way could never reach it again; the create is refused and the key is sent
// to the gateway's own route, which records it.
var passthroughTaskCollections = [...]string{"batches", "videos", "batchPredictionJobs"}

var passthroughTaskOperations = [...]string{"batchGenerateContent", "asyncBatchEmbedContent", "predictLongRunning"}

var passthroughVideoDerivations = [...]string{"remix", "edits", "extensions"}

// passthroughCreatesTask reports whether a passthrough request creates a batch or a video: a
// POST whose path, as it will be sent, ends on a task collection, on a task-creating model
// operation, or on a route under videos that derives a new video. Runware, which names the
// operation in the body, is read by runwareBodyCreatesTask.
func passthroughCreatesTask(r *schemas.BifrostPassthroughRequest) bool {
	if !strings.EqualFold(r.Method, "POST") {
		return false
	}
	segments := strings.Split(strings.Trim(path.Clean("/"+r.Path), "/"), "/")
	last := segments[len(segments)-1]
	name, operation, _ := strings.Cut(last, ":")
	if operation == "" && slices.Contains(passthroughTaskCollections[:], name) {
		return true
	}
	if slices.Contains(passthroughTaskOperations[:], operation) {
		return true
	}
	return slices.Contains(passthroughVideoDerivations[:], last) && slices.Contains(segments[:len(segments)-1], "videos")
}

// runwareTaskTypes are the Runware task types that always run as a task polled afterwards, and
// runwareAsyncDelivery the delivery method that makes any other type one (the typed video
// routes send upscale and removeBackground that way). Runware has one endpoint and names the
// operation in the body: a POST of [{"taskType": "videoInference", ...}] creates a task
// whatever the path, and the typed video routes then retrieve it by its task UUID.
var runwareTaskTypes = [...]string{"videoInference", "3dInference"}

const runwareAsyncDelivery = "async"

// runwareBodyCreatesTask reports whether a POST body sent to Runware holds an entry that
// creates a polled task: one of runwareTaskTypes, or any entry delivered asynchronously. It is
// asked only for providers built on Runware, so no other provider's passthrough pays for the
// parse. A body that does not parse is treated as creating one, since the provider may read
// what this decoder cannot.
func runwareBodyCreatesTask(method string, body []byte) bool {
	if !strings.EqualFold(method, "POST") || len(body) == 0 {
		return false
	}
	var decoded any
	if err := schemas.Unmarshal(body, &decoded); err != nil {
		return true
	}
	var creates func(value any, depth int) bool
	creates = func(value any, depth int) bool {
		if depth > openValueDepth {
			return true
		}
		switch v := value.(type) {
		case map[string]any:
			if taskType, _ := v["taskType"].(string); slices.Contains(runwareTaskTypes[:], taskType) {
				return true
			}
			if delivery, _ := v["deliveryMethod"].(string); delivery == runwareAsyncDelivery {
				return true
			}
			for _, item := range v {
				if creates(item, depth+1) {
					return true
				}
			}
		case []any:
			for _, item := range v {
				if creates(item, depth+1) {
					return true
				}
			}
		}
		return false
	}
	return creates(decoded, 0)
}

// passthroughTuningCollections are the path segments, folded, that name a tuning or
// customization collection: OpenAI fine_tuning (and Azure's legacy fine-tunes), Gemini
// tunedModels, Vertex tuningJobs, and Bedrock's customization collections (jobs, custom and
// imported models, their deployments, imports, copies and provisioned throughput).
var passthroughTuningCollections = [...]string{
	"fine_tuning", "fine-tunes", "tunedModels", "tuningJobs",
	"model-customization-jobs", "custom-models", "custom-model-deployments", "imported-models",
	"model-import-jobs", "model-copy-jobs", "provisioned-model-throughput", "provisioned-model-throughputs",
}

// passthroughReachesTuning reports whether a passthrough request reaches a tuning collection
// or tuned model on any provider (see passthroughTuningCollections, plus OpenAI's fine-tuned
// models/ft:...). None records which key created a job or tuned model, so a virtual key can
// neither manage its own nor be kept from another's: a list, read or cancel would reach other
// keys' jobs, and a create may name another key's tuned model as its base. Inference on one Gemini
// tuned model stays open (see tunedModelInference). Every method is
// matched, so neither the method nor a method-override header changes the answer. The path
// is matched as it will be sent, as in passthroughObjectRefs: BuildPassthroughURL re-encodes
// every literal %, so the provider reads exactly these segments. Segments are folded and cut
// at a Google custom-method ':' (tunedModels:generateText) or a ';' parameter, and a '\' is
// read as a separator; each can only refuse more. The provider is not consulted, so a custom
// provider built on any base type is covered alike. It allocates only for a path that holds a
// dot segment, a doubled slash or a backslash.
func passthroughReachesTuning(r *schemas.BifrostPassthroughRequest) bool {
	sent := r.Path
	if strings.Contains(sent, "\\") {
		sent = strings.ReplaceAll(sent, "\\", "/")
	}
	if strings.Contains(sent, "/.") || strings.Contains(sent, "//") || strings.HasPrefix(sent, ".") {
		sent = path.Clean("/" + sent)
	}
	afterModels := false
	for rest := sent; rest != ""; {
		var segment string
		segment, rest, _ = strings.Cut(rest, "/")
		// An OpenAI fine-tuned model (models/ft:...) belongs to the key that tuned it.
		if afterModels && len(segment) >= 3 && strings.EqualFold(segment[:3], "ft:") {
			return true
		}
		name := segment
		if i := strings.IndexAny(name, ":;"); i >= 0 {
			name = name[:i]
		}
		afterModels = strings.EqualFold(name, "models")
		// Inference on one Gemini tuned model (tunedModels/<id>:generateContent and the other
		// operations genAIInferencePath forwards) is governed by the key's model allow-list, as on
		// every other route; only that exact shape is exempt, spelled as the router matches it.
		if segment == "tunedModels" && tunedModelInference(rest) {
			return false
		}
		for _, collection := range passthroughTuningCollections {
			if strings.EqualFold(name, collection) {
				return true
			}
		}
	}
	return false
}

// tunedModelInferenceOperations are the GenAI operations that run a model rather than manage it,
// as transports' genAIInferencePath lists them.
var tunedModelInferenceOperations = [...]string{
	"generateContent", "streamGenerateContent", "countTokens", "embedContent", "batchEmbedContents",
	"predict", "rawPredict", "streamRawPredict",
}

// tunedModelInference reports whether the path after a tunedModels segment is exactly one
// "<id>:<operation>" segment naming an inference operation (matched exactly, unlike the
// collection names, so a spelling the exemption does not know stays refused).
func tunedModelInference(rest string) bool {
	if rest == "" || strings.ContainsAny(rest, "/;") {
		return false
	}
	id, operation, ok := strings.Cut(rest, ":")
	if !ok || id == "" {
		return false
	}
	for _, op := range tunedModelInferenceOperations {
		if operation == op {
			return true
		}
	}
	return false
}

// passthroughObjectRefs returns the provider objects a passthrough request reaches. The router
// forwards the provider's own path and body under the shared key, so the object a path addresses
// (files/<id>, batches/<id>, containers/<id> and the files inside it, videos/<id>,
// cachedContents/<id>) is checked directly, a GET of one of those collections is a list, and the
// objects a JSON body names are checked like the same references on the native routes (see
// jsonObjectRefs). A body that is not JSON names nothing that can be checked.
func passthroughObjectRefs(r *schemas.BifrostPassthroughRequest) []providerObjectRef {
	var refs []providerObjectRef
	gemini := r.Provider == schemas.Gemini
	add := func(kind, id string) {
		if ref, ok := objectRef(kind, r.Provider, strings.TrimSpace(id)); ok {
			if kind == configstoreTables.ProviderObjectKindVideo {
				ref.ids = videoIDForms(r.Provider, ref.requestID)
			}
			refs = append(refs, ref)
		}
	}
	// The path is matched as it will be sent: the HTTP client resolves . and .. segments and
	// collapses repeated slashes, so files/x/../file-other reaches file-other, not x. A path with
	// neither is already in that form and is not rebuilt.
	sent := r.Path
	if strings.Contains(sent, "/.") || strings.Contains(sent, "//") || strings.HasPrefix(sent, ".") {
		sent = path.Clean("/" + sent)
	}
	segments := strings.Split(strings.Trim(sent, "/"), "/")
	// A GET that ends on a collection lists it.
	if last := segments[len(segments)-1]; strings.EqualFold(r.Method, "GET") {
		if kind, ok := passthroughCollections[last]; ok {
			if last == "files" && len(segments) >= 3 && segments[len(segments)-3] == "containers" {
				kind = configstoreTables.ProviderObjectKindContainerFile
			}
			refs = append(refs, providerObjectRef{kind: kind, provider: string(r.Provider), isList: true, passthroughList: true})
		}
	}
	// A collection segment followed by an id addresses that object.
	for i := 0; i+1 < len(segments); i++ {
		raw := segments[i+1]
		id, _, _ := strings.Cut(raw, ":")
		if id == "" {
			continue
		}
		switch segments[i] {
		case "files":
			if gemini {
				id = "files/" + id
			}
			if i >= 2 && segments[i-2] == "containers" {
				add(configstoreTables.ProviderObjectKindContainerFile, id)
			} else {
				add(configstoreTables.ProviderObjectKindFile, id)
			}
		case "batches":
			if gemini {
				id = "batches/" + id
			}
			add(configstoreTables.ProviderObjectKindBatch, id)
		case "containers":
			add(configstoreTables.ProviderObjectKindContainer, id)
		case "videos":
			// A video id may carry its ":<provider>" suffix in the path; every form is checked.
			add(configstoreTables.ProviderObjectKindVideo, raw)
		case "cachedContents":
			add(configstoreTables.ProviderObjectKindCachedContent, "cachedContents/"+id)
		}
	}
	if len(r.Body) == 0 || !mayNameProviderObjects(r.Body) {
		return refs
	}
	var body any
	if err := schemas.Unmarshal(r.Body, &body); err != nil {
		return refs
	}
	return append(refs, jsonObjectRefs(r.Provider, body)...)
}

// providerObjectBodyMarkers are the keys a JSON body names a provider object under; a body that
// contains none of them names nothing and is not parsed.
var providerObjectBodyMarkers = [][]byte{
	[]byte(`"file_id`), []byte(`"fileUri"`), []byte(`"file_uri"`),
	[]byte(`"cached_content"`), []byte(`"cachedContent"`), []byte(`code_interpreter`),
	[]byte(`"input_file_id"`), []byte(`"training_file"`), []byte(`"validation_file"`), []byte(`"fileName"`),
}

var unicodeEscape = []byte(`\u`)

// mayNameProviderObjects reports whether a raw body could name a provider object, so passthrough
// bodies that cannot are forwarded without a JSON parse. A \u escape can spell any marker
// character (file\u005fid decodes to file_id), so a body carrying one is always parsed; the other
// escapes (\n, \") cannot produce a marker's letters and leave the shortcut in place.
func mayNameProviderObjects(body []byte) bool {
	if bytes.Contains(body, unicodeEscape) || mayNameRecordedFile(body) {
		return true
	}
	for _, marker := range providerObjectBodyMarkers {
		if bytes.Contains(body, marker) {
			return true
		}
	}
	return false
}

// mayNameRecordedFile reports whether a raw body could hold a URL in a form uploads are recorded
// under (see recordedFileID): "gs:" or "s3:", an S3 ARN, or one of the hosts a stored object is
// addressed under, in any case. The slashes are not required, since JSON may write them escaped
// ("gs:\/\/"). It over-matches (prose that ends a word in "gs:"), which only costs the parse.
func mayNameRecordedFile(body []byte) bool {
	startsWith := func(at int, marker string) bool {
		return len(body)-at >= len(marker) && strings.EqualFold(string(body[at:at+len(marker)]), marker)
	}
	for i := 0; i+2 < len(body); i++ {
		switch body[i] | 0x20 {
		case 'g':
			if body[i+1]|0x20 == 's' && body[i+2] == ':' {
				return true
			}
			if startsWith(i, googleAPIsDomain) {
				return true
			}
		case 's':
			if body[i+1] == '3' && body[i+2] == ':' {
				return true
			}
			if startsWith(i, gcsBrowserHost) {
				return true
			}
		case 'a':
			if startsWith(i, awsDomain) || startsWith(i, s3ARNPrefix+"aws") {
				return true
			}
		}
	}
	return false
}

// jsonObjectRefs returns the provider objects a JSON request body names anywhere in it: file_id
// and file_ids, a code_interpreter tool's container named by id, any string in a form uploads
// are recorded under (a Gemini file URI, a gs:// or s3:// storage URI; see recordedFileID),
// a cached content (cached_content or cachedContent), and the input file a batch or fine-tune
// runs on (input_file_id, training_file, validation_file, and a Gemini batch's fileName). It
// reads passthrough bodies and the bodies of inline batch items, which reach the provider as
// written.
func jsonObjectRefs(provider schemas.ModelProvider, body any) []providerObjectRef {
	var refs []providerObjectRef
	type named struct{ kind, id string }
	var seen map[named]struct{}
	full := false
	add := func(kind, id string) {
		id = strings.TrimSpace(id)
		if full || id == "" {
			return
		}
		// An object named again is the same reference (a file_id that is a storage URI is
		// reached under its key and again as a string).
		key := named{kind, id}
		if _, dup := seen[key]; dup {
			return
		}
		// Each distinct object is an ownership lookup; a body naming more than the bound is
		// refused rather than looked up (see maxFileReferences).
		if len(refs) >= maxFileReferences {
			full = true
			refs = append(refs, providerObjectRef{kind: kind, provider: string(provider), indirect: true, unreadable: true})
			return
		}
		if seen == nil {
			seen = make(map[named]struct{}, 4)
		}
		seen[key] = struct{}{}
		if ref, ok := objectRef(kind, provider, id); ok {
			ref.indirect = true
			ref.open = kind == configstoreTables.ProviderObjectKindFile
			refs = append(refs, ref)
		}
	}
	var walk func(value any)
	walk = func(value any) {
		switch v := value.(type) {
		case map[string]any:
			if t, _ := v["type"].(string); t == "code_interpreter" {
				if container, ok := v["container"].(string); ok {
					add(configstoreTables.ProviderObjectKindContainer, container)
				}
			}
			for key, item := range v {
				switch key {
				case "file_id", "input_file_id", "training_file", "validation_file":
					if id, ok := item.(string); ok {
						add(configstoreTables.ProviderObjectKindFile, id)
					}
				case "fileName":
					// A Gemini batch's inputConfig names its input file by resource name.
					if name, ok := item.(string); ok && strings.HasPrefix(name, "files/") {
						add(configstoreTables.ProviderObjectKindFile, name)
					}
				case "file_ids":
					if ids, ok := item.([]any); ok {
						for _, id := range ids {
							if s, ok := id.(string); ok {
								add(configstoreTables.ProviderObjectKindFile, s)
							}
						}
					}
				case "cached_content", "cachedContent":
					if name, ok := item.(string); ok {
						add(configstoreTables.ProviderObjectKindCachedContent, name)
					}
				}
				walk(item)
			}
		case []any:
			for _, item := range v {
				walk(item)
			}
		case string:
			// A location is read under a different key by each provider and operation
			// (fileUri, gcsUri, uri, url), so the key is not consulted: a string in a form
			// uploads are recorded under names that file wherever it sits.
			id, ok, unverifiable := recordedFileID(v)
			if unverifiable && !full {
				full = true
				refs = append(refs, providerObjectRef{kind: configstoreTables.ProviderObjectKindFile, provider: string(provider), indirect: true, unreadable: true})
			}
			if ok {
				add(configstoreTables.ProviderObjectKindFile, id)
			}
		}
	}
	walk(body)
	return refs
}

// requestsAutomaticContainer reports whether a Responses request's code_interpreter tool asks the
// provider for an automatic container (a container object rather than an existing id).
func requestsAutomaticContainer(req *schemas.BifrostRequest) bool {
	if req == nil || req.ResponsesRequest == nil || req.ResponsesRequest.Params == nil {
		return false
	}
	for _, tool := range req.ResponsesRequest.Params.Tools {
		if tool.ResponsesToolCodeInterpreter == nil {
			continue
		}
		switch tool.ResponsesToolCodeInterpreter.Container.(type) {
		case string, *string, nil:
		default:
			return true
		}
	}
	return false
}

// objectCreationRequestTypes are the calls that record an object of each kind, and so the calls
// whose upstream holds the objects recorded under a provider name. A file is uploaded, or written
// by a batch as its output or error file, which is recorded when a batch retrieve names it
// (recordBatchFiles) and lives where that retrieve went; a container is created, or opened
// automatically by a Responses call's code interpreter.
var objectCreationRequestTypes = map[string][]schemas.RequestType{
	configstoreTables.ProviderObjectKindFile:          {schemas.FileUploadRequest, schemas.BatchCreateRequest, schemas.BatchRetrieveRequest},
	configstoreTables.ProviderObjectKindBatch:         {schemas.BatchCreateRequest},
	configstoreTables.ProviderObjectKindVideo:         {schemas.VideoGenerationRequest, schemas.VideoEditRequest, schemas.VideoRemixRequest},
	configstoreTables.ProviderObjectKindContainer:     {schemas.ContainerCreateRequest, schemas.ResponsesRequest, schemas.ResponsesStreamRequest},
	configstoreTables.ProviderObjectKindContainerFile: {schemas.ContainerFileCreateRequest},
	configstoreTables.ProviderObjectKindCachedContent: {schemas.CachedContentCreateRequest},
	configstoreTables.ProviderObjectKindImageTask:     {schemas.ImageGenerationTaskRequest, schemas.ImageEditTaskRequest},
}

// objectHolders returns the provider names whose recorded objects of kind a requestType call
// through provider reaches. An ownership row is kept under the provider name an object was created
// through, and the object lives where that provider's creating call went. A call reaches it when
// it goes to the same upstream (callTarget, sameAs): the base URLs match, or a full-URL
// request_path_overrides entry of either call lies under the other's base URL, or both are the
// same full URL. Only providers on the same base are compared: sharing an API format is not
// sharing an upstream, and an id that matches on another service is not the same object.
//
// A provider can send one call elsewhere and leave the rest on its base URL (TXE-7368), so its
// rows describe objects at its creating calls' upstream, not wherever its other calls go. Two uses
// read this, and each errs on its own safe side when a provider's creating calls go to more than
// one upstream:
//   - checking access (every = false): a provider's rows count when any of its creating calls
//     may share this call's upstream (mayShare: base URLs match, or either call has a full-URL
//     override), so a key cannot reach another key's object by naming a provider whose override
//     goes there, however its URL is spelled. The calling provider's own rows always count: an
//     extra row can only refuse an id, never admit one;
//   - forgetting a delete (every = true): a provider's rows are forgotten only when every one of
//     its creating calls went where the delete went (sameAs), so a binding whose object may
//     survive on another upstream is kept. The calling provider's own rows follow the same rule.
//     A stale binding of a deleted object only refuses an id nobody has.
//
// A kind with no creating calls listed compares base URLs alone.
func (p *GovernancePlugin) objectHolders(kind string, requestType schemas.RequestType, provider string, every bool) []string {
	if p.inMemoryStore == nil {
		return []string{provider}
	}
	configured := p.inMemoryStore.GetConfiguredProviders()
	if len(configured) == 0 {
		return []string{provider}
	}
	self := schemas.ModelProvider(provider)
	base := baseProviderOf(self, configured)
	creating := objectCreationRequestTypes[kind]
	call := callUpstream{endpoint: providerEndpoint(self, configured)}
	if len(creating) > 0 {
		call = callTarget(requestType, self, configured)
	}
	// Access widens the match and forgetting narrows it, each toward refusing (see above).
	reaches := call.sameAs
	if !every {
		reaches = call.mayShare
	}
	holds := func(name schemas.ModelProvider) bool {
		if len(creating) == 0 {
			return call.endpoint == providerEndpoint(name, configured)
		}
		for _, createType := range creating {
			if reaches(callTarget(createType, name, configured)) != every {
				return !every
			}
		}
		return every
	}
	var holders []string
	if _, listed := configured[self]; !listed || !every || holds(self) {
		holders = append(holders, provider)
	}
	var others []string
	for name := range configured {
		if name != self && baseProviderOf(name, configured) == base && holds(name) {
			others = append(others, string(name))
		}
	}
	sort.Strings(others)
	return append(holders, others...)
}

// callUpstream is where one request type's call of a provider goes: the full-URL override it
// reads (its own, or the one it falls back to), or else its base URL.
type callUpstream struct {
	endpoint string
	override string
}

func callTarget(requestType schemas.RequestType, name schemas.ModelProvider, configured map[schemas.ModelProvider]configstore.ProviderConfig) callUpstream {
	target := callUpstream{endpoint: providerEndpoint(name, configured)}
	config, ok := configured[name]
	if !ok || config.CustomProviderConfig == nil || len(config.CustomProviderConfig.RequestPathOverrides) == 0 {
		return target
	}
	// The call reads the entries the provider code reads, in its order: a call with no entry of its
	// own can follow another call's (providerUtils.ResourcePathOverrideFallbacks), as batch results
	// follow batch_retrieve on the openai and gemini bases. The first non-blank entry decides; a
	// relative one stays on the base URL.
	for _, readType := range providerUtils.OverrideRequestTypes(baseProviderOf(name, configured), requestType) {
		url := canonicalURL(config.CustomProviderConfig.RequestPathOverrides[readType])
		if url == "" {
			continue
		}
		if strings.HasPrefix(url, "https://") || strings.HasPrefix(url, "http://") {
			target.override = url
		}
		break
	}
	return target
}

// sameAs reports whether two providers' calls of one request type reach the same upstream.
func (a callUpstream) sameAs(b callUpstream) bool {
	under := func(url, endpoint string) bool { return endpoint != "" && strings.HasPrefix(url, endpoint+"/") }
	switch {
	case a.override == "" && b.override == "":
		return a.endpoint == b.endpoint
	case a.override != "" && b.override != "":
		return a.override == b.override
	case a.override != "":
		return under(a.override, b.endpoint)
	default:
		return under(b.override, a.endpoint)
	}
}

// mayShare is the wider form of sameAs that access checks use. Two calls with no full-URL
// override compare base URLs. A call with one may reach any upstream of its base, so it matches
// every other call on that base. Telling from URLs whether two hosts serve one store fails open
// whenever the spellings differ: paths that differ by call, a default port written out, an alias
// host, an override the base provider does not read. Counting another provider's rows can only
// refuse an id, and an id another key holds on a different account of the same vendor is refused
// only if it collides.
func (a callUpstream) mayShare(b callUpstream) bool {
	if a.override == "" && b.override == "" {
		return a.endpoint == b.endpoint
	}
	return true
}

// canonicalURL lowercases a base URL or full-URL override, trims it and its trailing slashes, and
// drops a default port (:443 on https, :80 on http), so that spellings of one endpoint compare
// equal.
func canonicalURL(raw string) string {
	url := strings.TrimRight(strings.ToLower(strings.TrimSpace(raw)), "/")
	scheme, port := "https://", ":443"
	rest, ok := strings.CutPrefix(url, scheme)
	if !ok {
		scheme, port = "http://", ":80"
		if rest, ok = strings.CutPrefix(url, scheme); !ok {
			return url
		}
	}
	host, path, hasPath := strings.Cut(rest, "/")
	trimmed, cut := strings.CutSuffix(host, port)
	if !cut {
		return url
	}
	if hasPath {
		return scheme + trimmed + "/" + path
	}
	return scheme + trimmed
}

// baseProviderDefaultEndpoints are the endpoints the base providers a custom provider can be built
// on send to when no base URL is configured, as their constructors set them. Bedrock is regional and
// has none; a provider missing here compares by its configured URL alone.
var baseProviderDefaultEndpoints = map[schemas.ModelProvider]string{
	schemas.OpenAI:      "https://api.openai.com",
	schemas.Anthropic:   "https://api.anthropic.com",
	schemas.Cohere:      "https://api.cohere.ai",
	schemas.Gemini:      "https://generativelanguage.googleapis.com/v1beta",
	schemas.HuggingFace: "https://router.huggingface.co",
	schemas.Replicate:   "https://api.replicate.com",
	schemas.Kling:       "https://api-singapore.klingai.com",
}

// providerEndpoint is the base URL a configured provider sends to, normalized for comparison. An
// unset base URL is its base provider's default, so a provider left on the default and one naming
// that default explicitly compare equal.
func providerEndpoint(name schemas.ModelProvider, configured map[schemas.ModelProvider]configstore.ProviderConfig) string {
	var url string
	if config, ok := configured[name]; ok && config.NetworkConfig != nil {
		url = config.NetworkConfig.BaseURL
	}
	if strings.TrimSpace(url) == "" {
		url = baseProviderDefaultEndpoints[baseProviderOf(name, configured)]
	}
	return canonicalURL(url)
}

// baseProviderOf is the upstream a configured provider name reaches: the base of a custom
// provider, or the name itself.
func baseProviderOf(name schemas.ModelProvider, configured map[schemas.ModelProvider]configstore.ProviderConfig) schemas.ModelProvider {
	if config, ok := configured[name]; ok && config.CustomProviderConfig != nil && config.CustomProviderConfig.BaseProviderType != "" {
		return config.CustomProviderConfig.BaseProviderType
	}
	return name
}

// cachedContentCarrier returns the provider and extra params of a request that can run on a
// cached content, or false for any other request.
func cachedContentCarrier(req *schemas.BifrostRequest) (schemas.ModelProvider, map[string]interface{}, bool) {
	switch {
	case req.ChatRequest != nil:
		if req.ChatRequest.Params == nil {
			return req.ChatRequest.Provider, nil, true
		}
		return req.ChatRequest.Provider, req.ChatRequest.Params.ExtraParams, true
	case req.ResponsesRequest != nil:
		if req.ResponsesRequest.Params == nil {
			return req.ResponsesRequest.Provider, nil, true
		}
		return req.ResponsesRequest.Provider, req.ResponsesRequest.Params.ExtraParams, true
	case req.ImageGenerationRequest != nil:
		if req.ImageGenerationRequest.Params == nil {
			return req.ImageGenerationRequest.Provider, nil, true
		}
		return req.ImageGenerationRequest.Provider, req.ImageGenerationRequest.Params.ExtraParams, true
	case req.ImageEditRequest != nil:
		if req.ImageEditRequest.Params == nil {
			return req.ImageEditRequest.Provider, nil, true
		}
		return req.ImageEditRequest.Provider, req.ImageEditRequest.Params.ExtraParams, true
	case req.TranscriptionRequest != nil:
		if req.TranscriptionRequest.Params == nil {
			return req.TranscriptionRequest.Provider, nil, true
		}
		return req.TranscriptionRequest.Provider, req.TranscriptionRequest.Params.ExtraParams, true
	}
	return "", nil, false
}

// providerObjectOwners returns, per provider-side id, the virtual key recorded as its creator,
// and the ids whose creator cannot be told. The ownership row wins; for batches and videos the
// accounting job row is read next, and for files the batch rows naming the file as input, output
// or error. Ids nobody recorded are absent from both.
//
// Vertex records an object under the full resource name its create returned, and a short form
// (a bare job number, "cachedContents/<n>") cannot be expanded back to it without the key's
// project and region. So on Vertex a short id is also matched as the last segment of the full
// names on record, ownership rows and batch job rows alike, in one lookup per page. A job number
// is scoped to a project and region: when the full names that end in it belong to more than one
// virtual key, the id is ambiguous and reported as such rather than resolved by guessing. The
// suffix lookup is kept to providers built on Vertex.
//
// requestType is the call being checked: its upstream decides which providers' rows count
// (objectHolders).
func (p *GovernancePlugin) providerObjectOwners(ctx *schemas.BifrostContext, kind string, requestType schemas.RequestType, provider string, ids []string) (map[string]string, map[string]struct{}, error) {
	return p.providerObjectOwnersOf(ctx, kind, requestType, provider, ids, false)
}

// contestedOwner stands for the creator of an id whose ownership rows, under providers that reach
// the same upstream, name different virtual keys. No key is it, so the id is refused and hidden
// for every key rather than handed to whichever row was read last.
const contestedOwner = "\x00contested"

// providerObjectOwnersOf is providerObjectOwners with one more fact: sameObject says the ids
// are the forms of a single object (see providerObjectIDForms) rather than distinct objects. An
// ownership row under any form then answers for all of them, and the fallbacks, a suffix scan
// and a scan of the batch history for files, are skipped. The forms of one object cannot name
// two creators: an object is created once, by one key.
func (p *GovernancePlugin) providerObjectOwnersOf(ctx *schemas.BifrostContext, kind string, requestType schemas.RequestType, provider string, ids []string, sameObject bool) (map[string]string, map[string]struct{}, error) {
	owners := make(map[string]string, len(ids))
	ambiguous := make(map[string]struct{})
	if len(ids) == 0 {
		return owners, ambiguous, nil
	}
	providers := p.objectHolders(kind, requestType, provider, false)
	objectIDs := make([]string, 0, len(ids)*len(providers))
	for _, name := range providers {
		for _, id := range ids {
			objectIDs = append(objectIDs, configstoreTables.ProviderObjectID(kind, name, id))
		}
	}
	objects, err := p.configStore.GetProviderObjectsByIDs(ctx, objectIDs)
	if err != nil {
		return nil, nil, err
	}
	for _, object := range objects {
		if object == nil || object.VirtualKeyID == "" {
			continue
		}
		// Rows under two holders of one upstream that name different keys for one id cannot both
		// be right; the id goes to no key (contestedOwner) rather than to whichever row came last.
		if existing, seen := owners[object.ObjectID]; seen && existing != object.VirtualKeyID {
			owners[object.ObjectID] = contestedOwner
			continue
		}
		owners[object.ObjectID] = object.VirtualKeyID
	}
	// A second source that names a different key for an id already answered makes it ambiguous:
	// the id is refused rather than resolved by whichever source was read first.
	claim := func(id, owner string) {
		if id == "" || owner == "" {
			return
		}
		if existing, seen := owners[id]; seen {
			if existing != owner {
				ambiguous[id] = struct{}{}
			}
			return
		}
		owners[id] = owner
	}
	// The fallbacks answer for what the ownership rows did not, each id once.
	// A list page can carry thousands of ids, so past a handful they are deduplicated through a
	// set rather than a quadratic scan; a single-object request allocates no set.
	unresolved := make([]string, 0, len(ids))
	var queued map[string]struct{}
	if len(ids) > 8 {
		queued = make(map[string]struct{}, len(ids))
	}
	for _, id := range ids {
		if _, seen := owners[id]; seen {
			continue
		}
		if queued == nil {
			if !slices.Contains(unresolved, id) {
				unresolved = append(unresolved, id)
			}
			continue
		}
		if _, dup := queued[id]; !dup {
			queued[id] = struct{}{}
			unresolved = append(unresolved, id)
		}
	}
	if len(unresolved) == 0 || (sameObject && len(owners) > 0) {
		return owners, ambiguous, nil
	}
	if err := p.resolveShortResourceNames(ctx, kind, provider, providers, unresolved, owners, ambiguous); err != nil {
		return nil, nil, err
	}
	switch kind {
	case configstoreTables.ProviderObjectKindBatch, configstoreTables.ProviderObjectKindVideo:
		jobIDs := make([]string, 0, len(unresolved)*len(providers))
		for _, name := range providers {
			for _, id := range unresolved {
				jobIDs = append(jobIDs, configstoreTables.ProviderJobID(kind, name, id))
			}
		}
		jobs, err := p.configStore.GetProviderJobsByIDs(ctx, jobIDs)
		if err != nil {
			return nil, nil, err
		}
		for _, job := range jobs {
			if job != nil && job.VirtualKeyID != nil {
				claim(job.JobID, *job.VirtualKeyID)
			}
		}
	case configstoreTables.ProviderObjectKindFile:
		// Only the files a batch produced follow its key. Its input file was there before the
		// batch: one uploaded through the gateway has its own row, and one uploaded outside it
		// stays shared, so running a batch on it must not make it the batch key's.
		wanted := make(map[string]struct{}, len(unresolved))
		for _, id := range unresolved {
			wanted[id] = struct{}{}
		}
		for _, name := range providers {
			jobs, err := p.configStore.GetProviderJobsByFileIDs(ctx, name, unresolved)
			if err != nil {
				return nil, nil, err
			}
			for _, job := range jobs {
				if job == nil || job.VirtualKeyID == nil {
					continue
				}
				for _, fileID := range []string{derefString(job.OutputFileID), derefString(job.ErrorFileID)} {
					if _, ok := wanted[fileID]; ok {
						claim(fileID, *job.VirtualKeyID)
					}
				}
			}
		}
	}
	return owners, ambiguous, nil
}

// resolveShortResourceNames answers, for the short ids among unresolved, from the full resource
// names on record whose last segment they are: ownership rows of the kind, and for batches the
// accounting job rows. One lookup per store per provider name covers every short id on the page.
// A short id whose full names belong to one key is claimed for it; one whose full names belong to
// several keys is ambiguous. Only providers built on Vertex record full names.
func (p *GovernancePlugin) resolveShortResourceNames(ctx *schemas.BifrostContext, kind, provider string, providers, unresolved []string, owners map[string]string, ambiguous map[string]struct{}) error {
	if _, named := resourceCollections[kind]; !named || !p.builtOnVertex(provider) {
		return nil
	}
	bySuffix := make(map[string][]string)
	for _, id := range unresolved {
		suffix, ok := resourceNameSuffix(kind, id)
		if !ok {
			continue
		}
		// The bare and collection forms of one short id share a suffix; a row under either form
		// has answered for both, so no scan is spent on the other.
		if _, done := owners[suffix]; done {
			continue
		}
		if _, done := owners[resourceCollections[kind]+"/"+suffix]; done {
			continue
		}
		bySuffix[suffix] = append(bySuffix[suffix], id)
	}
	if len(bySuffix) == 0 {
		return nil
	}
	suffixes := make([]string, 0, len(bySuffix))
	for suffix := range bySuffix {
		suffixes = append(suffixes, suffix)
	}
	sort.Strings(suffixes)
	claimants := make(map[string]map[string]struct{}, len(suffixes))
	note := func(fullName, owner string) {
		if owner == "" {
			return
		}
		suffix := fullName[strings.LastIndex(fullName, "/")+1:]
		if _, wanted := bySuffix[suffix]; !wanted {
			return
		}
		if claimants[suffix] == nil {
			claimants[suffix] = make(map[string]struct{})
		}
		claimants[suffix][owner] = struct{}{}
	}
	for _, name := range providers {
		objects, err := p.configStore.GetProviderObjectsByObjectIDSuffixes(ctx, kind, name, suffixes)
		if err != nil {
			return err
		}
		for _, object := range objects {
			if object != nil {
				note(object.ObjectID, object.VirtualKeyID)
			}
		}
		if kind != configstoreTables.ProviderObjectKindBatch {
			continue
		}
		jobs, err := p.configStore.GetProviderJobsByJobIDSuffixes(ctx, kind, name, suffixes)
		if err != nil {
			return err
		}
		for _, job := range jobs {
			if job != nil && job.VirtualKeyID != nil {
				note(job.JobID, *job.VirtualKeyID)
			}
		}
	}
	for suffix, keys := range claimants {
		for _, id := range bySuffix[suffix] {
			if len(keys) > 1 {
				ambiguous[id] = struct{}{}
				continue
			}
			for owner := range keys {
				if _, seen := owners[id]; !seen {
					owners[id] = owner
				}
			}
		}
	}
	return nil
}

// resourceNameSuffix returns the last segment of a short form of a Google resource name: a bare
// id, or "<collection>/<id>" for the kind's collection. A full name ("projects/...") is not short,
// and neither is any other id with a path in it.
func resourceNameSuffix(kind, id string) (string, bool) {
	if id == "" {
		return "", false
	}
	if !strings.Contains(id, "/") {
		return id, true
	}
	collection := resourceCollections[kind]
	if collection != "" && strings.HasPrefix(id, collection+"/") && !strings.Contains(id[len(collection)+1:], "/") {
		return id[len(collection)+1:], true
	}
	return "", false
}

// builtOnVertex reports whether a provider name reaches Vertex: Vertex itself, or a custom
// provider built on it.
func (p *GovernancePlugin) builtOnVertex(provider string) bool {
	return configuredBaseProvider(p.inMemoryStore, schemas.ModelProvider(provider)) == schemas.Vertex
}

// enforceProviderObjectOwnership refuses a request for a provider object whose recorded creator
// is a virtual key other than the request's own. It runs after evaluation, which is what stamps
// the request's virtual key id. A store failure refuses the request rather than letting an
// unverified read through, and so does running without a config store: no creator is recorded
// anywhere then, so a virtual-key request that addresses a bound object cannot be checked and is
// refused. A list addresses nothing: its page is narrowed in the post-hook to what is recorded,
// and with no config store there is nothing to hide, so it goes through unfiltered, as it did
// before objects were bound; a list of tasks is the exception, refused there because a task is
// shown only when recorded for the key. Every object a request names is checked (see
// providerObjectReferences); the first one another key created refuses the request. None of
// this applies while client.enforce_vk_object_ownership is turned off; creators are still
// recorded then, so turning it back on also binds objects created meanwhile.
func (p *GovernancePlugin) enforceProviderObjectOwnership(ctx *schemas.BifrostContext, req *schemas.BifrostRequest) *schemas.LLMPluginShortCircuit {
	vkID := requestVirtualKeyID(ctx)
	if vkID == "" {
		return nil
	}
	// With client.enforce_vk_object_ownership turned off nothing is refused, but an automatic
	// container still needs its marker so the post-hook records its creator.
	enforced := p.objectOwnershipEnforced()
	autoContainer := requestsAutomaticContainer(req)
	if !enforced && !autoContainer {
		return nil
	}
	refs := providerObjectReferences(req)
	if autoContainer {
		var named []string
		for _, ref := range refs {
			if ref.kind == configstoreTables.ProviderObjectKindContainer && !ref.isList {
				named = append(named, ref.ids...)
			}
		}
		ctx.SetValue(governanceAutoContainerContextKey, named)
	}
	if !enforced {
		return nil
	}
	// Tuning jobs and tuned models carry no creator record (see passthroughReachesTuning).
	if r := req.PassthroughRequest; r != nil && passthroughReachesTuning(r) {
		ctx.SetValue(governanceRejectedContextKey, true)
		return &schemas.LLMPluginShortCircuit{Error: &schemas.BifrostError{
			Type:       bifrost.Ptr(string(DecisionAccessBlocked)),
			StatusCode: bifrost.Ptr(403),
			Error:      &schemas.ErrorField{Message: "fine-tuning through a passthrough route is not available to virtual keys: tuning jobs and tuned models carry no per-key ownership, so a request could reach another key's job or model"},
		}}
	}
	// A task created through a passthrough route would have no creator on record and be out of
	// its own creator's reach (see passthroughCreatesTask).
	if r := req.PassthroughRequest; r != nil && (passthroughCreatesTask(r) || (configuredBaseProvider(p.inMemoryStore, r.Provider) == schemas.Runware && runwareBodyCreatesTask(r.Method, r.Body))) {
		ctx.SetValue(governanceRejectedContextKey, true)
		return &schemas.LLMPluginShortCircuit{Error: &schemas.BifrostError{
			Type:       bifrost.Ptr(string(DecisionAccessBlocked)),
			StatusCode: bifrost.Ptr(403),
			Error:      &schemas.ErrorField{Message: "creating a batch or a video through a passthrough route is not available to virtual keys: the task would not be recorded for the key, and a virtual key reaches a task only under an id recorded for it; use the gateway's own batch or video route"},
		}}
	}
	// Settled before any lookup: a request that could not be read in full, or that names more
	// objects than the bound, is refused without a query.
	refs, verifiable := distinctObjectRefs(refs)
	if !verifiable {
		ctx.SetValue(governanceRejectedContextKey, true)
		return &schemas.LLMPluginShortCircuit{Error: &schemas.BifrostError{
			Type:       bifrost.Ptr(string(DecisionAccessBlocked)),
			StatusCode: bifrost.Ptr(400),
			Error:      &schemas.ErrorField{Message: fmt.Sprintf("the objects this request names cannot be verified for this virtual key: it names more than %d files, containers or caches, its provider-specific fields could not be read (raw JSON that does not parse, or data nested more than %d levels deep), or it names a stored object that cannot be resolved to one name (a wildcard or a '.' or '..' in its path, or an S3 access point or its alias in place of the bucket)", maxFileReferences, openValueDepth)},
		}}
	}
	// A retrieve by client_reference_id names no object id. With a config store the task the
	// vendor returns is checked in the post-hook (hideForeignVideoByReference); without one the
	// request falls through and is refused like any other video read that cannot be verified.
	if r := req.VideoRetrieveRequest; r != nil && r.IDType == schemas.VideoIDTypeClientReference && p.configStore != nil {
		ctx.SetValue(governanceVideoByReferenceContextKey, true)
		return nil
	}
	// Every object is checked before any folder is claimed, so a request refused for one
	// object claims nothing.
	var claims []string
	for _, ref := range refs {
		if ref.isList {
			continue
		}
		shortCircuit, own := p.refuseForeignProviderObject(ctx, req, ref, vkID)
		if shortCircuit != nil {
			return shortCircuit
		}
		claim, shortCircuit := p.refuseForeignStorageFolder(ctx, ref, own, vkID)
		if shortCircuit != nil {
			return shortCircuit
		}
		if claim != "" {
			claims = append(claims, claim)
		}
	}
	if shortCircuit := p.claimStorageFolders(ctx, claims, vkID); shortCircuit != nil {
		return shortCircuit
	}
	// A native list is narrowed in the post-hook. A passthrough list is the provider's raw page,
	// which cannot be, so a virtual key is sent to the gateway's own list route; the objects the
	// path names (a container whose files are listed) were checked above first. With no config
	// store nothing is recorded: for most kinds there is then nothing to hide, but a task is
	// shown only when it is recorded for the key (ownedOnlyByRecord), so a list of tasks cannot
	// be answered and is refused like a task addressed by id.
	for _, ref := range refs {
		if ref.isList && p.configStore == nil && ownedOnlyByRecord(ref.kind) {
			ctx.SetValue(governanceRejectedContextKey, true)
			return &schemas.LLMPluginShortCircuit{Error: &schemas.BifrostError{
				Type:       bifrost.Ptr(string(DecisionAccessBlocked)),
				StatusCode: bifrost.Ptr(403),
				Error:      &schemas.ErrorField{Message: fmt.Sprintf("%s access cannot be verified for virtual keys without a config store", providerObjectLabel(ref.kind))},
			}}
		}
	}
	for _, ref := range refs {
		if ref.passthroughList && p.configStore != nil {
			ctx.SetValue(governanceRejectedContextKey, true)
			return &schemas.LLMPluginShortCircuit{Error: &schemas.BifrostError{
				Type:       bifrost.Ptr(string(DecisionAccessBlocked)),
				StatusCode: bifrost.Ptr(403),
				Error:      &schemas.ErrorField{Message: fmt.Sprintf("listing %ss through a passthrough route is not available to virtual keys; use the gateway's list route, which shows the key's own objects", providerObjectLabel(ref.kind))},
			}}
		}
	}
	return nil
}

// distinctObjectRefs returns the references of a request with repeats of one object removed,
// and whether the request can be verified from them. Each remaining reference is one ownership
// lookup, so the count is bounded over the whole request, however its parts were read (typed
// fields, extra params, a passthrough body, every inline item of a batch): a request marked
// unreadable, or one naming more than maxFileReferences distinct objects, cannot be.
func distinctObjectRefs(refs []providerObjectRef) ([]providerObjectRef, bool) {
	if len(refs) <= 1 {
		return refs, len(refs) == 0 || !refs[0].unreadable
	}
	type objectKey struct{ kind, provider, id string }
	seen := make(map[objectKey]struct{}, len(refs))
	distinct := refs[:0:0]
	named := 0
	for _, ref := range refs {
		if ref.unreadable {
			return nil, false
		}
		if !ref.isList {
			key := objectKey{ref.kind, ref.provider, ref.requestID}
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			if named++; named > maxFileReferences {
				return nil, false
			}
		}
		distinct = append(distinct, ref)
	}
	return distinct, true
}

// refuseForeignProviderObject is enforceProviderObjectOwnership for one referenced object. own
// is true when the object is let through because it is recorded for the request's own key.
func (p *GovernancePlugin) refuseForeignProviderObject(ctx *schemas.BifrostContext, req *schemas.BifrostRequest, ref providerObjectRef, vkID string) (shortCircuit *schemas.LLMPluginShortCircuit, own bool) {
	label := providerObjectLabel(ref.kind)
	if p.configStore == nil {
		// A batch create only names its input file, and a container create or a request on a
		// cached content only names what it copies or runs on; with nothing recorded anywhere
		// those cannot be anyone else's on record, and the batch's own results stay refused
		// below. Creating and running are left open so a deployment without a config store can
		// still submit batches, build containers and use caches.
		if req.BatchCreateRequest != nil || ref.indirect {
			return nil, false
		}
		ctx.SetValue(governanceRejectedContextKey, true)
		return &schemas.LLMPluginShortCircuit{Error: &schemas.BifrostError{
			Type:       bifrost.Ptr(string(DecisionAccessBlocked)),
			StatusCode: bifrost.Ptr(403),
			Error:      &schemas.ErrorField{Message: fmt.Sprintf("%s access cannot be verified for virtual keys without a config store", label)},
		}}, false
	}
	owners, ambiguous, err := p.providerObjectOwnersOf(ctx, ref.kind, req.RequestType, ref.provider, ref.ids, true)
	if err != nil {
		p.logger.Error("failed to load %s %s for ownership check: %v", label, ref.requestID, err)
		ctx.SetValue(governanceRejectedContextKey, true)
		return &schemas.LLMPluginShortCircuit{Error: &schemas.BifrostError{
			StatusCode: bifrost.Ptr(500),
			Error:      &schemas.ErrorField{Message: fmt.Sprintf("failed to verify access to %s '%s'", label, ref.requestID)},
		}}, false
	}
	notFound := func() *schemas.LLMPluginShortCircuit {
		ctx.SetValue(governanceRejectedContextKey, true)
		return &schemas.LLMPluginShortCircuit{Error: &schemas.BifrostError{
			Type:       bifrost.Ptr(string(DecisionAccessBlocked)),
			StatusCode: bifrost.Ptr(404),
			Error:      &schemas.ErrorField{Message: fmt.Sprintf("%s '%s' not found", label, ref.requestID)},
		}}
	}
	for _, owner := range owners {
		if owner != vkID {
			return notFound(), false
		}
	}
	if len(ambiguous) > 0 {
		// A short Vertex id that several keys' full names end in names one job per project, which
		// cannot be told apart before a key is chosen. The caller is told how to name it exactly.
		ctx.SetValue(governanceRejectedContextKey, true)
		return &schemas.LLMPluginShortCircuit{Error: &schemas.BifrostError{
			Type:       bifrost.Ptr(string(DecisionAccessBlocked)),
			StatusCode: bifrost.Ptr(400),
			Error:      &schemas.ErrorField{Message: fmt.Sprintf("%s '%s' matches objects in more than one project; address it by its full resource name", label, ref.requestID)},
		}}, false
	}
	// A task fails closed (TXE-6431 for image tasks, TXE-7119 for batches and videos): a key
	// reaches one only under an id recorded for it. A task created outside the gateway, before
	// ownership was recorded, or whose row was lost has no owner to check the caller against, and
	// an id can carry more than the vendor's task id (BFL's carries the model and the polling
	// host), so "no row stays reachable" would let any key reach it by rewriting the rest.
	if ownedOnlyByRecord(ref.kind) && len(owners) == 0 {
		return notFound(), false
	}
	return nil, len(owners) > 0
}

// hiddenProviderObjects returns, from a page of ids, the ones the virtual key may not see, as a
// predicate on the listed id. Every form of each id is looked up, and an id is hidden when any
// of its forms is another key's. A task (ownedOnlyByRecord) is also hidden when none of its
// forms has an owner on record, as it is not found when addressed. A stored file with no row
// of its own is hidden when it lies in a folder another key claimed. The predicate is nil when
// nothing on the page is hidden.
func (p *GovernancePlugin) hiddenProviderObjects(ctx *schemas.BifrostContext, kind string, requestType schemas.RequestType, provider, vkID string, ids []string) (func(id string) bool, error) {
	forms := make([]string, 0, len(ids)*2)
	unverifiable := false
	for _, id := range ids {
		lookup, unclear := providerObjectLookupForms(kind, schemas.ModelProvider(provider), id)
		forms = append(forms, lookup...)
		unverifiable = unverifiable || unclear
	}
	owners, ambiguous, err := p.providerObjectOwners(ctx, kind, requestType, provider, forms)
	if err != nil {
		return nil, err
	}
	hidden := make(map[string]struct{})
	for id, owner := range owners {
		if owner != vkID {
			hidden[id] = struct{}{}
		}
	}
	// An item whose creator cannot be told is not shown rather than guessed at.
	for id := range ambiguous {
		hidden[id] = struct{}{}
	}
	// A stored object with no row of its own follows the folder it lies in (storagefolders.go).
	var inForeignFolder map[string]struct{}
	if kind == configstoreTables.ProviderObjectKindFile {
		if inForeignFolder, err = p.listedInForeignFolders(ctx, provider, vkID, ids, owners); err != nil {
			return nil, err
		}
	}
	closed := ownedOnlyByRecord(kind)
	if len(hidden) == 0 && len(inForeignFolder) == 0 && !unverifiable && !closed {
		return nil, nil
	}
	return func(id string) bool {
		if _, foreign := inForeignFolder[id]; foreign {
			return true
		}
		lookup, unclear := providerObjectLookupForms(kind, schemas.ModelProvider(provider), id)
		if unclear {
			return true
		}
		if closed {
			// Shown only when a form is recorded for this key and none for another.
			own := false
			for _, form := range lookup {
				if _, other := hidden[form]; other {
					return true
				}
				own = own || owners[form] == vkID
			}
			return !own
		}
		for _, form := range lookup {
			if _, ok := hidden[form]; ok {
				return true
			}
		}
		return false
	}, nil
}

// hideForeignVideoByReference is the ownership check of a video retrieve by client_reference_id:
// it returns a 404 when the task the vendor returned was created by a virtual key other than the
// request's own, exactly as the pre-hook answers a retrieve by another key's task id. It returns
// nil for every other response, and a 500 when the creator cannot be looked up, so an unchecked
// task is never sent.
func (p *GovernancePlugin) hideForeignVideoByReference(ctx *schemas.BifrostContext, provider string, result *schemas.BifrostResponse) *schemas.BifrostError {
	// The response type is checked first: every response that is not a video task object costs
	// one nil check here and no context read.
	if result == nil || result.VideoGenerationResponse == nil {
		return nil
	}
	if marked, _ := ctx.Value(governanceVideoByReferenceContextKey).(bool); !marked {
		return nil
	}
	if p.configStore == nil || !p.objectOwnershipEnforced() {
		return nil
	}
	vkID := requestVirtualKeyID(ctx)
	if vkID == "" {
		return nil
	}
	video := result.VideoGenerationResponse
	label := providerObjectLabel(configstoreTables.ProviderObjectKindVideo)
	hidden, err := p.hiddenProviderObjects(ctx, configstoreTables.ProviderObjectKindVideo, schemas.VideoRetrieveRequest, provider, vkID, []string{video.ID})
	if err != nil {
		p.logger.Error("failed to load %s ownership for a retrieve by client_reference_id: %v", label, err)
		return &schemas.BifrostError{
			StatusCode: bifrost.Ptr(500),
			Error:      &schemas.ErrorField{Message: fmt.Sprintf("failed to verify access to the %s", label)},
			ExtraFields: schemas.BifrostErrorExtraFields{
				RequestType: schemas.VideoRetrieveRequest,
				Provider:    schemas.ModelProvider(provider),
				ErrorType:   schemas.ErrorTypeBifrostInternal,
			},
		}
	}
	if hidden == nil || !hidden(video.ID) {
		return nil
	}
	return &schemas.BifrostError{
		Type:       bifrost.Ptr(string(DecisionAccessBlocked)),
		StatusCode: bifrost.Ptr(404),
		Error:      &schemas.ErrorField{Message: fmt.Sprintf("%s not found for this client_reference_id", label)},
		ExtraFields: schemas.BifrostErrorExtraFields{
			RequestType: schemas.VideoRetrieveRequest,
			Provider:    schemas.ModelProvider(provider),
		},
	}
}

// filterProviderObjectList narrows a list of provider objects to the ones the request's virtual
// key may see. Pagination cursors are the provider's and are left alone; only the page's items
// are narrowed. A provider can attach the whole unfiltered page as the raw response (OpenAI does
// so on every batch item), so once anything is removed the raw page is dropped from the list and
// from the kept items; a page nothing was removed from is left as the provider sent it. If the ownership lookup fails it returns an error and leaves the list
// untouched; the caller fails the request rather than send a page it could not check.
func (p *GovernancePlugin) filterProviderObjectList(ctx *schemas.BifrostContext, provider string, result *schemas.BifrostResponse) error {
	if p.configStore == nil || result == nil || !p.objectOwnershipEnforced() {
		return nil
	}
	vkID := requestVirtualKeyID(ctx)
	if vkID == "" {
		return nil
	}
	fail := func(kind string, err error) error {
		label := providerObjectLabel(kind)
		p.logger.Error("failed to load %s ownership for list filter: %v", label, err)
		return fmt.Errorf("failed to verify access to the %s list", label)
	}
	switch {
	case result.BatchListResponse != nil && len(result.BatchListResponse.Data) > 0:
		list := result.BatchListResponse
		ids := make([]string, 0, len(list.Data))
		for _, item := range list.Data {
			ids = append(ids, item.ID)
		}
		hidden, err := p.hiddenProviderObjects(ctx, configstoreTables.ProviderObjectKindBatch, schemas.BatchListRequest, provider, vkID, ids)
		if err != nil {
			return fail(configstoreTables.ProviderObjectKindBatch, err)
		}
		if hidden == nil {
			return nil
		}
		kept := make([]schemas.BifrostBatchRetrieveResponse, 0, len(list.Data))
		for _, item := range list.Data {
			if !hidden(item.ID) {
				item.ExtraFields.RawResponse = nil
				kept = append(kept, item)
			}
		}
		if len(kept) == len(list.Data) {
			return nil
		}
		list.Data = kept
		list.ExtraFields.RawResponse = nil
	case result.FileListResponse != nil && len(result.FileListResponse.Data) > 0:
		list := result.FileListResponse
		ids := make([]string, 0, len(list.Data))
		for _, item := range list.Data {
			ids = append(ids, item.ID)
		}
		hidden, err := p.hiddenProviderObjects(ctx, configstoreTables.ProviderObjectKindFile, schemas.FileListRequest, provider, vkID, ids)
		if err != nil {
			return fail(configstoreTables.ProviderObjectKindFile, err)
		}
		if hidden == nil {
			return nil
		}
		kept := make([]schemas.FileObject, 0, len(list.Data))
		for _, item := range list.Data {
			if !hidden(item.ID) {
				kept = append(kept, item)
			}
		}
		if len(kept) == len(list.Data) {
			return nil
		}
		list.Data = kept
		list.ExtraFields.RawResponse = nil
	case result.VideoListResponse != nil && len(result.VideoListResponse.Data) > 0:
		list := result.VideoListResponse
		ids := make([]string, 0, len(list.Data))
		for _, item := range list.Data {
			ids = append(ids, item.ID)
		}
		hidden, err := p.hiddenProviderObjects(ctx, configstoreTables.ProviderObjectKindVideo, schemas.VideoListRequest, provider, vkID, ids)
		if err != nil {
			return fail(configstoreTables.ProviderObjectKindVideo, err)
		}
		if hidden == nil {
			return nil
		}
		kept := make([]schemas.VideoObject, 0, len(list.Data))
		for _, item := range list.Data {
			if !hidden(item.ID) {
				kept = append(kept, item)
			}
		}
		if len(kept) == len(list.Data) {
			return nil
		}
		list.Data = kept
		list.ExtraFields.RawResponse = nil
	case result.ContainerListResponse != nil && len(result.ContainerListResponse.Data) > 0:
		list := result.ContainerListResponse
		ids := make([]string, 0, len(list.Data))
		for _, item := range list.Data {
			ids = append(ids, item.ID)
		}
		hidden, err := p.hiddenProviderObjects(ctx, configstoreTables.ProviderObjectKindContainer, schemas.ContainerListRequest, provider, vkID, ids)
		if err != nil {
			return fail(configstoreTables.ProviderObjectKindContainer, err)
		}
		if hidden == nil {
			return nil
		}
		kept := make([]schemas.ContainerObject, 0, len(list.Data))
		for _, item := range list.Data {
			if !hidden(item.ID) {
				kept = append(kept, item)
			}
		}
		if len(kept) == len(list.Data) {
			return nil
		}
		list.Data = kept
		list.ExtraFields.RawResponse = nil
	case result.ContainerFileListResponse != nil && len(result.ContainerFileListResponse.Data) > 0:
		list := result.ContainerFileListResponse
		ids := make([]string, 0, len(list.Data))
		for _, item := range list.Data {
			ids = append(ids, item.ID)
		}
		hidden, err := p.hiddenProviderObjects(ctx, configstoreTables.ProviderObjectKindContainerFile, schemas.ContainerFileListRequest, provider, vkID, ids)
		if err != nil {
			return fail(configstoreTables.ProviderObjectKindContainerFile, err)
		}
		if hidden == nil {
			return nil
		}
		kept := make([]schemas.ContainerFileObject, 0, len(list.Data))
		for _, item := range list.Data {
			if !hidden(item.ID) {
				kept = append(kept, item)
			}
		}
		if len(kept) == len(list.Data) {
			return nil
		}
		list.Data = kept
		list.ExtraFields.RawResponse = nil
	case result.CachedContentListResponse != nil && len(result.CachedContentListResponse.CachedContents) > 0:
		list := result.CachedContentListResponse
		ids := make([]string, 0, len(list.CachedContents))
		for _, item := range list.CachedContents {
			ids = append(ids, item.Name)
		}
		hidden, err := p.hiddenProviderObjects(ctx, configstoreTables.ProviderObjectKindCachedContent, schemas.CachedContentListRequest, provider, vkID, ids)
		if err != nil {
			return fail(configstoreTables.ProviderObjectKindCachedContent, err)
		}
		if hidden == nil {
			return nil
		}
		kept := make([]schemas.CachedContentObject, 0, len(list.CachedContents))
		for _, item := range list.CachedContents {
			if !hidden(item.Name) {
				kept = append(kept, item)
			}
		}
		if len(kept) == len(list.CachedContents) {
			return nil
		}
		list.CachedContents = kept
		list.ExtraFields.RawResponse = nil
	}
	return nil
}

// recordProviderObjectLifecycle remembers the virtual key a create response was produced under,
// and forgets an object a delete response confirms gone. A video create is told apart from a
// retrieve, which answers with the same shape, by the request type. An id is recorded only as the
// create returned it; short forms of a full resource name are resolved by suffix at lookup time
// (see resolveShortResourceNames). A delete forgets every form of the id it names, and a delete in
// short form also forgets the deleting key's own full name that ends in it. A write that fails is
// logged: the object exists either way, and the response carrying its id is the caller's.
func (p *GovernancePlugin) recordProviderObjectLifecycle(ctx *schemas.BifrostContext, requestType schemas.RequestType, provider string, result *schemas.BifrostResponse) {
	if p.configStore == nil || result == nil || provider == "" {
		return
	}
	vkID := requestVirtualKeyID(ctx)
	if vkID == "" {
		return
	}
	if named, auto := ctx.Value(governanceAutoContainerContextKey).([]string); auto {
		p.recordAutomaticContainers(ctx, provider, vkID, result, named)
	}
	var createdKind, createdID string
	var deletedKind, deletedID string
	switch {
	case result.FileUploadResponse != nil && requestType == schemas.FileUploadRequest:
		createdKind, createdID = configstoreTables.ProviderObjectKindFile, result.FileUploadResponse.ID
	case result.VideoGenerationResponse != nil && (requestType == schemas.VideoGenerationRequest || requestType == schemas.VideoEditRequest || requestType == schemas.VideoRemixRequest):
		createdKind, createdID = configstoreTables.ProviderObjectKindVideo, result.VideoGenerationResponse.ID
	case result.ImageTaskResponse != nil && (requestType == schemas.ImageGenerationTaskRequest || requestType == schemas.ImageEditTaskRequest):
		createdKind, createdID = configstoreTables.ProviderObjectKindImageTask, result.ImageTaskResponse.ID
	case result.ContainerCreateResponse != nil:
		createdKind, createdID = configstoreTables.ProviderObjectKindContainer, result.ContainerCreateResponse.ID
	case result.ContainerFileCreateResponse != nil:
		createdKind, createdID = configstoreTables.ProviderObjectKindContainerFile, result.ContainerFileCreateResponse.ID
	case result.CachedContentCreateResponse != nil:
		createdKind, createdID = configstoreTables.ProviderObjectKindCachedContent, result.CachedContentCreateResponse.Name
	case result.BatchCreateResponse != nil:
		createdKind, createdID = configstoreTables.ProviderObjectKindBatch, result.BatchCreateResponse.ID
	case result.BatchRetrieveResponse != nil && requestType == schemas.BatchRetrieveRequest:
		p.recordBatchFiles(ctx, provider, result.BatchRetrieveResponse)
		return
	case result.FileDeleteResponse != nil && result.FileDeleteResponse.Deleted:
		deletedKind, deletedID = configstoreTables.ProviderObjectKindFile, result.FileDeleteResponse.ID
	case result.VideoDeleteResponse != nil && result.VideoDeleteResponse.Deleted:
		deletedKind, deletedID = configstoreTables.ProviderObjectKindVideo, result.VideoDeleteResponse.ID
	case result.ContainerDeleteResponse != nil && result.ContainerDeleteResponse.Deleted:
		deletedKind, deletedID = configstoreTables.ProviderObjectKindContainer, result.ContainerDeleteResponse.ID
	case result.ContainerFileDeleteResponse != nil && result.ContainerFileDeleteResponse.Deleted:
		deletedKind, deletedID = configstoreTables.ProviderObjectKindContainerFile, result.ContainerFileDeleteResponse.ID
	case result.CachedContentDeleteResponse != nil && result.CachedContentDeleteResponse.Deleted:
		deletedKind, deletedID = configstoreTables.ProviderObjectKindCachedContent, result.CachedContentDeleteResponse.Name
	case result.BatchDeleteResponse != nil:
		deletedKind, deletedID = configstoreTables.ProviderObjectKindBatch, result.BatchDeleteResponse.ID
	default:
		return
	}
	if createdKind != "" && createdID != "" {
		// Recorded as the provider named it, and under no short form: a Vertex job number is one
		// project's, so a row keyed by it would be shared by every project's job of that number.
		// Short forms are resolved against the full names when they are asked for instead.
		p.recordProviderObjectCreator(ctx, createdKind, provider, createdID, vkID)
		return
	}
	// The object is forgotten under every provider name whose objects live where this delete went:
	// the delete was admitted through one of them, and a row recorded through another would
	// otherwise linger. A provider whose creating calls go anywhere else, even only in part, keeps
	// its rows, since the object there may survive (objectHolders, every = true; TXE-7368).
	aliases := p.objectHolders(deletedKind, requestType, provider, true)
	for _, alias := range aliases {
		for _, id := range providerObjectIDForms(deletedKind, schemas.ModelProvider(alias), deletedID) {
			if err := p.configStore.DeleteProviderObject(ctx, configstoreTables.ProviderObjectID(deletedKind, alias, id)); err != nil {
				p.logger.Warn("failed to forget deleted %s %s on %s: %v", providerObjectLabel(deletedKind), id, alias, err)
			}
		}
	}
	if suffix, short := resourceNameSuffix(deletedKind, deletedID); short && p.builtOnVertex(provider) {
		p.forgetShortFormDelete(ctx, deletedKind, aliases, suffix, vkID)
	}
}

// forgetShortFormDelete forgets the deleting key's own full name that a delete by a short form
// removed. The provider deleted one object and echoes only the short form; when exactly one of the
// key's full names ends in it, that one is forgotten. When several do (the key's jobs with one
// number in two projects), which one went is unknown, so every binding is kept: a stale binding
// to the same key is harmless, an unbound survivor is not. Other keys' rows are never touched.
func (p *GovernancePlugin) forgetShortFormDelete(ctx *schemas.BifrostContext, kind string, providers []string, suffix, vkID string) {
	var own []*configstoreTables.TableProviderObject
	for _, name := range providers {
		objects, err := p.configStore.GetProviderObjectsByObjectIDSuffixes(ctx, kind, name, []string{suffix})
		if err != nil {
			p.logger.Warn("failed to look up the full name of deleted %s %s on %s: %v", providerObjectLabel(kind), suffix, name, err)
			return
		}
		for _, object := range objects {
			if object != nil && object.VirtualKeyID == vkID {
				own = append(own, object)
			}
		}
	}
	if len(own) != 1 {
		return
	}
	if err := p.configStore.DeleteProviderObject(ctx, own[0].ID); err != nil {
		p.logger.Warn("failed to forget deleted %s %s on %s by its full name: %v", providerObjectLabel(kind), suffix, own[0].Provider, err)
	}
}

// recordAutomaticContainers records the containers a Responses answer names for the key that asked
// for an automatic container: the container on the response and the one each code_interpreter call
// ran in. The ownership write keeps the first creator, so a container already recorded keeps its
// owner. Only requests that asked for an automatic container get here (see
// requestsAutomaticContainer), and named lists the containers the same request named by id, which
// the response may list beside the automatic one: a container named by id is never claimed by
// using it.
func (p *GovernancePlugin) recordAutomaticContainers(ctx *schemas.BifrostContext, provider, vkID string, result *schemas.BifrostResponse, named []string) {
	resp := result.ResponsesResponse
	if resp == nil && result.ResponsesStreamResponse != nil {
		resp = result.ResponsesStreamResponse.Response
	}
	if resp == nil {
		return
	}
	seen := make(map[string]struct{}, len(named))
	for _, id := range named {
		seen[id] = struct{}{}
	}
	record := func(id string) {
		if id == "" {
			return
		}
		if _, dup := seen[id]; dup {
			return
		}
		seen[id] = struct{}{}
		p.recordProviderObjectCreator(ctx, configstoreTables.ProviderObjectKindContainer, provider, id, vkID)
	}
	if resp.Container != nil {
		record(resp.Container.ID)
	}
	for _, item := range resp.Output {
		if item.ResponsesToolMessage != nil && item.ResponsesToolMessage.ResponsesCodeInterpreterToolCall != nil {
			record(item.ResponsesToolMessage.ResponsesCodeInterpreterToolCall.ContainerID)
		}
	}
}

// recordProviderObjectCreator writes the ownership row of an object, trying once more when the
// write fails: the object exists at the provider either way, and a bound object beats an unbound
// one. A second failure is logged and the object follows the unbound-object rule.
func (p *GovernancePlugin) recordProviderObjectCreator(ctx *schemas.BifrostContext, kind, provider, objectID, vkID string) {
	row := &configstoreTables.TableProviderObject{
		ID:           configstoreTables.ProviderObjectID(kind, provider, objectID),
		Kind:         kind,
		Provider:     provider,
		ObjectID:     objectID,
		VirtualKeyID: vkID,
	}
	err := p.configStore.UpsertProviderObject(ctx, row)
	if err != nil {
		err = p.configStore.UpsertProviderObject(ctx, row)
	}
	if err != nil {
		p.logger.Warn("failed to record the creating virtual key of %s %s on %s (after a retry): %v", providerObjectLabel(kind), objectID, provider, err)
	}
}

// recordBatchFiles binds the output and error files a batch retrieve names to the batch's
// recorded creator. Those files exist only once the batch has run, so the create response could
// not record them, and the accounting rows that also name them are written only when the batch
// sweeper runs, which needs the logging plugin. A batch nobody recorded hands its files to no
// one: the retrieving key is not made their owner.
func (p *GovernancePlugin) recordBatchFiles(ctx *schemas.BifrostContext, provider string, batch *schemas.BifrostBatchRetrieveResponse) {
	fileIDs := make([]string, 0, 2)
	for _, fileID := range []string{derefString(batch.OutputFileID), derefString(batch.ErrorFileID)} {
		if fileID != "" {
			fileIDs = append(fileIDs, fileID)
		}
	}
	if len(fileIDs) == 0 || batch.ID == "" {
		return
	}
	owners, _, err := p.providerObjectOwners(ctx, configstoreTables.ProviderObjectKindBatch, schemas.BatchRetrieveRequest, provider, []string{batch.ID})
	if err != nil {
		p.logger.Warn("failed to look up the creating virtual key of batch %s on %s: %v", batch.ID, provider, err)
		return
	}
	owner := owners[batch.ID]
	if owner == "" {
		return
	}
	if owner == contestedOwner {
		// Two keys hold the batch on one upstream, so its files have no one owner to record. The
		// batch itself is refused to both, which is what reaches its output file id.
		p.logger.Warn("not recording the files of batch %s on %s: its ownership rows name more than one virtual key", batch.ID, provider)
		return
	}
	for _, fileID := range fileIDs {
		p.recordProviderObjectCreator(ctx, configstoreTables.ProviderObjectKindFile, provider, fileID, owner)
		p.claimBatchOutputFolder(ctx, fileID, owner)
	}
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
