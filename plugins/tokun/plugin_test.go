package tokun

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/schemas"

	plannerv1 "github.com/capsohq/txe/tokun/proto/gen/go/tokun/planner/v1"
	routingv1 "github.com/capsohq/txe/tokun/proto/gen/go/tokun/routing/v1"
)

// signing describes one signed route plan the way the gateway produces it; a test changes
// one field to break one check.
type signing struct {
	keyID     string
	secret    string
	plan      *routingv1.RoutePlan
	requestID string
	sendID    string
	expiresAt *timestamppb.Timestamp
	bodyBytes int64
	// planSHA256 is the signed hash; nil signs the hash of the plan bytes.
	planSHA256 []byte
}

// headers returns the tk-route-plan and tk-route-plan-sig values.
func (s signing) headers(t testing.TB) (string, string) {
	t.Helper()
	planBytes, err := proto.Marshal(s.plan)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	planSHA256 := s.planSHA256
	if planSHA256 == nil {
		sum := sha256.Sum256(planBytes)
		planSHA256 = sum[:]
	}
	claims, err := proto.Marshal(&routingv1.RoutePlanClaims{
		KeyId:      s.keyID,
		RequestId:  s.requestID,
		SendId:     s.sendID,
		ExpiresAt:  s.expiresAt,
		BodyBytes:  s.bodyBytes,
		PlanSha256: planSHA256,
	})
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	mac := hmac.New(sha256.New, []byte(s.secret))
	mac.Write(claims)
	signature, err := proto.Marshal(&routingv1.RoutePlanSignature{Claims: claims, Mac: mac.Sum(nil)})
	if err != nil {
		t.Fatalf("marshal signature: %v", err)
	}
	return base64.StdEncoding.EncodeToString(planBytes), base64.StdEncoding.EncodeToString(signature)
}

const (
	testRequestID = "req_01JC9B2K7XQ4"
	testBody      = `{"model":"caller/model","messages":[{"role":"user","content":"hi"}]}`
)

// testPlan has two providers: the first with one upstream and two keys, the second with one
// upstream and one key.
func testPlan() *routingv1.RoutePlan {
	return &routingv1.RoutePlan{
		RequestId:         testRequestID,
		RequestRate:       200,
		StaleAfterSeconds: 30,
		Threshold:         0.5,
		Providers: []*routingv1.RouteProvider{
			{ProviderId: "nimbus-cloud", Score: 0.82, Uncertainty: 0.02, Upstreams: []*routingv1.RouteUpstream{{
				UpstreamId: 31, Interface: "48213:text.chat.openai-chat", BifrostProvider: "up-a", ForwardModel: "model-a",
				Keys: []*plannerv1.KeyShare{{KeyId: "key-a1", Share: 0.33}, {KeyId: "key-a2", Share: 0.67}},
			}}},
			{ProviderId: "anthropic", Score: 0.74, Uncertainty: 0.03, Upstreams: []*routingv1.RouteUpstream{{
				UpstreamId: 12, Interface: "51007:text.chat.openai-chat", BifrostProvider: "up-b", ForwardModel: "model-b",
				Keys: []*plannerv1.KeyShare{{KeyId: "key-b1", Share: 1}},
			}}},
		},
	}
}

func validSigning(sendID string) signing {
	return signing{
		keyID:     "gw-2026-11",
		secret:    "gateway-secret",
		plan:      testPlan(),
		requestID: testRequestID,
		sendID:    sendID,
		expiresAt: timestamppb.New(time.Now().Add(time.Minute)),
		bodyBytes: int64(len(testBody)),
	}
}

func testPlugin(t testing.TB, keys map[string]string) *Plugin {
	t.Helper()
	encoded, err := json.Marshal(keys)
	if err != nil {
		t.Fatalf("encode keys: %v", err)
	}
	plugin, err := Init(&Config{SigningKeys: schemas.NewSecretVar(string(encoded))}, bifrost.NewNoOpLogger())
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	return plugin
}

func TestVerifyAcceptsASignedPlan(t *testing.T) {
	plugin := testPlugin(t, map[string]string{"gw-2026-11": "gateway-secret"})
	planHeader, signatureHeader := validSigning("snd_ok").headers(t)

	verified, refused := plugin.verify(planHeader, signatureHeader, len(testBody), time.Now())
	if refused != nil {
		t.Fatalf("refused a valid plan: %s: %s", refused.reason, refused.detail)
	}
	if !proto.Equal(verified.plan, testPlan()) {
		t.Fatalf("verified plan = %v, want the signed one", verified.plan)
	}
	if verified.claims.GetSendId() != "snd_ok" || verified.claims.GetRequestId() != testRequestID {
		t.Fatalf("claims = %v", verified.claims)
	}
}

// Every check refuses on its own: each case breaks exactly one thing of a valid signing.
func TestVerifyRefusesEachBrokenCheck(t *testing.T) {
	otherPlan := testPlan()
	otherPlan.Providers = otherPlan.Providers[:1]
	otherPlanBytes, err := proto.Marshal(otherPlan)
	if err != nil {
		t.Fatal(err)
	}
	otherPlanSHA := sha256.Sum256(otherPlanBytes)
	emptyPlan := &routingv1.RoutePlan{RequestId: testRequestID, Providers: []*routingv1.RouteProvider{{ProviderId: "p", Upstreams: []*routingv1.RouteUpstream{{UpstreamId: 1}}}}}

	cases := []struct {
		name      string
		change    func(*signing)
		planValue func(planHeader string) string
		sigValue  func(signatureHeader string) string
		bodyBytes int
		want      rejection
	}{
		{name: "no plan header", planValue: func(string) string { return "" }, want: rejectMissing},
		{name: "no signature header", sigValue: func(string) string { return "" }, want: rejectMissing},
		{name: "plan header not base64", planValue: func(string) string { return "not base64!" }, want: rejectMalformed},
		{name: "signature header unpadded base64", sigValue: func(s string) string { return s[:len(s)-1] }, want: rejectMalformed},
		{name: "unknown signing key", change: func(s *signing) { s.keyID = "gw-2025-01" }, want: rejectUnknownKey},
		{name: "mac from another secret", change: func(s *signing) { s.secret = "not-the-gateway-secret" }, want: rejectSignature},
		{name: "expired", change: func(s *signing) { s.expiresAt = timestamppb.New(time.Now().Add(-time.Second)) }, want: rejectExpired},
		{name: "no expiry", change: func(s *signing) { s.expiresAt = nil }, want: rejectExpired},
		{name: "signed hash of another plan", change: func(s *signing) { s.planSHA256 = otherPlanSHA[:] }, want: rejectPlanHash},
		{name: "signed another request_id", change: func(s *signing) { s.requestID = "req_other" }, want: rejectRequestID},
		{name: "body shorter than signed", bodyBytes: len(testBody) - 1, want: rejectBodyBytes},
		{name: "body longer than signed", bodyBytes: len(testBody) + 1, want: rejectBodyBytes},
		{name: "plan lists no key", change: func(s *signing) { s.plan = emptyPlan }, want: rejectEmptyPlan},
		{name: "no send_id", change: func(s *signing) { s.sendID = "" }, want: rejectMalformed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plugin := testPlugin(t, map[string]string{"gw-2026-11": "gateway-secret"})
			s := validSigning("snd_" + tc.name)
			if tc.change != nil {
				tc.change(&s)
			}
			planHeader, signatureHeader := s.headers(t)
			if tc.planValue != nil {
				planHeader = tc.planValue(planHeader)
			}
			if tc.sigValue != nil {
				signatureHeader = tc.sigValue(signatureHeader)
			}
			bodyBytes := len(testBody)
			if tc.bodyBytes != 0 {
				bodyBytes = tc.bodyBytes
			}
			_, refused := plugin.verify(planHeader, signatureHeader, bodyBytes, time.Now())
			if refused == nil {
				t.Fatalf("accepted, want %s", tc.want)
			}
			if refused.reason != tc.want {
				t.Fatalf("refused with %s (%s), want %s", refused.reason, refused.detail, tc.want)
			}
			if plugin.sendIDs.size() != 0 {
				t.Fatalf("a refused plan used up its send_id")
			}
		})
	}
}

