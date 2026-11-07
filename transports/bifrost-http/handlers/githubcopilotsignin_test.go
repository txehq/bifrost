package handlers

import (
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/capsohq/bifrost/core/network"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/capsohq/bifrost/framework/configstore"
	configstoreTables "github.com/capsohq/bifrost/framework/configstore/tables"
	"github.com/capsohq/bifrost/transports/v2/bifrost-http/lib"
	"github.com/fasthttp/router"
	"github.com/valyala/fasthttp"
)

// fakeGithubDeviceFlow stands in for github.com's device flow endpoints and api.github.com/user.
type fakeGithubDeviceFlow struct {
	server   *httptest.Server
	pollBody atomic.Value // string, the token endpoint's next reply
	clientID atomic.Value // string, client_id on the last form
}

func newFakeGithubDeviceFlow(t *testing.T) *fakeGithubDeviceFlow {
	t.Helper()
	f := &fakeGithubDeviceFlow{}
	f.pollBody.Store(`{"error":"authorization_pending"}`)
	mux := http.NewServeMux()
	mux.HandleFunc("/login/device/code", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.clientID.Store(r.PostForm.Get("client_id"))
		_, _ = fmt.Fprint(w, `{"device_code":"dc_1","user_code":"WDJB-MJHT","verification_uri":"https://github.com/login/device","expires_in":900,"interval":5}`)
	})
	mux.HandleFunc("/login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.clientID.Store(r.PostForm.Get("client_id"))
		_, _ = fmt.Fprint(w, f.pollBody.Load().(string))
	})
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"login":"octocat"}`)
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeGithubDeviceFlow) handler() *GithubCopilotSignInHandler {
	h := NewGithubCopilotSignInHandler(nil)
	h.endpoints = githubSignInEndpoints{
		deviceCodeURL: f.server.URL + "/login/device/code",
		tokenURL:      f.server.URL + "/login/oauth/access_token",
		userURL:       f.server.URL + "/user",
	}
	return h
}

func signInRequest(provider, body string) *fasthttp.RequestCtx {
	ctx := &fasthttp.RequestCtx{}
	ctx.SetUserValue("provider", provider)
	ctx.Request.SetBodyString(body)
	return ctx
}

// TestGithubCopilotSignInRoutesCoexistWithKeyRoutes pins that the sign-in routes register
// beside the provider routes; a conflicting pattern panics at startup.
func TestGithubCopilotSignInRoutesCoexistWithKeyRoutes(t *testing.T) {
	r := router.New()
	NewProviderHandler(nil, &lib.Config{}, nil).RegisterRoutes(r)
	NewGithubCopilotSignInHandler(nil).RegisterRoutes(r)
}

func TestGithubCopilotSignInRejectsOtherProviders(t *testing.T) {
	ctx := signInRequest("openai", "")
	NewGithubCopilotSignInHandler(nil).start(ctx)

	if ctx.Response.StatusCode() != fasthttp.StatusBadRequest {
		t.Fatalf("status %d, want 400 for a non-Copilot provider: %s", ctx.Response.StatusCode(), ctx.Response.Body())
	}
}

// forwardProxyTo answers as a proxy in front of fake and counts what it carries for
// github.invalid: absolute-URL requests, which it routes by path, and CONNECT tunnels (the
// provider proxy stack tunnels every target), which it splices onto fake's listener.
func forwardProxyTo(t *testing.T, fake *fakeGithubDeviceFlow) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var proxied atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			if r.Host != "github.invalid:80" {
				http.Error(w, "unexpected tunnel target", http.StatusForbidden)
				return
			}
			proxied.Add(1)
			upstream, err := net.Dial("tcp", fake.server.Listener.Addr().String())
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			client, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				_ = upstream.Close()
				return
			}
			_, _ = client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
			go func() { _, _ = io.Copy(upstream, client); _ = upstream.Close() }()
			_, _ = io.Copy(client, upstream)
			_ = client.Close()
			return
		}
		if r.Host == "github.invalid" {
			proxied.Add(1)
		}
		fake.server.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(proxy.Close)
	return proxy, &proxied
}

func startThroughGithubInvalid(t *testing.T, config *lib.Config) *fasthttp.RequestCtx {
	t.Helper()
	h := NewGithubCopilotSignInHandler(config)
	h.endpoints.deviceCodeURL = "http://github.invalid/login/device/code"
	start := signInRequest("github-copilot", "")
	h.start(start)
	return start
}

// TestGithubCopilotSignInUsesGlobalProxy pins that an operator's global proxy for API traffic
// applies to sign-in when the github-copilot provider names no proxy of its own.
func TestGithubCopilotSignInUsesGlobalProxy(t *testing.T) {
	fake := newFakeGithubDeviceFlow(t)
	proxy, proxied := forwardProxyTo(t, fake)
	config := &lib.Config{ProxyConfig: &configstoreTables.GlobalProxyConfig{
		Enabled: true, Type: network.GlobalProxyTypeHTTP, URL: proxy.URL, EnableForAPI: true,
	}}

	start := startThroughGithubInvalid(t, config)
	if start.Response.StatusCode() != fasthttp.StatusOK {
		t.Fatalf("start: status %d, want 200 through the proxy: %s", start.Response.StatusCode(), start.Response.Body())
	}
	if proxied.Load() != 1 {
		t.Fatalf("proxy saw %d GitHub requests, want 1", proxied.Load())
	}
}

// TestGithubCopilotSignInUsesProviderProxy pins that the github-copilot provider's own
// proxy_config wins over the global one, as it does for the provider's token exchange: a
// deployment whose Copilot egress needs a dedicated proxy signs in through it too.
func TestGithubCopilotSignInUsesProviderProxy(t *testing.T) {
	fake := newFakeGithubDeviceFlow(t)
	providerProxy, viaProvider := forwardProxyTo(t, fake)
	globalProxy, viaGlobal := forwardProxyTo(t, fake)
	config := &lib.Config{
		ProxyConfig: &configstoreTables.GlobalProxyConfig{
			Enabled: true, Type: network.GlobalProxyTypeHTTP, URL: globalProxy.URL, EnableForAPI: true, EnableForInference: true,
		},
		Providers: map[schemas.ModelProvider]configstore.ProviderConfig{
			schemas.GithubCopilot: {ProxyConfig: &schemas.ProxyConfig{Type: schemas.HTTPProxy, URL: schemas.NewSecretVar(providerProxy.URL)}},
		},
	}

	start := startThroughGithubInvalid(t, config)
	if start.Response.StatusCode() != fasthttp.StatusOK {
		t.Fatalf("start: status %d, want 200 through the provider proxy: %s", start.Response.StatusCode(), start.Response.Body())
	}
	if viaProvider.Load() != 1 || viaGlobal.Load() != 0 {
		t.Fatalf("provider proxy saw %d, global proxy saw %d; want 1 and 0", viaProvider.Load(), viaGlobal.Load())
	}
}

// TestGithubCopilotSignInTrustsProviderCA pins that the provider's network_config TLS settings
// apply: behind a TLS-inspecting proxy GitHub's certificate is signed by the operator's CA,
// which the provider's ca_cert_pem names. Without it the same endpoint is refused.
func TestGithubCopilotSignInTrustsProviderCA(t *testing.T) {
	fake := newFakeGithubDeviceFlow(t)
	tlsServer := httptest.NewTLSServer(fake.server.Config.Handler)
	t.Cleanup(tlsServer.Close)
	caPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: tlsServer.Certificate().Raw}))

	run := func(config *lib.Config) *fasthttp.RequestCtx {
		h := NewGithubCopilotSignInHandler(config)
		h.endpoints.deviceCodeURL = tlsServer.URL + "/login/device/code"
		start := signInRequest("github-copilot", "")
		h.start(start)
		return start
	}

	if refused := run(nil); refused.Response.StatusCode() != fasthttp.StatusBadGateway {
		t.Fatalf("without the CA: status %d, want 502 for an untrusted certificate: %s", refused.Response.StatusCode(), refused.Response.Body())
	}
	network := schemas.DefaultNetworkConfig
	network.CACertPEM = schemas.NewSecretVar(caPEM)
	trusted := run(&lib.Config{Providers: map[schemas.ModelProvider]configstore.ProviderConfig{
		schemas.GithubCopilot: {NetworkConfig: &network},
	}})
	if trusted.Response.StatusCode() != fasthttp.StatusOK {
		t.Fatalf("with the provider CA: status %d, want 200: %s", trusted.Response.StatusCode(), trusted.Response.Body())
	}
}

// TestGithubCopilotSignInRepliesAreNotCached pins Cache-Control: no-store on both replies: the
// device code redeems the sign-in and the poll reply carries the GitHub token itself.
func TestGithubCopilotSignInRepliesAreNotCached(t *testing.T) {
	fake := newFakeGithubDeviceFlow(t)
	h := fake.handler()
	start := signInRequest("github-copilot", "")
	h.start(start)
	fake.pollBody.Store(`{"access_token":"ghu_not_cached"}`)
	poll := signInRequest("github-copilot", `{"device_code":"dc_1"}`)
	h.poll(poll)
	for name, ctx := range map[string]*fasthttp.RequestCtx{"start": start, "poll": poll} {
		if ctx.Response.StatusCode() != fasthttp.StatusOK {
			t.Fatalf("%s: status %d: %s", name, ctx.Response.StatusCode(), ctx.Response.Body())
		}
		if got := string(ctx.Response.Header.Peek("Cache-Control")); got != "no-store" {
			t.Fatalf("%s: Cache-Control %q, want no-store", name, got)
		}
	}
}

// TestGithubCopilotSignIn pins the device flow: GitHub's Copilot App client ID on every call,
// the poll outcomes, and the token handed back on completion for the form to save.
func TestGithubCopilotSignIn(t *testing.T) {
	fake := newFakeGithubDeviceFlow(t)
	h := fake.handler()

	start := signInRequest("github-copilot", "")
	h.start(start)
	if start.Response.StatusCode() != fasthttp.StatusOK {
		t.Fatalf("start: status %d: %s", start.Response.StatusCode(), start.Response.Body())
	}
	var code githubDeviceCode
	if err := json.Unmarshal(start.Response.Body(), &code); err != nil || code.UserCode != "WDJB-MJHT" || code.DeviceCode != "dc_1" {
		t.Fatalf("start: got %+v, %v", code, err)
	}
	if got := fake.clientID.Load(); got != githubCopilotClientID {
		t.Fatalf("start sent client_id %v, want GitHub's Copilot App %s", got, githubCopilotClientID)
	}

	poll := func(reply string) githubSignInPollResult {
		t.Helper()
		fake.pollBody.Store(reply)
		ctx := signInRequest("github-copilot", `{"device_code":"dc_1"}`)
		h.poll(ctx)
		if ctx.Response.StatusCode() != fasthttp.StatusOK {
			t.Fatalf("poll: status %d: %s", ctx.Response.StatusCode(), ctx.Response.Body())
		}
		var result githubSignInPollResult
		if err := json.Unmarshal(ctx.Response.Body(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}

	for reply, want := range map[string]githubSignInPollResult{
		`{"error":"authorization_pending"}`:                                 {Status: "pending"},
		`{"error":"slow_down","interval":10}`:                               {Status: "pending", Interval: 10},
		`{"error":"expired_token"}`:                                         {Status: "expired"},
		`{"error":"access_denied"}`:                                         {Status: "denied"},
		`{"access_token":"ghu_signed_in","token_type":"bearer","scope":""}`: {Status: "complete", Token: "ghu_signed_in", Login: "octocat"},
	} {
		if got := poll(reply); got != want {
			t.Errorf("reply %s: got %+v, want %+v", reply, got, want)
		}
	}
	if got := fake.clientID.Load(); got != githubCopilotClientID {
		t.Fatalf("poll sent client_id %v, want %s", got, githubCopilotClientID)
	}
}

// TestGithubCopilotSignInPollUnknownCode pins that a code GitHub does not recognise is the
// caller's to restart (400), not a gateway failure (502).
func TestGithubCopilotSignInPollUnknownCode(t *testing.T) {
	fake := newFakeGithubDeviceFlow(t)
	h := fake.handler()
	for _, code := range []string{"incorrect_device_code", "bad_verification_code"} {
		fake.pollBody.Store(`{"error":"` + code + `"}`)
		ctx := signInRequest("github-copilot", `{"device_code":"dc_stale"}`)
		h.poll(ctx)

		if ctx.Response.StatusCode() != fasthttp.StatusBadRequest {
			t.Fatalf("%s: status %d, want 400: %s", code, ctx.Response.StatusCode(), ctx.Response.Body())
		}
		if !strings.Contains(string(ctx.Response.Body()), "start the sign-in again") {
			t.Fatalf("%s: body %s does not tell the operator to restart", code, ctx.Response.Body())
		}
	}
}

func TestGithubCopilotSignInPollRequiresDeviceCode(t *testing.T) {
	ctx := signInRequest("github-copilot", `{}`)
	NewGithubCopilotSignInHandler(nil).poll(ctx)

	if ctx.Response.StatusCode() != fasthttp.StatusBadRequest {
		t.Fatalf("status %d, want 400 without a device_code", ctx.Response.StatusCode())
	}
}

// outboundClientConstructors matches the ways a handler can build or borrow a client for an
// outbound HTTP call.
var outboundClientConstructors = regexp.MustCompile(`http\.Client\{|network\.DefaultTransport\(|http\.DefaultClient|http\.DefaultTransport|NewProviderHTTPClient\(|NewSSRFSafeHTTPClient\(|NewPrivateNetworkHTTPClient\(|fasthttp\.Client\{`)

// classifiedOutboundClients lists every handler file that builds an outbound client, how many
// it builds, and the egress rule it follows. A call made on a provider's behalf reaches that
// provider's hosts through lib.ManagementEgress and providerUtils.NewProviderHTTPClient, the
// provider's own proxy and TLS stack; a fetch of an operator- or caller-supplied URL goes
// through an SSRF-checked client. TXE-6085: the Copilot sign-in first shipped on the global API
// transport alone, so a provider proxy or custom CA that Copilot's token exchange honours did
// not apply to the sign-in that mints its token.
var classifiedOutboundClients = map[string]struct {
	count  int
	egress string
}{
	"config.go":              {1, "URL accessibility check: proxy-aware, public-only destination policy"},
	"githubcopilotsignin.go": {1, "GitHub device flow on behalf of github-copilot: lib.ManagementEgress + NewProviderHTTPClient"},
	"mcpopenapi.go":          {2, "caller-supplied OpenAPI spec URL: SSRF-safe when auth is bypassed, private-network client otherwise"},
	"skills_serving.go":      {1, "skill source_url fetch: SSRF-safe transport, global API proxy once installed"},
}

// TestHandlerOutboundClientsAreClassified fails when a handler file builds an outbound client
// that is not listed above, or builds a different number than listed, so each new one is
// assigned an egress rule on purpose instead of defaulting to whatever client was at hand.
func TestHandlerOutboundClientsAreClassified(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]int{}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			found[file] += len(outboundClientConstructors.FindAllString(line, -1))
		}
		if found[file] == 0 {
			delete(found, file)
		}
	}
	var problems []string
	for file, n := range found {
		want, ok := classifiedOutboundClients[file]
		switch {
		case !ok:
			problems = append(problems, file+": builds an outbound HTTP client; add it to classifiedOutboundClients with its egress rule (a call on a provider's behalf uses lib.ManagementEgress + NewProviderHTTPClient)")
		case want.count != n:
			problems = append(problems, file+": builds "+strconv.Itoa(n)+" outbound HTTP clients, classified "+strconv.Itoa(want.count)+"; classify the new one")
		}
	}
	for file := range classifiedOutboundClients {
		if _, ok := found[file]; !ok {
			problems = append(problems, file+": classified but builds no outbound client any more; remove the entry")
		}
	}
	sort.Strings(problems)
	for _, p := range problems {
		t.Error(p)
	}
}
