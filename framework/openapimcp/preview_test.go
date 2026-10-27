package openapimcp

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveBaseURL(t *testing.T) {
	doc := &Document{Servers: []string{"/relative", "https://api.example.com/v1/"}, Self: "https://self.example.com/spec.yaml"}

	base, explicit, err := ResolveBaseURL(doc, str(" https://override.example.com/ "), "")
	require.NoError(t, err)
	assert.Equal(t, "https://override.example.com", base)
	assert.True(t, explicit)

	_, _, err = ResolveBaseURL(doc, str("ftp://x"), "")
	require.ErrorContains(t, err, "absolute http(s) URL")

	base, explicit, err = ResolveBaseURL(&Document{Servers: []string{"https://api.example.com/v1/"}}, nil, "")
	require.NoError(t, err)
	assert.Equal(t, "https://api.example.com/v1", base)
	assert.False(t, explicit)

	base, _, err = ResolveBaseURL(doc, str(""), "")
	require.NoError(t, err)
	assert.Equal(t, "https://self.example.com/relative", base, "a relative server resolves against $self before the absolute one is reached")

	base, _, err = ResolveBaseURL(&Document{Servers: []string{"/v2"}}, nil, "https://host.example.com/docs/openapi.json")
	require.NoError(t, err)
	assert.Equal(t, "https://host.example.com/v2", base)

	_, _, err = ResolveBaseURL(&Document{Servers: []string{"/v2"}}, nil, "")
	require.ErrorContains(t, err, "set openapi_config.base_url")
	_, _, err = ResolveBaseURL(nil, nil, "")
	require.Error(t, err)
}

func TestPreview(t *testing.T) {
	data := fixture(t, "petstore-3.0.yaml")
	result, syn, err := Preview(data, ParseOptions{}, SynthesizeOptions{ClientName: "petstore"}, nil)
	require.NoError(t, err)
	require.NotNil(t, syn)

	assert.Equal(t, "Petstore", result.Title)
	assert.Equal(t, "3.0.3", result.OpenAPIVersion)
	assert.Equal(t, "https://petstore.example.com/v1", result.BaseURL)
	assert.Equal(t, 5, result.ToolCount)
	assert.Len(t, result.Tools, 5)
	assert.Len(t, result.Unsupported, 2)
	assert.Equal(t, len(data), result.SpecSize)
	assert.Equal(t, SpecHash(data), result.SpecHash)
	assert.Len(t, result.SecuritySchemes, 5)
	assert.Equal(t, "ApiKeyAuth", result.SecuritySchemes[0].Name, "schemes are sorted by name")

	raw, err := json.Marshal(result)
	require.NoError(t, err)
	var wire map[string]any
	require.NoError(t, json.Unmarshal(raw, &wire))
	for _, key := range []string{"title", "version", "openapi_version", "servers", "base_url", "security_schemes", "tools", "unsupported", "warnings", "tool_count", "spec_size", "spec_hash"} {
		assert.Contains(t, wire, key)
	}
	firstTool := asMap(asSlice(wire["tools"])[0])
	for _, key := range []string{"name", "method", "path", "description", "input_schema", "annotations"} {
		assert.Contains(t, firstTool, key)
	}

	withOverride, _, err := Preview(data, ParseOptions{}, SynthesizeOptions{}, str("https://other.example.com"))
	require.NoError(t, err)
	assert.Equal(t, "https://other.example.com", withOverride.BaseURL)

	noServers := []byte(`{"openapi":"3.0.0","info":{"title":"x","version":"1"},"paths":{"/a":{"get":{"responses":{"200":{"description":"ok"}}}}}}`)
	result, _, err = Preview(noServers, ParseOptions{}, SynthesizeOptions{}, nil)
	require.NoError(t, err)
	assert.Empty(t, result.BaseURL)
	assert.Contains(t, result.Warnings[len(result.Warnings)-1], "base_url")

	_, _, err = Preview([]byte("nope"), ParseOptions{}, SynthesizeOptions{}, nil)
	require.Error(t, err)
}

func TestApplyMetadata(t *testing.T) {
	data := fixture(t, "petstore-3.2.yaml")
	_, syn, err := Preview(data, ParseOptions{}, SynthesizeOptions{}, nil)
	require.NoError(t, err)
	cfg := &schemas.MCPOpenAPIConfig{Spec: string(data)}
	ApplyMetadata(cfg, data, syn.Document, syn)
	assert.Equal(t, len(data), cfg.SpecSize)
	assert.Equal(t, SpecHash(data), cfg.SpecHash)
	assert.Equal(t, "Petstore 3.2", cfg.SpecTitle)
	assert.Equal(t, "3.2.0", cfg.OpenAPIVersion)
	assert.Equal(t, 4, cfg.OperationCount)
	ApplyMetadata(nil, data, nil, nil)
}

