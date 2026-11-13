package configstore

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"testing"

	"github.com/capsohq/bifrost/framework/configstore/tables"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// setupProviderObjectTestStore extends the base test store with the provider_objects
// and batch_jobs tables through the same migration a live store runs.
func setupProviderObjectTestStore(t *testing.T) *RDBConfigStore {
	t.Helper()
	store := setupBatchJobTestStore(t)
	require.NoError(t, migrationAddProviderObjectsTable(context.Background(), store.DB(), testMigrationLogger))
	return store
}

func TestProviderObjectID(t *testing.T) {
	assert.Equal(t, "file:openai:file-abc", tables.ProviderObjectID(tables.ProviderObjectKindFile, "openai", "file-abc"))
	assert.NotEqual(t,
		tables.ProviderObjectID(tables.ProviderObjectKindVideo, "openai", "id_1"),
		tables.ProviderObjectID(tables.ProviderObjectKindFile, "openai", "id_1"),
		"the same provider id under two kinds is two objects")
}

// The first creator recorded for an object keeps it: a later write for the same id
// does not move the object to another key, and a delete forgets it.
func TestProviderObjectFirstCreatorWinsAndDeleteForgets(t *testing.T) {
	store := setupProviderObjectTestStore(t)
	ctx := context.Background()
	id := tables.ProviderObjectID(tables.ProviderObjectKindFile, "openai", "file-1")

	require.NoError(t, store.UpsertProviderObject(ctx, &tables.TableProviderObject{
		ID: id, Kind: tables.ProviderObjectKindFile, Provider: "openai", ObjectID: "file-1", VirtualKeyID: "vk-a",
	}))
	require.NoError(t, store.UpsertProviderObject(ctx, &tables.TableProviderObject{
		ID: id, Kind: tables.ProviderObjectKindFile, Provider: "openai", ObjectID: "file-1", VirtualKeyID: "vk-b",
	}))

	objects, err := store.GetProviderObjectsByIDs(ctx, []string{id, tables.ProviderObjectID(tables.ProviderObjectKindFile, "openai", "file-unknown")})
	require.NoError(t, err)
	require.Len(t, objects, 1, "ids with no row are absent, not reported")
	assert.Equal(t, "vk-a", objects[0].VirtualKeyID, "the first creator keeps the object")
	assert.False(t, objects[0].CreatedAt.IsZero())

	require.NoError(t, store.DeleteProviderObject(ctx, id))
	require.NoError(t, store.DeleteProviderObject(ctx, id), "deleting a forgotten object is not an error")
	objects, err = store.GetProviderObjectsByIDs(ctx, []string{id})
	require.NoError(t, err)
	assert.Empty(t, objects)

	empty, err := store.GetProviderObjectsByIDs(ctx, nil)
	require.NoError(t, err)
	assert.Nil(t, empty)
}

func TestUpsertProviderObjectRequiresIdentity(t *testing.T) {
	store := setupProviderObjectTestStore(t)
	ctx := context.Background()
	assert.Error(t, store.UpsertProviderObject(ctx, nil))
	assert.Error(t, store.UpsertProviderObject(ctx, &tables.TableProviderObject{ID: "x", Kind: "file", Provider: "openai", ObjectID: "f"}), "a creator is required")
	assert.Error(t, store.DeleteProviderObject(ctx, ""))
}

