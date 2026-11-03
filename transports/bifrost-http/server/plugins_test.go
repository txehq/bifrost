package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fasthttp/router"
	"github.com/valyala/fasthttp"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/capsohq/bifrost/framework/configstore"
	configstoreTables "github.com/capsohq/bifrost/framework/configstore/tables"
	"github.com/capsohq/bifrost/framework/logstore"
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

// tokunUpstreams stands in for the upstream APIs of three Bifrost provider entries, each
// under its own path. A key listed in failing is answered 500.
type tokunUpstreams struct {
	mu      sync.Mutex
	calls   []upstreamCall
	failing map[string]bool
}

func (u *tokunUpstreams) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	provider, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	body, _ := io.ReadAll(r.Body)
	var request struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &request)
	key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	u.mu.Lock()
	u.calls = append(u.calls, upstreamCall{provider: provider, key: key, model: request.Model, header: r.Header.Clone()})
	failing := u.failing[key]
	u.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if failing {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"upstream failed","type":"server_error"}}`)
		return
	}
	_, _ = io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"`+request.Model+`","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
}

func (u *tokunUpstreams) reset(failing ...string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.calls = nil
	u.failing = map[string]bool{}
	for _, key := range failing {
		u.failing[key] = true
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

// tokunTestPlan lists up-a (keys key-a1, key-a2) and then up-b (key-b1). The Bifrost entries
// also hold key-a3 on up-a and the provider up-x, which the plan does not list.
func tokunTestPlan(requestID string) *routingv1.RoutePlan {
	return &routingv1.RoutePlan{
		RequestId: requestID,
		Providers: []*routingv1.RouteProvider{
			{ProviderId: "provider-a", Score: 0.8, Upstreams: []*routingv1.RouteUpstream{{
				UpstreamId: 1, Interface: "1:text.chat.openai-chat", BifrostProvider: "up-a", ForwardModel: "model-a",
				Keys: []*plannerv1.KeyShare{{KeyId: "key-a1", Share: 1}, {KeyId: "key-a2", Share: 1}},
			}}},
			{ProviderId: "provider-b", Score: 0.7, Upstreams: []*routingv1.RouteUpstream{{
				UpstreamId: 2, Interface: "2:text.chat.openai-chat", BifrostProvider: "up-b", ForwardModel: "model-b",
				Keys: []*plannerv1.KeyShare{{KeyId: "key-b1", Share: 1}},
			}}},
		},
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
		status int
		body   string
	}
	sendTo := func(t *testing.T, method, uri string, headers map[string]string, body string) reply {
		t.Helper()
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
		r.Handler(ctx)
		return reply{status: ctx.Response.StatusCode(), body: string(ctx.Response.Body())}
	}
	send := func(t *testing.T, headers map[string]string, body string) reply {
		t.Helper()
		return sendTo(t, fasthttp.MethodPost, "/openai/v1/chat/completions", headers, body)
	}
	signed := func(t *testing.T, signer planSigner, plan *routingv1.RoutePlan, sendID string) map[string]string {
		planHeader, signatureHeader := signer.headers(t, plan, sendID, len(body), time.Now().Add(time.Minute))
		return map[string]string{tokun.HeaderRoutePlan: planHeader, tokun.HeaderRoutePlanSignature: signatureHeader}
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

	t.Run("fallbacks in plan order, one key each", func(t *testing.T) {
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
