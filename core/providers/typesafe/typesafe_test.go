package typesafe_test

import (
	"testing"

	"github.com/capsohq/bifrost/core/internal/llmtests"
	"github.com/capsohq/bifrost/core/livetest"
	"github.com/capsohq/bifrost/core/schemas"
)

func TestTypesafe(t *testing.T) {
	t.Parallel()
	livetest.Require(t, "TYPESAFE_API_KEY")

	client, ctx, cancel, err := llmtests.SetupTest()
	if err != nil {
		t.Fatalf("Error initializing test setup: %v", err)
	}
	defer cancel()
	defer client.Shutdown()

	testConfig := llmtests.ComprehensiveTestConfig{
		Provider:      schemas.Typesafe,
		DecisionModel: "jev-1.13.0",
		Scenarios: llmtests.TestScenarios{
			// Typesafe serves the decision operation only; every other operation
			// returns the standard unsupported-operation error. ListModels is
			// served from the static catalog without an upstream call.
			Decision:   true,
			ListModels: true,
		},
	}

	t.Run("TypesafeTests", func(t *testing.T) {
		llmtests.RunAllComprehensiveTests(t, client, ctx, testConfig)
	})
}
