package schemas

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// largePayloadWires are the integration types a native route stamps on the context, plus ""
// for the neutral /v1 routes and Go SDK callers.
var largePayloadWires = []string{"", "openai", "anthropic", "genai", "bedrock", "cohere", "typesafe"}

// largePayloadModels covers the model families a multi-family provider dispatches on.
var largePayloadModels = []string{"", "gpt-5", "claude-sonnet-4-5", "gemini-2.5-pro", "mistral-large", "1234567890"}

// declaredRequestTypes reads every RequestType constant from bifrost.go, so an operation
// added later is in the matrix without being listed here.
func declaredRequestTypes(t testing.TB) []RequestType {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "bifrost.go", nil, 0)
	if err != nil {
		t.Fatalf("parse bifrost.go: %v", err)
	}
	var requestTypes []RequestType
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			value := spec.(*ast.ValueSpec)
			if ident, ok := value.Type.(*ast.Ident); !ok || ident.Name != "RequestType" {
				continue
			}
			for _, expr := range value.Values {
				if lit, ok := expr.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					name, _ := strconv.Unquote(lit.Value)
					requestTypes = append(requestTypes, RequestType(name))
				}
			}
		}
	}
	if len(requestTypes) < 60 {
		t.Fatalf("found %d RequestType constants in bifrost.go, want the full set (60 or more)", len(requestTypes))
	}
	return requestTypes
}

func largePayloadCtx(wire string) *BifrostContext {
	ctx := NewBifrostContext(context.Background(), NoDeadline)
	if wire != "" {
		ctx.SetValue(BifrostContextKeyIntegrationType, wire)
	}
	return ctx
}

// A new provider states which wire its vendor takes unchanged, or that it converts. Without
// an entry it converts everything (the safe default); this test makes the entry explicit.
func TestLargePayloadDeclarationsCoverEveryProvider(t *testing.T) {
	standard := map[ModelProvider]bool{}
	for _, provider := range StandardProviders {
		standard[provider] = true
		declaration, ok := largePayloadDeclarations[provider]
		if !ok {
			t.Errorf("%s has no entry in largePayloadDeclarations (core/schemas/largepayload.go): say which wire format its vendor takes unchanged, or why every request is converted", provider)
			continue
		}
		if (declaration.nativeWire == nil) == (declaration.converts == "") {
			t.Errorf("%s must declare exactly one of a native wire and a reason it converts", provider)
		}
	}
	for provider := range largePayloadDeclarations {
		if !standard[provider] {
			t.Errorf("largePayloadDeclarations has %s, which is not a standard provider", provider)
		}
	}
}

// The rule over every provider, operation, route, model family and custom flag: this is the
// check that fails when a request or response that needs conversion would be passed through
// raw. It also pins the complete list of provider and route pairs that may skip conversion,
// so widening it is a visible change to this file.
func TestLargePayloadEligibilityMatrix(t *testing.T) {
	requestTypes := declaredRequestTypes(t)
	rawPairs := map[string]bool{}
	cells := 0
	for _, provider := range StandardProviders {
		for _, wire := range largePayloadWires {
			ctx := largePayloadCtx(wire)
			for _, requestType := range requestTypes {
				passthrough := requestType == PassthroughRequest || requestType == PassthroughStreamRequest
				for _, model := range largePayloadModels {
					for _, custom := range []bool{false, true} {
						cells++
						got := DecideLargePayloadEligibility(ctx, provider, custom, requestType, model)
						where := fmt.Sprintf("%s wire=%q %s model=%q custom=%v", provider, wire, requestType, model, custom)
						if passthrough {
							if !got.RawRequest || !got.RawResponse {
								t.Errorf("%s: a passthrough request forwards the caller's bytes, got %+v", where, got)
							}
							continue
						}
						if got.RawRequest && wire == "" {
							t.Errorf("%s: a unified request (no native route) was allowed through unconverted", where)
						}
						if got.RawRequest && custom {
							t.Errorf("%s: a custom provider's request was allowed through unconverted", where)
						}
						if got.RawRequest && !got.RawResponse {
							t.Errorf("%s: raw request without raw response: the caller's wire is the vendor's in both directions or in neither", where)
						}
						if got.RawResponse && !got.RawRequest && !largePayloadOpaqueResponse(requestType) {
							t.Errorf("%s: a response that carries a unified body would be streamed raw", where)
						}
						if largePayloadOpaqueResponse(requestType) && !got.RawResponse {
							t.Errorf("%s: opaque content has nothing to convert and must stay streamable", where)
						}
						if (!got.RawRequest || !got.RawResponse) && got.Reason == "" {
							t.Errorf("%s: a refusal must say why", where)
						}
						if got.RawRequest {
							rawPairs[string(provider)+" on "+wire] = true
						}
					}
				}
			}
		}
	}

	var got []string
	for pair := range rawPairs {
		got = append(got, pair)
	}
	sort.Strings(got)
	want := []string{
		"anthropic on anthropic",
		"azure on openai",
		"cohere on cohere",
		"gemini on genai",
		"openai on openai",
		"typesafe on typesafe",
		"vertex on genai",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("provider and route pairs that may skip conversion:\n got: %v\nwant: %v", got, want)
	}
	t.Logf("%d cells checked: %d providers, %d routes, %d operations, %d models, custom or not", cells, len(StandardProviders), len(largePayloadWires), len(requestTypes), len(largePayloadModels))
}

