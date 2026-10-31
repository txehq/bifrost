package logstore

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// newSessionGroupStore seeds a DB covering the shapes session grouping has to
// get right: a session whose first row is a fallback child, a session whose
// first row is filtered out by a status filter, a single-row session, and rows
// with no session at all.
//
// Named shared-cache DSN for the same reason as newRootsOnlyStore: SearchLogs
// queries from concurrent goroutines, and a plain :memory: DSN gives each
// pooled connection its own empty DB.
func newSessionGroupStore(t *testing.T) (*RDBLogStore, time.Time) {
	t.Helper()

	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	require.NoError(t, db.AutoMigrate(&Log{}))

	now := time.Now()
	strPtr := func(v string) *string { return &v }
	floatPtr := func(v float64) *float64 { return &v }

	seed := []Log{
		// Session s1, three turns. The second turn fell back once, and that
		// attempt is timestamped before the third turn — it must not be mistaken
		// for a session root.
		{ID: "s1-turn1", Timestamp: now, Status: "success", Provider: "openai", SessionID: strPtr("s1"), Cost: floatPtr(1), TotalTokens: 10},
		{ID: "s1-turn2", Timestamp: now.Add(time.Second), Status: "error", Provider: "openai", SessionID: strPtr("s1"), Cost: floatPtr(2), TotalTokens: 20},
		{ID: "s1-turn2-retry", Timestamp: now.Add(2 * time.Second), Status: "success", Provider: "anthropic", FallbackIndex: 1, ParentRequestID: strPtr("s1-turn2"), SessionID: strPtr("s1"), Cost: floatPtr(4), TotalTokens: 40},
		{ID: "s1-turn3", Timestamp: now.Add(3 * time.Second), Status: "success", Provider: "openai", SessionID: strPtr("s1"), Cost: floatPtr(8), TotalTokens: 80},

		// Session s2 whose earliest turn is a fallback child of a row in the same
		// session: the chain root is later in time. The session must still list,
		// rooted at the chain root rather than vanishing behind the child.
		{ID: "s2-child", Timestamp: now, Status: "error", Provider: "openai", FallbackIndex: 1, ParentRequestID: strPtr("s2-root"), SessionID: strPtr("s2"), Cost: floatPtr(1), TotalTokens: 5},
		{ID: "s2-root", Timestamp: now.Add(time.Second), Status: "success", Provider: "openai", SessionID: strPtr("s2"), Cost: floatPtr(3), TotalTokens: 15},

		// Single-row session: no group, so it must carry no session aggregates.
		{ID: "s3-only", Timestamp: now, Status: "success", Provider: "openai", SessionID: strPtr("s3"), Cost: floatPtr(1), TotalTokens: 7},

		// No session at all.
		{ID: "loose", Timestamp: now.Add(4 * time.Second), Status: "success", Provider: "openai", Cost: floatPtr(1), TotalTokens: 3},
	}
	for i := range seed {
		require.NoError(t, db.Create(&seed[i]).Error)
	}

	return &RDBLogStore{db: db, logger: bifrost.NewDefaultLogger(schemas.LogLevelInfo)}, now
}

// Session grouping collapses every row sharing a session_id onto that session's
// earliest chain root, and rolls the whole session up onto it.
func TestSearchLogsSessionGrouping(t *testing.T) {
	s, _ := newSessionGroupStore(t)
	ctx := context.Background()

	t.Run("collapses each session to its earliest chain root", func(t *testing.T) {
		result, err := s.SearchLogs(ctx, SearchFilters{RootsOnly: true, GroupSessions: true}, PaginationOptions{Limit: 50})
		require.NoError(t, err)
		require.ElementsMatch(t, []string{"s1-turn1", "s2-root", "s3-only", "loose"}, logIDs(result.Logs))
		require.EqualValues(t, 4, result.Pagination.TotalCount)
	})

	// The case that breaks if the anti-join forgets to require the peer to be a
	// chain root: s2-child is earlier than s2-root, but it is already hidden by
	// roots_only, so letting it suppress s2-root would drop the session entirely.
	t.Run("session whose earliest row is a fallback child still lists", func(t *testing.T) {
		result, err := s.SearchLogs(ctx, SearchFilters{RootsOnly: true, GroupSessions: true}, PaginationOptions{Limit: 50})
		require.NoError(t, err)
		require.Contains(t, logIDs(result.Logs), "s2-root")
	})

	t.Run("session root carries the whole session's rollup", func(t *testing.T) {
		result, err := s.SearchLogs(ctx, SearchFilters{RootsOnly: true, GroupSessions: true}, PaginationOptions{Limit: 50})
		require.NoError(t, err)

		byID := map[string]Log{}
		for _, log := range result.Logs {
			byID[log.ID] = log
		}

		s1 := byID["s1-turn1"]
		require.EqualValues(t, 3, s1.SessionChildCount)
		require.InDelta(t, 15, s1.SessionTotalCost, 1e-9)
		require.EqualValues(t, 150, s1.SessionTotalTokens)

		s2 := byID["s2-root"]
		require.EqualValues(t, 1, s2.SessionChildCount)
		require.InDelta(t, 4, s2.SessionTotalCost, 1e-9)
		require.EqualValues(t, 20, s2.SessionTotalTokens)
	})

	t.Run("single-row session and session-less rows stay ordinary", func(t *testing.T) {
		result, err := s.SearchLogs(ctx, SearchFilters{RootsOnly: true, GroupSessions: true}, PaginationOptions{Limit: 50})
		require.NoError(t, err)
		for _, log := range result.Logs {
			if log.ID == "s3-only" || log.ID == "loose" {
				require.Zero(t, log.SessionChildCount)
				require.Zero(t, log.SessionTotalCost)
				require.Zero(t, log.SessionTotalTokens)
			}
		}
	})

	t.Run("filtering out the earliest turn promotes the next one", func(t *testing.T) {
		result, err := s.SearchLogs(ctx, SearchFilters{RootsOnly: true, GroupSessions: true, Providers: []string{"anthropic"}}, PaginationOptions{Limit: 50})
		require.NoError(t, err)
		// Only s1-turn2-retry is anthropic. Its parent is filtered out, so it is
		// promoted to chain root, and it is then the session's only row.
		require.Equal(t, []string{"s1-turn2-retry"}, logIDs(result.Logs))
	})

	t.Run("ignored for queries already scoped to one group or row", func(t *testing.T) {
		bySession, err := s.SearchLogs(ctx, SearchFilters{RootsOnly: true, GroupSessions: true, SessionID: "s1"}, PaginationOptions{Limit: 50})
		require.NoError(t, err)
		require.ElementsMatch(t, []string{"s1-turn1", "s1-turn2", "s1-turn3"}, logIDs(bySession.Logs))

		byParent, err := s.SearchLogs(ctx, SearchFilters{RootsOnly: true, GroupSessions: true, ParentRequestID: "s1-turn2"}, PaginationOptions{Limit: 50})
		require.NoError(t, err)
		require.Equal(t, []string{"s1-turn2-retry"}, logIDs(byParent.Logs))

		byID, err := s.SearchLogs(ctx, SearchFilters{RootsOnly: true, GroupSessions: true, RequestID: "s1-turn3"}, PaginationOptions{Limit: 50})
		require.NoError(t, err)
		require.Equal(t, []string{"s1-turn3"}, logIDs(byID.Logs))
	})

	t.Run("roots_only alone is unchanged by the new flag", func(t *testing.T) {
		result, err := s.SearchLogs(ctx, SearchFilters{RootsOnly: true}, PaginationOptions{Limit: 50})
		require.NoError(t, err)
		require.ElementsMatch(t, []string{"s1-turn1", "s1-turn2", "s1-turn3", "s2-root", "s3-only", "loose"}, logIDs(result.Logs))
		for _, log := range result.Logs {
			require.Zero(t, log.SessionChildCount, "session aggregates leaked into a plain roots_only query for %s", log.ID)
		}
	})

	// The invariant the two-level view rests on: every row the flat view lists is
	// reachable from the grouped view exactly once, through the session root's
	// own chain plus the session's other chain roots and their chains.
	t.Run("grouped view reaches every filtered row exactly once", func(t *testing.T) {
		for _, f := range []SearchFilters{
			{},
			{Status: []string{"success"}},
			{Providers: []string{"openai"}},
		} {
			flat, err := s.SearchLogs(ctx, f, PaginationOptions{Limit: 50})
			require.NoError(t, err)

			groupFilters := f
			groupFilters.RootsOnly = true
			groupFilters.GroupSessions = true
			roots, err := s.SearchLogs(ctx, groupFilters, PaginationOptions{Limit: 50})
			require.NoError(t, err)

			var seen []string
			expand := func(parentID string) {
				childFilters := f
				childFilters.ParentRequestID = parentID
				children, err := s.SearchLogs(ctx, childFilters, PaginationOptions{Limit: 50})
				require.NoError(t, err)
				seen = append(seen, logIDs(children.Logs)...)
			}
			for _, root := range roots.Logs {
				seen = append(seen, root.ID)
				expand(root.ID)
				if root.SessionID == nil {
					continue
				}
				memberFilters := f
				memberFilters.RootsOnly = true
				memberFilters.SessionID = *root.SessionID
				members, err := s.SearchLogs(ctx, memberFilters, PaginationOptions{Limit: 50})
				require.NoError(t, err)
				for _, member := range members.Logs {
					if member.ID == root.ID {
						continue
					}
					seen = append(seen, member.ID)
					expand(member.ID)
				}
			}

			require.ElementsMatch(t, logIDs(flat.Logs), seen, "grouped view lost or duplicated rows under %+v", f)
		}
	})
}

func TestCanUseMatViewFiltersRejectsSessionGrouping(t *testing.T) {
	// Active grouping collapses rows, so the matview's pre-aggregated counts do
	// not describe the result set.
	require.False(t, canUseMatViewFilters(SearchFilters{GroupSessions: true, RootsOnly: true}))

	// Inactive grouping changes nothing about which rows match, so the matview
	// still answers correctly and there is no reason to pay for the raw scan.
	require.True(t, canUseMatViewFilters(SearchFilters{GroupSessions: true}))
}

// canUseMatViewFilters leans on RootsOnly alone to exclude every query that
// actually groups. That is only sound while grouping implies RootsOnly, so pin
// the implication here rather than leaving it as an unwritten assumption.
func TestSessionGroupingActiveImpliesRootsOnly(t *testing.T) {
	for _, f := range []SearchFilters{
		{GroupSessions: true},
		{GroupSessions: true, RootsOnly: true},
		{GroupSessions: true, RootsOnly: true, SessionID: "s1"},
		{GroupSessions: true, RootsOnly: true, ParentRequestID: "p1"},
		{GroupSessions: true, RootsOnly: true, RequestID: "r1"},
		{GroupSessions: false, RootsOnly: true},
	} {
		if f.SessionGroupingActive() {
			require.True(t, f.RootsOnly, "grouping active without RootsOnly under %+v: canUseMatViewFilters would wrongly allow the matview", f)
		}
	}
}

