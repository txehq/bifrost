package governance

import (
	"context"
	"strings"
	"testing"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/capsohq/bifrost/framework/configstore"
	configstoreTables "github.com/capsohq/bifrost/framework/configstore/tables"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStorageFoldersAbove(t *testing.T) {
	for uri, want := range map[string][]string{
		"gs://b/a/b/c.jsonl": {"gs://b/", "gs://b/a/", "gs://b/a/b/"},
		"gs://b/a/":          {"gs://b/", "gs://b/a/"},
		"s3://b/out//x":      {"s3://b/", "s3://b/out/", "s3://b/out//"},
		"s3://b/x":           {"s3://b/"},
		"s3://b":             nil,
		"file-abc":           nil,
	} {
		got, tooDeep := storageFoldersAbove(uri)
		assert.False(t, tooDeep, uri)
		assert.Equal(t, want, got, uri)
	}
	_, tooDeep := storageFoldersAbove("gs://b/" + strings.Repeat("d/", maxStorageFolderLevels-1) + "x")
	assert.False(t, tooDeep, "the bound itself is readable")
	_, tooDeep = storageFoldersAbove("gs://b/" + strings.Repeat("d/", maxStorageFolderLevels) + "x")
	assert.True(t, tooDeep)

	assert.Equal(t, "gs://b/out/", storageFolderOf("gs://b/out"))
	assert.Equal(t, "gs://b/out/", storageFolderOf("gs://b/out/"))
	assert.Equal(t, "gs://b/out//", storageFolderOf("gs://b/out//"), "a doubled slash is a folder of its own")
}

// storageFolderOwner returns the key a folder is claimed for, or "".
func storageFolderOwner(t *testing.T, store configstore.ConfigStore, folder string) string {
	t.Helper()
	rows, err := store.GetProviderObjectsByObjectID(context.Background(), configstoreTables.ProviderObjectKindStorageFolder, folder)
	require.NoError(t, err)
	require.LessOrEqual(t, len(rows), 1, "one row per folder, whatever provider name the request used")
	if len(rows) == 0 {
		return ""
	}
	return rows[0].VirtualKeyID
}

// newStorageFolderTestPlugin is a plugin on a real config store whose test key is allowed on
// the providers that read and write object storage.
func newStorageFolderTestPlugin(t *testing.T) (*GovernancePlugin, configstore.ConfigStore) {
	t.Helper()
	store := newProviderObjectConfigStore(t)
	vk := buildVKForMCPStamping(nil)
	vk.ProviderConfigs = append(vk.ProviderConfigs, buildProviderConfig("vertex", []string{"*"}), buildProviderConfig("bedrock", []string{"*"}))
	plugin := newAccessTestPlugin(t, vk, nil)
	plugin.configStore = store
	return plugin, store
}

// storageFolderSurfaces builds the ways a request names storage on one provider.
type storageFolderSurfaces struct {
	provider schemas.ModelProvider
	root     string
}

func (s storageFolderSurfaces) batchInto(location string) *schemas.BifrostRequest {
	r := &schemas.BifrostBatchCreateRequest{Provider: s.provider, Model: schemas.Ptr("m"), InputFileID: s.root + "unrecorded/input.jsonl"}
	if s.provider == schemas.Bedrock {
		r.ExtraParams = map[string]any{"output_s3_uri": location}
	} else {
		r.OutputFolder = &schemas.BatchOutputFolder{URL: location}
	}
	return &schemas.BifrostRequest{RequestType: schemas.BatchCreateRequest, BatchCreateRequest: r}
}

func (s storageFolderSurfaces) videoInto(location string) *schemas.BifrostRequest {
	return &schemas.BifrostRequest{RequestType: schemas.VideoGenerationRequest, VideoGenerationRequest: &schemas.BifrostVideoGenerationRequest{Provider: s.provider, Model: "m",
		Input: &schemas.VideoGenerationInput{Prompt: "pan left"}, Params: &schemas.VideoGenerationParameters{TaskControlParameters: schemas.TaskControlParameters{OutputStorageURI: schemas.Ptr(location)}}}}
}

func (s storageFolderSurfaces) batchFrom(input string) *schemas.BifrostRequest {
	return &schemas.BifrostRequest{RequestType: schemas.BatchCreateRequest, BatchCreateRequest: &schemas.BifrostBatchCreateRequest{Provider: s.provider, Model: schemas.Ptr("m"), InputFileID: input}}
}

func (s storageFolderSurfaces) chatFile(uri string) *schemas.BifrostRequest {
	return &schemas.BifrostRequest{RequestType: schemas.ChatCompletionRequest, ChatRequest: &schemas.BifrostChatRequest{Provider: s.provider, Model: "m", Input: []schemas.ChatMessage{{
		Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentBlocks: []schemas.ChatContentBlock{{Type: schemas.ChatContentBlockTypeFile, File: &schemas.ChatInputFile{FileURL: schemas.Ptr(uri)}}}}}}}}
}

