package voyage

import (
	"net/http"
	"strings"
	"time"

	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	schemas "github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

// VoyageProvider serves text embeddings with one upstream call per input batch.
type VoyageProvider struct {
	logger               schemas.Logger                // Logger for provider operations
	client               *fasthttp.Client              // HTTP client for unary API requests (ReadTimeout bounds overall response)
	streamingClient      *fasthttp.Client              // HTTP client for streaming API requests (no ReadTimeout; unused today, kept per provider pattern)
	networkConfig        schemas.NetworkConfig         // Network configuration including extra headers
	sendBackRawRequest   bool                          // Whether to include raw request in BifrostResponse
	sendBackRawResponse  bool                          // Whether to include raw response in BifrostResponse
	customProviderConfig *schemas.CustomProviderConfig // Custom provider config
}

// NewVoyageProvider creates a new Voyage provider instance.
func NewVoyageProvider(config *schemas.ProviderConfig, logger schemas.Logger) (*VoyageProvider, error) {
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

	defaultBaseURL := config.NetworkConfig.BaseURL == ""
	if defaultBaseURL {
		config.NetworkConfig.BaseURL = "https://api.voyageai.com"
	}
	config.NetworkConfig.BaseURL = strings.TrimRight(config.NetworkConfig.BaseURL, "/")

	return &VoyageProvider{
		logger:               logger,
		client:               client,
		streamingClient:      streamingClient,
		networkConfig:        config.NetworkConfig,
		sendBackRawRequest:   config.SendBackRawRequest,
		sendBackRawResponse:  config.SendBackRawResponse,
		customProviderConfig: config.CustomProviderConfig,
	}, nil
}

// GetProviderKey returns the provider identifier for Voyage, or the custom
// provider name when Voyage backs a custom provider.
func (provider *VoyageProvider) GetProviderKey() schemas.ModelProvider {
	return providerUtils.GetProviderName(schemas.Voyage, provider.customProviderConfig)
}

// buildRequestURL resolves the request URL, honouring a context path and the
// custom provider's request_path_overrides (a path or an absolute URL).
func (provider *VoyageProvider) buildRequestURL(ctx *schemas.BifrostContext, defaultPath string, requestType schemas.RequestType) string {
	path, isCompleteURL := providerUtils.GetRequestPath(ctx, defaultPath, provider.customProviderConfig, requestType)
	if isCompleteURL {
		return path
	}
	return provider.networkConfig.BaseURL + path
}

var _ schemas.Provider = (*VoyageProvider)(nil)

func (provider *VoyageProvider) completeRequest(ctx *schemas.BifrostContext, jsonData []byte, url string, key string) ([]byte, time.Duration, map[string]string, *schemas.BifrostError) {
	// Create the request with the JSON body
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	respOwned := true
	defer func() {
		if respOwned {
			fasthttp.ReleaseResponse(resp)
		}
	}()

	// Set any extra headers from network config
	providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)

	req.SetRequestURI(url)
	req.Header.SetMethod(http.MethodPost)
	req.Header.SetContentType("application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}

	usedLargePayloadBody := providerUtils.ApplyLargePayloadRequestBodyWithModelNormalization(ctx, req, schemas.Voyage)
	if !usedLargePayloadBody {
		req.SetBody(jsonData)
	}

	// Send the request with optional large response streaming
	activeClient := providerUtils.PrepareResponseStreaming(ctx, provider.client, resp)
	latency, bifrostErr, wait := providerUtils.MakeRequestWithContext(ctx, activeClient, req, resp)
	defer wait()
	if usedLargePayloadBody {
		providerUtils.DrainLargePayloadRemainder(ctx)
	}
	if bifrostErr != nil {
		return nil, latency, nil, bifrostErr
	}

	// Extract provider response headers before status check so error responses also forward them
	providerResponseHeaders := providerUtils.ExtractProviderResponseHeaders(resp)

	// Handle error response
	if resp.StatusCode() != fasthttp.StatusOK {
		providerUtils.MaterializeStreamErrorBody(ctx, resp)
		return nil, latency, providerResponseHeaders, providerUtils.SetErrorLatency(parseVoyageError(resp), latency)
	}

	body, isLargeResp, decodeErr := providerUtils.FinalizeResponseWithLargeDetection(ctx, resp, provider.logger)
	if decodeErr != nil {
		return nil, latency, providerResponseHeaders, decodeErr
	}
	if isLargeResp {
		respOwned = false
		return nil, latency, providerResponseHeaders, nil
	}

	return body, latency, providerResponseHeaders, nil
}

func (provider *VoyageProvider) Embedding(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostEmbeddingRequest) (*schemas.BifrostEmbeddingResponse, *schemas.BifrostError) {
	// Check if embedding is allowed
	if err := providerUtils.CheckOperationAllowed(schemas.Voyage, provider.customProviderConfig, schemas.EmbeddingRequest); err != nil {
		return nil, err
	}

	var converted *VoyageEmbeddingRequest
	if _, raw := providerUtils.CheckAndGetRawRequestBody(ctx, request); !raw {
		var validationErr *schemas.BifrostError
		converted, validationErr = ToVoyageEmbeddingRequest(request)
		if validationErr != nil {
			return nil, validationErr
		}
	}
	jsonBody, bifrostErr := providerUtils.CheckContextAndGetRequestBody(
		ctx,
		request,
		func() (providerUtils.RequestBodyWithExtraParams, error) {
			return converted, nil
		})
	if bifrostErr != nil {
		return nil, bifrostErr
	}

	// Create Bifrost request for conversion
	responseBody, latency, providerResponseHeaders, err := provider.completeRequest(ctx, jsonBody, provider.buildRequestURL(ctx, "/v1/embeddings", schemas.EmbeddingRequest), key.Value.GetValue())
	if providerResponseHeaders != nil {
		ctx.SetValue(schemas.BifrostContextKeyProviderResponseHeaders, providerResponseHeaders)
	}
	if err != nil {
		return nil, providerUtils.EnrichError(ctx, err, jsonBody, nil, provider.sendBackRawRequest, provider.sendBackRawResponse, latency)
	}

	// Large response mode: return lightweight response with metadata only
	if isLargeResp, _ := ctx.Value(schemas.BifrostContextKeyLargeResponseMode).(bool); isLargeResp {
		return &schemas.BifrostEmbeddingResponse{
			Model: request.Model,
			ExtraFields: schemas.BifrostResponseExtraFields{
				Latency:                 latency.Milliseconds(),
				ProviderResponseHeaders: providerResponseHeaders,
			},
		}, nil
	}

	// Decode the vendor response.
	response := &VoyageEmbeddingResponse{}

	rawRequest, rawResponse, bifrostErr := providerUtils.HandleProviderResponseCtx(ctx, responseBody, response, jsonBody, providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest), providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse))
	if bifrostErr != nil {
		return nil, providerUtils.EnrichError(ctx, bifrostErr, jsonBody, responseBody, provider.sendBackRawRequest, provider.sendBackRawResponse, latency)
	}

	if len(response.Data) == 0 {
		return nil, providerUtils.EnrichError(ctx, schemas.NewProviderEmptyResultError("voyage returned no embeddings"), jsonBody, responseBody, provider.sendBackRawRequest, provider.sendBackRawResponse, latency)
	}
	convTracer, convHandle := providerUtils.StartResponseConvertorSpan(ctx)
	bifrostResponse := response.ToBifrostEmbeddingResponse(request.Model)
	if convTracer != nil {
		convTracer.EndSpan(convHandle, schemas.SpanStatusOk, "")
	}

	// Set ExtraFields
	bifrostResponse.ExtraFields.Latency = latency.Milliseconds()
	bifrostResponse.ExtraFields.ProviderResponseHeaders = providerResponseHeaders

	// Set raw request if enabled
	if providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest) {
		bifrostResponse.ExtraFields.RawRequest = rawRequest
	}

	// Set raw response if enabled
	if providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse) {
		bifrostResponse.ExtraFields.RawResponse = rawResponse
	}

	return bifrostResponse, nil
}

// SupportedUnifiedFields participates in the shared unified-field admission check.
func (provider *VoyageProvider) SupportedUnifiedFields(rt schemas.RequestType) ([]string, bool) {
	if rt != schemas.EmbeddingRequest {
		return nil, false
	}
	return voyageEmbeddingFields, true
}

var voyageEmbeddingFields = []string{"dimensions", "encoding_format", "task_type", "auto_truncate"}