// An empty session_id is not a session. The collapse query has to agree with
// attachSessionAggregates, which already treats "" like NULL, or several
// sessionless rows fold onto a single row that then carries no rollup at all.
func TestSearchLogsSessionGroupingTreatsEmptySessionIDAsNoSession(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	require.NoError(t, db.AutoMigrate(&Log{}))

	now := time.Now()
	empty := ""
	seed := []Log{
		{ID: "empty-a", Timestamp: now, Status: "success", Provider: "openai", SessionID: &empty},
		{ID: "empty-b", Timestamp: now.Add(time.Second), Status: "success", Provider: "openai", SessionID: &empty},
		{ID: "null-c", Timestamp: now.Add(2 * time.Second), Status: "success", Provider: "openai"},
	}
	for i := range seed {
		require.NoError(t, db.Create(&seed[i]).Error)
	}
	s := &RDBLogStore{db: db, logger: bifrost.NewDefaultLogger(schemas.LogLevelInfo)}

	result, err := s.SearchLogs(context.Background(), SearchFilters{RootsOnly: true, GroupSessions: true}, PaginationOptions{Limit: 50})
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"empty-a", "empty-b", "null-c"}, logIDs(result.Logs),
		"rows with an empty session_id were collapsed as though they shared a session")
	require.EqualValues(t, 3, result.Pagination.TotalCount)
}

func TestMetricTrend(t *testing.T) {
	assert.Nil(t, metricTrend(0, 5), "zero to nonzero has no percentage")
	require.NotNil(t, metricTrend(0, 0))
	assert.Zero(t, *metricTrend(0, 0), "zero to zero is genuinely unchanged")
	require.NotNil(t, metricTrend(10, 15))
	assert.InDelta(t, 50.0, *metricTrend(10, 15), 0.001)
	require.NotNil(t, metricTrend(10, 0))
	assert.InDelta(t, -100.0, *metricTrend(10, 0), 0.001)
}

// trendRow is one entity's traffic in the previous and current hour. Each
// entity gets its own team, user and model so every ranking sees it alone.
type trendRow struct {
	id                    string
	prevTokens, curTokens int
	prevCost, curCost     float64
}

// newRankingTrendTestStore seeds one request per entity in each of the previous
// and current hour, and returns the filters for the current hour.
func newRankingTrendTestStore(t *testing.T, rows []trendRow) (*RDBLogStore, SearchFilters) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&Log{}))

	now := time.Now().UTC()
	insert := func(logID, entity string, ts time.Time, tokens int, cost float64) {
		id := entity
		require.NoError(t, db.Create(&Log{
			ID:          logID,
			Timestamp:   ts,
			Status:      "success",
			Provider:    "openai",
			Model:       entity,
			TeamID:      &id,
			UserID:      &id,
			TotalTokens: tokens,
			Cost:        &cost,
		}).Error)
	}
	for _, r := range rows {
		insert(r.id+"-prev", r.id, now.Add(-90*time.Minute), r.prevTokens, r.prevCost)
		insert(r.id+"-cur", r.id, now.Add(-5*time.Minute), r.curTokens, r.curCost)
	}

	start := now.Add(-time.Hour)
	return &RDBLogStore{db: db, logger: bifrost.NewDefaultLogger(schemas.LogLevelInfo)}, SearchFilters{StartTime: &start, EndTime: &now}
}

// A previous period that had requests but no tokens or cost - cache hits, a
// free model - gives the current period nothing to be a percentage of. Reporting
// 0% there would claim the metric held steady when it went from nothing to
// something; the trend must be null instead, while has_previous_period stays
// true because the request history is real.
func TestRankingTrendsZeroBaseline(t *testing.T) {
	rows := []trendRow{
		{id: "from-zero", prevTokens: 0, curTokens: 100, prevCost: 0, curCost: 0.5},
		{id: "flat", prevTokens: 100, curTokens: 100, prevCost: 0.5, curCost: 0.5},
		{id: "still-zero", prevTokens: 0, curTokens: 0, prevCost: 0, curCost: 0},
	}
	s, filters := newRankingTrendTestStore(t, rows)
	ctx := context.Background()

	check := func(t *testing.T, id string, has bool, requests float64, tokens, cost *float64) {
		t.Helper()
		assert.True(t, has, "%s: request history is present in both periods", id)
		assert.Zero(t, requests, "%s: one request in each period", id)
		switch id {
		case "from-zero":
			assert.Nil(t, tokens, "zero-to-nonzero tokens must not read as 0%")
			assert.Nil(t, cost, "zero-to-nonzero cost must not read as 0%")
		case "flat", "still-zero":
			require.NotNil(t, tokens, "%s: tokens trend is a real 0%%", id)
			require.NotNil(t, cost, "%s: cost trend is a real 0%%", id)
			assert.Zero(t, *tokens)
			assert.Zero(t, *cost)
		}
	}

	t.Run("dimension", func(t *testing.T) {
		res, err := s.GetDimensionRankings(ctx, filters, RankingDimensionTeam)
		require.NoError(t, err)
		require.Len(t, res.Rankings, len(rows))
		for _, r := range res.Rankings {
			check(t, r.ID, r.Trend.HasPreviousPeriod, r.Trend.RequestsTrend, r.Trend.TokensTrend, r.Trend.CostTrend)
		}
	})
	t.Run("user", func(t *testing.T) {
		res, err := s.GetUserRankings(ctx, filters)
		require.NoError(t, err)
		require.Len(t, res.Rankings, len(rows))
		for _, r := range res.Rankings {
			check(t, r.UserID, r.Trend.HasPreviousPeriod, r.Trend.RequestsTrend, r.Trend.TokensTrend, r.Trend.CostTrend)
		}
	})
	t.Run("model", func(t *testing.T) {
		res, err := s.GetModelRankings(ctx, filters)
		require.NoError(t, err)
		require.Len(t, res.Rankings, len(rows))
		for _, r := range res.Rankings {
			check(t, r.Model, r.Trend.HasPreviousPeriod, r.Trend.RequestsTrend, r.Trend.TokensTrend, r.Trend.CostTrend)
		}
	})
}

// Routing rule, provider key, alias and complexity tier are all filters on the
// Logs page, so "how much went through this rule" could be checked one rule at a
// time and "which rule takes the most traffic" could not be asked at all. Each
// ranks like any other single-owner dimension: rows that never had one are left
// out rather than bucketed, and the tool's own filters still apply.
func TestRoutingDimensionsRank(t *testing.T) {
	store, _ := newFanoutTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	row := func(id, provider, rule, ruleName, key, keyName, alias, tier, mechanism string, cost float64) {
		opt := func(v string) *string {
			if v == "" {
				return nil
			}
			return &v
		}
		require.NoError(t, store.Create(ctx, &Log{
			ID: id, Timestamp: base, CreatedAt: base, Object: "chat_completion", Provider: provider, Model: "m",
			Status: "success", Cost: &cost, TotalTokens: 10,
			RoutingRuleID: opt(rule), RoutingRuleName: opt(ruleName), SelectedKeyID: key, SelectedKeyName: keyName,
			Alias: opt(alias), ComplexityTier: opt(tier), ComplexityMechanism: opt(mechanism),
		}))
	}
	row("a", "anthropic", "rule-premium", "Premium tier", "key-1", "anthropic-primary", "smart", "COMPLEX", "semantic", 3)
	row("b", "anthropic", "rule-premium", "Premium tier", "key-1", "anthropic-primary", "smart", "COMPLEX", "semantic", 2)
	row("c", "openai", "rule-budget", "Budget tier", "key-2", "openai-primary", "fast", "SIMPLE", "llm", 1)
	row("d", "openai", "", "", "key-2", "openai-primary", "", "", "", 1)

	start, end := base.Add(-time.Hour), base.Add(time.Hour)
	filters := SearchFilters{StartTime: &start, EndTime: &end}
	ranked := func(dimension RankingDimension, f SearchFilters) map[string]DimensionRankingWithTrend {
		res, err := store.GetDimensionRankings(ctx, f, dimension)
		require.NoError(t, err, "dimension %s", dimension)
		return rankingByID(res)
	}

	rules := ranked(RankingDimensionRoutingRule, filters)
	require.Len(t, rules, 2, "the request no rule handled names no rule")
	assert.Equal(t, int64(2), rules["rule-premium"].TotalRequests)
	assert.Equal(t, "Premium tier", rules["rule-premium"].Name, "a rule is reported by its name, not only its id")
	assert.InDelta(t, 5.0, rules["rule-premium"].TotalCost, 1e-9)

	keys := ranked(RankingDimensionSelectedKey, filters)
	assert.Equal(t, int64(2), keys["key-2"].TotalRequests)
	assert.Equal(t, "openai-primary", keys["key-2"].Name)

	assert.Equal(t, int64(2), ranked(RankingDimensionAlias, filters)["smart"].TotalRequests)
	assert.Equal(t, int64(1), ranked(RankingDimensionComplexityTier, filters)["SIMPLE"].TotalRequests)
	assert.Equal(t, int64(2), ranked(RankingDimensionComplexityMechanism, filters)["semantic"].TotalRequests)

	// Composes with filters: which rules send traffic to Anthropic.
	toAnthropic := filters
	toAnthropic.Providers = []string{"anthropic"}
	assert.Len(t, ranked(RankingDimensionRoutingRule, toAnthropic), 1)

	// The complexity columns are not in the hourly matview. Read from it, they
	// would raise a shape error and trip the matview self-heal for a query that
	// was never the matview's to serve.
	for _, dimension := range []RankingDimension{RankingDimensionComplexityTier, RankingDimensionComplexityMechanism} {
		assert.True(t, dimensionColumns[dimension].RawOnly, "%s must not use the matview path", dimension)
	}
	// Ranked for in-process callers without widening the HTTP rankings endpoint.
	assert.False(t, ValidRankingDimensions[RankingDimensionRoutingRule])
}

// Routing engines and tool-call names are comma-separated on the row, because a
// request can pass through several engines and call several tools. A request
// that called two tools counts under both, so the attributed total can exceed
// the requests there actually were - the same relationship fail_reason has.
func TestCommaListDimensionsRank(t *testing.T) {
	store, _ := newFanoutTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	// Set through the slice fields: the write hook joins them into the columns,
	// which is the only way they are ever populated.
	row := func(id string, ts time.Time, tools, engines []string, cost float64) {
		require.NoError(t, store.Create(ctx, &Log{
			ID: id, Timestamp: ts, CreatedAt: ts, Object: "chat_completion", Provider: "openai", Model: "m",
			Status: "success", Cost: &cost, TotalTokens: 10,
			ToolCallNames: tools, RoutingEnginesUsed: engines,
		}))
	}
	row("a", base, []string{"get_weather", "search_docs"}, []string{"routing-rule", "loadbalancing"}, 2)
	row("b", base, []string{"get_weather"}, []string{"governance"}, 1)
	row("c", base, []string{" search_docs ", ""}, []string{"routing-rule"}, 1)
	row("d", base, nil, nil, 5)
	// The period before, for the trend: one get_weather call.
	row("p", base.Add(-2*time.Hour), []string{"get_weather"}, []string{"governance"}, 1)

	start, end := base.Add(-time.Hour), base.Add(time.Hour)
	filters := SearchFilters{StartTime: &start, EndTime: &end}

	res, err := store.GetDimensionRankings(ctx, filters, RankingDimensionToolCallName)
	require.NoError(t, err)
	tools := rankingByID(res)
	require.Len(t, tools, 2, "the row with no tool call, and the stray empty element, name nothing")
	assert.Equal(t, int64(2), tools["get_weather"].TotalRequests)
	assert.Equal(t, int64(2), tools["search_docs"].TotalRequests)
	assert.InDelta(t, 3.0, tools["get_weather"].TotalCost, 1e-9, "a request's cost counts under each tool it called")
	assert.Equal(t, int64(4), res.TotalActualRequests)
	assert.Equal(t, int64(4), res.TotalAttributedRequests, "a and b and c, with a counted twice")
	assert.True(t, tools["get_weather"].Trend.HasPreviousPeriod)
	assert.InDelta(t, 100.0, tools["get_weather"].Trend.RequestsTrend, 1e-9)
	assert.False(t, tools["search_docs"].Trend.HasPreviousPeriod)

	engines, err := store.GetDimensionRankings(ctx, filters, RankingDimensionRoutingEngine)
	require.NoError(t, err)
	require.NotEmpty(t, engines.Rankings)
	assert.Equal(t, "routing-rule", engines.Rankings[0].ID, "most requests first")
	assert.Equal(t, int64(2), engines.Rankings[0].TotalRequests)

	// The cap applies to the names, after splitting - not to the raw combinations.
	one := 1
	capped := filters
	capped.RankingLimit = &one
	res, err = store.GetDimensionRankings(ctx, capped, RankingDimensionRoutingEngine)
	require.NoError(t, err)
	assert.Len(t, res.Rankings, 1)
}

