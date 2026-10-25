package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// HELPERS
// =============================================================================

// mockToolClientManager is a ClientManager that returns a pre-defined set of MCP tools.
// It is used to drive ParseAndAddToolsToRequest without a real MCP server.
type mockToolClientManager struct {
	tools []schemas.ChatTool
}

func (m *mockToolClientManager) GetClientByName(clientName string) *schemas.MCPClientState {
	if clientName == "test-client" {
		return &schemas.MCPClientState{
			Name: "test-client",
			ExecutionConfig: &schemas.MCPClientConfig{
				ID:               "test-client",
				Name:             "test-client",
				IsCodeModeClient: false,
				ToolsToExecute:   []string{"*"},
			},
		}
	}
	return nil
}

func (m *mockToolClientManager) GetClientForTool(toolName string) *schemas.MCPClientState {
	return nil
}

func (m *mockToolClientManager) GetToolPerClient(ctx context.Context) map[string][]schemas.ChatTool {
	return map[string][]schemas.ChatTool{
		"test-client": m.tools,
	}
}

func (m *mockToolClientManager) GetPluginPipeline() PluginPipeline             { return nil }
func (m *mockToolClientManager) ReleasePluginPipeline(pipeline PluginPipeline) {}
func (m *mockToolClientManager) AcquireClientConn(ctx *schemas.BifrostContext, state *schemas.MCPClientState) (*client.Client, func(), error) {
	return nil, func() {}, nil
}
func (m *mockToolClientManager) ReconnectClient(id string) error { return nil }

func (m *mockToolClientManager) AwaitReconnect(clientID string, budget time.Duration) (bool, error) {
	return false, nil
}
func (m *mockToolClientManager) RunWithPluginPipeline(ctx *schemas.BifrostContext, req *schemas.BifrostMCPRequest, op MCPOpFunc) (*schemas.BifrostMCPResponse, *schemas.BifrostError) {
	resp, err := op(req)
	if err != nil {
		return nil, &schemas.BifrostError{IsBifrostError: false, Error: &schemas.ErrorField{Message: err.Error()}}
	}
	return resp, nil
}

// makeTool is a convenience constructor for test tool fixtures.
func makeTool(name string) schemas.ChatTool {
	return schemas.ChatTool{
		Type: schemas.ChatToolTypeFunction,
		Function: &schemas.ChatToolFunction{
			Name: name,
		},
	}
}

// toolNamesFromChatRequest collects the names of every tool in the request's
// ChatRequest.Params.Tools slice.
func toolNamesFromChatRequest(req *schemas.BifrostRequest) []string {
	if req.ChatRequest == nil || req.ChatRequest.Params == nil {
		return nil
	}
	names := make([]string, 0, len(req.ChatRequest.Params.Tools))
	for _, t := range req.ChatRequest.Params.Tools {
		if t.Function != nil {
			names = append(names, t.Function.Name)
		}
	}
	return names
}

// toolNamesFromResponsesRequest collects the names of every tool in the request's
// ResponsesRequest.Params.Tools slice (ResponsesParameters uses *string Name).
func toolNamesFromResponsesRequest(req *schemas.BifrostRequest) []string {
	if req.ResponsesRequest == nil || req.ResponsesRequest.Params == nil {
		return nil
	}
	names := make([]string, 0, len(req.ResponsesRequest.Params.Tools))
	for _, t := range req.ResponsesRequest.Params.Tools {
		if t.Name != nil {
			names = append(names, *t.Name)
		}
	}
	return names
}

// countOccurrences returns how many times name appears in slice.
func countOccurrences(slice []string, name string) int {
	n := 0
	for _, s := range slice {
		if s == name {
			n++
		}
	}
	return n
}

// newToolsManagerForTest creates a minimal ToolsManager backed by the provided
// ClientManager, with no code mode, no plugin pipeline, and a no-op logger.
func newToolsManagerForTest(cm ClientManager) *ToolsManager {
	return NewToolsManager(
		&schemas.MCPToolManagerConfig{
			MaxAgentDepth: 5,
		},
		cm,
		nil, // fetchNewRequestIDFunc
		nil, // oauth2Provider
		&MockLogger{},
	)
}

// contextWithUserAgent creates a BifrostContext with BifrostContextKeyUserAgent set.
func contextWithUserAgent(ua string) *schemas.BifrostContext {
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	ctx.SetValue(schemas.BifrostContextKeyUserAgent, ua)
	return ctx
}

// =============================================================================
// buildIntegrationDuplicateCheckMap – unit tests
// =============================================================================

func TestBuildIntegrationDuplicateCheckMap_EmptyInputs(t *testing.T) {
	t.Parallel()

	m := buildIntegrationDuplicateCheckMap(nil, "", defaultLogger)
	if len(m) != 0 {
		t.Errorf("expected empty map for nil tools and no agent, got %d entries", len(m))
	}
}

func TestBuildIntegrationDuplicateCheckMap_NoUserAgent_DirectMatch(t *testing.T) {
	t.Parallel()

	tools := []schemas.ChatTool{
		makeTool("echo"),
		makeTool("calculator"),
	}

	m := buildIntegrationDuplicateCheckMap(tools, "", defaultLogger)

	for _, want := range []string{"echo", "calculator"} {
		if !m[want] {
			t.Errorf("expected %q to be in map", want)
		}
	}
	if len(m) != 2 {
		t.Errorf("expected exactly 2 entries, got %d", len(m))
	}
}

func TestBuildIntegrationDuplicateCheckMap_NilFunction_IsSkipped(t *testing.T) {
	t.Parallel()

	tools := []schemas.ChatTool{
		{Type: schemas.ChatToolTypeFunction, Function: nil}, // nil Function
		makeTool(""),           // empty name
		makeTool("valid_tool"), // valid
	}

	m := buildIntegrationDuplicateCheckMap(tools, "", defaultLogger)

	if !m["valid_tool"] {
		t.Error("expected valid_tool to be in map")
	}
	// "" is technically inserted because the loop guard is `Name != ""` — nothing else
	if m[""] {
		t.Error("empty tool name should not be inserted")
	}
	// nil Function is skipped entirely; no panic
}

func TestBuildIntegrationDuplicateCheckMap_UnknownAgent_FallsBackToDirectMatch(t *testing.T) {
	t.Parallel()

	tools := []schemas.ChatTool{
		makeTool("search"),
		makeTool("weather"),
	}

	m := buildIntegrationDuplicateCheckMap(tools, "unknown-agent-xyz", defaultLogger)

	for _, want := range []string{"search", "weather"} {
		if !m[want] {
			t.Errorf("expected %q in map for unknown agent", want)
		}
	}
	if len(m) != 2 {
		t.Errorf("expected 2 entries for unknown agent, got %d", len(m))
	}
}

// ---------------------------------------------------------------------------
// ClaudeCLI — pattern: mcp__{server}__{tool_name}
// ---------------------------------------------------------------------------