func (s storageFolderSurfaces) fileContent(id string) *schemas.BifrostRequest {
	return &schemas.BifrostRequest{RequestType: schemas.FileContentRequest, FileContentRequest: &schemas.BifrostFileContentRequest{Provider: s.provider, FileID: id}}
}

// A virtual key that names an output location claims it as a folder before the provider is
// called, and from then on what lies in the folder is that key's: the result files a provider
// writes there have no rows of their own, and without the folder any key that learned their
// names could read them. The claim needs an exclusive location, so two keys cannot write under
// one prefix or one inside the other's (TXE-7121).
func TestOutputFolderIsClaimedAndClosesWhatIsInside(t *testing.T) {
	for _, tc := range []struct {
		provider schemas.ModelProvider
		scheme   string
		https    func(bucket, object string) string
	}{
		{schemas.Vertex, "gs", func(bucket, object string) string { return "https://storage.googleapis.com/" + bucket + "/" + object }},
		{schemas.Bedrock, "s3", func(bucket, object string) string {
			return "https://" + bucket + ".s3.us-east-1.amazonaws.com/" + object
		}},
	} {
		t.Run(string(tc.provider), func(t *testing.T) {
			plugin, store := newStorageFolderTestPlugin(t)
			own, other := "vk-mcp-stamp", "vk-someone-else"
			bucket := "folder-bucket"
			root := tc.scheme + "://" + bucket + "/"
			s := storageFolderSurfaces{provider: tc.provider, root: root}
			folderKind, fileKind := configstoreTables.ProviderObjectKindStorageFolder, configstoreTables.ProviderObjectKindFile
			storage := storageFolderProvider(root)

			status := func(req *schemas.BifrostRequest) int {
				t.Helper()
				_, shortCircuit, err := plugin.PreLLMHook(presentCtx(mcpTestVKValue), req)
				require.NoError(t, err)
				if shortCircuit == nil {
					return 200
				}
				require.NotNil(t, shortCircuit.Error.StatusCode)
				return *shortCircuit.Error.StatusCode
			}

			// The other key's folder, as its own batch create recorded it, and things of its
			// own elsewhere in the bucket.
			seedProviderObject(t, store, folderKind, storage, root+"other/", other)
			seedProviderObject(t, store, fileKind, string(tc.provider), root+"team", other)
			seedProviderObject(t, store, fileKind, string(tc.provider), root+"shared/uploads/other.pdf", other)
			otherVK := other
			require.NoError(t, store.UpsertProviderJob(context.Background(), &configstoreTables.TableProviderJob{
				ID: configstoreTables.ProviderJobID(configstoreTables.ProviderJobKindBatch, string(tc.provider), "job-other"), Kind: configstoreTables.ProviderJobKindBatch,
				Provider: string(tc.provider), JobID: "job-other", AccountingStatus: configstoreTables.ProviderJobAccountingStatusPending,
				VirtualKeyID: &otherVK, OutputFileID: schemas.Ptr(root + "jobs/other/job-other/out.jsonl"),
			}))
			// The key's own upload, which happens to sit in the other key's folder.
			seedProviderObject(t, store, fileKind, string(tc.provider), root+"other/mine.pdf", own)

			t.Run("what lies in another key's folder is not found", func(t *testing.T) {
				result := root + "other/job-1/predictions.jsonl"
				for name, req := range map[string]*schemas.BifrostRequest{
					"chat file":                 s.chatFile(result),
					"chat file, https form":     s.chatFile(tc.https(bucket, "other/job-1/predictions.jsonl")),
					"file content":              s.fileContent(result),
					"batch input":               s.batchFrom(result),
					"a sub-folder, as input":    s.chatFile(root + "other/job-1/"),
					"a doubled slash inside it": s.chatFile(root + "other//x"),
				} {
					assert.Equal(t, 404, status(req), name)
				}
				_, shortCircuit, err := plugin.PreLLMHook(emptyCtx(), s.chatFile(result))
				require.NoError(t, err)
				assert.Nil(t, shortCircuit, "a request that presented no key is unrestricted")
			})

			t.Run("the closest record decides", func(t *testing.T) {
				assert.Equal(t, 200, status(s.chatFile(root+"other/mine.pdf")), "the key's own upload stays its own inside another key's folder")
				assert.Equal(t, 404, status(s.batchInto(root+"other/mine.pdf")), "but its file row does not open the folder as an output location")
			})

			t.Run("a file row is not a folder", func(t *testing.T) {
				assert.Equal(t, 200, status(s.chatFile(root+"team/notes.pdf")), "another key's file called team does not own team/...")
				assert.Equal(t, 404, status(s.chatFile(root+"team")), "the file itself is still the other key's")
				assert.Equal(t, 200, status(s.chatFile(root+"other-2/x")), "a folder that only starts with the same name")
				assert.Equal(t, 200, status(s.chatFile(root+"unrecorded/x")), "an object nobody recorded, in no claimed folder, stays shared")
			})

			for name, into := range map[string]func(string) *schemas.BifrostRequest{"batch output": s.batchInto, "task output": s.videoInto} {
				t.Run(name+" needs an exclusive location", func(t *testing.T) {
					for location, why := range map[string]string{
						root + "other/":            "the other key's folder",
						root + "other":             "the same folder written without its slash",
						root + "other/sub/":        "a folder inside it",
						root + "other//":           "a doubled slash inside it",
						root:                       "the bucket, which holds the other key's folder",
						root + "shared/":           "a folder holding another key's upload",
						root + "shared/uploads":    "the folder that upload is in",
						root + "jobs/":             "a folder holding another key's batch results",
						root + "jobs/other/":       "the folder those results are in",
						tc.https(bucket, "other/"): "the other key's folder by an https form",
					} {
						assert.Equal(t, 404, status(into(location)), why)
						assert.NotEqual(t, own, storageFolderOwner(t, store, storageFolderOf(location)), "a refused request claims nothing: %s", why)
					}
					assert.Equal(t, other, storageFolderOwner(t, store, root+"other/"), "the other key's claim is untouched")
				})
			}

			t.Run("a claim is made once and covers what is inside", func(t *testing.T) {
				assert.Empty(t, storageFolderOwner(t, store, root+"mine/run/"))
				assert.Equal(t, 200, status(s.batchInto(root+"mine/run")), "an unclaimed location with nothing of another key's beneath")
				assert.Equal(t, own, storageFolderOwner(t, store, root+"mine/run/"), "claimed under the folder name, with its slash")
				assert.Equal(t, 200, status(s.batchInto(root+"mine/run/")), "again, by the same key")
				assert.Equal(t, 200, status(s.videoInto(root+"mine/run/sub")), "a folder inside the key's own")
				assert.Empty(t, storageFolderOwner(t, store, root+"mine/run/sub/"), "which the folder above already covers")
				assert.Equal(t, 200, status(s.chatFile(root+"mine/run/job-9/predictions.jsonl")), "the key reads its own results")
				assert.Equal(t, 200, status(s.batchInto(root+"mine/")), "a parent holding only the key's own records")
				assert.Equal(t, own, storageFolderOwner(t, store, root+"mine/"))
				assert.Equal(t, 200, status(s.batchInto(root+"sibling/")), "a sibling of another key's folder under a shared parent")
			})

			t.Run("a request refused for another object claims nothing", func(t *testing.T) {
				req := s.batchInto(root + "fresh/")
				req.BatchCreateRequest.InputFileID = root + "shared/uploads/other.pdf"
				assert.Equal(t, 404, status(req))
				assert.Empty(t, storageFolderOwner(t, store, root+"fresh/"))
			})

			t.Run("a location that cannot be checked in full is refused", func(t *testing.T) {
				assert.Equal(t, 400, status(s.chatFile(root+strings.Repeat("d/", maxStorageFolderLevels)+"x")), "nested past the bound")
				assert.Equal(t, 400, status(s.batchInto(root+strings.Repeat("x", maxStorageFolderLength))), "too long to record")
			})
		})
	}
}

