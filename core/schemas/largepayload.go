package schemas

import (
	"fmt"
	"strings"
)

// Large-payload passthrough eligibility (fork, TXE-7082).
//
// Two optimizations skip Bifrost's converters once a body crosses a size threshold. A large
// request is not parsed: the caller's bytes are streamed to the vendor as written. A large
// response is not parsed either: the vendor's bytes are streamed to the caller as written.
// Both are right only when the bytes are already in the shape the other side expects, which
// depends on the route the caller used, the provider, the model and the operation. Where they
// are not, the request reaches the vendor without its native parameter mapping and the caller
// receives the vendor's usage object instead of the unified one.
//
// DecideLargePayloadEligibility is the one rule. Core stamps its answer on the context before
// every provider attempt, and the shared entry points in core/providers/utils read the stamp,
// so no provider decides for itself. With no stamp nothing is passed through raw.

// BifrostContextKeyLargePayloadEligibility holds the LargePayloadEligibility of the current provider attempt (set by bifrost - DO NOT SET THIS MANUALLY).
const BifrostContextKeyLargePayloadEligibility BifrostContextKey = "bifrost-large-payload-eligibility"

// BifrostContextKeyBodyConversionRequired holds a string: why a plugin must read or rewrite the request or response body (set with RequireBodyConversion).
const BifrostContextKeyBodyConversionRequired BifrostContextKey = "bifrost-body-conversion-required"

// LargePayloadEligibility says whether one provider attempt may skip conversion for a large body.
type LargePayloadEligibility struct {
	RawRequest  bool   // the caller's unparsed request body may be streamed to the vendor
	RawResponse bool   // the vendor's response body may be streamed to the caller unparsed
	Reason      string // why raw is not allowed; empty when both are
}

// largePayloadDeclaration is a provider's answer to "which callers' bodies does your vendor
// take unchanged". nativeWire returns the integration type (BifrostContextKeyIntegrationType)
// whose bodies need no conversion for this request, or "" when the request is converted.
// converts is the reason given when nativeWire is nil: every request is converted.
type largePayloadDeclaration struct {
	nativeWire func(ctx *BifrostContext, requestType RequestType, model string) string
	converts   string
}

func nativeOn(wire string) largePayloadDeclaration {
	return largePayloadDeclaration{nativeWire: func(*BifrostContext, RequestType, string) string { return wire }}
}

func convertsBecause(reason string) largePayloadDeclaration {
	return largePayloadDeclaration{converts: reason}
}

const (
	largePayloadOpenAICompatible = "its requests are built by a converter that maps unified fields to the vendor's own, and its usage is converted on the way back"
	largePayloadOwnAPI           = "its API is not the caller's wire format: every request and response is converted"
)

