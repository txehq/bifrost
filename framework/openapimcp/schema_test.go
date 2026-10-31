package openapimcp

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSelectBodyContent(t *testing.T) {
	obj := map[string]any{"type": "object"}
	for _, tc := range []struct {
		name     string
		content  map[string]any
		wantCT   string
		wantOK   bool
		wantWord string
	}{
		{"json preferred over form", map[string]any{"application/x-www-form-urlencoded": map[string]any{"schema": obj}, "application/json": map[string]any{"schema": obj}}, ContentTypeJSON, true, ""},
		{"json with charset", map[string]any{"application/json; charset=utf-8": map[string]any{"schema": obj}}, ContentTypeJSON, true, ""},
		{"vendor json keeps its declared type", map[string]any{"application/vnd.api+json": map[string]any{"schema": obj}}, "application/vnd.api+json", true, ""},
		{"wildcard", map[string]any{"*/*": map[string]any{"schema": obj}}, ContentTypeJSON, true, ""},
		{"form only", map[string]any{"application/x-www-form-urlencoded": map[string]any{"schema": obj}}, ContentTypeForm, true, ""},
		{"text only, no schema", map[string]any{"text/plain": map[string]any{}}, ContentTypeText, true, ""},
		{"multipart only", map[string]any{"multipart/form-data": map[string]any{"schema": obj}}, "", false, "multipart"},
		{"octet stream", map[string]any{"application/octet-stream": map[string]any{}}, "", false, "binary"},
		{"image", map[string]any{"image/png": map[string]any{}}, "", false, "image/png"},
		{"streaming jsonl", map[string]any{"application/jsonl": map[string]any{"itemSchema": obj}}, "", false, "streaming"},
		{"jsonl with plain schema is streaming", map[string]any{"application/jsonl": map[string]any{"schema": obj}}, "", false, "streaming"},
		{"json-seq with plain schema is streaming", map[string]any{"application/json-seq": map[string]any{"schema": obj}}, "", false, "streaming"},
		{"json with itemSchema is streaming", map[string]any{"application/json": map[string]any{"itemSchema": obj}}, "", false, "streaming"},
		{"xml only", map[string]any{"application/xml": map[string]any{"schema": obj}}, "", false, "not supported"},
		{"empty", map[string]any{}, "", false, "no content"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ct, schema, ok, reason := SelectBodyContent(tc.content)
			assert.Equal(t, tc.wantOK, ok, reason)
			assert.Equal(t, tc.wantCT, ct)
			if tc.wantOK {
				assert.NotNil(t, schema)
			} else {
				assert.Contains(t, reason, tc.wantWord)
			}
		})
	}
}

