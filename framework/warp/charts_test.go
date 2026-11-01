package warp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bytedance/sonic"
	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/capsohq/bifrost/framework/logstore"
	"github.com/capsohq/bifrost/framework/postgresconn"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// Charts used to arrive as mermaid code the model wrote itself, numbers and
// all. render_chart reads the data, so a line chart's points are the
// histogram's buckets and the model supplies only what to plot.
func TestWarpRenderChartLineReadsTheHistogram(t *testing.T) {
	day := time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)
	fake := &fakeLogReader{histogramResult: &logstore.HistogramResult{Buckets: []logstore.HistogramBucket{
		{Timestamp: day, Count: 40, Error: 5},
		{Timestamp: day.Add(24 * time.Hour), Count: 55, Error: 30},
	}}}
	deps := &ToolDeps{logManager: fake, charts: newChartRegistry()}
	out, err := runTool(t, RenderChartTool, deps, map[string]any{
		"kind": "line", "title": "Errors per day", "metric": "errors", "interval": "day",
		"filters": map[string]any{"start_time": "-7d"},
	})
	require.NoError(t, err)
	require.Equal(t, int64(86400), fake.histogramBucket)

	result := out.(map[string]any)
	require.Equal(t, "chart-1", result["chart_id"])
	require.Equal(t, "```warp-chart\nchart-1\n```", result["block"])

	spec, ok := deps.charts.get("chart-1")
	require.True(t, ok)
	require.Equal(t, "line", spec.Kind)
	require.Equal(t, "count", spec.Unit)
	require.Equal(t, []ChartPoint{{X: "2026-09-22T00:00:00Z", Y: 5}, {X: "2026-09-23T00:00:00Z", Y: 30}}, spec.Points)
}

// A bar chart ranks, largest first, and stops at the limit.
func TestWarpRenderChartBarRanksLargestFirst(t *testing.T) {
	fake := &fakeLogReader{modelRankingResult: &logstore.ModelRankingResult{Rankings: []logstore.ModelRankingWithTrend{
		{ModelRankingEntry: logstore.ModelRankingEntry{Model: "gpt-4o-mini", TotalCost: 0.02}},
		{ModelRankingEntry: logstore.ModelRankingEntry{Model: "claude-3-opus", TotalCost: 2.1}},
		{ModelRankingEntry: logstore.ModelRankingEntry{Model: "claude-3-5-sonnet", TotalCost: 0.4}},
	}}}
	deps := &ToolDeps{logManager: fake, charts: newChartRegistry()}
	_, err := runTool(t, RenderChartTool, deps, map[string]any{
		"kind": "bar", "title": "Spend by model", "metric": "cost", "group": "model", "limit": float64(2),
		"filters": map[string]any{"start_time": "-7d"},
	})
	require.NoError(t, err)
	spec, _ := deps.charts.get("chart-1")
	require.Equal(t, []ChartPoint{{X: "claude-3-opus", Y: 2.1}, {X: "claude-3-5-sonnet", Y: 0.4}}, spec.Points)
}

// A cost chart ranked by the store's own order kept the busiest groups, not the
// costliest: the limit applied before sorting by cost. Every group is fetched,
// and the limit cuts the points only after they are sorted by the metric.
func TestWarpRenderChartBarLimitsAfterSortingByMetric(t *testing.T) {
	fake := &fakeLogReader{modelRankingResult: &logstore.ModelRankingResult{}}
	deps := &ToolDeps{logManager: fake, charts: newChartRegistry()}
	_, err := runTool(t, RenderChartTool, deps, map[string]any{
		"kind": "bar", "title": "Spend by model", "metric": "cost", "group": "model", "limit": float64(2),
		"filters": map[string]any{"start_time": "-7d"},
	})
	require.NoError(t, err)
	require.NotNil(t, fake.rankingFilters.RankingLimit)
	require.Zero(t, *fake.rankingFilters.RankingLimit, "the store ranks by requests, so a store-side limit drops the costliest groups")
}

// "Errors by team" counts failed requests per team exactly: the same ranking,
// narrowed to status error, rather than a success rate applied to a total.
func TestWarpRenderChartErrorBarsCountFailedRequests(t *testing.T) {
	fake := &fakeLogReader{dimensionRankingResult: &logstore.DimensionRankingResult{Rankings: []logstore.DimensionRankingWithTrend{
		{DimensionRankingEntry: logstore.DimensionRankingEntry{ID: "team-platform", Name: "Platform Engineering", TotalRequests: 15}},
	}}}
	deps := &ToolDeps{logManager: fake, charts: newChartRegistry()}
	_, err := runTool(t, RenderChartTool, deps, map[string]any{
		"kind": "bar", "title": "Errors by team", "metric": "errors", "group": "team",
		"filters": map[string]any{"start_time": "-7d"},
	})
	require.NoError(t, err)
	require.Equal(t, []string{"error"}, fake.rankingFilters.Status)
	spec, _ := deps.charts.get("chart-1")
	require.Equal(t, []ChartPoint{{X: "team-platform", Label: "Platform Engineering", Y: 15}}, spec.Points)
}