func TestBuildIntegrationDuplicateCheckMap_ClaudeCLI_StripsPrefix(t *testing.T) {
	t.Parallel()

	// These are the tool names Claude CLI sends when it has already connected to a
	// Bifrost MCP server. The prefix is mcp__{server}__{tool_name}.
	tools := []schemas.ChatTool{
		makeTool("mcp__bifrost__echo"),
		makeTool("mcp__bifrost__calculator"),
	}

	m := buildIntegrationDuplicateCheckMap(tools, schemas.ClaudeCLI.String(), defaultLogger)

	// Bare tool names must be recognised so Bifrost does not inject them again.
	for _, want := range []string{"echo", "calculator"} {
		if !m[want] {
			t.Errorf("ClaudeCLI: expected bare name %q to be in duplicate map", want)
		}
	}
	// Original prefixed names must also be present.
	for _, want := range []string{"mcp__bifrost__echo", "mcp__bifrost__calculator"} {
		if !m[want] {
			t.Errorf("ClaudeCLI: expected prefixed name %q to be in duplicate map", want)
		}
	}
}

func TestBuildIntegrationDuplicateCheckMap_ClaudeCLI_MultipleServers(t *testing.T) {
	t.Parallel()

	tools := []schemas.ChatTool{
		makeTool("mcp__bifrost__executeToolCode"),
		makeTool("mcp__bifrost__listToolFiles"),
		makeTool("mcp__calculator__calculator_add"),
	}

	m := buildIntegrationDuplicateCheckMap(tools, schemas.ClaudeCLI.String(), defaultLogger)

	// Last segment is always the bare tool name.
	for _, want := range []string{"executeToolCode", "listToolFiles", "calculator_add"} {
		if !m[want] {
			t.Errorf("ClaudeCLI multi-server: expected bare name %q in map", want)
		}
	}
}

func TestBuildIntegrationDuplicateCheckMap_ClaudeCLI_TwoPartName_NotExtracted(t *testing.T) {
	t.Parallel()

	// A two-segment name like "mcp__only_two" doesn't satisfy the len(parts) >= 3 guard,
	// so only the raw name ends up in the map — the "only_two" portion is NOT extracted.
	tools := []schemas.ChatTool{
		makeTool("mcp__only_two"),
	}

	m := buildIntegrationDuplicateCheckMap(tools, schemas.ClaudeCLI.String(), defaultLogger)

	if !m["mcp__only_two"] {
		t.Error("ClaudeCLI: original two-part name should still appear as direct match")
	}
	if m["only_two"] {
		t.Error("ClaudeCLI: bare segment from two-part name should NOT be extracted")
	}
}

func TestBuildIntegrationDuplicateCheckMap_ClaudeCLI_ToolWithUnderscores(t *testing.T) {
	t.Parallel()

	// Tool names that themselves contain underscores must not be split — only the
	// double-underscore __ delimiter is meaningful.
	tools := []schemas.ChatTool{
		makeTool("mcp__my_server__get_weather_data"),
	}

	m := buildIntegrationDuplicateCheckMap(tools, schemas.ClaudeCLI.String(), defaultLogger)

	// The entire last segment, including its internal underscores, is the tool name.
	if !m["get_weather_data"] {
		t.Error("ClaudeCLI: tool name with underscores should be extracted as-is")
	}
	if !m["mcp__my_server__get_weather_data"] {
		t.Error("ClaudeCLI: original prefixed name should be retained")
	}
	// Partial segments must not appear.
	for _, unexpected := range []string{"get", "weather", "data"} {
		if m[unexpected] {
			t.Errorf("ClaudeCLI: unexpected partial segment %q in map", unexpected)
		}
	}
}

func TestBuildIntegrationDuplicateCheckMap_ClaudeCLI_MixedPrefixedAndBare(t *testing.T) {
	t.Parallel()

	// A request might contain some already-prefixed tools from one MCP server and some
	// bare tools injected directly (e.g., by a plugin). Both should be in the map.
	tools := []schemas.ChatTool{
		makeTool("mcp__bifrost__echo"),
		makeTool("search"), // already bare
	}

	m := buildIntegrationDuplicateCheckMap(tools, schemas.ClaudeCLI.String(), defaultLogger)

	for _, want := range []string{"echo", "mcp__bifrost__echo", "search"} {
		if !m[want] {
			t.Errorf("ClaudeCLI mixed: expected %q in map", want)
		}
	}
}

// ---------------------------------------------------------------------------
// GeminiCLI — pattern: mcp_{server}_{tool_name} (single underscore)
// The current implementation treats GeminiCLI tools as direct matches only.
// These tests document the current (direct-match) behaviour and also define the
// expected behaviour once prefix-stripping for the mcp_{server}_{tool} pattern
// is added in a follow-up.
// ---------------------------------------------------------------------------

func TestBuildIntegrationDuplicateCheckMap_GeminiCLI_DirectMatch(t *testing.T) {
	t.Parallel()

	tools := []schemas.ChatTool{
		makeTool("echo"),
		makeTool("calculator"),
	}

	m := buildIntegrationDuplicateCheckMap(tools, schemas.GeminiCLI.String(), defaultLogger)

	for _, want := range []string{"echo", "calculator"} {
		if !m[want] {
			t.Errorf("GeminiCLI direct match: expected %q in map", want)
		}
	}
}

// TestBuildIntegrationDuplicateCheckMap_GeminiCLI_PrefixedTools_StripsPrefix verifies that
// the GeminiCLI case correctly extracts the tool name from the mcp_{server}_{tool} pattern
// by stripping "mcp_" and skipping past the first "_" (server name boundary).
func TestBuildIntegrationDuplicateCheckMap_GeminiCLI_PrefixedTools_StripsPrefix(t *testing.T) {
	t.Parallel()

	tools := []schemas.ChatTool{
		makeTool("mcp_bifrost_echo"),
		makeTool("mcp_bifrost_calculator"),
	}

	m := buildIntegrationDuplicateCheckMap(tools, schemas.GeminiCLI.String(), defaultLogger)

	for _, want := range []string{"echo", "calculator"} {
		if !m[want] {
			t.Errorf("GeminiCLI: expected bare name %q after prefix stripping", want)
		}
	}
}

// TestBuildIntegrationDuplicateCheckMap_GeminiCLI_ClientPrefixedTools verifies the
// dual-gateway scenario: Bifrost stores tools as "{client}-{tool}" and Gemini CLI
// wraps them as "mcp_{server}_{client}-{tool}". The dedup must extract "{client}-{tool}".
func TestBuildIntegrationDuplicateCheckMap_GeminiCLI_ClientPrefixedTools(t *testing.T) {
	t.Parallel()

	tools := []schemas.ChatTool{
		makeTool("mcp_bifrost_testing_exa-web_fetch_exa"),
		makeTool("mcp_bifrost_testing_exa-web_search_exa"),
		makeTool("mcp_bifrost_testing_websets-cancel_enrichment"),
		makeTool("mcp_bifrost_ctx7-resolve-library-id"),
	}

	m := buildIntegrationDuplicateCheckMap(tools, schemas.GeminiCLI.String(), defaultLogger)

	for _, want := range []string{
		"testing_exa-web_fetch_exa",
		"testing_exa-web_search_exa",
		"testing_websets-cancel_enrichment",
		"ctx7-resolve-library-id",
	} {
		if !m[want] {
			t.Errorf("GeminiCLI client-prefixed: expected %q in duplicate map", want)
		}
	}
}

