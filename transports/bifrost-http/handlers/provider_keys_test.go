package handlers

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bytedance/sonic"
	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/capsohq/bifrost/framework/configstore"
	"github.com/capsohq/bifrost/transports/v2/bifrost-http/lib"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

func TestProviderKeyReadRedactsNestedSecrets(t *testing.T) {
	const canary = "synthetic-provider-key-secret-canary-0123456789"
	const ref = "env.BIFROST_PROVIDER_KEY_REDACTION_TEST"
	t.Setenv("BIFROST_PROVIDER_KEY_REDACTION_TEST", canary)
	secret := func() *schemas.SecretVar { return schemas.NewSecretVar(ref) }
	for _, provider := range []schemas.ModelProvider{schemas.Bedrock, schemas.BedrockMantle} {
		t.Run(string(provider), func(t *testing.T) {
			key := schemas.Key{
				ID: "redaction-key", Name: "redaction-key", Value: *secret(),
				Aliases: schemas.KeyAliases{"test": {ModelID: "model", Region: secret(), ProjectID: secret()}},
			}
			if provider == schemas.Bedrock {
				key.BedrockKeyConfig = &schemas.BedrockKeyConfig{
					ProjectID: secret(), Endpoints: &schemas.BedrockEndpoints{Runtime: secret()},
				}
			} else {
				key.BedrockMantleKeyConfig = &schemas.BedrockMantleKeyConfig{
					ProjectID: secret(), Endpoints: &schemas.BedrockEndpoints{Runtime: secret()},
				}
			}
			h := &ProviderHandler{inMemoryStore: &lib.Config{
				Providers: map[schemas.ModelProvider]configstore.ProviderConfig{
					provider: {Keys: []schemas.Key{key}},
				},
			}}
			for name, handler := range map[string]func(*fasthttp.RequestCtx){
				"get": h.getProviderKey, "list": h.listProviderKeys,
			} {
				t.Run(name, func(t *testing.T) {
					ctx := newTestRequestCtx("")
					ctx.SetUserValue("provider", string(provider))
					ctx.SetUserValue("key_id", key.ID)
					handler(ctx)
					body := string(ctx.Response.Body())
					if ctx.Response.StatusCode() != fasthttp.StatusOK {
						t.Fatalf("expected 200, got %d: %s", ctx.Response.StatusCode(), body)
					}
					if strings.Contains(body, canary) {
						t.Fatalf("resolved secret leaked: %s", body)
					}
					// All five populated fields (including the credential control)
					// must be present, resolved, masked, and retain their references.
					if strings.Count(body, ref) != 5 || strings.Count(body, secret().Redacted().GetValue()) != 5 {
						t.Fatalf("missing masked values or references: %s", body)
					}
					if key.Aliases["test"].Region.GetValue() != canary {
						t.Fatal("reading a key modified the live secret")
					}
				})
			}
		})
	}
}

// TestMergeUpdatedKey_Value locks in the invariant that a masked key preview can
// never be persisted as the real key value. The provider keys API renders keys
// redacted on GET; when a client echoes that placeholder back on update, the
// stored credential must be preserved. This is the write-side guard for
// issue #4353 (a masked "*"-laden preview leaking into the config store and
// later breaking JSON re-parsing on governance reload).
func TestMergeUpdatedKey_Value(t *testing.T) {
	h := &ProviderHandler{}
	merge := func(oldRaw, update schemas.Key) schemas.Key {
		t.Helper()
		merged, err := h.mergeUpdatedKey(oldRaw, update)
		if err != nil {
			t.Fatalf("mergeUpdatedKey returned error: %v", err)
		}
		return merged
	}

	const rawValue = "sk-realkey1234567890abcdefghij"

	newRaw := func() schemas.Key {
		return schemas.Key{ID: "key-1", Value: *schemas.NewSecretVar(rawValue)}
	}
	redactedOf := func(raw schemas.Key) schemas.Key {
		return schemas.Key{ID: "key-1", Value: *raw.Value.Redacted()}
	}

	t.Run("echoed current redaction preserves stored value", func(t *testing.T) {
		oldRaw := newRaw()
		oldRedacted := redactedOf(oldRaw)
		// Client sends back exactly what GET rendered.
		update := schemas.Key{ID: "key-1", Value: oldRedacted.Value}

		merged := merge(oldRaw, update)
		if merged.Value.GetValue() != rawValue {
			t.Fatalf("expected stored raw value preserved, got %q", merged.Value.GetValue())
		}
	})

	t.Run("mismatched mask still preserves stored value", func(t *testing.T) {
		// A redacted preview whose bytes differ from the server's current
		// redaction (e.g. a stale render, a different asterisk count, or a
		// preview from another replica). The old exact-match guard let this
		// through and persisted the mask; the fix must still preserve.
		oldRaw := newRaw()
		oldRedacted := redactedOf(oldRaw)
		mismatched := "diff" + strings.Repeat("*", 24) + "XYZW" // redacted-shaped, != oldRedacted
		if !schemas.NewSecretVar(mismatched).IsRedacted() {
			t.Fatalf("test setup: %q is not recognized as redacted", mismatched)
		}
		if mismatched == oldRedacted.Value.GetValue() {
			t.Fatalf("test setup: mismatched mask unexpectedly equals current redaction")
		}
		update := schemas.Key{ID: "key-1", Value: *schemas.NewSecretVar(mismatched)}

		merged := merge(oldRaw, update)
		if merged.Value.GetValue() != rawValue {
			t.Fatalf("masked preview must not be persisted; expected %q, got %q", rawValue, merged.Value.GetValue())
		}
		if strings.Contains(merged.Value.GetValue(), "*") {
			t.Fatalf("merged value still contains mask characters: %q", merged.Value.GetValue())
		}
	})

	t.Run("genuine new plaintext value is applied", func(t *testing.T) {
		oldRaw := newRaw()
		const newValue = "sk-brandnewkey0987654321zyxwvu"
		update := schemas.Key{ID: "key-1", Value: *schemas.NewSecretVar(newValue)}

		merged := merge(oldRaw, update)
		if merged.Value.GetValue() != newValue {
			t.Fatalf("expected new plaintext value applied, got %q", merged.Value.GetValue())
		}
	})

	t.Run("genuine env ref is applied not preserved", func(t *testing.T) {
		oldRaw := newRaw()
		// env refs report IsRedacted() but are an intentional change.
		update := schemas.Key{ID: "key-1", Value: *schemas.NewSecretVar("env.SOME_NEW_KEY")}

		merged := merge(oldRaw, update)
		if !merged.Value.IsFromEnv() || merged.Value.GetRawRef() != "env.SOME_NEW_KEY" {
			t.Fatalf("expected env ref applied, got ref=%q fromEnv=%v", merged.Value.GetRawRef(), merged.Value.IsFromEnv())
		}
		if merged.Value.GetValue() == rawValue {
			t.Fatalf("stored raw value leaked into an env-ref update")
		}
	})

	t.Run("empty value is not treated as redacted", func(t *testing.T) {
		// Empty non-secret values must stay empty so the downstream
		// "must not be empty" validation still fires. The merge must not
		// silently resurrect the stored value here.
		oldRaw := newRaw()
		update := schemas.Key{ID: "key-1", Value: *schemas.NewSecretVar("")}

		merged := merge(oldRaw, update)
		if merged.Value.GetValue() != "" {
			t.Fatalf("expected empty value preserved for validation, got %q", merged.Value.GetValue())
		}
	})
}

// TestMergeUpdatedKey_Name locks in the invariant that a PUT update omitting
// name must not clear the stored one. config_keys.name carries a global unique
// index, so silently clearing it lets the first such update claim "" and wedge
// every later name-omitting update behind a 409 (observed in a downstream
// production deployment: a single edit that omitted name broke every
// subsequent key edit going through this path).
func TestMergeUpdatedKey_Name(t *testing.T) {
	h := &ProviderHandler{}
	merge := func(oldRaw, update schemas.Key) schemas.Key {
		t.Helper()
		merged, err := h.mergeUpdatedKey(oldRaw, update)
		if err != nil {
			t.Fatalf("mergeUpdatedKey returned error: %v", err)
		}
		return merged
	}

	t.Run("name omitted from update preserves stored name", func(t *testing.T) {
		oldRaw := schemas.Key{ID: "key-1", Name: "prod-openai-key", Value: *schemas.NewSecretVar("sk-realkey1234567890abcdefghij")}
		update := schemas.Key{ID: "key-1", Value: oldRaw.Value} // client PUT body has no "name" field

		merged := merge(oldRaw, update)
		if merged.Name != "prod-openai-key" {
			t.Fatalf("expected stored name preserved, got %q", merged.Name)
		}
	})

	t.Run("explicit new name is applied", func(t *testing.T) {
		oldRaw := schemas.Key{ID: "key-1", Name: "prod-openai-key", Value: *schemas.NewSecretVar("sk-realkey1234567890abcdefghij")}
		update := schemas.Key{ID: "key-1", Name: "renamed-key", Value: oldRaw.Value}

		merged := merge(oldRaw, update)
		if merged.Name != "renamed-key" {
			t.Fatalf("expected new name applied, got %q", merged.Name)
		}
	})
}

// TestDecodeKeyUpdate_PartialBody covers issue #7572: a PUT that names only
// some fields must leave the others as stored. Omitted fields used to decode to
// zero values, so a body carrying only models emptied the key's value and set
// its weight to 0, and the update still returned 200.
func TestDecodeKeyUpdate_PartialBody(t *testing.T) {
	h := &ProviderHandler{}
	const rawValue = "sk-sglrealkey1234567890abcdefghij"
	stored := func() schemas.Key {
		return schemas.Key{
			ID:                "key-1",
			Name:              "sgl-key",
			Value:             *schemas.NewSecretVar(rawValue),
			Models:            schemas.WhiteList{"old-model"},
			BlacklistedModels: schemas.BlackList{"blocked-model"},
			Weight:            1,
			SGLKeyConfig:      &schemas.SGLKeyConfig{URL: *schemas.NewSecretVar("http://old:30000")},
			Enabled:           schemas.Ptr(false),
			UseForBatchAPI:    schemas.Ptr(true),
			Description:       "primary",
		}
	}
	update := func(t *testing.T, body string) schemas.Key {
		t.Helper()
		oldRaw := stored()
		updateKey, err := decodeKeyUpdate([]byte(body), oldRaw)
		if err != nil {
			t.Fatalf("decodeKeyUpdate returned error: %v", err)
		}
		updateKey.ID = oldRaw.ID
		merged, err := h.mergeUpdatedKey(oldRaw, updateKey)
		if err != nil {
			t.Fatalf("mergeUpdatedKey returned error: %v", err)
		}
		return merged
	}

	t.Run("omitted fields keep their stored values", func(t *testing.T) {
		merged := update(t, `{"models":["m1","m2"],"sgl_key_config":{"url":"http://new:30000"}}`)
		if merged.Value.GetValue() != rawValue {
			t.Fatalf("value: got %q, want the stored value", merged.Value.GetValue())
		}
		if merged.Weight != 1 {
			t.Fatalf("weight: got %v, want 1", merged.Weight)
		}
		if len(merged.BlacklistedModels) != 1 || merged.BlacklistedModels[0] != "blocked-model" {
			t.Fatalf("blacklisted_models: got %v, want [blocked-model]", merged.BlacklistedModels)
		}
		if merged.Enabled == nil || *merged.Enabled {
			t.Fatalf("enabled: got %v, want the stored false", merged.Enabled)
		}
		if merged.UseForBatchAPI == nil || !*merged.UseForBatchAPI {
			t.Fatalf("use_for_batch_api: got %v, want the stored true", merged.UseForBatchAPI)
		}
		if merged.Description != "primary" {
			t.Fatalf("description: got %q, want primary", merged.Description)
		}
		// The fields that were sent still apply.
		if len(merged.Models) != 2 || merged.Models[0] != "m1" {
			t.Fatalf("models: got %v, want [m1 m2]", merged.Models)
		}
		if merged.SGLKeyConfig == nil || merged.SGLKeyConfig.URL.GetValue() != "http://new:30000" {
			t.Fatalf("sgl_key_config.url: got %+v, want http://new:30000", merged.SGLKeyConfig)
		}
	})

	t.Run("an omitted provider config is kept", func(t *testing.T) {
		merged := update(t, `{"weight":2}`)
		if merged.SGLKeyConfig == nil || merged.SGLKeyConfig.URL.GetValue() != "http://old:30000" {
			t.Fatalf("sgl_key_config: got %+v, want the stored config", merged.SGLKeyConfig)
		}
		if merged.Weight != 2 {
			t.Fatalf("weight: got %v, want 2", merged.Weight)
		}
	})

	t.Run("fields sent explicitly empty are cleared", func(t *testing.T) {
		merged := update(t, `{"value":"","weight":0,"models":[],"description":"","sgl_key_config":null}`)
		if merged.Value.GetValue() != "" {
			t.Fatalf("value: got %q, want it cleared", merged.Value.GetValue())
		}
		if merged.Weight != 0 {
			t.Fatalf("weight: got %v, want 0", merged.Weight)
		}
		if len(merged.Models) != 0 {
			t.Fatalf("models: got %v, want empty", merged.Models)
		}
		if merged.Description != "" {
			t.Fatalf("description: got %q, want it cleared", merged.Description)
		}
		if merged.SGLKeyConfig != nil {
			t.Fatalf("sgl_key_config: got %+v, want nil", merged.SGLKeyConfig)
		}
	})

	// The decoder matches field names case-insensitively, so presence must too: a field
	// it decoded must not then be replaced by the stored value.
	t.Run("case-variant field names apply", func(t *testing.T) {
		merged := update(t, `{"Value":"sk-rotated-value-0123456789","Enabled":true,"WEIGHT":3}`)
		if merged.Value.GetValue() != "sk-rotated-value-0123456789" {
			t.Fatalf("value: got %q, want the rotated value", merged.Value.GetValue())
		}
		if merged.Enabled == nil || !*merged.Enabled {
			t.Fatalf("enabled: got %v, want true", merged.Enabled)
		}
		if merged.Weight != 3 {
			t.Fatalf("weight: got %v, want 3", merged.Weight)
		}
		if merged.Description != "primary" {
			t.Fatalf("description: got %q, want the stored value", merged.Description)
		}
	})

	t.Run("a masked value under a case-variant name keeps the stored value", func(t *testing.T) {
		masked := schemas.NewSecretVar(rawValue).Redacted().GetValue()
		merged := update(t, `{"VALUE":"`+masked+`","WEIGHT":3}`)
		if merged.Value.GetValue() != rawValue {
			t.Fatalf("value: got %q, want the stored value restored", merged.Value.GetValue())
		}
		if merged.Weight != 3 {
			t.Fatalf("weight: got %v, want 3", merged.Weight)
		}
	})
}

// keyUpdateFieldSamples holds a JSON value for every field a key update patches, each
// distinct from a zero stored key. keyUpdateFixedFields are the Key fields decodeKeyUpdate
// does not patch.
var (
	keyUpdateFieldSamples = map[string]string{
		"value":                     `"sk-sample-value-0123456789"`,
		"models":                    `["sample-model"]`,
		"blacklisted_models":        `["sample-blocked"]`,
		"weight":                    `7`,
		"aliases":                   `{"sample":"alias"}`,
		"azure_key_config":          `{}`,
		"vertex_key_config":         `{}`,
		"bedrock_key_config":        `{}`,
		"bedrock_mantle_key_config": `{}`,
		"vllm_key_config":           `{}`,
		"replicate_key_config":      `{}`,
		"ollama_key_config":         `{}`,
		"sgl_key_config":            `{}`,
		"databricks_key_config":     `{}`,
		"github_copilot_key_config": `{}`,
		"cloudflare_key_config":     `{}`,
		"oauth_key_config":          `{}`,
		"enabled":                   `false`,
		"use_for_batch_api":         `false`,
		"use_anthropic_endpoints":   `false`,
		"use_openai_endpoints":      `false`,
		"description":               `"sample description"`,
	}
	keyUpdateFixedFields = []string{"id", "name", "config_hash", "status"}
)

// TestDecodeKeyUpdate_FieldNamesFoldAsTheDecoderDoes compares every spelling with the exact
// name through the real decoder, so a change in how sonic folds names fails here rather than
// silently splitting presence from what was decoded: a spelling the decoder takes applies,
// and one it ignores keeps the stored value.
func TestDecodeKeyUpdate_FieldNamesFoldAsTheDecoderDoes(t *testing.T) {
	keyType := reflect.TypeFor[schemas.Key]()
	for i := range keyType.NumField() {
		name, _, _ := strings.Cut(keyType.Field(i).Tag.Get("json"), ",")
		if _, patched := keyUpdateFieldSamples[name]; !patched && !slices.Contains(keyUpdateFixedFields, name) {
			t.Fatalf("schemas.Key field %q is not classified: add it to the patch list or keyUpdateFixedFields", name)
		}
	}

	stored := schemas.Key{ID: "key-1", Name: "stored", Weight: 1}
	decode := func(t *testing.T, body string) schemas.Key {
		t.Helper()
		key, err := decodeKeyUpdate([]byte(body), stored)
		if err != nil {
			t.Fatalf("decodeKeyUpdate(%s): %v", body, err)
		}
		return key
	}
	untouched := decode(t, `{}`)

	const kelvin, dottedI = "\u212a", "\u0130"
	for field, sample := range keyUpdateFieldSamples {
		t.Run(field, func(t *testing.T) {
			applied := decode(t, `{"`+field+`":`+sample+`}`)
			if reflect.DeepEqual(applied, untouched) {
				t.Fatalf("the exact name did not apply; the sample proves nothing")
			}
			spellings := []string{
				strings.ToUpper(field),
				strings.ToUpper(field[:1]) + field[1:],
				fmt.Sprintf("\\u%04x", field[0]) + field[1:], // an escaped first letter
			}
			// Unicode folds that strings.ToLower maps onto a field name's ASCII letters.
			if strings.Contains(field, "k") {
				spellings = append(spellings, strings.Replace(field, "k", kelvin, 1))
			}
			if strings.Contains(field, "i") {
				spellings = append(spellings, strings.Replace(field, "i", dottedI, 1))
			}
			for _, spelling := range spellings {
				if got := decode(t, `{"`+spelling+`":`+sample+`}`); !reflect.DeepEqual(got, applied) {
					t.Fatalf("spelling %s: got %+v, want the exact name's result %+v", spelling, got, applied)
				}
			}
			// Letters that fold only in the other direction, or not to a letter here,
			// are unknown keys to the decoder: the stored value stays.
			if strings.Contains(field, "s") {
				ignored := strings.Replace(field, "s", "\u017f", 1) // long s lowers to itself
				if got := decode(t, `{"`+ignored+`":`+sample+`}`); !reflect.DeepEqual(got, untouched) {
					t.Fatalf("spelling %s: got %+v, want the stored key", ignored, got)
				}
			}
		})
	}
}

