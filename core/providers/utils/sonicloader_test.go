package utils

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/build/constraint"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TXE-7504. sonic's JIT loader (github.com/bytedance/sonic/loader) registered each
// compiled function with the Go runtime through a lock-free append that could leave
// a module unreachable from runtime.firstmoduledata; a stack growth through such a
// frame aborted the process with "fatal error: unknown caller pc". No upstream
// release fixes it, so every module replaces the loader with the patched copy in
// third_party/sonic-loader. These tests fail when a module stops doing so, when the
// required loader version moves away from the version that copy was patched from,
// when a build context leaves the copy out, or when the Go version stops matching
// the range in which sonic enables its JIT.
const (
	sonicModule       = "github.com/bytedance/sonic"
	sonicLoaderModule = "github.com/bytedance/sonic/loader"
	patchedLoaderDir  = "third_party/sonic-loader"
)

type goModDirectives struct {
	Go      string
	Require []struct{ Path, Version string }
	Replace []struct {
		Old struct{ Path, Version string }
		New struct{ Path, Version string }
	}
}

// readGoMod parses go.mod with `go mod edit -json`, which covers the single-line
// and block forms of require and replace (see parseFasthttpDirectives).
func readGoMod(ctx context.Context, modPath string) (goModDirectives, error) {
	var mod goModDirectives
	out, err := exec.CommandContext(ctx, "go", "mod", "edit", "-json", modPath).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return mod, fmt.Errorf("go mod edit -json %s: %w: %s", modPath, err, exitErr.Stderr)
		}
		return mod, fmt.Errorf("go mod edit -json %s: %w", modPath, err)
	}
	if err := json.Unmarshal(out, &mod); err != nil {
		return mod, fmt.Errorf("parsing %s: %w", modPath, err)
	}
	return mod, nil
}

// patchedLoaderVersion reads the upstream version the patched copy was taken from.
func patchedLoaderVersion(t *testing.T, root string) string {
	t.Helper()
	notes := filepath.Join(root, patchedLoaderDir, "README.md")
	data, err := os.ReadFile(notes)
	if err != nil {
		t.Fatalf("reading %s: %v", notes, err)
	}
	m := regexp.MustCompile(`(?m)^Upstream version: (v[0-9][^\s]*)`).FindSubmatch(data)
	if m == nil {
		t.Fatalf("%s has no \"Upstream version: v...\" line", notes)
	}
	return string(m[1])
}

// loaderRequirementProblems reports why the go.mod at modPath (in modDir) does not
// build sonic's loader from the patched copy. It returns no problem for a module that
// does not require the loader at all.
func loaderRequirementProblems(mod goModDirectives, modDir, patchedDir, wantVersion string) (requires bool, problems []string) {
	for _, r := range mod.Require {
		if r.Path == sonicLoaderModule {
			requires = true
			if r.Version != wantVersion {
				problems = append(problems, fmt.Sprintf(
					"requires %s %s, but %s was patched from %s: re-vendor and re-patch it (see its README.md) or hold the version",
					sonicLoaderModule, r.Version, patchedLoaderDir, wantVersion))
			}
		}
	}
	if !requires {
		return false, nil
	}
	var replaced bool
	for _, r := range mod.Replace {
		if r.Old.Path != sonicLoaderModule {
			continue
		}
		replaced = true
		if r.Old.Version != "" {
			problems = append(problems, fmt.Sprintf("replaces %s only at %s; replace every version", sonicLoaderModule, r.Old.Version))
		}
		if r.New.Version != "" || filepath.Clean(filepath.Join(modDir, r.New.Path)) != filepath.Clean(patchedDir) {
			problems = append(problems, fmt.Sprintf("replaces %s with %s %s, not the directory %s",
				sonicLoaderModule, r.New.Path, r.New.Version, patchedLoaderDir))
		}
	}
	if !replaced {
		problems = append(problems, fmt.Sprintf("requires %s without replacing it with %s", sonicLoaderModule, patchedLoaderDir))
	}
	return true, problems
}

