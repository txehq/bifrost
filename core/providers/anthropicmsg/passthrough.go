package anthropicmsg

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

const DefaultAPIVersion = "2023-06-01"

// IsMessagesPath reports whether path targets the native Messages create endpoint.
func IsMessagesPath(path string) bool {
	if idx := strings.IndexByte(path, '?'); idx >= 0 {
		path = path[:idx]
	}
	return strings.TrimRight(path, "/") == "/v1/messages"
}

// Config supplies provider-owned transport settings to the shared Messages client.
type Config struct {
	Provider             schemas.ModelProvider
	Endpoint             string
	APIVersion           string
	APIKeyHeader         string
	APIKeyPrefix         string
	NetworkConfig        schemas.NetworkConfig
	CustomProviderConfig *schemas.CustomProviderConfig
	Logger               schemas.Logger
}

// requestURL joins the passthrough path onto the provider endpoint, rejecting
// paths that would leave the provider host.
func (c Config) requestURL(req *schemas.BifrostPassthroughRequest) (string, error) {
	return providerUtils.BuildPassthroughURL(c.Endpoint, req.Path, req.RawQuery)
}

func (c Config) setHeaders(ctx *schemas.BifrostContext, req *fasthttp.Request, requestURL string, key schemas.Key, safeHeaders map[string]string) {
	providerUtils.SetExtraHeaders(ctx, req, c.NetworkConfig.ExtraHeaders, nil)
	providerUtils.StripCallerAuthForInsecureURL(requestURL, safeHeaders)
	for name, value := range safeHeaders {
		req.Header.Set(name, value)
	}
	if len(req.Header.ContentType()) == 0 {
		req.Header.SetContentType("application/json")
	}
	if value := key.Value.GetValue(); value != "" {
		header := c.APIKeyHeader
		if header == "" {
			header = "x-api-key"
		}
		req.Header.Set(header, c.APIKeyPrefix+value)
	}
	version := c.APIVersion
	if version == "" {
		version = DefaultAPIVersion
	}
	if len(req.Header.Peek("anthropic-version")) == 0 {
		req.Header.Set("anthropic-version", version)
	}
}

// Passthrough posts a buffered native Anthropic request and preserves the response body.
func Passthrough(ctx *schemas.BifrostContext, client *fasthttp.Client, key schemas.Key, req *schemas.BifrostPassthroughRequest, cfg Config) (*schemas.BifrostPassthroughResponse, *schemas.BifrostError) {
	if err := providerUtils.CheckOperationAllowed(cfg.Provider, cfg.CustomProviderConfig, schemas.PassthroughRequest); err != nil {
		return nil, err
	}
	requestURL, urlErr := cfg.requestURL(req)
	if urlErr != nil {
		return nil, providerUtils.NewBifrostBadRequestError(urlErr.Error())
	}

	upstreamReq := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(resp)
	defer fasthttp.ReleaseRequest(upstreamReq)

	upstreamReq.Header.SetMethod(req.Method)
	upstreamReq.SetRequestURI(requestURL)
	cfg.setHeaders(ctx, upstreamReq, requestURL, key, req.SafeHeaders)
	if !providerUtils.ApplyLargePayloadRequestBodyWithModelNormalization(ctx, upstreamReq, cfg.Provider) {
		upstreamReq.SetBody(req.Body)
	}

	latency, bifrostErr, wait := providerUtils.MakeRequestWithContext(ctx, client, upstreamReq, resp)
	defer wait()
	if bifrostErr != nil {
		return nil, bifrostErr
	}

	headers := providerUtils.ExtractPassthroughProviderResponseHeaders(resp)
	ctx.SetValue(schemas.BifrostContextKeyProviderResponseHeaders, headers)
	body, err := providerUtils.CheckAndDecodeBody(resp)
	if err != nil {
		return nil, providerUtils.NewBifrostOperationError("failed to decode response body", err)
	}

	var usage *schemas.BifrostPassthroughUsage
	if resp.StatusCode() >= 200 && resp.StatusCode() < 300 {
		usage = ExtractUsage(req.Path, req.Body, body)
	}
	return &schemas.BifrostPassthroughResponse{
		StatusCode: resp.StatusCode(), Headers: headers, Body: body, PassthroughUsage: usage,
		ExtraFields: schemas.BifrostResponseExtraFields{
			Latency: latency.Milliseconds(), ProviderResponseHeaders: headers, PassthroughPath: req.Path,
		},
	}, nil
}

