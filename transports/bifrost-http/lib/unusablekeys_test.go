package lib

import (
	"context"
	"strings"
	"testing"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/capsohq/bifrost/framework/configstore"
)

// TestUnusableProviderKeys pins the keys reported at load: exactly those key selection skips
// for want of a credential (bifrost.KeyHasCredential), with the GitHub Copilot message for a
// key whose GitHub App credentials were removed (TXE-6085), and no credential in any message.
func TestUnusableProviderKeys(t *testing.T) {
	disabled := false
	t.Setenv("TXE6085_EMPTY_KEY", "")
	providers := map[schemas.ModelProvider]configstore.ProviderConfig{
		schemas.GithubCopilot: {Keys: []schemas.Key{
			{Name: "former-app-key", Value: *schemas.NewSecretVar("")},
			{Name: "pat", Value: *schemas.NewSecretVar("github_pat_secretvalue")},
			{Name: "off", Value: *schemas.NewSecretVar(""), Enabled: &disabled},
		}},
		schemas.OpenAI: {Keys: []schemas.Key{
			{Name: "blank", Value: *schemas.NewSecretVar("  ")},
			{Name: "minted", OAuthKeyConfig: &schemas.OAuthKeyConfig{}},
			{Name: "unset-env", Value: *schemas.NewSecretVar("env.TXE6085_EMPTY_KEY")},
		}},
		schemas.Bedrock: {Keys: []schemas.Key{{Name: "iam-role"}}},
		"acme": {
			CustomProviderConfig: &schemas.CustomProviderConfig{BaseProviderType: schemas.GithubCopilot},
			Keys:                 []schemas.Key{{ID: "acme-key-1"}},
		},
	}

	got := unusableProviderKeys(providers)
	want := []string{
		`provider acme key "acme-key-1" has no token, so it is never used. GitHub App authentication`,
		`provider github-copilot key "former-app-key" has no token, so it is never used. GitHub App authentication`,
		`provider openai key "blank" has no value, so it is never used`,
		`provider openai key "unset-env" resolved env.TXE6085_EMPTY_KEY to an empty value, so it is never used`,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d problems, want %d:\n%s", len(got), len(want), strings.Join(got, "\n"))
	}
	for i := range want {
		if !strings.HasPrefix(got[i], want[i]) {
			t.Errorf("problem %d = %q, want it to start with %q", i, got[i], want[i])
		}
	}
	for _, line := range got {
		for _, quiet := range []string{`"pat"`, `"off"`, `"minted"`, `"iam-role"`, "github_pat_secretvalue"} {
			if strings.Contains(line, quiet) {
				t.Errorf("reported a usable or disabled key, or a credential, %s: %s", quiet, line)
			}
		}
	}
}

// TestLoadProvidersReportsAFormerGithubAppKey loads a config.json written for App auth: the
// github_copilot_key_config block is no longer part of a key, so the key arrives with no token.
// Loading still succeeds and reports the key once, at error level, naming the fix, and never
// echoes the private key.
func TestLoadProvidersReportsAFormerGithubAppKey(t *testing.T) {
	const privateKey = "-----BEGIN RSA PRIVATE KEY-----txe6085-----END RSA PRIVATE KEY-----"
	raw := `{"providers": {"github-copilot": {"keys": [{"name": "org-app", "models": ["*"], "weight": 1,
		"github_copilot_key_config": {"app_id": "123456", "installation_id": "87654321", "repository_id": "999000111", "private_key": "` + privateKey + `"}}]}}}`
	var configData ConfigData
	if err := configData.UnmarshalJSON([]byte(raw)); err != nil {
		t.Fatalf("a config.json written for App auth must still load: %v", err)
	}

	log := newCapturingLogger()
	prev := getLogger()
	SetLogger(log)
	defer SetLogger(prev)
	config := &Config{}
	if err := loadProviders(context.Background(), config, &configData); err != nil {
		t.Fatalf("loadProviders: %v", err)
	}
	if _, ok := config.Providers[schemas.GithubCopilot]; !ok {
		t.Fatal("the provider must still load")
	}

	var reports []string
	for _, line := range log.logs {
		if strings.Contains(line, privateKey) || strings.Contains(line, "txe6085") {
			t.Errorf("a log line carries the private key: %s", line)
		}
		if strings.HasPrefix(line, "[ERROR]") && strings.Contains(line, `key "org-app" has no token`) {
			reports = append(reports, line)
		}
	}
	if len(reports) != 1 || !strings.Contains(reports[0], "Sign in with GitHub") {
		t.Fatalf("want one error naming the key and the fix, got %q (all logs: %q)", reports, log.logs)
	}
}
