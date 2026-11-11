package utils

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/capsohq/bifrost/core/network/proxytest"
	"github.com/capsohq/bifrost/core/schemas"
)

// TestRedactURLForError pins that nothing a caller could authenticate with survives into
// an error string. AWS documents pre-signed URLs as bearer tokens valid for up to 7 days
// (docs.aws.amazon.com/AmazonS3/latest/userguide/using-presigned-url.html), so the query
// half is the credential, not a detail.
func TestRedactURLForError(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		secrets []string
	}{
		{
			name:    "s3 sigv4 pre-signed url",
			input:   "https://amzn-s3-demo-bucket.s3.us-east-1.amazonaws.com/reports/q3.pdf?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=AKIAIOSFODNN7EXAMPLE%2F20260812%2Fus-east-1%2Fs3%2Faws4_request&X-Amz-Signature=deadbeefcafe",
			want:    "https://amzn-s3-demo-bucket.s3.us-east-1.amazonaws.com/reports/q3.pdf",
			secrets: []string{"X-Amz-Signature", "deadbeefcafe", "AKIAIOSFODNN7EXAMPLE"},
		},
		{
			name:    "azure sas token",
			input:   "https://acct.blob.core.windows.net/c/doc.pdf?sv=2022-11-02&sig=Zm9vYmFyc2ln&se=2026-08-13T00%3A00%3A00Z",
			want:    "https://acct.blob.core.windows.net/c/doc.pdf",
			secrets: []string{"sig=", "Zm9vYmFyc2ln"},
		},
		{
			name:    "userinfo credentials",
			input:   "https://alice:hunter2@files.example.com/private/doc.pdf",
			want:    "https://files.example.com/private/doc.pdf",
			secrets: []string{"hunter2", "alice"},
		},
		{
			name:    "fragment is dropped",
			input:   "https://files.example.com/doc.pdf#token=abc123",
			want:    "https://files.example.com/doc.pdf",
			secrets: []string{"abc123"},
		},
		{
			name:    "plain url is unchanged",
			input:   "https://files.example.com/doc.pdf",
			want:    "https://files.example.com/doc.pdf",
			secrets: nil,
		},
		{
			// No host means nothing safe is identifiable, so nothing is echoed. Better a
			// useless-but-safe placeholder than a guess at which half was the secret.
			name:    "unparseable input yields a placeholder",
			input:   "://not a url?sig=leaked",
			want:    "[redacted url]",
			secrets: []string{"leaked"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RedactURLForError(tt.input)
			if got != tt.want {
				t.Errorf("RedactURLForError(%q) = %q, want %q", tt.input, got, tt.want)
			}
			for _, secret := range tt.secrets {
				if strings.Contains(got, secret) {
					t.Errorf("redacted URL still contains %q: %s", secret, got)
				}
			}
		})
	}
}

// TestSanitizeFetchError covers the half a clean format string cannot reach. net/http
// wraps transport failures in *url.Error, whose Error() prints the request URL verbatim
// apart from the password, so wrapping the cause with %w re-leaks the signed query even
// when the caller redacted its own copy of the URL.
func TestSanitizeFetchError(t *testing.T) {
	signed := "https://amzn-s3-demo-bucket.s3.amazonaws.com/q3.pdf?X-Amz-Signature=deadbeefcafe"
	redacted := RedactURLForError(signed)

	t.Run("rewrites the URL inside a *url.Error", func(t *testing.T) {
		cause := errors.New("dial tcp 203.0.113.10:443: i/o timeout")
		sanitized := sanitizeFetchError(&url.Error{Op: "Get", URL: signed, Err: cause}, redacted)

		msg := sanitized.Error()
		if strings.Contains(msg, "deadbeefcafe") || strings.Contains(msg, "X-Amz-Signature") {
			t.Errorf("sanitized error still leaks the signature: %s", msg)
		}
		if !strings.Contains(msg, "i/o timeout") {
			t.Errorf("expected the underlying cause to survive for diagnostics, got %s", msg)
		}
		if !errors.Is(sanitized, cause) {
			t.Error("expected errors.Is to still reach the original cause")
		}
	})

	t.Run("passes through a non-url error unchanged", func(t *testing.T) {
		cause := errors.New("unexpected EOF")
		if got := sanitizeFetchError(cause, redacted); got != cause {
			t.Errorf("expected the original error to be returned, got %v", got)
		}
	})
}

// TestFetchAndEncodeURL_ErrorsAreRedacted covers the paths reachable without a dial.
// FetchAndEncodeURL routes through network.SSRFSafeDialContext, which rejects loopback
// unconditionally and has no test seam, so an httptest server is unreachable by design
// (same constraint documented in core/providers/openai/chatfileurl_test.go). The scheme
// and parse guards run before any dial, and the dial rejection itself is reachable.
// guardedFetches is every entry point of the SSRF-hardened fetcher: the inline fetch, the
// buffered media fetch and the streamed one (fork, TXE-7036). The tests of a property all
// three must keep run it against each, so a variant cannot lose one unnoticed. The streamed
// fetch is read to its end, so a refusal that only shows in the body still fails it.
var guardedFetches = map[string]func(ctx context.Context, resourceURL string) error{
	"FetchAndEncodeURL": func(ctx context.Context, resourceURL string) error {
		_, _, err := FetchAndEncodeURL(ctx, resourceURL)
		return err
	},
	"FetchMediaURL": func(ctx context.Context, resourceURL string) error {
		_, _, err := FetchMediaURL(ctx, resourceURL)
		return err
	},
	"FetchMediaStream": func(ctx context.Context, resourceURL string) error {
		stream, err := FetchMediaStream(ctx, resourceURL, MediaStreamOptions{})
		if err != nil {
			return err
		}
		defer stream.Body.Close()
		_, err = io.Copy(io.Discard, stream.Body)
		return err
	},
}

