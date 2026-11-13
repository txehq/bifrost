package schemas

import (
	"encoding/json"
	"reflect"
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

// fillNonZero sets every settable field reachable from v to a non-zero value, allocating
// pointers, slices and maps along the way, so a test can see which fields a copy dropped.
func fillNonZero(t *testing.T, v reflect.Value, seed int) {
	t.Helper()
	fillNonZeroDepth(t, v, seed, 0)
}

func fillNonZeroDepth(t *testing.T, v reflect.Value, seed, depth int) {
	t.Helper()
	if depth > 4 { // recursive types (BillingTurns []BifrostLLMUsage) stop here, left zero
		return
	}
	switch v.Kind() {
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		fillNonZeroDepth(t, v.Elem(), seed, depth+1)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() {
				fillNonZeroDepth(t, v.Field(i), seed+i+1, depth+1)
			}
		}
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		fillNonZeroDepth(t, v.Index(0), seed, depth+1)
	case reflect.Map:
		v.Set(reflect.MakeMap(v.Type()))
		k := reflect.New(v.Type().Key()).Elem()
		fillNonZeroDepth(t, k, seed, depth+1)
		e := reflect.New(v.Type().Elem()).Elem()
		fillNonZeroDepth(t, e, seed, depth+1)
		v.SetMapIndex(k, e)
	case reflect.String:
		v.SetString("v" + string(rune('a'+seed%26)))
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(int64(seed))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(uint64(seed))
	case reflect.Float32, reflect.Float64:
		v.SetFloat(float64(seed) + 0.5)
	case reflect.Interface:
		v.Set(reflect.ValueOf(seed))
	default:
		t.Fatalf("fillNonZero: unhandled kind %s", v.Kind())
	}
}

// TestMergeStreamFrameCarriesEveryField is the check behind contract C10 BF21: a stream's final
// usage must equal the non-streamed usage field for field. A frame with every field set, merged
// into an empty accumulator, must come out whole; a field added to BifrostLLMUsage and not handled
// in MergeStreamFrame fails here by name.
func TestMergeStreamFrameCarriesEveryField(t *testing.T) {
	var frame BifrostLLMUsage
	fillNonZero(t, reflect.ValueOf(&frame).Elem(), 0)
	frame.BillingTurns = nil // set only by AddBillingTurn, never on a vendor frame
	frame.TotalTokens = frame.PromptTokens + frame.CompletionTokens + 1

	got := &BifrostLLMUsage{}
	got.MergeStreamFrame(&frame)

	gv, fv := reflect.ValueOf(got).Elem(), reflect.ValueOf(&frame).Elem()
	for i := 0; i < fv.NumField(); i++ {
		name := fv.Type().Field(i).Name
		if !reflect.DeepEqual(gv.Field(i).Interface(), fv.Field(i).Interface()) {
			t.Errorf("MergeStreamFrame dropped or changed %s: got %#v, want %#v", name, gv.Field(i).Interface(), fv.Field(i).Interface())
		}
	}
}

// TestMergeStreamFrameMatchesNonStreamedUsage replays a recorded OpenAI-compatible stream with web
// search and an xAI-style reported cost: running usage frames followed by the trailing usage frame.
// The accumulated usage must serialise exactly like the same usage returned without streaming.
func TestMergeStreamFrameMatchesNonStreamedUsage(t *testing.T) {
	nonStreamed := `{"prompt_tokens":120,"prompt_tokens_details":{"cached_read_tokens":64},` +
		`"completion_tokens":48,"completion_tokens_details":{"reasoning_tokens":12,"num_search_queries":2},` +
		`"total_tokens":168,"tool_usage":{"web_search":{"num_requests":2}},` +
		`"cost":{"total_cost":0.0123},"cost_in_usd_ticks":123000000}`
	frames := []string{
		`{"prompt_tokens":120,"completion_tokens":1,"total_tokens":121}`,
		`{"prompt_tokens":120,"completion_tokens":30,"total_tokens":150}`,
		nonStreamed,
	}

	acc := &BifrostLLMUsage{}
	for _, f := range frames {
		var u BifrostLLMUsage
		if err := json.Unmarshal([]byte(f), &u); err != nil {
			t.Fatal(err)
		}
		acc.MergeStreamFrame(&u)
	}

	var want BifrostLLMUsage
	if err := json.Unmarshal([]byte(nonStreamed), &want); err != nil {
		t.Fatal(err)
	}
	gotJSON, _ := json.Marshal(acc)
	wantJSON, _ := json.Marshal(&want)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("streamed usage differs from non-streamed\n got: %s\nwant: %s", gotJSON, wantJSON)
	}
}

// TestMergeStreamFrameKeepsLargestTokenCounts: a later, smaller running total does not lower the
// counts, and total_tokens never falls below prompt plus completion.
func TestMergeStreamFrameKeepsLargestTokenCounts(t *testing.T) {
	acc := &BifrostLLMUsage{}
	acc.MergeStreamFrame(&BifrostLLMUsage{PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30})
	acc.MergeStreamFrame(&BifrostLLMUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15, ToolUsage: &ToolUsage{WebSearch: &WebSearchToolUsage{NumRequests: 1}}})
	acc.MergeStreamFrame(&BifrostLLMUsage{PromptTokens: 12, CompletionTokens: 22})
	if acc.PromptTokens != 12 || acc.CompletionTokens != 22 || acc.TotalTokens != 34 {
		t.Fatalf("token counts = %d/%d/%d, want 12/22/34", acc.PromptTokens, acc.CompletionTokens, acc.TotalTokens)
	}
	if acc.ToolUsage == nil || acc.ToolUsage.WebSearch.NumRequests != 1 {
		t.Fatalf("a frame without tool_usage cleared the earlier report: %#v", acc.ToolUsage)
	}
	var nilAcc *BifrostLLMUsage
	nilAcc.MergeStreamFrame(&BifrostLLMUsage{PromptTokens: 1}) // no panic
	acc.MergeStreamFrame(nil)                                  // no-op
}