// ---------------------------------------------------------------------------
// New integrations — Cursor, Codex CLI, n8n, Qwen CLI
//
// These agents are expected to use tool names without provider-specific prefixes
// (i.e. direct matching). Once the corresponding constants are added to
// core/schemas/useragents.go and the switch cases are wired in, these tests
// verify that the deduplication map is correctly populated.
//
// String literals are used instead of schema constants because the constants do
// not exist yet. Replace with schemas.CursorEditor.String() etc. once the
// constants land.
// ---------------------------------------------------------------------------

func TestBuildIntegrationDuplicateCheckMap_CursorEditor_DirectMatch(t *testing.T) {
	t.Parallel()

	const cursorUA = "cursor"

	tools := []schemas.ChatTool{
		makeTool("echo"),
		makeTool("read_file"),
	}

	m := buildIntegrationDuplicateCheckMap(tools, cursorUA, defaultLogger)

	for _, want := range []string{"echo", "read_file"} {
		if !m[want] {
			t.Errorf("Cursor: expected %q in duplicate map", want)
		}
	}
	if len(m) != 2 {
		t.Errorf("Cursor: expected 2 entries, got %d", len(m))
	}
}

func TestBuildIntegrationDuplicateCheckMap_CodexCLI_DirectMatch(t *testing.T) {
	t.Parallel()

	const codexUA = "codex"

	tools := []schemas.ChatTool{
		makeTool("bash"),
		makeTool("list_files"),
	}

	m := buildIntegrationDuplicateCheckMap(tools, codexUA, defaultLogger)

	for _, want := range []string{"bash", "list_files"} {
		if !m[want] {
			t.Errorf("Codex: expected %q in duplicate map", want)
		}
	}
}

func TestBuildIntegrationDuplicateCheckMap_N8N_DirectMatch(t *testing.T) {
	t.Parallel()

	const n8nUA = "n8n"

	tools := []schemas.ChatTool{
		makeTool("http_request"),
		makeTool("send_email"),
	}

	m := buildIntegrationDuplicateCheckMap(tools, n8nUA, defaultLogger)

	for _, want := range []string{"http_request", "send_email"} {
		if !m[want] {
			t.Errorf("n8n: expected %q in duplicate map", want)
		}
	}
}

func TestBuildIntegrationDuplicateCheckMap_QwenCLI_DirectMatch(t *testing.T) {
	t.Parallel()

	const qwenUA = "qwen-code"

	tools := []schemas.ChatTool{
		makeTool("code_interpreter"),
		makeTool("web_search"),
	}

	m := buildIntegrationDuplicateCheckMap(tools, qwenUA, defaultLogger)

	for _, want := range []string{"code_interpreter", "web_search"} {
		if !m[want] {
			t.Errorf("Qwen: expected %q in duplicate map", want)
		}
	}
}

// TestBuildIntegrationDuplicateCheckMap_CodexCLI_ClientPrefixedTools verifies the
// dual-gateway scenario for Codex CLI: format is mcp__{server}__{tool_name} but ALL
// hyphens in the original Bifrost tool name are converted to underscores. The dedup
// map stores the all-underscore form; callers must normalize before lookup.
func TestBuildIntegrationDuplicateCheckMap_CodexCLI_ClientPrefixedTools(t *testing.T) {
	t.Parallel()

	tools := []schemas.ChatTool{
		makeTool("mcp__bifrost__testing_exa_web_fetch_exa"),
		makeTool("mcp__bifrost__testing_exa_web_search_exa"),
		makeTool("mcp__bifrost__testing_websets_cancel_enrichment"),
		makeTool("mcp__bifrost__ctx7_query_docs"),
	}

	m := buildIntegrationDuplicateCheckMap(tools, schemas.CodexCLI.String(), defaultLogger)

	// All-underscore forms must be in the map (callers normalize Bifrost names before lookup).
	for _, want := range []string{
		"testing_exa_web_fetch_exa",
		"testing_exa_web_search_exa",
		"testing_websets_cancel_enrichment",
		"ctx7_query_docs",
	} {
		if !m[want] {
			t.Errorf("CodexCLI: expected %q in duplicate map", want)
		}
	}
}

// TestBuildIntegrationDuplicateCheckMap_QwenCLI_ClientPrefixedTools verifies the
// dual-gateway scenario for Qwen CLI: format is mcp__{server}__{tool_name} (double
// underscores). Strip "mcp__" then skip past first "__" to get the Bifrost tool name.
func TestBuildIntegrationDuplicateCheckMap_QwenCLI_ClientPrefixedTools(t *testing.T) {
	t.Parallel()

	tools := []schemas.ChatTool{
		makeTool("mcp__bifrost__testing_exa-web_fetch_exa"),
		makeTool("mcp__bifrost__testing_exa-web_search_exa"),
		makeTool("mcp__bifrost__testing_websets-cancel_enrichment"),
		makeTool("mcp__bifrost__ctx7-resolve-library-id"),
	}

	m := buildIntegrationDuplicateCheckMap(tools, schemas.QwenCodeCLI.String(), defaultLogger)

	for _, want := range []string{
		"testing_exa-web_fetch_exa",
		"testing_exa-web_search_exa",
		"testing_websets-cancel_enrichment",
		"ctx7-resolve-library-id",
	} {
		if !m[want] {
			t.Errorf("QwenCLI client-prefixed: expected %q in duplicate map", want)
		}
	}
}

// =============================================================================
// ParseAndAddToolsToRequest – end-to-end deduplication tests
//
// These tests wire a ToolsManager backed by a mockToolClientManager and verify
// that ParseAndAddToolsToRequest does not inject duplicate tools when the
// request already carries tools that match MCP-registered ones.
// =============================================================================

// buildChatRequest builds a minimal BifrostChatRequest with the given pre-existing
// tool names already populated in Params.Tools.
func buildChatRequest(existingToolNames ...string) *schemas.BifrostRequest {
	tools := make([]schemas.ChatTool, 0, len(existingToolNames))
	for _, name := range existingToolNames {
		tools = append(tools, makeTool(name))
	}
	return &schemas.BifrostRequest{
		ChatRequest: &schemas.BifrostChatRequest{
			Provider: schemas.OpenAI,
			Model:    "gpt-4",
			Params: &schemas.ChatParameters{
				Tools: tools,
			},
		},
		RequestType: schemas.ChatCompletionRequest,
	}
}