func TestNormalizeSchema(t *testing.T) {
	t.Run("3.1 null type array becomes nullable", func(t *testing.T) {
		got := NormalizeSchema(map[string]any{"type": []any{"string", "null"}, "maxLength": 3})
		assert.Equal(t, map[string]any{"type": "string", "nullable": true, "maxLength": 3}, got)
	})
	t.Run("multi type becomes anyOf", func(t *testing.T) {
		got := NormalizeSchema(map[string]any{"type": []any{"string", "integer", "null"}})
		assert.Equal(t, true, got["nullable"])
		assert.Nil(t, got["type"])
		assert.Equal(t, []any{map[string]any{"type": "string"}, map[string]any{"type": "integer"}}, got["anyOf"])
	})
	t.Run("bare null type", func(t *testing.T) {
		got := NormalizeSchema(map[string]any{"type": "null"})
		assert.Equal(t, map[string]any{"nullable": true}, got)
	})
	t.Run("unknown keywords stripped", func(t *testing.T) {
		got := NormalizeSchema(map[string]any{"type": "object", "xml": map[string]any{"name": "pet"}, "externalDocs": "x", "discriminator": map[string]any{"propertyName": "k"}, "example": 1, "x-vendor": true, "deprecated": true, "readOnly": true, "description": "kept", "title": "Pet"})
		assert.Equal(t, map[string]any{"type": "object", "description": "kept", "title": "Pet"}, got)
	})
	t.Run("boolean exclusive bounds dropped, numeric kept", func(t *testing.T) {
		got := NormalizeSchema(map[string]any{"type": "number", "minimum": 1, "exclusiveMinimum": true})
		assert.Equal(t, map[string]any{"type": "number", "minimum": 1}, got)
		got = NormalizeSchema(map[string]any{"type": "number", "exclusiveMinimum": 0})
		assert.Equal(t, 0, got["exclusiveMinimum"])
	})
	t.Run("readOnly properties dropped and required pruned", func(t *testing.T) {
		got := NormalizeSchema(map[string]any{
			"properties": map[string]any{
				"id":   map[string]any{"type": "integer", "readOnly": true},
				"name": map[string]any{"type": "string"},
			},
			"required": []any{"id", "name"},
		})
		assert.Equal(t, "object", got["type"], "properties imply object")
		assert.Equal(t, map[string]any{"name": map[string]any{"type": "string"}}, got["properties"])
		assert.Equal(t, []any{"name"}, got["required"])
	})
	t.Run("required removed entirely when empty", func(t *testing.T) {
		got := NormalizeSchema(map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "integer", "readOnly": true}}, "required": []any{"id"}})
		_, has := got["required"]
		assert.False(t, has)
	})
	t.Run("allOf of objects merged", func(t *testing.T) {
		got := NormalizeSchema(map[string]any{"allOf": []any{
			map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string"}}, "required": []any{"a"}},
			map[string]any{"properties": map[string]any{"b": map[string]any{"type": "integer"}}, "required": []any{"b"}, "description": "second"},
		}})
		_, hasAllOf := got["allOf"]
		assert.False(t, hasAllOf)
		assert.Equal(t, "object", got["type"])
		assert.Equal(t, []any{"a", "b"}, got["required"])
		assert.Len(t, got["properties"], 2)
	})
	t.Run("allOf with non-object member kept", func(t *testing.T) {
		got := NormalizeSchema(map[string]any{"allOf": []any{map[string]any{"type": "string"}, map[string]any{"minLength": 1}}})
		assert.Len(t, got["allOf"], 2)
	})
	t.Run("nested items and additionalProperties recurse", func(t *testing.T) {
		got := NormalizeSchema(map[string]any{
			"type":                 "array",
			"items":                map[string]any{"type": []any{"integer", "null"}, "xml": "x"},
			"additionalProperties": map[string]any{"type": "string", "example": "e"},
		})
		assert.Equal(t, map[string]any{"type": "integer", "nullable": true}, got["items"])
		assert.Equal(t, map[string]any{"type": "string"}, got["additionalProperties"])
	})
	t.Run("nil yields an object", func(t *testing.T) {
		assert.Equal(t, map[string]any{"type": "object"}, NormalizeSchema(nil))
	})
}

func decodeSchema(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	return m
}