// The multi-family providers are native only for the family whose body they forward as
// written: a Claude body on Azure or Vertex is rewritten, so it is converted.
func TestLargePayloadEligibilityByModelFamily(t *testing.T) {
	for _, tt := range []struct {
		provider ModelProvider
		wire     string
		model    string
		wantRaw  bool
	}{
		{Azure, "openai", "gpt-5", true},
		{Azure, "openai", "claude-sonnet-4-5", false},
		{Azure, "anthropic", "claude-sonnet-4-5", false},
		{Vertex, "genai", "gemini-2.5-pro", true},
		{Vertex, "genai", "claude-sonnet-4-5", false},
		{Vertex, "anthropic", "claude-sonnet-4-5", false},
		{BedrockMantle, "anthropic", "claude-sonnet-4-5", false},
		{Bedrock, "bedrock", "anthropic.claude-sonnet-4-5", false},
		{Anthropic, "anthropic", "claude-sonnet-4-5", true},
		{Anthropic, "openai", "claude-sonnet-4-5", false},
		{OpenAI, "anthropic", "gpt-5", false},
		{Mistral, "openai", "mistral-large", false},
		{Elevenlabs, "openai", "eleven_v3", false},
	} {
		got := DecideLargePayloadEligibility(largePayloadCtx(tt.wire), tt.provider, false, ChatCompletionRequest, tt.model)
		if got.RawRequest != tt.wantRaw || got.RawResponse != tt.wantRaw {
			t.Errorf("%s on %s with %s: got %+v, want raw=%v", tt.provider, tt.wire, tt.model, got, tt.wantRaw)
		}
	}
}

// A plugin that must read or rewrite the body turns raw passthrough off for every route,
// including a passthrough request, and its reason reaches the caller.
func TestRequireBodyConversionTurnsRawOff(t *testing.T) {
	for _, requestType := range []RequestType{ChatCompletionRequest, PassthroughRequest, PassthroughStreamRequest, FileContentRequest} {
		ctx := largePayloadCtx("anthropic")
		RequireBodyConversion(ctx, "the redaction guardrail rewrites message text")
		got := DecideLargePayloadEligibility(ctx, Anthropic, false, requestType, "claude-sonnet-4-5")
		if got.RawRequest || got.RawResponse {
			t.Errorf("%s: got %+v, want nothing raw", requestType, got)
		}
		if got.Reason != "the redaction guardrail rewrites message text" {
			t.Errorf("%s: reason = %q", requestType, got.Reason)
		}
	}
}

// With no stamp on the context nothing is raw: a provider called outside core's dispatch, or
// a new code path that forgets the decision, converts.
func TestLargePayloadEligibilityFromContextDefaultsToConverted(t *testing.T) {
	ctx := NewBifrostContext(context.Background(), NoDeadline)
	if got := LargePayloadEligibilityFromContext(ctx); got.RawRequest || got.RawResponse || got.Reason == "" {
		t.Errorf("no stamp: got %+v, want nothing raw and a reason", got)
	}
	ctx.SetValue(BifrostContextKeyLargePayloadEligibility, LargePayloadEligibility{RawRequest: true, RawResponse: true})
	if got := LargePayloadEligibilityFromContext(ctx); !got.RawRequest || !got.RawResponse {
		t.Errorf("stamped: got %+v", got)
	}
}

func TestNewLargePayloadNotConvertibleError(t *testing.T) {
	err := NewLargePayloadNotConvertibleError(Mistral, ChatCompletionRequest, "mistral converts")
	if err.StatusCode == nil || *err.StatusCode != 413 {
		t.Fatalf("status = %v, want 413", err.StatusCode)
	}
	if err.ExtraFields.ErrorType != ErrorTypeCallerInvalidRequest {
		t.Errorf("error_type = %q", err.ExtraFields.ErrorType)
	}
	if !strings.Contains(err.Error.Message, "chat_completion request must be converted: mistral converts") {
		t.Errorf("message = %q", err.Error.Message)
	}
}

// The decision runs once per provider attempt, and only when a large-payload mode is on.
func BenchmarkDecideLargePayloadEligibility(b *testing.B) {
	for _, bench := range []struct {
		name     string
		wire     string
		provider ModelProvider
		model    string
	}{
		{"native", "openai", OpenAI, "gpt-5"},
		{"converted", "openai", Mistral, "mistral-large"},
		{"neutral", "", Cohere, "embed-v4.0"},
	} {
		b.Run(bench.name, func(b *testing.B) {
			ctx := largePayloadCtx(bench.wire)
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = DecideLargePayloadEligibility(ctx, bench.provider, false, ChatCompletionRequest, bench.model)
			}
		})
	}
}
