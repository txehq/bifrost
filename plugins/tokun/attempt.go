package tokun

import (
	"fmt"
	"net/http"
	"reflect"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/capsohq/bifrost/core/schemas"
)

// HeaderStrippedParams is the response header that lists, comma separated, the strip_params
// the attempt that answered removed from the request body.
const HeaderStrippedParams = "tokun-stripped-params"

// integrationOpenAI is the integration type (BifrostContextKeyIntegrationType) of the OpenAI
// format's routes, the only format whose streams carry usage only when the body asks for it.
const integrationOpenAI = "openai"

// rewriteAttempt rewrites the request for one attempt, the entry e, always from the caller's
// body as Bifrost received it, so no attempt sees another attempt's rewrite:
//   - the parameters of e's strip_params present in the body are removed, and listed for
//     tokun-stripped-params;
//   - an attempt in the request's own format (same_format) sends the body as it is
//     (BifrostContextKeyUseRawRequestBody), changed in place only where it has to be: the
//     top-level model becomes e's forward_model, and a streamed OpenAI chat or completions body
//     gets stream_options.include_usage, so the stream reports usage;
//   - an attempt the target provider converts goes out from the typed request, whose provider,
//     model and key core set from e. When parameters were removed, the typed request is built
//     again from the stripped body by the route's own converter.
//
// A request whose route keeps no body (one that has none, a multipart upload, a route outside
// the SDK integrations) always goes out from the typed request.
//
// Every attempt carries e's interface_output_tokens (BifrostContextKeyAttemptOutputTokens): the
// provider sends the caller's max_tokens and thinking budget as written, and a format that
// requires max_tokens gets interface_output_tokens when the caller wrote none.
func (p *Plugin) rewriteAttempt(ctx *schemas.BifrostContext, req *schemas.BifrostRequest, state *requestState, e entry) (*schemas.BifrostRequest, *schemas.BifrostError) {
	ctx.SetValue(schemas.BifrostContextKeyAttemptOutputTokens, int(e.upstream.GetInterfaceOutputTokens()))
	if len(state.body) == 0 {
		state.stripped = nil
		ctx.SetValue(schemas.BifrostContextKeyUseRawRequestBody, false)
		return req, nil
	}
	body, stripped, err := stripParams(state.body, e.upstream.GetStripParams())
	if err != nil {
		return nil, rewriteError(err)
	}
	state.stripped = stripped

	if e.upstream.GetSameFormat() {
		if gjson.GetBytes(body, "model").Exists() {
			if body, err = sjson.SetBytes(body, "model", e.upstream.GetForwardModel()); err != nil {
				return nil, rewriteError(fmt.Errorf("set the model: %w", err))
			}
		}
		if state.openAIStream {
			if body, err = askForStreamUsage(body); err != nil {
				return nil, rewriteError(err)
			}
		}
		ctx.SetValue(schemas.BifrostContextKeyUseRawRequestBody, true)
		return req.WithRawRequestBody(body), nil
	}

	if len(stripped) == 0 {
		ctx.SetValue(schemas.BifrostContextKeyUseRawRequestBody, false)
		return req, nil
	}
	convert, ok := ctx.Value(schemas.BifrostContextKeyRequestBodyConverter).(schemas.RequestBodyConverter)
	if !ok {
		return nil, rewriteError(fmt.Errorf("the route kept the body but put no request body converter on the context"))
	}
	converted, err := convert(ctx, body)
	if err != nil {
		return nil, rewriteError(fmt.Errorf("convert the stripped body: %w", err))
	}
	// The conversion runs the route's pre-request callback again, which may choose the raw body
	// for its own reasons; this attempt is converted.
	ctx.SetValue(schemas.BifrostContextKeyUseRawRequestBody, false)
	provider, model, _ := req.GetRequestFields()
	converted.RequestType = req.RequestType
	converted.SetProvider(provider)
	converted.SetModel(model)
	converted.SetFallbacks(nil)
	return converted, nil
}

// stripParams removes from body every path of params that is present in it, and returns the
// paths it removed. body itself is never changed; when nothing is removed it is returned as
// it is. A path is a gjson path into the body, in the format of the request.
func stripParams(body []byte, params []string) ([]byte, []string, error) {
	var stripped []string
	for _, path := range params {
		if !gjson.GetBytes(body, path).Exists() {
			continue
		}
		var err error
		if body, err = sjson.DeleteBytes(body, path); err != nil {
			return nil, nil, fmt.Errorf("remove %s: %w", path, err)
		}
		stripped = append(stripped, path)
	}
	return body, stripped, nil
}

