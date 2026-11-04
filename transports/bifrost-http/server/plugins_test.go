package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fasthttp/router"
	"github.com/tidwall/gjson"
	"github.com/valyala/fasthttp"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/capsohq/bifrost/framework/configstore"
	configstoreTables "github.com/capsohq/bifrost/framework/configstore/tables"
	"github.com/capsohq/bifrost/framework/logstore"
	"github.com/capsohq/bifrost/plugins/compat"
	"github.com/capsohq/bifrost/plugins/tokun"
	"github.com/capsohq/bifrost/transports/v2/bifrost-http/handlers"
	"github.com/capsohq/bifrost/transports/v2/bifrost-http/lib"

	plannerv1 "github.com/capsohq/txe/tokun/proto/gen/go/tokun/planner/v1"
	routingv1 "github.com/capsohq/txe/tokun/proto/gen/go/tokun/routing/v1"
)

// upstreamCall is one request an upstream received.
type upstreamCall struct {
	provider string
	key      string
	model    string
	header   http.Header
}

// tokunUpstreams stands in for the upstream APIs of the Bifrost provider entries, each under
// its own path. A key listed in failing is answered 500, and a key given an answer is answered
// with it. Any other key is answered "ok"; a streamed request gets its first chunk, and then,
// once breakStream is closed, a dropped connection.
type tokunUpstreams struct {
	mu          sync.Mutex
	calls       []upstreamCall
	failing     map[string]bool
	answers     map[string]upstreamAnswer
	breakStream chan struct{}
}

// upstreamAnswer is an error answer: the HTTP status and the body, or, with status 0, a
// connection dropped before any response.
type upstreamAnswer struct {
	status int
	body   string
}

func (u *tokunUpstreams) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	provider, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	body, _ := io.ReadAll(r.Body)
	var request struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	_ = json.Unmarshal(body, &request)
	key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	u.mu.Lock()
	u.calls = append(u.calls, upstreamCall{provider: provider, key: key, model: request.Model, header: r.Header.Clone()})
	failing := u.failing[key]
	answer, answered := u.answers[key]
	breakStream := u.breakStream
	u.mu.Unlock()
	switch {
	case answered && answer.status == 0:
		// The connection closes before any byte of a response.
		panic(http.ErrAbortHandler)
	case answered:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(answer.status)
		_, _ = io.WriteString(w, answer.body)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if failing {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"upstream failed","type":"server_error"}}`)
		return
	}
	if request.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"`+request.Model+`","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":null}]}`+"\n\n")
		w.(http.Flusher).Flush()
		<-breakStream
		// The connection closes in the middle of the stream.
		panic(http.ErrAbortHandler)
	}
	_, _ = io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"`+request.Model+`","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
}

func (u *tokunUpstreams) reset(failing ...string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.calls = nil
	u.failing = map[string]bool{}
	u.answers = map[string]upstreamAnswer{}
	u.breakStream = make(chan struct{})
	for _, key := range failing {
		u.failing[key] = true
	}
}

// answer makes the upstream answer key with a.
func (u *tokunUpstreams) answer(key string, a upstreamAnswer) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.answers[key] = a
}

// breakStreams drops the connection of every stream the upstream holds open.
func (u *tokunUpstreams) breakStreams() {
	u.mu.Lock()
	defer u.mu.Unlock()
	select {
	case <-u.breakStream:
	default:
		close(u.breakStream)
	}
}

func (u *tokunUpstreams) call(i int) upstreamCall {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.calls[i]
}

// served lists provider/key of every call, in order.
func (u *tokunUpstreams) served() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	served := make([]string, 0, len(u.calls))
	for _, call := range u.calls {
		served = append(served, call.provider+"/"+call.key)
	}
	return served
}

// requestObserver is an LLM plugin registered after the tokun plugin: it records the request
// id each request reaches core with.
type requestObserver struct {
	mu         sync.Mutex
	requestIDs []string
}

func (o *requestObserver) GetName() string { return "tokun-test-observer" }
func (o *requestObserver) Cleanup() error  { return nil }
func (o *requestObserver) PreRequestHook(ctx *schemas.BifrostContext, _ *schemas.BifrostRequest) error {
	requestID, _ := ctx.Value(schemas.BifrostContextKeyRequestID).(string)
	o.mu.Lock()
	o.requestIDs = append(o.requestIDs, requestID)
	o.mu.Unlock()
	return nil
}
func (o *requestObserver) PreLLMHook(_ *schemas.BifrostContext, req *schemas.BifrostRequest) (*schemas.BifrostRequest, *schemas.LLMPluginShortCircuit, error) {
	return req, nil, nil
}
func (o *requestObserver) PostLLMHook(_ *schemas.BifrostContext, resp *schemas.BifrostResponse, err *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError, error) {
	return resp, err, nil
}
func (o *requestObserver) take() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	ids := o.requestIDs
	o.requestIDs = nil
	return ids
}

// planSigner signs route plans the way the tokun gateway does.
type planSigner struct {
	keyID  string
	secret string
}

// headers returns tk-route-plan and tk-route-plan-sig for plan, sent as sendID with a body of
// bodyBytes bytes that expires at expiresAt.
func (s planSigner) headers(t *testing.T, plan *routingv1.RoutePlan, sendID string, bodyBytes int, expiresAt time.Time) (string, string) {
	t.Helper()
	planBytes, err := proto.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	planSHA256 := sha256.Sum256(planBytes)
	claims, err := proto.Marshal(&routingv1.RoutePlanClaims{
		KeyId:      s.keyID,
		RequestId:  plan.GetRequestId(),
		SendId:     sendID,
		ExpiresAt:  timestamppb.New(expiresAt),
		BodyBytes:  int64(bodyBytes),
		PlanSha256: planSHA256[:],
	})
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, []byte(s.secret))
	mac.Write(claims)
	signature, err := proto.Marshal(&routingv1.RoutePlanSignature{Claims: claims, Mac: mac.Sum(nil)})
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(planBytes), base64.StdEncoding.EncodeToString(signature)
}

// tokunTestPlan lists up-a (keys key-a1, key-a2) and then up-b (key-b1), and draws them in that
// order every time: provider-b scores below the threshold and key-a2 has share 0, so the
// selection curve ranks both last in their layers. The Bifrost entries also hold key-a3 on up-a
// and the provider up-x, which the plan does not list. Both are chat completions interfaces, for
// a chat completion that is not streamed.
func tokunTestPlan(requestID string) *routingv1.RoutePlan {
	return &routingv1.RoutePlan{
		RequestId: requestID,
		Threshold: 0.75,
		Providers: []*routingv1.RouteProvider{
			{ProviderId: "provider-a", Score: 0.8, Upstreams: []*routingv1.RouteUpstream{{
				UpstreamId: 1, Interface: "1:text.chat.openai-chat", BifrostProvider: "up-a", ForwardModel: "model-a",
				RequestType: string(schemas.ChatCompletionRequest),
				Keys:        []*plannerv1.KeyShare{{KeyId: "key-a1", Share: 1}, {KeyId: "key-a2", Share: 0}},
			}}},
			{ProviderId: "provider-b", Score: 0.7, Upstreams: []*routingv1.RouteUpstream{{
				UpstreamId: 2, Interface: "2:text.chat.openai-chat", BifrostProvider: "up-b", ForwardModel: "model-b",
				RequestType: string(schemas.ChatCompletionRequest),
				Keys:        []*plannerv1.KeyShare{{KeyId: "key-b1", Share: 1}},
			}}},
		},
	}
}

// withRequestType sets the request_type of every upstream of plan, as the gateway writes it for
// the request the plan is sent with, and returns plan.
func withRequestType(plan *routingv1.RoutePlan, requestType schemas.RequestType) *routingv1.RoutePlan {
	for _, provider := range plan.GetProviders() {
		for _, upstream := range provider.GetUpstreams() {
			upstream.RequestType = string(requestType)
		}
	}
	return plan
}

// tokunChainPlan is drawn in one order every time, which steps from a key to the next key of the
// same upstream, then to the next upstream, then to the next provider: up-a's key-a1, then key-a2
// (share 0), then up-c's key-c1 (up-c scores below provider-a's upstream threshold), then
// provider-b's up-b with key-b1 (provider-b scores below the threshold). Its upstreams are chat
// completions interfaces, for a chat completion that is not streamed.
func tokunChainPlan(requestID string) *routingv1.RoutePlan {
	return withRequestType(&routingv1.RoutePlan{
		RequestId: requestID,
		Threshold: 0.75,
		Providers: []*routingv1.RouteProvider{
			{ProviderId: "provider-a", Score: 0.8, UpstreamThreshold: 0.5, Upstreams: []*routingv1.RouteUpstream{
				{UpstreamId: 1, Interface: "1:text.chat.openai-chat", BifrostProvider: "up-a", ForwardModel: "model-a", Score: 0.8,
					Keys: []*plannerv1.KeyShare{{KeyId: "key-a1", Share: 1}, {KeyId: "key-a2", Share: 0}}},
				{UpstreamId: 3, Interface: "3:text.chat.openai-chat", BifrostProvider: "up-c", ForwardModel: "model-c", Score: 0.3,
					Keys: []*plannerv1.KeyShare{{KeyId: "key-c1", Share: 1}}},
			}},
			{ProviderId: "provider-b", Score: 0.7, UpstreamThreshold: 0.5, Upstreams: []*routingv1.RouteUpstream{
				{UpstreamId: 2, Interface: "2:text.chat.openai-chat", BifrostProvider: "up-b", ForwardModel: "model-b", Score: 0.8,
					Keys: []*plannerv1.KeyShare{{KeyId: "key-b1", Share: 1}}},
			}},
		},
	}, schemas.ChatCompletionRequest)
}

// routeAttempts decodes a tk-route-attempts value.
func routeAttempts(t *testing.T, value string) *routingv1.RouteAttempts {
	t.Helper()
	if value == "" {
		t.Fatal("the response has no tk-route-attempts")
	}
	raw, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		t.Fatalf("tk-route-attempts is not standard base64: %v", err)
	}
	var attempts routingv1.RouteAttempts
	if err := proto.Unmarshal(raw, &attempts); err != nil {
		t.Fatalf("decode tk-route-attempts: %v", err)
	}
	return &attempts
}

// attemptSummary is the part of a RouteAttempt the flow tests compare: its entry, key, status,
// error scope and class, the upstream's error code, and the skip reason.
type attemptSummary struct {
	index         int32
	key           string
	status        int32
	scope, class  string
	code, skipped string
}

func summarize(attempts []*routingv1.RouteAttempt) []attemptSummary {
	summaries := make([]attemptSummary, 0, len(attempts))
	for _, a := range attempts {
		summaries = append(summaries, attemptSummary{a.GetIndex(), a.GetKeyId(), a.GetStatusCode(), a.GetErrorScope(), a.GetErrorClass(), a.GetUpstreamErrorCode(), a.GetSkipReason()})
	}
	return summaries
}

// tokunDrawPlan has providers of close scores, several upstreams and several keys, so the
// selection curve draws a different order for most seeds:
//   - provider-a: up-a (keys key-a1, key-a2) in the request's format, and up-c (key-c1, key-c2),
//     converted;
//   - provider-b: up-b (key-b1) and up-d (key-d1, key-d2), both in the request's format;
//   - provider-e: up-e (key-e1) in the request's format, scoring below the threshold.
//
// So every order is provider-a's up-a and provider-b's upstreams in a drawn order, then key-e1,
// then up-c's two keys in a drawn order.
func tokunDrawPlan(requestID string) *routingv1.RoutePlan {
	upstream := func(id int64, name string, score float64, sameFormat bool, keys ...string) *routingv1.RouteUpstream {
		shares := make([]*plannerv1.KeyShare, 0, len(keys))
		for _, key := range keys {
			shares = append(shares, &plannerv1.KeyShare{KeyId: key, Share: 1})
		}
		return &routingv1.RouteUpstream{
			UpstreamId: id, Interface: fmt.Sprintf("%d:text.chat.openai-chat", id), BifrostProvider: name, ForwardModel: "model-" + name,
			Score: score, Uncertainty: 0.05, SameFormat: sameFormat, Keys: shares, RequestType: string(schemas.ChatCompletionRequest),
		}
	}
	return &routingv1.RoutePlan{
		RequestId: requestID, RequestRate: 10, StaleAfterSeconds: 30, Threshold: 0.5,
		Providers: []*routingv1.RouteProvider{
			{ProviderId: "provider-a", Score: 0.8, Uncertainty: 0.05, UpstreamThreshold: 0.4, Upstreams: []*routingv1.RouteUpstream{
				upstream(1, "up-a", 0.8, true, "key-a1", "key-a2"),
				upstream(3, "up-c", 0.7, false, "key-c1", "key-c2"),
			}},
			{ProviderId: "provider-b", Score: 0.78, Uncertainty: 0.05, UpstreamThreshold: 0.4, Upstreams: []*routingv1.RouteUpstream{
				upstream(2, "up-b", 0.8, true, "key-b1"),
				upstream(4, "up-d", 0.79, true, "key-d1", "key-d2"),
			}},
			{ProviderId: "provider-e", Score: 0.3, Uncertainty: 0.05, UpstreamThreshold: 0.4, Upstreams: []*routingv1.RouteUpstream{
				upstream(5, "up-e", 0.8, true, "key-e1"),
			}},
		},
	}
}