// TestResolveServingBaseURL_InvalidOverrideIsAnError: operation servers stand in
// for a missing document server, never for an explicit override that is invalid.
func TestResolveServingBaseURL_InvalidOverrideIsAnError(t *testing.T) {
	spec := `{"openapi":"3.0.0","info":{"title":"t","version":"1"},"paths":{"/a":{"servers":[{"url":"https://a.example"}],"get":{"operationId":"getA","responses":{"200":{"description":"ok"}}}}}}`
	doc, err := Parse([]byte(spec), ParseOptions{})
	require.NoError(t, err)
	syn, err := Synthesize(doc, SynthesizeOptions{ClientName: "t"})
	require.NoError(t, err)
	_, _, err = ResolveServingBaseURL(doc, syn, str("ftp://example.com"), "")
	require.ErrorContains(t, err, "base_url")
	// Operation servers never stand in for a missing base URL (TXE-5296): they
	// are used only at its origin, so such a spec needs an explicit one.
	_, _, err = ResolveServingBaseURL(doc, syn, nil, "")
	require.ErrorContains(t, err, "base_url")
	base, explicit, err := ResolveServingBaseURL(doc, syn, str("https://a.example"), "")
	require.NoError(t, err)
	assert.Equal(t, "https://a.example", base)
	assert.True(t, explicit)
}

// TestPreview_OperationServersIgnoresOverride: operation_servers says whether
// every operation declares its own server, which a base URL override does not
// change; the form uses it to know the override is optional.
func TestPreview_OperationServersIgnoresOverride(t *testing.T) {
	spec := []byte(`{"openapi":"3.0.3","info":{"title":"ops","version":"1"},"paths":{"/a":{"servers":[{"url":"https://a.example.com"}],"get":{"operationId":"getA","responses":{"200":{"description":"ok"}}}}}}`)
	override := "https://gw.example.com"
	result, _, err := Preview(spec, ParseOptions{}, SynthesizeOptions{ClientName: "ops"}, &override)
	require.NoError(t, err)
	assert.Equal(t, "https://gw.example.com", result.BaseURL)
	assert.False(t, result.OperationServers, "operation servers never make a base URL optional (TXE-5296)")

	result, _, err = Preview(spec, ParseOptions{}, SynthesizeOptions{ClientName: "ops"}, nil)
	require.NoError(t, err)
	assert.Empty(t, result.BaseURL)
	assert.False(t, result.OperationServers)
	assert.Contains(t, strings.Join(result.Warnings, "\n"), "base_url", "the preview asks for a base URL")
}

// TestPreview_ReusedPathItemTextIsCappedPerTool: a path item reused by many
// paths hands its summary and operationId to every tool. The preview response
// carries each per tool, so both are capped like the description, and the
// serialized preview stays bounded rather than growing with the reuse.
func TestPreview_ReusedPathItemTextIsCappedPerTool(t *testing.T) {
	var paths strings.Builder
	for i := 0; i < 100; i++ {
		if i > 0 {
			paths.WriteByte(',')
		}
		paths.WriteString(`"/p` + strconv.Itoa(i) + `":{"$ref":"#/components/pathItems/Shared"}`)
	}
	spec := []byte(`{"openapi":"3.1.0","info":{"title":"t","version":"1"},"servers":[{"url":"https://a"}],"paths":{` + paths.String() + `},
"components":{"pathItems":{"Shared":{"get":{"operationId":"` + strings.Repeat("o", 256<<10) + `","summary":"` + strings.Repeat("s", 256<<10) + `","responses":{"200":{"description":"ok"}}}}}}}`)
	result, _, err := Preview(spec, ParseOptions{}, SynthesizeOptions{ClientName: "t"}, nil)
	require.NoError(t, err)
	require.Len(t, result.Tools, 100)
	for _, tool := range result.Tools {
		assert.LessOrEqual(t, len(tool.Summary), maxDescriptionLen+8)
		assert.LessOrEqual(t, len(tool.OperationID), maxDescriptionLen+8)
	}
	encoded, err := json.Marshal(result)
	require.NoError(t, err)
	assert.Less(t, len(encoded), 2<<20, "100 tools must not each carry the 256 KiB summary and operationId")
}
