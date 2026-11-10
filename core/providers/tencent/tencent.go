// Package tencent implements Tencent Cloud TokenHub's Hy (Hunyuan) image generation.
//
// TokenHub (大模型服务平台 TokenHub, https://tokenhub.tencentmaas.com) serves the Hy image
// models at their own paths, not OpenAI's /v1/images/generations, and the two models take
// different bodies, so they cannot run on an OpenAI-compatible custom provider. Source:
// "Hy 生图调用指南", https://cloud.tencent.com/document/product/1823/135745 (updated
// 2026-10-12). Tencent chat is OpenAI-compatible and runs on an openai-based custom provider.
package tencent

import (
	"strings"
	"time"

	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	schemas "github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

// tencentDefaultBaseURL is TokenHub's API host on Tencent Cloud's domestic (mainland) site.
const tencentDefaultBaseURL = "https://tokenhub.tencentmaas.com"

// TencentProvider implements the Provider interface for Tencent Cloud TokenHub.
type TencentProvider struct {
	logger               schemas.Logger
	client               *fasthttp.Client
	customProviderConfig *schemas.CustomProviderConfig // allowed_requests and request_path_overrides of a custom provider built on tencent
	networkConfig        schemas.NetworkConfig
	sendBackRawRequest   bool
	sendBackRawResponse  bool
}

// NewTencentProvider creates a Tencent TokenHub provider. Image generation is synchronous on
// TokenHub, so the provider holds only the unary client.
func NewTencentProvider(config *schemas.ProviderConfig, logger schemas.Logger) (*TencentProvider, error) {
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

	if config.NetworkConfig.BaseURL == "" {
		config.NetworkConfig.BaseURL = tencentDefaultBaseURL
	}
	config.NetworkConfig.BaseURL = strings.TrimRight(config.NetworkConfig.BaseURL, "/")

	return &TencentProvider{
		logger:               logger,
		client:               client,
		customProviderConfig: config.CustomProviderConfig,
		networkConfig:        config.NetworkConfig,
		sendBackRawRequest:   config.SendBackRawRequest,
		sendBackRawResponse:  config.SendBackRawResponse,
	}, nil
}

// GetProviderKey returns tencent, or the custom provider's own name when one is built on it.
func (provider *TencentProvider) GetProviderKey() schemas.ModelProvider {
	return providerUtils.GetProviderName(schemas.Tencent, provider.customProviderConfig)
}

// SupportedUnifiedFields declares the unified fields image generation delivers, so core refuses
// any other with caller_unsupported_parameter before a key is chosen (contract C4, BF4).
func (provider *TencentProvider) SupportedUnifiedFields(requestType schemas.RequestType) ([]string, bool) {
	if requestType == schemas.ImageGenerationRequest {
		return tencentImageGenerationFields, true
	}
	return nil, false
}

// ImageGeneration generates an image with a TokenHub Hy image model. One request is one TokenHub
// call; TokenHub finishes generation before it answers.
func (provider *TencentProvider) ImageGeneration(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostImageGenerationRequest) (*schemas.BifrostImageGenerationResponse, *schemas.BifrostError) {
	if bifrostErr := providerUtils.CheckOperationAllowed(schemas.Tencent, provider.customProviderConfig, schemas.ImageGenerationRequest); bifrostErr != nil {
		return nil, bifrostErr
	}
	family := tencentImageFamilyOf(request.Model)
	if family == tencentImageUnknown {
		return nil, newTencentModelUnknownError(request.Model)
	}
	if bifrostErr := validateTencentImageParams(request.Params); bifrostErr != nil {
		return nil, bifrostErr
	}
	jsonData, bifrostErr := providerUtils.CheckContextAndGetRequestBody(ctx, request,
		func() (providerUtils.RequestBodyWithExtraParams, error) {
			return ToTencentImageGenerationRequest(request)
		})
	if bifrostErr != nil {
		return nil, bifrostErr
	}

	sendBackRawRequest := providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest)
	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)

	body, latency, bifrostErr := provider.post(ctx, key, provider.requestURL(ctx, family), jsonData, sendBackRawRequest, sendBackRawResponse)
	if bifrostErr != nil {
		return nil, bifrostErr
	}

	var tencentResp TencentImageResponse
	rawRequest, rawResponse, bifrostErr := providerUtils.HandleProviderResponseCtx(ctx, body, &tencentResp, jsonData, sendBackRawRequest, sendBackRawResponse)
	if bifrostErr != nil {
		return nil, providerUtils.SetErrorLatency(bifrostErr, latency)
	}
	bifrostResp, bifrostErr := ToBifrostImageGenerationResponse(&tencentResp, request.Model)
	if bifrostErr != nil {
		return nil, providerUtils.EnrichError(ctx, bifrostErr, jsonData, body, sendBackRawRequest, sendBackRawResponse, latency)
	}
	bifrostResp.ExtraFields.Latency = latency.Milliseconds()
	if sendBackRawRequest {
		bifrostResp.ExtraFields.RawRequest = rawRequest
	}
	if sendBackRawResponse {
		bifrostResp.ExtraFields.RawResponse = rawResponse
	}
	return bifrostResp, nil
}

// requestURL is the model's TokenHub endpoint, or the custom provider's
// request_path_overrides.image_generation when it sets one.
func (provider *TencentProvider) requestURL(ctx *schemas.BifrostContext, family tencentImageFamily) string {
	path, isAbsolute := providerUtils.GetRequestPath(ctx, family.defaultPath(), provider.customProviderConfig, schemas.ImageGenerationRequest)
	if isAbsolute {
		return path
	}
	return provider.networkConfig.BaseURL + path
}

// post sends one JSON request with TokenHub's bearer key and returns the decoded body of a 2xx
// answer. A non-2xx answer becomes an error that keeps the vendor's status, code and body.
func (provider *TencentProvider) post(ctx *schemas.BifrostContext, key schemas.Key, url string, jsonData []byte, sendBackRawRequest, sendBackRawResponse bool) ([]byte, time.Duration, *schemas.BifrostError) {
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)
	req.SetRequestURI(url)
	req.Header.SetMethod(fasthttp.MethodPost)
	req.Header.SetContentType("application/json")
	if value := key.Value.GetValue(); value != "" {
		req.Header.Set("Authorization", "Bearer "+value)
	}
	req.SetBody(jsonData)

	latency, bifrostErr, wait := providerUtils.MakeRequestWithContext(ctx, provider.client, req, resp)
	defer wait()
	if bifrostErr != nil {
		return nil, latency, bifrostErr
	}
	if resp.StatusCode() < fasthttp.StatusOK || resp.StatusCode() >= fasthttp.StatusMultipleChoices {
		var errBody []byte
		if decoded, err := providerUtils.CheckAndDecodeBody(resp); err == nil {
			errBody = append([]byte(nil), decoded...)
		}
		return nil, latency, providerUtils.EnrichError(ctx, parseTencentError(resp), jsonData, errBody, sendBackRawRequest, sendBackRawResponse, latency)
	}

	body, err := providerUtils.CheckAndDecodeBody(resp)
	if err != nil {
		rawErrBody := append([]byte(nil), resp.Body()...)
		return nil, latency, providerUtils.EnrichError(ctx, providerUtils.NewBifrostOperationError(schemas.ErrProviderResponseDecode, err), jsonData, rawErrBody, sendBackRawRequest, sendBackRawResponse, latency)
	}
	return append([]byte(nil), body...), latency, nil
}