// checkDrawnRecord checks the record of a send of tokunDrawPlan whose every attempt failed: its
// ids; an order of one entry for each key of the plan, with the plan's ids, in the order the
// upstreams were called (served); the shares the selection curve drew with, whose products add
// up to 1 in each group, and only provider-e's entry below a threshold; and one failed attempt
// for each entry, in the order.
func checkDrawnRecord(t *testing.T, record *routingv1.RouteAttempts, plan *routingv1.RoutePlan, sendID string, served []string) {
	t.Helper()
	if record.GetRequestId() != plan.GetRequestId() || record.GetSendId() != sendID {
		t.Fatalf("request_id %q, send_id %q", record.GetRequestId(), record.GetSendId())
	}
	upstreamOf := map[int64]*routingv1.RouteUpstream{}
	providerOf := map[int64]string{}
	for _, provider := range plan.GetProviders() {
		for _, upstream := range provider.GetUpstreams() {
			upstreamOf[upstream.GetUpstreamId()], providerOf[upstream.GetUpstreamId()] = upstream, provider.GetProviderId()
		}
	}
	order := record.GetOrder()
	if len(order) != len(served) {
		t.Fatalf("order has %d entries, %d upstream calls were made", len(order), len(served))
	}
	groupTotal := map[bool]float64{}
	for i, c := range order {
		upstream := upstreamOf[c.GetUpstreamId()]
		if upstream == nil || c.GetProviderId() != providerOf[c.GetUpstreamId()] || c.GetInterface() != upstream.GetInterface() ||
			c.GetSameFormat() != upstream.GetSameFormat() || served[i] != upstream.GetBifrostProvider()+"/secret-"+c.GetKeyId() {
			t.Fatalf("order[%d] = %v, the upstream call was %s", i, c, served[i])
		}
		if c.GetBelowThreshold() != (c.GetProviderId() == "provider-e") {
			t.Fatalf("order[%d] = %v, want only provider-e below a threshold", i, c)
		}
		groupTotal[c.GetSameFormat()] += c.GetProviderShare() * c.GetUpstreamShare() * c.GetKeyShare()
	}
	for sameFormat, total := range groupTotal {
		if math.Abs(total-1) > 1e-9 {
			t.Fatalf("the shares' products of the group same_format=%v add up to %v, want 1: %v", sameFormat, total, order)
		}
	}
	attempts := record.GetAttempts()
	if len(attempts) != len(order) {
		t.Fatalf("%d attempts, want one for each of the %d entries", len(attempts), len(order))
	}
	for i, a := range attempts {
		if a.GetIndex() != int32(i) || a.GetKeyId() != order[i].GetKeyId() || a.GetUpstreamId() != order[i].GetUpstreamId() || a.GetSameFormat() != order[i].GetSameFormat() ||
			a.GetStatusCode() != http.StatusInternalServerError || a.GetErrorScope() != "upstream" || a.GetErrorClass() != "upstream_5xx" {
			t.Fatalf("attempts[%d] = %v, want order entry %d failed with 500", i, a, i)
		}
	}
}

