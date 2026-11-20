package bifrost

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	"github.com/capsohq/bifrost/core/schemas"
)

// notProviderOperations are the RequestType constants that never reach a provider, so
// allowed_requests has no flag for them.
var notProviderOperations = map[schemas.RequestType]bool{
	schemas.MCPToolExecutionRequest: true, // a tool call Bifrost runs itself
	schemas.UnknownRequest:          true,
}

// declaredRequestTypes reads every RequestType constant from core/schemas, so a request type
// added later is covered without editing this test.
func declaredRequestTypes(t *testing.T) []schemas.RequestType {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, "schemas", nil, 0)
	if err != nil {
		t.Fatalf("parse core/schemas: %v", err)
	}
	var types []schemas.RequestType
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				gen, ok := decl.(*ast.GenDecl)
				if !ok || gen.Tok != token.CONST {
					continue
				}
				for _, spec := range gen.Specs {
					vs := spec.(*ast.ValueSpec)
					ident, ok := vs.Type.(*ast.Ident)
					if !ok || ident.Name != "RequestType" {
						continue
					}
					for _, value := range vs.Values {
						lit, ok := value.(*ast.BasicLit)
						if !ok || lit.Kind != token.STRING {
							continue
						}
						s, err := strconv.Unquote(lit.Value)
						if err != nil {
							t.Fatalf("unquote %s: %v", lit.Value, err)
						}
						types = append(types, schemas.RequestType(s))
					}
				}
			}
		}
	}
	if len(types) < 60 {
		t.Fatalf("found only %d RequestType constants in core/schemas; the parser no longer sees them", len(types))
	}
	return types
}

// allowEverything returns an AllowedRequests with every flag set.
func allowEverything() *schemas.AllowedRequests {
	ar := &schemas.AllowedRequests{}
	v := reflect.ValueOf(ar).Elem()
	for i := 0; i < v.NumField(); i++ {
		v.Field(i).SetBool(true)
	}
	return ar
}

// TestEveryRequestTypeHasAnAllowedRequestsFlag fails when a request type that reaches a provider
// has no allowed_requests flag: IsOperationAllowed would refuse it for every custom provider that
// sets allowed_requests, even one that allows everything.
func TestEveryRequestTypeHasAnAllowedRequestsFlag(t *testing.T) {
	all := allowEverything()
	none := &schemas.AllowedRequests{}
	for _, rt := range declaredRequestTypes(t) {
		if notProviderOperations[rt] {
			continue
		}
		if !all.IsOperationAllowed(rt) {
			t.Errorf("%s: no allowed_requests flag admits it; add one in schemas.AllowedRequests.IsOperationAllowed", rt)
		}
		if none.IsOperationAllowed(rt) {
			t.Errorf("%s: an empty allowed_requests admits it", rt)
		}
	}
}

// TestCheckAllowedRequestEveryRequestType checks the central gate for every request type: a
// custom provider that allows nothing refuses it, one that allows everything admits it, and a
// provider without allowed_requests (built-in, or custom without the field) is not gated.
func TestCheckAllowedRequestEveryRequestType(t *testing.T) {
	provider := stubProvider{key: "custom-gate"}
	nothing := &schemas.ProviderConfig{CustomProviderConfig: &schemas.CustomProviderConfig{
		CustomProviderKey: "custom-gate", BaseProviderType: schemas.OpenAI, AllowedRequests: &schemas.AllowedRequests{},
	}}
	everything := &schemas.ProviderConfig{CustomProviderConfig: &schemas.CustomProviderConfig{
		CustomProviderKey: "custom-gate", BaseProviderType: schemas.OpenAI, AllowedRequests: allowEverything(),
	}}
	ungated := &schemas.ProviderConfig{CustomProviderConfig: &schemas.CustomProviderConfig{
		CustomProviderKey: "custom-gate", BaseProviderType: schemas.OpenAI,
	}}
	for _, rt := range declaredRequestTypes(t) {
		if notProviderOperations[rt] {
			continue
		}
		ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
		req := &schemas.BifrostRequest{RequestType: rt}
		err := checkAllowedRequest(ctx, provider, nothing, req)
		if err == nil {
			t.Errorf("%s: a custom provider that allows nothing let it through", rt)
		} else if err.Error == nil || err.Error.Code == nil || *err.Error.Code != "unsupported_operation" {
			t.Errorf("%s: want unsupported_operation, got %+v", rt, err.Error)
		}
		if err := checkAllowedRequest(ctx, provider, everything, req); err != nil {
			t.Errorf("%s: a custom provider that allows everything refused it: %s", rt, err.Error.Message)
		}
		if err := checkAllowedRequest(ctx, provider, ungated, req); err != nil {
			t.Errorf("%s: a custom provider without allowed_requests refused it", rt)
		}
		if err := checkAllowedRequest(ctx, provider, &schemas.ProviderConfig{}, req); err != nil {
			t.Errorf("%s: a built-in provider refused it", rt)
		}
	}
}

