package lib

import (
	"encoding/json"
	"testing"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/tidwall/gjson"
	"github.com/valyala/fasthttp"
)

// A converted body carries the upstream's own response under extra_fields.raw_response.
func TestAttachRawResponseConvertedBody(t *testing.T) {
	body := []byte(`{"id":"resp_1","usage":{"input_tokens":12},"extra_fields":{"provider":"openai"}}`)
	raw := json.RawMessage(`{"id":"resp_1","usage":{"input_tokens":12,"input_tokens_details":{"cached_tokens":4}}}`)
	out := AttachRawResponse(body, raw)
	if got := gjson.GetBytes(out, "extra_fields.raw_response.usage.input_tokens_details.cached_tokens").Int(); got != 4 {
		t.Fatalf("raw response not attached: %s", out)
	}
	if gjson.GetBytes(out, "extra_fields.provider").Str != "openai" || gjson.GetBytes(out, "extra_fields.raw_response_in_body").Exists() {
		t.Fatalf("extra_fields changed beyond the raw response: %s", out)
	}
}

// A body that is the upstream's response, byte for byte after compaction, says so instead of
// carrying a second copy.
func TestAttachRawResponseBodyIsRaw(t *testing.T) {
	body := []byte(`{"data":[{"b64_json":"AA=="}],"usage":{"output_tokens":272}}`)
	raw := json.RawMessage("{\n  \"data\": [{\"b64_json\": \"AA==\"}],\n  \"usage\": {\"output_tokens\": 272}\n}")
	out := AttachRawResponse(body, raw)
	if !gjson.GetBytes(out, "extra_fields.raw_response_in_body").Bool() || gjson.GetBytes(out, "extra_fields.raw_response").Exists() {
		t.Fatalf("in-body raw response not marked: %s", out)
	}
	if gjson.GetBytes(out, "usage.output_tokens").Int() != 272 {
		t.Fatalf("body changed: %s", out)
	}
}

// A string raw response (a stream event's JSON text) attaches as JSON; one that is not JSON,
// or a body that is not a JSON object, leaves the body as it was.
func TestAttachRawResponseShapes(t *testing.T) {
	out := AttachRawResponse([]byte(`{"type":"image_generation.completed"}`), `{"type":"image_generation.completed","usage":{"output_tokens":9}}`)
	if gjson.GetBytes(out, "extra_fields.raw_response.usage.output_tokens").Int() != 9 {
		t.Fatalf("string raw response not attached: %s", out)
	}
	for _, c := range []struct {
		body string
		raw  interface{}
	}{
		{`{"a":1}`, []byte{0x00, 0x00, 0x00, 0x18, 'f', 't', 'y', 'p'}},
		{`[1,2]`, json.RawMessage(`{"a":1}`)},
		{`{"a":1}`, nil},
	} {
		if out := AttachRawResponse([]byte(c.body), c.raw); string(out) != c.body {
			t.Fatalf("body %s changed to %s", c.body, out)
		}
	}
}

// The raw response is kept and attached only for a caller that asked for it.
func TestKeptRawResponseFollowsTheRequest(t *testing.T) {
	extra := schemas.BifrostResponseExtraFields{RawResponse: json.RawMessage(`{"usage":{"prompt_tokens":3}}`)}
	body := []byte(`{"usage":{"prompt_tokens":3},"model":"m"}`)

	asked := &fasthttp.RequestCtx{}
	askedCtx := schemas.NewBifrostContext(nil, schemas.NoDeadline)
	askedCtx.SetValue(schemas.BifrostContextKeySendBackRawResponse, true)
	keepRawResponse(asked, askedCtx, extra)
	if out := AttachKeptRawResponse(asked, body); gjson.GetBytes(out, "extra_fields.raw_response.usage.prompt_tokens").Int() != 3 {
		t.Fatalf("asked: %s", out)
	}

	notAsked := &fasthttp.RequestCtx{}
	keepRawResponse(notAsked, schemas.NewBifrostContext(nil, schemas.NoDeadline), extra)
	if out := AttachKeptRawResponse(notAsked, body); string(out) != string(body) {
		t.Fatalf("not asked: %s", out)
	}
}
