package schemas

import (
	"reflect"
	"sync"
	"testing"
)

// Core repopulates the extra fields after RunPostLLMHooks while plugins (governance billing,
// logging, telemetry) still read the response from goroutines their post-hooks started. With
// the values unchanged, that second pass must not write, or -race reports it.
func TestPopulateExtraFieldsLeavesUnchangedFieldsUnwritten(t *testing.T) {
	resp := &BifrostResponse{ChatResponse: &BifrostChatResponse{}}
	resp.PopulateExtraFields(ChatCompletionRequest, OpenAI, "gpt-6", "gpt-6")

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 100 {
			ef := resp.ChatResponse.ExtraFields
			_ = ef.RequestType
			_ = ef.Provider
			_ = ef.OriginalModelRequested
			_ = ef.ResolvedModelUsed
		}
	}()
	for range 100 {
		resp.PopulateExtraFields(ChatCompletionRequest, OpenAI, "gpt-6", "gpt-6")
	}
	wg.Wait()
}

func TestPopulateExtraFieldsOverwritesChangedFields(t *testing.T) {
	resp := &BifrostResponse{ChatResponse: &BifrostChatResponse{}}
	resp.ChatResponse.ExtraFields.RequestType = TextCompletionRequest
	resp.ChatResponse.ExtraFields.Provider = Anthropic
	resp.ChatResponse.ExtraFields.OriginalModelRequested = "stale"
	resp.ChatResponse.ExtraFields.ResolvedModelUsed = "stale-resolved"

	resp.PopulateExtraFields(ChatCompletionRequest, OpenAI, "gpt-6", "")

	ef := resp.ChatResponse.ExtraFields
	if ef.RequestType != ChatCompletionRequest || ef.Provider != OpenAI || ef.OriginalModelRequested != "gpt-6" || ef.ResolvedModelUsed != "gpt-6" {
		t.Fatalf("changed fields not written: %+v", ef)
	}
}

// BenchmarkPopulateExtraFieldsUnchanged measures the post-hook repopulation, where the values
// already match the pre-hook pass.
func BenchmarkPopulateExtraFieldsUnchanged(b *testing.B) {
	resp := &BifrostResponse{ChatResponse: &BifrostChatResponse{}}
	resp.PopulateExtraFields(ChatCompletionRequest, OpenAI, "gpt-6", "gpt-6")
	b.ReportAllocs()
	for b.Loop() {
		resp.PopulateExtraFields(ChatCompletionRequest, OpenAI, "gpt-6", "gpt-6")
	}
}

// TXE-7069: pricing costs a cache write by the request's prompt_cache_options.ttl for a vendor that
// reports no tier, and core reads that TTL through RequestedPromptCacheTTL. Every request kind on
// BifrostRequest whose parameters carry PromptCacheOptions is found by reflection, so a new one is
// covered here without anyone listing it, and fails until RequestedPromptCacheTTL reads it.
func TestRequestedPromptCacheTTLReadsEveryRequestWithPromptCacheOptions(t *testing.T) {
	optsType := reflect.TypeOf((*PromptCacheOptions)(nil))
	reqType := reflect.TypeOf(BifrostRequest{})
	found := 0
	for i := 0; i < reqType.NumField(); i++ {
		field := reqType.Field(i)
		if field.Type.Kind() != reflect.Pointer || field.Type.Elem().Kind() != reflect.Struct {
			continue
		}
		params, ok := field.Type.Elem().FieldByName("Params")
		if !ok || params.Type.Kind() != reflect.Pointer || params.Type.Elem().Kind() != reflect.Struct {
			continue
		}
		opts, ok := params.Type.Elem().FieldByName("PromptCacheOptions")
		if !ok || opts.Type != optsType {
			continue
		}
		found++
		t.Run(field.Name, func(t *testing.T) {
			ttl := "1h"
			paramsValue := reflect.New(params.Type.Elem())
			paramsValue.Elem().FieldByIndex(opts.Index).Set(reflect.ValueOf(&PromptCacheOptions{TTL: &ttl}))
			request := reflect.New(field.Type.Elem())
			request.Elem().FieldByIndex(params.Index).Set(paramsValue)
			var br BifrostRequest
			reflect.ValueOf(&br).Elem().Field(i).Set(request)
			if got := br.RequestedPromptCacheTTL(); got != ttl {
				t.Fatalf("BifrostRequest.%s with prompt_cache_options.ttl %q: RequestedPromptCacheTTL() = %q; read it there", field.Name, ttl, got)
			}
		})
	}
	if found < 2 {
		t.Fatalf("found %d request kinds with PromptCacheOptions, want at least chat and responses", found)
	}

	if got := (&BifrostRequest{ChatRequest: &BifrostChatRequest{Params: &ChatParameters{}}}).RequestedPromptCacheTTL(); got != "" {
		t.Fatalf("a request without prompt_cache_options: RequestedPromptCacheTTL() = %q, want \"\"", got)
	}
	if got := (*BifrostRequest)(nil).RequestedPromptCacheTTL(); got != "" {
		t.Fatalf("nil request: RequestedPromptCacheTTL() = %q, want \"\"", got)
	}
}

// The dispatch tests in package core spell the key out so they fail on a tree without it by
// assertion; this pins that spelling.
func TestRequestedPromptCacheTTLKeyIsStable(t *testing.T) {
	if BifrostContextKeyRequestedPromptCacheTTL != BifrostContextKey("bifrost-requested-prompt-cache-ttl") {
		t.Fatalf("BifrostContextKeyRequestedPromptCacheTTL = %q; update requestedPromptCacheTTLKey in core/promptcachedispatch_test.go", BifrostContextKeyRequestedPromptCacheTTL)
	}
}