func TestVerifyRefusesASendIDSeenOnThisPod(t *testing.T) {
	plugin := testPlugin(t, map[string]string{"gw-2026-11": "gateway-secret"})
	planHeader, signatureHeader := validSigning("snd_once").headers(t)
	if _, refused := plugin.verify(planHeader, signatureHeader, len(testBody), time.Now()); refused != nil {
		t.Fatalf("first use refused: %s: %s", refused.reason, refused.detail)
	}
	_, refused := plugin.verify(planHeader, signatureHeader, len(testBody), time.Now())
	if refused == nil || refused.reason != rejectSendIDReused {
		t.Fatalf("second use = %v, want %s", refused, rejectSendIDReused)
	}
	planHeader, signatureHeader = validSigning("snd_next").headers(t)
	if _, refused := plugin.verify(planHeader, signatureHeader, len(testBody), time.Now()); refused != nil {
		t.Fatalf("a new send_id refused: %s: %s", refused.reason, refused.detail)
	}
}

// During a rotation every configured key id verifies; a key id that was removed does not.
func TestVerifyAcceptsEveryConfiguredSigningKey(t *testing.T) {
	plugin := testPlugin(t, map[string]string{"gw-2026-10": "old-secret", "gw-2026-11": "new-secret"})
	for _, key := range []struct{ id, secret string }{{"gw-2026-10", "old-secret"}, {"gw-2026-11", "new-secret"}} {
		s := validSigning("snd_" + key.id)
		s.keyID, s.secret = key.id, key.secret
		planHeader, signatureHeader := s.headers(t)
		if _, refused := plugin.verify(planHeader, signatureHeader, len(testBody), time.Now()); refused != nil {
			t.Fatalf("key %s refused: %s: %s", key.id, refused.reason, refused.detail)
		}
	}

	rotated := testPlugin(t, map[string]string{"gw-2026-11": "new-secret"})
	s := validSigning("snd_after_rotation")
	s.keyID, s.secret = "gw-2026-10", "old-secret"
	planHeader, signatureHeader := s.headers(t)
	if _, refused := rotated.verify(planHeader, signatureHeader, len(testBody), time.Now()); refused == nil || refused.reason != rejectUnknownKey {
		t.Fatalf("removed key = %v, want %s", refused, rejectUnknownKey)
	}
}

func TestSendIDLedgerForgetsAnExpiredSend(t *testing.T) {
	ledger := newSendIDLedger()
	t0 := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	if !ledger.claim("snd_a", t0.Add(time.Minute), t0) {
		t.Fatal("first claim refused")
	}
	if !ledger.claim("snd_b", t0.Add(2*time.Minute), t0) {
		t.Fatal("another send refused")
	}
	if ledger.claim("snd_a", t0.Add(time.Minute), t0.Add(59*time.Second)) {
		t.Fatal("a send was accepted twice before it expired")
	}
	if !ledger.claim("snd_c", t0.Add(3*time.Minute), t0.Add(time.Minute)) {
		t.Fatal("claim refused")
	}
	if got := ledger.size(); got != 2 {
		t.Fatalf("ledger holds %d sends after snd_a expired, want 2", got)
	}
	if !ledger.claim("snd_a", t0.Add(4*time.Minute), t0.Add(time.Minute)) {
		t.Fatal("an expired send_id is still refused")
	}
}

func TestPreAuthHookRemovesThePlanAndTheCallersRouting(t *testing.T) {
	plugin := testPlugin(t, map[string]string{"gw-2026-11": "gateway-secret"})
	planHeader, signatureHeader := validSigning("snd_headers").headers(t)
	req := &schemas.HTTPRequest{
		Method: http.MethodPost,
		Path:   "/v1/chat/completions",
		Route:  "/v1/chat/completions",
		Headers: map[string]string{
			"Tk-Route-Plan":         planHeader,
			"tk-route-plan-sig":     signatureHeader,
			"X-Bf-Api-Key-Id":       "caller-key",
			"x-bf-api-key":          "caller-key-name",
			"X-Bf-Direct-Key":       "true",
			"X-Bf-Session-Affinity": "on",
			"Content-Type":          "application/json",
		},
		Body: []byte(testBody),
	}
	ctx := schemas.NewBifrostContext(t.Context(), schemas.NoDeadline)

	resp, err := plugin.HTTPTransportPreAuthHook(ctx, req)
	if err != nil || resp != nil {
		t.Fatalf("hook = %v, %v; want the request to continue", resp, err)
	}
	if want := map[string]string{"Content-Type": "application/json"}; !reflect.DeepEqual(req.Headers, want) {
		t.Fatalf("headers after the hook = %v, want %v", req.Headers, want)
	}
	if got := ctx.Value(schemas.BifrostContextKeyRequestID); got != "snd_headers" {
		t.Fatalf("request id = %v, want the send_id", got)
	}
	if got := ctx.Value(schemas.BifrostContextKeySessionAffinity); got != false {
		t.Fatalf("session affinity = %v, want false", got)
	}
	state, ok := ctx.Value(requestStateKey).(*requestState)
	if !ok || !proto.Equal(state.plan, testPlan()) {
		t.Fatalf("request state = %v", ctx.Value(requestStateKey))
	}
}

func TestPreAuthHookAnswers401(t *testing.T) {
	plugin := testPlugin(t, map[string]string{"gw-2026-11": "gateway-secret"})
	planHeader, signatureHeader := validSigning("snd_401").headers(t)
	req := &schemas.HTTPRequest{
		Method:  http.MethodPost,
		Path:    "/v1/chat/completions",
		Route:   "/v1/chat/completions",
		Headers: map[string]string{HeaderRoutePlan: planHeader, HeaderRoutePlanSignature: signatureHeader},
		Body:    []byte(testBody + " "),
	}
	ctx := schemas.NewBifrostContext(t.Context(), schemas.NoDeadline)
	resp, err := plugin.HTTPTransportPreAuthHook(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("response = %v, want 401", resp)
	}
	var body rejectionBody
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		t.Fatalf("401 body %s: %v", resp.Body, err)
	}
	if body.Error.Type != rejectionType || body.Error.Code != rejectBodyBytes {
		t.Fatalf("401 body = %+v", body)
	}
	if ctx.Value(requestStateKey) != nil {
		t.Fatal("a refused request carries a request state")
	}
}

