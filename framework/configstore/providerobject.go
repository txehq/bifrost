package configstore

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm/clause"

	"github.com/capsohq/bifrost/framework/configstore/tables"
	"github.com/capsohq/bifrost/framework/queryscope"
)

// UpsertProviderObject records the creating virtual key of a provider object. The
// first record for an id wins: an object has one creator, and a later write for
// the same id (a retrieve seen after the create, a replayed response) must not
// hand it to someone else.
func (s *RDBConfigStore) UpsertProviderObject(ctx context.Context, object *tables.TableProviderObject) error {
	if object == nil || object.ID == "" {
		return fmt.Errorf("provider object and id are required")
	}
	if object.Kind == "" || object.Provider == "" || object.ObjectID == "" || object.VirtualKeyID == "" {
		return fmt.Errorf("provider object kind, provider, object id and virtual key id are required")
	}
	if object.CreatedAt.IsZero() {
		object.CreatedAt = time.Now().UTC()
	}
	return s.DB().WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "id"}},
		DoNothing: true,
	}).Create(object).Error
}

// providerIDListInlineMax is the longest id list bound one parameter per id.
const providerIDListInlineMax = 1000

// GetProviderObjectsByIDs returns the provider objects among ids that exist, in no
// particular order. Ids with no row are simply absent from the result. A list page fans
// its ids out across every provider name sharing the API, so ids can outnumber the
// bind-parameter limit (SQLite 32,766, Postgres 65,535). A list longer than
// providerIDListInlineMax travels as one parameter (queryscope.InStrings); a short one,
// the common case, keeps the plain IN, which SQLite plans faster than a json_each scan.
func (s *RDBConfigStore) GetProviderObjectsByIDs(ctx context.Context, ids []string) ([]*tables.TableProviderObject, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	db := s.DB().WithContext(ctx)
	if len(ids) <= providerIDListInlineMax {
		db = db.Where("id IN ?", ids)
	} else {
		db = db.Where(queryscope.InStrings(db, "id", ids))
	}
	var objects []*tables.TableProviderObject
	if err := db.Find(&objects).Error; err != nil {
		return nil, err
	}
	return objects, nil
}

// DeleteProviderObjectsByObjectIDSuffix forgets the provider objects of kind on
// provider that virtualKeyID created and whose provider-side id ends in "/"+suffix:
// a full resource name such as "projects/<p>/locations/<l>/batchPredictionJobs/<n>"
// that a delete named by its last segment alone. A job number is scoped to a project
// and region, so another key's job of the same number in another project is not the
// deleter's to forget. The suffix matches whole, at a path boundary, with LIKE
// metacharacters taken literally.
func (s *RDBConfigStore) DeleteProviderObjectsByObjectIDSuffix(ctx context.Context, kind, provider, suffix, virtualKeyID string) error {
	if kind == "" || provider == "" || suffix == "" || virtualKeyID == "" {
		return fmt.Errorf("provider object kind, provider, id suffix and virtual key id are required")
	}
	escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(suffix)
	return s.DB().WithContext(ctx).
		Where(`kind = ? AND provider = ? AND virtual_key_id = ? AND object_id LIKE ? ESCAPE '\'`, kind, provider, virtualKeyID, "%/"+escaped).
		Delete(&tables.TableProviderObject{}).Error
}

// GetProviderObjectsByObjectIDSuffixes returns the provider objects of kind on provider
// whose provider-side id ends in "/"+one of suffixes, in no particular order: full
// resource names such as "projects/<p>/locations/<l>/cachedContents/<n>" addressed by
// their last segment alone. Each suffix matches whole, at a path boundary, with LIKE
// metacharacters taken literally; empty suffixes are ignored. Each suffix is its own LIKE
// term, so the suffixes are read suffixPatternChunkSize at a time.
func (s *RDBConfigStore) GetProviderObjectsByObjectIDSuffixes(ctx context.Context, kind, provider string, suffixes []string) ([]*tables.TableProviderObject, error) {
	patterns := suffixLikePatterns(suffixes)
	if kind == "" || provider == "" || len(patterns) == 0 {
		return nil, nil
	}
	var objects []*tables.TableProviderObject
	seen := make(map[string]struct{})
	for start := 0; start < len(patterns); start += suffixPatternChunkSize {
		chunk := patterns[start:min(start+suffixPatternChunkSize, len(patterns))]
		suffixWhere := s.DB()
		for _, pattern := range chunk {
			suffixWhere = suffixWhere.Or(`object_id LIKE ? ESCAPE '\'`, pattern)
		}
		var page []*tables.TableProviderObject
		if err := s.DB().WithContext(ctx).
			Where("kind = ? AND provider = ?", kind, provider).
			Where(suffixWhere).
			Find(&page).Error; err != nil {
			return nil, err
		}
		for _, object := range page {
			if _, dup := seen[object.ID]; dup {
				continue
			}
			seen[object.ID] = struct{}{}
			objects = append(objects, object)
		}
	}
	return objects, nil
}

