package governance

import (
	"reflect"
	"strings"
	"testing"

	"github.com/capsohq/bifrost/core/schemas"
	configstoreTables "github.com/capsohq/bifrost/framework/configstore/tables"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStorageLocationAllowed(t *testing.T) {
	prefixes := []string{"gs://bucket/tenant-a", "s3://other-bucket"}
	for location, want := range map[string]bool{
		"gs://bucket/tenant-a":             true,
		"gs://bucket/tenant-a/":            true,
		"gs://bucket/tenant-a/in/x.jsonl":  true,
		"gs://bucket/tenant-ab/x.jsonl":    false, // a name that only starts with the prefix
		"gs://bucket/tenant-b/x.jsonl":     false,
		"gs://bucket":                      false, // the folder's parent
		"gs://bucket/":                     false,
		"s3://bucket/tenant-a/x.jsonl":     false, // the same path in another store
		"s3://other-bucket":                true,
		"s3://other-bucket/any/x.jsonl":    true,
		"s3://other-bucket-2/any/x.jsonl":  false,
		"gs://BUCKET/tenant-a/x.jsonl":     false, // a bucket name is compared as written
		"https://example.com/tenant-a/x":   false,
		"gs://bucket/tenant-a/../tenant-b": true, // refused elsewhere as unresolvable, never reaches here
	} {
		if got := storageLocationAllowed(prefixes, location); got != want {
			t.Errorf("storageLocationAllowed(%q) = %v, want %v", location, got, want)
		}
	}
	if storageLocationAllowed(nil, "gs://bucket/x") {
		t.Errorf("an empty list allows a location")
	}
}

// restrictedPlugin returns a plugin whose test virtual key has the given allowlists.
func restrictedPlugin(t *testing.T, storagePrefixes, pubSubTopics []string) *GovernancePlugin {
	t.Helper()
	vk := buildVKForMCPStamping(nil)
	vk.ProviderConfigs = append(vk.ProviderConfigs, buildProviderConfig("vertex", []string{"*"}), buildProviderConfig("bedrock", []string{"*"}))
	vk.AllowedStoragePrefixes = storagePrefixes
	vk.AllowedPubSubTopics = pubSubTopics
	plugin := newAccessTestPlugin(t, vk, nil)
	plugin.configStore = newProviderObjectConfigStore(t)
	return plugin
}

// storageRequests builds, per surface, a request that names location on the provider: as an
// input, as an output, as the object a route addresses, in an extra param and in a raw body.
func storageRequests(provider schemas.ModelProvider, location string) map[string]*schemas.BifrostRequest {
	str := func(s string) *string { return &s }
	return map[string]*schemas.BifrostRequest{
		"chat file": {RequestType: schemas.ChatCompletionRequest, ChatRequest: &schemas.BifrostChatRequest{Provider: provider, Model: "m", Input: []schemas.ChatMessage{{
			Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentBlocks: []schemas.ChatContentBlock{{Type: schemas.ChatContentBlockTypeFile, File: &schemas.ChatInputFile{FileURL: str(location)}}}}}}}},
		"chat extra param": {RequestType: schemas.ChatCompletionRequest, ChatRequest: &schemas.BifrostChatRequest{Provider: provider, Model: "m",
			Params: &schemas.ChatParameters{ExtraParams: map[string]interface{}{"image": map[string]any{"gcsUri": location}}}}},
		"video input reference": {RequestType: schemas.VideoGenerationRequest, VideoGenerationRequest: &schemas.BifrostVideoGenerationRequest{Provider: provider, Model: "m",
			Input: &schemas.VideoGenerationInput{Prompt: "p", InputReference: str(location)}}},
		"video output storage uri": {RequestType: schemas.VideoGenerationRequest, VideoGenerationRequest: &schemas.BifrostVideoGenerationRequest{Provider: provider, Model: "m",
			Input: &schemas.VideoGenerationInput{Prompt: "p"}, Params: &schemas.VideoGenerationParameters{TaskControlParameters: schemas.TaskControlParameters{OutputStorageURI: str(location)}}}},
		"batch input file": {RequestType: schemas.BatchCreateRequest, BatchCreateRequest: &schemas.BifrostBatchCreateRequest{Provider: provider, Model: str("m"), InputFileID: location}},
		"batch output folder": {RequestType: schemas.BatchCreateRequest, BatchCreateRequest: &schemas.BifrostBatchCreateRequest{Provider: provider, Model: str("m"),
			Requests: []schemas.BatchRequestItem{{CustomID: "1", Body: map[string]any{"model": "m"}}}, OutputFolder: &schemas.BatchOutputFolder{URL: location}}},
		"batch output extra param": {RequestType: schemas.BatchCreateRequest, BatchCreateRequest: &schemas.BifrostBatchCreateRequest{Provider: provider, Model: str("m"),
			Requests: []schemas.BatchRequestItem{{CustomID: "1", Body: map[string]any{"model": "m"}}}, ExtraParams: map[string]any{"output_s3_uri": location}}},
		"batch inline item": {RequestType: schemas.BatchCreateRequest, BatchCreateRequest: &schemas.BifrostBatchCreateRequest{Provider: provider, Model: str("m"),
			Requests: []schemas.BatchRequestItem{{CustomID: "1", Body: map[string]any{"contents": []any{map[string]any{"fileData": map[string]any{"fileUri": location}}}}}}}},
		"file content":  {RequestType: schemas.FileContentRequest, FileContentRequest: &schemas.BifrostFileContentRequest{Provider: provider, FileID: location}},
		"file retrieve": {RequestType: schemas.FileRetrieveRequest, FileRetrieveRequest: &schemas.BifrostFileRetrieveRequest{Provider: provider, FileID: location}},
		"file delete":   {RequestType: schemas.FileDeleteRequest, FileDeleteRequest: &schemas.BifrostFileDeleteRequest{Provider: provider, FileID: location}},
		"passthrough body": {RequestType: schemas.PassthroughRequest, PassthroughRequest: &schemas.BifrostPassthroughRequest{Provider: provider, Method: "POST", Path: "/v1/models/m:generateContent",
			Body: []byte(`{"contents":[{"parts":[{"fileData":{"fileUri":"` + location + `"}}]}]}`)}},
	}
}