// TestBatchCreateIfNotExists_WriterSizedBatch pins that a batch as large as the
// log writer's default (DefaultWriterMaxBatchSize rows) is stored in full. A single
// multi-row INSERT of that size binds rows x columns parameters, which exceeds the
// per-statement parameter limit of both Postgres (65,535) and SQLite (32,766).
func TestBatchCreateIfNotExists_WriterSizedBatch(t *testing.T) {
	ctx := context.Background()
	store, err := newSqliteLogStore(ctx, &SQLiteConfig{Path: filepath.Join(t.TempDir(), "batch.db")}, testLogger{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close(ctx) })

	now := time.Now().UTC()
	entries := make([]*Log, DefaultWriterMaxBatchSize)
	for i := range entries {
		entries[i] = &Log{
			ID:        fmt.Sprintf("batch-%04d", i),
			Timestamp: now,
			Provider:  "openai",
			Model:     "gpt-4o",
			Status:    "success",
			Object:    "chat.completion",
		}
	}
	require.NoError(t, store.BatchCreateIfNotExists(ctx, entries))
	// Re-inserting the same batch is a no-op thanks to ON CONFLICT DO NOTHING.
	require.NoError(t, store.BatchCreateIfNotExists(ctx, entries))

	var count int64
	require.NoError(t, store.db.Model(&Log{}).Count(&count).Error)
	require.Equal(t, int64(DefaultWriterMaxBatchSize), count)
}

// TestBatchCreateMCPToolLogsIfNotExists_WriterSizedBatch is the MCP tool log
// counterpart of TestBatchCreateIfNotExists_WriterSizedBatch.
func TestBatchCreateMCPToolLogsIfNotExists_WriterSizedBatch(t *testing.T) {
	ctx := context.Background()
	store, err := newSqliteLogStore(ctx, &SQLiteConfig{Path: filepath.Join(t.TempDir(), "batchmcp.db")}, testLogger{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close(ctx) })

	now := time.Now().UTC()
	entries := make([]*MCPToolLog, DefaultWriterMaxBatchSize)
	for i := range entries {
		entries[i] = &MCPToolLog{
			ID:        fmt.Sprintf("mcp-batch-%04d", i),
			Timestamp: now,
			ToolName:  "search_web",
			Status:    "success",
			CreatedAt: now,
		}
	}
	require.NoError(t, store.BatchCreateMCPToolLogsIfNotExists(ctx, entries))

	var count int64
	require.NoError(t, store.db.Model(&MCPToolLog{}).Count(&count).Error)
	require.Equal(t, int64(DefaultWriterMaxBatchSize), count)
}

// TestInsertBatchSize_StaysUnderParameterLimit pins that the computed chunk size
// keeps every multi-row INSERT under the smallest supported parameter limit.
func TestInsertBatchSize_StaysUnderParameterLimit(t *testing.T) {
	ctx := context.Background()
	store, err := newSqliteLogStore(ctx, &SQLiteConfig{Path: filepath.Join(t.TempDir(), "size.db")}, testLogger{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close(ctx) })

	for _, model := range []any{&Log{}, &MCPToolLog{}} {
		size, columns := insertBatchSize(store.db, model)
		require.Positive(t, size)
		require.LessOrEqual(t, size*columns, maxInsertBindParams, "%T", model)
	}
}

// TestBatchCreateIfNotExists_WriterSizedBatchPostgres is the Postgres pin for
// issue #7843: a writer-sized batch (DefaultWriterMaxBatchSize rows x 118 bound
// columns) used to be sent as one INSERT, which pgx rejects with "extended
// protocol limited to 65535 parameters", so every full batch fell back to
// row-by-row inserts. The batch must land in full with no error.
func TestBatchCreateIfNotExists_WriterSizedBatchPostgres(t *testing.T) {
	db := trySetupPostgresDB(t)
	if db == nil {
		t.Skip("Postgres not available")
	}
	ctx := context.Background()
	dropAllManagedMatViews(db)
	require.NoError(t, db.Exec("DROP TABLE IF EXISTS mcp_tool_logs CASCADE").Error)
	require.NoError(t, db.Exec("DROP TABLE IF EXISTS async_jobs CASCADE").Error)
	require.NoError(t, db.Exec("DROP TABLE IF EXISTS webhook_deliveries CASCADE").Error)
	require.NoError(t, db.Exec("DROP TABLE IF EXISTS logs CASCADE").Error)
	require.NoError(t, db.Exec("CREATE TABLE IF NOT EXISTS migrations (id VARCHAR(255) PRIMARY KEY)").Error)
	require.NoError(t, db.Exec("DELETE FROM migrations").Error)
	require.NoError(t, triggerMigrations(ctx, db, testLogger{}))
	t.Cleanup(func() {
		_ = db.Exec("TRUNCATE logs").Error
		_ = db.Exec("TRUNCATE mcp_tool_logs").Error
	})
	store := &RDBLogStore{db: db, logger: testLogger{}}

	now := time.Now().UTC()
	entries := make([]*Log, DefaultWriterMaxBatchSize)
	for i := range entries {
		entries[i] = &Log{
			ID:        fmt.Sprintf("pg-batch-%04d", i),
			Timestamp: now,
			Provider:  "openai",
			Model:     "gpt-4o",
			Status:    "success",
			Object:    "chat.completion",
		}
	}
	require.NoError(t, store.BatchCreateIfNotExists(ctx, entries), "a writer-sized batch must not exceed the Postgres bind-parameter limit")
	// Idempotent: ON CONFLICT DO NOTHING must survive chunking.
	require.NoError(t, store.BatchCreateIfNotExists(ctx, entries))

	var count int64
	require.NoError(t, store.db.Model(&Log{}).Count(&count).Error)
	require.Equal(t, int64(DefaultWriterMaxBatchSize), count)

	// MCP tool logs stay under the limit at the default batch size (43 columns)
	// but must also be chunked for any configured max_batch_size.
	mcp := make([]*MCPToolLog, 2000)
	for i := range mcp {
		mcp[i] = &MCPToolLog{
			ID:        fmt.Sprintf("pg-mcp-batch-%04d", i),
			Timestamp: now,
			ToolName:  "search_web",
			Status:    "success",
			CreatedAt: now,
		}
	}
	require.NoError(t, store.BatchCreateMCPToolLogsIfNotExists(ctx, mcp), "a 2000-row MCP tool log batch must not exceed the Postgres bind-parameter limit")
	require.NoError(t, store.db.Model(&MCPToolLog{}).Count(&count).Error)
	require.Equal(t, int64(2000), count)
}

// Each count is one more than the database's per-statement bind parameter limit
// (SQLite 32,766, Postgres 65,535), so a previous-period query that binds one
// parameter per ranked id fails on that database.
const (
	exportInListUsersSQLite   = 32_767
	exportInListUsersPostgres = 65_536
)

// seedExportInListLogs inserts one current-period and one previous-period log for
// each of n distinct users and virtual keys, and returns filters covering the
// current period with the ranking limit disabled (export mode).
func seedExportInListLogs(t *testing.T, ctx context.Context, store *RDBLogStore, n int) SearchFilters {
	t.Helper()
	end := time.Now().UTC().Truncate(time.Second)
	start := end.Add(-time.Hour)
	entries := make([]*Log, 0, 2*n)
	for i := 0; i < n; i++ {
		userID := fmt.Sprintf("user-%05d", i)
		vkID := fmt.Sprintf("vk-%05d", i)
		for period, ts := range []time.Time{start.Add(30 * time.Minute), start.Add(-30 * time.Minute)} {
			entries = append(entries, &Log{
				ID:           fmt.Sprintf("log-%05d-%d", i, period),
				Timestamp:    ts,
				Provider:     "openai",
				Model:        "gpt-4o",
				Status:       "success",
				Object:       "chat.completion",
				UserID:       &userID,
				VirtualKeyID: &vkID,
			})
		}
	}
	require.NoError(t, store.BatchCreateIfNotExists(ctx, entries))
	unlimited := 0
	return SearchFilters{StartTime: &start, EndTime: &end, RankingLimit: &unlimited}
}

// assertExportRankingsCoverAllIDs runs the export-mode ranking queries against a
// store seeded by seedExportInListLogs and checks every one of the n ids comes
// back with its previous-period trend.
func assertExportRankingsCoverAllIDs(t *testing.T, ctx context.Context, store *RDBLogStore, filters SearchFilters, n int) {
	t.Helper()
	t.Run("GetUserRankings", func(t *testing.T) {
		res, err := store.GetUserRankings(ctx, filters)
		require.NoError(t, err)
		require.Len(t, res.Rankings, n)
		for _, r := range res.Rankings {
			require.True(t, r.Trend.HasPreviousPeriod, "user %s missing previous period", r.UserID)
		}
	})

	for _, dim := range []RankingDimension{RankingDimensionUser, RankingDimensionVirtualKey} {
		t.Run("GetDimensionRankings/"+string(dim), func(t *testing.T) {
			res, err := store.GetDimensionRankings(ctx, filters, dim)
			require.NoError(t, err)
			require.Len(t, res.Rankings, n)
			for _, r := range res.Rankings {
				require.True(t, r.Trend.HasPreviousPeriod, "%s %s missing previous period", dim, r.ID)
			}
		})
	}
}

// TestRankingsExport_PreviousPeriodInListOverParamLimit pins that the export
// (RankingLimit <= 0) ranking queries still work on SQLite when the
// previous-period id filter holds more ids than the database accepts as bind
// parameters. Before the fix each id was bound as its own parameter and SQLite
// rejected the statement with "too many SQL variables".
func TestRankingsExport_PreviousPeriodInListOverParamLimit(t *testing.T) {
	if testing.Short() {
		t.Skip("inserts 65k rows")
	}
	ctx := context.Background()
	store, err := newSqliteLogStore(ctx, &SQLiteConfig{Path: filepath.Join(t.TempDir(), "export.db")}, testLogger{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close(ctx) })
	filters := seedExportInListLogs(t, ctx, store, exportInListUsersSQLite)
	assertExportRankingsCoverAllIDs(t, ctx, store, filters, exportInListUsersSQLite)
}

