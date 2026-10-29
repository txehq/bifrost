package oauth2

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/capsohq/bifrost/core/network"
	"github.com/capsohq/bifrost/core/network/proxytest"
	"github.com/stretchr/testify/require"
)

// TestNewOAuthDiscoveryHTTPClientBlocksLoopback proves the OAuth discovery HTTP
// client is wired through the SSRF-safe dialer: a target the discovery chain
// picked up from a upstream MCP server's own response
// must not be reachable if it points at loopback or any other private/
// link-local/CGNAT address. A context that records no destination policy, and
// one recording the public-only policy, both stay public-only even when the
// private-target allowlist names the address. TestMain overrides the dialer
// package-wide for this package's other tests (which talk to real
// httptest.Server loopback addresses) - restore the production dialer for the
// duration of this test so it exercises the actual guard.
func TestNewOAuthDiscoveryHTTPClientBlocksLoopback(t *testing.T) {
	useProductionOAuthDialer(t, "127.0.0.1")

	for _, ctx := range []context.Context{context.Background(), withRequirePublicTarget(context.Background(), true)} {
		client := newOAuthDiscoveryHTTPClient(ctx, time.Second)
		transport, ok := client.Transport.(*network.ProxyAwareTransport)
		require.True(t, ok)
		require.NotNil(t, transport.Direct.DialContext)

		_, err := transport.Direct.DialContext(context.Background(), "tcp", "127.0.0.1:80")
		require.Error(t, err)
		require.Contains(t, err.Error(), "blocked connection to non-public address")
	}
}

// TestNewOAuthDiscoveryHTTPClientFollowsPrivateTargetAllowlist pins the
// allowlisted policy an authenticated admin's MCP client gets: a loopback OAuth
// server is reachable once allowlisted, refused when it is not, and the cloud
// metadata address is refused even with an allowlist in place.
func TestNewOAuthDiscoveryHTTPClientFollowsPrivateTargetAllowlist(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	ctx := withRequirePublicTarget(context.Background(), false)

	useProductionOAuthDialer(t)
	_, err := newOAuthDiscoveryHTTPClient(ctx, 2*time.Second).Get(server.URL)
	require.Error(t, err)
	require.Contains(t, err.Error(), "blocked connection to non-public address")

	useProductionOAuthDialer(t, "127.0.0.1")
	resp, err := newOAuthDiscoveryHTTPClient(ctx, 2*time.Second).Get(server.URL)
	require.NoError(t, err, "an allowlisted loopback OAuth server must be reachable")
	resp.Body.Close()

	transport, ok := newOAuthDiscoveryHTTPClient(ctx, time.Second).Transport.(*network.ProxyAwareTransport)
	require.True(t, ok)
	_, err = transport.Direct.DialContext(context.Background(), "tcp", "169.254.169.254:80")
	require.Error(t, err)
	require.Contains(t, err.Error(), "blocked connection to link-local address")
}

// TestSetPrivateTargetAllowlistDropsConnectionsApprovedUnderTheOldList pins
// that removing a host from the allowlist takes effect for the next request:
// the dial-time guard only runs when a connection is created, so a keep-alive
// connection approved under the old list must not be reused afterwards.
func TestSetPrivateTargetAllowlistDropsConnectionsApprovedUnderTheOldList(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	ctx := withRequirePublicTarget(context.Background(), false)

	useProductionOAuthDialer(t, "127.0.0.1")
	resp, err := newOAuthDiscoveryHTTPClient(ctx, 2*time.Second).Get(server.URL)
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close() // the connection goes back to the pool for reuse

	SetPrivateTargetAllowlist(nil)
	resp, err = newOAuthDiscoveryHTTPClient(ctx, 2*time.Second).Get(server.URL)
	if resp != nil {
		resp.Body.Close()
	}
	require.Error(t, err, "a pooled connection approved under the old allowlist must not be reused")
	require.Contains(t, err.Error(), "blocked connection to non-public address")
}

// TestNewOAuthDiscoveryTransportHonorsProxyEnvironment pins that the shared
// transport still routes through HTTPS_PROXY/HTTP_PROXY the way the bare
// default-transport clients it replaced did: a proxy-only installation must
// keep discovery, registration and token exchange working.
func TestNewOAuthDiscoveryTransportHonorsProxyEnvironment(t *testing.T) {
	transport := newOAuthTransport((&net.Dialer{}).DialContext, network.ProxiedDestinationPolicy(true))
	require.NotNil(t, transport.Proxy, "transport must consult the proxy environment")
	require.NotNil(t, transport.CheckDestination, "a proxied destination must be checked before the proxy is contacted")
}

