package bifrost

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	crand "crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"math/rand"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/capsohq/bifrost/core/schemas"
)

// The extra-params guard is one rule in one place (providerUtils.MergeExtraParamsIntoJSON):
// a passthrough extra param may add a field the converter left unset, and a request
// whose extra param names a field the converter set, or the routed model, is refused
// with a 400 before anything is sent. This file proves the rule holds on the wire for
// every provider Bifrost can construct, on every request type that takes extra params,
// buffered and streamed, so a new provider is covered the day it is added to
// schemas.StandardProviders.

const (
	guardAllowedModel = "allowed-model-7018"
	guardDeniedModel  = "DENIED-MODEL-7018"
	guardOriginalText = "ORIGINAL-INPUT-7018"
	guardReplacedText = "REPLACED-INPUT-7018"
	guardBenignField  = "guard_extra_7018"
	guardBenignValue  = "BENIGN-EXTRA-7018"
)

// guardPlant is what one pass of the test nests under extra_params.
type guardPlant int

const (
	// guardPlantBenign plants one field no converter sets. Where it reaches the wire,
	// the operation passes extra params through, and the other passes must be refused.
	guardPlantBenign guardPlant = iota
	// guardPlantModel plants the routed model's name under three spellings: what a
	// caller would send to reach a model its key does not allow.
	guardPlantModel
	// guardPlantInput plants every common name for a request's input: what a caller
	// would send to swap the content after the gateway has read it.
	guardPlantInput
)

func guardExtraParams(plant guardPlant) map[string]interface{} {
	switch plant {
	case guardPlantModel:
		return map[string]interface{}{"model": guardDeniedModel, "Model": guardDeniedModel, "MODEL": guardDeniedModel}
	case guardPlantInput:
		replacedMessages := []interface{}{map[string]interface{}{"role": "user", "content": guardReplacedText}}
		return map[string]interface{}{
			"input":     guardReplacedText,
			"Input":     guardReplacedText,
			"inputs":    guardReplacedText,
			"messages":  replacedMessages,
			"Messages":  replacedMessages,
			"prompt":    guardReplacedText,
			"text":      guardReplacedText,
			"texts":     []interface{}{guardReplacedText},
			"query":     guardReplacedText,
			"documents": []interface{}{guardReplacedText},
			"contents":  []interface{}{map[string]interface{}{"role": "user", "parts": []interface{}{map[string]interface{}{"text": guardReplacedText}}}},
			"system":    guardReplacedText,
		}
	}
	return map[string]interface{}{guardBenignField: guardBenignValue}
}

// guardUpstream is the only place a provider under test can reach: an HTTP proxy on
// this machine that answers every request itself. A CONNECT is not forwarded; the
// tunnel ends here, TLS included, under a certificate authority the provider is told
// to trust. So the bodies of HTTPS-only providers are read like any other, and no
// request leaves the machine whatever host a provider dials.
type guardUpstream struct {
	proxyURL string
	caPEM    string
	caCert   *x509.Certificate
	caKey    *ecdsa.PrivateKey

	mu     sync.Mutex
	seen   []guardRequest
	leaves map[string]*tls.Certificate
}

type guardRequest struct {
	target string // method, host, path and query
	body   []byte
}

// guardUpstreamHost is the base URL given to providers that take one. The .invalid
// top-level domain never resolves (RFC 2606), so a client that ignored the proxy
// would fail to dial instead of reaching anything.
const guardUpstreamHost = "guard-upstream.invalid"

