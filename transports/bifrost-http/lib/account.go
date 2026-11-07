// Package lib provides core functionality for the Bifrost HTTP service,
// including context propagation, header management, and integration with monitoring systems.
package lib

import (
	"context"
	"fmt"
	"strings"

	"github.com/capsohq/bifrost/core/network"
	"github.com/capsohq/bifrost/core/schemas"
	configstoreTables "github.com/capsohq/bifrost/framework/configstore/tables"
)

// BaseAccount implements the Account interface for Bifrost.
// It manages provider configurations using a in-memory store for persistent storage.
// All data processing (environment variables, key configs) is done upfront in the store.
type BaseAccount struct {
	store *Config // store for in-memory configuration
}

// NewBaseAccount creates a new BaseAccount with the given store
func NewBaseAccount(store *Config) *BaseAccount {
	return &BaseAccount{
		store: store,
	}
}

// GetConfiguredProviders returns a list of all configured providers.
// Implements the Account interface.
func (baseAccount *BaseAccount) GetConfiguredProviders() ([]schemas.ModelProvider, error) {
	if baseAccount.store == nil {
		return nil, fmt.Errorf("store not initialized")
	}
	return baseAccount.store.GetAllProviders()
}

// GetKeysForProvider returns the API keys configured for a specific provider.
// Keys are already processed (environment variables resolved) by the store.
// Implements the Account interface.
func (baseAccount *BaseAccount) GetKeysForProvider(ctx context.Context, providerKey schemas.ModelProvider) ([]schemas.Key, error) {
	if baseAccount.store == nil {
		return nil, fmt.Errorf("store not initialized")
	}
	config, err := baseAccount.store.GetProviderConfigRaw(providerKey)
	if err != nil {
		return nil, err
	}
	keys := config.Keys
	if v := ctx.Value(schemas.BifrostContextKeyGovernanceIncludeOnlyKeys); v != nil {
		if includeOnlyKeys, ok := v.([]string); ok {
			if len(includeOnlyKeys) == 0 {
				// header present but empty means "no keys allowed"
				keys = nil
			} else {
				set := make(map[string]struct{}, len(includeOnlyKeys))
				for _, id := range includeOnlyKeys {
					set[id] = struct{}{}
				}
				filtered := make([]schemas.Key, 0, len(keys))
				for _, key := range keys {
					if _, ok := set[key.ID]; ok {
						filtered = append(filtered, key)
					}
				}
				keys = filtered
			}
		}
	}
	return keys, nil
}

// GetConfigForProvider returns the complete configuration for a specific provider.
// Configuration is already fully processed (environment variables, key configs) by the store.
// Implements the Account interface.
func (baseAccount *BaseAccount) GetConfigForProvider(providerKey schemas.ModelProvider) (*schemas.ProviderConfig, error) {
	if baseAccount.store == nil {
		return nil, fmt.Errorf("store not initialized")
	}
	config, err := baseAccount.store.GetProviderConfigRaw(providerKey)
	if err != nil {
		return nil, err
	}
	providerConfig := &schemas.ProviderConfig{}
	if config.ProxyConfig != nil {
		providerConfig.ProxyConfig = config.ProxyConfig
	}
	if config.NetworkConfig != nil {
		providerConfig.NetworkConfig = *config.NetworkConfig
	} else {
		providerConfig.NetworkConfig = schemas.DefaultNetworkConfig
	}
	if inherited, ok := inheritGlobalProxy(providerConfig.ProxyConfig, baseAccount.store.GetGlobalProxyConfig()); ok {
		// skip_tls_verify travels on inherited.proxy (ProxyConfig.SkipTLSVerify), which
		// scopes it to TLS through the proxy. Setting NetworkConfig.InsecureSkipVerify
		// instead would also skip verification for no_proxy hosts reached directly.
		providerConfig.ProxyConfig = inherited.proxy
	}
	if config.ConcurrencyAndBufferSize != nil {
		providerConfig.ConcurrencyAndBufferSize = *config.ConcurrencyAndBufferSize
	} else {
		providerConfig.ConcurrencyAndBufferSize = schemas.DefaultConcurrencyAndBufferSize
	}
	providerConfig.SendBackRawRequest = config.SendBackRawRequest
	providerConfig.SendBackRawResponse = config.SendBackRawResponse
	providerConfig.StoreRawRequestResponse = config.StoreRawRequestResponse
	providerConfig.IgnoreProviderCost = config.IgnoreProviderCost
	if config.CustomProviderConfig != nil {
		providerConfig.CustomProviderConfig = config.CustomProviderConfig
	}
	if config.OpenAIConfig != nil {
		providerConfig.OpenAIConfig = config.OpenAIConfig
	}
	if config.PromptCache != nil {
		providerConfig.PromptCache = config.PromptCache
	}
	if config.InjectedTools != nil {
		providerConfig.InjectedTools = config.InjectedTools
	}
	return providerConfig, nil
}

