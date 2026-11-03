package tokun

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/schemas"

	routingv1 "github.com/capsohq/txe/tokun/proto/gen/go/tokun/routing/v1"
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

// The error scopes of RouteAttempt.error_scope, and the routing.stop_scopes a plan may list.
const (
	scopeKey      = "key"
	scopeUpstream = "upstream"
	scopeRequest  = "request"
)

// skipProviderNotSynced is the skip_reason of an entry whose provider entry is not on this
// Bifrost (RouteAttempt.skip_reason).
const skipProviderNotSynced = "provider_not_synced"

// The healthobs failure classes of RouteAttempt.error_class.
const (
	classTransport    = "transport"
	classUpstream5xx  = "upstream_5xx"
	classUpstream4xx  = "upstream_4xx"
	classThrottled    = "throttled"
	classAuth         = "auth"
	classStreamBroken = "stream_broken"
)

// attemptRecord is what the plugin keeps of one attempt until tk-route-attempts is written.
type attemptRecord struct {
	entry            int           // position in the drawn order (RouteAttempt.index)
	startedAt        time.Time     // when PreLLMHook began the attempt
	upstreamAtStart  time.Duration // the send's upstream time when the attempt began
	duration         time.Duration
	upstreamDuration time.Duration
	status           int32
	scope, class     string
	code, message    string
	ended            bool // PostLLMHook saw the attempt answer or fail
	answered         bool // a response or a chunk of the attempt passed PostLLMHook
}

// beginAttempt records the start of the attempt at position attempt of the chain, in PreLLMHook.
// An attempt core begins again on the same entry (a later turn of an MCP agent loop) starts its
// record over. An earlier attempt that never reached PostLLMHook (core failed to queue it) is
// left without an answer, which encodeAttempts writes as such.
func (p *Plugin) beginAttempt(ctx *schemas.BifrostContext, state *requestState, attempt int) {
	now := time.Now()
	upstream, _ := schemas.GetUpstreamLatency(ctx)
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.written {
		return
	}
	*state.recordOf(state.chain[attempt]) = attemptRecord{entry: state.chain[attempt], startedAt: now, upstreamAtStart: upstream}
}

// recordOf returns the record of the attempt on the entry at position entry of the order: the
// latest record when it is that entry's, otherwise a new one appended. The first record is kept
// in the request state itself, so a send whose primary answers allocates none. state.mu is held.
func (state *requestState) recordOf(entry int) *attemptRecord {
	if n := len(state.attempts); n > 0 && state.attempts[n-1].entry == entry {
		return &state.attempts[n-1]
	}
	if state.attempts == nil {
		state.attempts = state.firstAttempt[:0]
	}
	state.attempts = append(state.attempts, attemptRecord{entry: entry})
	return &state.attempts[len(state.attempts)-1]
}

// recordAttempt records how the attempt at position attempt of the chain ended, in PostLLMHook,
// and returns the error core decides on. Core moves to the next entry after any error, whatever
// its scope; when the plan's stop_scopes lists the error's scope, the error's AllowFallbacks is
// set to false and Bifrost stops there.
//
// A stream runs PostLLMHook for every chunk, on the provider's goroutine. Its first chunk ends
// the attempt as answered, and core stops switching once a valid chunk has passed; an error
// before that (a stream that opens with events Bifrost holds back, then fails) still ends the
// attempt as failed, and core moves on. Once tk-route-attempts is written nothing is recorded:
// the switches are over, and what a stream does afterwards cannot reach the header.
func (p *Plugin) recordAttempt(ctx *schemas.BifrostContext, state *requestState, attempt int, resp *schemas.BifrostResponse, bifrostErr *schemas.BifrostError) *schemas.BifrostError {
	entry := state.chain[attempt]
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.written {
		return bifrostErr
	}
	n := len(state.attempts)
	if bifrostErr == nil && n > 0 && state.attempts[n-1].entry == entry && state.attempts[n-1].answered {
		// A later chunk of a stream that answered.
		return nil
	}
	now := time.Now()
	upstream, measured := schemas.GetUpstreamLatency(ctx)
	if n == 0 || state.attempts[n-1].entry != entry {
		// A post-hook that no PreLLMHook began: the attempt starts and ends here.
		*state.recordOf(entry) = attemptRecord{entry: entry, startedAt: now, upstreamAtStart: upstream}
	}
	rec := &state.attempts[len(state.attempts)-1]
	rec.ended = true
	rec.duration = now.Sub(rec.startedAt)
	if measured {
		rec.upstreamDuration = upstream - rec.upstreamAtStart
	}
	if bifrostErr == nil {
		rec.answered = true
		rec.status, rec.scope, rec.class, rec.code, rec.message = http.StatusOK, "", "", "", ""
		return nil
	}
	var own bool
	rec.status, rec.scope, rec.class, own = classifyAttempt(bifrostErr, rec.answered)
	rec.code, rec.message = "", ""
	if !own {
		rec.code, rec.message = upstreamError(bifrostErr)
	}
	if rec.scope != "" && slices.Contains(state.plan.GetStopScopes(), rec.scope) {
		bifrostErr.AllowFallbacks = schemas.Ptr(false)
	}
	return bifrostErr
}

