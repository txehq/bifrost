// Package tokun is the tokun routing plugin. The tokun gateway sends each request with a
// signed route plan (tokun.routing.v1.RoutePlan in tk-route-plan, its RoutePlanSignature in
// tk-route-plan-sig). The plugin verifies the plan before authentication and answers 401 when
// it does not hold, so the request never enters Bifrost core; it then dispatches the request
// only to the upstreams and keys the plan lists, primary first and the rest as Bifrost
// fallbacks, each attempt with its own key pinned.
package tokun

import (
	"net/http"
	"strings"
	"time"

	"github.com/capsohq/bifrost/core/schemas"
)

// PluginName is the plugin's name in Bifrost's plugin configuration.
const PluginName = "tokun"

// HeaderRouteAttempts is the response header that carries the send's
// tokun.routing.v1.RouteAttempts back to the gateway.
const HeaderRouteAttempts = "tk-route-attempts"

// callerRoutingHeaders are the request headers through which a caller would pick a key or let
// its session reorder the route. A verified plan decides both, so they are removed with the
// plan headers.
var callerRoutingHeaders = []string{"x-bf-api-key-id", "x-bf-api-key", "x-bf-direct-key", "x-bf-session-affinity"}

// Plugin is the tokun routing plugin. It implements schemas.HTTPTransportPlugin,
// schemas.LLMPlugin and schemas.ConfigMarshallerPlugin.
type Plugin struct {
	keys    map[string][]byte
	sendIDs *sendIDLedger
	logger  schemas.Logger
}

// Init builds the plugin from its configuration. It fails when no signing key resolves.
func Init(config *Config, logger schemas.Logger) (*Plugin, error) {
	keys, err := config.signingKeys()
	if err != nil {
		return nil, err
	}
	return &Plugin{keys: keys, sendIDs: newSendIDLedger(), logger: logger}, nil
}

// GetName implements schemas.BasePlugin.
func (p *Plugin) GetName() string { return PluginName }

// Cleanup implements schemas.BasePlugin.
func (p *Plugin) Cleanup() error { return nil }

// HTTPTransportPreAuthHook verifies the route plan before authentication. A request whose
// plan fails any check is answered 401 here and never reaches authentication or Bifrost
// core. A verified plan and its signature are removed from the request with the caller's own
// key and session routing headers; the send_id becomes the id Bifrost processes the request
// under, and session affinity is off, so the chain keeps the plan's order.
func (p *Plugin) HTTPTransportPreAuthHook(ctx *schemas.BifrostContext, req *schemas.HTTPRequest) (*schemas.HTTPResponse, error) {
	verified, refused := p.verify(
		req.CaseInsensitiveHeaderLookup(HeaderRoutePlan),
		req.CaseInsensitiveHeaderLookup(HeaderRoutePlanSignature),
		len(req.Body),
		time.Now(),
	)
	if refused != nil {
		p.logger.Warn("tokun: refused a route plan on %s %s: %s: %s", req.Method, req.Path, refused.reason, refused.detail)
		return &schemas.HTTPResponse{
			StatusCode: http.StatusUnauthorized,
			Headers:    map[string]string{"Content-Type": "application/json"},
			Body:       refused.body(),
		}, nil
	}
	deleteHeader(req, HeaderRoutePlan)
	deleteHeader(req, HeaderRoutePlanSignature)
	for _, name := range callerRoutingHeaders {
		deleteHeader(req, name)
	}
	ctx.SetValue(requestStateKey, &requestState{plan: verified.plan, claims: verified.claims})
	ctx.SetValue(schemas.BifrostContextKeyRequestID, verified.claims.GetSendId())
	ctx.SetValue(schemas.BifrostContextKeySessionAffinity, false)
	return nil, nil
}

// deleteHeader removes every spelling of the header name from the request.
func deleteHeader(req *schemas.HTTPRequest, name string) {
	for key := range req.Headers {
		if strings.EqualFold(key, name) {
			delete(req.Headers, key)
		}
	}
}

