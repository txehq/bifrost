package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/capsohq/bifrost/core/schemas"
)

// codexUnionToolSchema returns a reduced version of Codex's automation_update
// schema: its root alternatives are references, while nullable fields retain a
// nested anyOf that Anthropic accepts.
func codexUnionToolSchema(t *testing.T) *schemas.ToolFunctionParameters {
	t.Helper()
	var schema schemas.ToolFunctionParameters
	raw := []byte(`{
		"type":"object",
		"properties":{},
		"$defs":{
			"view":{"type":"object","properties":{"mode":{"type":"string","enum":["view"]},"id":{"type":"string"}},"required":["mode","id"],"additionalProperties":false},
			"create":{"type":"object","properties":{"mode":{"type":"string","enum":["create"]},"name":{"type":"string"},"notificationPolicy":{"anyOf":[{"type":"string"},{"type":"null"}]}},"required":["mode","name"],"additionalProperties":false},
			"createGroup":{"oneOf":[{"$ref":"#/$defs/create"}]}
		},
		"oneOf":[{"$ref":"#/$defs/view"},{"$ref":"#/$defs/createGroup"}]
	}`)
	if err := schemas.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("unmarshal test schema: %v", err)
	}
	return &schema
}

// TestNormalizeAnthropicToolInputSchemaOtherRootCompositions verifies the two
// other composition keywords named by Anthropic's validation error.
func TestNormalizeAnthropicToolInputSchemaOtherRootCompositions(t *testing.T) {
	for _, test := range []struct {
		name         string
		raw          string
		wantRequired []string
	}{
		{
			name:         "anyOf intersects required fields",
			raw:          `{"type":"object","anyOf":[{"type":"object","properties":{"mode":{"type":"string"},"left":{"type":"string"}},"required":["mode","left"]},{"type":"object","properties":{"mode":{"type":"string"},"right":{"type":"string"}},"required":["mode","right"]}]}`,
			wantRequired: []string{"mode"},
		},
		{
			name:         "allOf unions required fields",
			raw:          `{"type":"object","allOf":[{"type":"object","properties":{"left":{"type":"string"}},"required":["left"]},{"type":"object","properties":{"right":{"type":"string"}},"required":["right"]}]}`,
			wantRequired: []string{"left", "right"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var schema schemas.ToolFunctionParameters
			if err := schemas.Unmarshal([]byte(test.raw), &schema); err != nil {
				t.Fatalf("unmarshal schema: %v", err)
			}
			normalized, err := NormalizeAnthropicToolInputSchema(&schema)
			if err != nil {
				t.Fatalf("normalize schema: %v", err)
			}
			if len(normalized.OneOf) != 0 || len(normalized.AnyOf) != 0 || len(normalized.AllOf) != 0 {
				t.Fatal("expected root composition to be removed")
			}
			if !equalStringSlices(normalized.Required, test.wantRequired) {
				t.Fatalf("required = %v, want %v", normalized.Required, test.wantRequired)
			}
		})
	}
}