func TestFlattenInputSchema(t *testing.T) {
	doc := parseFixture(t, "petstore-3.0.yaml")

	t.Run("params only", func(t *testing.T) {
		raw, args, body, warnings, err := FlattenInputSchema(findOp(t, doc, "GET", "/pets"))
		require.NoError(t, err)
		assert.Nil(t, body)
		assert.Empty(t, warnings)
		schema := decodeSchema(t, raw)
		props := asMap(schema["properties"])
		assert.ElementsMatch(t, []string{"limit", "tags", "X-Request-Id"}, sortedKeys(props))
		assert.Equal(t, "Page size (query parameter)", asMap(props["limit"])["description"])
		assert.Equal(t, "(header parameter)", asMap(props["X-Request-Id"])["description"])
		_, hasRequired := schema["required"]
		assert.False(t, hasRequired)
		require.Len(t, args, 3)
		assert.Equal(t, ArgBinding{ArgName: "limit", Location: InQuery, Name: "limit", Explode: true}, args[0])
		assert.False(t, args[1].Explode, "explode: false honored")
	})

	t.Run("flattened json body with readOnly dropped and required propagated", func(t *testing.T) {
		raw, args, body, _, err := FlattenInputSchema(findOp(t, doc, "POST", "/pets"))
		require.NoError(t, err)
		require.NotNil(t, body)
		assert.Equal(t, "flattened", body.Mode)
		assert.Equal(t, ContentTypeJSON, body.ContentType)
		assert.Equal(t, []string{"name", "owner", "tag"}, body.ArgNames)
		assert.Empty(t, args)
		schema := decodeSchema(t, raw)
		props := asMap(schema["properties"])
		assert.NotContains(t, props, "id", "readOnly property is not an input")
		assert.Equal(t, []any{"name"}, schema["required"], "id was required but readOnly; body is required so name stays required")
		assert.Equal(t, true, asMap(props["tag"])["nullable"])
	})

	t.Run("param colliding with another param is suffixed on both sides", func(t *testing.T) {
		raw, args, _, _, err := FlattenInputSchema(findOp(t, doc, "GET", "/pets/{petId}"))
		require.NoError(t, err)
		props := asMap(decodeSchema(t, raw)["properties"])
		assert.ElementsMatch(t, []string{"petId__path", "petId__query"}, sortedKeys(props))
		assert.Equal(t, []any{"petId__path"}, decodeSchema(t, raw)["required"])
		require.Len(t, args, 2)
		assert.Equal(t, "petId", args[0].Name)
		assert.Equal(t, InPath, args[0].Location)
		assert.True(t, args[0].Required)
	})

	t.Run("text body is a single string", func(t *testing.T) {
		raw, _, body, _, err := FlattenInputSchema(findOp(t, doc, "PUT", "/notes"))
		require.NoError(t, err)
		assert.Equal(t, "single", body.Mode)
		assert.Equal(t, "body", body.SingleArg)
		schema := decodeSchema(t, raw)
		assert.Equal(t, "string", asMap(asMap(schema["properties"])["body"])["type"])
		assert.Equal(t, []any{"body"}, schema["required"])
	})

	t.Run("body property colliding with a param keeps the plain name", func(t *testing.T) {
		op := Operation{Method: "POST", Path: "/x",
			Params: []Parameter{{Name: "name", In: InQuery, Schema: map[string]any{"type": "string"}}},
			Body:   &RequestBody{ContentType: ContentTypeJSON, Required: true, Schema: map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}}}},
		}
		raw, args, body, _, err := FlattenInputSchema(op)
		require.NoError(t, err)
		props := asMap(decodeSchema(t, raw)["properties"])
		assert.ElementsMatch(t, []string{"name", "name__query"}, sortedKeys(props))
		assert.Equal(t, []string{"name"}, body.ArgNames)
		assert.Equal(t, "name__query", args[0].ArgName)
	})

	t.Run("array and dictionary bodies are a single argument", func(t *testing.T) {
		for _, schema := range []map[string]any{
			{"type": "array", "items": map[string]any{"type": "string"}},
			{"type": "object", "additionalProperties": map[string]any{"type": "string"}},
			{"oneOf": []any{map[string]any{"type": "string"}, map[string]any{"type": "integer"}}},
		} {
			op := Operation{Method: "POST", Path: "/x", Body: &RequestBody{ContentType: ContentTypeJSON, Schema: schema}}
			raw, _, body, _, err := FlattenInputSchema(op)
			require.NoError(t, err)
			assert.Equal(t, "single", body.Mode)
			s := decodeSchema(t, raw)
			assert.Contains(t, asMap(s["properties"]), "body")
			_, hasRequired := s["required"]
			assert.False(t, hasRequired, "optional body is not required")
		}
	})

	t.Run("optional body properties are never required", func(t *testing.T) {
		op := Operation{Method: "POST", Path: "/x", Body: &RequestBody{ContentType: ContentTypeJSON, Required: false, Schema: map[string]any{"type": "object", "required": []any{"a"}, "properties": map[string]any{"a": map[string]any{"type": "string"}}}}}
		raw, _, _, _, err := FlattenInputSchema(op)
		require.NoError(t, err)
		_, hasRequired := decodeSchema(t, raw)["required"]
		assert.False(t, hasRequired)
	})

	t.Run("3.2 querystring object flattens into query args", func(t *testing.T) {
		doc32 := parseFixture(t, "petstore-3.2.yaml")
		raw, args, _, _, err := FlattenInputSchema(findOp(t, doc32, "GET", "/search"))
		require.NoError(t, err)
		schema := decodeSchema(t, raw)
		assert.ElementsMatch(t, []string{"page", "q"}, sortedKeys(asMap(schema["properties"])))
		assert.Equal(t, []any{"q"}, schema["required"])
		require.Len(t, args, 2)
		assert.Equal(t, InQuery, args[0].Location)
	})

	t.Run("3.2 querystring string is sent verbatim", func(t *testing.T) {
		op := Operation{Method: "GET", Path: "/x", Params: []Parameter{{Name: "raw", In: InQuerystring, Required: true, Schema: map[string]any{"type": "string"}}}}
		raw, args, _, _, err := FlattenInputSchema(op)
		require.NoError(t, err)
		assert.Equal(t, []any{"raw"}, decodeSchema(t, raw)["required"])
		assert.Equal(t, InQuerystring, args[0].Location)
	})

	t.Run("unknown location skipped with warning", func(t *testing.T) {
		op := Operation{Method: "GET", Path: "/x", Params: []Parameter{{Name: "weird", In: "matrix", Schema: map[string]any{"type": "string"}}}}
		_, args, _, warnings, err := FlattenInputSchema(op)
		require.NoError(t, err)
		assert.Empty(t, args)
		require.Len(t, warnings, 1)
	})
}