// TestRankingsExport_PreviousPeriodInListOverParamLimitPostgres is the Postgres
// counterpart: above 65,535 ids the old per-id binding failed with "extended
// protocol limited to 65535 parameters". Skips when Postgres is unavailable.
func TestRankingsExport_PreviousPeriodInListOverParamLimitPostgres(t *testing.T) {
	if testing.Short() {
		t.Skip("inserts 131k rows")
	}
	db := trySetupPostgresDB(t)
	if db == nil {
		t.Skip("Postgres not available")
	}
	ctx := context.Background()
	// Clean slate with no matviews, so every ranking takes the raw-table path
	// that carries the previous-period id filter.
	dropAllManagedMatViews(db)
	require.NoError(t, db.Exec("DROP TABLE IF EXISTS mcp_tool_logs CASCADE").Error)
	require.NoError(t, db.Exec("DROP TABLE IF EXISTS async_jobs CASCADE").Error)
	require.NoError(t, db.Exec("DROP TABLE IF EXISTS webhook_deliveries CASCADE").Error)
	require.NoError(t, db.Exec("DROP TABLE IF EXISTS logs CASCADE").Error)
	require.NoError(t, db.Exec("CREATE TABLE IF NOT EXISTS migrations (id VARCHAR(255) PRIMARY KEY)").Error)
	require.NoError(t, db.Exec("DELETE FROM migrations").Error)
	require.NoError(t, triggerMigrations(ctx, db, testLogger{}))
	t.Cleanup(func() { _ = db.Exec("TRUNCATE logs").Error })
	store := &RDBLogStore{db: db, logger: testLogger{}}

	filters := seedExportInListLogs(t, ctx, store, exportInListUsersPostgres)
	assertExportRankingsCoverAllIDs(t, ctx, store, filters, exportInListUsersPostgres)
}

// recordingRetentionManager is a retention stub for both tables that records
// the batch size each call asked for and returns one scripted count per call.
type recordingRetentionManager struct {
	logCounts  []int64
	mcpCounts  []int64
	logBatches []int
	mcpBatches []int
}

// DeleteLogsBatch records the batch size and returns the next scripted count.
func (m *recordingRetentionManager) DeleteLogsBatch(_ context.Context, _ time.Time, size int) (int64, error) {
	m.logBatches = append(m.logBatches, size)
	if len(m.logBatches) > len(m.logCounts) {
		return 0, nil
	}
	return m.logCounts[len(m.logBatches)-1], nil
}

// DeleteMCPToolLogsBatch records the batch size and returns the next scripted count.
func (m *recordingRetentionManager) DeleteMCPToolLogsBatch(_ context.Context, _ time.Time, size int) (int64, error) {
	m.mcpBatches = append(m.mcpBatches, size)
	if len(m.mcpBatches) > len(m.mcpCounts) {
		return 0, nil
	}
	return m.mcpCounts[len(m.mcpBatches)-1], nil
}

// TestHighFixesCleanerUsesLargeBatchesAndCleansMCP pins H2: the cleaner deletes
// in batches of 5000 (100 could not keep up with 5.6M expired rows per day) and
// runs the same retention over mcp_tool_logs, which previously had none.
func TestHighFixesCleanerUsesLargeBatchesAndCleansMCP(t *testing.T) {
	m := &recordingRetentionManager{
		logCounts: []int64{5000, 5000, 12},
		mcpCounts: []int64{5000, 3},
	}
	NewLogsCleaner(m, CleanerConfig{RetentionDays: 7}, testLogger{}).cleanupOldLogs(context.Background())

	require.Equal(t, []int{5000, 5000, 5000}, m.logBatches, "logs must be deleted 5000 rows at a time until a short batch")
	require.Equal(t, []int{5000, 5000}, m.mcpBatches, "mcp_tool_logs must get the same retention pass")
}

// backlogRetentionManager simulates a logs table that never finishes draining:
// every logs batch is full (or fails, when logErr is set). It counts the MCP
// batches the cleaner still gets to run.
type backlogRetentionManager struct {
	logErr     error
	mcpBatches atomic.Int32
}

// DeleteLogsBatch reports a full batch after a short pause, or logErr.
func (m *backlogRetentionManager) DeleteLogsBatch(ctx context.Context, _ time.Time, size int) (int64, error) {
	if m.logErr != nil {
		return 0, m.logErr
	}
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-time.After(2 * time.Millisecond):
	}
	return int64(size), nil
}

// DeleteMCPToolLogsBatch counts the call and reports a short batch.
func (m *backlogRetentionManager) DeleteMCPToolLogsBatch(_ context.Context, _ time.Time, _ int) (int64, error) {
	m.mcpBatches.Add(1)
	return 3, nil
}

// TestHighFixesCleanerMCPNotStarvedByLogsBacklog pins that mcp_tool_logs get a
// share of every retention pass: a logs backlog that outlasts the pass deadline,
// or a logs delete that keeps failing, must not skip MCP retention forever. A
// pass that was stopped (parent ctx cancelled) still ends without touching MCP.
func TestHighFixesCleanerMCPNotStarvedByLogsBacklog(t *testing.T) {
	t.Run("logs backlog outlasts the deadline", func(t *testing.T) {
		m := &backlogRetentionManager{}
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		NewLogsCleaner(m, CleanerConfig{RetentionDays: 7}, testLogger{}).cleanupOldLogs(ctx)
		require.Positive(t, m.mcpBatches.Load(), "a logs backlog must not starve MCP tool-log retention")
	})
	t.Run("logs delete fails", func(t *testing.T) {
		m := &backlogRetentionManager{logErr: fmt.Errorf("lock timeout")}
		NewLogsCleaner(m, CleanerConfig{RetentionDays: 7}, testLogger{}).cleanupOldLogs(context.Background())
		require.Positive(t, m.mcpBatches.Load(), "a failing logs delete must not skip MCP tool-log retention")
	})
	t.Run("stopped pass", func(t *testing.T) {
		m := &backlogRetentionManager{}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		NewLogsCleaner(m, CleanerConfig{RetentionDays: 7}, testLogger{}).cleanupOldLogs(ctx)
		require.Zero(t, m.mcpBatches.Load(), "a stopped pass must not start MCP retention")
	})
}

// hfInsertLogAt inserts a minimal logs row with explicit timestamp and created_at.
func hfInsertLogAt(t *testing.T, store *RDBLogStore, id string, ts time.Time) {
	t.Helper()
	require.NoError(t, store.Create(context.Background(), &Log{
		ID: id, Timestamp: ts, CreatedAt: ts, Object: "chat_completion",
		Provider: "openai", Model: "gpt-4o", Status: "success",
	}))
}

// hfInsertMCPAt inserts a minimal mcp_tool_logs row with explicit timestamps.
func hfInsertMCPAt(t *testing.T, store *RDBLogStore, id string, ts time.Time) {
	t.Helper()
	require.NoError(t, store.CreateMCPToolLog(context.Background(), &MCPToolLog{
		ID: id, RequestID: id, Timestamp: ts, CreatedAt: ts, ToolName: "search", Status: "success",
	}))
}

// hfLogIDs returns every id in logs, sorted.
func hfLogIDs(t *testing.T, db *gorm.DB, table string) []string {
	t.Helper()
	var ids []string
	require.NoError(t, db.Table(table).Order("id").Pluck("id", &ids).Error)
	return ids
}

// TestHighFixesDeleteLogsBatchOldestFirst pins that a retention batch takes the
// oldest expired rows first (ORDER BY on the indexed cutoff column), so each
// batch walks the index from its low end instead of an arbitrary heap order.
func TestHighFixesDeleteLogsBatchOldestFirst(t *testing.T) {
	store := newTestSQLiteStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// Inserted newest-expired first so insertion order disagrees with age.
	hfInsertLogAt(t, store, "old-10d", now.AddDate(0, 0, -10))
	hfInsertLogAt(t, store, "old-20d", now.AddDate(0, 0, -20))
	hfInsertLogAt(t, store, "old-30d", now.AddDate(0, 0, -30))
	hfInsertLogAt(t, store, "fresh", now)

	deleted, err := store.DeleteLogsBatch(ctx, now.AddDate(0, 0, -5), 2)
	require.NoError(t, err)
	require.Equal(t, int64(2), deleted)
	require.Equal(t, []string{"fresh", "old-10d"}, hfLogIDs(t, store.db, "logs"), "the two oldest expired rows go first")

	hfInsertMCPAt(t, store, "m-old-10d", now.AddDate(0, 0, -10))
	hfInsertMCPAt(t, store, "m-old-20d", now.AddDate(0, 0, -20))
	hfInsertMCPAt(t, store, "m-old-30d", now.AddDate(0, 0, -30))
	hfInsertMCPAt(t, store, "m-fresh", now)
	deleted, err = store.DeleteMCPToolLogsBatch(ctx, now.AddDate(0, 0, -5), 2)
	require.NoError(t, err)
	require.Equal(t, int64(2), deleted)
	require.Equal(t, []string{"m-fresh", "m-old-10d"}, hfLogIDs(t, store.db, "mcp_tool_logs"))
}

// TestHighFixesCleanerRetentionEndToEnd runs the real cleaner over a SQLite
// store: rows older than the retention cutoff go from both tables, newer rows
// stay, and the cutoff column for logs is still created_at.
func TestHighFixesCleanerRetentionEndToEnd(t *testing.T) {
	store := newTestSQLiteStore(t)
	now := time.Now().UTC()

	for i := range 7 {
		hfInsertLogAt(t, store, fmt.Sprintf("log-old-%d", i), now.AddDate(0, 0, -30-i))
		hfInsertMCPAt(t, store, fmt.Sprintf("mcp-old-%d", i), now.AddDate(0, 0, -30-i))
	}
	hfInsertLogAt(t, store, "log-new", now.AddDate(0, 0, -1))
	hfInsertMCPAt(t, store, "mcp-new", now.AddDate(0, 0, -1))
	// timestamp inside retention but created_at outside it: logs retention is
	// keyed on created_at, exactly as before.
	require.NoError(t, store.Create(context.Background(), &Log{
		ID: "log-created-old", Timestamp: now, CreatedAt: now.AddDate(0, 0, -30), Object: "chat_completion",
		Provider: "openai", Model: "gpt-4o", Status: "success",
	}))

	NewLogsCleaner(store, CleanerConfig{RetentionDays: 7}, testLogger{}).cleanupOldLogs(context.Background())

	assert.Equal(t, []string{"log-new"}, hfLogIDs(t, store.db, "logs"))
	assert.Equal(t, []string{"mcp-new"}, hfLogIDs(t, store.db, "mcp_tool_logs"))
}

// hfOwnerTimestampIndexes are the H4 composite indexes, per table.
var hfOwnerTimestampIndexes = map[string][]string{
	"logs":          {"idx_logs_vk_ts", "idx_logs_user_ts", "idx_logs_team_ts"},
	"mcp_tool_logs": {"idx_mcp_logs_vk_ts", "idx_mcp_logs_user_ts", "idx_mcp_logs_team_ts"},
}