// ProviderEgress returns the proxy and network config a provider's own calls leave the
// gateway with: GetConfigForProvider's resolution (the provider's proxy_config, else the
// inherited global proxy, and its network_config TLS settings). A provider that is not
// configured yet gets the default network config and the inherited global proxy, which is
// what it would get once added. Management-plane calls made on a provider's behalf (the
// GitHub Copilot sign-in) use this so they reach the provider's hosts the way the
// provider itself does, not through a second, differently configured stack.
func ProviderEgress(store *Config, provider schemas.ModelProvider) (*schemas.ProxyConfig, schemas.NetworkConfig) {
	if store == nil {
		return nil, schemas.DefaultNetworkConfig
	}
	if cfg, err := NewBaseAccount(store).GetConfigForProvider(provider); err == nil {
		return cfg.ProxyConfig, cfg.NetworkConfig
	}
	var proxy *schemas.ProxyConfig
	if inherited, ok := inheritGlobalProxy(nil, store.GetGlobalProxyConfig()); ok {
		proxy = inherited.proxy
	}
	return proxy, schemas.DefaultNetworkConfig
}

// ManagementEgress is ProviderEgress for a management-plane call made on a provider's behalf,
// which is API traffic to the gateway: when the provider's own resolution names no proxy, the
// global proxy applies if it is enabled for API requests, as it does for every other API-side
// call through network.DefaultTransport(network.ClientPurposeAPI).
func ManagementEgress(store *Config, provider schemas.ModelProvider) (*schemas.ProxyConfig, schemas.NetworkConfig) {
	proxy, networkConfig := ProviderEgress(store, provider)
	if hasOwnProxy(proxy) || store == nil {
		return proxy, networkConfig
	}
	if global := store.GetGlobalProxyConfig(); global != nil && global.EnableForAPI {
		if translated, ok := translateGlobalProxy(global); ok {
			return translated.proxy, networkConfig
		}
	}
	return proxy, networkConfig
}

// hasOwnProxy reports whether a provider's proxy_config names a proxy of its own.
// The UI saves type "none" when the proxy form is left untouched, so "none" and an
// empty type mean "not configured", the same as nil.
func hasOwnProxy(proxyConfig *schemas.ProxyConfig) bool {
	return proxyConfig != nil && proxyConfig.Type != "" && proxyConfig.Type != schemas.NoProxy
}

// inheritedProxy is the global proxy translated for one provider.
type inheritedProxy struct {
	proxy *schemas.ProxyConfig
}

// inheritGlobalProxy returns the global proxy for a provider that has none of its
// own, when the global proxy is enabled for inference. A provider's own proxy always
// wins. ok is false when nothing should be inherited.
//
// no_proxy carries over, so hosts such as a VPC endpoint stay direct. skip_tls_verify
// carries over as the provider's insecure_skip_verify, since that is what the global
// setting means for traffic tunnelled through a TLS-inspecting proxy. The global
// timeout does not: each provider keeps its own network_config timeouts.
func inheritGlobalProxy(own *schemas.ProxyConfig, global *configstoreTables.GlobalProxyConfig) (inheritedProxy, bool) {
	if hasOwnProxy(own) || global == nil || !global.EnableForInference {
		return inheritedProxy{}, false
	}
	return translateGlobalProxy(global)
}

// translateGlobalProxy turns an enabled global proxy into the provider-level form, whatever
// purpose the caller checked it for. ok is false when it is disabled, empty or has no
// provider-level equivalent.
func translateGlobalProxy(global *configstoreTables.GlobalProxyConfig) (inheritedProxy, bool) {
	if global == nil || !global.Enabled || strings.TrimSpace(global.URL) == "" {
		return inheritedProxy{}, false
	}
	var proxyType schemas.ProxyType
	switch global.Type {
	case network.GlobalProxyTypeHTTP, "":
		proxyType = schemas.HTTPProxy
	case network.GlobalProxyTypeSOCKS5:
		proxyType = schemas.Socks5Proxy
	default:
		// "tcp" has no provider-level equivalent (the UI does not offer it).
		return inheritedProxy{}, false
	}
	return inheritedProxy{
		proxy: &schemas.ProxyConfig{
			Type:     proxyType,
			URL:      plainSecret(global.URL),
			Username: plainSecret(global.Username),
			Password: plainSecret(global.Password),
			NoProxy:  global.NoProxy,
			// The only carrier of the global skip_tls_verify: the proxy stacks skip
			// verification for the TLS hop to an https:// proxy and for TLS through
			// the proxy, and still verify no_proxy hosts reached directly.
			// NetworkConfig is deliberately left alone.
			SkipTLSVerify: global.SkipTLSVerify,
		},
	}, true
}

// plainSecret wraps a literal value. The global proxy stores plain strings, so a
// password that happens to start with "env." must not be read as an env reference,
// which NewSecretVar would do.
func plainSecret(value string) *schemas.SecretVar {
	if value == "" {
		return nil
	}
	return &schemas.SecretVar{Val: value, SecretType: schemas.SecretTypePlainText}
}