// classifyAttempt returns how a failed attempt is recorded: the status of the upstream's
// response, the error scope and the healthobs failure class, and own when Bifrost made the error
// itself (bifrostsOwn), so it carries no upstream error code or message. answered is set when a
// response or a chunk of the attempt passed PostLLMHook before the error: the stream broke after
// it began.
//
// The scope follows the design's statuses, and Bifrost's own failure classification
// (bifrost.ClassifyFailure, which reads the provider's error code, type and message) where the
// status alone does not decide:
//   - key: 401, 403 and 429 (a quota exhausted under a 429 excepted); otherwise a rate limit, a
//     rejected key, a model this key cannot reach, or a location this key's account is refused,
//     which Bifrost binds to the key: the next key of the same upstream may serve.
//   - upstream: 402 and a quota exhausted (the keys of an upstream share its account), 5xx, a
//     connection error, a timeout, a retired model, and a failure inside Bifrost, whose upstream
//     never answered. A status Bifrost's classification does not recognise (a 404 for the path,
//     a 405) is upstream too: it shows neither the key nor the request at fault, and request is
//     claimed only on the upstream's own refusal, since stop_scopes may stop on it.
//   - request: the upstream refused the request itself (Bifrost's caller fault): invalid, over
//     the context length, refused by moderation.
//   - none, with no class: the caller cancelled. The attempt failed through no fault of the key,
//     the upstream or the request, and core stops on it.
//
// The class is transport for an attempt no response came back to (Bifrost's own error),
// stream_broken for one whose stream broke after it began, auth for 401, 403 and a rejected key,
// throttled for 429 and a rate limit, upstream_5xx for 5xx, none for a request-scope error
// (healthobs counts a request the upstream refused as rejected, not as a failure of a class), and
// upstream_4xx for the other 4xx.
func classifyAttempt(err *schemas.BifrostError, answered bool) (status int32, scope, class string, own bool) {
	failure := bifrost.ClassifyFailure(err)
	if answered {
		status = http.StatusOK
	}
	if failure == schemas.FailureClassCancelled {
		return status, "", "", true
	}
	if bifrostsOwn(err, failure) {
		if answered {
			return status, scopeUpstream, classStreamBroken, true
		}
		return 0, scopeUpstream, classTransport, true
	}
	code := 0
	if err.StatusCode != nil {
		code = *err.StatusCode
	}
	if !answered {
		status = int32(code)
	}
	switch {
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		scope = scopeKey
	case code == http.StatusPaymentRequired || failure == schemas.FailureClassQuota:
		scope = scopeUpstream
	case code == http.StatusTooManyRequests:
		scope = scopeKey
	case code >= http.StatusInternalServerError:
		scope = scopeUpstream
	case failure == schemas.FailureClassRateLimit, failure == schemas.FailureClassCredential,
		failure == schemas.FailureClassModelAccess, failure == schemas.FailureClassRegionBlocked:
		scope = scopeKey
	case failure == schemas.FailureClassCallerFault:
		scope = scopeRequest
	default:
		scope = scopeUpstream
	}
	switch {
	case answered:
		class = classStreamBroken
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		class = classAuth
	case code == http.StatusTooManyRequests:
		class = classThrottled
	case code >= http.StatusInternalServerError:
		class = classUpstream5xx
	case scope == scopeRequest:
		class = ""
	case failure == schemas.FailureClassCredential:
		class = classAuth
	case failure == schemas.FailureClassRateLimit:
		class = classThrottled
	default:
		class = classUpstream4xx
	}
	return status, scope, class, false
}