// A file is found through any of the three file columns of a batch row, and only on
// its own provider.
func TestGetProviderJobsByFileIDs(t *testing.T) {
	store := setupProviderObjectTestStore(t)
	ctx := context.Background()
	seed := func(provider, batchID, input string, output, errFile *string) {
		job := &tables.TableProviderJob{
			ID:               tables.ProviderJobID(tables.ProviderJobKindBatch, provider, batchID),
			Provider:         provider,
			JobID:            batchID,
			InputFileID:      input,
			OutputFileID:     output,
			ErrorFileID:      errFile,
			AccountingStatus: tables.ProviderJobAccountingStatusPending,
		}
		require.NoError(t, store.UpsertProviderJob(ctx, job))
	}
	out := "file-out"
	errFile := "file-err"
	seed("openai", "batch-1", "file-in", &out, &errFile)
	seed("azure", "batch-2", "file-in", nil, nil)

	ids := func(jobs []*tables.TableProviderJob) []string {
		got := make([]string, 0, len(jobs))
		for _, job := range jobs {
			got = append(got, job.JobID)
		}
		return got
	}
	for _, fileID := range []string{"file-in", "file-out", "file-err"} {
		jobs, err := store.GetProviderJobsByFileIDs(ctx, "openai", []string{fileID})
		require.NoError(t, err)
		assert.Equal(t, []string{"batch-1"}, ids(jobs), "file %s", fileID)
	}
	jobs, err := store.GetProviderJobsByFileIDs(ctx, "openai", []string{"file-in", "file-none"})
	require.NoError(t, err)
	assert.Equal(t, []string{"batch-1"}, ids(jobs))

	jobs, err = store.GetProviderJobsByFileIDs(ctx, "openai", []string{"file-none"})
	require.NoError(t, err)
	assert.Empty(t, jobs)

	jobs, err = store.GetProviderJobsByFileIDs(ctx, "", []string{"file-in"})
	require.NoError(t, err)
	assert.Nil(t, jobs, "no provider, no lookup")
}

// A list filter looks up every item of a provider page, under every provider name that shares the
// upstream, so one lookup can carry tens of thousands of ids: more than SQLite binds in one
// statement (32766) and more than Postgres accepts (65535). The lookups split the ids across
// queries, and a job that names files in two of those queries comes back once.
func TestProviderLookupsAcceptMoreIDsThanOneStatementBinds(t *testing.T) {
	store := setupProviderObjectTestStore(t)
	ctx := context.Background()
	const total = 40000
	ids := make([]string, total)
	fileIDs := make([]string, total)
	for i := range ids {
		ids[i] = fmt.Sprintf("file:openai:file-%05d", i)
		fileIDs[i] = fmt.Sprintf("file-%05d", i)
	}

	for _, i := range []int{0, 17000, total - 1} {
		require.NoError(t, store.UpsertProviderObject(ctx, &tables.TableProviderObject{
			ID: ids[i], Kind: tables.ProviderObjectKindFile, Provider: "openai", ObjectID: fileIDs[i], VirtualKeyID: "vk-a",
		}))
	}
	objects, err := store.GetProviderObjectsByIDs(ctx, ids)
	require.NoError(t, err)
	got := make([]string, 0, len(objects))
	for _, object := range objects {
		got = append(got, object.ObjectID)
	}
	sort.Strings(got)
	assert.Equal(t, []string{fileIDs[0], fileIDs[17000], fileIDs[total-1]}, got)

	// The batch names its input file early in the list and its output file late, so the two
	// matches fall in different queries.
	output := fileIDs[total-1]
	batch := &tables.TableProviderJob{
		ID: tables.ProviderJobID(tables.ProviderJobKindBatch, "openai", "batch-wide"), Kind: tables.ProviderJobKindBatch,
		Provider: "openai", JobID: "batch-wide", InputFileID: fileIDs[1], OutputFileID: &output,
		AccountingStatus: tables.ProviderJobAccountingStatusPending,
	}
	require.NoError(t, store.UpsertProviderJob(ctx, batch))
	jobs, err := store.GetProviderJobsByFileIDs(ctx, "openai", fileIDs)
	require.NoError(t, err)
	require.Len(t, jobs, 1, "the batch is returned once though two queries matched it")
	assert.Equal(t, "batch-wide", jobs[0].JobID)

	jobIDs := make([]string, total)
	for i := range jobIDs {
		jobIDs[i] = tables.ProviderJobID(tables.ProviderJobKindBatch, "openai", fmt.Sprintf("batch-%05d", i))
	}
	jobIDs[total-1] = batch.ID
	jobs, err = store.GetProviderJobsByIDs(ctx, jobIDs)
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	assert.Equal(t, "batch-wide", jobs[0].JobID)
}

