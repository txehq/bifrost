package utils

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

func slim(line string) []byte {
	s := newLineSlimmer()
	s.Write([]byte(line))
	return s.Result()
}

// TXE-7526: each vendor's oversized line keeps what settlement bills (ids, status, model and
// usage) while its bulk (a base64 image, a long text, an embedding vector) is cut.
func TestLineSlimmerKeepsEveryVendorsUsage(t *testing.T) {
	huge := strings.Repeat("A", 3<<20)
	vector := strings.TrimSuffix(strings.Repeat("0.0123456789,", 200000), ",")
	for name, tc := range map[string]struct{ line, want string }{
		"openai image": {
			`{"id":"batch_req_1","custom_id":"img-1","response":{"status_code":200,"request_id":"r","body":{"model":"gpt-image-1","data":[{"b64_json":"` + huge + `"}],"usage":{"input_tokens":12,"output_tokens":4160,"total_tokens":4172,"output_tokens_details":{"image_tokens":4160}}}},"error":null}` + "\n",
			`{"id":"batch_req_1","custom_id":"img-1","response":{"status_code":200,"request_id":"r","body":{"model":"gpt-image-1","data":[{"b64_json":""}],"usage":{"input_tokens":12,"output_tokens":4160,"total_tokens":4172,"output_tokens_details":{"image_tokens":4160}}}},"error":null}`,
		},
		"openai embedding": {
			`{"custom_id":"e-1","response":{"status_code":200,"body":{"object":"list","model":"text-embedding-3-large","data":[{"object":"embedding","index":0,"embedding":[` + vector + `]}],"usage":{"prompt_tokens":8,"total_tokens":8}}}}`,
			`{"custom_id":"e-1","response":{"status_code":200,"body":{"object":"list","model":"text-embedding-3-large","data":[{"object":"embedding","index":0,"embedding":[` + strings.TrimSuffix(strings.Repeat("0.0123456789,", 16), ",") + `]}],"usage":{"prompt_tokens":8,"total_tokens":8}}}}`,
		},
		"gemini image": {
			`{"key":"k-1","response":{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"` + huge + `"}}]}}],"usageMetadata":{"promptTokenCount":9,"candidatesTokenCount":1290,"totalTokenCount":1299},"modelVersion":"gemini-2.5-flash-image"}}`,
			`{"key":"k-1","response":{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":""}}]}}],"usageMetadata":{"promptTokenCount":9,"candidatesTokenCount":1290,"totalTokenCount":1299},"modelVersion":"gemini-2.5-flash-image"}}`,
		},
		"anthropic text": {
			`{"custom_id":"a-1","result":{"type":"succeeded","message":{"model":"claude-opus-4-7","content":[{"type":"text","text":"` + huge + `"}],"usage":{"input_tokens":30,"output_tokens":64000,"cache_read_input_tokens":5}}}}`,
			`{"custom_id":"a-1","result":{"type":"succeeded","message":{"model":"claude-opus-4-7","content":[{"type":"text","text":""}],"usage":{"input_tokens":30,"output_tokens":64000,"cache_read_input_tokens":5}}}}`,
		},
	} {
		if got := slim(tc.line); string(got) != tc.want {
			t.Errorf("%s: slim line\n%.400s\nwant\n%.400s", name, got, tc.want)
		}
	}
}

func TestLineSlimmerKeepsShortValuesAndEscapes(t *testing.T) {
	for _, tc := range []struct{ line, want string }{
		{`{"a":"say \"hi\"\\","b":"\u0041\n","c":[],"d":{},"e":-1.5e+3,"f":true,"g":null}`, `{"a":"say \"hi\"\\","b":"\u0041\n","c":[],"d":{},"e":-1.5e+3,"f":true,"g":null}`},
		{` { "a" : [ 1 , 2 ] }  ` + "\r\n", `{"a":[1,2]}`},
		{`{"long":"` + strings.Repeat(`\"`, 600) + `"}`, `{"long":""}`},
		{`[` + strings.TrimSuffix(strings.Repeat(`{"x":1},`, 40), ",") + `]`, `[` + strings.TrimSuffix(strings.Repeat(`{"x":1},`, 16), ",") + `]`},
		{`[[` + strings.TrimSuffix(strings.Repeat(`[1,2],`, 20), ",") + `]]`, `[[` + strings.TrimSuffix(strings.Repeat(`[1,2],`, 16), ",") + `]]`},
		{`"just a string"`, `"just a string"`},
		{`42`, `42`},
	} {
		if got := slim(tc.line); string(got) != tc.want {
			t.Errorf("slim(%.80q) = %.200q, want %.200q", tc.line, got, tc.want)
		}
	}
}

