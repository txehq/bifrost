package governance

import (
	"fmt"
	"reflect"
	"strings"

	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/schemas"
	configstoreTables "github.com/capsohq/bifrost/framework/configstore/tables"
)

// Cloud resources a virtual key may name (TXE-7088).
//
// A request can carry the name of storage or of a Pub/Sub topic that the provider then reads,
// writes or publishes to with the operator's own cloud identity: a gs:// or s3:// input, an
// output location, the bucket of a file operation, Veo's pubsubTopic. Ownership (providerjobs.go)
// refuses what is recorded for another key; it says nothing about a path nobody recorded, so
// every key sharing a provider key could reach whatever that identity can.
//
// A virtual key can therefore be given the storage it may use (allowed_storage_prefixes) and the
// topics it may name (allowed_pubsub_topics). A key with a list may name only what the list
// covers; a key with none is not restricted, as before. The check is one function over the
// references the ownership walk already finds (requestFileRefs, see filerefs.go), on every
// provider and every route, so a field or a route added later is covered when it is classified
// there. It does not depend on client.enforce_vk_object_ownership: the list is the key's own.

// The allowlists of the request's virtual key, stamped by StampVirtualKeyScope when the key has
// one. Absent means not restricted.
const (
	governanceAllowedStoragePrefixesContextKey schemas.BifrostContextKey = "bf-governance-allowed-storage-prefixes"
	governanceAllowedPubSubTopicsContextKey    schemas.BifrostContextKey = "bf-governance-allowed-pubsub-topics"
)

// stampVirtualKeyCloudResources publishes the key's allowlists for enforceCloudResourceAllowlists.
// The slices are the store's own, stamped as they are and only read: the lists are normalized
// once, when the key is saved (configstoreTables.NormalizeVirtualKeyAllowlists), not on the
// request path. A list is stamped whenever the key has one. An entry that was not normalized
// (a key that reached memory some other way) is compared as written and covers at most what it
// literally says, never more.
func stampVirtualKeyCloudResources(ctx *schemas.BifrostContext, virtualKey *configstoreTables.TableVirtualKey) {
	if ctx == nil || virtualKey == nil {
		return
	}
	if len(virtualKey.AllowedStoragePrefixes) > 0 {
		ctx.SetValue(governanceAllowedStoragePrefixesContextKey, virtualKey.AllowedStoragePrefixes)
	}
	if len(virtualKey.AllowedPubSubTopics) > 0 {
		ctx.SetValue(governanceAllowedPubSubTopicsContextKey, virtualKey.AllowedPubSubTopics)
	}
}

// storageLocationAllowed reports whether location, a gs:// or s3:// URI as recordedFileID
// returns it, lies under one of the prefixes at a folder boundary: "gs://b/tenant-a" covers
// "gs://b/tenant-a" and "gs://b/tenant-a/x", not "gs://b/tenant-ab/x". Prefixes are normalized
// (configstoreTables.NormalizeVirtualKeyStoragePrefix): no trailing slash, scheme in lower case.
// An entry that is not, or that names no bucket, covers nothing.
func storageLocationAllowed(prefixes []string, location string) bool {
	for _, prefix := range prefixes {
		if !isStorageURI(prefix) || strings.HasSuffix(prefix, "/") || len(location) < len(prefix) || location[:len(prefix)] != prefix {
			continue
		}
		if len(location) == len(prefix) || location[len(prefix)] == '/' {
			return true
		}
	}
	return false
}

// isStorageURI reports whether id is a storage URI in the form recordedFileID returns.
func isStorageURI(id string) bool {
	for _, scheme := range storageURISchemes {
		if strings.HasPrefix(id, scheme) {
			return true
		}
	}
	return false
}

// fileStorageConfigType is the storage_config of the file operations.
var fileStorageConfigType = reflect.TypeOf((*schemas.FileStorageConfig)(nil))

// storageConfigLocation is the storage a file operation runs in. A list reads every object
// whose name starts with its prefix, as a string: the prefix "tenant-a" also lists "tenant-ab/".
// So a list's location is raw, compared as a string that must begin inside an allowed folder,
// while an upload's or a lookup's is a folder.
type storageConfigLocation struct {
	location string
	raw      bool // a list's name prefix, not a folder
}