func TestSynthesize_Fixtures(t *testing.T) {
	t.Run("3.0", func(t *testing.T) {
		syn, err := Synthesize(parseFixture(t, "petstore-3.0.yaml"), SynthesizeOptions{ClientName: "petstore"})
		require.NoError(t, err)
		assert.Equal(t, []string{"putNote", "listPets", "createPet", "getPetById", "get_search"}, syn.ToolNames())
		var reasons []string
		for _, u := range syn.Unsupported {
			reasons = append(reasons, u.OperationID+": "+u.Reason)
		}
		require.Len(t, reasons, 2)
		assert.Contains(t, reasons[0], "deletePet: deprecated")
		assert.Contains(t, reasons[1], "uploadPetPhoto: multipart")

		get := findTool(t, syn, "getPetById")
		assert.True(t, *get.Annotations.ReadOnlyHint)
		assert.Empty(t, get.Warnings, "anonymous (security: []) operations have nothing to warn about")

		withDeprecated, err := Synthesize(parseFixture(t, "petstore-3.0.yaml"), SynthesizeOptions{ClientName: "petstore", IncludeDeprecated: true})
		require.NoError(t, err)
		assert.Contains(t, withDeprecated.ToolNames(), "deletePet")
		assert.Contains(t, findTool(t, withDeprecated, "deletePet").Description, "[deprecated]")
	})
	t.Run("swagger 2", func(t *testing.T) {
		syn, err := Synthesize(parseFixture(t, "petstore-swagger2.json"), SynthesizeOptions{ClientName: "petstore"})
		require.NoError(t, err)
		assert.Equal(t, []string{"login", "listPets", "createPet", "getPetById"}, syn.ToolNames())
		login := findTool(t, syn, "login")
		assert.Equal(t, ContentTypeForm, login.Body.ContentType)
		assert.Equal(t, "flattened", login.Body.Mode)
	})
	t.Run("3.1", func(t *testing.T) {
		syn, err := Synthesize(parseFixture(t, "petstore-3.1.json"), SynthesizeOptions{ClientName: "petstore"})
		require.NoError(t, err)
		create := findTool(t, syn, "createPet")
		props := asMap(decodeSchema(t, create.InputSchema)["properties"])
		assert.Equal(t, map[string]any{"type": "string", "nullable": true, "maxLength": float64(20)}, props["tag"])
		assert.Equal(t, []any{map[string]any{"type": "string"}, map[string]any{"type": "integer"}}, asMap(props["status"])["anyOf"])
		assert.Equal(t, float64(0), asMap(props["weight"])["exclusiveMinimum"])
	})
	t.Run("3.2", func(t *testing.T) {
		syn, err := Synthesize(parseFixture(t, "petstore-3.2.yaml"), SynthesizeOptions{ClientName: "petstore"})
		require.NoError(t, err)
		assert.Equal(t, []string{"listPets", "queryPets", "purgePets", "searchPets"}, syn.ToolNames())
		assert.True(t, *findTool(t, syn, "queryPets").Annotations.ReadOnlyHint)
		assert.True(t, *findTool(t, syn, "purgePets").Annotations.DestructiveHint)
		require.Len(t, syn.Unsupported, 1)
		assert.Contains(t, syn.Unsupported[0].Reason, "streaming")
	})
}

func TestSynthesize_WarningsAndLimits(t *testing.T) {
	doc := &Document{
		SecuritySchemes: map[string]SecurityScheme{"OAuth": {Name: "OAuth", Type: "oauth2"}},
		GlobalSecurity:  []SecurityRequirement{{"OAuth": {}}},
	}
	for i := 0; i < ToolCountWarnThreshold+1; i++ {
		doc.Operations = append(doc.Operations, Operation{ID: "op", Method: "GET", Path: "/p" + string(rune('a'+i%26)) + string(rune('a'+i/26))})
	}
	syn, err := Synthesize(doc, SynthesizeOptions{ClientName: "c"})
	require.NoError(t, err)
	assert.Len(t, syn.Tools, ToolCountWarnThreshold+1)
	joined := ""
	for _, w := range syn.Warnings {
		joined += w + "\n"
	}
	assert.Contains(t, joined, "exceed the recommended")
	assert.Contains(t, joined, "renamed")
	assert.Contains(t, syn.Tools[0].Warnings[0], "OAuth (oauth2)")

	huge := &Document{}
	for i := 0; i <= MaxToolCount; i++ {
		huge.Operations = append(huge.Operations, Operation{Method: "GET", Path: "/x"})
	}
	_, err = Synthesize(huge, SynthesizeOptions{})
	require.ErrorContains(t, err, "above the limit")

	_, err = Synthesize(nil, SynthesizeOptions{})
	require.Error(t, err)
}

