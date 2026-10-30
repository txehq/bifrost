package schemas

import (
	"encoding/json"
	"strings"
	"testing"
)

// ChatParameters' reasoning union accepts clients that mirror the same
// directive in both spellings (flat reasoning_* shorthand plus the reasoning
// object) and canonicalizes to the object form, while still rejecting
// contradictory values. Agents built on ai-sdk are known to emit every vendor
// dialect at once — reasoning_effort and reasoning.effort carrying the same
// value — and the previous "both present" rejection 400'd those requests
// before routing.
func TestChatParametersReasoningUnion(t *testing.T) {
	t.Run("duplicate spellings with equal values are accepted", func(t *testing.T) {
		var cp ChatParameters
		err := Unmarshal([]byte(`{"reasoning_effort":"high","reasoning":{"effort":"high"}}`), &cp)
		if err != nil {
			t.Fatalf("equal duplicate effort should decode, got %v", err)
		}
		if cp.Reasoning == nil || cp.Reasoning.Effort == nil || *cp.Reasoning.Effort != "high" {
			t.Fatalf("effort should canonicalize onto the reasoning object, got %+v", cp.Reasoning)
		}

		// Re-encoding must carry exactly one spelling so the union invariant
		// still holds on the wire.
		out, err := json.Marshal(&cp)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if strings.Contains(string(out), "reasoning_effort") {
			t.Fatalf("marshalled payload should drop the shorthand, got %s", out)
		}
	})

	t.Run("conflicting effort values are rejected", func(t *testing.T) {
		var cp ChatParameters
		err := Unmarshal([]byte(`{"reasoning_effort":"high","reasoning":{"effort":"max"}}`), &cp)
		if err == nil {
			t.Fatal("conflicting effort values should error")
		}
		if !strings.Contains(err.Error(), "conflicts with reasoning.effort") {
			t.Fatalf("error should name the conflict, got %v", err)
		}
	})

	t.Run("shorthand and object max_tokens agree", func(t *testing.T) {
		var cp ChatParameters
		err := Unmarshal([]byte(`{"reasoning_max_tokens":2048,"reasoning":{"max_tokens":2048}}`), &cp)
		if err != nil {
			t.Fatalf("equal duplicate max_tokens should decode, got %v", err)
		}
		if cp.Reasoning == nil || cp.Reasoning.MaxTokens == nil || *cp.Reasoning.MaxTokens != 2048 {
			t.Fatalf("max_tokens should canonicalize onto the reasoning object, got %+v", cp.Reasoning)
		}
	})

	t.Run("conflicting max_tokens values are rejected", func(t *testing.T) {
		var cp ChatParameters
		err := Unmarshal([]byte(`{"reasoning_max_tokens":2048,"reasoning":{"max_tokens":4096}}`), &cp)
		if err == nil {
			t.Fatal("conflicting max_tokens values should error")
		}
	})

	t.Run("duplicate display agrees, conflict rejected", func(t *testing.T) {
		var cp ChatParameters
		if err := Unmarshal([]byte(`{"reasoning_display":"summarized","reasoning":{"display":"summarized"}}`), &cp); err != nil {
			t.Fatalf("equal duplicate display should decode, got %v", err)
		}
		if err := Unmarshal([]byte(`{"reasoning_display":"summarized","reasoning":{"display":"omitted"}}`), &cp); err == nil {
			t.Fatal("conflicting display values should error")
		}
	})

	t.Run("single-spelling requests keep working", func(t *testing.T) {
		var shorthand, object ChatParameters
		if err := Unmarshal([]byte(`{"reasoning_effort":"low"}`), &shorthand); err != nil {
			t.Fatalf("shorthand-only decode: %v", err)
		}
		if err := Unmarshal([]byte(`{"reasoning":{"effort":"low"}}`), &object); err != nil {
			t.Fatalf("object-only decode: %v", err)
		}
		if shorthand.Reasoning == nil || object.Reasoning == nil ||
			*shorthand.Reasoning.Effort != "low" || *object.Reasoning.Effort != "low" {
			t.Fatal("both single-spelling forms should decode to the same canonical state")
		}
	})
}