// The route is refused before the plan is read: a valid plan does not open a route tokun does not
// offer, and the refusal does not use up its send_id.
func TestPreAuthHookRefusesByRoute(t *testing.T) {
	for _, tc := range []struct {
		name, method, route string
		status              int
		code                rejection
	}{
		{"WebSocket Responses", http.MethodGet, "/v1/responses", http.StatusNotFound, rejectNotOffered},
		{"live session", http.MethodPost, "/openai/v1/live/sessions", http.StatusNotFound, rejectNotOffered},
		{"MCP", http.MethodPost, "/mcp", http.StatusNotFound, rejectNotOffered},
		{"Agent Gateway push callback", http.MethodPost, "/agents/a2a/{name}/push/callback", http.StatusNotFound, rejectNotOffered},
		{"realtime WebSocket", http.MethodGet, "/v1/realtime", http.StatusNotImplemented, rejectNotImplemented},
		{"realtime client secret", http.MethodPost, "/openai/v1/realtime/client_secrets", http.StatusNotImplemented, rejectNotImplemented},
		{"a route the plugin does not know", http.MethodPost, "/v1/unknown", http.StatusInternalServerError, rejectUnclassified},
		{"no route recorded", http.MethodPost, "", http.StatusInternalServerError, rejectUnclassified},
		{"a known template under another method", http.MethodPut, "/v1/chat/completions", http.StatusInternalServerError, rejectUnclassified},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plugin := testPlugin(t, map[string]string{"gw-2026-11": "gateway-secret"})
			planHeader, signatureHeader := validSigning("snd_route").headers(t)
			req := &schemas.HTTPRequest{
				Method:  tc.method,
				Path:    "/requested/path",
				Route:   tc.route,
				Headers: map[string]string{HeaderRoutePlan: planHeader, HeaderRoutePlanSignature: signatureHeader},
				Body:    []byte(testBody),
			}
			ctx := schemas.NewBifrostContext(t.Context(), schemas.NoDeadline)
			resp, err := plugin.HTTPTransportPreAuthHook(ctx, req)
			if err != nil || resp == nil || resp.StatusCode != tc.status {
				t.Fatalf("hook = %+v, %v; want %d", resp, err, tc.status)
			}
			var body rejectionBody
			if err := json.Unmarshal(resp.Body, &body); err != nil {
				t.Fatalf("body %s: %v", resp.Body, err)
			}
			if body.Error.Type != routeRejectionType || body.Error.Code != tc.code || body.Error.Message == "" {
				t.Fatalf("body = %+v, want type %s, code %s", body, routeRejectionType, tc.code)
			}
			if ctx.Value(requestStateKey) != nil || plugin.sendIDs.size() != 0 {
				t.Fatal("a refused route read the plan")
			}
		})
	}
}

// Every class the table lists is reachable, and the realtime routes are the only 501s.
func TestRouteClasses(t *testing.T) {
	counts := map[string]int{}
	for _, entry := range ClassifiedRoutes() {
		method, template, _ := strings.Cut(entry, " ")
		class := RouteClass(method, template)
		if class == "" {
			t.Fatalf("%s is listed without a class", entry)
		}
		counts[class]++
		if (class == "not_implemented") != strings.Contains(template, "/realtime") {
			t.Errorf("%s is %s", entry, class)
		}
	}
	if counts["plan"] == 0 || counts["not_offered"] == 0 || counts["not_implemented"] == 0 {
		t.Fatalf("classes = %v", counts)
	}
	if got := RouteClass("PUT", "/v1/chat/completions"); got != "" {
		t.Fatalf("an unlisted method of a listed template is %q", got)
	}
}

func TestRouteDispatchesOnlyInsideThePlan(t *testing.T) {
	plugin := testPlugin(t, map[string]string{"gw-2026-11": "gateway-secret"})
	newRequest := func() *schemas.BifrostRequest {
		return &schemas.BifrostRequest{RequestType: schemas.ChatCompletionRequest, ChatRequest: &schemas.BifrostChatRequest{
			Provider:  "caller-provider",
			Model:     "caller-model",
			Fallbacks: []schemas.Fallback{{Provider: "caller-fallback", Model: "m", KeyID: "caller-key"}},
		}}
	}

	// The order the same seed draws for plan, as Bifrost targets.
	drawnTargets := func(plan *routingv1.RoutePlan, seed uint64) []schemas.Fallback {
		var targets []schemas.Fallback
		for _, e := range drawOrder(plan, seeded(seed)) {
			targets = append(targets, schemas.Fallback{Provider: schemas.ModelProvider(e.upstream.GetBifrostProvider()), Model: e.upstream.GetForwardModel(), KeyID: e.key.GetKeyId()})
		}
		return targets
	}

	t.Run("every entry in drawn order", func(t *testing.T) {
		// Seeds that draw each of the plan's entries as the primary.
		primaries := map[string]bool{}
		for seed := uint64(1); seed <= 256; seed++ {
			plugin.SeedDraws(seed)
			state := &requestState{plan: testPlan()}
			ctx := schemas.NewBifrostContext(t.Context(), schemas.NoDeadline)
			req := newRequest()
			plugin.route(ctx, req, state)

			want := drawnTargets(testPlan(), seed)
			provider, model, fallbacks := req.GetRequestFields()
			if provider != want[0].Provider || model != want[0].Model {
				t.Fatalf("seed %d: primary = %s/%s, want %s/%s", seed, provider, model, want[0].Provider, want[0].Model)
			}
			if got := ctx.Value(schemas.BifrostContextKeyRoutingPinnedAPIKeyID); got != want[0].KeyID {
				t.Fatalf("seed %d: pinned key = %v, want %s", seed, got, want[0].KeyID)
			}
			if !reflect.DeepEqual(fallbacks, want[1:]) {
				t.Fatalf("seed %d: fallbacks = %+v, want %+v", seed, fallbacks, want[1:])
			}
			if !reflect.DeepEqual(state.chain, []int{0, 1, 2}) || len(state.order) != 3 || len(state.skipped) != 0 {
				t.Fatalf("seed %d: chain %v, order %d entries, skipped %v", seed, state.chain, len(state.order), state.skipped)
			}
			primaries[want[0].KeyID] = true
		}
		if len(primaries) != 3 {
			t.Fatalf("256 seeds drew %v as primaries, want each of the three keys", primaries)
		}
	})

	t.Run("at most max_attempts entries", func(t *testing.T) {
		plan := testPlan()
		plan.MaxAttempts = 2
		plugin.SeedDraws(7)
		state := &requestState{plan: plan}
		ctx := schemas.NewBifrostContext(t.Context(), schemas.NoDeadline)
		req := newRequest()
		plugin.route(ctx, req, state)
		_, _, fallbacks := req.GetRequestFields()
		if want := drawnTargets(plan, 7)[1:2]; !reflect.DeepEqual(fallbacks, want) {
			t.Fatalf("fallbacks = %+v, want %+v", fallbacks, want)
		}
		if !reflect.DeepEqual(state.chain, []int{0, 1}) {
			t.Fatalf("chain = %v", state.chain)
		}
	})
}

