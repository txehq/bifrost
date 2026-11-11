package utils

import (
	"compress/gzip"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/capsohq/bifrost/core/network"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
	"golang.org/x/net/http/httpproxy"
)

// RedactURLForError reduces a resource URL to the part that is safe to echo back in an
// error: scheme, host, and path. Userinfo, query, and fragment are dropped.
//
// A pre-signed URL carries its credential in the query -- AWS SigV4 puts it in
// X-Amz-Signature, Azure in the SAS `sig` parameter -- and AWS documents pre-signed URLs
// as bearer tokens that "grant access to those who possess them", valid for up to 7 days
// (docs.aws.amazon.com/AmazonS3/latest/userguide/using-presigned-url.html). These errors
// reach the client as a BifrostError and land in logs, so echoing the query hands out
// working access to whoever reads either one.
//
// url.URL.Redacted() is deliberately not used: it masks the password and keeps the query,
// which is the half that actually carries the credential.
func RedactURLForError(resourceURL string) string {
	parsed, err := url.Parse(resourceURL)
	if err != nil || parsed.Host == "" {
		// Nothing safe is identifiable, so nothing is echoed. A useless-but-safe
		// placeholder beats guessing which half of an unparseable string was the secret.
		return "[redacted url]"
	}
	return (&url.URL{Scheme: parsed.Scheme, Host: parsed.Host, Path: parsed.Path}).String()
}

// sanitizeFetchError strips the URL out of the cause as well as the message. net/http
// wraps transport failures in *url.Error, whose Error() prints the request URL verbatim
// apart from the password (net/http's own stripPassword), so a pre-signed signature
// survives into the message even when the caller's format string is already clean.
//
// The Op and the underlying cause are kept: "dial tcp ...: i/o timeout" is where the
// diagnostic value lives, and it names a host at most.
func sanitizeFetchError(err error, redacted string) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return &url.Error{Op: urlErr.Op, URL: redacted, Err: urlErr.Err}
	}
	return err
}

// FetchAndEncodeURL downloads a remote resource (image, document, etc.) and
// returns its base64 encoding plus the response Content-Type. Used by providers
// (Bedrock Converse, Anthropic-on-Vertex) whose upstream surface only accepts
// inline bytes, not remote URLs. Bounded by a 20s timeout and a 25 MiB body cap;
// non-2xx responses error. The provided ctx is honored for cancellation and
// deadlines; pass context.Background() if no request context is available.
//
// SSRF-hardened: only http/https schemes are accepted, and the dialer rejects
// connections to loopback, private, CGNAT, link-local, unique-local, site-local,
// and unspecified addresses (including IPv4 targets smuggled inside IPv6
// transition addresses). The IP check runs at dial time (not just lookup time)
// so DNS rebinding does not bypass it. Redirect targets are subject to the same
// scheme + dial-time IP validation.
//
// Proxy: when ctx carries the serving provider's proxy config
// (schemas.BifrostContextKeyProviderProxyConfig, set by bifrost on every attempt),
// the fetch leaves through that proxy, the same egress as the provider's inference
// traffic. The target host is still refused if it resolves to a non-public address,
// and the tunnel is opened to the checked address rather than the hostname; see
// newFetchClient. With no proxy configured the fetch connects directly, as before.
func FetchAndEncodeURL(ctx context.Context, resourceURL string) (mediaType string, encoded string, err error) {
	mediaType, body, err := fetchURL(ctx, resourceURL, defaultFetchLimits, nil)
	if err != nil {
		return "", "", err
	}
	return mediaType, base64.StdEncoding.EncodeToString(body), nil
}

// FetchMediaURL downloads a provider-hosted media file, such as the signed URL of a finished
// video, and returns its raw bytes and Content-Type. A provider response names that URL, so
// it is not trusted: the fetch takes the same SSRF-hardened, proxy-aware path as
// FetchAndEncodeURL (http/https only, every resolved address and redirect hop checked at dial
// time). Media is larger than an inline document, so the body cap is 256 MiB and the whole
// fetch is bounded by 5 minutes; the clients for this limit are cached separately.
func FetchMediaURL(ctx context.Context, resourceURL string) (mediaType string, body []byte, err error) {
	return fetchURL(ctx, resourceURL, mediaFetchLimits, nil)
}

