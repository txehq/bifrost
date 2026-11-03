package tokun

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"reflect"
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

func TestRouteDispatchesOnlyInsideThePlan(t *testing.T) {
	plugin := testPlugin(t, map[string]string{"gw-2026-11": "gateway-secret"})
	newRequest := func() *schemas.BifrostRequest {
		return &schemas.BifrostRequest{RequestType: schemas.ChatCompletionRequest, ChatRequest: &schemas.BifrostChatRequest{
			Provider:  "caller-provider",
			Model:     "caller-model",
			Fallbacks: []schemas.Fallback{{Provider: "caller-fallback", Model: "m", KeyID: "caller-key"}},
		}}
	}

	t.Run("every entry in plan order", func(t *testing.T) {
		state := &requestState{plan: testPlan()}
		ctx := schemas.NewBifrostContext(t.Context(), schemas.NoDeadline)
		req := newRequest()
		plugin.route(ctx, req, state)

		provider, model, fallbacks := req.GetRequestFields()
		if provider != "up-a" || model != "model-a" {
			t.Fatalf("primary = %s/%s, want up-a/model-a", provider, model)
		}
		if got := ctx.Value(schemas.BifrostContextKeyRoutingPinnedAPIKeyID); got != "key-a1" {
			t.Fatalf("pinned key = %v, want key-a1", got)
		}
		want := []schemas.Fallback{{Provider: "up-a", Model: "model-a", KeyID: "key-a2"}, {Provider: "up-b", Model: "model-b", KeyID: "key-b1"}}
		if !reflect.DeepEqual(fallbacks, want) {
			t.Fatalf("fallbacks = %+v, want %+v", fallbacks, want)
		}
		if !reflect.DeepEqual(state.chain, []int{0, 1, 2}) || len(state.order) != 3 || len(state.skipped) != 0 {
			t.Fatalf("chain %v, order %d entries, skipped %v", state.chain, len(state.order), state.skipped)
		}
	})

	t.Run("at most max_attempts entries", func(t *testing.T) {
		plan := testPlan()
		plan.MaxAttempts = 2
		state := &requestState{plan: plan}
		ctx := schemas.NewBifrostContext(t.Context(), schemas.NoDeadline)
		req := newRequest()
		plugin.route(ctx, req, state)
		_, _, fallbacks := req.GetRequestFields()
		if want := []schemas.Fallback{{Provider: "up-a", Model: "model-a", KeyID: "key-a2"}}; !reflect.DeepEqual(fallbacks, want) {
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
	state := &requestState{plan: testPlan(), order: drawOrder(testPlan())}
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
		req := &schemas.HTTPRequest{
			Method:  http.MethodPost,
			Path:    "/v1/chat/completions",
			Headers: map[string]string{HeaderRoutePlan: signed[i].plan, HeaderRoutePlanSignature: signed[i].signature},
			Body:    body,
		}
		ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
		if resp, err := plugin.HTTPTransportPreAuthHook(ctx, req); resp != nil || err != nil {
			b.Fatalf("refused: %v %v", resp, err)
		}
	}
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
