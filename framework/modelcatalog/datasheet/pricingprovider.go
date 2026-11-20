package datasheet

import (
	"strings"
	"sync"

	"github.com/capsohq/bifrost/core/schemas"
)

// datasheetProviderNames maps a fork provider to the provider name the datasheet files its rows
// under, where the two differ (fork, TXE-7406). Before this, every request on such a provider looked
// up rows under the fork's name, found none, and was logged with no cost: 2,016 qwen and 532 glm
// requests in production between 2026-09-13 and 2026-11-13. The provider's own name is still tried
// first, so a row filed under it wins.
//
// TestEveryStandardProviderIsADatasheetProviderOrListed fails when a provider in
// schemas.StandardProviders matches no datasheet provider and is in neither this map nor
// providersWithoutDatasheetRows.
var datasheetProviderNames = map[schemas.ModelProvider]string{
	// core/providers/glm sends to https://api.z.ai by default; the datasheet files Z.ai's GLM rows under zai.
	schemas.GLM: "zai",
	// core/providers/qwen sends to Alibaba Cloud Model Studio (dashscope-us.aliyuncs.com) by default;
	// the datasheet files Model Studio's rows under dashscope.
	schemas.Qwen: string(schemas.DashScope),
	// core/providers/volcengine sends ModelArk to BytePlus's international Ark endpoint
	// (ark.ap-southeast.bytepluses.com), the endpoint the byteplus provider and its rows serve.
	schemas.ModelArk: string(schemas.BytePlus),
	// The datasheet spells the provider with an underscore.
	schemas.GithubCopilot: "github_copilot",
	// The datasheet files Black Forest Labs' FLUX rows under its full name.
	schemas.BFL: "black_forest_labs",
}

// customProviderBases holds the base provider of every configured custom provider. A custom
// provider has a name the operator chose (Tokun's tkp_52), so the datasheet has no row under it.
type customProviderBases struct {
	mu    sync.RWMutex
	bases map[schemas.ModelProvider]schemas.ModelProvider
}

// SetCustomProviderBase records the base provider of a custom provider, so that a request on it
// is priced from the base provider's rows when none is filed under its own name (fork, TXE-7406).
// An empty base forgets the provider: it was removed, or is no longer a custom provider.
func (s *Store) SetCustomProviderBase(provider, base schemas.ModelProvider) {
	s.customBases.mu.Lock()
	defer s.customBases.mu.Unlock()
	if base == "" || base == provider {
		delete(s.customBases.bases, provider)
		return
	}
	if s.customBases.bases == nil {
		s.customBases.bases = make(map[schemas.ModelProvider]schemas.ModelProvider)
	}
	s.customBases.bases[provider] = base
}

// ReplaceCustomProviderBases resets every custom provider's base provider at once (startup).
func (s *Store) ReplaceCustomProviderBases(bases map[schemas.ModelProvider]schemas.ModelProvider) {
	next := make(map[schemas.ModelProvider]schemas.ModelProvider, len(bases))
	for provider, base := range bases {
		if base != "" && base != provider {
			next[provider] = base
		}
	}
	s.customBases.mu.Lock()
	s.customBases.bases = next
	s.customBases.mu.Unlock()
}

// CustomProviderBase returns the recorded base provider of a custom provider.
func (s *Store) CustomProviderBase(provider schemas.ModelProvider) (schemas.ModelProvider, bool) {
	if s == nil {
		return "", false
	}
	s.customBases.mu.RLock()
	defer s.customBases.mu.RUnlock()
	base, ok := s.customBases.bases[provider]
	return base, ok
}

// pricingProviders returns the datasheet providers whose rows price a request on provider, in the
// order they are tried: the provider's own name, the datasheet's name for it, then the same two
// for its base provider when it is a custom one. Nothing repeats. Overrides are unaffected: they
// still match the provider the request was sent to.
func (s *Store) pricingProviders(provider string) []string {
	out := make([]string, 0, 4)
	add := func(p string) {
		if p == "" {
			return
		}
		for _, have := range out {
			if have == p {
				return
			}
		}
		out = append(out, p)
	}
	addWithDatasheetName := func(p schemas.ModelProvider) {
		add(normalizeProvider(string(p)))
		add(datasheetProviderNames[p])
	}
	addWithDatasheetName(schemas.ModelProvider(provider))
	if base, ok := s.CustomProviderBase(schemas.ModelProvider(provider)); ok {
		addWithDatasheetName(base)
	}
	return out
}

// maxUnpricedWarnings bounds the (provider, model, mode) triples warnUnpriced remembers, so a
// caller sending endless model names cannot grow the set without limit.
const maxUnpricedWarnings = 4096

// unpricedWarnings remembers the triples already warned about.
type unpricedWarnings struct {
	mu   sync.Mutex
	seen map[string]struct{}
}

// warnUnpriced logs, once per (provider, model, mode), that a request was recorded with no cost
// and why (fork, TXE-7406): no row under any provider pricingProviders tried, and no override.
// Before this the reason was a debug line, and 64,222 production requests were logged with no
// cost and nothing said so.
func (s *Store) warnUnpriced(provider, model string, requestType schemas.RequestType) {
	mode := normalizeRequestType(requestType)
	key := makeKey(model, provider, mode)
	s.unpriced.mu.Lock()
	if _, seen := s.unpriced.seen[key]; seen || len(s.unpriced.seen) >= maxUnpricedWarnings {
		s.unpriced.mu.Unlock()
		return
	}
	if s.unpriced.seen == nil {
		s.unpriced.seen = make(map[string]struct{})
	}
	s.unpriced.seen[key] = struct{}{}
	s.unpriced.mu.Unlock()
	tried := strings.Join(s.pricingProviders(provider), ", ")
	if _, custom := s.CustomProviderBase(schemas.ModelProvider(provider)); !custom && !isStandardProvider(provider) {
		tried += " (an unknown provider: if it is a custom provider, its base provider is not recorded)"
	}
	s.logger.Warn("pricing: model %s on provider %s (%s mode) is recorded with no cost: the datasheet has no row for it under %s, and no pricing override matches; add a pricing override to price it (logged once per provider, model and mode)",
		model, provider, mode, tried)
}

// isStandardProvider reports whether provider is one of schemas.StandardProviders.
func isStandardProvider(provider string) bool {
	for _, p := range schemas.StandardProviders {
		if string(p) == provider {
			return true
		}
	}
	return false
}