// A virtual key with allowed_storage_prefixes names only storage the list covers, whether the
// request reads it or writes it, on every surface and in every form of the name; a key with no
// list, and a request with no key, are not restricted (TXE-7088). The objects here are recorded
// for nobody, so ownership has nothing to say: this is the rule for a path nobody recorded.
func TestVirtualKeyStorageAllowlist(t *testing.T) {
	plugin := restrictedPlugin(t, []string{"gs://shared-bucket/tenant-a/", "s3://tenant-a-bucket"}, nil)
	open := restrictedPlugin(t, nil, nil)
	for _, tc := range []struct {
		provider        schemas.ModelProvider
		inside, outside string
		insideByHTTPS   string
		outsideByHTTPS  string
		outsideNamesake string
	}{
		{schemas.Vertex, "gs://shared-bucket/tenant-a/in/a.jsonl", "gs://shared-bucket/tenant-b/in/a.jsonl",
			"https://storage.googleapis.com/shared-bucket/tenant-a/in/a.jsonl", "https://storage.googleapis.com/shared-bucket/tenant-b/in/a.jsonl", "gs://shared-bucket/tenant-ab/a.jsonl"},
		{schemas.Bedrock, "s3://tenant-a-bucket/in/a.jsonl", "s3://tenant-b-bucket/in/a.jsonl",
			"https://tenant-a-bucket.s3.us-east-1.amazonaws.com/in/a.jsonl", "arn:aws:s3:::tenant-b-bucket/in/a.jsonl", "s3://tenant-a-bucket-2/a.jsonl"},
	} {
		for surface := range storageRequests(tc.provider, tc.inside) {
			t.Run(string(tc.provider)+" "+surface, func(t *testing.T) {
				build := func(location string) *schemas.BifrostRequest { return storageRequests(tc.provider, location)[surface] }
				for _, location := range []string{tc.inside, tc.insideByHTTPS} {
					_, shortCircuit, err := plugin.PreLLMHook(presentCtx(mcpTestVKValue), build(location))
					require.NoError(t, err)
					assert.Nil(t, shortCircuit, "%s is inside the key's storage", location)
				}
				for _, location := range []string{tc.outside, tc.outsideByHTTPS, tc.outsideNamesake} {
					_, shortCircuit, err := plugin.PreLLMHook(presentCtx(mcpTestVKValue), build(location))
					require.NoError(t, err)
					require.NotNil(t, shortCircuit, "%s is outside the key's storage", location)
					assert.Equal(t, 403, *shortCircuit.Error.StatusCode, location)
					assert.Contains(t, shortCircuit.Error.Error.Message, "outside the storage this virtual key may use", location)

					_, shortCircuit, err = open.PreLLMHook(presentCtx(mcpTestVKValue), build(location))
					require.NoError(t, err)
					assert.Nil(t, shortCircuit, "a key with no list is not restricted: %s", location)
					_, shortCircuit, err = plugin.PreLLMHook(emptyCtx(), build(location))
					require.NoError(t, err)
					assert.Nil(t, shortCircuit, "a request that presented no key is not restricted: %s", location)
				}
				// A name that cannot be resolved cannot be shown to be inside the list.
				_, shortCircuit, err := plugin.PreLLMHook(presentCtx(mcpTestVKValue), build(strings.TrimSuffix(tc.inside, "a.jsonl")+"*"))
				require.NoError(t, err)
				require.NotNil(t, shortCircuit, "a wildcard cannot be verified against the list")
				assert.Equal(t, 400, *shortCircuit.Error.StatusCode)
			})
		}
	}

	// A request that names no storage is untouched, and so is one naming a file by a provider id.
	for name, req := range map[string]*schemas.BifrostRequest{
		"plain chat": {RequestType: schemas.ChatCompletionRequest, ChatRequest: &schemas.BifrostChatRequest{Provider: schemas.OpenAI, Model: "m",
			Input: []schemas.ChatMessage{{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: schemas.Ptr("a gs://shared-bucket/tenant-b/ path in prose")}}}}},
		"web image": storageRequests(schemas.OpenAI, "https://example.com/cat.png")["chat file"],
		"file id":   {RequestType: schemas.FileContentRequest, FileContentRequest: &schemas.BifrostFileContentRequest{Provider: schemas.OpenAI, FileID: "file-abc"}},
	} {
		_, shortCircuit, err := plugin.PreLLMHook(presentCtx(mcpTestVKValue), req)
		require.NoError(t, err)
		assert.Nil(t, shortCircuit, name)
	}

	// The list is the key's own: it holds with ownership enforcement turned off.
	plugin.objectOwnershipEnforcedFn = func() bool { return false }
	_, shortCircuit, err := plugin.PreLLMHook(presentCtx(mcpTestVKValue), storageRequests(schemas.Vertex, "gs://shared-bucket/tenant-b/x")["chat file"])
	require.NoError(t, err)
	require.NotNil(t, shortCircuit, "the allowlist does not depend on client.enforce_vk_object_ownership")
	assert.Equal(t, 403, *shortCircuit.Error.StatusCode)
}