// TestNewOAuthDiscoveryHTTPClientRefusesRedirectForPOST pins that a 307/308 on
// the token or registration POST is not followed: Go would otherwise replay the
// credential-bearing body at whatever location the upstream server names.
func TestNewOAuthDiscoveryHTTPClientRefusesRedirectForPOST(t *testing.T) {
	hits := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	client := newOAuthDiscoveryHTTPClient(context.Background(), 2*time.Second)
	resp, err := client.Post(redirector.URL, "application/x-www-form-urlencoded", strings.NewReader("grant_type=authorization_code&code=secret"))
	if resp != nil {
		resp.Body.Close()
	}
	require.Error(t, err, "a redirected POST must fail rather than be replayed")
	require.Zero(t, hits, "the redirect target must never receive the replayed POST body")

	// Discovery GETs may still follow a redirect.
	resp, err = client.Get(redirector.URL)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, 1, hits)
}

// oauthTestResolver answers per hostname; a name it does not know fails to resolve.
type oauthTestResolver map[string][]net.IP

func (r oauthTestResolver) LookupIP(_ context.Context, _ string, host string) ([]net.IP, error) {
	if ips, ok := r[host]; ok {
		return ips, nil
	}
	return nil, errors.New("no such host")
}

// rebindingResolver answers each name from a sequence, the last answer repeating, and
// counts lookups per name: a name whose first answer is public and whose later answers
// are private stands in for DNS rebinding, and the count shows how often the transport
// asked.
type rebindingResolver struct {
	mu      sync.Mutex
	answers map[string][][]net.IP
	lookups map[string]int
}

func newRebindingResolver(answers map[string][][]net.IP) *rebindingResolver {
	return &rebindingResolver{answers: answers, lookups: map[string]int{}}
}

func (r *rebindingResolver) LookupIP(_ context.Context, _ string, host string) ([]net.IP, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	seq, ok := r.answers[host]
	if !ok || len(seq) == 0 {
		return nil, errors.New("no such host")
	}
	n := r.lookups[host]
	r.lookups[host] = n + 1
	if n >= len(seq) {
		n = len(seq) - 1
	}
	return seq[n], nil
}

func (r *rebindingResolver) calls(host string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lookups[host]
}

// fixedOAuthProxy is a proxy selector that sends every request through raw.
func fixedOAuthProxy(t *testing.T, raw string) func(*http.Request) (*url.URL, error) {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err)
	return func(*http.Request) (*url.URL, error) { return u, nil }
}

// oauthTestTransport builds the OAuth transport for one destination policy, as
// oauthDiscoveryTransport (strict) and newAllowlistedTransport (allowlisted) do, with
// the proxy selector and resolver injected.
func oauthTestTransport(strict bool, allow func() *network.Allowlist, proxy func(*http.Request) (*url.URL, error), resolver network.IPLookuper) *network.ProxyAwareTransport {
	if strict {
		return newOAuthTransportWith(network.SSRFSafeDialContext(2*time.Second), network.ProxiedDestinationPolicy(true), proxy, resolver)
	}
	return newOAuthTransportWith(network.AllowlistedPrivateDialContext(2*time.Second, allow), network.AllowlistedPrivatePolicy(allow), proxy, resolver)
}