// FetchMediaURLWithHeader is FetchMediaURL for a file whose host wants a credential: header is
// sent to the host resourceURL names and to no other host a redirect leads to.
func FetchMediaURLWithHeader(ctx context.Context, resourceURL string, header http.Header) (mediaType string, body []byte, err error) {
	return fetchURL(ctx, resourceURL, mediaFetchLimits, header)
}

// fetchMediaURLResponse is FetchMediaURLWithHeader that also returns the host's response header.
func fetchMediaURLResponse(ctx context.Context, resourceURL string, header http.Header) (mediaType string, body []byte, responseHeader http.Header, err error) {
	return fetchURLResponse(ctx, resourceURL, mediaFetchLimits, header)
}

// ClientForVendorURL returns the client a provider sends a request with when the vendor's
// response, not the provider's base URL, named the request's address: a poll or stream URL of
// a task, a next-page link, a result file. Such an address is not trusted. Unless it is on
// the origin of baseURL, which the operator configured, the request goes out on the
// SSRF-hardened fetch client for the provider's proxy: http and https only, every resolved
// address checked at dial time, a proxied tunnel bound to the checked address. stream picks
// the client whose body is read as it arrives; the caller drives it as it drives its own
// (MakeRequestWithContext, DoStreamingRequest) and must not follow redirects on it, since a
// redirect hop is not this function's to check (fork, TXE-7090).
//
// An address that is not http or https is an error, on either client. providerClient is
// returned for an address on baseURL's origin; an empty baseURL never matches.
func ClientForVendorURL(ctx context.Context, vendorURL, baseURL string, providerClient *fasthttp.Client, stream bool) (*fasthttp.Client, error) {
	parsed, err := url.Parse(vendorURL)
	if err != nil || parsed.Host == "" {
		return nil, fmt.Errorf("invalid address %q in the provider's response", RedactURLForError(vendorURL))
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("unsupported URL scheme %q in the provider's response (only http/https allowed)", parsed.Scheme)
	}
	if baseURL != "" && providerClient != nil && sameOrigin(vendorURL, baseURL) {
		return providerClient, nil
	}
	limits := mediaFetchLimits
	if stream {
		limits = mediaStreamLimits
	}
	client, err := fetchClientForLimits(ctx, limits)
	if err != nil {
		return nil, err
	}
	return client.Transport.(*fasthttpRoundTripper).client, nil
}

// MediaStream is a media file being downloaded by FetchMediaStream.
type MediaStream struct {
	// ContentType is the vendor's Content-Type without its parameters, "" when it sent none.
	ContentType string
	// ContentLength is the number of bytes Body yields, or -1 when the vendor stated none or
	// encoded the body.
	ContentLength int64
	// Header is the vendor's response header.
	Header http.Header
	// Body yields the file as the vendor sends it. A read fails with ErrStreamIdleTimeout
	// when the vendor sends nothing for the idle timeout, and with the context's error once
	// the context ends. The caller must Close it, which releases the vendor connection.
	Body io.ReadCloser
}

// MediaStreamOptions are FetchMediaStream's optional settings.
type MediaStreamOptions struct {
	// Header is sent with the request, to the host the URL names and to no other host a
	// redirect leads to: it carries the credential of a vendor whose file needs one.
	Header http.Header
	// IdleTimeout is how long the vendor may send nothing before a read fails. Zero or
	// less means DefaultStreamIdleTimeout.
	IdleTimeout time.Duration
}

