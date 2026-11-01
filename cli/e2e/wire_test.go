//go:build linux

// Package e2e drives the built CLI binary against fake gateways on loopback. Nothing
// here reaches a provider or runs a real coding agent: the gateways are in-process
// fixtures and Codex is a stub script. It is Linux-only because Go honours
// SSL_CERT_FILE for the HTTPS fixtures there.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	adminKey   = "sk-mgmt-wire-correct"
	badKey     = "sk-mgmt-wire-bad"
	virtualKey = "sk-bf-wire-virtual"
)

// cliBinary is the binary under test: BIFROST_CLI_BIN when set, so an older build can
// run these same rows, otherwise this checkout built once in TestMain.
var cliBinary string

func TestMain(m *testing.M) {
	code, err := runWithBinary(m)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	os.Exit(code)
}

func runWithBinary(m *testing.M) (int, error) {
	if cliBinary = os.Getenv("BIFROST_CLI_BIN"); cliBinary != "" {
		return m.Run(), nil
	}
	dir, err := os.MkdirTemp("", "bifrost-cli-wire-*")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(dir)
	cliBinary = filepath.Join(dir, "bifrost")
	build := exec.Command("go", "build", "-o", cliBinary, "..")
	if out, err := build.CombinedOutput(); err != nil {
		return 0, fmt.Errorf("build CLI: %v\n%s", err, out)
	}
	return m.Run(), nil
}

// gateway is a fake Bifrost: /health and auth discovery answer anyone, as the real
// gateway's whitelist does, and /api/providers and /v1/models need credentials.
type gateway struct {
	*httptest.Server
	requireAdmin bool
	requireKey   bool

	mu    sync.Mutex
	paths []string
}

func (g *gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	g.paths = append(g.paths, r.URL.Path)
	g.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/health":
		fmt.Fprint(w, `{"status":"ok"}`)
	case "/api/session/is-auth-enabled":
		fmt.Fprint(w, `{"is_auth_enabled":true}`)
	case "/api/providers":
		if g.requireAdmin && r.Header.Get("Authorization") != "Bearer "+adminKey {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":{"message":"Unauthorized"}}`)
			return
		}
		fmt.Fprint(w, `{"providers":[]}`)
	case "/v1/models", "/openai/v1/models":
		if g.requireKey && r.Header.Get("x-bf-vk") != virtualKey {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":{"message":"virtual key required"}}`)
			return
		}
		fmt.Fprint(w, `{"data":[{"id":"openai/gpt-wire"}]}`)
	default:
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":{"message":"not found"}}`)
	}
}

func (g *gateway) hits(path string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	count := 0
	for _, requested := range g.paths {
		if requested == path {
			count++
		}
	}
	return count
}

func startGateway(t *testing.T, requireAdmin, requireKey bool) *gateway {
	t.Helper()
	g := &gateway{requireAdmin: requireAdmin, requireKey: requireKey}
	g.Server = httptest.NewServer(g)
	t.Cleanup(g.Close)
	return g
}

// result is one finished CLI process.
type result struct {
	stdout, stderr string
	exit           int
}

func (r result) all() string { return r.stdout + "\n" + r.stderr }

// runCLI runs the binary with exactly the environment given, plus a private HOME.
func runCLI(t *testing.T, home string, env map[string]string, args ...string) result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, cliBinary, args...)
	command.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin", "NO_COLOR=1", "TMPDIR=" + os.TempDir()}
	for name, value := range env {
		if name == "PATH" {
			value += ":/usr/bin:/bin"
		}
		command.Env = append(command.Env, name+"="+value)
	}
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	if ctx.Err() != nil {
		t.Fatalf("%v: timed out\n%s\n%s", args, stdout.String(), stderr.String())
	}
	out := result{stdout: stdout.String(), stderr: stderr.String()}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		out.exit = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("start CLI: %v", err)
	}
	return out
}

type doctorCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

func runDoctor(t *testing.T, g *gateway, env map[string]string) (map[string]doctorCheck, result) {
	t.Helper()
	out := runCLI(t, t.TempDir(), env, "--base-url", g.URL, "--output", "json", "doctor")
	var parsed struct {
		Data []doctorCheck `json:"data"`
	}
	if err := json.Unmarshal([]byte(out.stdout), &parsed); err != nil {
		t.Fatalf("doctor output is not structured JSON: %v\nstdout: %s\nstderr: %s", err, out.stdout, out.stderr)
	}
	checks := make(map[string]doctorCheck, len(parsed.Data))
	for _, check := range parsed.Data {
		checks[check.Name] = check
	}
	return checks, out
}