// TestNormalizeAnthropicToolInputSchemaRootOneOf verifies that root
// alternatives are merged without removing accepted nested composition.
func TestNormalizeAnthropicToolInputSchemaRootOneOf(t *testing.T) {
	original := codexUnionToolSchema(t)
	normalized, err := NormalizeAnthropicToolInputSchema(original)
	if err != nil {
		t.Fatalf("normalize schema: %v", err)
	}

	if len(normalized.OneOf) != 0 || len(normalized.AnyOf) != 0 || len(normalized.AllOf) != 0 {
		t.Fatal("expected all root composition keywords to be removed")
	}
	if normalized.Type != "object" {
		t.Fatalf("expected object root, got %q", normalized.Type)
	}
	if got := normalized.Properties.Keys(); !equalStringSlices(got, []string{"mode", "id", "name", "notificationPolicy"}) {
		t.Fatalf("unexpected merged property order: %v", got)
	}
	if got := normalized.Required; !equalStringSlices(got, []string{"mode"}) {
		t.Fatalf("expected common required fields, got %v", got)
	}
	if normalized.AdditionalProperties == nil || normalized.AdditionalProperties.AdditionalPropertiesBool == nil || *normalized.AdditionalProperties.AdditionalPropertiesBool {
		t.Fatal("expected merged schema to retain additionalProperties:false")
	}
	mode, _ := normalized.Properties.Get("mode")
	modeSchema := anthropicOrderedMap(mode)
	if modeSchema == nil {
		t.Fatal("expected merged mode schema")
	}
	if _, ok := modeSchema.Get("anyOf"); !ok {
		t.Fatal("expected conflicting mode definitions to become a nested anyOf")
	}
	notification, _ := normalized.Properties.Get("notificationPolicy")
	notificationSchema := anthropicOrderedMap(notification)
	if notificationSchema == nil {
		t.Fatal("expected notificationPolicy schema")
	}
	if _, ok := notificationSchema.Get("anyOf"); !ok {
		t.Fatal("expected the existing nested anyOf to survive")
	}

	if len(original.OneOf) != 2 || original.Properties.Len() != 0 {
		t.Fatal("normalization mutated the caller-owned schema")
	}
}

// TestAnthropicOpenAICompatibleToolPathsNormalizeRootOneOf verifies both API
// integrations use the same Anthropic compatibility transform.
func TestAnthropicOpenAICompatibleToolPathsNormalizeRootOneOf(t *testing.T) {
	t.Run("chat completions", func(t *testing.T) {
		schema := codexUnionToolSchema(t)
		req := &schemas.BifrostChatRequest{
			Provider: schemas.Anthropic,
			Model:    "claude-fable-5",
			Input: []schemas.ChatMessage{{
				Role:    schemas.ChatMessageRoleUser,
				Content: &schemas.ChatMessageContent{ContentStr: schemas.Ptr("hello")},
			}},
			Params: &schemas.ChatParameters{Tools: []schemas.ChatTool{{
				Type: schemas.ChatToolTypeFunction,
				Function: &schemas.ChatToolFunction{
					Name:       "automation_update",
					Parameters: schema,
				},
			}}},
		}
		ctx, cancel := schemas.NewBifrostContextWithCancel(context.Background())
		defer cancel()
		converted, err := ToAnthropicChatRequest(ctx, req)
		if err != nil {
			t.Fatalf("convert chat request: %v", err)
		}
		assertAnthropicRootCompositionRemoved(t, converted.Tools[0].InputSchema)
	})

	t.Run("responses", func(t *testing.T) {
		schema := codexUnionToolSchema(t)
		name := "automation_update"
		tool := &schemas.ResponsesTool{
			Type: schemas.ResponsesToolTypeFunction,
			Name: &name,
			ResponsesToolFunction: &schemas.ResponsesToolFunction{
				Parameters: schema,
			},
		}
		convertedTools, _, err := convertBifrostToolsToAnthropic(
			schemas.ResolveModelCaps(schemas.Anthropic, "claude-fable-5"),
			[]schemas.ResponsesTool{*tool},
			schemas.Anthropic,
		)
		if err != nil {
			t.Fatalf("convert responses tools: %v", err)
		}
		if len(convertedTools) != 1 {
			t.Fatal("expected converted tool")
		}
		assertAnthropicRootCompositionRemoved(t, convertedTools[0].InputSchema)
	})
}

