package network

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/valyala/fasthttp"
)

// testNetAddr is in TEST-NET-3 (RFC 5737): never routed, so a guard failure here cannot reach
// anything, and no test depends on DNS.
const testNetAddr = "203.0.113.10:443"

func TestIsLoopbackHost(t *testing.T) {
	for host, want := range map[string]bool{
		"localhost": true, "LOCALHOST.": true, "fixture.localhost": true,
		"127.0.0.1": true, "127.8.9.10": true, "::1": true, "[::1]": true, "::1%lo0": true,
		"0.0.0.0": false, "::": false, "10.0.0.1": false, "192.168.1.2": false,
		"bedrock-runtime.us-east-1.amazonaws.com": false, "localhost.example.com": false,
		"127.0.0.1.nip.io": false, "": false,
	} {
		if got := IsLoopbackHost(host); got != want {
			t.Errorf("IsLoopbackHost(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestCheckOfflineEgress(t *testing.T) {
	defer SetOfflineEgressGuard(false)()
	if err := CheckOfflineEgress(testNetAddr); err != nil {
		t.Fatalf("guard off must allow every address: %v", err)
	}
	SetOfflineEgressGuard(true)
	for _, addr := range []string{"127.0.0.1:8791", "localhost:80", "[::1]:443"} {
		if err := CheckOfflineEgress(addr); err != nil {
			t.Errorf("loopback %q refused: %v", addr, err)
		}
	}
	for _, addr := range []string{testNetAddr, "10.0.0.5:443", "[2001:db8::1]:443"} {
		err := CheckOfflineEgress(addr)
		if err == nil {
			t.Fatalf("%q allowed in an offline run", addr)
		}
		if !strings.HasPrefix(err.Error(), OfflineEgressMarker) || !strings.Contains(err.Error(), addr) {
			t.Errorf("refusal must start with the marker and name %q: %v", addr, err)
		}
	}
}

// TestGuardDialerRefusesBeforeConnecting: the refusal comes from the dialer's Control hook,
// which runs before connect(2), and an existing Control or ControlContext still runs for an
// allowed address. Unix sockets are never judged.
func TestGuardDialerRefusesBeforeConnecting(t *testing.T) {
	defer SetOfflineEgressGuard(true)()
	innerRan := false
	inner := func(string, string, syscall.RawConn) error { innerRan = true; return nil }

	_, err := GuardDialer(&net.Dialer{Control: inner}).DialContext(context.Background(), "tcp", testNetAddr)
	var refusal *OfflineEgressError
	if !errors.As(err, &refusal) || innerRan {
		t.Fatalf("want the refusal before the inner Control: innerRan=%v err=%v", innerRan, err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	conn, err := GuardDialer(&net.Dialer{Control: inner}).Dial("tcp", listener.Addr().String())
	if err != nil || !innerRan {
		t.Fatalf("loopback must connect and run the inner Control: innerRan=%v err=%v", innerRan, err)
	}
	conn.Close()

	ctxRan := false
	withCtx := &net.Dialer{ControlContext: func(context.Context, string, string, syscall.RawConn) error { ctxRan = true; return nil }}
	if _, err := GuardDialer(withCtx).Dial("tcp", testNetAddr); !errors.As(err, &refusal) || ctxRan {
		t.Fatalf("ControlContext dialer: want the refusal first: ctxRan=%v err=%v", ctxRan, err)
	}

	// A short directory: t.TempDir() can exceed the 104-byte unix socket path limit on macOS.
	dir, err := os.MkdirTemp("/tmp", "og")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := dir + "/s.sock"
	unixListener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("unix listener: %v", err)
	}
	defer unixListener.Close()
	if conn, err := GuardDialer(nil).Dial("unix", socket); err != nil {
		t.Fatalf("a unix socket must not be judged: %v", err)
	} else {
		conn.Close()
	}
}

// TestNameOfflineEgressSaysWhichHost: a name that resolved to a refused address is named in
// the error next to the address.
func TestNameOfflineEgressSaysWhichHost(t *testing.T) {
	err := NameOfflineEgress(&OfflineEgressError{Addr: testNetAddr}, "bedrock-runtime.us-east-1.amazonaws.com")
	want := OfflineEgressMarker + " bedrock-runtime.us-east-1.amazonaws.com (" + testNetAddr + ") refused"
	if !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("got %q, want prefix %q", err, want)
	}
	plain := errors.New("connection refused")
	if NameOfflineEgress(plain, "x") != plain {
		t.Fatal("other errors must pass through unchanged")
	}
}

// TestOfflineGuardCoversTheSharedClients checks the dial paths other packages build on: the
// client factory's net/http transport, http.DefaultTransport, the SSRF dialer (whose stub resolver maps a name to a
// non-loopback address, so the error names the host) and a proxy that is not loopback.
func TestOfflineGuardCoversTheSharedClients(t *testing.T) {
	defer SetOfflineEgressGuard(true)()
	checks := map[string]error{}
	_, checks["factory net/http"] = NewHTTPClientFactory(nil, nil).GetHTTPClient(ClientPurposeInference).Get("https://" + testNetAddr + "/v1/models")
	// http.DefaultClient and a bare &http.Client{} dial through http.DefaultTransport.
	_, checks["http.DefaultClient"] = http.DefaultClient.Get("https://" + testNetAddr + "/")
	_, checks["bare http.Client"] = (&http.Client{}).Get("http://" + testNetAddr + "/")
	// The factory's fasthttp client and a client given GuardedFasthttpDial (fasthttp's own
	// dialer would bypass the guard).
	_, _, checks["factory fasthttp"] = NewHTTPClientFactory(nil, nil).GetFasthttpClient(ClientPurposeInference).Get(nil, "http://"+testNetAddr+"/")
	_, _, checks["GuardedFasthttpDial"] = (&fasthttp.Client{Dial: GuardedFasthttpDial}).Get(nil, "http://"+testNetAddr+"/")

	resolver := offlineStubResolver{"api.vendor.example": {net.ParseIP("8.8.8.8")}}
	_, checks["ssrf dialer"] = ssrfSafeDialContext(resolver, GuardDialer(&net.Dialer{}).DialContext, nil)(context.Background(), "tcp", "api.vendor.example:443")

	proxyURL, _ := url.Parse("http://" + testNetAddr)
	_, checks["non-loopback proxy"] = DialViaProxy(context.Background(), proxyURL, "127.0.0.1:1")
	for name, err := range checks {
		if err == nil || !strings.Contains(err.Error(), OfflineEgressMarker) {
			t.Errorf("%s: want the offline refusal, got %v", name, err)
		}
	}
	if err := checks["ssrf dialer"]; err == nil || !strings.Contains(err.Error(), "api.vendor.example (8.8.8.8:443)") {
		t.Errorf("ssrf dialer: the refusal must name the host and the address: %v", err)
	}

	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) }))
	defer fixture.Close()
	resp, err := NewHTTPClientFactory(nil, nil).GetHTTPClient(ClientPurposeInference).Get(fixture.URL)
	if err != nil {
		t.Fatalf("loopback fixture refused: %v", err)
	}
	resp.Body.Close()
}

