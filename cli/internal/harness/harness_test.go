package harness

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/bytedance/sonic"
)

func TestClaudePreLaunchPinsSelectedModelAcrossClaudeTiers(t *testing.T) {
	unsetEnvForTest(t, "ENABLE_TOOL_SEARCH")
	unsetEnvForTest(t, "CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY")

	env, cleanup, err := claudePreLaunch("https://example.com/anthropic", "test-key", "openai/gpt-5")
	if err != nil {
		t.Fatalf("claudePreLaunch() error = %v", err)
	}
	defer cleanup()

	for _, want := range []string{
		"ENABLE_TOOL_SEARCH=true",
		"CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1",
		"ANTHROPIC_MODEL=openai/gpt-5",
		"ANTHROPIC_DEFAULT_SONNET_MODEL=openai/gpt-5",
		"ANTHROPIC_DEFAULT_OPUS_MODEL=openai/gpt-5",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL=openai/gpt-5",
	} {
		parts := strings.SplitN(want, "=", 2)
		if got := envValue(env, parts[0]); got != parts[1] {
			t.Fatalf("env[%q] = %q, want %q", parts[0], got, parts[1])
		}
	}

	if got := envValue(env, "CLAUDE_CODE_SIMPLE"); got != "" {
		t.Fatalf("did not expect CLAUDE_CODE_SIMPLE in env, got %#v", env)
	}
}

// unsetEnvForTest removes an environment variable for the duration of the
// test and restores its original state (unset or original value) afterward.
// t.Setenv is not used here because setting an empty value still leaves the
// variable "present" to os.LookupEnv, defeating isolation for code that
// distinguishes absence from an empty value.
func unsetEnvForTest(t *testing.T, key string) {
	t.Helper()
	original, wasSet := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("unset %s: %v", key, err)
	}
	t.Cleanup(func() {
		if wasSet {
			_ = os.Setenv(key, original)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}

func TestClaudeWriteNativeConfigPinsTierDefaults(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	settingsDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(settingsDir, 0o755); err != nil {
		t.Fatalf("mkdir settings dir: %v", err)
	}
	settingsPath := filepath.Join(settingsDir, "settings.json")
	initial := `{"apiKeyHelper":"stale-helper","env":{"EXISTING":"keep","ANTHROPIC_MODEL":"stale-model","ANTHROPIC_API_KEY":"stale-key"}}`
	if err := os.WriteFile(settingsPath, []byte(initial), 0o600); err != nil {
		t.Fatalf("write initial settings: %v", err)
	}

	if err := claudeWriteNativeConfig("https://example.com/anthropic", "test-key", "openai/gpt-5"); err != nil {
		t.Fatalf("claudeWriteNativeConfig() error = %v", err)
	}

	b, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}

	var settings map[string]any
	if err := sonic.Unmarshal(b, &settings); err != nil {
		t.Fatalf("unmarshal settings: %v", err)
	}

	envRaw, ok := settings["env"]
	if !ok {
		t.Fatalf("expected env map in settings, got %#v", settings)
	}
	envMap, ok := envRaw.(map[string]any)
	if !ok {
		t.Fatalf("env map type = %T, want map[string]any", envRaw)
	}

	for key, want := range map[string]string{
		"EXISTING":                                   "keep",
		"ANTHROPIC_BASE_URL":                         "https://example.com/anthropic",
		"ANTHROPIC_AUTH_TOKEN":                       "test-key",
		"ANTHROPIC_MODEL":                            "openai/gpt-5",
		"ANTHROPIC_DEFAULT_SONNET_MODEL":             "openai/gpt-5",
		"ANTHROPIC_DEFAULT_OPUS_MODEL":               "openai/gpt-5",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL":              "openai/gpt-5",
		"ENABLE_TOOL_SEARCH":                         "true",
		"CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY": "1",
	} {
		if got, _ := envMap[key].(string); got != want {
			t.Fatalf("env[%q] = %q, want %q", key, got, want)
		}
	}

	if _, ok := envMap["ANTHROPIC_API_KEY"]; ok {
		t.Fatalf("did not expect competing ANTHROPIC_API_KEY in settings env: %#v", envMap)
	}
	if _, ok := settings["apiKeyHelper"]; ok {
		t.Fatalf("did not expect competing apiKeyHelper in settings: %#v", settings)
	}
	if got, _ := settings["model"].(string); got != "openai/gpt-5" {
		t.Fatalf("settings.model = %q", got)
	}
}

func TestOpencodeModelRef(t *testing.T) {
	t.Parallel()

	if got := opencodeModelRef("gpt-4.1"); got != "bifrost/gpt-4.1" {
		t.Fatalf("opencodeModelRef() = %q, want %q", got, "bifrost/gpt-4.1")
	}
}

func TestOpencodePreLaunchWritesCustomProviderConfig(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)

	env, cleanup, err := opencodePreLaunch("https://example.com/openai", "test-key", "gpt-4.1")
	if err != nil {
		t.Fatalf("opencodePreLaunch() error = %v", err)
	}
	defer cleanup()

	if len(env) != 2 {
		t.Fatalf("unexpected env returned: %#v", env)
	}

	configPath := envValue(env, "OPENCODE_CONFIG")
	if configPath == "" {
		t.Fatalf("expected OPENCODE_CONFIG in env, got %#v", env)
	}
	b, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read generated config: %v", err)
	}

	cfg := string(b)
	for _, want := range []string{
		`"model": "bifrost/gpt-4.1"`,
		`"bifrost": {`,
		`"npm": "@ai-sdk/openai-compatible"`,
		`"baseURL": "https://example.com/openai"`,
		`"apiKey": "test-key"`,
		`"gpt-4.1": {`,
	} {
		if !strings.Contains(cfg, want) {
			t.Fatalf("expected generated config to contain %q, got %s", want, cfg)
		}
	}

	tuiPath := envValue(env, "OPENCODE_TUI_CONFIG")
	if tuiPath == "" {
		t.Fatalf("expected OPENCODE_TUI_CONFIG in env, got %#v", env)
	}
	tuiCfg, err := os.ReadFile(tuiPath)
	if err != nil {
		t.Fatalf("read generated tui config: %v", err)
	}
	if !strings.Contains(string(tuiCfg), `"theme": "system"`) {
		t.Fatalf("expected generated tui config to set system theme, got %s", string(tuiCfg))
	}
}

