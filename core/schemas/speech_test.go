package schemas

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// BackfillParams used to overwrite usage.input_chars with Bifrost's code-point count on every
// response, replacing a count the vendor reported (MiniMax usage_characters). The vendor's
// figure now stays; Bifrost's count fills the field only when the vendor reported none.
func TestSpeechBackfillParamsKeepsVendorCharacterCount(t *testing.T) {
	req := &BifrostSpeechRequest{Input: &SpeechInput{Input: "你好, world"}} // 9 code points

	resp := &BifrostSpeechResponse{Usage: &SpeechUsage{InputChars: 13}}
	resp.BackfillParams(req)
	if resp.Usage.InputChars != 13 {
		t.Fatalf("input_chars = %d, want the vendor's 13", resp.Usage.InputChars)
	}

	stream := &BifrostSpeechStreamResponse{Usage: &SpeechUsage{InputChars: 13}}
	stream.BackfillParams(req)
	if stream.Usage.InputChars != 13 {
		t.Fatalf("stream input_chars = %d, want the vendor's 13", stream.Usage.InputChars)
	}
}

func TestSpeechBackfillParamsFillsUnreportedCharacterCount(t *testing.T) {
	req := &BifrostSpeechRequest{Input: &SpeechInput{Input: "你好, world"}}

	resp := &BifrostSpeechResponse{}
	resp.BackfillParams(req)
	if resp.Usage == nil || resp.Usage.InputChars != 9 {
		t.Fatalf("usage = %+v, want input_chars 9", resp.Usage)
	}

	stream := &BifrostSpeechStreamResponse{Usage: &SpeechUsage{OutputTokens: 4}}
	stream.BackfillParams(req)
	if stream.Usage.InputChars != 9 || stream.Usage.OutputTokens != 4 {
		t.Fatalf("stream usage = %+v, want input_chars 9 and the vendor's output_tokens", stream.Usage)
	}
}

// The semantic cache stores a response as JSON and replays it on a hit; the provider-recorded
// media type of headerless audio (Gemini raw PCM) must survive that, or a cached hit is labeled
// audio/mpeg.
func TestSpeechResponseContentTypeSurvivesJSONRoundTrip(t *testing.T) {
	in := BifrostSpeechResponse{Audio: []byte{1, 0, 2, 0}, ContentType: "audio/L16;codec=pcm;rate=24000"}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out BifrostSpeechResponse
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if out.ContentType != in.ContentType {
		t.Fatalf("content type after round trip = %q, want %q (json %s)", out.ContentType, in.ContentType, data)
	}
	empty, _ := json.Marshal(BifrostSpeechResponse{Audio: []byte{1}})
	if strings.Contains(string(empty), "content_type") {
		t.Fatalf("an unknown media type must be omitted: %s", empty)
	}
}