// Each wrong shape names the fix, so the model can correct itself in one step.
func TestWarpRenderChartRejectsShapesItCannotDraw(t *testing.T) {
	cases := map[string]struct {
		args map[string]any
		want string
	}{
		"line without interval":   {map[string]any{"kind": "line", "title": "t", "metric": "requests"}, `needs interval "hour", "day" or "week"`},
		"line with a group":       {map[string]any{"kind": "line", "title": "t", "metric": "requests", "interval": "day", "group": "model"}, "group is for bar charts"},
		"bar with neither":        {map[string]any{"kind": "bar", "title": "t", "metric": "cost"}, "a bar chart needs interval (bars over time) or group"},
		"bar with both":           {map[string]any{"kind": "bar", "title": "t", "metric": "cost", "group": "model", "interval": "day"}, "not both"},
		"latency by team":         {map[string]any{"kind": "bar", "title": "t", "metric": "latency", "group": "team"}, "latency can only be grouped by provider or model"},
		"pie":                     {map[string]any{"kind": "pie", "title": "t", "metric": "cost"}, "not supported"},
		"hourly over thirty days": {map[string]any{"kind": "line", "title": "t", "metric": "requests", "interval": "hour", "filters": map[string]any{"start_time": "-30d"}}, `use interval "day"`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if _, ok := c.args["filters"]; !ok {
				c.args["filters"] = map[string]any{"start_time": "-7d"}
			}
			_, err := runTool(t, RenderChartTool, &ToolDeps{logManager: &fakeLogReader{}, charts: newChartRegistry()}, c.args)
			require.ErrorContains(t, err, c.want)
		})
	}
}

// Only an id render_chart issued this turn becomes a chart. A spec the model
// wrote out itself - numbers it typed - and an id it made up are dropped.
func TestWarpExpandChartBlocksOnlyDrawsIssuedCharts(t *testing.T) {
	charts := newChartRegistry()
	spec, err := charts.add(ChartSpec{Kind: "bar", Title: "Spend by provider", Metric: "cost", Unit: "usd", Points: []ChartPoint{{X: "anthropic", Y: 2.58}}})
	require.NoError(t, err)

	answer := "Anthropic dominates.\n\n```warp-chart\n" + spec.ID + "\n```\n\nThat is 97% of spend."
	expanded := expandChartBlocks(answer, charts)
	body := strings.TrimSuffix(strings.SplitN(expanded, "```warp-chart\n", 2)[1], "\n```\n\nThat is 97% of spend.")
	var got ChartSpec
	require.NoError(t, sonic.UnmarshalString(body, &got), expanded)
	require.Equal(t, spec, got)
	require.True(t, strings.HasPrefix(expanded, "Anthropic dominates."))

	require.Contains(t, expandChartBlocks("```warp-chart\n{\"id\":\""+spec.ID+"\"}\n```", charts), `"points":[{"x":"anthropic","y":2.58}]`, "a quoted id is still an id")

	invented := `{"kind":"bar","points":[{"x":"openai","y":99}]}`
	require.Equal(t, "Look:\n\n", expandChartBlocks("Look:\n\n```warp-chart\n"+invented+"\n```", charts))
	require.Equal(t, "", expandChartBlocks("```warp-chart\nchart-9\n```", charts))
	require.Equal(t, "no charts here", expandChartBlocks("no charts here", nil))
}

// End to end: the model draws a chart and pastes the id; what streams, and so
// what history saves, carries the spec the tool built.
func TestWarpAgentStreamsExpandedCharts(t *testing.T) {
	day := time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)
	model := &scriptedModel{turns: []*schemas.BifrostResponsesResponse{
		ToolTurn("c-1", RenderChartTool, `{"kind":"line","title":"Requests per day","metric":"requests","interval":"day","filters":{"start_time":"-7d"}}`),
		TextTurn("Traffic doubled on the 23rd.\n\n```warp-chart\nchart-1\n```"),
	}}
	fake := &fakeLogReader{histogramResult: &logstore.HistogramResult{Buckets: []logstore.HistogramBucket{
		{Timestamp: day, Count: 40}, {Timestamp: day.Add(24 * time.Hour), Count: 80},
	}}}
	events := collectEvents(t, newTestAgent(model, fake, 8), context.Background())

	var streamed strings.Builder
	for _, event := range events {
		if event.Type == EventDelta {
			streamed.WriteString(event.Delta)
		}
	}
	require.Contains(t, streamed.String(), `"title":"Requests per day"`)
	require.Contains(t, streamed.String(), `{"x":"2026-09-23T00:00:00Z","y":80}`)
	require.NotContains(t, streamed.String(), "```warp-chart\nchart-1\n```")
}