// A delete that names a full resource name by its last segment forgets the row the full name
// is recorded under: whole, at a path boundary, with LIKE metacharacters literal, among the
// named kind and provider only.
func TestDeleteProviderObjectsByObjectIDSuffixForgetsTheFullName(t *testing.T) {
	store := setupProviderObjectTestStore(t)
	ctx := context.Background()
	seed := func(kind, provider, objectID string) string {
		id := tables.ProviderObjectID(kind, provider, objectID)
		require.NoError(t, store.UpsertProviderObject(ctx, &tables.TableProviderObject{
			ID: id, Kind: kind, Provider: provider, ObjectID: objectID, VirtualKeyID: "vk-a",
		}))
		return id
	}
	gone := seed(tables.ProviderObjectKindBatch, "vertex", "projects/p/locations/l/batchPredictionJobs/123")
	kept := []string{
		seed(tables.ProviderObjectKindBatch, "vertex", "projects/p/locations/l/batchPredictionJobs/9123"),
		seed(tables.ProviderObjectKindBatch, "vertex", "projects/p/locations/l/batchPredictionJobs/1_3"),
		seed(tables.ProviderObjectKindBatch, "vertex", "123"),
		seed(tables.ProviderObjectKindCachedContent, "vertex", "projects/p/locations/l/cachedContents/123"),
		seed(tables.ProviderObjectKindBatch, "gemini", "batches/123"),
	}
	// Another key's job with the same number in another project is not the deleter's to forget.
	otherKeys := tables.ProviderObjectID(tables.ProviderObjectKindBatch, "vertex", "projects/q/locations/l/batchPredictionJobs/123")
	require.NoError(t, store.UpsertProviderObject(ctx, &tables.TableProviderObject{
		ID: otherKeys, Kind: tables.ProviderObjectKindBatch, Provider: "vertex", ObjectID: "projects/q/locations/l/batchPredictionJobs/123", VirtualKeyID: "vk-b",
	}))
	kept = append(kept, otherKeys)

	require.NoError(t, store.DeleteProviderObjectsByObjectIDSuffix(ctx, tables.ProviderObjectKindBatch, "vertex", "123", "vk-a"))

	objects, err := store.GetProviderObjectsByIDs(ctx, append([]string{gone}, kept...))
	require.NoError(t, err)
	ids := make([]string, 0, len(objects))
	for _, object := range objects {
		ids = append(ids, object.ID)
	}
	assert.ElementsMatch(t, kept, ids, "only vk-a's vertex batch whose name ends in /123 is forgotten")

	assert.Error(t, store.DeleteProviderObjectsByObjectIDSuffix(ctx, tables.ProviderObjectKindBatch, "vertex", "", "vk-a"), "an empty suffix would forget every full name")
	assert.Error(t, store.DeleteProviderObjectsByObjectIDSuffix(ctx, tables.ProviderObjectKindBatch, "vertex", "123", ""), "an empty virtual key would forget every key's full name")
}

// A full resource name is also addressed by its last segment. The suffix lookup finds the
// ownership rows by that segment alone, whole and at a path boundary, with LIKE metacharacters
// taken literally, among the named kind and provider only, for several suffixes at once.
func TestGetProviderObjectsByObjectIDSuffixesMatchTheLastSegmentWhole(t *testing.T) {
	store := setupProviderObjectTestStore(t)
	ctx := context.Background()
	seed := func(kind, provider, objectID, vk string) {
		require.NoError(t, store.UpsertProviderObject(ctx, &tables.TableProviderObject{
			ID: tables.ProviderObjectID(kind, provider, objectID), Kind: kind, Provider: provider, ObjectID: objectID, VirtualKeyID: vk,
		}))
	}
	seed(tables.ProviderObjectKindCachedContent, "vertex", "projects/p/locations/l/cachedContents/123", "vk-a")
	seed(tables.ProviderObjectKindCachedContent, "vertex", "projects/q/locations/l/cachedContents/123", "vk-b")
	seed(tables.ProviderObjectKindCachedContent, "vertex", "projects/p/locations/l/cachedContents/9123", "vk-a")
	seed(tables.ProviderObjectKindCachedContent, "vertex", "projects/p/locations/l/cachedContents/1_3", "vk-a")
	seed(tables.ProviderObjectKindCachedContent, "vertex", "123", "vk-a")
	seed(tables.ProviderObjectKindBatch, "vertex", "projects/p/locations/l/batchPredictionJobs/123", "vk-a")
	seed(tables.ProviderObjectKindCachedContent, "gemini", "cachedContents/123", "vk-a")

	objects, err := store.GetProviderObjectsByObjectIDSuffixes(ctx, tables.ProviderObjectKindCachedContent, "vertex", []string{"123", "1_3", ""})
	require.NoError(t, err)
	ids := make([]string, 0, len(objects))
	for _, object := range objects {
		ids = append(ids, object.ObjectID)
	}
	assert.ElementsMatch(t, []string{
		"projects/p/locations/l/cachedContents/123",
		"projects/q/locations/l/cachedContents/123",
		"projects/p/locations/l/cachedContents/1_3",
	}, ids, "9123 is not 123, a bare 123 is not a resource name, the underscore is literal, and other kinds and providers are not consulted")

	objects, err = store.GetProviderObjectsByObjectIDSuffixes(ctx, tables.ProviderObjectKindCachedContent, "vertex", nil)
	require.NoError(t, err)
	assert.Nil(t, objects, "no suffixes match nothing")
}