// askForStreamUsage sets stream_options.include_usage to true in an OpenAI chat or completions
// body, keeping the rest of stream_options. A stream_options that is not an object cannot hold
// it and is replaced.
func askForStreamUsage(body []byte) ([]byte, error) {
	options := gjson.GetBytes(body, "stream_options")
	var err error
	if options.Exists() && !options.IsObject() {
		body, err = sjson.SetRawBytes(body, "stream_options", []byte(`{"include_usage":true}`))
	} else {
		body, err = sjson.SetBytes(body, "stream_options.include_usage", true)
	}
	if err != nil {
		return nil, fmt.Errorf("set stream_options.include_usage: %w", err)
	}
	return body, nil
}

// rewriteError fails an attempt whose request could not be rewritten. The body is the same for
// every attempt, so nothing falls back from it.
func rewriteError(err error) *schemas.BifrostError {
	return sendError(http.StatusInternalServerError, "tokun_attempt_rewrite_failed", err.Error())
}

// strippedHeader writes tokun-stripped-params for a successful response whose attempt removed
// parameters. The attempt that answered a successful response is the last one made.
func strippedHeader(state *requestState, resp *schemas.HTTPResponseMetadata) {
	if len(state.stripped) == 0 || resp.StatusCode >= http.StatusBadRequest {
		return
	}
	resp.SetHeader(HeaderStrippedParams, strings.Join(state.stripped, ", "))
}

// TODO(tokun-B1): 判断这次尝试失败的错误范围（key、upstream、request），记下尝试记录（序号、provider_id、upstream_id、key_id、状态码、错误范围、耗时、上游耗时）；错误范围在计划的 stop_scopes 里时把 AllowFallbacks 设为 false
// 阶段：B1 放入；B1-D6「执行」完成时删除这两行和占位
func (p *Plugin) recordAttempt(ctx *schemas.BifrostContext, state *requestState, attempt int, resp *schemas.BifrostResponse, bifrostErr *schemas.BifrostError) *schemas.BifrostError {
	return bifrostErr
}

// TODO(tokun-B1): 把 request_id、send_id、抽出的顺序和每次尝试（包括跳过的项）编码成 RouteAttempts，写进响应头 tk-route-attempts
// 阶段：B1 放入；B1-D6「执行」完成时删除这两行和占位
func (p *Plugin) writeAttempts(state *requestState, resp *schemas.HTTPResponseMetadata) {}

// filterChunk keeps the usage the plugin asked for out of a stream whose caller did not ask for
// it. On a streamed OpenAI chat or completions request without stream_options.include_usage, a
// chunk that carries usage and nothing else is dropped, and a chunk that carries output and
// usage is passed on without the usage. Every other chunk, and every chunk of a caller that
// asked for usage, passes unchanged.
func (p *Plugin) filterChunk(state *requestState, chunk *schemas.BifrostStreamChunk) *schemas.BifrostStreamChunk {
	if !state.dropUsage || chunk == nil {
		return chunk
	}
	switch {
	case chunk.BifrostChatResponse != nil && chunk.BifrostChatResponse.Usage != nil:
		if !choicesCarryOutput(chunk.BifrostChatResponse.Choices) {
			return nil
		}
		withoutUsage := *chunk.BifrostChatResponse
		withoutUsage.Usage = nil
		return &schemas.BifrostStreamChunk{BifrostChatResponse: &withoutUsage}
	case chunk.BifrostTextCompletionResponse != nil && chunk.BifrostTextCompletionResponse.Usage != nil:
		if !choicesCarryOutput(chunk.BifrostTextCompletionResponse.Choices) {
			return nil
		}
		withoutUsage := *chunk.BifrostTextCompletionResponse
		withoutUsage.Usage = nil
		return &schemas.BifrostStreamChunk{BifrostTextCompletionResponse: &withoutUsage}
	}
	return chunk
}

// choicesCarryOutput reports whether any choice of a stream chunk carries something besides its
// index: a finish reason, log probabilities, content filter results, text, or a delta that is
// not empty.
func choicesCarryOutput(choices []schemas.BifrostResponseChoice) bool {
	for i := range choices {
		c := &choices[i]
		switch {
		case c.FinishReason != nil && *c.FinishReason != "",
			c.LogProbs != nil,
			len(c.ContentFilterResults) > 0,
			c.ChatNonStreamResponseChoice != nil,
			c.TextCompletionResponseChoice != nil && c.Text != nil && *c.Text != "",
			c.ChatStreamResponseChoice != nil && c.Delta != nil && !reflect.ValueOf(c.Delta).Elem().IsZero():
			return true
		}
	}
	return false
}