func TestFetchAndEncodeURL_ErrorsAreRedacted(t *testing.T) {
	secrets := []string{"X-Amz-Signature", "deadbeefcafe", "hunter2"}

	tests := []struct {
		name string
		url  string
	}{
		{
			name: "unsupported scheme",
			url:  "ftp://alice:hunter2@files.example.com/doc.pdf?X-Amz-Signature=deadbeefcafe",
		},
		{
			name: "blocked by the SSRF dialer",
			url:  "https://alice:hunter2@127.0.0.1/doc.pdf?X-Amz-Signature=deadbeefcafe",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Fork: the media fetch, buffered and streamed, takes the same guarded path.
			for variant, fetch := range guardedFetches {
				err := fetch(t.Context(), tt.url)
				if err == nil {
					t.Fatalf("%s: expected an error", variant)
				}
				for _, secret := range secrets {
					if strings.Contains(err.Error(), secret) {
						t.Errorf("%s: error leaks %q: %s", variant, secret, err.Error())
					}
				}
			}
		})
	}
}

// TestFetchAndEncodeURL_UsesProviderProxy pins that a URL fetch made on a provider's behalf
// leaves through that provider's proxy_config, the same way its inference traffic does. A
// deployment whose only egress is a proxy otherwise times out on every image or document
// URL. The proxy here is a plain-HTTP forward proxy on loopback: an operator-configured
// proxy may sit on a private or loopback address, so the SSRF gate must let Bifrost dial it
// while still judging the target host.
func TestFetchAndEncodeURL_UsesProviderProxy(t *testing.T) {
	var hits atomic.Int32
	var secretHits atomic.Int32
	proxy := proxytest.ForwardProxy(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host == "10.0.0.5" {
			secretHits.Add(1)
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Host != "203.0.113.10" {
			http.Error(w, "unexpected target "+r.URL.Host, http.StatusBadGateway)
			return
		}
		hits.Add(1)
		switch r.URL.Path {
		case "/img.png":
			w.Header().Set("Content-Type", "image/png; charset=binary")
			_, _ = w.Write([]byte("PNGDATA"))
		case "/redirect":
			http.Redirect(w, r, "http://10.0.0.5/secret", http.StatusFound)
		case "/redirect-scheme":
			w.Header().Set("Location", "file:///etc/passwd")
			w.WriteHeader(http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	})

	proxyCtx := func(t *testing.T) context.Context {
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		t.Cleanup(cancel)
		return context.WithValue(ctx, schemas.BifrostContextKeyProviderProxyConfig, &schemas.ProxyConfig{
			Type: schemas.HTTPProxy,
			URL:  schemas.NewSecretVar(proxy.URL),
		})
	}

	t.Run("public target goes through the proxy", func(t *testing.T) {
		hits.Store(0)
		mediaType, encoded, err := FetchAndEncodeURL(proxyCtx(t), "http://203.0.113.10/img.png")
		if err != nil {
			t.Fatalf("fetch through proxy failed: %v", err)
		}
		if hits.Load() != 1 {
			t.Fatalf("proxy saw %d requests, want 1", hits.Load())
		}
		if mediaType != "image/png" {
			t.Errorf("mediaType = %q, want image/png", mediaType)
		}
		if want := base64.StdEncoding.EncodeToString([]byte("PNGDATA")); encoded != want {
			t.Errorf("encoded = %q, want %q", encoded, want)
		}
	})

	t.Run("private target is refused before reaching the proxy", func(t *testing.T) {
		secretHits.Store(0)
		for variant, fetch := range guardedFetches {
			err := fetch(proxyCtx(t), "http://10.0.0.5/secret")
			if err == nil || !strings.Contains(err.Error(), "non-public address") {
				t.Fatalf("%s: expected a non-public address error, got %v", variant, err)
			}
			if secretHits.Load() != 0 {
				t.Fatalf("%s: private target reached the proxy %d times", variant, secretHits.Load())
			}
		}
	})

	t.Run("redirect to a private target is refused", func(t *testing.T) {
		secretHits.Store(0)
		for variant, fetch := range guardedFetches {
			err := fetch(proxyCtx(t), "http://203.0.113.10/redirect")
			if err == nil || !strings.Contains(err.Error(), "non-public address") {
				t.Fatalf("%s: expected a non-public address error, got %v", variant, err)
			}
			if secretHits.Load() != 0 {
				t.Fatalf("%s: redirected private target reached the proxy %d times", variant, secretHits.Load())
			}
		}
	})

	t.Run("redirect to another scheme is refused", func(t *testing.T) {
		for variant, fetch := range guardedFetches {
			err := fetch(proxyCtx(t), "http://203.0.113.10/redirect-scheme")
			if err == nil || !strings.Contains(err.Error(), "unsupported scheme") {
				t.Fatalf("%s: expected an unsupported scheme error, got %v", variant, err)
			}
		}
	})

	t.Run("no proxy in context keeps the direct SSRF dialer", func(t *testing.T) {
		for variant, fetch := range guardedFetches {
			err := fetch(t.Context(), proxy.URL+"/img.png")
			if err == nil || !strings.Contains(err.Error(), "non-public address") {
				t.Fatalf("%s: expected the loopback proxy address itself to be refused as a direct target, got %v", variant, err)
			}
		}
	})
}

// TestFetchClientFor_OneClientPerResolvedProxy pins how fetch clients are cached: one
// client per distinct resolved proxy (so connections pool across fetches), a different
// client whenever any value that changes the route differs, the password included, and
// one shared direct client for every config that means "no proxy".
func TestFetchClientFor_OneClientPerResolvedProxy(t *testing.T) {
	clientFor := func(pc *schemas.ProxyConfig) *http.Client {
		t.Helper()
		ctx := context.WithValue(context.Background(), schemas.BifrostContextKeyProviderProxyConfig, pc)
		client, err := fetchClientFor(ctx)
		if err != nil {
			t.Fatalf("fetchClientFor: %v", err)
		}
		return client
	}
	proxy := func(password string) *schemas.ProxyConfig {
		return &schemas.ProxyConfig{
			Type:     schemas.HTTPProxy,
			URL:      schemas.NewSecretVar("http://127.0.0.1:3128"),
			Username: schemas.NewSecretVar("svc"),
			Password: schemas.NewSecretVar(password),
		}
	}

	reused := clientFor(proxy("one"))
	if clientFor(proxy("one")) != reused {
		t.Error("the same resolved proxy must reuse one client")
	}
	if clientFor(proxy("one")) == clientFor(proxy("two")) {
		t.Error("a different proxy password must get its own client")
	}
	direct := clientFor(nil)
	for name, pc := range map[string]*schemas.ProxyConfig{
		"none":       {Type: schemas.NoProxy},
		"empty type": {},
	} {
		if clientFor(pc) != direct {
			t.Errorf("%s must share the direct client", name)
		}
	}
	if clientFor(proxy("one")) == direct {
		t.Error("a proxied config must not get the direct client")
	}
	// An inherited global skip_tls_verify changes the client's TLS, so toggling it
	// must not keep serving the client built with the old setting.
	skipping := proxy("one")
	skipping.SkipTLSVerify = true
	if clientFor(skipping) == clientFor(proxy("one")) {
		t.Error("a proxy config that skips TLS verification must get its own client")
	}

	env := &schemas.ProxyConfig{Type: schemas.EnvProxy}
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:3128")
	first := clientFor(env)
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:3129")
	if clientFor(env) == first {
		t.Error("type environment must get a new client when the proxy variables change")
	}
}

// TestFetchAndEncodeURL_ProxiedHostnameTargetNeedsLocalDNS pins a deliberate limit: the
// SSRF check resolves the target on the Bifrost host even when the fetch is proxied. A
// hostname only the proxy can resolve is refused, never sent unchecked, because a name
// that resolves only at a corporate proxy can just as well name an intranet host. The
// target is under .invalid, which RFC 6761 guarantees never resolves, so the local lookup
// fails on every resolver (unlike .test, which some local resolvers answer).
func TestFetchAndEncodeURL_ProxiedHostnameTargetNeedsLocalDNS(t *testing.T) {
	var hits atomic.Int32
	proxy := proxytest.ForwardProxy(t, func(w http.ResponseWriter, r *http.Request) { hits.Add(1) })
	ctx := context.WithValue(t.Context(), schemas.BifrostContextKeyProviderProxyConfig, &schemas.ProxyConfig{
		Type: schemas.HTTPProxy,
		URL:  schemas.NewSecretVar(proxy.URL),
	})

	for variant, fetch := range guardedFetches {
		err := fetch(ctx, "http://only-the-proxy-resolves.invalid/img.png")
		if err == nil || !strings.Contains(err.Error(), "DNS lookup failed") {
			t.Fatalf("%s: expected a local DNS failure, got %v", variant, err)
		}
		if hits.Load() != 0 {
			t.Fatalf("%s: an unchecked target reached the proxy %d times", variant, hits.Load())
		}
	}
}

// TestFetchAndEncodeURL_ProxiedTunnelIsBoundToTheCheckedAddress pins that a proxied
// fetch opens its tunnel to the address the SSRF check approved, not to the hostname.
// The proxy would otherwise resolve the name itself, so a DNS answer that changed after
// the check could send the fetch to an internal address.
func TestFetchAndEncodeURL_ProxiedTunnelIsBoundToTheCheckedAddress(t *testing.T) {
	prev := fetchResolveTarget
	fetchResolveTarget = func(_ context.Context, host string) ([]net.IP, error) {
		if host == "pinned.example" {
			return []net.IP{net.ParseIP("203.0.113.10")}, nil
		}
		return nil, fmt.Errorf("DNS lookup failed for %s", host)
	}
	t.Cleanup(func() { fetchResolveTarget = prev })

	var targets []string
	var mu sync.Mutex
	proxy := proxytest.ForwardProxy(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		targets = append(targets, r.URL.Host)
		mu.Unlock()
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("PNGDATA"))
	})
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, schemas.BifrostContextKeyProviderProxyConfig, &schemas.ProxyConfig{
		Type: schemas.HTTPProxy,
		URL:  schemas.NewSecretVar(proxy.URL),
	})

	for variant, fetch := range guardedFetches {
		mu.Lock()
		targets = nil
		mu.Unlock()
		if err := fetch(ctx, "http://pinned.example/img.png"); err != nil {
			t.Fatalf("%s: fetch through proxy failed: %v", variant, err)
		}
		mu.Lock()
		if len(targets) != 1 || targets[0] != "203.0.113.10" {
			t.Fatalf("%s: proxy was asked for %v, want exactly the checked address [203.0.113.10]", variant, targets)
		}
		mu.Unlock()
	}
}

