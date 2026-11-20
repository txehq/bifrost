package governance

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// declaredRequestTypes reads every RequestType constant from core/schemas, so a request type added
// later is checked without editing this test.
func declaredRequestTypes(t *testing.T) []schemas.RequestType {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, "../../core/schemas", nil, 0)
	require.NoError(t, err)
	var types []schemas.RequestType
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				gen, ok := decl.(*ast.GenDecl)
				if !ok || gen.Tok != token.CONST {
					continue
				}
				for _, spec := range gen.Specs {
					value, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					ident, ok := value.Type.(*ast.Ident)
					if !ok || ident.Name != "RequestType" {
						continue
					}
					for _, v := range value.Values {
						lit, ok := v.(*ast.BasicLit)
						if !ok || lit.Kind != token.STRING {
							continue
						}
						s, err := strconv.Unquote(lit.Value)
						require.NoError(t, err)
						types = append(types, schemas.RequestType(s))
					}
				}
			}
		}
	}
	require.Greater(t, len(types), 50, "found only %d RequestType constants in core/schemas; the parser no longer sees them", len(types))
	return types
}

// TestEveryRequestTypeHasABudgetPricing is the request-type half of the TXE-7957 class check: every
// request type says how a dollar budget is charged for it, so a new operation cannot reach a
// provider under a budget with nobody having decided whether its answers are priced. A type that
// is no longer declared must leave the table.
func TestEveryRequestTypeHasABudgetPricing(t *testing.T) {
	declared := map[schemas.RequestType]bool{}
	for _, requestType := range declaredRequestTypes(t) {
		declared[requestType] = true
		_, classified := requestTypeBudgetPricing[requestType]
		assert.True(t, classified, "request type %q has no entry in requestTypeBudgetPricing: say whether its answer is priced, priced at settlement, or never charged", requestType)
	}
	for requestType := range requestTypeBudgetPricing {
		assert.True(t, declared[requestType], "requestTypeBudgetPricing lists %q, which core/schemas no longer declares", requestType)
	}
	// What is priced at settlement must be what job accounting settles; anything else priced on a
	// queued create would charge nothing and never be judged.
	for requestType, pricing := range requestTypeBudgetPricing {
		if pricing == budgetPricingSettled {
			assert.Contains(t, []schemas.RequestType{schemas.VideoGenerationRequest, schemas.VideoEditRequest, schemas.VideoRemixRequest, schemas.BatchCreateRequest}, requestType,
				"%s is marked priced at settlement, but framework/jobaccounting settles only video and batch jobs", requestType)
		}
	}
}

// The unpriced set refuses a triple it holds, folds a stream onto its request type, keeps every
// settled job type under one key, and once full refuses rather than forgets.
func TestUnpricedAnswers(t *testing.T) {
	u := newUnpricedAnswers()
	u.record(schemas.OpenAI, "m", schemas.ChatCompletionStreamRequest)
	assert.True(t, u.contains(schemas.OpenAI, "m", schemas.ChatCompletionRequest))
	assert.False(t, u.contains(schemas.OpenAI, "m", schemas.EmbeddingRequest))
	assert.False(t, u.contains(schemas.Anthropic, "m", schemas.ChatCompletionRequest))

	u.record(schemas.OpenAI, "clip", schemas.BatchCreateRequest)
	assert.True(t, u.contains(schemas.OpenAI, "clip", schemas.VideoGenerationRequest))

	for i := 0; len(u.seen) < maxUnpricedAnswers; i++ {
		u.record(schemas.OpenAI, strconv.Itoa(i), schemas.ChatCompletionRequest)
	}
	assert.False(t, u.contains(schemas.OpenAI, "never-seen", schemas.ChatCompletionRequest))
	u.record(schemas.OpenAI, "one-too-many", schemas.ChatCompletionRequest)
	assert.True(t, u.contains(schemas.OpenAI, "never-seen", schemas.ChatCompletionRequest), "a full set refuses every triple rather than forget one")
	assert.LessOrEqual(t, len(u.seen), maxUnpricedAnswers)
}