func TestOpencodePreLaunchPreservesExistingTheme(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)

	tuiPath := filepath.Join(xdg, "opencode", "tui.json")
	if err := os.MkdirAll(filepath.Dir(tuiPath), 0o755); err != nil {
		t.Fatalf("mkdir tui dir: %v", err)
	}
	if err := os.WriteFile(tuiPath, []byte("{\n  \"theme\": \"light\"\n}\n"), 0o600); err != nil {
		t.Fatalf("write tui config: %v", err)
	}

	env, cleanup, err := opencodePreLaunch("https://example.com/openai", "test-key", "gpt-4.1")
	if err != nil {
		t.Fatalf("opencodePreLaunch() error = %v", err)
	}
	defer cleanup()

	if got := envValue(env, "OPENCODE_TUI_CONFIG"); got != "" {
		t.Fatalf("did not expect OPENCODE_TUI_CONFIG override when user theme exists, got %#v", env)
	}
	if got := envValue(env, "OPENCODE_CONFIG"); got == "" {
		t.Fatalf("expected OPENCODE_CONFIG to remain present, got %#v", env)
	}
}

func TestOpencodePreLaunchAddsSystemThemeWithoutModel(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)

	env, cleanup, err := opencodePreLaunch("https://example.com/openai", "test-key", "")
	if err != nil {
		t.Fatalf("opencodePreLaunch() error = %v", err)
	}

	tuiPath := envValue(env, "OPENCODE_TUI_CONFIG")
	if tuiPath == "" {
		t.Fatalf("expected OPENCODE_TUI_CONFIG in env, got %#v", env)
	}
	if got := envValue(env, "OPENCODE_CONFIG"); got != "" {
		t.Fatalf("did not expect OPENCODE_CONFIG without a model, got %#v", env)
	}
	if _, err := os.Stat(tuiPath); err != nil {
		t.Fatalf("expected generated tui config to exist: %v", err)
	}

	cleanup()
	if _, err := os.Stat(tuiPath); !os.IsNotExist(err) {
		t.Fatalf("expected generated tui config to be removed after cleanup, stat err=%v", err)
	}
}

func TestLoadOpencodeTUIConfigSupportsJSONC(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "tui.json")
	content := "{\n  // keep my choice\n  \"theme\": \"light\",\n  \"foo\": true,\n}\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write tui config: %v", err)
	}

	cfg, hasTheme, err := loadOpencodeTUIConfig(path)
	if err != nil {
		t.Fatalf("loadOpencodeTUIConfig() error = %v", err)
	}
	if !hasTheme {
		t.Fatal("expected theme to be detected")
	}
	if cfg["theme"] != "light" {
		t.Fatalf("cfg[theme] = %#v, want %q", cfg["theme"], "light")
	}
}

func TestOpencodeTUIConfigPathPrefersXDG(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)

	got, err := opencodeTUIConfigPath()
	if err != nil {
		t.Fatalf("opencodeTUIConfigPath() error = %v", err)
	}
	want := filepath.Join(xdg, "opencode", "tui.json")
	if got != want {
		t.Fatalf("opencodeTUIConfigPath() = %q, want %q", got, want)
	}
}

