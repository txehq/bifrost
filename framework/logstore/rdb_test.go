package logstore

import (
	"context"
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
