package runway_test

import (
	"testing"

	"github.com/capsohq/bifrost/core/internal/llmtests"
	"github.com/capsohq/bifrost/core/livetest"
	"github.com/capsohq/bifrost/core/schemas"
)

func TestRunway(t *testing.T) {
	t.Parallel()
	livetest.Require(t, "RUNWAY_API_KEY")

	client, ctx, cancel, err := llmtests.SetupTest()
	if err != nil {
		t.Fatalf("Error initializing test setup: %v", err)
	}
	defer cancel()
	defer client.Shutdown()

	testConfig := llmtests.ComprehensiveTestConfig{
		Provider:             schemas.Runway,
		VideoGenerationModel: "gen4.5",
		ImageGenerationModel: "gen4_image",
		ImageEditModel:       "gen4_image",
		Scenarios: llmtests.TestScenarios{
			ImageGeneration: true,
			ImageEdit:       true,
			VideoGeneration: false, // disabled for now because of long running operations
			VideoRetrieve:   false,
			VideoRemix:      false,
			VideoDownload:   false,
			VideoList:       false,
			VideoDelete:     false,
		},
	}

	t.Run("RunwayTests", func(t *testing.T) {
		llmtests.RunAllComprehensiveTests(t, client, ctx, testConfig)
	})
}