func TestNormalizeAnthropicToolInputSchemaPreservesAllOfPropertyConjunction(t *testing.T) {
	var schema schemas.ToolFunctionParameters
	if err := schemas.Unmarshal([]byte(`{
		"type":"object",
		"allOf":[
			{"type":"object","properties":{"name":{"type":"string"}}},
			{"type":"object","properties":{"name":{"maxLength":5}}}
		]
	}`), &schema); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}

	normalized, err := NormalizeAnthropicToolInputSchema(&schema)
	if err != nil {
		t.Fatalf("normalize schema: %v", err)
	}
	assertAnthropicPropertyComposition(t, normalized.Normalized(), "name", "allOf", 2)

	chatTool, err := convertFunctionToolToAnthropic(schemas.ChatTool{
		Type: schemas.ChatToolTypeFunction,
		Function: &schemas.ChatToolFunction{
			Name:       "constrained_name",
			Parameters: &schema,
		},
	})
	if err != nil {
		t.Fatalf("convert chat tool: %v", err)
	}
	assertAnthropicPropertyComposition(t, chatTool.InputSchema, "name", "allOf", 2)

	name := "constrained_name"
	responseTools, _, err := convertBifrostToolsToAnthropic(
		schemas.ResolveModelCaps(schemas.Anthropic, "claude-fable-5"),
		[]schemas.ResponsesTool{{
			Type:                  schemas.ResponsesToolTypeFunction,
			Name:                  &name,
			ResponsesToolFunction: &schemas.ResponsesToolFunction{Parameters: &schema},
		}},
		schemas.Anthropic,
	)
	if err != nil {
		t.Fatalf("convert responses tool: %v", err)
	}
	assertAnthropicPropertyComposition(t, responseTools[0].InputSchema, "name", "allOf", 2)
}

func TestNormalizeAnthropicToolInputSchemaRejectsExcessiveComplexity(t *testing.T) {
	t.Run("branch count", func(t *testing.T) {
		_, err := NormalizeAnthropicToolInputSchema(explosiveAnthropicToolSchema(t))
		if err == nil || !strings.Contains(err.Error(), "branch count") {
			t.Fatalf("expected branch-count error, got %v", err)
		}
	})

	t.Run("recursion depth", func(t *testing.T) {
		branch := `{"type":"object","properties":{}}`
		for range maxAnthropicSchemaExpansionDepth + 1 {
			branch = `{"anyOf":[` + branch + `]}`
		}
		var schema schemas.ToolFunctionParameters
		if err := schemas.Unmarshal([]byte(`{"type":"object","oneOf":[`+branch+`]}`), &schema); err != nil {
			t.Fatalf("unmarshal deep schema: %v", err)
		}
		_, err := NormalizeAnthropicToolInputSchema(&schema)
		if err == nil || !strings.Contains(err.Error(), "depth") {
			t.Fatalf("expected depth error, got %v", err)
		}
	})
}

func TestAnthropicSchemaComplexityErrorsAreBadRequests(t *testing.T) {
	schema := explosiveAnthropicToolSchema(t)
	ctx, cancel := schemas.NewBifrostContextWithCancel(context.Background())
	defer cancel()

	chatRequest := &schemas.BifrostChatRequest{
		Provider: schemas.Anthropic,
		Model:    "claude-fable-5",
		Input: []schemas.ChatMessage{{
			Role:    schemas.ChatMessageRoleUser,
			Content: &schemas.ChatMessageContent{ContentStr: schemas.Ptr("hello")},
		}},
		Params: &schemas.ChatParameters{Tools: []schemas.ChatTool{{
			Type:     schemas.ChatToolTypeFunction,
			Function: &schemas.ChatToolFunction{Name: "complex", Parameters: schema},
		}}},
	}
	_, chatErr := BuildAnthropicChatRequestBody(ctx, chatRequest, AnthropicRequestBuildConfig{Provider: schemas.Anthropic})
	assertAnthropicBadRequest(t, chatErr)

	name := "complex"
	responsesRequest := &schemas.BifrostResponsesRequest{
		Provider: schemas.Anthropic,
		Model:    "claude-fable-5",
		Input: []schemas.ResponsesMessage{{
			Role:    schemas.Ptr(schemas.ResponsesInputMessageRoleUser),
			Content: &schemas.ResponsesMessageContent{ContentStr: schemas.Ptr("hello")},
		}},
		Params: &schemas.ResponsesParameters{Tools: []schemas.ResponsesTool{{
			Type:                  schemas.ResponsesToolTypeFunction,
			Name:                  &name,
			ResponsesToolFunction: &schemas.ResponsesToolFunction{Parameters: schema},
		}}},
	}
	_, responsesErr := BuildAnthropicResponsesRequestBody(ctx, responsesRequest, AnthropicRequestBuildConfig{Provider: schemas.Anthropic})
	assertAnthropicBadRequest(t, responsesErr)
}

