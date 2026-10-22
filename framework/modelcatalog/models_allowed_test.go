package modelcatalog

import (
	"testing"

	"github.com/capsohq/bifrost/core/schemas"
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

// TestIsModelAllowedForProvider_WildcardAllowsUncataloguedModel covers a
// provider the datasheet describes but list-models cannot enumerate: Fireworks
// builds its list from the key's configured models, so a key added with no
// models leaves the live store empty and the pricing sheet, which lags new
// releases, is the only catalog. A virtual key's ["*"] must not be narrower
// than the provider key itself; key restrictions and blacklists still apply.
func TestIsModelAllowedForProvider_WildcardAllowsUncataloguedModel(t *testing.T) {
	ds := datasheet.NewTestStore(nil)
	ds.SetPricingRowsForTest([]configstoreTables.TableModelPricing{
		{Model: "accounts/fireworks/models/glm-5p2", Provider: "fireworks_ai", Mode: "chat"},
		{Model: "mistral-large", Provider: "mistral", Mode: "chat"},
	})
	newCatalog := func(key schemas.Key) *ModelCatalog {
		kc := keyconfig.New(nil)
		kc.Replace(map[schemas.ModelProvider][]schemas.Key{schemas.Fireworks: {key}})
		mc := &ModelCatalog{datasheet: ds, live: live.New(nil), keyconf: kc, done: make(chan struct{})}
		mc.initCaches()
		return mc
	}
	wildcard := schemas.WhiteList{"*"}
	const known, unlisted = "accounts/fireworks/models/glm-5p2", "accounts/fireworks/models/glm-5p3"

	open := newCatalog(schemas.Key{ID: "fw", Enabled: ptrBool(true), Models: schemas.WhiteList{"*"}})
	if !open.IsModelAllowedForProvider(schemas.Fireworks, known, nil, wildcard) {
		t.Fatalf("control: a datasheet-known Fireworks model must be allowed under a wildcard")
	}
	if !open.IsModelAllowedForProvider(schemas.Fireworks, unlisted, nil, wildcard) {
		t.Errorf("wildcard refused %q on an unrestricted, non-enumerable provider", unlisted)
	}

	restricted := newCatalog(schemas.Key{ID: "fw", Enabled: ptrBool(true), Models: schemas.WhiteList{known}})
	if restricted.IsModelAllowedForProvider(schemas.Fireworks, unlisted, nil, wildcard) {
		t.Errorf("wildcard allowed %q although no provider key may serve it", unlisted)
	}
	blocked := newCatalog(schemas.Key{ID: "fw", Enabled: ptrBool(true), Models: schemas.WhiteList{"*"},
		BlacklistedModels: schemas.BlackList{unlisted}})
	if blocked.IsModelAllowedForProvider(schemas.Fireworks, unlisted, nil, wildcard) {
		t.Errorf("wildcard allowed %q although the provider key blacklists it", unlisted)
	}
	// A provider with no configured key has nothing to defer to.
	if open.IsModelAllowedForProvider(schemas.Mistral, "mistral-next", nil, wildcard) {
		t.Errorf("wildcard allowed a model on a provider with no configured key")
	}
}

// TestIsModelAllowedForProvider_WildcardFallbackNeverBroadensRestrictions pins that
// the uncatalogued-model fallback only widens an unrestricted ["*"] grant on a
// provider list-models cannot enumerate. An explicit model list, an empty list, an
// enumerable provider's catalog, and a disabled or block-all key all keep refusing
// a model they do not name.
func TestIsModelAllowedForProvider_WildcardFallbackNeverBroadensRestrictions(t *testing.T) {
	ds := datasheet.NewTestStore(nil)
	ds.SetPricingRowsForTest([]configstoreTables.TableModelPricing{
		{Model: "accounts/fireworks/models/glm-5p2", Provider: "fireworks_ai", Mode: "chat"},
	})
	const known, unlisted = "accounts/fireworks/models/glm-5p2", "accounts/fireworks/models/glm-5p3"
	newCatalog := func(key schemas.Key, listed []string) *ModelCatalog {
		kc := keyconfig.New(nil)
		kc.Replace(map[schemas.ModelProvider][]schemas.Key{schemas.Fireworks: {key}})
		lv := live.New(nil)
		if listed != nil {
			lv.Upsert(schemas.Fireworks, key.ID, true, listed)
		}
		mc := &ModelCatalog{datasheet: ds, live: lv, keyconf: kc, done: make(chan struct{})}
		mc.initCaches()
		return mc
	}
	openKey := schemas.Key{ID: "fw", Enabled: ptrBool(true), Models: schemas.WhiteList{"*"}}

	open := newCatalog(openKey, nil)
	if !open.IsModelAllowedForProvider(schemas.Fireworks, unlisted, nil, schemas.WhiteList{"*"}) {
		t.Fatalf("control: the fallback must allow %q under an unrestricted wildcard", unlisted)
	}
	if open.IsModelAllowedForProvider(schemas.Fireworks, unlisted, nil, schemas.WhiteList{known}) {
		t.Errorf("an explicit model list was broadened to %q", unlisted)
	}
	if !open.IsModelAllowedForProvider(schemas.Fireworks, known, nil, schemas.WhiteList{known}) {
		t.Errorf("control: an explicit list must still allow the model it names")
	}
	if open.IsModelAllowedForProvider(schemas.Fireworks, unlisted, nil, schemas.WhiteList{}) {
		t.Errorf("an empty model list (deny by default) was broadened to %q", unlisted)
	}

	enumerable := newCatalog(openKey, []string{known})
	if enumerable.IsModelAllowedForProvider(schemas.Fireworks, unlisted, nil, schemas.WhiteList{"*"}) {
		t.Errorf("a provider whose list-models answered was broadened past its catalog to %q", unlisted)
	}

	disabled := newCatalog(schemas.Key{ID: "fw", Enabled: ptrBool(false), Models: schemas.WhiteList{"*"}}, nil)
	if disabled.IsModelAllowedForProvider(schemas.Fireworks, unlisted, nil, schemas.WhiteList{"*"}) {
		t.Errorf("a disabled key authorized %q through the fallback", unlisted)
	}
	blockAll := newCatalog(schemas.Key{ID: "fw", Enabled: ptrBool(true), Models: schemas.WhiteList{"*"},
		BlacklistedModels: schemas.BlackList{"*"}}, nil)
	if blockAll.IsModelAllowedForProvider(schemas.Fireworks, unlisted, nil, schemas.WhiteList{"*"}) {
		t.Errorf("a block-all key authorized %q through the fallback", unlisted)
	}
}