// TXE-7073: the input character count a per-character price applies to is the vendor's
// billed count, else Bifrost's estimate of it, else Bifrost's code-point count.
func TestSpeechUsageBilledInputCharsOrder(t *testing.T) {
	billed, estimate := 13, 30
	cases := []struct {
		name  string
		usage *SpeechUsage
		want  int
	}{
		{"nil usage", nil, 0},
		{"input chars only", &SpeechUsage{InputChars: 1}, 1},
		{"estimate over input chars", &SpeechUsage{InputChars: 1, EstimatedBilledCharacters: &estimate}, 30},
		{"vendor count over estimate", &SpeechUsage{InputChars: 1, BilledCharacters: &billed, EstimatedBilledCharacters: &estimate}, 13},
		{"vendor count over input chars", &SpeechUsage{InputChars: 1, BilledCharacters: &billed}, 13},
	}
	for _, tc := range cases {
		if got := tc.usage.BilledInputChars(); got != tc.want {
			t.Errorf("%s: BilledInputChars() = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// speechCharacterBilling says, for every provider that implements Speech, where the quantity
// Tokun and Bifrost bill a synthesis on comes from (contract C5, TXE-7073). A provider that
// implements Speech and is missing here fails TestEverySpeechProviderDeclaresHowItsCharactersAreBilled:
// whoever adds one decides, against the vendor's billing rule, which of these it is.
var speechCharacterBilling = map[string]struct {
	source speechBillingSource
	why    string
}{
	"azurespeech":    {speechBilledEstimate, "Azure reports no count and bills the SSML markup Bifrost adds (<prosody>) and each Han character twice: EstimateBilledCharacters"},
	"minimax":        {speechBilledVendorCharacters, "usage_characters"},
	"dashscope":      {speechBilledVendorCharacters, "usage.characters (Qwen3-TTS)"},
	"elevenlabs":     {speechBilledVendorCost, "character-cost header"},
	"gemini":         {speechBilledVendorQuantity, "usageMetadata tokens"},
	"vertex":         {speechBilledVendorQuantity, "usageMetadata tokens, through the Gemini converter"},
	"xai":            {speechBilledVendorQuantity, "audio duration"},
	"openai":         {speechBilledInputChars, "input sent as given, no markup added"},
	"azure":          {speechBilledInputChars, "the OpenAI speech API: input sent as given, no markup added"},
	"groq":           {speechBilledInputChars, "the OpenAI speech API: input sent as given, no markup added"},
	"openrouter":     {speechBilledInputChars, "the OpenAI speech API: input sent as given, no markup added"},
	"huggingface":    {speechBilledInputChars, "input sent as given, no markup added"},
	"sarvam":         {speechBilledInputChars, "input sent as given, no markup added"},
	"googlecloudtts": {speechBilledInputChars, "the caller's text or SSML sent as given; Bifrost adds no markup"},
}

type speechBillingSource int

const (
	// speechBilledVendorCharacters: the vendor reports its billed characters (billed_characters).
	speechBilledVendorCharacters speechBillingSource = iota
	// speechBilledVendorCost: the vendor reports what it charged (vendor_cost).
	speechBilledVendorCost
	// speechBilledVendorQuantity: the vendor bills a quantity it reports other than characters.
	speechBilledVendorQuantity
	// speechBilledEstimate: the vendor reports nothing and bills more than the input's code
	// points; the provider sets estimated_billed_characters by the vendor's documented rule.
	speechBilledEstimate
	// speechBilledInputChars: the vendor reports nothing, and Bifrost sends the caller's input
	// unchanged, so input_chars (Bifrost's code-point count) is the measure.
	speechBilledInputChars
)

// TestEverySpeechProviderDeclaresHowItsCharactersAreBilled scans core/providers for every
// provider whose Speech method does more than refuse the operation, and requires a row in
// speechCharacterBilling for it. It also holds each row to what the provider's code does: an
// estimate row sets EstimatedBilledCharacters, a vendor-characters row sets BilledCharacters,
// and no other provider sets EstimatedBilledCharacters.
func TestEverySpeechProviderDeclaresHowItsCharactersAreBilled(t *testing.T) {
	dirs, err := os.ReadDir(filepath.Join("..", "providers"))
	if err != nil {
		t.Fatal(err)
	}
	supported := map[string]bool{}
	setsField := map[string]map[string]bool{}
	for _, dir := range dirs {
		if !dir.IsDir() {
			continue
		}
		name := dir.Name()
		fset := token.NewFileSet()
		files, err := filepath.Glob(filepath.Join("..", "providers", name, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range files {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			ast.Inspect(file, func(n ast.Node) bool {
				switch node := n.(type) {
				case *ast.FuncDecl:
					if node.Recv != nil && node.Name.Name == "Speech" && node.Body != nil && !refusesOperation(node.Body) {
						supported[name] = true
					}
				case *ast.KeyValueExpr:
					if key, ok := node.Key.(*ast.Ident); ok {
						markField(setsField, name, key.Name)
					}
				case *ast.SelectorExpr:
					markField(setsField, name, node.Sel.Name)
				}
				return true
			})
		}
	}
	if len(supported) < 10 {
		t.Fatalf("found %d speech providers, want at least 10: the scan is not reading core/providers", len(supported))
	}

	var missing []string
	for name := range supported {
		if _, ok := speechCharacterBilling[name]; !ok {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("speech providers with no row in speechCharacterBilling: %v. Read the vendor's billing rule and say where the billed quantity comes from (TXE-7073)", missing)
	}
	for name, row := range speechCharacterBilling {
		if !supported[name] {
			t.Errorf("speechCharacterBilling lists %q, which does not implement Speech: remove the row", name)
			continue
		}
		sets := setsField[name]
		if row.source == speechBilledEstimate && !sets["EstimatedBilledCharacters"] {
			t.Errorf("%s is listed as an estimate (%s) and never sets EstimatedBilledCharacters", name, row.why)
		}
		if row.source != speechBilledEstimate && sets["EstimatedBilledCharacters"] {
			t.Errorf("%s sets EstimatedBilledCharacters and is not listed as an estimate", name)
		}
		if row.source == speechBilledVendorCharacters && !sets["BilledCharacters"] {
			t.Errorf("%s is listed as vendor-reported characters (%s) and never sets BilledCharacters", name, row.why)
		}
	}
}

func markField(sets map[string]map[string]bool, provider, field string) {
	if field != "EstimatedBilledCharacters" && field != "BilledCharacters" {
		return
	}
	if sets[provider] == nil {
		sets[provider] = map[string]bool{}
	}
	sets[provider][field] = true
}

// refusesOperation reports whether body is a single return whose value calls
// NewUnsupportedOperationError: a provider that does not implement the operation.
func refusesOperation(body *ast.BlockStmt) bool {
	if len(body.List) != 1 {
		return false
	}
	ret, ok := body.List[0].(*ast.ReturnStmt)
	if !ok {
		return false
	}
	for _, result := range ret.Results {
		call, ok := result.(*ast.CallExpr)
		if !ok {
			continue
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "NewUnsupportedOperationError" {
			return true
		}
	}
	return false
}
