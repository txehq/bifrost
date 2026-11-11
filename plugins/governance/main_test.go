package governance

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/capsohq/bifrost/framework/modelcatalog"
	"github.com/capsohq/bifrost/framework/modelcatalog/datasheet"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every Decision that refuses a request. Hand-kept: Go cannot enumerate constants.
// A missed entry surfaces at runtime as schemas.ErrorTypeOther, which is alarmed on.
var allRefusalDecisions = []Decision{
	DecisionAccessNotFound,
	DecisionAccessBlocked,
	DecisionAccessUnresolved,
	DecisionModelBlocked,
	DecisionProviderBlocked,
	DecisionRateLimited,
	DecisionTokenLimited,
	DecisionRequestLimited,
	DecisionBudgetExceeded,
	DecisionMCPToolBlocked,
}

// The status code cannot tell these apart: three decisions answer 403, and our 429
// differs from an upstream's.
func TestEveryRefusalDeclaresAnErrorType(t *testing.T) {
	for _, decision := range allRefusalDecisions {
		t.Run(string(decision), func(t *testing.T) {
			// decide is nil-safe on ctx and touches no plugin state when refusing.
			_, bifrostErr := (&GovernancePlugin{}).decide(nil, &EvaluationResult{
				Decision: decision,
				Reason:   "refused for test",
			})
			if bifrostErr == nil {
				t.Fatalf("decision %q must refuse the request with an error", decision)
			}
			if bifrostErr.ExtraFields.ErrorType == "" {
				t.Fatalf("decision %q declared no ErrorType; it will be counted as %q",
					decision, schemas.ErrorTypeOther)
			}
			got := schemas.ClassifyErrorType(bifrostErr, schemas.ChatCompletionRequest)
			if got == schemas.ErrorTypeOther {
				t.Errorf("decision %q classified as %q", decision, schemas.ErrorTypeOther)
			}
			if got != bifrostErr.ExtraFields.ErrorType {
				t.Errorf("declared %q but classified as %q; the declaration must win",
					bifrostErr.ExtraFields.ErrorType, got)
			}
		})
	}
}

// Not merely non-empty: a copy-paste declaring everything policy_access_denied would
// pass the test above while making the metric useless.
func TestRefusalsDeclareTheIntendedErrorType(t *testing.T) {
	for decision, want := range map[Decision]schemas.ErrorType{
		DecisionModelBlocked:    schemas.ErrorTypePolicyModelBlocked,
		DecisionProviderBlocked: schemas.ErrorTypePolicyProviderBlocked,
		DecisionAccessNotFound:  schemas.ErrorTypePolicyAccessDenied,
		DecisionAccessBlocked:   schemas.ErrorTypePolicyAccessDenied,
		DecisionBudgetExceeded:  schemas.ErrorTypePolicyBudgetExceeded,
		DecisionRateLimited:     schemas.ErrorTypePolicyRateLimited,
		DecisionTokenLimited:    schemas.ErrorTypePolicyRateLimited,
		DecisionRequestLimited:  schemas.ErrorTypePolicyRateLimited,
		DecisionMCPToolBlocked:  schemas.ErrorTypePolicyToolBlocked,
		// Not a policy refusal: the deployment is misassembled, not the caller.
		DecisionAccessUnresolved: schemas.ErrorTypeBifrostInternal,
	} {
		_, bifrostErr := (&GovernancePlugin{}).decide(nil, &EvaluationResult{Decision: decision})
		if bifrostErr == nil {
			t.Fatalf("decision %q must refuse the request with an error", decision)
		}
		if got := bifrostErr.ExtraFields.ErrorType; got != want {
			t.Errorf("decision %q declared %q, want %q", decision, got, want)
		}
	}
}

// Type still carries the raw decision, so clients matching on it keep working.
func TestRefusalStillCarriesRawDecisionInType(t *testing.T) {
	for _, decision := range allRefusalDecisions {
		_, bifrostErr := (&GovernancePlugin{}).decide(nil, &EvaluationResult{Decision: decision})
		if bifrostErr == nil || bifrostErr.Type == nil {
			t.Fatalf("decision %q lost its wire-level Type", decision)
		}
		if *bifrostErr.Type != string(decision) {
			t.Errorf("decision %q surfaced as Type %q", decision, *bifrostErr.Type)
		}
	}
}

// A vendor that bills embeddings by character (Vertex text models report only
// usage.billed_characters) advances a virtual key's dollar budget through the per-character
// price (TXE-7081): before, such a response cost nothing, so an authorised caller could
// repeat paid embeddings without using budget. Token limits count tokens only, so a
// character-only response leaves them where they were; the dollar budget and the request
// limit are what bound it. Bifrost never converts characters into tokens (contract C5).
func TestPostHookWorkerChargesCharacterPricedEmbeddingsToTheDollarBudget(t *testing.T) {
	pricing := filepath.Join(t.TempDir(), "pricing.json")
	require.NoError(t, os.WriteFile(pricing, []byte(`{"text-embedding-005":{"mode":"embedding","provider":"vertex","base_model":"text-embedding-005","input_cost_per_token":0,"input_cost_per_character":2.5e-8,"output_cost_per_token":0}}`), 0o600))
	ds := datasheet.New(nil, NewMockLogger(), datasheet.Config{URL: "file://" + pricing})
	require.NoError(t, ds.LoadFromURLIntoMemory(context.Background()))

	fixture := newAccountingFixture(t)
	plugin := &GovernancePlugin{
		ctx:          context.Background(),
		tracker:      fixture.tracker,
		modelCatalog: modelcatalog.NewTestCatalogWithDatasheet(ds),
	}
	chars := 12000
	result := &schemas.BifrostResponse{EmbeddingResponse: &schemas.BifrostEmbeddingResponse{
		Usage: &schemas.BifrostLLMUsage{BilledCharacters: &chars},
		ExtraFields: schemas.BifrostResponseExtraFields{
			RequestType: schemas.EmbeddingRequest,
			RoutingInfo: schemas.RoutingInfo{Provider: schemas.Vertex, Model: "text-embedding-005"},
		},
	}}

	settled := settleLimits(fixture.store, "sk-bf-acct", schemas.Vertex, "text-embedding-005", &UsageUpdate{})
	plugin.postHookWorker(result, nil, schemas.Vertex, "text-embedding-005", schemas.EmbeddingRequest, "chars-1", "", false, 0, 0, false, nil, settled.Budgets, settled.RateLimits, nil)

	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.InDelta(c, 12000*2.5e-8, fixture.cost(), 1e-15)
		assert.Equal(c, int64(1), fixture.requests())
	}, time.Second, 10*time.Millisecond)
	assert.Equal(t, int64(0), fixture.tokens(), "characters are not tokens; the token limit does not move")
}
