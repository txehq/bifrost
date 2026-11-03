// Package tokun is the tokun routing plugin. The tokun gateway sends each request with a
// signed route plan (tokun.routing.v1.RoutePlan in tk-route-plan, its RoutePlanSignature in
// tk-route-plan-sig). Before authentication the plugin refuses the routes tokun does not offer
// (routes.go) and verifies the plan, answering 401 when it does not hold, so a refused request
// never enters Bifrost core; it then dispatches the request only to the upstreams and keys the
// plan lists, primary first and the rest as Bifrost fallbacks, each attempt with its own key
// pinned.
package tokun

import (
	"fmt"
	"math/rand/v2"
	"net/http"
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
	// uniform is the source of the uniform numbers in [0, 1) the selection curve draws with:
	// math/rand/v2's global source, so every request draws its own numbers (SeedDraws).
	uniform func() float64
	logger  schemas.Logger
}

// Init builds the plugin from its configuration. It fails when no signing key resolves.
func Init(config *Config, logger schemas.Logger) (*Plugin, error) {
	keys, err := config.signingKeys()
	if err != nil {
		return nil, err
	}
	return &Plugin{keys: keys, sendIDs: newSendIDLedger(), uniform: rand.Float64, logger: logger}, nil
}

// GetName implements schemas.BasePlugin.
func (p *Plugin) GetName() string { return PluginName }

// Cleanup implements schemas.BasePlugin.
func (p *Plugin) Cleanup() error { return nil }

// HTTPTransportPreAuthHook refuses the routes tokun does not offer, then verifies the route plan,
// all before authentication; a refused request never reaches authentication or Bifrost core.
// The route decides first (routeClasses): a route that is not one of tokun's endpoint types is
// answered 404, the realtime conversation 501, and a route the plugin does not know 500. On the
// other routes a request whose plan fails any check is answered 401. A verified plan and its
// signature are removed from the request with the caller's own key and session routing headers;
// the send_id becomes the id Bifrost processes the request under, and session affinity is off,
// so the chain keeps the plan's order.
func (p *Plugin) HTTPTransportPreAuthHook(ctx *schemas.BifrostContext, req *schemas.HTTPRequest) (*schemas.HTTPResponse, error) {
	switch class := classifyRoute(req.Method, req.Route); class {
	case routePlan:
	case routeNotOffered:
		// These routes are not tokun's endpoint types (the WebSocket Responses API, live
		// sessions, MCP, the Agent Gateway): tokun does not offer them, so no plan opens them.
		return p.refuse(req, http.StatusNotFound, routeRejectionType, &refusal{rejectNotOffered,
			fmt.Sprintf("%s %s is not one of tokun's endpoint types", req.Method, req.Route)}), nil
	case routeNotImplemented:
		return p.refuse(req, http.StatusNotImplemented, routeRejectionType, &refusal{rejectNotImplemented,
			fmt.Sprintf("%s %s, the realtime conversation, is not implemented", req.Method, req.Route)}), nil
	case routeUnclassified:
		// A route the table does not list, or a request without a route template: closed until
		// routes.go classifies it.
		return p.refuse(req, http.StatusInternalServerError, routeRejectionType, &refusal{rejectUnclassified,
			fmt.Sprintf("%s %s (route %q) has no tokun route class", req.Method, req.Path, req.Route)}), nil
	default:
		panic(fmt.Sprintf("tokun: route class %d has no answer", class))
	}
	verified, refused := p.verify(
		req.CaseInsensitiveHeaderLookup(HeaderRoutePlan),
		req.CaseInsensitiveHeaderLookup(HeaderRoutePlanSignature),
		len(req.Body),
		time.Now(),
	)
	if refused != nil {
		return p.refuse(req, http.StatusUnauthorized, rejectionType, refused), nil
	}
	req.DeleteHeader(HeaderRoutePlan)
	req.DeleteHeader(HeaderRoutePlanSignature)
	for _, name := range callerRoutingHeaders {
		req.DeleteHeader(name)
	}
	ctx.SetValue(requestStateKey, &requestState{plan: verified.plan, claims: verified.claims})
	ctx.SetValue(schemas.BifrostContextKeyRequestID, verified.claims.GetSendId())
	ctx.SetValue(schemas.BifrostContextKeySessionAffinity, false)
	return nil, nil
}

