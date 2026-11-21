package perplexity

import (
	"testing"

	"github.com/capsohq/bifrost/core/schemas"
)

// BenchmarkPerplexityReasoningBudgetError: the check every Perplexity chat request runs before its
// body is built (fork, TXE-8011). A request without a reasoning budget pays a nil check.
func BenchmarkPerplexityReasoningBudgetError(b *testing.B) {
	for name, reasoning := range map[string]*schemas.ChatReasoning{
		"no reasoning": nil,
		"effort":       {Effort: schemas.Ptr("high")},
		"budget":       {MaxTokens: schemas.Ptr(2048)},
	} {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = perplexityReasoningBudgetError(reasoning)
			}
		})
	}
}