func newGuardUpstream(t *testing.T) *guardUpstream {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), crand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "extra params guard test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(crand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	up := &guardUpstream{
		caPEM:  string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})),
		caCert: caCert, caKey: caKey, leaves: map[string]*tls.Certificate{},
	}

	// A port picked at random from 20000-60000, as every local vendor in this
	// program is bound; retry on a port that is taken.
	var listener net.Listener
	for attempt := 0; attempt < 50 && listener == nil; attempt++ {
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", 20000+rand.Intn(40000)))
		if err == nil {
			listener = l
		}
	}
	if listener == nil {
		t.Fatal("no free port in 20000-60000 for the local upstream")
	}
	up.proxyURL = "http://" + listener.Addr().String()
	answer := http.HandlerFunc(up.answer)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			up.answer(w, r)
			return
		}
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "cannot hijack", http.StatusInternalServerError)
			return
		}
		conn, _, err := hijacker.Hijack()
		if err != nil {
			return
		}
		if _, err := conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
			conn.Close()
			return
		}
		// A TLS client speaks first with a handshake record (0x16); anything else is
		// plain HTTP inside the tunnel.
		reader := bufio.NewReader(conn)
		var tunnelled net.Conn = &guardBufferedConn{Conn: conn, reader: reader}
		if first, err := reader.Peek(1); err == nil && first[0] == 0x16 {
			leaf, err := up.leafFor(r.URL.Hostname())
			if err != nil {
				conn.Close()
				return
			}
			tunnelled = tls.Server(tunnelled, &tls.Config{Certificates: []tls.Certificate{*leaf}})
		}
		go func() { _ = http.Serve(&guardOneConnListener{conn: tunnelled}, answer) }()
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return up
}

