package harness

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bytedance/sonic"
	"github.com/capsohq/bifrost/cli/internal/config"
)

// claudePreLaunch pins the selected model across Claude Code's model tiers.
// Tool search lets Claude load tool definitions on demand, while gateway model
// discovery lets it query models exposed by Bifrost's Anthropic-compatible
// endpoint. Existing user overrides always win.
func claudePreLaunch(baseURL, apiKey, model string) ([]string, func(), error) {
	var env []string
	if _, exists := os.LookupEnv("ENABLE_TOOL_SEARCH"); !exists {
		env = append(env, "ENABLE_TOOL_SEARCH=true")
	}
	if _, exists := os.LookupEnv("CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY"); !exists {
		env = append(env, "CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1")
	}
	if model = strings.TrimSpace(model); model != "" {
		env = append(env, "ANTHROPIC_MODEL="+model)
		env = append(env, claudeTierModelEnv(model)...)
	}
	return env, func() {}, nil
}

// claudeWriteNativeConfig writes the bifrost endpoint, API key, and model
// into Claude Code's settings file (~/.claude/settings.json) so the same
// configuration is available when users launch Claude Code directly.
//
// It merges into the existing file, preserving any user-defined settings.
func claudeWriteNativeConfig(baseURL, apiKey, model string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home dir: %w", err)
	}

	dir := filepath.Join(home, ".claude")
	settingsPath := filepath.Join(dir, "settings.json")

	// Read existing settings or start fresh
	settings := make(map[string]any)
	if b, err := os.ReadFile(settingsPath); err == nil {
		if err := sonic.Unmarshal(b, &settings); err != nil {
			return fmt.Errorf("parse existing claude settings: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read claude settings: %w", err)
	}

	// Get or create the env map
	envRaw, ok := settings["env"]
	var envMap map[string]any
	if ok {
		envMap, ok = envRaw.(map[string]any)
		if !ok {
			envMap = make(map[string]any)
		}
	} else {
		envMap = make(map[string]any)
	}

	envMap["ANTHROPIC_BASE_URL"] = baseURL
	envMap["ANTHROPIC_AUTH_TOKEN"] = apiKey
	delete(envMap, "ANTHROPIC_API_KEY")
	if _, exists := envMap["ENABLE_TOOL_SEARCH"]; !exists {
		envMap["ENABLE_TOOL_SEARCH"] = "true"
	}
	if _, exists := envMap["CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY"]; !exists {
		envMap["CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY"] = "1"
	}
	delete(settings, "apiKeyHelper")
	if model = strings.TrimSpace(model); model != "" {
		for key, value := range claudeTierModelEnvMap(model) {
			envMap[key] = value
		}
		envMap["ANTHROPIC_MODEL"] = model
		settings["model"] = model
	}

	settings["env"] = envMap

	b, err := sonic.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal claude settings: %w", err)
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create claude config dir: %w", err)
	}
	return config.WriteAtomic(settingsPath, b, 0o600)
}

func claudeTierModelEnv(model string) []string {
	envMap := claudeTierModelEnvMap(model)
	return []string{
		"ANTHROPIC_DEFAULT_SONNET_MODEL=" + envMap["ANTHROPIC_DEFAULT_SONNET_MODEL"],
		"ANTHROPIC_DEFAULT_OPUS_MODEL=" + envMap["ANTHROPIC_DEFAULT_OPUS_MODEL"],
		"ANTHROPIC_DEFAULT_HAIKU_MODEL=" + envMap["ANTHROPIC_DEFAULT_HAIKU_MODEL"],
	}
}

func claudeTierModelEnvMap(model string) map[string]string {
	return map[string]string{
		"ANTHROPIC_DEFAULT_SONNET_MODEL": model,
		"ANTHROPIC_DEFAULT_OPUS_MODEL":   model,
		"ANTHROPIC_DEFAULT_HAIKU_MODEL":  model,
	}
}