// "Make it a bar chart with per-week errors" had no render_chart shape: bars
// only compared groups and there was no week interval, so the model counted
// each week with count_logs and typed its own chart block - which expansion
// rightly dropped, leaving no chart. Bars now run over time too, and a week is
// a UTC ISO week built from daily buckets.
func TestWarpRenderChartBarsPerWeek(t *testing.T) {
	monday := time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)
	fake := &fakeLogReader{histogramResult: &logstore.HistogramResult{Buckets: []logstore.HistogramBucket{
		{Timestamp: monday, Count: 100, Error: 10},
		{Timestamp: monday.Add(3 * 24 * time.Hour), Count: 50, Error: 5},
		{Timestamp: monday.Add(7 * 24 * time.Hour), Count: 40, Error: 30},
	}}}
	deps := &ToolDeps{logManager: fake, charts: newChartRegistry()}
	_, err := runTool(t, RenderChartTool, deps, map[string]any{
		"kind": "bar", "title": "Errors per week", "metric": "errors", "interval": "week",
		"filters": map[string]any{"start_time": "-28d"},
	})
	require.NoError(t, err)
	require.Equal(t, int64(86400), fake.histogramBucket, "weeks are summed from daily buckets")
	spec, _ := deps.charts.get(spec1(t, deps))
	require.Equal(t, "week", spec.Interval)
	require.Equal(t, []ChartPoint{{X: "2026-09-14T00:00:00Z", Y: 15}, {X: "2026-09-21T00:00:00Z", Y: 30}}, spec.Points, "time bars keep time order")
}

// "Failure rates" came back as error counts: there was no rate to plot. A
// week's rate is its errors over its requests, not an average of daily rates.
func TestWarpRenderChartErrorRateOverTime(t *testing.T) {
	monday := time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)
	fake := &fakeLogReader{histogramResult: &logstore.HistogramResult{Buckets: []logstore.HistogramBucket{
		{Timestamp: monday, Count: 100, Error: 10},
		{Timestamp: monday.Add(24 * time.Hour), Count: 0},
		{Timestamp: monday.Add(3 * 24 * time.Hour), Count: 50, Error: 5},
	}}}
	deps := &ToolDeps{logManager: fake, charts: newChartRegistry()}
	_, err := runTool(t, RenderChartTool, deps, map[string]any{
		"kind": "line", "title": "Failure rate per day", "metric": "error_rate", "interval": "day",
		"filters": map[string]any{"start_time": "-7d"},
	})
	require.NoError(t, err)
	spec, _ := deps.charts.get(spec1(t, deps))
	require.Equal(t, "percent", spec.Unit)
	require.Equal(t, []ChartPoint{{X: "2026-09-14T00:00:00Z", Y: 10}, {X: "2026-09-17T00:00:00Z", Y: 10}}, spec.Points, "a bucket with no requests has no rate and is left out")

	deps = &ToolDeps{logManager: fake, charts: newChartRegistry()}
	_, err = runTool(t, RenderChartTool, deps, map[string]any{
		"kind": "bar", "title": "Failure rate per week", "metric": "error_rate", "interval": "week",
		"filters": map[string]any{"start_time": "-28d"},
	})
	require.NoError(t, err)
	spec, _ = deps.charts.get(spec1(t, deps))
	require.Equal(t, []ChartPoint{{X: "2026-09-14T00:00:00Z", Y: 10}}, spec.Points, "15 errors over 150 requests")
}

// An empty bucket has no latency: it used to plot as 0 ms, and a week's average
// counted it as one request at 0 ms. Buckets are weighted by their requests.
func TestWarpRenderChartLatencySkipsEmptyBuckets(t *testing.T) {
	monday := time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)
	fake := &fakeLogReader{latencyHistogramResult: &logstore.LatencyHistogramResult{Buckets: []logstore.LatencyHistogramBucket{
		{Timestamp: monday, AvgLatency: 200, TotalRequests: 100},
		{Timestamp: monday.Add(24 * time.Hour)},
		{Timestamp: monday.Add(2 * 24 * time.Hour), AvgLatency: 500, TotalRequests: 50},
	}}}
	deps := &ToolDeps{logManager: fake, charts: newChartRegistry()}
	_, err := runTool(t, RenderChartTool, deps, map[string]any{
		"kind": "line", "title": "Latency per day", "metric": "latency", "interval": "day",
		"filters": map[string]any{"start_time": "-7d"},
	})
	require.NoError(t, err)
	spec, _ := deps.charts.get(spec1(t, deps))
	require.Equal(t, []ChartPoint{{X: "2026-09-14T00:00:00Z", Y: 200}, {X: "2026-09-16T00:00:00Z", Y: 500}}, spec.Points, "a bucket with no requests is left out, not drawn at 0 ms")

	deps = &ToolDeps{logManager: fake, charts: newChartRegistry()}
	_, err = runTool(t, RenderChartTool, deps, map[string]any{
		"kind": "bar", "title": "Latency per week", "metric": "latency", "interval": "week",
		"filters": map[string]any{"start_time": "-28d"},
	})
	require.NoError(t, err)
	spec, _ = deps.charts.get(spec1(t, deps))
	require.Equal(t, []ChartPoint{{X: "2026-09-14T00:00:00Z", Y: 300}}, spec.Points, "(100*200 + 50*500) / 150 requests")
}