// TestTokunPluginRoutesInsideTheSignedPlan runs the tokun plugin's whole hook flow in one
// Bifrost: the plugin is built from its configuration as the server builds it, requests go
// through the HTTP transport's pre-auth and transport hooks into the OpenAI route, Bifrost
// core and its fallbacks, and out to real upstream HTTP servers.
func TestTokunPluginRoutesInsideTheSignedPlan(t *testing.T) {
	previousLogger := logger
	logger = noopTestLogger{}
	t.Cleanup(func() { logger = previousLogger })

	upstreams := &tokunUpstreams{}
	upstreamServer := httptest.NewServer(upstreams)
	t.Cleanup(upstreamServer.Close)

	providerEntry := func(name string, keyIDs ...string) configstore.ProviderConfig {
		network := schemas.DefaultNetworkConfig
		network.BaseURL = upstreamServer.URL + "/" + name
		network.AllowPrivateNetwork = true
		network.MaxRetries = 0
		keys := make([]schemas.Key, 0, len(keyIDs))
		for _, id := range keyIDs {
			keys = append(keys, schemas.Key{ID: id, Name: id, Value: schemas.SecretVar{Val: "secret-" + id}, Models: schemas.WhiteList{"*"}, Weight: 1})
		}
		return configstore.ProviderConfig{
			Keys:                 keys,
			NetworkConfig:        &network,
			CustomProviderConfig: &schemas.CustomProviderConfig{BaseProviderType: schemas.OpenAI},
		}
	}
	config := &lib.Config{
		ClientConfig: &configstore.ClientConfig{},
		Providers: map[schemas.ModelProvider]configstore.ProviderConfig{
			"up-a": providerEntry("up-a", "key-a1", "key-a2", "key-a3"),
			"up-b": providerEntry("up-b", "key-b1"),
			"up-c": providerEntry("up-c", "key-c1", "key-c2"),
			"up-d": providerEntry("up-d", "key-d1", "key-d2"),
			"up-e": providerEntry("up-e", "key-e1"),
			"up-x": providerEntry("up-x", "key-x1"),
		},
	}
	// Forward every tk-* request header to the upstreams, so a plan header the plugin failed
	// to remove would reach them.
	config.SetHeaderMatcher(lib.NewHeaderMatcher(&configstoreTables.GlobalHeaderFilterConfig{Allowlist: []string{"tk-*"}}))

	// Two signing keys, as during a rotation.
	t.Setenv("TOKUN_TEST_ROUTE_PLAN_SIGNING_KEYS", `{"gw-2026-10":"old-gateway-secret","gw-2026-11":"new-gateway-secret"}`)
	oldKey := planSigner{keyID: "gw-2026-10", secret: "old-gateway-secret"}
	newKey := planSigner{keyID: "gw-2026-11", secret: "new-gateway-secret"}
	plugin, err := loadBuiltinPlugin(context.Background(), tokun.PluginName, map[string]any{"signing_keys": "env.TOKUN_TEST_ROUTE_PLAN_SIGNING_KEYS"}, config)
	if err != nil {
		t.Fatalf("load the tokun plugin: %v", err)
	}
	if err := config.ReloadPlugin(plugin); err != nil {
		t.Fatalf("register the tokun plugin: %v", err)
	}
	observer := &requestObserver{}
	client, err := bifrost.Init(context.Background(), schemas.BifrostConfig{
		Account:         lib.NewBaseAccount(config),
		LLMPlugins:      []schemas.LLMPlugin{plugin.(schemas.LLMPlugin), observer},
		Logger:          bifrost.NewNoOpLogger(),
		InitialPoolSize: 1,
	})
	if err != nil {
		t.Fatalf("init Bifrost: %v", err)
	}
	t.Cleanup(client.Shutdown)

	// Every inference route, registered as the server registers them.
	r := inferenceRouter(t, config, client)

	// The caller names a provider, a model, a fallback and a key outside the plan.
	body := `{"model":"up-x/caller-model","messages":[{"role":"user","content":"hi"}],"fallbacks":["up-x/caller-fallback"]}`
	type reply struct {
		status   int
		body     string
		attempts string // tk-route-attempts
	}
	// request builds the caller's request; the caller's response headers are final once the
	// router's handler returns, before a streamed body is read.
	request := func(method, uri string, headers map[string]string, body string) *fasthttp.RequestCtx {
		var req fasthttp.Request
		req.Header.SetMethod(method)
		req.SetRequestURI(uri)
		req.Header.SetContentType("application/json")
		req.Header.Set("x-bf-api-key-id", "key-a3")
		req.Header.Set("tk-trace", "forwarded")
		for name, value := range headers {
			req.Header.Set(name, value)
		}
		req.SetBodyString(body)
		ctx := &fasthttp.RequestCtx{}
		ctx.Init(&req, nil, nil)
		return ctx
	}
	sendTo := func(t *testing.T, method, uri string, headers map[string]string, body string) reply {
		t.Helper()
		ctx := request(method, uri, headers, body)
		r.Handler(ctx)
		return reply{status: ctx.Response.StatusCode(), body: string(ctx.Response.Body()), attempts: string(ctx.Response.Header.Peek(tokun.HeaderRouteAttempts))}
	}
	send := func(t *testing.T, headers map[string]string, body string) reply {
		t.Helper()
		return sendTo(t, fasthttp.MethodPost, "/openai/v1/chat/completions", headers, body)
	}
	signedFor := func(t *testing.T, signer planSigner, plan *routingv1.RoutePlan, sendID, body string) map[string]string {
		planHeader, signatureHeader := signer.headers(t, plan, sendID, len(body), time.Now().Add(time.Minute))
		return map[string]string{tokun.HeaderRoutePlan: planHeader, tokun.HeaderRoutePlanSignature: signatureHeader}
	}
	signed := func(t *testing.T, signer planSigner, plan *routingv1.RoutePlan, sendID string) map[string]string {
		return signedFor(t, signer, plan, sendID, body)
	}

	t.Run("primary entry with its own key", func(t *testing.T) {
		upstreams.reset()
		got := send(t, signed(t, newKey, tokunTestPlan("req_primary"), "snd_primary"), body)
		if got.status != http.StatusOK {
			t.Fatalf("status %d: %s", got.status, got.body)
		}
		if served := upstreams.served(); !reflect.DeepEqual(served, []string{"up-a/secret-key-a1"}) {
			t.Fatalf("served by %v, want up-a with key-a1 only", served)
		}
		call := upstreams.call(0)
		if call.model != "model-a" {
			t.Errorf("upstream model = %q, want the plan's forward model", call.model)
		}
		for _, name := range []string{tokun.HeaderRoutePlan, tokun.HeaderRoutePlanSignature} {
			if value := call.header.Get(name); value != "" {
				t.Errorf("the upstream received %s", name)
			}
		}
		if call.header.Get("tk-trace") != "forwarded" {
			t.Errorf("the tk-* allowlist forwarded nothing, so the plan headers' absence proves nothing")
		}
		if ids := observer.take(); !reflect.DeepEqual(ids, []string{"snd_primary"}) {
			t.Errorf("core saw request ids %v, want the send_id", ids)
		}
	})

	t.Run("fallbacks in the drawn order, one key each", func(t *testing.T) {
		upstreams.reset("secret-key-a1", "secret-key-a2")
		got := send(t, signed(t, newKey, tokunTestPlan("req_fallbacks"), "snd_fallbacks"), body)
		if got.status != http.StatusOK {
			t.Fatalf("status %d: %s", got.status, got.body)
		}
		want := []string{"up-a/secret-key-a1", "up-a/secret-key-a2", "up-b/secret-key-b1"}
		if served := upstreams.served(); !reflect.DeepEqual(served, want) {
			t.Fatalf("served by %v, want %v", served, want)
		}
		observer.take()
	})

	t.Run("no more than max_attempts entries", func(t *testing.T) {
		upstreams.reset("secret-key-a1", "secret-key-a2")
		plan := tokunTestPlan("req_max_attempts")
		plan.MaxAttempts = 2
		got := send(t, signed(t, newKey, plan, "snd_max_attempts"), body)
		if got.status == http.StatusOK {
			t.Fatalf("status %d, want the last attempt's failure", got.status)
		}
		want := []string{"up-a/secret-key-a1", "up-a/secret-key-a2"}
		if served := upstreams.served(); !reflect.DeepEqual(served, want) {
			t.Fatalf("served by %v, want %v", served, want)
		}
		// The record lists the whole order and the two attempts max_attempts let through.
		record := routeAttempts(t, got.attempts)
		wantAttempts := []attemptSummary{
			{index: 0, key: "key-a1", status: 500, scope: "upstream", class: "upstream_5xx", code: "server_error"},
			{index: 1, key: "key-a2", status: 500, scope: "upstream", class: "upstream_5xx", code: "server_error"},
		}
		if len(record.GetOrder()) != 3 || !reflect.DeepEqual(summarize(record.GetAttempts()), wantAttempts) {
			t.Fatalf("order %v, attempts %+v; want 3 entries and %+v", record.GetOrder(), summarize(record.GetAttempts()), wantAttempts)
		}
		observer.take()
	})

	// Every upstream of tokunDrawPlan fails, so each send tries the whole drawn order. The same
	// seed sends to the same upstreams and keys in the same order; other seeds draw other orders.
	t.Run("the primary and the fallbacks are drawn, the same for the same seed", func(t *testing.T) {
		failing := []string{"secret-key-a1", "secret-key-a2", "secret-key-b1", "secret-key-c1", "secret-key-c2", "secret-key-d1", "secret-key-d2", "secret-key-e1"}
		tokunPlugin := plugin.(*tokun.Plugin)
		sendDrawn := func(t *testing.T, seed uint64, sendID string) []string {
			t.Helper()
			upstreams.reset(failing...)
			tokunPlugin.SeedDraws(seed)
			plan := tokunDrawPlan("req_" + sendID)
			got := send(t, signed(t, newKey, plan, sendID), body)
			if got.status == http.StatusOK {
				t.Fatalf("status %d, want the last attempt's failure", got.status)
			}
			if ids := observer.take(); !reflect.DeepEqual(ids, []string{sendID}) {
				t.Fatalf("core saw request ids %v, want the send_id", ids)
			}
			served := upstreams.served()
			checkDrawnRecord(t, routeAttempts(t, got.attempts), plan, sendID, served)
			return served
		}
		upstreamOf := func(call string) string { name, _, _ := strings.Cut(call, "/"); return name }
		// runs counts the runs of consecutive calls with the same value of what.
		runs := func(calls []string, what func(string) string) int {
			n := 0
			for i, call := range calls {
				if i == 0 || what(call) != what(calls[i-1]) {
					n++
				}
			}
			return n
		}
		providerOf := func(call string) string {
			return map[string]string{"up-a": "provider-a", "up-b": "provider-b", "up-d": "provider-b"}[upstreamOf(call)]
		}

		orders := map[string]bool{}
		primaries := map[string]bool{}
		for seed := uint64(1); seed <= 12; seed++ {
			served := sendDrawn(t, seed, fmt.Sprintf("snd_draw_%d", seed))
			if again := sendDrawn(t, seed, fmt.Sprintf("snd_draw_%d_again", seed)); !reflect.DeepEqual(again, served) {
				t.Fatalf("seed %d sent to %v, then to %v", seed, served, again)
			}
			if len(served) != len(failing) {
				t.Fatalf("seed %d: sent to %v, want every key of the plan once", seed, served)
			}
			// The same-format group: provider-a's up-a and provider-b's two upstreams, each
			// provider's and each upstream's calls together, then provider-e below the threshold.
			// The converted group, up-c, follows.
			sameFormat, below, converted := served[:5], served[5], served[6:]
			if runs(sameFormat, providerOf) != 2 || runs(sameFormat, upstreamOf) != 3 || below != "up-e/secret-key-e1" {
				t.Fatalf("seed %d: sent to %v, want provider-a's and provider-b's upstreams, then up-e", seed, served)
			}
			if upstreamOf(converted[0]) != "up-c" || upstreamOf(converted[1]) != "up-c" {
				t.Fatalf("seed %d: sent to %v, want up-c's keys last", seed, served)
			}
			orders[strings.Join(served, " ")] = true
			primaries[served[0]] = true
		}
		if len(orders) < 6 || len(primaries) < 3 {
			t.Fatalf("12 seeds drew %d orders and %d primaries (%v), want them to differ", len(orders), len(primaries), primaries)
		}
		t.Logf("12 seeds drew %d orders and %d primaries", len(orders), len(primaries))
	})

	// The record of every attempt, tk-route-attempts, decoded: each error scope with its class,
	// status and upstream error code; the switches from a key to the next key of its upstream, to
	// the next upstream and to the next provider; and routing.stop_scopes.
	const (
		invalidKeyBody    = `{"error":{"message":"Incorrect API key provided","type":"invalid_request_error","code":"invalid_api_key"}}`
		rateLimitBody     = `{"error":{"message":"Rate limit reached for requests","type":"requests","code":"rate_limit_exceeded"}}`
		serverErrorBody   = `{"error":{"message":"upstream failed","type":"server_error"}}`
		noCreditsBody     = `{"error":{"message":"Insufficient credits","type":"billing_error","code":"insufficient_credits"}}`
		contextLengthBody = `{"error":{"message":"This model's maximum context length is 8192 tokens","type":"invalid_request_error","code":"context_length_exceeded"}}`
	)
	for _, tc := range []struct {
		name       string
		answers    map[string]upstreamAnswer
		stopScopes []string
		status     int
		// served lists the upstream calls. When a dropped connection was a pooled one, Bifrost's
		// HTTP client sends the request again on other connections within the same attempt
		// (network.StaleConnectionRetryIfErr), as many times as the pool holds idle connections,
		// so with redials the calls are compared with their repeats collapsed.
		served   []string
		redials  bool
		attempts []attemptSummary
	}{
		{
			name:    "a key, then the next key, the next upstream and the next provider",
			answers: map[string]upstreamAnswer{"key-a1": {401, invalidKeyBody}, "key-a2": {429, rateLimitBody}, "key-c1": {500, serverErrorBody}},
			status:  http.StatusOK,
			served:  []string{"up-a/secret-key-a1", "up-a/secret-key-a2", "up-c/secret-key-c1", "up-b/secret-key-b1"},
			attempts: []attemptSummary{
				{index: 0, key: "key-a1", status: 401, scope: "key", class: "auth", code: "invalid_api_key"},
				{index: 1, key: "key-a2", status: 429, scope: "key", class: "throttled", code: "rate_limit_exceeded"},
				{index: 2, key: "key-c1", status: 500, scope: "upstream", class: "upstream_5xx", code: "server_error"},
				{index: 3, key: "key-b1", status: 200},
			},
		},
		{
			name:    "402 and a dropped connection are the upstream's",
			answers: map[string]upstreamAnswer{"key-a1": {402, noCreditsBody}, "key-a2": {0, ""}},
			status:  http.StatusOK,
			served:  []string{"up-a/secret-key-a1", "up-a/secret-key-a2", "up-c/secret-key-c1"},
			redials: true,
			attempts: []attemptSummary{
				{index: 0, key: "key-a1", status: 402, scope: "upstream", class: "upstream_4xx", code: "insufficient_credits"},
				{index: 1, key: "key-a2", status: 0, scope: "upstream", class: "transport"},
				{index: 2, key: "key-c1", status: 200},
			},
		},
		{
			name:    "a request-scope error moves on when stop_scopes is empty",
			answers: map[string]upstreamAnswer{"key-a1": {400, contextLengthBody}},
			status:  http.StatusOK,
			served:  []string{"up-a/secret-key-a1", "up-a/secret-key-a2"},
			attempts: []attemptSummary{
				{index: 0, key: "key-a1", status: 400, scope: "request", code: "context_length_exceeded"},
				{index: 1, key: "key-a2", status: 200},
			},
		},
		{
			name:       "stop_scopes [request] stops on a request-scope error",
			answers:    map[string]upstreamAnswer{"key-a1": {400, contextLengthBody}},
			stopScopes: []string{"request"},
			status:     http.StatusBadRequest,
			served:     []string{"up-a/secret-key-a1"},
			attempts:   []attemptSummary{{index: 0, key: "key-a1", status: 400, scope: "request", code: "context_length_exceeded"}},
		},
		{
			name:       "stop_scopes [request] moves on after a key-scope error",
			answers:    map[string]upstreamAnswer{"key-a1": {429, rateLimitBody}},
			stopScopes: []string{"request"},
			status:     http.StatusOK,
			served:     []string{"up-a/secret-key-a1", "up-a/secret-key-a2"},
			attempts: []attemptSummary{
				{index: 0, key: "key-a1", status: 429, scope: "key", class: "throttled", code: "rate_limit_exceeded"},
				{index: 1, key: "key-a2", status: 200},
			},
		},
		{
			name:       "stop_scopes [upstream] stops on a 500",
			answers:    map[string]upstreamAnswer{"key-a1": {500, serverErrorBody}},
			stopScopes: []string{"upstream"},
			status:     http.StatusInternalServerError,
			served:     []string{"up-a/secret-key-a1"},
			attempts:   []attemptSummary{{index: 0, key: "key-a1", status: 500, scope: "upstream", class: "upstream_5xx", code: "server_error"}},
		},
	} {
		t.Run("tk-route-attempts: "+tc.name, func(t *testing.T) {
			upstreams.reset()
			for key, answer := range tc.answers {
				upstreams.answer("secret-"+key, answer)
			}
			plan := tokunChainPlan("req_chain")
			plan.StopScopes = tc.stopScopes
			sendID := "snd_chain_" + strings.NewReplacer(" ", "_", "[", "", "]", "").Replace(tc.name)
			got := send(t, signed(t, newKey, plan, sendID), body)
			if got.status != tc.status {
				t.Fatalf("status %d: %s, want %d", got.status, got.body, tc.status)
			}
			served := upstreams.served()
			if tc.redials {
				served = slices.Compact(served)
			}
			if !reflect.DeepEqual(served, tc.served) {
				t.Fatalf("served by %v, want %v", served, tc.served)
			}
			record := routeAttempts(t, got.attempts)
			if record.GetRequestId() != "req_chain" || record.GetSendId() != sendID {
				t.Fatalf("request_id %q, send_id %q", record.GetRequestId(), record.GetSendId())
			}
			var order []string
			for _, c := range record.GetOrder() {
				order = append(order, fmt.Sprintf("%s/%d/%s %g %g %g %v", c.GetProviderId(), c.GetUpstreamId(), c.GetKeyId(), c.GetProviderShare(), c.GetUpstreamShare(), c.GetKeyShare(), c.GetBelowThreshold()))
			}
			wantOrder := []string{"provider-a/1/key-a1 1 1 1 false", "provider-a/1/key-a2 1 1 0 false", "provider-a/3/key-c1 1 0 1 true", "provider-b/2/key-b1 0 1 1 true"}
			if !reflect.DeepEqual(order, wantOrder) {
				t.Fatalf("order %v, want %v", order, wantOrder)
			}
			if got := summarize(record.GetAttempts()); !reflect.DeepEqual(got, tc.attempts) {
				t.Fatalf("attempts %+v, want %+v", got, tc.attempts)
			}
			for _, a := range record.GetAttempts() {
				if a.GetStartedAt() == nil || a.GetDuration() == nil || a.GetUpstreamDuration() == nil {
					t.Fatalf("attempt %d has no times: %v", a.GetIndex(), a)
				}
				if a.GetStatusCode() != 0 && a.GetUpstreamDuration().AsDuration() <= 0 {
					t.Fatalf("attempt %d was answered but spent no time on the upstream: %v", a.GetIndex(), a)
				}
			}
			observer.take()
		})
	}

	// An entry whose provider entry is not on this Bifrost is passed over by core without an
	// attempt, and recorded as provider_not_synced.
	t.Run("tk-route-attempts: entries whose provider entry is not on this Bifrost", func(t *testing.T) {
		upstreams.reset()
		plan := tokunChainPlan("req_missing")
		plan.GetProviders()[0].GetUpstreams()[0].BifrostProvider = "up-missing"
		got := send(t, signed(t, newKey, plan, "snd_missing"), body)
		if got.status != http.StatusOK {
			t.Fatalf("status %d: %s", got.status, got.body)
		}
		if served := upstreams.served(); !reflect.DeepEqual(served, []string{"up-c/secret-key-c1"}) {
			t.Fatalf("served by %v, want up-c only", served)
		}
		want := []attemptSummary{
			{index: 0, key: "key-a1", skipped: "provider_not_synced"},
			{index: 1, key: "key-a2", skipped: "provider_not_synced"},
			{index: 2, key: "key-c1", status: 200},
		}
		if got := summarize(routeAttempts(t, got.attempts).GetAttempts()); !reflect.DeepEqual(got, want) {
			t.Fatalf("attempts %+v, want %+v", got, want)
		}
		observer.take()
	})

	// A stream switches only before its first chunk, and carries tk-route-attempts before it: the
	// primary fails, the first fallback streams its first chunk and holds the stream open. The
	// caller's response headers, final while the upstream still holds the stream, carry the
	// record; then the upstream drops the connection, and no further entry is tried.
	t.Run("tk-route-attempts of a stream that breaks after its first chunk", func(t *testing.T) {
		upstreams.reset()
		defer upstreams.breakStreams()
		upstreams.answer("secret-key-a1", upstreamAnswer{500, serverErrorBody})
		streamBody := `{"model":"up-x/caller-model","messages":[{"role":"user","content":"hi"}],"stream":true}`
		ctx := request(fasthttp.MethodPost, "/openai/v1/chat/completions", signedFor(t, newKey, withRequestType(tokunChainPlan("req_stream"), schemas.ChatCompletionStreamRequest), "snd_stream", streamBody), streamBody)
		handled := make(chan struct{})
		go func() {
			defer close(handled)
			r.Handler(ctx)
		}()
		select {
		case <-handled:
		case <-time.After(30 * time.Second):
			t.Fatal("the handler did not return while the upstream held the stream open")
		}
		if ctx.Response.StatusCode() != http.StatusOK {
			t.Fatalf("status %d", ctx.Response.StatusCode())
		}
		want := []attemptSummary{
			{index: 0, key: "key-a1", status: 500, scope: "upstream", class: "upstream_5xx", code: "server_error"},
			{index: 1, key: "key-a2", status: 200},
		}
		if got := summarize(routeAttempts(t, string(ctx.Response.Header.Peek(tokun.HeaderRouteAttempts))).GetAttempts()); !reflect.DeepEqual(got, want) {
			t.Fatalf("attempts %+v, want %+v", got, want)
		}
		wantServed := []string{"up-a/secret-key-a1", "up-a/secret-key-a2"}
		if served := upstreams.served(); !reflect.DeepEqual(served, wantServed) {
			t.Fatalf("served by %v before the stream broke, want %v", served, wantServed)
		}
		upstreams.breakStreams()
		if streamed := string(ctx.Response.Body()); !strings.Contains(streamed, `"content":"ok"`) {
			t.Fatalf("the caller did not receive the first chunk: %s", streamed)
		}
		if served := upstreams.served(); !reflect.DeepEqual(served, wantServed) {
			t.Fatalf("served by %v after the stream broke, want %v: the send switched after its first chunk", served, wantServed)
		}
		observer.take()
	})

	t.Run("the key being rotated out still verifies", func(t *testing.T) {
		upstreams.reset()
		got := send(t, signed(t, oldKey, tokunTestPlan("req_old_key"), "snd_old_key"), body)
		if got.status != http.StatusOK {
			t.Fatalf("status %d: %s", got.status, got.body)
		}
		observer.take()
	})

	refusals := []struct {
		name    string
		headers func(t *testing.T) map[string]string
		body    string
		code    string
	}{
		{"no plan", func(t *testing.T) map[string]string { return nil }, body, "route_plan_missing"},
		{"signature under a key the plugin does not hold", func(t *testing.T) map[string]string {
			return signed(t, planSigner{keyID: "gw-2026-11", secret: "forged"}, tokunTestPlan("req_forged"), "snd_forged")
		}, body, "signature_invalid"},
		{"expired", func(t *testing.T) map[string]string {
			planHeader, signatureHeader := newKey.headers(t, tokunTestPlan("req_expired"), "snd_expired", len(body), time.Now().Add(-time.Second))
			return map[string]string{tokun.HeaderRoutePlan: planHeader, tokun.HeaderRoutePlanSignature: signatureHeader}
		}, body, "route_plan_expired"},
		{"body of another length", func(t *testing.T) map[string]string {
			return signed(t, newKey, tokunTestPlan("req_body"), "snd_body")
		}, body + " ", "body_bytes_mismatch"},
		{"send_id used before on this pod", func(t *testing.T) map[string]string {
			return signed(t, newKey, tokunTestPlan("req_primary"), "snd_primary")
		}, body, "send_id_reused"},
	}
	for _, refusal := range refusals {
		t.Run("401: "+refusal.name, func(t *testing.T) {
			upstreams.reset()
			got := send(t, refusal.headers(t), refusal.body)
			if got.status != http.StatusUnauthorized {
				t.Fatalf("status %d: %s, want 401", got.status, got.body)
			}
			var answer struct {
				Error struct {
					Type string `json:"type"`
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(got.body), &answer); err != nil {
				t.Fatalf("401 body %q: %v", got.body, err)
			}
			if answer.Error.Type != "tokun_route_plan_rejected" || answer.Error.Code != refusal.code {
				t.Fatalf("401 body %s, want code %s", got.body, refusal.code)
			}
			if served := upstreams.served(); len(served) != 0 {
				t.Fatalf("a refused request reached %v", served)
			}
			if ids := observer.take(); len(ids) != 0 {
				t.Fatalf("a refused request entered Bifrost core as %v", ids)
			}
		})
	}

	// The route decides before the plan: a route tokun does not offer is 404 and the realtime
	// conversation 501, whatever plan the request carries. Every refusal below carries the same
	// valid plan, and the plan route after them accepts it, so no refusal used up its send_id.
	routeRefusals := []struct {
		name, method, uri string
		status            int
		code              string
	}{
		{"WebSocket Responses", fasthttp.MethodGet, "/v1/responses", http.StatusNotFound, "tokun_route_not_offered"},
		{"live session", fasthttp.MethodPost, "/v1/live/sessions", http.StatusNotFound, "tokun_route_not_offered"},
		{"Bifrost's MCP server", fasthttp.MethodPost, "/mcp", http.StatusNotFound, "tokun_route_not_offered"},
		{"MCP tool execution", fasthttp.MethodPost, "/v1/mcp/tool/execute", http.StatusNotFound, "tokun_route_not_offered"},
		{"realtime WebSocket", fasthttp.MethodGet, "/v1/realtime", http.StatusNotImplemented, "tokun_route_not_implemented"},
		{"realtime WebRTC call", fasthttp.MethodPost, "/v1/realtime/calls", http.StatusNotImplemented, "tokun_route_not_implemented"},
		{"realtime client secret", fasthttp.MethodPost, "/v1/realtime/client_secrets", http.StatusNotImplemented, "tokun_route_not_implemented"},
		{"realtime session under /openai", fasthttp.MethodPost, "/openai/v1/realtime/sessions", http.StatusNotImplemented, "tokun_route_not_implemented"},
	}
	refusedRouteHeaders := signed(t, newKey, tokunTestPlan("req_route"), "snd_route")
	for _, refusal := range routeRefusals {
		t.Run(fmt.Sprintf("%d: %s", refusal.status, refusal.name), func(t *testing.T) {
			upstreams.reset()
			got := sendTo(t, refusal.method, refusal.uri, refusedRouteHeaders, body)
			if got.status != refusal.status {
				t.Fatalf("status %d: %s, want %d", got.status, got.body, refusal.status)
			}
			var answer struct {
				Error struct {
					Type    string `json:"type"`
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(got.body), &answer); err != nil {
				t.Fatalf("body %q: %v", got.body, err)
			}
			if answer.Error.Type != "tokun_route_rejected" || answer.Error.Code != refusal.code || answer.Error.Message == "" {
				t.Fatalf("body %s, want code %s", got.body, refusal.code)
			}
			if served := upstreams.served(); len(served) != 0 {
				t.Fatalf("a refused request reached %v", served)
			}
			if ids := observer.take(); len(ids) != 0 {
				t.Fatalf("a refused request entered Bifrost core as %v", ids)
			}
		})
	}

	t.Run("a plan route of the inference handler, with the plan the refused routes carried", func(t *testing.T) {
		upstreams.reset()
		got := sendTo(t, fasthttp.MethodPost, "/v1/chat/completions", refusedRouteHeaders, body)
		if got.status != http.StatusOK {
			t.Fatalf("status %d: %s", got.status, got.body)
		}
		if served := upstreams.served(); !reflect.DeepEqual(served, []string{"up-a/secret-key-a1"}) {
			t.Fatalf("served by %v, want up-a with key-a1 only", served)
		}
		if ids := observer.take(); !reflect.DeepEqual(ids, []string{"snd_route"}) {
			t.Errorf("core saw request ids %v, want the send_id", ids)
		}
	})
}

// inferenceRouter registers every inference route on a router the way the server does: the
// router records the matched route template, and the routes run the common route middleware, the
// pre-auth transport phase and the transport phase after authentication. The authentication
// middleware is left out, as with enforce_auth_on_inference off. It sets config's WebSocket
// defaults, which the WebSocket handlers read.
func inferenceRouter(t *testing.T, config *lib.Config, client *bifrost.Bifrost) *router.Router {
	t.Helper()
	handlers.SetLogger(bifrost.NewNoOpLogger())
	lib.SetLogger(bifrost.NewNoOpLogger())
	config.WebSocketConfig = &schemas.WebSocketConfig{}
	config.WebSocketConfig.CheckAndSetDefaults()
	s := &BifrostHTTPServer{Config: config, Client: client, Router: router.New()}
	s.Router.SaveMatchedRoutePath = true
	if err := s.InitializeAgentGateway(context.Background(), nil, nil); err != nil {
		t.Fatalf("initialize the Agent Gateway: %v", err)
	}
	t.Cleanup(s.CloseAgentGateway)
	if err := s.RegisterInferenceRoutes(context.Background(),
		handlers.HTTPRouteMiddleware(),
		handlers.TransportPreAuthInterceptorMiddleware(config),
		handlers.TransportInterceptorMiddleware(config),
	); err != nil {
		t.Fatalf("register the inference routes: %v", err)
	}
	t.Cleanup(func() { s.IntegrationHandler.Close() })
	return s.Router
}

// TestTokunRouteClassesCoverEveryInferenceRoute lists the routes RegisterInferenceRoutes
// actually registers, with every optional handler on (the Agent Gateway and the async routes),
// and requires each to have a tokun route class; a route added to Bifrost without a class fails
// here. Every route the plugin classifies must also be registered, so the table holds no route
// Bifrost no longer has.
func TestTokunRouteClassesCoverEveryInferenceRoute(t *testing.T) {
	SetLogger(agentGatewayRouteLogger{})
	t.Cleanup(func() { SetLogger(nil) })
	config := &lib.Config{
		ConfigStore:      &agentGatewayRouteStore{},
		ClientConfig:     &configstore.ClientConfig{},
		Providers:        map[schemas.ModelProvider]configstore.ProviderConfig{},
		AsyncJobExecutor: logstore.NewAsyncJobExecutor(nil, nil, nil, nil, bifrost.NewNoOpLogger()),
	}
	client, err := bifrost.Init(context.Background(), schemas.BifrostConfig{
		Account: lib.NewBaseAccount(config), Logger: bifrost.NewNoOpLogger(), InitialPoolSize: 1,
	})
	if err != nil {
		t.Fatalf("init Bifrost: %v", err)
	}
	t.Cleanup(client.Shutdown)
	r := inferenceRouter(t, config, client)

	registered := map[string]bool{}
	classes := map[string]int{}
	for method, templates := range r.List() {
		for _, template := range templates {
			registered[method+" "+template] = true
			class := tokun.RouteClass(method, template)
			if class == "" {
				t.Errorf("%s %s has no tokun route class: add it to plugins/tokun/routes.go", method, template)
				continue
			}
			classes[class]++
		}
	}
	for _, route := range tokun.ClassifiedRoutes() {
		if !registered[route] {
			t.Errorf("plugins/tokun/routes.go classifies %s, which Bifrost does not register", route)
		}
	}
	if len(registered) == 0 || classes["plan"] == 0 || classes["not_offered"] == 0 || classes["not_implemented"] == 0 {
		t.Fatalf("registered %d routes, classes %v", len(registered), classes)
	}
	t.Logf("%d inference routes: %v", len(registered), classes)
}

// formatCall is one request a format upstream received: the Bifrost provider entry it was sent
// to, the API path under the entry, the credential and the body bytes.
type formatCall struct {
	entry, path, key string
	body             []byte
}

// formatUpstreams stands in for the upstream APIs of the provider entries of the text format
// tests, each entry under its own path prefix: OpenAI's chat completions, Responses and input
// token count, and Anthropic's Messages and token count, streamed or not. It answers "ok" in
// the format of the path, and 500 to a credential listed in failing. A streamed chat completion
// sends its trailing usage-only chunk only when the body asks for it, as OpenAI does.
type formatUpstreams struct {
	mu      sync.Mutex
	calls   []formatCall
	failing map[string]bool
}

func (u *formatUpstreams) reset(failing ...string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.calls = nil
	u.failing = map[string]bool{}
	for _, key := range failing {
		u.failing[key] = true
	}
}

func (u *formatUpstreams) take() []formatCall {
	u.mu.Lock()
	defer u.mu.Unlock()
	calls := u.calls
	u.calls = nil
	return calls
}

func (u *formatUpstreams) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	entry, path, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	path = "/" + path
	body, _ := io.ReadAll(r.Body)
	key := r.Header.Get("x-api-key")
	if key == "" {
		key = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	u.mu.Lock()
	u.calls = append(u.calls, formatCall{entry: entry, path: path, key: key, body: body})
	failing := u.failing[key]
	u.mu.Unlock()
	if failing {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"api_error","message":"upstream failed"}}`)
		return
	}
	model, err := json.Marshal(gjson.GetBytes(body, "model").String())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	fill := func(template string) string { return strings.ReplaceAll(template, "MODEL", string(model)) }
	answers, streams, usageChunk := formatAnswers, formatStreams, chatUsageChunk
	if entryAnswers, ok := usageEntryAnswers[entry]; ok {
		// An entry that reports every usage field, and serves only the APIs it lists.
		answers, streams, usageChunk = entryAnswers, usageEntryStreams[entry], richChatUsageChunk
		if _, ok := answers[path]; !ok {
			http.Error(w, `{"error":{"message":"Unknown request URL: `+path+`","type":"invalid_request_error"}}`, http.StatusNotFound)
			return
		}
	}
	if gjson.GetBytes(body, "stream").Bool() {
		events := streams[path]
		if path == "/v1/chat/completions" && gjson.GetBytes(body, "stream_options.include_usage").Bool() {
			events = append(slices.Clone(events), usageChunk)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range events {
			_, _ = io.WriteString(w, fill(event)+"\n\n")
		}
		if path == "/v1/chat/completions" {
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		}
		return
	}
	answer, ok := answers[path]
	if !ok {
		http.Error(w, "no such API "+path, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, fill(answer))
}

// richChatUsage is the usage of the up-chatonly entry's chat completions: every field a chat
// completion reports, cache reads, cache writes with their 5-minute and 1-hour parts, reasoning
// and two web searches among them.
const richChatUsage = `{"prompt_tokens":1200,"completion_tokens":300,"total_tokens":1500,` +
	`"prompt_tokens_details":{"text_tokens":1100,"audio_tokens":50,"image_tokens":50,"cached_tokens":400,"cache_write_tokens":250,` +
	`"cached_write_token_details":{"cached_write_tokens_5m":150,"cached_write_tokens_1h":100}},` +
	`"completion_tokens_details":{"text_tokens":170,"audio_tokens":5,"image_tokens":7,"reasoning_tokens":120,"accepted_prediction_tokens":3,"rejected_prediction_tokens":4},` +
	`"tool_usage":{"web_search":{"num_requests":2}}}`

// richChatUsageChunk is the trailing usage-only chunk of the up-chatonly entry's streams.
const richChatUsageChunk = `data: {"id":"chatcmpl-2","object":"chat.completion.chunk","created":1,"model":MODEL,"choices":[],"usage":` + richChatUsage + `}`

// usageEntryAnswers are the answers of the entries that report every usage field, by entry and
// API path; such an entry answers 404 to any other path:
//   - up-chatonly, an OpenAI-compatible upstream that has chat completions and no Responses API;
//   - up-anthropic-cache, Anthropic's Messages API, whose usage writes the cache for 5 minutes and
//     for 1 hour.
var usageEntryAnswers = map[string]map[string]string{
	"up-chatonly": {
		"/v1/chat/completions": `{"id":"chatcmpl-2","object":"chat.completion","created":1,"model":MODEL,"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":` + richChatUsage + `}`,
	},
	"up-anthropic-cache": {
		"/v1/messages": `{"id":"msg_2","type":"message","role":"assistant","model":MODEL,"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","stop_sequence":null,` +
			`"usage":{"input_tokens":550,"output_tokens":300,"cache_read_input_tokens":400,"cache_creation_input_tokens":250,"cache_creation":{"ephemeral_5m_input_tokens":150,"ephemeral_1h_input_tokens":100}}}`,
	},
}

// usageEntryStreams are the streamed answers of the entries of usageEntryAnswers.
var usageEntryStreams = map[string]map[string][]string{
	"up-chatonly": {
		"/v1/chat/completions": {
			`data: {"id":"chatcmpl-2","object":"chat.completion.chunk","created":1,"model":MODEL,"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
			`data: {"id":"chatcmpl-2","object":"chat.completion.chunk","created":1,"model":MODEL,"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`,
		},
	},
	"up-anthropic-cache": {
		"/v1/messages": {
			"event: message_start\n" + `data: {"type":"message_start","message":{"id":"msg_2","type":"message","role":"assistant","model":MODEL,"content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":550,"output_tokens":1,"cache_read_input_tokens":400,"cache_creation_input_tokens":250,"cache_creation":{"ephemeral_5m_input_tokens":150,"ephemeral_1h_input_tokens":100}}}}`,
			"event: content_block_start\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			"event: content_block_delta\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
			"event: content_block_stop\n" + `data: {"type":"content_block_stop","index":0}`,
			"event: message_delta\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":300}}`,
			"event: message_stop\n" + `data: {"type":"message_stop"}`,
		},
	},
}

// formatAnswers are the non-streamed answers by API path; MODEL is the requested model.
var formatAnswers = map[string]string{
	"/v1/chat/completions":       `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":MODEL,"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`,
	"/v1/responses":              `{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":MODEL,"output":[{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"ok","annotations":[]}]}],"usage":{"input_tokens":5,"output_tokens":1,"total_tokens":6}}`,
	"/v1/responses/input_tokens": `{"object":"response.input_tokens","input_tokens":12}`,
	"/v1/messages":               `{"id":"msg_1","type":"message","role":"assistant","model":MODEL,"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":5,"output_tokens":1}}`,
	"/v1/messages/count_tokens":  `{"input_tokens":12}`,
	"/model/up-model/converse":   `{"output":{"message":{"role":"assistant","content":[{"text":"ok"}]}},"stopReason":"end_turn","usage":{"inputTokens":5,"outputTokens":1,"totalTokens":6},"metrics":{"latencyMs":1}}`,
	"/v2/chat":                   `{"id":"chat-1","finish_reason":"COMPLETE","message":{"role":"assistant","content":[{"type":"text","text":"ok"}]},"usage":{"billed_units":{"input_tokens":5,"output_tokens":1},"tokens":{"input_tokens":5,"output_tokens":1}}}`,
}

// chatUsageChunk is the trailing usage-only chunk of a chat completion stream that asked for it.
const chatUsageChunk = `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":MODEL,"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`

// formatStreams are the streamed answers by API path. The chat completion's last content chunk
// carries the finish reason, so the usage the stream reports comes in a chunk of its own.
var formatStreams = map[string][]string{
	"/v1/chat/completions": {
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":MODEL,"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":MODEL,"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`,
	},
	"/v1/responses": {
		"event: response.created\n" + `data: {"type":"response.created","sequence_number":0,"response":{"id":"resp_1","object":"response","created_at":1,"status":"in_progress","model":MODEL,"output":[]}}`,
		"event: response.output_item.added\n" + `data: {"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"id":"msg_1","type":"message","status":"in_progress","role":"assistant","content":[]}}`,
		"event: response.content_part.added\n" + `data: {"type":"response.content_part.added","sequence_number":2,"item_id":"msg_1","output_index":0,"content_index":0,"part":{"type":"output_text","text":"","annotations":[]}}`,
		"event: response.output_text.delta\n" + `data: {"type":"response.output_text.delta","sequence_number":3,"item_id":"msg_1","output_index":0,"content_index":0,"delta":"ok"}`,
		"event: response.output_text.done\n" + `data: {"type":"response.output_text.done","sequence_number":4,"item_id":"msg_1","output_index":0,"content_index":0,"text":"ok"}`,
		"event: response.content_part.done\n" + `data: {"type":"response.content_part.done","sequence_number":5,"item_id":"msg_1","output_index":0,"content_index":0,"part":{"type":"output_text","text":"ok","annotations":[]}}`,
		"event: response.output_item.done\n" + `data: {"type":"response.output_item.done","sequence_number":6,"output_index":0,"item":{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"ok","annotations":[]}]}}`,
		"event: response.completed\n" + `data: {"type":"response.completed","sequence_number":7,"response":{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":MODEL,"output":[{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"ok","annotations":[]}]}],"usage":{"input_tokens":5,"output_tokens":1,"total_tokens":6}}}`,
	},
	"/v1/messages": {
		"event: message_start\n" + `data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":MODEL,"content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":5,"output_tokens":1}}}`,
		"event: content_block_start\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		"event: content_block_delta\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
		"event: content_block_stop\n" + `data: {"type":"content_block_stop","index":0}`,
		"event: message_delta\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}`,
		"event: message_stop\n" + `data: {"type":"message_stop"}`,
	},
}

// sseEvent is one event of a server-sent event stream.
type sseEvent struct{ event, data string }

func sseEvents(body string) []sseEvent {
	var events []sseEvent
	for _, block := range strings.Split(body, "\n\n") {
		var e sseEvent
		for _, line := range strings.Split(block, "\n") {
			if name, ok := strings.CutPrefix(line, "event:"); ok {
				e.event = strings.TrimSpace(name)
			} else if data, ok := strings.CutPrefix(line, "data:"); ok {
				e.data = strings.TrimSpace(data)
			}
		}
		if e.data != "" {
			events = append(events, e)
		}
	}
	return events
}

// formatObserver is an LLM plugin registered after the tokun plugin: it records the integration
// and the request type each request reaches core with.
type formatObserver struct {
	mu   sync.Mutex
	seen []string
}

func (o *formatObserver) GetName() string { return "tokun-format-observer" }
func (o *formatObserver) Cleanup() error  { return nil }
func (o *formatObserver) PreRequestHook(ctx *schemas.BifrostContext, req *schemas.BifrostRequest) error {
	integration, _ := ctx.Value(schemas.BifrostContextKeyIntegrationType).(string)
	o.mu.Lock()
	o.seen = append(o.seen, integration+" "+string(req.RequestType))
	o.mu.Unlock()
	return nil
}
func (o *formatObserver) PreLLMHook(_ *schemas.BifrostContext, req *schemas.BifrostRequest) (*schemas.BifrostRequest, *schemas.LLMPluginShortCircuit, error) {
	return req, nil, nil
}
func (o *formatObserver) PostLLMHook(_ *schemas.BifrostContext, resp *schemas.BifrostResponse, err *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError, error) {
	return resp, err, nil
}
func (o *formatObserver) take() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	seen := o.seen
	o.seen = nil
	return seen
}

// formatReply is what the caller of a text format test received.
type formatReply struct {
	status   int
	body     string
	stripped string
	attempts string // tk-route-attempts
}

// formatServer is one Bifrost with every inference route, whose provider entries are up-openai
// (key oa-1) and up-openai-2 (key ob-1), OpenAI's API, up-anthropic (key an-1), Anthropic's,
// up-cohere (key co-1), Cohere's, and up-bedrock (key bk-1), Bedrock's over TLS, all answered by
// upstreams. send writes the request_type of the plan's upstreams that have none
// (plannedRequestTypes) and signs plan for the request; sendAsIs signs it as it is. A nil plan
// sends the request without one.
type formatServer struct {
	upstreams *formatUpstreams
	observer  *formatObserver
	send      func(t *testing.T, uri, body string, plan *routingv1.RoutePlan, sendID string) formatReply
	sendAsIs  func(t *testing.T, uri, body string, plan *routingv1.RoutePlan, sendID string) formatReply
}

// newFormatServer builds a formatServer, with the tokun plugin loaded when withPlugin is set and
// the LLM plugins extra after the others.
func newFormatServer(t *testing.T, withPlugin bool, extra ...schemas.LLMPlugin) *formatServer {
	t.Helper()
	previousLogger := logger
	logger = noopTestLogger{}
	t.Cleanup(func() { logger = previousLogger })

	upstreams := &formatUpstreams{}
	upstreamServer := httptest.NewServer(upstreams)
	t.Cleanup(upstreamServer.Close)
	providerEntry := func(name string, base schemas.ModelProvider, keyID string) configstore.ProviderConfig {
		network := schemas.DefaultNetworkConfig
		network.BaseURL = upstreamServer.URL + "/" + name
		network.AllowPrivateNetwork = true
		network.MaxRetries = 0
		return configstore.ProviderConfig{
			Keys:                 []schemas.Key{{ID: keyID, Name: keyID, Value: schemas.SecretVar{Val: keyID}, Models: schemas.WhiteList{"*"}, Weight: 1}},
			NetworkConfig:        &network,
			CustomProviderConfig: &schemas.CustomProviderConfig{BaseProviderType: base},
		}
	}
	// Bedrock's runtime host is set per key and called over HTTPS; its upstream sees the API path
	// under /up-bedrock, as the others see theirs under their entry.
	bedrockServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.URL.Path = "/up-bedrock" + r.URL.Path
		upstreams.ServeHTTP(w, r)
	}))
	t.Cleanup(bedrockServer.Close)
	bedrockNetwork := schemas.DefaultNetworkConfig
	bedrockNetwork.AllowPrivateNetwork = true
	bedrockNetwork.InsecureSkipVerify = true
	bedrockNetwork.MaxRetries = 0
	bedrockEntry := configstore.ProviderConfig{
		Keys: []schemas.Key{{ID: "bk-1", Name: "bk-1", Value: schemas.SecretVar{Val: "bk-1"}, Models: schemas.WhiteList{"*"}, Weight: 1,
			BedrockKeyConfig: &schemas.BedrockKeyConfig{
				Region:    schemas.NewSecretVar("us-east-1"),
				Endpoints: &schemas.BedrockEndpoints{Runtime: schemas.NewSecretVar(strings.TrimPrefix(bedrockServer.URL, "https://"))},
			}}},
		NetworkConfig:        &bedrockNetwork,
		CustomProviderConfig: &schemas.CustomProviderConfig{BaseProviderType: schemas.Bedrock},
	}
	config := &lib.Config{
		ClientConfig: &configstore.ClientConfig{},
		Providers: map[schemas.ModelProvider]configstore.ProviderConfig{
			"up-openai":    providerEntry("up-openai", schemas.OpenAI, "oa-1"),
			"up-openai-2":  providerEntry("up-openai-2", schemas.OpenAI, "ob-1"),
			"up-anthropic": providerEntry("up-anthropic", schemas.Anthropic, "an-1"),
			"up-cohere":    providerEntry("up-cohere", schemas.Cohere, "co-1"),
			"up-bedrock":   bedrockEntry,
			// The entries of usageEntryAnswers.
			"up-chatonly":        providerEntry("up-chatonly", schemas.OpenAI, "ch-1"),
			"up-anthropic-cache": providerEntry("up-anthropic-cache", schemas.Anthropic, "ac-1"),
		},
	}
	t.Setenv("TOKUN_TEST_FORMAT_SIGNING_KEYS", `{"gw-2026-11":"gateway-secret"}`)
	signer := planSigner{keyID: "gw-2026-11", secret: "gateway-secret"}
	observer := &formatObserver{}
	// The compat plugin with the server's default settings, which the server always loads ahead
	// of the tokun plugin.
	compatPlugin, err := loadBuiltinPlugin(context.Background(), compat.PluginName, &compat.Config{ForceReasoningOnlyModelsToResponses: true}, config)
	if err != nil {
		t.Fatalf("load the compat plugin: %v", err)
	}
	llmPlugins := []schemas.LLMPlugin{compatPlugin.(schemas.LLMPlugin), observer}
	if withPlugin {
		plugin, err := loadBuiltinPlugin(context.Background(), tokun.PluginName, map[string]any{"signing_keys": "env.TOKUN_TEST_FORMAT_SIGNING_KEYS"}, config)
		if err != nil {
			t.Fatalf("load the tokun plugin: %v", err)
		}
		if err := config.ReloadPlugin(plugin); err != nil {
			t.Fatalf("register the tokun plugin: %v", err)
		}
		llmPlugins = []schemas.LLMPlugin{compatPlugin.(schemas.LLMPlugin), plugin.(schemas.LLMPlugin), observer}
	}
	llmPlugins = append(llmPlugins, extra...)
	client, err := bifrost.Init(context.Background(), schemas.BifrostConfig{
		Account:         lib.NewBaseAccount(config),
		LLMPlugins:      llmPlugins,
		Logger:          bifrost.NewNoOpLogger(),
		InitialPoolSize: 1,
	})
	if err != nil {
		t.Fatalf("init Bifrost: %v", err)
	}
	t.Cleanup(client.Shutdown)
	r := inferenceRouter(t, config, client)

	sendAsIs := func(t *testing.T, uri, body string, plan *routingv1.RoutePlan, sendID string) formatReply {
		t.Helper()
		var req fasthttp.Request
		req.Header.SetMethod(fasthttp.MethodPost)
		req.SetRequestURI(uri)
		req.Header.SetContentType("application/json")
		if plan != nil {
			planHeader, signatureHeader := signer.headers(t, plan, sendID, len(body), time.Now().Add(time.Minute))
			req.Header.Set(tokun.HeaderRoutePlan, planHeader)
			req.Header.Set(tokun.HeaderRoutePlanSignature, signatureHeader)
		}
		req.SetBodyString(body)
		ctx := &fasthttp.RequestCtx{}
		ctx.Init(&req, nil, nil)
		r.Handler(ctx)
		// A streamed response is written by its body stream writer.
		return formatReply{
			status:   ctx.Response.StatusCode(),
			body:     string(ctx.Response.Body()),
			stripped: string(ctx.Response.Header.Peek(tokun.HeaderStrippedParams)),
			attempts: string(ctx.Response.Header.Peek(tokun.HeaderRouteAttempts)),
		}
	}
	send := func(t *testing.T, uri, body string, plan *routingv1.RoutePlan, sendID string) formatReply {
		t.Helper()
		if plan != nil {
			plannedRequestTypes(uri, body, plan)
		}
		return sendAsIs(t, uri, body, plan, sendID)
	}
	return &formatServer{upstreams: upstreams, observer: observer, send: send, sendAsIs: sendAsIs}
}