// FetchMediaStream is FetchMediaURL for a caller that forwards the file instead of holding it
// (fork, TXE-7036). It opens the download on the same SSRF-hardened, proxy-aware path (http
// and https only; every resolved address checked at dial time, on every redirect hop; a
// proxied tunnel bound to the checked address; errors that name the URL without its query)
// and returns once the vendor's headers are in. The body then arrives as the caller reads
// it: there is no size cap and no bound on the whole download, only the idle timeout and
// the context. Dial, request write and the wait for the headers are bounded per hop.
//
// A gzip body is decoded on the way through; any other Content-Encoding is refused, since
// the file would otherwise reach the caller encoded with nothing saying so.
func FetchMediaStream(ctx context.Context, resourceURL string, opts MediaStreamOptions) (*MediaStream, error) {
	resp, redacted, err := openFetch(ctx, resourceURL, mediaStreamLimits, opts.Header)
	if err != nil {
		return nil, err
	}
	stream := &MediaStream{
		ContentType:   mediaTypeOf(resp.Header.Get("Content-Type")),
		ContentLength: resp.ContentLength,
		Header:        resp.Header,
	}
	body := newMediaStreamBody(ctx, resp.Body, opts.IdleTimeout, redacted)
	switch strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding"))) {
	case "", "identity":
		stream.Body = body
	case "gzip", "x-gzip":
		// The length on the wire is not the file's. The gzip header is read from the socket
		// here, through the guarded body, so a vendor that stalls on it is still timed out.
		stream.ContentLength = -1
		decoded, err := gzip.NewReader(body)
		if err != nil {
			_ = body.Close()
			return nil, fmt.Errorf("failed to read body from %q: %w", redacted, err)
		}
		stream.Body = &gzipMediaStreamBody{Reader: decoded, body: body}
	default:
		_ = body.Close()
		return nil, fmt.Errorf("resource at %q has unsupported Content-Encoding %q", redacted, resp.Header.Get("Content-Encoding"))
	}
	return stream, nil
}

// gzipMediaStreamBody decodes a gzip media stream and closes the guarded body under it.
type gzipMediaStreamBody struct {
	*gzip.Reader
	body io.Closer
}

func (g *gzipMediaStreamBody) Close() error {
	_ = g.Reader.Close()
	return g.body.Close()
}

// mediaStreamBody guards a streamed media body: a read that waits longer than the idle
// timeout, or past the end of the context, closes the vendor connection and fails. The
// timer runs only while a read is waiting on the vendor, so a slow reader of the file is
// never mistaken for a stalled vendor.
type mediaStreamBody struct {
	ctx       context.Context
	body      io.ReadCloser
	idle      time.Duration
	timer     *time.Timer
	stopCtx   func() bool
	idleFired atomic.Bool
	closeOnce sync.Once
	redacted  string
}

func newMediaStreamBody(ctx context.Context, body io.ReadCloser, idle time.Duration, redacted string) *mediaStreamBody {
	if idle <= 0 {
		idle = DefaultStreamIdleTimeout
	}
	b := &mediaStreamBody{ctx: ctx, body: body, idle: idle, redacted: redacted}
	b.timer = time.AfterFunc(idle, func() {
		b.idleFired.Store(true)
		b.closeBody()
	})
	b.timer.Stop()
	b.stopCtx = context.AfterFunc(ctx, b.closeBody)
	return b
}

func (b *mediaStreamBody) closeBody() {
	b.closeOnce.Do(func() { _ = b.body.Close() })
}

func (b *mediaStreamBody) Read(p []byte) (int, error) {
	b.timer.Reset(b.idle)
	n, err := b.body.Read(p)
	b.timer.Stop()
	if err == nil || err == io.EOF {
		return n, err
	}
	if b.idleFired.Load() {
		return n, ErrStreamIdleTimeout
	}
	if ctxErr := b.ctx.Err(); ctxErr != nil {
		return n, ctxErr
	}
	return n, fmt.Errorf("failed to read body from %q: %w", b.redacted, sanitizeFetchError(err, b.redacted))
}

func (b *mediaStreamBody) Close() error {
	b.timer.Stop()
	b.stopCtx()
	b.closeBody()
	return nil
}

// MediaFetchStatusError is a media fetch the host answered with a status outside 2xx.
type MediaFetchStatusError struct {
	// URL is the redacted resource URL.
	URL        string
	StatusCode int
}