// TestOAuthProxiedRequestsTunnelToTheCheckedAddress pins TXE-5377: a proxied OAuth
// request is tunnelled to the address the destination check approved, so the proxy
// never resolves the name itself. The resolver answers rebind.example public once
// and metadata afterwards; a second lookup anywhere (the proxy's, or the dialer's)
// would see the metadata answer. Every proxy kind (http and socks5 with credentials,
// and an https global proxy reached with its skip_tls_verify setting), both request
// schemes and both destination policies are crossed. Before the fix the proxy was
// handed "GET rebind.example" and "CONNECT rebind.example:443".
func TestOAuthProxiedRequestsTunnelToTheCheckedAddress(t *testing.T) {
	httpProxy := proxytest.NewHTTPRecorder(t, "http", "tcp4")
	socksProxy := proxytest.NewSOCKS5Recorder(t, "socks")
	tlsProxy := proxytest.NewHTTPSRecorder(t, "https")
	tlsProxyURL := "https://127.0.0.1:" + tlsProxy.Port()
	previous := network.DefaultHTTPClientFactory()
	network.SetDefaultHTTPClientFactory(network.NewHTTPClientFactory(&network.GlobalProxyConfig{
		Enabled: true, Type: network.GlobalProxyTypeHTTP, URL: tlsProxyURL, SkipTLSVerify: true, EnableForAPI: true,
	}, nil))
	t.Cleanup(func() { network.SetDefaultHTTPClientFactory(previous) })
	al, err := network.ValidatePrivateTargetAllowlist([]string{"10.0.0.0/8"})
	require.NoError(t, err)
	allow := func() *network.Allowlist { return al }
	creds := proxytest.User + ":" + proxytest.Pass + "@"

	for _, proxy := range []struct {
		name string
		rec  *proxytest.Recorder
		url  string
		auth string
	}{
		{"http proxy with credentials", httpProxy, "http://" + creds + "127.0.0.1:" + httpProxy.Port(), proxytest.BasicAuth},
		{"socks5 proxy with credentials", socksProxy, "socks5://" + creds + "127.0.0.1:" + socksProxy.Port(), proxytest.SOCKSAuth(proxytest.User, proxytest.Pass)},
		{"https global proxy", tlsProxy, tlsProxyURL, ""},
	} {
		for _, scheme := range []string{"http", "https"} {
			for _, strict := range []bool{true, false} {
				t.Run(fmt.Sprintf("%s/%s/strict=%v", proxy.name, scheme, strict), func(t *testing.T) {
					proxy.rec.Reset()
					resolver := newRebindingResolver(map[string][][]net.IP{
						"rebind.example": {{net.ParseIP("93.184.216.34")}, {net.ParseIP("169.254.169.254")}},
					})
					client := newOAuthHTTPClient(oauthTestTransport(strict, allow, fixedOAuthProxy(t, proxy.url), resolver), 3*time.Second)
					if resp, err := client.Get(scheme + "://rebind.example/.well-known/oauth-authorization-server"); err == nil {
						resp.Body.Close()
					}
					port := "80"
					if scheme == "https" {
						port = "443"
					}
					seen := proxy.rec.Seen()
					require.Len(t, seen, 1, "proxy saw %+v", seen)
					require.Equal(t, net.JoinHostPort("93.184.216.34", port), seen[0].Target, "the proxy must be asked for the address the check approved, never the name")
					require.Equal(t, proxy.auth, seen[0].Auth, "the proxy credentials must still be presented")
					require.Equal(t, 1, resolver.calls("rebind.example"), "the destination is resolved once, by the check")
				})
			}
		}
	}
}