func TestParseAndAddToolsToRequest_NoUserAgent_NoDuplicate(t *testing.T) {
	t.Parallel()

	cm := &mockToolClientManager{
		tools: []schemas.ChatTool{
			makeTool("echo"),
			makeTool("calculator"),
		},
	}
	tm := newToolsManagerForTest(cm)

	// Request already has "echo" — Bifrost should not add it again.
	req := buildChatRequest("echo")
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)

	result := tm.ParseAndAddToolsToRequest(ctx, req)

	names := toolNamesFromChatRequest(result)
	if countOccurrences(names, "echo") != 1 {
		t.Errorf("expected exactly 1 'echo', got %d (names: %v)", countOccurrences(names, "echo"), names)
	}
	// "calculator" is not in the existing tools so it should be added exactly once.
	if countOccurrences(names, "calculator") != 1 {
		t.Errorf("expected exactly 1 'calculator', got %d", countOccurrences(names, "calculator"))
	}
}

func TestParseAndAddToolsToRequest_NoUserAgent_HyphenUnderscoreDistinct(t *testing.T) {
	t.Parallel()

	cm := &mockToolClientManager{
		tools: []schemas.ChatTool{
			makeTool("foo-bar"),
			makeTool("foo_bar"),
		},
	}
	tm := newToolsManagerForTest(cm)

	req := buildChatRequest()
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)

	result := tm.ParseAndAddToolsToRequest(ctx, req)

	names := toolNamesFromChatRequest(result)
	if countOccurrences(names, "foo-bar") != 1 || countOccurrences(names, "foo_bar") != 1 {
		t.Errorf("without Codex UA, foo-bar and foo_bar are distinct tools; got names %v", names)
	}
}

func TestParseAndAddToolsToRequest_ClaudeCLI_NoDuplicate(t *testing.T) {
	t.Parallel()

	// Available MCP tools registered on the Bifrost server.
	cm := &mockToolClientManager{
		tools: []schemas.ChatTool{
			makeTool("echo"),
			makeTool("calculator"),
		},
	}
	tm := newToolsManagerForTest(cm)

	// Claude CLI already carries the tools under the mcp__{server}__{name} pattern.
	req := buildChatRequest("mcp__bifrost__echo", "mcp__bifrost__calculator")
	ctx := contextWithUserAgent(schemas.ClaudeCLI.String())

	result := tm.ParseAndAddToolsToRequest(ctx, req)

	names := toolNamesFromChatRequest(result)

	// The bare names must NOT be injected as new entries — they were already present
	// under the prefixed names.
	if countOccurrences(names, "echo") != 0 {
		t.Errorf("ClaudeCLI: bare 'echo' should not be added again, got %d occurrences (names: %v)",
			countOccurrences(names, "echo"), names)
	}
	if countOccurrences(names, "calculator") != 0 {
		t.Errorf("ClaudeCLI: bare 'calculator' should not be added again, got %d occurrences",
			countOccurrences(names, "calculator"))
	}
	// Prefixed originals must still be present exactly once.
	for _, want := range []string{"mcp__bifrost__echo", "mcp__bifrost__calculator"} {
		if countOccurrences(names, want) != 1 {
			t.Errorf("ClaudeCLI: expected exactly 1 %q, got %d (names: %v)", want,
				countOccurrences(names, want), names)
		}
	}
}

func TestParseAndAddToolsToRequest_ClaudeCLI_NewToolsInjected(t *testing.T) {
	t.Parallel()

	// MCP server exposes echo and search. Claude CLI has only echo (prefixed).
	// Bifrost should inject search (not already present in any form).
	cm := &mockToolClientManager{
		tools: []schemas.ChatTool{
			makeTool("echo"),
			makeTool("search"),
		},
	}
	tm := newToolsManagerForTest(cm)

	req := buildChatRequest("mcp__bifrost__echo")
	ctx := contextWithUserAgent(schemas.ClaudeCLI.String())

	result := tm.ParseAndAddToolsToRequest(ctx, req)

	names := toolNamesFromChatRequest(result)

	// echo is covered by the prefixed entry — must not be injected again.
	if countOccurrences(names, "echo") != 0 {
		t.Errorf("ClaudeCLI new tool: bare 'echo' injected unexpectedly (names: %v)", names)
	}
	// search is new — must be injected exactly once.
	if countOccurrences(names, "search") != 1 {
		t.Errorf("ClaudeCLI new tool: 'search' should be injected once, got %d (names: %v)",
			countOccurrences(names, "search"), names)
	}
}

func TestParseAndAddToolsToRequest_GeminiCLI_NoDuplicate(t *testing.T) {
	t.Parallel()

	cm := &mockToolClientManager{
		tools: []schemas.ChatTool{
			makeTool("echo"),
		},
	}
	tm := newToolsManagerForTest(cm)

	// Gemini CLI sends tools with their bare names (no prefix in current implementation).
	req := buildChatRequest("echo")
	ctx := contextWithUserAgent(schemas.GeminiCLI.String())

	result := tm.ParseAndAddToolsToRequest(ctx, req)

	names := toolNamesFromChatRequest(result)
	if countOccurrences(names, "echo") != 1 {
		t.Errorf("GeminiCLI: expected exactly 1 'echo', got %d (names: %v)",
			countOccurrences(names, "echo"), names)
	}
}

func TestParseAndAddToolsToRequest_CursorEditor_NoDuplicate(t *testing.T) {
	t.Parallel()

	const cursorUA = "cursor"

	cm := &mockToolClientManager{
		tools: []schemas.ChatTool{
			makeTool("echo"),
		},
	}
	tm := newToolsManagerForTest(cm)

	req := buildChatRequest("echo")
	ctx := contextWithUserAgent(cursorUA)

	result := tm.ParseAndAddToolsToRequest(ctx, req)

	names := toolNamesFromChatRequest(result)
	if countOccurrences(names, "echo") != 1 {
		t.Errorf("Cursor: expected exactly 1 'echo', got %d (names: %v)",
			countOccurrences(names, "echo"), names)
	}
}

func TestParseAndAddToolsToRequest_CodexCLI_NoDuplicate(t *testing.T) {
	t.Parallel()

	const codexUA = "codex"

	cm := &mockToolClientManager{
		tools: []schemas.ChatTool{
			makeTool("bash"),
		},
	}
	tm := newToolsManagerForTest(cm)

	req := buildChatRequest("bash")
	ctx := contextWithUserAgent(codexUA)

	result := tm.ParseAndAddToolsToRequest(ctx, req)

	names := toolNamesFromChatRequest(result)
	if countOccurrences(names, "bash") != 1 {
		t.Errorf("Codex: expected exactly 1 'bash', got %d (names: %v)",
			countOccurrences(names, "bash"), names)
	}
}