// refuse answers a refused request with the error body {"error": {"type", "code", "message"}}.
func (p *Plugin) refuse(req *schemas.HTTPRequest, status int, errorType string, refused *refusal) *schemas.HTTPResponse {
	p.logger.Warn("tokun: refused %s %s with %d: %s: %s", req.Method, req.Path, status, refused.reason, refused.detail)
	return &schemas.HTTPResponse{
		StatusCode: status,
		Headers:    map[string]string{"Content-Type": "application/json"},
		Body:       refused.body(errorType),
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

// PreLLMHook runs before each attempt. It fails a send whose plan was never applied to the
// request or none of whose entries is on this Bifrost, and an attempt whose index is not one of
// the entries sent; otherwise it rewrites the request for the attempt's entry.
func (p *Plugin) PreLLMHook(ctx *schemas.BifrostContext, req *schemas.BifrostRequest) (*schemas.BifrostRequest, *schemas.LLMPluginShortCircuit, error) {
	state, ok := ctx.Value(requestStateKey).(*requestState)
	if !ok {
		return req, nil, nil
	}
	if state.order == nil {
		return req, &schemas.LLMPluginShortCircuit{Error: notAppliedError()}, nil
	}
	if len(state.chain) == 0 {
		return req, &schemas.LLMPluginShortCircuit{Error: unroutableError()}, nil
	}
	attempt, fault := attemptIndex(ctx, state)
	if fault != nil {
		return req, &schemas.LLMPluginShortCircuit{Error: fault}, nil
	}
	return p.rewriteAttempt(ctx, req, state.order[state.chain[attempt]]), nil, nil
}

// PostLLMHook runs after each attempt and records it. A send PreLLMHook failed before any
// attempt passes through as it is.
func (p *Plugin) PostLLMHook(ctx *schemas.BifrostContext, resp *schemas.BifrostResponse, bifrostErr *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError, error) {
	state, ok := ctx.Value(requestStateKey).(*requestState)
	if !ok || len(state.chain) == 0 {
		return resp, bifrostErr, nil
	}
	attempt, fault := attemptIndex(ctx, state)
	if fault != nil {
		return nil, fault, nil
	}
	return resp, p.recordAttempt(ctx, state, attempt, resp, bifrostErr), nil
}

// attemptIndex returns the position in state.chain of the attempt the hook runs for.
//
// Core sets BifrostContextKeyFallbackIndex before every attempt that handleRequest,
// handleStreamRequest and continueOnServedRoute make: 0 for the primary, i+1 for fallback i.
// The WebSocket Responses, realtime and live paths run the hooks through RunStreamPreHooks and
// RunRealtimeTurnPreHooks, which never set it; each of those makes one attempt, the primary. So
// an absent index is the primary. An index of another type, or one outside the entries sent,
// fails the attempt.
func attemptIndex(ctx *schemas.BifrostContext, state *requestState) (int, *schemas.BifrostError) {
	attempt := 0
	if value := ctx.Value(schemas.BifrostContextKeyFallbackIndex); value != nil {
		index, ok := value.(int)
		if !ok {
			return 0, attemptFault(fmt.Sprintf("the attempt index is a %T, not an int", value))
		}
		attempt = index
	}
	if attempt < 0 || attempt >= len(state.chain) {
		return 0, attemptFault(fmt.Sprintf("attempt %d is not one of the %d plan entries sent", attempt, len(state.chain)))
	}
	return attempt, nil
}

// unroutableError fails a send whose plan has no entry this Bifrost can send. Nothing is
// tried, so there is nothing to fall back to.
func unroutableError() *schemas.BifrostError {
	return sendError(http.StatusServiceUnavailable, "tokun_route_plan_unroutable",
		"no provider entry or key of the route plan is on this Bifrost")
}

// notAppliedError fails a send that reached an attempt without PreRequestHook applying its plan:
// a route that runs the LLM hooks without the pre-request phase dispatches by its own choice,
// so the send could leave the plan.
func notAppliedError() *schemas.BifrostError {
	return sendError(http.StatusNotImplemented, "tokun_route_plan_not_applied",
		"this route dispatches without applying the route plan")
}

// attemptFault fails an attempt core numbered outside the plan entries the plugin sent.
func attemptFault(detail string) *schemas.BifrostError {
	return sendError(http.StatusInternalServerError, "tokun_attempt_index_invalid", detail)
}

// sendError is a failure of the whole send: nothing falls back from it.
func sendError(status int, errorType, message string) *schemas.BifrostError {
	allowFallbacks := false
	return &schemas.BifrostError{
		IsBifrostError: true,
		StatusCode:     &status,
		Type:           &errorType,
		AllowFallbacks: &allowFallbacks,
		Error: &schemas.ErrorField{
			Type:    &errorType,
			Message: message,
		},
	}
}