// TestOAuthProxiedDestinationPolicy pins the destination policy on the proxied path for
// both OAuth transports: a public literal, or a hostname resolving to public addresses
// only, is tunnelled to the checked address; a private, loopback or link-local literal,
// a hostname resolving to one (mixed answers included), and a hostname the gateway
// cannot resolve are refused before the proxy is contacted (TXE-5086). The allowlisted
// policy also lets an allowlisted address through, never a metadata one, and reads the
// allowlist per request.
func TestOAuthProxiedDestinationPolicy(t *testing.T) {
	resolver := oauthTestResolver{
		"auth.example.com":   {net.ParseIP("93.184.216.34")},
		"internal.corp":      {net.ParseIP("10.0.0.7")},
		"mixed.example.com":  {net.ParseIP("93.184.216.34"), net.ParseIP("127.0.0.1")},
		"metadata.attack.io": {net.ParseIP("169.254.169.254")},
		"unlisted.corp":      {net.ParseIP("93.184.216.34"), net.ParseIP("10.0.0.5")},
	}
	rec := proxytest.NewHTTPRecorder(t, "proxy", "tcp4")
	proxy := fixedOAuthProxy(t, "http://127.0.0.1:"+rec.Port())
	al, err := network.ValidatePrivateTargetAllowlist([]string{"127.0.0.1", "10.0.0.7"})
	require.NoError(t, err)
	var current atomic.Pointer[network.Allowlist]
	current.Store(al)
	strict := newOAuthHTTPClient(oauthTestTransport(true, nil, proxy, resolver), 2*time.Second)
	allowlisted := newOAuthHTTPClient(oauthTestTransport(false, current.Load, proxy, resolver), 2*time.Second)

	assertRoute := func(t *testing.T, client *http.Client, raw, want string) {
		t.Helper()
		rec.Reset()
		resp, err := client.Get(raw)
		if resp != nil {
			resp.Body.Close()
		}
		seen := rec.Seen()
		if want == "" {
			require.Error(t, err, raw)
			require.Contains(t, err.Error(), "blocked proxied OAuth connection", raw)
			require.Empty(t, seen, "%s must be refused before the proxy is contacted", raw)
			return
		}
		require.Len(t, seen, 1, "%s: proxy saw %+v", raw, seen)
		require.Equal(t, want, seen[0].Target, raw)
	}
	for _, tc := range []struct{ raw, strict, allowlisted string }{
		{"http://203.0.113.5/.well-known/oauth-authorization-server", "203.0.113.5:80", "203.0.113.5:80"},
		{"https://auth.example.com/token", "93.184.216.34:443", "93.184.216.34:443"},
		{"http://127.0.0.1:8080/token", "", "127.0.0.1:8080"},
		{"https://internal.corp/token", "", "10.0.0.7:443"},
		{"https://mixed.example.com/register", "", "93.184.216.34:443"},
		{"http://10.0.0.5/register", "", ""},
		{"http://169.254.169.254/latest/meta-data/", "", ""},
		{"https://metadata.attack.io/token", "", ""},
		{"https://unlisted.corp/register", "", ""},
		{"https://only-the-proxy-knows.example/token", "", ""},
	} {
		assertRoute(t, strict, tc.raw, tc.strict)
		assertRoute(t, allowlisted, tc.raw, tc.allowlisted)
	}

	// An allowlist removal applies to the next proxied request on the same transport.
	current.Store(nil)
	assertRoute(t, allowlisted, "https://internal.corp/token", "")
	assertRoute(t, allowlisted, "https://auth.example.com/token", "93.184.216.34:443")
}

// TestNewOAuthDiscoveryTransportDialsConfiguredProxyOnPrivateAddress pins the
// proxy-only deployment shape: an operator-configured proxy on a private or
// loopback address must be dialable, while the destination is never dialed
// directly and a direct private destination stays blocked.
func TestNewOAuthDiscoveryTransportDialsConfiguredProxyOnPrivateAddress(t *testing.T) {
	var mu sync.Mutex
	var proxied []string
	fwd := proxytest.ForwardProxy(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		proxied = append(proxied, r.URL.Host)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	proxyURL, err := url.Parse(fwd.URL)
	require.NoError(t, err)
	var useProxy atomic.Bool
	useProxy.Store(true)
	selector := func(*http.Request) (*url.URL, error) {
		if useProxy.Load() {
			return proxyURL, nil
		}
		return nil, nil
	}
	client := newOAuthHTTPClient(newOAuthTransportWith(network.SSRFSafeDialContext(2*time.Second), network.ProxiedDestinationPolicy(true), selector, oauthTestResolver{}), 2*time.Second)

	// TEST-NET-3 is never routable; the only way this succeeds is through the proxy.
	resp, err := client.Get("http://203.0.113.5/.well-known/oauth-authorization-server")
	require.NoError(t, err, "a configured proxy on a private address must be dialable")
	resp.Body.Close()
	mu.Lock()
	require.Equal(t, []string{"203.0.113.5"}, proxied)
	mu.Unlock()

	// With no proxy chosen, a private destination is still refused at dial time.
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("a private destination must never be dialed directly")
	}))
	defer other.Close()
	useProxy.Store(false)
	_, err = client.Get(other.URL)
	require.Error(t, err)
	require.Contains(t, err.Error(), "blocked connection to non-public address")
}

// TestOAuthDirectRequestToTheProxyAddressKeepsThePolicy pins TXE-5377's second gap: the
// OAuth dialer used to recognise proxies by host:port and dial those addresses
// directly, so a direct request whose destination equalled the global proxy's address
// (a loopback proxy is always reached directly) skipped the destination policy. The
// direct transport now never treats any address as a proxy.
func TestOAuthDirectRequestToTheProxyAddressKeepsThePolicy(t *testing.T) {
	useProductionOAuthDialer(t)
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	network.SetDefaultHTTPClientFactory(network.NewHTTPClientFactory(&network.GlobalProxyConfig{
		Enabled: true, Type: network.GlobalProxyTypeHTTP, URL: server.URL, EnableForAPI: true,
	}, nil))
	t.Cleanup(func() { network.SetDefaultHTTPClientFactory(nil) })

	for _, ctx := range []context.Context{context.Background(), withRequirePublicTarget(context.Background(), true), withRequirePublicTarget(context.Background(), false)} {
		resp, err := newOAuthDiscoveryHTTPClient(ctx, 2*time.Second).Get(server.URL + "/token")
		if resp != nil {
			resp.Body.Close()
		}
		require.Error(t, err, "a direct request to the proxy's address must keep the destination policy")
		require.Contains(t, err.Error(), "blocked connection to non-public address")
	}
	require.Zero(t, hits.Load(), "the loopback destination must never be reached")
}