// BenchmarkMergeStreamFrame measures the per-frame cost of the stream accumulators' usage merge
// (TXE-6522): a token-only frame (the common OpenAI frame) and a frame with every field a vendor
// reports beyond tokens (web search calls, cost, cost_in_usd_ticks).
func BenchmarkMergeStreamFrame(b *testing.B) {
	ticks := int64(123000000)
	frames := map[string]*BifrostLLMUsage{
		"tokens-only": {PromptTokens: 120, CompletionTokens: 48, TotalTokens: 168},
		"extended": {
			PromptTokens: 120, CompletionTokens: 48, TotalTokens: 168,
			PromptTokensDetails:     &ChatPromptTokensDetails{CachedReadTokens: 64},
			CompletionTokensDetails: &ChatCompletionTokensDetails{ReasoningTokens: 12, NumSearchQueries: new(2)},
			ToolUsage:               &ToolUsage{WebSearch: &WebSearchToolUsage{NumRequests: 2}},
			Cost:                    &BifrostCost{TotalCost: 0.0123},
			CostInUsdTicks:          &ticks,
		},
	}
	for name, frame := range frames {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				var acc BifrostLLMUsage
				acc.MergeStreamFrame(frame)
			}
		})
	}
}

// TestToolUsageAddDeepCopyCoverEveryField fails when a new ToolUsage counter is
// added without being summed by Add or owned by DeepCopy.
func TestToolUsageAddDeepCopyCoverEveryField(t *testing.T) {
	var tu ToolUsage
	v := reflect.ValueOf(&tu).Elem()
	perTool := reflect.TypeOf(map[string]ToolRequestUsage{})
	for i := 0; i < v.NumField(); i++ {
		switch v.Field(i).Type() {
		case reflect.TypeOf(&ToolRequestUsage{}):
			v.Field(i).Set(reflect.ValueOf(&ToolRequestUsage{NumRequests: i + 1}))
		case perTool:
			v.Field(i).Set(reflect.ValueOf(map[string]ToolRequestUsage{"tool": {NumRequests: i + 1}}))
		default:
			t.Fatalf("ToolUsage.%s is neither a *ToolRequestUsage nor the per-tool map; extend this test", v.Type().Field(i).Name)
		}
	}
	sum := reflect.ValueOf(tu.Add(&tu)).Elem()
	cp := reflect.ValueOf(tu.DeepCopy()).Elem()
	for i := 0; i < v.NumField(); i++ {
		name := v.Type().Field(i).Name
		if v.Field(i).Type() == perTool {
			if s := sum.Field(i).Interface().(map[string]ToolRequestUsage); s["tool"].NumRequests != 2*(i+1) {
				t.Fatalf("Add drops or mis-sums ToolUsage.%s: %+v", name, s)
			}
			c := cp.Field(i).Interface().(map[string]ToolRequestUsage)
			c["tool"] = ToolRequestUsage{NumRequests: -1}
			if tu.PerTool["tool"].NumRequests != i+1 {
				t.Fatalf("DeepCopy does not own ToolUsage.%s", name)
			}
			continue
		}
		if s := sum.Field(i).Interface().(*ToolRequestUsage); s == nil || s.NumRequests != 2*(i+1) {
			t.Fatalf("Add drops or mis-sums ToolUsage.%s: %+v", name, s)
		}
		orig := v.Field(i).Interface().(*ToolRequestUsage)
		if c := cp.Field(i).Interface().(*ToolRequestUsage); c == nil || c == orig || c.NumRequests != orig.NumRequests {
			t.Fatalf("DeepCopy does not own ToolUsage.%s", name)
		}
	}
}