// A file list shows a stored object as a request naming it would find it: what lies in
// another key's folder is left out, the key's own upload there is kept, and so is everything
// in its own folders and in no folder at all.
func TestFileListLeavesOutWhatLiesInAnotherKeysFolder(t *testing.T) {
	plugin, store := newStorageFolderTestPlugin(t)
	root := "s3://list-bucket/"
	folderKind, fileKind := configstoreTables.ProviderObjectKindStorageFolder, configstoreTables.ProviderObjectKindFile
	seedProviderObject(t, store, folderKind, "s3", root+"other/", "vk-someone-else")
	seedProviderObject(t, store, folderKind, "s3", root+"mine/", "vk-mcp-stamp")
	seedProviderObject(t, store, fileKind, "bedrock", root+"other/mine.pdf", "vk-mcp-stamp")

	ctx := presentCtx(mcpTestVKValue)
	_, shortCircuit, err := plugin.PreLLMHook(ctx, &schemas.BifrostRequest{RequestType: schemas.FileListRequest, FileListRequest: &schemas.BifrostFileListRequest{Provider: schemas.Bedrock}})
	require.NoError(t, err)
	require.Nil(t, shortCircuit)
	page := func(ids ...string) *schemas.BifrostResponse {
		list := &schemas.BifrostFileListResponse{}
		for _, id := range ids {
			list.Data = append(list.Data, schemas.FileObject{ID: id})
		}
		return &schemas.BifrostResponse{FileListResponse: list}
	}
	shown := func(resp *schemas.BifrostResponse) []string {
		ids := make([]string, 0, len(resp.FileListResponse.Data))
		for _, item := range resp.FileListResponse.Data {
			ids = append(ids, item.ID)
		}
		return ids
	}
	list := page(root+"other/job-1/out.jsonl", root+"other/mine.pdf", root+"mine/job-2/out.jsonl", root+"unrecorded/x.jsonl", root+"other-2/y.jsonl", "file-abc")
	require.NoError(t, plugin.filterProviderObjectList(ctx, "bedrock", list))
	assert.Equal(t, []string{root + "other/mine.pdf", root + "mine/job-2/out.jsonl", root + "unrecorded/x.jsonl", root + "other-2/y.jsonl", "file-abc"}, shown(list))

	untouched := page(root+"mine/job-2/out.jsonl", root+"unrecorded/x.jsonl")
	require.NoError(t, plugin.filterProviderObjectList(ctx, "bedrock", untouched))
	assert.Len(t, shown(untouched), 2, "a page with nothing of another key's is left as it is")
}