// answer records the request and replies with a JSON object that also satisfies an
// OAuth token exchange, which some providers make before the model request.
func (u *guardUpstream) answer(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	u.mu.Lock()
	u.seen = append(u.seen, guardRequest{target: r.Method + " " + r.Host + r.URL.RequestURI(), body: body})
	u.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"access_token":"guard-token","token_type":"Bearer","expires_in":3600}`))
}

func (u *guardUpstream) leafFor(host string) (*tls.Certificate, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if leaf, ok := u.leaves[host]; ok {
		return leaf, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), crand.Reader)
	if err != nil {
		return nil, err
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(int64(len(u.leaves) + 2)), Subject: pkix.Name{CommonName: host},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(host); ip != nil {
		template.IPAddresses = []net.IP{ip}
	} else {
		template.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(crand.Reader, template, u.caCert, &key.PublicKey, u.caKey)
	if err != nil {
		return nil, err
	}
	leaf := &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	u.leaves[host] = leaf
	return leaf, nil
}

func (u *guardUpstream) take() []guardRequest {
	u.mu.Lock()
	defer u.mu.Unlock()
	seen := u.seen
	u.seen = nil
	return seen
}

type guardBufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *guardBufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

// guardOneConnListener hands http.Serve one already-accepted connection.
type guardOneConnListener struct {
	mu   sync.Mutex
	conn net.Conn
}

func (l *guardOneConnListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.conn == nil {
		return nil, io.EOF
	}
	conn := l.conn
	l.conn = nil
	return conn, nil
}
func (l *guardOneConnListener) Close() error   { return nil }
func (l *guardOneConnListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }

// guardKey returns a key for the provider. Providers that take their endpoint or
// their credentials from the key instead of the network config get them here; every
// value is a placeholder, and the endpoint is the unresolvable guardUpstreamHost.
func guardKey(provider schemas.ModelProvider) schemas.Key {
	key := schemas.Key{ID: "guard-key", Value: *schemas.NewSecretVar("guard-key-value")}
	url := *schemas.NewSecretVar("http://" + guardUpstreamHost)
	secret := func(v string) *schemas.SecretVar { return schemas.NewSecretVar(v) }
	switch provider {
	case schemas.Azure:
		key.AzureKeyConfig = &schemas.AzureKeyConfig{Endpoint: url}
	case schemas.VLLM:
		key.VLLMKeyConfig = &schemas.VLLMKeyConfig{URL: url, ModelName: guardAllowedModel}
	case schemas.Ollama:
		key.OllamaKeyConfig = &schemas.OllamaKeyConfig{URL: url}
	case schemas.SGL:
		key.SGLKeyConfig = &schemas.SGLKeyConfig{URL: url}
	case schemas.Databricks:
		key.DatabricksKeyConfig = &schemas.DatabricksKeyConfig{WorkspaceURL: *secret(guardUpstreamHost)}
	case schemas.Cloudflare:
		key.CloudflareKeyConfig = &schemas.CloudflareKeyConfig{AccountID: *secret("guard-account")}
	case schemas.Bedrock:
		key.Value = *secret("")
		key.BedrockKeyConfig = &schemas.BedrockKeyConfig{AccessKey: *secret("AKIAGUARDGUARDGUARD00"), SecretKey: *secret("guard-secret"), Region: secret("us-east-1")}
	case schemas.BedrockMantle:
		key.Value = *secret("")
		key.BedrockMantleKeyConfig = &schemas.BedrockMantleKeyConfig{AccessKey: *secret("AKIAGUARDGUARDGUARD00"), SecretKey: *secret("guard-secret"), Region: secret("us-east-1")}
	case schemas.Vertex:
		key.VertexKeyConfig = &schemas.VertexKeyConfig{ProjectID: *secret("guard-project"), Region: *secret("us-central1")}
	}
	return key
}

// guardModel is the model a request names. Providers that pick a request shape or
// an inference route from the model's name get a name of the shape they expect.
func guardModel(provider schemas.ModelProvider) string {
	switch provider {
	case schemas.Vertex:
		return "gemini-" + guardAllowedModel
	case schemas.HuggingFace:
		return "hf-inference/" + guardAllowedModel
	case schemas.Tencent:
		return "hy-image-v3"
	}
	return guardAllowedModel
}

// guardOperation sends one request type, with the planted extra params, through a
// provider. It returns false when the provider does not serve the operation.
type guardOperation struct {
	name string
	run  func(ctx *schemas.BifrostContext, provider schemas.Provider, key schemas.Key, model string) *schemas.BifrostError
}

func guardDrain(stream chan *schemas.BifrostStreamChunk, err *schemas.BifrostError) *schemas.BifrostError {
	if err != nil || stream == nil {
		return err
	}
	timeout := time.After(10 * time.Second)
	for {
		select {
		case _, ok := <-stream:
			if !ok {
				return nil
			}
		case <-timeout:
			return nil
		}
	}
}

func guardPostHook(_ *schemas.BifrostContext, result *schemas.BifrostResponse, err *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError) {
	return result, err
}

func guardOperations(plant guardPlant) []guardOperation {
	return guardOperationsWith(func() map[string]interface{} { return guardExtraParams(plant) })
}

// guardOperationsWith is guardOperations for any extra params.
func guardOperationsWith(guardExtraParams func() map[string]interface{}) []guardOperation {
	text := guardOriginalText
	chat := func(p schemas.ModelProvider, model string) *schemas.BifrostChatRequest {
		return &schemas.BifrostChatRequest{
			Provider: p, Model: model,
			Input:  []schemas.ChatMessage{{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: &text}}},
			Params: &schemas.ChatParameters{ExtraParams: guardExtraParams()},
		}
	}
	responses := func(p schemas.ModelProvider, model string) *schemas.BifrostResponsesRequest {
		role, kind := schemas.ResponsesInputMessageRoleUser, schemas.ResponsesMessageTypeMessage
		return &schemas.BifrostResponsesRequest{
			Provider: p, Model: model,
			Input:  []schemas.ResponsesMessage{{Type: &kind, Role: &role, Content: &schemas.ResponsesMessageContent{ContentStr: &text}}},
			Params: &schemas.ResponsesParameters{ExtraParams: guardExtraParams()},
		}
	}
	completion := func(p schemas.ModelProvider, model string) *schemas.BifrostTextCompletionRequest {
		return &schemas.BifrostTextCompletionRequest{
			Provider: p, Model: model,
			Input:  &schemas.TextCompletionInput{PromptStr: &text},
			Params: &schemas.TextCompletionParameters{ExtraParams: guardExtraParams()},
		}
	}
	speech := func(p schemas.ModelProvider, model string) *schemas.BifrostSpeechRequest {
		return &schemas.BifrostSpeechRequest{
			Provider: p, Model: model,
			Input:  &schemas.SpeechInput{Input: text},
			Params: &schemas.SpeechParameters{ExtraParams: guardExtraParams()},
		}
	}
	transcription := func(p schemas.ModelProvider, model string) *schemas.BifrostTranscriptionRequest {
		return &schemas.BifrostTranscriptionRequest{
			Provider: p, Model: model,
			Input:  &schemas.TranscriptionInput{File: []byte(guardOriginalText), Filename: "audio.wav"},
			Params: &schemas.TranscriptionParameters{ExtraParams: guardExtraParams()},
		}
	}
	image := func(p schemas.ModelProvider, model string) *schemas.BifrostImageGenerationRequest {
		return &schemas.BifrostImageGenerationRequest{
			Provider: p, Model: model,
			Input:  &schemas.ImageGenerationInput{Prompt: text},
			Params: &schemas.ImageGenerationParameters{ExtraParams: guardExtraParams()},
		}
	}
	return []guardOperation{
		{"chat", func(ctx *schemas.BifrostContext, pr schemas.Provider, key schemas.Key, model string) *schemas.BifrostError {
			_, err := pr.ChatCompletion(ctx, key, chat(pr.GetProviderKey(), model))
			return err
		}},
		{"chat stream", func(ctx *schemas.BifrostContext, pr schemas.Provider, key schemas.Key, model string) *schemas.BifrostError {
			return guardDrain(pr.ChatCompletionStream(ctx, guardPostHook, nil, key, chat(pr.GetProviderKey(), model)))
		}},
		{"responses", func(ctx *schemas.BifrostContext, pr schemas.Provider, key schemas.Key, model string) *schemas.BifrostError {
			_, err := pr.Responses(ctx, key, responses(pr.GetProviderKey(), model))
			return err
		}},
		{"responses stream", func(ctx *schemas.BifrostContext, pr schemas.Provider, key schemas.Key, model string) *schemas.BifrostError {
			return guardDrain(pr.ResponsesStream(ctx, guardPostHook, nil, key, responses(pr.GetProviderKey(), model)))
		}},
		{"count tokens", func(ctx *schemas.BifrostContext, pr schemas.Provider, key schemas.Key, model string) *schemas.BifrostError {
			_, err := pr.CountTokens(ctx, key, responses(pr.GetProviderKey(), model))
			return err
		}},
		{"text completion", func(ctx *schemas.BifrostContext, pr schemas.Provider, key schemas.Key, model string) *schemas.BifrostError {
			_, err := pr.TextCompletion(ctx, key, completion(pr.GetProviderKey(), model))
			return err
		}},
		{"text completion stream", func(ctx *schemas.BifrostContext, pr schemas.Provider, key schemas.Key, model string) *schemas.BifrostError {
			return guardDrain(pr.TextCompletionStream(ctx, guardPostHook, nil, key, completion(pr.GetProviderKey(), model)))
		}},
		{"embedding", func(ctx *schemas.BifrostContext, pr schemas.Provider, key schemas.Key, model string) *schemas.BifrostError {
			_, err := pr.Embedding(ctx, key, &schemas.BifrostEmbeddingRequest{
				Provider: pr.GetProviderKey(), Model: model,
				Input:  []schemas.EmbeddingInputItem{{Content: schemas.EmbeddingContent{{Type: schemas.EmbeddingContentPartTypeText, Text: &text}}}},
				Params: &schemas.EmbeddingParameters{ExtraParams: guardExtraParams()},
			})
			return err
		}},
		{"rerank", func(ctx *schemas.BifrostContext, pr schemas.Provider, key schemas.Key, model string) *schemas.BifrostError {
			_, err := pr.Rerank(ctx, key, &schemas.BifrostRerankRequest{
				Provider: pr.GetProviderKey(), Model: model, Query: text,
				Documents: []schemas.RerankDocument{{Text: text}},
				Params:    &schemas.RerankParameters{ExtraParams: guardExtraParams()},
			})
			return err
		}},
		{"speech", func(ctx *schemas.BifrostContext, pr schemas.Provider, key schemas.Key, model string) *schemas.BifrostError {
			_, err := pr.Speech(ctx, key, speech(pr.GetProviderKey(), model))
			return err
		}},
		{"speech stream", func(ctx *schemas.BifrostContext, pr schemas.Provider, key schemas.Key, model string) *schemas.BifrostError {
			return guardDrain(pr.SpeechStream(ctx, guardPostHook, nil, key, speech(pr.GetProviderKey(), model)))
		}},
		{"transcription", func(ctx *schemas.BifrostContext, pr schemas.Provider, key schemas.Key, model string) *schemas.BifrostError {
			_, err := pr.Transcription(ctx, key, transcription(pr.GetProviderKey(), model))
			return err
		}},
		{"transcription stream", func(ctx *schemas.BifrostContext, pr schemas.Provider, key schemas.Key, model string) *schemas.BifrostError {
			return guardDrain(pr.TranscriptionStream(ctx, guardPostHook, nil, key, transcription(pr.GetProviderKey(), model)))
		}},
		{"image generation", func(ctx *schemas.BifrostContext, pr schemas.Provider, key schemas.Key, model string) *schemas.BifrostError {
			_, err := pr.ImageGeneration(ctx, key, image(pr.GetProviderKey(), model))
			return err
		}},
		{"image generation stream", func(ctx *schemas.BifrostContext, pr schemas.Provider, key schemas.Key, model string) *schemas.BifrostError {
			return guardDrain(pr.ImageGenerationStream(ctx, guardPostHook, nil, key, image(pr.GetProviderKey(), model)))
		}},
		{"image edit", func(ctx *schemas.BifrostContext, pr schemas.Provider, key schemas.Key, model string) *schemas.BifrostError {
			_, err := pr.ImageEdit(ctx, key, &schemas.BifrostImageEditRequest{
				Provider: pr.GetProviderKey(), Model: model,
				Input:  &schemas.ImageEditInput{Prompt: text, Images: []schemas.ImageInput{{Image: []byte("png")}}},
				Params: &schemas.ImageEditParameters{ExtraParams: guardExtraParams()},
			})
			return err
		}},
		{"video generation", func(ctx *schemas.BifrostContext, pr schemas.Provider, key schemas.Key, model string) *schemas.BifrostError {
			_, err := pr.VideoGeneration(ctx, key, &schemas.BifrostVideoGenerationRequest{
				Provider: pr.GetProviderKey(), Model: model,
				Input:  &schemas.VideoGenerationInput{Prompt: text},
				Params: &schemas.VideoGenerationParameters{ExtraParams: guardExtraParams()},
			})
			return err
		}},
		{"decision", func(ctx *schemas.BifrostContext, pr schemas.Provider, key schemas.Key, model string) *schemas.BifrostError {
			_, err := pr.Decision(ctx, key, &schemas.BifrostDecisionRequest{
				Provider: pr.GetProviderKey(), Model: model,
				Input:       schemas.DecisionInput{Text: &text},
				Questions:   []schemas.DecisionQuestion{{Type: "predicate", Name: schemas.Ptr("q")}},
				ExtraParams: guardExtraParams(),
			})
			return err
		}},
		{"ocr", func(ctx *schemas.BifrostContext, pr schemas.Provider, key schemas.Key, model string) *schemas.BifrostError {
			url := "https://example.invalid/" + guardOriginalText + ".pdf"
			_, err := pr.OCR(ctx, key, &schemas.BifrostOCRRequest{
				Provider: pr.GetProviderKey(), Model: model,
				Document: schemas.OCRDocument{Type: schemas.OCRDocumentTypeDocumentURL, DocumentURL: &url},
				Params:   &schemas.OCRParameters{ExtraParams: guardExtraParams()},
			})
			return err
		}},
	}
}

// guardProvidersWithoutLocalUpstream names the providers none of whose operations
// can be observed by this test, with the reason. A provider not listed here that
// serves any of the operations must reach the local upstream on at least one, or the
// test fails: a new provider is either covered or consciously exempted. It is empty
// today.
var guardProvidersWithoutLocalUpstream = map[schemas.ModelProvider]string{}

// guardVendorModelInputField names, per provider, the body field that holds the
// vendor model's own input object. Extra params are merged into that object, where a
// "model" is one of the vendor model's parameters (Replicate's routed model is in the
// URL or the top-level version). So the denied-model check reads the rest of the
// body, and a nested model is not required to be refused.
var guardVendorModelInputField = map[schemas.ModelProvider]string{
	schemas.Replicate: "input",
}

// guardBodyOutsideField returns body without one top-level field.
func guardBodyOutsideField(t *testing.T, body []byte, field string) []byte {
	t.Helper()
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(body, &decoded); err != nil {
		return body
	}
	delete(decoded, field)
	rest, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	return rest
}

func TestExtraParamsNeverReplaceTheModelOrInputOnAnyProvider(t *testing.T) {
	upstream := newGuardUpstream(t)
	client := &Bifrost{logger: NewNoOpLogger()}
	original := []byte(guardOriginalText)
	originalBase64 := []byte(base64.StdEncoding.EncodeToString(original))

	// Every standard provider, and a custom provider on each base type that allows
	// one: a custom provider runs its base provider's code under another name.
	type guardCase struct {
		name     string
		provider schemas.ModelProvider // the standard provider whose code runs
		custom   bool
	}
	cases := make([]guardCase, 0, 2*len(schemas.StandardProviders))
	for _, providerKey := range schemas.StandardProviders {
		cases = append(cases, guardCase{name: string(providerKey), provider: providerKey})
	}
	for _, providerKey := range schemas.StandardProviders {
		if IsSupportedBaseProvider(providerKey) {
			cases = append(cases, guardCase{name: "custom-" + string(providerKey), provider: providerKey, custom: true})
		}
	}

	for _, tc := range cases {
		providerKey := tc.provider
		t.Run(tc.name, func(t *testing.T) {
			config := &schemas.ProviderConfig{
				NetworkConfig: schemas.NetworkConfig{BaseURL: "http://" + guardUpstreamHost, DefaultRequestTimeoutInSeconds: 10},
				ProxyConfig: &schemas.ProxyConfig{
					Type: schemas.HTTPProxy, URL: schemas.NewSecretVar(upstream.proxyURL), CACertPEM: schemas.NewSecretVar(upstream.caPEM),
				},
			}
			if tc.custom {
				config.CustomProviderConfig = &schemas.CustomProviderConfig{CustomProviderKey: tc.name, BaseProviderType: providerKey}
			}
			provider, err := client.createBaseProvider(schemas.ModelProvider(tc.name), config)
			if err != nil {
				t.Fatalf("construct %s: %v", tc.name, err)
			}
			key := guardKey(providerKey)
			send := func(op guardOperation) (*schemas.BifrostError, []guardRequest) {
				upstream.take()
				ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
				ctx.SetValue(schemas.BifrostContextKeyPassthroughExtraParams, true)
				var bifrostErr *schemas.BifrostError
				func() {
					// The upstream's answer is no provider's response shape, and a few
					// providers do not survive that. What they sent is already recorded.
					defer func() { _ = recover() }()
					bifrostErr = op.run(ctx, provider, key, guardModel(providerKey))
				}()
				return bifrostErr, upstream.take()
			}
			carries := func(requests []guardRequest, marker []byte) bool {
				for _, request := range requests {
					if bytes.Contains(request.body, marker) {
						return true
					}
				}
				return false
			}
			vendorInputField, hasVendorInput := guardVendorModelInputField[providerKey]

			// Pass 1: a harmless extra param. It shows which operations reach the wire at
			// all, and which of them pass extra params through.
			reached := map[string]bool{}
			passesExtraParams := map[string]bool{}
			served := []string{} // operations the provider did not answer with unsupported_operation
			for _, op := range guardOperations(guardPlantBenign) {
				bifrostErr, requests := send(op)
				if bifrostErr == nil || bifrostErr.Error == nil || bifrostErr.Error.Code == nil || *bifrostErr.Error.Code != "unsupported_operation" {
					served = append(served, op.name)
				}
				if len(requests) > 0 {
					reached[op.name] = true
					passesExtraParams[op.name] = carries(requests, []byte(guardBenignValue))
				}
			}

			// Pass 2: the routed model. Pass 3: the input.
			refused := []string{}
			for _, plant := range []guardPlant{guardPlantModel, guardPlantInput} {
				for _, op := range guardOperations(plant) {
					bifrostErr, requests := send(op)
					for _, request := range requests {
						modelScope := request.body
						if hasVendorInput {
							modelScope = guardBodyOutsideField(t, request.body, vendorInputField)
						}
						if strings.Contains(request.target, guardDeniedModel) || bytes.Contains(modelScope, []byte(guardDeniedModel)) {
							t.Errorf("%s: the denied model reached the upstream\n%s\n%s", op.name, request.target, request.body)
						}
						if bytes.Contains(request.body, []byte(guardReplacedText)) &&
							!bytes.Contains(request.body, original) && !bytes.Contains(request.body, originalBase64) {
							t.Errorf("%s: an extra param replaced the request's input\n%s\n%s", op.name, request.target, request.body)
						}
					}
					if plant != guardPlantModel || !passesExtraParams[op.name] || hasVendorInput {
						continue
					}
					// Extra params reach the wire on this operation, so one naming the model
					// must be refused: a 400 naming it, and the request never sent.
					if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusBadRequest ||
						bifrostErr.ExtraFields.ErrorType != schemas.ErrorTypeCallerInvalidRequest ||
						!strings.EqualFold(fmt.Sprint(bifrostErr.Error.Param), "model") {
						t.Errorf("%s: want a 400 caller_invalid_request with error.param model, got %+v", op.name, bifrostErr)
					}
					if carries(requests, original) || carries(requests, originalBase64) {
						t.Errorf("%s: the request was sent although its extra params named the model", op.name)
					}
					refused = append(refused, op.name)
				}
			}
			t.Logf("refused a model among the extra params: %s", strings.Join(refused, ", "))
			names := make([]string, 0, len(reached))
			for name := range reached {
				names = append(names, name)
			}
			sort.Strings(names)
			t.Logf("checked on the wire: %s", strings.Join(names, ", "))
			// A provider that serves none of these operations (one that is registered
			// before its first operation lands) has nothing to check yet. The day it
			// serves one, it must be seen on the wire.
			if _, exempt := guardProvidersWithoutLocalUpstream[providerKey]; len(reached) == 0 && len(served) > 0 && !exempt {
				t.Errorf("%s serves %s but none reached the local upstream; give it what it needs in guardKey or guardModel, or list it in guardProvidersWithoutLocalUpstream with the reason", tc.name, strings.Join(served, ", "))
			}
		})
	}
}

// TXE-6995, folded into TXE-7018: on an openai-based custom provider, the kind Tokun
// configures for most vendors, a model among the extra params of an image generation,
// a video create or an embedding is a 400 naming it, and nothing is sent upstream.
func TestExtraParamsModelIsRefusedOnAnOpenAIBasedCustomProvider(t *testing.T) {
	upstream := newGuardUpstream(t)
	client := &Bifrost{logger: NewNoOpLogger()}
	config := &schemas.ProviderConfig{
		NetworkConfig: schemas.NetworkConfig{BaseURL: "http://" + guardUpstreamHost, DefaultRequestTimeoutInSeconds: 10},
		ProxyConfig: &schemas.ProxyConfig{
			Type: schemas.HTTPProxy, URL: schemas.NewSecretVar(upstream.proxyURL), CACertPEM: schemas.NewSecretVar(upstream.caPEM),
		},
		CustomProviderConfig: &schemas.CustomProviderConfig{CustomProviderKey: "custom-vendor", BaseProviderType: schemas.OpenAI},
	}
	provider, err := client.createBaseProvider("custom-vendor", config)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"image generation": true, "image generation stream": true, "video generation": true, "embedding": true}
	for _, spelling := range []string{"model", "Model", "MODEL"} {
		for _, op := range guardOperationsWith(func() map[string]interface{} { return map[string]interface{}{spelling: guardDeniedModel} }) {
			if !want[op.name] {
				continue
			}
			t.Run(op.name+"/"+spelling, func(t *testing.T) {
				upstream.take()
				ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
				ctx.SetValue(schemas.BifrostContextKeyPassthroughExtraParams, true)
				bifrostErr := op.run(ctx, provider, guardKey(schemas.OpenAI), guardAllowedModel)
				if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusBadRequest {
					t.Fatalf("want a 400, got %+v", bifrostErr)
				}
				if bifrostErr.Error.Param != spelling || bifrostErr.ExtraFields.ErrorType != schemas.ErrorTypeCallerInvalidRequest {
					t.Errorf("error.param / error_type = %v / %q, want %q / %q", bifrostErr.Error.Param, bifrostErr.ExtraFields.ErrorType, spelling, schemas.ErrorTypeCallerInvalidRequest)
				}
				if requests := upstream.take(); len(requests) != 0 {
					t.Errorf("%d request(s) reached the upstream, want none: %s", len(requests), requests[0].target)
				}
			})
		}
	}
}
