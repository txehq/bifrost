package logstore

import (
	"context"
	"testing"
	"time"

	"github.com/capsohq/bifrost/framework/queryscope"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestHiddenRequestTypesReads(t *testing.T) {
	s := newTestSQLiteStore(t)
	base := context.Background()
	t.Cleanup(func() { require.NoError(t, s.Close(base)) })
	ctx := context.WithValue(base, HiddenRequestTypesContextKey, []string{"count_tokens", "embedding"})
	now := time.Now().UTC().Truncate(time.Minute)
	session := "session"
	for i, object := range []string{"count_tokens", "chat_completion", "embedding", "responses", "chat_completion_stream"} {
		cost := 1.0
		// Even a context carrying visibility must not suppress writes.
		require.NoError(t, s.Create(ctx, &Log{
			ID: object, Object: object, Timestamp: now.Add(time.Duration(i) * time.Second),
			Provider: "openai", Model: object, Status: "success", Cost: &cost,
			TotalTokens: 10, ParentRequestID: &session,
		}))
	}
	page := PaginationOptions{Limit: 2, SortBy: "timestamp", Order: "asc"}
	first, err := s.SearchLogs(ctx, SearchFilters{}, page)
	require.NoError(t, err)
	require.Equal(t, int64(3), first.Pagination.TotalCount)
	require.Len(t, first.Logs, 2)
	require.Equal(t, "chat_completion", first.Logs[0].ID)
	require.Equal(t, "responses", first.Logs[1].ID)
	page.Offset = 2
	second, err := s.SearchLogs(ctx, SearchFilters{}, page)
	require.NoError(t, err)
	require.Equal(t, int64(3), second.Pagination.TotalCount)
	require.Len(t, second.Logs, 1)
	require.Equal(t, "chat_completion_stream", second.Logs[0].ID)

	stats, err := s.GetStats(ctx, SearchFilters{})
	require.NoError(t, err)
	require.Equal(t, int64(3), stats.TotalRequests)
	require.Equal(t, int64(30), stats.TotalTokens)
	require.Equal(t, 3.0, stats.TotalCost)
	hist, err := s.GetHistogram(ctx, SearchFilters{}, 60)
	require.NoError(t, err)
	require.Len(t, hist.Buckets, 1)
	require.Equal(t, int64(3), hist.Buckets[0].Count)

	detail, err := s.GetSessionLogs(ctx, session, PaginationOptions{Limit: 2, Offset: 2})
	require.NoError(t, err)
	require.Equal(t, int64(3), detail.Count)
	require.Len(t, detail.Logs, 1)
	require.False(t, detail.HasMore)
	summary, err := s.GetSessionSummary(ctx, session)
	require.NoError(t, err)
	require.Equal(t, int64(3), summary.Count)
	require.Equal(t, int64(30), summary.TotalTokens)

	models, err := s.GetDistinctModels(ctx, 100, "")
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"chat_completion", "responses", "chat_completion_stream"}, models)
	_, err = s.FindByID(ctx, "embedding")
	require.ErrorIs(t, err, ErrNotFound)
	_, err = s.FindByID(base, "embedding")
	require.NoError(t, err, "hidden logs remain stored")
	all, err := s.SearchLogs(base, SearchFilters{}, PaginationOptions{Limit: 10})
	require.NoError(t, err)
	require.Equal(t, int64(5), all.Pagination.TotalCount)

	// An explicit request-type filter cannot re-include a hidden type.
	onlyHidden, err := s.SearchLogs(ctx, SearchFilters{Objects: []string{"embedding"}}, page)
	require.NoError(t, err)
	require.Zero(t, onlyHidden.Pagination.TotalCount)
	require.Empty(t, onlyHidden.Logs)

	// Visibility intersects with access control instead of replacing it.
	scoped := queryscope.WithQueryScope(ctx, func(db *gorm.DB) *gorm.DB {
		return db.Where("model = ?", "responses")
	})
	stats, err = s.GetStats(scoped, SearchFilters{})
	require.NoError(t, err)
	require.Equal(t, int64(1), stats.TotalRequests)

	// The consolidated dashboard also reads MCP tables, which lack object_type.
	_, err = s.SearchMCPToolLogs(ctx, MCPToolLogSearchFilters{}, PaginationOptions{Limit: 10})
	require.NoError(t, err)
	_, err = s.GetMCPHistogram(ctx, MCPToolLogSearchFilters{}, 60)
	require.NoError(t, err)

	allHidden := context.WithValue(base, HiddenRequestTypesContextKey, []string{"count_tokens", "embedding", "chat_completion", "responses", "chat_completion_stream"})
	hasLogs, err := s.HasLogs(allHidden)
	require.NoError(t, err)
	require.False(t, hasLogs)
}

