package datasheet

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/schemas"
	configstoreTables "github.com/capsohq/bifrost/framework/configstore/tables"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pricedChatRow is a chat row at $1 per million input tokens and $2 per million output tokens.
func pricedChatRow(model, provider string) configstoreTables.TableModelPricing {
	return configstoreTables.TableModelPricing{
		Model:              model,
		Provider:           provider,
		Mode:               "chat",
		InputCostPerToken:  bifrost.Ptr(1e-6),
		OutputCostPerToken: bifrost.Ptr(2e-6),
	}
}

var pricingProviderTestUsage = &schemas.BifrostLLMUsage{PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500}

// pricingProviderTestCost is pricingProviderTestUsage on pricedChatRow: 1000 x 1e-6 + 500 x 2e-6.
const pricingProviderTestCost = 0.002

// datasheetName is the provider the datasheet files provider's rows under.
func datasheetName(provider schemas.ModelProvider) string {
	if name, ok := datasheetProviderNames[provider]; ok {
		return name
	}
	return normalizeProvider(string(provider))
}

// TestCustomProviderIsPricedFromItsBaseOnEveryBase is the class check for custom providers (fork,
// TXE-7406). A custom provider is named by its operator (Tokun's tkp_52), so the datasheet files no
// row under that name. Pricing looked rows up only by the name the request was sent to, and 35,956
// successful production requests on Tokun's custom providers between 2026-09-13 and 2026-11-13 were logged
// with no cost. Every provider a custom provider can be based on is checked: a model its base
// prices is priced the same on the custom provider, through the response path that logging,
// governance and telemetry read, and through the bare-usage path that a failed request's partial
// usage takes.
func TestCustomProviderIsPricedFromItsBaseOnEveryBase(t *testing.T) {
	for _, base := range schemas.StandardProviders {
		t.Run(string(base), func(t *testing.T) {
			s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
				makeKey("base-model", datasheetName(base), "chat"): pricedChatRow("base-model", datasheetName(base)),
			})
			// The name must not contain a base's name: normalizeProvider folds any name containing
			// "bedrock" or "cohere" onto that provider.
			custom := schemas.ModelProvider("tkp_under_test")

			require.Nil(t, s.CalculateCostBreakdown(makeChatResponse(custom, "base-model", pricingProviderTestUsage), nil),
				"before its base is recorded, nothing prices the custom provider")

			s.SetCustomProviderBase(custom, base)
			breakdown := s.CalculateCostBreakdown(makeChatResponse(custom, "base-model", pricingProviderTestUsage), nil)
			require.NotNil(t, breakdown, "a custom provider over %s was logged with no cost for a model %s prices", base, base)
			assert.InDelta(t, pricingProviderTestCost, breakdown.TotalCost, 1e-12)
			assert.InDelta(t, pricingProviderTestCost,
				s.CalculateCostForUsage(pricingProviderTestUsage, custom, "base-model", schemas.ChatCompletionStreamRequest, nil), 1e-12)

			s.SetCustomProviderBase(custom, "")
			assert.Nil(t, s.CalculateCostBreakdown(makeChatResponse(custom, "base-model", pricingProviderTestUsage), nil),
				"a removed custom provider keeps no base")
		})
	}
}