// Azure OpenAI decorates a chat completion with a top-level prompt_filter_results
// array and a per-choice content_filter_results object. Clients read them to tell
// "filtered" from "empty", so a response must round-trip them untouched.
func TestChatResponsePreservesAzureContentFilterAnnotations(t *testing.T) {
	const upstream = `{
		"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"gpt-4o",
		"prompt_filter_results":[{"prompt_index":0,"content_filter_results":{"hate":{"filtered":false,"severity":"safe"}}}],
		"choices":[{"index":0,"finish_reason":"stop",
			"message":{"role":"assistant","content":"hello"},
			"content_filter_results":{"violence":{"filtered":false,"severity":"safe"}}}],
		"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`

	var resp BifrostChatResponse
	if err := Unmarshal([]byte(upstream), &resp); err != nil {
		t.Fatalf("decode azure response: %v", err)
	}
	out, err := json.Marshal(&resp)
	if err != nil {
		t.Fatalf("encode response: %v", err)
	}

	var got struct {
		PromptFilterResults []struct {
			ContentFilterResults map[string]struct {
				Severity string `json:"severity"`
			} `json:"content_filter_results"`
		} `json:"prompt_filter_results"`
		Choices []struct {
			ContentFilterResults map[string]struct {
				Severity string `json:"severity"`
			} `json:"content_filter_results"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("re-decode %s: %v", out, err)
	}
	if len(got.PromptFilterResults) != 1 || got.PromptFilterResults[0].ContentFilterResults["hate"].Severity != "safe" {
		t.Fatalf("prompt_filter_results was dropped: %s", out)
	}
	if len(got.Choices) != 1 || got.Choices[0].ContentFilterResults["violence"].Severity != "safe" {
		t.Fatalf("choice content_filter_results was dropped: %s", out)
	}

	t.Run("responses without annotations stay unchanged", func(t *testing.T) {
		var plain BifrostChatResponse
		if err := Unmarshal([]byte(`{"id":"c","object":"chat.completion","created":1,"model":"gpt-4o","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}]}`), &plain); err != nil {
			t.Fatalf("decode: %v", err)
		}
		enc, err := json.Marshal(&plain)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		if strings.Contains(string(enc), "filter_results") {
			t.Fatalf("annotation keys leaked into a response that had none: %s", enc)
		}
	})
}

// reasoning.mode ("standard" | "pro") is an OpenAI Responses-only knob that chat
// callers send inside the reasoning object, next to effort.
func TestChatParametersReasoningMode(t *testing.T) {
	var cp ChatParameters
	if err := Unmarshal([]byte(`{"reasoning":{"effort":"high","mode":"pro"}}`), &cp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cp.Reasoning == nil || cp.Reasoning.Mode == nil || *cp.Reasoning.Mode != "pro" {
		t.Fatalf("reasoning.mode should decode, got %+v", cp.Reasoning)
	}
	if cp.Reasoning.Effort == nil || *cp.Reasoning.Effort != "high" {
		t.Fatalf("reasoning.effort should decode alongside mode, got %+v", cp.Reasoning)
	}
}

func TestBifrostLLMUsageNormalizeReasoningTokens(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want *int
	}{
		{"sgl top-level", `{"completion_tokens":300,"reasoning_tokens":180}`, Ptr(180)},
		{"openai nested", `{"completion_tokens":300,"completion_tokens_details":{"reasoning_tokens":180}}`, Ptr(180)},
		{"nested wins over top-level", `{"completion_tokens_details":{"reasoning_tokens":180},"reasoning_tokens":7}`, Ptr(180)},
		{"zero top-level leaves details nil", `{"completion_tokens":300,"reasoning_tokens":0}`, nil},
		{"absent", `{"completion_tokens":300}`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var u BifrostLLMUsage
			if err := Unmarshal([]byte(tc.in), &u); err != nil {
				t.Fatal(err)
			}
			u.NormalizeReasoningTokens()
			if u.ReasoningTokens != 0 {
				t.Fatalf("expected top-level reasoning_tokens cleared, got %d", u.ReasoningTokens)
			}
			if tc.want == nil {
				if u.CompletionTokensDetails != nil {
					t.Fatalf("expected nil completion_tokens_details, got %+v", u.CompletionTokensDetails)
				}
				return
			}
			if u.CompletionTokensDetails == nil || u.CompletionTokensDetails.ReasoningTokens != *tc.want {
				t.Fatalf("expected reasoning_tokens=%d, got %+v", *tc.want, u.CompletionTokensDetails)
			}
		})
	}
	var nilUsage *BifrostLLMUsage
	nilUsage.NormalizeReasoningTokens()
}