// PassthroughStream posts a native Anthropic streaming request and relays every
// upstream byte unchanged while observing framed SSE usage events.
func PassthroughStream(ctx *schemas.BifrostContext, postHookRunner schemas.PostHookRunner, postHookSpanFinalizer func(context.Context), streamingClient *fasthttp.Client, key schemas.Key, req *schemas.BifrostPassthroughRequest, cfg Config) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	if err := providerUtils.CheckOperationAllowed(cfg.Provider, cfg.CustomProviderConfig, schemas.PassthroughStreamRequest); err != nil {
		return nil, err
	}
	requestURL, urlErr := cfg.requestURL(req)
	if urlErr != nil {
		return nil, providerUtils.NewBifrostBadRequestError(urlErr.Error())
	}

	startTime := time.Now()
	upstreamReq := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	resp.StreamBody = true
	defer fasthttp.ReleaseRequest(upstreamReq)

	upstreamReq.Header.SetMethod(req.Method)
	upstreamReq.SetRequestURI(requestURL)
	cfg.setHeaders(ctx, upstreamReq, requestURL, key, req.SafeHeaders)
	upstreamReq.Header.Set("Connection", "close")
	if !providerUtils.ApplyLargePayloadRequestBodyWithModelNormalization(ctx, upstreamReq, cfg.Provider) {
		upstreamReq.SetBody(req.Body)
	}

	activeClient := providerUtils.PrepareResponseStreaming(ctx, streamingClient, resp)
	err := providerUtils.DoStreamingRequest(ctx, activeClient, upstreamReq, resp)
	latency := time.Since(startTime)
	if err != nil {
		providerUtils.ReleaseStreamingResponse(ctx, resp)
		if errors.Is(err, context.Canceled) {
			return nil, providerUtils.SetErrorLatency(&schemas.BifrostError{
				IsBifrostError: false,
				Error:          &schemas.ErrorField{Type: schemas.Ptr(schemas.RequestCancelled), Message: schemas.ErrRequestCancelled, Error: err},
			}, latency)
		}
		if errors.Is(err, fasthttp.ErrTimeout) || errors.Is(err, context.DeadlineExceeded) {
			return nil, providerUtils.SetErrorLatency(providerUtils.NewBifrostTimeoutError(schemas.ErrProviderRequestTimedOut, err), latency)
		}
		return nil, providerUtils.SetErrorLatency(providerUtils.NewBifrostUpstreamConnectionError(schemas.ErrProviderDoRequest, err), latency)
	}

	headers := providerUtils.ExtractPassthroughProviderResponseHeaders(resp)
	ctx.SetValue(schemas.BifrostContextKeyProviderResponseHeaders, headers)
	bodyStream := resp.BodyStream()
	if bodyStream == nil {
		providerUtils.ReleaseStreamingResponse(ctx, resp)
		return nil, providerUtils.NewBifrostOperationError("provider returned an empty stream body", fmt.Errorf("provider returned an empty stream body"))
	}

	providerUtils.SetStreamIdleTimeoutIfEmpty(ctx, cfg.NetworkConfig.StreamIdleTimeoutInSeconds)
	var observer func([]byte) *schemas.BifrostPassthroughUsage
	if ClassifyPath(req.Path) == schemas.ResponsesRequest {
		usage := &StreamUsage{}
		observer = usage.ObserveEvent
	} else {
		observer = func(event []byte) *schemas.BifrostPassthroughUsage {
			return ExtractUsage(req.Path, req.Body, event)
		}
	}

	return providerUtils.StreamPassthrough(ctx, postHookRunner, postHookSpanFinalizer, resp, io.Reader(bodyStream), providerUtils.PassthroughStreamParams{
		StatusCode: resp.StatusCode(), Headers: headers, Path: req.Path,
		RawRequest: req.Body, CancellationBody: req.Body, StartTime: startTime,
		Logger: cfg.Logger, HasUsage: HasUsage, Observe: observer,
	}), nil
}