// TestCheckAllowedRequestExemptsJobAccounting: Bifrost's own provider-job accounting polls are
// not refused, so a job a caller was allowed to create is still billed.
func TestCheckAllowedRequestExemptsJobAccounting(t *testing.T) {
	provider := stubProvider{key: "custom-gate"}
	config := &schemas.ProviderConfig{CustomProviderConfig: &schemas.CustomProviderConfig{
		CustomProviderKey: "custom-gate", BaseProviderType: schemas.OpenAI,
		AllowedRequests: &schemas.AllowedRequests{VideoGeneration: true, BatchCreate: true},
	}}
	for _, rt := range []schemas.RequestType{schemas.BatchRetrieveRequest, schemas.BatchResultsRequest, schemas.VideoRetrieveRequest, schemas.VideoDownloadRequest} {
		req := &schemas.BifrostRequest{RequestType: rt}
		caller := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
		if checkAllowedRequest(caller, provider, config, req) == nil {
			t.Errorf("%s: a caller's request was let through", rt)
		}
		sweeper := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
		sweeper.SetValue(schemas.BifrostContextKeyProviderJobAccounting, true)
		if err := checkAllowedRequest(sweeper, provider, config, req); err != nil {
			t.Errorf("%s: the accounting sweeper's poll was refused: %s", rt, err.Error.Message)
		}
		// Only a bool true counts.
		spoof := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
		spoof.SetValue(schemas.BifrostContextKeyProviderJobAccounting, "true")
		if checkAllowedRequest(spoof, provider, config, req) == nil {
			t.Errorf("%s: a string marker exempted the request", rt)
		}
	}
}