// bifrostsOwn reports whether Bifrost made the error itself, no upstream response carrying it: a
// failure inside Bifrost, a timeout, or a connection to the upstream that failed (Bifrost's 502
// provider_connection_failed, or its network error messages). failure is the error's
// bifrost.ClassifyFailure.
func bifrostsOwn(err *schemas.BifrostError, failure schemas.FailureClass) bool {
	if err.IsBifrostError || failure == schemas.FailureClassTimeout {
		return true
	}
	if err.Error == nil {
		return false
	}
	if err.Error.Type != nil && *err.Error.Type == schemas.ProviderConnectionFailed {
		return true
	}
	return err.Error.Message == schemas.ErrProviderDoRequest || err.Error.Message == schemas.ErrProviderNetworkError
}

// upstreamError returns the error code and message of an attempt the upstream failed, as Bifrost
// received them: the provider's code, or its error type when it gives no code (Anthropic's), and
// its message. Text that is not valid UTF-8 has each invalid run replaced, as a protobuf string
// must be.
func upstreamError(err *schemas.BifrostError) (code, message string) {
	if err.Error != nil {
		message = err.Error.Message
		switch {
		case err.Error.Code != nil && *err.Error.Code != "":
			code = *err.Error.Code
		case err.Error.Type != nil:
			code = *err.Error.Type
		}
	}
	if code == "" && err.Type != nil {
		code = *err.Type
	}
	return strings.ToValidUTF8(code, string(utf8.RuneError)), strings.ToValidUTF8(message, string(utf8.RuneError))
}

// writeAttempts writes the send's tokun.routing.v1.RouteAttempts into tk-route-attempts, as the
// standard base64 of the encoded message, in HTTPTransportResponseHeadersHook. That hook runs
// before the response headers are sent, so a stream carries the header before its first chunk,
// when core has stopped switching. From here on recordAttempt records nothing, so the records
// are read without the lock.
func (p *Plugin) writeAttempts(state *requestState, resp *schemas.HTTPResponseMetadata) error {
	now := time.Now()
	state.mu.Lock()
	state.written = true
	state.mu.Unlock()
	header, err := encodeAttempts(state, now)
	if err != nil {
		return fmt.Errorf("tokun: encode %s: %w", HeaderRouteAttempts, err)
	}
	resp.SetHeader(HeaderRouteAttempts, header)
	return nil
}

// attemptsEncoder is the scratch a send's RouteAttempts is built and encoded in. encoders keeps
// them across requests, so writing the header allocates only the header's string; release
// clears every message it held, so no string of a request outlives it. An encoder keeps the
// capacity of the largest send it encoded until the garbage collector empties the pool.
type attemptsEncoder struct {
	message   routingv1.RouteAttempts
	order     []routingv1.DrawnCandidate
	attempts  []routingv1.RouteAttempt
	stamps    []timestamppb.Timestamp
	durations []durationpb.Duration
	buf       []byte
}

var encoders = sync.Pool{New: func() any { return new(attemptsEncoder) }}

// release clears the encoder's messages and returns it to encoders.
func (enc *attemptsEncoder) release() {
	clear(enc.order)
	clear(enc.attempts)
	clear(enc.stamps)
	clear(enc.durations)
	clear(enc.message.Order)
	clear(enc.message.Attempts)
	enc.message.RequestId, enc.message.SendId = "", ""
	encoders.Put(enc)
}

// resize returns s with length n, keeping its capacity. Its elements are zero: release cleared
// every element an earlier send used.
func resize[T any](s []T, n int) []T {
	return slices.Grow(s[:0], n)[:n]
}

// passedOver counts the entries of the chain core passed over without an attempt: those before
// the last attempt made that have no record. Core skips a fallback whose provider entry it has
// no configuration for, and fails a primary whose provider entry it has no queue for, before
// any hook of the attempt runs. The chain and the records are both in the order of the entries.
func passedOver(state *requestState) int {
	if len(state.attempts) == 0 {
		return 0
	}
	last, made, n := state.attempts[len(state.attempts)-1].entry, state.attempts, 0
	for _, e := range state.chain {
		if e >= last {
			break
		}
		if made[0].entry == e {
			made = made[1:]
			continue
		}
		n++
	}
	return n
}