func TestToolUsageAddDeepCopyCarryEveryCounter(t *testing.T) {
	a := &ToolUsage{WebSearch: &ToolRequestUsage{NumRequests: 1}, WebFetch: &ToolRequestUsage{NumRequests: 2}, CodeExecution: &ToolRequestUsage{NumRequests: 5}}
	b := &ToolUsage{WebFetch: &ToolRequestUsage{NumRequests: 3}, CodeExecution: &ToolRequestUsage{NumRequests: 1}}

	sum := a.Add(b)
	if sum.WebSearch.NumRequests != 1 || sum.WebFetch.NumRequests != 5 || sum.CodeExecution.NumRequests != 6 {
		t.Fatalf("Add = %+v/%+v/%+v, want 1/5/6", *sum.WebSearch, *sum.WebFetch, *sum.CodeExecution)
	}
	if b.Add(nil).WebSearch != nil {
		t.Fatal("a tool absent on both sides must stay absent")
	}
	cp := a.DeepCopy()
	if cp.WebFetch == a.WebFetch || cp.CodeExecution == a.CodeExecution || cp.WebFetch.NumRequests != 2 || cp.CodeExecution.NumRequests != 5 {
		t.Fatalf("DeepCopy must own every counter, got %+v", cp)
	}
	out, err := Marshal(&ToolUsage{WebFetch: &ToolRequestUsage{NumRequests: 2}, CodeExecution: &ToolRequestUsage{NumRequests: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"web_fetch":{"num_requests":2},"code_execution":{"num_requests":1}}`; string(out) != want {
		t.Fatalf("json = %s, want %s", out, want)
	}
}

// toolUsageSink keeps benchmark results on the heap, as real callers that store them do.
var toolUsageSink *ToolUsage

// Request-path cost of the tool_usage merge (TXE-6534): nil operands, the existing
// web-search-only shape, and copies.
func BenchmarkToolUsageAdd_NilNil(b *testing.B) {
	var x, y *ToolUsage
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		toolUsageSink = x.Add(y)
	}
}

func BenchmarkToolUsageAdd_WebSearch(b *testing.B) {
	x := &ToolUsage{WebSearch: &WebSearchToolUsage{NumRequests: 1}}
	y := &ToolUsage{WebSearch: &WebSearchToolUsage{NumRequests: 2}}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		toolUsageSink = x.Add(y)
	}
}

func BenchmarkToolUsageDeepCopy_WebSearch(b *testing.B) {
	x := &ToolUsage{WebSearch: &WebSearchToolUsage{NumRequests: 1}}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		toolUsageSink = x.DeepCopy()
	}
}

func BenchmarkToolUsageAdd_AllTools(b *testing.B) {
	x := &ToolUsage{WebSearch: &ToolRequestUsage{NumRequests: 1}, WebFetch: &ToolRequestUsage{NumRequests: 2}, CodeExecution: &ToolRequestUsage{NumRequests: 1}}
	y := &ToolUsage{WebFetch: &ToolRequestUsage{NumRequests: 1}}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		toolUsageSink = x.Add(y)
	}
}

// TXE-6959: CachedWriteTiers is the one enumeration of cache-write lifetime tiers that cost,
// span attributes and metrics loop over. A tier field added to ChatCachedWriteTokenDetails
// without an entry here (or without Tokens, Plus, a span attribute or a vendor name) fails.
func TestCachedWriteTiersCoverEveryField(t *testing.T) {
	typ := reflect.TypeOf(ChatCachedWriteTokenDetails{})
	listed := map[string]bool{}
	for _, tier := range CachedWriteTiers {
		listed[tier] = true
	}
	if typ.NumField() != len(CachedWriteTiers) {
		t.Fatalf("ChatCachedWriteTokenDetails has %d fields but CachedWriteTiers lists %d", typ.NumField(), len(CachedWriteTiers))
	}

	// Give every field a distinct value through reflection, so a Tokens/Plus case that reads
	// the wrong field cannot pass.
	var d ChatCachedWriteTokenDetails
	v := reflect.ValueOf(&d).Elem()
	want := map[string]int{}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.Type.Kind() != reflect.Int {
			t.Fatalf("field %s is %s; every tier field must be an int", f.Name, f.Type)
		}
		tag := strings.Split(f.Tag.Get("json"), ",")[0]
		tier, ok := strings.CutPrefix(tag, "cached_write_tokens_")
		if !ok || !listed[tier] {
			t.Fatalf("field %s (json %q) has no entry in CachedWriteTiers", f.Name, tag)
		}
		v.Field(i).SetInt(int64(10 * (i + 1)))
		want[tier] = 10 * (i + 1)
	}

	sum := d.Plus(&d)
	attrs := map[string]bool{}
	for _, tier := range CachedWriteTiers {
		if got := d.Tokens(tier); got != want[tier] {
			t.Errorf("Tokens(%q) = %d, want %d", tier, got, want[tier])
		}
		if got := sum.Tokens(tier); got != 2*want[tier] {
			t.Errorf("Plus: tier %q = %d, want %d", tier, got, 2*want[tier])
		}
		for _, responses := range []bool{false, true} {
			key := CachedWriteTierAttr(responses, tier)
			if !strings.HasSuffix(key, ".cached_write_tokens_"+tier) || attrs[key] {
				t.Errorf("CachedWriteTierAttr(%v, %q) = %q: missing, misnamed or duplicated", responses, tier, key)
			}
			attrs[key] = true
		}
		if CachedWriteTierVendors(tier) == "vendor-reported" {
			t.Errorf("CachedWriteTierVendors(%q) names no vendor", tier)
		}
	}
	if got := d.TokensAbove1h(); got != want[CachedWriteTier1h]+want[CachedWriteTier12h] {
		t.Errorf("TokensAbove1h = %d, want the 1h and 12h tiers", got)
	}
	var nilDetails *ChatCachedWriteTokenDetails
	if nilDetails.Tokens(CachedWriteTier1h) != 0 || nilDetails.TokensAbove1h() != 0 || nilDetails.Plus(nil) != (ChatCachedWriteTokenDetails{}) {
		t.Error("nil details must read as zero")
	}
}

// TXE-6959: span usage carries OpenAI's 30m and 12h cache-write tiers in the namespace of the
// request family.
func TestLLMSpanDataCarriesEveryCacheWriteTier(t *testing.T) {
	usage := &BifrostLLMUsage{PromptTokens: 100, PromptTokensDetails: &ChatPromptTokensDetails{
		CachedWriteTokens:       80,
		CachedWriteTokenDetails: &ChatCachedWriteTokenDetails{CachedWriteTokens30m: 50, CachedWriteTokens12h: 30},
	}}
	for _, rt := range []RequestType{ChatCompletionRequest, ResponsesRequest} {
		attrs := (&LLMSpanData{RequestType: rt, Usage: usage}).Attributes()
		responses := rt == ResponsesRequest
		if got := attrs[CachedWriteTierAttr(responses, CachedWriteTier30m)]; got != 50 {
			t.Errorf("%s: 30m attribute = %v, want 50", rt, got)
		}
		if got := attrs[CachedWriteTierAttr(responses, CachedWriteTier12h)]; got != 30 {
			t.Errorf("%s: 12h attribute = %v, want 30", rt, got)
		}
		for _, tier := range []string{CachedWriteTier5m, CachedWriteTier1h} {
			if _, ok := attrs[CachedWriteTierAttr(responses, tier)]; ok {
				t.Errorf("%s: unreported %s tier emitted", rt, tier)
			}
		}
	}
}

// TXE-6959: span usage attributes per cache-write tier shape.
func BenchmarkLLMSpanDataCacheWriteTierAttributes(b *testing.B) {
	cases := []struct {
		name  string
		tiers *ChatCachedWriteTokenDetails
	}{
		{"no_tiers", nil},
		{"anthropic_5m_1h", &ChatCachedWriteTokenDetails{CachedWriteTokens5m: 50, CachedWriteTokens1h: 30}},
		{"openai_30m_12h", &ChatCachedWriteTokenDetails{CachedWriteTokens30m: 50, CachedWriteTokens12h: 30}},
	}
	for _, tc := range cases {
		d := &LLMSpanData{RequestType: ChatCompletionRequest, Usage: &BifrostLLMUsage{
			PromptTokens: 100, CompletionTokens: 10, TotalTokens: 110,
			PromptTokensDetails: &ChatPromptTokensDetails{CachedWriteTokens: 80, CachedWriteTokenDetails: tc.tiers},
		}}
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				attrs := make(map[string]any, 16)
				d.appendUsage(attrs)
			}
		})
	}
}

func TestProcessedTokensMergeAndDeepCopy(t *testing.T) {
	a := &BifrostLLMUsage{PromptTokens: 5, ProcessedTokens: &ProcessedTokenUsage{PromptTokens: 71, CompletionTokens: 10}}
	b := &BifrostLLMUsage{PromptTokens: 1, ProcessedTokens: &ProcessedTokenUsage{PromptTokens: 2, CompletionTokens: 3}}
	m := MergeBifrostLLMUsage(a, b)
	if m.ProcessedTokens == nil || *m.ProcessedTokens != (ProcessedTokenUsage{PromptTokens: 73, CompletionTokens: 13}) {
		t.Fatalf("merged processed_tokens = %+v", m.ProcessedTokens)
	}
	if MergeBifrostLLMUsage(&BifrostLLMUsage{PromptTokens: 1}, &BifrostLLMUsage{PromptTokens: 2}).ProcessedTokens != nil {
		t.Fatal("processed_tokens must stay absent when neither side reports it")
	}
	cp := a.DeepCopy()
	if cp.ProcessedTokens == a.ProcessedTokens || *cp.ProcessedTokens != *a.ProcessedTokens {
		t.Fatal("DeepCopy must own processed_tokens")
	}
}

// Cost of carrying processed_tokens through usage merge and copy (TXE-6596) for the
// common case where it is absent.
var llmUsageSink *BifrostLLMUsage

func BenchmarkMergeBifrostLLMUsage_Tokens(b *testing.B) {
	x, y := &BifrostLLMUsage{PromptTokens: 5, CompletionTokens: 7, TotalTokens: 12}, &BifrostLLMUsage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		llmUsageSink = MergeBifrostLLMUsage(x, y)
	}
}

func BenchmarkBifrostLLMUsageDeepCopy_Tokens(b *testing.B) {
	x := &BifrostLLMUsage{PromptTokens: 5, CompletionTokens: 7, TotalTokens: 12}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		llmUsageSink = x.DeepCopy()
	}
}

// TestToolUsagePerToolAddAndDeepCopy: the per-tool map (TXE-6549) is summed tool by tool when
// separate calls are added up, and DeepCopy owns its map.
func TestToolUsagePerToolAddAndDeepCopy(t *testing.T) {
	a := &ToolUsage{WebSearch: &WebSearchToolUsage{NumRequests: 2}, PerTool: map[string]ToolRequestUsage{"web_search": {NumRequests: 2}, "fetch_url": {NumRequests: 1}}}
	b := &ToolUsage{PerTool: map[string]ToolRequestUsage{"fetch_url": {NumRequests: 4}, "sandbox": {NumRequests: 1}}}

	sum := a.Add(b)
	want := map[string]ToolRequestUsage{"web_search": {NumRequests: 2}, "fetch_url": {NumRequests: 5}, "sandbox": {NumRequests: 1}}
	if !reflect.DeepEqual(sum.PerTool, want) || sum.WebSearch.NumRequests != 2 {
		t.Fatalf("Add = %+v / %+v, want %+v / web_search 2", sum.PerTool, sum.WebSearch, want)
	}
	sum.PerTool["web_search"] = ToolRequestUsage{NumRequests: 99}
	if a.PerTool["web_search"].NumRequests != 2 {
		t.Fatal("Add shares an operand's map")
	}
	if only := (&ToolUsage{WebSearch: &WebSearchToolUsage{NumRequests: 1}}).Add(nil); only.PerTool != nil {
		t.Fatalf("a usage with no per-tool report must not gain a map: %+v", only.PerTool)
	}

	cp := a.DeepCopy()
	cp.PerTool["fetch_url"] = ToolRequestUsage{NumRequests: 7}
	if a.PerTool["fetch_url"].NumRequests != 1 {
		t.Fatal("DeepCopy shares the per-tool map")
	}

	out, err := Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	var back ToolUsage
	if err := Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(&back, a) {
		t.Fatalf("JSON round trip = %+v, want %+v (wire %s)", back, a, out)
	}
}

// TestSearchContextSizeSurvivesCopyMergeAndResponsesConversion: the tier the vendor billed
// (TXE-6549) is owned by DeepCopy, kept by MergeBifrostLLMUsage (the later call's report, else
// the earlier one's) and carried both ways through the chat/Responses usage conversion.
func TestSearchContextSizeSurvivesCopyMergeAndResponsesConversion(t *testing.T) {
	high, low := "high", "low"
	u := &BifrostLLMUsage{PromptTokens: 1, TotalTokens: 1, SearchContextSize: &high,
		ToolUsage: &ToolUsage{PerTool: map[string]ToolRequestUsage{"web_search": {NumRequests: 1}}}}

	cp := u.DeepCopy()
	if cp.SearchContextSize == u.SearchContextSize || *cp.SearchContextSize != "high" {
		t.Fatalf("DeepCopy must own search_context_size, got %v", cp.SearchContextSize)
	}

	if got := MergeBifrostLLMUsage(u, &BifrostLLMUsage{PromptTokens: 1}); got.SearchContextSize == nil || *got.SearchContextSize != "high" {
		t.Fatalf("merge with a call that reports no tier dropped it: %v", got.SearchContextSize)
	}
	if got := MergeBifrostLLMUsage(u, &BifrostLLMUsage{SearchContextSize: &low}); *got.SearchContextSize != "low" {
		t.Fatalf("merge = %q, want the later call's tier", *got.SearchContextSize)
	}

	ru := u.ToResponsesResponseUsage()
	if ru.SearchContextSize == nil || *ru.SearchContextSize != "high" || ru.ToolUsage.PerTool["web_search"].NumRequests != 1 {
		t.Fatalf("ToResponsesResponseUsage lost the tier or the per-tool counts: %+v", ru)
	}
	back := ru.ToBifrostLLMUsage()
	if back.SearchContextSize == nil || *back.SearchContextSize != "high" || back.ToolUsage.PerTool["web_search"].NumRequests != 1 {
		t.Fatalf("ToBifrostLLMUsage lost the tier or the per-tool counts: %+v", back)
	}
}

// stepFunChatBody is a chat completion in the shape of StepFun's API reference for its
// international site (platform.stepfun.ai, chat completion object): the cache-hit count is a
// top-level usage.cached_tokens and there is no prompt_tokens_details object. The usage numbers
// are the ones in StepFun's prompt-cache guide: 512 of the 591 prompt tokens came from the cache.
// No response recorded from a real request exists yet (the vendor keys are not issued).
const stepFunChatBody = `{"id":"0190f7e2c1b27c1a9a0f6d1f3b2a4c5d","object":"chat.completion","created":1762761600,"model":"step-5-preview","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"cached_tokens":512,"prompt_tokens":591,"completion_tokens":120,"total_tokens":711}}`

// A top-level usage.cached_tokens (StepFun) must land in prompt_tokens_details.cached_read_tokens
// once the usage is normalized, in a whole response and in a streamed frame (StepFun sends usage
// on its chunks in the same four fields), and must be served back at the unified path only
// (contract C5).
func TestBifrostLLMUsageNormalizeCachedTokens(t *testing.T) {
	streamFrame := `{"id":"0190f7e2","object":"chat.completion.chunk","created":1762761600,"model":"step-5-preview","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"cached_tokens":512,"prompt_tokens":591,"completion_tokens":120,"total_tokens":711}}`
	for label, body := range map[string]string{"response": stepFunChatBody, "stream frame": streamFrame} {
		var resp BifrostChatResponse
		if err := Unmarshal([]byte(body), &resp); err != nil {
			t.Fatalf("%s: decode: %v", label, err)
		}
		resp.Usage.NormalizeVendorSpellings()
		u := resp.Usage
		if u == nil || u.PromptTokensDetails == nil {
			t.Fatalf("%s: usage = %+v, want prompt_tokens_details set", label, u)
		}
		if got := u.PromptTokensDetails.CachedReadTokens; got != 512 {
			t.Errorf("%s: cached_read_tokens = %d, want 512", label, got)
		}
		if u.CachedTokens != 0 {
			t.Errorf("%s: top-level cached_tokens = %d after normalizing, want it cleared", label, u.CachedTokens)
		}
		if u.PromptTokens != 591 || u.CompletionTokens != 120 || u.TotalTokens != 711 {
			t.Errorf("%s: token counts = %d/%d/%d, want 591/120/711", label, u.PromptTokens, u.CompletionTokens, u.TotalTokens)
		}
		out, err := Marshal(u)
		if err != nil {
			t.Fatalf("%s: marshal: %v", label, err)
		}
		var served struct {
			CachedTokens        *int `json:"cached_tokens"`
			PromptTokensDetails struct {
				CachedReadTokens int `json:"cached_read_tokens"`
				CachedTokens     int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		}
		if err := json.Unmarshal(out, &served); err != nil {
			t.Fatalf("%s: decode served usage: %v", label, err)
		}
		if served.PromptTokensDetails.CachedReadTokens != 512 || served.PromptTokensDetails.CachedTokens != 512 {
			t.Errorf("%s: served usage = %s, want cached_read_tokens and cached_tokens 512 under prompt_tokens_details", label, out)
		}
		if served.CachedTokens != nil {
			t.Errorf("%s: served usage = %s, want no top-level cached_tokens", label, out)
		}
	}
}

// The top-level count is a fallback only: a cached-read count nested under prompt_tokens_details
// wins, and a vendor that reports zero or nothing gets no prompt_tokens_details invented for it.
func TestBifrostLLMUsageNormalizeCachedTokensPrecedence(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		wantDetails bool
		wantRead    int
		wantWrite   int
		wantText    int
	}{
		{"nested cached_tokens wins", `{"prompt_tokens":10,"cached_tokens":9,"prompt_tokens_details":{"cached_tokens":4}}`, true, 4, 0, 0},
		{"nested cached_read_tokens wins", `{"prompt_tokens":10,"cached_tokens":9,"prompt_tokens_details":{"cached_read_tokens":3}}`, true, 3, 0, 0},
		{"nested details without a cached count take the top-level one", `{"prompt_tokens":10,"cached_tokens":9,"prompt_tokens_details":{"text_tokens":10}}`, true, 9, 0, 10},
		{"nested cache writes are kept beside the top-level read", `{"prompt_tokens":10,"cached_tokens":9,"prompt_tokens_details":{"cached_write_tokens":2}}`, true, 9, 2, 0},
		{"top-level zero creates nothing", `{"prompt_tokens":10,"cached_tokens":0}`, false, 0, 0, 0},
		{"top-level null creates nothing", `{"prompt_tokens":10,"cached_tokens":null}`, false, 0, 0, 0},
		{"absent creates nothing", `{"prompt_tokens":10}`, false, 0, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var u BifrostLLMUsage
			if err := Unmarshal([]byte(tc.body), &u); err != nil {
				t.Fatalf("decode: %v", err)
			}
			u.NormalizeVendorSpellings()
			if u.PromptTokens != 10 {
				t.Errorf("prompt_tokens = %d, want 10", u.PromptTokens)
			}
			if u.CachedTokens != 0 {
				t.Errorf("top-level cached_tokens = %d after normalizing, want it cleared", u.CachedTokens)
			}
			if !tc.wantDetails {
				if u.PromptTokensDetails != nil {
					t.Fatalf("prompt_tokens_details = %+v, want nil", u.PromptTokensDetails)
				}
				return
			}
			if u.PromptTokensDetails == nil {
				t.Fatal("prompt_tokens_details = nil")
			}
			d := u.PromptTokensDetails
			if d.CachedReadTokens != tc.wantRead || d.CachedWriteTokens != tc.wantWrite || d.TextTokens != tc.wantText {
				t.Errorf("details read/write/text = %d/%d/%d, want %d/%d/%d", d.CachedReadTokens, d.CachedWriteTokens, d.TextTokens, tc.wantRead, tc.wantWrite, tc.wantText)
			}
		})
	}
	var nilUsage *BifrostLLMUsage
	nilUsage.NormalizeCachedTokens()
	nilUsage.NormalizeVendorSpellings()
}

// StepFun sends usage on every streamed chunk. Each frame is normalized as it is decoded, so the
// accumulated usage carries the cached count at the unified path and never the top-level one.
func TestMergeStreamFrameCarriesNormalizedCachedTokens(t *testing.T) {
	acc := &BifrostLLMUsage{}
	for _, body := range []string{
		`{"cached_tokens":512,"prompt_tokens":591,"completion_tokens":1,"total_tokens":592}`,
		`{"cached_tokens":512,"prompt_tokens":591,"completion_tokens":120,"total_tokens":711}`,
	} {
		var frame BifrostLLMUsage
		if err := Unmarshal([]byte(body), &frame); err != nil {
			t.Fatalf("decode: %v", err)
		}
		frame.NormalizeVendorSpellings()
		acc.MergeStreamFrame(&frame)
	}
	if acc.PromptTokensDetails == nil || acc.PromptTokensDetails.CachedReadTokens != 512 {
		t.Fatalf("accumulated details = %+v, want cached_read_tokens 512", acc.PromptTokensDetails)
	}
	if acc.CachedTokens != 0 || acc.CompletionTokens != 120 || acc.TotalTokens != 711 {
		t.Errorf("accumulated usage = %+v, want no top-level cached_tokens, completion 120, total 711", acc)
	}
}

// BenchmarkBifrostLLMUsageNormalizeVendorSpellings measures the response-path cost of decoding a
// usage object and normalizing it (TXE-6439). A usage with no top-level vendor spelling, which
// is every vendor but SGLang and StepFun, must pay nothing beyond the decode: the normalizer is
// two integer comparisons there. Only the StepFun row allocates, for the details object it fills.
func BenchmarkBifrostLLMUsageNormalizeVendorSpellings(b *testing.B) {
	bodies := map[string][]byte{
		"tokens-only":      []byte(`{"prompt_tokens":591,"completion_tokens":120,"total_tokens":711}`),
		"nested-details":   []byte(`{"prompt_tokens":591,"completion_tokens":120,"total_tokens":711,"prompt_tokens_details":{"cached_tokens":512,"audio_tokens":0},"completion_tokens_details":{"reasoning_tokens":4,"audio_tokens":0,"accepted_prediction_tokens":0,"rejected_prediction_tokens":0}}`),
		"top-level-cached": []byte(`{"cached_tokens":512,"prompt_tokens":591,"completion_tokens":120,"total_tokens":711}`),
	}
	for _, name := range []string{"tokens-only", "nested-details", "top-level-cached"} {
		body := bodies[name]
		b.Run("decode/"+name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				var u BifrostLLMUsage
				if err := Unmarshal(body, &u); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run("decode-and-normalize/"+name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				var u BifrostLLMUsage
				if err := Unmarshal(body, &u); err != nil {
					b.Fatal(err)
				}
				u.NormalizeVendorSpellings()
			}
		})
	}
}

func TestProcessedTokensSurviveChatResponsesConversion(t *testing.T) {
	cu := &BifrostLLMUsage{PromptTokens: 5, CompletionTokens: 418, TotalTokens: 423, ProcessedTokens: &ProcessedTokenUsage{PromptTokens: 71, CompletionTokens: 418}}
	ru := cu.ToResponsesResponseUsage()
	if ru.ProcessedTokens == nil || ru.ProcessedTokens == cu.ProcessedTokens || *ru.ProcessedTokens != *cu.ProcessedTokens {
		t.Fatalf("ToResponsesResponseUsage processed_tokens = %+v, want an owned copy of %+v", ru.ProcessedTokens, *cu.ProcessedTokens)
	}
	back := ru.ToBifrostLLMUsage()
	if back.ProcessedTokens == nil || back.ProcessedTokens == ru.ProcessedTokens || *back.ProcessedTokens != *cu.ProcessedTokens {
		t.Fatalf("ToBifrostLLMUsage processed_tokens = %+v", back.ProcessedTokens)
	}
	if (&BifrostLLMUsage{TotalTokens: 1}).ToResponsesResponseUsage().ProcessedTokens != nil {
		t.Fatal("absent processed_tokens must stay absent")
	}
}

// sharedUsageFields are the fields BifrostLLMUsage and ResponsesResponseUsage both have, under the
// same name and type, so a conversion between them can copy them unchanged.
func sharedUsageFields() []string {
	llm, resp := reflect.TypeOf(BifrostLLMUsage{}), reflect.TypeOf(ResponsesResponseUsage{})
	var names []string
	for i := 0; i < llm.NumField(); i++ {
		f := llm.Field(i)
		if g, ok := resp.FieldByName(f.Name); ok && g.Type == f.Type {
			names = append(names, f.Name)
		}
	}
	return names
}

// TestUsageConversionsCarrySharedFields: ToResponsesResponseUsage and ToBifrostLLMUsage copy every field
// the two usage types share (fork, TXE-6967). vendor_cost reaches a Responses caller served by the chat
// fallback, and a chat caller served by a Responses upstream, only through these conversions; a shared
// field added later and not copied fails here by name.
func TestUsageConversionsCarrySharedFields(t *testing.T) {
	names := sharedUsageFields()
	if len(names) == 0 {
		t.Fatal("no shared fields found")
	}

	var llm BifrostLLMUsage
	fillNonZero(t, reflect.ValueOf(&llm).Elem(), 0)
	resp := llm.ToResponsesResponseUsage()
	var r ResponsesResponseUsage
	fillNonZero(t, reflect.ValueOf(&r).Elem(), 0)
	r.Iterations = nil
	back := r.ToBifrostLLMUsage()

	for _, name := range names {
		if got, want := reflect.ValueOf(resp).Elem().FieldByName(name).Interface(), reflect.ValueOf(&llm).Elem().FieldByName(name).Interface(); !reflect.DeepEqual(got, want) {
			t.Errorf("ToResponsesResponseUsage drops %s: got %#v, want %#v", name, got, want)
		}
		if got, want := reflect.ValueOf(back).Elem().FieldByName(name).Interface(), reflect.ValueOf(&r).Elem().FieldByName(name).Interface(); !reflect.DeepEqual(got, want) {
			t.Errorf("ToBifrostLLMUsage drops %s: got %#v, want %#v", name, got, want)
		}
	}
}

// TestNormalizeProviderCostSetsVendorCost (TXE-6967): cost_in_usd_ticks becomes vendor_cost in usd_ticks on
// chat and Responses usage. A reported zero is kept as amount 0, a cost the vendor sent itself does not stop
// it, no ticks leaves it nil (contract C5 agreement two), and a vendor_cost already set is not overwritten.
func TestNormalizeProviderCostSetsVendorCost(t *testing.T) {
	ticks := func(n int64) *int64 { return &n }
	cases := []struct {
		name  string
		ticks *int64
		cost  *BifrostCost
		want  *VendorCost
	}{
		{"ticks", ticks(5584000), nil, &VendorCost{Amount: 5584000, Unit: VendorCostUnitUSDTicks}},
		{"zero ticks", ticks(0), nil, &VendorCost{Amount: 0, Unit: VendorCostUnitUSDTicks}},
		{"ticks with a vendor-sent cost", ticks(7), &BifrostCost{TotalCost: 1}, &VendorCost{Amount: 7, Unit: VendorCostUnitUSDTicks}},
		{"no ticks", nil, nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chat := &BifrostLLMUsage{CostInUsdTicks: tc.ticks, Cost: tc.cost}
			chat.NormalizeProviderCost()
			resp := &ResponsesResponseUsage{CostInUsdTicks: tc.ticks, Cost: tc.cost}
			resp.NormalizeProviderCost()
			for name, got := range map[string]*VendorCost{"chat": chat.VendorCost, "responses": resp.VendorCost} {
				if !reflect.DeepEqual(got, tc.want) {
					t.Errorf("%s vendor_cost = %+v, want %+v", name, got, tc.want)
				}
			}
			if tc.cost != nil && (chat.Cost != tc.cost || resp.Cost != tc.cost) {
				t.Errorf("a vendor-sent cost was replaced")
			}
		})
	}

	set := &VendorCost{Amount: 1, Unit: VendorCostUnitUSD}
	u := &BifrostLLMUsage{CostInUsdTicks: ticks(9), VendorCost: set}
	u.NormalizeProviderCost()
	if u.VendorCost != set {
		t.Errorf("an existing vendor_cost was overwritten: %+v", u.VendorCost)
	}
	(*BifrostLLMUsage)(nil).NormalizeProviderCost()
	(*ResponsesResponseUsage)(nil).NormalizeProviderCost()
}

// TestVendorCostSerialisesOnUsage: vendor_cost is on the wire for chat and Responses usage when set, absent
// when not, and DeepCopy owns its copy.
func TestVendorCostSerialisesOnUsage(t *testing.T) {
	vc := &VendorCost{Amount: 5584000, Unit: VendorCostUnitUSDTicks}
	for name, v := range map[string]any{"chat": &BifrostLLMUsage{VendorCost: vc}, "responses": &ResponsesResponseUsage{VendorCost: vc}} {
		b, _ := json.Marshal(v)
		if !strings.Contains(string(b), `"vendor_cost":{"amount":5584000,"unit":"usd_ticks"}`) {
			t.Errorf("%s: %s", name, b)
		}
	}
	for name, v := range map[string]any{"chat": &BifrostLLMUsage{}, "responses": &ResponsesResponseUsage{}} {
		b, _ := json.Marshal(v)
		if strings.Contains(string(b), "vendor_cost") {
			t.Errorf("%s carries vendor_cost unset: %s", name, b)
		}
	}
	orig := &BifrostLLMUsage{VendorCost: &VendorCost{Amount: 1, Unit: VendorCostUnitUSDTicks}}
	c := orig.DeepCopy()
	c.VendorCost.Amount = 9
	if orig.VendorCost.Amount != 1 {
		t.Errorf("DeepCopy shares vendor_cost")
	}
}

// TestMergeBifrostLLMUsageSumsVendorCost (TXE-6967): a request made of several upstream calls (an MCP agent's
// turns, injected-tool stream turns) carries the vendor's charge for all of them: the sum when every call
// reported one in the same unit, otherwise nothing, never a partial sum. cost_in_usd_ticks follows the same rule.
func TestMergeBifrostLLMUsageSumsVendorCost(t *testing.T) {
	ticks := func(n int64) *int64 { return &n }
	vc := func(a float64, u VendorCostUnit) *VendorCost { return &VendorCost{Amount: a, Unit: u} }
	cases := []struct {
		name      string
		base, add *BifrostLLMUsage
		want      *VendorCost
		wantTicks *int64
	}{
		{"both reported", &BifrostLLMUsage{VendorCost: vc(100, VendorCostUnitUSDTicks), CostInUsdTicks: ticks(100)}, &BifrostLLMUsage{VendorCost: vc(250, VendorCostUnitUSDTicks), CostInUsdTicks: ticks(250)}, vc(350, VendorCostUnitUSDTicks), ticks(350)},
		{"one call unreported", &BifrostLLMUsage{VendorCost: vc(100, VendorCostUnitUSDTicks), CostInUsdTicks: ticks(100)}, &BifrostLLMUsage{PromptTokens: 1}, nil, nil},
		{"units differ", &BifrostLLMUsage{VendorCost: vc(1, VendorCostUnitUSD)}, &BifrostLLMUsage{VendorCost: vc(2, VendorCostUnitCredit)}, nil, nil},
		{"neither reported", &BifrostLLMUsage{PromptTokens: 1}, &BifrostLLMUsage{PromptTokens: 2}, nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := MergeBifrostLLMUsage(tc.base, tc.add)
			if !reflect.DeepEqual(got.VendorCost, tc.want) {
				t.Errorf("vendor_cost = %+v, want %+v", got.VendorCost, tc.want)
			}
			if !reflect.DeepEqual(got.CostInUsdTicks, tc.wantTicks) {
				t.Errorf("cost_in_usd_ticks = %v, want %v", got.CostInUsdTicks, tc.wantTicks)
			}
		})
	}
	// AddBillingTurn is the path the injected-tool streams take.
	total := AddBillingTurn(&BifrostLLMUsage{VendorCost: vc(1, VendorCostUnitUSDTicks)}, &BifrostLLMUsage{VendorCost: vc(2, VendorCostUnitUSDTicks)})
	if !reflect.DeepEqual(total.VendorCost, vc(3, VendorCostUnitUSDTicks)) {
		t.Errorf("AddBillingTurn vendor_cost = %+v", total.VendorCost)
	}
}