func TestHiddenRequestTypesHourlyViewScope(t *testing.T) {
	s := newScopedDBTestLogStore(t)
	ctx := context.WithValue(context.Background(), HiddenRequestTypesContextKey, []string{"embedding"})
	stmt := s.scopedLogsDB(ctx).Session(&gorm.Session{DryRun: true}).Table("mv_logs_hourly").Select("SUM(total_requests)").Find(&struct{}{}).Statement
	require.Contains(t, stmt.SQL.String(), "object_type NOT IN (?)")
	require.Contains(t, stmt.Vars, "embedding")
}

func TestHiddenRequestTypesSessionsAndFallbacks(t *testing.T) {
	s := newTestSQLiteStore(t)
	base := context.Background()
	t.Cleanup(func() { require.NoError(t, s.Close(base)) })
	ctx := context.WithValue(base, HiddenRequestTypesContextKey, []string{"embedding"})
	now := time.Now().UTC()
	rootID := "root"
	for _, row := range []*Log{
		{ID: rootID, Object: "responses", Status: "error", Timestamp: now},
		{ID: "child", Object: "embedding", Status: "success", Timestamp: now, ParentRequestID: &rootID, FallbackIndex: 1},
	} {
		require.NoError(t, s.Create(base, row))
	}
	stats, err := s.GetStats(ctx, SearchFilters{})
	require.NoError(t, err)
	require.Zero(t, stats.UserFacingSuccessRate, "hidden fallback successes must not affect visible statistics")
	root, err := s.FindByID(ctx, rootID)
	require.NoError(t, err)
	require.Zero(t, root.ChildCount, "hidden children must not enter session rollups")

	// A visible child of a hidden root is still discoverable in the root list.
	ctx = context.WithValue(base, HiddenRequestTypesContextKey, []string{"responses"})
	page, err := s.SearchLogs(ctx, SearchFilters{RootsOnly: true}, PaginationOptions{Limit: 10})
	require.NoError(t, err)
	require.Equal(t, int64(1), page.Pagination.TotalCount)
	require.Len(t, page.Logs, 1)
	require.Equal(t, "child", page.Logs[0].ID)
}

func TestHiddenRequestTypesPostgres(t *testing.T) {
	s, db := setupPerfTestDB(t)
	s.logger = testLogger{}
	base := context.Background()
	ctx := context.WithValue(base, HiddenRequestTypesContextKey, []string{"embedding"})
	for _, object := range []string{"embedding", "responses"} {
		require.NoError(t, s.Create(base, &Log{
			ID: object, Object: object, Status: "success", Model: object,
			Timestamp: time.Now().UTC().Add(-2 * time.Hour),
		}))
	}
	refreshTestMatViews(t, db)
	s.matViewsReady.Store(true)
	require.True(t, s.canUseMatViewForFreshAggregate(context.Background(), SearchFilters{}))
	require.False(t, s.canUseFilterMatView(ctx))
	page, err := s.SearchLogs(ctx, SearchFilters{}, PaginationOptions{Limit: 10})
	require.NoError(t, err)
	require.Equal(t, int64(1), page.Pagination.TotalCount)
	require.Len(t, page.Logs, 1)
	stats, err := s.GetStats(ctx, SearchFilters{})
	require.NoError(t, err)
	require.Equal(t, int64(1), stats.TotalRequests)
	models, err := s.GetDistinctModels(ctx, 100, "")
	require.NoError(t, err)
	require.Equal(t, []string{"responses"}, models)
}

// passthroughVisibilityRows are one plain typed call, the two kinds of passthrough
// call now stored under their inferred type and tagged, and a passthrough call
// written before the tag existed.
func passthroughVisibilityRows(now time.Time) []*Log {
	cost := 1.0
	return []*Log{
		{ID: "plain", Object: "chat_completion", Status: "success", Model: "m", Timestamp: now, Cost: &cost, TotalTokens: 10},
		{ID: "tagged", Object: "chat_completion", IsPassthrough: true, Status: "success", Model: "m", Timestamp: now.Add(time.Second), Cost: &cost, TotalTokens: 10},
		{ID: "tagged-stream", Object: "chat_completion_stream", IsPassthrough: true, Stream: true, Status: "success", Model: "m", Timestamp: now.Add(2 * time.Second), Cost: &cost, TotalTokens: 10},
		{ID: "untagged", Object: "passthrough", Status: "success", Model: "m", Timestamp: now.Add(3 * time.Second), Cost: &cost, TotalTokens: 10},
	}
}