// TestOAuthProxiedRedirectIsCheckedPerHop pins that every redirect hop of a proxied
// discovery GET is checked and pinned on its own: a hop to another public name is
// tunnelled to that name's checked address, and a hop to a name resolving to a private
// address is refused before the proxy is asked for it.
func TestOAuthProxiedRedirectIsCheckedPerHop(t *testing.T) {
	var mu sync.Mutex
	var hops []string
	fwd := proxytest.ForwardProxy(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hops = append(hops, r.URL.Host)
		mu.Unlock()
		switch r.URL.Host {
		case "93.184.216.34":
			http.Redirect(w, r, "http://next.example/.well-known/oauth-authorization-server", http.StatusFound)
		case "93.184.216.35":
			http.Redirect(w, r, "http://internal.example/.well-known/oauth-authorization-server", http.StatusFound)
		default:
			w.WriteHeader(http.StatusOK)
		}
	})
	resolver := oauthTestResolver{
		"start.example":    {net.ParseIP("93.184.216.34")},
		"next.example":     {net.ParseIP("93.184.216.35")},
		"internal.example": {net.ParseIP("10.0.0.5")},
	}
	client := newOAuthHTTPClient(oauthTestTransport(true, nil, fixedOAuthProxy(t, fwd.URL), resolver), 3*time.Second)
	resp, err := client.Get("http://start.example/.well-known/oauth-protected-resource")
	if resp != nil {
		resp.Body.Close()
	}
	require.Error(t, err)
	require.Contains(t, err.Error(), "blocked proxied OAuth connection")
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"93.184.216.34", "93.184.216.35"}, hops, "each hop reaches the proxy as its checked address; the private hop never does")
}

// TestOAuthProxiedRequestHonoursCancellation pins that a proxied OAuth request ends at
// its context deadline while the proxy has accepted the connection but never answers
// the CONNECT.
func TestOAuthProxiedRequestHonoursCancellation(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	var mu sync.Mutex
	var held []net.Conn
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, conn)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range held {
			c.Close()
		}
	})
	client := newOAuthHTTPClient(oauthTestTransport(true, nil, fixedOAuthProxy(t, "http://"+ln.Addr().String()), oauthTestResolver{"auth.example.com": {net.ParseIP("93.184.216.34")}}), 30*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://auth.example.com/token", nil)
	require.NoError(t, err)
	start := time.Now()
	resp, err := client.Do(req)
	if resp != nil {
		resp.Body.Close()
	}
	elapsed := time.Since(start)
	require.Error(t, err)
	require.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)
	require.Less(t, elapsed, 5*time.Second, "a stalled proxy must not outlive the request's deadline: %v", err)
}

// TestSetPrivateTargetAllowlistAppliesToProxiedRequests pins the hot swap on the
// proxied path of the production allowlisted transport: an allowlisted private address
// is tunnelled to while listed and refused before the proxy once it is removed.
func TestSetPrivateTargetAllowlistAppliesToProxiedRequests(t *testing.T) {
	var mu sync.Mutex
	var proxied []string
	fwd := proxytest.ForwardProxy(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		proxied = append(proxied, r.URL.Host)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	network.SetDefaultHTTPClientFactory(network.NewHTTPClientFactory(&network.GlobalProxyConfig{
		Enabled: true, Type: network.GlobalProxyTypeHTTP, URL: fwd.URL, EnableForAPI: true,
	}, nil))
	t.Cleanup(func() { network.SetDefaultHTTPClientFactory(nil) })
	useProductionOAuthDialer(t, "10.0.0.7")
	ctx := withRequirePublicTarget(context.Background(), false)

	resp, err := newOAuthDiscoveryHTTPClient(ctx, 2*time.Second).Get("http://10.0.0.7/token")
	require.NoError(t, err, "an allowlisted address must be reachable through the proxy")
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	SetPrivateTargetAllowlist(nil)
	resp, err = newOAuthDiscoveryHTTPClient(ctx, 2*time.Second).Get("http://10.0.0.7/token")
	if resp != nil {
		resp.Body.Close()
	}
	require.Error(t, err, "a removed address must be refused on the proxied path")
	require.Contains(t, err.Error(), "blocked proxied OAuth connection")
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"10.0.0.7"}, proxied)
}

