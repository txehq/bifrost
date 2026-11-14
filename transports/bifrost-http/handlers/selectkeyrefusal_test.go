package handlers

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/capsohq/bifrost/framework/configstore"
	"github.com/capsohq/bifrost/transports/v2/bifrost-http/lib"
	"github.com/fasthttp/router"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

// notAllowedProvider is an OpenAI-based custom provider whose allowed_requests lists chat only, so
// every session type is left out (TXE-7423).
const notAllowedProvider = schemas.ModelProvider("custom-chatonly")

func newNotAllowedClient(t *testing.T) (*bifrost.Bifrost, *lib.Config) {
	t.Helper()
	SetLogger(&mockLogger{})
	config := &lib.Config{
		ClientConfig: &configstore.ClientConfig{},
		Providers: map[schemas.ModelProvider]configstore.ProviderConfig{
			notAllowedProvider: {
				Keys: []schemas.Key{{ID: "key-chatonly", Name: "chatonly", Value: *schemas.NewSecretVar("sk-test"), Weight: 1,
					Models: []string{"gpt-live-1", "gpt-realtime", "gpt-5"}}},
				CustomProviderConfig: &schemas.CustomProviderConfig{
					BaseProviderType: schemas.OpenAI,
					AllowedRequests:  &schemas.AllowedRequests{ChatCompletion: true, ChatCompletionStream: true},
				},
			},
		},
	}
	client, err := bifrost.Init(context.Background(), schemas.BifrostConfig{
		Account: lib.NewBaseAccount(config),
		Logger:  bifrost.NewDefaultLogger(schemas.LogLevelError),
	})
	require.NoError(t, err)
	t.Cleanup(client.Shutdown)
	config.SetBifrostClient(client)
	return client, config
}

// assertUnsupportedOperation checks a refusal against the one the provider's own
// CheckOperationAllowed gave before TXE-7370 moved the check into key selection: status 400, code
// unsupported_operation, no type. On a WebSocket session the realtime wire mapping turns that into
// type invalid_request_error (the provider's policy is the caller's problem) and keeps the code;
// a type on the error would hide the code there (TXE-7423).
func assertUnsupportedOperation(t *testing.T, be *schemas.BifrostError, requestType schemas.RequestType) {
	t.Helper()
	require.NotNil(t, be, "the session is refused")
	require.NotNil(t, be.StatusCode)
	assert.Equal(t, 400, *be.StatusCode)
	require.NotNil(t, be.Error)
	require.NotNil(t, be.Error.Code)
	assert.Equal(t, schemas.UnsupportedOperationCode, *be.Error.Code)
	assert.Nil(t, be.Type, "as the provider's own check gave it")
	assert.Nil(t, be.Error.Type, "as the provider's own check gave it")
	assert.Contains(t, be.Error.Message, "not supported by "+string(notAllowedProvider))
	assert.Equal(t, notAllowedProvider, be.ExtraFields.Provider)
	assert.Equal(t, requestType, be.ExtraFields.RequestType)

	var event struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
			Code string `json:"code"`
		} `json:"error"`
	}
	payload := newRealtimeTurnErrorEventPayload(be)
	require.NoError(t, json.Unmarshal(payload, &event), string(payload))
	assert.Equal(t, "error", event.Type, string(payload))
	assert.Equal(t, "invalid_request_error", event.Error.Type, "a WebSocket session reads it as the caller's error: %s", payload)
	assert.Equal(t, schemas.UnsupportedOperationCode, event.Error.Code, "a WebSocket session keeps the code: %s", payload)
}

func TestLiveAdmissionRefusesAnOperationNotAllowedAsUnsupported(t *testing.T) {
	client, config := newNotAllowedClient(t)
	g := &liveGateway{client: client, config: config, handlerStore: config}
	provider := &fakeLiveWebRTCProvider{}
	target := liveTarget{provider: provider, providerKey: notAllowedProvider, voiceModel: "gpt-live-1"}

	admission, be := g.admit(&authHeaders{}, liveCallerCtx(t, ""), map[any]any{}, "/v1/live/sessions", target, "bf-notallowed")
	if admission != nil {
		t.Cleanup(admission.cancel)
	}
	assert.Nil(t, admission)
	assertUnsupportedOperation(t, be, schemas.LiveRequest)
	assert.Nil(t, provider.body, "the provider was never asked to open the session")

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	defer ctx.Cancel()
	_, be = g.controlKey(ctx, notAllowedProvider)
	assertUnsupportedOperation(t, be, schemas.LiveRequest)
}