// codexPreLaunch gives each launch its own temporary Codex home with a Bifrost
// Responses provider, seeded from the user's config.toml and removed on exit, so a
// copy of that file and any secret in it never outlives the launch, and concurrent
// launches keep their own model and endpoint. Only session state is kept: it lives
// in a store per gateway, linked into the temporary home, so the next launch on that
// gateway can resume it. Session history holds prompts and responses, which is why
// the store is owner-only; the credential stays in the child process environment.
func codexPreLaunch(baseURL, apiKey string, model string) ([]string, func(), error) {
	sourceHome, err := codexUserHome()
	if err != nil {
		return nil, nil, err
	}
	store, err := codexSessionStore(baseURL)
	if err != nil {
		return nil, nil, err
	}
	if err := prepareCodexSessionStore(store); err != nil {
		return nil, nil, fmt.Errorf("prepare Codex session store: %w", err)
	}
	temporaryHome, err := os.MkdirTemp("", "bifrost-codex-home-*")
	if err != nil {
		return nil, nil, fmt.Errorf("create temporary Codex home: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(temporaryHome) }
	existing, readErr := os.ReadFile(filepath.Join(sourceHome, "config.toml"))
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		cleanup()
		return nil, nil, fmt.Errorf("read Codex config: %w", readErr)
	}
	// env_key fails a launch that supplies no key, so a keyless gateway names none.
	auth := codexAuthNone
	if codexHasKey(apiKey) {
		auth = codexAuthEnvironment
	}
	configured := codexRenderConfigTOML(existing, baseURL, model, auth)
	if err := config.WriteAtomic(filepath.Join(temporaryHome, "config.toml"), configured, 0o600); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("write temporary Codex config: %w", err)
	}
	environment := []string{"CODEX_HOME=" + temporaryHome}
	if linkCodexSessionState(temporaryHome, store) {
		environment = append(environment, "CODEX_SQLITE_HOME="+filepath.Join(store, codexStateDirectory))
	} else {
		// The launch still works, with its sessions removed along with the home.
		fmt.Fprintln(os.Stderr, "bifrost: this system cannot link Codex session history, so this session will not be kept after exit")
	}
	return environment, cleanup, nil
}

const (
	codexStateDirectory = "state"
	codexHistoryFile    = "history.jsonl"
)

// codexSessionDirectories are the directories Codex keeps session rollouts in.
var codexSessionDirectories = []string{"sessions", "archived_sessions"}

// codexSessionStore is the store of one gateway's Codex sessions, next to the Bifrost
// CLI's own state (~/.bifrost/codex-home/<endpoint digest>). One store per endpoint
// keeps a session recorded against one gateway from being resumed, and resent, to
// another.
func codexSessionStore(baseURL string) (string, error) {
	statePath, err := config.DefaultStatePath()
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(codexProviderURL(baseURL)))
	return filepath.Join(filepath.Dir(statePath), "codex-home", hex.EncodeToString(digest[:8])), nil
}

// prepareCodexSessionStore creates the store owner-only, tightens whatever already
// exists, and refuses a symlink or the wrong file type: history outlives the launch,
// so it must not land wherever a link points.
func prepareCodexSessionStore(store string) error {
	directories := []string{store, filepath.Join(store, codexStateDirectory)}
	for _, name := range codexSessionDirectories {
		directories = append(directories, filepath.Join(store, name))
	}
	for _, directory := range directories {
		if err := ensurePrivateDirectory(directory); err != nil {
			return err
		}
	}
	history := filepath.Join(store, codexHistoryFile)
	file, err := os.OpenFile(history, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	info, err := os.Lstat(history)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", history)
	}
	return os.Chmod(history, 0o600)
}

// ensurePrivateDirectory creates path owner-only, tightens one that already exists,
// and refuses a symlink or a file in its place.
func ensurePrivateDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", path)
	}
	return os.Chmod(path, 0o700)
}

// linkCodexSessionState links the store's session state into a temporary home. It
// reports false, with no link left behind, where links are unavailable, such as
// Windows without the privilege.
func linkCodexSessionState(temporaryHome, store string) bool {
	names := append([]string{codexHistoryFile}, codexSessionDirectories...)
	for index, name := range names {
		if err := os.Symlink(filepath.Join(store, name), filepath.Join(temporaryHome, name)); err != nil {
			for _, linked := range names[:index] {
				_ = os.Remove(filepath.Join(temporaryHome, linked))
			}
			return false
		}
	}
	return true
}

func codexUserHome() (string, error) {
	if configured := strings.TrimSpace(os.Getenv("CODEX_HOME")); configured != "" {
		return configured, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, ".codex"), nil
}