func TestWireDoctorCorrectAdminCredential(t *testing.T) {
	g := startGateway(t, true, true)
	checks, out := runDoctor(t, g, map[string]string{"BIFROST_MANAGEMENT_KEY": adminKey, "BIFROST_VIRTUAL_KEY": virtualKey})
	if out.exit != 0 {
		t.Fatalf("doctor exit = %d with correct credentials:\n%s", out.exit, out.all())
	}
	for name, check := range checks {
		if check.Status != "ok" {
			t.Errorf("check %q = %+v", name, check)
		}
	}
	if strings.Contains(out.all(), adminKey) || strings.Contains(out.all(), virtualKey) {
		t.Fatalf("doctor printed a credential:\n%s", out.all())
	}
}

func TestWireDoctorProbesTheProtectedRoute(t *testing.T) {
	g := startGateway(t, true, true)
	runDoctor(t, g, map[string]string{"BIFROST_MANAGEMENT_KEY": adminKey, "BIFROST_VIRTUAL_KEY": virtualKey})
	if g.hits("/api/providers") != 1 {
		t.Fatalf("doctor made %d requests to the protected management route, want 1", g.hits("/api/providers"))
	}
}

func assertManagementRefused(t *testing.T, env map[string]string, secret string) {
	t.Helper()
	g := startGateway(t, true, true)
	checks, out := runDoctor(t, g, env)
	management := checks["management authentication"]
	if management.Status != "failed" {
		t.Fatalf("management check = %+v, want failed: the gateway refused the credential", management)
	}
	if !strings.Contains(management.Detail, "HTTP 401") {
		t.Fatalf("management detail = %q, want the gateway's HTTP 401", management.Detail)
	}
	if out.exit == 0 {
		t.Fatalf("doctor exit = 0 although a check failed:\n%s", out.all())
	}
	if !strings.Contains(out.stderr, "one or more diagnostic checks failed") {
		t.Fatalf("stderr = %q, want the diagnostic failure", out.stderr)
	}
	if checks["gateway health"].Status != "ok" || checks["model discovery"].Status != "ok" {
		t.Fatalf("the other checks must stand on their own: %+v", checks)
	}
	if secret != "" && strings.Contains(out.all(), secret) {
		t.Fatalf("doctor printed the refused credential:\n%s", out.all())
	}
	if strings.Contains(out.all(), virtualKey) {
		t.Fatalf("doctor printed the virtual key:\n%s", out.all())
	}
}

func TestWireDoctorBadAdminCredential(t *testing.T) {
	assertManagementRefused(t, map[string]string{"BIFROST_MANAGEMENT_KEY": badKey, "BIFROST_VIRTUAL_KEY": virtualKey}, badKey)
}

func TestWireDoctorMissingAdminCredential(t *testing.T) {
	assertManagementRefused(t, map[string]string{"BIFROST_VIRTUAL_KEY": virtualKey}, "")
}

func TestWireDoctorGatewayWithoutAuth(t *testing.T) {
	g := startGateway(t, false, false)
	checks, out := runDoctor(t, g, nil)
	if out.exit != 0 || checks["management authentication"].Status != "ok" {
		t.Fatalf("doctor on a gateway without auth = %+v, exit %d\n%s", checks, out.exit, out.all())
	}
}

// startServer starts handler on its own loopback port, over TLS when secure is set.
func startServer(t *testing.T, secure bool, handler http.Handler) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(handler)
	if secure {
		server.StartTLS()
	} else {
		server.Start()
	}
	t.Cleanup(server.Close)
	return server
}

