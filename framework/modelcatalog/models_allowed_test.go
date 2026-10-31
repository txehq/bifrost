package modelcatalog

import (
	"testing"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/capsohq/bifrost/framework/configstore"
	configstoreTables "github.com/capsohq/bifrost/framework/configstore/tables"
	"github.com/capsohq/bifrost/framework/modelcatalog/datasheet"
	"github.com/capsohq/bifrost/framework/modelcatalog/keyconfig"
	"github.com/capsohq/bifrost/framework/modelcatalog/live"
)

// TestIsModelAllowedForProvider_ExplicitList pins the explicit-allowlist branch
// after the option-1 lazy-defer rewrite: bare-name and provider-prefixed
// matching must behave exactly as before.
func TestIsModelAllowedForProvider_ExplicitList(t *testing.T) {
	mc := &ModelCatalog{
		datasheet: datasheet.NewTestStore(map[string]string{"gpt-4o": "gpt-4o"}),
		live:      live.New(nil),
		keyconf:   keyconfig.New(nil),
		done:      make(chan struct{}),
	}
	mc.initCaches()
	// Give OpenAI a live catalog carrying a provider-prefixed entry, so the
	// prefixed branch has something to match (ParseModelString only strips
	// recognized provider prefixes, so this must be a real provider).
	provider := schemas.OpenAI
	mc.UpsertLive(provider, "k1", false, []string{"openai/gpt-4o", "gpt-4o"})

	cases := []struct {
		name    string
		model   string
		allowed schemas.WhiteList
		want    bool
	}{
		{"bare direct match", "gpt-4o", schemas.WhiteList{"gpt-4o", "claude"}, true},
		{"bare match is case-insensitive like every other list check", "gpt-4o", schemas.WhiteList{"GPT-4O"}, true},
		{"bare no match (deny)", "gpt-4o", schemas.WhiteList{"claude", "gemini"}, false},
		{"empty allowlist denies", "gpt-4o", schemas.WhiteList{}, false},
		{"prefixed match", "gpt-4o", schemas.WhiteList{"openai/gpt-4o"}, true},
		{"prefixed present but wrong model", "gpt-4o-mini", schemas.WhiteList{"openai/gpt-4o"}, false},
		{"match after a prefixed miss (ordering)", "gpt-4o", schemas.WhiteList{"openai/other", "openai/gpt-4o"}, true},
		{"regex entry matches the bare name", "gpt-4o-mini", schemas.WhiteList{"regex:^gpt-4.*"}, true},
		{"regex entry is matched against the bare name only", "gpt-4o", schemas.WhiteList{"regex:^openai/gpt-4o$"}, false},
		{"regex entry miss", "gpt-3.5-turbo", schemas.WhiteList{"regex:^gpt-4.*"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mc.IsModelAllowedForProvider(provider, tc.model, nil, tc.allowed)
			if got != tc.want {
				t.Errorf("IsModelAllowedForProvider(%q, %v) = %v, want %v", tc.model, tc.allowed, got, tc.want)
			}
		})
	}
}

// TestIsModelAllowedForProvider_WildcardAllowsUncataloguedModel covers a provider the
// datasheet describes but list-models cannot enumerate. A virtual key's ["*"] must not be
// narrower than the provider key itself, and must not be wider either: the keys'
// allow-lists and blacklists still decide, and explicit lists are not touched.
func TestIsModelAllowedForProvider_WildcardAllowsUncataloguedModel(t *testing.T) {
	ds := datasheet.NewTestStore(nil)
	ds.SetPricingRowsForTest([]configstoreTables.TableModelPricing{
		{Model: "accounts/fireworks/models/glm-5p2", Provider: "fireworks_ai", Mode: "chat"},
		{Model: "mistral-large", Provider: "mistral", Mode: "chat"},
	})
	newCatalog := func(keys ...schemas.Key) *ModelCatalog {
		kc := keyconfig.New(nil)
		kc.Replace(map[schemas.ModelProvider][]schemas.Key{schemas.Fireworks: keys})
		mc := &ModelCatalog{datasheet: ds, live: live.New(nil), keyconf: kc, done: make(chan struct{})}
		mc.initCaches()
		return mc
	}
	wildcard := schemas.WhiteList{"*"}
	const known, unlisted = "accounts/fireworks/models/glm-5p2", "accounts/fireworks/models/glm-5p3"
	on := ptrBool(true)
	open := schemas.Key{ID: "fw", Enabled: on, Models: schemas.WhiteList{"*"}}

	for _, tc := range []struct {
		name     string
		catalog  *ModelCatalog
		provider schemas.ModelProvider
		model    string
		config   *configstore.ProviderConfig
		allowed  schemas.WhiteList
		want     bool
	}{
		{"datasheet model under a wildcard", newCatalog(open), schemas.Fireworks, known, nil, wildcard, true},
		{"uncatalogued model with an open key", newCatalog(open), schemas.Fireworks, unlisted, nil, wildcard, true},
		{"one of two keys allows it", newCatalog(schemas.Key{ID: "a", Enabled: on, Models: schemas.WhiteList{known}},
			schemas.Key{ID: "b", Enabled: on, Models: schemas.WhiteList{unlisted}}), schemas.Fireworks, unlisted, nil, wildcard, true},
		{"key restricted to another model", newCatalog(schemas.Key{ID: "fw", Enabled: on, Models: schemas.WhiteList{known}}), schemas.Fireworks, unlisted, nil, wildcard, false},
		{"key blacklists it", newCatalog(schemas.Key{ID: "fw", Enabled: on, Models: schemas.WhiteList{"*"}, BlacklistedModels: schemas.BlackList{unlisted}}), schemas.Fireworks, unlisted, nil, wildcard, false},
		{"only key disabled", newCatalog(schemas.Key{ID: "fw", Enabled: ptrBool(false), Models: schemas.WhiteList{"*"}}), schemas.Fireworks, unlisted, nil, wildcard, false},
		{"provider with no configured key", newCatalog(open), schemas.Mistral, "mistral-next", nil, wildcard, false},
		{"explicit list stays explicit", newCatalog(open), schemas.Fireworks, unlisted, nil, schemas.WhiteList{known}, false},
		{"empty list stays deny", newCatalog(open), schemas.Fireworks, unlisted, nil, schemas.WhiteList{}, false},
		{"custom provider without list-models", newCatalog(schemas.Key{ID: "fw", Enabled: on, Models: schemas.WhiteList{known}}), schemas.Fireworks, unlisted,
			&configstore.ProviderConfig{CustomProviderConfig: &schemas.CustomProviderConfig{AllowedRequests: &schemas.AllowedRequests{ChatCompletion: true}}}, wildcard, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.catalog.IsModelAllowedForProvider(tc.provider, tc.model, tc.config, tc.allowed); got != tc.want {
				t.Fatalf("IsModelAllowedForProvider = %v, want %v", got, tc.want)
			}
		})
	}

	// An enumerable provider's live list narrows a wildcard whatever its keys allow.
	enumerable := newCatalog(open)
	enumerable.UpsertLive(schemas.Fireworks, "fw", true, []string{known})
	if enumerable.IsModelAllowedForProvider(schemas.Fireworks, unlisted, nil, wildcard) {
		t.Errorf("a wildcard went past the live list of an enumerable provider")
	}
}