func (e *MediaFetchStatusError) Error() string {
	return fmt.Sprintf("fetch %q returned non-2xx status %d", e.URL, e.StatusCode)
}

// mediaTypeOf is a Content-Type without its parameters.
func mediaTypeOf(contentType string) string {
	if i := strings.Index(contentType, ";"); i != -1 {
		return strings.TrimSpace(contentType[:i])
	}
	return contentType
}

// fetchCredentialHeadersKey carries, on a fetch's request context, the names of the headers
// its caller supplied, for the redirect check to drop when a hop leaves the first host.
type fetchCredentialHeadersKey struct{}

// VideoContentTypeOr pins the Content-Type of a downloaded video to a video/* type. The bytes come
// from a URL the upstream names, and /v1/videos/{id}/content serves them with this type on the
// gateway's own origin, so a type such as text/html from a compromised upstream would make a
// browser render it there. A browser never renders an explicit video/* type as a document, so a
// well-formed video/<subtype> is kept and anything else (another type, none, a malformed one)
// becomes fallback, which the caller sets to the video type its provider produces.
func VideoContentTypeOr(contentType, fallback string) string {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	subtype, ok := strings.CutPrefix(ct, "video/")
	if !ok || subtype == "" {
		return fallback
	}
	for _, r := range subtype {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '.' && r != '+' && r != '-' {
			return fallback
		}
	}
	return ct
}

// openFetch is the shared opening of every fetch: it checks the URL, picks the client for the
// provider's proxy and the fetch class, sends the GET and returns the 2xx response with its
// body unread, plus the redacted URL for the caller's own errors. header, when set, is sent
// to the first host only (see fetchCheckRedirect).
func openFetch(ctx context.Context, resourceURL string, limits fetchLimits, header http.Header) (*http.Response, string, error) {
	// Every error below names the resource by its redacted form only; see
	// RedactURLForError for why the query and userinfo never make it into a message.
	redacted := RedactURLForError(resourceURL)

	parsed, err := url.Parse(resourceURL)
	if err != nil {
		return nil, redacted, fmt.Errorf("invalid resource URL %q: %w", redacted, sanitizeFetchError(err, redacted))
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, redacted, fmt.Errorf("unsupported URL scheme %q (only http/https allowed)", parsed.Scheme)
	}

	client, err := fetchClientForLimits(ctx, limits)
	if err != nil {
		return nil, redacted, fmt.Errorf("failed to fetch from %q: %w", redacted, err)
	}

	reqCtx := ctx
	if len(header) > 0 {
		names := make([]string, 0, len(header))
		for name := range header {
			names = append(names, name)
		}
		reqCtx = context.WithValue(ctx, fetchCredentialHeadersKey{}, names)
	}
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, resourceURL, nil)
	if err != nil {
		return nil, redacted, fmt.Errorf("invalid resource URL %q: %w", redacted, sanitizeFetchError(err, redacted))
	}
	for name, values := range header {
		req.Header[http.CanonicalHeaderKey(name)] = append([]string(nil), values...)
	}
	req.Header.Set("User-Agent", "bifrost-fetch/1")

	resp, err := DoHTTPRequest(client, req)
	if err != nil {
		if errors.Is(err, fasthttp.ErrBodyTooLarge) {
			return nil, redacted, fmt.Errorf("resource at %q exceeds %d-byte limit", redacted, limits.maxBytes)
		}
		return nil, redacted, fmt.Errorf("failed to fetch from %q: %w", redacted, sanitizeFetchError(err, redacted))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_ = resp.Body.Close()
		return nil, redacted, &MediaFetchStatusError{URL: redacted, StatusCode: resp.StatusCode}
	}
	return resp, redacted, nil
}

// fetchURL is the shared body of FetchAndEncodeURL and FetchMediaURL.
func fetchURL(ctx context.Context, resourceURL string, limits fetchLimits, header http.Header) (mediaType string, body []byte, err error) {
	mediaType, body, _, err = fetchURLResponse(ctx, resourceURL, limits, header)
	return mediaType, body, err
}