// trustFile writes the fixtures' shared test certificate for SSL_CERT_FILE.
func trustFile(t *testing.T, server *httptest.Server) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixture-ca.pem")
	block := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(path, block, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestWireRedirects runs real redirects between real listeners. Same-port scheme
// changes and the default ports cannot be listened on here; the matrix in
// internal/client covers those.
func TestWireRedirects(t *testing.T) {
	cases := []struct {
		name                    string
		sourceSecure, tgtSecure bool
		sameServer              bool
		otherHost               bool
		follow                  bool
	}{
		{name: "http same origin", sameServer: true, follow: true},
		{name: "https same origin", sourceSecure: true, tgtSecure: true, sameServer: true, follow: true},
		{name: "http other port", follow: false},
		{name: "https other port", sourceSecure: true, tgtSecure: true, follow: false},
		{name: "https to http other port", sourceSecure: true, follow: false},
		{name: "http to https other port", tgtSecure: true, follow: false},
		{name: "http other host", sameServer: true, otherHost: true, follow: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var targetMu sync.Mutex
			var targetAuth []string
			target := startServer(t, tc.tgtSecure, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				targetMu.Lock()
				targetAuth = append(targetAuth, r.Header.Get("Authorization"))
				targetMu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"reached":true}`)
			}))
			source := startServer(t, tc.sourceSecure, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				scheme := "http"
				if r.TLS != nil {
					scheme = "https"
				}
				sourceURL := scheme + "://" + r.Host
				if r.URL.Path == "/moved" {
					targetMu.Lock()
					targetAuth = append(targetAuth, r.Header.Get("Authorization"))
					targetMu.Unlock()
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, `{"reached":true}`)
					return
				}
				location := target.URL + "/moved"
				if tc.sameServer {
					location = sourceURL + "/moved"
				}
				if tc.otherHost {
					location = strings.Replace(sourceURL, "127.0.0.1", "localhost", 1) + "/moved"
				}
				http.Redirect(w, r, location, http.StatusTemporaryRedirect)
			}))
			env := map[string]string{"BIFROST_MANAGEMENT_KEY": adminKey}
			for _, server := range []*httptest.Server{source, target} {
				if server.TLS != nil {
					env["SSL_CERT_FILE"] = trustFile(t, server)
				}
			}
			out := runCLI(t, t.TempDir(), env, "--base-url", source.URL, "request", "GET", "/api/providers", "--auth", "management")
			targetMu.Lock()
			defer targetMu.Unlock()
			if strings.Contains(out.all(), adminKey) {
				t.Fatalf("the CLI printed the credential:\n%s", out.all())
			}
			if tc.follow {
				if out.exit != 0 || len(targetAuth) != 1 || targetAuth[0] != "Bearer "+adminKey {
					t.Fatalf("redirect not followed: exit %d, target saw %q\n%s", out.exit, targetAuth, out.all())
				}
				return
			}
			if len(targetAuth) != 0 {
				t.Fatalf("the disallowed origin received %d requests, authorization %q", len(targetAuth), targetAuth)
			}
			if out.exit == 0 || !strings.Contains(out.stderr, "refusing to follow") {
				t.Fatalf("exit %d, stderr %q; want a refusal", out.exit, out.stderr)
			}
		})
	}
}

// stubCodex installs a fake codex recording each launch. It applies the rule of
// Codex's ModelProviderInfo::api_key: a provider naming env_key fails when the
// variable is unset. It keeps one line per launch under CODEX_HOME, as a session.
const stubCodex = `#!/bin/sh
if [ "$1" = "--version" ]; then echo "codex-cli 0.0.0-stub"; exit 0; fi
if grep -q '^env_key' "$CODEX_HOME/config.toml" && [ -z "$OPENAI_API_KEY" ]; then
  echo "Missing environment variable: OPENAI_API_KEY" >&2
  exit 7
fi
mkdir -p "$CODEX_HOME/sessions"
prior=0
[ -f "$CODEX_HOME/sessions/rollout.jsonl" ] && prior=$(wc -l <"$CODEX_HOME/sessions/rollout.jsonl")
echo '{"turn":1}' >>"$CODEX_HOME/sessions/rollout.jsonl"
printf '%s\n%s\n%s\n' "$CODEX_HOME" "$prior" "$OPENAI_API_KEY" >"$STUB_RECORD"
`

type launch struct {
	codexHome string
	prior     int
	key       string
}

func installStub(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "codex"), []byte(stubCodex), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func runCodex(t *testing.T, home, stub string, g *gateway, key string) (launch, result) {
	t.Helper()
	record := filepath.Join(t.TempDir(), "record")
	env := map[string]string{"PATH": stub, "STUB_RECORD": record}
	if key != "" {
		env["BIFROST_VIRTUAL_KEY"] = key
	}
	out := runCLI(t, home, env, "--base-url", g.URL, "run", "codex", "--model", "openai/gpt-wire")
	data, err := os.ReadFile(record)
	if err != nil {
		return launch{}, out
	}
	fields := strings.Split(string(data), "\n")
	if len(fields) < 3 {
		t.Fatalf("stub record = %q", data)
	}
	var prior int
	fmt.Sscan(strings.TrimSpace(fields[1]), &prior)
	return launch{codexHome: fields[0], prior: prior, key: fields[2]}, out
}

func mustLaunch(t *testing.T, home, stub string, g *gateway, key string) launch {
	t.Helper()
	launched, out := runCodex(t, home, stub, g, key)
	if out.exit != 0 || launched.codexHome == "" {
		t.Fatalf("launch failed: exit %d\n%s", out.exit, out.all())
	}
	return launched
}

// filesContaining lists the files under root holding needle.
func filesContaining(t *testing.T, root, needle string) []string {
	t.Helper()
	var found []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if bytes.Contains(data, []byte(needle)) {
				found = append(found, path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

func TestWireLauncherHistorySurvivesLaunches(t *testing.T) {
	g, home, stub := startGateway(t, false, true), t.TempDir(), installStub(t)
	first := mustLaunch(t, home, stub, g, virtualKey)
	second := mustLaunch(t, home, stub, g, virtualKey)
	third := mustLaunch(t, home, stub, g, virtualKey)
	if first.codexHome != second.codexHome || second.codexHome != third.codexHome {
		t.Fatalf("launches used different homes: %q %q %q", first.codexHome, second.codexHome, third.codexHome)
	}
	if first.prior != 0 || second.prior != 1 || third.prior != 2 {
		t.Fatalf("sessions seen by each launch = %d, %d, %d; want 0, 1, 2", first.prior, second.prior, third.prior)
	}
}

func TestWireLauncherHomeIsPrivate(t *testing.T) {
	g, home, stub := startGateway(t, false, true), t.TempDir(), installStub(t)
	launched := mustLaunch(t, home, stub, g, virtualKey)
	if !strings.HasPrefix(launched.codexHome, filepath.Join(home, ".bifrost", "codex-home")+"/") {
		t.Fatalf("CODEX_HOME = %q, want it inside this user's home", launched.codexHome)
	}
	info, err := os.Stat(launched.codexHome)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("launcher home mode = %o, want 700", got)
	}
}

func TestWireLauncherKeepsCredentialsInTheEnvironment(t *testing.T) {
	g, home, stub := startGateway(t, false, true), t.TempDir(), installStub(t)
	userCodex := filepath.Join(home, ".codex")
	if err := os.MkdirAll(userCodex, 0o700); err != nil {
		t.Fatal(err)
	}
	userConfig := "model_reasoning_effort = \"high\"\n"
	userAuth := `{"auth_mode":"apikey","OPENAI_API_KEY":"sk-user-own-wire"}`
	if err := os.WriteFile(filepath.Join(userCodex, "config.toml"), []byte(userConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userCodex, "auth.json"), []byte(userAuth), 0o600); err != nil {
		t.Fatal(err)
	}
	launched := mustLaunch(t, home, stub, g, virtualKey)
	if launched.key != virtualKey {
		t.Fatalf("the child process did not get the launch's key: %q", launched.key)
	}
	if files := filesContaining(t, home, virtualKey); len(files) != 0 {
		t.Fatalf("the launch's key was written to disk: %v", files)
	}
	if files := filesContaining(t, home, "sk-user-own-wire"); len(files) != 1 {
		t.Fatalf("the user's own key was copied: %v", files)
	}
	for name, want := range map[string]string{"config.toml": userConfig, "auth.json": userAuth} {
		got, err := os.ReadFile(filepath.Join(userCodex, name))
		if err != nil || string(got) != want {
			t.Fatalf("user %s changed: %q, %v", name, got, err)
		}
	}
}

func TestWireLauncherIsolatesUsers(t *testing.T) {
	g, stub := startGateway(t, false, true), installStub(t)
	userA, userB := t.TempDir(), t.TempDir()
	mustLaunch(t, userA, stub, g, virtualKey)
	if other := mustLaunch(t, userB, stub, g, virtualKey); other.prior != 0 {
		t.Fatalf("a second user saw %d sessions of the first", other.prior)
	}
}

func TestWireLauncherEndpointChange(t *testing.T) {
	first, second, home, stub := startGateway(t, false, true), startGateway(t, false, true), t.TempDir(), installStub(t)
	onFirst := mustLaunch(t, home, stub, first, virtualKey)
	onSecond := mustLaunch(t, home, stub, second, virtualKey)
	if onSecond.prior != 0 {
		t.Fatalf("the second gateway's launch saw %d sessions of the first", onSecond.prior)
	}
	if back := mustLaunch(t, home, stub, first, virtualKey); back.prior != 1 || back.codexHome != onFirst.codexHome {
		t.Fatalf("back on the first gateway: %+v, want its one earlier session in %q", back, onFirst.codexHome)
	}
	config, err := os.ReadFile(filepath.Join(onSecond.codexHome, "config.toml"))
	if err != nil {
		t.Fatalf("read the second gateway's config: %v", err)
	}
	if !strings.Contains(string(config), second.URL) || strings.Contains(string(config), first.URL) {
		t.Fatalf("the second launch's config names the wrong endpoint:\n%s", config)
	}
}

func TestWireLauncherKeylessGateway(t *testing.T) {
	g, home, stub := startGateway(t, false, false), t.TempDir(), installStub(t)
	launched, out := runCodex(t, home, stub, g, "")
	if out.exit != 0 || launched.codexHome == "" {
		t.Fatalf("a keyless gateway's launch failed: exit %d\n%s", out.exit, out.all())
	}
	if launched.key != "" {
		t.Fatalf("a keyless launch carried a key: %q", launched.key)
	}
}

// TestWireConfigureSavesAKeyADirectLaunchCanUse pins the saved Codex config. Changing
// gateway replaces the key, and `configure` refuses to run with no key at all; the
// keyless write behind that refusal is pinned in internal/harness.
func TestWireConfigureSavesAKeyADirectLaunchCanUse(t *testing.T) {
	first, second, home, stub := startGateway(t, false, true), startGateway(t, false, true), t.TempDir(), installStub(t)
	configPath, authPath := filepath.Join(home, ".codex", "config.toml"), filepath.Join(home, ".codex", "auth.json")
	configure := func(g *gateway, key string) (result, string) {
		t.Helper()
		env := map[string]string{"PATH": stub}
		if key != "" {
			env["BIFROST_VIRTUAL_KEY"] = key
		}
		out := runCLI(t, home, env, "--base-url", g.URL, "configure", "codex", "--model", "openai/gpt-wire", "--force")
		config, err := os.ReadFile(configPath)
		if err != nil {
			t.Fatalf("read config.toml after configure (exit %d): %v\n%s", out.exit, err, out.all())
		}
		return out, string(config)
	}

	out, config := configure(first, virtualKey)
	if out.exit != 0 {
		t.Fatalf("configure exit %d\n%s", out.exit, out.all())
	}
	if strings.Contains(config, "env_key") || !strings.Contains(config, "requires_openai_auth = true") {
		t.Fatalf("a saved key must be read from auth.json, a direct launch has no environment key:\n%s", config)
	}
	if strings.Contains(config, virtualKey) {
		t.Fatal("config.toml holds the key")
	}
	if info, err := os.Stat(authPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("auth.json = %v, %v; want mode 600", info, err)
	}

	const secondKey = "sk-bf-wire-second"
	out, config = configure(second, secondKey)
	if out.exit != 0 {
		t.Fatalf("second configure exit %d\n%s", out.exit, out.all())
	}
	if !strings.Contains(config, second.URL) || strings.Contains(config, first.URL) {
		t.Fatalf("config kept the previous endpoint:\n%s", config)
	}
	if files := filesContaining(t, filepath.Join(home, ".codex"), virtualKey); len(files) != 0 {
		t.Fatalf("the previous gateway's key is still saved, so it would go to the new endpoint: %v", files)
	}

	out, after := configure(first, "")
	if out.exit == 0 || !strings.Contains(out.stderr, "a virtual key is required") {
		t.Fatalf("configure without a key: exit %d, stderr %q", out.exit, out.stderr)
	}
	if after != config {
		t.Fatalf("a refused configure changed the config:\n%s", after)
	}
}