// TestNormalizeSchema_AllOfWithSiblingsIsOrderIndependent: allOf merged inside
// the key loop overwrote sibling properties/required depending on map order.
func TestNormalizeSchema_AllOfWithSiblingsIsOrderIndependent(t *testing.T) {
	for i := 0; i < 50; i++ {
		got := NormalizeSchema(map[string]any{
			"type":       "object",
			"required":   []any{"x"},
			"properties": map[string]any{"x": map[string]any{"type": "string"}},
			"allOf": []any{
				map[string]any{"type": "object", "required": []any{"y"}, "properties": map[string]any{"y": map[string]any{"type": "integer"}}},
			},
		})
		props := asMap(got["properties"])
		require.Contains(t, props, "x", "iteration %d", i)
		require.Contains(t, props, "y", "iteration %d", i)
		require.ElementsMatch(t, []any{"x", "y"}, got["required"], "iteration %d", i)
	}
}

// TestFlattenInputSchema_BodyWithAdditionalPropertiesStaysWhole: flattening a
// body that allows extra keys would drop every key not named in the schema.
func TestFlattenInputSchema_BodyWithAdditionalPropertiesStaysWhole(t *testing.T) {
	for _, extra := range []any{true, map[string]any{"type": "string"}} {
		op := Operation{Method: "POST", Path: "/labels", Body: &RequestBody{ContentType: ContentTypeJSON, Schema: map[string]any{
			"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}}, "additionalProperties": extra,
		}}}
		_, _, body, _, err := FlattenInputSchema(op)
		require.NoError(t, err)
		require.NotNil(t, body)
		assert.Equal(t, "single", body.Mode, "additionalProperties=%v", extra)
	}
	// additionalProperties: false keeps flattening.
	op := Operation{Method: "POST", Path: "/labels", Body: &RequestBody{ContentType: ContentTypeJSON, Schema: map[string]any{
		"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}}, "additionalProperties": false,
	}}}
	_, _, body, _, err := FlattenInputSchema(op)
	require.NoError(t, err)
	assert.Equal(t, "flattened", body.Mode)
}

// TestFlattenInputSchema_SuffixedNamesStayUnique: a suffixed parameter name can
// itself collide with a body property; every argument must get its own name.
func TestFlattenInputSchema_SuffixedNamesStayUnique(t *testing.T) {
	op := Operation{Method: "POST", Path: "/items", Params: []Parameter{
		{Name: "id", In: InQuery, Schema: map[string]any{"type": "string"}},
		{Name: "qs", In: InQuerystring, Schema: map[string]any{"type": "object", "properties": map[string]any{"id__query": map[string]any{"type": "string"}}}},
	}, Body: &RequestBody{ContentType: ContentTypeJSON, Schema: map[string]any{
		"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}, "id__query": map[string]any{"type": "string"}},
	}}}
	raw, args, body, _, err := FlattenInputSchema(op)
	require.NoError(t, err)
	seen := map[string]bool{}
	for _, name := range body.ArgNames {
		seen[name] = true
	}
	for _, a := range args {
		assert.False(t, seen[a.ArgName], "argument %q for %s %q reuses a taken name", a.ArgName, a.Location, a.Name)
		seen[a.ArgName] = true
	}
	var schema map[string]any
	require.NoError(t, json.Unmarshal(raw, &schema))
	assert.Len(t, asMap(schema["properties"]), 4, "body id, body id__query, query id and querystring id__query each get a property")
}

// TestSelectBodyContent_KeepsDeclaredJSONMediaType: a vendor JSON type is
// encoded as JSON but sent with the type the spec declares.
func TestSelectBodyContent_KeepsDeclaredJSONMediaType(t *testing.T) {
	ct, _, ok, _ := SelectBodyContent(map[string]any{"application/vnd.api+json": map[string]any{"schema": map[string]any{"type": "object"}}})
	require.True(t, ok)
	assert.Equal(t, "application/vnd.api+json", ct)
}

