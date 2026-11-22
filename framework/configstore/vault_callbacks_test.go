package configstore

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/capsohq/bifrost/framework/configstore/tables"
	"github.com/capsohq/bifrost/framework/encrypt"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// stubVaultHooks installs store/remove hooks that mimic the enterprise vault
// registry: store records the path and rewrites the value to "vault.<path>";
// remove records the deleted path. Hooks are restored on cleanup.
func stubVaultHooks(t *testing.T) (stored map[string]string, removed *[]string) {
	t.Helper()
	stored = make(map[string]string)
	rem := []string{}
	prevStore, prevRemove := schemas.VaultStoreHook, schemas.VaultRemoveHook
	schemas.VaultStoreHook = func(_ context.Context, path string, value *string) error {
		stored[path] = *value
		*value = "vault." + path
		return nil
	}
	schemas.VaultRemoveHook = func(_ context.Context, path string) error {
		rem = append(rem, path)
		return nil
	}
	t.Cleanup(func() {
		schemas.VaultStoreHook = prevStore
		schemas.VaultRemoveHook = prevRemove
	})
	return stored, &rem
}

func TestVaultCallbacks_AutoStoreAndRemove(t *testing.T) {
	stored, removed := stubVaultHooks(t)

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	RegisterVaultCallbacks(db)
	require.NoError(t, db.AutoMigrate(&tables.TableMCPClient{}))

	client := &tables.TableMCPClient{
		ClientID:       "client-1",
		Name:           "test-client",
		ConnectionType: "http",
		AuthType:       "headers",
		Headers: map[string]schemas.SecretVar{
			"Authorization": {Val: "secret-token"},
		},
	}
	require.NoError(t, db.Create(client).Error)

	// The global store callback should have pushed the plaintext header to vault
	// before BeforeSave serialized Headers into HeadersJSON.
	headerPath := "bifrost/config_mcp_clients/client-1/headers/Authorization"
	require.Equal(t, "secret-token", stored[headerPath], "header secret not stored to vault")

	// HeadersJSON persisted in the row should hold the vault ref, not plaintext.
	var row tables.TableMCPClient
	require.NoError(t, db.First(&row, "client_id = ?", "client-1").Error)
	var headers map[string]string
	require.NoError(t, json.Unmarshal([]byte(row.HeadersJSON), &headers))
	require.Equal(t, "vault."+headerPath, headers["Authorization"], "HeadersJSON should store vault ref")

	// Deleting the row should trigger the global remove callback. Load first so
	// the model has its Headers populated for the reflection walk.
	var toDelete tables.TableMCPClient
	require.NoError(t, db.First(&toDelete, "client_id = ?", "client-1").Error)
	require.NoError(t, db.Delete(&toDelete).Error)

	found := false
	for _, p := range *removed {
		if p == headerPath {
			found = true
		}
	}
	require.True(t, found, "expected vault remove for %q, got %v", headerPath, *removed)
}

// TestVaultCallbacks_SelfManagedStoresPlaintext verifies that TableKey, whose
// SecretVar columns are populated inside BeforeSave, stores the PLAINTEXT secret to
// vault and persists a vault ref — both when encryption is off and when it is on.
// With encryption on, the inline vault store must run before encryption so the vault
// holds plaintext (not ciphertext) and the column holds the ref (not encrypted data).
func TestVaultCallbacks_SelfManagedStoresPlaintext(t *testing.T) {
	cases := []struct {
		name          string
		encryptionKey string
	}{
		{"encryption off", ""},
		{"encryption on", "test-encryption-key"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stored, _ := stubVaultHooks(t)
			encrypt.Init(tc.encryptionKey, bifrost.NewDefaultLogger(schemas.LogLevelInfo))
			t.Cleanup(func() { encrypt.Init("", bifrost.NewDefaultLogger(schemas.LogLevelInfo)) })

			db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
			require.NoError(t, err)
			RegisterVaultCallbacks(db)
			require.NoError(t, db.AutoMigrate(&tables.TableKey{}))

			keyID := fmt.Sprintf("key-%d", i)
			key := &tables.TableKey{
				Name:     fmt.Sprintf("k%d", i),
				KeyID:    keyID,
				Provider: "bedrock",
				Value:    schemas.SecretVar{Val: "primary-value"},
				Models:   schemas.WhiteList{"*"},
				BedrockKeyConfig: &schemas.BedrockKeyConfig{
					SecretKey: schemas.SecretVar{Val: "bedrock-secret"},
				},
			}
			require.NoError(t, db.Create(key).Error)

			// The vault must receive PLAINTEXT, regardless of encryption state.
			secretPath := fmt.Sprintf("bifrost/config_keys/%s/bedrock_secret_key", keyID)
			require.Equal(t, "bedrock-secret", stored[secretPath], "vault must store plaintext, not ciphertext")

			// The persisted column should hold the vault ref (which is never re-encrypted).
			var row tables.TableKey
			require.NoError(t, db.First(&row, "key_id = ?", keyID).Error)
			require.NotNil(t, row.BedrockSecretKey)
			require.Equal(t, "vault."+secretPath, row.BedrockSecretKey.GetRawRef(), "column should store vault ref")
		})
	}
}