// plannedRequestTypes writes the request_type of each upstream of plan that has none, the way the
// gateway writes it for a request of the route uri with body: an upstream in the request's format
// takes the request's own type; any other takes its interface's type for the same operation,
// count_tokens on a token count route, and otherwise Responses on the OpenAI, Anthropic and
// Bedrock entries and chat completions on the Cohere entry, streamed when the request is.
func plannedRequestTypes(uri, body string, plan *routingv1.RoutePlan) {
	stream := gjson.Get(body, "stream").Bool()
	own := schemas.ResponsesRequest
	switch {
	case strings.HasSuffix(uri, "/count_tokens"):
		own = schemas.CountTokensRequest
	case strings.HasSuffix(uri, "/chat/completions"), strings.HasSuffix(uri, "/v2/chat"):
		own = schemas.ChatCompletionRequest
	}
	streamed := func(requestType schemas.RequestType) schemas.RequestType {
		switch {
		case !stream || requestType == schemas.CountTokensRequest:
			return requestType
		case requestType == schemas.ChatCompletionRequest:
			return schemas.ChatCompletionStreamRequest
		}
		return schemas.ResponsesStreamRequest
	}
	for _, provider := range plan.GetProviders() {
		for _, upstream := range provider.GetUpstreams() {
			if upstream.GetRequestType() != "" {
				continue
			}
			requestType := own
			if !upstream.GetSameFormat() && own != schemas.CountTokensRequest {
				requestType = schemas.ResponsesRequest
				if upstream.GetBifrostProvider() == "up-cohere" {
					requestType = schemas.ChatCompletionRequest
				}
			}
			upstream.RequestType = string(streamed(requestType))
		}
	}
}