// TestUpstreamOperationFollowsDispatchConversions: the gate checks the operation dispatch sends
// upstream, as the provider methods did.
func TestUpstreamOperationFollowsDispatchConversions(t *testing.T) {
	mode := "pro"
	prompt := "hi"
	textInput := &schemas.TextCompletionInput{PromptStr: &prompt}
	cases := []struct {
		name       string
		provider   schemas.ModelProvider
		base       schemas.ModelProvider
		changeType schemas.RequestType
		req        schemas.BifrostRequest
		want       schemas.RequestType
	}{
		{"text as chat", "p", "", schemas.ChatCompletionRequest,
			schemas.BifrostRequest{RequestType: schemas.TextCompletionRequest, TextCompletionRequest: &schemas.BifrostTextCompletionRequest{Input: textInput}}, schemas.ChatCompletionRequest},
		{"text stream as chat stream", "p", "", schemas.ChatCompletionRequest,
			schemas.BifrostRequest{RequestType: schemas.TextCompletionStreamRequest, TextCompletionRequest: &schemas.BifrostTextCompletionRequest{Input: textInput}}, schemas.ChatCompletionStreamRequest},
		{"text without input stays text", "p", "", schemas.ChatCompletionRequest,
			schemas.BifrostRequest{RequestType: schemas.TextCompletionRequest, TextCompletionRequest: &schemas.BifrostTextCompletionRequest{}}, schemas.TextCompletionRequest},
		{"chat as responses", "p", "", schemas.ResponsesRequest,
			schemas.BifrostRequest{RequestType: schemas.ChatCompletionRequest, ChatRequest: &schemas.BifrostChatRequest{}}, schemas.ResponsesRequest},
		{"chat stream as responses stream", "p", "", schemas.ResponsesRequest,
			schemas.BifrostRequest{RequestType: schemas.ChatCompletionStreamRequest, ChatRequest: &schemas.BifrostChatRequest{}}, schemas.ResponsesStreamRequest},
		{"chat with reasoning.mode on an openai base", "custom-openai", schemas.OpenAI, "",
			schemas.BifrostRequest{RequestType: schemas.ChatCompletionRequest, ChatRequest: &schemas.BifrostChatRequest{Params: &schemas.ChatParameters{Reasoning: &schemas.ChatReasoning{Mode: &mode}}}}, schemas.ResponsesRequest},
		{"chat with reasoning.mode elsewhere stays chat", "custom-deepseek", schemas.DeepSeek, "",
			schemas.BifrostRequest{RequestType: schemas.ChatCompletionRequest, ChatRequest: &schemas.BifrostChatRequest{Params: &schemas.ChatParameters{Reasoning: &schemas.ChatReasoning{Mode: &mode}}}}, schemas.ChatCompletionRequest},
		{"responses as chat", "p", "", schemas.ChatCompletionRequest,
			schemas.BifrostRequest{RequestType: schemas.ResponsesRequest, ResponsesRequest: &schemas.BifrostResponsesRequest{}}, schemas.ChatCompletionRequest},
		{"responses stream as chat stream", "p", "", schemas.ChatCompletionRequest,
			schemas.BifrostRequest{RequestType: schemas.ResponsesStreamRequest, ResponsesRequest: &schemas.BifrostResponsesRequest{}}, schemas.ChatCompletionStreamRequest},
		{"decision emulated through responses", "p", "", schemas.ResponsesRequest,
			schemas.BifrostRequest{RequestType: schemas.DecisionRequest}, schemas.ResponsesRequest},
		{"embedding unchanged", "p", "", schemas.ChatCompletionRequest,
			schemas.BifrostRequest{RequestType: schemas.EmbeddingRequest}, schemas.EmbeddingRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
			if tc.changeType != "" {
				ctx.SetValue(schemas.BifrostContextKeyChangeRequestType, tc.changeType)
			}
			if tc.base != "" {
				ctx.SetValue(schemas.BifrostContextKeyBaseProviderType, tc.base)
			}
			got := upstreamOperation(ctx, stubProvider{key: tc.provider}, &tc.req)
			if got != tc.want {
				t.Fatalf("upstreamOperation = %s, want %s", got, tc.want)
			}
		})
	}
}

