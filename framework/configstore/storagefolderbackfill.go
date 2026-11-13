package configstore

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/capsohq/bifrost/framework/configstore/tables"
	"github.com/capsohq/bifrost/framework/migrator"
	"gorm.io/gorm"
)

// Backfill of batch output folders (fork, TXE-7259).
//
// Since TXE-7121 a virtual key claims the output folder of a batch when it creates the batch.
// A batch created before that has no claim, and the results it wrote are nobody's: any key
// that learns their names reads them. The batch's output directory is on its accounting row
// (batch_jobs.output_file_id: Vertex's gcsOutputDirectory, Bedrock's output S3 URI), so this
// migration claims that directory for the batch's key, under the same rule a request follows:
//
//   - only a directory exactly one key's batches name, with no other key's batch directory
//     inside it or around it;
//   - not a bucket root, not longer than a folder id can be, not nested too deep to check;
//   - no folder claimed at or above it already (the key's own claim covers it; another key's
//     claim decides against it), no record of another key at it or above it by either name
//     (with or without the trailing slash), and none beneath it.
//
// Anything less is left as it was. The migration only adds storage_folder rows, which a
// binary without the folder rule never reads, so it is safe to run under the previous
// binary; there is nothing to roll back.

// backfillFolderMaxLength and backfillFolderMaxLevels match the bounds of the folder rule
// (plugins/governance maxStorageFolderLength, maxStorageFolderLevels).
const (
	backfillFolderMaxLength = 512
	backfillFolderMaxLevels = 64
)

// batchOutputFolderOf returns the folder a batch output directory names, and whether it can
// be claimed by the backfill at all.
func batchOutputFolderOf(location string) (folder string, ok bool) {
	var scheme string
	for _, candidate := range []string{"gs://", "s3://"} {
		if strings.HasPrefix(location, candidate) {
			scheme = candidate
		}
	}
	if scheme == "" {
		return "", false
	}
	folder = location
	if !strings.HasSuffix(folder, "/") {
		folder += "/"
	}
	rest := folder[len(scheme):]
	slash := strings.IndexByte(rest, '/')
	if slash <= 0 || slash == len(rest)-1 || len(folder) > backfillFolderMaxLength || strings.Count(rest, "/") > backfillFolderMaxLevels {
		return "", false
	}
	return folder, true
}

// foldersAbove returns every folder from the bucket down to folder itself.
func foldersAbove(folder string) []string {
	start := strings.Index(folder, "://") + len("://")
	var out []string
	for i := start; i < len(folder); i++ {
		if folder[i] == '/' {
			out = append(out, folder[:i+1])
		}
	}
	return out
}

// exclusiveBatchOutputFolders returns, from the output folders of every recorded batch by key,
// the ones the backfill claims: named by one key only, with no other key's folder inside or
// around them, and passing claimable (the checks against what is already recorded), which also
// makes the claim. Folders go in byte order, so an ancestor is decided before what lies in it,
// and of one key's nested folders only one that was actually claimed covers the rest: a folder
// whose ancestor failed claimable is still tried on its own.
func exclusiveBatchOutputFolders(byFolder map[string]map[string]struct{}, claimable func(folder, vk string) (bool, error)) (map[string]string, error) {
	folders := make([]string, 0, len(byFolder))
	for folder := range byFolder {
		folders = append(folders, folder)
	}
	sort.Strings(folders)
	owner := make(map[string]string, len(folders))
	for _, folder := range folders {
		if len(byFolder[folder]) == 1 {
			for vk := range byFolder[folder] {
				owner[folder] = vk
			}
		}
	}
	keep := make(map[string]string)
	for i, folder := range folders {
		vk, single := owner[folder]
		if !single {
			continue
		}
		covered, conflict := false, false
		// A folder nested in this one sorts right after it.
		for _, inner := range folders[i+1:] {
			if !strings.HasPrefix(inner, folder) {
				break
			}
			if owner[inner] != vk {
				conflict = true
				break
			}
		}
		above := foldersAbove(folder)
		for _, outer := range above[:len(above)-1] {
			if _, named := byFolder[outer]; !named {
				continue
			}
			// An ancestor sorts before its descendants, so keep already holds its answer. Only an
			// ancestor that is itself claimed covers this folder; one of the key's that could not
			// be claimed leaves this one to stand on its own.
			if owner[outer] != vk {
				conflict = true
			} else if keep[outer] == vk {
				covered = true
			}
		}
		if conflict || covered {
			continue
		}
		ok, err := claimable(folder, vk)
		if err != nil {
			return keep, err
		}
		if ok {
			keep[folder] = vk
		}
	}
	return keep, nil
}

