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
	"github.com/tidwall/gjson"
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
	// StripAnthropicOnlyFields is set by providers that are not Anthropic but serve
	// its Messages API (xAI): the body loses the Claude Code fields only Anthropic
	// accepts, as upstream strips them on every converted non-Anthropic request.
	StripAnthropicOnlyFields bool
}

// requestURL joins the endpoint with the caller's passthrough path and query, and
// refuses one that would leave the endpoint's host (see BuildPassthroughURL).
func (c Config) requestURL(req *schemas.BifrostPassthroughRequest) (string, *schemas.BifrostError) {
	url, err := providerUtils.BuildPassthroughURL(c.Endpoint, req.Path, req.RawQuery)
	if err != nil {
		return "", providerUtils.NewBifrostBadRequestError(err.Error())
	}
	return url, nil
}

func (c Config) requestBody(req *schemas.BifrostPassthroughRequest) ([]byte, *schemas.BifrostError) {
	if !c.StripAnthropicOnlyFields || !IsMessagesPath(req.Path) {
		return req.Body, nil
	}
	body, err := StripAnthropicOnlyFields(req.Body)
	if err != nil {
		return nil, providerUtils.NewBifrostOperationError("failed to prepare native Messages request body", err)
	}
	return body, nil
}

// StripAnthropicOnlyFields removes from a native Messages body what only Anthropic
// should receive: Claude Code's undocumented `safeguards` and `diagnostics` fields,
// and its per-request billing metadata line in `system`. The billing line changes on
// every request, so left in place it would defeat the provider's prompt cache
// (upstream #7393, #7243, #7431). A body without them is returned unchanged.
func StripAnthropicOnlyFields(body []byte) ([]byte, error) {
	var err error
	if providerUtils.JSONFieldExists(body, "safeguards") {
		if body, err = providerUtils.DeleteJSONField(body, "safeguards"); err != nil {
			return nil, fmt.Errorf("strip safeguards: %w", err)
		}
	}
	if providerUtils.JSONFieldExists(body, "diagnostics") {
		if body, err = providerUtils.DeleteJSONField(body, "diagnostics"); err != nil {
			return nil, fmt.Errorf("strip diagnostics: %w", err)
		}
	}
	system := providerUtils.GetJSONField(body, "system")
	switch {
	case system.Type == gjson.String:
		if schemas.IsStandaloneAnthropicBillingHeader(system.Str) {
			if body, err = providerUtils.DeleteJSONField(body, "system"); err != nil {
				return nil, fmt.Errorf("strip billing header: %w", err)
			}
		}
	case system.IsArray():
		blocks := system.Array()
		kept := make([]string, 0, len(blocks))
		for _, block := range blocks {
			text := block.Get("text")
			if block.Get("type").Str == "text" && text.Type == gjson.String && schemas.IsStandaloneAnthropicBillingHeader(text.Str) {
				continue
			}
			kept = append(kept, block.Raw)
		}
		if len(kept) == len(blocks) {
			return body, nil
		}
		if len(kept) == 0 {
			body, err = providerUtils.DeleteJSONField(body, "system")
		} else {
			body, err = providerUtils.SetRawJSONField(body, "system", []byte("["+strings.Join(kept, ",")+"]"))
		}
		if err != nil {
			return nil, fmt.Errorf("strip billing header: %w", err)
		}
	}
	return body, nil
}

func (c Config) setHeaders(ctx *schemas.BifrostContext, req *fasthttp.Request, key schemas.Key, safeHeaders map[string]string) {
	providerUtils.SetExtraHeaders(ctx, req, c.NetworkConfig.ExtraHeaders, nil)
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
	requestURL, bifrostErr := cfg.requestURL(req)
	if bifrostErr != nil {
		return nil, bifrostErr
	}

	upstreamReq := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(resp)
	defer fasthttp.ReleaseRequest(upstreamReq)

	upstreamReq.Header.SetMethod(req.Method)
	upstreamReq.SetRequestURI(requestURL)
	providerUtils.StripCallerAuthForInsecureURL(requestURL, req.SafeHeaders)
	cfg.setHeaders(ctx, upstreamReq, key, req.SafeHeaders)
	if !providerUtils.ApplyLargePayloadRequestBodyWithModelNormalization(ctx, upstreamReq, cfg.Provider) {
		body, bifrostErr := cfg.requestBody(req)
		if bifrostErr != nil {
			return nil, bifrostErr
		}
		upstreamReq.SetBody(body)
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
	requestURL, bifrostErr := cfg.requestURL(req)
	if bifrostErr != nil {
		return nil, bifrostErr
	}

	startTime := time.Now()
	upstreamReq := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	resp.StreamBody = true
	defer fasthttp.ReleaseRequest(upstreamReq)

	upstreamReq.Header.SetMethod(req.Method)
	upstreamReq.SetRequestURI(requestURL)
	providerUtils.StripCallerAuthForInsecureURL(requestURL, req.SafeHeaders)
	cfg.setHeaders(ctx, upstreamReq, key, req.SafeHeaders)
	upstreamReq.Header.Set("Connection", "close")
	if !providerUtils.ApplyLargePayloadRequestBodyWithModelNormalization(ctx, upstreamReq, cfg.Provider) {
		body, bifrostErr := cfg.requestBody(req)
		if bifrostErr != nil {
			fasthttp.ReleaseResponse(resp)
			return nil, bifrostErr
		}
		upstreamReq.SetBody(body)
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
	strippedPath := req.Path
	if idx := strings.IndexByte(strippedPath, '?'); idx >= 0 {
		strippedPath = strippedPath[:idx]
	}
	if strings.HasSuffix(strippedPath, "/messages") {
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
