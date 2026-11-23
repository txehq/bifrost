package lib

import (
	"fmt"
	"sort"

	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/capsohq/bifrost/framework/configstore"
)

// unusableProviderKeys names every enabled key that key selection will never pick because it
// carries no credential, by core's own rule (bifrost.KeyHasCredential), checked once at load
// rather than on the request path. Core skips such a key without a log line, so a request only
// reports that no key supports the model. An env reference that resolved to nothing is caught
// too, named by its reference. No message carries a credential value.
//
// GitHub Copilot keys, and custom providers based on it, get their own message: GitHub App
// authentication was removed (upstream d9633b2fa, adopted in TXE-6085), so a key that held only
// App credentials now has no token. Its stored App columns are kept, unused
// (tables.TableKeyGithubCopilotApp).
func unusableProviderKeys(providers map[schemas.ModelProvider]configstore.ProviderConfig) []string {
	var problems []string
	for provider, cfg := range providers {
		base := provider
		if cfg.CustomProviderConfig != nil && cfg.CustomProviderConfig.BaseProviderType != "" {
			base = cfg.CustomProviderConfig.BaseProviderType
		}
		for i := range cfg.Keys {
			key := &cfg.Keys[i]
			if key.Enabled != nil && !*key.Enabled {
				continue
			}
			if bifrost.KeyHasCredential(base, key) {
				continue
			}
			name := key.Name
			if name == "" {
				name = key.ID
			}
			switch ref := key.Value.GetRawRef(); {
			case base == schemas.GithubCopilot:
				problems = append(problems, fmt.Sprintf("provider %s key %q has no token, so it is never used. GitHub App authentication and GitHub Enterprise domains are no longer supported; stored App credentials are kept but unused. Sign in with GitHub on the key, or set its value to a fine-grained personal access token with the Copilot Requests permission", provider, name))
			case key.Value.IsFromSecret() && ref != "":
				problems = append(problems, fmt.Sprintf("provider %s key %q resolved %s to an empty value, so it is never used", provider, name, ref))
			default:
				problems = append(problems, fmt.Sprintf("provider %s key %q has no value, so it is never used", provider, name))
			}
		}
	}
	sort.Strings(problems)
	return problems
}

// logUnusableProviderKeys reports unusableProviderKeys at error level, once per key.
func logUnusableProviderKeys(providers map[schemas.ModelProvider]configstore.ProviderConfig) {
	if logger == nil {
		return
	}
	for _, problem := range unusableProviderKeys(providers) {
		logger.Error("%s", problem)
	}
}