// HTTPTransportPreHook has nothing to do: the plan was verified and removed before
// authentication.
func (p *Plugin) HTTPTransportPreHook(_ *schemas.BifrostContext, _ *schemas.HTTPRequest) (*schemas.HTTPResponse, error) {
	return nil, nil
}

// HTTPTransportPostHook has nothing to do: the attempt record is a response header, written in
// HTTPTransportResponseHeadersHook, which also runs before a stream's headers are sent.
func (p *Plugin) HTTPTransportPostHook(_ *schemas.BifrostContext, _ *schemas.HTTPRequest, _ *schemas.HTTPResponse) error {
	return nil
}

// HTTPTransportResponseHeadersHook writes the send's attempt record into the response
// headers, once every switch between attempts is over.
func (p *Plugin) HTTPTransportResponseHeadersHook(ctx *schemas.BifrostContext, _ *schemas.HTTPRequest, resp *schemas.HTTPResponseMetadata) error {
	state, ok := ctx.Value(requestStateKey).(*requestState)
	if !ok {
		return nil
	}
	p.writeAttempts(state, resp)
	return nil
}

// HTTPTransportStreamChunkHook passes each chunk to the caller.
func (p *Plugin) HTTPTransportStreamChunkHook(ctx *schemas.BifrostContext, _ *schemas.HTTPRequest, chunk *schemas.BifrostStreamChunk) (*schemas.BifrostStreamChunk, error) {
	state, ok := ctx.Value(requestStateKey).(*requestState)
	if !ok {
		return chunk, nil
	}
	return p.filterChunk(state, chunk), nil
}

// PreRequestHook dispatches a request that carries a verified plan inside that plan. A
// request without one (one Bifrost makes for itself) is left as it is: every request from a
// caller has been verified before authentication.
func (p *Plugin) PreRequestHook(ctx *schemas.BifrostContext, req *schemas.BifrostRequest) error {
	state, ok := ctx.Value(requestStateKey).(*requestState)
	if !ok {
		return nil
	}
	p.route(ctx, req, state)
	return nil
}

// PreLLMHook runs before each attempt. It fails a send none of whose plan entries is on this
// Bifrost, and otherwise rewrites the request for the attempt's entry.
func (p *Plugin) PreLLMHook(ctx *schemas.BifrostContext, req *schemas.BifrostRequest) (*schemas.BifrostRequest, *schemas.LLMPluginShortCircuit, error) {
	state, ok := ctx.Value(requestStateKey).(*requestState)
	if !ok {
		return req, nil, nil
	}
	if len(state.chain) == 0 {
		return req, &schemas.LLMPluginShortCircuit{Error: unroutableError()}, nil
	}
	attempt, _ := ctx.Value(schemas.BifrostContextKeyFallbackIndex).(int)
	return p.rewriteAttempt(ctx, req, state.order[state.chain[attempt]]), nil, nil
}

// PostLLMHook runs after each attempt and records it.
func (p *Plugin) PostLLMHook(ctx *schemas.BifrostContext, resp *schemas.BifrostResponse, bifrostErr *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError, error) {
	state, ok := ctx.Value(requestStateKey).(*requestState)
	if !ok || len(state.chain) == 0 {
		return resp, bifrostErr, nil
	}
	attempt, _ := ctx.Value(schemas.BifrostContextKeyFallbackIndex).(int)
	return resp, p.recordAttempt(ctx, state, attempt, resp, bifrostErr), nil
}

// unroutableError fails a send whose plan has no entry this Bifrost can send. Nothing is
// tried, so there is nothing to fall back to.
func unroutableError() *schemas.BifrostError {
	status := http.StatusServiceUnavailable
	errorType := "tokun_route_plan_unroutable"
	allowFallbacks := false
	return &schemas.BifrostError{
		IsBifrostError: true,
		StatusCode:     &status,
		Type:           &errorType,
		AllowFallbacks: &allowFallbacks,
		Error: &schemas.ErrorField{
			Type:    &errorType,
			Message: "no provider entry or key of the route plan is on this Bifrost",
		},
	}
}