func TestLoaderRequirementProblemsCoversEveryForm(t *testing.T) {
	root := t.TempDir()
	patched := filepath.Join(root, patchedLoaderDir)
	modDir := filepath.Join(root, "core")
	if err := os.MkdirAll(modDir, 0o755); err != nil {
		t.Fatal(err)
	}
	header := "module example.com/m\ngo 1.27.3\n\n"
	require := "require github.com/bytedance/sonic/loader v0.5.2 // indirect\n\n"
	tests := []struct {
		name         string
		body         string
		wantRequires bool
		wantProblem  string
	}{
		{"patched replace, single line", header + require + "replace github.com/bytedance/sonic/loader => ../third_party/sonic-loader\n", true, ""},
		{"patched replace, block forms", header + "require (\n\tgithub.com/bytedance/sonic/loader v0.5.2 // indirect\n)\n\nreplace (\n\tgithub.com/bytedance/sonic/loader => ../third_party/sonic-loader\n)\n", true, ""},
		{"no replace", header + require, true, "without replacing it"},
		{"replace to another directory", header + require + "replace github.com/bytedance/sonic/loader => ../vendor/loader\n", true, "not the directory"},
		{"replace to a module version", header + require + "replace github.com/bytedance/sonic/loader => github.com/example/loader v0.5.2\n", true, "not the directory"},
		{"replace qualified by version", header + require + "replace github.com/bytedance/sonic/loader v0.5.2 => ../third_party/sonic-loader\n", true, "only at v0.5.2"},
		{"bumped loader", header + "require github.com/bytedance/sonic/loader v0.5.3\n\nreplace github.com/bytedance/sonic/loader => ../third_party/sonic-loader\n", true, "was patched from v0.5.2"},
		{"no loader at all", header, false, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			modPath := filepath.Join(modDir, "go.mod")
			if err := os.WriteFile(modPath, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			mod, err := readGoMod(t.Context(), modPath)
			if err != nil {
				t.Fatal(err)
			}
			requires, problems := loaderRequirementProblems(mod, modDir, patched, "v0.5.2")
			if requires != tc.wantRequires {
				t.Errorf("requires = %v, want %v", requires, tc.wantRequires)
			}
			joined := strings.Join(problems, "; ")
			if tc.wantProblem == "" && len(problems) > 0 {
				t.Errorf("unexpected problems: %s", joined)
			}
			if tc.wantProblem != "" && !strings.Contains(joined, tc.wantProblem) {
				t.Errorf("problems %q do not mention %q", joined, tc.wantProblem)
			}
		})
	}
}