// codexWriteNativeConfig writes the bifrost endpoint, API key, and model
// into Codex CLI's config files (~/.codex/auth.json and
// ~/.codex/config.toml) so the same configuration is available when users
// launch Codex directly.
//
// It merges into the existing files, preserving any user-defined settings.
func codexWriteNativeConfig(baseURL, apiKey, model string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home dir: %w", err)
	}

	dir := filepath.Join(home, ".codex")
	hasKey := codexHasKey(apiKey)
	if hasKey {
		if err := refuseCodexCredentialStore(filepath.Join(dir, "config.toml")); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create codex config dir: %w", err)
	}

	// A direct launch has no launcher-supplied OPENAI_API_KEY, so the provider
	// reads the key saved in auth.json, or sends none at all.
	auth := codexAuthNone
	if hasKey {
		if err := codexWriteAuth(filepath.Join(dir, "auth.json"), strings.TrimSpace(apiKey)); err != nil {
			return err
		}
		auth = codexAuthStored
	}
	return codexWriteConfigTOML(filepath.Join(dir, "config.toml"), baseURL, model, auth)
}

// refuseCodexCredentialStore rejects a config whose credentials do not live in
// auth.json, where Codex would not read the saved key. Changing the setting is the
// user's call: it decides what every provider in that Codex home authenticates with.
func refuseCodexCredentialStore(path string) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read codex config: %w", err)
	}
	store := topLevelTOMLString(data, "cli_auth_credentials_store")
	if store == "" || store == "file" {
		return nil
	}
	return fmt.Errorf("Codex keeps its credentials in %q storage (cli_auth_credentials_store), so it would not read a saved key; use 'bifrost run codex', which saves none, or set cli_auth_credentials_store = \"file\" in %s yourself", store, path)
}

// topLevelTOMLString returns the string value of key before the first table, or "".
func topLevelTOMLString(data []byte, key string) string {
	for _, line := range strings.Split(string(data), "\n") {
		if isTOMLTableHeader(line) {
			return ""
		}
		if _, ok := matchTopLevelTOMLKey(line, map[string]string{key: ""}); !ok {
			continue
		}
		_, value, _ := strings.Cut(line, "=")
		value = strings.TrimSpace(value)
		if value == "" {
			return ""
		}
		quote := value[0]
		if quote != '"' && quote != '\'' {
			return value
		}
		if end := strings.IndexByte(value[1:], quote); end >= 0 {
			return value[1 : end+1]
		}
		return ""
	}
	return ""
}

// codexHasKey reports whether apiKey is a real credential, not the launcher's
// keyless placeholder.
func codexHasKey(apiKey string) bool {
	apiKey = strings.TrimSpace(apiKey)
	return apiKey != "" && apiKey != "dummy-key"
}

// codexProviderAuth selects where the Bifrost provider obtains its key.
type codexProviderAuth int

const (
	// codexAuthEnvironment reads the OPENAI_API_KEY a wrapped launch supplies.
	codexAuthEnvironment codexProviderAuth = iota
	// codexAuthStored reads the API key saved in Codex's auth.json.
	codexAuthStored
	// codexAuthNone sends no credential, for keyless gateways.
	codexAuthNone
)