// TestUsageCarriesVideoDocumentAndToolUsePromptTokens pins the unified usage fields TXE-6535
// adds (contract C5): video_tokens and document_tokens in the prompt and input token
// details, and tool_use_prompt_tokens with its breakdown. They survive JSON both ways, the
// chat and Responses conversions, summing across calls, and a deep copy that shares nothing.
func TestUsageCarriesVideoDocumentAndToolUsePromptTokens(t *testing.T) {
	const wire = `{"prompt_tokens":17500,"prompt_tokens_details":{"text_tokens":12,"audio_tokens":1500,"video_tokens":15480,"document_tokens":508,"cached_tokens":0,"cache_write_tokens":0},` +
		`"completion_tokens":120,"total_tokens":18170,"tool_use_prompt_tokens":550,"tool_use_prompt_tokens_details":{"text_tokens":530,"image_tokens":20}}`
	var usage BifrostLLMUsage
	if err := json.Unmarshal([]byte(wire), &usage); err != nil {
		t.Fatal(err)
	}
	if usage.PromptTokensDetails.VideoTokens != 15480 || usage.PromptTokensDetails.DocumentTokens != 508 {
		t.Fatalf("decoded video/document tokens = %d/%d, want 15480/508", usage.PromptTokensDetails.VideoTokens, usage.PromptTokensDetails.DocumentTokens)
	}
	if got, _ := json.Marshal(&usage); string(got) != wire {
		t.Fatalf("usage did not round-trip\n got: %s\nwant: %s", got, wire)
	}

	responsesUsage := usage.ToResponsesResponseUsage()
	if responsesUsage.InputTokensDetails.VideoTokens != 15480 || responsesUsage.InputTokensDetails.DocumentTokens != 508 ||
		responsesUsage.ToolUsePromptTokens != 550 || !reflect.DeepEqual(responsesUsage.ToolUsePromptTokensDetails, usage.ToolUsePromptTokensDetails) {
		t.Fatalf("chat to Responses usage lost a field: %+v / %+v", responsesUsage.InputTokensDetails, responsesUsage)
	}
	responsesWire, _ := json.Marshal(responsesUsage)
	var decoded ResponsesResponseUsage
	if err := json.Unmarshal(responsesWire, &decoded); err != nil {
		t.Fatal(err)
	}
	if back, _ := json.Marshal(decoded.ToBifrostLLMUsage()); string(back) != wire {
		t.Fatalf("Responses usage did not convert back\n got: %s\nwant: %s", back, wire)
	}

	sum := MergeBifrostLLMUsage(&usage, &usage)
	if sum.PromptTokensDetails.VideoTokens != 30960 || sum.PromptTokensDetails.DocumentTokens != 1016 || sum.ToolUsePromptTokens != 1100 ||
		!reflect.DeepEqual(sum.ToolUsePromptTokensDetails, &ToolUsePromptTokensDetails{TextTokens: 1060, ImageTokens: 40}) {
		t.Fatalf("summed usage = %+v / %+v / %d", sum.PromptTokensDetails, sum.ToolUsePromptTokensDetails, sum.ToolUsePromptTokens)
	}
	if merged := MergeBifrostLLMUsage(&BifrostLLMUsage{PromptTokens: 1}, &BifrostLLMUsage{PromptTokens: 2}); merged.ToolUsePromptTokensDetails != nil || merged.ToolUsePromptTokens != 0 {
		t.Fatalf("a sum of usages without tool-use prompt tokens invented some: %+v", merged)
	}

	clone := usage.DeepCopy()
	clone.ToolUsePromptTokensDetails.TextTokens = 1
	clone.PromptTokensDetails.VideoTokens = 1
	if usage.ToolUsePromptTokensDetails.TextTokens != 530 || usage.PromptTokensDetails.VideoTokens != 15480 {
		t.Fatal("DeepCopy shares the token details with the original")
	}
}