// TestFlattenInputSchema_BodyChoicesStayWhole: flattening a body that also
// carries oneOf/anyOf would drop the fields defined inside the choice, and a
// nullable body could no longer be sent as null.
func TestFlattenInputSchema_BodyChoicesStayWhole(t *testing.T) {
	named := map[string]any{"name": map[string]any{"type": "string"}}
	for label, schema := range map[string]map[string]any{
		"oneOf":    {"type": "object", "properties": named, "oneOf": []any{map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string"}}}}},
		"anyOf":    {"type": "object", "properties": named, "anyOf": []any{map[string]any{"type": "object", "properties": map[string]any{"b": map[string]any{"type": "string"}}}}},
		"nullable": {"type": "object", "properties": named, "nullable": true},
	} {
		op := Operation{Method: "POST", Path: "/x", Body: &RequestBody{ContentType: ContentTypeJSON, Schema: schema}}
		_, _, body, _, err := FlattenInputSchema(op)
		require.NoError(t, err)
		assert.Equal(t, "single", body.Mode, label)
	}
}

// TestFlattenInputSchema_AllOfMemberChoicesStayWhole: an allOf member carrying
// a choice or open extra keys cannot be merged into flat properties without
// losing fields, so the body is sent whole.
func TestFlattenInputSchema_AllOfMemberChoicesStayWhole(t *testing.T) {
	id := map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}}}
	for label, member := range map[string]map[string]any{
		"oneOf":                {"type": "object", "oneOf": []any{map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string"}}}}},
		"anyOf":                {"type": "object", "anyOf": []any{map[string]any{"type": "object", "properties": map[string]any{"b": map[string]any{"type": "string"}}}}},
		"additionalProperties": {"type": "object", "properties": map[string]any{"c": map[string]any{"type": "string"}}, "additionalProperties": true},
		"nullable":             {"type": "object", "properties": map[string]any{"d": map[string]any{"type": "string"}}, "nullable": true},
	} {
		schema := NormalizeSchema(map[string]any{"allOf": []any{id, member}})
		op := Operation{Method: "POST", Path: "/x", Body: &RequestBody{ContentType: ContentTypeJSON, Schema: schema}}
		_, _, body, _, err := FlattenInputSchema(op)
		require.NoError(t, err)
		assert.Equal(t, "single", body.Mode, label)
	}
}

// TestFlattenInputSchema_RootPropertiesWithUnmergedAllOfStayWhole: a body
// naming its own properties beside an allOf that cannot be merged would
// flatten to the root properties alone, so fields supplied for the allOf
// branch were never sent.
func TestFlattenInputSchema_RootPropertiesWithUnmergedAllOfStayWhole(t *testing.T) {
	contact := map[string]any{"type": "object", "oneOf": []any{
		map[string]any{"type": "object", "properties": map[string]any{"email": map[string]any{"type": "string"}}},
		map[string]any{"type": "object", "properties": map[string]any{"phone": map[string]any{"type": "string"}}},
	}}
	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any{"id": map[string]any{"type": "string"}},
		"allOf":      []any{contact},
	}
	require.Contains(t, NormalizeSchema(schema), "allOf", "fixture: the allOf must survive normalization unmerged")
	op := Operation{Method: "POST", Path: "/x", Body: &RequestBody{ContentType: ContentTypeJSON, Schema: schema}}
	_, _, body, _, err := FlattenInputSchema(op)
	require.NoError(t, err)
	assert.Equal(t, "single", body.Mode)
}

// partialAllOfSchema is a body whose allOf cannot be merged: required names a
// field declared only inside it, one declared in a choice inside it, one that is
// readOnly there, and a nested member repeats the shape one level down.
func partialAllOfSchema() map[string]any {
	choice := map[string]any{"oneOf": []any{
		map[string]any{"type": "object", "properties": map[string]any{"email": map[string]any{"type": "string", "format": "email", "maxLength": 254.0}}},
		map[string]any{"type": "object", "properties": map[string]any{"phone": map[string]any{"type": "string", "pattern": "^[0-9]+$"}}},
	}}
	return map[string]any{
		"type":     "object",
		"required": []any{"id", "name", "nested", "email", "server_side"},
		"properties": map[string]any{
			"id": map[string]any{"type": "string", "minLength": 3.0},
		},
		"allOf": []any{
			map[string]any{"type": "object", "properties": map[string]any{
				"name":        map[string]any{"type": "string", "maxLength": 40.0},
				"server_side": map[string]any{"type": "string", "readOnly": true},
			}},
			map[string]any{"allOf": []any{
				map[string]any{"type": "object", "properties": map[string]any{"nested": map[string]any{"type": "integer", "minimum": 1.0}}},
				choice,
			}},
		},
	}
}

