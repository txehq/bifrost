// Package assemblyai implements the AssemblyAI provider for Bifrost.
// AssemblyAI serves pre-recorded transcription through its Sync API: one
// POST /v1/transcribe returns the transcript, with no upload step and no polling.
// Bifrost exposes it as the transcription operation; every other operation returns
// unsupported.
package assemblyai

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/bytedance/sonic"
	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	schemas "github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

const (
	// assemblyAIDefaultBaseURL is the Sync API's global endpoint. The US and EU data
	// residency endpoints (sync.us.assemblyai.com, sync.eu.assemblyai.com) are set through
	// network_config.base_url.
	assemblyAIDefaultBaseURL = "https://sync.assemblyai.com"
	assemblyAITranscribePath = "/v1/transcribe"
	// assemblyAIModelHeader names the speech model; the Sync API requires it on every request.
	assemblyAIModelHeader = "X-AAI-Model"
)

// AssemblyAIProvider implements the Provider interface for AssemblyAI's Sync API.
type AssemblyAIProvider struct {
	logger              schemas.Logger        // Logger for provider operations
	client              *fasthttp.Client      // HTTP client for unary API requests (ReadTimeout bounds overall response)
	streamingClient     *fasthttp.Client      // HTTP client for streaming API requests (no ReadTimeout; unused today, kept per provider pattern)
	networkConfig       schemas.NetworkConfig // Network configuration including extra headers
	sendBackRawRequest  bool                  // Whether to include raw request in BifrostResponse
	sendBackRawResponse bool                  // Whether to include raw response in BifrostResponse
}

// NewAssemblyAIProvider creates a new AssemblyAI provider instance.
func NewAssemblyAIProvider(config *schemas.ProviderConfig, logger schemas.Logger) (*AssemblyAIProvider, error) {
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

	// Configure proxy if provided
	client = providerUtils.ConfigureProxy(client, config.ProxyConfig, logger)
	client = providerUtils.ConfigureDialer(client, config.NetworkConfig.AllowPrivateNetwork)
	client = providerUtils.ConfigureTLS(client, config.NetworkConfig, logger)
	streamingClient := providerUtils.BuildStreamingClient(client)

	if config.NetworkConfig.BaseURL == "" {
		config.NetworkConfig.BaseURL = assemblyAIDefaultBaseURL
	}
	config.NetworkConfig.BaseURL = strings.TrimRight(config.NetworkConfig.BaseURL, "/")

	return &AssemblyAIProvider{
		logger:              logger,
		client:              client,
		streamingClient:     streamingClient,
		networkConfig:       config.NetworkConfig,
		sendBackRawRequest:  config.SendBackRawRequest,
		sendBackRawResponse: config.SendBackRawResponse,
	}, nil
}

// GetProviderKey returns the provider identifier for AssemblyAI.
func (provider *AssemblyAIProvider) GetProviderKey() schemas.ModelProvider {
	return schemas.AssemblyAI
}

// SupportedUnifiedFields declares the unified transcription fields the Sync API takes, so a
// request carrying any other is refused before a key is chosen (contract C4).
func (provider *AssemblyAIProvider) SupportedUnifiedFields(requestType schemas.RequestType) ([]string, bool) {
	if requestType == schemas.TranscriptionRequest {
		return assemblyAITranscriptionUnifiedFields, true
	}
	return nil, false
}

// Transcription transcribes audio with one call to the Sync API's POST /v1/transcribe. The
// request's model goes in the X-AAI-Model header, the audio in the `audio` part and the
// request fields in the `config` part. The call is made once: no upload, no polling, no
// retry (contract C3).
func (provider *AssemblyAIProvider) Transcription(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostTranscriptionRequest) (*schemas.BifrostTranscriptionResponse, *schemas.BifrostError) {
	assemblyAIReq, bifrostErr := ToAssemblyAITranscriptionRequest(request)
	if bifrostErr != nil {
		return nil, bifrostErr
	}

	var body bytes.Buffer
	// One allocation for the audio plus the part headers and the config.
	body.Grow(len(assemblyAIReq.Audio) + 1024)
	writer := multipart.NewWriter(&body)
	if bifrostErr := writeAssemblyAITranscriptionMultipart(writer, assemblyAIReq); bifrostErr != nil {
		return nil, bifrostErr
	}
	contentType := writer.FormDataContentType()
	if err := writer.Close(); err != nil {
		return nil, providerUtils.NewBifrostOperationError("failed to finalize multipart transcription request", err)
	}

	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)

	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)
	req.SetRequestURI(provider.networkConfig.BaseURL + providerUtils.GetPathFromContext(ctx, assemblyAITranscribePath))
	req.Header.SetMethod(http.MethodPost)
	req.Header.SetContentType(contentType)
	req.Header.Set(assemblyAIModelHeader, assemblyAIReq.Model)
	if value := key.Value.GetValue(); value != "" {
		// AssemblyAI takes the API key itself as the Authorization value.
		req.Header.Set("Authorization", value)
	}
	// The buffer outlives the request (wait runs before this function returns), so the body
	// is sent without a second copy of the audio.
	req.SetBodyRaw(body.Bytes())

	latency, bifrostErr, wait := providerUtils.MakeRequestWithContext(ctx, provider.client, req, resp)
	defer wait()
	if bifrostErr != nil {
		return nil, providerUtils.EnrichError(ctx, bifrostErr, nil, nil, false, sendBackRawResponse, latency)
	}

	providerResponseHeaders := providerUtils.ExtractProviderResponseHeaders(resp)
	ctx.SetValue(schemas.BifrostContextKeyProviderResponseHeaders, providerResponseHeaders)

	if resp.StatusCode() < fasthttp.StatusOK || resp.StatusCode() >= fasthttp.StatusMultipleChoices {
		return nil, providerUtils.EnrichError(ctx, parseAssemblyAIError(resp), nil, nil, false, sendBackRawResponse, latency)
	}

	responseBody, err := providerUtils.CheckAndDecodeBody(resp)
	if err != nil {
		return nil, providerUtils.EnrichError(ctx, providerUtils.NewBifrostOperationError(schemas.ErrProviderResponseDecode, err), nil, nil, false, sendBackRawResponse, latency)
	}

	var assemblyAIResp AssemblyAITranscriptionResponse
	if err := sonic.Unmarshal(responseBody, &assemblyAIResp); err != nil {
		return nil, providerUtils.EnrichError(ctx, providerUtils.NewBifrostOperationError(schemas.ErrProviderResponseUnmarshal, err), nil, responseBody, false, sendBackRawResponse, latency)
	}

	response := ToBifrostTranscriptionResponse(&assemblyAIResp)
	response.BackfillParams(request)
	response.ExtraFields = schemas.BifrostResponseExtraFields{
		Latency:                 latency.Milliseconds(),
		ProviderResponseHeaders: providerResponseHeaders,
	}
	if sendBackRawResponse {
		var rawResponse interface{}
		if err := sonic.Unmarshal(responseBody, &rawResponse); err != nil {
			rawResponse = string(responseBody)
		}
		response.ExtraFields.RawResponse = rawResponse
	}
	return response, nil
}
