package openai

import (
	"strings"
	"testing"

	"github.com/capsohq/bifrost/core/schemas"
)

// TestToolSearchOutputItem_SanitizesEmbeddedDefinitions pins that a replayed
// tool_search_output loads each definition in the same OpenAI-safe shape the top-level
// tools serialize in: Anthropic-only fields would 400 the request from inside the output.
func TestToolSearchOutputItem_SanitizesEmbeddedDefinitions(t *testing.T) {
	req := &OpenAIResponsesRequest{
		Tools: []schemas.ResponsesTool{{
			Type:                schemas.ResponsesToolTypeFunction,
			Name:                schemas.Ptr("lookup"),
			DeferLoading:        schemas.Ptr(true),
			CacheControl:        &schemas.CacheControl{Type: "ephemeral"},
			InputExamples:       []schemas.ChatToolInputExample{{Input: []byte(`{"q":"x"}`)}},
			EagerInputStreaming: schemas.Ptr(true),
			AllowedCallers:      []string{schemas.ResponsesToolCallerCodeExecutionPrefix + "20250825"},
		}},
		keepDeferLoading: true,
	}

	item := req.toolSearchOutputItem(nil, toolSearchExecutionServer, []string{"lookup"})
	tools := string(item.ToolSearchOutputTools)
	for _, field := range []string{"cache_control", "input_examples", "eager_input_streaming", schemas.ResponsesToolCallerCodeExecutionPrefix} {
		if strings.Contains(tools, field) {
			t.Errorf("tool_search_output.tools carries %q: %s", field, tools)
		}
	}
	if !strings.Contains(tools, `"defer_loading":true`) || !strings.Contains(tools, `"name":"lookup"`) {
		t.Errorf("tool_search_output.tools lost the loaded definition: %s", tools)
	}
	if req.Tools[0].CacheControl == nil || len(req.Tools[0].InputExamples) == 0 {
		t.Error("request tools were mutated")
	}
}