func TestParseAndAddToolsToRequest_CodexCLI_MCPHyphenUnderscoreVariantsDeduped(t *testing.T) {
	t.Parallel()

	cm := &mockToolClientManager{
		tools: []schemas.ChatTool{
			makeTool("foo-bar"),
			makeTool("foo_bar"),
		},
	}
	tm := newToolsManagerForTest(cm)

	req := buildChatRequest()
	ctx := contextWithUserAgent(schemas.CodexCLI.String())

	result := tm.ParseAndAddToolsToRequest(ctx, req)

	names := toolNamesFromChatRequest(result)
	if len(names) != 1 {
		t.Fatalf("Codex: hyphen/underscore variants should yield one injected tool, got %d: %v", len(names), names)
	}
	if countOccurrences(names, "foo-bar")+countOccurrences(names, "foo_bar") != 1 {
		t.Errorf("Codex: expected exactly one of foo-bar or foo_bar, got %v", names)
	}
}

func TestParseAndAddToolsToRequest_N8N_NoDuplicate(t *testing.T) {
	t.Parallel()

	const n8nUA = "n8n"

	cm := &mockToolClientManager{
		tools: []schemas.ChatTool{
			makeTool("http_request"),
		},
	}
	tm := newToolsManagerForTest(cm)

	req := buildChatRequest("http_request")
	ctx := contextWithUserAgent(n8nUA)

	result := tm.ParseAndAddToolsToRequest(ctx, req)

	names := toolNamesFromChatRequest(result)
	if countOccurrences(names, "http_request") != 1 {
		t.Errorf("n8n: expected exactly 1 'http_request', got %d (names: %v)",
			countOccurrences(names, "http_request"), names)
	}
}

func TestParseAndAddToolsToRequest_QwenCLI_NoDuplicate(t *testing.T) {
	t.Parallel()

	const qwenUA = "qwen-code"

	cm := &mockToolClientManager{
		tools: []schemas.ChatTool{
			makeTool("code_interpreter"),
		},
	}
	tm := newToolsManagerForTest(cm)

	req := buildChatRequest("code_interpreter")
	ctx := contextWithUserAgent(qwenUA)

	result := tm.ParseAndAddToolsToRequest(ctx, req)

	names := toolNamesFromChatRequest(result)
	if countOccurrences(names, "code_interpreter") != 1 {
		t.Errorf("Qwen: expected exactly 1 'code_interpreter', got %d (names: %v)",
			countOccurrences(names, "code_interpreter"), names)
	}
}

// =============================================================================
// Responses API deduplication – mirrors the ChatRequest tests above but for
// the /responses endpoint path in ParseAndAddToolsToRequest.
// =============================================================================

// buildResponsesRequest creates a minimal BifrostResponsesRequest with the given
// pre-existing tool names already populated in Params.Tools.
func buildResponsesRequest(existingToolNames ...string) *schemas.BifrostRequest {
	tools := make([]schemas.ResponsesTool, 0, len(existingToolNames))
	for _, name := range existingToolNames {
		n := name // capture
		tools = append(tools, schemas.ResponsesTool{Name: &n})
	}
	return &schemas.BifrostRequest{
		ResponsesRequest: &schemas.BifrostResponsesRequest{
			Provider: schemas.OpenAI,
			Model:    "gpt-4",
			Params: &schemas.ResponsesParameters{
				Tools: tools,
			},
		},
		RequestType: schemas.ResponsesRequest,
	}
}

func TestParseAndAddToolsToRequest_ResponsesAPI_ClaudeCLI_NoDuplicate(t *testing.T) {
	t.Parallel()

	cm := &mockToolClientManager{
		tools: []schemas.ChatTool{
			makeTool("echo"),
		},
	}
	tm := newToolsManagerForTest(cm)

	// Responses API: tool is carried under the Claude CLI prefix.
	req := buildResponsesRequest("mcp__bifrost__echo")
	ctx := contextWithUserAgent(schemas.ClaudeCLI.String())

	result := tm.ParseAndAddToolsToRequest(ctx, req)

	names := toolNamesFromResponsesRequest(result)
	if countOccurrences(names, "echo") != 0 {
		t.Errorf("Responses/ClaudeCLI: bare 'echo' should not be injected, got %d (names: %v)",
			countOccurrences(names, "echo"), names)
	}
	if countOccurrences(names, "mcp__bifrost__echo") != 1 {
		t.Errorf("Responses/ClaudeCLI: prefixed name should remain exactly once, got %d",
			countOccurrences(names, "mcp__bifrost__echo"))
	}
}

func TestParseAndAddToolsToRequest_ResponsesAPI_NoUserAgent_NoDuplicate(t *testing.T) {
	t.Parallel()

	cm := &mockToolClientManager{
		tools: []schemas.ChatTool{
			makeTool("echo"),
			makeTool("search"),
		},
	}
	tm := newToolsManagerForTest(cm)

	req := buildResponsesRequest("echo")
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)

	result := tm.ParseAndAddToolsToRequest(ctx, req)

	names := toolNamesFromResponsesRequest(result)
	if countOccurrences(names, "echo") != 1 {
		t.Errorf("Responses/no-agent: expected exactly 1 'echo', got %d (names: %v)",
			countOccurrences(names, "echo"), names)
	}
	// search is new — must be injected.
	if countOccurrences(names, "search") != 1 {
		t.Errorf("Responses/no-agent: 'search' should be injected once, got %d", countOccurrences(names, "search"))
	}
}

func TestParseAndAddToolsToRequest_ResponsesAPI_CodexCLI_MCPHyphenUnderscoreVariantsDeduped(t *testing.T) {
	t.Parallel()

	cm := &mockToolClientManager{
		tools: []schemas.ChatTool{
			makeTool("foo-bar"),
			makeTool("foo_bar"),
		},
	}
	tm := newToolsManagerForTest(cm)

	req := buildResponsesRequest()
	ctx := contextWithUserAgent(schemas.CodexCLI.String())

	result := tm.ParseAndAddToolsToRequest(ctx, req)

	names := toolNamesFromResponsesRequest(result)
	if len(names) != 1 {
		t.Fatalf("Responses/Codex: hyphen/underscore variants should yield one injected tool, got %d: %v", len(names), names)
	}
	if countOccurrences(names, "foo-bar")+countOccurrences(names, "foo_bar") != 1 {
		t.Errorf("Responses/Codex: expected exactly one of foo-bar or foo_bar, got %v", names)
	}
}

// ---------------------------------------------------------------------------
// OpenCode — pattern: {server_name}_{tool_name} (no mcp_ prefix, hyphens preserved)
// ---------------------------------------------------------------------------