// fetchURLResponse is fetchURL that also returns the host's response header.
func fetchURLResponse(ctx context.Context, resourceURL string, limits fetchLimits, header http.Header) (mediaType string, body []byte, responseHeader http.Header, err error) {
	maxBytes := limits.maxBytes

	resp, redacted, err := openFetch(ctx, resourceURL, limits, header)
	if err != nil {
		return "", nil, nil, err
	}
	defer resp.Body.Close()

	if n := resp.ContentLength; n > maxBytes {
		return "", nil, nil, fmt.Errorf("resource at %q exceeds %d-byte limit", redacted, maxBytes)
	} else if n >= 0 {
		// Fork: the body length is known (the fasthttp round tripper always sets it), so read
		// into one exact-size buffer instead of letting io.ReadAll grow and copy it, which for
		// a 256 MiB media file would hold several transient copies.
		body = make([]byte, n)
		if _, err = io.ReadFull(resp.Body, body); err != nil {
			return "", nil, nil, fmt.Errorf("failed to read body from %q: %w", redacted, sanitizeFetchError(err, redacted))
		}
	} else {
		body, err = io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
		if err != nil {
			return "", nil, nil, fmt.Errorf("failed to read body from %q: %w", redacted, sanitizeFetchError(err, redacted))
		}
		if int64(len(body)) > maxBytes {
			return "", nil, nil, fmt.Errorf("resource at %q exceeds %d-byte limit", redacted, maxBytes)
		}
	}

	return mediaTypeOf(resp.Header.Get("Content-Type")), body, resp.Header, nil
}

// maxFetchBytes caps a fetched resource at 25 MiB.
const maxFetchBytes int64 = 25 * 1024 * 1024

// fetchLimits bounds one class of fetch: the body cap and the timeout of the whole request.
// A streamed class has no body cap, and its timeout bounds each hop's dial, request write
// and wait for the headers, never the body.
type fetchLimits struct {
	maxBytes int64
	timeout  time.Duration
	stream   bool
}

var (
	// defaultFetchLimits is FetchAndEncodeURL's inline image and document fetch.
	defaultFetchLimits = fetchLimits{maxBytes: maxFetchBytes, timeout: 20 * time.Second}
	// mediaFetchLimits is FetchMediaURL's provider-hosted media download (fork).
	mediaFetchLimits = fetchLimits{maxBytes: 256 * 1024 * 1024, timeout: 5 * time.Minute}
	// mediaStreamLimits is FetchMediaStream's forwarded media download (fork, TXE-7036).
	mediaStreamLimits = fetchLimits{timeout: time.Minute, stream: true}
)

// proxyEnvVars are the variables golang.org/x/net/http/httpproxy reads.
var proxyEnvVars = []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "NO_PROXY", "no_proxy", "REQUEST_METHOD"}

// fetchClients holds one http.Client per distinct proxy, so connections are pooled
// across fetches instead of a fresh transport being built per call. Keyed by
// fetchClientKeyFor; the set is bounded by the number of distinct proxy configs.
var fetchClients sync.Map // fetchClientKey -> *http.Client

// fetchClientFor returns the client for the proxy config carried on ctx, or the
// direct client when there is none.
func fetchClientFor(ctx context.Context) (*http.Client, error) {
	return fetchClientForLimits(ctx, defaultFetchLimits)
}

// fetchClientForLimits is fetchClientFor for a given fetch class; the limits are part of the
// cache key, so the set stays bounded by distinct proxy configs times the two fetch classes.
func fetchClientForLimits(ctx context.Context, limits fetchLimits) (*http.Client, error) {
	proxyConfig, _ := ctx.Value(schemas.BifrostContextKeyProviderProxyConfig).(*schemas.ProxyConfig)
	key := fetchClientKeyFor(proxyConfig)
	key.limits = limits
	if cached, ok := fetchClients.Load(key); ok {
		return cached.(*http.Client), nil
	}
	actual, _ := fetchClients.LoadOrStore(key, newFetchClientWithLimits(proxyConfig, limits))
	return actual.(*http.Client), nil
}

