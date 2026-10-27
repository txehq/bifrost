package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"testing"
)

// fixtureStruct type-checks a tiny package with no imports and returns the
// named struct type to walk, so the checker is exercised without loading the
// bifrost module graph.
func fixtureStruct(t *testing.T, src, typeName string) types.Type {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "fixture.go", src, 0)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	pkg, err := (&types.Config{}).Check("example.com/fixture", fset, []*ast.File{file}, nil)
	if err != nil {
		t.Fatalf("type-check fixture: %v", err)
	}
	obj := pkg.Scope().Lookup(typeName)
	if obj == nil {
		t.Fatalf("fixture type %s not found", typeName)
	}
	return obj.Type()
}

func newTestChecker(schema map[string]any) *checker {
	return &checker{schema: schema, enumConsts: map[string][]string{}, visited: map[string]bool{}}
}

func missingInSchema(findings []Finding) map[string]Finding {
	out := map[string]Finding{}
	for _, f := range findings {
		if f.Category == "missing-in-schema" {
			out[f.Detail] = f
		}
	}
	return out
}

// TestWalkTypeExemptsInjectedToolsOnGovernanceProvidersOnly pins the
// ignoreGoFields entry for governance.providers[].injected_tools: the field is
// a gorm:"-" member of the TableProvider rows that governance.providers decodes
// into, and the config loader only ever reads budget_id and rate_limit_id from
// those rows (injected_tools is configured under providers.<name>), so the
// schema deliberately does not advertise it there. The exemption must stay
// narrow: an unrelated missing field at the same path is still an error, and
// the same struct walked at another path still reports injected_tools.
func TestWalkTypeExemptsInjectedToolsOnGovernanceProvidersOnly(t *testing.T) {
	const src = `package fixture

type InjectedTools struct {
	WebSearch string ` + "`json:\"web_search\"`" + `
}

type Provider struct {
	Name          string         ` + "`json:\"name\"`" + `
	BudgetID      *string        ` + "`json:\"budget_id,omitempty\"`" + `
	InjectedTools *InjectedTools ` + "`gorm:\"-\" json:\"injected_tools,omitempty\"`" + `
	Surprise      string         ` + "`json:\"surprise\"`" + `
}
`
	provider := fixtureStruct(t, src, "Provider")
	providerItem := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name":      map[string]any{"type": "string"},
			"budget_id": map[string]any{"type": "string"},
		},
	}
	schema := map[string]any{
		"properties": map[string]any{
			"governance": map[string]any{
				"properties": map[string]any{
					"providers": map[string]any{"type": "array", "items": providerItem},
				},
			},
			"providers": map[string]any{"type": "object", "additionalProperties": providerItem},
		},
	}

	t.Run("governance.providers exempts injected_tools and nothing else", func(t *testing.T) {
		c := newTestChecker(schema)
		c.walkType(provider, "/properties/governance/properties/providers/items", "fixture.Provider")
		missing := missingInSchema(c.findings)
		if _, reported := missing["injected_tools"]; reported {
			t.Errorf("injected_tools must be exempt at governance.providers[]: %+v", missing["injected_tools"])
		}
		surprise, reported := missing["surprise"]
		if !reported {
			t.Fatalf("an unrelated field missing from the schema must still be an error; findings: %+v", c.findings)
		}
		if surprise.Severity != "ERROR" || surprise.Path != "/properties/governance/properties/providers/items/properties/surprise" {
			t.Errorf("unexpected finding for surprise: %+v", surprise)
		}
		if len(missing) != 1 {
			t.Errorf("expected exactly one missing-in-schema finding, got %+v", missing)
		}
	})

	t.Run("the exemption is scoped to that path", func(t *testing.T) {
		c := newTestChecker(schema)
		c.walkType(provider, "/properties/providers/additionalProperties", "fixture.Provider")
		missing := missingInSchema(c.findings)
		if _, reported := missing["injected_tools"]; !reported {
			t.Errorf("injected_tools missing from the schema elsewhere must still be reported; findings: %+v", c.findings)
		}
		if _, reported := missing["surprise"]; !reported {
			t.Errorf("surprise must be reported at the other path too; findings: %+v", c.findings)
		}
	})
}

// TestWalkTypeExemptsOpenAPIConfigServerComputedMetadata pins the ignoreGoFields
// entries for an OpenAPI MCP client's openapi_config metadata (spec_size,
// spec_hash, spec_title, openapi_version, operation_count): the server computes
// them from the document on every load and save (openapimcp.ApplyMetadata), so
// the schema does not advertise them as config.json input. Without the entries
// the release gate failed on main 14907f01c (TXE-5318). The exemption is scoped:
// another missing field at the same path is still an error, and the same struct
// walked at another path still reports the metadata fields.
func TestWalkTypeExemptsOpenAPIConfigServerComputedMetadata(t *testing.T) {
	const src = `package fixture

type OpenAPIConfig struct {
	Spec           string ` + "`json:\"spec,omitempty\"`" + `
	SpecSize       int    ` + "`json:\"spec_size,omitempty\"`" + `
	SpecHash       string ` + "`json:\"spec_hash,omitempty\"`" + `
	SpecTitle      string ` + "`json:\"spec_title,omitempty\"`" + `
	OpenAPIVersion string ` + "`json:\"openapi_version,omitempty\"`" + `
	OperationCount int    ` + "`json:\"operation_count,omitempty\"`" + `
	Surprise       string ` + "`json:\"surprise\"`" + `
}
`
	cfg := fixtureStruct(t, src, "OpenAPIConfig")
	node := map[string]any{"type": "object", "properties": map[string]any{"spec": map[string]any{"type": "string"}}}
	schema := map[string]any{
		"properties": map[string]any{
			"mcp": map[string]any{"properties": map[string]any{"client_configs": map[string]any{"type": "array", "items": map[string]any{
				"type": "object", "properties": map[string]any{"openapi_config": node},
			}}}},
			"elsewhere": node,
		},
	}
	metadata := []string{"spec_size", "spec_hash", "spec_title", "openapi_version", "operation_count"}

	c := newTestChecker(schema)
	c.walkType(cfg, "/properties/mcp/properties/client_configs/items/properties/openapi_config", "fixture.OpenAPIConfig")
	missing := missingInSchema(c.findings)
	for _, name := range metadata {
		if _, reported := missing[name]; reported {
			t.Errorf("%s must be exempt at mcp.client_configs[].openapi_config: %+v", name, missing[name])
		}
	}
	if _, reported := missing["surprise"]; !reported || len(missing) != 1 {
		t.Errorf("only the unrelated field must be reported at openapi_config, got %+v", missing)
	}

	c = newTestChecker(schema)
	c.walkType(cfg, "/properties/elsewhere", "fixture.OpenAPIConfig")
	missing = missingInSchema(c.findings)
	for _, name := range metadata {
		if _, reported := missing[name]; !reported {
			t.Errorf("the exemption is scoped to openapi_config; %s must be reported elsewhere, got %+v", name, missing)
		}
	}
}
