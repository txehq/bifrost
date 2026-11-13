package handlers

import (
	"context"
	"encoding/json"
	"net/url"
	"testing"

	configstoreTables "github.com/capsohq/bifrost/framework/configstore/tables"
	"github.com/fasthttp/router"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

// TXE-7259: an operator lists the folders virtual keys have claimed, filtered by key and by a
// literal URI prefix, and releases one by its exact URI. File rows are not folders and are
// neither listed nor released.
func TestStorageFolderClaimsListAndRelease(t *testing.T) {
	store := newScaleTestStore(t)
	ctx := context.Background()
	put := func(kind, provider, objectID, vk string) {
		require.NoError(t, store.UpsertProviderObject(ctx, &configstoreTables.TableProviderObject{
			ID: configstoreTables.ProviderObjectID(kind, provider, objectID), Kind: kind, Provider: provider, ObjectID: objectID, VirtualKeyID: vk,
		}))
	}
	folder := configstoreTables.ProviderObjectKindStorageFolder
	put(folder, "gcs", "gs://b/out-a/", "vk-a")
	put(folder, "gcs", "gs://b/out_a/", "vk-a")
	put(folder, "s3", "s3://b/out-b/", "vk-b")
	put(configstoreTables.ProviderObjectKindFile, "vertex", "gs://b/out-a/file.jsonl", "vk-a")

	SetLogger(&mockLogger{})
	r := router.New()
	(&GovernanceHandler{configStore: store}).RegisterRoutes(r)
	call := func(method, query string) (int, map[string]any) {
		t.Helper()
		var req fasthttp.Request
		req.Header.SetMethod(method)
		req.SetRequestURI("/api/governance/storage-folders?" + query)
		var rc fasthttp.RequestCtx
		rc.Init(&req, nil, nil)
		r.Handler(&rc)
		var body map[string]any
		require.NoError(t, json.Unmarshal(rc.Response.Body(), &body), string(rc.Response.Body()))
		return rc.Response.StatusCode(), body
	}
	folders := func(body map[string]any) []string {
		var out []string
		for _, item := range body["storage_folders"].([]any) {
			out = append(out, item.(map[string]any)["folder"].(string))
		}
		return out
	}

	status, body := call(fasthttp.MethodGet, "")
	require.Equal(t, 200, status)
	assert.Equal(t, []string{"gs://b/out-a/", "gs://b/out_a/", "s3://b/out-b/"}, folders(body))
	assert.EqualValues(t, 3, body["total_count"])

	status, body = call(fasthttp.MethodGet, "virtual_key_id=vk-a&prefix="+url.QueryEscape("gs://b/out-")+"&limit=10")
	require.Equal(t, 200, status)
	assert.Equal(t, []string{"gs://b/out-a/"}, folders(body), "the prefix is literal: _ is not a wildcard")

	status, _ = call(fasthttp.MethodGet, "limit=-1")
	assert.Equal(t, 400, status)

	status, _ = call(fasthttp.MethodDelete, "")
	assert.Equal(t, 400, status, "the folder is required")
	status, _ = call(fasthttp.MethodDelete, "folder="+url.QueryEscape("gs://b/out-a"))
	assert.Equal(t, 404, status, "the URI is exact: the claim ends in /")
	status, _ = call(fasthttp.MethodDelete, "folder="+url.QueryEscape("gs://b/out-a/file.jsonl"))
	assert.Equal(t, 404, status, "a file row is not a folder claim")

	status, body = call(fasthttp.MethodDelete, "folder="+url.QueryEscape("gs://b/out-a/"))
	require.Equal(t, 200, status)
	assert.Equal(t, "vk-a", body["virtual_key_id"])
	rows, err := store.GetProviderObjectsByObjectID(ctx, folder, "gs://b/out-a/")
	require.NoError(t, err)
	assert.Empty(t, rows, "the claim is gone")
	rows, err = store.GetProviderObjectsByObjectID(ctx, configstoreTables.ProviderObjectKindFile, "gs://b/out-a/file.jsonl")
	require.NoError(t, err)
	assert.Len(t, rows, 1, "the file row inside it stays")
}