// Nothing is claimed for a request with no virtual key, with ownership enforcement off, or
// with no config store.
func TestOutputFolderIsNotClaimedWithoutEnforcement(t *testing.T) {
	s := storageFolderSurfaces{provider: schemas.Vertex, root: "gs://quiet-bucket/"}
	folder := s.root + "out/"

	plugin, store := newStorageFolderTestPlugin(t)
	_, shortCircuit, err := plugin.PreLLMHook(emptyCtx(), s.batchInto(folder))
	require.NoError(t, err)
	require.Nil(t, shortCircuit)
	assert.Empty(t, storageFolderOwner(t, store, folder), "no virtual key")

	plugin.objectOwnershipEnforcedFn = func() bool { return false }
	_, shortCircuit, err = plugin.PreLLMHook(presentCtx(mcpTestVKValue), s.batchInto(folder))
	require.NoError(t, err)
	require.Nil(t, shortCircuit)
	assert.Empty(t, storageFolderOwner(t, store, folder), "enforcement off")

	vk := buildVKForMCPStamping(nil)
	vk.ProviderConfigs = append(vk.ProviderConfigs, buildProviderConfig("vertex", []string{"*"}))
	bare := newAccessTestPlugin(t, vk, nil)
	_, shortCircuit, err = bare.PreLLMHook(presentCtx(mcpTestVKValue), s.batchInto(folder))
	require.NoError(t, err)
	assert.Nil(t, shortCircuit, "a batch create with no config store is left open, as before")
}