// TestDecodeKeyUpdate_RepeatedSpellingsFollowTheDecoder: a body naming one field in several
// spellings is ambiguous, and whatever the decoder makes of it is what must be kept, so the
// guards that run on the decoded key judge what is stored.
func TestDecodeKeyUpdate_RepeatedSpellingsFollowTheDecoder(t *testing.T) {
	stored := schemas.Key{ID: "key-1", Name: "stored", Value: *schemas.NewSecretVar("sk-stored-literal"), Weight: 1, Description: "stored"}
	for _, body := range []string{
		`{"value":"sk-first-0123456789","VALUE":"sk-second-0123456789"}`,
		`{"VALUE":"sk-first-0123456789","value":"sk-second-0123456789"}`,
		`{"weight":3,"WEIGHT":4}`,
		`{"WEIGHT":4,"weight":3}`,
		`{"weight":3,"Weight":null}`,
		`{"Weight":null,"weight":3}`,
		`{"description":"one","DESCRIPTION":"","Description":"three"}`,
		`{"ollama_key_config":{"url":"http://first:11434"},"OLLAMA_KEY_CONFIG":null}`,
		`{"OLLAMA_KEY_CONFIG":null,"ollama_key_config":{"url":"http://second:11434"}}`,
	} {
		t.Run(body, func(t *testing.T) {
			var decoded schemas.Key
			if err := sonic.Unmarshal([]byte(body), &decoded); err != nil {
				t.Fatalf("decode: %v", err)
			}
			got, err := decodeKeyUpdate([]byte(body), stored)
			if err != nil {
				t.Fatalf("decodeKeyUpdate: %v", err)
			}
			if strings.Contains(strings.ToLower(body), `"value"`) && got.Value.GetValue() != decoded.Value.GetValue() {
				t.Fatalf("value: got %q, the decoder took %q", got.Value.GetValue(), decoded.Value.GetValue())
			}
			if strings.Contains(strings.ToLower(body), `"weight"`) && got.Weight != decoded.Weight {
				t.Fatalf("weight: got %v, the decoder took %v", got.Weight, decoded.Weight)
			}
			if strings.Contains(strings.ToLower(body), `"description"`) && got.Description != decoded.Description {
				t.Fatalf("description: got %q, the decoder took %q", got.Description, decoded.Description)
			}
			if strings.Contains(strings.ToLower(body), "ollama_key_config") && !reflect.DeepEqual(got.OllamaKeyConfig, decoded.OllamaKeyConfig) {
				t.Fatalf("ollama_key_config: got %+v, the decoder took %+v", got.OllamaKeyConfig, decoded.OllamaKeyConfig)
			}
		})
	}
}

func BenchmarkDecodeKeyUpdate(b *testing.B) {
	stored := schemas.Key{ID: "key-1", Name: "stored", Value: *schemas.NewSecretVar("sk-stored-literal"), Weight: 1, Description: "stored"}
	body := []byte(`{"name":"stored","models":["gpt-5"],"weight":2,"description":"edited","enabled":true}`)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := decodeKeyUpdate(body, stored); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRejectUnknownOAuthKeyConfigFields(b *testing.B) {
	body := []byte(`{"name":"stored","models":["gpt-5"],"weight":2,"description":"edited","enabled":true}`)
	b.ReportAllocs()
	for b.Loop() {
		if err := rejectUnknownOAuthKeyConfigFields(body); err != nil {
			b.Fatal(err)
		}
	}
}

func TestMergeUpdatedKey_ProviderConfigMaskedPreviews(t *testing.T) {
	h := &ProviderHandler{}
	merge := func(oldRaw, update schemas.Key) schemas.Key {
		t.Helper()
		merged, err := h.mergeUpdatedKey(oldRaw, update)
		if err != nil {
			t.Fatalf("mergeUpdatedKey returned error: %v", err)
		}
		return merged
	}
	secret := func(value string) schemas.SecretVar { return *schemas.NewSecretVar(value) }
	secretPtr := func(value string) *schemas.SecretVar { return schemas.NewSecretVar(value) }
	staleMaskValue := func(prefix, suffix string) string {
		return prefix + strings.Repeat("*", 24) + suffix
	}
	staleMask := func(prefix, suffix string) schemas.SecretVar {
		return secret(staleMaskValue(prefix, suffix))
	}

	oldRaw := schemas.Key{
		AzureKeyConfig: &schemas.AzureKeyConfig{
			Endpoint:     secret("https://current.azure.example.com"),
			ClientSecret: secretPtr("azure-client-secret-current"),
		},
		VertexKeyConfig: &schemas.VertexKeyConfig{
			AuthCredentials: secret("vertex-auth-credentials-current"),
		},
		BedrockKeyConfig: &schemas.BedrockKeyConfig{
			AccessKey:    secret("bedrock-access-key-current"),
			SessionToken: secretPtr("bedrock-session-token-current"),
		},
		BedrockMantleKeyConfig: &schemas.BedrockMantleKeyConfig{
			SecretKey: secret("mantle-secret-key-current"),
		},
		VLLMKeyConfig:       &schemas.VLLMKeyConfig{URL: secret("https://current.vllm.example.com")},
		OllamaKeyConfig:     &schemas.OllamaKeyConfig{URL: secret("https://current.ollama.example.com")},
		SGLKeyConfig:        &schemas.SGLKeyConfig{URL: secret("https://current.sgl.example.com")},
		CloudflareKeyConfig: &schemas.CloudflareKeyConfig{AccountID: secret("0123456789abcdef0123456789abcdef")},
	}
	update := schemas.Key{
		AzureKeyConfig: &schemas.AzureKeyConfig{
			Endpoint:     staleMask("azur", "0001"),
			ClientSecret: secretPtr(staleMaskValue("azcs", "0002")),
		},
		VertexKeyConfig: &schemas.VertexKeyConfig{
			AuthCredentials: staleMask("vert", "0003"),
		},
		BedrockKeyConfig: &schemas.BedrockKeyConfig{
			AccessKey:    staleMask("beda", "0004"),
			SessionToken: secretPtr(staleMaskValue("beds", "0005")),
		},
		BedrockMantleKeyConfig: &schemas.BedrockMantleKeyConfig{
			SecretKey: staleMask("mant", "0006"),
		},
		VLLMKeyConfig:       &schemas.VLLMKeyConfig{URL: staleMask("vllm", "0007")},
		OllamaKeyConfig:     &schemas.OllamaKeyConfig{URL: staleMask("olla", "0008")},
		SGLKeyConfig:        &schemas.SGLKeyConfig{URL: staleMask("sgla", "0009")},
		CloudflareKeyConfig: &schemas.CloudflareKeyConfig{AccountID: staleMask("0123", "cdef")},
	}

	merged := merge(oldRaw, update)
	checks := []struct {
		name string
		got  string
		want string
	}{
		{"azure endpoint", merged.AzureKeyConfig.Endpoint.GetValue(), oldRaw.AzureKeyConfig.Endpoint.GetValue()},
		{"azure client secret", merged.AzureKeyConfig.ClientSecret.GetValue(), oldRaw.AzureKeyConfig.ClientSecret.GetValue()},
		{"vertex credentials", merged.VertexKeyConfig.AuthCredentials.GetValue(), oldRaw.VertexKeyConfig.AuthCredentials.GetValue()},
		{"bedrock access key", merged.BedrockKeyConfig.AccessKey.GetValue(), oldRaw.BedrockKeyConfig.AccessKey.GetValue()},
		{"bedrock session token", merged.BedrockKeyConfig.SessionToken.GetValue(), oldRaw.BedrockKeyConfig.SessionToken.GetValue()},
		{"mantle secret key", merged.BedrockMantleKeyConfig.SecretKey.GetValue(), oldRaw.BedrockMantleKeyConfig.SecretKey.GetValue()},
		{"vllm url", merged.VLLMKeyConfig.URL.GetValue(), oldRaw.VLLMKeyConfig.URL.GetValue()},
		{"ollama url", merged.OllamaKeyConfig.URL.GetValue(), oldRaw.OllamaKeyConfig.URL.GetValue()},
		{"sgl url", merged.SGLKeyConfig.URL.GetValue(), oldRaw.SGLKeyConfig.URL.GetValue()},
		{"cloudflare account id", merged.CloudflareKeyConfig.AccountID.GetValue(), oldRaw.CloudflareKeyConfig.AccountID.GetValue()},
	}
	for _, check := range checks {
		if check.got != check.want {
			t.Errorf("%s: expected stored value %q, got %q", check.name, check.want, check.got)
		}
	}

	update.VLLMKeyConfig.URL = secret("env.NEW_VLLM_URL")
	merged = merge(oldRaw, update)
	if !merged.VLLMKeyConfig.URL.IsFromEnv() || merged.VLLMKeyConfig.URL.GetRawRef() != "env.NEW_VLLM_URL" {
		t.Fatalf("expected nested env ref applied, got ref=%q", merged.VLLMKeyConfig.URL.GetRawRef())
	}
}

// TestMergeUpdatedKey_GithubCopilotKeyConfig covers the masked-update path for the GitHub
// App bundle. The API renders keys redacted on GET, so a client that edits one field and
// submits the form sends back a mask for every field it did not touch. Getting the merge
// wrong here either persists the literal mask as a credential, or silently drops the four
// App fields, and both failures look like a working save.
func TestMergeUpdatedKey_GithubCopilotKeyConfig(t *testing.T) {
	h := &ProviderHandler{}
	secret := func(v string) schemas.SecretVar { return *schemas.NewSecretVar(v) }
	mask := func(prefix, suffix string) schemas.SecretVar {
		return secret(prefix + strings.Repeat("*", 24) + suffix)
	}

	stored := func() schemas.Key {
		return schemas.Key{GithubCopilotKeyConfig: &schemas.GithubCopilotKeyConfig{
			AppID:          secret("123456"),
			InstallationID: secret("87654321"),
			RepositoryID:   secret("999000111"),
			PrivateKey:     secret(pkcs1TestPEM(t)),
			GithubDomain:   secret("acme.ghe.com"),
		}}
	}

	t.Run("masks resolve back to the stored values", func(t *testing.T) {
		update := schemas.Key{GithubCopilotKeyConfig: &schemas.GithubCopilotKeyConfig{
			AppID:          mask("1234", "3456"),
			InstallationID: mask("8765", "4321"),
			RepositoryID:   mask("9990", "0111"),
			PrivateKey:     mask("----", "----"),
			GithubDomain:   mask("acme", ".com"),
		}}

		merged, err := h.mergeUpdatedKey(stored(), update)
		if err != nil {
			t.Fatalf("mergeUpdatedKey returned error: %v", err)
		}
		cfg := merged.GithubCopilotKeyConfig
		if cfg == nil {
			t.Fatal("the App config was dropped by the merge")
		}
		for _, c := range []struct{ name, got, want string }{
			{"app_id", cfg.AppID.GetValue(), "123456"},
			{"installation_id", cfg.InstallationID.GetValue(), "87654321"},
			{"repository_id", cfg.RepositoryID.GetValue(), "999000111"},
			{"private_key", cfg.PrivateKey.GetValue(), pkcs1TestPEM(t)},
			{"github_domain", cfg.GithubDomain.GetValue(), "acme.ghe.com"},
		} {
			if c.got != c.want {
				t.Errorf("%s: got %q, want the stored value %q", c.name, c.got, c.want)
			}
			if strings.Contains(c.got, "****") {
				t.Errorf("%s: the mask was persisted as a credential", c.name)
			}
		}
	})

	t.Run("a mask with no stored counterpart is rejected", func(t *testing.T) {
		// Nothing to restore from, so accepting it would write asterisks as the App ID.
		update := schemas.Key{GithubCopilotKeyConfig: &schemas.GithubCopilotKeyConfig{
			AppID: mask("1234", "3456"),
		}}

		if _, err := h.mergeUpdatedKey(schemas.Key{}, update); err == nil {
			t.Fatal("expected an error when a mask has no stored value behind it")
		}
	})

	t.Run("unmasked literals overwrite the stored values", func(t *testing.T) {
		update := schemas.Key{GithubCopilotKeyConfig: &schemas.GithubCopilotKeyConfig{
			AppID:          secret("Iv1.b507a08c87ecfe98"),
			InstallationID: secret("11112222"),
			RepositoryID:   secret("333344445"),
			PrivateKey:     secret(pkcs8TestPEM(t)),
			GithubDomain:   secret(""),
		}}

		merged, err := h.mergeUpdatedKey(stored(), update)
		if err != nil {
			t.Fatalf("mergeUpdatedKey returned error: %v", err)
		}
		cfg := merged.GithubCopilotKeyConfig
		if cfg.AppID.GetValue() != "Iv1.b507a08c87ecfe98" {
			t.Errorf("app_id: got %q, want the new literal", cfg.AppID.GetValue())
		}
		if cfg.InstallationID.GetValue() != "11112222" {
			t.Errorf("installation_id: got %q, want the new literal", cfg.InstallationID.GetValue())
		}
		if cfg.PrivateKey.GetValue() != pkcs8TestPEM(t) {
			t.Error("private_key: the rotated key did not take effect")
		}
	})
}

func TestMergeUpdatedKey_RejectsMaskWithoutStoredCounterpart(t *testing.T) {
	h := &ProviderHandler{}
	mask := *schemas.NewSecretVar("abcd" + strings.Repeat("*", 24) + "wxyz")

	tests := []struct {
		name    string
		oldRaw  schemas.Key
		update  schemas.Key
		wantErr string
	}{
		{
			name:    "missing config section",
			oldRaw:  schemas.Key{},
			update:  schemas.Key{VLLMKeyConfig: &schemas.VLLMKeyConfig{URL: mask}},
			wantErr: "vllm_key_config.url",
		},
		{
			name: "missing optional field",
			oldRaw: schemas.Key{BedrockKeyConfig: &schemas.BedrockKeyConfig{
				AccessKey: *schemas.NewSecretVar("stored-access-key"),
			}},
			update: schemas.Key{BedrockKeyConfig: &schemas.BedrockKeyConfig{
				SessionToken: schemas.NewSecretVar(mask.GetValue()),
			}},
			wantErr: "bedrock_key_config.session_token",
		},
		{
			name: "empty stored value",
			oldRaw: schemas.Key{VLLMKeyConfig: &schemas.VLLMKeyConfig{
				URL: *schemas.NewSecretVar(""),
			}},
			update:  schemas.Key{VLLMKeyConfig: &schemas.VLLMKeyConfig{URL: mask}},
			wantErr: "vllm_key_config.url",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := h.mergeUpdatedKey(tt.oldRaw, tt.update)
			if err == nil {
				t.Fatal("expected masked preview without stored counterpart to fail")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error to name %q, got %q", tt.wantErr, err)
			}
		})
	}
}