// A provider object is found by its kind and provider-side id under every provider name it was
// recorded under, including one no longer configured, and only rows of that kind and id are read.
func TestGetProviderObjectsByObjectIDSpansProviders(t *testing.T) {
	store := setupProviderObjectTestStore(t)
	ctx := context.Background()
	seed := func(kind, provider, objectID, vk string) {
		require.NoError(t, store.UpsertProviderObject(ctx, &tables.TableProviderObject{
			ID: tables.ProviderObjectID(kind, provider, objectID), Kind: kind, Provider: provider, ObjectID: objectID, VirtualKeyID: vk,
		}))
	}
	seed(tables.ProviderObjectKindLiveSession, "custom-live", "live_1", "vk-a")
	seed(tables.ProviderObjectKindLiveSession, "openai", "live_1", "vk-a")
	seed(tables.ProviderObjectKindLiveSession, "openai", "live_2", "vk-b")
	seed(tables.ProviderObjectKindFile, "openai", "live_1", "vk-c")

	objects, err := store.GetProviderObjectsByObjectID(ctx, tables.ProviderObjectKindLiveSession, "live_1")
	require.NoError(t, err)
	providers := make([]string, 0, len(objects))
	for _, object := range objects {
		assert.Equal(t, "live_1", object.ObjectID)
		assert.Equal(t, tables.ProviderObjectKindLiveSession, object.Kind)
		providers = append(providers, object.Provider)
	}
	assert.ElementsMatch(t, []string{"custom-live", "openai"}, providers, "every provider name the session was recorded under, and no other kind")

	objects, err = store.GetProviderObjectsByObjectID(ctx, tables.ProviderObjectKindLiveSession, "")
	require.NoError(t, err)
	assert.Empty(t, objects, "an empty id matches nothing")
}

// The ownership check on a live session's control routes looks rows up by kind and object id
// across providers; an index on that pair keeps it off a scan of every recorded session. The
// rollback drops only that index.
func TestProviderObjectsObjectIDIndexMigration(t *testing.T) {
	store := setupProviderObjectTestStore(t)
	db := store.DB()
	require.NoError(t, migrationAddProviderObjectsObjectIDIndex(context.Background(), db, testMigrationLogger))
	require.True(t, indexExists(t, db, "provider_objects", "idx_provider_objects_kind_object_id"), "the (kind, object_id) index is created")
	require.NoError(t, migrationAddProviderObjectsObjectIDIndex(context.Background(), db, testMigrationLogger), "running it again is a no-op")

	require.NoError(t, providerObjectsObjectIDIndexMigration(context.Background(), "add_provider_objects_object_id_index").Rollback(db))
	require.False(t, indexExists(t, db, "provider_objects", "idx_provider_objects_kind_object_id"), "the rollback drops the index")
	require.True(t, db.Migrator().HasTable("provider_objects"), "and keeps the table")
}