// A request naming more stored objects than the bound is refused, and what it gathered on the
// way, the annotations included, stays within the bound.
func TestStorageAnnotationsAreBoundedWithTheReferences(t *testing.T) {
	extra := map[string]any{}
	var many []any
	for i := 0; i < maxFileReferences*4; i++ {
		many = append(many, "gs://b/in/"+strings.Repeat("x", i%7)+string(rune('a'+i%26))+strings.Repeat("y", i))
		extra["storageUri"+strings.Repeat("_", i)] = "ignored"
	}
	extra["inputs"] = many
	req := &schemas.BifrostRequest{RequestType: schemas.ImageGenerationRequest, ImageGenerationRequest: &schemas.BifrostImageGenerationRequest{Provider: schemas.Vertex, Model: "m",
		Input: &schemas.ImageGenerationInput{Prompt: "a cat"}, Params: &schemas.ImageGenerationParameters{ExtraParams: extra}}}
	ids, locations, open, unreadable := requestFileReferences(req)
	assert.True(t, unreadable)
	assert.Len(t, ids, maxFileReferences)
	assert.LessOrEqual(t, len(open), maxFileReferences)
	assert.LessOrEqual(t, len(locations), maxFileReferences)
}

// A claim outlives the request that made it, also a request the provider then refuses:
// another request of the key may already be writing into the folder, and withdrawing the claim
// under it would leave what it writes unclaimed.
func TestOutputFolderClaimStaysWhenTheRequestFails(t *testing.T) {
	plugin, store := newStorageFolderTestPlugin(t)
	s := storageFolderSurfaces{provider: schemas.Bedrock, root: "s3://kept-bucket/"}
	folder := s.root + "out/"
	ctx := presentCtx(mcpTestVKValue)
	_, shortCircuit, err := plugin.PreLLMHook(ctx, s.batchInto(folder))
	require.NoError(t, err)
	require.Nil(t, shortCircuit)
	_, _, err = plugin.PostLLMHook(ctx, nil, &schemas.BifrostError{StatusCode: schemas.Ptr(400), Error: &schemas.ErrorField{Message: "role cannot write to the bucket"}})
	require.NoError(t, err)
	assert.Equal(t, "vk-mcp-stamp", storageFolderOwner(t, store, folder))
}

// A folder has one row and the first writer keeps it. Two keys that claimed nested folders in
// the same instant both hold a claim; the closest one decides, so neither reaches into the
// other's folder.
func TestConcurrentFolderClaims(t *testing.T) {
	folderKind := configstoreTables.ProviderObjectKindStorageFolder
	plugin, store := newStorageFolderTestPlugin(t)
	s := storageFolderSurfaces{provider: schemas.Vertex, root: "gs://race-bucket/"}
	status := func(req *schemas.BifrostRequest) int {
		t.Helper()
		_, shortCircuit, err := plugin.PreLLMHook(presentCtx(mcpTestVKValue), req)
		require.NoError(t, err)
		if shortCircuit == nil {
			return 200
		}
		return *shortCircuit.Error.StatusCode
	}

	// The other key's row for the same folder landed between this key's check and its write.
	seedProviderObject(t, store, folderKind, "gcs", s.root+"same/", "vk-someone-else")
	shortCircuit := plugin.claimStorageFolders(presentCtx(mcpTestVKValue), []string{s.root + "same/"}, "vk-mcp-stamp")
	require.NotNil(t, shortCircuit)
	assert.Equal(t, 404, *shortCircuit.Error.StatusCode)
	assert.Equal(t, "vk-someone-else", storageFolderOwner(t, store, s.root+"same/"), "the first writer keeps the folder")

	// Nested claims that both landed: this key holds out/, the other key out/run-2/.
	require.Nil(t, plugin.claimStorageFolders(presentCtx(mcpTestVKValue), []string{s.root + "out/"}, "vk-mcp-stamp"))
	seedProviderObject(t, store, folderKind, "gcs", s.root+"out/run-2/", "vk-someone-else")
	assert.Equal(t, 200, status(s.chatFile(s.root+"out/run-1/predictions.jsonl")), "the key reads its own folder")
	assert.Equal(t, 404, status(s.chatFile(s.root+"out/run-2/predictions.jsonl")), "but not the folder the other key holds inside it")
	assert.Equal(t, 404, status(s.batchInto(s.root+"out/run-2/again/")), "nor does it write there")
	assert.Equal(t, 200, status(s.batchInto(s.root+"out/")), "and it keeps writing into its own")
}