// hfReplacedSingleIndexes are the single-column indexes the composites replace.
var hfReplacedSingleIndexes = map[string][]string{
	"logs":          {"idx_logs_virtual_key_id", "idx_logs_user_id", "idx_logs_team_id"},
	"mcp_tool_logs": {"idx_mcp_logs_virtual_key_id", "idx_mcp_logs_user_id", "idx_mcp_logs_team_id"},
}

// TestHighFixesCreateTableBuildsOwnerTimestampIndexes pins that a fresh install
// (CreateTable from the struct tags) builds the (owner, timestamp) composites,
// and that a migrated SQLite store ends with the composites and without the
// single-column indexes they replace.
func TestHighFixesCreateTableBuildsOwnerTimestampIndexes(t *testing.T) {
	dsn := "file:" + url.QueryEscape(t.Name()) + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, db.Migrator().CreateTable(&Log{}, &MCPToolLog{}))
	for _, name := range hfOwnerTimestampIndexes["logs"] {
		assert.True(t, db.Migrator().HasIndex(&Log{}, name), "fresh logs table must have %s", name)
	}
	for _, name := range hfOwnerTimestampIndexes["mcp_tool_logs"] {
		assert.True(t, db.Migrator().HasIndex(&MCPToolLog{}, name), "fresh mcp_tool_logs table must have %s", name)
	}

	store := newTestSQLiteStore(t)
	for table, names := range hfOwnerTimestampIndexes {
		for _, name := range names {
			assert.True(t, store.db.Migrator().HasIndex(table, name), "migrated %s must have %s", table, name)
		}
	}
	for table, names := range hfReplacedSingleIndexes {
		for _, name := range names {
			assert.False(t, store.db.Migrator().HasIndex(table, name), "migrated %s must not keep %s", table, name)
		}
	}
}

// hfRankingNameIndexes are the (id, timestamp) composites behind the ranking
// name fallback, each with the single-column index it replaces.
var hfRankingNameIndexes = []struct{ name, column, replaces string }{
	{"idx_logs_selected_key_ts", "selected_key_id", "idx_logs_selected_key_id"},
	{"idx_logs_routing_rule_ts", "routing_rule_id", "idx_logs_routing_rule_id"},
	{"idx_logs_customer_ts", "customer_id", "idx_logs_customer_id"},
	{"idx_logs_business_unit_ts", "business_unit_id", "idx_logs_business_unit_id"},
	{"idx_logs_project_ts", "project_id", "idx_logs_project_id"},
}

// TestHighFixesRankingNameIndexes pins that a fresh install builds the
// (id, timestamp) composites the ranking name fallback walks, and that a
// migrated SQLite store ends with them and without the single-column indexes
// they replace.
func TestHighFixesRankingNameIndexes(t *testing.T) {
	dsn := "file:" + url.QueryEscape(t.Name()) + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, db.Migrator().CreateTable(&Log{}))
	for _, idx := range hfRankingNameIndexes {
		assert.True(t, db.Migrator().HasIndex(&Log{}, idx.name), "fresh logs table must have %s", idx.name)
	}

	store := newTestSQLiteStore(t)
	for _, idx := range hfRankingNameIndexes {
		assert.True(t, store.db.Migrator().HasIndex("logs", idx.name), "migrated logs must have %s", idx.name)
		assert.False(t, store.db.Migrator().HasIndex("logs", idx.replaces), "migrated logs must not keep %s", idx.replaces)
	}
}

// hfBulkInsertLogs inserts n logs rows in one statement through a recursive CTE.
func hfBulkInsertLogs(t *testing.T, db *gorm.DB, prefix string, n int) {
	t.Helper()
	require.NoError(t, db.Exec(`
		WITH RECURSIVE seq(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM seq WHERE x < ?)
		INSERT INTO logs (id, timestamp, object_type, provider, model, status, created_at)
		SELECT ? || x, CURRENT_TIMESTAMP, 'chat_completion', 'openai', 'gpt-4o', 'success', CURRENT_TIMESTAMP FROM seq
	`, n, prefix).Error)
}

// hfBulkInsertMCP inserts n mcp_tool_logs rows in one statement.
func hfBulkInsertMCP(t *testing.T, db *gorm.DB, prefix string, n int) {
	t.Helper()
	require.NoError(t, db.Exec(`
		WITH RECURSIVE seq(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM seq WHERE x < ?)
		INSERT INTO mcp_tool_logs (id, request_id, timestamp, tool_name, status, created_at)
		SELECT ? || x, ? || x, CURRENT_TIMESTAMP, 'search', 'success', CURRENT_TIMESTAMP FROM seq
	`, n, prefix, prefix).Error)
}

// TestHighFixesSearchCountIsExact pins that the logs and MCP logs list counts
// stay exact above 10,000 matching rows, as they were before the scale work:
// the pagination footer, page count and navigation all depend on the real
// total, so the count must never stop at a cap. SkipCount still skips it.
func TestHighFixesSearchCountIsExact(t *testing.T) {
	const many = 10045
	store := newTestSQLiteStore(t)
	ctx := context.Background()

	hfBulkInsertLogs(t, store.db, "few-", 5)
	res, err := store.SearchLogs(ctx, SearchFilters{}, PaginationOptions{Limit: 10})
	require.NoError(t, err)
	assert.Equal(t, int64(5), res.Pagination.TotalCount)

	hfBulkInsertLogs(t, store.db, "many-", many)
	res, err = store.SearchLogs(ctx, SearchFilters{}, PaginationOptions{Limit: 10})
	require.NoError(t, err)
	assert.Equal(t, int64(many+5), res.Pagination.TotalCount, "the logs count must be exact above 10,000")
	assert.Len(t, res.Logs, 10)

	res, err = store.SearchLogs(ctx, SearchFilters{}, PaginationOptions{Limit: 10, SkipCount: true})
	require.NoError(t, err)
	assert.Equal(t, int64(0), res.Pagination.TotalCount, "SkipCount runs no count")
	assert.Len(t, res.Logs, 10)

	hfBulkInsertMCP(t, store.db, "mcp-few-", 3)
	mres, err := store.SearchMCPToolLogs(ctx, MCPToolLogSearchFilters{}, PaginationOptions{Limit: 10})
	require.NoError(t, err)
	assert.Equal(t, int64(3), mres.Pagination.TotalCount)

	hfBulkInsertMCP(t, store.db, "mcp-many-", many)
	mres, err = store.SearchMCPToolLogs(ctx, MCPToolLogSearchFilters{}, PaginationOptions{Limit: 10})
	require.NoError(t, err)
	assert.Equal(t, int64(many+3), mres.Pagination.TotalCount, "the MCP logs count must be exact above 10,000")
}

// TestHighFixesKeysetPagination pins the (timestamp, id) keyset cursor: pages
// taken after the last row of the previous page cover every row exactly once,
// including rows that share one timestamp.
func TestHighFixesKeysetPagination(t *testing.T) {
	store := newTestSQLiteStore(t)
	ctx := context.Background()
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	var want []string
	for i := range 11 {
		id := fmt.Sprintf("k%02d", i)
		// Pairs of rows share a timestamp.
		hfInsertLogAt(t, store, id, base.Add(time.Duration(i/2)*time.Second))
		want = append(want, id)
	}
	start := base.Add(-time.Minute)
	end := base.Add(time.Hour)
	filters := SearchFilters{StartTime: &start, EndTime: &end}
	var got []string
	pagination := PaginationOptions{Limit: 3, SortBy: "timestamp", Order: "asc", SkipCount: true}
	for range 10 {
		res, err := store.SearchLogs(ctx, filters, pagination)
		require.NoError(t, err)
		for _, l := range res.Logs {
			got = append(got, l.ID)
		}
		if len(res.Logs) < 3 {
			break
		}
		last := res.Logs[len(res.Logs)-1]
		ts := last.Timestamp
		pagination.AfterTimestamp = &ts
		pagination.AfterID = last.ID
	}
	require.Equal(t, want, got)
}

// hfQueryCapture records the SQL and bind variables of every query a gorm DB runs.
type hfQueryCapture struct {
	mu    sync.Mutex
	stmts []hfCapturedStmt
}

// hfCapturedStmt is one captured query: its SQL and the same SQL with the bind
// variables inlined by the dialect.
type hfCapturedStmt struct {
	sql      string
	rendered string
}

// hfCaptureQueries registers a query callback on db for the rest of the test.
func hfCaptureQueries(t *testing.T, db *gorm.DB) *hfQueryCapture {
	t.Helper()
	c := &hfQueryCapture{}
	name := "hf:capture:" + t.Name()
	record := func(tx *gorm.DB) {
		c.mu.Lock()
		defer c.mu.Unlock()
		sqlText := tx.Statement.SQL.String()
		c.stmts = append(c.stmts, hfCapturedStmt{sql: sqlText, rendered: tx.Dialector.Explain(sqlText, tx.Statement.Vars...)})
	}
	require.NoError(t, db.Callback().Query().After("gorm:query").Register(name, record))
	require.NoError(t, db.Callback().Row().After("gorm:row").Register(name, record))
	t.Cleanup(func() {
		_ = db.Callback().Query().Remove(name)
		_ = db.Callback().Row().Remove(name)
	})
	return c
}

// snapshot returns a copy of the captured statements.
func (c *hfQueryCapture) snapshot() []hfCapturedStmt {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]hfCapturedStmt(nil), c.stmts...)
}

// hfHasStatusList reports whether the rendered statement filters status on
// exactly the given list, whatever quote style the dialect renders.
func hfHasStatusList(s hfCapturedStmt, want []string) bool {
	plain := strings.NewReplacer(`"`, "", "'", "", " ", "").Replace(s.rendered)
	return strings.Contains(plain, "statusIN("+strings.Join(want, ",")+")")
}

