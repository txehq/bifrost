// Package deepgram implements the Deepgram provider for Bifrost.
// Deepgram's pre-recorded speech-to-text API (POST /v1/listen) takes its options as query
// parameters and the audio as the request body, and answers synchronously. Bifrost serves
// it through the transcription operation; every other operation returns unsupported.
package deepgram

import (
	"net/http"
	"strings"
	"time"

	"github.com/bytedance/sonic"
	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	schemas "github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

const (
	deepgramDefaultBaseURL = "https://api.deepgram.com"
	deepgramListenPath     = "/v1/listen"
)

// DeepgramProvider implements the Provider interface for Deepgram's API.
type DeepgramProvider struct {
	logger              schemas.Logger        // Logger for provider operations
	client              *fasthttp.Client      // HTTP client for unary API requests (ReadTimeout bounds overall response)
	streamingClient     *fasthttp.Client      // HTTP client for streaming API requests (no ReadTimeout; unused today, kept per provider pattern)
	networkConfig       schemas.NetworkConfig // Network configuration including extra headers
	sendBackRawRequest  bool                  // Whether to include raw request in BifrostResponse
	sendBackRawResponse bool                  // Whether to include raw response in BifrostResponse
}

// NewDeepgramProvider creates a new Deepgram provider instance.
func NewDeepgramProvider(config *schemas.ProviderConfig, logger schemas.Logger) (*DeepgramProvider, error) {
	config.CheckAndSetDefaults()

	requestTimeout := time.Second * time.Duration(config.NetworkConfig.DefaultRequestTimeoutInSeconds)
	client := &fasthttp.Client{
		ReadTimeout:         requestTimeout,
		WriteTimeout:        requestTimeout,
		MaxConnsPerHost:     config.NetworkConfig.MaxConnsPerHost,
		MaxIdleConnDuration: time.Second * time.Duration(config.NetworkConfig.KeepAliveTimeoutInSeconds),
		MaxConnWaitTimeout:  requestTimeout,
		MaxConnDuration:     time.Second * time.Duration(schemas.DefaultMaxConnDurationInSeconds),
		ConnPoolStrategy:    fasthttp.FIFO,
	}

	client = providerUtils.ConfigureProxy(client, config.ProxyConfig, logger)
	client = providerUtils.ConfigureDialer(client, config.NetworkConfig.AllowPrivateNetwork)
	client = providerUtils.ConfigureTLS(client, config.NetworkConfig, logger)
	streamingClient := providerUtils.BuildStreamingClient(client)

	if config.NetworkConfig.BaseURL == "" {
		config.NetworkConfig.BaseURL = deepgramDefaultBaseURL
	}
	config.NetworkConfig.BaseURL = strings.TrimRight(config.NetworkConfig.BaseURL, "/")

	return &DeepgramProvider{
		logger:              logger,
		client:              client,
		streamingClient:     streamingClient,
		networkConfig:       config.NetworkConfig,
		sendBackRawRequest:  config.SendBackRawRequest,
		sendBackRawResponse: config.SendBackRawResponse,
	}, nil
}

// GetProviderKey returns the provider identifier for Deepgram.
func (provider *DeepgramProvider) GetProviderKey() schemas.ModelProvider {
	return schemas.Deepgram
}

// SupportedUnifiedFields declares the unified transcription fields Deepgram takes, so a
// request carrying any other is refused with caller_unsupported_parameter before a key is
// chosen (contract C4). Other operations are unsupported outright and declare nothing.
func (provider *DeepgramProvider) SupportedUnifiedFields(requestType schemas.RequestType) ([]string, bool) {
	if requestType == schemas.TranscriptionRequest {
		return deepgramSupportedTranscriptionFields, true
	}
	return nil, false
}

// AcceptsTranscriptionURLInput reports that Deepgram takes an audio URL in place of the
// file: it fetches the URL itself (contract C4, agreement four).
func (provider *DeepgramProvider) AcceptsTranscriptionURLInput() bool {
	return true
}

// Transcription sends one POST /v1/listen to Deepgram and converts its answer: the
// transcript into text and the billable duration into usage. Deepgram answers
// synchronously, so one request is one upstream call.
func (provider *DeepgramProvider) Transcription(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostTranscriptionRequest) (*schemas.BifrostTranscriptionResponse, *schemas.BifrostError) {
	ct, ch := providerUtils.StartPhaseSpan(ctx, "convertor")
	deepgramReq, bifrostErr := ToDeepgramTranscriptionRequest(request)
	if ct != nil {
		if bifrostErr != nil {
			ct.EndSpan(ch, schemas.SpanStatusError, bifrostErr.Error.Message)
		} else {
			ct.EndSpan(ch, schemas.SpanStatusOk, "")
		}
	}
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	// Only the JSON URL body can be sent back as the raw request; audio bytes are not JSON.
	var rawRequest []byte
	if deepgramReq.IsURL {
		rawRequest = deepgramReq.Body
	}

	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)

	req.SetRequestURI(provider.networkConfig.BaseURL + providerUtils.GetPathFromContext(ctx, deepgramListenPath) + "?" + deepgramReq.Query)
	req.Header.SetMethod(http.MethodPost)
	req.Header.SetContentType(deepgramReq.ContentType)
	if key.Value.GetValue() != "" {
		req.Header.Set("Authorization", "Token "+key.Value.GetValue())
	}
	// SetBodyRaw sends the caller's audio without copying it; the request is released
	// before this function returns, so the slice is not retained.
	req.SetBodyRaw(deepgramReq.Body)

	latency, bifrostErr, wait := providerUtils.MakeRequestWithContext(ctx, provider.client, req, resp)
	defer wait()
	if bifrostErr != nil {
		return nil, providerUtils.EnrichError(ctx, bifrostErr, rawRequest, nil, provider.sendBackRawRequest, provider.sendBackRawResponse, latency)
	}

	providerResponseHeaders := providerUtils.ExtractProviderResponseHeaders(resp)
	ctx.SetValue(schemas.BifrostContextKeyProviderResponseHeaders, providerResponseHeaders)

	if resp.StatusCode() != fasthttp.StatusOK {
		return nil, providerUtils.EnrichError(ctx, parseDeepgramError(resp), rawRequest, resp.Body(), provider.sendBackRawRequest, provider.sendBackRawResponse, latency)
	}

	ft, fh := providerUtils.StartPhaseSpan(ctx, "response-finalize")
	responseBody, err := providerUtils.CheckAndDecodeBody(resp)
	if ft != nil {
		if err != nil {
			ft.EndSpan(fh, schemas.SpanStatusError, err.Error())
		} else {
			ft.EndSpan(fh, schemas.SpanStatusOk, "")
		}
	}
	if err != nil {
		return nil, providerUtils.EnrichError(ctx, providerUtils.NewBifrostOperationError(schemas.ErrProviderResponseDecode, err), rawRequest, resp.Body(), provider.sendBackRawRequest, provider.sendBackRawResponse, latency)
	}

	var deepgramResp DeepgramTranscriptionResponse
	pt, ph := providerUtils.StartResponseParseSpan(ctx)
	umErr := sonic.Unmarshal(responseBody, &deepgramResp)
	if pt != nil {
		if umErr != nil {
			pt.EndSpan(ph, schemas.SpanStatusError, "response parse failed")
		} else {
			pt.EndSpan(ph, schemas.SpanStatusOk, "")
		}
	}
	if umErr != nil {
		return nil, providerUtils.EnrichError(ctx, providerUtils.NewBifrostOperationError("failed to parse Deepgram transcription response", umErr), rawRequest, responseBody, provider.sendBackRawRequest, provider.sendBackRawResponse, latency)
	}

	response := ToBifrostTranscriptionResponse(&deepgramResp)
	if response == nil {
		// A 200 with no results is not a transcript (contract C6): the vendor answered
		// but delivered nothing.
		return nil, providerUtils.EnrichError(ctx, schemas.NewProviderEmptyResultError("Deepgram returned a successful response with no transcription results"), rawRequest, responseBody, provider.sendBackRawRequest, provider.sendBackRawResponse, latency)
	}
	response.ExtraFields = schemas.BifrostResponseExtraFields{
		Latency:                 latency.Milliseconds(),
		ProviderResponseHeaders: providerResponseHeaders,
	}
	if rawRequest != nil && providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest) {
		providerUtils.ParseAndSetRawRequest(&response.ExtraFields, rawRequest)
	}
	if providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse) {
		var rawResponse interface{}
		if err := sonic.Unmarshal(responseBody, &rawResponse); err != nil {
			rawResponse = string(responseBody)
		}
		response.ExtraFields.RawResponse = rawResponse
	}

	return response, nil
}