// A send none of whose entries this Bifrost can send fails before any upstream is called.
func TestPreLLMHookFailsASendWithNothingToSend(t *testing.T) {
	plugin := testPlugin(t, map[string]string{"gw-2026-11": "gateway-secret"})
	state := &requestState{plan: testPlan(), order: drawOrder(testPlan(), seeded(1))}
	ctx := schemas.NewBifrostContext(t.Context(), schemas.NoDeadline)
	ctx.SetValue(requestStateKey, state)
	_, shortCircuit, err := plugin.PreLLMHook(ctx, &schemas.BifrostRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if shortCircuit == nil || shortCircuit.Error == nil {
		t.Fatalf("short circuit = %v, want an error", shortCircuit)
	}
	if *shortCircuit.Error.StatusCode != http.StatusServiceUnavailable || *shortCircuit.Error.AllowFallbacks {
		t.Fatalf("error = %+v", shortCircuit.Error)
	}
}

// A send that reaches an attempt without PreRequestHook applying its plan (a route that
// dispatches by its own choice) fails before any upstream is called.
func TestPreLLMHookFailsASendWhosePlanWasNotApplied(t *testing.T) {
	plugin := testPlugin(t, map[string]string{"gw-2026-11": "gateway-secret"})
	ctx := schemas.NewBifrostContext(t.Context(), schemas.NoDeadline)
	ctx.SetValue(requestStateKey, &requestState{plan: testPlan()})
	_, shortCircuit, err := plugin.PreLLMHook(ctx, &schemas.BifrostRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if shortCircuit == nil || shortCircuit.Error == nil || *shortCircuit.Error.Type != "tokun_route_plan_not_applied" {
		t.Fatalf("short circuit = %+v, want tokun_route_plan_not_applied", shortCircuit)
	}
	if *shortCircuit.Error.AllowFallbacks {
		t.Fatal("the failure allows fallbacks")
	}
}

// routedState is the state of a send of testPlan after PreRequestHook: three entries sent.
func routedState(t testing.TB, plugin *Plugin) *requestState {
	t.Helper()
	state := &requestState{plan: testPlan()}
	req := &schemas.BifrostRequest{RequestType: schemas.ChatCompletionRequest, ChatRequest: &schemas.BifrostChatRequest{}}
	plugin.route(schemas.NewBifrostContext(context.Background(), schemas.NoDeadline), req, state)
	if len(state.chain) != 3 {
		t.Fatalf("chain = %v, want three entries", state.chain)
	}
	return state
}

// The attempt index core sets picks the entry; an absent index is the primary; an index of
// another type or outside the entries sent fails the attempt in both hooks.
func TestAttemptIndex(t *testing.T) {
	plugin := testPlugin(t, map[string]string{"gw-2026-11": "gateway-secret"})
	state := routedState(t, plugin)

	for _, tc := range []struct {
		name  string
		index any // nil leaves the index unset
		want  int
	}{
		{name: "unset is the primary", index: nil, want: 0},
		{name: "primary", index: 0, want: 0},
		{name: "first fallback", index: 1, want: 1},
		{name: "last fallback", index: 2, want: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := schemas.NewBifrostContext(t.Context(), schemas.NoDeadline)
			if tc.index != nil {
				ctx.SetValue(schemas.BifrostContextKeyFallbackIndex, tc.index)
			}
			got, fault := attemptIndex(ctx, state)
			if fault != nil || got != tc.want {
				t.Fatalf("attemptIndex = %d, %+v; want %d", got, fault, tc.want)
			}
			ctx.SetValue(requestStateKey, state)
			if _, shortCircuit, err := plugin.PreLLMHook(ctx, &schemas.BifrostRequest{}); shortCircuit != nil || err != nil {
				t.Fatalf("PreLLMHook = %+v, %v; want the attempt to go ahead", shortCircuit, err)
			}
			resp := &schemas.BifrostResponse{}
			if gotResp, gotErr, err := plugin.PostLLMHook(ctx, resp, nil); gotResp != resp || gotErr != nil || err != nil {
				t.Fatalf("PostLLMHook = %v, %+v, %v; want the response unchanged", gotResp, gotErr, err)
			}
		})
	}

	for _, tc := range []struct {
		name  string
		index any
	}{
		{name: "int64", index: int64(1)},
		{name: "string", index: "1"},
		{name: "negative", index: -1},
		{name: "past the entries sent", index: 3},
	} {
		t.Run("fails: "+tc.name, func(t *testing.T) {
			ctx := schemas.NewBifrostContext(t.Context(), schemas.NoDeadline)
			ctx.SetValue(schemas.BifrostContextKeyFallbackIndex, tc.index)
			if _, fault := attemptIndex(ctx, state); fault == nil {
				t.Fatalf("attemptIndex accepted %#v", tc.index)
			}
			ctx.SetValue(requestStateKey, state)
			_, shortCircuit, err := plugin.PreLLMHook(ctx, &schemas.BifrostRequest{})
			if err != nil || shortCircuit == nil || shortCircuit.Error == nil {
				t.Fatalf("PreLLMHook = %+v, %v; want the attempt failed", shortCircuit, err)
			}
			if *shortCircuit.Error.Type != "tokun_attempt_index_invalid" || *shortCircuit.Error.StatusCode != http.StatusInternalServerError || *shortCircuit.Error.AllowFallbacks {
				t.Fatalf("PreLLMHook error = %+v", shortCircuit.Error)
			}
			gotResp, gotErr, err := plugin.PostLLMHook(ctx, &schemas.BifrostResponse{}, nil)
			if err != nil || gotResp != nil || gotErr == nil || *gotErr.Type != "tokun_attempt_index_invalid" {
				t.Fatalf("PostLLMHook = %v, %+v, %v; want the response replaced by the failure", gotResp, gotErr, err)
			}
		})
	}
}

// BenchmarkPreAuthHook measures verifying and removing a signed plan of two providers and three
// keys: two base64 decodes, three protobuf decodes, one HMAC and one SHA-256 of the plan.
func BenchmarkPreAuthHook(b *testing.B) {
	plugin := testPlugin(b, map[string]string{"gw-2026-11": "gateway-secret"})
	type headers struct{ plan, signature string }
	signed := make([]headers, b.N)
	for i := range signed {
		signed[i].plan, signed[i].signature = validSigning("snd_bench_" + strconv.Itoa(i)).headers(b)
	}
	body := []byte(testBody)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// The transport hands the hook a pooled request, as here.
		req := schemas.AcquireHTTPRequest()
		req.Method, req.Path, req.Route, req.Body = http.MethodPost, "/v1/chat/completions", "/v1/chat/completions", body
		req.Headers[HeaderRoutePlan] = signed[i].plan
		req.Headers[HeaderRoutePlanSignature] = signed[i].signature
		ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
		if resp, err := plugin.HTTPTransportPreAuthHook(ctx, req); resp != nil || err != nil {
			b.Fatalf("refused: %v %v", resp, err)
		}
		schemas.ReleaseHTTPRequest(req)
	}
}