// validateProviderKeyURL must enforce every nested field config.schema.json
// marks as required, so neither create nor a masked-update merge can persist a
// key missing them.
func TestValidateProviderKeyRequiredNestedFields(t *testing.T) {
	region := schemas.NewSecretVar("us-east-1")
	cases := []struct {
		name     string
		provider schemas.ModelProvider
		key      schemas.Key
		wantErr  string
	}{
		{"azure missing endpoint", schemas.Azure, schemas.Key{AzureKeyConfig: &schemas.AzureKeyConfig{}}, "azure_key_config.endpoint"},
		{"azure nil config", schemas.Azure, schemas.Key{}, "azure_key_config.endpoint"},
		{"azure ok", schemas.Azure, schemas.Key{AzureKeyConfig: &schemas.AzureKeyConfig{Endpoint: *schemas.NewSecretVar("https://x.openai.azure.com")}}, ""},
		{"bedrock missing region", schemas.Bedrock, schemas.Key{BedrockKeyConfig: &schemas.BedrockKeyConfig{}}, "bedrock_key_config.region"},
		{"bedrock ok", schemas.Bedrock, schemas.Key{BedrockKeyConfig: &schemas.BedrockKeyConfig{Region: region}}, ""},
		{"mantle missing region", schemas.BedrockMantle, schemas.Key{BedrockMantleKeyConfig: &schemas.BedrockMantleKeyConfig{}}, "bedrock_mantle_key_config.region"},
		{"mantle ok", schemas.BedrockMantle, schemas.Key{BedrockMantleKeyConfig: &schemas.BedrockMantleKeyConfig{Region: region}}, ""},
		{"vllm missing url", schemas.VLLM, schemas.Key{VLLMKeyConfig: &schemas.VLLMKeyConfig{ModelName: "m"}}, "vllm_key_config.url"},
		{"vllm missing model_name", schemas.VLLM, schemas.Key{VLLMKeyConfig: &schemas.VLLMKeyConfig{URL: *schemas.NewSecretVar("http://vllm:8000")}}, "vllm_key_config.model_name"},
		{"vllm ok", schemas.VLLM, schemas.Key{VLLMKeyConfig: &schemas.VLLMKeyConfig{URL: *schemas.NewSecretVar("http://vllm:8000"), ModelName: "m"}}, ""},
		{"openai unaffected", schemas.OpenAI, schemas.Key{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateProviderKeyURL(tc.provider, tc.key)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected no error, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

// TestMergeUpdatedKey_OAuthKeyConfig covers the masked-update path for oauth_key_config: a
// client that edits one field sends back a mask for every secret it did not touch, and the
// merge must restore the stored secret rather than persist the asterisks.
func TestMergeUpdatedKey_OAuthKeyConfig(t *testing.T) {
	h := &ProviderHandler{}
	secret := func(v string) *schemas.SecretVar { return schemas.NewSecretVar(v) }
	mask := func(prefix, suffix string) *schemas.SecretVar {
		return secret(prefix + strings.Repeat("*", 24) + suffix)
	}
	stored := func() schemas.Key {
		return schemas.Key{OAuthKeyConfig: &schemas.OAuthKeyConfig{
			GrantType:    schemas.OAuthGrantJWTBearer,
			TokenURL:     *secret("https://idp.example/oauth2/token"),
			ClientID:     secret("client-123"),
			ClientSecret: secret("client-secret-456"),
			PrivateKey:   secret(pkcs1TestPEM(t)),
			Issuer:       "client-123",
			Audience:     "api://acme",
		}}
	}

	t.Run("masks resolve back to the stored values", func(t *testing.T) {
		update := schemas.Key{OAuthKeyConfig: &schemas.OAuthKeyConfig{
			GrantType:    schemas.OAuthGrantJWTBearer,
			TokenURL:     *mask("http", "oken"),
			ClientID:     mask("clie", "-123"),
			ClientSecret: mask("clie", "-456"),
			PrivateKey:   mask("----", "----"),
			Issuer:       "client-123",
			Audience:     "api://other",
		}}
		merged, err := h.mergeUpdatedKey(stored(), update)
		if err != nil {
			t.Fatalf("mergeUpdatedKey returned error: %v", err)
		}
		cfg := merged.OAuthKeyConfig
		if cfg == nil {
			t.Fatal("the oauth config was dropped by the merge")
		}
		for _, c := range []struct{ name, got, want string }{
			{"token_url", cfg.TokenURL.GetValue(), "https://idp.example/oauth2/token"},
			{"client_id", cfg.ClientID.GetValue(), "client-123"},
			{"client_secret", cfg.ClientSecret.GetValue(), "client-secret-456"},
			{"private_key", cfg.PrivateKey.GetValue(), pkcs1TestPEM(t)},
		} {
			if c.got != c.want {
				t.Errorf("%s: got %q, want the stored value %q", c.name, c.got, c.want)
			}
		}
		if cfg.Audience != "api://other" {
			t.Errorf("audience: got %q, want the updated literal", cfg.Audience)
		}
	})

	t.Run("a mask with no stored counterpart is rejected", func(t *testing.T) {
		update := schemas.Key{OAuthKeyConfig: &schemas.OAuthKeyConfig{
			GrantType: schemas.OAuthGrantClientCredentials, TokenURL: *secret("https://idp.example/token"),
			ClientID: secret("id"), ClientSecret: mask("abcd", "wxyz"),
		}}
		if _, err := h.mergeUpdatedKey(schemas.Key{}, update); err == nil {
			t.Fatal("expected an error when a mask has no stored value behind it")
		}
	})

	t.Run("unmasked literals overwrite the stored values", func(t *testing.T) {
		update := schemas.Key{OAuthKeyConfig: &schemas.OAuthKeyConfig{
			GrantType: schemas.OAuthGrantClientCredentials, TokenURL: *secret("https://idp2.example/token"),
			ClientID: secret("new-id"), ClientSecret: secret("new-secret"),
		}}
		merged, err := h.mergeUpdatedKey(stored(), update)
		if err != nil {
			t.Fatalf("mergeUpdatedKey returned error: %v", err)
		}
		cfg := merged.OAuthKeyConfig
		if cfg.ClientSecret.GetValue() != "new-secret" || cfg.TokenURL.GetValue() != "https://idp2.example/token" {
			t.Errorf("literals were not applied: %q %q", cfg.ClientSecret.GetValue(), cfg.TokenURL.GetValue())
		}
		if cfg.PrivateKey != nil {
			t.Errorf("a field the update left out must not be resurrected from the stored config")
		}
	})

	t.Run("masked extra_params values resolve back to the stored values", func(t *testing.T) {
		storedWithParams := stored()
		storedWithParams.OAuthKeyConfig.ExtraParams = map[string]string{"client_assertion": "assertion-secret", "resource": "api://acme"}
		update := schemas.Key{OAuthKeyConfig: &schemas.OAuthKeyConfig{
			GrantType: schemas.OAuthGrantJWTBearer, TokenURL: *secret("https://idp.example/oauth2/token"),
			PrivateKey: secret(pkcs1TestPEM(t)), Issuer: "client-123", Audience: "api://acme",
			ExtraParams: map[string]string{"client_assertion": "asse" + strings.Repeat("*", 24) + "cret", "resource": "api://other"},
		}}
		merged, err := h.mergeUpdatedKey(storedWithParams, update)
		if err != nil {
			t.Fatalf("mergeUpdatedKey returned error: %v", err)
		}
		if merged.OAuthKeyConfig.ExtraParams["client_assertion"] != "assertion-secret" {
			t.Errorf("client_assertion: got %q, want the stored value", merged.OAuthKeyConfig.ExtraParams["client_assertion"])
		}
		if merged.OAuthKeyConfig.ExtraParams["resource"] != "api://other" {
			t.Errorf("resource: got %q, want the new literal", merged.OAuthKeyConfig.ExtraParams["resource"])
		}
	})

	t.Run("a masked extra_params value that looks like a secret reference restores the stored string", func(t *testing.T) {
		// "env.production" is sent to the token endpoint verbatim; it is not a Bifrost reference.
		// The GET masks it, and the masked form must resolve back to the stored string.
		storedWithParams := stored()
		storedWithParams.OAuthKeyConfig.ExtraParams = map[string]string{"resource": "env.production"}
		masked := storedWithParams.OAuthKeyConfig.Redacted().ExtraParams["resource"]
		update := schemas.Key{OAuthKeyConfig: &schemas.OAuthKeyConfig{
			GrantType: schemas.OAuthGrantJWTBearer, TokenURL: *secret("https://idp.example/oauth2/token"),
			PrivateKey: secret(pkcs1TestPEM(t)), Issuer: "client-123", Audience: "api://acme",
			ExtraParams: map[string]string{"resource": masked},
		}}
		merged, err := h.mergeUpdatedKey(storedWithParams, update)
		if err != nil {
			t.Fatalf("mergeUpdatedKey returned error: %v", err)
		}
		if merged.OAuthKeyConfig.ExtraParams["resource"] != "env.production" {
			t.Errorf("resource: got %q, want the stored string restored from its mask %q", merged.OAuthKeyConfig.ExtraParams["resource"], masked)
		}
	})

	// The UI switches auth methods by sending an explicit empty value (API key -> OAuth) or an
	// explicit null section (OAuth -> API key). Both must clear the stored half; an omitted
	// field keeps it. resolveKey prefers a static value, so a surviving one silently wins.
	t.Run("an explicit empty value clears the stored static key", func(t *testing.T) {
		old := stored()
		old.Value = *secret("sk-old")
		decoded, err := decodeKeyUpdate([]byte(`{"name":"k","value":""}`), old)
		if err != nil {
			t.Fatalf("decodeKeyUpdate: %v", err)
		}
		merged, err := h.mergeUpdatedKey(old, decoded)
		if err != nil {
			t.Fatalf("mergeUpdatedKey: %v", err)
		}
		if merged.Value.GetValue() != "" {
			t.Fatalf("value = %q, want cleared", merged.Value.GetValue())
		}
	})

	t.Run("an explicit null clears the stored oauth block", func(t *testing.T) {
		decoded, err := decodeKeyUpdate([]byte(`{"name":"k","value":"sk-new","oauth_key_config":null}`), stored())
		if err != nil {
			t.Fatalf("decodeKeyUpdate: %v", err)
		}
		if decoded.OAuthKeyConfig != nil {
			t.Fatal("an explicit null must clear the stored oauth_key_config")
		}
	})

	t.Run("an update without the section keeps the stored one", func(t *testing.T) {
		body := []byte(`{"name":"renamed"}`)
		decoded, err := decodeKeyUpdate(body, stored())
		if err != nil {
			t.Fatalf("decodeKeyUpdate: %v", err)
		}
		if decoded.OAuthKeyConfig == nil || decoded.OAuthKeyConfig.ClientSecret.GetValue() != "client-secret-456" {
			t.Fatal("an omitted oauth_key_config must keep the stored section")
		}
	})
}

// TestValidateProviderKeyOAuthKeyConfig pins the setup-time checks on oauth_key_config: it is
// accepted on any provider's key, must be whole for its grant, and literal values are
// shape-checked so a typo fails at save rather than at the first inference call.
func TestValidateProviderKeyOAuthKeyConfig(t *testing.T) {
	cc := func(mutate func(*schemas.OAuthKeyConfig)) *schemas.OAuthKeyConfig {
		c := &schemas.OAuthKeyConfig{
			GrantType:    schemas.OAuthGrantClientCredentials,
			TokenURL:     *schemas.NewSecretVar("https://idp.example/oauth2/token"),
			ClientID:     schemas.NewSecretVar("id"),
			ClientSecret: schemas.NewSecretVar("secret"),
		}
		if mutate != nil {
			mutate(c)
		}
		return c
	}
	jb := func(mutate func(*schemas.OAuthKeyConfig)) *schemas.OAuthKeyConfig {
		c := &schemas.OAuthKeyConfig{
			GrantType:  schemas.OAuthGrantJWTBearer,
			TokenURL:   *schemas.NewSecretVar("https://idp.example/oauth2/token"),
			PrivateKey: schemas.NewSecretVar(pkcs1TestPEM(t)),
			Issuer:     "client",
			Audience:   "https://idp.example/oauth2/token",
		}
		if mutate != nil {
			mutate(c)
		}
		return c
	}
	tests := []struct {
		name     string
		provider schemas.ModelProvider
		key      schemas.Key
		wantErr  string
	}{
		{"client_credentials on openai", schemas.OpenAI, schemas.Key{OAuthKeyConfig: cc(nil)}, ""},
		{"jwt_bearer on openai", schemas.OpenAI, schemas.Key{OAuthKeyConfig: jb(nil)}, ""},
		{"jwt_bearer with a PKCS#8 key", schemas.OpenAI, schemas.Key{OAuthKeyConfig: jb(func(c *schemas.OAuthKeyConfig) { c.PrivateKey = schemas.NewSecretVar(pkcs8TestPEM(t)) })}, ""},
		{"http on loopback is allowed", schemas.OpenAI, schemas.Key{OAuthKeyConfig: cc(func(c *schemas.OAuthKeyConfig) { c.TokenURL = *schemas.NewSecretVar("http://127.0.0.1:9/token") })}, ""},
		{"http on localhost in any letter case is allowed, as in core", schemas.OpenAI, schemas.Key{OAuthKeyConfig: cc(func(c *schemas.OAuthKeyConfig) { c.TokenURL = *schemas.NewSecretVar("http://LOCALHOST:8080/token") })}, ""},
		{"env-backed token url is not shape checked", schemas.OpenAI, schemas.Key{OAuthKeyConfig: cc(func(c *schemas.OAuthKeyConfig) { c.TokenURL = *schemas.NewSecretVar("env.IDP_TOKEN_URL") })}, ""},
		{"userinfo in the token url is refused", schemas.OpenAI, schemas.Key{OAuthKeyConfig: cc(func(c *schemas.OAuthKeyConfig) {
			c.TokenURL = *schemas.NewSecretVar("https://user:pass@idp.example/token")
		})}, "must not carry userinfo, a query, or a fragment"},
		{"a query in the token url is refused", schemas.OpenAI, schemas.Key{OAuthKeyConfig: cc(func(c *schemas.OAuthKeyConfig) {
			c.TokenURL = *schemas.NewSecretVar("https://idp.example/token?client_secret=x")
		})}, "must not carry userinfo, a query, or a fragment"},
		{"a fragment in the token url is refused", schemas.OpenAI, schemas.Key{OAuthKeyConfig: cc(func(c *schemas.OAuthKeyConfig) {
			c.TokenURL = *schemas.NewSecretVar("https://idp.example/token#frag")
		})}, "must not carry userinfo, a query, or a fragment"},
		{"plain http is refused", schemas.OpenAI, schemas.Key{OAuthKeyConfig: cc(func(c *schemas.OAuthKeyConfig) { c.TokenURL = *schemas.NewSecretVar("http://idp.example/token") })}, "https"},
		{"missing client secret", schemas.OpenAI, schemas.Key{OAuthKeyConfig: cc(func(c *schemas.OAuthKeyConfig) { c.ClientSecret = nil })}, "client_secret"},
		{"jwt_bearer missing audience", schemas.OpenAI, schemas.Key{OAuthKeyConfig: jb(func(c *schemas.OAuthKeyConfig) { c.Audience = "" })}, "audience is required"},
		{"jwt_bearer literal key is not a PEM", schemas.OpenAI, schemas.Key{OAuthKeyConfig: jb(func(c *schemas.OAuthKeyConfig) { c.PrivateKey = schemas.NewSecretVar("not a key") })}, "RSA or EC private key"},
		{"jwt_bearer env-backed key is not shape checked", schemas.OpenAI, schemas.Key{OAuthKeyConfig: jb(func(c *schemas.OAuthKeyConfig) { c.PrivateKey = schemas.NewSecretVar("env.IDP_KEY") })}, ""},
		{"an EC key under the default RS256 is refused at save", schemas.OpenAI, schemas.Key{OAuthKeyConfig: jb(func(c *schemas.OAuthKeyConfig) {
			c.PrivateKey = schemas.NewSecretVar(ecCurveTestPEM(t, elliptic.P256(), false))
			c.SigningAlgorithm = ""
		})}, "signing_algorithm"},
		{"an RSA key with ES256 is refused at save", schemas.OpenAI, schemas.Key{OAuthKeyConfig: jb(func(c *schemas.OAuthKeyConfig) {
			c.PrivateKey = schemas.NewSecretVar(pkcs1TestPEM(t))
			c.SigningAlgorithm = "ES256"
		})}, "signing_algorithm"},
		{"a P-384 key with ES256 is refused at save", schemas.OpenAI, schemas.Key{OAuthKeyConfig: jb(func(c *schemas.OAuthKeyConfig) {
			c.PrivateKey = schemas.NewSecretVar(ecCurveTestPEM(t, elliptic.P384(), false))
			c.SigningAlgorithm = "ES256"
		})}, "signing_algorithm"},
		{"a P-256 key with ES256 is accepted", schemas.OpenAI, schemas.Key{OAuthKeyConfig: jb(func(c *schemas.OAuthKeyConfig) {
			c.PrivateKey = schemas.NewSecretVar(ecCurveTestPEM(t, elliptic.P256(), false))
			c.SigningAlgorithm = "ES256"
		})}, ""},
		{"a P-384 PKCS#8 key with ES384 is accepted", schemas.OpenAI, schemas.Key{OAuthKeyConfig: jb(func(c *schemas.OAuthKeyConfig) {
			c.PrivateKey = schemas.NewSecretVar(ecCurveTestPEM(t, elliptic.P384(), true))
			c.SigningAlgorithm = "ES384"
		})}, ""},
		{"a P-521 key with ES512 is accepted", schemas.OpenAI, schemas.Key{OAuthKeyConfig: jb(func(c *schemas.OAuthKeyConfig) {
			c.PrivateKey = schemas.NewSecretVar(ecCurveTestPEM(t, elliptic.P521(), false))
			c.SigningAlgorithm = "ES512"
		})}, ""},
		{"an RSA key with PS256 is accepted", schemas.OpenAI, schemas.Key{OAuthKeyConfig: jb(func(c *schemas.OAuthKeyConfig) {
			c.PrivateKey = schemas.NewSecretVar(pkcs8TestPEM(t))
			c.SigningAlgorithm = "PS256"
		})}, ""},
		{"a half block behind a value is still refused", schemas.OpenAI, schemas.Key{Value: *schemas.NewSecretVar("sk"), OAuthKeyConfig: cc(func(c *schemas.OAuthKeyConfig) { c.GrantType = "" })}, "grant_type is required"},
		{"refused on a provider that does not resolve it", schemas.Anthropic, schemas.Key{Value: *schemas.NewSecretVar("sk"), OAuthKeyConfig: cc(nil)}, "only supported on openai"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateProviderKeyURL(tt.provider, tt.key)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("got %v, want an error containing %q", err, tt.wantErr)
			}
		})
	}

	t.Run("the token url is a dial target", func(t *testing.T) {
		targets := keyDialTargets(schemas.Key{OAuthKeyConfig: cc(nil)})
		if targets["oauth_key_config.token_url"] == nil {
			t.Fatal("oauth_key_config.token_url must be guarded as a caller-chosen dial target")
		}
	})
}

// TestValidateProviderKeyGithubCopilotFormats pins that a literal GitHub App credential is
// checked for shape at save time, not just for presence. A non-numeric installation_id or a
// mangled PEM otherwise persists happily and fails on the first inference call, where it
// reads as a runtime fault rather than a typo in a form.
//
// Environment and vault references are exempt: their values are not available here, so
// checking them would reject every legitimate headless configuration.
func TestValidateProviderKeyGithubCopilotFormats(t *testing.T) {
	pemBody := pkcs1TestPEM(t)

	appConfig := func(mutate func(*schemas.GithubCopilotKeyConfig)) *schemas.GithubCopilotKeyConfig {
		c := &schemas.GithubCopilotKeyConfig{
			AppID:          *schemas.NewSecretVar("123456"),
			InstallationID: *schemas.NewSecretVar("87654321"),
			RepositoryID:   *schemas.NewSecretVar("999000111"),
			PrivateKey:     *schemas.NewSecretVar(pemBody),
		}
		if mutate != nil {
			mutate(c)
		}
		return c
	}

	cases := []struct {
		name    string
		key     schemas.Key
		wantErr string
	}{
		{"valid literal app config", schemas.Key{GithubCopilotKeyConfig: appConfig(nil)}, ""},
		{"direct token needs no app config", schemas.Key{Value: *schemas.NewSecretVar("tid=abc")}, ""},
		{
			"non-numeric installation_id",
			schemas.Key{GithubCopilotKeyConfig: appConfig(func(c *schemas.GithubCopilotKeyConfig) {
				c.InstallationID = *schemas.NewSecretVar("my-install")
			})},
			"installation_id",
		},
		{
			"path traversal in installation_id",
			schemas.Key{GithubCopilotKeyConfig: appConfig(func(c *schemas.GithubCopilotKeyConfig) {
				c.InstallationID = *schemas.NewSecretVar("1/../../../user")
			})},
			"installation_id",
		},
		{
			"non-numeric repository_id",
			schemas.Key{GithubCopilotKeyConfig: appConfig(func(c *schemas.GithubCopilotKeyConfig) {
				c.RepositoryID = *schemas.NewSecretVar("my-repo")
			})},
			"repository_id",
		},
		{
			"private key that is not PEM",
			schemas.Key{GithubCopilotKeyConfig: appConfig(func(c *schemas.GithubCopilotKeyConfig) {
				c.PrivateKey = *schemas.NewSecretVar("not a key")
			})},
			"private_key",
		},
		{
			"valid PKCS#8 private key",
			schemas.Key{GithubCopilotKeyConfig: appConfig(func(c *schemas.GithubCopilotKeyConfig) {
				c.PrivateKey = *schemas.NewSecretVar(pkcs8TestPEM(t))
			})},
			"",
		},
		{
			// An EC key is a perfectly well-formed PEM, so an envelope-only check waves it
			// through. GitHub App JWTs are RS256, so it fails at the first request instead.
			"EC private key",
			schemas.Key{GithubCopilotKeyConfig: appConfig(func(c *schemas.GithubCopilotKeyConfig) {
				c.PrivateKey = *schemas.NewSecretVar(ecTestPEM(t))
			})},
			"private_key",
		},
		{
			// Passphrase-protected keys cannot be used unattended, and the envelope alone
			// does not say so.
			"encrypted private key envelope",
			schemas.Key{GithubCopilotKeyConfig: appConfig(func(c *schemas.GithubCopilotKeyConfig) {
				c.PrivateKey = *schemas.NewSecretVar("-----BEGIN ENCRYPTED PRIVATE KEY-----\nZm9v\n-----END ENCRYPTED PRIVATE KEY-----")
			})},
			"private_key",
		},
		{
			"well-formed envelope with a garbage DER payload",
			schemas.Key{GithubCopilotKeyConfig: appConfig(func(c *schemas.GithubCopilotKeyConfig) {
				c.PrivateKey = *schemas.NewSecretVar("-----BEGIN RSA PRIVATE KEY-----\nZm9vYmFy\n-----END RSA PRIVATE KEY-----")
			})},
			"private_key",
		},
		{
			"PEM whose newlines survived as literal backslash-n",
			schemas.Key{GithubCopilotKeyConfig: appConfig(func(c *schemas.GithubCopilotKeyConfig) {
				c.PrivateKey = *schemas.NewSecretVar(strings.ReplaceAll(pkcs1TestPEM(t), "\n", `\n`))
			})},
			"",
		},
		{
			"no token and no app config at all",
			schemas.Key{},
			"github_copilot_key_config is required",
		},
		{
			"no token, missing app_id",
			schemas.Key{GithubCopilotKeyConfig: appConfig(func(c *schemas.GithubCopilotKeyConfig) {
				c.AppID = schemas.SecretVar{}
			})},
			"github_copilot_key_config.app_id is required",
		},
		{
			"no token, missing installation_id",
			schemas.Key{GithubCopilotKeyConfig: appConfig(func(c *schemas.GithubCopilotKeyConfig) {
				c.InstallationID = schemas.SecretVar{}
			})},
			"github_copilot_key_config.installation_id is required",
		},
		{
			"no token, missing repository_id",
			schemas.Key{GithubCopilotKeyConfig: appConfig(func(c *schemas.GithubCopilotKeyConfig) {
				c.RepositoryID = schemas.SecretVar{}
			})},
			"github_copilot_key_config.repository_id is required",
		},
		{
			"no token, missing private_key",
			schemas.Key{GithubCopilotKeyConfig: appConfig(func(c *schemas.GithubCopilotKeyConfig) {
				c.PrivateKey = schemas.SecretVar{}
			})},
			"github_copilot_key_config.private_key is required",
		},
		{
			// A token does not excuse a half-filled App block: it persists silently and only
			// breaks later, when the token expires or is removed.
			"direct token alongside an incomplete app config",
			schemas.Key{
				Value: *schemas.NewSecretVar("tid=abc"),
				GithubCopilotKeyConfig: &schemas.GithubCopilotKeyConfig{
					AppID: *schemas.NewSecretVar("123456"),
				},
			},
			"installation_id",
		},
		{
			"direct token alongside a malformed app config",
			schemas.Key{
				Value: *schemas.NewSecretVar("tid=abc"),
				GithubCopilotKeyConfig: appConfig(func(c *schemas.GithubCopilotKeyConfig) {
					c.RepositoryID = *schemas.NewSecretVar("not-a-number")
				}),
			},
			"repository_id",
		},
		{
			"direct token alongside a complete app config",
			schemas.Key{Value: *schemas.NewSecretVar("tid=abc"), GithubCopilotKeyConfig: appConfig(nil)},
			"",
		},
		{
			// GitHub documents the JWT issuer as "the client ID or application ID", and says
			// "use of the client ID is recommended". Client IDs look like Iv1.b507a08c87ecfe98,
			// so a digits-only rule on app_id would reject the recommended configuration.
			// https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-a-json-web-token-jwt-for-a-github-app
			"client ID as app_id is accepted",
			schemas.Key{GithubCopilotKeyConfig: appConfig(func(c *schemas.GithubCopilotKeyConfig) {
				c.AppID = *schemas.NewSecretVar("Iv1.b507a08c87ecfe98")
			})},
			"",
		},
		{
			"env references are not format-checked",
			schemas.Key{GithubCopilotKeyConfig: &schemas.GithubCopilotKeyConfig{
				AppID:          *schemas.NewSecretVar("env.COPILOT_APP_ID"),
				InstallationID: *schemas.NewSecretVar("env.COPILOT_INSTALLATION_ID"),
				RepositoryID:   *schemas.NewSecretVar("env.COPILOT_REPOSITORY_ID"),
				PrivateKey:     *schemas.NewSecretVar("env.COPILOT_PRIVATE_KEY"),
			}},
			"",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateProviderKeyURL(schemas.GithubCopilot, tc.key)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected no error, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

// refreshHandlerForTest builds a ProviderHandler with one keyed provider and a
// recording models manager.
func refreshHandlerForTest(mgr *mockModelsManager) *ProviderHandler {
	SetLogger(&mockLogger{})
	lib.SetLogger(&mockLogger{})
	return &ProviderHandler{
		inMemoryStore: &lib.Config{
			Providers: map[schemas.ModelProvider]configstore.ProviderConfig{
				"openai": {Keys: []schemas.Key{{ID: "key-1"}}},
			},
		},
		modelsManager: mgr,
	}
}

func TestRefreshProviderModels_DelegatesToModelsManager(t *testing.T) {
	mgr := &mockModelsManager{}
	h := refreshHandlerForTest(mgr)

	ctx := newTestRequestCtx("")
	ctx.SetUserValue("provider", "openai")
	h.refreshProviderModels(ctx)

	if ctx.Response.StatusCode() != fasthttp.StatusOK {
		t.Fatalf("status got %d, want 200; body=%s", ctx.Response.StatusCode(), ctx.Response.Body())
	}
	if len(mgr.refreshProviderCalls) != 1 || mgr.refreshProviderCalls[0] != "openai" {
		t.Fatalf("expected one provider-level refresh for openai, got %v", mgr.refreshProviderCalls)
	}
}

func TestRefreshProviderKeyModels_DelegatesToModelsManager(t *testing.T) {
	mgr := &mockModelsManager{}
	h := refreshHandlerForTest(mgr)

	ctx := newTestRequestCtx("")
	ctx.SetUserValue("provider", "openai")
	ctx.SetUserValue("key_id", "key-1")
	h.refreshProviderKeyModels(ctx)

	if ctx.Response.StatusCode() != fasthttp.StatusOK {
		t.Fatalf("status got %d, want 200; body=%s", ctx.Response.StatusCode(), ctx.Response.Body())
	}
	if len(mgr.refreshKeyCalls) != 1 || mgr.refreshKeyCalls[0].keyID != "key-1" {
		t.Fatalf("expected one refresh for key-1, got %v", mgr.refreshKeyCalls)
	}
}

// A refresh already running for the provider must surface as 409 rather than
// stacking another (enabled keys x 2) burst of upstream calls, so the UI can
// tell the user to wait instead of silently doubling the load.
func TestRefreshProviderModels_InFlightReturns409(t *testing.T) {
	mgr := &mockModelsManager{refreshErr: ErrRefreshInProgress}
	h := refreshHandlerForTest(mgr)

	ctx := newTestRequestCtx("")
	ctx.SetUserValue("provider", "openai")
	h.refreshProviderModels(ctx)

	if ctx.Response.StatusCode() != fasthttp.StatusConflict {
		t.Fatalf("status got %d, want 409; body=%s", ctx.Response.StatusCode(), ctx.Response.Body())
	}
}

func TestRefreshProviderKeyModels_UnknownKeyReturns404(t *testing.T) {
	mgr := &mockModelsManager{}
	h := refreshHandlerForTest(mgr)

	ctx := newTestRequestCtx("")
	ctx.SetUserValue("provider", "openai")
	ctx.SetUserValue("key_id", "does-not-exist")
	h.refreshProviderKeyModels(ctx)

	if ctx.Response.StatusCode() != fasthttp.StatusNotFound {
		t.Fatalf("status got %d, want 404; body=%s", ctx.Response.StatusCode(), ctx.Response.Body())
	}
	if len(mgr.refreshKeyCalls) != 0 {
		t.Fatalf("expected no upstream refresh for an unknown key, got %v", mgr.refreshKeyCalls)
	}
}

func TestRefreshProviderModels_UnknownProviderReturns404(t *testing.T) {
	mgr := &mockModelsManager{}
	h := refreshHandlerForTest(mgr)

	ctx := newTestRequestCtx("")
	ctx.SetUserValue("provider", "does-not-exist")
	h.refreshProviderModels(ctx)

	if ctx.Response.StatusCode() != fasthttp.StatusNotFound {
		t.Fatalf("status got %d, want 404; body=%s", ctx.Response.StatusCode(), ctx.Response.Body())
	}
	if len(mgr.refreshProviderCalls) != 0 {
		t.Fatalf("expected no upstream refresh for an unknown provider, got %v", mgr.refreshProviderCalls)
	}
}

// TestDeleteProviderKey_KeylessProviderRemovesResidualKey is the recovery
// half of issue #8500. A provider switched from keyed to keyless keeps its old
// keys, and adding or updating keys there is rightly refused, but refusing the
// DELETE too left no API or UI path to ever remove them. Deleting can only
// shrink the key set of a provider that ignores keys, so it must go through.
func TestDeleteProviderKey_KeylessProviderRemovesResidualKey(t *testing.T) {
	SetLogger(&mockLogger{})
	lib.SetLogger(&mockLogger{})

	const provider schemas.ModelProvider = "keyless-custom"
	h := &ProviderHandler{
		inMemoryStore: &lib.Config{
			Providers: map[schemas.ModelProvider]configstore.ProviderConfig{
				provider: {
					Keys: []schemas.Key{{ID: "residual-key", Name: "residual", Value: *schemas.NewSecretVar("sk-residual"), Weight: 1, Enabled: schemas.Ptr(false)}},
					CustomProviderConfig: &schemas.CustomProviderConfig{
						IsKeyLess:        true,
						BaseProviderType: schemas.OpenAI,
					},
				},
			},
		},
		modelsManager: &mockModelsManager{},
	}
	attachBifrostClient(t, h.inMemoryStore)

	ctx := newTestRequestCtx("")
	ctx.SetUserValue("provider", string(provider))
	ctx.SetUserValue("key_id", "residual-key")
	h.deleteProviderKey(ctx)

	if ctx.Response.StatusCode() != fasthttp.StatusOK {
		t.Fatalf("delete residual key on keyless provider: status got %d, want 200; body=%s", ctx.Response.StatusCode(), ctx.Response.Body())
	}
	if keys := h.inMemoryStore.Providers[provider].Keys; len(keys) != 0 {
		t.Fatalf("residual key still stored after delete: %+v", keys)
	}
}

// Regression for the custom-provider path: required-field validation must run
// against the resolved BASE provider, not the custom route name, or a custom
// provider based on Bedrock would skip the region requirement entirely.
func TestCreateProviderKey_CustomBedrockRequiresRegion(t *testing.T) {
	SetLogger(&mockLogger{})
	lib.SetLogger(&mockLogger{})

	h := &ProviderHandler{
		inMemoryStore: &lib.Config{
			Providers: map[schemas.ModelProvider]configstore.ProviderConfig{
				"aws-custom": {
					CustomProviderConfig: &schemas.CustomProviderConfig{
						BaseProviderType: schemas.Bedrock,
					},
				},
			},
		},
		modelsManager: &mockModelsManager{},
	}

	ctx := newTestRequestCtx(`{"value":"AKIAEXAMPLEKEY","weight":1.0,"bedrock_key_config":{}}`)
	ctx.SetUserValue("provider", "aws-custom")

	h.createProviderKey(ctx)
	if ctx.Response.StatusCode() != fasthttp.StatusBadRequest {
		t.Fatalf("custom-bedrock create without region: status got %d, want 400; body=%s", ctx.Response.StatusCode(), ctx.Response.Body())
	}
	if body := string(ctx.Response.Body()); !strings.Contains(body, "bedrock_key_config.region") {
		t.Fatalf("expected bedrock_key_config.region error, got %s", body)
	}
}

// TestUpdateProviderKey_EndpointGuardWhenAuthBypassed pins that the fail-open bypass only
// blocks a key update that moves the dial destination. Keeping the stored endpoint and
// editing an unrelated field (weight) must still go through, or the auth-disabled UI
// cannot edit any existing Ollama/SGL/VLLM/Azure key.
func TestUpdateProviderKey_EndpointGuardWhenAuthBypassed(t *testing.T) {
	SetLogger(&mockLogger{})
	lib.SetLogger(&mockLogger{})

	const storedURL = "http://localhost:11434"
	cases := []struct {
		name       string
		body       string
		wantStatus int
		wantURL    string
		wantWeight float64
	}{
		{
			name:       "unchanged endpoint, weight edit",
			body:       `{"weight":2,"ollama_key_config":{"url":"` + storedURL + `"}}`,
			wantStatus: fasthttp.StatusOK,
			wantURL:    storedURL,
			wantWeight: 2,
		},
		{
			name:       "changed endpoint",
			body:       `{"weight":1,"ollama_key_config":{"url":"http://169.254.169.254/"}}`,
			wantStatus: fasthttp.StatusForbidden,
			wantURL:    storedURL,
			wantWeight: 1,
		},
		{
			name:       "changed endpoint under a case-variant name",
			body:       `{"WEIGHT":3,"OLLAMA_KEY_CONFIG":{"url":"http://169.254.169.254/"}}`,
			wantStatus: fasthttp.StatusForbidden,
			wantURL:    storedURL,
			wantWeight: 1,
		},
		{
			name:       "unchanged endpoint under a case-variant name",
			body:       `{"WEIGHT":2,"OLLAMA_KEY_CONFIG":{"url":"` + storedURL + `"}}`,
			wantStatus: fasthttp.StatusOK,
			wantURL:    storedURL,
			wantWeight: 2,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &ProviderHandler{
				inMemoryStore: &lib.Config{
					Providers: map[schemas.ModelProvider]configstore.ProviderConfig{
						schemas.Ollama: {Keys: []schemas.Key{{
							ID:              "key-1",
							Name:            "ollama-key",
							Weight:          1,
							OllamaKeyConfig: &schemas.OllamaKeyConfig{URL: *schemas.NewSecretVar(storedURL)},
						}}},
					},
				},
				modelsManager: &mockModelsManager{},
			}
			attachBifrostClient(t, h.inMemoryStore)

			ctx := newTestRequestCtx(tc.body)
			ctx.SetUserValue("provider", string(schemas.Ollama))
			ctx.SetUserValue("key_id", "key-1")
			ctx.SetUserValue(schemas.BifrostContextKeyAuthBypassed, true)

			h.updateProviderKey(ctx)

			if ctx.Response.StatusCode() != tc.wantStatus {
				t.Fatalf("status got %d, want %d; body=%s", ctx.Response.StatusCode(), tc.wantStatus, ctx.Response.Body())
			}
			stored := h.inMemoryStore.Providers[schemas.Ollama].Keys[0]
			if got := stored.OllamaKeyConfig.URL.GetValue(); got != tc.wantURL {
				t.Fatalf("stored url got %q, want %q", got, tc.wantURL)
			}
			if stored.Weight != tc.wantWeight {
				t.Fatalf("stored weight got %v, want %v", stored.Weight, tc.wantWeight)
			}
		})
	}
}

// TestProviderKeyWeight_RejectsNegative pins that neither key write path stores a negative weight.
// Key selection draws a key by its share of the pool's total weight, and a negative weight once
// drove that total below zero, which panicked inside the request worker and took the gateway down.
func TestProviderKeyWeight_RejectsNegative(t *testing.T) {
	SetLogger(&mockLogger{})
	lib.SetLogger(&mockLogger{})

	cases := []struct {
		name       string
		create     bool
		body       string
		wantStatus int
		wantWeight float64 // the stored weight of key-1 after the call
	}{
		{name: "create with a negative weight", create: true, body: `{"name":"new-key","value":"sk-test","weight":-1}`, wantStatus: fasthttp.StatusBadRequest, wantWeight: 1},
		{name: "create with a zero weight", create: true, body: `{"name":"new-key","value":"sk-test","weight":0}`, wantStatus: fasthttp.StatusOK, wantWeight: 1},
		{name: "update to a negative weight", body: `{"name":"openai-key","weight":-0.5}`, wantStatus: fasthttp.StatusBadRequest, wantWeight: 1},
		{name: "update to a zero weight", body: `{"name":"openai-key","weight":0}`, wantStatus: fasthttp.StatusOK, wantWeight: 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &ProviderHandler{
				inMemoryStore: &lib.Config{
					Providers: map[schemas.ModelProvider]configstore.ProviderConfig{
						schemas.OpenAI: {Keys: []schemas.Key{{
							ID:     "key-1",
							Name:   "openai-key",
							Value:  *schemas.NewSecretVar("sk-stored"),
							Weight: 1,
						}}},
					},
				},
				modelsManager: &mockModelsManager{},
			}
			attachBifrostClient(t, h.inMemoryStore)

			ctx := newTestRequestCtx(tc.body)
			ctx.SetUserValue("provider", string(schemas.OpenAI))
			if tc.create {
				h.createProviderKey(ctx)
			} else {
				ctx.SetUserValue("key_id", "key-1")
				h.updateProviderKey(ctx)
			}

			if ctx.Response.StatusCode() != tc.wantStatus {
				t.Fatalf("status got %d, want %d; body=%s", ctx.Response.StatusCode(), tc.wantStatus, ctx.Response.Body())
			}
			if tc.wantStatus == fasthttp.StatusBadRequest && !strings.Contains(string(ctx.Response.Body()), "weight") {
				t.Fatalf("expected the error to name the weight, got %s", ctx.Response.Body())
			}
			keys := h.inMemoryStore.Providers[schemas.OpenAI].Keys
			if tc.create && tc.wantStatus == fasthttp.StatusBadRequest && len(keys) != 1 {
				t.Fatalf("a rejected key must not be stored, got %d keys", len(keys))
			}
			if keys[0].Weight != tc.wantWeight {
				t.Fatalf("stored weight of key-1 got %v, want %v", keys[0].Weight, tc.wantWeight)
			}
		})
	}
}

// TestProviderKeyEndpointGuard_CoversEveryDialTarget pins that the fail-open bypass guard
// keys off the fields Bifrost dials, not a provider-type list. Each of these fields sends the
// key's credential to the configured host, so a bypassed caller must not be able to set or
// move any of them. A Bedrock key with only a region carries no caller-chosen host and must
// still be creatable, which a provider-type guard would wrongly reject.
func TestProviderKeyEndpointGuard_CoversEveryDialTarget(t *testing.T) {
	SetLogger(&mockLogger{})
	lib.SetLogger(&mockLogger{})

	copilotPEM := pkcs1TestPEM(t)
	copilotConfig := func(domain string) string {
		return `{"app_id":"123","installation_id":"456","repository_id":"789","private_key":` + strconv.Quote(copilotPEM) + `,"github_domain":"` + domain + `"}`
	}

	// A Vertex credential JSON names where the Google client dials: a service account's
	// token_uri, and for an external account the token_url plus a credential_source the client
	// reads (here a local file) and posts there. Swapping the JSON therefore moves the key's
	// destination as surely as moving an Azure endpoint does.
	const vertexServiceAccountJSON = `{"type":"service_account","project_id":"p","client_email":"sa@p.iam.gserviceaccount.com","private_key":"-----BEGIN PRIVATE KEY-----\nMIIB\n-----END PRIVATE KEY-----\n","token_uri":"https://oauth2.googleapis.com/token"}`
	const vertexExternalAccountJSON = `{"type":"external_account","audience":"//iam.googleapis.com/projects/1/locations/global/workloadIdentityPools/p/providers/x","subject_token_type":"urn:ietf:params:oauth:token-type:jwt","token_url":"https://evil.example.com/token","credential_source":{"file":"/etc/bifrost/config.json"}}`
	vertexConfig := func(credentials string) string {
		return `{"project_id":"p","region":"us-central1","auth_credentials":` + strconv.Quote(credentials) + `}`
	}
	vertexStored := func() *schemas.Key {
		return &schemas.Key{
			ID: "key-1", Name: "vertex-key", Weight: 1,
			VertexKeyConfig: &schemas.VertexKeyConfig{
				ProjectID:       *schemas.NewSecretVar("p"),
				Region:          *schemas.NewSecretVar("us-central1"),
				AuthCredentials: *schemas.NewSecretVar(vertexServiceAccountJSON),
			},
		}
	}

	cases := []struct {
		name        string
		provider    schemas.ModelProvider
		stored      *schemas.Key // nil means the request creates the key
		genuineAuth bool         // the request carries a real credential, not the fail-open bypass
		body        string
		want403     bool
		wantNamed   string // a 403 must name this field
	}{
		{
			name:     "create bedrock key with region only",
			provider: schemas.Bedrock,
			body:     `{"name":"bedrock-key","weight":1,"bedrock_key_config":{"access_key":"AKIAEXAMPLE","secret_key":"secret","region":"us-east-1"}}`,
			want403:  false,
		},
		{
			name:     "create bedrock key with runtime endpoint override",
			provider: schemas.Bedrock,
			body:     `{"name":"bedrock-key","weight":1,"bedrock_key_config":{"access_key":"AKIAEXAMPLE","secret_key":"secret","region":"us-east-1","endpoints":{"runtime":"evil.example.com"}}}`,
			want403:  true,
		},
		{
			name:     "update databricks workspace url",
			provider: schemas.Databricks,
			stored: &schemas.Key{
				ID: "key-1", Name: "databricks-key", Weight: 1,
				Value:               *schemas.NewSecretVar("dapi-token"),
				DatabricksKeyConfig: &schemas.DatabricksKeyConfig{WorkspaceURL: *schemas.NewSecretVar("https://adb-1.azuredatabricks.net")},
			},
			body:    `{"value":"dapi-token","weight":1,"databricks_key_config":{"workspace_url":"https://evil.example.com"}}`,
			want403: true,
		},
		{
			name:     "update bedrock mantle endpoint override",
			provider: schemas.BedrockMantle,
			stored: &schemas.Key{
				ID: "key-1", Name: "mantle-key", Weight: 1,
				BedrockMantleKeyConfig: &schemas.BedrockMantleKeyConfig{Region: schemas.NewSecretVar("us-east-1")},
			},
			body:    `{"weight":1,"bedrock_mantle_key_config":{"region":"us-east-1","endpoints":{"mantle":"evil.example.com"}}}`,
			want403: true,
		},
		{
			name:     "update github copilot github_domain",
			provider: schemas.GithubCopilot,
			stored: &schemas.Key{
				ID: "key-1", Name: "copilot-key", Weight: 1,
				Value: *schemas.NewSecretVar("ghu_copilot_token"),
				GithubCopilotKeyConfig: &schemas.GithubCopilotKeyConfig{
					AppID:          *schemas.NewSecretVar("123"),
					InstallationID: *schemas.NewSecretVar("456"),
					RepositoryID:   *schemas.NewSecretVar("789"),
					PrivateKey:     *schemas.NewSecretVar(copilotPEM),
					GithubDomain:   *schemas.NewSecretVar("acme.ghe.com"),
				},
			},
			body:    `{"value":"ghu_copilot_token","weight":1,"github_copilot_key_config":` + copilotConfig("evil.example.com") + `}`,
			want403: true,
		},
		{
			name:     "update azure key adding a per-alias endpoint",
			provider: schemas.Azure,
			stored: &schemas.Key{
				ID: "key-1", Name: "azure-key", Weight: 1,
				Value:          *schemas.NewSecretVar("azure-api-key"),
				AzureKeyConfig: &schemas.AzureKeyConfig{Endpoint: *schemas.NewSecretVar("https://myres.openai.azure.com")},
			},
			body:    `{"value":"azure-api-key","weight":1,"azure_key_config":{"endpoint":"https://myres.openai.azure.com"},"aliases":{"gpt-4o":{"model_id":"gpt-4o","endpoint":"https://evil.example.com"}}}`,
			want403: true,
		},
		{
			name:      "create vertex key with a credential json",
			provider:  schemas.Vertex,
			body:      `{"name":"vertex-key","weight":1,"vertex_key_config":` + vertexConfig(vertexServiceAccountJSON) + `}`,
			want403:   true,
			wantNamed: "vertex_key_config.auth_credentials",
		},
		{
			name:      "update vertex key swapping the credential json for an external account",
			provider:  schemas.Vertex,
			stored:    vertexStored(),
			body:      `{"weight":1,"vertex_key_config":` + vertexConfig(vertexExternalAccountJSON) + `}`,
			want403:   true,
			wantNamed: "vertex_key_config.auth_credentials",
		},
		{
			name:     "update vertex key removing the credential json",
			provider: schemas.Vertex,
			stored:   vertexStored(),
			body:     `{"weight":1,"vertex_key_config":{"project_id":"p","region":"us-central1","auth_credentials":""}}`,
			want403:  true,
		},
		{
			name:     "update vertex key keeping its stored credential json",
			provider: schemas.Vertex,
			stored:   vertexStored(),
			body:     `{"weight":2,"vertex_key_config":` + vertexConfig(vertexServiceAccountJSON) + `}`,
			want403:  false,
		},
		{
			name:        "genuinely authenticated update vertex key swapping the credential json",
			provider:    schemas.Vertex,
			stored:      vertexStored(),
			genuineAuth: true,
			body:        `{"weight":1,"vertex_key_config":` + vertexConfig(vertexExternalAccountJSON) + `}`,
			want403:     false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			providerConfig := configstore.ProviderConfig{}
			if tc.stored != nil {
				providerConfig.Keys = []schemas.Key{*tc.stored}
			}
			h := &ProviderHandler{
				inMemoryStore: &lib.Config{Providers: map[schemas.ModelProvider]configstore.ProviderConfig{tc.provider: providerConfig}},
				modelsManager: &mockModelsManager{},
			}
			attachBifrostClient(t, h.inMemoryStore)

			ctx := newTestRequestCtx(tc.body)
			ctx.SetUserValue("provider", string(tc.provider))
			if !tc.genuineAuth {
				ctx.SetUserValue(schemas.BifrostContextKeyAuthBypassed, true)
			}
			if tc.stored != nil {
				ctx.SetUserValue("key_id", tc.stored.ID)
				h.updateProviderKey(ctx)
			} else {
				h.createProviderKey(ctx)
			}

			got403 := ctx.Response.StatusCode() == fasthttp.StatusForbidden
			if got403 != tc.want403 {
				t.Fatalf("got status %d, want403=%v; body=%s", ctx.Response.StatusCode(), tc.want403, ctx.Response.Body())
			}
			if tc.wantNamed != "" && !strings.Contains(string(ctx.Response.Body()), tc.wantNamed) {
				t.Fatalf("the refusal must name %s, got %s", tc.wantNamed, ctx.Response.Body())
			}
			keys := h.inMemoryStore.Providers[tc.provider].Keys
			switch {
			case tc.want403 && tc.stored != nil:
				if len(keys) != 1 || !reflect.DeepEqual(keys[0], *tc.stored) {
					t.Fatalf("expected stored key unchanged after 403, got %+v", keys)
				}
			case tc.want403:
				if len(keys) != 0 {
					t.Fatalf("expected no key persisted after 403, got %d", len(keys))
				}
			default:
				if ctx.Response.StatusCode() != fasthttp.StatusOK || len(keys) != 1 {
					t.Fatalf("expected key persisted with 200, got status %d and %d keys; body=%s", ctx.Response.StatusCode(), len(keys), ctx.Response.Body())
				}
			}
		})
	}
}

// TestProviderKeyURL_RejectsLinkLocalDestination pins that a key-level server URL
// (Ollama/SGL/VLLM) is held to the same destination rule as a provider base URL, even for a
// genuinely authenticated admin: link-local and unspecified addresses are refused with 400,
// while loopback and private hosts - the documented self-hosted setup - stay allowed.
func TestProviderKeyURL_RejectsLinkLocalDestination(t *testing.T) {
	SetLogger(&mockLogger{})
	lib.SetLogger(&mockLogger{})

	const storedURL = "http://localhost:11434"
	cases := []struct {
		name       string
		stored     bool
		body       string
		wantStatus int
	}{
		{name: "create with link-local url", body: `{"name":"ollama-key","weight":1,"ollama_key_config":{"url":"http://169.254.169.254/"}}`, wantStatus: fasthttp.StatusBadRequest},
		{name: "create with unspecified url", body: `{"name":"ollama-key","weight":1,"ollama_key_config":{"url":"http://0.0.0.0:11434"}}`, wantStatus: fasthttp.StatusBadRequest},
		{name: "create with private url", body: `{"name":"ollama-key","weight":1,"ollama_key_config":{"url":"http://10.0.0.5:11434"}}`, wantStatus: fasthttp.StatusOK},
		{name: "update to link-local url", stored: true, body: `{"weight":1,"ollama_key_config":{"url":"http://169.254.169.254/"}}`, wantStatus: fasthttp.StatusBadRequest},
		{name: "update to loopback url", stored: true, body: `{"weight":1,"ollama_key_config":{"url":"http://127.0.0.1:11435"}}`, wantStatus: fasthttp.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			providerConfig := configstore.ProviderConfig{}
			if tc.stored {
				providerConfig.Keys = []schemas.Key{{
					ID: "key-1", Name: "ollama-key", Weight: 1,
					OllamaKeyConfig: &schemas.OllamaKeyConfig{URL: *schemas.NewSecretVar(storedURL)},
				}}
			}
			h := &ProviderHandler{
				inMemoryStore: &lib.Config{Providers: map[schemas.ModelProvider]configstore.ProviderConfig{schemas.Ollama: providerConfig}},
				modelsManager: &mockModelsManager{},
			}
			attachBifrostClient(t, h.inMemoryStore)

			ctx := newTestRequestCtx(tc.body)
			ctx.SetUserValue("provider", string(schemas.Ollama))
			if tc.stored {
				ctx.SetUserValue("key_id", "key-1")
				h.updateProviderKey(ctx)
			} else {
				h.createProviderKey(ctx)
			}

			if ctx.Response.StatusCode() != tc.wantStatus {
				t.Fatalf("status got %d, want %d; body=%s", ctx.Response.StatusCode(), tc.wantStatus, ctx.Response.Body())
			}
			keys := h.inMemoryStore.Providers[schemas.Ollama].Keys
			switch {
			case tc.wantStatus != fasthttp.StatusOK && tc.stored:
				if len(keys) != 1 || keys[0].OllamaKeyConfig.URL.GetValue() != storedURL {
					t.Fatalf("expected stored url unchanged after 400, got %+v", keys)
				}
			case tc.wantStatus != fasthttp.StatusOK:
				if len(keys) != 0 {
					t.Fatalf("expected no key persisted after 400, got %d", len(keys))
				}
			default:
				if len(keys) != 1 {
					t.Fatalf("expected one key persisted, got %d", len(keys))
				}
			}
		})
	}
}

// TestProviderKeyRegion_RejectsHostShapedValues pins that a key-level or per-alias region can
// only name a vendor region. Region is interpolated into the provider host
// (<region>-aiplatform.googleapis.com, <service>.<region>.amazonaws.com), so a host-shaped value
// would carry the key's credential to a caller-chosen host while region stays outside the
// endpoint guard (a region-only Bedrock key must remain creatable without an admin session).
func TestProviderKeyRegion_RejectsHostShapedValues(t *testing.T) {
	SetLogger(&mockLogger{})
	lib.SetLogger(&mockLogger{})

	cases := []struct {
		name       string
		provider   schemas.ModelProvider
		body       string
		wantStatus int
	}{
		{name: "bedrock valid region", provider: schemas.Bedrock, body: `{"name":"k","weight":1,"bedrock_key_config":{"access_key":"AKIAEXAMPLE","secret_key":"secret","region":"us-gov-west-1"}}`, wantStatus: fasthttp.StatusOK},
		{name: "bedrock host-shaped region", provider: schemas.Bedrock, body: `{"name":"k","weight":1,"bedrock_key_config":{"access_key":"AKIAEXAMPLE","secret_key":"secret","region":"evil.example/#"}}`, wantStatus: fasthttp.StatusBadRequest},
		{name: "bedrock region with userinfo", provider: schemas.Bedrock, body: `{"name":"k","weight":1,"bedrock_key_config":{"access_key":"AKIAEXAMPLE","secret_key":"secret","region":"us-east-1@evil.example"}}`, wantStatus: fasthttp.StatusBadRequest},
		{name: "bedrock alias host-shaped region", provider: schemas.Bedrock, body: `{"name":"k","weight":1,"bedrock_key_config":{"access_key":"AKIAEXAMPLE","secret_key":"secret","region":"us-east-1"},"aliases":{"claude":{"model_id":"anthropic.claude-3","region":"evil.example/#"}}}`, wantStatus: fasthttp.StatusBadRequest},
		{name: "bedrock alias valid region", provider: schemas.Bedrock, body: `{"name":"k","weight":1,"bedrock_key_config":{"access_key":"AKIAEXAMPLE","secret_key":"secret","region":"us-east-1"},"aliases":{"claude":{"model_id":"anthropic.claude-3","region":"eu-west-1"}}}`, wantStatus: fasthttp.StatusOK},
		{name: "vertex host-shaped region", provider: schemas.Vertex, body: `{"name":"k","weight":1,"vertex_key_config":{"project_id":"p","region":"attacker.example/x?","auth_credentials":"{}"}}`, wantStatus: fasthttp.StatusBadRequest},
		{name: "mantle host-shaped region", provider: schemas.BedrockMantle, body: `{"name":"k","weight":1,"bedrock_mantle_key_config":{"region":"evil.example/#"}}`, wantStatus: fasthttp.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &ProviderHandler{
				inMemoryStore: &lib.Config{Providers: map[schemas.ModelProvider]configstore.ProviderConfig{tc.provider: {}}},
				modelsManager: &mockModelsManager{},
			}
			attachBifrostClient(t, h.inMemoryStore)
			ctx := newTestRequestCtx(tc.body)
			ctx.SetUserValue("provider", string(tc.provider))
			h.createProviderKey(ctx)
			if ctx.Response.StatusCode() != tc.wantStatus {
				t.Fatalf("status got %d, want %d; body=%s", ctx.Response.StatusCode(), tc.wantStatus, ctx.Response.Body())
			}
			if tc.wantStatus == fasthttp.StatusBadRequest {
				if !strings.Contains(string(ctx.Response.Body()), "region") {
					t.Fatalf("rejection must name the region field, got %s", ctx.Response.Body())
				}
				if len(h.inMemoryStore.Providers[tc.provider].Keys) != 0 {
					t.Fatal("a rejected key must not be persisted")
				}
			}
		})
	}
}

