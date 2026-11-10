package schemas

import (
	"strings"
	"testing"
)

func TestMergeBifrostLLMUsage(t *testing.T) {
	base := &BifrostLLMUsage{
		PromptTokens:     10,
		CompletionTokens: 5,
		TotalTokens:      15,
		PromptTokensDetails: &ChatPromptTokensDetails{
			TextTokens:        3,
			AudioTokens:       2,
			ImageTokens:       1,
			CachedReadTokens:  4,
			CachedWriteTokens: 6,
			CachedWriteTokenDetails: &ChatCachedWriteTokenDetails{
				CachedWriteTokens5m: 2,
				CachedWriteTokens1h: 4,
			},
		},
		CompletionTokensDetails: &ChatCompletionTokensDetails{
			TextTokens:               1,
			AcceptedPredictionTokens: 2,
			AudioTokens:              3,
			CitationTokens:           new(4),
			NumSearchQueries:         new(5),
			ReasoningTokens:          6,
			ImageTokens:              new(7),
			RejectedPredictionTokens: 8,
		},
		ToolUsage: &ToolUsage{WebSearch: &WebSearchToolUsage{NumRequests: 2}},
		Cost: &BifrostCost{
			InputCost:             1,
			InputCostDetails:      &InputCostDetails{TextCost: 1},
			OutputCost:            2,
			OutputCostDetails:     &OutputCostDetails{TextCost: 2},
			AdditionalCost:        3,
			AdditionalCostDetails: &AdditionalCostDetails{GuardrailCost: 3},
			TotalCost:             6,
		},
	}
	add := &BifrostLLMUsage{
		PromptTokens:     20,
		CompletionTokens: 7,
		TotalTokens:      27,
		PromptTokensDetails: &ChatPromptTokensDetails{
			TextTokens:        5,
			AudioTokens:       4,
			ImageTokens:       3,
			CachedReadTokens:  2,
			CachedWriteTokens: 1,
			CachedWriteTokenDetails: &ChatCachedWriteTokenDetails{
				CachedWriteTokens5m: 8,
				CachedWriteTokens1h: 9,
			},
		},
		CompletionTokensDetails: &ChatCompletionTokensDetails{
			TextTokens:               8,
			AcceptedPredictionTokens: 9,
			AudioTokens:              10,
			CitationTokens:           new(11),
			NumSearchQueries:         new(12),
			ReasoningTokens:          13,
			ImageTokens:              new(14),
			RejectedPredictionTokens: 15,
		},
		ToolUsage: &ToolUsage{WebSearch: &WebSearchToolUsage{NumRequests: 3}},
		Cost: &BifrostCost{
			InputCost:             10,
			InputCostDetails:      &InputCostDetails{TextCost: 10},
			OutputCost:            20,
			OutputCostDetails:     &OutputCostDetails{TextCost: 20},
			AdditionalCost:        30,
			AdditionalCostDetails: &AdditionalCostDetails{GuardrailCost: 30},
			TotalCost:             60,
		},
	}

	merged := MergeBifrostLLMUsage(base, add)
	if merged.PromptTokens != 30 || merged.CompletionTokens != 12 || merged.TotalTokens != 42 {
		t.Fatalf("unexpected token totals: %+v", merged)
	}
	if got := merged.ToolUsage.WebSearch.NumRequests; got != 5 {
		t.Fatalf("tool_usage web search = %d, want 5", got)
	}
	if merged.ToolUsage == base.ToolUsage || merged.ToolUsage == add.ToolUsage {
		t.Fatal("merged tool_usage aliases an input")
	}
	if merged.PromptTokensDetails.TextTokens != 8 ||
		merged.PromptTokensDetails.AudioTokens != 6 ||
		merged.PromptTokensDetails.ImageTokens != 4 ||
		merged.PromptTokensDetails.CachedReadTokens != 6 ||
		merged.PromptTokensDetails.CachedWriteTokens != 7 {
		t.Fatalf("unexpected prompt details: %+v", merged.PromptTokensDetails)
	}
	if merged.PromptTokensDetails.CachedWriteTokenDetails.CachedWriteTokens5m != 10 ||
		merged.PromptTokensDetails.CachedWriteTokenDetails.CachedWriteTokens1h != 13 {
		t.Fatalf("unexpected cache write details: %+v", merged.PromptTokensDetails.CachedWriteTokenDetails)
	}
	if merged.CompletionTokensDetails.TextTokens != 9 ||
		merged.CompletionTokensDetails.AcceptedPredictionTokens != 11 ||
		merged.CompletionTokensDetails.AudioTokens != 13 ||
		*merged.CompletionTokensDetails.CitationTokens != 15 ||
		*merged.CompletionTokensDetails.NumSearchQueries != 17 ||
		merged.CompletionTokensDetails.ReasoningTokens != 19 ||
		*merged.CompletionTokensDetails.ImageTokens != 21 ||
		merged.CompletionTokensDetails.RejectedPredictionTokens != 23 {
		t.Fatalf("unexpected completion details: %+v", merged.CompletionTokensDetails)
	}
	if merged.Cost.InputCost != 11 ||
		merged.Cost.OutputCost != 22 ||
		merged.Cost.AdditionalCost != 33 ||
		merged.Cost.TotalCost != 66 ||
		merged.Cost.InputCostDetails.TextCost != 11 ||
		merged.Cost.OutputCostDetails.TextCost != 22 ||
		merged.Cost.AdditionalCostDetails.GuardrailCost != 33 {
		t.Fatalf("unexpected cost: %+v", merged.Cost)
	}
}