// migrationBackfillBatchOutputFolders runs the backfill once.
func migrationBackfillBatchOutputFolders(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	migrationName := "backfill_batch_output_folders"
	logger.Info("[configstore] starting migration %s", migrationName)
	defer logger.Info("[configstore] finished migration %s", migrationName)
	noTxOpts := *migrator.DefaultOptions
	noTxOpts.UseTransaction = false
	m := migrator.New(db, &noTxOpts, []*migrator.Migration{{
		ID: migrationName,
		Migrate: func(tx *gorm.DB) error {
			claimed, err := backfillBatchOutputFolders(ctx, tx.WithContext(ctx))
			if err != nil {
				return err
			}
			logger.Info("[configstore] %s: claimed %d batch output folders", migrationName, claimed)
			return nil
		},
		Rollback: func(*gorm.DB) error { return nil },
	}})
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("error while running db migration %s: %w", migrationName, err)
	}
	return nil
}

// backfillBatchOutputFolders claims the exclusive output folders of recorded batches and
// returns how many it claimed. Each claim is one insert that keeps an existing row, so a run
// cut short and run again claims the rest.
func backfillBatchOutputFolders(ctx context.Context, db *gorm.DB) (int, error) {
	store := &RDBConfigStore{}
	store.db.Store(db)
	byFolder := make(map[string]map[string]struct{})
	var jobs []tables.TableProviderJob
	err := db.Model(&tables.TableProviderJob{}).
		Select("id", "output_file_id", "virtual_key_id").
		Where("kind = ? AND virtual_key_id IS NOT NULL AND virtual_key_id <> '' AND (output_file_id LIKE 'gs://%' OR output_file_id LIKE 's3://%')", tables.ProviderJobKindBatch).
		FindInBatches(&jobs, 500, func(*gorm.DB, int) error {
			for _, job := range jobs {
				folder, ok := batchOutputFolderOf(derefJobString(job.OutputFileID))
				if !ok {
					continue
				}
				if byFolder[folder] == nil {
					byFolder[folder] = make(map[string]struct{}, 1)
				}
				byFolder[folder][*job.VirtualKeyID] = struct{}{}
			}
			return nil
		}).Error
	if err != nil {
		return 0, fmt.Errorf("read batch output folders: %w", err)
	}
	claimed := 0
	kinds := []string{tables.ProviderObjectKindFile, tables.ProviderObjectKindStorageFolder}
	_, err = exclusiveBatchOutputFolders(byFolder, func(folder, vk string) (bool, error) {
		above, err := store.GetProviderObjectsByObjectIDs(ctx, tables.ProviderObjectKindStorageFolder, foldersAbove(folder))
		if err != nil || len(above) > 0 {
			return false, err
		}
		// Another key's record at this folder or one above, by either name (an output directory
		// recorded with or without its trailing slash), or anywhere beneath it.
		names := make([]string, 0, 2*len(foldersAbove(folder)))
		for _, name := range foldersAbove(folder) {
			names = append(names, name, strings.TrimSuffix(name, "/"))
		}
		foreign, err := store.HasForeignRecordAt(ctx, names, vk)
		if err == nil && !foreign {
			foreign, err = store.HasForeignProviderObjectUnderPrefix(ctx, kinds, folder, vk)
		}
		if err == nil && !foreign {
			foreign, err = store.HasForeignProviderJobFileUnderPrefix(ctx, folder, vk)
		}
		if err != nil || foreign {
			return false, err
		}
		provider := tables.StorageFolderProvider(folder)
		inserted, err := store.InsertProviderObject(ctx, &tables.TableProviderObject{
			ID:           tables.ProviderObjectID(tables.ProviderObjectKindStorageFolder, provider, folder),
			Kind:         tables.ProviderObjectKindStorageFolder,
			Provider:     provider,
			ObjectID:     folder,
			VirtualKeyID: vk,
		})
		if err != nil {
			return false, err
		}
		if inserted {
			claimed++
			return true, nil
		}
		// Another writer took the folder between the checks and the insert; it covers nothing
		// for this key.
		return false, nil
	})
	return claimed, err
}

func derefJobString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