// A group's failure rate is its failed requests over its requests, read from
// the same ranking with and without status error.
func TestWarpRenderChartErrorRateByModel(t *testing.T) {
	fake := &fakeLogReader{modelRankingResult: &logstore.ModelRankingResult{Rankings: []logstore.ModelRankingWithTrend{
		{ModelRankingEntry: logstore.ModelRankingEntry{Model: "claude-3-5-sonnet", TotalRequests: 100, SuccessRate: 70}},
		{ModelRankingEntry: logstore.ModelRankingEntry{Model: "gpt-4o-mini", TotalRequests: 60, SuccessRate: 95}},
	}}, errorModelRankingResult: &logstore.ModelRankingResult{Rankings: []logstore.ModelRankingWithTrend{
		{ModelRankingEntry: logstore.ModelRankingEntry{Model: "claude-3-5-sonnet", TotalRequests: 30}},
		{ModelRankingEntry: logstore.ModelRankingEntry{Model: "gpt-4o-mini", TotalRequests: 3}},
	}}}
	deps := &ToolDeps{logManager: fake, charts: newChartRegistry()}
	_, err := runTool(t, RenderChartTool, deps, map[string]any{
		"kind": "bar", "title": "Failure rate by model", "metric": "error_rate", "group": "model",
		"filters": map[string]any{"start_time": "-7d"},
	})
	require.NoError(t, err)
	spec, _ := deps.charts.get(spec1(t, deps))
	require.Equal(t, []ChartPoint{{X: "claude-3-5-sonnet", Y: 30}, {X: "gpt-4o-mini", Y: 5}}, spec.Points)
}

