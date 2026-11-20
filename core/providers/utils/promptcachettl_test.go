package utils

import (
	"context"
	"strings"
	"testing"

	"github.com/capsohq/bifrost/core/schemas"
)

// BenchmarkPromptCacheTTLInBody states what reading the TTL from the body costs each request
// (fork, TXE-7996): a 4 KB and a 256 KB chat body without the field, and one with it at the end.
func BenchmarkPromptCacheTTLInBody(b *testing.B) {
	message := `{"role":"user","content":"` + strings.Repeat("lorem ipsum ", 30) + `"},`
	body := func(kib int, tail string) []byte {
		messages := strings.Repeat(message, kib*1024/len(message))
		return []byte(`{"model":"kimi-k3","messages":[` + strings.TrimSuffix(messages, ",") + `]` + tail + `}`)
	}
	for _, c := range []struct {
		name string
		body []byte
	}{
		{"4KiB without ttl", body(4, "")},
		{"256KiB without ttl", body(256, "")},
		{"256KiB with ttl", body(256, `,"prompt_cache_options":{"ttl":"1h"}`)},
	} {
		b.Run(c.name, func(b *testing.B) {
			b.SetBytes(int64(len(c.body)))
			b.ReportAllocs()
			for b.Loop() {
				_ = PromptCacheTTLInBody(c.body)
			}
		})
	}
}

// TestPromptCacheTTLIsReadFromTheBodyOnlyForARequestTTLPricedBase: the body is scanned only on an
// attempt whose base provider bills a cache write by the requested TTL (fork, TXE-7996), so no
// other provider pays for the scan; for those, the stamp is the body's TTL, "" when it has none.
func TestPromptCacheTTLIsReadFromTheBodyOnlyForARequestTTLPricedBase(t *testing.T) {
	body := []byte(`{"model":"m","prompt_cache_options":{"ttl":"1h"}}`)
	for _, c := range []struct {
		base schemas.ModelProvider
		body []byte
		want string
	}{
		{schemas.Moonshot, body, "1h"},
		{schemas.Moonshot, []byte(`{"model":"m"}`), ""},
		{schemas.OpenAI, body, "typed"},
		{"", body, "typed"},
	} {
		ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
		ctx.SetValue(schemas.BifrostContextKeyRequestedPromptCacheTTL, "typed")
		if c.base != "" {
			ctx.SetValue(schemas.BifrostContextKeyBaseProviderType, c.base)
		}
		stampPromptCacheTTLFromBody(ctx, c.body)
		if got, _ := ctx.Value(schemas.BifrostContextKeyRequestedPromptCacheTTL).(string); got != c.want {
			t.Errorf("base %q: stamped %q, want %q", c.base, got, c.want)
		}
	}
	for _, base := range []schemas.ModelProvider{schemas.OpenAI, schemas.Anthropic, schemas.Gemini} {
		if _, listed := schemas.RequestTTLPricedCacheWrites[base]; listed {
			t.Fatalf("%s reports its cache-write tier; it must not be listed", base)
		}
	}
}

// BenchmarkStampPromptCacheTTLFromBody: the per-request cost on an attempt whose base provider is
// not listed (every provider but Kimi today), where the body is not scanned (fork, TXE-7996).
func BenchmarkStampPromptCacheTTLFromBody(b *testing.B) {
	body := []byte(`{"model":"m","messages":[` + strings.Repeat(`{"role":"user","content":"hi"},`, 8000) + `{"role":"user","content":"hi"}]}`)
	for _, base := range []schemas.ModelProvider{schemas.OpenAI, schemas.Moonshot} {
		b.Run(string(base), func(b *testing.B) {
			ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
			ctx.SetValue(schemas.BifrostContextKeyBaseProviderType, base)
			b.ReportAllocs()
			for b.Loop() {
				stampPromptCacheTTLFromBody(ctx, body)
			}
		})
	}
}