// GetProviderObjectsByObjectID returns the provider objects of kind recorded for objectID under
// any provider name, in no particular order. An id carries its provider in the stable key, so a
// lookup by the stable key misses a row recorded under a provider name that is no longer
// configured; this one does not. The kind leads the composite index, so only rows of that kind
// are read.
func (s *RDBConfigStore) GetProviderObjectsByObjectID(ctx context.Context, kind, objectID string) ([]*tables.TableProviderObject, error) {
	if kind == "" || objectID == "" {
		return nil, nil
	}
	var objects []*tables.TableProviderObject
	if err := s.DB().WithContext(ctx).Where("kind = ? AND object_id = ?", kind, objectID).Find(&objects).Error; err != nil {
		return nil, err
	}
	return objects, nil
}

// InsertProviderObject records the creating virtual key of a provider object like
// UpsertProviderObject and reports whether this call wrote the row. False with no error
// means a row for the id was already there, whoever it names. A caller that makes a claim
// before the work it covers uses this to know which claims are its to withdraw.
func (s *RDBConfigStore) InsertProviderObject(ctx context.Context, object *tables.TableProviderObject) (bool, error) {
	if object == nil || object.ID == "" {
		return false, fmt.Errorf("provider object and id are required")
	}
	if object.Kind == "" || object.Provider == "" || object.ObjectID == "" || object.VirtualKeyID == "" {
		return false, fmt.Errorf("provider object kind, provider, object id and virtual key id are required")
	}
	if object.CreatedAt.IsZero() {
		object.CreatedAt = time.Now().UTC()
	}
	result := s.DB().WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "id"}},
		DoNothing: true,
	}).Create(object)
	return result.Error == nil && result.RowsAffected > 0, result.Error
}

// GetProviderObjectsByObjectIDs returns the provider objects of kind recorded for any of
// objectIDs under any provider name, in no particular order. It is GetProviderObjectsByObjectID
// for a short list: the folders above one stored object.
func (s *RDBConfigStore) GetProviderObjectsByObjectIDs(ctx context.Context, kind string, objectIDs []string) ([]*tables.TableProviderObject, error) {
	if kind == "" || len(objectIDs) == 0 {
		return nil, nil
	}
	var objects []*tables.TableProviderObject
	if err := s.DB().WithContext(ctx).Where("kind = ? AND object_id IN ?", kind, objectIDs).Find(&objects).Error; err != nil {
		return nil, err
	}
	return objects, nil
}

// prefixRange returns the half-open byte range [from, to) that holds exactly the strings
// starting with prefix. ok is false for a prefix with no upper bound (empty, or all 0xFF).
func prefixRange(prefix string) (from, to string, ok bool) {
	end := []byte(prefix)
	for len(end) > 0 && end[len(end)-1] == 0xFF {
		end = end[:len(end)-1]
	}
	if len(end) == 0 {
		return "", "", false
	}
	end[len(end)-1]++
	return prefix, string(end), true
}

// bytewise names column for a comparison in byte order. A prefix is a byte range only in
// byte order: Postgres compares text in the database's collation unless told otherwise, and
// the indexes that serve these lookups are built on the same expression
// (migrationAddStoragePrefixIndexes). SQLite compares in byte order already.
func (s *RDBConfigStore) bytewise(column string) string {
	if s.DB().Dialector.Name() == "postgres" {
		return column + ` COLLATE "C"`
	}
	return column
}