// TestHighFixesErrorDimensionsBoundByFailedStatus pins H9: error_type,
// error_code and status_code rankings only read failed rows (status error or
// cancelled), so idx_logs_status_timestamp bounds the scan, and the results
// are unchanged because only failed rows carry error_details.
func TestHighFixesErrorDimensionsBoundByFailedStatus(t *testing.T) {
	store := newTestSQLiteStore(t)
	ctx := context.Background()
	base := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)

	create := func(id, status string, ts time.Time, errType, errCode string, code int) {
		l := &Log{ID: id, Timestamp: ts, CreatedAt: ts, Object: "chat_completion", Provider: "openai", Model: "gpt-4o", Status: status, TotalTokens: 10, Cost: new(0.01)}
		if errType != "" {
			l.ErrorDetailsParsed = &schemas.BifrostError{StatusCode: new(code), Error: &schemas.ErrorField{Type: new(errType), Code: new(errCode), Message: "m"}}
		}
		require.NoError(t, store.Create(ctx, l))
	}
	for i := range 5 {
		create(fmt.Sprintf("ok-%d", i), "success", base.Add(time.Duration(i)*time.Minute), "", "", 0)
	}
	create("e1", "error", base.Add(10*time.Minute), "rate_limit_error", "rate_limited", 429)
	create("e2", "error", base.Add(11*time.Minute), "rate_limit_error", "rate_limited", 429)
	create("e3", "error", base.Add(12*time.Minute), "authentication_error", "invalid_api_key", 401)
	create("c1", "cancelled", base.Add(13*time.Minute), "request_cancelled", "client_closed", 499)
	// Previous period: one error so the trend is populated.
	create("prev-e1", "error", base.Add(-30*time.Minute), "rate_limit_error", "rate_limited", 429)

	start := base.Add(-time.Minute)
	end := base.Add(time.Hour)
	filters := SearchFilters{StartTime: &start, EndTime: &end}

	capture := hfCaptureQueries(t, store.db)

	type row struct {
		id    string
		total int64
		trend bool
	}
	expect := map[RankingDimension][]row{
		RankingDimensionErrorType:  {{"rate_limit_error", 2, true}, {"authentication_error", 1, false}, {"request_cancelled", 1, false}},
		RankingDimensionErrorCode:  {{"rate_limited", 2, true}, {"client_closed", 1, false}, {"invalid_api_key", 1, false}},
		RankingDimensionStatusCode: {{"429", 2, true}, {"401", 1, false}, {"499", 1, false}},
	}
	for dim, want := range expect {
		res, err := store.GetJSONFieldDimensionRankings(ctx, filters, dim)
		require.NoError(t, err, dim)
		var got []row
		for _, r := range res.Rankings {
			got = append(got, row{r.ID, r.TotalRequests, r.Trend.HasPreviousPeriod})
		}
		assert.Equal(t, want, got, dim)
		assert.Equal(t, int64(9), res.TotalActualRequests, "actual requests still count every terminal row (%s)", dim)
		assert.Equal(t, int64(4), res.TotalAttributedRequests, dim)
	}

	bounded := 0
	for _, s := range capture.snapshot() {
		if hfHasStatusList(s, failedLogStatuses) {
			bounded++
		}
	}
	// Per dimension: current rankings, attributed count, previous period.
	assert.GreaterOrEqual(t, bounded, 9, "every error-dimension scan must carry the failed-status predicate")
}

// TestHighFixesMCPSearchIgnoresKeysetCursor pins what PaginationOptions documents:
// the keyset cursor is honoured by the LLM log searches only, and
// SearchMCPToolLogs pages by Offset exactly as before, so a cursor set on its
// options changes nothing.
func TestHighFixesMCPSearchIgnoresKeysetCursor(t *testing.T) {
	store := newTestSQLiteStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	hfInsertMCPAt(t, store, "mcp-1", now.Add(-3*time.Minute))
	hfInsertMCPAt(t, store, "mcp-2", now.Add(-2*time.Minute))
	hfInsertMCPAt(t, store, "mcp-3", now.Add(-1*time.Minute))

	plain, err := store.SearchMCPToolLogs(ctx, MCPToolLogSearchFilters{}, PaginationOptions{Limit: 10})
	require.NoError(t, err)
	newest := now.Add(-1 * time.Minute)
	withCursor, err := store.SearchMCPToolLogs(ctx, MCPToolLogSearchFilters{}, PaginationOptions{Limit: 10, AfterTimestamp: &newest, AfterID: "mcp-3"})
	require.NoError(t, err)
	require.Len(t, withCursor.Logs, 3, "the MCP search must ignore the keyset cursor")
	for i := range plain.Logs {
		require.Equal(t, plain.Logs[i].ID, withCursor.Logs[i].ID)
	}
}

// TestGetDistinctMetadataKeysHidesLoadBalancerKeys checks that no key under
// schemas.LoadBalancerMetadataPrefix reaches the metadata filter or the metadata columns, whatever
// the key: they are the router's own record of an attempt, not caller metadata. Ordinary caller
// keys stay.
func TestGetDistinctMetadataKeysHidesLoadBalancerKeys(t *testing.T) {
	store := newTestSQLiteStore(t)
	defer store.Close(context.Background())
	prefix := schemas.LoadBalancerMetadataPrefix
	now := time.Now().UTC()
	insertLogWithMetadata(t, store, "lb-keys",
		`{"tenant":"acme","`+prefix+`decision":"pinned_rerouted","`+prefix+`route_state":"healthy","`+prefix+`v":"1","`+prefix+`excluded_providers":"groq:failed","`+prefix+`excluded_keys":"k1:rate_limit"}`,
		now.Add(-time.Second))

	keys, err := store.GetDistinctMetadataKeys(context.Background(), 100, "")
	require.NoError(t, err)
	require.Equal(t, map[string][]string{"tenant": {"acme"}}, keys)
}

// TestLogsCleanerUpdateRetentionDays verifies changed and default retention for both log tables.
func TestLogsCleanerUpdateRetentionDays(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		initialDays, updatedDays int
		keepBoundary             bool
	}{
		{"increase", 365, 1000, true},
		{"decrease", 1000, 365, false},
		{"zero_uses_default", 1000, 0, false},
		{"negative_uses_default", 1000, -1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestSQLiteStore(t)
			cleaner := NewLogsCleaner(store, CleanerConfig{RetentionDays: tc.initialDays}, testLogger{})
			cleaner.cleanupOldLogs(context.Background())
			now := time.Now().UTC()
			for _, age := range []int{200, 500, 1200} {
				id := fmt.Sprintf("age-%d", age)
				ts := now.AddDate(0, 0, -age)
				hfInsertLogAt(t, store, id, ts)
				hfInsertMCPAt(t, store, id, ts)
			}
			cleaner.UpdateRetentionDays(tc.updatedDays)
			cleaner.cleanupOldLogs(context.Background())
			want := []string{"age-200"}
			if tc.keepBoundary {
				want = append(want, "age-500")
			}
			assert.Equal(t, want, hfLogIDs(t, store.db, "logs"))
			assert.Equal(t, want, hfLogIDs(t, store.db, "mcp_tool_logs"))
		})
	}
}

// retentionCutoffManager records cutoffs and can update retention during a pass.
type retentionCutoffManager struct {
	logCutoff, mcpCutoff time.Time
	onLogsDelete         func()
}

// DeleteLogsBatch captures the logs cutoff and invokes the optional update callback.
func (m *retentionCutoffManager) DeleteLogsBatch(_ context.Context, cutoff time.Time, _ int) (int64, error) {
	m.logCutoff = cutoff
	if m.onLogsDelete != nil {
		m.onLogsDelete()
	}
	return 0, nil
}

// DeleteMCPToolLogsBatch captures the MCP cutoff from the same cleanup pass.
func (m *retentionCutoffManager) DeleteMCPToolLogsBatch(_ context.Context, cutoff time.Time, _ int) (int64, error) {
	m.mcpCutoff = cutoff
	return 0, nil
}

// TestLogsCleanerRetentionUpdateDuringPass verifies updates only affect subsequent passes.
func TestLogsCleanerRetentionUpdateDuringPass(t *testing.T) {
	m := &retentionCutoffManager{}
	cleaner := NewLogsCleaner(m, CleanerConfig{RetentionDays: 365}, testLogger{})
	m.onLogsDelete = func() { cleaner.UpdateRetentionDays(1000) }
	for _, days := range []int{365, 1000} {
		now := time.Now().UTC()
		cleaner.cleanupOldLogs(context.Background())
		assert.WithinDuration(t, now.AddDate(0, 0, -days), m.logCutoff, time.Second)
		assert.Equal(t, m.logCutoff, m.mcpCutoff, "both tables must use the same cutoff when retention changes during a pass")
	}
}

// TestLogsCleanerRetentionUpdateConcurrent exercises retention reads and updates under the race detector.
func TestLogsCleanerRetentionUpdateConcurrent(t *testing.T) {
	m := &retentionCutoffManager{}
	cleaner := NewLogsCleaner(m, CleanerConfig{RetentionDays: 7}, testLogger{})
	var updates sync.WaitGroup
	updates.Add(1)
	go func() {
		defer updates.Done()
		for i := range 1000 {
			days := 7
			if i%2 == 0 {
				days = 30
			}
			cleaner.UpdateRetentionDays(days)
			runtime.Gosched()
		}
	}()
	defer updates.Wait()
	for range 1000 {
		cleaner.cleanupOldLogs(context.Background())
		days := int(math.Round(time.Since(m.logCutoff).Hours() / 24))
		assert.True(t, days == 7 || days == 30, "unexpected retention: %d", days)
		assert.Equal(t, m.logCutoff, m.mcpCutoff)
		runtime.Gosched()
	}
}

// TestBatchUpdateSkipsMissingRows pins the semantics the logging plugin's
// evicted-partial reconcile relies on: BatchUpdate writes each entry's
// non-zero fields onto the present row, leaves content_summary as the row has
// it, and silently skips absent ids instead of inserting or failing the batch.
func TestBatchUpdateSkipsMissingRows(t *testing.T) {
	ctx := context.Background()
	store, err := newSqliteLogStore(ctx, &SQLiteConfig{Path: filepath.Join(t.TempDir(), "update.db")}, testLogger{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close(ctx) })

	now := time.Now().UTC()
	require.NoError(t, store.CreateIfNotExists(ctx, &Log{
		ID: "present-row", Timestamp: now, CreatedAt: now, Object: "chat.completion", Provider: "openai", Model: "gpt-4o",
		Status: "processing", ContentSummary: "the input summary",
	}))

	require.NoError(t, store.BatchUpdate(ctx, []*Log{
		{ID: "present-row", TotalTokens: 42},
		{ID: "present-row", Status: "success"},
		{ID: "absent-row", Status: "success", TotalTokens: 7},
		nil,
	}))
	require.NoError(t, store.BatchUpdate(ctx, nil))

	row, err := store.FindByID(ctx, "present-row")
	require.NoError(t, err)
	require.Equal(t, "success", row.Status, "later entries for the same id apply too")
	require.Equal(t, 42, row.TotalTokens)
	require.Equal(t, "openai", row.Provider, "zero fields leave existing columns alone")
	require.Equal(t, "the input summary", row.ContentSummary, "content_summary is not regenerated from an output-only entry")

	_, err = store.FindByID(ctx, "absent-row")
	require.ErrorIs(t, err, ErrNotFound, "BatchUpdate must not insert")
}

// TestBatchUpdateWritesFinalVisibility: content_hidden is the one column a
// reconcile must be able to clear, so BatchUpdate writes it from every entry
// even when false.
func TestBatchUpdateWritesFinalVisibility(t *testing.T) {
	ctx := context.Background()
	store, err := newSqliteLogStore(ctx, &SQLiteConfig{Path: filepath.Join(t.TempDir(), "visibility.db")}, testLogger{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close(ctx) })

	now := time.Now().UTC()
	for _, id := range []string{"to-visible", "to-hidden"} {
		require.NoError(t, store.CreateIfNotExists(ctx, &Log{
			ID: id, Timestamp: now, CreatedAt: now, Object: "chat.completion", Provider: "openai", Model: "gpt-4o",
			Status: "processing", ContentHidden: id == "to-visible",
		}))
	}
	require.NoError(t, store.BatchUpdate(ctx, []*Log{
		{ID: "to-visible", Status: "success", ContentHidden: false},
		{ID: "to-hidden", Status: "success", ContentHidden: true},
	}))

	visible, err := store.FindByID(ctx, "to-visible")
	require.NoError(t, err)
	require.False(t, visible.ContentHidden, "a final visible decision clears the flag")
	hidden, err := store.FindByID(ctx, "to-hidden")
	require.NoError(t, err)
	require.True(t, hidden.ContentHidden, "a final hidden decision sets the flag")
}