func BenchmarkBifrostLLMUsageUnmarshal(b *testing.B) {
	cases := map[string][]byte{
		"openai": []byte(`{"prompt_tokens":1200,"completion_tokens":300,"total_tokens":1500,"prompt_tokens_details":{"cached_tokens":1024,"audio_tokens":0},"completion_tokens_details":{"reasoning_tokens":180,"audio_tokens":0,"accepted_prediction_tokens":0,"rejected_prediction_tokens":0}}`),
		"sgl":    []byte(`{"prompt_tokens":1200,"completion_tokens":300,"total_tokens":1500,"prompt_tokens_details":{"cached_tokens":1024},"reasoning_tokens":180}`),
	}
	for name, data := range cases {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				var u BifrostLLMUsage
				if err := Unmarshal(data, &u); err != nil {
					b.Fatal(err)
				}
				u.NormalizeReasoningTokens()
			}
		})
	}
}

// TestAddBillingTurn pins what pricing depends on: totals still add up, every provider call
// stays separate however the sum was built, and a single call records no turns.
func TestAddBillingTurn(t *testing.T) {
	a := &BifrostLLMUsage{PromptTokens: 10, CompletionTokens: 1, TotalTokens: 11}
	b := &BifrostLLMUsage{PromptTokens: 20, CompletionTokens: 2, TotalTokens: 22}
	c := &BifrostLLMUsage{PromptTokens: 30, CompletionTokens: 3, TotalTokens: 33}

	if AddBillingTurn(nil, nil) != nil {
		t.Fatal("no usage sums to no usage")
	}
	if got := AddBillingTurn(nil, a); got != a || len(got.BillingTurns) != 0 {
		t.Fatalf("one call is that call, without turns: %+v", got)
	}
	if got := AddBillingTurn(a, nil); got != a {
		t.Fatalf("a turn that reported nothing changes nothing: %+v", got)
	}

	ab := AddBillingTurn(a, b)
	abc := AddBillingTurn(ab, c)
	if abc.PromptTokens != 60 || abc.CompletionTokens != 6 || abc.TotalTokens != 66 {
		t.Fatalf("totals = %+v", abc)
	}
	if len(ab.BillingTurns) != 2 {
		t.Fatalf("an earlier sum must keep its own turns, got %d", len(ab.BillingTurns))
	}
	if len(abc.BillingTurns) != 3 {
		t.Fatalf("turns = %d, want 3", len(abc.BillingTurns))
	}
	for i, want := range []int{10, 20, 30} {
		if abc.BillingTurns[i].PromptTokens != want || len(abc.BillingTurns[i].BillingTurns) != 0 {
			t.Fatalf("turn %d = %+v", i, abc.BillingTurns[i])
		}
	}
	if len(a.BillingTurns) != 0 || len(c.BillingTurns) != 0 {
		t.Fatal("adding must not modify its arguments")
	}

	joined := AddBillingTurn(ab, AddBillingTurn(c, a))
	if len(joined.BillingTurns) != 4 {
		t.Fatalf("two sums keep all their calls, got %d", len(joined.BillingTurns))
	}
}