// encodeAttempts encodes the send's RouteAttempts: request_id, send_id, every entry of the drawn
// order with its shares (DrawnCandidate), and the attempts by their position in the order:
//   - the attempts made, from their records. One begun but never answered by now (its request
//     ended first, or core failed to queue it) is written as one no response came back to:
//     upstream scope, transport class, its duration up to now;
//   - the entries the plugin left out of the chain, with their skip_reason;
//   - the entries of the chain core passed over before a later attempt (passedOver), which have
//     no provider entry on this Bifrost: skip_reason provider_not_synced.
func encodeAttempts(state *requestState, now time.Time) (string, error) {
	enc := encoders.Get().(*attemptsEncoder)
	defer enc.release()
	enc.order = resize(enc.order, len(state.order))
	enc.attempts = resize(enc.attempts, len(state.attempts)+len(state.skipped)+passedOver(state))
	enc.stamps = resize(enc.stamps, len(state.attempts))
	enc.durations = resize(enc.durations, 2*len(state.attempts))
	order, attempts, stamps, durations := enc.order, enc.attempts, enc.stamps, enc.durations
	message := &enc.message
	message.RequestId, message.SendId = state.plan.GetRequestId(), state.claims.GetSendId()
	message.Order = resize(message.Order, len(order))
	message.Attempts = resize(message.Attempts, len(attempts))
	for i, e := range state.order {
		c := &order[i]
		c.ProviderId, c.UpstreamId, c.Interface, c.KeyId = e.provider.GetProviderId(), e.upstream.GetUpstreamId(), e.upstream.GetInterface(), e.key.GetKeyId()
		c.ProviderShare, c.UpstreamShare, c.KeyShare = e.providerShare, e.upstreamShare, e.keyShare
		c.BelowThreshold, c.SameFormat = e.belowThreshold, e.upstream.GetSameFormat()
		message.Order[i] = c
	}

	// One walk over the order, with the skipped entries, the records and the chain each consumed
	// as the walk reaches their entries.
	made, skipped, chain := state.attempts, state.skipped, state.chain
	last := -1
	if len(made) > 0 {
		last = made[len(made)-1].entry
	}
	written, recorded := 0, 0
	for entry, e := range state.order {
		inChain := len(chain) > 0 && chain[0] == entry
		if inChain {
			chain = chain[1:]
		}
		var a *routingv1.RouteAttempt
		switch {
		case len(skipped) > 0 && skipped[0].index == entry:
			a = &attempts[written]
			a.SkipReason = skipped[0].reason
			skipped = skipped[1:]
		case len(made) > 0 && made[0].entry == entry:
			a = &attempts[written]
			rec := &made[0]
			made = made[1:]
			status, scope, class, code, msg, duration := rec.status, rec.scope, rec.class, rec.code, rec.message, rec.duration
			if !rec.ended {
				status, scope, class, code, msg, duration = 0, scopeUpstream, classTransport, "", "", now.Sub(rec.startedAt)
			}
			a.StatusCode, a.ErrorScope, a.ErrorClass, a.UpstreamErrorCode, a.UpstreamErrorMessage = status, scope, class, code, msg
			n := recorded
			recorded++
			stamps[n].Seconds, stamps[n].Nanos = rec.startedAt.Unix(), int32(rec.startedAt.Nanosecond())
			durations[2*n].Seconds, durations[2*n].Nanos = splitDuration(duration)
			durations[2*n+1].Seconds, durations[2*n+1].Nanos = splitDuration(rec.upstreamDuration)
			a.StartedAt, a.Duration, a.UpstreamDuration = &stamps[n], &durations[2*n], &durations[2*n+1]
		case inChain && entry < last:
			a = &attempts[written]
			a.SkipReason = skipProviderNotSynced
		default:
			continue
		}
		message.Attempts[written] = a
		written++
		a.Index = int32(entry)
		a.ProviderId, a.UpstreamId, a.Interface, a.KeyId = e.provider.GetProviderId(), e.upstream.GetUpstreamId(), e.upstream.GetInterface(), e.key.GetKeyId()
		a.SameFormat = e.upstream.GetSameFormat()
	}

	// The encoded bytes and their base64 share the encoder's buffer.
	options := proto.MarshalOptions{UseCachedSize: true}
	size := options.Size(message)
	raw, err := options.MarshalAppend(slices.Grow(enc.buf[:0], size+base64.StdEncoding.EncodedLen(size)), message)
	if err != nil {
		return "", err
	}
	encoded := base64.StdEncoding.AppendEncode(raw[len(raw):], raw)
	enc.buf = raw
	return string(encoded), nil
}

// splitDuration is d as a google.protobuf.Duration's seconds and nanoseconds.
func splitDuration(d time.Duration) (int64, int32) {
	return int64(d / time.Second), int32(d % time.Second)
}

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
