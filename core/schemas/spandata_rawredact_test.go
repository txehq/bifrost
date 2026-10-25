package schemas

import (
	"encoding/json"
	"strings"
	"testing"
)

// Guardrail replacements are literal text, but a raw provider body carries that
// text JSON-escaped. Raw payloads are exported when a connector opts in, so a
// replacement the typed fields honour must not survive in the raw body.
func TestRedactRawPayloadMatchesJSONEscapedContent(t *testing.T) {
	const secret = "Jane \"JJ\" Doe\nlives at <5 Main St> & co"
	const masked = "[REDACTED]"
	replacements := map[string]string{secret: masked}

	body, err := json.Marshal(map[string]any{"output": []any{map[string]any{"text": "name: " + secret}}})
	if err != nil {
		t.Fatal(err)
	}
	unicodeBody := `{"text":"café owner ` + strings.ReplaceAll(strings.ReplaceAll(secret, `"`, `\"`), "\n", `\n`) + `"}`
	sse := "data: " + string(body) + "\n\ndata: [DONE]\n\n"

	for name, raw := range map[string]string{"json": string(body), "unicode escape": unicodeBody, "sse": sse} {
		d := &LLMSpanData{RawRequest: raw, RawResponse: raw}
		d.redact(replacements, replacements)
		for side, got := range map[string]string{"request": d.RawRequest, "response": d.RawResponse} {
			for _, leak := range []string{secret, "JJ", "Main St"} {
				if strings.Contains(got, leak) {
					t.Errorf("%s %s: raw payload still holds %q: %s", name, side, leak, got)
				}
			}
			if !strings.Contains(got, masked) {
				t.Errorf("%s %s: replacement not applied: %s", name, side, got)
			}
		}
	}

	// A body the replacements do not touch is left byte-for-byte as the provider sent it.
	untouched := `{"b": 1,  "a": "fine"}`
	d := &LLMSpanData{RawResponse: untouched}
	d.redact(nil, replacements)
	if d.RawResponse != untouched {
		t.Errorf("unmatched body was rewritten: %q", d.RawResponse)
	}
}