// Hiding passthrough or passthrough_stream keeps hiding those calls now that they are
// stored under their inferred request type: passthrough hides the non-streamed tagged
// rows, passthrough_stream the streamed ones, as each did when the rows carried that
// object type, and rows written before the tag still match on object_type.
func TestHiddenRequestTypesHideTaggedPassthroughRows(t *testing.T) {
	s := newTestSQLiteStore(t)
	base := context.Background()
	t.Cleanup(func() { require.NoError(t, s.Close(base)) })
	now := time.Now().UTC().Truncate(time.Minute)
	for _, row := range passthroughVisibilityRows(now) {
		require.NoError(t, s.Create(base, row))
	}
	for _, tc := range []struct {
		hidden []string
		want   []string
	}{
		{nil, []string{"plain", "tagged", "tagged-stream", "untagged"}},
		{[]string{"passthrough"}, []string{"plain", "tagged-stream"}},
		{[]string{"passthrough_stream"}, []string{"plain", "tagged", "untagged"}},
		{[]string{"passthrough", "passthrough_stream"}, []string{"plain"}},
		{[]string{"chat_completion"}, []string{"tagged-stream", "untagged"}},
	} {
		ctx := base
		if tc.hidden != nil {
			ctx = context.WithValue(base, HiddenRequestTypesContextKey, tc.hidden)
		}
		page, err := s.SearchLogs(ctx, SearchFilters{}, PaginationOptions{Limit: 10, SortBy: "timestamp", Order: "asc"})
		require.NoError(t, err, "hidden=%v", tc.hidden)
		var ids []string
		for _, l := range page.Logs {
			ids = append(ids, l.ID)
		}
		require.Equal(t, tc.want, ids, "hidden=%v", tc.hidden)
		require.Equal(t, int64(len(tc.want)), page.Pagination.TotalCount, "hidden=%v", tc.hidden)
		stats, err := s.GetStats(ctx, SearchFilters{})
		require.NoError(t, err)
		require.Equal(t, int64(len(tc.want)), stats.TotalRequests, "hidden=%v", tc.hidden)
		require.Equal(t, float64(len(tc.want)), stats.TotalCost, "hidden=%v", tc.hidden)
		hist, err := s.GetHistogram(ctx, SearchFilters{}, 60)
		require.NoError(t, err)
		var count int64
		for _, b := range hist.Buckets {
			count += b.Count
		}
		require.Equal(t, int64(len(tc.want)), count, "hidden=%v", tc.hidden)
	}

	// A hidden passthrough fallback success does not count toward the visible success rate.
	rootID := "root"
	require.NoError(t, s.Create(base, &Log{ID: rootID, Object: "chat_completion", Status: "error", Timestamp: now.Add(time.Hour)}))
	require.NoError(t, s.Create(base, &Log{ID: "fallback", Object: "chat_completion", IsPassthrough: true, Status: "success",
		Timestamp: now.Add(time.Hour), ParentRequestID: &rootID, FallbackIndex: 1}))
	window := SearchFilters{StartTime: schemasTime(now.Add(30 * time.Minute)), EndTime: schemasTime(now.Add(2 * time.Hour))}
	hiddenCtx := context.WithValue(base, HiddenRequestTypesContextKey, []string{"passthrough"})
	stats, err := s.GetStats(hiddenCtx, window)
	require.NoError(t, err)
	require.Zero(t, stats.UserFacingSuccessRate, "a hidden passthrough fallback must not rescue the visible root")
	stats, err = s.GetStats(base, window)
	require.NoError(t, err)
	require.NotZero(t, stats.UserFacingSuccessRate, "without hiding, the fallback succeeds the root")
}

func schemasTime(t time.Time) *time.Time { return &t }

// On Postgres the hourly view has no is_passthrough column, so a read that hides
// passthrough is served from the raw table, while other hidden types keep the view.
func TestHiddenPassthroughTypesSkipHourlyViewPostgres(t *testing.T) {
	s, db := setupPerfTestDB(t)
	s.logger = testLogger{}
	base := context.Background()
	for _, row := range passthroughVisibilityRows(time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Hour)) {
		require.NoError(t, s.Create(base, row))
	}
	refreshTestMatViews(t, db)
	s.matViewsReady.Store(true)

	hidePassthrough := context.WithValue(base, HiddenRequestTypesContextKey, []string{"passthrough"})
	hideEmbedding := context.WithValue(base, HiddenRequestTypesContextKey, []string{"embedding"})
	require.False(t, s.canUseMatView(hidePassthrough, SearchFilters{}), "the view cannot tell tagged passthrough rows apart")
	require.False(t, s.canUseMatViewForFreshAggregate(hidePassthrough, SearchFilters{}))
	require.True(t, s.canUseMatViewForFreshAggregate(hideEmbedding, SearchFilters{}), "other hidden types keep the view")

	stats, err := s.GetStats(hidePassthrough, SearchFilters{})
	require.NoError(t, err)
	require.Equal(t, int64(2), stats.TotalRequests)
	hist, err := s.GetHistogram(hidePassthrough, SearchFilters{}, 3600)
	require.NoError(t, err)
	var count int64
	for _, b := range hist.Buckets {
		count += b.Count
	}
	require.Equal(t, int64(2), count)
	stats, err = s.GetStats(hideEmbedding, SearchFilters{})
	require.NoError(t, err)
	require.Equal(t, int64(4), stats.TotalRequests, "the view path still serves every passthrough row when they are not hidden")
}