// Every file operation that carries a storage_config runs in a bucket and folder the request
// names. Each is found by its field, so a file operation added later is read too, and its
// bucket and folder must be inside the key's storage.
func TestVirtualKeyStorageAllowlistCoversEveryStorageConfig(t *testing.T) {
	plugin := restrictedPlugin(t, []string{"gs://shared-bucket/tenant-a", "s3://shared-bucket/tenant-a"}, nil)
	root := reflect.TypeOf(schemas.BifrostRequest{})
	found := 0
	for i := 0; i < root.NumField(); i++ {
		field := root.Field(i)
		if field.Type.Kind() != reflect.Pointer || field.Type.Elem().Kind() != reflect.Struct {
			continue
		}
		config, ok := field.Type.Elem().FieldByName("StorageConfig")
		if !ok || config.Type != fileStorageConfigType {
			continue
		}
		found++
		for name, tc := range map[string]struct {
			config  *schemas.FileStorageConfig
			allowed bool
		}{
			"gcs inside":            {&schemas.FileStorageConfig{GCS: &schemas.GCSStorageConfig{Bucket: "shared-bucket", Prefix: "tenant-a/uploads"}}, true},
			"gcs outside":           {&schemas.FileStorageConfig{GCS: &schemas.GCSStorageConfig{Bucket: "shared-bucket", Prefix: "tenant-b/uploads"}}, false},
			"gcs bucket root":       {&schemas.FileStorageConfig{GCS: &schemas.GCSStorageConfig{Bucket: "shared-bucket"}}, false},
			"s3 inside":             {&schemas.FileStorageConfig{S3: &schemas.S3StorageConfig{Bucket: "shared-bucket", Prefix: "/tenant-a/"}}, true},
			"s3 bucket as a uri":    {&schemas.FileStorageConfig{S3: &schemas.S3StorageConfig{Bucket: "s3://shared-bucket/tenant-a/", Prefix: "in"}}, true},
			"s3 outside":            {&schemas.FileStorageConfig{S3: &schemas.S3StorageConfig{Bucket: "other-bucket", Prefix: "tenant-a"}}, false},
			"one inside, one out":   {&schemas.FileStorageConfig{GCS: &schemas.GCSStorageConfig{Bucket: "shared-bucket", Prefix: "tenant-a"}, S3: &schemas.S3StorageConfig{Bucket: "other-bucket"}}, false},
			"a prefix that climbs":  {&schemas.FileStorageConfig{S3: &schemas.S3StorageConfig{Bucket: "shared-bucket", Prefix: "tenant-a/../tenant-b"}}, false},
			"no storage config set": {nil, true},
			// A prefix with no trailing slash is a folder for an upload or a lookup, and a name
			// prefix for a list, where "tenant-a" also lists "tenant-ab/".
			"a folder named without its slash": {&schemas.FileStorageConfig{S3: &schemas.S3StorageConfig{Bucket: "shared-bucket", Prefix: "tenant-a"}}, !strings.Contains(field.Name, "List")},
			"a name prefix past the folder":    {&schemas.FileStorageConfig{S3: &schemas.S3StorageConfig{Bucket: "shared-bucket", Prefix: "tenant-a/in"}}, true},
		} {
			req := &schemas.BifrostRequest{}
			slot := reflect.ValueOf(req).Elem().Field(i)
			slot.Set(reflect.New(field.Type.Elem()))
			slot.Elem().FieldByName("Provider").Set(reflect.ValueOf(schemas.Bedrock))
			if tc.config != nil {
				slot.Elem().FieldByName("StorageConfig").Set(reflect.ValueOf(tc.config))
			}
			_, shortCircuit, err := plugin.PreLLMHook(presentCtx(mcpTestVKValue), req)
			require.NoError(t, err)
			if tc.allowed {
				assert.Nil(t, shortCircuit, "%s, %s", field.Name, name)
			} else {
				require.NotNil(t, shortCircuit, "%s, %s: the bucket and folder are outside the key's storage", field.Name, name)
			}
		}
		// Bedrock also takes the bucket and folder as extra params, which stand in for the
		// storage_config or override it piece by piece. Every bucket is paired with every prefix.
		if _, ok := field.Type.Elem().FieldByName("ExtraParams"); ok {
			inside := &schemas.FileStorageConfig{S3: &schemas.S3StorageConfig{Bucket: "shared-bucket", Prefix: "tenant-a/in"}}
			for name, tc := range map[string]struct {
				config  *schemas.FileStorageConfig
				extra   map[string]interface{}
				allowed bool
			}{
				"a bucket in extra params":                       {nil, map[string]interface{}{"s3_bucket": "other-bucket", "s3_prefix": "in"}, false},
				"an inside bucket and prefix in extra params":    {nil, map[string]interface{}{"s3_bucket": "shared-bucket", "s3_prefix": "tenant-a/in"}, true},
				"a prefix in extra params over an inside config": {inside, map[string]interface{}{"s3_prefix": "tenant-b/in"}, false},
				"a bucket in extra params over an inside config": {inside, map[string]interface{}{"s3_bucket": "other-bucket"}, false},
				"extra params that say the same as the config":   {inside, map[string]interface{}{"s3_bucket": "shared-bucket", "s3_prefix": "tenant-a/in"}, true},
			} {
				req := &schemas.BifrostRequest{}
				slot := reflect.ValueOf(req).Elem().Field(i)
				slot.Set(reflect.New(field.Type.Elem()))
				slot.Elem().FieldByName("Provider").Set(reflect.ValueOf(schemas.Bedrock))
				if tc.config != nil {
					slot.Elem().FieldByName("StorageConfig").Set(reflect.ValueOf(tc.config))
				}
				slot.Elem().FieldByName("ExtraParams").Set(reflect.ValueOf(tc.extra))
				_, shortCircuit, err := plugin.PreLLMHook(presentCtx(mcpTestVKValue), req)
				require.NoError(t, err)
				if tc.allowed {
					assert.Nil(t, shortCircuit, "%s, %s", field.Name, name)
				} else {
					require.NotNil(t, shortCircuit, "%s, %s: outside the key's storage", field.Name, name)
				}
			}
		}
	}
	require.GreaterOrEqual(t, found, 5, "the file operations with a storage_config were not found by reflection")
}