// TestFetchAndEncodeURL_EnvironmentProxyFollowsTheRequestScheme pins that an https
// fetch on a port other than 443 uses HTTPS_PROXY: the environment proxy is chosen by
// the request's scheme, not guessed from its port. Nothing about the target outlives
// the fetch: fetch targets come from requests, so a record kept per address would grow
// with every distinct host.
func TestFetchAndEncodeURL_EnvironmentProxyFollowsTheRequestScheme(t *testing.T) {
	prev := fetchResolveTarget
	fetchResolveTarget = func(_ context.Context, host string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("203.0.113.10")}, nil
	}
	t.Cleanup(func() { fetchResolveTarget = prev })

	plain := proxytest.NewHTTPRecorder(t, "http-proxy", "tcp4")
	secure := proxytest.NewHTTPRecorder(t, "https-proxy", "tcp4")
	for _, name := range []string{"http_proxy", "https_proxy", "no_proxy", "NO_PROXY", "ALL_PROXY", "all_proxy"} {
		t.Setenv(name, "")
	}
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:"+plain.Port())
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:"+secure.Port())

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, schemas.BifrostContextKeyProviderProxyConfig, &schemas.ProxyConfig{Type: schemas.EnvProxy})
	_, _, _ = FetchAndEncodeURL(ctx, "https://secure.example:8443/img.png")

	if seen := plain.Seen(); len(seen) != 0 {
		t.Fatalf("HTTP_PROXY carried the https fetch: %+v", seen)
	}
	if seen := secure.Seen(); len(seen) != 1 || seen[0].Target != "203.0.113.10:8443" {
		t.Fatalf("HTTPS_PROXY saw %+v, want one CONNECT to 203.0.113.10:8443", seen)
	}

	// Fork: the streamed media fetch chooses its proxy the same way and keeps no record either.
	if stream, err := FetchMediaStream(ctx, "https://secure.example:8443/clip.mp4", MediaStreamOptions{}); err == nil {
		_ = stream.Body.Close()
	}
	if seen := plain.Seen(); len(seen) != 0 {
		t.Fatalf("HTTP_PROXY carried the streamed https fetch: %+v", seen)
	}
	if seen := secure.Seen(); len(seen) != 2 || seen[1].Target != "203.0.113.10:8443" {
		t.Fatalf("HTTPS_PROXY saw %+v, want a second CONNECT to 203.0.113.10:8443", seen)
	}
	streamClient, err := fetchClientForLimits(ctx, mediaStreamLimits)
	if err != nil {
		t.Fatal(err)
	}
	if n := streamClient.Transport.(*fasthttpRoundTripper).schemes.size(); n != 0 {
		t.Fatalf("the streamed fetch client still holds %d scheme entries after the fetch", n)
	}

	client, err := fetchClientFor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rt := client.Transport.(*fasthttpRoundTripper)
	if rt.schemes == nil {
		t.Fatal("the fetch client keeps no scheme records, so its proxy dialer guesses by port")
	}
	if n := rt.schemes.size(); n != 0 {
		t.Fatalf("the fetch client still holds %d scheme entries after the fetch", n)
	}
}