// formatPlan lists each upstream under a provider of its own, each provider scored far below the
// one before it with almost no uncertainty, so every order of the plan is the order given: the
// selection curve gives a later provider a share of practically 0.
func formatPlan(requestID string, upstreams ...*routingv1.RouteUpstream) *routingv1.RoutePlan {
	plan := &routingv1.RoutePlan{RequestId: requestID, RequestRate: 1e9, StaleAfterSeconds: 30}
	for i, upstream := range upstreams {
		score := 0.9 - 0.4*float64(i)
		upstream.UpstreamId = int64(i + 1)
		upstream.Score, upstream.Uncertainty = score, 0.001
		plan.Providers = append(plan.Providers, &routingv1.RouteProvider{
			ProviderId: fmt.Sprintf("provider-%d", i+1), Score: score, Uncertainty: 0.001,
			Upstreams: []*routingv1.RouteUpstream{upstream},
		})
	}
	return plan
}

// formatUpstream is a plan entry on the Bifrost provider entry bifrostProvider, whose one key is
// keyID, sending the model as forwardModel, with an interface output limit of 8192 tokens.
func formatUpstream(bifrostProvider, keyID, forwardModel string, sameFormat bool, stripParams ...string) *routingv1.RouteUpstream {
	return &routingv1.RouteUpstream{
		Interface: "1:" + bifrostProvider, BifrostProvider: bifrostProvider, ForwardModel: forwardModel,
		SameFormat: sameFormat, StripParams: stripParams, InterfaceOutputTokens: 8192,
		Keys: []*plannerv1.KeyShare{{KeyId: keyID, Share: 1}},
	}
}

