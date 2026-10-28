package logstore

import (
	"context"
	"slices"

	"github.com/capsohq/bifrost/core/schemas"
	"gorm.io/gorm"
)

type visibilityContextKey string

// HiddenRequestTypesContextKey carries []string request types on dashboard read
// contexts. Writers and background jobs must not set this key.
const HiddenRequestTypesContextKey visibilityContextKey = "logstore-hidden-request-types"

// HiddenRequestTypesFromContext returns the request types excluded from a read.
func HiddenRequestTypesFromContext(ctx context.Context) []string {
	if ctx == nil {
		return nil
	}
	types, _ := ctx.Value(HiddenRequestTypesContextKey).([]string)
	return types
}

// scopedLogsDB composes dashboard visibility with the caller's access scope.
// Only LLM log readers use it: MCP tables do not have an object_type column.
// Filtering here keeps counts, aggregates and pagination on the same row set.
func (s *RDBLogStore) scopedLogsDB(ctx context.Context) *gorm.DB {
	db := s.ScopedDB(ctx)
	if types := HiddenRequestTypesFromContext(ctx); len(types) > 0 {
		clause, args := hiddenRequestTypesPredicate(types)
		db = db.Where(clause, args...)
	}
	return db
}

// hiddenRequestTypesPredicate is the WHERE clause that hides types from a logs read.
// A passthrough call is stored under the request type its route maps to and tagged
// is_passthrough, so hiding passthrough or passthrough_stream also hides the tagged
// rows that were stored as that before the tag existed: non-streamed ones for
// passthrough, streamed ones for passthrough_stream. The tagged rows need the logs
// table's is_passthrough column, which the hourly materialized view does not carry;
// hidesPassthroughTypes keeps those reads on the raw table.
func hiddenRequestTypesPredicate(types []string) (string, []any) {
	clause, args := "object_type NOT IN ?", []any{types}
	if slices.Contains(types, string(schemas.PassthroughRequest)) {
		clause += " AND NOT (is_passthrough = ? AND COALESCE(stream, ?) = ?)"
		args = append(args, true, false, false)
	}
	if slices.Contains(types, string(schemas.PassthroughStreamRequest)) {
		clause += " AND NOT (is_passthrough = ? AND COALESCE(stream, ?) = ?)"
		args = append(args, true, false, true)
	}
	return clause, args
}

// hidesPassthroughTypes reports whether the read hides passthrough calls, which only
// the raw logs table can tell apart from the typed requests they are stored as.
func hidesPassthroughTypes(ctx context.Context) bool {
	types := HiddenRequestTypesFromContext(ctx)
	return slices.Contains(types, string(schemas.PassthroughRequest)) || slices.Contains(types, string(schemas.PassthroughStreamRequest))
}

func (s *RDBLogStore) canUseFilterMatView(ctx context.Context) bool {
	// Filter-only materialized views omit object_type, unlike mv_logs_hourly.
	return s.db.Dialector.Name() == "postgres" && s.matViewsReady.Load() && len(HiddenRequestTypesFromContext(ctx)) == 0
}
