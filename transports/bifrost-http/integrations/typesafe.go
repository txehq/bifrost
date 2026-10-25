package integrations

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/bytedance/sonic"
	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/providers/typesafe"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/capsohq/bifrost/transports/bifrost-http/lib"
	"github.com/valyala/fasthttp"
)

// TypesafeRouter holds route registrations for Typesafe endpoints: the native
// System One evaluation endpoint and model listing.
type TypesafeRouter struct {
	*GenericRouter
}

// NewTypesafeRouter creates a new TypesafeRouter with the given bifrost client.
func NewTypesafeRouter(client *bifrost.Bifrost, handlerStore lib.HandlerStore, accessResolver AccessResolver, logger schemas.Logger) *TypesafeRouter {
	return &TypesafeRouter{
		GenericRouter: NewGenericRouter(client, handlerStore, accessResolver, CreateTypesafeRouteConfigs("/typesafe"), nil, logger),
	}
}

// CreateTypesafeRouteConfigs creates route configurations for Typesafe API endpoints.
func CreateTypesafeRouteConfigs(pathPrefix string) []RouteConfig {
	var routes []RouteConfig

	// Decision endpoint (v1/systemone)
	routes = append(routes, RouteConfig{
		Type:   RouteConfigTypeTypesafe,
		Path:   pathPrefix + "/v1/systemone",
		Method: "POST",
		GetHTTPRequestType: func(ctx *fasthttp.RequestCtx) schemas.RequestType {
			return schemas.DecisionRequest
		},
		GetRequestTypeInstance: func(ctx context.Context) interface{} {
			return &typesafe.TypesafeDecisionRequest{}
		},
		RequestConverter: func(ctx *schemas.BifrostContext, req interface{}) (*schemas.BifrostRequest, error) {
			if typesafeReq, ok := req.(*typesafe.TypesafeDecisionRequest); ok {
				decisionReq, err := typesafeReq.ToBifrostDecisionRequest(ctx)
				if err != nil {
					return nil, err
				}
				// The native surface is the wire shape: extensions the SDK
				// forwarded always reach the provider, no header needed.
				if len(decisionReq.ExtraParams) > 0 && ctx != nil {
					ctx.SetValue(schemas.BifrostContextKeyPassthroughExtraParams, true)
				}
				return &schemas.BifrostRequest{
					DecisionRequest: decisionReq,
				}, nil
			}
			return nil, errors.New("invalid request type")
		},
		DecisionResponseConverter: func(ctx *schemas.BifrostContext, resp *schemas.BifrostDecisionResponse) (interface{}, error) {
			native, err := typesafe.ToTypesafeNativeDecisionResponse(resp)
			if err != nil {
				return nil, err
			}
			// The raw and native provider bodies were kept before post-hooks ran; relay one
			// only while it still carries the answers the processed response does, so a
			// post-hook's change reaches the client.
			if resp.ExtraFields.Provider == schemas.Typesafe {
				if resp.ExtraFields.RawResponse != nil && typesafeRawAnswersMatch(resp.ExtraFields.RawResponse, native) {
					return resp.ExtraFields.RawResponse, nil
				}
				if len(resp.NativeResponse) > 0 && typesafeRawAnswersMatch(resp.NativeResponse, native) {
					return resp.NativeResponse, nil
				}
			}
			return native, nil
		},
		ErrorConverter: func(ctx *schemas.BifrostContext, err *schemas.BifrostError) interface{} {
			return typesafe.ToTypesafeNativeErrorBody(err)
		},
	})

	// Models endpoint: the endpoint's native GET /v1/models catalog in native
	// shape (the pinned jev catalog backs the default endpoint).
	routes = append(routes, RouteConfig{
		Type:   RouteConfigTypeTypesafe,
		Path:   pathPrefix + "/v1/models",
		Method: "GET",
		GetHTTPRequestType: func(ctx *fasthttp.RequestCtx) schemas.RequestType {
			return schemas.ListModelsRequest
		},
		GetRequestTypeInstance: func(ctx context.Context) interface{} {
			return &schemas.BifrostListModelsRequest{}
		},
		RequestConverter: func(ctx *schemas.BifrostContext, req interface{}) (*schemas.BifrostRequest, error) {
			if listModelsReq, ok := req.(*schemas.BifrostListModelsRequest); ok {
				if listModelsReq.Provider == "" {
					listModelsReq.Provider = schemas.Typesafe
				}
				return &schemas.BifrostRequest{
					ListModelsRequest: listModelsReq,
				}, nil
			}
			return nil, errors.New("invalid request type")
		},
		ListModelsResponseConverter: func(ctx *schemas.BifrostContext, resp *schemas.BifrostListModelsResponse) (interface{}, error) {
			return typesafe.ToTypesafeNativeListModelsResponse(resp), nil
		},
		ErrorConverter: func(ctx *schemas.BifrostContext, err *schemas.BifrostError) interface{} {
			return typesafe.ToTypesafeNativeErrorBody(err)
		},
	})

	return routes
}

// typesafeRawAnswersMatch reports whether a raw systemone body carries the same answers
// as native, compared in their serialized form. A body that cannot be read as a systemone
// response does not match.
func typesafeRawAnswersMatch(raw interface{}, native *typesafe.TypesafeDecisionResponse) bool {
	var body []byte
	switch v := raw.(type) {
	case json.RawMessage:
		body = v
	case []byte:
		body = v
	case string:
		body = []byte(v)
	default:
		marshaled, err := sonic.Marshal(v)
		if err != nil {
			return false
		}
		body = marshaled
	}
	var parsed typesafe.TypesafeDecisionResponse
	if err := sonic.Unmarshal(body, &parsed); err != nil {
		return false
	}
	got, err1 := sonic.ConfigStd.Marshal(parsed.Answers)
	want, err2 := sonic.ConfigStd.Marshal(native.Answers)
	return err1 == nil && err2 == nil && bytes.Equal(got, want)
}