// TestEveryModuleBuildsThePatchedSonicLoader walks every go.mod in the repository.
// A replace applies only when its module is the main module, and the release builds
// run with GOWORK=off from several module directories, so each go.mod carries it.
func TestEveryModuleBuildsThePatchedSonicLoader(t *testing.T) {
	root := repoRoot(t)
	patched := filepath.Join(root, patchedLoaderDir)
	want := patchedLoaderVersion(t, root)
	var checked int
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "tmp":
				return filepath.SkipDir
			}
			if path == patched {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() != "go.mod" {
			return nil
		}
		mod, err := readGoMod(t.Context(), path)
		if err != nil {
			return err
		}
		requires, problems := loaderRequirementProblems(mod, filepath.Dir(path), patched, want)
		if requires {
			checked++
		}
		rel, _ := filepath.Rel(root, path)
		for _, p := range problems {
			t.Errorf("%s %s", rel, p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking modules: %v", err)
	}
	if checked == 0 {
		t.Fatalf("no go.mod requiring %s found under %s", sonicLoaderModule, root)
	}
}

// TestWorkspaceAndImagesCarryThePatchedSonicLoader: the workspace must use the
// patched copy (so the Go unit shards run its regression tests), and every
// Dockerfile that copies the workspace must copy third_party too, or the image
// build cannot resolve the replace.
func TestWorkspaceAndImagesCarryThePatchedSonicLoader(t *testing.T) {
	root := repoRoot(t)

	workPath := filepath.Join(root, "go.work")
	out, err := exec.CommandContext(t.Context(), "go", "work", "edit", "-json", workPath).Output()
	if err != nil {
		t.Fatalf("go work edit -json %s: %v", workPath, err)
	}
	var work struct{ Use []struct{ DiskPath string } }
	if err := json.Unmarshal(out, &work); err != nil {
		t.Fatalf("parsing go.work: %v", err)
	}
	var used bool
	for _, u := range work.Use {
		if filepath.Clean(u.DiskPath) == patchedLoaderDir {
			used = true
		}
	}
	if !used {
		t.Errorf("go.work does not use ./%s", patchedLoaderDir)
	}

	dockerfiles, err := filepath.Glob(filepath.Join(root, "transports", "Dockerfile*"))
	if err != nil {
		t.Fatal(err)
	}
	copiesWork := regexp.MustCompile(`(?m)^\s*COPY\s+go\.work\s`)
	copiesThirdParty := regexp.MustCompile(`(?m)^\s*COPY\s+third_party/?\s`)
	var withWorkspace int
	for _, path := range dockerfiles {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !copiesWork.Match(data) {
			continue
		}
		withWorkspace++
		if !copiesThirdParty.Match(data) {
			rel, _ := filepath.Rel(root, path)
			t.Errorf("%s copies go.work but not third_party; the %s replace cannot resolve in the image", rel, sonicLoaderModule)
		}
	}
	if withWorkspace == 0 {
		t.Fatal("no transports/Dockerfile* copies go.work; update this guard to the build context in use")
	}
}

// sonicJITEnabled evaluates the build constraint on sonic.go (the file that holds the
// JIT-backed API) for linux/amd64 at Go 1.<minor>.
func sonicJITEnabled(sonicGo string, minor int) (bool, error) {
	f, err := os.Open(sonicGo)
	if err != nil {
		return false, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if !constraint.IsGoBuild(line) {
			continue
		}
		expr, err := constraint.Parse(line)
		if err != nil {
			return false, fmt.Errorf("%s: %w", sonicGo, err)
		}
		return expr.Eval(func(tag string) bool {
			switch tag {
			case "amd64", "linux", "unix", "gc", "cgo":
				return true
			}
			if v, ok := strings.CutPrefix(tag, "go1."); ok {
				n, err := strconv.Atoi(v)
				return err == nil && n <= minor
			}
			return false
		}), nil
	}
	return false, fmt.Errorf("%s has no //go:build line", sonicGo)
}

// TestSonicSupportsTheGoVersionInUse fails when go.mod's Go version falls outside
// the range in which the pinned sonic enables its JIT on amd64 (production's arch).
// Outside it sonic silently swaps in its encoding/json fallback, and the loader
// patch above would no longer describe what runs; either way the pin must move
// with the toolchain.
func TestSonicSupportsTheGoVersionInUse(t *testing.T) {
	root := repoRoot(t)
	mod, err := readGoMod(t.Context(), filepath.Join(root, "core", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.SplitN(mod.Go, ".", 3)
	if len(parts) < 2 || parts[0] != "1" {
		t.Fatalf("core/go.mod has unexpected go version %q", mod.Go)
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		t.Fatalf("core/go.mod go version %q: %v", mod.Go, err)
	}

	cmd := exec.CommandContext(t.Context(), "go", "list", "-m", "-f", "{{.Version}}\t{{.Dir}}", sonicModule)
	cmd.Dir = filepath.Join(root, "core")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -m %s: %v", sonicModule, err)
	}
	version, dir, _ := strings.Cut(strings.TrimSpace(string(out)), "\t")
	if dir == "" {
		t.Skipf("%s %s is not in the module cache; run go mod download", sonicModule, version)
	}
	enabled, err := sonicJITEnabled(filepath.Join(dir, "sonic.go"), minor)
	if err != nil {
		t.Fatal(err)
	}
	if !enabled {
		t.Errorf("%s %s does not enable its JIT for go %s on amd64 (sonic.go build constraint); move the sonic pin to a release that supports this Go version, then re-check %s",
			sonicModule, version, mod.Go, patchedLoaderDir)
	}
}

func TestSonicJITEnabledReadsTheBuildConstraint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sonic.go")
	body := "//go:build (amd64 && go1.17 && !go1.28) || (arm64 && go1.20 && !go1.28)\n// +build amd64,go1.17,!go1.28 arm64,go1.20,!go1.28\n\npackage sonic\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	for minor, want := range map[int]bool{16: false, 17: true, 27: true, 28: false} {
		got, err := sonicJITEnabled(path, minor)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("go1.%d: got %v, want %v", minor, got, want)
		}
	}
}