func explosiveAnthropicToolSchema(t *testing.T) *schemas.ToolFunctionParameters {
	t.Helper()
	members := make([]string, 7) // 2^7 combinations exceed the 64-branch limit.
	for i := range members {
		members[i] = `{"anyOf":[{"type":"object","properties":{"left":{"type":"string"}}},{"type":"object","properties":{"right":{"type":"string"}}}]}`
	}
	var schema schemas.ToolFunctionParameters
	if err := schemas.Unmarshal([]byte(`{"type":"object","allOf":[`+strings.Join(members, ",")+`]}`), &schema); err != nil {
		t.Fatalf("unmarshal explosive schema: %v", err)
	}
	return &schema
}

func assertAnthropicPropertyComposition(t *testing.T, schema *schemas.ToolFunctionParameters, property, keyword string, wantMembers int) {
	t.Helper()
	value, ok := schema.Properties.Get(property)
	if !ok {
		t.Fatalf("property %q missing", property)
	}
	propertySchema := anthropicOrderedMap(value)
	members, ok := propertySchema.Get(keyword)
	if !ok {
		t.Fatalf("property %q does not contain %s", property, keyword)
	}
	if got := len(anthropicOrderedMapSlice(members)); got != wantMembers {
		t.Fatalf("property %q %s has %d members, want %d", property, keyword, got, wantMembers)
	}
	if _, ok := propertySchema.Get("anyOf"); keyword == "allOf" && ok {
		t.Fatalf("property %q conjunction was broadened to anyOf", property)
	}
}

func assertAnthropicBadRequest(t *testing.T, err *schemas.BifrostError) {
	t.Helper()
	if err == nil || err.StatusCode == nil || *err.StatusCode != 400 {
		t.Fatalf("expected HTTP 400 error, got %+v", err)
	}
}

// assertAnthropicRootCompositionRemoved checks the provider-facing schema.
func assertAnthropicRootCompositionRemoved(t *testing.T, schema *schemas.ToolFunctionParameters) {
	t.Helper()
	if schema == nil {
		t.Fatal("expected input schema")
	}
	if len(schema.OneOf) != 0 || len(schema.AnyOf) != 0 || len(schema.AllOf) != 0 {
		t.Fatalf("root composition survived Anthropic conversion: %+v", schema)
	}
	if schema.Properties == nil || schema.Properties.Len() != 4 {
		t.Fatalf("expected four merged properties, got %v", schema.Properties)
	}
}

