package openapimcp

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToolName(t *testing.T) {
	for _, tc := range []struct {
		name string
		op   Operation
		max  int
		want string
	}{
		{"operationId kept", Operation{ID: "listPets", Method: "GET", Path: "/pets"}, 60, "listPets"},
		{"hyphens and dots sanitized", Operation{ID: "list-pets.v2", Method: "GET", Path: "/pets"}, 60, "list_pets_v2"},
		{"spaces and unicode", Operation{ID: "list pets ünïcode", Method: "GET", Path: "/pets"}, 60, "list_pets_n_code"},
		{"leading digit prefixed", Operation{ID: "2ndList", Method: "GET", Path: "/pets"}, 60, "op_2ndList"},
		{"fallback from method and path", Operation{Method: "GET", Path: "/pets/{petId}/photos"}, 60, "get_pets_by_petId_photos"},
		{"fallback root path", Operation{Method: "POST", Path: "/"}, 60, "post_root"},
		{"custom method fallback", Operation{Method: "PURGE", Path: "/cache"}, 60, "purge_cache"},
		{"only symbols falls back", Operation{ID: "---", Method: "GET", Path: "/a-b"}, 60, "get_a_b"},
		{"truncated to budget", Operation{ID: strings.Repeat("abcde", 20), Method: "GET", Path: "/x"}, 20, strings.Repeat("abcde", 4)},
		{"truncation trims trailing underscore", Operation{ID: "abcdefghij_klmnop", Method: "GET", Path: "/x"}, 11, "abcdefghij"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ToolName(tc.op, tc.max))
		})
	}
}

func TestToolNameBudget(t *testing.T) {
	assert.Equal(t, MaxToolNameLen-len("petstore")-1, toolNameBudget("petstore"))
	assert.Equal(t, MaxToolNameLen-81, toolNameBudget(strings.Repeat("x", 80)), "the budget is what remains; Synthesize refuses one below minToolNameBudget")
	assert.Equal(t, MaxToolNameLen-1, toolNameBudget(""))
}

func TestDedupeToolNames(t *testing.T) {
	tools := []Tool{
		{Name: "listPets", Operation: Operation{Method: "GET", Path: "/pets"}},
		{Name: "listPets", Operation: Operation{Method: "GET", Path: "/pets/all"}},
		{Name: "listPets", Operation: Operation{Method: "GET", Path: "/pets/old"}},
		{Name: "listPets_2", Operation: Operation{Method: "GET", Path: "/legacy"}},
		{Name: "createPet", Operation: Operation{Method: "POST", Path: "/pets"}},
	}
	warnings := DedupeToolNames(tools, 60)
	names := make([]string, len(tools))
	for i, tool := range tools {
		names[i] = tool.Name
	}
	assert.Equal(t, []string{"listPets", "listPets_3", "listPets_4", "listPets_2", "createPet"}, names, "walk order decides the suffix, and an existing listPets_2 is not clobbered")
	assert.Len(t, warnings, 2)

	again := DedupeToolNames(tools, 60)
	assert.Empty(t, again, "a second pass over unique names is a no-op")

	long := []Tool{
		{Name: strings.Repeat("a", 20), Operation: Operation{Method: "GET", Path: "/1"}},
		{Name: strings.Repeat("a", 20), Operation: Operation{Method: "GET", Path: "/2"}},
	}
	DedupeToolNames(long, 20)
	assert.Equal(t, strings.Repeat("a", 18)+"_2", long[1].Name, "suffixed name re-truncates to the budget")
}

func TestBuildDescription(t *testing.T) {
	op := Operation{Method: "GET", Path: "/pets", Summary: "List pets", Description: "Returns every pet."}
	assert.Equal(t, "List pets\n\nReturns every pet.\n\nGET /pets", BuildDescription(op))

	assert.Equal(t, "GET /pets", BuildDescription(Operation{Method: "GET", Path: "/pets"}))
	assert.Equal(t, "Same\n\nGET /pets", BuildDescription(Operation{Method: "GET", Path: "/pets", Summary: "Same", Description: "Same"}))
	assert.Equal(t, "DELETE /pets [deprecated]", BuildDescription(Operation{Method: "DELETE", Path: "/pets", Deprecated: true}))

	long := BuildDescription(Operation{Method: "GET", Path: "/x", Description: strings.Repeat("d", maxDescriptionLen+50)})
	require.Contains(t, long, "…")
	assert.Less(t, len(long), maxDescriptionLen+20)

	// The summary is capped too: every tool built from a reused path item gets
	// its own copy of the description.
	longSummary := BuildDescription(Operation{Method: "GET", Path: "/x", Summary: strings.Repeat("s", maxDescriptionLen+50)})
	require.Contains(t, longSummary, "…")
	assert.Less(t, len(longSummary), maxDescriptionLen+20)
}