// One RSA key for the whole file. Generating them is slow enough to notice per case.
var (
	handlerKeyOnce sync.Once
	handlerRSAKey  *rsa.PrivateKey
)

func handlerRSA(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	handlerKeyOnce.Do(func() {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		handlerRSAKey = key
	})
	return handlerRSAKey
}

func pkcs1TestPEM(t *testing.T) string {
	t.Helper()
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(handlerRSA(t))}))
}

func pkcs8TestPEM(t *testing.T) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(handlerRSA(t))
	if err != nil {
		t.Fatalf("marshal pkcs8: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

// ecCurveTestPEM returns an EC private key on the given curve, as SEC1 or as PKCS#8.
func ecCurveTestPEM(t *testing.T, curve elliptic.Curve, pkcs8 bool) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(curve, rand.Reader)
	if err != nil {
		t.Fatalf("generate ec: %v", err)
	}
	if pkcs8 {
		der, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			t.Fatalf("marshal pkcs8: %v", err)
		}
		return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal ec: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}))
}

func ecTestPEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate ec: %v", err)
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal ec: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}))
}

// TestMergeUpdatedKey_MaskedQueryURLRestoresStoredURL: a service URL carrying a
// query credential is shown masked; sending that masked value back on update must
// keep the stored URL, credential included, rather than store the mask.
func TestMergeUpdatedKey_MaskedQueryURLRestoresStoredURL(t *testing.T) {
	h := &ProviderHandler{}
	stored := schemas.Key{
		ID: "k", Name: "vllm", Value: *schemas.NewSecretVar(""),
		VLLMKeyConfig: &schemas.VLLMKeyConfig{URL: *schemas.NewSecretVar("https://vllm.example/v1?token=sentinel-7f3a"), ModelName: "m"},
	}
	masked := stored.VLLMKeyConfig.URL.RedactedServiceURL()
	require.True(t, masked.IsMaskedPlaceholder())
	require.NotContains(t, masked.GetValue(), "sentinel-7f3a")
	update := stored
	cfg := *stored.VLLMKeyConfig
	cfg.URL = *masked
	update.VLLMKeyConfig = &cfg
	merged, err := h.mergeUpdatedKey(stored, update)
	require.NoError(t, err)
	require.Equal(t, "https://vllm.example/v1?token=sentinel-7f3a", merged.VLLMKeyConfig.URL.GetValue())
}