// benchPlan is a plan of 10 providers, each with 5 upstreams of 3 keys: the upstreams alternate
// between the request's format and a converted one, the last two providers and each provider's
// last upstream score below their thresholds, and one upstream in five has no registered limit.
func benchPlan() *routingv1.RoutePlan {
	plan := &routingv1.RoutePlan{RequestId: testRequestID, RequestRate: 200, StaleAfterSeconds: 30, Threshold: 0.6}
	for p := 0; p < 10; p++ {
		provider := &routingv1.RouteProvider{
			ProviderId:        fmt.Sprintf("provider-%d", p),
			Score:             0.9 - 0.04*float64(p),
			Uncertainty:       0.02 + 0.005*float64(p),
			UpstreamThreshold: 0.7,
		}
		for u := 0; u < 5; u++ {
			upstream := &routingv1.RouteUpstream{
				UpstreamId:      int64(100*p + u),
				Interface:       fmt.Sprintf("%d:text.chat.openai-chat", 100*p+u),
				BifrostProvider: fmt.Sprintf("up-%d-%d", p, u),
				ForwardModel:    "model",
				Score:           0.85 - 0.05*float64(u),
				Uncertainty:     0.03,
				Capacity:        40 + 10*float64(u),
				SameFormat:      u%2 == 0,
			}
			if u == 3 {
				upstream.Capacity = 0
			}
			for k := 0; k < 3; k++ {
				upstream.Keys = append(upstream.Keys, &plannerv1.KeyShare{KeyId: fmt.Sprintf("key-%d-%d-%d", p, u, k), Share: 0.2 + 0.1*float64(k)})
			}
			provider.Upstreams = append(provider.Upstreams, upstream)
		}
		plan.Providers = append(plan.Providers, provider)
	}
	return plan
}

// BenchmarkDrawOrder measures the selection curve on benchPlan: 10 providers, 50 upstreams and
// 150 keys, drawn from the global source as in production.
func BenchmarkDrawOrder(b *testing.B) {
	plan := benchPlan()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if order := drawOrder(plan, rand.Float64); len(order) != 150 {
			b.Fatalf("drew %d entries, want 150", len(order))
		}
	}
}