// TestBillingTurnsDoNotAliasTheirUsage: a recorded turn shares nothing with the usage it came
// from, a deep copy shares nothing with the record, and two sums built from one earlier sum do
// not share a backing array, so nothing later done to one reaches another's bill.
func TestBillingTurnsDoNotAliasTheirUsage(t *testing.T) {
	first := &BifrostLLMUsage{PromptTokens: 10, TotalTokens: 10, PromptTokensDetails: &ChatPromptTokensDetails{CachedReadTokens: 4}}
	second := &BifrostLLMUsage{PromptTokens: 20, TotalTokens: 20}
	sum := AddBillingTurn(first, second)
	if len(sum.BillingTurns) != 2 {
		t.Fatalf("turns = %d, want 2", len(sum.BillingTurns))
	}

	first.PromptTokensDetails.CachedReadTokens = 999
	if got := sum.BillingTurns[0].PromptTokensDetails.CachedReadTokens; got != 4 {
		t.Fatalf("a turn aliases the provider's usage: cached reads = %d", got)
	}

	copied := sum.DeepCopy()
	copied.BillingTurns[0].PromptTokens = 777
	copied.BillingTurns[0].PromptTokensDetails.CachedReadTokens = 777
	if sum.BillingTurns[0].PromptTokens != 10 || sum.BillingTurns[0].PromptTokensDetails.CachedReadTokens != 4 {
		t.Fatalf("a deep copy shares its turns: %+v", sum.BillingTurns[0])
	}

	// Three turns leave spare capacity behind an append, which is what two later sums could share.
	three := AddBillingTurn(sum, &BifrostLLMUsage{PromptTokens: 30, TotalTokens: 30})
	left := AddBillingTurn(three, &BifrostLLMUsage{PromptTokens: 1, TotalTokens: 1})
	right := AddBillingTurn(three, &BifrostLLMUsage{PromptTokens: 2, TotalTokens: 2})
	if left.BillingTurns[3].PromptTokens != 1 || right.BillingTurns[3].PromptTokens != 2 {
		t.Fatalf("sums share a backing array: %d, %d", left.BillingTurns[3].PromptTokens, right.BillingTurns[3].PromptTokens)
	}
}

// TestBillingTurnsStayOffTheWire: the turns are the gateway's own accounting. A client sees
// the totals on chat and Responses usage alike.
func TestBillingTurnsStayOffTheWire(t *testing.T) {
	sum := AddBillingTurn(
		&BifrostLLMUsage{PromptTokens: 10, CompletionTokens: 1, TotalTokens: 11},
		&BifrostLLMUsage{PromptTokens: 20, CompletionTokens: 2, TotalTokens: 22},
	)
	body, err := Marshal(sum)
	if err != nil {
		t.Fatal(err)
	}
	responsesBody, err := Marshal(sum.ToResponsesResponseUsage())
	if err != nil {
		t.Fatal(err)
	}
	for _, wire := range [][]byte{body, responsesBody} {
		if strings.Contains(strings.ToLower(string(wire)), "billing") {
			t.Fatalf("billing turns reached the wire: %s", wire)
		}
	}
	if round := sum.ToResponsesResponseUsage().ToBifrostLLMUsage(); len(round.BillingTurns) != 2 || round.PromptTokens != 30 {
		t.Fatalf("the conversions must carry the turns: %+v", round)
	}
}

// BenchmarkAddBillingTurn measures both shapes on the request path: a single call adds nothing
// (no turn is recorded, nothing is allocated), and a ten-turn loop, the default agent depth,
// is the cost of keeping each call. Run with -benchmem.
func BenchmarkAddBillingTurn(b *testing.B) {
	turn := &BifrostLLMUsage{
		PromptTokens: 150000, CompletionTokens: 1000, TotalTokens: 151000,
		PromptTokensDetails:     &ChatPromptTokensDetails{CachedReadTokens: 1000},
		CompletionTokensDetails: &ChatCompletionTokensDetails{ReasoningTokens: 100},
	}
	b.Run("single", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if AddBillingTurn(nil, turn) != turn {
				b.Fatal("a single call must stay that call")
			}
		}
	})
	b.Run("loop10", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			var total *BifrostLLMUsage
			for range 10 {
				total = AddBillingTurn(total, turn)
			}
			if total.PromptTokens != 1500000 {
				b.Fatalf("prompt tokens = %d", total.PromptTokens)
			}
		}
	})
}