// TestMergeUpdatedKey_VertexAWSWorkloadIdentityRoleARN covers the one masked field inside the
// Vertex federation block. GET renders aws_role_arn as a mask, so an edit that leaves it alone
// sends the mask back; the merge must restore the stored ARN and must never persist the mask.
// The identifiers in the block (audience, service account, region) are returned in clear and
// therefore pass through untouched.
func TestMergeUpdatedKey_VertexAWSWorkloadIdentityRoleARN(t *testing.T) {
	h := &ProviderHandler{}
	secret := func(value string) schemas.SecretVar { return *schemas.NewSecretVar(value) }
	secretPtr := func(value string) *schemas.SecretVar { return schemas.NewSecretVar(value) }
	mask := "arn:" + strings.Repeat("*", 24) + "Hop0"

	stored := schemas.Key{
		VertexKeyConfig: &schemas.VertexKeyConfig{
			ProjectID: secret("my-project"),
			Region:    secret("us-central1"),
			AWSWorkloadIdentity: &schemas.VertexAWSWorkloadIdentityConfig{
				Audience:            secret("//iam.googleapis.com/projects/1/locations/global/workloadIdentityPools/p/providers/aws"),
				ServiceAccountEmail: secretPtr("vertex@my-project.iam.gserviceaccount.com"),
				AWSRoleARN:          secretPtr("arn:aws:iam::123456789012:role/VertexHop"),
			},
		},
	}
	update := schemas.Key{
		VertexKeyConfig: &schemas.VertexKeyConfig{
			ProjectID: secret("my-project"),
			Region:    secret("us-central1"),
			AWSWorkloadIdentity: &schemas.VertexAWSWorkloadIdentityConfig{
				Audience:            secret("//iam.googleapis.com/projects/1/locations/global/workloadIdentityPools/p/providers/aws"),
				ServiceAccountEmail: secretPtr("other@my-project.iam.gserviceaccount.com"),
				AWSRoleARN:          secretPtr(mask),
			},
		},
	}

	merged, err := h.mergeUpdatedKey(stored, update)
	if err != nil {
		t.Fatalf("mergeUpdatedKey returned error: %v", err)
	}
	if got := merged.VertexKeyConfig.AWSWorkloadIdentity.AWSRoleARN.GetValue(); got != "arn:aws:iam::123456789012:role/VertexHop" {
		t.Errorf("expected stored aws_role_arn to be preserved, got %q", got)
	}
	if got := merged.VertexKeyConfig.AWSWorkloadIdentity.ServiceAccountEmail.GetValue(); got != "other@my-project.iam.gserviceaccount.com" {
		t.Errorf("expected edited service_account_email to be applied, got %q", got)
	}

	// A mask with nothing stored behind it is a client bug and must be rejected, not persisted.
	// The first merge rewrote the update's pointer in place, so send a fresh mask.
	stored.VertexKeyConfig.AWSWorkloadIdentity = nil
	update.VertexKeyConfig.AWSWorkloadIdentity.AWSRoleARN = secretPtr(mask)
	if _, err := h.mergeUpdatedKey(stored, update); err == nil {
		t.Fatal("expected masked aws_role_arn without a stored value to be rejected")
	}

	// Omitting the role ARN entirely (direct access, no hop) must not trip the guard.
	update.VertexKeyConfig.AWSWorkloadIdentity.AWSRoleARN = nil
	if _, err := h.mergeUpdatedKey(stored, update); err != nil {
		t.Fatalf("expected federation block without aws_role_arn to merge cleanly, got: %v", err)
	}
}