// TestBuildIntegrationDuplicateCheckMap_OpenCode_ClientPrefixedTools verifies the
// dual-gateway scenario for OpenCode: format is {server}_{tool_name} (no mcp_ prefix,
// single underscore separator, hyphens in the Bifrost tool name are preserved).
// Strip up to and including the first "_" to recover the Bifrost tool name.
func TestBuildIntegrationDuplicateCheckMap_OpenCode_ClientPrefixedTools(t *testing.T) {
	t.Parallel()

	tools := []schemas.ChatTool{
		makeTool("bifrost_testing_exa-web_fetch_exa"),
		makeTool("bifrost_testing_exa-web_search_exa"),
		makeTool("bifrost_testing_websets-cancel_enrichment"),
		makeTool("bifrost_ctx7-query-docs"),
		makeTool("bifrost_filesystem-create_directory"),
	}

	m := buildIntegrationDuplicateCheckMap(tools, schemas.OpenCode.String(), defaultLogger)

	for _, want := range []string{
		"testing_exa-web_fetch_exa",
		"testing_exa-web_search_exa",
		"testing_websets-cancel_enrichment",
		"ctx7-query-docs",
		"filesystem-create_directory",
	} {
		if !m[want] {
			t.Errorf("OpenCode: expected %q in duplicate map", want)
		}
	}
	// Original prefixed names must also be retained.
	for _, want := range []string{
		"bifrost_testing_exa-web_fetch_exa",
		"bifrost_ctx7-query-docs",
	} {
		if !m[want] {
			t.Errorf("OpenCode: expected original prefixed name %q in duplicate map", want)
		}
	}
}

// TestParseAndAddToolsToRequest_OpenCode_NoDuplicate verifies end-to-end deduplication
// for OpenCode in the dual-gateway scenario.
func TestParseAndAddToolsToRequest_OpenCode_NoDuplicate(t *testing.T) {
	t.Parallel()

	cm := &mockToolClientManager{
		tools: []schemas.ChatTool{
			makeTool("testing_exa-web_fetch_exa"),
			makeTool("ctx7-query-docs"),
			makeTool("filesystem-create_directory"),
		},
	}
	tm := newToolsManagerForTest(cm)

	// OpenCode sends tools prefixed as {server}_{tool_name}.
	req := buildChatRequest(
		"bifrost_testing_exa-web_fetch_exa",
		"bifrost_ctx7-query-docs",
		"bifrost_filesystem-create_directory",
	)
	ctx := contextWithUserAgent(schemas.OpenCode.String())

	result := tm.ParseAndAddToolsToRequest(ctx, req)

	names := toolNamesFromChatRequest(result)

	// Bare Bifrost names must NOT be injected again — they're covered by prefixed entries.
	for _, bare := range []string{"testing_exa-web_fetch_exa", "ctx7-query-docs", "filesystem-create_directory"} {
		if countOccurrences(names, bare) != 0 {
			t.Errorf("OpenCode: bare %q should not be injected, got %d (names: %v)",
				bare, countOccurrences(names, bare), names)
		}
	}
	// Prefixed originals must remain exactly once.
	for _, prefixed := range []string{
		"bifrost_testing_exa-web_fetch_exa",
		"bifrost_ctx7-query-docs",
		"bifrost_filesystem-create_directory",
	} {
		if countOccurrences(names, prefixed) != 1 {
			t.Errorf("OpenCode: expected exactly 1 %q, got %d (names: %v)",
				prefixed, countOccurrences(names, prefixed), names)
		}
	}
}

// =============================================================================
// Reactive repair: a tool call that fails because the connection underneath it
// is dead. Uses sessionInvalidator / buildExpiringSessionMCPServer from
// connectionchecker_reconnect_test.go (same package).
// =============================================================================

// buildRecoveryMCPServer is buildExpiringSessionMCPServer with control over
// the echo tool's annotations, which decide whether the auto-retry is allowed
// at all: attemptCallFailureRecovery fails closed on missing hints (MCP's own
// defaults are destructiveHint=true, idempotentHint=false), so an unannotated
// tool is never retried automatically. Reuses sessionInvalidator from
// connectionchecker_reconnect_test.go.
func buildRecoveryMCPServer(t *testing.T, toolOpts ...mcpgo.ToolOption) (*httptest.Server, *sessionInvalidator) {
	t.Helper()

	s := server.NewMCPServer("test-toolcall-recovery", "1.0.0", server.WithToolCapabilities(true))
	s.AddTool(
		mcpgo.NewTool("echo", append([]mcpgo.ToolOption{mcpgo.WithDescription("Echo tool")}, toolOpts...)...),
		func(_ context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			return mcpgo.NewToolResultText("ok"), nil
		},
	)

	streamable := server.NewStreamableHTTPServer(s)
	invalidator := &sessionInvalidator{dead: map[string]bool{}}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if invalidator.reject(w, r) {
			return
		}
		streamable.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts, invalidator
}

func newRecoveryClientConfig(id, name, url string) *schemas.MCPClientConfig {
	return &schemas.MCPClientConfig{
		ID:                     id,
		Name:                   name,
		AuthType:               schemas.MCPAuthTypeHeaders,
		ConnectionType:         schemas.MCPConnectionTypeHTTP,
		ConnectionString:       schemas.NewSecretVar(url),
		NeedsSessionStickiness: schemas.Ptr(true),
		ToolsToExecute:         []string{"*"},
	}
}

// executeEchoTool runs the client's echo tool through the real execution path
// (ExecuteChatTool -> prepareToolExecution -> executeToolInternal), unlike
// callEchoTool in makebeforebreak_test.go which talks to the connection
// directly. The recovery under test lives in executeToolInternal, so it is
// only reachable this way.
func executeEchoTool(m *MCPManager, toolName string) (*schemas.ChatMessage, *schemas.BifrostError) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	bfCtx := schemas.NewBifrostContext(ctx, schemas.NoDeadline)
	return m.ExecuteChatTool(bfCtx, &schemas.ChatAssistantMessageToolCall{
		Function: schemas.ChatAssistantMessageToolCallFunction{
			Name:      &toolName,
			Arguments: "{}",
		},
	})
}

// TestExecuteTool_DeadSession_SalvagesTheCallForARetryableTool pins the
// reactive half of MCP connection repair. A tool call is the strongest
// evidence available that a connection is dead: a real request, over the real
// connection, from a real caller. Bifrost already acted on one class of call
// failure this way (a clean upstream auth rejection), reconnecting and
// retrying the same call once so the caller never saw the failure.
//
// A session the upstream had abandoned got none of that. It is not an auth
// rejection, so the recovery never ran, and it matches no transient substring,
// so ToolCallRetryConfig did not even retry it. The call failed outright and
// every later call failed the same way until the periodic checker happened to
// reconnect, up to a full check interval away.
func TestExecuteTool_DeadSession_SalvagesTheCallForARetryableTool(t *testing.T) {
	// Read-only: safe to auto-retry, so the caller's own request is salvaged.
	ts, upstream := buildRecoveryMCPServer(t, mcpgo.WithReadOnlyHintAnnotation(true))

	m := NewMCPManager(context.Background(), schemas.MCPConfig{}, nil, &MockLogger{}, nil)
	// connectToMCPClient starts a connection checker per client; without this
	// its goroutine outlives the test and can probe an httptest server that
	// t.Cleanup has already closed.
	t.Cleanup(func() { _ = m.Cleanup() })
	config := newRecoveryClientConfig("client-dead-session-salvage", "salvage-client", ts.URL)
	require.NoError(t, m.connectToMCPClient(context.Background(), config))
	toolName := config.Name + "-echo"

	_, bErr := executeEchoTool(m, toolName)
	require.Nil(t, bErr, "sanity: the tool works before the session dies")

	before, ok := snapshotClientState(m, config.ID)
	require.True(t, ok)

	// The upstream expires the session behind the live connection. A fresh
	// dial would still succeed, so this is recoverable with no human.
	upstream.expireCurrentSession()

	msg, bErr := executeEchoTool(m, toolName)
	require.Nil(t, bErr, "a dead session must be reconnected and the call retried, not surfaced to the caller")
	require.NotNil(t, msg)

	after, ok := snapshotClientState(m, config.ID)
	require.True(t, ok)
	assert.Greater(t, after.ConnGeneration, before.ConnGeneration, "recovery means a genuinely fresh connection, not a lucky retry")
	assert.NotSame(t, before.Conn, after.Conn)
}