func TestCodexWriteNativeConfigMergesAuthAndTOML(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir codex dir: %v", err)
	}

	authPath := filepath.Join(dir, "auth.json")
	initialAuth := `{"auth_mode":"apikey","OPENAI_API_KEY":"stale-key","tokens":{"id_token":"keep-me"}}`
	if err := os.WriteFile(authPath, []byte(initialAuth), 0o600); err != nil {
		t.Fatalf("write initial auth: %v", err)
	}

	configPath := filepath.Join(dir, "config.toml")
	initialTOML := `openai_base_url="http://old.example/openai/v1"
env_key="stale-literal-key"
model = "gpt-old"
model_reasoning_effort = "medium"

[projects."/Users/me/proj"]
trust_level = "trusted"
`
	if err := os.WriteFile(configPath, []byte(initialTOML), 0o600); err != nil {
		t.Fatalf("write initial config.toml: %v", err)
	}

	if err := codexWriteNativeConfig("http://localhost:8080/openai/v1", "sk-bf-new", "gpt-5.5"); err != nil {
		t.Fatalf("codexWriteNativeConfig() error = %v", err)
	}

	// auth.json belongs to the user's own Codex login and is left alone.
	authBytes, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatalf("read auth.json: %v", err)
	}
	if string(authBytes) != initialAuth {
		t.Fatalf("auth.json changed: %s", authBytes)
	}

	// config.toml should select a custom Responses provider, preserve
	// model_reasoning_effort and the [projects.*] table.
	tomlBytes, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config.toml: %v", err)
	}
	got := string(tomlBytes)
	for _, want := range []string{
		`model_provider = "bifrost"`,
		`model = "gpt-5.5"`,
		`model_reasoning_effort = "medium"`,
		`[model_providers.bifrost]`,
		`name = "Bifrost"`,
		`base_url = "http://localhost:8080/openai/v1"`,
		`wire_api = "responses"`,
		`experimental_bearer_token = "sk-bf-new"`,
		`requires_openai_auth = false`,
		`supports_websockets = false`,
		`[projects."/Users/me/proj"]`,
		`trust_level = "trusted"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected config.toml to contain %q, got:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{
		`"http://old.example/openai/v1"`,
		`"stale-literal-key"`,
		`"gpt-old"`,
		`openai_base_url=`,
		// A direct launch has no OPENAI_API_KEY in its environment, so the
		// provider carries its own key.
		`env_key`,
		`requires_openai_auth = true`,
	} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("expected config.toml to no longer contain %q, got:\n%s", unwanted, got)
		}
	}
}

func TestCodexWriteNativeConfigCreatesFilesWhenMissing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if err := codexWriteNativeConfig("http://localhost:8080/openai/v1", "sk-bf-fresh", ""); err != nil {
		t.Fatalf("codexWriteNativeConfig() error = %v", err)
	}

	if _, err := os.Stat(filepath.Join(home, ".codex", "auth.json")); !os.IsNotExist(err) {
		t.Fatalf("configure wrote an auth.json: %v", err)
	}

	tomlBytes, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if err != nil {
		t.Fatalf("read config.toml: %v", err)
	}
	got := string(tomlBytes)
	for _, want := range []string{
		`model_provider = "bifrost"`,
		`[model_providers.bifrost]`,
		`base_url = "http://localhost:8080/openai/v1"`,
		`wire_api = "responses"`,
		`experimental_bearer_token = "sk-bf-fresh"`,
		`requires_openai_auth = false`,
		`supports_websockets = false`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected config.toml to contain %q, got:\n%s", want, got)
		}
	}
	if strings.Contains(got, `env_key`) {
		t.Fatalf("persistent config must not require an environment key, got:\n%s", got)
	}
	if strings.Contains(got, `model =`) {
		t.Fatalf("did not expect model line when model is empty, got:\n%s", got)
	}
}

func TestCodexWriteNativeConfigSkipsAuthForDummyKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if err := codexWriteNativeConfig("http://localhost:8080/openai/v1", "dummy-key", ""); err != nil {
		t.Fatalf("codexWriteNativeConfig() error = %v", err)
	}

	if _, err := os.Stat(filepath.Join(home, ".codex", "auth.json")); !os.IsNotExist(err) {
		t.Fatalf("expected no auth.json for dummy key, stat err=%v", err)
	}
	configured, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if err != nil {
		t.Fatalf("expected config.toml to still be written, err=%v", err)
	}
	// A keyless gateway needs no credential: neither an environment key a
	// direct launch lacks, nor OpenAI auth that would prompt for a login.
	got := string(configured)
	if strings.Contains(got, `env_key`) || !strings.Contains(got, `requires_openai_auth = false`) {
		t.Fatalf("keyless config must require no credential, got:\n%s", got)
	}
}

// TestCodexWriteNativeConfigStaysOutOfCodexCredentialSelection pins that the provider
// is independent of Codex's own credential: whatever store the config selects, globally
// or in a profile, the provider never asks for OpenAI auth, the setting is left as it
// was, and auth.json is not touched. A store enforced by managed policy is outside this
// file; requires_openai_auth = false with the provider's own token is what covers it
// (resolve_provider_auth in openai/codex model-provider/src/auth.rs).
func TestCodexWriteNativeConfigStaysOutOfCodexCredentialSelection(t *testing.T) {
	for _, line := range []string{
		"",
		`cli_auth_credentials_store = "file"`,
		`cli_auth_credentials_store = "keyring"`,
		`cli_auth_credentials_store = "auto"`,
		`cli_auth_credentials_store = "ephemeral"`,
		"[profiles.work]\n" + `cli_auth_credentials_store = "keyring"`,
	} {
		t.Run(line, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			dir := filepath.Join(home, ".codex")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			userAuth := `{"auth_mode":"chatgpt","tokens":{"id_token":"keep-me"}}`
			if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(line+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(userAuth), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := codexWriteNativeConfig("http://localhost:8080/openai/v1", "sk-bf-test", ""); err != nil {
				t.Fatal(err)
			}
			configured, err := os.ReadFile(filepath.Join(dir, "config.toml"))
			if err != nil {
				t.Fatal(err)
			}
			got := string(configured)
			if !strings.Contains(got, line) {
				t.Fatalf("config changed the user's setting %q:\n%s", line, got)
			}
			if strings.Contains(got, `requires_openai_auth = true`) || strings.Count(got, "cli_auth_credentials_store") > 1 {
				t.Fatalf("the provider reaches Codex's own credential:\n%s", got)
			}
			if !strings.Contains(got, `experimental_bearer_token = "sk-bf-test"`) {
				t.Fatalf("the provider has no key of its own:\n%s", got)
			}
			if authBytes, err := os.ReadFile(filepath.Join(dir, "auth.json")); err != nil || string(authBytes) != userAuth {
				t.Fatalf("auth.json changed: %s, %v", authBytes, err)
			}
			assertMode(t, filepath.Join(dir, "config.toml"), 0o600)
		})
	}
}

// TestCodexWriteNativeConfigRefusesAnInsecureGateway pins that a key is not saved for
// Codex to send in plain text: Codex does not go through the CLI's transport checks.
func TestCodexWriteNativeConfigRefusesAnInsecureGateway(t *testing.T) {
	for _, baseURL := range []string{"http://gateway.example/openai", "ftp://gateway.example/openai"} {
		home := t.TempDir()
		t.Setenv("HOME", home)
		if err := codexWriteNativeConfig(baseURL, "sk-bf-test", ""); err == nil {
			t.Fatalf("codexWriteNativeConfig(%s) saved a key", baseURL)
		}
		if _, err := os.Stat(filepath.Join(home, ".codex")); !os.IsNotExist(err) {
			t.Fatalf("a refused configure wrote to the Codex home: %v", err)
		}
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, baseURL := range []string{"http://127.0.0.1:8080/openai", "http://localhost:8080/openai", "https://gateway.example/openai"} {
		if err := codexWriteNativeConfig(baseURL, "sk-bf-test", ""); err != nil {
			t.Fatalf("codexWriteNativeConfig(%s) = %v", baseURL, err)
		}
	}
}

func TestCodexWriteNativeConfigEscapesTheKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := codexWriteNativeConfig("http://localhost:8080/openai/v1", `sk"bf\key`, ""); err != nil {
		t.Fatal(err)
	}
	configured, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(configured), `experimental_bearer_token = "sk\"bf\\key"`) {
		t.Fatalf("key not escaped:\n%s", configured)
	}
}