// newFetchClient builds the SSRF-hardened fetch client on fasthttp, with the proxy
// from the same ConfigureProxy the provider's inference clients use.
//
// Direct connections, including targets the proxy is told to skip (errBypassProxy),
// go through network.SSRFSafeDialContext, which checks the resolved target on every
// dial. Once a request is proxied the dial goes to the proxy, which is operator
// configuration and may well be on a private address, so it is not checked. The
// target is resolved and checked first (fetchResolveTarget), and the proxy is asked
// for a tunnel to one of the checked addresses, never the hostname: the proxy cannot
// resolve the name to somewhere else after the check. Redirect hops dial again and
// are checked the same way. The bypass decision (no_proxy, NO_PROXY) is still made on
// the hostname, before the address is pinned.
// proxyBypassFunc reports whether a fetch to host:port skips proxyConfig's proxy, the
// way ConfigureProxy's dialer decides it: a no_proxy match, or for the environment
// proxy, NO_PROXY or no variable set for the request's scheme. An empty scheme is
// chosen by port (443 is https, anything else http), as envProxyDialFunc does.
func proxyBypassFunc(proxyConfig *schemas.ProxyConfig) func(scheme, host, port string) bool {
	noProxy := proxyConfig.NoProxy
	var envProxy func(*url.URL) (*url.URL, error)
	if proxyConfig.Type == schemas.EnvProxy {
		envProxy = httpproxy.FromEnvironment().ProxyFunc()
	}
	return func(scheme, host, port string) bool {
		if noProxy != "" && network.MatchesNoProxy(host, noProxy) {
			return true
		}
		if envProxy == nil {
			return false
		}
		if scheme == "" {
			scheme = "http"
			if port == "443" {
				scheme = "https"
			}
		}
		proxyURL, err := envProxy(&url.URL{Scheme: scheme, Host: net.JoinHostPort(host, port)})
		return err == nil && proxyURL == nil
	}
}

// fetchResolveTarget resolves and checks a proxied fetch target. A variable so tests
// can stand in for DNS.
var fetchResolveTarget = network.ResolvePublicTarget

func newFetchClient(proxyConfig *schemas.ProxyConfig) *http.Client {
	return newFetchClientWithLimits(proxyConfig, defaultFetchLimits)
}