// TestExecuteTool_DeadSession_HealsEvenWhenTheRetryIsSuppressed covers the
// other side of the safety gate. A tool with no annotations is treated as
// destructive and non-idempotent (MCP's own defaults, and
// attemptCallFailureRecovery fails closed on missing hints), so replaying the
// call could cause a real-world side effect twice and the auto-retry is
// suppressed. The reconnect must still run: the caller eats this one failure,
// but the connection is repaired behind it so the next call succeeds instead
// of waiting on the periodic checker.
func TestExecuteTool_DeadSession_HealsEvenWhenTheRetryIsSuppressed(t *testing.T) {
	ts, upstream := buildRecoveryMCPServer(t) // unannotated: assumed destructive

	m := NewMCPManager(context.Background(), schemas.MCPConfig{}, nil, &MockLogger{}, nil)
	// connectToMCPClient starts a connection checker per client; without this
	// its goroutine outlives the test and can probe an httptest server that
	// t.Cleanup has already closed.
	t.Cleanup(func() { _ = m.Cleanup() })
	config := newRecoveryClientConfig("client-dead-session-heal", "heal-client", ts.URL)
	require.NoError(t, m.connectToMCPClient(context.Background(), config))
	toolName := config.Name + "-echo"

	_, bErr := executeEchoTool(m, toolName)
	require.Nil(t, bErr, "sanity: the tool works before the session dies")

	before, ok := snapshotClientState(m, config.ID)
	require.True(t, ok)

	upstream.expireCurrentSession()

	_, bErr = executeEchoTool(m, toolName)
	require.NotNil(t, bErr, "a destructive, non-idempotent tool must not be replayed automatically")

	require.Eventually(t, func() bool {
		after, exists := snapshotClientState(m, config.ID)
		return exists && after.ConnGeneration > before.ConnGeneration
	}, 15*time.Second, 100*time.Millisecond, "the reconnect must run regardless of whether the retry was allowed")

	_, bErr = executeEchoTool(m, toolName)
	require.Nil(t, bErr, "the next call must land on the healed connection")
}

// buildSlowReconnectMCPServer is buildRecoveryMCPServer with a knob that stalls
// the `initialize` of any NEW connection. A sessionless request is exactly the
// initialize of a fresh dial, so this lets a test make the recovery reconnect
// outlast the tool execution budget while the original session stays expired.
func buildSlowReconnectMCPServer(t *testing.T, toolOpts ...mcpgo.ToolOption) (*httptest.Server, *sessionInvalidator, *atomic.Int64) {
	t.Helper()

	s := server.NewMCPServer("test-slow-reconnect", "1.0.0", server.WithToolCapabilities(true))
	s.AddTool(
		mcpgo.NewTool("echo", append([]mcpgo.ToolOption{mcpgo.WithDescription("Echo tool")}, toolOpts...)...),
		func(_ context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			return mcpgo.NewToolResultText("ok"), nil
		},
	)

	streamable := server.NewStreamableHTTPServer(s)
	invalidator := &sessionInvalidator{dead: map[string]bool{}}
	var initDelay atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if invalidator.reject(w, r) {
			return
		}
		if d := initDelay.Load(); d > 0 && r.Header.Get("Mcp-Session-Id") == "" {
			time.Sleep(time.Duration(d))
		}
		streamable.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts, invalidator, &initDelay
}

// TestExecuteTool_DeadSession_RecoveryHonoursTheToolExecutionTimeout pins the
// budget the recovery runs under. `tool_execution_timeout` is what an operator
// sets to bound a tool call's worst-case latency, and the first attempt already
// runs inside it. Recovery must spend what is left of that same budget, not
// start a fresh one: waiting the full reconnect budget and then granting the
// retry another complete timeout makes a recovered call take up to twice the
// configured bound plus the wait, and in agent mode that overshoot is paid once
// per step.
//
// The upstream here stalls every new connection's initialize for longer than
// the tool budget, so a recovery bounded by the budget gives up inside it while
// an unbounded one waits out the reconnect and then retries.
func TestExecuteTool_DeadSession_RecoveryHonoursTheToolExecutionTimeout(t *testing.T) {
	const toolBudget = 2 * time.Second

	ts, upstream, initDelay := buildSlowReconnectMCPServer(t, mcpgo.WithReadOnlyHintAnnotation(true))

	m := NewMCPManager(context.Background(), schemas.MCPConfig{}, nil, &MockLogger{}, nil)
	// connectToMCPClient starts a connection checker per client; without this
	// its goroutine outlives the test and can probe an httptest server that
	// t.Cleanup has already closed.
	t.Cleanup(func() { _ = m.Cleanup() })
	config := newRecoveryClientConfig("client-dead-session-deadline", "deadline-client", ts.URL)
	config.ToolExecutionTimeout = toolBudget
	require.NoError(t, m.connectToMCPClient(context.Background(), config))
	toolName := config.Name + "-echo"

	_, bErr := executeEchoTool(m, toolName)
	require.Nil(t, bErr, "sanity: the tool works before the session dies")

	upstream.expireCurrentSession()
	initDelay.Store(int64(3 * toolBudget)) // the reconnect cannot finish inside the budget

	start := time.Now()
	_, bErr = executeEchoTool(m, toolName)
	elapsed := time.Since(start)

	// Without this the elapsed-time assertion below passes for a regression that
	// gives up immediately with the wrong sentinel, and the trailing healed-call
	// assertion still passes on the background reconnect. The budget being spent
	// is what makes this a timeout rather than a tool error, so classify it.
	require.NotNil(t, bErr, "the caller must still see the failure the recovery could not salvage")
	assert.Equal(t, "timeout", mcpErrorType(bErr),
		"a call whose recovery consumed the tool budget is a timeout, not a generic tool error")

	assert.Less(t, elapsed, 2*toolBudget,
		"a recovered call must stay within tool_execution_timeout, not wait out the reconnect budget and then start a fresh one")

	// The budget bounds the caller's wait, it does not abandon the repair: the
	// reconnect keeps running and the next call lands on the healed connection.
	initDelay.Store(0)
	require.Eventually(t, func() bool {
		_, err := executeEchoTool(m, toolName)
		return err == nil
	}, 30*time.Second, 250*time.Millisecond, "the background reconnect must still heal the client")
}