// A cancelled request is not a failed one. The model and provider bars took
// 100 minus the success rate, so 80 successes and 20 cancellations with no
// error at all charted a 20% error rate, while the dimension bars, which count
// failed requests, charted 0% for the same traffic.
func TestWarpRenderChartErrorRateIgnoresCancellations(t *testing.T) {
	t.Run("model", func(t *testing.T) {
		fake := &fakeLogReader{modelRankingResult: &logstore.ModelRankingResult{Rankings: []logstore.ModelRankingWithTrend{
			{ModelRankingEntry: logstore.ModelRankingEntry{Model: "gpt-4o", Provider: "openai", TotalRequests: 100, SuccessCount: 80, SuccessRate: 80}},
		}}, errorModelRankingResult: &logstore.ModelRankingResult{}}
		deps := &ToolDeps{logManager: fake, charts: newChartRegistry()}
		_, err := runTool(t, RenderChartTool, deps, map[string]any{
			"kind": "bar", "title": "Failure rate by model", "metric": "error_rate", "group": "model",
			"filters": map[string]any{"start_time": "-7d"},
		})
		require.NoError(t, err)
		spec, _ := deps.charts.get(spec1(t, deps))
		require.Equal(t, []ChartPoint{{X: "gpt-4o", Y: 0}}, spec.Points, "20 cancellations and no errors is a 0% error rate")
	})
	t.Run("provider", func(t *testing.T) {
		fake := &fakeLogReader{
			providerCostHistogramResult: &logstore.ProviderCostHistogramResult{Providers: []string{"openai"}},
			statsByProvider:             map[string]*logstore.SearchStats{"openai": {TotalRequests: 100, SuccessRate: 80}},
			errorStatsByProvider:        map[string]*logstore.SearchStats{"openai": {TotalRequests: 5}},
		}
		deps := &ToolDeps{logManager: fake, charts: newChartRegistry()}
		_, err := runTool(t, RenderChartTool, deps, map[string]any{
			"kind": "bar", "title": "Failure rate by provider", "metric": "error_rate", "group": "provider",
			"filters": map[string]any{"start_time": "-7d"},
		})
		require.NoError(t, err)
		spec, _ := deps.charts.get(spec1(t, deps))
		require.Equal(t, []ChartPoint{{X: "openai", Y: 5}}, spec.Points, "5 errors over 100 requests; the other 15 that did not succeed were cancelled")
	})
	t.Run("in-flight requests", func(t *testing.T) {
		// 10 of the 110 are still running. The rate is over the 100 that finished,
		// as the model and dimension bars count it.
		completed := int64(100)
		fake := &fakeLogReader{
			providerCostHistogramResult: &logstore.ProviderCostHistogramResult{Providers: []string{"openai"}},
			statsByProvider:             map[string]*logstore.SearchStats{"openai": {TotalRequests: 110, CacheHitRateTotalRequests: &completed, SuccessRate: 80}},
			errorStatsByProvider:        map[string]*logstore.SearchStats{"openai": {TotalRequests: 5}},
		}
		deps := &ToolDeps{logManager: fake, charts: newChartRegistry()}
		_, err := runTool(t, RenderChartTool, deps, map[string]any{
			"kind": "bar", "title": "Failure rate by provider", "metric": "error_rate", "group": "provider",
			"filters": map[string]any{"start_time": "-7d"},
		})
		require.NoError(t, err)
		spec, _ := deps.charts.get(spec1(t, deps))
		require.Equal(t, []ChartPoint{{X: "openai", Y: 5}}, spec.Points)
	})
	t.Run("status filter excluding errors", func(t *testing.T) {
		// Asked for successful requests only, the population holds no errors: the
		// rate is 0, not the unfiltered errors over the success-only total.
		fake := &fakeLogReader{
			providerCostHistogramResult: &logstore.ProviderCostHistogramResult{Providers: []string{"openai"}},
			statsByProvider:             map[string]*logstore.SearchStats{"openai": {TotalRequests: 80, SuccessRate: 100}},
			errorStatsByProvider:        map[string]*logstore.SearchStats{"openai": {TotalRequests: 120}},
		}
		deps := &ToolDeps{logManager: fake, charts: newChartRegistry()}
		_, err := runTool(t, RenderChartTool, deps, map[string]any{
			"kind": "bar", "title": "Failure rate by provider", "metric": "error_rate", "group": "provider",
			"filters": map[string]any{"start_time": "-7d", "status": []any{"success"}},
		})
		require.NoError(t, err)
		spec, _ := deps.charts.get(spec1(t, deps))
		require.Equal(t, []ChartPoint{{X: "openai", Y: 0}}, spec.Points)
	})
}

func spec1(t *testing.T, deps *ToolDeps) string {
	t.Helper()
	require.NotEmpty(t, deps.charts.order, "render_chart registered no chart")
	return deps.charts.order[0]
}

// expandChartBlocks reports how many blocks it dropped, so the agent can tell
// the model rather than leave a silent gap where a chart should have been.
func TestWarpExpandChartBlocksCountsWhatItDrops(t *testing.T) {
	charts := newChartRegistry()
	spec, err := charts.add(ChartSpec{Kind: "line", Title: "t", Metric: "requests", Unit: "count"})
	require.NoError(t, err)
	_, dropped := expandChartBlocksCounted("```warp-chart\n"+spec.ID+"\n```\n```warp-chart\n{\"id\":\"chart-2\",\"points\":[]}\n```", charts)
	require.Equal(t, 1, dropped)
}

// End to end: a hand-written chart block is dropped, and the model is told so
// once - here it then calls render_chart and pastes the real block.
func TestWarpAgentRedirectsAHandWrittenChart(t *testing.T) {
	day := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	var nudge string
	calls := 0
	chat := func(_ context.Context, req *schemas.BifrostResponsesRequest) (*schemas.BifrostResponsesResponse, *schemas.BifrostError) {
		calls++
		switch calls {
		case 1:
			return ToolTurn("c-1", "count_logs", `{"filters":{"start_time":"-7d"}}`), nil
		case 2:
			return TextTurn("Here:\n\n```warp-chart\n{\"id\":\"chart-2\",\"kind\":\"bar\",\"points\":[{\"x\":\"w1\",\"y\":34}]}\n```"), nil
		case 3:
			last := req.Input[len(req.Input)-1]
			if last.Content != nil && last.Content.ContentStr != nil {
				nudge = *last.Content.ContentStr
			}
			return ToolTurn("c-2", RenderChartTool, `{"kind":"bar","title":"Errors per week","metric":"errors","interval":"week","filters":{"start_time":"-7d"}}`), nil
		default:
			return TextTurn("```warp-chart\nchart-1\n```"), nil
		}
	}
	agent := newTestAgent(&scriptedModel{}, &fakeLogReader{histogramResult: &logstore.HistogramResult{Buckets: []logstore.HistogramBucket{{Timestamp: day, Count: 9, Error: 3}}}}, 8)
	agent.chat = chat
	events := collectEvents(t, agent, context.Background())

	require.Contains(t, nudge, "render_chart did not produce")
	var streamed strings.Builder
	for _, event := range events {
		if event.Type == EventDelta {
			streamed.WriteString(event.Delta)
		}
	}
	require.NotContains(t, streamed.String(), `"y":34`, "the hand-typed chart never streams")
	require.Contains(t, streamed.String(), `"interval":"week"`)
}