// BenchmarkCheckAllowedRequest measures the check the request worker now runs on every queued
// request. A built-in provider and a custom provider without allowed_requests return on the
// first nil test; the allowed and refused cases are a custom provider with allowed_requests.
func BenchmarkCheckAllowedRequest(b *testing.B) {
	var provider schemas.Provider = stubProvider{key: "custom-bench"} // boxed once, not per call
	chat := &schemas.BifrostRequest{RequestType: schemas.ChatCompletionRequest, ChatRequest: &schemas.BifrostChatRequest{}}
	mode := "pro"
	chatWithMode := &schemas.BifrostRequest{RequestType: schemas.ChatCompletionRequest, ChatRequest: &schemas.BifrostChatRequest{
		Params: &schemas.ChatParameters{Reasoning: &schemas.ChatReasoning{Mode: &mode}},
	}}
	custom := func(allowed *schemas.AllowedRequests) *schemas.ProviderConfig {
		return &schemas.ProviderConfig{CustomProviderConfig: &schemas.CustomProviderConfig{
			CustomProviderKey: "custom-bench", BaseProviderType: schemas.OpenAI, AllowedRequests: allowed,
		}}
	}
	cases := []struct {
		name   string
		config *schemas.ProviderConfig
		req    *schemas.BifrostRequest
	}{
		{"built-in provider", &schemas.ProviderConfig{}, chat},
		{"custom provider without allowed_requests", custom(nil), chat},
		{"custom provider, chat allowed", custom(&schemas.AllowedRequests{ChatCompletion: true}), chat},
		{"custom provider, chat with reasoning.mode checked as responses", custom(&schemas.AllowedRequests{Responses: true}), chatWithMode},
		{"custom provider, chat refused", custom(&schemas.AllowedRequests{}), chat},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
			ctx.SetValue(schemas.BifrostContextKeyBaseProviderType, schemas.OpenAI)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = checkAllowedRequest(ctx, provider, tc.config, tc.req)
			}
		})
	}
}

// BenchmarkErrOperationNotAllowed measures the error SelectKeyForProviderRequestType returns for an
// operation allowed_requests leaves out. Only a refused session builds it; a key handed out never
// does. "before" is the plain error it returned before TXE-7423.
var errOperationNotAllowedSink error

func BenchmarkErrOperationNotAllowed(b *testing.B) {
	const name = schemas.ModelProvider("custom-openai")
	b.Run("before", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			errOperationNotAllowedSink = fmt.Errorf("%s is not supported by %s provider", schemas.LiveRequest, name)
		}
	})
	b.Run("after", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			errOperationNotAllowedSink = errOperationNotAllowed(schemas.LiveRequest, name)
		}
	})
}