type offlineStubResolver map[string][]net.IP

func (r offlineStubResolver) LookupIP(_ context.Context, _, host string) ([]net.IP, error) {
	if ips, ok := r[host]; ok {
		return ips, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

// TestOfflineRunTurnsOnTheGuard runs only under scripts/test-offline.sh (BIFROST_TEST_OFFLINE=1),
// which tests/test_offline_test.sh does: the guard must be on from the environment alone, and a
// dial to a non-loopback address must fail loudly, naming it, without being attempted.
func TestOfflineRunTurnsOnTheGuard(t *testing.T) {
	if os.Getenv(OfflineEnv) != "1" {
		t.Skip("runs under scripts/test-offline.sh")
	}
	if !OfflineEgressGuarded() {
		t.Fatalf("%s=1 is set but the egress guard is off", OfflineEnv)
	}
	_, err := GuardDialer(nil).Dial("tcp", testNetAddr)
	if err == nil || !strings.Contains(err.Error(), OfflineEgressMarker+" "+testNetAddr+" refused") {
		t.Fatalf("want the offline refusal naming %s, got %v", testNetAddr, err)
	}
	t.Logf("refused: %v", err)
}

// BenchmarkOfflineControl measures the check every guarded dial runs once per new connection
// (not per request): off, the production case, and on, refusing and allowing.
func BenchmarkOfflineControl(b *testing.B) {
	for _, tc := range []struct {
		name string
		on   bool
		addr string
	}{
		{"guard off", false, testNetAddr},
		{"guard on, refused", true, testNetAddr},
		{"guard on, loopback", true, "127.0.0.1:8791"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			defer SetOfflineEgressGuard(tc.on)()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = offlineControl("tcp4", tc.addr, nil)
			}
		})
	}
}

// TestEveryNetDialerInCoreIsGuarded fails when core builds a net.Dialer for TCP without the
// offline egress guard (TXE-7083): a literal must be wrapped in GuardDialer, or dial a unix
// socket on the same line. offline.go, which builds the guarded dialer, is exempt.
func TestEveryNetDialerInCoreIsGuarded(t *testing.T) {
	found := 0
	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "internal" && filepath.Dir(path) == ".." {
				return filepath.SkipDir // test-only harness packages (llmtests, mcptests)
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || strings.HasSuffix(path, filepath.Join("network", "offline.go")) {
			return nil
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(source), "\n") {
			if !strings.Contains(line, "net.Dialer{") || strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			found++
			if !strings.Contains(line, "GuardDialer(") && !strings.Contains(line, `"unix"`) {
				t.Errorf("%s:%d: a net.Dialer without the offline egress guard; wrap it in network.GuardDialer", path, i+1)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if found < 10 {
		t.Fatalf("found %d net.Dialer literals; the scan no longer sees core's sources", found)
	}
}