// largePayloadDeclarations has one entry per standard provider. A provider with no entry is
// treated as converting everything, and TestLargePayloadDeclarationsCoverEveryProvider fails
// until it has one, so a new provider states its requirement instead of inheriting a default.
//
// A provider is native on a wire only when its vendor takes that wire's body with nothing
// added, removed or renamed. OpenAI-compatible vendors are not: their converters rename and
// drop fields. Vertex, Azure and Bedrock Mantle are not for Claude: they rewrite the raw
// Anthropic body (tool versions, unsupported fields, anthropic_version), which needs the bytes.
var largePayloadDeclarations = map[ModelProvider]largePayloadDeclaration{
	OpenAI:    nativeOn("openai"),
	Anthropic: nativeOn("anthropic"),
	Gemini:    nativeOn("genai"),
	Cohere:    nativeOn("cohere"),
	Typesafe:  nativeOn("typesafe"),
	Azure: {nativeWire: func(ctx *BifrostContext, _ RequestType, model string) string {
		if IsAnthropicModelFamily(ctx, model) {
			return ""
		}
		return "openai"
	}},
	Vertex: {nativeWire: func(ctx *BifrostContext, _ RequestType, model string) string {
		if model == "" || IsAllDigitsASCII(model) || IsGeminiModelFamily(ctx, model) || IsGemmaModelFamily(ctx, model) ||
			IsImagenModelFamily(ctx, model) || IsVeoModelFamily(ctx, model) {
			return "genai"
		}
		return ""
	}},

	Bedrock:        convertsBecause("its requests are signed over the body Bifrost builds, so a body cannot be streamed unparsed"),
	BedrockMantle:  convertsBecause("its requests are signed over the body Bifrost builds, so a body cannot be streamed unparsed"),
	Cerebras:       convertsBecause(largePayloadOpenAICompatible),
	Cloudflare:     convertsBecause(largePayloadOpenAICompatible),
	DashScope:      convertsBecause(largePayloadOpenAICompatible),
	Databricks:     convertsBecause(largePayloadOpenAICompatible),
	DeepSeek:       convertsBecause(largePayloadOpenAICompatible),
	Fireworks:      convertsBecause(largePayloadOpenAICompatible),
	GithubCopilot:  convertsBecause(largePayloadOpenAICompatible),
	GLM:            convertsBecause(largePayloadOpenAICompatible),
	Groq:           convertsBecause(largePayloadOpenAICompatible),
	Mistral:        convertsBecause(largePayloadOpenAICompatible),
	ModelArk:       convertsBecause(largePayloadOpenAICompatible),
	Moonshot:       convertsBecause(largePayloadOpenAICompatible),
	Nebius:         convertsBecause(largePayloadOpenAICompatible),
	Ollama:         convertsBecause(largePayloadOpenAICompatible),
	OpencodeGo:     convertsBecause(largePayloadOpenAICompatible),
	OpencodeZen:    convertsBecause(largePayloadOpenAICompatible),
	OpenRouter:     convertsBecause(largePayloadOpenAICompatible),
	Parasail:       convertsBecause(largePayloadOpenAICompatible),
	Perplexity:     convertsBecause(largePayloadOpenAICompatible),
	Qwen:           convertsBecause(largePayloadOpenAICompatible),
	SGL:            convertsBecause(largePayloadOpenAICompatible),
	VLLM:           convertsBecause(largePayloadOpenAICompatible),
	Volcengine:     convertsBecause(largePayloadOpenAICompatible),
	Wafer:          convertsBecause(largePayloadOpenAICompatible),
	XAI:            convertsBecause(largePayloadOpenAICompatible),
	AssemblyAI:     convertsBecause(largePayloadOwnAPI),
	AzureSpeech:    convertsBecause(largePayloadOwnAPI),
	BFL:            convertsBecause(largePayloadOwnAPI),
	BytePlus:       convertsBecause(largePayloadOwnAPI),
	Deepgram:       convertsBecause(largePayloadOwnAPI),
	Elevenlabs:     convertsBecause(largePayloadOwnAPI),
	GoogleCloudTTS: convertsBecause(largePayloadOwnAPI),
	HuggingFace:    convertsBecause(largePayloadOwnAPI),
	MiniMax:        convertsBecause(largePayloadOwnAPI),
	Replicate:      convertsBecause(largePayloadOwnAPI),
	Runware:        convertsBecause(largePayloadOwnAPI),
	Runway:         convertsBecause(largePayloadOwnAPI),
	Sarvam:         convertsBecause(largePayloadOwnAPI),
	Tencent:        convertsBecause(largePayloadOwnAPI),
}

// LargePayloadDeclared reports whether provider has an entry in the declarations.
func LargePayloadDeclared(provider ModelProvider) bool {
	_, ok := largePayloadDeclarations[provider]
	return ok
}

// largePayloadOpaqueResponse reports the operations whose response body is content with no
// unified shape and no usage in it: there is nothing to convert, whatever the route.
func largePayloadOpaqueResponse(requestType RequestType) bool {
	switch requestType {
	case FileContentRequest, ContainerFileContentRequest, VideoDownloadRequest:
		return true
	}
	return false
}

// RequireBodyConversion is how a plugin that must read or rewrite a body (a redaction
// guardrail, say) says so. A body streamed unparsed never reaches a plugin, and the raw-body
// adapters a native route installs need the bytes, so while this is set a large request is
// refused before the upstream write and a large response is buffered. Call it from a
// transport pre-hook or PreLLMHook; reason is shown to the caller.
func RequireBodyConversion(ctx *BifrostContext, reason string) {
	if ctx == nil {
		return
	}
	if reason == "" {
		reason = "a plugin must read or rewrite the body"
	}
	ctx.SetValue(BifrostContextKeyBodyConversionRequired, reason)
}