// A virtual key with allowed_pubsub_topics names only those topics. A topic reaches the
// provider through untyped data, where any string that is a full topic name is one.
func TestVirtualKeyPubSubTopicAllowlist(t *testing.T) {
	const own, other = "projects/p/topics/tenant-a", "projects/p/topics/tenant-b"
	plugin := restrictedPlugin(t, nil, []string{own})
	open := restrictedPlugin(t, nil, nil)
	video := func(extra map[string]interface{}) *schemas.BifrostRequest {
		return &schemas.BifrostRequest{RequestType: schemas.VideoGenerationRequest, VideoGenerationRequest: &schemas.BifrostVideoGenerationRequest{Provider: schemas.OpenAI, Model: "m",
			Input: &schemas.VideoGenerationInput{Prompt: "p"}, Params: &schemas.VideoGenerationParameters{ExtraParams: extra}}}
	}
	passthrough := func(body string) *schemas.BifrostRequest {
		return &schemas.BifrostRequest{RequestType: schemas.PassthroughRequest, PassthroughRequest: &schemas.BifrostPassthroughRequest{Provider: schemas.OpenAI, Method: "POST", Path: "/v1/models/veo:predictLongRunning", Body: []byte(body)}}
	}
	for name, tc := range map[string]struct {
		req     *schemas.BifrostRequest
		refused bool
	}{
		"the key's own topic":               {video(map[string]interface{}{"pubsubTopic": own}), false},
		"another topic":                     {video(map[string]interface{}{"pubsubTopic": other}), true},
		"another topic under another key":   {video(map[string]interface{}{"notify": map[string]any{"topic": other}}), true},
		"another topic in a list":           {video(map[string]interface{}{"topics": []any{own, other}}), true},
		"another topic in a raw body":       {passthrough(`{"parameters":{"pubsubTopic":"` + other + `"}}`), true},
		"the key's own topic in a raw body": {passthrough(`{"parameters":{"pubsubTopic":"` + own + `"}}`), false},
		"no topic":                          {video(map[string]interface{}{"resolution": "720p"}), false},
		"a topic name in the prompt": {&schemas.BifrostRequest{RequestType: schemas.VideoGenerationRequest, VideoGenerationRequest: &schemas.BifrostVideoGenerationRequest{Provider: schemas.OpenAI, Model: "m",
			Input: &schemas.VideoGenerationInput{Prompt: other}}}, false},
	} {
		_, shortCircuit, err := plugin.PreLLMHook(presentCtx(mcpTestVKValue), tc.req)
		require.NoError(t, err)
		if !tc.refused {
			assert.Nil(t, shortCircuit, name)
			continue
		}
		require.NotNil(t, shortCircuit, name)
		assert.Equal(t, 403, *shortCircuit.Error.StatusCode, name)
		assert.Contains(t, shortCircuit.Error.Error.Message, other, name)
		_, shortCircuit, err = open.PreLLMHook(presentCtx(mcpTestVKValue), tc.req)
		require.NoError(t, err)
		assert.Nil(t, shortCircuit, "a key with no topic list is not restricted: %s", name)
		_, shortCircuit, err = plugin.PreLLMHook(emptyCtx(), tc.req)
		require.NoError(t, err)
		assert.Nil(t, shortCircuit, "a request that presented no key is not restricted: %s", name)
	}
}