// newPassthroughStore seeds an in-memory sqlite store with a typed chat row, a passthrough row stored
// under its inferred type, and a legacy/unknown-route passthrough row. See newRootsOnlyStore for why
// the DSN is a named shared cache and closed on cleanup.
func newPassthroughStore(t *testing.T) *RDBLogStore {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	require.NoError(t, db.AutoMigrate(&Log{}))

	now := time.Now()
	seed := []Log{
		{ID: "typed-chat", Timestamp: now, Status: "success", Provider: "openai", Object: "chat_completion"},
		{ID: "passthrough-chat", Timestamp: now.Add(-time.Second), Status: "success", Provider: "openai", Object: "chat_completion", IsPassthrough: true},
		{ID: "passthrough-raw", Timestamp: now.Add(-2 * time.Second), Status: "success", Provider: "openai", Object: "passthrough", IsPassthrough: true},
		{ID: "embedding", Timestamp: now.Add(-3 * time.Second), Status: "success", Provider: "openai", Object: "embedding"},
		{ID: "passthrough-chat-stream", Timestamp: now.Add(-4 * time.Second), Status: "success", Provider: "openai", Object: "chat_completion_stream", IsPassthrough: true, Stream: true},
		{ID: "legacy-passthrough-stream", Timestamp: now.Add(-5 * time.Second), Status: "success", Provider: "openai", Object: "passthrough_stream", Stream: true},
	}
	for i := range seed {
		require.NoError(t, seed[i].SerializeFields())
		require.NoError(t, db.Create(&seed[i]).Error)
	}
	return &RDBLogStore{db: db}
}

// TestSearchLogsObjectsFilterMatchesTypedPassthrough pins that a passthrough call stored under its
// inferred type counts as that type, and that asking for passthrough still finds it.
func TestSearchLogsObjectsFilterMatchesTypedPassthrough(t *testing.T) {
	store := newPassthroughStore(t)

	require.Equal(t, []string{"typed-chat", "passthrough-chat"}, searchIDs(t, store, SearchFilters{Objects: []string{"chat_completion"}}))
	// passthrough matches every passthrough call, streamed or not, typed or not.
	require.Equal(t, []string{"passthrough-chat", "passthrough-raw", "passthrough-chat-stream", "legacy-passthrough-stream"}, searchIDs(t, store, SearchFilters{Objects: []string{"passthrough"}}))
	require.Equal(t, []string{"passthrough-chat", "passthrough-raw", "embedding", "passthrough-chat-stream", "legacy-passthrough-stream"}, searchIDs(t, store, SearchFilters{Objects: []string{"passthrough", "embedding"}}))
	// passthrough_stream alone matches only the streamed ones.
	require.Equal(t, []string{"passthrough-chat-stream", "legacy-passthrough-stream"}, searchIDs(t, store, SearchFilters{Objects: []string{"passthrough_stream"}}))
	require.Equal(t, []string{"passthrough-chat", "passthrough-raw", "passthrough-chat-stream", "legacy-passthrough-stream"}, searchIDs(t, store, SearchFilters{Objects: []string{"passthrough", "passthrough_stream"}}))

	res, err := store.SearchLogs(t.Context(), SearchFilters{RequestID: "passthrough-chat"}, PaginationOptions{Limit: 1})
	require.NoError(t, err)
	require.Len(t, res.Logs, 1)
	require.True(t, res.Logs[0].IsPassthrough, "the list query must return is_passthrough so the UI can tag the row")
}

func TestCanUseMatViewFiltersRejectsPassthroughObjects(t *testing.T) {
	require.True(t, canUseMatViewFilters(SearchFilters{Objects: []string{"chat_completion"}}))
	require.False(t, canUseMatViewFilters(SearchFilters{Objects: []string{"passthrough"}}))
	require.False(t, canUseMatViewFilters(SearchFilters{Objects: []string{"embedding", "passthrough_stream"}}))
}

// guardrailOutcomeFixtures returns one log per guardrail shape: a recorded
// block, a legacy block with only the intervention error, a detect-only match,
// a judge call that allowed the request, an unrelated error and a clean
// success. IDs are prefixed so shared databases can be filtered and cleaned.
func guardrailOutcomeFixtures(prefix string, at time.Time) []*Log {
	ruleID := uint(9)
	return []*Log{
		{
			ID: prefix + "blocked", Timestamp: at, Provider: "openai", Model: "gpt-4o", Status: "error", Object: "chat.completion",
			ErrorDetailsParsed: &schemas.BifrostError{
				StatusCode: new(400),
				Error:      &schemas.ErrorField{Type: new("guardrail_intervention"), Message: "Secret detected"},
			},
			GuardrailDebugParsed: &schemas.BifrostGuardrailMetadata{Interventions: []schemas.BifrostGuardrailIntervention{
				{Phase: "input", RuleID: &ruleID, RuleName: "block-secrets", GuardrailName: "azure", Reason: "Secret detected"},
			}},
		},
		{
			ID: prefix + "legacy-blocked", Timestamp: at.Add(time.Second), Provider: "openai", Model: "gpt-4o", Status: "error", Object: "chat.completion",
			ErrorDetailsParsed: &schemas.BifrostError{
				StatusCode: new(400),
				Error:      &schemas.ErrorField{Type: new("guardrail_intervention"), Message: "Request blocked by guardrail"},
			},
		},
		{
			ID: prefix + "detected", Timestamp: at.Add(2 * time.Second), Provider: "openai", Model: "gpt-4o", Status: "success", Object: "chat.completion",
			GuardrailDebugParsed: &schemas.BifrostGuardrailMetadata{Detections: []schemas.BifrostGuardrailDetection{
				{Phase: "output", RuleName: "pii-audit", GuardrailName: "bedrock", Reason: "PII found"},
			}},
		},
		{
			ID: prefix + "judge-allowed", Timestamp: at.Add(3 * time.Second), Provider: "openai", Model: "gpt-4o", Status: "success", Object: "chat.completion",
			GuardrailDebugParsed: &schemas.BifrostGuardrailMetadata{JudgeCalls: []schemas.BifrostGuardrailJudgeCall{
				{Phase: "input", RuleName: "judge", Action: "NONE", TotalTokens: 12},
			}},
		},
		{
			ID: prefix + "rate-limited", Timestamp: at.Add(4 * time.Second), Provider: "openai", Model: "gpt-4o", Status: "error", Object: "chat.completion",
			ErrorDetailsParsed: &schemas.BifrostError{
				StatusCode: new(429),
				Error:      &schemas.ErrorField{Type: new("rate_limit_error"), Message: "slow down"},
			},
		},
		{ID: prefix + "clean", Timestamp: at.Add(5 * time.Second), Provider: "openai", Model: "gpt-4o", Status: "success", Object: "chat.completion"},
	}
}

// assertGuardrailOutcomeFilter seeds the fixtures into store and checks each
// outcome combination returns exactly the rows a guardrail acted on.
func assertGuardrailOutcomeFilter(t *testing.T, store LogStore, prefix string, at time.Time) {
	t.Helper()
	ctx := context.Background()
	for _, entry := range guardrailOutcomeFixtures(prefix, at) {
		require.NoError(t, entry.SerializeFields())
		require.NoError(t, store.CreateIfNotExists(ctx, entry))
	}
	start, end := at.Add(-time.Minute), at.Add(time.Minute)

	cases := []struct {
		outcomes []string
		want     []string
	}{
		{[]string{GuardrailOutcomeBlocked}, []string{"blocked", "legacy-blocked"}},
		{[]string{GuardrailOutcomeDetected}, []string{"detected"}},
		{[]string{GuardrailOutcomeBlocked, GuardrailOutcomeDetected}, []string{"blocked", "legacy-blocked", "detected"}},
		{[]string{"unknown"}, nil},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprint(tc.outcomes), func(t *testing.T) {
			result, err := store.SearchLogs(ctx, SearchFilters{StartTime: &start, EndTime: &end, GuardrailOutcomes: tc.outcomes}, PaginationOptions{Limit: 50})
			require.NoError(t, err)
			var got []string
			for _, row := range result.Logs {
				got = append(got, row.ID[len(prefix):])
			}
			want := slices.Clone(tc.want)
			slices.Sort(got)
			slices.Sort(want)
			assert.Equal(t, want, got)
		})
	}

	// The list rows carry what the Incidents view renders: the guardrail
	// metadata, and error details for guardrail blocks only, never for other
	// failures whose provider payload is unbounded.
	result, err := store.SearchLogs(ctx, SearchFilters{StartTime: &start, EndTime: &end}, PaginationOptions{Limit: 50})
	require.NoError(t, err)
	listed := map[string]Log{}
	for _, row := range result.Logs {
		listed[row.ID[len(prefix):]] = row
	}
	require.NotNil(t, listed["blocked"].GuardrailDebugParsed)
	assert.Equal(t, "block-secrets", listed["blocked"].GuardrailDebugParsed.Interventions[0].RuleName)
	require.NotNil(t, listed["detected"].GuardrailDebugParsed)
	assert.Equal(t, "pii-audit", listed["detected"].GuardrailDebugParsed.Detections[0].RuleName)
	require.NotNil(t, listed["legacy-blocked"].ErrorDetailsParsed)
	assert.Equal(t, "Request blocked by guardrail", listed["legacy-blocked"].ErrorDetailsParsed.Error.Message)
	assert.Nil(t, listed["rate-limited"].ErrorDetailsParsed, "non-guardrail errors stay out of the list")
}

// TestGuardrailOutcomeFilterSQLite verifies the guardrail outcome filter on SQLite.
func TestGuardrailOutcomeFilterSQLite(t *testing.T) {
	assertGuardrailOutcomeFilter(t, newTestSQLiteStore(t), "gof-", time.Date(2026, 3, 4, 5, 6, 0, 0, time.UTC))
}

// TestGuardrailOutcomeFilterPostgres verifies the guardrail outcome filter on Postgres.
func TestGuardrailOutcomeFilterPostgres(t *testing.T) {
	db := trySetupPostgresDB(t)
	if db == nil {
		t.Skip("Postgres not available")
	}
	// The bifrost_safe_jsonb helper SearchLogs relies on is created by the
	// migrations, so run them on a clean schema exactly as the parity tests do.
	dropAllManagedMatViews(db)
	require.NoError(t, db.Exec("DROP TABLE IF EXISTS mcp_tool_logs CASCADE").Error)
	require.NoError(t, db.Exec("DROP TABLE IF EXISTS async_jobs CASCADE").Error)
	require.NoError(t, db.Exec("DROP TABLE IF EXISTS webhook_deliveries CASCADE").Error)
	require.NoError(t, db.Exec("DROP TABLE IF EXISTS logs CASCADE").Error)
	require.NoError(t, db.Exec("CREATE TABLE IF NOT EXISTS migrations (id VARCHAR(255) PRIMARY KEY)").Error)
	require.NoError(t, db.Exec("DELETE FROM migrations").Error)
	require.NoError(t, triggerMigrations(context.Background(), db, testLogger{}))
	prefix := fmt.Sprintf("gof-pg-%d-", time.Now().UnixNano())
	store := &RDBLogStore{db: db, logger: testLogger{}}
	// A fixed historical window keeps rows written by other tests out of the result.
	assertGuardrailOutcomeFilter(t, store, prefix, time.Date(2011, 7, 8, 9, 10, 0, 0, time.UTC))
}

