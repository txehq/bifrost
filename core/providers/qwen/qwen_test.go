package qwen_test

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

func TestQwen(t *testing.T) {
	t.Parallel()

	livetest.Require(t, "QWEN_API_KEY")

	client, ctx, cancel, err := llmtests.SetupTest()
	if err != nil {
		t.Fatalf("Error initializing test setup: %v", err)
	}
	defer cancel()

	testConfig := llmtests.ComprehensiveTestConfig{
		Provider:  schemas.Qwen,
		ChatModel: envOrDefault("QWEN_CHAT_MODEL", "qwen-plus-latest"),
		TextModel: envOrDefault("QWEN_TEXT_MODEL", "qwen-plus-latest"),
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

	t.Run("QwenTests", func(t *testing.T) {
		llmtests.RunAllComprehensiveTests(t, client, ctx, testConfig)
	})
	client.Shutdown()
}