// A reply that was only a hand-written chart block expands to nothing. Once the
// one redirect is spent, it used to end the turn as an empty answer - a blank
// message with no reason. It now says the chart could not be drawn.
func TestWarpAgentAnswersWhenOnlyADroppedChartIsLeft(t *testing.T) {
	handWritten := TextTurn("```warp-chart\n{\"id\":\"chart-2\",\"kind\":\"bar\",\"points\":[{\"x\":\"w1\",\"y\":34}]}\n```")
	for name, maxIterations := range map[string]int{"redirect already spent": 8, "no iteration left to redirect": 2} {
		t.Run(name, func(t *testing.T) {
			model := &scriptedModel{turns: []*schemas.BifrostResponsesResponse{
				ToolTurn("c-1", "count_logs", `{"filters":{"start_time":"-7d"}}`),
				handWritten,
				handWritten,
			}}
			events := collectEvents(t, newTestAgent(model, &fakeLogReader{}, maxIterations), context.Background())

			var streamed strings.Builder
			for _, event := range events {
				if event.Type == EventDelta {
					streamed.WriteString(event.Delta)
				}
			}
			last := events[len(events)-1]
			require.Equal(t, EventDone, last.Type)
			require.Equal(t, droppedChartAnswer, strings.TrimSpace(streamed.String()), "the turn must not end as an empty answer")
			require.NotContains(t, streamed.String(), `"y":34`)
		})
	}
}

// A failures chart's "Open in Logs" opened every request in the window: the
// link was built from the chart's filters, and the error restriction only ever
// lived inside the query. For errors and error_rate it opens the failures.
func TestWarpRenderChartFailureLinksOpenTheFailures(t *testing.T) {
	for metric, wantErrors := range map[string]bool{"errors": true, "error_rate": true, "requests": false, "cost": false} {
		t.Run(metric, func(t *testing.T) {
			deps := &ToolDeps{logManager: &fakeLogReader{}, charts: newChartRegistry()}
			_, err := runTool(t, RenderChartTool, deps, map[string]any{
				"kind": "line", "title": "t", "metric": metric, "interval": "day",
				"filters": map[string]any{"start_time": "-30d"},
			})
			require.NoError(t, err)
			spec, _ := deps.charts.get(spec1(t, deps))
			require.Equal(t, wantErrors, strings.Contains(spec.Link, "status=error"), spec.Link)
		})
	}
}

// TestWarpRenderChartModelBarsAggregateAcrossProviders: rankings are per model and
// provider, but a model chart labels bars by model, so one model served by two
// providers is one bar with their combined value (latency weighted by requests),
// not two identically labelled bars.
func TestWarpRenderChartModelBarsAggregateAcrossProviders(t *testing.T) {
	rankings := &logstore.ModelRankingResult{Rankings: []logstore.ModelRankingWithTrend{
		{ModelRankingEntry: logstore.ModelRankingEntry{Model: "gpt-4o", Provider: "openai", TotalRequests: 30, TotalCost: 3, AvgLatency: 100}},
		{ModelRankingEntry: logstore.ModelRankingEntry{Model: "gpt-4o", Provider: "azure", TotalRequests: 10, TotalCost: 2, AvgLatency: 300}},
		{ModelRankingEntry: logstore.ModelRankingEntry{Model: "claude", Provider: "anthropic", TotalRequests: 5, TotalCost: 4, AvgLatency: 50}},
	}}
	for _, tc := range []struct {
		metric string
		want   []ChartPoint
	}{
		{"cost", []ChartPoint{{X: "gpt-4o", Y: 5}, {X: "claude", Y: 4}}},
		{"latency", []ChartPoint{{X: "gpt-4o", Y: 150}, {X: "claude", Y: 50}}},
	} {
		fake := &fakeLogReader{modelRankingResult: rankings}
		deps := &ToolDeps{logManager: fake, charts: newChartRegistry()}
		_, err := runTool(t, RenderChartTool, deps, map[string]any{
			"kind": "bar", "title": tc.metric + " by model", "metric": tc.metric, "group": "model",
			"filters": map[string]any{"start_time": "-7d"},
		})
		require.NoError(t, err)
		spec, _ := deps.charts.get(spec1(t, deps))
		require.Equal(t, tc.want, spec.Points, tc.metric)
	}
}