// storageConfigLocations returns the storage a file operation runs in: the bucket and folder of
// its storage_config and of Bedrock's s3_bucket and s3_prefix extra params. It reads every
// request type that carries a storage_config (upload, list, retrieve, content, delete), found
// by its field so a file operation added later is read too.
//
// Bedrock lets the extra params stand in for the storage_config, or override it, piece by
// piece, differently per operation. Rather than repeat each operation's rule, every bucket the
// request names is paired with every prefix it names: a pair the provider does not use costs a
// compare, one this check left out would run unchecked.
func storageConfigLocations(req *schemas.BifrostRequest) []storageConfigLocation {
	var locations []storageConfigLocation
	root := reflect.ValueOf(req).Elem()
	for i := 0; i < root.NumField(); i++ {
		field := root.Field(i)
		if field.Kind() != reflect.Pointer || field.IsNil() || field.Elem().Kind() != reflect.Struct {
			continue
		}
		request := field.Elem()
		config := request.FieldByName("StorageConfig")
		if !config.IsValid() || config.Type() != fileStorageConfigType {
			continue
		}
		raw := strings.Contains(root.Type().Field(i).Name, "List")
		add := func(scheme string, buckets, prefixes []string) {
			for _, bucket := range buckets {
				bucket = strings.TrimSpace(bucket)
				if hasPrefixFold(bucket, scheme) {
					bucket = bucket[len(scheme):]
				}
				name, inBucket, _ := strings.Cut(bucket, "/")
				if name == "" {
					continue
				}
				for _, prefix := range prefixes {
					// A list's prefix is sent to the storage as written, so it is compared as
					// written: "/tenant-a/" lists under "<bucket>//tenant-a/", which is not the
					// folder "tenant-a". An upload or a lookup names a folder, which the
					// providers trim.
					object := inBucket + prefix
					if !raw {
						object = strings.Trim(object, "/")
					}
					location := scheme + name
					if object != "" || raw {
						location += "/" + object
					}
					locations = append(locations, storageConfigLocation{location: location, raw: raw})
				}
			}
		}
		var s3Buckets []string
		s3Prefixes := []string{""}
		if storage, _ := config.Interface().(*schemas.FileStorageConfig); storage != nil {
			if storage.GCS != nil {
				add("gs://", []string{storage.GCS.Bucket}, []string{storage.GCS.Prefix})
			}
			if storage.S3 != nil {
				s3Buckets = append(s3Buckets, storage.S3.Bucket)
				s3Prefixes = append(s3Prefixes, storage.S3.Prefix)
			}
		}
		if extra := request.FieldByName("ExtraParams"); extra.IsValid() && extra.Kind() == reflect.Map {
			if params, ok := extra.Interface().(map[string]interface{}); ok {
				if bucket, ok := params["s3_bucket"].(string); ok {
					s3Buckets = append(s3Buckets, bucket)
				}
				if prefix, ok := params["s3_prefix"].(string); ok {
					s3Prefixes = append(s3Prefixes, prefix)
				}
			}
		}
		// The empty prefix stands for "no prefix given" and is dropped when one was.
		if len(s3Prefixes) > 1 {
			s3Prefixes = s3Prefixes[1:]
		}
		add("s3://", s3Buckets, s3Prefixes)
	}
	return locations
}

// storageConfigAllowed reports whether a file operation's storage lies inside the prefixes. A
// folder is a location like any other. A list's raw name prefix must begin inside an allowed
// folder: "s3://b/tenant-a/" and "s3://b/tenant-a/in" are inside "s3://b/tenant-a",
// "s3://b/tenant-a" is not, since it also lists "tenant-ab/".
func storageConfigAllowed(prefixes []string, config storageConfigLocation) bool {
	if !config.raw {
		return storageLocationAllowed(prefixes, config.location)
	}
	for _, prefix := range prefixes {
		if isStorageURI(prefix) && !strings.HasSuffix(prefix, "/") && strings.HasPrefix(config.location, prefix+"/") {
			return true
		}
	}
	return false
}

// requestStorageLocations returns every storage location a request names among its references,
// as gs:// or s3:// URIs: the stored objects of requestFileRefs and the objects its own shape
// addresses, in whatever URL form they were written. unverifiable is true when a reference
// could not be read or resolved, in which case the list is not the whole answer.
func requestStorageLocations(req *schemas.BifrostRequest) (locations []string, unverifiable bool) {
	for _, ref := range providerObjectReferences(req) {
		if ref.unreadable {
			unverifiable = true
		}
		if ref.isList || ref.kind != configstoreTables.ProviderObjectKindFile {
			continue
		}
		for _, id := range ref.ids {
			if isStorageURI(id) {
				locations = append(locations, id)
			}
		}
	}
	return locations, unverifiable
}

// topicCollector gathers the Pub/Sub topic names in a request's untyped data.
type topicCollector struct {
	topics     []string
	unreadable bool
}

