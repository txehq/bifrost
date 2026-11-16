package glm_test

import (
	"os"
	"strings"
	"testing"

	"github.com/capsohq/bifrost/core/internal/llmtests"
	"github.com/capsohq/bifrost/core/livetest"
	"github.com/capsohq/bifrost/core/schemas"
)

func envOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func TestGLM(t *testing.T) {
	t.Parallel()

	livetest.Require(t, "GLM_API_KEY")

	client, ctx, cancel, err := llmtests.SetupTest()
	if err != nil {
		t.Fatalf("Error initializing test setup: %v", err)
	}
	defer cancel()

	testConfig := llmtests.ComprehensiveTestConfig{
		Provider:  schemas.GLM,
		ChatModel: envOrDefault("GLM_CHAT_MODEL", "glm-5"),
		TextModel: envOrDefault("GLM_TEXT_MODEL", "glm-4.7"),
		Scenarios: llmtests.TestScenarios{
			TextCompletion:        true,
			TextCompletionStream:  true,
			SimpleChat:            true,
			CompletionStream:      true,
			MultiTurnConversation: true,
			ToolCalls:             true,
			MultipleToolCalls:     true,
			End2EndToolCalling:    true,
			AutomaticFunctionCall: true,
			ListModels:            true,
		},
	}

	t.Run("GLMTests", func(t *testing.T) {
		llmtests.RunAllComprehensiveTests(t, client, ctx, testConfig)
	})
	client.Shutdown()
}
