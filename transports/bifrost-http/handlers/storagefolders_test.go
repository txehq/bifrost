package handlers

import (
	"context"
	"encoding/json"
	"net/url"
	"testing"

	"github.com/capsohq/bifrost/core/schemas"
	configstoreTables "github.com/capsohq/bifrost/framework/configstore/tables"
	"github.com/capsohq/bifrost/framework/queryscope"
	"github.com/fasthttp/router"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"gorm.io/gorm"
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

// TXE-7259: in Enterprise a caller with a row scope (DAC) lists and releases only the claims of
// the virtual keys its scope shows it. Another key's claim, and a deleted key's, is neither
// listed nor released: it reads as not found, and the row stays.
func TestStorageFolderClaimsStayWithinTheCallersScope(t *testing.T) {
	SetLogger(&mockLogger{})
	store := newScaleTestStore(t)
	ctx := context.Background()
	active := true
	for _, id := range []string{"vk-a", "vk-b"} {
		require.NoError(t, store.CreateVirtualKey(ctx, &configstoreTables.TableVirtualKey{ID: id, Name: id, Value: *schemas.NewSecretVar("sk-bf-" + id), IsActive: &active}))
	}
	folder := configstoreTables.ProviderObjectKindStorageFolder
	for objectID, vk := range map[string]string{"gs://b/a/": "vk-a", "gs://b/b/": "vk-b", "gs://b/gone/": "vk-deleted"} {
		require.NoError(t, store.UpsertProviderObject(ctx, &configstoreTables.TableProviderObject{
			ID: configstoreTables.ProviderObjectID(folder, "gcs", objectID), Kind: folder, Provider: "gcs", ObjectID: objectID, VirtualKeyID: vk,
		}))
	}
	onlyA := queryscope.QueryScope(func(db *gorm.DB) *gorm.DB { return db.Where("governance_virtual_keys.id = ?", "vk-a") })

	r := router.New()
	(&GovernanceHandler{configStore: store}).RegisterRoutes(r)
	call := func(method, query string, scope queryscope.QueryScope) (int, map[string]any) {
		t.Helper()
		var req fasthttp.Request
		req.Header.SetMethod(method)
		req.SetRequestURI("/api/governance/storage-folders?" + query)
		var rc fasthttp.RequestCtx
		rc.Init(&req, nil, nil)
		if scope != nil {
			rc.SetUserValue(schemas.BifrostContextKeyQueryScope, scope)
		}
		r.Handler(&rc)
		var body map[string]any
		require.NoError(t, json.Unmarshal(rc.Response.Body(), &body), string(rc.Response.Body()))
		return rc.Response.StatusCode(), body
	}

	status, body := call(fasthttp.MethodGet, "", onlyA)
	require.Equal(t, 200, status)
	assert.EqualValues(t, 1, body["total_count"])
	assert.Equal(t, "gs://b/a/", body["storage_folders"].([]any)[0].(map[string]any)["folder"])
	status, body = call(fasthttp.MethodGet, "virtual_key_id=vk-b", onlyA)
	require.Equal(t, 200, status)
	assert.EqualValues(t, 0, body["total_count"], "a filter on another key does not widen the scope")

	for _, other := range []string{"gs://b/b/", "gs://b/gone/"} {
		status, _ = call(fasthttp.MethodDelete, "folder="+url.QueryEscape(other), onlyA)
		assert.Equal(t, 404, status, other)
		rows, err := store.GetProviderObjectsByObjectID(ctx, folder, other)
		require.NoError(t, err)
		assert.Len(t, rows, 1, "the claim stays: %s", other)
	}
	status, body = call(fasthttp.MethodDelete, "folder="+url.QueryEscape("gs://b/a/"), onlyA)
	require.Equal(t, 200, status)
	assert.Equal(t, "vk-a", body["virtual_key_id"])

	status, body = call(fasthttp.MethodGet, "", nil)
	require.Equal(t, 200, status)
	assert.EqualValues(t, 2, body["total_count"], "an unscoped admin sees the rest, the deleted key's claim included")
}
