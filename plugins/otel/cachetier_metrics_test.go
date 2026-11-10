package otel

import (
	"context"
	"testing"
	"time"

	"github.com/capsohq/bifrost/core/schemas"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// TXE-6959: the OTel metrics path reads every cache-write lifetime tier off the final LLM span,
// from either the chat (prompt_token_details) or Responses (input_token_details) namespace, and
// records OpenAI's 30m and 12h tiers on their own counters.
func TestRecordMetricsFromTraceCoversEveryCacheWriteTier(t *testing.T) {
	for _, responses := range []bool{false, true} {
		reader := sdkmetric.NewManualReader()
		provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
		m := &MetricsExporter{provider: provider, meter: provider.Meter("test")}
		m.initMetrics()

		now := time.Now()
		trace := &schemas.Trace{Spans: []*schemas.Span{{
			Kind:      schemas.SpanKindLLMCall,
			StartTime: now, EndTime: now.Add(time.Second),
			Status: schemas.SpanStatusOk,
			Attributes: map[string]any{
				schemas.AttrProviderName:                                           "openai",
				schemas.AttrRequestModel:                                           "gpt-5.6",
				schemas.AttrUsageCacheCreationInputTokens:                          80,
				schemas.CachedWriteTierAttr(responses, schemas.CachedWriteTier30m): 50,
				schemas.CachedWriteTierAttr(responses, schemas.CachedWriteTier12h): 30,
			},
		}}}

		(&OtelPlugin{}).recordMetricsFromTrace(context.Background(), m, trace, false)

		var rm metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatalf("Collect: %v", err)
		}
		for name, want := range map[string]int64{
			"bifrost_cache_write_input_tokens_30m_total": 50,
			"bifrost_cache_write_input_tokens_12h_total": 30,
			"bifrost_cache_write_input_tokens_5m_total":  0,
			"bifrost_cache_write_input_tokens_1h_total":  0,
		} {
			if got := int64SumTotal(&rm, name); got != want {
				t.Errorf("responses=%v: %s = %d, want %d", responses, name, got, want)
			}
		}
	}
}

// int64SumTotal sums every data point of the named int64 counter; 0 when it was never recorded.
func int64SumTotal(rm *metricdata.ResourceMetrics, name string) int64 {
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, metric := range sm.Metrics {
			if metric.Name != name {
				continue
			}
			if sum, ok := metric.Data.(metricdata.Sum[int64]); ok {
				for _, dp := range sum.DataPoints {
					total += dp.Value
				}
			}
		}
	}
	return total
}