// TestNormalizeSchema_PartialAllOfKeepsItsDeclarations: what normalization
// could not merge stays with every constraint, and a required name declared
// inside the kept allOf is not "gone", while a readOnly one is.
func TestNormalizeSchema_PartialAllOfKeepsItsDeclarations(t *testing.T) {
	stored := partialAllOfSchema()
	got := NormalizeSchema(stored)
	assert.Equal(t, partialAllOfSchema(), stored, "normalizing must not write to the stored schema")

	assert.Equal(t, []any{"id", "name", "nested", "email"}, got["required"])
	assert.Equal(t, 3.0, got["properties"].(map[string]any)["id"].(map[string]any)["minLength"])
	members := got["allOf"].([]any)
	require.Len(t, members, 2)
	first := members[0].(map[string]any)["properties"].(map[string]any)
	assert.Equal(t, 40.0, first["name"].(map[string]any)["maxLength"])
	assert.NotContains(t, first, "server_side")
	nested := members[1].(map[string]any)["allOf"].([]any)
	require.Len(t, nested, 2)
	assert.Equal(t, 1.0, nested[0].(map[string]any)["properties"].(map[string]any)["nested"].(map[string]any)["minimum"])
	branches := nested[1].(map[string]any)["oneOf"].([]any)
	assert.Equal(t, "email", branches[0].(map[string]any)["properties"].(map[string]any)["email"].(map[string]any)["format"])
	assert.Equal(t, "^[0-9]+$", branches[1].(map[string]any)["properties"].(map[string]any)["phone"].(map[string]any)["pattern"])

	wrapped := NormalizeSchema(map[string]any{"type": "object", "properties": map[string]any{"item": partialAllOfSchema()}})
	inner := wrapped["properties"].(map[string]any)["item"].(map[string]any)
	assert.Equal(t, []any{"id", "name", "nested", "email"}, inner["required"], "a nested schema keeps them too")
	assert.Contains(t, inner, "allOf")
}

// TestFlattenInputSchema_PartialAllOfArgumentNamesStayDistinct: the whole body
// takes the "body" argument, so a parameter of that name is suffixed rather than
// shadowing it, and the tool's schema is the whole allOf.
func TestFlattenInputSchema_PartialAllOfArgumentNamesStayDistinct(t *testing.T) {
	stored := partialAllOfSchema()
	op := Operation{Method: "POST", Path: "/x", Body: &RequestBody{Required: true, ContentType: ContentTypeJSON, Schema: stored},
		Params: []Parameter{{Name: "body", In: InQuery, Schema: map[string]any{"type": "string"}}}}
	raw, args, body, _, err := FlattenInputSchema(op)
	require.NoError(t, err)
	assert.Equal(t, partialAllOfSchema(), stored)
	assert.Equal(t, "single", body.Mode)
	assert.Equal(t, "body", body.SingleArg)
	require.Len(t, args, 1)
	assert.Equal(t, "body__query", args[0].ArgName)
	var schema map[string]any
	require.NoError(t, json.Unmarshal(raw, &schema))
	whole := schema["properties"].(map[string]any)["body"].(map[string]any)
	assert.Contains(t, whole, "allOf")
	assert.Equal(t, []any{"id", "name", "nested", "email"}, whole["required"])
	assert.Equal(t, []any{"body"}, schema["required"])
}

// TestMergeAllOf_MemberWithItsOwnUnmergedAllOfStaysWhole: merging such a member
// would keep only its named properties and lose the allOf beside them.
func TestMergeAllOf_MemberWithItsOwnUnmergedAllOfStaysWhole(t *testing.T) {
	member := map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string"}},
		"allOf": []any{map[string]any{"oneOf": []any{map[string]any{"type": "object"}, map[string]any{"type": "string"}}}}}
	got := NormalizeSchema(map[string]any{"allOf": []any{member, map[string]any{"type": "object", "properties": map[string]any{"b": map[string]any{"type": "string"}}}}})
	require.Contains(t, got, "allOf")
	assert.Contains(t, got["allOf"].([]any)[0], "allOf")
}

// TestSelectBodyContent_BooleanSchemas: OpenAPI 3.1 `schema: true` accepts any
// value, so it is not narrowed to an object; `schema: false` accepts nothing.
func TestSelectBodyContent_BooleanSchemas(t *testing.T) {
	_, schema, ok, _ := SelectBodyContent(map[string]any{"application/json": map[string]any{"schema": true}})
	require.True(t, ok)
	assert.NotContains(t, schema, "type", "schema: true must not be narrowed to an object")

	_, _, ok, reason := SelectBodyContent(map[string]any{"application/json": map[string]any{"schema": false}})
	assert.False(t, ok)
	assert.Contains(t, reason, "schema: false")
}