func TestMergeBifrostLLMUsageNilInputs(t *testing.T) {
	usage := &BifrostLLMUsage{PromptTokens: 3, TotalTokens: 3}

	if got := MergeBifrostLLMUsage(nil, nil); got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}
	if got := MergeBifrostLLMUsage(usage, nil); got != usage {
		t.Fatalf("expected base usage returned for nil add")
	}
	if got := MergeBifrostLLMUsage(nil, usage); got != usage {
		t.Fatalf("expected added usage returned for nil base")
	}
}

func TestToolUsageRoundTripsResponsesAndChatUsage(t *testing.T) {
	ru := &ResponsesResponseUsage{TotalTokens: 1, ToolUsage: &ToolUsage{WebSearch: &WebSearchToolUsage{NumRequests: 4}}}
	cu := ru.ToBifrostLLMUsage()
	if got := cu.ToolUsage.WebSearch.NumRequests; got != 4 {
		t.Fatalf("chat tool_usage = %d, want 4", got)
	}
	if got := cu.ToResponsesResponseUsage().ToolUsage.WebSearch.NumRequests; got != 4 {
		t.Fatalf("responses tool_usage = %d, want 4", got)
	}
	if cp := cu.DeepCopy(); cp.ToolUsage == cu.ToolUsage || cp.ToolUsage.WebSearch.NumRequests != 4 {
		t.Fatal("DeepCopy must own tool_usage")
	}
	var parsed ResponsesResponseUsage
	if err := Unmarshal([]byte(`{"total_tokens":1,"tool_usage":{"web_search":{"num_requests":1}}}`), &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.ToolUsage.WebSearch.NumRequests != 1 {
		t.Fatal("tool_usage.web_search.num_requests not decoded")
	}
}

func TestToolUsageAddHandlesNilOperands(t *testing.T) {
	var none *ToolUsage
	if none.Add(nil) != nil {
		t.Fatal("nil + nil must stay nil")
	}
	one := &ToolUsage{WebSearch: &WebSearchToolUsage{NumRequests: 2}}
	for _, sum := range []*ToolUsage{none.Add(one), one.Add(nil)} {
		if sum == one || sum.WebSearch == one.WebSearch || sum.WebSearch.NumRequests != 2 {
			t.Fatalf("sum with nil must be an owned copy of the other side, got %+v", sum)
		}
	}
	if got := (&ToolUsage{}).Add(one).WebSearch.NumRequests; got != 2 {
		t.Fatalf("empty + one = %d, want 2", got)
	}
}

// usageRoundTrip marshals v, checks every want substring appears, unmarshals into a fresh value
// and marshals that again: the second encoding must equal the first.
func usageRoundTrip[T any](t *testing.T, v *T, want ...string) *T {
	t.Helper()
	out, err := Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, w := range want {
		if !strings.Contains(string(out), w) {
			t.Fatalf("%s missing from %s", w, out)
		}
	}
	back := new(T)
	if err := Unmarshal(out, back); err != nil {
		t.Fatalf("unmarshal %s: %v", out, err)
	}
	again, err := Marshal(back)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if string(again) != string(out) {
		t.Fatalf("round trip changed the encoding:\n first %s\nsecond %s", out, again)
	}
	return back
}

func assertNoKeys(t *testing.T, v any, keys ...string) {
	t.Helper()
	out, err := Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, k := range keys {
		if strings.Contains(string(out), `"`+k+`"`) {
			t.Fatalf("empty %s serialised in %s", k, out)
		}
	}
}

// TXE-6518 (contract C5 BF6): the six new image usage fields serialise, survive a round trip and
// are omitted when the vendor reported none. A vendor-reported zero is kept, not dropped.
func TestImageUsageVendorFields(t *testing.T) {
	u := &ImageUsage{
		InputTokens:         10,
		OutputTokens:        1290,
		OutputTokensDetails: &ImageTokenDetails{ImageTokens: 1290, ReasoningTokens: new(212)},
		OutputImages:        new(2),
		InputImages:         new(0),
		OutputMegapixels:    new(2.1),
		InputMegapixels:     new(1.05),
		VendorCost:          &VendorCost{Amount: 0.08, Unit: VendorCostUnitUSD},
	}
	back := usageRoundTrip(t, u,
		`"output_images":2`, `"input_images":0`, `"output_megapixels":2.1`, `"input_megapixels":1.05`,
		`"reasoning_tokens":212`, `"vendor_cost":{"amount":0.08,"unit":"usd"}`)
	if back.OutputTokensDetails.ReasoningTokens == nil || *back.OutputTokensDetails.ReasoningTokens != 212 {
		t.Fatalf("reasoning_tokens lost: %+v", back.OutputTokensDetails)
	}

	empty := &ImageUsage{InputTokens: 10, OutputTokensDetails: &ImageTokenDetails{ImageTokens: 5}, NumInputImages: 3}
	assertNoKeys(t, empty, "output_images", "input_images", "output_megapixels", "input_megapixels", "reasoning_tokens", "vendor_cost")
}

// DeepCopy must not share the new pointers: cost calculation derives values from a copy.
func TestImageUsageDeepCopyVendorFields(t *testing.T) {
	u := &ImageUsage{
		OutputTokensDetails: &ImageTokenDetails{ReasoningTokens: new(1)},
		InputTokensDetails:  &ImageTokenDetails{ReasoningTokens: new(1)},
		OutputImages:        new(1), InputImages: new(1),
		OutputMegapixels: new(1.0), InputMegapixels: new(1.0),
		VendorCost: &VendorCost{Amount: 1, Unit: VendorCostUnitCredit},
	}
	c := u.DeepCopy()
	*c.OutputTokensDetails.ReasoningTokens = 9
	*c.InputTokensDetails.ReasoningTokens = 9
	*c.OutputImages, *c.InputImages = 9, 9
	*c.OutputMegapixels, *c.InputMegapixels = 9, 9
	c.VendorCost.Amount = 9
	if *u.OutputTokensDetails.ReasoningTokens != 1 || *u.InputTokensDetails.ReasoningTokens != 1 ||
		*u.OutputImages != 1 || *u.InputImages != 1 || *u.OutputMegapixels != 1 || *u.InputMegapixels != 1 ||
		u.VendorCost.Amount != 1 {
		t.Fatalf("DeepCopy shares state with the original: %+v", u)
	}
}

// TXE-6518 (contract C5 BF6): the task object's six new usage fields.
func TestVideoUsageVendorFields(t *testing.T) {
	u := &VideoUsage{
		OutputTokens:   new(108900),
		OutputSeconds:  new(5.0),
		InputSeconds:   new(2.5),
		InputImages:    new(1),
		ComputeSeconds: new(41.7),
		VendorCost:     &VendorCost{Amount: 140, Unit: VendorCostUnitUnit},
	}
	usageRoundTrip(t, u,
		`"output_tokens":108900`, `"output_seconds":5`, `"input_seconds":2.5`, `"input_images":1`,
		`"compute_seconds":41.7`, `"vendor_cost":{"amount":140,"unit":"unit"}`)
	assertNoKeys(t, &VideoUsage{Cost: &BifrostCost{TotalCost: 0.5}},
		"output_tokens", "output_seconds", "input_seconds", "input_images", "compute_seconds", "vendor_cost")

	// The task object carries it under usage.
	out, err := Marshal(&BifrostVideoGenerationResponse{ID: "v", Usage: &VideoUsage{OutputSeconds: new(5.0)}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(out), `"usage":{"output_seconds":5}`) {
		t.Fatalf("task usage = %s", out)
	}
}

// TXE-6518 (contract C5 BF6): TTS usage's two new fields, next to Bifrost's own input_chars.
func TestSpeechUsageVendorFields(t *testing.T) {
	u := &SpeechUsage{InputChars: 11, BilledCharacters: new(12), VendorCost: &VendorCost{Amount: 12, Unit: VendorCostUnitCredit}}
	back := usageRoundTrip(t, u, `"input_chars":11`, `"billed_characters":12`, `"vendor_cost":{"amount":12,"unit":"credit"}`)
	if back.InputChars != 11 || *back.BilledCharacters != 12 {
		t.Fatalf("input_chars and billed_characters must stay separate: %+v", back)
	}
	assertNoKeys(t, &SpeechUsage{InputChars: 11, AudioSeconds: 3}, "billed_characters", "vendor_cost")

	// The SSE done event carries it under usage.
	out, err := Marshal(&BifrostSpeechStreamResponse{Type: SpeechStreamResponseTypeDone, Usage: &SpeechUsage{BilledCharacters: new(5)}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(out), `"billed_characters":5`) {
		t.Fatalf("done event = %s", out)
	}
}

// A reported zero charge is kept as amount 0; the four contract units are the only valid ones.
func TestVendorCost(t *testing.T) {
	usageRoundTrip(t, &VendorCost{Amount: 0, Unit: VendorCostUnitUSDTicks}, `{"amount":0,"unit":"usd_ticks"}`)
	usageRoundTrip(t, &VendorCost{Amount: 12345678901, Unit: VendorCostUnitUSDTicks}, `"amount":12345678901`)
	for _, u := range []VendorCostUnit{VendorCostUnitUSD, VendorCostUnitUSDTicks, VendorCostUnitCredit, VendorCostUnitUnit} {
		if !u.IsValid() {
			t.Errorf("%q should be valid", u)
		}
	}
	for _, u := range []VendorCostUnit{"", "USD", "eur", "credits"} {
		if u.IsValid() {
			t.Errorf("%q should not be valid", u)
		}
	}
}