// A request billed over several calls (a gateway-run tool loop, an MCP agent loop) reports
// the tools xAI ran in every call: the per-tool counts are summed, one side reporting is
// kept, and neither input is changed (TXE-7043, contract C5).
func TestMergeBifrostLLMUsageSumsServerSideToolUsage(t *testing.T) {
	first := &BifrostLLMUsage{PromptTokens: 1, ServerSideToolUsageDetails: &ResponsesServerSideToolUsageDetails{WebSearchCalls: 2, XSearchCalls: 1, MCPCalls: 4}}
	second := &BifrostLLMUsage{PromptTokens: 2, ServerSideToolUsageDetails: &ResponsesServerSideToolUsageDetails{WebSearchCalls: 3, CodeInterpreterCalls: 1, FileSearchCalls: 2, DocumentSearchCalls: 5}}
	want := ResponsesServerSideToolUsageDetails{WebSearchCalls: 5, XSearchCalls: 1, CodeInterpreterCalls: 1, FileSearchCalls: 2, MCPCalls: 4, DocumentSearchCalls: 5}
	if got := MergeBifrostLLMUsage(first, second).ServerSideToolUsageDetails; got == nil || *got != want {
		t.Fatalf("merged counts = %+v, want %+v", got, want)
	}
	if first.ServerSideToolUsageDetails.WebSearchCalls != 2 || second.ServerSideToolUsageDetails.WebSearchCalls != 3 {
		t.Fatal("the merge changed one of its inputs")
	}
	none := &BifrostLLMUsage{PromptTokens: 3}
	if got := MergeBifrostLLMUsage(none, first).ServerSideToolUsageDetails; got == nil || *got != *first.ServerSideToolUsageDetails {
		t.Fatalf("one side reporting: merged counts = %+v, want %+v", got, first.ServerSideToolUsageDetails)
	}
	if got := MergeBifrostLLMUsage(none, &BifrostLLMUsage{PromptTokens: 4}).ServerSideToolUsageDetails; got != nil {
		t.Fatalf("neither side reporting: merged counts = %+v, want nil", got)
	}
	// Every counter of the type is summed: a counter added later fails here until Add has it.
	var all ResponsesServerSideToolUsageDetails
	v := reflect.ValueOf(&all).Elem()
	for i := 0; i < v.NumField(); i++ {
		v.Field(i).SetInt(1)
	}
	sum := reflect.ValueOf(*all.Add(&all))
	for i := 0; i < sum.NumField(); i++ {
		if sum.Field(i).Int() != 2 {
			t.Errorf("Add does not sum %s", sum.Type().Field(i).Name)
		}
	}
}