// TestVaultCallbacks_SelfManagedRemovesVaultSecrets verifies that deleting a TableKey
// (a self-managed model) removes all its owned vault secrets via the global remove callback.
func TestVaultCallbacks_SelfManagedRemovesVaultSecrets(t *testing.T) {
	stored, removed := stubVaultHooks(t)

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	RegisterVaultCallbacks(db)
	require.NoError(t, db.AutoMigrate(&tables.TableKey{}))

	keyID := "key-delete-test"
	key := &tables.TableKey{
		Name:     "k-delete",
		KeyID:    keyID,
		Provider: "openai",
		Value:    schemas.SecretVar{Val: "sk-secret-value"},
		Models:   schemas.WhiteList{"*"},
	}
	require.NoError(t, db.Create(key).Error)

	valuePath := fmt.Sprintf("bifrost/config_keys/%s/value", keyID)
	require.Equal(t, "sk-secret-value", stored[valuePath], "value not stored to vault on create")

	// Load fresh to get the vault ref populated in SecretVar fields.
	var toDelete tables.TableKey
	require.NoError(t, db.First(&toDelete, "key_id = ?", keyID).Error)
	require.Equal(t, "vault."+valuePath, toDelete.Value.GetRawRef(), "loaded key should carry vault ref")

	require.NoError(t, db.Delete(&toDelete).Error)

	found := false
	for _, p := range *removed {
		if p == valuePath {
			found = true
		}
	}
	require.True(t, found, "expected vault remove for %q, got %v", valuePath, *removed)
}

func TestVaultCallbacks_NoOpWhenDisabled(t *testing.T) {
	// No hooks installed -> VaultStoreEnabled() is false -> callbacks no-op.
	prevStore := schemas.VaultStoreHook
	schemas.VaultStoreHook = nil
	t.Cleanup(func() { schemas.VaultStoreHook = prevStore })

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	RegisterVaultCallbacks(db)
	require.NoError(t, db.AutoMigrate(&tables.TableMCPClient{}))

	client := &tables.TableMCPClient{
		ClientID:       "client-2",
		Name:           "plain-client",
		ConnectionType: "http",
		AuthType:       "headers",
		Headers:        map[string]schemas.SecretVar{"Authorization": {Val: "plain-secret"}},
	}
	require.NoError(t, db.Create(client).Error)

	var row tables.TableMCPClient
	require.NoError(t, db.First(&row, "client_id = ?", "client-2").Error)
	require.False(t, strings.Contains(row.HeadersJSON, "vault."), "no vault ref expected when disabled: %s", row.HeadersJSON)
}