// BenchmarkPreRequestHook measures applying a plan of two providers and three keys to a request,
// and passing over a request that carries no plan.
func BenchmarkPreRequestHook(b *testing.B) {
	plugin := testPlugin(b, map[string]string{"gw-2026-11": "gateway-secret"})
	plan := testPlan()
	req := &schemas.BifrostRequest{RequestType: schemas.ChatCompletionRequest, ChatRequest: &schemas.BifrostChatRequest{}}
	b.Run("with plan", func(b *testing.B) {
		ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
		state := &requestState{plan: plan}
		ctx.SetValue(requestStateKey, state)
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			// A new send's state: the order, chain and fallbacks are built for every request.
			state.order, state.chain, state.skipped = nil, nil, nil
			req.ChatRequest.Provider, req.ChatRequest.Model, req.ChatRequest.Fallbacks = "caller-provider", "caller-model", nil
			if err := plugin.PreRequestHook(ctx, req); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("without plan", func(b *testing.B) {
		ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if err := plugin.PreRequestHook(ctx, req); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func TestStripParams(t *testing.T) {
	body := []byte(`{"model":"m", "temperature":1,"top_p":1,"tools":[{"type":"function"}],"extra":{"a":1,"b":2}}`)
	original := string(body)
	got, stripped, err := stripParams(body, []string{"top_p", "absent", "extra.b", "temperature"})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"model":"m","tools":[{"type":"function"}],"extra":{"a":1}}`; string(got) != want {
		t.Fatalf("stripped body = %s, want %s", got, want)
	}
	if !reflect.DeepEqual(stripped, []string{"top_p", "extra.b", "temperature"}) {
		t.Fatalf("stripped = %v, want the present paths in the plan's order", stripped)
	}
	if string(body) != original {
		t.Fatalf("the caller's body changed: %s", body)
	}

	got, stripped, err = stripParams(body, []string{"absent"})
	if err != nil || stripped != nil || &got[0] != &body[0] {
		t.Fatalf("a body with nothing to strip = %s, %v, %v; want the same bytes", got, stripped, err)
	}
}

func TestAskForStreamUsage(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{`{"stream":true}`, `{"stream":true,"stream_options":{"include_usage":true}}`},
		{`{"stream":true,"stream_options":{"include_usage":false,"x":1}}`, `{"stream":true,"stream_options":{"include_usage":true,"x":1}}`},
		{`{"stream":true,"stream_options":{"x":1}}`, `{"stream":true,"stream_options":{"x":1,"include_usage":true}}`},
		{`{"stream":true,"stream_options":null}`, `{"stream":true,"stream_options":{"include_usage":true}}`},
	} {
		got, err := askForStreamUsage([]byte(tc.body))
		if err != nil || string(got) != tc.want {
			t.Errorf("askForStreamUsage(%s) = %s, %v; want %s", tc.body, got, err, tc.want)
		}
	}
}

func TestRequestStateCapture(t *testing.T) {
	for _, tc := range []struct {
		name, integration, body string
		requestType             schemas.RequestType
		openAIStream, dropUsage bool
	}{
		{"openai stream without usage", "openai", `{"stream":true}`, schemas.ChatCompletionStreamRequest, true, true},
		{"openai stream that asked for usage", "openai", `{"stream":true,"stream_options":{"include_usage":true}}`, schemas.ChatCompletionStreamRequest, true, false},
		{"openai completions stream", "openai", `{"stream":true}`, schemas.TextCompletionStreamRequest, true, true},
		{"openai without a stream", "openai", `{}`, schemas.ChatCompletionRequest, false, false},
		{"openai responses stream", "openai", `{"stream":true}`, schemas.ResponsesStreamRequest, false, false},
		{"anthropic stream", "anthropic", `{"stream":true}`, schemas.ResponsesStreamRequest, false, false},
		{"openai stream without a kept body", "openai", ``, schemas.ChatCompletionStreamRequest, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := schemas.NewBifrostContext(t.Context(), schemas.NoDeadline)
			ctx.SetValue(schemas.BifrostContextKeyIntegrationType, tc.integration)
			req := &schemas.BifrostRequest{RequestType: tc.requestType}
			switch tc.requestType {
			case schemas.TextCompletionStreamRequest:
				req.TextCompletionRequest = &schemas.BifrostTextCompletionRequest{}
			case schemas.ChatCompletionRequest, schemas.ChatCompletionStreamRequest:
				req.ChatRequest = &schemas.BifrostChatRequest{}
			default:
				req.ResponsesRequest = &schemas.BifrostResponsesRequest{}
			}
			if tc.body != "" {
				req.SetRawRequestBody([]byte(tc.body))
			}
			state := &requestState{}
			state.capture(ctx, req)
			if state.openAIStream != tc.openAIStream || state.dropUsage != tc.dropUsage {
				t.Fatalf("openAIStream %v dropUsage %v, want %v %v", state.openAIStream, state.dropUsage, tc.openAIStream, tc.dropUsage)
			}
		})
	}
}

// Every attempt is rewritten from the caller's body: the request the attempt is handed keeps it,
// and an attempt in the request's format goes out from a copy.
func TestRewriteAttempt(t *testing.T) {
	plugin := testPlugin(t, map[string]string{"gw-2026-11": "gateway-secret"})
	body := `{"model":"caller/model","stream":true,"temperature":1,"messages":[]}`
	newSend := func(sameFormat bool, stripParams ...string) (*schemas.BifrostContext, *schemas.BifrostRequest, *requestState, entry) {
		ctx := schemas.NewBifrostContext(t.Context(), schemas.NoDeadline)
		ctx.SetValue(schemas.BifrostContextKeyIntegrationType, integrationOpenAI)
		req := &schemas.BifrostRequest{RequestType: schemas.ChatCompletionStreamRequest, ChatRequest: &schemas.BifrostChatRequest{RawRequestBody: []byte(body)}}
		upstream := &routingv1.RouteUpstream{BifrostProvider: "up-a", ForwardModel: "model-a", SameFormat: sameFormat, StripParams: stripParams}
		state := &requestState{}
		state.capture(ctx, req)
		return ctx, req, state, entry{upstream: upstream}
	}

	t.Run("in the request's format", func(t *testing.T) {
		ctx, req, state, e := newSend(true, "temperature")
		got, fault := plugin.rewriteAttempt(ctx, req, state, e)
		if fault != nil {
			t.Fatal(fault.Error.Message)
		}
		want := `{"model":"model-a","stream":true,"messages":[],"stream_options":{"include_usage":true}}`
		if string(got.GetRawRequestBody()) != want || got.RequestType != req.RequestType || got.ChatRequest == req.ChatRequest {
			t.Fatalf("rewritten = %s (a copy: %v), want %s", got.GetRawRequestBody(), got.ChatRequest != req.ChatRequest, want)
		}
		if string(req.GetRawRequestBody()) != body || string(state.body) != body {
			t.Fatalf("the caller's body changed: %s", req.GetRawRequestBody())
		}
		if raw, _ := ctx.Value(schemas.BifrostContextKeyUseRawRequestBody).(bool); !raw {
			t.Fatal("the attempt does not send the body as it is")
		}
		if !reflect.DeepEqual(state.stripped, []string{"temperature"}) {
			t.Fatalf("stripped = %v", state.stripped)
		}
	})

	t.Run("converted, nothing to strip", func(t *testing.T) {
		ctx, req, state, e := newSend(false)
		ctx.SetValue(schemas.BifrostContextKeyUseRawRequestBody, true)
		state.stripped = []string{"left by an earlier attempt"}
		got, fault := plugin.rewriteAttempt(ctx, req, state, e)
		if fault != nil || got != req {
			t.Fatalf("rewriteAttempt = %v, %+v; want the request itself", got, fault)
		}
		if raw, _ := ctx.Value(schemas.BifrostContextKeyUseRawRequestBody).(bool); raw {
			t.Fatal("a converted attempt sends the raw body")
		}
		if state.stripped != nil {
			t.Fatalf("stripped = %v, want none", state.stripped)
		}
	})

	t.Run("converted, stripped, on a route that gave no converter", func(t *testing.T) {
		ctx, req, state, e := newSend(false, "temperature")
		_, fault := plugin.rewriteAttempt(ctx, req, state, e)
		if fault == nil || *fault.Type != "tokun_attempt_rewrite_failed" || *fault.AllowFallbacks {
			t.Fatalf("rewriteAttempt fault = %+v, want tokun_attempt_rewrite_failed", fault)
		}
	})

	t.Run("converted, stripped", func(t *testing.T) {
		ctx, req, state, e := newSend(false, "temperature")
		var converted string
		ctx.SetValue(schemas.BifrostContextKeyRequestBodyConverter, schemas.RequestBodyConverter(func(convertCtx *schemas.BifrostContext, b []byte) (*schemas.BifrostRequest, error) {
			converted = string(b)
			// The route's own pre-request callback may choose the raw body again.
			convertCtx.SetValue(schemas.BifrostContextKeyUseRawRequestBody, true)
			return &schemas.BifrostRequest{ChatRequest: &schemas.BifrostChatRequest{Provider: "caller", Model: "caller/model"}}, nil
		}))
		req.ChatRequest.Provider, req.ChatRequest.Model = "up-a", "model-a"
		got, fault := plugin.rewriteAttempt(ctx, req, state, e)
		if fault != nil {
			t.Fatal(fault.Error.Message)
		}
		if converted != `{"model":"caller/model","stream":true,"messages":[]}` {
			t.Fatalf("converted %s, want the body without temperature", converted)
		}
		provider, model, fallbacks := got.GetRequestFields()
		if got.RequestType != req.RequestType || provider != "up-a" || model != "model-a" || fallbacks != nil {
			t.Fatalf("converted request %s %s/%s %v, want the attempt's", got.RequestType, provider, model, fallbacks)
		}
		if raw, _ := ctx.Value(schemas.BifrostContextKeyUseRawRequestBody).(bool); raw {
			t.Fatal("a converted attempt sends the raw body")
		}
	})

	t.Run("a route that keeps no body", func(t *testing.T) {
		ctx := schemas.NewBifrostContext(t.Context(), schemas.NoDeadline)
		ctx.SetValue(schemas.BifrostContextKeyUseRawRequestBody, true)
		req := &schemas.BifrostRequest{RequestType: schemas.ChatCompletionRequest, ChatRequest: &schemas.BifrostChatRequest{}}
		state := &requestState{stripped: []string{"x"}}
		got, fault := plugin.rewriteAttempt(ctx, req, state, entry{upstream: &routingv1.RouteUpstream{SameFormat: true, StripParams: []string{"x"}}})
		if fault != nil || got != req || state.stripped != nil {
			t.Fatalf("rewriteAttempt = %v, %+v, stripped %v; want the request itself", got, fault, state.stripped)
		}
		if raw, _ := ctx.Value(schemas.BifrostContextKeyUseRawRequestBody).(bool); raw {
			t.Fatal("an attempt without a kept body sends a raw body")
		}
	})

	t.Run("every attempt carries its interface_output_tokens", func(t *testing.T) {
		ctx, req, state, _ := newSend(false)
		for _, upstream := range []*routingv1.RouteUpstream{
			{SameFormat: true, ForwardModel: "model-a", InterfaceOutputTokens: 64000},
			{SameFormat: false, ForwardModel: "model-b", InterfaceOutputTokens: 32000},
			{SameFormat: false, ForwardModel: "model-c"},
		} {
			if _, fault := plugin.rewriteAttempt(ctx, req, state, entry{upstream: upstream}); fault != nil {
				t.Fatal(fault.Error.Message)
			}
			limit, ok := ctx.Value(schemas.BifrostContextKeyAttemptOutputTokens).(int)
			if !ok || int64(limit) != upstream.GetInterfaceOutputTokens() {
				t.Fatalf("attempt on %s carries %v, want its interface_output_tokens %d", upstream.GetForwardModel(),
					ctx.Value(schemas.BifrostContextKeyAttemptOutputTokens), upstream.GetInterfaceOutputTokens())
			}
		}
		noBody := schemas.NewBifrostContext(t.Context(), schemas.NoDeadline)
		if _, fault := plugin.rewriteAttempt(noBody, &schemas.BifrostRequest{ChatRequest: &schemas.BifrostChatRequest{}}, &requestState{},
			entry{upstream: &routingv1.RouteUpstream{InterfaceOutputTokens: 8192}}); fault != nil {
			t.Fatal(fault.Error.Message)
		}
		if limit, _ := noBody.Value(schemas.BifrostContextKeyAttemptOutputTokens).(int); limit != 8192 {
			t.Fatalf("an attempt of a route that keeps no body carries %d, want 8192", limit)
		}
	})
}

func TestStrippedHeader(t *testing.T) {
	for _, tc := range []struct {
		status   int
		stripped []string
		want     string
	}{
		{http.StatusOK, []string{"temperature", "top_p"}, "temperature, top_p"},
		{http.StatusOK, nil, ""},
		{http.StatusBadGateway, []string{"temperature"}, ""},
	} {
		resp := &schemas.HTTPResponseMetadata{StatusCode: tc.status, Headers: map[string]string{}}
		strippedHeader(&requestState{stripped: tc.stripped}, resp)
		if got := resp.Header(HeaderStrippedParams); got != tc.want {
			t.Errorf("status %d, stripped %v: header %q, want %q", tc.status, tc.stripped, got, tc.want)
		}
	}
}

func TestFilterChunk(t *testing.T) {
	plugin := testPlugin(t, map[string]string{"gw-2026-11": "gateway-secret"})
	usage := &schemas.BifrostLLMUsage{PromptTokens: 5, CompletionTokens: 1, TotalTokens: 6}
	text, stop := "ok", "stop"
	chatChunk := func(usage *schemas.BifrostLLMUsage, choices ...schemas.BifrostResponseChoice) *schemas.BifrostStreamChunk {
		return &schemas.BifrostStreamChunk{BifrostChatResponse: &schemas.BifrostChatResponse{Usage: usage, Choices: choices}}
	}
	emptyDelta := schemas.BifrostResponseChoice{ChatStreamResponseChoice: &schemas.ChatStreamResponseChoice{Delta: &schemas.ChatStreamResponseChoiceDelta{}}}
	contentDelta := schemas.BifrostResponseChoice{ChatStreamResponseChoice: &schemas.ChatStreamResponseChoice{Delta: &schemas.ChatStreamResponseChoiceDelta{Content: &text}}}
	finish := schemas.BifrostResponseChoice{FinishReason: &stop, ChatStreamResponseChoice: &schemas.ChatStreamResponseChoice{Delta: &schemas.ChatStreamResponseChoiceDelta{}}}
	drop := &requestState{dropUsage: true}

	for _, chunk := range []*schemas.BifrostStreamChunk{chatChunk(usage), chatChunk(usage, emptyDelta),
		{BifrostTextCompletionResponse: &schemas.BifrostTextCompletionResponse{Usage: usage, Choices: []schemas.BifrostResponseChoice{{}}}}} {
		if got := plugin.filterChunk(drop, chunk); got != nil {
			t.Errorf("a usage-only chunk was kept: %+v", got)
		}
	}
	for _, choice := range []schemas.BifrostResponseChoice{contentDelta, finish} {
		chunk := chatChunk(usage, choice)
		got := plugin.filterChunk(drop, chunk)
		if got == nil || got.BifrostChatResponse.Usage != nil || len(got.BifrostChatResponse.Choices) != 1 {
			t.Fatalf("a chunk with output and usage = %+v, want the output without the usage", got)
		}
		if chunk.BifrostChatResponse.Usage != usage {
			t.Fatal("the chunk the hook was handed changed")
		}
	}
	textChunk := &schemas.BifrostStreamChunk{BifrostTextCompletionResponse: &schemas.BifrostTextCompletionResponse{
		Usage: usage, Choices: []schemas.BifrostResponseChoice{{TextCompletionResponseChoice: &schemas.TextCompletionResponseChoice{Text: &text}}},
	}}
	if got := plugin.filterChunk(drop, textChunk); got == nil || got.BifrostTextCompletionResponse.Usage != nil {
		t.Fatalf("a completions chunk with text and usage = %+v, want the text without the usage", got)
	}
	for _, chunk := range []*schemas.BifrostStreamChunk{chatChunk(nil, contentDelta), {BifrostResponsesStreamResponse: &schemas.BifrostResponsesStreamResponse{}}} {
		if got := plugin.filterChunk(drop, chunk); got != chunk {
			t.Errorf("a chunk without usage changed: %+v", got)
		}
	}
	usageOnly := chatChunk(usage)
	if got := plugin.filterChunk(&requestState{}, usageOnly); got != usageOnly {
		t.Fatal("the usage-only chunk of a caller that asked for usage was not passed on")
	}
}

// benchChatBody is a streamed chat completion body of about 1 KiB.
var benchChatBody = []byte(`{"model":"caller/model","stream":true,"temperature":1,"messages":[{"role":"system","content":"` +
	strings.Repeat("You are a helpful assistant. ", 24) + `"},{"role":"user","content":"` + strings.Repeat("hello ", 60) + `"}]}`)

// BenchmarkPreLLMHook measures one attempt's pre-hook: a fallback of a routed send whose route
// keeps no body; with the caller's 1 KiB streamed chat body kept, an attempt in the request's
// format (the model replaced and stream_options.include_usage added in the body, a copy of the
// request) and a converted attempt without parameters to strip; and a request that carries no
// plan.
func BenchmarkPreLLMHook(b *testing.B) {
	plugin := testPlugin(b, map[string]string{"gw-2026-11": "gateway-secret"})
	req := &schemas.BifrostRequest{RequestType: schemas.ChatCompletionRequest, ChatRequest: &schemas.BifrostChatRequest{}}
	b.Run("with plan", func(b *testing.B) {
		ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
		ctx.SetValue(requestStateKey, routedState(b, plugin))
		ctx.SetValue(schemas.BifrostContextKeyFallbackIndex, 1)
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, shortCircuit, err := plugin.PreLLMHook(ctx, req); shortCircuit != nil || err != nil {
				b.Fatalf("PreLLMHook = %+v, %v", shortCircuit, err)
			}
		}
	})
	streamReq := &schemas.BifrostRequest{RequestType: schemas.ChatCompletionStreamRequest, ChatRequest: &schemas.BifrostChatRequest{RawRequestBody: benchChatBody}}
	for _, sameFormat := range []bool{true, false} {
		name := "with plan, converted"
		if sameFormat {
			name = "with plan, in the request's format"
		}
		b.Run(name, func(b *testing.B) {
			ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
			ctx.SetValue(schemas.BifrostContextKeyIntegrationType, integrationOpenAI)
			plan := testPlan()
			for _, provider := range plan.GetProviders() {
				for _, upstream := range provider.GetUpstreams() {
					upstream.SameFormat = sameFormat
				}
			}
			state := &requestState{plan: plan}
			state.capture(ctx, streamReq)
			plugin.route(ctx, streamReq, state)
			ctx.SetValue(requestStateKey, state)
			ctx.SetValue(schemas.BifrostContextKeyFallbackIndex, 1)
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, shortCircuit, err := plugin.PreLLMHook(ctx, streamReq); shortCircuit != nil || err != nil {
					b.Fatalf("PreLLMHook = %+v, %v", shortCircuit, err)
				}
			}
		})
	}
	b.Run("without plan", func(b *testing.B) {
		ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, shortCircuit, err := plugin.PreLLMHook(ctx, req); shortCircuit != nil || err != nil {
				b.Fatalf("PreLLMHook = %+v, %v", shortCircuit, err)
			}
		}
	})
}

// BenchmarkStreamChunkHook measures one chunk of a streamed chat completion: a content chunk and
// the usage-only chunk of a caller that did not ask for usage, a chunk of a caller that did, and
// a request that carries no plan.
func BenchmarkStreamChunkHook(b *testing.B) {
	plugin := testPlugin(b, map[string]string{"gw-2026-11": "gateway-secret"})
	content := "ok"
	contentChunk := &schemas.BifrostStreamChunk{BifrostChatResponse: &schemas.BifrostChatResponse{
		Choices: []schemas.BifrostResponseChoice{{ChatStreamResponseChoice: &schemas.ChatStreamResponseChoice{Delta: &schemas.ChatStreamResponseChoiceDelta{Content: &content}}}},
	}}
	usageChunk := &schemas.BifrostStreamChunk{BifrostChatResponse: &schemas.BifrostChatResponse{
		Usage:   &schemas.BifrostLLMUsage{PromptTokens: 5, CompletionTokens: 1, TotalTokens: 6},
		Choices: []schemas.BifrostResponseChoice{{ChatStreamResponseChoice: &schemas.ChatStreamResponseChoice{Delta: &schemas.ChatStreamResponseChoiceDelta{}}}},
	}}
	for _, tc := range []struct {
		name      string
		dropUsage bool
		chunk     *schemas.BifrostStreamChunk
		dropped   bool
	}{
		{"content chunk, usage not asked for", true, contentChunk, false},
		{"usage-only chunk, usage not asked for", true, usageChunk, true},
		{"usage-only chunk, usage asked for", false, usageChunk, false},
	} {
		b.Run(tc.name, func(b *testing.B) {
			ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
			ctx.SetValue(requestStateKey, &requestState{plan: testPlan(), dropUsage: tc.dropUsage})
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if got, err := plugin.HTTPTransportStreamChunkHook(ctx, nil, tc.chunk); err != nil || (got == nil) != tc.dropped {
					b.Fatalf("HTTPTransportStreamChunkHook = %v, %v", got, err)
				}
			}
		})
	}
	b.Run("without plan", func(b *testing.B) {
		ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if got, err := plugin.HTTPTransportStreamChunkHook(ctx, nil, usageChunk); got != usageChunk || err != nil {
				b.Fatalf("HTTPTransportStreamChunkHook = %v, %v", got, err)
			}
		}
	})
}

// BenchmarkPostLLMHook measures one attempt's post-hook: a fallback of a routed send, and a
// request that carries no plan.
func BenchmarkPostLLMHook(b *testing.B) {
	plugin := testPlugin(b, map[string]string{"gw-2026-11": "gateway-secret"})
	resp := &schemas.BifrostResponse{}
	b.Run("with plan", func(b *testing.B) {
		ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
		ctx.SetValue(requestStateKey, routedState(b, plugin))
		ctx.SetValue(schemas.BifrostContextKeyFallbackIndex, 1)
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if got, gotErr, err := plugin.PostLLMHook(ctx, resp, nil); got != resp || gotErr != nil || err != nil {
				b.Fatalf("PostLLMHook = %v, %+v, %v", got, gotErr, err)
			}
		}
	})
	b.Run("without plan", func(b *testing.B) {
		ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if got, gotErr, err := plugin.PostLLMHook(ctx, resp, nil); got != resp || gotErr != nil || err != nil {
				b.Fatalf("PostLLMHook = %v, %+v, %v", got, gotErr, err)
			}
		}
	})
}

