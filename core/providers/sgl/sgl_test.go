package sgl_test

import (
	"testing"

	"github.com/capsohq/bifrost/core/internal/llmtests"
	"github.com/capsohq/bifrost/core/livetest"

	"github.com/capsohq/bifrost/core/schemas"
)

func TestSGL(t *testing.T) {
	t.Parallel()
	livetest.Require(t, "SGL_BASE_URL")

	client, ctx, cancel, err := llmtests.SetupTest()
	if err != nil {
		t.Fatalf("Error initializing test setup: %v", err)
	}
	defer cancel()
	defer client.Shutdown()

	testConfig := llmtests.ComprehensiveTestConfig{
		Provider:       schemas.SGL,
		ChatModel:      "qwen/qwen2.5-0.5b-instruct",
		VisionModel:    "Qwen/Qwen2.5-VL-7B-Instruct",
		TextModel:      "qwen/qwen2.5-0.5b-instruct",
		EmbeddingModel: "Alibaba-NLP/gte-Qwen2-1.5B-instruct",
		Scenarios: llmtests.TestScenarios{
			TextCompletion:             true,
			SimpleChat:                 true,
			CompletionStream:           true,
			MultiTurnConversation:      true,
			ToolCalls:                  true,
			ToolCallsStreaming:         true,
			MultipleToolCalls:          true,
			MultipleToolCallsStreaming: true,
			End2EndToolCalling:         true,
			AutomaticFunctionCall:      true,
			ImageURL:                   true,
			ImageBase64:                true,
			MultipleImages:             true,
			CompleteEnd2End:            true,
			Embedding:                  true,
			ListModels:                 true,
			PassThroughExtraParams:     true,
		},
	}

	t.Run("SGLTests", func(t *testing.T) {
		llmtests.RunAllComprehensiveTests(t, client, ctx, testConfig)
	})
}