// TestVaultCallbacks_GithubCopilotAppColumnsFollowTheVault pins that a GitHub Copilot key's
// stored App columns, which TableKey no longer maps (TXE-6085), are still protected as they
// were as mapped fields: with vault writes on, a save of the key moves a plaintext App private
// key into the vault under the key's path (the older binary's path) and the column holds the
// reference, with encryption off and on; an env reference is left as written; a delete of the
// key removes the vault entry. Codex security review P2 on PR 433: before this the plaintext
// credential stayed in the database on such a deployment.
func TestVaultCallbacks_GithubCopilotAppColumnsFollowTheVault(t *testing.T) {
	for i, encryptionKey := range []string{"", "test-encryption-key"} {
		t.Run(fmt.Sprintf("encryption=%v", encryptionKey != ""), func(t *testing.T) {
			stored, removed := stubVaultHooks(t)
			encrypt.Init(encryptionKey, bifrost.NewDefaultLogger(schemas.LogLevelInfo))
			t.Cleanup(func() { encrypt.Init("", bifrost.NewDefaultLogger(schemas.LogLevelInfo)) })

			db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
			require.NoError(t, err)
			RegisterVaultCallbacks(db)
			require.NoError(t, db.AutoMigrate(&tables.TableKey{}))
			for _, column := range tables.GithubCopilotAppColumns {
				require.NoError(t, db.Migrator().AddColumn(&tables.TableKeyGithubCopilotApp{}, column))
			}
			keyID := fmt.Sprintf("copilot-app-%d", i)
			const pem = "-----BEGIN RSA PRIVATE KEY-----vault-----END RSA PRIVATE KEY-----"
			// A key written by the older binary before vault writes were turned on.
			require.NoError(t, db.Exec(`INSERT INTO config_keys (name, provider_id, provider, key_id, value, encryption_status, created_at, updated_at,
				github_copilot_app_id, github_copilot_private_key) VALUES (?, 1, 'github-copilot', ?, '', 'plain_text', datetime('now'), datetime('now'), ?, ?)`,
				keyID, keyID, "env.GITHUB_COPILOT_APP_ID", pem).Error)

			var key tables.TableKey
			require.NoError(t, db.First(&key, "key_id = ?", keyID).Error)
			key.Value = schemas.SecretVar{Val: "github_pat_after_upgrade"}
			require.NoError(t, db.Save(&key).Error)

			path := "bifrost/config_keys/" + keyID + "/github_copilot_private_key"
			require.Equal(t, pem, stored[path], "the App private key goes to the vault as plaintext")
			var raw map[string]any
			require.NoError(t, db.Table("config_keys").Where("key_id = ?", keyID).Take(&raw).Error)
			require.Equal(t, "vault."+path, raw["github_copilot_private_key"], "the column holds the vault reference")
			require.Equal(t, "env.GITHUB_COPILOT_APP_ID", raw["github_copilot_app_id"], "an env reference is left as written")
			for p, v := range stored {
				require.NotContains(t, v, "GITHUB_COPILOT_APP_ID", "the env reference went to the vault at %s", p)
			}

			var toDelete tables.TableKey
			require.NoError(t, db.First(&toDelete, "key_id = ?", keyID).Error)
			require.NoError(t, db.Delete(&toDelete).Error)
			require.Contains(t, *removed, path, "deleting the key removes its App vault entry")
		})
	}
}

// TestVaultCallbacks_EncryptedGithubCopilotAppColumnsReachTheVaultDecrypted pins the encrypted
// case of the test above: an App private key the older binary stored encrypted is decrypted
// before it goes to the vault, so the vault never holds ciphertext.
func TestVaultCallbacks_EncryptedGithubCopilotAppColumnsReachTheVaultDecrypted(t *testing.T) {
	stored, _ := stubVaultHooks(t)
	encrypt.Init("test-encryption-key", bifrost.NewDefaultLogger(schemas.LogLevelInfo))
	t.Cleanup(func() { encrypt.Init("", bifrost.NewDefaultLogger(schemas.LogLevelInfo)) })

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	RegisterVaultCallbacks(db)
	require.NoError(t, db.AutoMigrate(&tables.TableKey{}))
	for _, column := range tables.GithubCopilotAppColumns {
		require.NoError(t, db.Migrator().AddColumn(&tables.TableKeyGithubCopilotApp{}, column))
	}
	const pem = "-----BEGIN RSA PRIVATE KEY-----encrypted-----END RSA PRIVATE KEY-----"
	ciphertext, err := encrypt.Encrypt(pem)
	require.NoError(t, err)
	require.NoError(t, db.Exec(`INSERT INTO config_keys (name, provider_id, provider, key_id, value, encryption_status, created_at, updated_at,
		github_copilot_private_key) VALUES ('k', 1, 'github-copilot', 'copilot-enc', '', 'encrypted', datetime('now'), datetime('now'), ?)`, ciphertext).Error)

	var key tables.TableKey
	require.NoError(t, db.First(&key, "key_id = ?", "copilot-enc").Error)
	key.Value = schemas.SecretVar{Val: "github_pat_after_upgrade"}
	require.NoError(t, db.Save(&key).Error)

	path := "bifrost/config_keys/copilot-enc/github_copilot_private_key"
	require.Equal(t, pem, stored[path], "the vault holds the decrypted App private key")
	var raw map[string]any
	require.NoError(t, db.Table("config_keys").Where("key_id = ?", "copilot-enc").Take(&raw).Error)
	require.Equal(t, "vault."+path, raw["github_copilot_private_key"])
}