// The bodies the callers of the text format tests send: odd spacing, a field no format registers,
// and a model name only tokun knows. Each attempt in the request's format must forward these bytes
// with only the model changed.
const (
	chatCallerBody      = `{"model":"tk/chat-model" , "messages":[{"role":"user","content":"hi"}],"temperature":1,"zz_unknown":{"b":1,"a":[ ]},"max_completion_tokens":64}`
	responsesCallerBody = `{"model":"tk/responses-model","input":[{"role":"user","content":"hi"}] ,"max_output_tokens":64,"zz_unknown":{"b":1},"store":false}`
	messagesCallerBody  = `{"model":"tk/claude","max_tokens":64 ,"messages":[{"role":"user","content":"hi"}],"zz_unknown":{"b":1}}`
	countCallerBody     = `{"model":"tk/claude","messages":[{"role":"user","content":"hi"}] ,"zz_unknown":{"b":1}}`
	cohereCallerBody    = `{"model":"tk/command","messages":[{"role":"user","content":"hi"}] ,"zz_unknown":{"b":1},"max_tokens":64}`
	// A Converse body names no model: the model is in the path.
	converseCallerBody = `{"messages":[{"role":"user","content":[{"text":"hi"}]}] ,"inferenceConfig":{"maxTokens":64},"zz_unknown":{"b":1}}`
)

// streamed adds "stream":true and extra to the end of a caller body.
func streamed(body, extra string) string {
	return strings.TrimSuffix(body, "}") + `,"stream":true` + extra + "}"
}

// forwarded is body with its model replaced by model, the only change a same-format attempt makes;
// a body that names no model (from is empty) is forwarded as it is.
func forwarded(t *testing.T, body, from, model string) []byte {
	t.Helper()
	if from == "" {
		return []byte(body)
	}
	old := `"model":"` + from + `"`
	if strings.Count(body, old) != 1 {
		t.Fatalf("%s is not in the body once: %s", old, body)
	}
	return []byte(strings.Replace(body, old, `"model":"`+model+`"`, 1))
}

