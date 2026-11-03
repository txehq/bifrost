package bifrost

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// recordedRequest is one request a provider sent while the capability table ran.
type recordedRequest struct {
	Method      string
	Host        string
	Path        string
	RawQuery    string
	ContentType string
	Body        []byte
}

// recordingProxy is the upstream every provider reaches while the capability table runs:
// an HTTP proxy that terminates TLS for any host with a certificate from its own CA,
// records each request and answers it without forwarding. A token request (Google's
// OAuth endpoint) receives an access token, a HuggingFace Hub model lookup the model's
// inference mapping, and every other request an empty JSON object.
type recordingProxy struct {
	listener net.Listener
	caPEM    string
	caCert   *x509.Certificate
	caKey    *ecdsa.PrivateKey

	mu       sync.Mutex
	leaves   map[string]*tls.Certificate
	requests []recordedRequest
	wg       sync.WaitGroup
}

func startRecordingProxy() (*recordingProxy, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "bifrost capability table CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	caCert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &recordingProxy{
		listener: l, caCert: caCert, caKey: caKey, leaves: map[string]*tls.Certificate{},
		caPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
	}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		_ = http.Serve(l, http.HandlerFunc(p.serve))
	}()
	return p, nil
}

// URL is the proxy's address for a provider's proxy_config.
func (p *recordingProxy) URL() string { return "http://" + p.listener.Addr().String() }

// Close stops the proxy and waits for it.
func (p *recordingProxy) Close() {
	_ = p.listener.Close()
	p.wg.Wait()
}

// take returns the requests recorded since the last call, and forgets them.
func (p *recordingProxy) take() []recordedRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.requests
	p.requests = nil
	return out
}

func (p *recordingProxy) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.tunnel(w, r)
		return
	}
	p.answer(w, r, r.URL.Host)
}

// tunnel accepts a CONNECT and serves the requests inside it over TLS for that host.
func (p *recordingProxy) tunnel(w http.ResponseWriter, r *http.Request) {
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = r.Host
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking unsupported", http.StatusInternalServerError)
		return
	}
	conn, buf, err := hj.Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}
	tlsConn := tls.Server(&bufferedConn{Conn: conn, r: buf.Reader}, &tls.Config{
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return p.leaf(host) },
		NextProtos:     []string{"http/1.1"},
	})
	if err := tlsConn.Handshake(); err != nil {
		return
	}
	reader := bufio.NewReader(tlsConn)
	for {
		req, err := http.ReadRequest(reader)
		if err != nil {
			return
		}
		rw := &connResponseWriter{header: http.Header{}, conn: tlsConn}
		p.answer(rw, req, host)
		rw.finish()
		req.Body.Close()
		if req.Close {
			return
		}
	}
}

// answer records a request and writes the stub response.
func (p *recordingProxy) answer(w http.ResponseWriter, r *http.Request, host string) {
	body, _ := io.ReadAll(r.Body)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	w.Header().Set("Content-Type", "application/json")
	if host == "oauth2.googleapis.com" || strings.HasSuffix(r.URL.Path, "/token") {
		_, _ = w.Write([]byte(`{"access_token":"capability-table","token_type":"Bearer","expires_in":3600}`))
		return
	}
	if host == "huggingface.co" && strings.HasPrefix(r.URL.Path, "/api/models/") {
		_, _ = w.Write(huggingFaceMapping(strings.TrimPrefix(r.URL.Path, "/api/models/")))
		return
	}
	p.mu.Lock()
	p.requests = append(p.requests, recordedRequest{Method: r.Method, Host: host, Path: r.URL.Path, RawQuery: r.URL.RawQuery,
		ContentType: r.Header.Get("Content-Type"), Body: body})
	p.mu.Unlock()
	_, _ = w.Write([]byte(`{}`))
}

// huggingFaceMapping answers the Hub's model lookup for a probe model "capability/<task>":
// the model is served for that task on every inference route the HuggingFace driver
// looks a model up for.
func huggingFaceMapping(model string) []byte {
	task := model[strings.LastIndexByte(model, '/')+1:]
	mapping := map[string]any{}
	for _, route := range []string{"hf-inference", "nebius", "sambanova", "scaleway", "together", "fal-ai"} {
		mapping[route] = map[string]any{"status": "live", "providerId": model, "task": task}
	}
	b, _ := json.Marshal(map[string]any{"_id": model, "id": model, "pipeline_tag": task, "inferenceProviderMapping": mapping})
	return b
}

// leaf is the certificate the proxy presents for host, signed by its CA.
func (p *recordingProxy) leaf(host string) (*tls.Certificate, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.leaves[host]; ok {
		return c, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.caCert, &key.PublicKey, p.caKey)
	if err != nil {
		return nil, fmt.Errorf("leaf certificate for %s: %w", host, err)
	}
	c := &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	p.leaves[host] = c
	return c, nil
}

// bufferedConn reads what the CONNECT reader already buffered before the connection.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(b []byte) (int, error) { return c.r.Read(b) }

// connResponseWriter writes one HTTP/1.1 response on a tunnelled connection.
type connResponseWriter struct {
	header http.Header
	conn   net.Conn
	status int
	body   []byte
}

func (w *connResponseWriter) Header() http.Header { return w.header }

func (w *connResponseWriter) WriteHeader(status int) { w.status = status }

func (w *connResponseWriter) Write(b []byte) (int, error) {
	w.body = append(w.body, b...)
	return len(b), nil
}

func (w *connResponseWriter) finish() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	resp := &http.Response{StatusCode: w.status, ProtoMajor: 1, ProtoMinor: 1, Header: w.header,
		ContentLength: int64(len(w.body)), Body: io.NopCloser(strings.NewReader(string(w.body)))}
	_ = resp.Write(w.conn)
}