// TestCustomProviderPricingKeepsItsOwnRowsAndOverrides: a row filed under the custom provider's own
// name still wins over its base's, an override scoped to the custom provider still applies on top
// of the base row, and a model the base does not price stays unpriced rather than borrowing another
// provider's rate.
func TestCustomProviderPricingKeepsItsOwnRowsAndOverrides(t *testing.T) {
	own := pricedChatRow("m", "tkp_own")
	own.InputCostPerToken = bifrost.Ptr(10e-6)
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("m", "openai", "chat"):          pricedChatRow("m", "openai"),
		makeKey("m", "tkp_own", "chat"):         own,
		makeKey("glm-only", "zai", "chat"):      pricedChatRow("glm-only", "zai"),
		makeKey("other-only", "openai", "chat"): pricedChatRow("other-only", "openai"),
	})
	s.ReplaceCustomProviderBases(map[schemas.ModelProvider]schemas.ModelProvider{
		"tkp_own":      schemas.OpenAI,
		"tkp_override": schemas.OpenAI,
		"tkp_glm":      schemas.GLM,
		"same":         "same",
	})
	_, recorded := s.CustomProviderBase("same")
	assert.False(t, recorded, "a provider is not its own base")

	assert.InDelta(t, 1000*10e-6+500*2e-6, s.CalculateCostForUsage(pricingProviderTestUsage, "tkp_own", "m", schemas.ChatCompletionRequest, nil), 1e-12,
		"the custom provider's own row wins")

	providerID := "tkp_override"
	require.NoError(t, s.SetOverrides([]configstoreTables.TablePricingOverride{{
		ID:               "tkp-override",
		ScopeKind:        string(ScopeKindProvider),
		ProviderID:       &providerID,
		MatchType:        string(MatchTypeExact),
		Pattern:          "other-only",
		RequestTypes:     []schemas.RequestType{schemas.ChatCompletionRequest},
		PricingPatchJSON: `{"input_cost_per_token":0.00003}`,
	}}))
	assert.InDelta(t, 1000*30e-6+500*2e-6, s.CalculateCostForUsage(pricingProviderTestUsage, "tkp_override", "other-only", schemas.ChatCompletionRequest, nil), 1e-12,
		"an override on the custom provider applies on top of the base row")

	assert.InDelta(t, pricingProviderTestCost, s.CalculateCostForUsage(pricingProviderTestUsage, "tkp_glm", "glm-only", schemas.ChatCompletionRequest, nil), 1e-12,
		"a custom provider over glm reads the datasheet's name for glm")
	assert.Zero(t, s.CalculateCostForUsage(pricingProviderTestUsage, "tkp_own", "glm-only", schemas.ChatCompletionRequest, nil),
		"a model the base does not price stays unpriced")
}

// TestProviderPricedUnderTheDatasheetsName: glm and qwen requests are priced from the rows the
// datasheet files under zai and dashscope (fork, TXE-7406; 988 glm and 2,105 qwen production
// requests were logged with no cost), and a row under the fork's own name would still win.
func TestProviderPricedUnderTheDatasheetsName(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("glm-5.2", "zai", "chat"):           pricedChatRow("glm-5.2", "zai"),
		makeKey("qwen3.7-max", "dashscope", "chat"): pricedChatRow("qwen3.7-max", "dashscope"),
	})
	assert.InDelta(t, pricingProviderTestCost, s.CalculateCostForUsage(pricingProviderTestUsage, schemas.GLM, "glm-5.2", schemas.ChatCompletionRequest, nil), 1e-12)
	assert.InDelta(t, pricingProviderTestCost, s.CalculateCostForUsage(pricingProviderTestUsage, schemas.Qwen, "qwen3.7-max", schemas.ChatCompletionStreamRequest, nil), 1e-12)

	own := pricedChatRow("glm-5.2", "glm")
	own.InputCostPerToken = bifrost.Ptr(10e-6)
	s.pricingData[makeKey("glm-5.2", "glm", "chat")] = own
	assert.InDelta(t, 1000*10e-6+500*2e-6, s.CalculateCostForUsage(pricingProviderTestUsage, schemas.GLM, "glm-5.2", schemas.ChatCompletionRequest, nil), 1e-12)
}

// upstreamProvidersSnapshot is testdata/upstream_providers.json: every provider the upstream
// datasheet files rows under (after the sync's normalizeProvider), each with one of its models.
type upstreamProvidersSnapshot struct {
	Source    string            `json:"source"`
	Fetched   string            `json:"fetched"`
	Providers map[string]string `json:"providers"`
}

const upstreamProvidersSnapshotPath = "testdata/upstream_providers.json"

// providersWithoutDatasheetRows are the fork's providers the datasheet files no rows under, by any
// name, each with the reason. A request on one is priced only by a pricing override or a
// provider-reported cost; it is logged with no cost otherwise.
var providersWithoutDatasheetRows = map[schemas.ModelProvider]string{
	schemas.SGL:            "self-hosted SGLang server: the operator's own hardware has no list price",
	schemas.VLLM:           "self-hosted vLLM server: the operator's own hardware has no list price",
	schemas.Parasail:       "the datasheet publishes no Parasail rows under any name",
	schemas.Kling:          "the datasheet publishes no Kling rows under any name; videoAudioDefaults records the kling.ai rate, which no row carries",
	schemas.GoogleCloudTTS: "the datasheet files Google's speech rows under vertex (vertex_ai/chirp), keyed by models the Cloud Text-to-Speech API does not take; no row matches a voice request",
	schemas.AzureSpeech:    "the datasheet files Azure Speech's rows under azure as speech/azure-tts and speech/azure-tts-hd, model names an azurespeech request does not send",
}