// topicScanDepth bounds the nesting the topic scan follows, counted in containers (structs,
// maps, lists). Request types nest a dozen levels and provider data a few more; data nested
// deeper is not read and marks the request unreadable.
const topicScanDepth = 64

// scan reads a value for strings that are a full Pub/Sub topic name. No typed request field
// carries a topic: one reaches the provider only through untyped data (extra params, a raw
// body, the body of an inline batch item), so a string counts only once the walk has passed
// into such data (open). There the key a provider reads a topic under is its own (Veo's
// pubsubTopic) and is not consulted: a string of that shape names that topic wherever it sits.
func (c *topicCollector) scan(value reflect.Value, open bool, depth int) {
	if depth > topicScanDepth {
		c.unreadable = true
		return
	}
	switch value.Kind() {
	case reflect.Interface:
		if !value.IsNil() {
			c.scan(value.Elem(), true, depth)
		}
	case reflect.Pointer:
		if !value.IsNil() {
			c.scan(value.Elem(), open, depth)
		}
	case reflect.String:
		if s := strings.TrimSpace(value.String()); open && configstoreTables.IsPubSubTopicName(s) {
			c.topics = append(c.topics, s)
		}
	case reflect.Slice, reflect.Array:
		if value.Type().Elem().Kind() == reflect.Uint8 {
			// Bytes inside untyped data are a vendor member kept as raw JSON. Typed byte
			// fields are read by name in the struct case: most are file and media content.
			if open {
				c.scanBytes(value, depth)
			}
			return
		}
		for i := 0; i < value.Len(); i++ {
			c.scan(value.Index(i), open, depth+1)
		}
	case reflect.Map:
		iter := value.MapRange()
		for iter.Next() {
			c.scan(iter.Value(), true, depth+1)
		}
	case reflect.Struct:
		for i := 0; i < value.NumField(); i++ {
			field := value.Type().Field(i)
			if !field.IsExported() {
				continue
			}
			// A body forwarded to the provider as it was received is read; any other typed
			// byte field is content (an uploaded file, audio, an image) and is not.
			if isRawBodyField(field) {
				c.scanBytes(value.Field(i), depth)
				continue
			}
			c.scan(value.Field(i), open, depth+1)
		}
	}
}

// scanBytes reads raw bytes: a passthrough body, a raw request body, a vendor member kept as
// raw JSON. Bytes that mention no topic are not parsed; JSON is decoded and read as untyped
// data; bytes that mention a topic and do not decode cannot be read.
func (c *topicCollector) scanBytes(value reflect.Value, depth int) {
	if value.Kind() != reflect.Slice || value.Len() == 0 {
		return
	}
	raw := value.Bytes()
	if !strings.Contains(string(raw), "topics") && !strings.Contains(string(raw), `\u`) {
		return
	}
	var decoded any
	if err := schemas.Unmarshal(raw, &decoded); err != nil {
		if s := strings.TrimSpace(string(raw)); configstoreTables.IsPubSubTopicName(s) {
			c.topics = append(c.topics, s)
		} else {
			c.unreadable = true
		}
		return
	}
	c.scan(reflect.ValueOf(decoded), true, depth+1)
}

// requestPubSubTopics returns the Pub/Sub topics a request names in its untyped data, on
// whatever request type is set. It follows the plan the file extraction is built on
// (requestFileRefPlan), which already leads to every field holding untyped data or a forwarded
// body and skips the rest, so a chat's messages are not walked for it. A passthrough body and
// the bodies of inline batch items, which that plan leaves to their own readers, are read here.
// unreadable is true when part of the request could not be read.
func requestPubSubTopics(req *schemas.BifrostRequest) (topics []string, unreadable bool) {
	var collector topicCollector
	collector.walk(reflect.ValueOf(req).Elem(), requestFileRefPlan())
	if r := req.PassthroughRequest; r != nil {
		collector.scanBytes(reflect.ValueOf(r.Body), 0)
	}
	if r := req.BatchCreateRequest; r != nil {
		for i := range r.Requests {
			collector.scan(reflect.ValueOf(r.Requests[i].Body), true, 0)
			collector.scan(reflect.ValueOf(r.Requests[i].Params), true, 0)
		}
	}
	return collector.topics, collector.unreadable
}

// walk visits the planned fields of a struct value: untyped data and forwarded bodies are
// scanned, typed fields that lead to them are followed, file reference fields are skipped.
func (c *topicCollector) walk(value reflect.Value, plan *fileRefPlan) {
	for i := range plan.steps {
		step := &plan.steps[i]
		field := value.Field(step.index)
		switch {
		case step.kind != 0:
		case step.open:
			if field.Kind() == reflect.Slice && field.Type().Elem().Kind() == reflect.Uint8 {
				c.scanBytes(field, 0)
			} else {
				c.scan(field, true, 0)
			}
		default:
			c.walkStructs(field, step.next)
		}
	}
}