// A storage name in untyped data (an extra param, a passthrough body, an inline batch item)
// may be an input or an output location; nothing says which. It is checked both ways and
// claims nothing. The extra params known to be output locations are claimed like the typed ones.
func TestStorageNamedInUntypedDataIsCheckedAsAFolderToo(t *testing.T) {
	plugin, store := newStorageFolderTestPlugin(t)
	root := "gs://open-bucket/"
	folderKind, fileKind := configstoreTables.ProviderObjectKindStorageFolder, configstoreTables.ProviderObjectKindFile
	seedProviderObject(t, store, folderKind, "gcs", root+"other/out/", "vk-someone-else")
	seedProviderObject(t, store, fileKind, "vertex", root+"shared/uploads/other.pdf", "vk-someone-else")
	status := func(req *schemas.BifrostRequest) int {
		t.Helper()
		_, shortCircuit, err := plugin.PreLLMHook(presentCtx(mcpTestVKValue), req)
		require.NoError(t, err)
		if shortCircuit == nil {
			return 200
		}
		return *shortCircuit.Error.StatusCode
	}
	image := func(extra map[string]any) *schemas.BifrostRequest {
		return &schemas.BifrostRequest{RequestType: schemas.ImageGenerationRequest, ImageGenerationRequest: &schemas.BifrostImageGenerationRequest{Provider: schemas.Vertex, Model: "m",
			Input: &schemas.ImageGenerationInput{Prompt: "a cat"}, Params: &schemas.ImageGenerationParameters{ExtraParams: extra}}}
	}
	inline := func(body map[string]any) *schemas.BifrostRequest {
		return &schemas.BifrostRequest{RequestType: schemas.BatchCreateRequest, BatchCreateRequest: &schemas.BifrostBatchCreateRequest{Provider: schemas.Vertex, Model: schemas.Ptr("m"),
			Requests: []schemas.BatchRequestItem{{CustomID: "1", Body: body}}}}
	}

	// Written without its slash, the other key's folder is no object in it and no folder above
	// it; as the place a provider writes beneath, it is that folder.
	for name, req := range map[string]*schemas.BifrostRequest{
		"an extra param of unknown meaning":   image(map[string]any{"someVendorOutput": root + "other/out"}),
		"nested in an extra param":            image(map[string]any{"config": map[string]any{"target": []any{root + "other/out"}}}),
		"a known output extra param":          image(map[string]any{"storageUri": root + "other/out"}),
		"an inline batch item":                inline(map[string]any{"outputConfig": map[string]any{"gcsDestination": map[string]any{"outputUriPrefix": root + "other/out"}}}),
		"a folder holding another key's file": image(map[string]any{"someVendorOutput": root + "shared/"}),
	} {
		assert.Equal(t, 404, status(req), name)
	}

	assert.Equal(t, 200, status(image(map[string]any{"gcsUri": root + "free/in.png"})), "a name with nothing of another key's at, above or beneath it")
	assert.Empty(t, storageFolderOwner(t, store, root+"free/in.png/"), "a name in untyped data claims nothing")
	assert.Equal(t, 200, status(image(map[string]any{"storageUri": root + "mine/images"})), "a known output extra param on a free location")
	assert.Equal(t, "vk-mcp-stamp", storageFolderOwner(t, store, root+"mine/images/"), "is claimed like a typed output location")
}

// BenchmarkStorageFolderCheck measures what the folder rule adds to a request that names a
// stored object: one lookup of the folders above it for an input, and for an output location
// the claim on first use or the same lookup once the key holds the folder.
func BenchmarkStorageFolderCheck(b *testing.B) {
	t := &testing.T{}
	plugin, store := newStorageFolderTestPlugin(t)
	s := storageFolderSurfaces{provider: schemas.Vertex, root: "gs://bench-bucket/"}
	seedProviderObject(t, store, configstoreTables.ProviderObjectKindStorageFolder, "gcs", s.root+"tenant/out/", "vk-mcp-stamp")
	cases := map[string]*schemas.BifrostRequest{
		"chat, no stored object":          {RequestType: schemas.ChatCompletionRequest, ChatRequest: &schemas.BifrostChatRequest{Provider: schemas.Vertex, Model: "m", Input: []schemas.ChatMessage{{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: schemas.Ptr("hi")}}}}},
		"chat, stored input":              s.chatFile(s.root + "uploads/a/b/c/input.pdf"),
		"chat, input in the key's folder": s.chatFile(s.root + "tenant/out/job-1/predictions.jsonl"),
		"batch, output folder held":       s.batchInto(s.root + "tenant/out/"),
	}
	for name, req := range cases {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, shortCircuit, err := plugin.PreLLMHook(presentCtx(mcpTestVKValue), req); err != nil || shortCircuit != nil {
					b.Fatalf("refused: %v %v", err, shortCircuit)
				}
			}
		})
	}
}
