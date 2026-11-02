package update

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

func setTestHome(t *testing.T, home string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", home)
		return
	}
	t.Setenv("HOME", home)
}

func TestResolveManagedBinaryTargetFallsBackToCLIInstallLocation(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)

	binaryName := "bifrost"
	if runtime.GOOS == "windows" {
		binaryName = "bifrost.exe"
	}

	got, err := bifrostCLIManagedBinaryPath(binaryName)
	if err != nil {
		t.Fatalf("bifrostCLIManagedBinaryPath() error = %v", err)
	}

	want := filepath.Join(home, ".bifrost", "bin", binaryName)
	if got != want {
		t.Fatalf("bifrostCLIManagedBinaryPath() = %q, want %q", got, want)
	}
}

func TestIsWrapperManagedBinaryPathMatchesCLIInstallLocation(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)

	binaryName := "bifrost"
	if runtime.GOOS == "windows" {
		binaryName = "bifrost.exe"
	}

	path := filepath.Join(home, ".bifrost", "bin", binaryName)
	if !isWrapperManagedBinaryPath(path, binaryName) {
		t.Fatalf("expected %q to be recognized as wrapper-managed", path)
	}
}

func TestIsWrapperManagedBinaryPathMatchesCacheInstallLocation(t *testing.T) {
	if runtime.GOOS == "windows" {
		localAppData := t.TempDir()
		t.Setenv("LOCALAPPDATA", localAppData)
		path := filepath.Join(localAppData, "bifrost", "v1.2.3", "bin", "bifrost.exe-0")
		if !isWrapperManagedBinaryPath(path, "bifrost.exe") {
			t.Fatalf("expected %q to be recognized as wrapper-managed cache path", path)
		}
		return
	}

	home := t.TempDir()
	setTestHome(t, home)
	if runtime.GOOS == "linux" {
		xdg := filepath.Join(home, ".custom-cache")
		t.Setenv("XDG_CACHE_HOME", xdg)
		path := filepath.Join(xdg, "bifrost", "v1.2.3", "bin", "bifrost-0")
		if !isWrapperManagedBinaryPath(path, "bifrost") {
			t.Fatalf("expected %q to be recognized as wrapper-managed cache path", path)
		}
		return
	}

	path := filepath.Join(home, "Library", "Caches", "bifrost", "v1.2.3", "bin", "bifrost-0")
	if !isWrapperManagedBinaryPath(path, "bifrost") {
		t.Fatalf("expected %q to be recognized as wrapper-managed cache path", path)
	}
}

func TestStageUpdateBinaryCopiesIntoTargetDirectory(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	targetDir := filepath.Join(root, ".bifrost", "bin")
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		t.Fatalf("mkdir target dir: %v", err)
	}

	downloadPath := filepath.Join(t.TempDir(), "downloaded-bifrost")
	if err := os.WriteFile(downloadPath, []byte("new-binary"), 0o600); err != nil {
		t.Fatalf("write download: %v", err)
	}

	targetPath := filepath.Join(targetDir, "bifrost")
	stagePath, err := stageUpdateBinary(downloadPath, targetPath, 0o755)
	if err != nil {
		t.Fatalf("stageUpdateBinary() error = %v", err)
	}

	if filepath.Dir(stagePath) != targetDir {
		t.Fatalf("stage path dir = %q, want %q", filepath.Dir(stagePath), targetDir)
	}
	if got, err := os.ReadFile(stagePath); err != nil {
		t.Fatalf("read staged file: %v", err)
	} else if string(got) != "new-binary" {
		t.Fatalf("staged file contents = %q, want %q", string(got), "new-binary")
	}
}

type countingTransport struct{ calls atomic.Int32 }

func (c *countingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	c.calls.Add(1)
	return nil, errors.New("no network in this test")
}

func useChannel(t *testing.T, channel string) *countingTransport {
	t.Helper()
	transport := &countingTransport{}
	previousTransport, previousChannel := http.DefaultTransport, releaseChannel
	http.DefaultTransport, releaseChannel = transport, channel
	t.Cleanup(func() { http.DefaultTransport, releaseChannel = previousTransport, previousChannel })
	t.Setenv("BIFROST_NO_UPDATE_CHECK", "")
	return transport
}

func TestForkBuildNeverReachesUpstream(t *testing.T) {
	transport := useChannel(t, forkChannel)

	if result := doCheck("v1.0.1-sync.1", filepath.Join(t.TempDir(), "state.json")); result != nil {
		t.Fatalf("doCheck = %+v, want nil", result)
	}
	err := RunSelfUpdate("v1.0.1-sync.1")
	if err == nil {
		t.Fatal("RunSelfUpdate succeeded on a fork build")
	}
	if !strings.Contains(err.Error(), "github.com/txehq/bifrost/releases") || !strings.Contains(err.Error(), "SHA256SUMS") {
		t.Fatalf("error %q does not say where to get a release", err)
	}
	if strings.Contains(err.Error(), "getmaxim") {
		t.Fatalf("error %q names upstream's host", err)
	}
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("fork build made %d HTTP requests", got)
	}
}

func TestUpstreamBuildKeepsUpdating(t *testing.T) {
	transport := useChannel(t, "")

	if result := doCheck("v1.0.0", filepath.Join(t.TempDir(), "state.json")); result != nil {
		t.Fatalf("doCheck = %+v, want nil on a failed fetch", result)
	}
	if got := transport.calls.Load(); got != 1 {
		t.Fatalf("doCheck made %d requests, want 1", got)
	}
	if err := RunSelfUpdate("v1.0.0"); err == nil || !strings.Contains(err.Error(), "check latest version") {
		t.Fatalf("RunSelfUpdate = %v, want a fetch failure", err)
	}
	if got := transport.calls.Load(); got != 2 {
		t.Fatalf("RunSelfUpdate reached the network %d times in total, want 2", got)
	}
}

func TestUnknownChannelIsNotAForkBuild(t *testing.T) {
	useChannel(t, "other")
	if isForkBuild() {
		t.Fatal("an unknown channel counted as the fork")
	}
}