// countingUpstream answers every call with a minimal chat completion and counts the calls.
func countingUpstream(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","created":1,"model":"m",` +
			`"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(server.Close)
	return server, &calls
}

func customProviderAccount(name schemas.ModelProvider, base schemas.ModelProvider, url string, allowed *schemas.AllowedRequests) *MockAccount {
	account := NewMockAccount()
	account.AddProviderWithBaseURL(name, 1, 1, url)
	account.configs[name].NetworkConfig.MaxRetries = 0
	account.SetCustomProviderConfig(name, &schemas.CustomProviderConfig{
		BaseProviderType: base,
		AllowedRequests:  allowed,
	})
	account.SetKeysForProvider(name, []schemas.Key{
		{ID: "custom-key", Value: *schemas.NewSecretVar("sk-custom"), Models: schemas.WhiteList{"*"}, Weight: 100},
	})
	return account
}

// TestCustomProviderChatNotAllowedNeverReachesUpstream: DeepSeek's provider method does not gate
// chat itself (a gap measured on #221); the central gate refuses it before any call.
func TestCustomProviderChatNotAllowedNeverReachesUpstream(t *testing.T) {
	server, calls := countingUpstream(t)
	const name = schemas.ModelProvider("custom-deepseek")
	chat := func(client *Bifrost) *schemas.BifrostError {
		ctx := schemas.NewBifrostContext(context.Background(), time.Now().Add(30*time.Second))
		defer ctx.Cancel()
		_, err := client.ChatCompletionRequest(ctx, &schemas.BifrostChatRequest{
			Provider: name,
			Model:    "deepseek-chat",
			Input:    []schemas.ChatMessage{{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: Ptr("hi")}}},
		})
		return err
	}

	refused := newStreamTestClient(t, customProviderAccount(name, schemas.DeepSeek, server.URL, &schemas.AllowedRequests{ChatCompletionStream: true}))
	err := chat(refused)
	if err == nil || err.Error == nil || err.Error.Code == nil || *err.Error.Code != "unsupported_operation" {
		t.Fatalf("want unsupported_operation, got %+v", err)
	}
	if err.ExtraFields.Provider != name || err.ExtraFields.RequestType != schemas.ChatCompletionRequest || err.ExtraFields.OriginalModelRequested != "deepseek-chat" {
		t.Fatalf("error extra fields: %+v", err.ExtraFields)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("the upstream got %d calls for an operation the provider does not allow", got)
	}

	// Control: the same provider with chat allowed reaches the upstream.
	allowed := newStreamTestClient(t, customProviderAccount(name, schemas.DeepSeek, server.URL, &schemas.AllowedRequests{ChatCompletion: true}))
	if err := chat(allowed); err != nil {
		t.Fatalf("allowed chat failed: %v", err.Error)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("allowed chat made %d upstream calls, want 1", got)
	}
}

// TestCustomProviderOCRNotAllowed: the OCR check that dispatch made on its own is now the
// worker's, and still keeps the caller's model on the error.
func TestCustomProviderOCRNotAllowed(t *testing.T) {
	server, calls := countingUpstream(t)
	const name = schemas.ModelProvider("custom-ocr")
	client := newStreamTestClient(t, customProviderAccount(name, schemas.OpenAI, server.URL, &schemas.AllowedRequests{ChatCompletion: true}))
	ctx := schemas.NewBifrostContext(context.Background(), time.Now().Add(30*time.Second))
	defer ctx.Cancel()
	_, err := client.OCRRequest(ctx, &schemas.BifrostOCRRequest{
		Provider: name,
		Model:    "mistral-ocr-latest",
		Document: schemas.OCRDocument{Type: schemas.OCRDocumentTypeDocumentURL, DocumentURL: Ptr("https://example.com/doc.pdf")},
	})
	if err == nil || err.Error == nil || err.Error.Code == nil || *err.Error.Code != "unsupported_operation" {
		t.Fatalf("want unsupported_operation, got %+v", err)
	}
	if err.ExtraFields.Provider != name || err.ExtraFields.RequestType != schemas.OCRRequest || err.ExtraFields.OriginalModelRequested != "mistral-ocr-latest" {
		t.Fatalf("error extra fields: %+v", err.ExtraFields)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("the upstream got %d calls", got)
	}
}

// TestResponsesServedThroughChatNeedsOnlyResponses: the deepseek, glm and qwen bases serve a
// Responses request through their own chat call. Their ChatCompletion now checks chat, so
// Responses calls the unchecked path: a provider that allows responses but not chat still
// serves Responses, and the request is checked once, as responses.
func TestResponsesServedThroughChatNeedsOnlyResponses(t *testing.T) {
	for _, base := range []schemas.ModelProvider{schemas.DeepSeek, schemas.GLM, schemas.Qwen} {
		t.Run(string(base), func(t *testing.T) {
			server, calls := countingUpstream(t)
			name := schemas.ModelProvider("responses-only-" + string(base))
			provider, err := (&Bifrost{logger: NewNoOpLogger()}).createBaseProvider(name, &schemas.ProviderConfig{
				NetworkConfig: schemas.NetworkConfig{BaseURL: server.URL, DefaultRequestTimeoutInSeconds: 10},
				CustomProviderConfig: &schemas.CustomProviderConfig{
					CustomProviderKey: string(name), BaseProviderType: base,
					AllowedRequests: &schemas.AllowedRequests{Responses: true},
				},
			})
			if err != nil {
				t.Fatalf("construct: %v", err)
			}
			key := schemas.Key{ID: "k", Value: *schemas.NewSecretVar("sk-test"), Models: schemas.WhiteList{"*"}, Weight: 1}
			ctx := schemas.NewBifrostContext(context.Background(), time.Now().Add(30*time.Second))
			defer ctx.Cancel()

			_, chatErr := provider.ChatCompletion(ctx, key, &schemas.BifrostChatRequest{
				Provider: name, Model: "m",
				Input: []schemas.ChatMessage{{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: Ptr("hi")}}},
			})
			if chatErr == nil || chatErr.Error == nil || chatErr.Error.Code == nil || *chatErr.Error.Code != "unsupported_operation" {
				t.Fatalf("chat is not allowed, want unsupported_operation, got %+v", chatErr)
			}
			if got := calls.Load(); got != 0 {
				t.Fatalf("a refused chat made %d upstream calls", got)
			}

			_, respErr := provider.Responses(ctx, key, &schemas.BifrostResponsesRequest{
				Provider: name, Model: "m",
				Input: []schemas.ResponsesMessage{{Role: Ptr(schemas.ResponsesInputMessageRoleUser), Content: &schemas.ResponsesMessageContent{ContentStr: Ptr("hi")}}},
			})
			if respErr != nil && respErr.Error != nil && respErr.Error.Code != nil && *respErr.Error.Code == "unsupported_operation" {
				t.Fatalf("responses is allowed, but it was refused: %s", respErr.Error.Message)
			}
			if got := calls.Load(); got != 1 {
				t.Fatalf("an allowed Responses request made %d upstream calls, want 1", got)
			}
		})
	}
}

// TestSelectKeyForProviderRequestTypeEnforcesAllowedRequests: realtime, live and WebSocket
// Responses sessions take their key here and dial the vendor themselves.
func TestSelectKeyForProviderRequestTypeEnforcesAllowedRequests(t *testing.T) {
	const name = schemas.ModelProvider("custom-openai")
	client := newStreamTestClient(t, customProviderAccount(name, schemas.OpenAI, "http://127.0.0.1:1", &schemas.AllowedRequests{Realtime: true}))
	ctx := schemas.NewBifrostContext(context.Background(), time.Now().Add(30*time.Second))
	defer ctx.Cancel()
	for _, rt := range []schemas.RequestType{schemas.WebSocketResponsesRequest, schemas.LiveRequest, schemas.LiveSidebandRequest} {
		_, err := client.SelectKeyForProviderRequestType(ctx, rt, name, "gpt-realtime")
		if err == nil {
			t.Errorf("%s: a key was handed out for an operation the provider does not allow", rt)
			continue
		}
		// Typed, so the session handlers answer it as unsupported_operation (TXE-7423).
		var refused *schemas.OperationNotAllowedError
		if !errors.As(err, &refused) {
			t.Errorf("%s: refusal is %T, want *schemas.OperationNotAllowedError", rt, err)
			continue
		}
		if refused.RequestType != rt || refused.Provider != name {
			t.Errorf("%s: refusal names %s on %s", rt, refused.RequestType, refused.Provider)
		}
	}
	key, err := client.SelectKeyForProviderRequestType(ctx, schemas.RealtimeRequest, name, "gpt-realtime")
	if err != nil {
		t.Fatalf("realtime is allowed, got %v", err)
	}
	if key.ID != "custom-key" {
		t.Fatalf("realtime got key %q", key.ID)
	}
}

// allowAllBut returns an AllowedRequests with every flag set except the one that allows
// operation.
func allowAllBut(t *testing.T, operation schemas.RequestType) *schemas.AllowedRequests {
	t.Helper()
	ar := allowEverything()
	v := reflect.ValueOf(ar).Elem()
	for i := 0; i < v.NumField(); i++ {
		v.Field(i).SetBool(false)
		if !ar.IsOperationAllowed(operation) {
			return ar
		}
		v.Field(i).SetBool(true)
	}
	t.Fatalf("no allowed_requests flag allows %s", operation)
	return nil
}

// TestAllowedRequestsGateAgreesWithEveryProvider is the class check for TXE-7027. For every
// base a custom provider may use and every call in the gate tables, it builds a provider that
// allows everything except that call's own request type and asks two questions: does the
// provider method reach the vendor (it served the request through a conversion of its own,
// such as OpenAI answering Responses with a chat completion), and does core's gate let the
// request through. The answers must agree. A provider that converts a request without
// declaring it through schemas.UpstreamOperationConverter is refused by the gate before its
// conversion can run (the 400 TXE-7370 gave stepfun); a declared conversion the provider
// does not make lets through a request the provider then refuses.
func TestAllowedRequestsGateAgreesWithEveryProvider(t *testing.T) {
	upstream := newGuardUpstream(t)
	client := &Bifrost{logger: NewNoOpLogger()}
	calls := make(map[schemas.RequestType]resourceOverrideCall, len(resourceOverrideCalls)+len(allowedRequestsGateCalls))
	for requestType, call := range resourceOverrideCalls {
		calls[requestType] = call
	}
	for requestType, call := range allowedRequestsGateCalls {
		calls[requestType] = call
	}
	if calls[schemas.ResponsesRequest] == nil {
		t.Fatal("allowedRequestsGateCalls has no Responses call; the OpenAI responses-to-chat conversion would go unchecked")
	}
	requestTypes := make([]schemas.RequestType, 0, len(calls))
	for requestType := range calls {
		requestTypes = append(requestTypes, requestType)
	}
	slices.Sort(requestTypes)

	converted := 0
	for _, base := range schemas.SupportedBaseProviders {
		t.Run(string(base), func(t *testing.T) {
			for _, requestType := range requestTypes {
				name := schemas.ModelProvider("agree-" + string(base))
				config := &schemas.ProviderConfig{
					NetworkConfig: schemas.NetworkConfig{BaseURL: "http://" + guardUpstreamHost, DefaultRequestTimeoutInSeconds: 10},
					ProxyConfig: &schemas.ProxyConfig{
						Type: schemas.HTTPProxy, URL: schemas.NewSecretVar(upstream.proxyURL), CACertPEM: schemas.NewSecretVar(upstream.caPEM),
					},
					CustomProviderConfig: &schemas.CustomProviderConfig{CustomProviderKey: string(name), BaseProviderType: base, AllowedRequests: allowAllBut(t, requestType)},
				}
				provider, err := client.createBaseProvider(name, config)
				if err != nil {
					t.Fatalf("construct: %v", err)
				}
				ids := resourceOverrideIDs
				if placeholders, ok := providerUtils.ResourcePathPlaceholders[requestType]; ok {
					ids = resourceOverrideIDs[:len(placeholders)]
				}
				upstream.take()
				ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
				ctx.SetValue(schemas.BifrostContextKeyBaseProviderType, base)
				func() {
					defer func() { _ = recover() }()
					_ = calls[requestType](ctx, provider, []schemas.Key{guardKey(base)}, base, ids)
				}()
				served := len(upstream.take()) > 0

				req := &schemas.BifrostRequest{RequestType: requestType}
				switch requestType {
				case schemas.ChatCompletionRequest:
					req.ChatRequest = &schemas.BifrostChatRequest{}
				case schemas.ResponsesRequest:
					req.ResponsesRequest = &schemas.BifrostResponsesRequest{}
				}
				gateCtx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
				gateCtx.SetValue(schemas.BifrostContextKeyBaseProviderType, base)
				gateErr := checkAllowedRequest(gateCtx, provider, config, req)
				passed := gateErr == nil

				switch {
				case served && !passed:
					t.Errorf("%s %s: the provider method reaches the vendor with %s disabled, but the gate refuses it. A conversion to an allowed operation is declared with schemas.UpstreamOperationConverter; otherwise the method is missing its own CheckOperationAllowed", base, requestType, requestType)
				case passed && !served:
					t.Errorf("%s %s: the gate lets it through as %s, but the provider never reaches the vendor", base, requestType, upstreamOperation(gateCtx, provider, req))
				case served:
					converted++
				}
			}
		})
	}
	// The OpenAI base's Responses-as-chat is the one conversion known today; the sweep must see it.
	if converted == 0 {
		t.Fatal("no provider served a disabled request type through a conversion; the sweep no longer reaches the OpenAI responses-to-chat path")
	}
}

// chatPathUpstream answers chat completions, plain or streamed, and records each request path.
func chatPathUpstream(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		if strings.Contains(string(body), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":null}]}`+"\n\n")
			_, _ = io.WriteString(w, `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`+"\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","created":1,"model":"m",` +
			`"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(server.Close)
	return server, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), paths...)
	}
}