// codexWriteAuth merges the bifrost virtual key into Codex's auth.json,
// preserving any other fields the user (or Codex itself) may have written.
func codexWriteAuth(path, apiKey string) error {
	auth := make(map[string]any)
	if b, err := os.ReadFile(path); err == nil {
		if err := sonic.Unmarshal(b, &auth); err != nil {
			// Unparseable auth.json — overwrite rather than fail the launch,
			// since the user can't easily recover from a corrupt file.
			auth = make(map[string]any)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read codex auth: %w", err)
	}

	auth["auth_mode"] = "apikey"
	auth["OPENAI_API_KEY"] = apiKey

	b, err := sonic.MarshalIndent(auth, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal codex auth: %w", err)
	}
	return config.WriteAtomic(path, b, 0o600)
}

// codexWriteConfigTOML merges the Bifrost provider into the config file at path.
func codexWriteConfigTOML(path, baseURL, model string, auth codexProviderAuth) error {
	var existing []byte
	if b, err := os.ReadFile(path); err == nil {
		existing = b
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read codex config: %w", err)
	}
	return config.WriteAtomic(path, codexRenderConfigTOML(existing, baseURL, model, auth), 0o600)
}

// codexRenderConfigTOML declares Bifrost as a custom Responses provider.
// Current Codex versions ignore OPENAI_BASE_URL and require a custom provider
// to disable OpenAI OAuth, so the provider table is part of the routing contract.
// With codexAuthStored, requires_openai_auth makes Codex send the API key saved in
// auth.json (auth_mode "apikey"), which needs no OAuth login.
func codexRenderConfigTOML(existing []byte, baseURL, model string, auth codexProviderAuth) []byte {
	targets := map[string]string{"model_provider": "bifrost"}
	if m := strings.TrimSpace(model); m != "" {
		targets["model"] = m
	}
	existing = removeTopLevelTOMLKeys(existing, map[string]struct{}{
		"openai_base_url": {},
		"env_key":         {},
	})
	configured := setTopLevelTOMLKeys(existing, targets)
	providerBody := []string{
		`name = "Bifrost"`,
		"base_url = " + tomlQuote(codexProviderURL(baseURL)),
		`wire_api = "responses"`,
	}
	switch auth {
	case codexAuthEnvironment:
		providerBody = append(providerBody, `env_key = "OPENAI_API_KEY"`, `requires_openai_auth = false`)
	case codexAuthStored:
		providerBody = append(providerBody, `requires_openai_auth = true`)
	default:
		providerBody = append(providerBody, `requires_openai_auth = false`)
	}
	providerBody = append(providerBody, `supports_websockets = false`)
	return replaceTOMLTable(configured, "model_providers.bifrost", providerBody)
}

// codexProviderURL is the Responses base URL Codex is given for a gateway endpoint.
func codexProviderURL(baseURL string) string {
	providerURL := strings.TrimSuffix(strings.TrimSpace(baseURL), "/")
	if !strings.HasSuffix(providerURL, "/v1") {
		providerURL += "/v1"
	}
	return providerURL
}

// removeTopLevelTOMLKeys drops obsolete assignments before the first table.
func removeTopLevelTOMLKeys(data []byte, keys map[string]struct{}) []byte {
	hasTrailingNewline := strings.HasSuffix(string(data), "\n")
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	result := make([]string, 0, len(lines))
	inTopLevel := true
	for _, line := range lines {
		if inTopLevel && isTOMLTableHeader(line) {
			inTopLevel = false
		}
		if inTopLevel {
			head, _, ok := strings.Cut(strings.TrimSpace(line), "=")
			if ok {
				if _, remove := keys[strings.TrimSpace(head)]; remove {
					continue
				}
			}
		}
		result = append(result, line)
	}
	joined := strings.Join(result, "\n")
	if hasTrailingNewline && joined != "" {
		joined += "\n"
	}
	return []byte(joined)
}

// replaceTOMLTable replaces one exact table while preserving all unrelated content.
func replaceTOMLTable(data []byte, name string, body []string) []byte {
	header := "[" + name + "]"
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	result := make([]string, 0, len(lines)+len(body)+2)
	skipping := false
	for _, line := range lines {
		if tomlTableHeaderMatches(line, name) {
			skipping = true
			continue
		}
		if skipping {
			if isTOMLTableHeader(line) {
				skipping = false
			} else {
				continue
			}
		}
		result = append(result, line)
	}
	for len(result) > 0 && strings.TrimSpace(result[len(result)-1]) == "" {
		result = result[:len(result)-1]
	}
	if len(result) > 0 {
		result = append(result, "")
	}
	result = append(result, header)
	result = append(result, body...)
	return []byte(strings.Join(result, "\n") + "\n")
}

// setTopLevelTOMLKeys returns data with each key in targets set to its
// target value in the top-level section (above any [table] header). Keys
// that already exist in the top-level section are replaced in place; keys
// that don't are appended just before the first table header (or at EOF if
// none). Everything else — table contents, comments, blank lines, ordering
// — is preserved as-is.
func setTopLevelTOMLKeys(data []byte, targets map[string]string) []byte {
	if len(targets) == 0 {
		return data
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		keys := make([]string, 0, len(targets))
		for k := range targets {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		for _, k := range keys {
			b.WriteString(k)
			b.WriteString(" = ")
			b.WriteString(tomlQuote(targets[k]))
			b.WriteByte('\n')
		}
		return []byte(b.String())
	}

	hasTrailingNewline := strings.HasSuffix(string(data), "\n")
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")

	seen := make(map[string]bool, len(targets))
	out := make([]string, 0, len(lines)+len(targets))
	firstTableIdx := -1

	for _, raw := range lines {
		if firstTableIdx < 0 && isTOMLTableHeader(raw) {
			firstTableIdx = len(out)
		}
		if firstTableIdx < 0 {
			if key, ok := matchTopLevelTOMLKey(raw, targets); ok {
				if seen[key] {
					// Drop duplicate top-level definitions.
					continue
				}
				out = append(out, key+" = "+tomlQuote(targets[key]))
				seen[key] = true
				continue
			}
		}
		out = append(out, raw)
	}

	missing := make([]string, 0, len(targets))
	for k := range targets {
		if !seen[k] {
			missing = append(missing, k+" = "+tomlQuote(targets[k]))
		}
	}
	sort.Strings(missing)

	if len(missing) > 0 {
		if firstTableIdx < 0 {
			for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
				out = out[:len(out)-1]
			}
			out = append(out, missing...)
		} else {
			head := out[:firstTableIdx]
			tail := append([]string{}, out[firstTableIdx:]...)
			for len(head) > 0 && strings.TrimSpace(head[len(head)-1]) == "" {
				head = head[:len(head)-1]
			}
			head = append(head, missing...)
			head = append(head, "")
			out = append(head, tail...)
		}
	}

	result := strings.Join(out, "\n")
	if hasTrailingNewline || len(missing) > 0 {
		result += "\n"
	}
	return []byte(result)
}

// tomlUnquoteKey strips a basic ("...") or literal ('...') TOML string
// wrapper from a key or dotted-path segment, so a quoted identifier can be
// compared against its bare equivalent — TOML treats model_provider,
// "model_provider", and 'model_provider' as the same identifier. Escape
// sequences inside a basic string are deliberately not decoded: a segment
// that needs them is left quoted (and so never matches) rather than risk
// silently mismatching. This is a scoped helper for this file's own
// hardcoded, escape-free target keys and table paths, not a general TOML
// parser (it doesn't handle a literal "." inside a quoted segment).
func tomlUnquoteKey(segment string) string {
	if len(segment) >= 2 {
		if segment[0] == '"' && segment[len(segment)-1] == '"' && !strings.ContainsAny(segment[1:len(segment)-1], `"\`) {
			return segment[1 : len(segment)-1]
		}
		if segment[0] == '\'' && segment[len(segment)-1] == '\'' {
			return segment[1 : len(segment)-1]
		}
	}
	return segment
}

// tomlTableHeaderMatches reports whether line declares the given dotted
// table name, tolerating a trailing inline comment (e.g.
// "[model_providers.bifrost] # custom provider") and a quoted form of any
// dotted segment (e.g. [model_providers."bifrost"]), which TOML treats as
// identical to the bare form.
func tomlTableHeaderMatches(line, name string) bool {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "[") {
		return false
	}
	idx := strings.LastIndex(trimmed, "]")
	if idx < 0 {
		return false
	}
	rest := strings.TrimSpace(trimmed[idx+1:])
	if rest != "" && !strings.HasPrefix(rest, "#") {
		return false
	}
	declared := strings.Split(trimmed[1:idx], ".")
	target := strings.Split(name, ".")
	if len(declared) != len(target) {
		return false
	}
	for i, segment := range declared {
		if tomlUnquoteKey(strings.TrimSpace(segment)) != target[i] {
			return false
		}
	}
	return true
}

// isTOMLTableHeader reports whether a line is a [table] or [[array]] header,
// optionally followed by an inline comment.
func isTOMLTableHeader(line string) bool {
	t := strings.TrimSpace(line)
	if !strings.HasPrefix(t, "[") {
		return false
	}
	idx := strings.LastIndex(t, "]")
	if idx < 0 {
		return false
	}
	rest := strings.TrimSpace(t[idx+1:])
	return rest == "" || strings.HasPrefix(rest, "#")
}

// matchTopLevelTOMLKey returns the target key name if line is a simple
// `key = ...` assignment whose key appears in targets.
func matchTopLevelTOMLKey(line string, targets map[string]string) (string, bool) {
	t := strings.TrimSpace(line)
	if t == "" || strings.HasPrefix(t, "#") {
		return "", false
	}
	head, _, ok := strings.Cut(t, "=")
	if !ok {
		return "", false
	}
	name := tomlUnquoteKey(strings.TrimSpace(head))
	if _, ok := targets[name]; ok {
		return name, true
	}
	return "", false
}

// tomlQuote returns a TOML basic string literal for s.
func tomlQuote(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}