// BenchmarkGetProviderObjectsByIDs measures the ownership lookup of a typical list page
// (20 ids, half of them recorded) on SQLite, the common case the id-list binding must not
// slow down.
func BenchmarkGetProviderObjectsByIDs(b *testing.B) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(b.TempDir(), "bench.db")), inListGormConfig())
	require.NoError(b, err)
	require.NoError(b, db.AutoMigrate(&tables.TableProviderObject{}))
	store := newInListTestStore(db)
	ctx := context.Background()
	ids := make([]string, 0, 20)
	for i := range 20 {
		objectID := fmt.Sprintf("file-%02d", i)
		id := tables.ProviderObjectID(tables.ProviderObjectKindFile, "openai", objectID)
		ids = append(ids, id)
		if i%2 == 0 {
			require.NoError(b, store.UpsertProviderObject(ctx, &tables.TableProviderObject{
				ID: id, Kind: tables.ProviderObjectKindFile, Provider: "openai", ObjectID: objectID, VirtualKeyID: "vk-a",
			}))
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		objects, err := store.GetProviderObjectsByIDs(ctx, ids)
		if err != nil || len(objects) != 10 {
			b.Fatalf("lookup: %v, %d rows", err, len(objects))
		}
	}
}

// storagePrefixTestStores returns a store on SQLite and, when the local test Postgres is
// reachable, one on Postgres with the provider_objects and batch_jobs tables and the prefix
// indexes in place. The prefix lookups compare differently per dialect, so both are read.
func storagePrefixTestStores(t *testing.T) map[string]*RDBConfigStore {
	t.Helper()
	stores := map[string]*RDBConfigStore{"sqlite": setupProviderObjectTestStore(t)}
	for _, ndb := range indexMigrationDBs(t) {
		if ndb.name != "postgres" {
			continue
		}
		require.NoError(t, ndb.db.AutoMigrate(&tables.TableProviderJob{}))
		require.NoError(t, migrationAddProviderObjectsTable(context.Background(), ndb.db, testMigrationLogger))
		store := &RDBConfigStore{logger: nil}
		store.db.Store(ndb.db)
		stores["postgres"] = store
	}
	for _, store := range stores {
		require.NoError(t, migrationAddStoragePrefixIndexes(context.Background(), store.DB(), testMigrationLogger))
	}
	return stores
}

// A folder check asks whether another key has anything recorded under a prefix. The match
// is byte for byte: a sibling whose name only starts the same way, a name differing in case,
// LIKE metacharacters and a doubled slash are all literal, and the caller's own rows and rows
// of other kinds do not count.
func TestHasForeignProviderObjectUnderPrefix(t *testing.T) {
	for name, store := range storagePrefixTestStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			file, folder := tables.ProviderObjectKindFile, tables.ProviderObjectKindStorageFolder
			seed := func(kind, objectID, vk string) {
				require.NoError(t, store.UpsertProviderObject(ctx, &tables.TableProviderObject{
					ID: tables.ProviderObjectID(kind, "vertex", objectID), Kind: kind, Provider: "vertex", ObjectID: objectID, VirtualKeyID: vk,
				}))
			}
			seed(file, "gs://b/out/a/result.jsonl", "vk-a")
			seed(folder, "gs://b/claimed/sub/", "vk-a")
			seed(file, "gs://b/outer.jsonl", "vk-a")
			seed(file, "gs://b/Mixed/x", "vk-a")
			seed(file, "gs://b/pct/100%_done", "vk-a")
			seed(file, "gs://b/dbl//x", "vk-a")
			seed(tables.ProviderObjectKindBatch, "gs://b/batchkind/x", "vk-a")
			seed(file, "gs://b/mine/x", "vk-b")

			kinds := []string{file, folder}
			cases := []struct {
				prefix string
				want   bool
				why    string
			}{
				{"gs://b/out/", true, "a file two levels down"},
				{"gs://b/out/a/", true, "a file directly inside"},
				{"gs://b/claimed/", true, "a folder record beneath"},
				{"gs://b/claimed/sub/", true, "a folder record at the prefix itself"},
				{"gs://b/", true, "the bucket holds another key's records"},
				{"gs://b/oute/", false, "a sibling that only starts the same way"},
				{"gs://b/outer.jsonl/", false, "a file is not a folder of that name"},
				{"gs://b/mixed/", false, "case is literal"},
				{"gs://b/pct/100__done", false, "an underscore is not a wildcard"},
				{"gs://b/pct/1%", false, "a percent sign is not a wildcard"},
				{"gs://b/dbl/", true, "a doubled slash is under the single one"},
				{"gs://b/dbl//", true, "and under itself"},
				{"gs://b/dbl///", false, "but not under a tripled one"},
				{"gs://b/batchkind/", false, "other kinds are not read"},
				{"gs://b/mine/", false, "the caller's own rows do not count"},
				{"gs://other/", false, "another bucket"},
			}
			for _, c := range cases {
				got, err := store.HasForeignProviderObjectUnderPrefix(ctx, kinds, c.prefix, "vk-b")
				require.NoError(t, err, c.prefix)
				assert.Equal(t, c.want, got, "%s: %s", c.prefix, c.why)
			}
			_, err := store.HasForeignProviderObjectUnderPrefix(ctx, kinds, "", "vk-b")
			require.Error(t, err, "an empty prefix would match every row")
			_, err = store.HasForeignProviderObjectUnderPrefix(ctx, kinds, "gs://b/", "")
			require.Error(t, err)
		})
	}
}