// BenchmarkSendIDLedgerClaim measures recording an accepted send_id, and reports the memory the
// ledger retains for each send it holds (retained-B/send), measured over 100000 sends. The
// send_id string itself is not counted: protobuf decoding allocated it, and the ledger keeps it
// alive (30 bytes, a 32-byte allocation, for "snd_" and a ULID).
func BenchmarkSendIDLedgerClaim(b *testing.B) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	expiresAt := now.Add(time.Hour)
	const held = 100000
	ids := make([]string, held+b.N)
	for i := range ids {
		// The length of a real one: "snd_" and 26 characters.
		ids[i] = fmt.Sprintf("snd_%026d", i)
	}

	ledger := newSendIDLedger()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for _, id := range ids[:held] {
		ledger.claim(id, expiresAt, now)
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	retained := float64(after.HeapAlloc-before.HeapAlloc) / held

	b.ReportAllocs()
	b.ResetTimer()
	for _, id := range ids[held:] {
		if !ledger.claim(id, expiresAt, now) {
			b.Fatal("a new send_id was refused")
		}
	}
	b.StopTimer()
	b.ReportMetric(retained, "retained-B/send")
	runtime.KeepAlive(ids)
}

func TestConfigSigningKeys(t *testing.T) {
	t.Setenv("TOKUN_TEST_SIGNING_KEYS", `{"gw-2026-11":"gateway-secret","tw-2026-11":"worker-secret"}`)
	var config Config
	if err := json.Unmarshal([]byte(`{"signing_keys":"env.TOKUN_TEST_SIGNING_KEYS"}`), &config); err != nil {
		t.Fatal(err)
	}
	keys, err := config.signingKeys()
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string][]byte{"gw-2026-11": []byte("gateway-secret"), "tw-2026-11": []byte("worker-secret")}; !reflect.DeepEqual(keys, want) {
		t.Fatalf("keys = %v", keys)
	}

	for name, value := range map[string]string{
		"unset env":       `{"signing_keys":"env.TOKUN_TEST_SIGNING_KEYS_UNSET"}`,
		"not set":         `{}`,
		"not an object":   `{"signing_keys":"[\"gw\"]"}`,
		"no key":          `{"signing_keys":"{}"}`,
		"empty secret":    `{"signing_keys":"{\"gw\":\"\"}"}`,
		"empty key id":    `{"signing_keys":"{\"\":\"secret\"}"}`,
		"secret not text": `{"signing_keys":"{\"gw\":1}"}`,
	} {
		t.Run(name, func(t *testing.T) {
			var config Config
			if err := json.Unmarshal([]byte(value), &config); err != nil {
				t.Fatal(err)
			}
			if _, err := Init(&config, bifrost.NewNoOpLogger()); err == nil {
				t.Fatalf("Init accepted %s", value)
			}
		})
	}
}

// The config store keeps the reference and the API shows no part of a secret.
func TestConfigNeverStoresOrShowsTheSecrets(t *testing.T) {
	t.Setenv("TOKUN_TEST_SIGNING_KEYS", `{"gw-2026-11":"gateway-secret"}`)
	plugin := testPlugin(t, map[string]string{"gw-2026-11": "gateway-secret"})
	raw := map[string]any{"signing_keys": "env.TOKUN_TEST_SIGNING_KEYS"}

	stored, err := plugin.MarshalConfigForStorage(raw)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]any{"signing_keys": "env.TOKUN_TEST_SIGNING_KEYS"}; !reflect.DeepEqual(stored, want) {
		t.Fatalf("stored = %v, want %v", stored, want)
	}

	redacted, err := plugin.RedactConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	shown, err := json.Marshal(redacted)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := redacted["signing_keys"]; !ok {
		t.Fatalf("redacted = %s, want signing_keys", shown)
	}
	for _, secret := range []string{"gateway-secret", "gw-2026-11"} {
		if strings.Contains(string(shown), secret) {
			t.Fatalf("redacted config %s shows %q", shown, secret)
		}
	}
}