// TestResponsesOnChatOnlyOpenAICustomProviderServedAsChat pins the TXE-7027 decision through
// the whole client: an OpenAI-base custom provider whose allowed_requests has chat but not
// responses (StepFun as Tokun's reconciler writes it) serves /v1/responses, plain and
// streamed, with one chat completion upstream. A provider that allows neither is refused
// before any upstream call.
func TestResponsesOnChatOnlyOpenAICustomProviderServedAsChat(t *testing.T) {
	const name = schemas.ModelProvider("stepfun")
	input := []schemas.ResponsesMessage{{Role: Ptr(schemas.ResponsesInputMessageRoleUser), Content: &schemas.ResponsesMessageContent{ContentStr: Ptr("hi")}}}
	newCtx := func(t *testing.T) *schemas.BifrostContext {
		ctx := schemas.NewBifrostContext(context.Background(), time.Now().Add(30*time.Second))
		t.Cleanup(ctx.Cancel)
		return ctx
	}

	t.Run("served as chat", func(t *testing.T) {
		server, paths := chatPathUpstream(t)
		client := newStreamTestClient(t, customProviderAccount(name, schemas.OpenAI, server.URL,
			&schemas.AllowedRequests{ChatCompletion: true, ChatCompletionStream: true, Speech: true}))

		resp, err := client.ResponsesRequest(newCtx(t), &schemas.BifrostResponsesRequest{Provider: name, Model: "step-5-preview", Input: input})
		if err != nil {
			t.Fatalf("responses on a chat-only provider: want it served as chat, got %v", err.Error)
		}
		if resp == nil || resp.ExtraFields.ConvertedRequestType != schemas.ChatCompletionRequest {
			t.Fatalf("want a Responses answer converted from chat, got %+v", resp)
		}

		stream, err := client.ResponsesStreamRequest(newCtx(t), &schemas.BifrostResponsesRequest{Provider: name, Model: "step-5-preview", Input: input})
		if err != nil {
			t.Fatalf("responses stream on a chat-only provider: want it served as chat stream, got %v", err.Error)
		}
		chunks := 0
		for chunk := range stream {
			if chunk.BifrostError != nil {
				t.Fatalf("stream error: %+v", chunk.BifrostError.Error)
			}
			chunks++
		}
		if chunks == 0 {
			t.Fatal("the streamed Responses request returned no chunks")
		}

		got := paths()
		if len(got) != 2 || got[0] != "/v1/chat/completions" || got[1] != "/v1/chat/completions" {
			t.Fatalf("upstream paths %v, want two calls to /v1/chat/completions", got)
		}
	})

	t.Run("refused when neither is allowed", func(t *testing.T) {
		server, paths := chatPathUpstream(t)
		client := newStreamTestClient(t, customProviderAccount(name, schemas.OpenAI, server.URL, &schemas.AllowedRequests{Speech: true}))
		_, err := client.ResponsesRequest(newCtx(t), &schemas.BifrostResponsesRequest{Provider: name, Model: "step-5-preview", Input: input})
		if err == nil || err.Error == nil || err.Error.Code == nil || *err.Error.Code != "unsupported_operation" {
			t.Fatalf("want unsupported_operation, got %+v", err)
		}
		_, err = client.ResponsesStreamRequest(newCtx(t), &schemas.BifrostResponsesRequest{Provider: name, Model: "step-5-preview", Input: input})
		if err == nil || err.Error == nil || err.Error.Code == nil || *err.Error.Code != "unsupported_operation" {
			t.Fatalf("stream: want unsupported_operation, got %+v", err)
		}
		if got := paths(); len(got) != 0 {
			t.Fatalf("a refused request reached the upstream: %v", got)
		}
	})
}