// equalStringSlices reports whether two strings slices have identical order.
func equalStringSlices(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

// TestNormalizeAnthropicToolInputSchemaResolvesReferencesPerRFC6901 pins how a root
// alternative's $ref is read: the URI fragment is percent-decoded before it is split,
// so %2F separates tokens, and ~1 and ~0 are then unescaped inside each token. A
// reference that names nothing, or is not local, is refused instead of being flattened
// into an empty branch that drops its properties and requirements.
func TestNormalizeAnthropicToolInputSchemaResolvesReferencesPerRFC6901(t *testing.T) {
	var schema schemas.ToolFunctionParameters
	raw := []byte(`{
		"type":"object",
		"properties":{},
		"$defs":{
			"a/b":{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]},
			"c~d":{"type":"object","properties":{"id":{"type":"string"},"name":{"type":"string"}},"required":["id","name"]},
			"group":{"child":{"type":"object","properties":{"id":{"type":"string"},"tag":{"type":"string"}},"required":["id"]}}
		},
		"oneOf":[{"$ref":"#/$defs/a~1b"},{"$ref":"#/$defs/c~0d"},{"$ref":"#/$defs/group%2Fchild"}]
	}`)
	if err := schemas.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("unmarshal test schema: %v", err)
	}
	normalized, err := NormalizeAnthropicToolInputSchema(&schema)
	if err != nil {
		t.Fatalf("normalize schema: %v", err)
	}
	if got := normalized.Properties.Keys(); !equalStringSlices(got, []string{"id", "name", "tag"}) {
		t.Fatalf("properties = %v, want every referenced definition's properties", got)
	}
	if got := normalized.Required; !equalStringSlices(got, []string{"id"}) {
		t.Fatalf("required = %v, want the common requirement", got)
	}

	for _, ref := range []string{"#/$defs/absent", "#/$defs/a%2Fb", "https://example.com/schema.json", "#/$defs/group"} {
		var refused schemas.ToolFunctionParameters
		body := `{"type":"object","properties":{},"$defs":{"a/b":{"type":"object"},"group":{"child":{"type":"object"}}},` +
			`"oneOf":[{"$ref":"` + ref + `"},{"type":"object","properties":{"x":{"type":"string"}}}]}`
		if err := schemas.Unmarshal([]byte(body), &refused); err != nil {
			t.Fatal(err)
		}
		_, err := NormalizeAnthropicToolInputSchema(&refused)
		if ref == "#/$defs/group" {
			// group is an object, so it resolves; it only carries no properties.
			if err != nil {
				t.Fatalf("%s: %v", ref, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), ref) {
			t.Fatalf("%s: an unresolvable reference must be refused, got %v", ref, err)
		}
	}
}

// TestNormalizeAnthropicToolInputSchemaConjoinsRefSiblings pins that keywords next to a
// branch's $ref apply together with the referenced schema: a branch that adds a
// required property beside its reference keeps that property and requirement.
func TestNormalizeAnthropicToolInputSchemaConjoinsRefSiblings(t *testing.T) {
	var schema schemas.ToolFunctionParameters
	raw := []byte(`{
		"type":"object",
		"properties":{},
		"$defs":{"base":{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}},
		"allOf":[{"$ref":"#/$defs/base","properties":{"reason":{"type":"string"}},"required":["reason"],"additionalProperties":false}]
	}`)
	if err := schemas.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("unmarshal test schema: %v", err)
	}
	normalized, err := NormalizeAnthropicToolInputSchema(&schema)
	if err != nil {
		t.Fatalf("normalize schema: %v", err)
	}
	if got := normalized.Properties.Keys(); !equalStringSlices(got, []string{"id", "reason"}) {
		t.Fatalf("properties = %v, want the reference's and its siblings' properties", got)
	}
	if got := normalized.Required; !equalStringSlices(got, []string{"id", "reason"}) {
		t.Fatalf("required = %v, want both the reference's and the sibling requirement", got)
	}
	if normalized.AdditionalProperties == nil || normalized.AdditionalProperties.AdditionalPropertiesBool == nil || *normalized.AdditionalProperties.AdditionalPropertiesBool {
		t.Fatalf("additionalProperties = %+v, want the sibling false kept", normalized.AdditionalProperties)
	}
}