// A list whose entries do not normalize leaves the key restricted to nothing, never open.
func TestVirtualKeyAllowlistWithNoValidEntryAllowsNothing(t *testing.T) {
	plugin := restrictedPlugin(t, []string{"not-a-storage-prefix"}, []string{"not-a-topic"})
	_, shortCircuit, err := plugin.PreLLMHook(presentCtx(mcpTestVKValue), storageRequests(schemas.Vertex, "gs://bucket/x")["chat file"])
	require.NoError(t, err)
	require.NotNil(t, shortCircuit)
	assert.Equal(t, 403, *shortCircuit.Error.StatusCode)
}

func TestNormalizeVirtualKeyAllowlists(t *testing.T) {
	prefixes, topics, err := configstoreTables.NormalizeVirtualKeyAllowlists(
		[]string{" GS://bucket/tenant-a/ ", "gs://bucket/tenant-a", "s3://bucket"}, []string{"projects/p/topics/t", " projects/p/topics/t "})
	require.NoError(t, err)
	assert.Equal(t, []string{"gs://bucket/tenant-a", "s3://bucket"}, prefixes)
	assert.Equal(t, []string{"projects/p/topics/t"}, topics)
	for _, bad := range []string{"", "bucket/tenant-a", "https://storage.googleapis.com/bucket/x", "gs://", "gs:///x", "gs://bucket/*", "gs://bucket/a/../b", "gs://bucket//x", "s3://bucket/./x"} {
		_, _, err := configstoreTables.NormalizeVirtualKeyAllowlists([]string{bad}, nil)
		assert.Error(t, err, "storage prefix %q", bad)
	}
	for _, bad := range []string{"", "tenant-a", "projects/p/topics/", "projects//topics/t", "projects/p/topics/t/extra", "projects/p/subscriptions/s", "projects/p/topics/*"} {
		_, _, err := configstoreTables.NormalizeVirtualKeyAllowlists(nil, []string{bad})
		assert.Error(t, err, "topic %q", bad)
	}
	none, noTopics, err := configstoreTables.NormalizeVirtualKeyAllowlists(nil, []string{})
	require.NoError(t, err)
	assert.Nil(t, none)
	assert.Nil(t, noTopics)
}