func TestRateLimitTokens(t *testing.T) {
	p := &ProcessedTokenUsage{PromptTokens: 71, CompletionTokens: 418}
	cases := []struct {
		name      string
		chat      *BifrostLLMUsage
		responses *ResponsesResponseUsage
		want      int
	}{
		{"nil", nil, nil, 0},
		{"total only", &BifrostLLMUsage{TotalTokens: 30}, &ResponsesResponseUsage{TotalTokens: 30}, 30},
		{"processed above billed", &BifrostLLMUsage{TotalTokens: 423, ProcessedTokens: p}, &ResponsesResponseUsage{TotalTokens: 423, ProcessedTokens: p}, 489},
		{"billed above processed", &BifrostLLMUsage{TotalTokens: 600, ProcessedTokens: p}, &ResponsesResponseUsage{TotalTokens: 600, ProcessedTokens: p}, 600},
	}
	for _, tc := range cases {
		if got := tc.chat.RateLimitTokens(); got != tc.want {
			t.Errorf("%s: BifrostLLMUsage.RateLimitTokens() = %d, want %d", tc.name, got, tc.want)
		}
		if got := tc.responses.RateLimitTokens(); got != tc.want {
			t.Errorf("%s: ResponsesResponseUsage.RateLimitTokens() = %d, want %d", tc.name, got, tc.want)
		}
	}
}