// HasForeignProviderObjectUnderPrefix reports whether a provider object of one of kinds,
// under any provider name, has an object id starting with prefix and a creator other than
// virtualKeyID. The match is byte for byte: LIKE metacharacters, case and doubled slashes
// in the prefix are literal. It reads at most one row.
func (s *RDBConfigStore) HasForeignProviderObjectUnderPrefix(ctx context.Context, kinds []string, prefix, virtualKeyID string) (bool, error) {
	from, to, ok := prefixRange(prefix)
	if len(kinds) == 0 || virtualKeyID == "" || !ok {
		return false, fmt.Errorf("provider object kinds, a prefix and a virtual key id are required")
	}
	column := s.bytewise("object_id")
	var ids []string
	err := s.DB().WithContext(ctx).Model(&tables.TableProviderObject{}).
		Where("kind IN ? AND "+column+" >= ? AND "+column+" < ? AND virtual_key_id <> ?", kinds, from, to, virtualKeyID).
		Limit(1).Pluck("id", &ids).Error
	return len(ids) > 0, err
}

// HasForeignProviderJobFileUnderPrefix reports whether an accounting job row created by a
// virtual key other than virtualKeyID names an output or error file starting with prefix.
// Those files have ownership rows of their own only once the batch was retrieved through the
// gateway; the job row names them as soon as the sweeper has seen the batch. A job with no
// virtual key is nobody's and does not count. It reads at most one row per column.
func (s *RDBConfigStore) HasForeignProviderJobFileUnderPrefix(ctx context.Context, prefix, virtualKeyID string) (bool, error) {
	from, to, ok := prefixRange(prefix)
	if virtualKeyID == "" || !ok {
		return false, fmt.Errorf("a prefix and a virtual key id are required")
	}
	for _, name := range []string{"output_file_id", "error_file_id"} {
		column := s.bytewise(name)
		var ids []string
		err := s.DB().WithContext(ctx).Model(&tables.TableProviderJob{}).
			Where(column+" >= ? AND "+column+" < ? AND virtual_key_id IS NOT NULL AND virtual_key_id <> ?", from, to, virtualKeyID).
			Limit(1).Pluck("id", &ids).Error
		if err != nil || len(ids) > 0 {
			return len(ids) > 0, err
		}
	}
	return false, nil
}

// ProviderObjectQuery selects a page of provider objects of one kind (ListProviderObjects).
type ProviderObjectQuery struct {
	Kind string
	// VirtualKeyID, when set, keeps the objects of that key only.
	VirtualKeyID string
	// Prefix, when set, keeps the objects whose id starts with it, byte for byte.
	Prefix string
	Limit  int
	Offset int
}

// maxProviderObjectPage bounds one page of ListProviderObjects.
const maxProviderObjectPage = 1000

// ListProviderObjects returns a page of the provider objects of query.Kind, in object id
// order, and how many match in all. A limit outside 1..maxProviderObjectPage is
// maxProviderObjectPage.
func (s *RDBConfigStore) ListProviderObjects(ctx context.Context, query ProviderObjectQuery) ([]*tables.TableProviderObject, int64, error) {
	if query.Kind == "" {
		return nil, 0, fmt.Errorf("provider object kind is required")
	}
	if query.Limit <= 0 || query.Limit > maxProviderObjectPage {
		query.Limit = maxProviderObjectPage
	}
	if query.Offset < 0 {
		query.Offset = 0
	}
	db := s.DB().WithContext(ctx).Model(&tables.TableProviderObject{}).Where("kind = ?", query.Kind)
	if query.VirtualKeyID != "" {
		db = db.Where("virtual_key_id = ?", query.VirtualKeyID)
	}
	if query.Prefix != "" {
		from, to, ok := prefixRange(query.Prefix)
		if !ok {
			return nil, 0, fmt.Errorf("invalid object id prefix")
		}
		column := s.bytewise("object_id")
		db = db.Where(column+" >= ? AND "+column+" < ?", from, to)
	}
	var total int64
	if err := db.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var objects []*tables.TableProviderObject
	err := db.Order(s.bytewise("object_id")).Order("id").Limit(query.Limit).Offset(query.Offset).Find(&objects).Error
	return objects, total, err
}

// DeleteProviderObject forgets a provider object's creator once the object itself
// is gone. Deleting an id with no row is not an error.
func (s *RDBConfigStore) DeleteProviderObject(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("provider object id is required")
	}
	return s.DB().WithContext(ctx).Where("id = ?", id).Delete(&tables.TableProviderObject{}).Error
}