func TestRealtimeClientSecretRefusesAnOperationNotAllowedAsUnsupported(t *testing.T) {
	client, config := newNotAllowedClient(t)
	r := router.New()
	NewRealtimeClientSecretsHandler(client, config).RegisterRoutes(r)

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod(fasthttp.MethodPost)
	ctx.Request.SetRequestURI("/v1/realtime/client_secrets")
	ctx.Request.Header.SetContentType("application/json")
	ctx.Request.SetBodyString(`{"session":{"type":"realtime","model":"` + string(notAllowedProvider) + `/gpt-realtime"}}`)
	r.Handler(ctx)

	assertRefusalBody(t, ctx)
}

func TestRealtimeWebRTCRefusesAnOperationNotAllowedAsUnsupported(t *testing.T) {
	client, config := newNotAllowedClient(t)
	r := router.New()
	h := NewWebRTCRealtimeHandler(client, config)
	t.Cleanup(h.Close)
	h.RegisterRoutes(r)

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod(fasthttp.MethodPost)
	ctx.Request.SetRequestURI("/v1/realtime?model=" + string(notAllowedProvider) + "/gpt-realtime")
	ctx.Request.Header.SetContentType("application/sdp")
	ctx.Request.SetBodyString("v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\ns=-\r\nt=0 0\r\n")
	r.Handler(ctx)

	assertRefusalBody(t, ctx)
}

// assertRefusalBody checks an HTTP refusal as the caller reads it.
func assertRefusalBody(t *testing.T, ctx *fasthttp.RequestCtx) {
	t.Helper()
	assert.Equal(t, fasthttp.StatusBadRequest, ctx.Response.StatusCode(), string(ctx.Response.Body()))
	var body struct {
		Error struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(ctx.Response.Body(), &body), string(ctx.Response.Body()))
	assert.Equal(t, schemas.UnsupportedOperationCode, body.Error.Code, string(ctx.Response.Body()))
	assert.Empty(t, body.Error.Type, "as the provider's own check gave it: %s", ctx.Response.Body())
	assert.Contains(t, body.Error.Message, "not supported by "+string(notAllowedProvider))
}

// The WebSocket realtime handler writes a key-selection error with writeRealtimeError, whose
// payload assertUnsupportedOperation reads, and WebSocket Responses with writeWSError. Each is
// checked here on the error the real key selection returns for its operation.
func TestWebSocketSessionsRefuseAnOperationNotAllowedAsUnsupported(t *testing.T) {
	client, _ := newNotAllowedClient(t)
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	defer ctx.Cancel()

	_, err := client.SelectKeyForProviderRequestType(ctx, schemas.RealtimeRequest, notAllowedProvider, "gpt-realtime")
	require.Error(t, err)
	assertUnsupportedOperation(t, withSelectKeyRefusal(newRealtimeWireBifrostError(400, "invalid_request_error", err.Error()), err), schemas.RealtimeRequest)

	_, err = client.SelectKeyForProviderRequestType(ctx, schemas.WebSocketResponsesRequest, notAllowedProvider, "gpt-5")
	require.Error(t, err)
	assert.Equal(t, schemas.UnsupportedOperationCode, selectKeyErrorCode(err, "invalid_request_error"))

	// Any other key-selection error keeps the handler's own code.
	other := assert.AnError
	assert.Equal(t, "invalid_request_error", selectKeyErrorCode(other, "invalid_request_error"))
	plain := newRealtimeWireBifrostError(400, "invalid_request_error", other.Error())
	assert.Same(t, plain, withSelectKeyRefusal(plain, other))
}

// Every handler that takes a session key from SelectKeyForProviderRequestType must answer its error
// through withSelectKeyRefusal or selectKeyErrorCode, so a new session type cannot turn an
// allowed_requests refusal back into a bare invalid_request_error (TXE-7423).
func TestEverySelectKeyCallerKeepsTheRefusalCode(t *testing.T) {
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	callers := 0
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		src, err := os.ReadFile(file)
		require.NoError(t, err)
		f, err := parser.ParseFile(token.NewFileSet(), file, src, 0)
		require.NoError(t, err, file)
		calls := map[string]int{}
		ast.Inspect(f, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				calls[sel.Sel.Name]++
			}
			if call, ok := n.(*ast.CallExpr); ok {
				if id, ok := call.Fun.(*ast.Ident); ok {
					calls[id.Name]++
				}
			}
			return true
		})
		n := calls["SelectKeyForProviderRequestType"]
		if n == 0 {
			continue
		}
		callers++
		passed := calls["withSelectKeyRefusal"] + calls["selectKeyErrorCode"]
		assert.Greater(t, passed, 0, "%s selects a session key %d time(s) but never passes an allowed_requests refusal through withSelectKeyRefusal or selectKeyErrorCode", file, n)
	}
	assert.GreaterOrEqual(t, callers, 5, "the guard found the live, realtime client secret, WebRTC, WebSocket realtime and WebSocket Responses handlers")
}
