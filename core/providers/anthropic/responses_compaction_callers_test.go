package anthropic

import (
	"testing"

	"github.com/capsohq/bifrost/core/schemas"
)

// TestAnthropicRoundTrip_CompactionToolChangeResolvesProgrammaticCaller: a function added by
// value inside a compaction block's tool_changes gets the same programmatic caller resolution
// as a top-level tool_addition. Validation already accepts the nested restriction, so dropping
// it on egress would silently widen the tool to any caller.
func TestAnthropicRoundTrip_CompactionToolChangeResolvesProgrammaticCaller(t *testing.T) {
	t.Parallel()
	req := inlineProgrammaticRequest(string(AnthropicToolTypeCodeExecution20260120))
	addition := req.Input[1].Content.ContentBlocks[0]
	req.Input[1] = schemas.ResponsesMessage{
		Role: schemas.Ptr(schemas.ResponsesInputMessageRoleAssistant),
		Content: &schemas.ResponsesMessageContent{ContentBlocks: []schemas.ResponsesMessageContentBlock{{
			Type: schemas.ResponsesOutputMessageContentTypeCompaction,
			ResponsesOutputMessageContentCompaction: &schemas.ResponsesOutputMessageContentCompaction{
				Summary:     "earlier turns",
				ToolChanges: []schemas.ResponsesMessageContentBlock{addition},
			},
		}}},
	}

	out, err := ToAnthropicResponsesRequest(effortTestCtx(t), req)
	if err != nil {
		t.Fatalf("ToAnthropicResponsesRequest: %v", err)
	}
	var def *AnthropicTool
	for _, m := range out.Messages {
		for _, b := range m.Content.ContentBlocks {
			for _, change := range b.ToolChanges {
				if change.Type == AnthropicContentBlockTypeToolAddition && change.Tool != nil {
					def = change.Tool.Definition
				}
			}
		}
	}
	if def == nil {
		t.Fatalf("compaction tool_changes definition missing on the wire: %+v", out.Messages)
	}
	if len(def.AllowedCallers) != 1 || def.AllowedCallers[0] != string(AnthropicToolTypeCodeExecution20260120) {
		t.Errorf("allowed_callers = %v, want [%s] (programmatic caller dropped, tool widened to any caller)", def.AllowedCallers, AnthropicToolTypeCodeExecution20260120)
	}
	nested := req.Input[1].Content.ContentBlocks[0].ResponsesOutputMessageContentCompaction.ToolChanges[0].ToolChange.Definition
	if len(nested.AllowedCallers) != 1 || nested.AllowedCallers[0] != schemas.ResponsesToolCallerProgrammatic {
		t.Errorf("caller-owned input mutated: allowed_callers = %v", nested.AllowedCallers)
	}
}