// TestCodexWriteNativeConfigDoesNotWriteThroughASymlink pins that a key is not written
// into a dotfile repository the config links to.
func TestCodexWriteNativeConfigDoesNotWriteThroughASymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unprivileged symlinks only")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	shared := filepath.Join(t.TempDir(), "dotfiles-config.toml")
	if err := os.WriteFile(shared, []byte("model_reasoning_effort = \"high\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(shared, filepath.Join(home, ".codex", "config.toml")); err != nil {
		t.Fatal(err)
	}
	if err := codexWriteNativeConfig("http://localhost:8080/openai/v1", "sk-bf-test", ""); err != nil {
		t.Fatal(err)
	}
	target, err := os.ReadFile(shared)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(target), "sk-bf-test") {
		t.Fatalf("the key was written through the link:\n%s", target)
	}
	assertMode(t, filepath.Join(home, ".codex", "config.toml"), 0o600)
}

// TestCodexWriteNativeConfigKeylessLeavesTheCredentialStore pins that a config which
// saves no key does not change where the user's credentials live.
func TestCodexWriteNativeConfigKeylessLeavesTheCredentialStore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	userConfig := `cli_auth_credentials_store = "keyring"` + "\n"
	if err := os.WriteFile(filepath.Join(home, ".codex", "config.toml"), []byte(userConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := codexWriteNativeConfig("http://localhost:8080/openai/v1", "dummy-key", ""); err != nil {
		t.Fatal(err)
	}
	configured, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(configured), `cli_auth_credentials_store = "keyring"`) {
		t.Fatalf("a keyless config changed the credential store:\n%s", configured)
	}
}

// TestCodexWriteNativeConfigChangingGatewayReplacesTheKey pins that a saved key never
// reaches the next gateway, keyed or keyless.
func TestCodexWriteNativeConfigChangingGatewayReplacesTheKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	read := func() string {
		t.Helper()
		configured, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
		if err != nil {
			t.Fatal(err)
		}
		return string(configured)
	}

	if err := codexWriteNativeConfig("https://keyed.example/openai/v1", "sk-bf-old", ""); err != nil {
		t.Fatal(err)
	}
	if err := codexWriteNativeConfig("https://other.example/openai/v1", "sk-bf-new", ""); err != nil {
		t.Fatal(err)
	}
	if got := read(); strings.Contains(got, "sk-bf-old") || !strings.Contains(got, "sk-bf-new") || strings.Contains(got, "keyed.example") {
		t.Fatalf("the previous gateway's key or endpoint survived:\n%s", got)
	}
	if err := codexWriteNativeConfig("http://localhost:8080/openai/v1", "dummy-key", ""); err != nil {
		t.Fatal(err)
	}
	got := read()
	if !strings.Contains(got, `base_url = "http://localhost:8080/openai/v1"`) || strings.Contains(got, "other.example") {
		t.Fatalf("config kept the previous endpoint:\n%s", got)
	}
	if strings.Contains(got, "sk-bf-new") || strings.Contains(got, "experimental_bearer_token") || strings.Contains(got, `env_key`) {
		t.Fatalf("a keyless endpoint must carry no key:\n%s", got)
	}
}

// codexLaunchHome runs codexPreLaunch and returns the CODEX_HOME it selected. The
// launch ends with the test, or earlier through the returned cleanup.
func codexLaunchHome(t *testing.T, baseURL, apiKey, model string) (string, func()) {
	t.Helper()
	environment, cleanup, err := codexPreLaunch(baseURL, apiKey, model)
	if err != nil {
		t.Fatalf("codexPreLaunch() error = %v", err)
	}
	if cleanup == nil {
		t.Fatal("codexPreLaunch() returned no cleanup")
	}
	t.Cleanup(cleanup)
	codexHome := envValue(environment, "CODEX_HOME")
	if codexHome == "" {
		t.Fatal("codexPreLaunch() set no CODEX_HOME")
	}
	return codexHome, cleanup
}