// The output and error files of another key's batch count as records under a prefix as soon
// as the job row names them; a job with no virtual key is nobody's.
func TestHasForeignProviderJobFileUnderPrefix(t *testing.T) {
	for name, store := range storagePrefixTestStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			seed := func(batchID string, output, errFile, vk *string) {
				require.NoError(t, store.UpsertProviderJob(ctx, &tables.TableProviderJob{
					ID: tables.ProviderJobID(tables.ProviderJobKindBatch, "bedrock", batchID), Provider: "bedrock", JobID: batchID,
					OutputFileID: output, ErrorFileID: errFile, VirtualKeyID: vk, AccountingStatus: tables.ProviderJobAccountingStatusPending,
				}))
			}
			str := func(s string) *string { return &s }
			seed("job-a", str("s3://b/out-a/job-a/out.jsonl"), nil, str("vk-a"))
			seed("job-a2", nil, str("s3://b/err-a/job-a2/err.jsonl"), str("vk-a"))
			seed("job-none", str("s3://b/nobody/out.jsonl"), nil, nil)
			seed("job-b", str("s3://b/out-b/job-b/out.jsonl"), nil, str("vk-b"))

			for prefix, want := range map[string]bool{
				"s3://b/out-a/":  true,
				"s3://b/err-a/":  true,
				"s3://b/":        true,
				"s3://b/out-/":   false,
				"s3://b/nobody/": false,
				"s3://b/out-b/":  false,
			} {
				got, err := store.HasForeignProviderJobFileUnderPrefix(ctx, prefix, "vk-b")
				require.NoError(t, err, prefix)
				assert.Equal(t, want, got, prefix)
			}
		})
	}
}

// A claim is made before the work it covers, so the caller has to know whether the row is
// the one it just wrote; the folders above an object are read in one lookup.
func TestInsertProviderObjectReportsWhoWroteTheRow(t *testing.T) {
	store := setupProviderObjectTestStore(t)
	ctx := context.Background()
	folder := tables.ProviderObjectKindStorageFolder
	row := func(provider, objectID, vk string) *tables.TableProviderObject {
		return &tables.TableProviderObject{ID: tables.ProviderObjectID(folder, provider, objectID), Kind: folder, Provider: provider, ObjectID: objectID, VirtualKeyID: vk}
	}
	inserted, err := store.InsertProviderObject(ctx, row("vertex", "gs://b/out/", "vk-a"))
	require.NoError(t, err)
	assert.True(t, inserted)
	inserted, err = store.InsertProviderObject(ctx, row("vertex", "gs://b/out/", "vk-b"))
	require.NoError(t, err)
	assert.False(t, inserted, "the row was already there")
	_, err = store.InsertProviderObject(ctx, &tables.TableProviderObject{ID: "x"})
	require.Error(t, err)

	inserted, err = store.InsertProviderObject(ctx, row("vertex-eu", "gs://b/", "vk-c"))
	require.NoError(t, err)
	require.True(t, inserted)
	objects, err := store.GetProviderObjectsByObjectIDs(ctx, folder, []string{"gs://b/", "gs://b/out/", "gs://b/out/deeper/"})
	require.NoError(t, err)
	owners := map[string]string{}
	for _, object := range objects {
		owners[object.ObjectID] = object.VirtualKeyID
	}
	assert.Equal(t, map[string]string{"gs://b/": "vk-c", "gs://b/out/": "vk-a"}, owners, "rows under any provider name, first writer kept")
	objects, err = store.GetProviderObjectsByObjectIDs(ctx, tables.ProviderObjectKindFile, []string{"gs://b/"})
	require.NoError(t, err)
	assert.Empty(t, objects, "the kind is part of the lookup")
}