// TestGuardrailOutcomeFilterClickHouse verifies the guardrail outcome filter on ClickHouse.
func TestGuardrailOutcomeFilterClickHouse(t *testing.T) {
	store := trySetupClickHouseStore(t)
	if store == nil {
		t.Skip("ClickHouse not available")
	}
	prefix := fmt.Sprintf("gof-ch-%d-", time.Now().UnixNano())
	assertGuardrailOutcomeFilter(t, store, prefix, time.Date(2012, 7, 8, 9, 10, 0, 0, time.UTC))
}

// TestHybrid_GuardrailDebugStaysQueryable verifies guardrail_debug is kept in
// the database row so the outcome filter works with object storage on.
func TestHybrid_GuardrailDebugStaysQueryable(t *testing.T) {
	hybrid, inner, _ := newTestHybrid(t)
	defer hybrid.Close(context.Background())
	ctx := context.Background()

	input := "hello"
	now := time.Now().UTC()
	entry := &Log{
		ID: "log-guardrail", Timestamp: now, Provider: "openai", Model: "gpt-4o", Status: "success", Object: "chat.completion",
		InputHistoryParsed:  []schemas.ChatMessage{{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: &input}}},
		OutputMessageParsed: &schemas.ChatMessage{Content: &schemas.ChatMessageContent{ContentStr: new("reply")}},
		GuardrailDebugParsed: &schemas.BifrostGuardrailMetadata{Detections: []schemas.BifrostGuardrailDetection{
			{Phase: "input", RuleName: "pii-audit", Reason: "PII found"},
		}},
	}
	require.NoError(t, entry.SerializeFields())
	require.NoError(t, hybrid.CreateIfNotExists(ctx, entry))
	waitForOffload(t, inner, "log-guardrail")

	row, err := inner.FindByID(ctx, "log-guardrail")
	require.NoError(t, err)
	assert.Contains(t, row.GuardrailDebug, "pii-audit", "guardrail_debug must stay in the database row")
	assert.Empty(t, row.OutputMessage, "content is still offloaded")

	start, end := now.Add(-time.Hour), now.Add(time.Hour)
	found, err := hybrid.SearchLogs(ctx, SearchFilters{StartTime: &start, EndTime: &end, GuardrailOutcomes: []string{GuardrailOutcomeDetected}}, PaginationOptions{Limit: 10})
	require.NoError(t, err)
	assert.Len(t, found.Logs, 1)
}

const (
	hiddenTestContent = "what is my balance?"
	hiddenTestAnswer  = "your balance is 42"
)

// hiddenUpdateTestLog is a visible log row that carries content and pricing
// metadata, for the tests that hide it with an update.
func hiddenUpdateTestLog(id string, now time.Time) *Log {
	question, answer := hiddenTestContent, hiddenTestAnswer
	return &Log{
		ID: id, Timestamp: now, CreatedAt: now, Object: "chat.completion", Provider: "openai", Model: "gpt-4o",
		Status: "processing", ContentSummary: question,
		InputHistoryParsed:  []schemas.ChatMessage{{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: &question}}},
		OutputMessageParsed: &schemas.ChatMessage{Role: schemas.ChatMessageRoleAssistant, Content: &schemas.ChatMessageContent{ContentStr: &answer}},
		TokenUsageParsed:    &schemas.BifrostLLMUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
		CacheDebug:          `{"cache_hit":false}`,
		RawRequest:          `{"raw":"what is my balance?"}`,
		ErrorDetails:        `{"error":{"message":"your balance is 42"}}`,
		RoutingEngineLogs:   "routed on what is my balance?",
	}
}

// requireHiddenColumnsEmpty reads the row's raw columns the update must have
// handled, not the parsed view of them, so a column FindByID leaves out counts.
func requireHiddenColumnsEmpty(t *testing.T, read func(column string) string) {
	t.Helper()
	for column := range hiddenContentColumns() {
		assert.Empty(t, read(column), "column %s survived the hidden update", column)
	}
	assert.Empty(t, read("content_summary"), "the content summary survived the hidden update")
	assert.Empty(t, read("input_history"), "the input history survived the hidden update")
	assert.Contains(t, read("token_usage"), `"prompt_tokens":10`, "pricing metadata is not content and stays")
	assert.Equal(t, `{"cache_hit":false}`, read("cache_debug"), "pricing metadata is not content and stays")
}

// exerciseHiddenUpdates runs the shared cases on one store, each on its own row.
// columns reads one raw column of one row.
func exerciseHiddenUpdates(t *testing.T, store LogStore, columns func(t *testing.T, id, column string) string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	seed := func(t *testing.T, id string) {
		t.Helper()
		require.NoError(t, store.CreateIfNotExists(ctx, hiddenUpdateTestLog(id, now)))
		require.Contains(t, columns(t, id, "input_history"), hiddenTestContent, "the fixture must start with stored content")
		require.Equal(t, hiddenTestContent, columns(t, id, "content_summary"))
		require.Contains(t, columns(t, id, "raw_request"), hiddenTestContent)
	}

	t.Run("an update that hides the row empties its content", func(t *testing.T) {
		seed(t, "hide-plain")
		require.NoError(t, store.BatchUpdate(ctx, []*Log{{ID: "hide-plain", Status: "success", ContentHidden: true}}))
		found, err := store.FindByID(ctx, "hide-plain")
		require.NoError(t, err)
		require.True(t, found.ContentHidden)
		assert.Equal(t, "success", found.Status)
		require.NotNil(t, found.TokenUsageParsed, "token usage must survive for pricing")
		assert.Equal(t, 15, found.TokenUsageParsed.TotalTokens)
		requireHiddenColumnsEmpty(t, func(column string) string { return columns(t, "hide-plain", column) })
	})

	t.Run("output the hiding entry carries is not stored either", func(t *testing.T) {
		seed(t, "hide-carrying")
		answer := "late output"
		require.NoError(t, store.BatchUpdate(ctx, []*Log{{
			ID: "hide-carrying", Status: "success", ContentHidden: true,
			OutputMessageParsed: &schemas.ChatMessage{Role: schemas.ChatMessageRoleAssistant, Content: &schemas.ChatMessageContent{ContentStr: &answer}},
		}}))
		requireHiddenColumnsEmpty(t, func(column string) string { return columns(t, "hide-carrying", column) })
	})

	t.Run("a visible update keeps the content", func(t *testing.T) {
		seed(t, "hide-negative")
		require.NoError(t, store.BatchUpdate(ctx, []*Log{{ID: "hide-negative", Status: "success"}}))
		assert.Contains(t, columns(t, "hide-negative", "input_history"), hiddenTestContent)
		assert.Equal(t, hiddenTestContent, columns(t, "hide-negative", "content_summary"))
		assert.Contains(t, columns(t, "hide-negative", "output_message"), hiddenTestAnswer)
		assert.Contains(t, columns(t, "hide-negative", "raw_request"), hiddenTestContent)
	})

	t.Run("a later visible entry in the batch does not bring it back", func(t *testing.T) {
		seed(t, "hide-then-visible")
		require.NoError(t, store.BatchUpdate(ctx, []*Log{
			{ID: "hide-then-visible", Status: "success", ContentHidden: true},
			{ID: "hide-then-visible", Status: "success"},
		}))
		found, err := store.FindByID(ctx, "hide-then-visible")
		require.NoError(t, err)
		assert.False(t, found.ContentHidden, "the last entry's decision is the row's flag")
		assert.Empty(t, columns(t, "hide-then-visible", "input_history"), "cleared content stays cleared")
		assert.Empty(t, columns(t, "hide-then-visible", "content_summary"))
	})

	t.Run("reading the row back recovers nothing, and a later visible update keeps only its own output", func(t *testing.T) {
		seed(t, "hide-later")
		require.NoError(t, store.BatchUpdate(ctx, []*Log{{ID: "hide-later", Status: "success", ContentHidden: true}}))
		hidden, err := store.FindByID(ctx, "hide-later")
		require.NoError(t, err)
		assert.Empty(t, hidden.InputHistoryParsed, "no input comes back through the store")
		assert.Nil(t, hidden.OutputMessageParsed, "no output comes back through the store")
		assert.Empty(t, hidden.ContentSummary)
		assert.Empty(t, hidden.RawRequest)

		later := "visible again"
		require.NoError(t, store.BatchUpdate(ctx, []*Log{{
			ID: "hide-later", Status: "success",
			OutputMessageParsed: &schemas.ChatMessage{Role: schemas.ChatMessageRoleAssistant, Content: &schemas.ChatMessageContent{ContentStr: &later}},
		}}))
		found, err := store.FindByID(ctx, "hide-later")
		require.NoError(t, err)
		assert.False(t, found.ContentHidden, "a later final decision clears the flag")
		assert.Contains(t, columns(t, "hide-later", "output_message"), later)
		assert.NotContains(t, columns(t, "hide-later", "output_message"), hiddenTestAnswer)
		assert.Empty(t, columns(t, "hide-later", "input_history"), "what the hidden update emptied stays empty")
		assert.Empty(t, columns(t, "hide-later", "raw_request"))
		require.NotNil(t, found.TokenUsageParsed)
		assert.Equal(t, 15, found.TokenUsageParsed.TotalTokens, "usage survives both updates")
		assert.Equal(t, `{"cache_hit":false}`, columns(t, "hide-later", "cache_debug"))
	})

	t.Run("a hidden update of a missing row is skipped", func(t *testing.T) {
		require.NoError(t, store.BatchUpdate(ctx, []*Log{{ID: "hide-missing", Status: "success", ContentHidden: true}}))
		_, err := store.FindByID(ctx, "hide-missing")
		require.ErrorIs(t, err, ErrNotFound)
	})
}

// sqlColumnReader reads one raw column of a logs row. COALESCE because a column
// a row never set is NULL, which is as empty as the update leaves it.
func sqlColumnReader(db *gorm.DB) func(t *testing.T, id, column string) string {
	return func(t *testing.T, id, column string) string {
		t.Helper()
		var value string
		require.NoError(t, db.Raw("SELECT COALESCE("+column+", '') FROM logs WHERE id = ?", id).Scan(&value).Error)
		return value
	}
}

// TestBatchUpdateHidingALogClearsItsContent: an update that makes a log hidden
// must remove the content the row already holds. Updates skip zero values, so
// clearing only the update entry would leave the stored content in place.
func TestBatchUpdateHidingALogClearsItsContent(t *testing.T) {
	ctx := context.Background()
	store, err := newSqliteLogStore(ctx, &SQLiteConfig{Path: filepath.Join(t.TempDir(), "hide.db")}, testLogger{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close(ctx) })
	exerciseHiddenUpdates(t, store, sqlColumnReader(store.db))
}

// TestBatchUpdateHidingALogClearsItsContentPostgres runs the same cases on the
// migrated Postgres schema, whose column types the empty strings must fit.
func TestBatchUpdateHidingALogClearsItsContentPostgres(t *testing.T) {
	store, db := setupPerfTestDB(t)
	store.logger = testLogger{}
	exerciseHiddenUpdates(t, store, sqlColumnReader(db))
}