// walkStructs follows a field down to the structs it holds.
func (c *topicCollector) walkStructs(value reflect.Value, plan *fileRefPlan) {
	switch value.Kind() {
	case reflect.Pointer:
		if !value.IsNil() {
			c.walkStructs(value.Elem(), plan)
		}
	case reflect.Struct:
		c.walk(value, plan)
	case reflect.Slice, reflect.Array:
		for i := 0; i < value.Len(); i++ {
			c.walkStructs(value.Index(i), plan)
		}
	case reflect.Map:
		iter := value.MapRange()
		for iter.Next() {
			c.walkStructs(iter.Value(), plan)
		}
	}
}

// enforceCloudResourceAllowlists refuses a request that names storage or a Pub/Sub topic
// outside what its virtual key may use. A key with no list, and a request with no virtual key,
// are not restricted. A reference that cannot be read or resolved cannot be shown to be inside
// the list, so it is refused.
func (p *GovernancePlugin) enforceCloudResourceAllowlists(ctx *schemas.BifrostContext, req *schemas.BifrostRequest) *schemas.LLMPluginShortCircuit {
	if ctx == nil || req == nil {
		return nil
	}
	// Restricted when a list was stamped, even one with no valid entry left.
	prefixes, storageRestricted := ctx.Value(governanceAllowedStoragePrefixesContextKey).([]string)
	topics, topicsRestricted := ctx.Value(governanceAllowedPubSubTopicsContextKey).([]string)
	if !storageRestricted && !topicsRestricted {
		return nil
	}
	refuse := func(status int, message string) *schemas.LLMPluginShortCircuit {
		ctx.SetValue(governanceRejectedContextKey, true)
		return &schemas.LLMPluginShortCircuit{Error: &schemas.BifrostError{
			Type:       bifrost.Ptr(string(DecisionAccessBlocked)),
			StatusCode: bifrost.Ptr(status),
			Error:      &schemas.ErrorField{Message: message},
		}}
	}
	if storageRestricted {
		locations, unverifiable := requestStorageLocations(req)
		if unverifiable {
			return refuse(400, "this virtual key is limited to its allowed storage, and the storage this request names cannot be verified against it: a name could not be read or resolved to one bucket and path")
		}
		for _, location := range locations {
			if !storageLocationAllowed(prefixes, location) {
				return refuse(403, fmt.Sprintf("storage location '%s' is outside the storage this virtual key may use", location))
			}
		}
		// An inline batch on Vertex or Bedrock is turned into an input file that the provider
		// uploads to a location it derives itself (a sibling of the output folder on Vertex, a
		// bucket-root folder on Bedrock), after this check and outside the key's storage. The
		// request names no such location, so it cannot be held to the list; the key uploads its
		// input inside its storage and names it instead.
		if batch := req.BatchCreateRequest; batch != nil && len(batch.Requests) > 0 {
			switch configuredBaseProvider(p.inMemoryStore, batch.Provider) {
			case schemas.Vertex, schemas.Bedrock:
				return refuse(403, "this virtual key is limited to its allowed storage, and an inline batch on this provider is uploaded as an input file to a location the gateway chooses, outside it; upload the input file inside the key's storage and create the batch with input_file_id")
			}
		}
		for _, config := range storageConfigLocations(req) {
			if unresolvableStorageURI(config.location) {
				return refuse(400, "this virtual key is limited to its allowed storage, and the bucket and prefix this request names cannot be verified against it: they hold a wildcard, a '.' or '..' segment, or an S3 access point")
			}
			if !storageConfigAllowed(prefixes, config) {
				return refuse(403, fmt.Sprintf("storage location '%s' is outside the storage this virtual key may use", config.location))
			}
		}
	}
	if topicsRestricted {
		named, unreadable := requestPubSubTopics(req)
		if unreadable {
			return refuse(400, "this virtual key is limited to its allowed Pub/Sub topics, and the topics this request names cannot be verified against them: part of the request could not be read")
		}
		for _, topic := range named {
			allowed := false
			for _, candidate := range topics {
				allowed = allowed || candidate == topic
			}
			if !allowed {
				return refuse(403, fmt.Sprintf("Pub/Sub topic '%s' is not one this virtual key may use", topic))
			}
		}
	}
	return nil
}