// TestValidateVertexKeyAuth pins the API-side counterpart of the config.schema.json rule: a Vertex key
// must not carry both an aws_workload_identity block and a non-empty auth_credentials JSON. Without
// this check the API silently let federation take precedence over a credentials JSON the operator
// also supplied, while config.json rejected the same combination.
func TestValidateVertexKeyAuth(t *testing.T) {
	secret := func(v string) schemas.SecretVar { return *schemas.NewSecretVar(v) }
	wif := &schemas.VertexAWSWorkloadIdentityConfig{Audience: secret("//iam.googleapis.com/projects/1/locations/global/workloadIdentityPools/p/providers/aws")}
	vertex := func(cfg *schemas.VertexKeyConfig) schemas.Key { return schemas.Key{VertexKeyConfig: cfg} }

	err := validateVertexKeyAuth(schemas.Vertex, vertex(&schemas.VertexKeyConfig{
		ProjectID: secret("p"), Region: secret("r"),
		AuthCredentials:     secret(`{"type":"service_account"}`),
		AWSWorkloadIdentity: wif,
	}))
	if err == nil {
		t.Fatal("expected auth_credentials alongside aws_workload_identity to be rejected")
	}
	for _, want := range []string{"auth_credentials", "aws_workload_identity"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should name %s", err.Error(), want)
		}
	}

	// An env reference for the credentials is just as much a second identity.
	t.Setenv("TEST_VERTEX_CREDS", `{"type":"service_account"}`)
	if err := validateVertexKeyAuth(schemas.Vertex, vertex(&schemas.VertexKeyConfig{
		ProjectID: secret("p"), Region: secret("r"),
		AuthCredentials: secret("env.TEST_VERTEX_CREDS"), AWSWorkloadIdentity: wif,
	})); err == nil {
		t.Fatal("expected env-referenced auth_credentials alongside aws_workload_identity to be rejected")
	}

	// A present block without an audience is a broken federation config, not "no federation": the
	// configuration schema requires audience whenever the block exists, and so does the API.
	for name, block := range map[string]*schemas.VertexAWSWorkloadIdentityConfig{
		"empty block":    {},
		"email only":     {ServiceAccountEmail: schemas.NewSecretVar("sa@p.iam.gserviceaccount.com")},
		"empty audience": {Audience: secret("")},
	} {
		err := validateVertexKeyAuth(schemas.Vertex, vertex(&schemas.VertexKeyConfig{ProjectID: secret("p"), Region: secret("r"), AWSWorkloadIdentity: block}))
		if err == nil || !strings.Contains(err.Error(), "audience") {
			t.Errorf("%s: expected an audience error, got %v", name, err)
		}
	}
	// token_lifetime_seconds mirrors the configuration schema bounds; zero means "not set".
	for _, lifetime := range []int{599, 43201, -1} {
		err := validateVertexKeyAuth(schemas.Vertex, vertex(&schemas.VertexKeyConfig{ProjectID: secret("p"), Region: secret("r"),
			AWSWorkloadIdentity: &schemas.VertexAWSWorkloadIdentityConfig{Audience: wif.Audience, TokenLifetimeSeconds: lifetime}}))
		if err == nil || !strings.Contains(err.Error(), "token_lifetime_seconds") {
			t.Errorf("lifetime %d: expected a token_lifetime_seconds error, got %v", lifetime, err)
		}
	}

	accepted := []schemas.Key{
		vertex(&schemas.VertexKeyConfig{ProjectID: secret("p"), Region: secret("r"), AWSWorkloadIdentity: wif}),
		vertex(&schemas.VertexKeyConfig{ProjectID: secret("p"), Region: secret("r"), AuthCredentials: secret(""), AWSWorkloadIdentity: wif}),
		vertex(&schemas.VertexKeyConfig{ProjectID: secret("p"), Region: secret("r"), AuthCredentials: secret(`{"type":"service_account"}`)}),
		vertex(&schemas.VertexKeyConfig{ProjectID: secret("p"), Region: secret("r")}),
		vertex(&schemas.VertexKeyConfig{ProjectID: secret("p"), Region: secret("r"), AWSWorkloadIdentity: &schemas.VertexAWSWorkloadIdentityConfig{Audience: wif.Audience, TokenLifetimeSeconds: 600}}),
		vertex(&schemas.VertexKeyConfig{ProjectID: secret("p"), Region: secret("r"), AWSWorkloadIdentity: &schemas.VertexAWSWorkloadIdentityConfig{Audience: wif.Audience, TokenLifetimeSeconds: 43200}}),
		{Value: secret("api-key")},
	}
	for i, key := range accepted {
		if err := validateVertexKeyAuth(schemas.Vertex, key); err != nil {
			t.Errorf("accepted case %d rejected: %v", i, err)
		}
	}
	// Other providers never carry the block and are never touched.
	if err := validateVertexKeyAuth(schemas.OpenAI, vertex(&schemas.VertexKeyConfig{AuthCredentials: secret("x"), AWSWorkloadIdentity: wif})); err != nil {
		t.Errorf("non-vertex provider must not be validated: %v", err)
	}
}

// TestRejectUnknownOAuthKeyConfigFields pins API parity with config.schema.json, which sets
// additionalProperties:false on oauth_key_config: sonic drops unknown fields, so a misspelled
// "scope" would otherwise save a block that never requests its scopes.
func TestRejectUnknownOAuthKeyConfigFields(t *testing.T) {
	valid := `"grant_type":"client_credentials","token_url":"https://idp.example/oauth2/token","client_id":"id","client_secret":"s"`
	cases := []struct {
		name    string
		body    string
		wantErr string
	}{
		{"no block", `{"value":"sk","weight":1}`, ""},
		{"null block", `{"value":"sk","oauth_key_config":null}`, ""},
		{"known fields only", `{"oauth_key_config":{` + valid + `,"scopes":["inference"],"extra_params":{"resource":"r"}}}`, ""},
		{"secret references", `{"oauth_key_config":{` + valid + `,"client_secret":{"ref":"OAUTH_SECRET","type":"env"}}}`, ""},
		{"misspelled scopes", `{"oauth_key_config":{` + valid + `,"scope":["inference"]}}`, `unknown field "scope"`},
		{"unknown nested field", `{"oauth_key_config":{` + valid + `,"client_secrett":"s"}}`, `unknown field "client_secrett"`},
		{"unknown field outside the block is not this check's job", `{"oauth_key_config":{` + valid + `},"colour":"blue"}`, ""},
		{"not an object", `null`, ""},
		// The decoder takes these spellings into the same field, so they get the same check.
		{"case-variant block", `{"OAUTH_KEY_CONFIG":{` + valid + `,"scope":["inference"]}}`, `unknown field "scope"`},
		{"title-case block", `{"Oauth_Key_Config":{` + valid + `,"scope":["inference"]}}`, `unknown field "scope"`},
		{"Kelvin sign in the name", `{"oauth_\u212aey_config":{` + valid + `,"scope":["inference"]}}`, `unknown field "scope"`},
		{"escaped name", `{"\u006fauth_key_config":{` + valid + `,"scope":["inference"]}}`, `unknown field "scope"`},
		{"case-variant block with known fields", `{"OAUTH_KEY_CONFIG":{` + valid + `}}`, ""},
		{"null case-variant block", `{"OAUTH_KEY_CONFIG":null}`, ""},
		{"bad block before a good one", `{"OAUTH_KEY_CONFIG":{` + valid + `,"scope":["inference"]},"oauth_key_config":{` + valid + `}}`, `unknown field "scope"`},
		{"bad block after a good one", `{"oauth_key_config":{` + valid + `},"OAUTH_KEY_CONFIG":{` + valid + `,"scope":["inference"]}}`, `unknown field "scope"`},
		{"bad block after a null one", `{"oauth_key_config":null,"OAUTH_KEY_CONFIG":{` + valid + `,"scope":["inference"]}}`, `unknown field "scope"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := rejectUnknownOAuthKeyConfigFields([]byte(tc.body))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error got %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// TestProviderKeyWrites_RejectUnknownOAuthKeyConfigFields pins that both write paths refuse a
// block carrying a field the schema does not know, naming the field, instead of saving it
// without that setting.
func TestProviderKeyWrites_RejectUnknownOAuthKeyConfigFields(t *testing.T) {
	SetLogger(&mockLogger{})
	lib.SetLogger(&mockLogger{})
	block := `{"grant_type":"client_credentials","token_url":"https://idp.example/oauth2/token","client_id":"id","client_secret":"s","scope":["inference"]}`

	t.Run("create", func(t *testing.T) {
		h := &ProviderHandler{
			inMemoryStore: &lib.Config{Providers: map[schemas.ModelProvider]configstore.ProviderConfig{schemas.OpenAI: {}}},
			modelsManager: &mockModelsManager{},
		}
		attachBifrostClient(t, h.inMemoryStore)
		ctx := newTestRequestCtx(`{"name":"oauth","weight":1,"oauth_key_config":` + block + `}`)
		ctx.SetUserValue("provider", string(schemas.OpenAI))
		h.createProviderKey(ctx)
		if ctx.Response.StatusCode() != fasthttp.StatusBadRequest {
			t.Fatalf("status got %d, want 400; body=%s", ctx.Response.StatusCode(), ctx.Response.Body())
		}
		// The JSON error body escapes the quotes around the field name.
		if body := string(ctx.Response.Body()); !strings.Contains(body, `oauth_key_config: unknown field \"scope\"`) {
			t.Fatalf("the response must name the unknown field, got %s", body)
		}
	})

	t.Run("update", func(t *testing.T) {
		h := &ProviderHandler{
			inMemoryStore: &lib.Config{Providers: map[schemas.ModelProvider]configstore.ProviderConfig{
				schemas.OpenAI: {Keys: []schemas.Key{{ID: "key-1", Name: "oauth", Weight: 1, OAuthKeyConfig: &schemas.OAuthKeyConfig{
					GrantType:    schemas.OAuthGrantClientCredentials,
					TokenURL:     *schemas.NewSecretVar("https://idp.example/oauth2/token"),
					ClientID:     schemas.NewSecretVar("id"),
					ClientSecret: schemas.NewSecretVar("s"),
				}}}},
			}},
			modelsManager: &mockModelsManager{},
		}
		attachBifrostClient(t, h.inMemoryStore)
		ctx := newTestRequestCtx(`{"oauth_key_config":` + block + `}`)
		ctx.SetUserValue("provider", string(schemas.OpenAI))
		ctx.SetUserValue("key_id", "key-1")
		h.updateProviderKey(ctx)
		if ctx.Response.StatusCode() != fasthttp.StatusBadRequest {
			t.Fatalf("status got %d, want 400; body=%s", ctx.Response.StatusCode(), ctx.Response.Body())
		}
		// The JSON error body escapes the quotes around the field name.
		if body := string(ctx.Response.Body()); !strings.Contains(body, `oauth_key_config: unknown field \"scope\"`) {
			t.Fatalf("the response must name the unknown field, got %s", body)
		}
		if stored := h.inMemoryStore.Providers[schemas.OpenAI].Keys[0]; len(stored.OAuthKeyConfig.Scopes) != 0 || stored.OAuthKeyConfig.ClientID.GetValue() != "id" {
			t.Fatalf("the stored key must be untouched, got %+v", stored.OAuthKeyConfig)
		}
	})
}