// On Postgres the prefix lookups need indexes in byte order; on SQLite the existing ones
// serve and nothing is built. The migration is safe to re-run and its rollback drops only
// what it built.
func TestStoragePrefixIndexesMigration(t *testing.T) {
	for name, store := range storagePrefixTestStores(t) {
		t.Run(name, func(t *testing.T) {
			db := store.DB()
			require.NoError(t, db.Exec("DELETE FROM migrations WHERE id = ?", "add_storage_prefix_indexes").Error)
			require.NoError(t, migrationAddStoragePrefixIndexes(context.Background(), db, testMigrationLogger), "running it again is a no-op")
			for _, index := range storagePrefixIndexes {
				assert.Equal(t, name == "postgres", indexExists(t, db, index.table, index.name), "%s on %s", index.name, name)
			}
			rollback := storagePrefixIndexesMigration(context.Background(), "add_storage_prefix_indexes").Rollback
			require.NoError(t, rollback(db))
			require.NoError(t, rollback(db), "rollback is safe to re-run")
			for _, index := range storagePrefixIndexes {
				assert.False(t, indexExists(t, db, index.table, index.name))
			}
			assert.True(t, db.Migrator().HasTable("provider_objects"))
		})
	}
}

// A page of one kind's objects comes back in byte order of the object id, filtered by key
// and by a literal prefix, with the count of every match.
func TestListProviderObjects(t *testing.T) {
	for name, store := range storagePrefixTestStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			folder := tables.ProviderObjectKindStorageFolder
			put := func(kind, objectID, vk string) {
				require.NoError(t, store.UpsertProviderObject(ctx, &tables.TableProviderObject{
					ID: tables.ProviderObjectID(kind, "gcs", objectID), Kind: kind, Provider: "gcs", ObjectID: objectID, VirtualKeyID: vk,
				}))
			}
			put(folder, "gs://b/out-b/", "vk-b")
			put(folder, "gs://b/Out-a/", "vk-a")
			put(folder, "gs://b/out-a/", "vk-a")
			put(folder, "gs://b/out_a/", "vk-a")
			put(folder, "gs://c/", "vk-a")
			put(tables.ProviderObjectKindFile, "gs://b/out-a/file.jsonl", "vk-a")

			ids := func(rows []*tables.TableProviderObject) []string {
				out := make([]string, 0, len(rows))
				for _, row := range rows {
					out = append(out, row.ObjectID)
				}
				return out
			}
			rows, total, err := store.ListProviderObjects(ctx, ProviderObjectQuery{Kind: folder})
			require.NoError(t, err)
			assert.EqualValues(t, 5, total)
			assert.Equal(t, []string{"gs://b/Out-a/", "gs://b/out-a/", "gs://b/out-b/", "gs://b/out_a/", "gs://c/"}, ids(rows), "byte order, folders only")

			rows, total, err = store.ListProviderObjects(ctx, ProviderObjectQuery{Kind: folder, VirtualKeyID: "vk-a", Prefix: "gs://b/out", Limit: 1, Offset: 1})
			require.NoError(t, err)
			assert.EqualValues(t, 2, total, "the prefix is literal: case and _ are not wildcards")
			assert.Equal(t, []string{"gs://b/out_a/"}, ids(rows))

			_, _, err = store.ListProviderObjects(ctx, ProviderObjectQuery{})
			require.Error(t, err)
		})
	}
}