// TestProviderHTTPClient_SchemeRecordsDoNotOutliveRequests: the auth client's scheme
// records are held only while a request needs them, so a client that reached many
// distinct hosts holds none once they are done.
func TestProviderHTTPClient_SchemeRecordsDoNotOutliveRequests(t *testing.T) {
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy", "NO_PROXY", "no_proxy", "ALL_PROXY", "all_proxy"} {
		t.Setenv(name, "")
	}
	client := NewProviderHTTPClient(nil, schemas.NetworkConfig{DefaultRequestTimeoutInSeconds: 2}, nil)
	rt := client.Transport.(*fasthttpRoundTripper)
	for range 5 {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"access_token":"t"}`))
		}))
		resp, err := client.Get(server.URL + "/token")
		if err != nil {
			server.Close()
			t.Fatal(err)
		}
		resp.Body.Close()
		server.Close()
	}
	if n := rt.schemes.size(); n != 0 {
		t.Fatalf("the auth client still holds %d scheme entries after its requests ended", n)
	}
}

// size reports how many addresses hold a scheme.
func (d *dialSchemes) size() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.entries)
}

// TestFetchAndEncodeURL_PreservesPathBytes pins that the fetch sends the resource path
// exactly as given: repeated slashes and escaped slashes in an object key are part of
// what a pre-signed URL's signature covers and of which object is selected, so the
// client must not normalize them, on the first request or after a redirect.
func TestFetchAndEncodeURL_PreservesPathBytes(t *testing.T) {
	const signedPath = "/bucket//dir%2Fkey.png"
	const redirectedPath = "/bucket//moved%2Fkey.png"
	var mu sync.Mutex
	var seen []string
	proxy := proxytest.ForwardProxy(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.RequestURI)
		mu.Unlock()
		switch r.RequestURI {
		case "/redirect":
			// Not http.Redirect: it path.Cleans a relative Location itself.
			w.Header().Set("Location", redirectedPath+"?X-Amz-Signature=def")
			w.WriteHeader(http.StatusFound)
		case signedPath + "?X-Amz-Signature=abc", redirectedPath + "?X-Amz-Signature=def":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("PNGDATA"))
		default:
			http.Error(w, "unexpected request URI "+r.RequestURI, http.StatusForbidden)
		}
	})
	proxyCtx := func(t *testing.T) context.Context {
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		t.Cleanup(cancel)
		return context.WithValue(ctx, schemas.BifrostContextKeyProviderProxyConfig, &schemas.ProxyConfig{
			Type: schemas.HTTPProxy,
			URL:  schemas.NewSecretVar(proxy.URL),
		})
	}

	for name, resource := range map[string]string{
		"direct":         "http://203.0.113.10" + signedPath + "?X-Amz-Signature=abc",
		"after redirect": "http://203.0.113.10/redirect",
	} {
		t.Run(name, func(t *testing.T) {
			mu.Lock()
			seen = nil
			mu.Unlock()
			for variant, fetch := range guardedFetches {
				mu.Lock()
				seen = nil
				mu.Unlock()
				if err := fetch(proxyCtx(t), resource); err != nil {
					mu.Lock()
					defer mu.Unlock()
					t.Fatalf("%s: fetch failed: %v; upstream saw request URIs %v", variant, err, seen)
				}
			}
		})
	}
}

// TestFetchMediaURL_RawBytesAndMediaLimit pins the fork's media fetch: raw bytes through the same
// proxy-aware, SSRF-checked client, with a body cap above the 25 MiB inline-fetch limit, and a
// private target still refused.
func TestFetchMediaURL_RawBytesAndMediaLimit(t *testing.T) {
	large := bytes.Repeat([]byte("v"), int(maxFetchBytes)+1024)
	var privateHits atomic.Int32
	proxy := proxytest.ForwardProxy(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host == "10.0.0.5" {
			privateHits.Add(1)
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Host != "203.0.113.10" {
			http.Error(w, "unexpected target "+r.URL.Host, http.StatusBadGateway)
			return
		}
		switch r.URL.Path {
		case "/clip.mp4":
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write([]byte("MP4DATA"))
		case "/large.mp4":
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write(large)
		default:
			http.NotFound(w, r)
		}
	})
	proxyCtx := func(t *testing.T) context.Context {
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		t.Cleanup(cancel)
		return context.WithValue(ctx, schemas.BifrostContextKeyProviderProxyConfig, &schemas.ProxyConfig{
			Type: schemas.HTTPProxy,
			URL:  schemas.NewSecretVar(proxy.URL),
		})
	}

	mediaType, body, err := FetchMediaURL(proxyCtx(t), "http://203.0.113.10/clip.mp4")
	if err != nil {
		t.Fatalf("FetchMediaURL: %v", err)
	}
	if mediaType != "video/mp4" || string(body) != "MP4DATA" {
		t.Errorf("got %q %q, want video/mp4 MP4DATA", mediaType, body)
	}

	if _, _, err := FetchAndEncodeURL(proxyCtx(t), "http://203.0.113.10/large.mp4"); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("inline fetch of %d bytes should exceed its limit, got %v", len(large), err)
	}
	if _, body, err := FetchMediaURL(proxyCtx(t), "http://203.0.113.10/large.mp4"); err != nil || len(body) != len(large) {
		t.Fatalf("media fetch of %d bytes: len=%d err=%v", len(large), len(body), err)
	}

	if _, _, err := FetchMediaURL(proxyCtx(t), "http://10.0.0.5/secret.mp4"); err == nil || !strings.Contains(err.Error(), "non-public address") {
		t.Fatalf("expected a non-public address error, got %v", err)
	}
	if privateHits.Load() != 0 {
		t.Fatalf("private target reached the proxy %d times", privateHits.Load())
	}
}

// TestVideoContentTypeOr pins the type a downloaded video is served with: a well-formed video/*
// type is kept (lowercased), and every other type, a missing one and a malformed one become the
// caller's fallback, so an upstream cannot make the gateway serve a document.
func TestVideoContentTypeOr(t *testing.T) {
	for _, tc := range []struct{ in, fallback, want string }{
		{"video/mp4", "video/mp4", "video/mp4"},
		{"video/webm", "video/mp4", "video/webm"},
		{" Video/QuickTime ", "video/mp4", "video/quicktime"},
		{"video/vnd.dlna.mpeg-tts", "video/mp4", "video/vnd.dlna.mpeg-tts"},
		{"video/3gpp+x", "video/mp4", "video/3gpp+x"},
		{"", "video/mp4", "video/mp4"},
		{"video/", "video/mp4", "video/mp4"},
		{"text/html", "video/mp4", "video/mp4"},
		{"image/svg+xml", "video/quicktime", "video/quicktime"},
		{"application/octet-stream", "video/mp4", "video/mp4"},
		{"video/mp4<script>", "video/mp4", "video/mp4"},
		{"video/mp4; codecs=avc1", "video/mp4", "video/mp4"},
		{"video/mp4\r\nSet-Cookie: a=b", "video/quicktime", "video/quicktime"},
	} {
		if got := VideoContentTypeOr(tc.in, tc.fallback); got != tc.want {
			t.Errorf("VideoContentTypeOr(%q, %q) = %q, want %q", tc.in, tc.fallback, got, tc.want)
		}
	}
}

// benchFetchProxy serves body for http://203.0.113.10/<anything> through a loopback forward
// proxy, so the fetch benchmarks run offline through the real SSRF-checked, proxied client.
func benchFetchProxy(b *testing.B, body []byte) context.Context {
	b.Helper()
	proxy := proxytest.ForwardProxy(b, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write(body)
	})
	return context.WithValue(b.Context(), schemas.BifrostContextKeyProviderProxyConfig, &schemas.ProxyConfig{
		Type: schemas.HTTPProxy,
		URL:  schemas.NewSecretVar(proxy.URL),
	})
}

// BenchmarkFetchClientFor is the per-fetch cached client lookup, direct and proxied.
func BenchmarkFetchClientFor(b *testing.B) {
	b.Run("direct", func(b *testing.B) {
		ctx := b.Context()
		b.ReportAllocs()
		for b.Loop() {
			if _, err := fetchClientFor(ctx); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("proxied", func(b *testing.B) {
		ctx := context.WithValue(b.Context(), schemas.BifrostContextKeyProviderProxyConfig, &schemas.ProxyConfig{
			Type: schemas.HTTPProxy,
			URL:  schemas.NewSecretVar("http://127.0.0.1:3128"),
		})
		b.ReportAllocs()
		for b.Loop() {
			if _, err := fetchClientFor(ctx); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkFetchAndEncodeURL is an inline image or document fetch through a proxy.
func BenchmarkFetchAndEncodeURL(b *testing.B) {
	for _, size := range []int{4 << 10, 1 << 20} {
		b.Run(strconv.Itoa(size>>10)+"KiB", func(b *testing.B) {
			ctx := benchFetchProxy(b, bytes.Repeat([]byte("i"), size))
			b.ReportAllocs()
			for b.Loop() {
				if _, _, err := FetchAndEncodeURL(ctx, "http://203.0.113.10/img.png"); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkFetchMediaURL is the fork's provider-hosted media download through a proxy (new: no
// earlier implementation to compare against).
func BenchmarkFetchMediaURL(b *testing.B) {
	for _, size := range []int{4 << 10, 1 << 20, 16 << 20} {
		b.Run(strconv.Itoa(size>>10)+"KiB", func(b *testing.B) {
			ctx := benchFetchProxy(b, bytes.Repeat([]byte("v"), size))
			b.ReportAllocs()
			for b.Loop() {
				if _, body, err := FetchMediaURL(ctx, "http://203.0.113.10/clip.mp4"); err != nil || len(body) != size {
					b.Fatalf("len=%d err=%v", len(body), err)
				}
			}
		})
	}
}

// mediaStreamProxyCtx is a context whose provider proxy is the given proxy, so a streamed
// fetch of a public-looking address reaches the test's handler on loopback.
func mediaStreamProxyCtx(t testing.TB, proxyURL string, timeout time.Duration) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	t.Cleanup(cancel)
	return context.WithValue(ctx, schemas.BifrostContextKeyProviderProxyConfig, &schemas.ProxyConfig{
		Type: schemas.HTTPProxy,
		URL:  schemas.NewSecretVar(proxyURL),
	})
}

// TestFetchMediaStreamStreamsPastTheBufferedCap pins the first acceptance criterion of
// TXE-7036: a file larger than the buffered media fetch's 256 MiB cap is forwarded, and it is
// never held. The allocation bound is the one TestFetchVideoContentStreamsPastTheOldCap puts
// on the provider-client download.
func TestFetchMediaStreamStreamsPastTheBufferedCap(t *testing.T) {
	fileBytes := int(mediaFetchLimits.maxBytes) + 8<<20
	proxy := proxytest.NewOriginTunnel(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4; codecs=avc1")
		w.Header().Set("Content-Length", strconv.Itoa(fileBytes))
		chunk := make([]byte, 1<<20)
		for written := 0; written < fileBytes; written += len(chunk) {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	})
	ctx := mediaStreamProxyCtx(t, proxy.URL, 2*time.Minute)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	stream, err := FetchMediaStream(ctx, "http://203.0.113.10/large.mp4?X-Amz-Signature=abc", MediaStreamOptions{})
	if err != nil {
		t.Fatalf("FetchMediaStream: %v", err)
	}
	drained, err := io.CopyBuffer(io.Discard, stream.Body, make([]byte, 64<<10))
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if err := stream.Body.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	runtime.ReadMemStats(&after)

	if drained != int64(fileBytes) || stream.ContentLength != int64(fileBytes) {
		t.Fatalf("drained %d bytes, content length %d, want %d", drained, stream.ContentLength, fileBytes)
	}
	if stream.ContentType != "video/mp4" {
		t.Fatalf("content type %q, want video/mp4", stream.ContentType)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 32<<20 {
		t.Fatalf("forwarding a %d MiB file allocated %d MiB; the body is being buffered", fileBytes>>20, allocated>>20)
	}

	// The buffered fetch still refuses the same file: the cap is dropped for the stream only.
	if _, _, err := FetchMediaURL(ctx, "http://203.0.113.10/large.mp4"); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("buffered media fetch of %d bytes should exceed its limit, got %v", fileBytes, err)
	}
}

// TestFetchMediaStreamDeliversBytesBeforeTheVendorFinishes proves the body is a live stream:
// the vendor sends half the file and waits for the caller to have read it before sending the
// rest. A fetch that buffered would never return.
func TestFetchMediaStreamDeliversBytesBeforeTheVendorFinishes(t *testing.T) {
	firstHalfRead := make(chan struct{})
	proxy := proxytest.NewOriginTunnel(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write([]byte("FIRST-HALF|"))
		w.(http.Flusher).Flush()
		select {
		case <-firstHalfRead:
		case <-time.After(10 * time.Second):
			return
		}
		_, _ = w.Write([]byte("SECOND-HALF"))
	})
	stream, err := FetchMediaStream(mediaStreamProxyCtx(t, proxy.URL, 20*time.Second), "http://203.0.113.10/clip.mp4", MediaStreamOptions{})
	if err != nil {
		t.Fatalf("FetchMediaStream: %v", err)
	}
	defer stream.Body.Close()
	if stream.ContentLength != -1 {
		t.Fatalf("content length %d for a chunked file, want -1", stream.ContentLength)
	}
	first := make([]byte, len("FIRST-HALF|"))
	if _, err := io.ReadFull(stream.Body, first); err != nil || string(first) != "FIRST-HALF|" {
		t.Fatalf("first half: %q, %v", first, err)
	}
	close(firstHalfRead)
	rest, err := io.ReadAll(stream.Body)
	if err != nil || string(rest) != "SECOND-HALF" {
		t.Fatalf("second half: %q, %v", rest, err)
	}
}

// TestFetchMediaStreamBoundsAStalledVendor pins what replaces the buffered fetch's bound on
// the whole request: a vendor that stops sending fails the read after the idle timeout, a
// context that ends fails it at once, and a reader that is merely slow is not timed out.
func TestFetchMediaStreamBoundsAStalledVendor(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	proxy := proxytest.NewOriginTunnel(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write([]byte("HEAD|"))
		w.(http.Flusher).Flush()
		if r.URL.Path == "/complete.mp4" {
			_, _ = w.Write([]byte("TAIL"))
			return
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})

	t.Run("idle timeout", func(t *testing.T) {
		stream, err := FetchMediaStream(mediaStreamProxyCtx(t, proxy.URL, 20*time.Second), "http://203.0.113.10/stalled.mp4", MediaStreamOptions{IdleTimeout: 200 * time.Millisecond})
		if err != nil {
			t.Fatalf("FetchMediaStream: %v", err)
		}
		defer stream.Body.Close()
		start := time.Now()
		got, err := io.ReadAll(stream.Body)
		if !errors.Is(err, ErrStreamIdleTimeout) || string(got) != "HEAD|" {
			t.Fatalf("read %q, %v; want HEAD| and ErrStreamIdleTimeout", got, err)
		}
		if waited := time.Since(start); waited > 5*time.Second {
			t.Fatalf("the stalled read took %s to fail", waited)
		}
	})

	t.Run("context ended", func(t *testing.T) {
		ctx, cancel := context.WithCancel(mediaStreamProxyCtx(t, proxy.URL, 20*time.Second))
		defer cancel()
		stream, err := FetchMediaStream(ctx, "http://203.0.113.10/stalled.mp4", MediaStreamOptions{IdleTimeout: time.Minute})
		if err != nil {
			t.Fatalf("FetchMediaStream: %v", err)
		}
		defer stream.Body.Close()
		head := make([]byte, len("HEAD|"))
		if _, err := io.ReadFull(stream.Body, head); err != nil {
			t.Fatalf("head: %v", err)
		}
		time.AfterFunc(100*time.Millisecond, cancel)
		start := time.Now()
		if _, err := stream.Body.Read(head); !errors.Is(err, context.Canceled) {
			t.Fatalf("read after cancel: %v, want context.Canceled", err)
		}
		if waited := time.Since(start); waited > 5*time.Second {
			t.Fatalf("the cancelled read took %s to fail", waited)
		}
	})

	t.Run("a slow reader is not a stalled vendor", func(t *testing.T) {
		stream, err := FetchMediaStream(mediaStreamProxyCtx(t, proxy.URL, 20*time.Second), "http://203.0.113.10/complete.mp4", MediaStreamOptions{IdleTimeout: 150 * time.Millisecond})
		if err != nil {
			t.Fatalf("FetchMediaStream: %v", err)
		}
		defer stream.Body.Close()
		head := make([]byte, len("HEAD|"))
		if _, err := io.ReadFull(stream.Body, head); err != nil {
			t.Fatalf("head: %v", err)
		}
		time.Sleep(450 * time.Millisecond)
		rest, err := io.ReadAll(stream.Body)
		if err != nil || string(rest) != "TAIL" {
			t.Fatalf("after a pause longer than the idle timeout: %q, %v", rest, err)
		}
	})
}

// TestFetchMediaStreamStatusEncodingAndCredentials covers what the stream does with the
// vendor's answer and the caller's credential: a refusal is a typed status error whose body
// is not waited for, a gzip body is decoded, another encoding is refused, and a header the
// caller supplied reaches the host the URL names and no host a redirect leads to.
func TestFetchMediaStreamStatusEncodingAndCredentials(t *testing.T) {
	type seenRequest struct{ host, path, apiKey, auth string }
	var mu sync.Mutex
	var seen []seenRequest
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	proxy := proxytest.NewOriginTunnel(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, seenRequest{r.Host, r.URL.Path, r.Header.Get("x-goog-api-key"), r.Header.Get("Authorization")})
		mu.Unlock()
		switch r.URL.Path {
		case "/gone.mp4":
			// The refusal's headers arrive and its body never ends.
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("AccessDenied"))
			w.(http.Flusher).Flush()
			select {
			case <-release:
			case <-r.Context().Done():
			}
		case "/gzip.mp4":
			w.Header().Set("Content-Type", "video/mp4")
			w.Header().Set("Content-Encoding", "gzip")
			gz := gzip.NewWriter(w)
			_, _ = gz.Write([]byte("MP4DATA"))
			_ = gz.Close()
		case "/br.mp4":
			w.Header().Set("Content-Encoding", "br")
			_, _ = w.Write([]byte("not really brotli"))
		case "/same-host":
			http.Redirect(w, r, "/clip.mp4", http.StatusFound)
		case "/other-host":
			http.Redirect(w, r, "http://203.0.113.20/clip.mp4", http.StatusFound)
		case "/clip.mp4":
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write([]byte("MP4DATA"))
		default:
			http.NotFound(w, r)
		}
	})
	ctx := func(t *testing.T) context.Context { return mediaStreamProxyCtx(t, proxy.URL, 5*time.Second) }

	t.Run("refusal", func(t *testing.T) {
		start := time.Now()
		_, err := FetchMediaStream(ctx(t), "http://203.0.113.10/gone.mp4?sig=deadbeefcafe", MediaStreamOptions{})
		var statusErr *MediaFetchStatusError
		if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusForbidden {
			t.Fatalf("got %v, want a MediaFetchStatusError for 403", err)
		}
		if strings.Contains(err.Error(), "deadbeefcafe") {
			t.Fatalf("the status error leaks the signed query: %v", err)
		}
		if waited := time.Since(start); waited > 2*time.Second {
			t.Fatalf("the refusal took %s: its unending body was waited for", waited)
		}
	})

	t.Run("gzip is decoded", func(t *testing.T) {
		stream, err := FetchMediaStream(ctx(t), "http://203.0.113.10/gzip.mp4", MediaStreamOptions{})
		if err != nil {
			t.Fatalf("FetchMediaStream: %v", err)
		}
		defer stream.Body.Close()
		got, err := io.ReadAll(stream.Body)
		if err != nil || string(got) != "MP4DATA" || stream.ContentLength != -1 {
			t.Fatalf("got %q, %v, content length %d; want MP4DATA and no length", got, err, stream.ContentLength)
		}
	})

	t.Run("another encoding is refused", func(t *testing.T) {
		if _, err := FetchMediaStream(ctx(t), "http://203.0.113.10/br.mp4", MediaStreamOptions{}); err == nil || !strings.Contains(err.Error(), "Content-Encoding") {
			t.Fatalf("got %v, want an unsupported Content-Encoding error", err)
		}
	})

	header := http.Header{"x-goog-api-key": {"vendor-key"}, "Authorization": {"Bearer vendor-token"}}
	fetchers := map[string]func(ctx context.Context, resourceURL string) (string, error){
		"stream": func(ctx context.Context, resourceURL string) (string, error) {
			stream, err := FetchMediaStream(ctx, resourceURL, MediaStreamOptions{Header: header})
			if err != nil {
				return "", err
			}
			defer stream.Body.Close()
			got, err := io.ReadAll(stream.Body)
			return string(got), err
		},
		"buffered": func(ctx context.Context, resourceURL string) (string, error) {
			_, got, err := FetchMediaURLWithHeader(ctx, resourceURL, header)
			return string(got), err
		},
	}
	for variant, fetch := range fetchers {
		for path, wantOnLastHop := range map[string]string{"/same-host": "vendor-key", "/other-host": ""} {
			t.Run("credential "+variant+path, func(t *testing.T) {
				mu.Lock()
				seen = nil
				mu.Unlock()
				got, err := fetch(ctx(t), "http://203.0.113.10"+path)
				if err != nil || got != "MP4DATA" {
					t.Fatalf("got %q, %v", got, err)
				}
				mu.Lock()
				defer mu.Unlock()
				if len(seen) != 2 {
					t.Fatalf("saw %d requests, want the first hop and the redirect: %+v", len(seen), seen)
				}
				if seen[0].apiKey != "vendor-key" || seen[0].auth != "Bearer vendor-token" {
					t.Fatalf("the host the URL names did not get the credential: %+v", seen[0])
				}
				if seen[1].apiKey != wantOnLastHop || (wantOnLastHop == "") != (seen[1].auth == "") {
					t.Fatalf("redirect hop to %s carried key %q and authorization %q", seen[1].host, seen[1].apiKey, seen[1].auth)
				}
			})
		}
	}
}

// TestFetchMediaStreamReturnsItsConnections: a file read to its end gives its connection back
// to the pool, so the next download reuses it, and one abandoned half-read does not poison
// the pool for the download after it.
func TestFetchMediaStreamReturnsItsConnections(t *testing.T) {
	proxy := proxytest.NewOriginTunnel(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write(bytes.Repeat([]byte("v"), 256<<10))
	})
	ctx := mediaStreamProxyCtx(t, proxy.URL, 20*time.Second)
	fetch := func(readAll bool) {
		t.Helper()
		stream, err := FetchMediaStream(ctx, "http://203.0.113.10/clip.mp4", MediaStreamOptions{})
		if err != nil {
			t.Fatalf("FetchMediaStream: %v", err)
		}
		if readAll {
			if n, err := io.Copy(io.Discard, stream.Body); err != nil || n != 256<<10 {
				t.Fatalf("read %d bytes, %v", n, err)
			}
		} else if _, err := io.ReadFull(stream.Body, make([]byte, 16)); err != nil {
			t.Fatalf("partial read: %v", err)
		}
		if err := stream.Body.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
	}
	fetch(true)
	before := proxy.Connects()
	fetch(true)
	if got := proxy.Connects(); got != before {
		t.Fatalf("a second download opened %d new connections; the first one's was not pooled", got-before)
	}
	fetch(false)
	fetch(true)
	fetch(true)
}

// BenchmarkFetchMediaStream is the streamed media download through a proxy (fork, TXE-7036):
// what a forwarded file costs, to set against BenchmarkFetchMediaURL holding the same file.
func BenchmarkFetchMediaStream(b *testing.B) {
	for _, size := range []int{1 << 20, 16 << 20} {
		b.Run(strconv.Itoa(size>>20)+"MiB", func(b *testing.B) {
			body := bytes.Repeat([]byte("v"), size)
			proxy := proxytest.NewOriginTunnel(b, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "video/mp4")
				_, _ = w.Write(body)
			})
			ctx := context.WithValue(b.Context(), schemas.BifrostContextKeyProviderProxyConfig, &schemas.ProxyConfig{
				Type: schemas.HTTPProxy,
				URL:  schemas.NewSecretVar(proxy.URL),
			})
			buf := make([]byte, 64<<10)
			b.SetBytes(int64(size))
			b.ReportAllocs()
			for b.Loop() {
				stream, err := FetchMediaStream(ctx, "http://203.0.113.10/clip.mp4", MediaStreamOptions{})
				if err != nil {
					b.Fatal(err)
				}
				n, err := io.CopyBuffer(io.Discard, stream.Body, buf)
				_ = stream.Body.Close()
				if err != nil || n != int64(size) {
					b.Fatalf("read %d bytes, %v", n, err)
				}
			}
		})
	}
}

// BenchmarkVideoContentTypeOr is the per-download check of the type a video is served with (the
// rule MiniMax's download already applied, now shared with BytePlus).
func BenchmarkVideoContentTypeOr(b *testing.B) {
	for _, tc := range []struct{ name, contentType string }{
		{"video_mp4", "video/mp4"},
		{"text_html", "text/html; charset=utf-8"},
		{"empty", ""},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if got := VideoContentTypeOr(tc.contentType, "video/mp4"); got != "video/mp4" {
					b.Fatalf("got %q", got)
				}
			}
		})
	}
}