// The synthetic server secret the TXE-4989 tests place in the environment. Tests assert on
// whether it was delivered or echoed and never print it.
const (
	txe4989SecretEnv = "TXE4989_SYNTHETIC_SERVER_SECRET"
	txe4989Secret    = "txe4989-synthetic-server-secret-0123456789"
	txe4989OtherEnv  = "TXE4989_SYNTHETIC_OTHER_SECRET"
	txe4989Other     = "txe4989-synthetic-other-secret-9876543210"
)

// TestCreateProviderKey_BypassedSecretReferenceNeverReachesTheProvider is the end-to-end form
// of TXE-4989: an administrator configured a custom provider whose base URL is an owned loopback
// target, and a caller let through only because dashboard auth is off creates a key whose value
// is an env. reference to a server secret. The create must be refused and inference must not
// carry the secret. The genuine-admin control proves the target does see the secret when the
// same key is legitimately configured, so the bypassed case cannot pass by delivering nothing.
func TestCreateProviderKey_BypassedSecretReferenceNeverReachesTheProvider(t *testing.T) {
	SetLogger(&mockLogger{})
	lib.SetLogger(&mockLogger{})
	t.Setenv(txe4989SecretEnv, txe4989Secret)

	for _, tc := range []struct {
		name          string
		bypassed      bool
		wantStatus    int
		wantDelivered int64
	}{
		{name: "auth bypassed", bypassed: true, wantStatus: fasthttp.StatusForbidden, wantDelivered: 0},
		{name: "genuine admin", bypassed: false, wantStatus: fasthttp.StatusOK, wantDelivered: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests, delivered atomic.Int64
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if strings.Contains(r.Header.Get("Authorization"), txe4989Secret) {
					delivered.Add(1)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","created":1,"model":"gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
			}))
			defer target.Close()

			const provider = schemas.ModelProvider("owned-openai")
			store := &lib.Config{
				ClientConfig: &configstore.ClientConfig{},
				Providers: map[schemas.ModelProvider]configstore.ProviderConfig{
					provider: {
						NetworkConfig:            &schemas.NetworkConfig{BaseURL: target.URL, AllowPrivateNetwork: true, DefaultRequestTimeoutInSeconds: 10},
						ConcurrencyAndBufferSize: &schemas.ConcurrencyAndBufferSize{Concurrency: 1, BufferSize: 4},
						CustomProviderConfig:     &schemas.CustomProviderConfig{BaseProviderType: schemas.OpenAI},
					},
				},
			}
			client, err := bifrost.Init(context.Background(), schemas.BifrostConfig{Account: lib.NewBaseAccount(store), Logger: bifrost.NewDefaultLogger(schemas.LogLevelError)})
			if err != nil {
				t.Fatalf("init: %v", err)
			}
			t.Cleanup(client.Shutdown)
			store.SetBifrostClient(client)
			h := &ProviderHandler{inMemoryStore: store, modelsManager: &mockModelsManager{}}

			ctx := newTestRequestCtx(`{"name":"referenced","value":"env.` + txe4989SecretEnv + `","models":["*"],"weight":1}`)
			ctx.SetUserValue("provider", string(provider))
			if tc.bypassed {
				ctx.SetUserValue(schemas.BifrostContextKeyAuthBypassed, true)
			}
			h.createProviderKey(ctx)
			// Errorf, not Fatalf: inference below still runs, so a missing guard is reported with
			// what it costs (the target receiving the secret), not only the status.
			if got := ctx.Response.StatusCode(); got != tc.wantStatus {
				t.Errorf("create status got %d, want %d", got, tc.wantStatus)
			}
			if strings.Contains(string(ctx.Response.Body()), txe4989Secret) {
				t.Errorf("the create response disclosed the resolved server secret")
			}
			if tc.bypassed {
				if keys := store.Providers[provider].Keys; len(keys) != 0 {
					t.Errorf("a refused key was stored (%d keys)", len(keys))
				}
			}

			bctx := schemas.NewBifrostContext(context.Background(), time.Now().Add(15*time.Second))
			_, _ = client.ChatCompletionRequest(bctx, &schemas.BifrostChatRequest{Provider: provider, Model: "gpt-4o-mini",
				Input: []schemas.ChatMessage{{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: new("hi")}}}})
			if got := delivered.Load(); got != tc.wantDelivered {
				t.Fatalf("owned target requests carrying the synthetic secret: got %d, want %d (requests %d)", got, tc.wantDelivered, requests.Load())
			}
		})
	}
}

// keySecretSite is one place a SecretVar can sit in a schemas.Key, as the steps that reach it
// from the Key: a field index, or one of the container steps below.
type keySecretSite []int

const (
	siteDeref    = -1 // follow (allocating) a pointer
	siteMapEntry = -2 // the map entry under key "m"
	siteSliceEl  = -3 // the slice's single element
)

// enumerateKeySecretSites lists every SecretVar site in the Key type graph, generated from the
// types themselves so a SecretVar added to any key config later is in the list automatically.
func enumerateKeySecretSites(t *testing.T) []keySecretSite {
	t.Helper()
	var sites []keySecretSite
	seen := map[reflect.Type]bool{}
	var walk func(typ reflect.Type, steps []int)
	walk = func(typ reflect.Type, steps []int) {
		next := func(step int) []int { return append(slices.Clone(steps), step) }
		if typ == secretVarReflectType {
			sites = append(sites, keySecretSite(slices.Clone(steps)))
			return
		}
		switch typ.Kind() {
		case reflect.Pointer:
			walk(typ.Elem(), next(siteDeref))
		case reflect.Struct:
			if seen[typ] {
				return
			}
			seen[typ] = true
			defer delete(seen, typ)
			for i := range typ.NumField() {
				walk(typ.Field(i).Type, next(i))
			}
		case reflect.Map:
			if typ.Key().Kind() != reflect.String {
				t.Fatalf("%s: a key config map with a non-string key needs a path rule in collectSecretRefs", typ)
			}
			walk(typ.Elem(), next(siteMapEntry))
		case reflect.Slice:
			walk(typ.Elem(), next(siteSliceEl))
		case reflect.Array, reflect.Interface:
			if typeReachesSecretVar(typ, map[reflect.Type]bool{}) {
				t.Fatalf("%s: arrays and interfaces holding a SecretVar need a site rule here", typ)
			}
		}
	}
	walk(reflect.TypeOf(schemas.Key{}), nil)
	return sites
}

func typeReachesSecretVar(typ reflect.Type, seen map[reflect.Type]bool) bool {
	return typeReaches(typ, secretVarReflectType, seen)
}

// typeReaches reports whether a value of typ can hold a target through fields, pointers,
// slices, arrays or map values.
func typeReaches(typ, target reflect.Type, seen map[reflect.Type]bool) bool {
	if typ == target {
		return true
	}
	if seen[typ] {
		return false
	}
	seen[typ] = true
	switch typ.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
		return typeReaches(typ.Elem(), target, seen)
	case reflect.Struct:
		for i := range typ.NumField() {
			if typeReaches(typ.Field(i).Type, target, seen) {
				return true
			}
		}
	}
	return false
}

// TestProviderWritesCarryNoKeys pins why provider create and update need no key secret
// reference guard of their own (TXE-4989): neither payload type can hold a schemas.Key, so a
// "keys" array sent to POST /api/providers is dropped at decode and PUT /api/providers/{provider}
// refuses one. If either payload gains keys, it must run requireGenuineAuthForKeySecretRefs on
// them first.
func TestProviderWritesCarryNoKeys(t *testing.T) {
	SetLogger(&mockLogger{})
	lib.SetLogger(&mockLogger{})
	t.Setenv(txe4989SecretEnv, txe4989Secret)
	keyType := reflect.TypeOf(schemas.Key{})
	for _, payload := range []any{providerCreatePayload{}, providerUpdatePayload{}} {
		if typ := reflect.TypeOf(payload); typeReaches(typ, keyType, map[reflect.Type]bool{}) {
			t.Fatalf("%s can carry a provider key; guard its secret references under the auth bypass", typ)
		}
	}
	keys := `"keys":[{"id":"k","name":"k","value":"env.` + txe4989SecretEnv + `","weight":1}]`

	t.Run("create drops keys", func(t *testing.T) {
		h := &ProviderHandler{
			inMemoryStore: &lib.Config{ClientConfig: &configstore.ClientConfig{}, Providers: map[schemas.ModelProvider]configstore.ProviderConfig{}},
			modelsManager: &mockModelsManager{},
		}
		attachBifrostClient(t, h.inMemoryStore)
		ctx := newTestRequestCtx(`{"provider":"custom-openai","custom_provider_config":{"base_provider_type":"openai"},` + keys + `}`)
		ctx.SetUserValue(schemas.BifrostContextKeyAuthBypassed, true)
		h.addProvider(ctx)
		if strings.Contains(string(ctx.Response.Body()), txe4989Secret) {
			t.Fatalf("the response disclosed the resolved server secret")
		}
		if config, ok := h.inMemoryStore.Providers["custom-openai"]; ok && len(config.Keys) != 0 {
			t.Fatalf("provider create stored %d keys from the body", len(config.Keys))
		}
	})

	t.Run("update refuses keys", func(t *testing.T) {
		h := &ProviderHandler{
			inMemoryStore: &lib.Config{ClientConfig: &configstore.ClientConfig{}, Providers: map[schemas.ModelProvider]configstore.ProviderConfig{
				schemas.OpenAI: {Keys: []schemas.Key{{ID: "key-existing", Value: *schemas.NewSecretVar("sk-stored-literal"), Weight: 1}}},
			}},
			modelsManager: &mockModelsManager{},
		}
		ctx := newTestRequestCtx(`{"concurrency_and_buffer_size":{"concurrency":1,"buffer_size":4},` + keys + `}`)
		ctx.SetUserValue("provider", string(schemas.OpenAI))
		ctx.SetUserValue(schemas.BifrostContextKeyAuthBypassed, true)
		h.updateProvider(ctx)
		if ctx.Response.StatusCode() != fasthttp.StatusBadRequest || !strings.Contains(string(ctx.Response.Body()), "keys are not accepted") {
			t.Fatalf("status %d, want 400 from the keys guard: %s", ctx.Response.StatusCode(), ctx.Response.Body())
		}
		if stored := h.inMemoryStore.Providers[schemas.OpenAI].Keys; len(stored) != 1 || len(keySecretRefs(&stored[0])) != 0 {
			t.Fatalf("provider update changed the stored keys")
		}
	})
}

// keyWithSecretAt returns a Key holding secret at site and nothing else set on the way there.
func keyWithSecretAt(site keySecretSite, secret schemas.SecretVar) *schemas.Key {
	key := &schemas.Key{}
	var set func(v reflect.Value, steps []int)
	set = func(v reflect.Value, steps []int) {
		if len(steps) == 0 {
			v.Set(reflect.ValueOf(secret))
			return
		}
		switch step := steps[0]; step {
		case siteDeref:
			if v.IsNil() {
				v.Set(reflect.New(v.Type().Elem()))
			}
			set(v.Elem(), steps[1:])
		case siteMapEntry:
			if v.IsNil() {
				v.Set(reflect.MakeMap(v.Type()))
			}
			elem := reflect.New(v.Type().Elem()).Elem()
			set(elem, steps[1:])
			v.SetMapIndex(reflect.ValueOf("m").Convert(v.Type().Key()), elem)
		case siteSliceEl:
			elem := reflect.New(v.Type().Elem()).Elem()
			set(elem, steps[1:])
			v.Set(reflect.Append(v, elem))
		default:
			set(v.Field(step), steps[1:])
		}
	}
	set(reflect.ValueOf(key).Elem(), site)
	return key
}

func secretRef(t *testing.T, ref string) schemas.SecretVar {
	t.Helper()
	var secret schemas.SecretVar
	if err := sonic.Unmarshal([]byte(strconv.Quote(ref)), &secret); err != nil {
		t.Fatalf("decode reference: %v", err)
	}
	return secret
}

// TestKeySecretRefs_CoversEveryKeyField pins the path-keyed comparison over every SecretVar
// site of schemas.Key, generated from its type graph: each site is found under its own path,
// no two sites share a path (so a reference moved to another field is never read as
// unchanged), and an unchanged reference, a removal and a literal all pass.
func TestKeySecretRefs_CoversEveryKeyField(t *testing.T) {
	sites := enumerateKeySecretSites(t)
	// Value plus the Azure, Vertex, Bedrock, Bedrock Mantle, vLLM, Ollama, SGL, Databricks,
	// Copilot, Cloudflare, OAuth and per-alias fields. A drop below this means the generator
	// stopped seeing part of the key, not that fields were removed.
	if len(sites) < 40 {
		t.Fatalf("found %d SecretVar sites in schemas.Key, want at least 40", len(sites))
	}
	ref := secretRef(t, "env."+txe4989SecretEnv)
	other := secretRef(t, "env."+txe4989OtherEnv)
	vaultRef := secretRef(t, "vault.txe4989/synthetic")
	literal := *schemas.NewSecretVar("synthetic-literal")

	paths := make([]string, len(sites))
	for i, site := range sites {
		refs := keySecretRefs(keyWithSecretAt(site, ref))
		if len(refs) != 1 {
			t.Fatalf("site %v: got %d references, want 1", site, len(refs))
		}
		for path := range refs {
			paths[i] = path
		}
	}
	for i := range paths {
		for j := range i {
			if paths[i] == paths[j] {
				t.Fatalf("sites %v and %v share the path %s", sites[i], sites[j], paths[i])
			}
		}
	}

	for i, site := range sites {
		t.Run(paths[i], func(t *testing.T) {
			stored := keyWithSecretAt(site, ref)
			if changed := changedKeySecretRefs(nil, stored); !slices.Equal(changed, []string{paths[i]}) {
				t.Fatalf("create: changed %v, want [%s]", changed, paths[i])
			}
			if changed := changedKeySecretRefs(stored, keyWithSecretAt(site, ref)); len(changed) != 0 {
				t.Fatalf("unchanged reference: changed %v", changed)
			}
			if changed := changedKeySecretRefs(stored, &schemas.Key{}); len(changed) != 0 {
				t.Fatalf("removal: changed %v", changed)
			}
			if changed := changedKeySecretRefs(stored, keyWithSecretAt(site, literal)); len(changed) != 0 {
				t.Fatalf("literal: changed %v", changed)
			}
			for name, repointed := range map[string]schemas.SecretVar{"another env var": other, "a vault path": vaultRef} {
				if changed := changedKeySecretRefs(stored, keyWithSecretAt(site, repointed)); !slices.Equal(changed, []string{paths[i]}) {
					t.Fatalf("repointed to %s: changed %v, want [%s]", name, changed, paths[i])
				}
			}
			moved := keyWithSecretAt(sites[(i+1)%len(sites)], ref)
			if changed := changedKeySecretRefs(stored, moved); !slices.Equal(changed, []string{paths[(i+1)%len(sites)]}) {
				t.Fatalf("moved to %s: changed %v", paths[(i+1)%len(sites)], changed)
			}
		})
	}
}

// TestKeySecretRefs_NoUnexportedSecretFields pins the one thing collectSecretRefs skips:
// unexported fields. None of the key's types may hold a SecretVar in one.
func TestKeySecretRefs_NoUnexportedSecretFields(t *testing.T) {
	seen := map[reflect.Type]bool{}
	var check func(typ reflect.Type, path string)
	check = func(typ reflect.Type, path string) {
		if typ == secretVarReflectType || seen[typ] {
			return
		}
		seen[typ] = true
		switch typ.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
			check(typ.Elem(), path)
		case reflect.Struct:
			for i := range typ.NumField() {
				field := typ.Field(i)
				if !field.IsExported() && typeReachesSecretVar(field.Type, map[reflect.Type]bool{}) {
					t.Errorf("%s.%s is unexported and holds a SecretVar the bypass guard cannot see", path, field.Name)
				}
				check(field.Type, path+"."+field.Name)
			}
		}
	}
	check(reflect.TypeOf(schemas.Key{}), "Key")
}