// TestNormalizeAnthropicToolInputSchemaBoundsRepeatedReferences pins one work budget for
// the whole expansion. Each Dn is an allOf of ten references to Dn-1, so every expansion
// yields a single branch and the depth stays at eleven; without a shared budget the
// root reference to D10 visits about 10^10 schemas before returning.
func TestNormalizeAnthropicToolInputSchemaBoundsRepeatedReferences(t *testing.T) {
	defs := map[string]any{"D0": map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"type": "string"}}}}
	for i := 1; i <= 10; i++ {
		refs := make([]any, 10)
		for j := range refs {
			refs[j] = map[string]any{"$ref": fmt.Sprintf("#/$defs/D%d", i-1)}
		}
		defs[fmt.Sprintf("D%d", i)] = map[string]any{"allOf": refs}
	}
	raw, err := json.Marshal(map[string]any{"type": "object", "properties": map[string]any{}, "$defs": defs,
		"allOf": []any{map[string]any{"$ref": "#/$defs/D10"}}})
	if err != nil {
		t.Fatal(err)
	}
	var schema schemas.ToolFunctionParameters
	if err := schemas.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("unmarshal test schema: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := NormalizeAnthropicToolInputSchema(&schema)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "too complex") {
			t.Fatalf("a reference DAG that multiplies work must be refused as too complex, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("normalization of a repeated-reference DAG did not return")
	}
}

// TestNormalizeAnthropicToolInputSchemaRefusesSchemaValuedAdditionalProperties pins that a
// branch constraining undeclared properties with a schema is refused: the flattened root
// cannot carry it, and dropping it would accept {"count":123} where the original rejects it.
func TestNormalizeAnthropicToolInputSchemaRefusesSchemaValuedAdditionalProperties(t *testing.T) {
	for name, raw := range map[string]string{
		"direct conjunct": `{"type":"object","properties":{},"allOf":[{"additionalProperties":{"type":"string"}}]}`,
		"reference sibling": `{"type":"object","properties":{},"$defs":{"base":{"type":"object"}},
			"allOf":[{"$ref":"#/$defs/base","additionalProperties":{"type":"string"}}]}`,
	} {
		var schema schemas.ToolFunctionParameters
		if err := schemas.Unmarshal([]byte(raw), &schema); err != nil {
			t.Fatalf("%s: unmarshal: %v", name, err)
		}
		if _, err := NormalizeAnthropicToolInputSchema(&schema); err == nil || !strings.Contains(err.Error(), "additionalProperties") {
			t.Fatalf("%s: want a refusal naming additionalProperties, got %v", name, err)
		}
	}
}

// TestNormalizeAnthropicToolInputSchemaResolvesReferencesThroughArrays pins that a pointer
// may step through array indices (#/$defs/group/allOf/0) as long as it ends on a schema
// object, and that an index with a leading zero or out of range names nothing.
func TestNormalizeAnthropicToolInputSchemaResolvesReferencesThroughArrays(t *testing.T) {
	body := func(ref string) []byte {
		return []byte(`{"type":"object","properties":{},
			"$defs":{"group":{"allOf":[{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}]}},
			"oneOf":[{"$ref":"` + ref + `"},{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}]}`)
	}
	var schema schemas.ToolFunctionParameters
	if err := schemas.Unmarshal(body("#/$defs/group/allOf/0"), &schema); err != nil {
		t.Fatal(err)
	}
	normalized, err := NormalizeAnthropicToolInputSchema(&schema)
	if err != nil {
		t.Fatalf("normalize schema: %v", err)
	}
	if got := normalized.Required; !equalStringSlices(got, []string{"id"}) {
		t.Fatalf("required = %v, want the array element's requirement", got)
	}
	for _, ref := range []string{"#/$defs/group/allOf/00", "#/$defs/group/allOf/1", "#/$defs/group/allOf"} {
		var refused schemas.ToolFunctionParameters
		if err := schemas.Unmarshal(body(ref), &refused); err != nil {
			t.Fatal(err)
		}
		if _, err := NormalizeAnthropicToolInputSchema(&refused); err == nil || !strings.Contains(err.Error(), ref) {
			t.Fatalf("%s: want an unresolved-reference refusal, got %v", ref, err)
		}
	}
}