// TestSynthesize_ReusedPathItemSummaryIsBounded: a path item reused by many
// paths hands its summary to every tool. Past the document budget the reuse is
// refused at parse; within it, each tool's copy is capped.
func TestSynthesize_ReusedPathItemSummaryIsBounded(t *testing.T) {
	spec := func(uses, size int) string {
		var paths strings.Builder
		for i := 0; i < uses; i++ {
			if i > 0 {
				paths.WriteByte(',')
			}
			paths.WriteString(`"/p` + strconv.Itoa(i) + `":{"$ref":"#/components/pathItems/Shared"}`)
		}
		return `{"openapi":"3.1.0","info":{"title":"t","version":"1"},"servers":[{"url":"https://a"}],"paths":{` + paths.String() + `},
"components":{"pathItems":{"Shared":{"get":{"summary":"` + strings.Repeat("s", size) + `","responses":{"200":{"description":"ok"}}}}}}}`
	}
	_, err := Parse([]byte(spec(200, 1<<20)), ParseOptions{})
	require.Error(t, err, "200 uses of a 1 MiB summary exceed the document budget")
	assert.Contains(t, err.Error(), "across the document")

	doc, err := Parse([]byte(spec(50, 256<<10)), ParseOptions{})
	require.NoError(t, err)
	syn, err := Synthesize(doc, SynthesizeOptions{ClientName: "t"})
	require.NoError(t, err)
	require.Len(t, syn.Tools, 50)
	total := 0
	for _, tool := range syn.Tools {
		assert.LessOrEqual(t, len(tool.Description), maxDescriptionLen+64, tool.Name)
		total += len(tool.Description)
	}
	assert.Less(t, total, 50*(maxDescriptionLen+64))
}

func TestAnnotationsFor(t *testing.T) {
	get := annotationsFor("GET")
	assert.True(t, *get.ReadOnlyHint)
	assert.False(t, *get.DestructiveHint)
	assert.True(t, *get.IdempotentHint)
	assert.True(t, *get.OpenWorldHint)

	query := annotationsFor("QUERY")
	assert.True(t, *query.ReadOnlyHint)

	put := annotationsFor("PUT")
	assert.False(t, *put.ReadOnlyHint)
	assert.False(t, *put.DestructiveHint)
	assert.True(t, *put.IdempotentHint)

	del := annotationsFor("DELETE")
	assert.True(t, *del.DestructiveHint)
	assert.True(t, *del.IdempotentHint)

	post := annotationsFor("POST")
	assert.True(t, *post.DestructiveHint)
	assert.False(t, *post.IdempotentHint)

	purge := annotationsFor("PURGE")
	assert.True(t, *purge.DestructiveHint)
}

// TestSynthesize_ToolNamesFitWithLongClientNames: "<client>-<tool>" must fit
// MaxToolNameLen. A client name that leaves too little room is refused rather
// than producing names providers reject.
func TestSynthesize_ToolNamesFitWithLongClientNames(t *testing.T) {
	spec := `{"openapi":"3.0.0","info":{"title":"t","version":"1"},"servers":[{"url":"https://a"}],"paths":{"/a":{"get":{"operationId":"getEverythingNow","responses":{"200":{"description":"ok"}}}}}}`
	doc, err := Parse([]byte(spec), ParseOptions{})
	require.NoError(t, err)
	_, err = Synthesize(doc, SynthesizeOptions{ClientName: strings.Repeat("c", 50)})
	require.ErrorContains(t, err, "too long")

	client := strings.Repeat("c", 40)
	syn, err := Synthesize(doc, SynthesizeOptions{ClientName: client})
	require.NoError(t, err)
	for _, name := range syn.ToolNames() {
		assert.LessOrEqual(t, len(client)+1+len(name), MaxToolNameLen, name)
	}
}

// TestSynthesize_SecurityWarningIsBoundedPerTool: an unsupported scheme with a
// huge name, required by the root security of many operations, is named in a
// warning on every tool. Each warning is capped where it is composed, so the
// tools do not each retain a copy of the name.
func TestSynthesize_SecurityWarningIsBoundedPerTool(t *testing.T) {
	name := strings.Repeat("n", 1<<20)
	var paths strings.Builder
	for i := 0; i < 100; i++ {
		if i > 0 {
			paths.WriteByte(',')
		}
		paths.WriteString(`"/p` + strconv.Itoa(i) + `":{"get":{"operationId":"g` + strconv.Itoa(i) + `","responses":{"200":{"description":"ok"}}}}`)
	}
	spec := `{"openapi":"3.0.3","info":{"title":"t","version":"1"},"servers":[{"url":"https://a"}],"security":[{"` + name + `":[]}],
"components":{"securitySchemes":{"` + name + `":{"type":"oauth2","flows":{"clientCredentials":{"tokenUrl":"https://a/t","scopes":{}}}}}},
"paths":{` + paths.String() + `}}`
	doc, err := Parse([]byte(spec), ParseOptions{})
	require.NoError(t, err)
	syn, err := Synthesize(doc, SynthesizeOptions{ClientName: "t"})
	require.NoError(t, err)
	require.Len(t, syn.Tools, 100)
	total := 0
	for _, tool := range syn.Tools {
		require.NotEmpty(t, tool.Warnings, "the unsupported scheme is still reported")
		for _, w := range tool.Warnings {
			assert.LessOrEqual(t, len(w), maxDescriptionLen+8)
			total += len(w)
		}
	}
	assert.Less(t, total, 100*(maxDescriptionLen+8)*2)
}