// checkChatStream checks a chat completion stream the caller received: chunks that spell "ok" and
// end with [DONE], and whether any chunk carries usage.
func checkChatStream(t *testing.T, got formatReply, wantUsage bool) {
	t.Helper()
	events := sseEvents(got.body)
	if len(events) == 0 || events[len(events)-1].data != "[DONE]" {
		t.Fatalf("the stream does not end with [DONE]: %s", got.body)
	}
	var text strings.Builder
	usage := false
	for _, e := range events[:len(events)-1] {
		chunk := gjson.Parse(e.data)
		if chunk.Get("object").String() != "chat.completion.chunk" {
			t.Fatalf("not a chat completion chunk: %s", e.data)
		}
		text.WriteString(chunk.Get("choices.0.delta.content").String())
		if chunk.Get("usage").IsObject() {
			usage = true
			if chunk.Get("usage.total_tokens").Int() != 6 {
				t.Errorf("the usage chunk reports %s", chunk.Get("usage").Raw)
			}
		}
	}
	if text.String() != "ok" {
		t.Errorf("the chunks spell %q: %s", text.String(), got.body)
	}
	if usage != wantUsage {
		t.Errorf("a chunk carries usage: %v, want %v: %s", usage, wantUsage, got.body)
	}
}

// checkCallerFormat checks that the caller received "ok" in the format of the route it called. A
// streamed chat completion is one whose caller asked for usage.
func checkCallerFormat(t *testing.T, uri string, stream bool, got formatReply) {
	t.Helper()
	if got.status != http.StatusOK {
		t.Fatalf("status %d: %s", got.status, got.body)
	}
	switch {
	case strings.HasSuffix(uri, "/chat/completions") && stream:
		checkChatStream(t, got, true)
	case strings.HasSuffix(uri, "/chat/completions"):
		reply := gjson.Parse(got.body)
		if reply.Get("object").String() != "chat.completion" || reply.Get("choices.0.message.content").String() != "ok" {
			t.Fatalf("not a chat completion saying ok: %s", got.body)
		}
	case strings.HasSuffix(uri, "/responses") && stream:
		var text strings.Builder
		completed := false
		for _, e := range sseEvents(got.body) {
			event := gjson.Parse(e.data)
			switch event.Get("type").String() {
			case "response.output_text.delta":
				text.WriteString(event.Get("delta").String())
			case "response.completed":
				completed = true
			}
		}
		if text.String() != "ok" || !completed {
			t.Fatalf("not a Responses stream saying ok and completing: %s", got.body)
		}
	case strings.HasSuffix(uri, "/responses"):
		reply := gjson.Parse(got.body)
		if reply.Get("object").String() != "response" || reply.Get("output.0.content.0.text").String() != "ok" {
			t.Fatalf("not a response saying ok: %s", got.body)
		}
	case strings.HasSuffix(uri, "/messages") && stream:
		var text strings.Builder
		var types []string
		for _, e := range sseEvents(got.body) {
			event := gjson.Parse(e.data)
			types = append(types, event.Get("type").String())
			if event.Get("type").String() == "content_block_delta" {
				text.WriteString(event.Get("delta.text").String())
			}
		}
		if text.String() != "ok" || len(types) == 0 || types[0] != "message_start" || types[len(types)-1] != "message_stop" {
			t.Fatalf("not a Messages stream saying ok from message_start to message_stop: %s", got.body)
		}
	case strings.HasSuffix(uri, "/messages"):
		reply := gjson.Parse(got.body)
		if reply.Get("type").String() != "message" || reply.Get("content.0.text").String() != "ok" {
			t.Fatalf("not a message saying ok: %s", got.body)
		}
	case strings.HasSuffix(uri, "/count_tokens"):
		if gjson.Get(got.body, "input_tokens").Int() != 12 {
			t.Fatalf("not a token count of 12: %s", got.body)
		}
	case strings.HasSuffix(uri, "/converse"):
		if gjson.Get(got.body, "output.message.content.0.text").String() != "ok" {
			t.Fatalf("not a Converse answer saying ok: %s", got.body)
		}
	case strings.HasSuffix(uri, "/v2/chat"):
		if gjson.Get(got.body, "message.content.0.text").String() != "ok" {
			t.Fatalf("not a Cohere chat answer saying ok: %s", got.body)
		}
	default:
		t.Fatalf("no format check for %s", uri)
	}
}

// TestTokunPluginSendsEachTextFormatThroughItsIntegration sends the three text formats and
// count_tokens, each with a signed plan, into the route of its own integration, streamed and not,
// with one candidate in the request's format and one the target provider converts. The attempt in
// the request's format reaches its upstream with the caller's bytes and only the model changed;
// the converted attempt reaches its upstream in the upstream's format; the caller gets its own
// format back either way.
func TestTokunPluginSendsEachTextFormatThroughItsIntegration(t *testing.T) {
	server := newFormatServer(t, true)
	cases := []struct {
		name, uri, body, callerModel string
		stream                       bool
		// enters is the integration and request type core sees.
		enters string
		// same is the entry in the request's format and the API path it reaches; other the entry
		// the target provider converts for, the path it reaches and a field of its format.
		sameEntry, samePath, otherEntry, otherPath, otherFormatField string
	}{
		{"chat completions", "/openai/v1/chat/completions", chatCallerBody, "tk/chat-model", false, "openai chat_completion",
			"up-openai", "/v1/chat/completions", "up-anthropic", "/v1/messages", "max_tokens"},
		{"chat completions, streamed", "/openai/v1/chat/completions", streamed(chatCallerBody, `,"stream_options":{"include_usage":true}`), "tk/chat-model", true, "openai chat_completion_stream",
			"up-openai", "/v1/chat/completions", "up-anthropic", "/v1/messages", "max_tokens"},
		{"responses", "/openai/v1/responses", responsesCallerBody, "tk/responses-model", false, "openai responses",
			"up-openai", "/v1/responses", "up-anthropic", "/v1/messages", "max_tokens"},
		{"responses, streamed", "/openai/v1/responses", streamed(responsesCallerBody, ""), "tk/responses-model", true, "openai responses_stream",
			"up-openai", "/v1/responses", "up-anthropic", "/v1/messages", "max_tokens"},
		{"messages", "/anthropic/v1/messages", messagesCallerBody, "tk/claude", false, "anthropic responses",
			"up-anthropic", "/v1/messages", "up-openai", "/v1/responses", "input"},
		{"messages, streamed", "/anthropic/v1/messages", streamed(messagesCallerBody, ""), "tk/claude", true, "anthropic responses_stream",
			"up-anthropic", "/v1/messages", "up-openai", "/v1/responses", "input"},
		{"count_tokens", "/anthropic/v1/messages/count_tokens", countCallerBody, "tk/claude", false, "anthropic count_tokens",
			"up-anthropic", "/v1/messages/count_tokens", "up-openai", "/v1/responses/input_tokens", "input"},
		// The other two integrations whose attempts in their own format send the caller's body.
		{"bedrock converse", "/bedrock/model/tk-claude/converse", converseCallerBody, "", false, "bedrock responses",
			"up-bedrock", "/model/up-model/converse", "up-openai", "/v1/responses", "input"},
		{"cohere chat", "/cohere/v2/chat", cohereCallerBody, "tk/command", false, "cohere chat_completion",
			"up-cohere", "/v2/chat", "up-anthropic", "/v1/messages", "max_tokens"},
	}
	for i, tc := range cases {
		keyOf := map[string]string{"up-openai": "oa-1", "up-anthropic": "an-1", "up-cohere": "co-1", "up-bedrock": "bk-1"}
		t.Run(tc.name+": the candidate in the request's format", func(t *testing.T) {
			server.upstreams.reset()
			server.observer.take()
			plan := formatPlan("req_same", formatUpstream(tc.sameEntry, keyOf[tc.sameEntry], "up-model", true))
			got := server.send(t, tc.uri, tc.body, plan, fmt.Sprintf("snd_same_%d", i))
			checkCallerFormat(t, tc.uri, tc.stream, got)
			calls := server.upstreams.take()
			if len(calls) != 1 || calls[0].entry != tc.sameEntry || calls[0].path != tc.samePath {
				t.Fatalf("upstream calls %+v, want one to %s%s", calls, tc.sameEntry, tc.samePath)
			}
			if want := forwarded(t, tc.body, tc.callerModel, "up-model"); !bytes.Equal(calls[0].body, want) {
				t.Fatalf("the upstream received\n%s\nwant the caller's bytes with only the model changed\n%s", calls[0].body, want)
			}
			if seen := server.observer.take(); !reflect.DeepEqual(seen, []string{tc.enters}) {
				t.Errorf("core saw %v, want %q", seen, tc.enters)
			}
		})
		t.Run(tc.name+": a candidate the target provider converts for", func(t *testing.T) {
			server.upstreams.reset()
			server.observer.take()
			plan := formatPlan("req_other", formatUpstream(tc.otherEntry, keyOf[tc.otherEntry], "up-model", false))
			got := server.send(t, tc.uri, tc.body, plan, fmt.Sprintf("snd_other_%d", i))
			checkCallerFormat(t, tc.uri, tc.stream, got)
			calls := server.upstreams.take()
			if len(calls) != 1 || calls[0].entry != tc.otherEntry || calls[0].path != tc.otherPath {
				t.Fatalf("upstream calls %+v, want one to %s%s", calls, tc.otherEntry, tc.otherPath)
			}
			upstreamBody := gjson.ParseBytes(calls[0].body)
			if upstreamBody.Get("model").String() != "up-model" || !upstreamBody.Get(tc.otherFormatField).Exists() || upstreamBody.Get("zz_unknown").Exists() {
				t.Fatalf("the upstream received %s, want a converted body with the forward model and %s", calls[0].body, tc.otherFormatField)
			}
			if seen := server.observer.take(); !reflect.DeepEqual(seen, []string{tc.enters}) {
				t.Errorf("core saw %v, want %q", seen, tc.enters)
			}
		})
	}
}