// TestNormalizeSchema_DropsBooleanRequired: `required: true` on a property is
// invalid JSON Schema (required is a list on the parent); it is dropped rather
// than passed into the tool's input schema.
func TestNormalizeSchema_DropsBooleanRequired(t *testing.T) {
	out := NormalizeSchema(map[string]any{
		"type":       "object",
		"required":   []any{"b"},
		"properties": map[string]any{"a": map[string]any{"type": "string", "required": true}, "b": map[string]any{"type": "string"}},
	})
	assert.NotContains(t, out["properties"].(map[string]any)["a"], "required")
	assert.Equal(t, []any{"b"}, out["required"])
}

// TestFlattenInputSchema_QuerystringWithExtraKeysStaysWhole: an OpenAPI 3.2
// querystring object that allows extra keys or choices cannot be split into
// named arguments without losing keys, so it stays one argument.
func TestFlattenInputSchema_QuerystringWithExtraKeysStaysWhole(t *testing.T) {
	op := Operation{Method: "GET", Path: "/x", Params: []Parameter{{Name: "qs", In: InQuerystring, Schema: map[string]any{
		"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string"}}, "additionalProperties": true,
	}}}}
	_, args, _, _, err := FlattenInputSchema(op)
	require.NoError(t, err)
	require.Len(t, args, 1)
	assert.Equal(t, InQuerystring, args[0].Location)
}

// TestFlattenInputSchema_QuerystringBooleanPropertySchemas: in a 3.2
// querystring object a `true` property accepts any value and stays
// unconstrained, a `false` property accepts none and is not offered, and a
// required `false` property makes the operation unusable.
func TestFlattenInputSchema_QuerystringBooleanPropertySchemas(t *testing.T) {
	op := Operation{Method: "GET", Path: "/x", Params: []Parameter{{Name: "qs", In: InQuerystring, Required: true, Schema: map[string]any{
		"type": "object", "properties": map[string]any{"any": true, "never": false, "q": map[string]any{"type": "string"}},
	}}}}
	raw, args, _, _, err := FlattenInputSchema(op)
	require.NoError(t, err)
	var schema map[string]any
	require.NoError(t, json.Unmarshal(raw, &schema))
	props := schema["properties"].(map[string]any)
	require.Contains(t, props, "any")
	assert.NotContains(t, props["any"], "type", "a true schema accepts any value")
	assert.NotContains(t, props, "never", "a false schema accepts no value")
	names := make([]string, 0, len(args))
	for _, a := range args {
		names = append(names, a.Name)
	}
	assert.ElementsMatch(t, []string{"any", "q"}, names)

	op.Params[0].Schema["required"] = []any{"never"}
	_, _, _, _, err = FlattenInputSchema(op)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "never")

	// An optional querystring that requires such a field can only be left
	// out: the tool keeps working without it and offers none of its fields.
	op.Params[0].Required = false
	op.Params = append(op.Params, Parameter{Name: "page", In: InQuery, Schema: map[string]any{"type": "integer"}})
	raw, args, _, warnings, err := FlattenInputSchema(op)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &schema))
	assert.Equal(t, []string{"page"}, sortedKeys(schema["properties"].(map[string]any)))
	require.Len(t, args, 1)
	assert.Equal(t, "page", args[0].Name)
	assert.NotEmpty(t, warnings)
}

// TestSelectBodyContent_FallsBackToAUsableMediaType: a preferred media type
// that cannot be sent (streaming itemSchema, schema false) does not hide a
// usable alternative the operation also offers.
func TestSelectBodyContent_FallsBackToAUsableMediaType(t *testing.T) {
	form := map[string]any{"schema": map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string"}}}}
	ct, _, ok, reason := SelectBodyContent(map[string]any{"application/json": map[string]any{"itemSchema": map[string]any{}}, "application/x-www-form-urlencoded": form})
	require.True(t, ok, reason)
	assert.Equal(t, ContentTypeForm, ct)
	ct, _, ok, reason = SelectBodyContent(map[string]any{"application/json": map[string]any{"schema": false}, "text/plain": map[string]any{}})
	require.True(t, ok, reason)
	assert.Equal(t, ContentTypeText, ct)
	_, _, ok, reason = SelectBodyContent(map[string]any{"application/json": map[string]any{"schema": false}})
	assert.False(t, ok)
	assert.Contains(t, reason, "schema: false")
}

// BenchmarkNormalizeSchema_PartialAllOf: normalization runs at config time, never
// per tool call.
func BenchmarkNormalizeSchema_PartialAllOf(b *testing.B) {
	schema := partialAllOfSchema()
	b.ReportAllocs()
	for b.Loop() {
		NormalizeSchema(schema)
	}
}