// TestEveryStandardProviderIsADatasheetProviderOrListed is the class check for provider names
// (fork, TXE-7406). Pricing looks rows up by provider name, so a fork provider whose name the
// datasheet does not use is never priced: glm (the datasheet's zai) and qwen (its dashscope) were
// logged with no cost. Each provider in schemas.StandardProviders must be a provider in the
// committed snapshot of the datasheet under its own name or its datasheetProviderNames entry, or be
// listed in providersWithoutDatasheetRows with the reason. Neither list may keep a stale entry.
//
// Regenerate the snapshot from a datasheet downloaded by hand (tests run offline):
//
//	curl -sSL -o /tmp/datasheet.json https://getbifrost.ai/datasheet
//	UPSTREAM_DATASHEET_FILE=/tmp/datasheet.json UPSTREAM_DATASHEET_FETCHED=$(date -u +%F) \
//	  go test ./modelcatalog/datasheet/ -run TestEveryStandardProviderIsADatasheetProviderOrListed
//
// It runs on every pull request in the Go unit tests (offline) job.
func TestEveryStandardProviderIsADatasheetProviderOrListed(t *testing.T) {
	if source := os.Getenv("UPSTREAM_DATASHEET_FILE"); source != "" {
		writeUpstreamProvidersSnapshot(t, source)
	}
	raw, err := os.ReadFile(upstreamProvidersSnapshotPath)
	require.NoError(t, err)
	var snapshot upstreamProvidersSnapshot
	require.NoError(t, json.Unmarshal(raw, &snapshot))
	require.NotEmpty(t, snapshot.Providers, "%s lists no providers", upstreamProvidersSnapshotPath)

	for _, provider := range schemas.StandardProviders {
		own := normalizeProvider(string(provider))
		_, ownPublished := snapshot.Providers[own]
		alias, aliased := datasheetProviderNames[provider]
		_, aliasPublished := snapshot.Providers[alias]
		reason, listed := providersWithoutDatasheetRows[provider]
		switch {
		case aliased && !aliasPublished:
			t.Errorf("datasheetProviderNames maps %s to %s, which the datasheet no longer publishes", provider, alias)
		case aliased && ownPublished:
			t.Errorf("the datasheet now publishes rows under %s itself (for example %s): drop its datasheetProviderNames entry or say why both are needed", provider, snapshot.Providers[own])
		case (ownPublished || aliasPublished) && listed:
			t.Errorf("%s is in providersWithoutDatasheetRows (%q), but the datasheet publishes rows for it: remove the entry", provider, reason)
		case !ownPublished && !aliasPublished && !listed:
			t.Errorf("the datasheet files no rows under %s, so every %s request is logged with no cost: map it to the datasheet's name in datasheetProviderNames, or list it in providersWithoutDatasheetRows with the reason", provider, provider)
		case listed && strings.TrimSpace(reason) == "":
			t.Errorf("%s is in providersWithoutDatasheetRows without a reason", provider)
		}
	}
	standard := map[schemas.ModelProvider]bool{}
	for _, provider := range schemas.StandardProviders {
		standard[provider] = true
	}
	for provider := range datasheetProviderNames {
		if !standard[provider] {
			t.Errorf("datasheetProviderNames names %s, which is not in schemas.StandardProviders", provider)
		}
	}
	for provider := range providersWithoutDatasheetRows {
		if !standard[provider] {
			t.Errorf("providersWithoutDatasheetRows names %s, which is not in schemas.StandardProviders", provider)
		}
	}
}