// TestDiscoverOAuthMetadataUsesGlobalProxy pins that MCP OAuth discovery honours the
// global proxy when it is enabled for API traffic, and opens its tunnel to the checked
// address. It used to follow only the environment proxy.
func TestDiscoverOAuthMetadataUsesGlobalProxy(t *testing.T) {
	set := proxytest.NewSet(t)
	network.SetDefaultHTTPClientFactory(network.NewHTTPClientFactory(&network.GlobalProxyConfig{
		Enabled: true, Type: network.GlobalProxyTypeHTTP, URL: "http://127.0.0.1:" + set.Config.Port(), EnableForAPI: true,
	}, nil))
	t.Cleanup(func() { network.SetDefaultHTTPClientFactory(nil) })

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _ = DiscoverOAuthMetadata(ctx, "https://93.184.216.34/mcp")
	seen := set.Config.Seen()
	if len(seen) == 0 {
		t.Fatal("OAuth discovery never reached the global proxy")
	}
	for _, hit := range seen {
		if hit.Target != "93.184.216.34:443" {
			t.Errorf("proxy saw %+v, want only CONNECTs to 93.184.216.34:443", hit)
		}
	}
}

// newOAuthBenchProxy is a loopback forward proxy answering "ok" to absolute-form
// requests and to every request inside a CONNECT tunnel, without dialing anything.
func newOAuthBenchProxy(b *testing.B) *httptest.Server {
	b.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			_, _ = w.Write([]byte("ok"))
			return
		}
		conn, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
		reader := bufio.NewReader(io.MultiReader(buffered.Reader, conn))
		for {
			req, err := http.ReadRequest(reader)
			if err != nil {
				return
			}
			_, _ = io.Copy(io.Discard, req.Body)
			req.Body.Close()
			if _, err := conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")); err != nil {
				return
			}
		}
	}))
	b.Cleanup(srv.Close)
	return srv
}

// BenchmarkOAuthDiscoveryClientRoundTrip measures one OAuth chain request on the
// shared production transports, per request building the client the way every OAuth
// call does: direct (the allowlisted policy reaching a loopback server, the default
// deployment's path) and proxied (the global proxy, public literal destination,
// pooled proxy connection or tunnel reused across iterations).
func BenchmarkOAuthDiscoveryClientRoundTrip(b *testing.B) {
	prevDial := testDialContextOverride
	testDialContextOverride = nil
	prevAllow := privateTargetAllowlist.Load()
	b.Cleanup(func() {
		testDialContextOverride = prevDial
		SetPrivateTargetAllowlist(prevAllow)
		network.SetDefaultHTTPClientFactory(nil)
	})
	al, err := network.ValidatePrivateTargetAllowlist([]string{"127.0.0.1"})
	if err != nil {
		b.Fatal(err)
	}
	SetPrivateTargetAllowlist(al)
	run := func(b *testing.B, ctx context.Context, target string) {
		b.ReportAllocs()
		for b.Loop() {
			resp, err := newOAuthDiscoveryHTTPClient(ctx, 5*time.Second).Get(target)
			if err != nil {
				b.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}

	b.Run("direct", func(b *testing.B) {
		network.SetDefaultHTTPClientFactory(nil)
		dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("ok"))
		}))
		b.Cleanup(dest.Close)
		run(b, withRequirePublicTarget(context.Background(), false), dest.URL+"/token")
	})
	b.Run("proxied", func(b *testing.B) {
		proxy := newOAuthBenchProxy(b)
		network.SetDefaultHTTPClientFactory(network.NewHTTPClientFactory(&network.GlobalProxyConfig{
			Enabled: true, Type: network.GlobalProxyTypeHTTP, URL: proxy.URL, EnableForAPI: true,
		}, nil))
		run(b, context.Background(), "http://93.184.216.34/token")
	})
}