func TestLineSlimmerGivesUpOnWhatItCannotKeep(t *testing.T) {
	for name, line := range map[string]string{
		"cut off":           `{"a":"b`,
		"missing colon":     `{"a" 1}`,
		"trailing comma":    `[1,]`,
		"comma first":       `{,}`,
		"bad literal":       `{"a":tru}`,
		"two values":        `{} {}`,
		"unclosed":          `{"a":[1,2]`,
		"stray close":       `]`,
		"control in string": "{\"a\":\"x\ny\"}",
		"too deep":          strings.Repeat("[", slimDepth+1) + strings.Repeat("]", slimDepth+1),
		"too many keys":     `{` + strings.TrimSuffix(strings.Repeat(`"k":1,`, slimLineBytes/4), ",") + `}`,
		"long number":       `[` + strings.Repeat("1", slimScalarBytes+1) + `]`,
		"not json":          `hello`,
		"empty":             ``,
		"mismatched close":  `{"a":1]`,
		"key without value": `{"a"}`,
		"value without key": `{1}`,
		"close after colon": `{"a":}`,
		"array colon":       `[1:2]`,
		"top-level comma":   `1,2`,
	} {
		if got := slim(line); got != nil {
			t.Errorf("%s: slim(%.80q) = %.80q, want nil", name, line, got)
		}
	}
}

// Fed in any chunks, and for any JSON with no long string or array, the slim line is the
// input compacted.
func TestLineSlimmerIsTheCompactLineForSmallJSON(t *testing.T) {
	r := rand.New(rand.NewSource(7526))
	var gen func(depth int) any
	gen = func(depth int) any {
		switch k := r.Intn(7); {
		case k == 0 || depth > 4:
			return r.Intn(2000) - 1000
		case k == 1:
			return fmt.Sprintf("s%d \"q\" \\ é", r.Intn(100))
		case k == 2:
			return r.Intn(2) == 0
		case k == 3:
			return nil
		case k == 4:
			out := []any{}
			for range r.Intn(slimArrayItems + 1) {
				out = append(out, gen(depth+1))
			}
			return out
		default:
			out := map[string]any{}
			for i := range r.Intn(6) {
				out[fmt.Sprintf("k%d", i)] = gen(depth + 1)
			}
			return out
		}
	}
	for i := range 500 {
		value := gen(0)
		raw, _ := json.MarshalIndent(value, "", " ")
		var want bytes.Buffer
		_ = json.Compact(&want, raw)
		s := newLineSlimmer()
		for start := 0; start < len(raw); {
			end := min(len(raw), start+1+r.Intn(7))
			s.Write(raw[start:end])
			start = end
		}
		if got := s.Result(); !bytes.Equal(got, want.Bytes()) {
			t.Fatalf("case %d: slim\n%s\nwant\n%s", i, got, want.Bytes())
		}
	}
}

// BenchmarkLineSlimmer measures a 3 MiB image line streamed through the slimmer: the work an
// oversized result line costs, which no ordinary line pays.
func BenchmarkLineSlimmer(b *testing.B) {
	line := []byte(`{"custom_id":"img-1","response":{"status_code":200,"body":{"model":"gpt-image-1","data":[{"b64_json":"` + strings.Repeat("A", 3<<20) + `"}],"usage":{"input_tokens":12,"output_tokens":4160,"total_tokens":4172}}}}`)
	b.SetBytes(int64(len(line)))
	b.ReportAllocs()
	for b.Loop() {
		s := newLineSlimmer()
		for start := 0; start < len(line); start += 64 * 1024 {
			s.Write(line[start:min(len(line), start+64*1024)])
		}
		if s.Result() == nil {
			b.Fatal("no slim line")
		}
	}
}