// storeLogReader serves Warp's reads from a real log store, so a test sees the SQL
// each backend runs. Anything else it is asked panics through LogReaderStub.
type storeLogReader struct {
	LogReaderStub
	store logstore.LogStore
}

func (r storeLogReader) Search(ctx context.Context, filters *logstore.SearchFilters, pagination *logstore.PaginationOptions) (*logstore.SearchResult, error) {
	return r.store.SearchLogs(ctx, *filters, *pagination)
}

func (r storeLogReader) GetLog(ctx context.Context, id string) (*logstore.Log, error) {
	return r.store.FindByID(ctx, id)
}

func (r storeLogReader) GetStats(ctx context.Context, filters *logstore.SearchFilters) (*logstore.SearchStats, error) {
	return r.store.GetStats(ctx, *filters)
}

func (r storeLogReader) GetHistogram(ctx context.Context, filters *logstore.SearchFilters, bucket int64) (*logstore.HistogramResult, error) {
	return r.store.GetHistogram(ctx, *filters, bucket)
}

func (r storeLogReader) GetModelRankings(ctx context.Context, filters *logstore.SearchFilters) (*logstore.ModelRankingResult, error) {
	return r.store.GetModelRankings(ctx, *filters)
}

func (r storeLogReader) GetDimensionRankings(ctx context.Context, filters *logstore.SearchFilters, dimension logstore.RankingDimension) (*logstore.DimensionRankingResult, error) {
	return r.store.GetDimensionRankings(ctx, *filters, dimension)
}

func (r storeLogReader) GetProviderCostHistogram(ctx context.Context, filters *logstore.SearchFilters, bucket int64) (*logstore.ProviderCostHistogramResult, error) {
	return r.store.GetProviderCostHistogram(ctx, *filters, bucket)
}

// warpTestWindow is an hour of its own in the past, so rows from another run on a
// shared database stay out of a window that filters on nothing else.
func warpTestWindow() time.Time {
	return time.Now().UTC().AddDate(-5, 0, 0).Add(time.Duration(time.Now().UnixNano()%(1<<45)) * time.Microsecond).Truncate(time.Hour)
}

// warpTestLogStores opens every backend this run can reach. SQLite always runs;
// Postgres and ClickHouse run against a dedicated database named by
// BIFROST_TEST_WARP_POSTGRES_* and BIFROST_TEST_CLICKHOUSE_*, and are skipped
// without one.
func warpTestLogStores(t *testing.T) map[string]func(t *testing.T) logstore.LogStore {
	t.Helper()
	logger := bifrost.NewDefaultLogger(schemas.LogLevelError)
	open := func(t *testing.T, kind logstore.LogStoreType, config any) logstore.LogStore {
		t.Helper()
		ctx := context.Background()
		store, err := logstore.NewLogStore(ctx, &logstore.Config{Enabled: true, Type: kind, Config: config}, logger)
		require.NoError(t, err)
		t.Cleanup(func() { _ = store.Close(ctx) })
		return store
	}
	return map[string]func(t *testing.T) logstore.LogStore{
		"sqlite": func(t *testing.T) logstore.LogStore {
			return open(t, logstore.LogStoreTypeSQLite, &logstore.SQLiteConfig{Path: filepath.Join(t.TempDir(), "logs.db")})
		},
		"postgres": func(t *testing.T) logstore.LogStore {
			host := os.Getenv("BIFROST_TEST_WARP_POSTGRES_HOST")
			if host == "" {
				t.Skip("BIFROST_TEST_WARP_POSTGRES_HOST names no dedicated database")
			}
			return open(t, logstore.LogStoreTypePostgres, &logstore.PostgresConfig{Config: postgresconn.Config{
				Host: schemas.NewSecretVar(host), Port: schemas.NewSecretVar(os.Getenv("BIFROST_TEST_WARP_POSTGRES_PORT")),
				User: schemas.NewSecretVar(os.Getenv("BIFROST_TEST_WARP_POSTGRES_USER")), Password: schemas.NewSecretVar(os.Getenv("BIFROST_TEST_WARP_POSTGRES_PASSWORD")),
				DBName: schemas.NewSecretVar(os.Getenv("BIFROST_TEST_WARP_POSTGRES_DB")), SSLMode: schemas.NewSecretVar("disable"),
			}})
		},
		"clickhouse": func(t *testing.T) logstore.LogStore {
			host := os.Getenv("BIFROST_TEST_CLICKHOUSE_HOST")
			if host == "" {
				t.Skip("BIFROST_TEST_CLICKHOUSE_HOST names no dedicated database")
			}
			return open(t, logstore.LogStoreTypeClickHouse, &logstore.ClickHouseConfig{
				Host: schemas.NewSecretVar(host), Port: schemas.NewSecretVar(os.Getenv("BIFROST_TEST_CLICKHOUSE_PORT")),
				Database: schemas.NewSecretVar(os.Getenv("BIFROST_TEST_CLICKHOUSE_DB")), Username: schemas.NewSecretVar(os.Getenv("BIFROST_TEST_CLICKHOUSE_USER")),
				Password: schemas.NewSecretVar(os.Getenv("BIFROST_TEST_CLICKHOUSE_PASSWORD")),
			})
		},
	}
}