// DecideLargePayloadEligibility is the rule for one provider attempt. provider is the base
// provider and custom says the attempt runs on a custom provider built on it.
//
// A body is passed through raw only when nothing has to change in it:
//   - a passthrough request forwards the caller's bytes by definition;
//   - otherwise the caller must have used a native route (the integration type on the
//     context) and the provider must declare that wire native for this model and operation.
//     The neutral /v1 routes and Go SDK callers carry no integration type: their requests are
//     unified, so they are always converted;
//   - a response is also raw when the operation returns opaque content (a file, a video);
//   - a custom provider converts, because its overrides are not visible to this rule;
//   - a plugin that called RequireBodyConversion turns both off.
func DecideLargePayloadEligibility(ctx *BifrostContext, provider ModelProvider, custom bool, requestType RequestType, model string) LargePayloadEligibility {
	if ctx != nil {
		if reason, _ := ctx.Value(BifrostContextKeyBodyConversionRequired).(string); reason != "" {
			return LargePayloadEligibility{Reason: reason}
		}
	}
	if requestType == PassthroughRequest || requestType == PassthroughStreamRequest {
		return LargePayloadEligibility{RawRequest: true, RawResponse: true}
	}
	opaque := largePayloadOpaqueResponse(requestType)
	wire := ""
	if ctx != nil {
		wire, _ = ctx.Value(BifrostContextKeyIntegrationType).(string)
	}
	reason := ""
	switch declaration, declared := largePayloadDeclarations[provider]; {
	case wire == "":
		reason = "the request did not come through a provider's native route, so it is in the unified format and is converted"
	case custom:
		reason = fmt.Sprintf("%s is the base of a custom provider, whose overrides may change the request", provider)
	case !declared:
		reason = fmt.Sprintf("%s declares no wire format it takes unchanged", provider)
	case declaration.nativeWire == nil:
		reason = fmt.Sprintf("%s: %s", provider, declaration.converts)
	case declaration.nativeWire(ctx, requestType, model) != wire:
		reason = fmt.Sprintf("%s does not take the %s wire format unchanged for model %q", provider, wire, model)
	default:
		return LargePayloadEligibility{RawRequest: true, RawResponse: true}
	}
	if opaque {
		return LargePayloadEligibility{RawResponse: true, Reason: reason}
	}
	return LargePayloadEligibility{Reason: reason}
}

// StagedLargePayloadModel returns the model a staged large request body names, as it would go
// on the wire to provider: the model the transport hook read from the body, less a known
// provider prefix, which is the one thing the raw path rewrites. It is "" when no body is
// staged or the body names no model (the genai routes carry it in the URL).
//
// A staged body is forwarded unparsed, so it cannot be rewritten to another model. An attempt
// that serves a different model than the body names (a fallback to another model) must not
// forward it: the key and governance checks would have admitted one model and the vendor
// would be asked for another. Core's per-attempt decision refuses that attempt (TXE-7082).
func StagedLargePayloadModel(ctx *BifrostContext, provider ModelProvider) string {
	if ctx == nil {
		return ""
	}
	if staged, _ := ctx.Value(BifrostContextKeyLargePayloadMode).(bool); !staged {
		return ""
	}
	metadata, _ := ctx.Value(BifrostContextKeyLargePayloadMetadata).(*LargePayloadMetadata)
	if metadata == nil {
		return ""
	}
	raw := strings.TrimSpace(metadata.Model)
	if raw == "" {
		return ""
	}
	_, model := ParseModelString(raw, provider)
	return model
}

// LargePayloadEligibilityFromContext returns the stamp of the current attempt, and the zero
// value (nothing raw) when there is none.
func LargePayloadEligibilityFromContext(ctx interface{ Value(any) any }) LargePayloadEligibility {
	if ctx == nil {
		return LargePayloadEligibility{Reason: "no provider attempt has decided large-payload eligibility on this context"}
	}
	if eligibility, ok := ctx.Value(BifrostContextKeyLargePayloadEligibility).(LargePayloadEligibility); ok {
		return eligibility
	}
	return LargePayloadEligibility{Reason: "no provider attempt has decided large-payload eligibility on this context"}
}

// NewLargePayloadNotConvertibleError is the refusal for a large request that would have to be
// converted: 413 before any upstream write. A body above the threshold is never parsed, so
// Bifrost cannot build the vendor's request from it, and sending it as written would drop the
// provider's parameter mapping.
func NewLargePayloadNotConvertibleError(provider ModelProvider, requestType RequestType, reason string) *BifrostError {
	operation := "request"
	if requestType != "" {
		operation = string(requestType) + " request"
	}
	return &BifrostError{
		IsBifrostError: true,
		StatusCode:     Ptr(413),
		Error: &ErrorField{
			Type:    Ptr("invalid_request_error"),
			Code:    Ptr("large_payload_not_convertible"),
			Message: fmt.Sprintf("the request body is above the large-payload threshold and is forwarded unparsed, but this %s must be converted: %s. Send it below the threshold, or through the provider's native route", operation, reason),
		},
		ExtraFields: BifrostErrorExtraFields{
			ErrorType:   ErrorTypeCallerInvalidRequest,
			Provider:    provider,
			RequestType: requestType,
		},
	}
}
