package configstore

import (
	"context"
	"strings"
	"testing"

	"github.com/capsohq/bifrost/framework/configstore/tables"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TXE-7259: the output directories of batches recorded before folders were claimed at create
// time are claimed for their batch's key, only where the folder rule would have let that key
// claim them, and a second run changes nothing.
func TestBackfillBatchOutputFolders(t *testing.T) {
	for name, store := range storagePrefixTestStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			n := 0
			job := func(output, vk string) {
				n++
				id := "job-" + strings.Repeat("x", n)
				row := &tables.TableProviderJob{ID: tables.ProviderJobID(tables.ProviderJobKindBatch, "vertex", id), Kind: tables.ProviderJobKindBatch,
					Provider: "vertex", JobID: id, AccountingStatus: tables.ProviderJobAccountingStatusPending}
				if output != "" {
					row.OutputFileID = &output
				}
				if vk != "" {
					row.VirtualKeyID = &vk
				}
				require.NoError(t, store.UpsertProviderJob(ctx, row))
			}
			object := func(kind, objectID, vk string) {
				require.NoError(t, store.UpsertProviderObject(ctx, &tables.TableProviderObject{
					ID: tables.ProviderObjectID(kind, tables.StorageFolderProvider(objectID), objectID), Kind: kind,
					Provider: tables.StorageFolderProvider(objectID), ObjectID: objectID, VirtualKeyID: vk}))
			}
			job("gs://b/a/prediction-1", "vk-a")  // claimed
			job("gs://b/a/prediction-1/", "vk-a") // the same folder
			job("gs://b/a2/", "vk-a")             // claimed; a2 is a sibling of a, not inside it
			job("gs://b/a2/inner/", "vk-a")       // covered by a2
			job("s3://b/shared/", "vk-a")         // named by two keys
			job("s3://b/shared/", "vk-b")         //
			job("s3://b/outer/", "vk-a")          // another key's directory is inside it
			job("s3://b/outer/b-job/", "vk-b")    //
			job("gs://b/", "vk-a")                // a bucket root
			job("gs://b/mine/p/", "vk-a")         // a folder of the key's is above it already
			job("gs://b/theirs/p/", "vk-a")       // another key's folder is above it
			job("gs://b/upload-under/", "vk-a")   // another key uploaded a file inside it
			job("gs://b/nobody/", "")             // no key
			job("file-openai-output", "vk-a")     // not object storage
			object(tables.ProviderObjectKindStorageFolder, "gs://b/mine/", "vk-a")
			object(tables.ProviderObjectKindStorageFolder, "gs://b/theirs/", "vk-b")
			object(tables.ProviderObjectKindFile, "gs://b/upload-under/f.jsonl", "vk-b")

			claimed, err := backfillBatchOutputFolders(ctx, store.DB())
			require.NoError(t, err)
			assert.Equal(t, 2, claimed)
			rows, _, err := store.ListProviderObjects(ctx, ProviderObjectQuery{Kind: tables.ProviderObjectKindStorageFolder})
			require.NoError(t, err)
			got := map[string]string{}
			for _, row := range rows {
				got[row.ObjectID] = row.VirtualKeyID
			}
			assert.Equal(t, map[string]string{
				"gs://b/a/prediction-1/": "vk-a",
				"gs://b/a2/":             "vk-a",
				"gs://b/mine/":           "vk-a",
				"gs://b/theirs/":         "vk-b",
			}, got)

			claimed, err = backfillBatchOutputFolders(ctx, store.DB())
			require.NoError(t, err)
			assert.Zero(t, claimed, "a second run finds every folder claimed or still not claimable")
		})
	}
}

func TestBatchOutputFolderOf(t *testing.T) {
	for location, want := range map[string]string{
		"gs://b/out/prediction-1": "gs://b/out/prediction-1/",
		"s3://b/out/":             "s3://b/out/",
		"s3://b/out//":            "s3://b/out//",
		"gs://b/":                 "",
		"gs://b":                  "",
		"gs:///x/":                "",
		"file-abc":                "",
		"gs://b/" + strings.Repeat("d", backfillFolderMaxLength):    "",
		"gs://b/" + strings.Repeat("d/", backfillFolderMaxLevels+1): "",
	} {
		got, ok := batchOutputFolderOf(location)
		assert.Equal(t, want != "", ok, location)
		assert.Equal(t, want, got, location)
	}
}