// TestIsDeadSessionError_TypedSentinelAndTextFallback pins both halves of the
// dead-session check and, more importantly, why both have to exist and where
// the text half is allowed to look.
//
// The typed half matches mcp-go's ErrSessionTerminated through its own wrapping
// without depending on how that sentinel is worded. The text half is the only
// signal a server gives when it answers a dead session with something other
// than the spec's 404: a 422 carrying the server's own prose, which mcp-go
// formats straight into the message with no typed error anywhere.
//
// The text half must only read transport-framed errors. client.sendRequest
// returns exactly two shapes: a transport failure wrapped in *transport.Error,
// or the tool's own JSON-RPC error, flattened to its message with no wrapper
// (mcp.JSONRPCErrorDetails.AsError). A tool that legitimately answers "invalid
// session" or "session not found" about its OWN domain arrives in the second
// shape, and must not be mistaken for a dead MCP session: the shared-path
// recovery would reconnect a healthy client and, for a retry-safe tool,
// replay the call.
func TestIsDeadSessionError_TypedSentinelAndTextFallback(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			// Exactly the shape a 404 arrives in: the sentinel, wrapped by
			// SendRequest with %w and again by client.sendRequest in a
			// *transport.Error that implements Unwrap.
			name: "typed sentinel survives mcp-go's wrapping",
			err:  transport.NewError(fmt.Errorf("failed to send request: %w", transport.ErrSessionTerminated)),
			want: true,
		},
		{
			// A non-compliant server: no typed error exists anywhere in this
			// chain, so the text is the only thing to go on. The shape is
			// exactly what client.sendRequest produces for a non-404 status:
			// SendRequest's formatted prose inside *transport.Error.
			name: "non-404 server is caught by text inside the transport frame",
			err:  transport.NewError(errors.New("request failed with status 422: Unexpected message, expect initialize request")),
			want: true,
		},
		{
			// Same 422, but ExecuteWithRetry and callers may wrap the
			// transport error further; the frame must still be found.
			name: "transport frame is found through outer wrapping",
			err:  fmt.Errorf("attempt 2: %w", transport.NewError(errors.New("request failed with status 422: Unexpected message, expect initialize request"))),
			want: true,
		},
		{
			name: "an ordinary transport failure is not a dead session",
			err:  transport.NewError(errors.New("failed to send request: connection reset by peer")),
			want: false,
		},
		{
			// Finding the frame is not a licence to read the whole chain.
			// The needle here is in a wrapper's prose, outside the frame; the
			// transport itself reported nothing session-shaped. Only the
			// frame's own text may match.
			name: "a needle outside the transport frame does not count",
			err:  fmt.Errorf("session not found in retry bookkeeping: %w", transport.NewError(errors.New("failed to send request: connection reset by peer"))),
			want: false,
		},
		{
			// Same rule through errors.Join, which errors.As also walks: a
			// tool's own error joined to an unrelated transport failure must
			// not have the tool's text matched on the transport's behalf.
			name: "a tool error joined to a benign transport error does not count",
			err: errors.Join(
				fmt.Errorf("%w: invalid session key for user", mcpgo.ErrInternalError),
				transport.NewError(errors.New("failed to send request: connection reset by peer")),
			),
			want: false,
		},
		{
			// Near miss: the tool's own JSON-RPC error, flattened by
			// AsError into "internal error: <tool's message>" with no
			// transport frame. The MCP session is fine; the tool is talking
			// about a session of its own.
			name: "a tool's own 'invalid session' error is not a dead MCP session",
			err:  fmt.Errorf("%w: invalid session key for user", mcpgo.ErrInternalError),
			want: false,
		},
		{
			name: "a tool's own 'session not found' error is not a dead MCP session",
			err:  errors.New("session not found in cache: sess_9f2a"),
			want: false,
		},
		{
			name: "a tool's own 'unknown session' error is not a dead MCP session",
			err:  fmt.Errorf("%w: unknown session id", mcpgo.ErrInvalidParams),
			want: false,
		},
		{
			// The bare, unframed text is not enough on its own even when it
			// is word-for-word the transport's prose: only the frame says
			// the transport, not the tool, produced it.
			name: "dead-session prose without a transport frame is not trusted",
			err:  errors.New("request failed with status 422: Unexpected message, expect initialize request"),
			want: false,
		},
		{
			name: "nil is not a dead session",
			err:  nil,
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, isDeadSessionError(tc.err))
		})
	}
}

// TestExecuteTool_ToolErrorMentioningSession_DoesNotTriggerRecovery is the
// end-to-end form of the near-miss cases above. A tool whose own error talks
// about a session ("session not found") is served by a healthy MCP session
// over a healthy connection. The call must fail as an ordinary tool error:
// no reconnect, no replay, same connection generation afterwards.
//
// Read-only annotation is deliberate. It is the case where a misclassification
// would do the most damage: the recovery gate would allow a synchronous retry,
// so a false positive here means a replayed call on top of a needless
// reconnect.
func TestExecuteTool_ToolErrorMentioningSession_DoesNotTriggerRecovery(t *testing.T) {
	s := server.NewMCPServer("test-tool-error-near-miss", "1.0.0", server.WithToolCapabilities(true))
	var calls atomic.Int32
	s.AddTool(
		mcpgo.NewTool("lookup", mcpgo.WithDescription("Session lookup tool"), mcpgo.WithReadOnlyHintAnnotation(true)),
		func(_ context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			calls.Add(1)
			// The tool's own domain error. mcp-go's server wraps it as a
			// JSON-RPC INTERNAL_ERROR carrying this text verbatim.
			return nil, errors.New("session not found: sess_9f2a")
		},
	)
	ts := httptest.NewServer(server.NewStreamableHTTPServer(s))
	t.Cleanup(ts.Close)

	m := NewMCPManager(context.Background(), schemas.MCPConfig{}, nil, &MockLogger{}, nil)
	t.Cleanup(func() { _ = m.Cleanup() })
	config := newRecoveryClientConfig("client-tool-error-near-miss", "near-miss-client", ts.URL)
	require.NoError(t, m.connectToMCPClient(context.Background(), config))
	toolName := config.Name + "-lookup"

	before, ok := snapshotClientState(m, config.ID)
	require.True(t, ok)

	_, bErr := executeEchoTool(m, toolName)
	require.NotNil(t, bErr, "the tool's own error must reach the caller")
	assert.Contains(t, bErr.Error.Message, "session not found", "the tool's own message must be preserved")

	after, ok := snapshotClientState(m, config.ID)
	require.True(t, ok)
	assert.Equal(t, before.ConnGeneration, after.ConnGeneration, "a tool error is not a dead session: the connection must not be swapped")
	assert.Same(t, before.Conn, after.Conn)
	assert.Equal(t, int32(1), calls.Load(), "a tool error is not a dead session: the call must not be replayed")
}