// writeUpstreamProvidersSnapshot rewrites testdata/upstream_providers.json from a downloaded datasheet.
func writeUpstreamProvidersSnapshot(t *testing.T, source string) {
	t.Helper()
	raw, err := os.ReadFile(source)
	require.NoError(t, err)
	var datasheet map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &datasheet))
	providers := map[string]string{}
	for _, model := range sortedKeys(datasheet) {
		var entry struct {
			Provider string `json:"provider"`
		}
		if json.Unmarshal(datasheet[model], &entry) != nil || entry.Provider == "" {
			continue
		}
		provider := normalizeProvider(entry.Provider)
		if _, seen := providers[provider]; !seen {
			providers[provider] = model
		}
	}
	out, err := json.MarshalIndent(upstreamProvidersSnapshot{
		Source:    "https://getbifrost.ai/datasheet",
		Fetched:   os.Getenv("UPSTREAM_DATASHEET_FETCHED"),
		Providers: providers,
	}, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Clean(upstreamProvidersSnapshotPath), append(out, '\n'), 0o644))
}

// warnCapture records Warn lines; every other level is dropped.
type warnCapture struct {
	noOpLogger
	mu    sync.Mutex
	warns []string
}

func (w *warnCapture) Warn(format string, args ...any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.warns = append(w.warns, fmt.Sprintf(format, args...))
}

// TestUnpricedRequestIsReportedOnceWithTheReason: a request no row and no override prices is
// recorded with no cost, and the gateway says so once per provider, model and mode, naming the
// providers whose rows it tried (fork, TXE-7406). It said nothing above debug before, so 64,222
// production requests went unpriced unnoticed. The set it remembers is bounded.
func TestUnpricedRequestIsReportedOnceWithTheReason(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gpt-test", "openai", "chat"): pricedChatRow("gpt-test", "openai"),
	})
	logger := &warnCapture{}
	s.logger = logger
	s.SetCustomProviderBase("tkp_52", schemas.OpenAI)

	for range 3 {
		assert.Zero(t, s.CalculateCostForUsage(pricingProviderTestUsage, "tkp_52", "glm-5.2", schemas.ChatCompletionStreamRequest, nil))
	}
	require.Len(t, logger.warns, 1, "reported once per provider, model and mode")
	assert.Contains(t, logger.warns[0], "glm-5.2 on provider tkp_52 (chat mode) is recorded with no cost")
	assert.Contains(t, logger.warns[0], "under tkp_52, openai")

	assert.Zero(t, s.CalculateCostForUsage(pricingProviderTestUsage, "tkp_unknown", "glm-5.2", schemas.ChatCompletionRequest, nil))
	require.Len(t, logger.warns, 2)
	assert.Contains(t, logger.warns[1], "its base provider is not recorded")

	assert.NotZero(t, s.CalculateCostForUsage(pricingProviderTestUsage, "tkp_52", "gpt-test", schemas.ChatCompletionRequest, nil))
	require.Len(t, logger.warns, 2, "a priced request reports nothing")

	for i := range maxUnpricedWarnings + 10 {
		s.CalculateCostForUsage(pricingProviderTestUsage, "tkp_52", fmt.Sprintf("m-%d", i), schemas.ChatCompletionRequest, nil)
	}
	assert.Len(t, s.unpriced.seen, maxUnpricedWarnings, "the remembered set is bounded")
}

// TestOrdinaryProviderPricingAddsNoAllocations: the fallback names (the datasheet's name for a
// provider, a custom provider's base) are built only when the lookup under the provider's own name
// misses (fork, TXE-7406). A request whose row is filed under its own provider must allocate no
// more than it did before the fallbacks existed: 6 allocations on origin/main c4d539212b, measured
// with this test's loop. Building the fallback list first cost one more on every catalog-priced
// request. Raising the budget needs the same before/after measurement.
func TestOrdinaryProviderPricingAddsNoAllocations(t *testing.T) {
	const allocationsBeforeFallbacks = 6
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("own-model", "openai", "chat"): pricedChatRow("own-model", "openai"),
	})
	s.SetCustomProviderBase("tkp_under_test", schemas.OpenAI)
	routing := schemas.RoutingInfo{Provider: schemas.OpenAI, Model: "own-model"}

	require.NotNil(t, s.resolvePricing(routing, schemas.ChatCompletionRequest, LookupScopes{}))
	allocations := testing.AllocsPerRun(200, func() {
		_ = s.resolvePricing(routing, schemas.ChatCompletionRequest, LookupScopes{})
	})
	assert.LessOrEqual(t, allocations, float64(allocationsBeforeFallbacks),
		"pricing a request under its own provider allocates %v times, %d before the fallbacks: build the fallback names only after the own-name lookup misses", allocations, allocationsBeforeFallbacks)
}