// TestTokunPluginRewritesEachAttempt runs the rewrites of each attempt end to end: every attempt
// starts from the caller's body with its own forward model and its own strip_params, the
// parameters removed are listed in tokun-stripped-params, and a streamed chat completion asks
// its upstream for usage but returns it to the caller only when the caller asked.
func TestTokunPluginRewritesEachAttempt(t *testing.T) {
	server := newFormatServer(t, true)
	const uri = "/openai/v1/chat/completions"

	t.Run("each fallback with its own forward model and strip_params", func(t *testing.T) {
		server.upstreams.reset("oa-1")
		plan := formatPlan("req_fallbacks",
			formatUpstream("up-openai", "oa-1", "first-model", true, "temperature"),
			formatUpstream("up-openai-2", "ob-1", "second-model", true))
		got := server.send(t, uri, chatCallerBody, plan, "snd_fallbacks")
		checkCallerFormat(t, uri, false, got)
		calls := server.upstreams.take()
		if len(calls) != 2 {
			t.Fatalf("upstream calls %+v, want two", calls)
		}
		firstWant := []byte(strings.Replace(string(forwarded(t, chatCallerBody, "tk/chat-model", "first-model")), `"temperature":1,`, "", 1))
		if calls[0].entry != "up-openai" || !bytes.Equal(calls[0].body, firstWant) {
			t.Fatalf("the first attempt sent to %s\n%s\nwant\n%s", calls[0].entry, calls[0].body, firstWant)
		}
		if want := forwarded(t, chatCallerBody, "tk/chat-model", "second-model"); calls[1].entry != "up-openai-2" || !bytes.Equal(calls[1].body, want) {
			t.Fatalf("the second attempt sent to %s\n%s\nwant\n%s", calls[1].entry, calls[1].body, want)
		}
		if got.stripped != "" {
			t.Errorf("tokun-stripped-params = %q, but the attempt that answered removed nothing", got.stripped)
		}
	})

	t.Run("strip_params in the request's format", func(t *testing.T) {
		server.upstreams.reset()
		plan := formatPlan("req_strip_same", formatUpstream("up-openai", "oa-1", "up-model", true, "temperature", "top_p", "zz_unknown.b"))
		got := server.send(t, uri, chatCallerBody, plan, "snd_strip_same")
		checkCallerFormat(t, uri, false, got)
		calls := server.upstreams.take()
		want := `{"model":"up-model" , "messages":[{"role":"user","content":"hi"}],"zz_unknown":{"a":[ ]},"max_completion_tokens":64}`
		if len(calls) != 1 || string(calls[0].body) != want {
			t.Fatalf("upstream calls %+v, want one with\n%s", calls, want)
		}
		if got.stripped != "temperature, zz_unknown.b" {
			t.Errorf("tokun-stripped-params = %q, want the two parameters the body had", got.stripped)
		}
	})

	t.Run("strip_params on a converted attempt", func(t *testing.T) {
		server.upstreams.reset()
		plan := formatPlan("req_kept_other", formatUpstream("up-anthropic", "an-1", "up-model", false))
		checkCallerFormat(t, uri, false, server.send(t, uri, chatCallerBody, plan, "snd_kept_other"))
		calls := server.upstreams.take()
		if len(calls) != 1 || gjson.GetBytes(calls[0].body, "temperature").Float() != 1 {
			t.Fatalf("upstream calls %+v, want the converted body to carry temperature", calls)
		}

		server.upstreams.reset()
		plan = formatPlan("req_strip_other", formatUpstream("up-anthropic", "an-1", "up-model", false, "temperature"))
		got := server.send(t, uri, chatCallerBody, plan, "snd_strip_other")
		checkCallerFormat(t, uri, false, got)
		calls = server.upstreams.take()
		if len(calls) != 1 || calls[0].path != "/v1/messages" || gjson.GetBytes(calls[0].body, "temperature").Exists() || gjson.GetBytes(calls[0].body, "model").String() != "up-model" {
			t.Fatalf("upstream calls %+v, want one converted body without temperature", calls)
		}
		if got.stripped != "temperature" {
			t.Errorf("tokun-stripped-params = %q, want temperature", got.stripped)
		}
	})

	t.Run("strip_params on a converted attempt of a route that reads its path", func(t *testing.T) {
		// The Converse route takes the model from the path: the stripped body is converted with
		// the route's own pre-request callback, as the caller's body was.
		const converse = "/bedrock/model/tk-claude/converse"
		server.upstreams.reset()
		plan := formatPlan("req_kept_converse", formatUpstream("up-openai", "oa-1", "up-model", false))
		checkCallerFormat(t, converse, false, server.send(t, converse, converseCallerBody, plan, "snd_kept_converse"))
		calls := server.upstreams.take()
		if len(calls) != 1 || gjson.GetBytes(calls[0].body, "max_output_tokens").Int() != 64 {
			t.Fatalf("upstream calls %+v, want the converted body to carry max_output_tokens 64", calls)
		}

		server.upstreams.reset()
		plan = formatPlan("req_strip_converse", formatUpstream("up-openai", "oa-1", "up-model", false, "inferenceConfig.maxTokens"))
		got := server.send(t, converse, converseCallerBody, plan, "snd_strip_converse")
		checkCallerFormat(t, converse, false, got)
		calls = server.upstreams.take()
		if len(calls) != 1 || calls[0].path != "/v1/responses" || gjson.GetBytes(calls[0].body, "max_output_tokens").Exists() || gjson.GetBytes(calls[0].body, "model").String() != "up-model" {
			t.Fatalf("upstream calls %+v, want one Responses body without max_output_tokens", calls)
		}
		if got.stripped != "inferenceConfig.maxTokens" {
			t.Errorf("tokun-stripped-params = %q, want inferenceConfig.maxTokens", got.stripped)
		}
	})

	t.Run("an attempt in the request's format after a converted one", func(t *testing.T) {
		server.upstreams.reset("oa-1")
		plan := formatPlan("req_switch",
			formatUpstream("up-openai", "oa-1", "first-model", true),
			formatUpstream("up-anthropic", "an-1", "second-model", false))
		checkCallerFormat(t, uri, false, server.send(t, uri, chatCallerBody, plan, "snd_switch"))
		calls := server.upstreams.take()
		if len(calls) != 2 || calls[0].path != "/v1/chat/completions" || calls[1].path != "/v1/messages" {
			t.Fatalf("upstream calls %+v, want the chat completion and then the converted message", calls)
		}
		if want := forwarded(t, chatCallerBody, "tk/chat-model", "first-model"); !bytes.Equal(calls[0].body, want) {
			t.Fatalf("the first attempt sent\n%s\nwant\n%s", calls[0].body, want)
		}
		if gjson.GetBytes(calls[1].body, "model").String() != "second-model" || gjson.GetBytes(calls[1].body, "zz_unknown").Exists() {
			t.Fatalf("the converted attempt sent %s", calls[1].body)
		}
	})

	for _, tc := range []struct {
		name, body, upstreamBody string
		wantUsage                bool
	}{
		{"a caller that asked for usage", streamed(chatCallerBody, `,"stream_options":{"include_usage":true}`),
			strings.Replace(streamed(chatCallerBody, `,"stream_options":{"include_usage":true}`), "tk/chat-model", "up-model", 1), true},
		{"a caller that said no usage", streamed(chatCallerBody, `,"stream_options":{"include_usage":false}`),
			strings.Replace(streamed(chatCallerBody, `,"stream_options":{"include_usage":true}`), "tk/chat-model", "up-model", 1), false},
		{"a caller that did not ask", streamed(chatCallerBody, ""),
			strings.Replace(streamed(chatCallerBody, `,"stream_options":{"include_usage":true}`), "tk/chat-model", "up-model", 1), false},
	} {
		t.Run("include_usage: "+tc.name, func(t *testing.T) {
			server.upstreams.reset()
			plan := formatPlan("req_usage", formatUpstream("up-openai", "oa-1", "up-model", true))
			got := server.send(t, uri, tc.body, plan, "snd_usage_"+strings.ReplaceAll(tc.name, " ", "_"))
			if got.status != http.StatusOK {
				t.Fatalf("status %d: %s", got.status, got.body)
			}
			checkChatStream(t, got, tc.wantUsage)
			calls := server.upstreams.take()
			if len(calls) != 1 || string(calls[0].body) != tc.upstreamBody {
				t.Fatalf("upstream calls %+v, want one with\n%s", calls, tc.upstreamBody)
			}
		})
	}

	t.Run("include_usage: a converted attempt of a caller that did not ask", func(t *testing.T) {
		server.upstreams.reset()
		plan := formatPlan("req_usage_other", formatUpstream("up-anthropic", "an-1", "up-model", false))
		got := server.send(t, uri, streamed(chatCallerBody, ""), plan, "snd_usage_other")
		if got.status != http.StatusOK {
			t.Fatalf("status %d: %s", got.status, got.body)
		}
		checkChatStream(t, got, false)
	})
}

// The output limits of the requests the output-limit tests send to claude-haiku-4-5, whose known
// output ceiling is 64000 tokens.
const (
	outputLimitModel   = "claude-haiku-4-5"
	outputLimitCeiling = 64000
	// The output limit the plan gives the upstream interface.
	outputLimitInterface = 12345
)

// anthropicOutputLimits reads max_tokens and thinking.budget_tokens of the one call the Anthropic
// upstream received.
func anthropicOutputLimits(t *testing.T, calls []formatCall) (maxTokens, budget int64) {
	t.Helper()
	if len(calls) != 1 || calls[0].entry != "up-anthropic" || calls[0].path != "/v1/messages" {
		t.Fatalf("upstream calls %+v, want one Messages call to up-anthropic", calls)
	}
	return gjson.GetBytes(calls[0].body, "max_tokens").Int(), gjson.GetBytes(calls[0].body, "thinking.budget_tokens").Int()
}

// TestTokunPluginForwardsTheCallersOutputLimits sends requests with a signed plan to
// claude-haiku-4-5 on the Anthropic provider: the caller's max_tokens above the model's output
// ceiling and its thinking budget reach the upstream as written, in the request's format and
// converted, and a converted request that names no output limit gets the plan's
// interface_output_tokens.
func TestTokunPluginForwardsTheCallersOutputLimits(t *testing.T) {
	server := newFormatServer(t, true)
	upstream := func(sameFormat bool) *routingv1.RouteUpstream {
		u := formatUpstream("up-anthropic", "an-1", outputLimitModel, sameFormat)
		u.InterfaceOutputTokens = outputLimitInterface
		return u
	}

	t.Run("Messages above the ceiling, in the request's format", func(t *testing.T) {
		server.upstreams.reset()
		body := `{"model":"tk/claude","max_tokens":128000 ,"thinking":{"type":"enabled","budget_tokens":80000},"messages":[{"role":"user","content":"hi"}]}`
		got := server.send(t, "/anthropic/v1/messages", body, formatPlan("req_limits_messages", upstream(true)), "snd_limits_messages")
		checkCallerFormat(t, "/anthropic/v1/messages", false, got)
		calls := server.upstreams.take()
		anthropicOutputLimits(t, calls)
		if want := forwarded(t, body, "tk/claude", outputLimitModel); !bytes.Equal(calls[0].body, want) {
			t.Fatalf("the upstream received\n%s\nwant the caller's bytes with only the model changed\n%s", calls[0].body, want)
		}
	})

	t.Run("chat completions above the ceiling, converted", func(t *testing.T) {
		server.upstreams.reset()
		body := `{"model":"tk/claude","max_completion_tokens":128000,"reasoning":{"max_tokens":80000},"messages":[{"role":"user","content":"hi"}]}`
		got := server.send(t, "/openai/v1/chat/completions", body, formatPlan("req_limits_chat", upstream(false)), "snd_limits_chat")
		checkCallerFormat(t, "/openai/v1/chat/completions", false, got)
		if maxTokens, budget := anthropicOutputLimits(t, server.upstreams.take()); maxTokens != 128000 || budget != 80000 {
			t.Fatalf("the upstream received max_tokens %d and a thinking budget of %d, want the caller's 128000 and 80000", maxTokens, budget)
		}
	})

	for _, tc := range []struct{ name, uri, body string }{
		{"chat completions", "/openai/v1/chat/completions", `{"model":"tk/claude","messages":[{"role":"user","content":"hi"}]}`},
		{"Responses", "/openai/v1/responses", `{"model":"tk/claude","input":[{"role":"user","content":"hi"}]}`},
	} {
		t.Run(tc.name+" without an output limit, converted", func(t *testing.T) {
			server.upstreams.reset()
			got := server.send(t, tc.uri, tc.body, formatPlan("req_limits_default", upstream(false)), "snd_limits_default_"+strings.ReplaceAll(tc.name, " ", "_"))
			checkCallerFormat(t, tc.uri, false, got)
			if maxTokens, _ := anthropicOutputLimits(t, server.upstreams.take()); maxTokens != outputLimitInterface {
				t.Fatalf("the upstream received max_tokens %d, want the plan's interface_output_tokens %d", maxTokens, outputLimitInterface)
			}
		})
	}
}

// TestBifrostClampsOutputLimitsWithoutTheTokunPlugin sends the converted requests of
// TestTokunPluginForwardsTheCallersOutputLimits to a Bifrost without the tokun plugin: a
// max_tokens above claude-haiku-4-5's output ceiling is lowered to it with the thinking budget
// refitted below it, and a request that names no output limit gets the ceiling.
func TestBifrostClampsOutputLimitsWithoutTheTokunPlugin(t *testing.T) {
	server := newFormatServer(t, false)
	const uri = "/openai/v1/chat/completions"
	model := "up-anthropic/" + outputLimitModel

	t.Run("above the ceiling", func(t *testing.T) {
		server.upstreams.reset()
		body := `{"model":"` + model + `","max_completion_tokens":128000,"reasoning":{"max_tokens":80000},"messages":[{"role":"user","content":"hi"}]}`
		checkCallerFormat(t, uri, false, server.send(t, uri, body, nil, ""))
		maxTokens, budget := anthropicOutputLimits(t, server.upstreams.take())
		if maxTokens != outputLimitCeiling || budget >= outputLimitCeiling {
			t.Fatalf("the upstream received max_tokens %d and a thinking budget of %d, want %d and a budget below it", maxTokens, budget, outputLimitCeiling)
		}
	})

	t.Run("without an output limit", func(t *testing.T) {
		server.upstreams.reset()
		body := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`
		checkCallerFormat(t, uri, false, server.send(t, uri, body, nil, ""))
		if maxTokens, _ := anthropicOutputLimits(t, server.upstreams.take()); maxTokens != outputLimitCeiling {
			t.Fatalf("the upstream received max_tokens %d, want the model's ceiling %d", maxTokens, outputLimitCeiling)
		}
	})
}