// newFetchClientWithLimits is newFetchClient with the body cap and timeouts of limits.
func newFetchClientWithLimits(proxyConfig *schemas.ProxyConfig, limits fetchLimits) *http.Client {
	ssrfDial := network.SSRFSafeDialContext(10 * time.Second)
	direct := func(addr string) (net.Conn, error) {
		return ssrfDial(context.Background(), "tcp", addr)
	}
	client := &fasthttp.Client{
		ReadTimeout:              limits.timeout,
		WriteTimeout:             limits.timeout,
		MaxIdleConnDuration:      30 * time.Second,
		MaxResponseBodySize:      int(limits.maxBytes) + 1,
		StreamResponseBody:       limits.stream,
		NoDefaultUserAgentHeader: true,
		// The path is sent byte for byte: repeated or escaped slashes in an object key
		// are covered by a pre-signed URL's signature and select the object.
		DisablePathNormalizing: true,
	}
	proxied := false
	// Dial address -> request scheme, so the environment proxy is chosen by the
	// request's scheme rather than guessed from its port (an https URL on 8443 uses
	// HTTPS_PROXY). Pinned dials hold it under the address they dial.
	schemes := newDialSchemes()
	if namesProxy(proxyConfig) {
		client = configureProxy(client, proxyConfig, getLogger(), schemes.lookup)
		if proxyDial := client.Dial; proxyDial != nil {
			proxied = true
			bypass := proxyBypassFunc(proxyConfig)
			client.Dial = func(addr string) (net.Conn, error) {
				host, port, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				scheme := schemes.lookup(addr)
				if bypass(scheme, host, port) {
					return direct(addr)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				ips, err := fetchResolveTarget(ctx, host)
				if err != nil {
					return nil, err
				}
				var lastErr error
				for _, ip := range ips {
					pinned := net.JoinHostPort(ip.String(), port)
					release := schemes.hold(pinned, scheme)
					conn, err := proxyDial(pinned)
					release()
					if errors.Is(err, errBypassProxy) {
						// no_proxy names the address (a CIDR entry): connect directly,
						// still through the SSRF dialer.
						conn, err = direct(pinned)
					}
					if err == nil {
						return conn, nil
					}
					lastErr = err
				}
				return nil, lastErr
			}
		}
	}
	if !proxied {
		client.Dial = direct
	}
	client.Transport = NewContextTransport()

	// http.Client.Timeout covers the body too, so a streamed class leaves it unset: its
	// hops are bounded by the fasthttp client's timeouts above, its body by the caller.
	clientTimeout := limits.timeout
	if limits.stream {
		clientTimeout = 0
	}
	return &http.Client{
		Timeout:       clientTimeout,
		Transport:     &fasthttpRoundTripper{client: client, schemes: schemes, stream: limits.stream},
		CheckRedirect: fetchCheckRedirect,
	}
}

// fetchCheckRedirect is the redirect policy of every fetch client: http and https only, ten
// hops at most, no Referer, and the headers the caller supplied (a vendor credential) dropped from a hop
// that leaves the first host. net/http drops Authorization and Cookie itself on such a hop;
// a credential under another name, such as x-goog-api-key, is only known to the caller.
func fetchCheckRedirect(req *http.Request, via []*http.Request) error {
	if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
		return fmt.Errorf("blocked redirect to unsupported scheme %q", req.URL.Scheme)
	}
	if len(via) >= 10 {
		return fmt.Errorf("stopped after 10 redirects")
	}
	// net/http sets Referer to the previous hop's URL, query included, and the query of a
	// fetched URL is its credential: a pre-signed signature, or a vendor key such as
	// Vertex's ?key=. No hop is told where the fetch came from.
	req.Header.Del("Referer")
	if names, _ := req.Context().Value(fetchCredentialHeadersKey{}).([]string); len(names) > 0 && len(via) > 0 {
		if first := via[0].URL; req.URL.Scheme != first.Scheme || req.URL.Host != first.Host {
			for _, name := range names {
				req.Header.Del(name)
			}
		}
	}
	return nil
}

// fetchClientKey is the set of resolved values that change which client a fetch needs.
// It is a comparable struct used directly as the cache key: the values already live in
// memory on the provider's ProxyConfig, so keying by them exposes nothing new, and no
// hashing is needed. Everything that NetHTTPProxy treats as "no proxy" maps to the
// zero key.
type fetchClientKey struct {
	proxyType schemas.ProxyType
	url       string
	username  string
	password  string
	caCertPEM string
	noProxy   string
	// skipTLSVerify is an inherited global skip_tls_verify; it changes the client's TLS.
	skipTLSVerify bool
	// env holds the proxy variables for type "environment": EnvProxyFunc reads them
	// when the client is built, so the values it read are part of which client this is.
	env string
	// limits is the fetch class (fork: FetchMediaURL has its own body cap and timeout).
	limits fetchLimits
}

func fetchClientKeyFor(proxyConfig *schemas.ProxyConfig) fetchClientKey {
	if proxyConfig == nil || proxyConfig.Type == "" || proxyConfig.Type == schemas.NoProxy {
		return fetchClientKey{}
	}
	key := fetchClientKey{
		proxyType:     proxyConfig.Type,
		url:           proxyConfig.URL.GetValue(),
		username:      proxyConfig.Username.GetValue(),
		password:      proxyConfig.Password.GetValue(),
		caCertPEM:     proxyConfig.CACertPEM.GetValue(),
		noProxy:       proxyConfig.NoProxy,
		skipTLSVerify: proxyConfig.SkipTLSVerify,
	}
	if proxyConfig.Type == schemas.EnvProxy {
		var env strings.Builder
		for _, name := range proxyEnvVars {
			env.WriteString(os.Getenv(name))
			env.WriteByte(0)
		}
		key.env = env.String()
	}
	return key
}
