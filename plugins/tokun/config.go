package tokun

import (
	"encoding/json"
	"fmt"

	"github.com/capsohq/bifrost/core/schemas"
)

// Config is the plugin's entry in Bifrost's plugin configuration:
//
//	{"name": "tokun", "enabled": true, "config": {"signing_keys": "env.TOKUN_ROUTE_PLAN_SIGNING_KEYS"}}
//
// The plugin is loaded only when such an entry exists and is enabled.
type Config struct {
	// SigningKeys holds the route plan signing keys as a JSON object of key id to secret,
	// {"gw-2026-11": "...", "tw-2026-11": "...", "hp-2026-11": "..."}: one key each for the
	// gateway, the task-worker and the health probe. It is a secret reference (env.<NAME> or
	// vault.<path>) resolved when the plugin is built, so the keys live in Bifrost's Secret.
	// A rotation edits only that Secret: the new id goes in beside the old one, both verify,
	// and the old one is removed once every signer has switched. The configuration keeps
	// naming the same reference, so the config store is never written. The HMAC key is the
	// UTF-8 bytes of a secret.
	SigningKeys *schemas.SecretVar `json:"signing_keys"`
}

// signingKeys resolves the configured keys by key id. Every id and every secret is non-empty.
func (c *Config) signingKeys() (map[string][]byte, error) {
	if c == nil || !c.SigningKeys.IsSet() {
		return nil, fmt.Errorf("tokun: signing_keys is not set")
	}
	raw := c.SigningKeys.GetValue()
	if raw == "" {
		return nil, fmt.Errorf("tokun: signing_keys %s resolves to an empty value", c.SigningKeys.GetRawRef())
	}
	var secrets map[string]string
	if err := json.Unmarshal([]byte(raw), &secrets); err != nil {
		return nil, fmt.Errorf("tokun: signing_keys is not a JSON object of key id to secret: %w", err)
	}
	if len(secrets) == 0 {
		return nil, fmt.Errorf("tokun: signing_keys names no key")
	}
	keys := make(map[string][]byte, len(secrets))
	for id, secret := range secrets {
		if id == "" {
			return nil, fmt.Errorf("tokun: signing_keys has a key with an empty id")
		}
		if secret == "" {
			return nil, fmt.Errorf("tokun: signing key %q has an empty secret", id)
		}
		keys[id] = []byte(secret)
	}
	return keys, nil
}

// storedConfig is Config as the config store keeps it: the signing keys as their reference
// (or literal), never the resolved secrets.
type storedConfig struct {
	SigningKeys string `json:"signing_keys"`
}

// redactedConfig is Config as the API shows it.
type redactedConfig struct {
	SigningKeys *schemas.SecretVar `json:"signing_keys"`
}

// MarshalConfigForStorage implements schemas.ConfigMarshallerPlugin: the stored config names
// the signing keys' reference, never their resolved value.
func (p *Plugin) MarshalConfigForStorage(raw map[string]any) (map[string]any, error) {
	config, err := decodeConfig(raw)
	if err != nil {
		return nil, err
	}
	return encodeConfig(storedConfig{SigningKeys: schemas.SecretVarAsString(config.SigningKeys)})
}

// RedactConfig implements schemas.ConfigMarshallerPlugin: no part of a secret is shown.
func (p *Plugin) RedactConfig(raw map[string]any) (map[string]any, error) {
	config, err := decodeConfig(raw)
	if err != nil {
		return nil, err
	}
	return encodeConfig(redactedConfig{SigningKeys: config.SigningKeys.FullyRedacted()})
}

func decodeConfig(raw map[string]any) (*Config, error) {
	data, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("tokun: encode plugin config: %w", err)
	}
	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("tokun: decode plugin config: %w", err)
	}
	return &config, nil
}

func encodeConfig(v any) (map[string]any, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("tokun: encode plugin config: %w", err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("tokun: decode plugin config: %w", err)
	}
	return out, nil
}