// TestProviderKeyWrites_BypassedSecretReferenceEveryField drives every SecretVar site through
// both key routes under the bypass: a create and an update carrying an env. reference there are
// refused with the site's path, store nothing, and never echo the resolved secret. The body is
// the key as JSON clients send it; the one site with no JSON name (the deprecated per-alias
// Vertex project, json:"-") is sent as the field it is promoted to.
func TestProviderKeyWrites_BypassedSecretReferenceEveryField(t *testing.T) {
	SetLogger(&mockLogger{})
	lib.SetLogger(&mockLogger{})
	ref := secretRef(t, "env."+txe4989SecretEnv) // resolved while the variable is unset
	t.Setenv(txe4989SecretEnv, txe4989Secret)

	for _, site := range enumerateKeySecretSites(t) {
		exact, err := sonic.Marshal(keyWithSecretAt(site, ref))
		if err != nil {
			t.Fatalf("site %v: marshal: %v", site, err)
		}
		// The decoder takes these spellings of a field name, so they reach the same guard.
		for spelling, body := range map[string][]byte{
			"exact":  exact,
			"upper":  renameTopLevelFields(t, exact, strings.ToUpper),
			"kelvin": renameTopLevelFields(t, exact, func(name string) string { return strings.Replace(name, "k", "\u212a", 1) }),
		} {
			for _, create := range []bool{true, false} {
				name := fmt.Sprintf("%v/%s/update", site, spelling)
				if create {
					name = fmt.Sprintf("%v/%s/create", site, spelling)
				}
				t.Run(name, func(t *testing.T) {
					stored := schemas.Key{ID: "key-1", Name: "stored", Value: *schemas.NewSecretVar("sk-stored-literal"), Weight: 1}
					h := &ProviderHandler{
						inMemoryStore: &lib.Config{Providers: map[schemas.ModelProvider]configstore.ProviderConfig{
							schemas.OpenAI: {Keys: []schemas.Key{stored}},
						}},
						modelsManager: &mockModelsManager{},
					}
					attachBifrostClient(t, h.inMemoryStore) // so a write the guard misses is stored, not a nil-client panic
					ctx := newTestRequestCtx(string(body))
					ctx.SetUserValue("provider", string(schemas.OpenAI))
					ctx.SetUserValue(schemas.BifrostContextKeyAuthBypassed, true)
					if create {
						h.createProviderKey(ctx)
					} else {
						ctx.SetUserValue("key_id", "key-1")
						h.updateProviderKey(ctx)
					}
					resp := string(ctx.Response.Body())
					if ctx.Response.StatusCode() != fasthttp.StatusForbidden || !strings.Contains(resp, "server secret reference") {
						t.Fatalf("status %d, want 403 from the secret reference guard (%d-byte body)", ctx.Response.StatusCode(), len(resp))
					}
					if strings.Contains(resp, txe4989Secret) {
						t.Fatalf("the refusal disclosed the resolved server secret")
					}
					keys := h.inMemoryStore.Providers[schemas.OpenAI].Keys
					if len(keys) != 1 || len(keySecretRefs(&keys[0])) != 0 || keys[0].Value.GetValue() != "sk-stored-literal" {
						t.Fatalf("the refused write changed the stored keys")
					}
				})
			}
		}
	}
}

// renameTopLevelFields rewrites a JSON object with rename applied to its top-level names.
func renameTopLevelFields(t *testing.T, body []byte, rename func(string) string) []byte {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := sonic.Unmarshal(body, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	renamed := make(map[string]json.RawMessage, len(fields))
	for name, value := range fields {
		renamed[rename(name)] = value
	}
	out, err := sonic.Marshal(renamed)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return out
}

// TestUpdateProviderKey_RepeatedSpellingsReachTheGuardsAsDecoded: a body that names a field in
// several spellings is judged by what the decoder made of it, whichever spelling came last, so
// no order can carry a server secret reference or a new dial target past the bypass guards.
func TestUpdateProviderKey_RepeatedSpellingsReachTheGuardsAsDecoded(t *testing.T) {
	SetLogger(&mockLogger{})
	lib.SetLogger(&mockLogger{})
	t.Setenv(txe4989SecretEnv, txe4989Secret)
	ref := "env." + txe4989SecretEnv
	const literal = "sk-another-literal-0123456789"

	for _, tc := range []struct {
		name       string
		body       string
		wantStatus int
		wantValue  string
		provider   schemas.ModelProvider
	}{
		{"reference after a literal", `{"value":"` + literal + `","VALUE":"` + ref + `"}`, fasthttp.StatusForbidden, "sk-stored-literal", ""},
		{"structured reference after a literal", `{"value":"` + literal + `","VALUE":{"ref":"` + txe4989SecretEnv + `","type":"env"}}`, fasthttp.StatusForbidden, "sk-stored-literal", ""},
		{"literal after a reference", `{"VALUE":"` + ref + `","value":"` + literal + `"}`, fasthttp.StatusOK, literal, ""},
		{"reference in a later block", `{"azure_key_config":{"endpoint":"https://azure.example.com"},"AZURE_KEY_CONFIG":{"endpoint":"https://azure.example.com","client_secret":"` + ref + `"}}`, fasthttp.StatusForbidden, "sk-stored-literal", ""},
		{"reference in the earlier block of two that merge", `{"AZURE_KEY_CONFIG":{"endpoint":"https://azure.example.com","client_secret":"` + ref + `"},"azure_\u212aey_config":{"tenant_id":"tenant"}}`, fasthttp.StatusForbidden, "sk-stored-literal", ""},
		{"dial target in a later spelling", `{"sgl_key_config":{"url":"http://stored:30000"},"SGL_KEY_CONFIG":{"url":"http://169.254.169.254/"}}`, fasthttp.StatusForbidden, "sk-stored-literal", schemas.SGL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stored := schemas.Key{ID: "key-1", Name: "stored", Value: *schemas.NewSecretVar("sk-stored-literal"), Weight: 1}
			provider := schemas.OpenAI
			if tc.provider != "" {
				provider = tc.provider
				stored.SGLKeyConfig = &schemas.SGLKeyConfig{URL: *schemas.NewSecretVar("http://stored:30000")}
			}
			h := &ProviderHandler{
				inMemoryStore: &lib.Config{Providers: map[schemas.ModelProvider]configstore.ProviderConfig{provider: {Keys: []schemas.Key{stored}}}},
				modelsManager: &mockModelsManager{},
			}
			attachBifrostClient(t, h.inMemoryStore)
			ctx := newTestRequestCtx(tc.body)
			ctx.SetUserValue("provider", string(provider))
			ctx.SetUserValue("key_id", "key-1")
			ctx.SetUserValue(schemas.BifrostContextKeyAuthBypassed, true)
			h.updateProviderKey(ctx)
			if strings.Contains(string(ctx.Response.Body()), txe4989Secret) {
				t.Fatalf("the response disclosed the resolved server secret")
			}
			if got := ctx.Response.StatusCode(); got != tc.wantStatus {
				t.Fatalf("status %d, want %d: %s", got, tc.wantStatus, ctx.Response.Body())
			}
			keys := h.inMemoryStore.Providers[provider].Keys
			if len(keys) != 1 || len(keySecretRefs(&keys[0])) != 0 || keys[0].Value.GetValue() != tc.wantValue {
				t.Fatalf("stored key after the update: value %q, %d references", keys[0].Value.GetValue(), len(keySecretRefs(&keys[0])))
			}
		})
	}
}

// errorMessage returns the decoded message of an error response, so a path holding quotes
// (aliases["m.x"]) compares as written rather than JSON-escaped.
func errorMessage(t *testing.T, ctx *fasthttp.RequestCtx) string {
	t.Helper()
	var resp schemas.BifrostError
	if err := sonic.Unmarshal(ctx.Response.Body(), &resp); err != nil || resp.Error == nil {
		t.Fatalf("not an error response (%v): %d bytes", err, len(ctx.Response.Body()))
	}
	return resp.Error.Message
}

// TestUpdateProviderKey_SecretReferenceGuardWhenAuthBypassed covers the update cases the guard
// must tell apart for a stored key holding env. references: unchanged references, a masked
// echo of the redacted GET, removals and literal edits pass; a repointed reference, one moved to
// another field, and a swap between two fields are refused; a genuine admin may do any of them.
func TestUpdateProviderKey_SecretReferenceGuardWhenAuthBypassed(t *testing.T) {
	SetLogger(&mockLogger{})
	lib.SetLogger(&mockLogger{})
	t.Setenv(txe4989SecretEnv, txe4989Secret)
	t.Setenv(txe4989OtherEnv, txe4989Other)

	newStore := func() *lib.Config {
		return &lib.Config{Providers: map[schemas.ModelProvider]configstore.ProviderConfig{
			schemas.Bedrock: {Keys: []schemas.Key{{
				ID:     "key-1",
				Name:   "bedrock-key",
				Value:  *schemas.NewSecretVar("env." + txe4989SecretEnv),
				Weight: 1,
				BedrockKeyConfig: &schemas.BedrockKeyConfig{
					AccessKey: *schemas.NewSecretVar("env." + txe4989OtherEnv),
					SecretKey: *schemas.NewSecretVar("synthetic-literal-secret-key"),
					Region:    schemas.NewSecretVar("us-east-1"),
				},
			}}},
		}}
	}
	maskedEcho := func(t *testing.T) string {
		t.Helper()
		store := newStore()
		redacted, err := store.GetProviderKeyRedacted(schemas.Bedrock, "key-1")
		if err != nil {
			t.Fatalf("redacted read: %v", err)
		}
		redacted.Weight = 3
		body, err := sonic.Marshal(redacted)
		if err != nil {
			t.Fatalf("marshal redacted key: %v", err)
		}
		if strings.Contains(string(body), txe4989Secret) || strings.Contains(string(body), txe4989Other) {
			t.Fatalf("the redacted read disclosed a resolved server secret")
		}
		return string(body)
	}
	region := `"region":"us-east-1"`

	cases := []struct {
		name       string
		body       func(t *testing.T) string
		bypassed   bool
		wantStatus int
		wantPath   string
		wantValue  string // stored Value reference after the call
		wantAccess string // stored access_key reference after the call
	}{
		{name: "weight only", body: func(*testing.T) string { return `{"weight":2}` }, bypassed: true, wantStatus: fasthttp.StatusOK, wantValue: "env." + txe4989SecretEnv, wantAccess: "env." + txe4989OtherEnv},
		{name: "unchanged references resent", body: func(*testing.T) string {
			return `{"value":"env.` + txe4989SecretEnv + `","bedrock_key_config":{"access_key":"env.` + txe4989OtherEnv + `","secret_key":"synthetic-literal-secret-key",` + region + `}}`
		}, bypassed: true, wantStatus: fasthttp.StatusOK, wantValue: "env." + txe4989SecretEnv, wantAccess: "env." + txe4989OtherEnv},
		{name: "masked echo of the redacted read", body: maskedEcho, bypassed: true, wantStatus: fasthttp.StatusOK, wantValue: "env." + txe4989SecretEnv, wantAccess: "env." + txe4989OtherEnv},
		{name: "references removed for literals", body: func(*testing.T) string {
			return `{"value":"sk-synthetic-literal","bedrock_key_config":{"access_key":"synthetic-literal-access","secret_key":"synthetic-literal-secret-key",` + region + `}}`
		}, bypassed: true, wantStatus: fasthttp.StatusOK},
		{name: "value repointed", body: func(*testing.T) string { return `{"value":"env.` + txe4989OtherEnv + `"}` }, bypassed: true, wantStatus: fasthttp.StatusForbidden, wantPath: "value", wantValue: "env." + txe4989SecretEnv, wantAccess: "env." + txe4989OtherEnv},
		{name: "value repointed to vault", body: func(*testing.T) string { return `{"value":"vault.txe4989/synthetic"}` }, bypassed: true, wantStatus: fasthttp.StatusForbidden, wantPath: "value", wantValue: "env." + txe4989SecretEnv, wantAccess: "env." + txe4989OtherEnv},
		{name: "value reference moved to secret_key", body: func(*testing.T) string {
			return `{"value":"sk-synthetic-literal","bedrock_key_config":{"access_key":"env.` + txe4989OtherEnv + `","secret_key":"env.` + txe4989SecretEnv + `",` + region + `}}`
		}, bypassed: true, wantStatus: fasthttp.StatusForbidden, wantPath: "bedrock_key_config.secret_key", wantValue: "env." + txe4989SecretEnv, wantAccess: "env." + txe4989OtherEnv},
		{name: "references swapped between fields", body: func(*testing.T) string {
			return `{"value":"env.` + txe4989OtherEnv + `","bedrock_key_config":{"access_key":"env.` + txe4989SecretEnv + `","secret_key":"synthetic-literal-secret-key",` + region + `}}`
		}, bypassed: true, wantStatus: fasthttp.StatusForbidden, wantPath: "bedrock_key_config.access_key, value", wantValue: "env." + txe4989SecretEnv, wantAccess: "env." + txe4989OtherEnv},
		{name: "object-form reference", body: func(*testing.T) string {
			return `{"value":{"value":"","ref":"env.` + txe4989OtherEnv + `","type":"env"}}`
		}, bypassed: true, wantStatus: fasthttp.StatusForbidden, wantPath: "value", wantValue: "env." + txe4989SecretEnv, wantAccess: "env." + txe4989OtherEnv},
		{name: "legacy env_var form", body: func(*testing.T) string {
			return `{"value":{"env_var":"` + txe4989OtherEnv + `","from_env":true}}`
		}, bypassed: true, wantStatus: fasthttp.StatusForbidden, wantPath: "value", wantValue: "env." + txe4989SecretEnv, wantAccess: "env." + txe4989OtherEnv},
		{name: "genuine admin repoints and moves", body: func(*testing.T) string {
			return `{"value":"env.` + txe4989OtherEnv + `","bedrock_key_config":{"access_key":"env.` + txe4989SecretEnv + `","secret_key":"synthetic-literal-secret-key",` + region + `}}`
		}, wantStatus: fasthttp.StatusOK, wantValue: "env." + txe4989OtherEnv, wantAccess: "env." + txe4989SecretEnv},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &ProviderHandler{inMemoryStore: newStore(), modelsManager: &mockModelsManager{}}
			attachBifrostClient(t, h.inMemoryStore)
			ctx := newTestRequestCtx(tc.body(t))
			ctx.SetUserValue("provider", string(schemas.Bedrock))
			ctx.SetUserValue("key_id", "key-1")
			if tc.bypassed {
				ctx.SetUserValue(schemas.BifrostContextKeyAuthBypassed, true)
			}
			h.updateProviderKey(ctx)

			resp := string(ctx.Response.Body())
			if ctx.Response.StatusCode() != tc.wantStatus {
				t.Fatalf("status got %d, want %d (%d-byte body)", ctx.Response.StatusCode(), tc.wantStatus, len(resp))
			}
			if strings.Contains(resp, txe4989Secret) || strings.Contains(resp, txe4989Other) {
				t.Fatalf("the response disclosed a resolved server secret")
			}
			if tc.wantPath != "" && !strings.Contains(errorMessage(t, ctx), "server secret reference ("+tc.wantPath+")") {
				t.Fatalf("the refusal must name %s, got %q", tc.wantPath, errorMessage(t, ctx))
			}
			stored := h.inMemoryStore.Providers[schemas.Bedrock].Keys[0]
			if got := stored.Value.GetRawRef(); got != tc.wantValue {
				t.Fatalf("stored value reference got %q, want %q", got, tc.wantValue)
			}
			if got := stored.BedrockKeyConfig.AccessKey.GetRawRef(); got != tc.wantAccess {
				t.Fatalf("stored access_key reference got %q, want %q", got, tc.wantAccess)
			}
		})
	}
}

// TestCreateProviderKey_SecretReferenceGuardWhenAuthBypassed pins the create side: a literal
// key is still creatable under the bypass, an alias-level reference is refused under its
// quoted alias path, and a genuine admin may create a referenced key.
func TestCreateProviderKey_SecretReferenceGuardWhenAuthBypassed(t *testing.T) {
	SetLogger(&mockLogger{})
	lib.SetLogger(&mockLogger{})
	t.Setenv(txe4989SecretEnv, txe4989Secret)

	cases := []struct {
		name       string
		body       string
		bypassed   bool
		wantStatus int
		wantPath   string
	}{
		{name: "literal", body: `{"name":"k","value":"sk-synthetic-literal","weight":1}`, bypassed: true, wantStatus: fasthttp.StatusOK},
		{name: "env reference", body: `{"name":"k","value":"env.` + txe4989SecretEnv + `","weight":1}`, bypassed: true, wantStatus: fasthttp.StatusForbidden, wantPath: "value"},
		{name: "alias project reference", body: `{"name":"k","value":"sk-synthetic-literal","weight":1,"aliases":{"m.x":{"model_id":"gpt-4o","project_id":"env.` + txe4989SecretEnv + `"}}}`, bypassed: true, wantStatus: fasthttp.StatusForbidden, wantPath: `aliases["m.x"].project_id`},
		{name: "oauth client secret reference", body: `{"name":"k","weight":1,"oauth_key_config":{"token_url":"https://auth.example.com/token","client_id":"id","client_secret":"env.` + txe4989SecretEnv + `"}}`, bypassed: true, wantStatus: fasthttp.StatusForbidden, wantPath: "oauth_key_config.client_secret"},
		{name: "genuine admin env reference", body: `{"name":"k","value":"env.` + txe4989SecretEnv + `","weight":1}`, wantStatus: fasthttp.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &ProviderHandler{
				inMemoryStore: &lib.Config{Providers: map[schemas.ModelProvider]configstore.ProviderConfig{schemas.OpenAI: {}}},
				modelsManager: &mockModelsManager{},
			}
			attachBifrostClient(t, h.inMemoryStore)
			ctx := newTestRequestCtx(tc.body)
			ctx.SetUserValue("provider", string(schemas.OpenAI))
			if tc.bypassed {
				ctx.SetUserValue(schemas.BifrostContextKeyAuthBypassed, true)
			}
			h.createProviderKey(ctx)

			resp := string(ctx.Response.Body())
			if ctx.Response.StatusCode() != tc.wantStatus {
				t.Fatalf("status got %d, want %d (%d-byte body)", ctx.Response.StatusCode(), tc.wantStatus, len(resp))
			}
			if strings.Contains(resp, txe4989Secret) {
				t.Fatalf("the response disclosed the resolved server secret")
			}
			keys := h.inMemoryStore.Providers[schemas.OpenAI].Keys
			if tc.wantStatus == fasthttp.StatusForbidden {
				if !strings.Contains(errorMessage(t, ctx), "server secret reference ("+tc.wantPath+")") {
					t.Fatalf("the refusal must name %s, got %q", tc.wantPath, errorMessage(t, ctx))
				}
				if len(keys) != 0 {
					t.Fatalf("a refused key was stored")
				}
				return
			}
			if len(keys) != 1 {
				t.Fatalf("got %d stored keys, want 1", len(keys))
			}
		})
	}
}