// Every way of grouping one window has to report one failure rate for one set of
// requests. openai: 60 successes, 20 errors, 20 cancelled and 10 in flight. azure:
// 10 successes and 10 errors, on the same model. So 30 errors over 120 finished
// requests on the model bar, 20 over 100 and 10 over 20 on the provider bars, and
// the team bar and the timeline agree.
func TestWarpErrorRatePopulationsAgreeOnTheStore(t *testing.T) {
	for backend, open := range warpTestLogStores(t) {
		t.Run(backend, func(t *testing.T) {
			store := open(t)
			ctx := context.Background()
			start := warpTestWindow()
			team := "team-" + uuid.NewString()
			entries := []*logstore.Log{}
			seed := func(provider, status string, count int) {
				for range count {
					entries = append(entries, &logstore.Log{
						ID: uuid.NewString(), Timestamp: start.Add(30 * time.Minute), Object: string(schemas.ChatCompletionRequest),
						Provider: provider, Model: "gpt-4o", Status: status, TeamID: &team, Latency: new(100.0),
					})
				}
			}
			seed("openai", "success", 60)
			seed("openai", "error", 20)
			seed("openai", "cancelled", 20)
			seed("openai", "processing", 10)
			seed("azure", "success", 10)
			seed("azure", "error", 10)
			require.NoError(t, store.BatchCreateIfNotExists(ctx, entries))

			deps := &ToolDeps{logManager: storeLogReader{store: store}, charts: newChartRegistry()}
			chart := func(kind, group string, status []any) []ChartPoint {
				t.Helper()
				deps.charts = newChartRegistry()
				filters := map[string]any{"start_time": start.Format(time.RFC3339), "end_time": start.Add(2 * time.Hour).Format(time.RFC3339), "team_ids": []any{team}}
				if status != nil {
					filters["status"] = status
				}
				args := map[string]any{"kind": kind, "title": "Failure rate", "metric": "error_rate", "filters": filters}
				if group != "" {
					args["group"] = group
				}
				_, err := runTool(t, RenderChartTool, deps, args)
				require.NoError(t, err)
				spec, _ := deps.charts.get(spec1(t, deps))
				return spec.Points
			}
			require.Equal(t, []ChartPoint{{X: "gpt-4o", Y: 25}}, chart("bar", "model", nil))
			require.ElementsMatch(t, []ChartPoint{{X: "openai", Y: 20}, {X: "azure", Y: 50}}, chart("bar", "provider", nil))
			require.Equal(t, []ChartPoint{{X: team, Y: 25}}, chart("bar", "team", nil))

			// A population holding no errors has no failure rate above zero.
			require.Equal(t, []ChartPoint{{X: "gpt-4o", Y: 0}}, chart("bar", "model", []any{"success"}))
			require.ElementsMatch(t, []ChartPoint{{X: "openai", Y: 0}, {X: "azure", Y: 0}}, chart("bar", "provider", []any{"success"}))
			// Asked for errors alone, everything in the population failed.
			require.Equal(t, []ChartPoint{{X: "gpt-4o", Y: 100}}, chart("bar", "model", []any{"error"}))
		})
	}
}

// BenchmarkWarpBarChartErrorRate measures a failure-rate bar chart over 50 model and
// provider pairs, with the store answering at once.
func BenchmarkWarpBarChartErrorRate(b *testing.B) {
	rankings := make([]logstore.ModelRankingWithTrend, 0, 50)
	for index := range 50 {
		rankings = append(rankings, logstore.ModelRankingWithTrend{ModelRankingEntry: logstore.ModelRankingEntry{
			Model: fmt.Sprintf("model-%d", index/2), Provider: fmt.Sprintf("provider-%d", index%2), TotalRequests: 100, SuccessRate: 90, AvgLatency: 100,
		}})
	}
	fake := &fakeLogReader{modelRankingResult: &logstore.ModelRankingResult{Rankings: rankings}}
	deps := &ToolDeps{logManager: fake}
	filters := &logstore.SearchFilters{}
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := barChartPoints(ctx, deps, filters, "error_rate", "model", 10); err != nil {
			b.Fatal(err)
		}
	}
}