func readCodexConfig(t *testing.T, codexHome string) string {
	t.Helper()
	configured, err := os.ReadFile(filepath.Join(codexHome, "config.toml"))
	if err != nil {
		t.Fatalf("read launcher config.toml: %v", err)
	}
	return string(configured)
}

// writeCodexSession records one session line the way a launched Codex does.
func writeCodexSession(t *testing.T, codexHome string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(codexHome, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(filepath.Join(codexHome, "sessions", "rollout.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("write session: %v", err)
	}
	defer file.Close()
	if _, err := file.WriteString("{\"turn\":1}\n"); err != nil {
		t.Fatal(err)
	}
}

func hasCodexSession(codexHome string) bool {
	_, err := os.Stat(filepath.Join(codexHome, "sessions", "rollout.jsonl"))
	return err == nil
}

// codexStoreFiles lists every file kept under the user's Codex session stores.
func codexStoreFiles(t *testing.T, home string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(filepath.Join(home, ".bifrost", "codex-home"), func(path string, entry os.DirEntry, err error) error {
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestCodexPreLaunchKeepsSessionsAcrossLaunches(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")

	codexHome, cleanup := codexLaunchHome(t, "http://localhost:8080/openai", "sk-bf-test", "gpt-test")
	configured := readCodexConfig(t, codexHome)
	if !strings.Contains(configured, `model_provider = "bifrost"`) {
		t.Fatalf("launcher config missing Bifrost provider:\n%s", configured)
	}
	// The launcher supplies OPENAI_API_KEY, so the wrapped session reads it
	// from the environment rather than from any saved auth.json.
	if !strings.Contains(configured, `env_key = "OPENAI_API_KEY"`) || !strings.Contains(configured, `requires_openai_auth = false`) {
		t.Fatalf("launcher config must read the launcher-supplied key:\n%s", configured)
	}
	if _, err := os.Stat(filepath.Join(home, ".codex", "config.toml")); !os.IsNotExist(err) {
		t.Fatalf("pre-launch mutated the real Codex config: %v", err)
	}

	// A session written by the launched Codex survives its exit, so it can be resumed.
	writeCodexSession(t, codexHome)
	cleanup()
	if _, err := os.Stat(codexHome); !os.IsNotExist(err) {
		t.Fatalf("the launch's home outlived it: %v", err)
	}
	again, _ := codexLaunchHome(t, "http://localhost:8080/openai/", "sk-bf-test", "gpt-test")
	if !hasCodexSession(again) {
		t.Fatal("launcher session history was lost across launches")
	}
}

func TestCodexPreLaunchSharesTheStateDatabasePerGateway(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	sqliteHome := func(baseURL string) string {
		environment, cleanup, err := codexPreLaunch(baseURL, "sk-bf-test", "gpt-test")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(cleanup)
		path := envValue(environment, "CODEX_SQLITE_HOME")
		if !strings.HasPrefix(path, filepath.Join(home, ".bifrost", "codex-home")+string(filepath.Separator)) {
			t.Fatalf("CODEX_SQLITE_HOME = %q, want it in the user's session store", path)
		}
		return path
	}
	first := sqliteHome("http://localhost:8080/openai")
	if again := sqliteHome("http://localhost:8080/openai"); again != first {
		t.Fatalf("one gateway got two state databases: %q %q", first, again)
	}
	if other := sqliteHome("https://gateway.example/openai"); other == first {
		t.Fatalf("two gateways share a state database: %q", other)
	}
}

// TestCodexPreLaunchKeepsEachGatewaySeparate pins that no session crosses gateways: a
// shared history could resend one gateway's transcript to the other.
func TestCodexPreLaunchKeepsEachGatewaySeparate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")

	first, _ := codexLaunchHome(t, "http://localhost:8080/openai", "sk-bf-test", "gpt-test")
	writeCodexSession(t, first)
	second, _ := codexLaunchHome(t, "https://gateway.example/openai", "sk-bf-other", "gpt-test")
	if hasCodexSession(second) {
		t.Fatal("a session recorded on one gateway is visible on another")
	}
	if configured := readCodexConfig(t, second); !strings.Contains(configured, `base_url = "https://gateway.example/openai/v1"`) {
		t.Fatalf("second gateway's config has the wrong endpoint:\n%s", configured)
	}
	if files := strings.Join(codexStoreFiles(t, home), " "); strings.Contains(files, "gateway.example") {
		t.Fatalf("store paths expose the gateway: %s", files)
	}
}

// TestCodexPreLaunchKeepsConcurrentLaunchesApart pins that two running launches on one
// gateway neither overwrite each other's config nor lose each other's sessions.
func TestCodexPreLaunchKeepsConcurrentLaunchesApart(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	const endpoint = "http://localhost:8080/openai"

	first, _ := codexLaunchHome(t, endpoint, "sk-bf-test", "gpt-first")
	second, _ := codexLaunchHome(t, endpoint, "sk-bf-test", "gpt-second")
	if first == second {
		t.Fatalf("concurrent launches share %q", first)
	}
	if configured := readCodexConfig(t, first); !strings.Contains(configured, `model = "gpt-first"`) {
		t.Fatalf("the second launch overwrote the first one's config:\n%s", configured)
	}
	if configured := readCodexConfig(t, second); !strings.Contains(configured, `model = "gpt-second"`) {
		t.Fatalf("second launch has the wrong model:\n%s", configured)
	}
	writeCodexSession(t, first)
	if !hasCodexSession(second) {
		t.Fatal("concurrent launches on one gateway do not share sessions")
	}
}

func TestCodexPreLaunchKeepsEachUserSeparate(t *testing.T) {
	t.Setenv("CODEX_HOME", "")
	userA, userB := t.TempDir(), t.TempDir()
	t.Setenv("HOME", userA)
	homeA, _ := codexLaunchHome(t, "http://localhost:8080/openai", "sk-bf-test", "gpt-test")
	writeCodexSession(t, homeA)
	t.Setenv("HOME", userB)
	homeB, _ := codexLaunchHome(t, "http://localhost:8080/openai", "sk-bf-test", "gpt-test")
	if hasCodexSession(homeB) {
		t.Fatal("one user's session is visible to another")
	}
}

// TestCodexPreLaunchLeavesNoCredentialBehind pins that what persists is session state
// alone: not the launch's key, a copy of the user's Codex authentication, or the
// user's config.toml with whatever secrets it holds.
func TestCodexPreLaunchLeavesNoCredentialBehind(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	userHome := filepath.Join(home, ".codex")
	if err := os.MkdirAll(userHome, 0o700); err != nil {
		t.Fatal(err)
	}
	userAuth := `{"auth_mode":"apikey","OPENAI_API_KEY":"sk-user-own"}`
	if err := os.WriteFile(filepath.Join(userHome, "auth.json"), []byte(userAuth), 0o600); err != nil {
		t.Fatal(err)
	}
	userConfig := "[mcp_servers.local.env]\nTOKEN = \"sk-config-secret\"\n"
	if err := os.WriteFile(filepath.Join(userHome, "config.toml"), []byte(userConfig), 0o600); err != nil {
		t.Fatal(err)
	}

	codexHome, cleanup := codexLaunchHome(t, "http://localhost:8080/openai", "sk-bf-secret", "gpt-test")
	if _, err := os.Stat(filepath.Join(codexHome, "auth.json")); !os.IsNotExist(err) {
		t.Fatalf("launcher home holds an auth.json: %v", err)
	}
	configured := readCodexConfig(t, codexHome)
	if !strings.Contains(configured, "sk-config-secret") {
		t.Fatalf("the launch lost the user's MCP setting:\n%s", configured)
	}
	if strings.Contains(configured, "sk-bf-secret") || strings.Contains(configured, "sk-user-own") {
		t.Fatalf("launcher config holds a credential:\n%s", configured)
	}
	writeCodexSession(t, codexHome)
	cleanup()
	for _, path := range codexStoreFiles(t, home) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{"sk-bf-secret", "sk-user-own", "sk-config-secret"} {
			if strings.Contains(string(data), secret) {
				t.Fatalf("%s outlived the launch in %s", secret, path)
			}
		}
		if name := filepath.Base(path); name == "config.toml" || name == "auth.json" {
			t.Fatalf("%s outlived the launch", path)
		}
	}
	original, err := os.ReadFile(filepath.Join(userHome, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(original) != userAuth {
		t.Fatalf("user auth.json changed: %s", original)
	}
}

// TestCodexPreLaunchStripsASavedKey pins that a key saved by `configure` for a direct
// launch does not reach a launch's temporary config, or the gateway it points at.
func TestCodexPreLaunchStripsASavedKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	if err := codexWriteNativeConfig("https://saved.example/openai/v1", "sk-bf-saved", ""); err != nil {
		t.Fatal(err)
	}
	for _, apiKey := range []string{"sk-bf-launch", "dummy-key"} {
		codexHome, _ := codexLaunchHome(t, "http://localhost:8080/openai", apiKey, "gpt-test")
		configured := readCodexConfig(t, codexHome)
		if strings.Contains(configured, "sk-bf-saved") || strings.Contains(configured, "experimental_bearer_token") || strings.Contains(configured, "saved.example") {
			t.Fatalf("a launch's config kept the saved key or endpoint:\n%s", configured)
		}
	}
}

// TestCodexPreLaunchFollowsKeyedAndKeylessLaunches pins that a keyless launch names
// no env_key, which Codex fails when the variable is unset, and that a keyed launch
// on the same endpoint restores it.
func TestCodexPreLaunchFollowsKeyedAndKeylessLaunches(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	const endpoint = "http://localhost:8080/openai"
	config := func(apiKey string) string {
		t.Helper()
		codexHome, _ := codexLaunchHome(t, endpoint, apiKey, "gpt-test")
		return readCodexConfig(t, codexHome)
	}

	if keyless := config("dummy-key"); strings.Contains(keyless, "env_key") || !strings.Contains(keyless, `requires_openai_auth = false`) {
		t.Fatalf("keyless launch must name no credential:\n%s", keyless)
	}
	if keyed := config("sk-bf-test"); !strings.Contains(keyed, `env_key = "OPENAI_API_KEY"`) {
		t.Fatalf("keyed launch must read the launcher-supplied key:\n%s", keyed)
	}
	if keyless := config(""); strings.Contains(keyless, "env_key") {
		t.Fatalf("keyless launch kept the keyed launch's env_key:\n%s", keyless)
	}
}

// TestCodexPreLaunchInsideALauncherSession covers a launch from a shell the launcher
// started, whose CODEX_HOME is the outer launch's home.
func TestCodexPreLaunchInsideALauncherSession(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	const endpoint = "http://localhost:8080/openai"
	outer, _ := codexLaunchHome(t, endpoint, "sk-bf-test", "gpt-test")
	if err := os.WriteFile(filepath.Join(outer, "config.toml"), []byte("model_reasoning_effort = \"high\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", outer)
	inner, _ := codexLaunchHome(t, endpoint, "sk-bf-test", "gpt-test")
	configured := readCodexConfig(t, inner)
	if !strings.Contains(configured, "model_reasoning_effort") || !strings.Contains(configured, `model_provider = "bifrost"`) {
		t.Fatalf("nested launch must start from the outer config:\n%s", configured)
	}
}

// TestLinkCodexSessionStateLeavesNoPartialLinks pins the fallback where linking fails:
// a launch either has all of its session state in the store or none of it.
func TestLinkCodexSessionStateLeavesNoPartialLinks(t *testing.T) {
	store, codexHome := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(codexHome, "archived_sessions"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if linkCodexSessionState(codexHome, store) {
		t.Fatal("linkCodexSessionState() reported success over an existing file")
	}
	for _, name := range []string{"history.jsonl", "sessions"} {
		if _, err := os.Lstat(filepath.Join(codexHome, name)); !os.IsNotExist(err) {
			t.Fatalf("a partial link to %s was left behind: %v", name, err)
		}
	}
}

func TestCodexPreLaunchProtectsSessionStore(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes and unprivileged symlinks only")
	}
	t.Setenv("CODEX_HOME", "")
	const endpoint = "http://localhost:8080/openai"
	// storeOf runs one launch and returns the store it created.
	storeOf := func(t *testing.T) string {
		t.Helper()
		environment, cleanup, err := codexPreLaunch(endpoint, "sk-bf-test", "gpt-test")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(cleanup)
		return filepath.Dir(envValue(environment, "CODEX_SQLITE_HOME"))
	}

	t.Run("new store is owner-only", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		store := storeOf(t)
		for _, path := range []string{store, filepath.Join(store, "sessions"), filepath.Join(store, "archived_sessions"), filepath.Join(store, "state")} {
			assertMode(t, path, 0o700)
		}
		assertMode(t, filepath.Join(store, "history.jsonl"), 0o600)
	})

	t.Run("loose store is tightened", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		store := storeOf(t)
		if err := os.Chmod(filepath.Join(store, "sessions"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(store, "history.jsonl"), 0o644); err != nil {
			t.Fatal(err)
		}
		storeOf(t)
		assertMode(t, filepath.Join(store, "sessions"), 0o700)
		assertMode(t, filepath.Join(store, "history.jsonl"), 0o600)
	})

	t.Run("symlinked store is refused", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		store := storeOf(t)
		if err := os.RemoveAll(store); err != nil {
			t.Fatal(err)
		}
		shared := t.TempDir()
		if err := os.Chmod(shared, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(shared, store); err != nil {
			t.Fatal(err)
		}
		if _, _, err := codexPreLaunch(endpoint, "sk-bf-test", "gpt-test"); err == nil {
			t.Fatal("codexPreLaunch() followed a symlinked session store")
		}
		assertMode(t, shared, 0o755)
	})

	t.Run("symlinked sessions are refused", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		store := storeOf(t)
		sessions := filepath.Join(store, "sessions")
		if err := os.RemoveAll(sessions); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(t.TempDir(), sessions); err != nil {
			t.Fatal(err)
		}
		if _, _, err := codexPreLaunch(endpoint, "sk-bf-test", "gpt-test"); err == nil {
			t.Fatal("codexPreLaunch() followed symlinked sessions")
		}
	})

	t.Run("symlinked history is refused", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		store := storeOf(t)
		history := filepath.Join(store, "history.jsonl")
		if err := os.Remove(history); err != nil {
			t.Fatal(err)
		}
		shared := filepath.Join(t.TempDir(), "shared-history")
		if err := os.WriteFile(shared, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(shared, history); err != nil {
			t.Fatal(err)
		}
		if _, _, err := codexPreLaunch(endpoint, "sk-bf-test", "gpt-test"); err == nil {
			t.Fatal("codexPreLaunch() followed symlinked history")
		}
		assertMode(t, shared, 0o644)
	})

	t.Run("file in its place is refused", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		store := storeOf(t)
		if err := os.RemoveAll(store); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(store, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := codexPreLaunch(endpoint, "sk-bf-test", "gpt-test"); err == nil {
			t.Fatal("codexPreLaunch() accepted a file as its session store")
		}
	})
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %o, want %o", path, got, want)
	}
}

func TestCodexPreLaunchPreservesUserPreferences(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	userHome := filepath.Join(home, ".codex")
	if err := os.MkdirAll(userHome, 0o700); err != nil {
		t.Fatal(err)
	}
	userConfig := `model = "gpt-user"
model_provider = "openai"
model_reasoning_effort = "high"

[mcp_servers.local]
command = "example-mcp"

[projects."/workspace"]
trust_level = "trusted"

[model_providers.bifrost]
name = "Stale Bifrost"
base_url = "https://stale.example/v1"
`
	if err := os.WriteFile(filepath.Join(userHome, "config.toml"), []byte(userConfig), 0o600); err != nil {
		t.Fatal(err)
	}

	environment, cleanup, err := codexPreLaunch("http://localhost:8080/openai", "sk-bf-test", "gpt-selected")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	codexHome := envValue(environment, "CODEX_HOME")
	configured, err := os.ReadFile(filepath.Join(codexHome, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(configured)
	for _, expected := range []string{
		`model = "gpt-selected"`, `model_provider = "bifrost"`, `model_reasoning_effort = "high"`,
		`[mcp_servers.local]`, `command = "example-mcp"`, `[projects."/workspace"]`, `trust_level = "trusted"`,
		`base_url = "http://localhost:8080/openai/v1"`,
	} {
		if !strings.Contains(got, expected) {
			t.Fatalf("launcher config does not contain %q:\n%s", expected, got)
		}
	}
	if strings.Contains(got, "stale.example") || strings.Contains(got, `model_provider = "openai"`) {
		t.Fatalf("launcher config retained stale routing:\n%s", got)
	}
	original, err := os.ReadFile(filepath.Join(userHome, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(original) != userConfig {
		t.Fatal("Codex pre-launch modified the user's config")
	}
}

// TestReplaceTOMLTableRecognizesCommentedHeader verifies a table header with
// a trailing comment is still recognized as the existing table, so it is
// replaced in place instead of duplicated.
func TestReplaceTOMLTableRecognizesCommentedHeader(t *testing.T) {
	t.Parallel()

	input := []byte(`model = "gpt-5.5"

[model_providers.bifrost] # custom provider
base_url = "http://old.example.com"
`)
	out := string(replaceTOMLTable(input, "model_providers.bifrost", []string{`base_url = "http://new.example.com"`}))

	if count := strings.Count(out, "[model_providers.bifrost]"); count != 1 {
		t.Fatalf("expected exactly one [model_providers.bifrost] table, got %d:\n%s", count, out)
	}
	if strings.Contains(out, "http://old.example.com") {
		t.Fatalf("expected old table body removed, got:\n%s", out)
	}
	if !strings.Contains(out, "http://new.example.com") {
		t.Fatalf("expected new table body present, got:\n%s", out)
	}
}

// TestReplaceTOMLTableRecognizesQuotedTableName verifies a table declared
// with a quoted final segment ([a."b"]) is recognized as the same table as
// the bare form (a.b), which TOML treats as identical identifiers, so the
// existing table is replaced instead of duplicated.
func TestReplaceTOMLTableRecognizesQuotedTableName(t *testing.T) {
	t.Parallel()

	input := []byte(`model = "gpt-5.5"

[model_providers."bifrost"]
base_url = "http://old.example.com"
`)
	out := string(replaceTOMLTable(input, "model_providers.bifrost", []string{`base_url = "http://new.example.com"`}))

	if strings.Contains(out, "http://old.example.com") {
		t.Fatalf("expected old (quoted-header) table body removed, got:\n%s", out)
	}
	if !strings.Contains(out, "http://new.example.com") {
		t.Fatalf("expected new table body present, got:\n%s", out)
	}
	if count := strings.Count(out, "base_url ="); count != 1 {
		t.Fatalf("expected exactly one base_url definition, got %d:\n%s", count, out)
	}
}

// TestSetTopLevelTOMLKeysReplacesQuotedExistingKey verifies a top-level key
// written with quotes ("model" = ...) is recognized as the same identifier
// as the bare form when a target sets it, so the existing quoted line is
// replaced instead of a bare duplicate being appended.
func TestSetTopLevelTOMLKeysReplacesQuotedExistingKey(t *testing.T) {
	t.Parallel()

	input := []byte(`"model" = "gpt-old"

[projects."x"]
trust_level = "trusted"
`)
	out := string(setTopLevelTOMLKeys(input, map[string]string{"model": "gpt-5.5"}))

	if strings.Contains(out, "gpt-old") {
		t.Fatalf("expected the quoted key's stale value removed, got:\n%s", out)
	}
	if !strings.Contains(out, `"gpt-5.5"`) {
		t.Fatalf("expected the new model value present, got:\n%s", out)
	}
	if count := strings.Count(out, "model"); count != 1 {
		t.Fatalf("expected exactly one model key (no bare duplicate), got %d:\n%s", count, out)
	}
}

func TestSetTopLevelTOMLKeysAppendsBeforeFirstTable(t *testing.T) {
	t.Parallel()

	input := []byte(`# header comment
model_reasoning_effort = "medium"

[projects."x"]
trust_level = "trusted"
`)
	out := string(setTopLevelTOMLKeys(input, map[string]string{
		"openai_base_url": "http://localhost:8080/openai/v1",
		"env_key":         "OPENAI_API_KEY",
		"model":           "gpt-5.5",
	}))

	preTable, _, ok := strings.Cut(out, `[projects."x"]`)
	if !ok {
		t.Fatalf("expected [projects.\"x\"] header preserved, got:\n%s", out)
	}
	for _, want := range []string{
		`# header comment`,
		`model_reasoning_effort = "medium"`,
		`openai_base_url = "http://localhost:8080/openai/v1"`,
		`env_key = "OPENAI_API_KEY"`,
		`model = "gpt-5.5"`,
	} {
		if !strings.Contains(preTable, want) {
			t.Fatalf("expected pre-table section to contain %q, got:\n%s", want, preTable)
		}
	}
	if !strings.Contains(out, `trust_level = "trusted"`) {
		t.Fatalf("expected table body preserved, got:\n%s", out)
	}
}

func envValue(env []string, key string) string {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix)
		}
	}
	return ""
}
