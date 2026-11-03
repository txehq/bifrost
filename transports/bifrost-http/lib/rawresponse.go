package lib

import (
	"bytes"
	"encoding/json"

	"github.com/bytedance/sonic"
	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

// FastHTTPUserValueRawResponse holds the upstream's raw response of a non-streaming request
// whose caller asked for it (x-bf-send-back-raw-response), for the body written after the
// routed-identity headers.
const FastHTTPUserValueRawResponse = "__bifrost_raw_response"

// The keys a caller that asked for the raw response finds under extra_fields of every JSON
// object Bifrost returns it, whatever the route's format: raw_response is the upstream's own
// response, and raw_response_in_body says the body itself is that response.
const (
	rawResponseKey       = extraFieldsKey + ".raw_response"
	rawResponseInBodyKey = extraFieldsKey + ".raw_response_in_body"
)

// RawResponseRequested reports whether the request asked for the upstream's raw response; a
// handler run without a Bifrost context has asked for nothing.
func RawResponseRequested(bifrostCtx *schemas.BifrostContext) bool {
	if bifrostCtx == nil {
		return false
	}
	requested, _ := bifrostCtx.Value(schemas.BifrostContextKeySendBackRawResponse).(bool)
	return requested
}

// rawJSON is a captured raw response as JSON bytes: providers keep it as json.RawMessage, a
// stream event as the event's JSON text.
func rawJSON(raw interface{}) ([]byte, bool) {
	switch r := raw.(type) {
	case nil:
		return nil, false
	case json.RawMessage:
		return r, len(r) > 0
	case []byte:
		return r, len(r) > 0
	case string:
		return []byte(r), r != ""
	}
	b, err := sonic.Marshal(raw)
	return b, err == nil && len(b) > 0
}

// AttachRawResponse gives a JSON object body the upstream's raw response under extra_fields:
// raw_response_in_body when the body is that response, otherwise raw_response. A body that
// is not a JSON object, or a raw response that is not JSON, leaves the body unchanged.
func AttachRawResponse(body []byte, raw interface{}) []byte {
	rawBytes, ok := rawJSON(raw)
	if !ok || !json.Valid(rawBytes) {
		return body
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return body
	}
	var compactBody, compactRaw bytes.Buffer
	if json.Compact(&compactBody, trimmed) == nil && json.Compact(&compactRaw, rawBytes) == nil && bytes.Equal(compactBody.Bytes(), compactRaw.Bytes()) {
		out, err := providerUtils.SetJSONField(body, rawResponseInBodyKey, true)
		if err != nil {
			return body
		}
		return out
	}
	out, err := providerUtils.SetRawJSONField(body, rawResponseKey, rawBytes)
	if err != nil {
		return body
	}
	return out
}

// keepRawResponse stores the response's raw response for the body sendSuccess writes, when
// the caller asked for it.
func keepRawResponse(ctx *fasthttp.RequestCtx, bifrostCtx *schemas.BifrostContext, extra schemas.BifrostResponseExtraFields) {
	if extra.RawResponse != nil && RawResponseRequested(bifrostCtx) {
		ctx.SetUserValue(FastHTTPUserValueRawResponse, extra.RawResponse)
	}
}

// AttachKeptRawResponse is AttachRawResponse with the raw response keepRawResponse stored.
func AttachKeptRawResponse(ctx *fasthttp.RequestCtx, body []byte) []byte {
	raw := ctx.UserValue(FastHTTPUserValueRawResponse)
	if raw == nil {
		return body
	}
	return AttachRawResponse(body, raw)
}
