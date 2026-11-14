package datasheet

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"strconv"
	"testing"

	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/providers/minimax"
	"github.com/capsohq/bifrost/core/schemas"
	configstoreTables "github.com/capsohq/bifrost/framework/configstore/tables"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCalculateCost_RealtimeTranscriptionPricingOverride(t *testing.T) {
	pricing := configstoreTables.TableModelPricing{
		Model:                  "gpt-4o-transcribe",
		Provider:               "openai",
		Mode:                   "audio_transcription",
		InputCostPerToken:      bifrost.Ptr(0.0000025),
		OutputCostPerToken:     bifrost.Ptr(0.00001),
		InputCostPerAudioToken: bifrost.Ptr(0.0000025),
	}
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey(pricing.Model, pricing.Provider, pricing.Mode): pricing,
	})
	resp := &schemas.BifrostResponse{
		ResponsesResponse: &schemas.BifrostResponsesResponse{
			Usage: &schemas.ResponsesResponseUsage{
				InputTokens:  29,
				OutputTokens: 14,
				TotalTokens:  43,
				InputTokensDetails: &schemas.ResponsesResponseInputTokens{
					AudioTokens: 28,
					TextTokens:  1,
				},
			},
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType:        schemas.RealtimeRequest,
				PricingRequestType: schemas.TranscriptionRequest,
				RoutingInfo:        routingInfoFor(schemas.OpenAI, "gpt-4o-transcribe"),
			},
		},
	}

	breakdown := s.CalculateCostBreakdown(resp, nil)
	require.NotNil(t, breakdown)
	assert.InDelta(t, 0.0002125, breakdown.TotalCost, 1e-12)
	assert.InDelta(t, 0.00007, breakdown.InputCostDetails.AudioCost, 1e-12)
	assert.InDelta(t, 0.0000025, breakdown.InputCostDetails.TextCost, 1e-12)
	assert.InDelta(t, 0.00014, breakdown.OutputCostDetails.TextCost, 1e-12)
}

// TestCalculateCost_RealtimeTextMessagesAreBilledPerMessage (TXE-7363): the text messages a client
// sent into an xAI realtime voice turn are charged input_cost_per_text_message each, on the input
// side as text. The rates are the upstream datasheet's for xai/grok-voice-3.0 (14 Nov 2026):
// $0.004 per text message, $0.08 per minute of audio. Before the key was parsed a text message
// was free.
func TestCalculateCost_RealtimeTextMessagesAreBilledPerMessage(t *testing.T) {
	pricing := configstoreTables.TableModelPricing{
		Model: "grok-voice-3.0", Provider: "xai", Mode: "realtime",
		InputCostPerSecond:      bifrost.Ptr(0.001333333333333),
		InputCostPerTextMessage: bifrost.Ptr(0.004),
		InputCostPerToken:       bifrost.Ptr(0.000001),
	}
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{makeKey(pricing.Model, pricing.Provider, pricing.Mode): pricing})
	turn := func(usage *schemas.ResponsesResponseUsage) *schemas.BifrostResponse {
		return &schemas.BifrostResponse{ResponsesResponse: &schemas.BifrostResponsesResponse{
			Usage: usage,
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType: schemas.RealtimeRequest, RoutingInfo: routingInfoFor(schemas.XAI, "grok-voice-3.0"),
			},
		}}
	}

	breakdown := s.CalculateCostBreakdown(turn(&schemas.ResponsesResponseUsage{TextMessages: bifrost.Ptr(3)}), nil)
	require.NotNil(t, breakdown, "a turn that reported only text messages still owes them")
	assert.InDelta(t, 3*0.004, breakdown.TotalCost, 1e-12)
	require.NotNil(t, breakdown.InputCostDetails)
	assert.InDelta(t, 3*0.004, breakdown.InputCostDetails.TextCost, 1e-12)

	withTokens := s.CalculateCostBreakdown(turn(&schemas.ResponsesResponseUsage{InputTokens: 100, TotalTokens: 100, TextMessages: bifrost.Ptr(2)}), nil)
	require.NotNil(t, withTokens)
	assert.InDelta(t, 100*0.000001+2*0.004, withTokens.TotalCost, 1e-12, "the message charge is added to the turn's own price")

	assert.InDelta(t, 100*0.000001, s.CalculateCost(turn(&schemas.ResponsesResponseUsage{InputTokens: 100, TotalTokens: 100}), nil), 1e-12,
		"a turn with no text messages is charged no message fee")

	// A turn that failed, or a session that closed, carries the messages on the error's
	// BilledUsage; governance and logging price it here at the same rate.
	assert.InDelta(t, 2*0.004, s.CalculateCostForUsage(&schemas.BifrostLLMUsage{TextMessages: bifrost.Ptr(2)}, schemas.XAI, "grok-voice-3.0", schemas.RealtimeRequest, nil), 1e-12,
		"the text messages of a failed turn are charged")
}

func TestCalculateCost_RealtimeTranscriptionDurationPricing(t *testing.T) {
	seconds := 3.4
	pricing := configstoreTables.TableModelPricing{
		Model:                      "whisper-1",
		Provider:                   "openai",
		Mode:                       "audio_transcription",
		InputCostPerAudioPerSecond: bifrost.Ptr(0.0001),
	}
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey(pricing.Model, pricing.Provider, pricing.Mode): pricing,
	})
	resp := &schemas.BifrostResponse{
		ResponsesResponse: &schemas.BifrostResponsesResponse{
			Usage: &schemas.ResponsesResponseUsage{AudioSeconds: &seconds},
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType:        schemas.RealtimeRequest,
				PricingRequestType: schemas.TranscriptionRequest,
				RoutingInfo:        routingInfoFor(schemas.OpenAI, "whisper-1"),
			},
		},
	}

	breakdown := s.CalculateCostBreakdown(resp, nil)
	require.NotNil(t, breakdown)
	assert.InDelta(t, 0.00034, breakdown.TotalCost, 1e-12)
	assert.InDelta(t, 0.00034, breakdown.InputCost, 1e-12)
}

func TestCalculateCost_RealtimeTranscriptionStreamDurationPricing(t *testing.T) {
	seconds := 3.4
	pricing := configstoreTables.TableModelPricing{
		Model:                      "whisper-1",
		Provider:                   "openai",
		Mode:                       "audio_transcription",
		InputCostPerAudioPerSecond: bifrost.Ptr(0.0001),
	}
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey(pricing.Model, pricing.Provider, pricing.Mode): pricing,
	})
	resp := &schemas.BifrostResponse{
		ResponsesStreamResponse: &schemas.BifrostResponsesStreamResponse{
			Response: &schemas.BifrostResponsesResponse{
				Usage: &schemas.ResponsesResponseUsage{AudioSeconds: &seconds},
			},
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType:        schemas.RealtimeRequest,
				PricingRequestType: schemas.TranscriptionRequest,
				RoutingInfo:        routingInfoFor(schemas.OpenAI, "whisper-1"),
			},
		},
	}

	breakdown := s.CalculateCostBreakdown(resp, nil)
	require.NotNil(t, breakdown)
	assert.InDelta(t, 0.00034, breakdown.TotalCost, 1e-12)
	assert.InDelta(t, 0.00034, breakdown.InputCost, 1e-12)
}

func TestCalculateCost_RealtimeTranscriptionStreamSplitTokenPricing(t *testing.T) {
	pricing := configstoreTables.TableModelPricing{
		Model:                  "gpt-4o-transcribe",
		Provider:               "openai",
		Mode:                   "audio_transcription",
		InputCostPerToken:      bifrost.Ptr(0.0000025),
		OutputCostPerToken:     bifrost.Ptr(0.00001),
		InputCostPerAudioToken: bifrost.Ptr(0.0000025),
	}
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey(pricing.Model, pricing.Provider, pricing.Mode): pricing,
	})
	resp := &schemas.BifrostResponse{
		ResponsesStreamResponse: &schemas.BifrostResponsesStreamResponse{
			Response: &schemas.BifrostResponsesResponse{
				Usage: &schemas.ResponsesResponseUsage{
					InputTokens:  29,
					OutputTokens: 14,
					TotalTokens:  43,
					InputTokensDetails: &schemas.ResponsesResponseInputTokens{
						AudioTokens: 28,
						TextTokens:  1,
					},
				},
			},
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType:        schemas.RealtimeRequest,
				PricingRequestType: schemas.TranscriptionRequest,
				RoutingInfo:        routingInfoFor(schemas.OpenAI, "gpt-4o-transcribe"),
			},
		},
	}

	breakdown := s.CalculateCostBreakdown(resp, nil)
	require.NotNil(t, breakdown)
	assert.InDelta(t, 0.0002125, breakdown.TotalCost, 1e-12)
	assert.InDelta(t, 0.00007, breakdown.InputCostDetails.AudioCost, 1e-12)
	assert.InDelta(t, 0.0000025, breakdown.InputCostDetails.TextCost, 1e-12)
	assert.InDelta(t, 0.00014, breakdown.OutputCostDetails.TextCost, 1e-12)
}

func TestCalculateCost_NormalRealtimeDoesNotUseTranscriptionPricing(t *testing.T) {
	pricing := configstoreTables.TableModelPricing{
		Model:              "gpt-realtime",
		Provider:           "openai",
		Mode:               "responses",
		InputCostPerToken:  bifrost.Ptr(0.000005),
		OutputCostPerToken: bifrost.Ptr(0.00002),
	}
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey(pricing.Model, pricing.Provider, pricing.Mode): pricing,
	})
	resp := &schemas.BifrostResponse{
		ResponsesResponse: &schemas.BifrostResponsesResponse{
			Usage: &schemas.ResponsesResponseUsage{InputTokens: 29, OutputTokens: 14, TotalTokens: 43},
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType: schemas.RealtimeRequest,
				RoutingInfo: routingInfoFor(schemas.OpenAI, "gpt-realtime"),
			},
		},
	}

	assert.InDelta(t, 0.000425, s.CalculateCost(resp, nil), 1e-12)
}

func TestCalculateCost_RealtimeTranscriptionMissingPricingIsNonFatal(t *testing.T) {
	s := testStoreWithPricing(nil)
	resp := &schemas.BifrostResponse{
		ResponsesResponse: &schemas.BifrostResponsesResponse{
			Usage: &schemas.ResponsesResponseUsage{InputTokens: 29, OutputTokens: 14, TotalTokens: 43},
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType:        schemas.RealtimeRequest,
				PricingRequestType: schemas.TranscriptionRequest,
				RoutingInfo:        routingInfoFor(schemas.OpenAI, "unknown-transcription-model"),
			},
		},
	}

	assert.Nil(t, s.CalculateCostBreakdown(resp, nil))
}

// liveVoiceWindow is the synthetic response the Live handler bills per voice window.
func liveVoiceWindow(model string, seconds float64) *schemas.BifrostResponse {
	return &schemas.BifrostResponse{
		ResponsesResponse: &schemas.BifrostResponsesResponse{
			Usage: &schemas.ResponsesResponseUsage{AudioSeconds: &seconds},
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType: schemas.LiveRequest,
				RoutingInfo: routingInfoFor(schemas.OpenAI, model),
			},
		},
	}
}

func TestCalculateCost_LiveVoiceDuration(t *testing.T) {
	pricing := configstoreTables.TableModelPricing{
		Model:              "gpt-live-1",
		Provider:           "openai",
		Mode:               "live",
		InputCostPerSecond: new(0.05 / 60),
		CostPerRequest:     new(0.01),
	}
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey(pricing.Model, pricing.Provider, pricing.Mode): pricing,
	})

	// 37s is the final usage of a recorded live session: 37 × $0.05/min.
	breakdown := s.CalculateCostBreakdown(liveVoiceWindow("gpt-live-1", 37), nil)
	require.NotNil(t, breakdown)
	assert.InDelta(t, 0.0308333333, breakdown.TotalCost, 1e-9)
	require.NotNil(t, breakdown.InputCostDetails)
	assert.InDelta(t, breakdown.TotalCost, breakdown.InputCostDetails.AudioCost, 1e-12)
	// A billing window is not a request, so the flat per-request fee is not added.
	assert.Zero(t, breakdown.InputCostDetails.RequestCost)
	assert.Zero(t, breakdown.OutputCost)
}

func TestCalculateCost_LiveFallsBackToRealtimeModeRow(t *testing.T) {
	// The datasheet feed files GPT Live models under mode "realtime", the same mode as the
	// Realtime API models, so a live window must find its per-second rate there.
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gpt-live-1", "openai", "realtime"): {
			Model: "gpt-live-1", Provider: "openai", Mode: "realtime",
			InputCostPerSecond: new(0.05 / 60),
		},
	})

	breakdown := s.CalculateCostBreakdown(liveVoiceWindow("gpt-live-1", 116), nil)
	require.NotNil(t, breakdown)
	assert.InDelta(t, 0.0966666667, breakdown.TotalCost, 1e-9)
}

func TestCalculateCost_LiveDoesNotFallBackToTokenPricing(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gpt-live-1", "openai", "responses"): {
			Model: "gpt-live-1", Provider: "openai", Mode: "responses",
			InputCostPerToken:  new(0.000005),
			InputCostPerSecond: new(1.0),
		},
	})

	assert.Nil(t, s.CalculateCostBreakdown(liveVoiceWindow("gpt-live-1", 30), nil))
}

func TestCalculateCost_LiveMissingPricingIsZero(t *testing.T) {
	s := testStoreWithPricing(nil)
	assert.Nil(t, s.CalculateCostBreakdown(liveVoiceWindow("gpt-live-1", 30), nil))

	noRate := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gpt-live-1", "openai", "live"): {Model: "gpt-live-1", Provider: "openai", Mode: "live"},
	})
	assert.Nil(t, noRate.CalculateCostBreakdown(liveVoiceWindow("gpt-live-1", 30), nil))
}

func TestCalculateCost_LiveOverrideOnlyPricing(t *testing.T) {
	s := testStoreWithPricing(nil)
	require.NoError(t, s.SetOverrides([]configstoreTables.TablePricingOverride{
		{
			ID:               "live-override",
			ScopeKind:        string(ScopeKindGlobal),
			MatchType:        string(MatchTypeExact),
			Pattern:          "gpt-live-1",
			RequestTypes:     []schemas.RequestType{schemas.LiveRequest},
			PricingPatchJSON: `{"input_cost_per_second":0.001}`,
		},
	}))

	assert.InDelta(t, 0.03, s.CalculateCost(liveVoiceWindow("gpt-live-1", 30), nil), 1e-12)
}

// chatPricing returns a TableModelPricing with the given per-token rates.
func chatPricing(input, output float64) configstoreTables.TableModelPricing {
	return configstoreTables.TableModelPricing{
		Model:              "test-model",
		Provider:           "test-provider",
		Mode:               "chat",
		InputCostPerToken:  bifrost.Ptr(input),
		OutputCostPerToken: bifrost.Ptr(output),
	}
}

// testStoreWithPricing creates a catalog pre-loaded with the given pricing entries.
func testStoreWithPricing(entries map[string]configstoreTables.TableModelPricing) *Store {
	s := newTestStore()

	for k, v := range entries {
		s.pricingData[k] = v
	}
	return s
}

// routingInfoFor builds a minimal RoutingInfo populated by core.bifrost for a
// non-aliased request — the form pricing reads from.
func routingInfoFor(provider schemas.ModelProvider, model string) schemas.RoutingInfo {
	return schemas.RoutingInfo{Provider: provider, Model: model}
}

// makeChatResponse builds a minimal BifrostResponse for a chat completion.
func makeChatResponse(provider schemas.ModelProvider, model string, usage *schemas.BifrostLLMUsage) *schemas.BifrostResponse {
	return &schemas.BifrostResponse{
		ChatResponse: &schemas.BifrostChatResponse{
			Usage: usage,
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType: schemas.ChatCompletionRequest,
				RoutingInfo: routingInfoFor(provider, model),
			},
		},
	}
}

// makeEmbeddingResponse builds a minimal BifrostResponse for an embedding request.
func makeEmbeddingResponse(provider schemas.ModelProvider, model string, usage *schemas.BifrostLLMUsage) *schemas.BifrostResponse {
	return &schemas.BifrostResponse{
		EmbeddingResponse: &schemas.BifrostEmbeddingResponse{
			Usage: usage,
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType: schemas.EmbeddingRequest,
				RoutingInfo: routingInfoFor(provider, model),
			},
		},
	}
}

// makeRerankResponse builds a minimal BifrostResponse for a rerank request.
func makeRerankResponse(provider schemas.ModelProvider, model string, usage *schemas.BifrostLLMUsage) *schemas.BifrostResponse {
	return &schemas.BifrostResponse{
		RerankResponse: &schemas.BifrostRerankResponse{
			Usage: usage,
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType: schemas.RerankRequest,
				RoutingInfo: routingInfoFor(provider, model),
			},
		},
	}
}

// makeDecisionResponse builds a minimal BifrostResponse for a decision request.
func makeDecisionResponse(provider schemas.ModelProvider, model string, usage *schemas.BifrostLLMUsage) *schemas.BifrostResponse {
	return &schemas.BifrostResponse{
		DecisionResponse: &schemas.BifrostDecisionResponse{
			Usage: usage,
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType: schemas.DecisionRequest,
				RoutingInfo: routingInfoFor(provider, model),
			},
		},
	}
}

// makeImageResponse builds a minimal BifrostResponse for an image generation request.
func makeImageResponse(provider schemas.ModelProvider, model string, usage *schemas.ImageUsage) *schemas.BifrostResponse {
	return &schemas.BifrostResponse{
		ImageGenerationResponse: &schemas.BifrostImageGenerationResponse{
			Usage: usage,
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType: schemas.ImageGenerationRequest,
				RoutingInfo: routingInfoFor(provider, model),
			},
		},
	}
}

func derefF(f *float64) float64 {
	if f == nil {
		return 0
	}
	return *f
}

// The compute* functions return a per-category cost breakdown
// (*schemas.BifrostCost). These *Total wrappers return just the aggregate cost
// for the many unit tests that assert on the total; tests that care about the
// input/output/cache split assert on the breakdown object directly.
func bcTotal(bd *schemas.BifrostCost) float64 {
	if bd == nil {
		return 0
	}
	return bd.TotalCost
}

func computeTextCostTotal(pricing *configstoreTables.TableModelPricing, usage *schemas.BifrostLLMUsage, tier serviceTier) float64 {
	return bcTotal(computeTextCost(pricing, usage, tier))
}

func computeEmbeddingCostTotal(pricing *configstoreTables.TableModelPricing, usage *schemas.BifrostLLMUsage, tier serviceTier) float64 {
	return bcTotal(computeEmbeddingCost(pricing, usage, tier))
}

func computeRerankCostTotal(pricing *configstoreTables.TableModelPricing, usage *schemas.BifrostLLMUsage, tier serviceTier) float64 {
	return bcTotal(computeRerankCost(pricing, usage, tier))
}

func computeDecisionCostTotal(pricing *configstoreTables.TableModelPricing, usage *schemas.BifrostLLMUsage, tier serviceTier) float64 {
	return bcTotal(computeDecisionCost(pricing, usage, tier))
}

func computeSpeechCostTotal(pricing *configstoreTables.TableModelPricing, usage *schemas.BifrostLLMUsage, audioSeconds *float64, audioTextInputChars int, tier serviceTier) float64 {
	return bcTotal(computeSpeechCost(pricing, usage, audioSeconds, audioTextInputChars, tier))
}

func computeTranscriptionCostTotal(pricing *configstoreTables.TableModelPricing, usage *schemas.BifrostLLMUsage, audioSeconds *float64, audioTokenDetails *schemas.TranscriptionUsageInputTokenDetails, tier serviceTier) float64 {
	return bcTotal(computeTranscriptionCost(pricing, usage, audioSeconds, audioTokenDetails, tier))
}

func computeImageCostTotal(pricing *configstoreTables.TableModelPricing, imageUsage *schemas.ImageUsage, imageSize string, imageQuality string, tier serviceTier) float64 {
	return bcTotal(computeImageCost(pricing, imageUsage, imageRequestShape{size: imageSize, quality: imageQuality}, tier))
}

func computeVideoCostTotal(pricing *configstoreTables.TableModelPricing, usage *schemas.BifrostLLMUsage, videoSeconds *int, tier serviceTier) float64 {
	return bcTotal(computeVideoCost(pricing, usage, videoSeconds, "", 0, false, videoJob{}, tier))
}

func computeVideoCostTotalSized(pricing *configstoreTables.TableModelPricing, usage *schemas.BifrostLLMUsage, videoSeconds *int, videoSize string, videoCount int, tier serviceTier) float64 {
	return bcTotal(computeVideoCost(pricing, usage, videoSeconds, videoSize, videoCount, false, videoJob{}, tier))
}

func computeContainerCreationCostTotal(pricing *configstoreTables.TableModelPricing) float64 {
	return bcTotal(computeContainerCreationCost(pricing))
}

// =========================================================================
// 1. computeTextCost — unit tests (pure function, no catalog)
// =========================================================================

func TestComputeTextCost_BasicInputOutput(t *testing.T) {
	// GPT-4o: $5/M input, $15/M output
	p := chatPricing(0.000005, 0.000015)
	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     1000,
		CompletionTokens: 500,
		TotalTokens:      1500,
	}
	cost := computeTextCostTotal(&p, usage, serviceTier{})
	// 1000 * 0.000005 + 500 * 0.000015 = 0.005 + 0.0075 = 0.0125
	assert.InDelta(t, 0.0125, cost, 1e-12)
}

func TestComputeTextCost_NilUsage(t *testing.T) {
	p := chatPricing(0.000005, 0.000015)
	assert.Equal(t, 0.0, computeTextCostTotal(&p, nil, serviceTier{}))
}

func TestComputeTextCost_ZeroTokens(t *testing.T) {
	p := chatPricing(0.000005, 0.000015)
	usage := &schemas.BifrostLLMUsage{}
	assert.Equal(t, 0.0, computeTextCostTotal(&p, usage, serviceTier{}))
}

func TestComputeTextCost_WithCachedPromptTokens(t *testing.T) {
	// Claude 3.5 Sonnet (Bedrock): input=$3/M, output=$15/M, cache_read=$0.3/M, cache_creation=$3.75/M
	p := chatPricing(0.000003, 0.000015)
	p.CacheReadInputTokenCost = bifrost.Ptr(0.0000003)
	p.CacheCreationInputTokenCost = bifrost.Ptr(0.00000375)

	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     2000,
		CompletionTokens: 500,
		TotalTokens:      2500,
		PromptTokensDetails: &schemas.ChatPromptTokensDetails{
			CachedReadTokens:  1500, // 1500 read from cache
			CachedWriteTokens: 200,  // 200 cache creation tokens
		},
	}

	cost := computeTextCostTotal(&p, usage, serviceTier{})

	// Both cached read and write tokens are input-side deductions from promptTokens.
	// Input: (2000-1500-200)*0.000003 + 1500*0.0000003 + 200*0.00000375 = 0.0009 + 0.00045 + 0.00075 = 0.0021
	// Output: 500*0.000015 = 0.0075
	// Total: 0.0021 + 0.0075 = 0.0096
	assert.InDelta(t, 0.0096, cost, 1e-12)
}

// gpt56SolPricing returns the full tiered pricing for gpt-5.6-sol (per the OpenAI
// pricing page), including flex/priority and the 272k context tier used to
// exercise the cache-write (cache-creation) tiering added with gpt-5.6.
func gpt56SolPricing() configstoreTables.TableModelPricing {
	p := chatPricing(0.000005, 0.00003) // standard input $5/M, output $30/M
	p.InputCostPerTokenAbove272kTokens = bifrost.Ptr(0.00001)
	p.InputCostPerTokenFlex = bifrost.Ptr(0.0000025)
	p.InputCostPerTokenFlexAbove272kTokens = bifrost.Ptr(0.000005)
	p.InputCostPerTokenPriority = bifrost.Ptr(0.00001)
	p.OutputCostPerTokenAbove272kTokens = bifrost.Ptr(0.000045)
	p.OutputCostPerTokenFlex = bifrost.Ptr(0.000015)
	p.OutputCostPerTokenFlexAbove272kTokens = bifrost.Ptr(0.0000225)
	p.OutputCostPerTokenPriority = bifrost.Ptr(0.00006)
	p.CacheReadInputTokenCost = bifrost.Ptr(0.0000005)
	p.CacheReadInputTokenCostAbove272kTokens = bifrost.Ptr(0.000001)
	p.CacheReadInputTokenCostFlex = bifrost.Ptr(0.00000025)
	p.CacheReadInputTokenCostFlexAbove272kTokens = bifrost.Ptr(0.0000005)
	p.CacheReadInputTokenCostPriority = bifrost.Ptr(0.000001)
	p.CacheCreationInputTokenCost = bifrost.Ptr(0.00000625)
	p.CacheCreationInputTokenCostAbove272kTokens = bifrost.Ptr(0.0000125)
	p.CacheCreationInputTokenCostFlex = bifrost.Ptr(0.000003125)
	p.CacheCreationInputTokenCostFlexAbove272kTokens = bifrost.Ptr(0.00000625)
	p.CacheCreationInputTokenCostPriority = bifrost.Ptr(0.0000125)
	return p
}

// The reported bug scenario: a fresh-cache gpt-5.6-sol turn now returns and bills
// cache-write tokens at the base cache-creation rate.
func TestComputeTextCost_GPT56_StandardCacheWrite(t *testing.T) {
	p := gpt56SolPricing()
	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     28006,
		CompletionTokens: 443,
		TotalTokens:      28449,
		PromptTokensDetails: &schemas.ChatPromptTokensDetails{
			CachedReadTokens:  0,
			CachedWriteTokens: 28003,
		},
	}
	cost := computeTextCostTotal(&p, usage, serviceTier{})
	// input: (28006-28003)*0.000005 = 0.000015
	// write: 28003*0.00000625 = 0.17501875
	// output: 443*0.00003 = 0.01329
	assert.InDelta(t, 0.000015+0.17501875+0.01329, cost, 1e-9)
}

func TestComputeTextCost_GPT56_StandardCacheWriteAbove272k(t *testing.T) {
	p := gpt56SolPricing()
	usage := &schemas.BifrostLLMUsage{
		PromptTokens:        300000,
		CompletionTokens:    1000,
		TotalTokens:         301000,
		PromptTokensDetails: &schemas.ChatPromptTokensDetails{CachedWriteTokens: 100000},
	}
	cost := computeTextCostTotal(&p, usage, serviceTier{})
	// input: 200000*0.00001 = 2.0; write: 100000*0.0000125 = 1.25; output: 1000*0.000045 = 0.045
	assert.InDelta(t, 2.0+1.25+0.045, cost, 1e-9)
}

func TestComputeTextCost_GPT56_FlexTier(t *testing.T) {
	p := gpt56SolPricing()
	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     10000,
		CompletionTokens: 1000,
		TotalTokens:      11000,
		PromptTokensDetails: &schemas.ChatPromptTokensDetails{
			CachedReadTokens:  4000,
			CachedWriteTokens: 2000,
		},
	}
	cost := computeTextCostTotal(&p, usage, serviceTier{isFlex: true})
	// input: 4000*0.0000025 = 0.01; read: 4000*0.00000025 = 0.001
	// write: 2000*0.000003125 = 0.00625; output: 1000*0.000015 = 0.015
	assert.InDelta(t, 0.01+0.001+0.00625+0.015, cost, 1e-12)
}

func TestComputeTextCost_GPT56_FlexTierAbove272k(t *testing.T) {
	p := gpt56SolPricing()
	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     300000,
		CompletionTokens: 1000,
		TotalTokens:      301000,
		PromptTokensDetails: &schemas.ChatPromptTokensDetails{
			CachedReadTokens:  100000,
			CachedWriteTokens: 50000,
		},
	}
	cost := computeTextCostTotal(&p, usage, serviceTier{isFlex: true})
	// input: 150000*0.000005 = 0.75; read: 100000*0.0000005 = 0.05
	// write: 50000*0.00000625 = 0.3125; output: 1000*0.0000225 = 0.0225
	assert.InDelta(t, 0.75+0.05+0.3125+0.0225, cost, 1e-9)
}

func TestComputeTextCost_GPT56_PriorityTier(t *testing.T) {
	p := gpt56SolPricing()
	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     10000,
		CompletionTokens: 1000,
		TotalTokens:      11000,
		PromptTokensDetails: &schemas.ChatPromptTokensDetails{
			CachedReadTokens:  4000,
			CachedWriteTokens: 2000,
		},
	}
	cost := computeTextCostTotal(&p, usage, serviceTier{isPriority: true})
	// input: 4000*0.00001 = 0.04; read: 4000*0.000001 = 0.004
	// write: 2000*0.0000125 = 0.025; output: 1000*0.00006 = 0.06
	assert.InDelta(t, 0.04+0.004+0.025+0.06, cost, 1e-12)
}

// Regression: a flex model without a flex-272k column keeps the flat flex rate
// above 272k (no new tiering leaks into existing flex models).
func TestComputeTextCost_FlexFlatAbove272kWhenNoFlexTierColumn(t *testing.T) {
	p := chatPricing(0.000005, 0.00003)
	p.InputCostPerTokenFlex = bifrost.Ptr(0.0000025)
	usage := &schemas.BifrostLLMUsage{PromptTokens: 300000, TotalTokens: 300000}
	cost := computeTextCostTotal(&p, usage, serviceTier{isFlex: true})
	assert.InDelta(t, 300000*0.0000025, cost, 1e-9)
}

// TestComputeTextCost_Breakdown asserts the per-category split (input / output /
// cache) that computeTextCost now returns, with cache reads as a subset of input.
func TestComputeTextCost_Breakdown(t *testing.T) {
	p := chatPricing(0.000003, 0.000015)
	p.CacheReadInputTokenCost = bifrost.Ptr(0.0000003)
	p.CacheCreationInputTokenCost = bifrost.Ptr(0.00000375)

	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     2000,
		CompletionTokens: 500,
		TotalTokens:      2500,
		PromptTokensDetails: &schemas.ChatPromptTokensDetails{
			CachedReadTokens:  1500,
			CachedWriteTokens: 200,
		},
	}

	bd := computeTextCost(&p, usage, serviceTier{})
	require.NotNil(t, bd)

	// Input: (2000-1500-200)*3e-6 + 1500*3e-7 + 200*3.75e-6 = 0.0009 + 0.00045 + 0.00075 = 0.0021
	assert.InDelta(t, 0.0021, bd.InputCost, 1e-12)
	// Output: 500*1.5e-5 = 0.0075
	assert.InDelta(t, 0.0075, bd.OutputCost, 1e-12)
	// Cache read/write are input sub-categories surfaced in the details.
	require.NotNil(t, bd.InputCostDetails)
	assert.InDelta(t, 0.00045, bd.InputCostDetails.CachedReadCost, 1e-12)  // 1500*3e-7
	assert.InDelta(t, 0.00075, bd.InputCostDetails.CachedWriteCost, 1e-12) // 200*3.75e-6
	// Detail components sum back to the input total.
	assert.InDelta(t, bd.InputCost,
		bd.InputCostDetails.TextCost+bd.InputCostDetails.CachedReadCost+bd.InputCostDetails.CachedWriteCost+bd.InputCostDetails.AudioCost, 1e-12)
	// Total is input + output.
	assert.InDelta(t, 0.0096, bd.TotalCost, 1e-12)
	assert.InDelta(t, bd.InputCost+bd.OutputCost, bd.TotalCost, 1e-12)
}

// TestComputeTextCost_AudioBreakdown asserts audio tokens are charged in full at
// the audio rate under AudioCost (never a negative rate delta), TextCost keeps
// only non-audio tokens, and the side totals are unchanged whether the audio
// rate is above or below the text rate.
func TestComputeTextCost_AudioBreakdown(t *testing.T) {
	const inputRate, outputRate = 0.000003, 0.000015
	usage := &schemas.BifrostLLMUsage{
		PromptTokens:            2000,
		CompletionTokens:        400,
		TotalTokens:             2400,
		PromptTokensDetails:     &schemas.ChatPromptTokensDetails{AudioTokens: 500},
		CompletionTokensDetails: &schemas.ChatCompletionTokensDetails{AudioTokens: 100},
	}

	t.Run("audio rate above text rate", func(t *testing.T) {
		p := chatPricing(inputRate, outputRate)
		p.InputCostPerAudioToken = bifrost.Ptr(0.00001)
		p.OutputCostPerAudioToken = bifrost.Ptr(0.00002)

		bd := computeTextCost(&p, usage, serviceTier{})
		require.NotNil(t, bd)
		require.NotNil(t, bd.InputCostDetails)
		require.NotNil(t, bd.OutputCostDetails)

		// Input: 1500 text @ 3e-6 + 500 audio @ 1e-5.
		assert.InDelta(t, 0.0045, bd.InputCostDetails.TextCost, 1e-12)
		assert.InDelta(t, 0.005, bd.InputCostDetails.AudioCost, 1e-12)
		assert.InDelta(t, 0.0095, bd.InputCost, 1e-12)
		// Output: 300 text @ 1.5e-5 + 100 audio @ 2e-5.
		assert.InDelta(t, 0.0045, bd.OutputCostDetails.TextCost, 1e-12)
		assert.InDelta(t, 0.002, bd.OutputCostDetails.AudioCost, 1e-12)
		assert.InDelta(t, 0.0065, bd.OutputCost, 1e-12)
		// Details reconcile with the side totals.
		assert.InDelta(t, bd.InputCost, bd.InputCostDetails.TextCost+bd.InputCostDetails.AudioCost, 1e-12)
		assert.InDelta(t, bd.OutputCost, bd.OutputCostDetails.TextCost+bd.OutputCostDetails.AudioCost, 1e-12)
	})

	t.Run("audio rate below text rate stays non-negative", func(t *testing.T) {
		p := chatPricing(inputRate, outputRate)
		p.InputCostPerAudioToken = bifrost.Ptr(0.000001)  // below input text rate
		p.OutputCostPerAudioToken = bifrost.Ptr(0.000005) // below output text rate

		bd := computeTextCost(&p, usage, serviceTier{})
		require.NotNil(t, bd)
		require.NotNil(t, bd.InputCostDetails)
		require.NotNil(t, bd.OutputCostDetails)

		// AudioCost is the full audio-token charge, never a negative delta.
		assert.InDelta(t, 0.0005, bd.InputCostDetails.AudioCost, 1e-12) // 500 * 1e-6
		assert.InDelta(t, 0.0045, bd.InputCostDetails.TextCost, 1e-12)  // 1500 * 3e-6
		assert.GreaterOrEqual(t, bd.InputCostDetails.AudioCost, 0.0)
		assert.GreaterOrEqual(t, bd.OutputCostDetails.AudioCost, 0.0)
		// Side totals match the delta-based aggregate they replaced.
		assert.InDelta(t, 0.005, bd.InputCost, 1e-12) // 2000*3e-6 + 500*(1e-6 - 3e-6)
		assert.InDelta(t, bd.InputCost, bd.InputCostDetails.TextCost+bd.InputCostDetails.AudioCost, 1e-12)
		assert.InDelta(t, bd.OutputCost, bd.OutputCostDetails.TextCost+bd.OutputCostDetails.AudioCost, 1e-12)
	})

	t.Run("audio overlapping cached tokens keeps TextCost non-negative", func(t *testing.T) {
		// Audio (500) exceeds the non-cached prompt (1000-700=300). Without the
		// clamp, textInputCost would subtract 500 tokens it never held and go
		// negative, understating InputCost/TotalCost.
		u := &schemas.BifrostLLMUsage{
			PromptTokens: 1000,
			TotalTokens:  1000,
			PromptTokensDetails: &schemas.ChatPromptTokensDetails{
				CachedReadTokens: 700,
				AudioTokens:      500,
			},
		}
		p := chatPricing(inputRate, outputRate)
		p.InputCostPerAudioToken = bifrost.Ptr(0.00001)
		p.CacheReadInputTokenCost = bifrost.Ptr(0.000001)

		bd := computeTextCost(&p, u, serviceTier{})
		require.NotNil(t, bd)
		require.NotNil(t, bd.InputCostDetails)

		// Audio clamps to the 300 non-cached tokens, so text drops to 0 (float
		// subtraction leaves only rounding noise, never the -6e-4 the bug produced).
		assert.GreaterOrEqual(t, bd.InputCostDetails.TextCost, -1e-12)
		assert.InDelta(t, 0.0, bd.InputCostDetails.TextCost, 1e-12)          // 300*3e-6 - 300*3e-6
		assert.InDelta(t, 0.003, bd.InputCostDetails.AudioCost, 1e-12)       // 300 * 1e-5
		assert.InDelta(t, 0.0007, bd.InputCostDetails.CachedReadCost, 1e-12) // 700 * 1e-6
		assert.InDelta(t, 0.0037, bd.InputCost, 1e-12)
		assert.InDelta(t, bd.InputCost,
			bd.InputCostDetails.TextCost+bd.InputCostDetails.AudioCost+bd.InputCostDetails.CachedReadCost, 1e-12)
	})
}

func TestComputeTextCost_FastMode(t *testing.T) {
	// Opus 4.8: standard $5/$25, fast $10/$50 per MTok.
	p := chatPricing(0.000005, 0.000025)
	p.InputCostPerTokenFast = bifrost.Ptr(0.00001)
	p.OutputCostPerTokenFast = bifrost.Ptr(0.00005)

	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     1000,
		CompletionTokens: 500,
		TotalTokens:      1500,
	}

	// Standard speed → standard rates.
	standard := computeTextCostTotal(&p, usage, serviceTier{})
	assert.InDelta(t, 1000*0.000005+500*0.000025, standard, 1e-12)

	// Fast speed → fast rates.
	fast := computeTextCostTotal(&p, usage, serviceTier{isFast: true})
	assert.InDelta(t, 1000*0.00001+500*0.00005, fast, 1e-12)
}

func TestComputeTextCost_FastMode_FlatAcrossContextWindow(t *testing.T) {
	// Fast mode is flat across the full window — it must ignore the 200k tier rate.
	p := chatPricing(0.000005, 0.000025)
	p.InputCostPerTokenFast = bifrost.Ptr(0.00001)
	p.OutputCostPerTokenFast = bifrost.Ptr(0.00005)
	p.InputCostPerTokenAbove200kTokens = bifrost.Ptr(0.0000075)
	p.OutputCostPerTokenAbove200kTokens = bifrost.Ptr(0.0000375)

	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     250000,
		CompletionTokens: 1000,
		TotalTokens:      251000, // above the 200k tier
	}

	fast := computeTextCostTotal(&p, usage, serviceTier{isFast: true})
	// Flat fast rate, not the above-200k rate.
	assert.InDelta(t, 250000*0.00001+1000*0.00005, fast, 1e-9)
}

func TestComputeTextCost_FastMode_FallsBackWhenUnconfigured(t *testing.T) {
	// Model without fast columns (e.g. non-Opus) → fast flag is a no-op, standard rates apply.
	p := chatPricing(0.000005, 0.000025)
	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     1000,
		CompletionTokens: 500,
		TotalTokens:      1500,
	}
	fast := computeTextCostTotal(&p, usage, serviceTier{isFast: true})
	assert.InDelta(t, 1000*0.000005+500*0.000025, fast, 1e-12)
}

func TestComputeTextCost_FastMode_UsesFastCacheRates(t *testing.T) {
	// Fast mode has dedicated cache columns; when set, cache tokens bill at the
	// _fast rate, not the standard rate.
	p := chatPricing(0.000005, 0.000025)
	p.InputCostPerTokenFast = bifrost.Ptr(0.00001)
	p.OutputCostPerTokenFast = bifrost.Ptr(0.00005)
	p.CacheReadInputTokenCost = bifrost.Ptr(0.0000005)         // standard read (ignored in fast)
	p.CacheCreationInputTokenCost = bifrost.Ptr(0.00000625)    // standard 5m write (ignored in fast)
	p.CacheReadInputTokenCostFast = bifrost.Ptr(0.000001)      // fast read
	p.CacheCreationInputTokenCostFast = bifrost.Ptr(0.0000125) // fast 5m write

	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     2000,
		CompletionTokens: 500,
		TotalTokens:      2500,
		PromptTokensDetails: &schemas.ChatPromptTokensDetails{
			CachedReadTokens:  1500,
			CachedWriteTokens: 200,
		},
	}

	fast := computeTextCostTotal(&p, usage, serviceTier{isFast: true})
	// non-cached 300*fast + read 1500*fastRead + write 200*fastWrite + output 500*fast
	expected := 300*0.00001 + 1500*0.000001 + 200*0.0000125 + 500*0.00005
	assert.InDelta(t, expected, fast, 1e-12)
}

func TestComputeTextCost_FastMode_CacheFallsBackToStandardWhenFastUnset(t *testing.T) {
	// When the _fast cache columns are absent, cache tokens fall back to standard
	// cache rates (mirrors the input/output fast fallback) — the flag is a no-op.
	p := chatPricing(0.000005, 0.000025)
	p.InputCostPerTokenFast = bifrost.Ptr(0.00001)
	p.OutputCostPerTokenFast = bifrost.Ptr(0.00005)
	p.CacheReadInputTokenCost = bifrost.Ptr(0.0000005)
	p.CacheCreationInputTokenCost = bifrost.Ptr(0.00000625)

	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     2000,
		CompletionTokens: 500,
		TotalTokens:      2500,
		PromptTokensDetails: &schemas.ChatPromptTokensDetails{
			CachedReadTokens:  1500,
			CachedWriteTokens: 200,
		},
	}

	fast := computeTextCostTotal(&p, usage, serviceTier{isFast: true})
	// input/output use fast; cache uses standard.
	expected := 300*0.00001 + 1500*0.0000005 + 200*0.00000625 + 500*0.00005
	assert.InDelta(t, expected, fast, 1e-12)
}

// TestComputeTextCost_FastMode_Opus48CacheRegression pins the reported real-world
// miscalculation: fast mode + cache_control billed cache creation at the standard
// rate. Opus 4.8: fast $10/$50, fast 5m cache write $12.50 per MTok.
func TestComputeTextCost_FastMode_Opus48CacheRegression(t *testing.T) {
	p := chatPricing(0.000005, 0.000025)
	p.InputCostPerTokenFast = bifrost.Ptr(0.00001)
	p.OutputCostPerTokenFast = bifrost.Ptr(0.00005)
	p.CacheCreationInputTokenCost = bifrost.Ptr(0.00000625)    // standard 5m write (ignored in fast)
	p.CacheCreationInputTokenCostFast = bifrost.Ptr(0.0000125) // fast 5m write

	// input_tokens=2, cache_creation=44667 (all 5m), output=135. PromptTokens
	// carries the cache-creation tokens (Anthropic responses usage mapping).
	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     44669,
		CompletionTokens: 135,
		TotalTokens:      44804,
		PromptTokensDetails: &schemas.ChatPromptTokensDetails{
			CachedWriteTokens: 44667,
			CachedWriteTokenDetails: &schemas.ChatCachedWriteTokenDetails{
				CachedWriteTokens5m: 44667,
			},
		},
	}

	fast := computeTextCostTotal(&p, usage, serviceTier{isFast: true})
	// 2*$10/M + 44667*$12.50/M (fast 5m cache) + 135*$50/M = $0.565108
	expected := 2*0.00001 + 44667*0.0000125 + 135*0.00005
	assert.InDelta(t, expected, fast, 1e-9)
}

// TestComputeTextCost_InferenceGeoUS_AppliesMultiplier verifies the Anthropic
// data-residency multiplier (inference_geo:"us") scales every token/cache cost by
// 1.1x while leaving the flat per-search fee untouched.
func TestComputeTextCost_InferenceGeoUS_AppliesMultiplier(t *testing.T) {
	p := chatPricing(0.00001, 0.00005)
	p.CacheReadInputTokenCost = bifrost.Ptr(0.000001)
	p.CacheCreationInputTokenCost = bifrost.Ptr(0.0000125)
	p.WebSearchCostPerRequest = bifrost.Ptr(0.01)
	p.InferenceGeoUSMultiplier = bifrost.Ptr(1.1)

	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     1000, // 500 non-cached + 200 read + 300 write
		CompletionTokens: 100,
		PromptTokensDetails: &schemas.ChatPromptTokensDetails{
			CachedReadTokens:  200,
			CachedWriteTokens: 300,
		},
		ToolUsage: &schemas.ToolUsage{WebSearch: &schemas.WebSearchToolUsage{NumRequests: 2}},
	}

	tokenCost := 500*0.00001 + 200*0.000001 + 300*0.0000125 + 100*0.00005
	searchCost := 2 * 0.01

	got := computeTextCostTotal(&p, usage, serviceTier{inferenceGeoUS: true})
	assert.InDelta(t, tokenCost*1.1+searchCost, got, 1e-9)

	// Without US residency the multiplier is a no-op; the search fee is identical.
	base := computeTextCostTotal(&p, usage, serviceTier{})
	assert.InDelta(t, tokenCost+searchCost, base, 1e-9)
}

// TestComputeTextCost_InferenceGeoUS_NoMultiplierColumn verifies US residency is a
// safe no-op until the datasheet populates the multiplier column upstream.
func TestComputeTextCost_InferenceGeoUS_NoMultiplierColumn(t *testing.T) {
	p := chatPricing(0.00001, 0.00005)
	usage := &schemas.BifrostLLMUsage{PromptTokens: 1000, CompletionTokens: 100}
	withUS := computeTextCostTotal(&p, usage, serviceTier{inferenceGeoUS: true})
	without := computeTextCostTotal(&p, usage, serviceTier{})
	assert.InDelta(t, without, withUS, 1e-9)
}

func TestTierFromResponse_Speed(t *testing.T) {
	assert.False(t, tierFromResponse(nil, nil, nil).isFast)
	assert.False(t, tierFromResponse(nil, bifrost.Ptr("standard"), nil).isFast)
	assert.True(t, tierFromResponse(nil, bifrost.Ptr("fast"), nil).isFast)
}

func TestTierFromResponse_InferenceGeo(t *testing.T) {
	assert.False(t, tierFromResponse(nil, nil, nil).inferenceGeoUS)
	assert.False(t, tierFromResponse(nil, nil, bifrost.Ptr("global")).inferenceGeoUS)
	assert.True(t, tierFromResponse(nil, nil, bifrost.Ptr("us")).inferenceGeoUS)
	assert.True(t, tierFromResponse(nil, nil, bifrost.Ptr("US")).inferenceGeoUS)
}

func TestComputeTextCost_With1hrCacheCreationTokens(t *testing.T) {
	// claude-3-5-sonnet-20241022-v2:0 on Bedrock:
	// input=$3/M, output=$15/M, cache_creation=$3.75/M, cache_creation_1hr=$7.50/M, cache_read=$0.3/M
	p := chatPricing(0.000003, 0.000015)
	p.CacheReadInputTokenCost = bifrost.Ptr(3e-7)
	p.CacheCreationInputTokenCost = bifrost.Ptr(0.00000375)
	p.CacheCreationInputTokenCostAbove1hr = bifrost.Ptr(0.0000075)

	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     2000,
		CompletionTokens: 500,
		TotalTokens:      2500,
		PromptTokensDetails: &schemas.ChatPromptTokensDetails{
			CachedWriteTokens: 1000,
			CachedWriteTokenDetails: &schemas.ChatCachedWriteTokenDetails{
				CachedWriteTokens1h: 600, // 600 at 1hr rate, 400 at standard rate
			},
		},
	}

	cost := computeTextCostTotal(&p, usage, serviceTier{})

	// Input (non-cached): (2000-1000)*0.000003 = 0.003
	// Cache creation (1hr): 600*0.0000075 = 0.0045
	// Cache creation (standard): 400*0.00000375 = 0.0015
	// Output: 500*0.000015 = 0.0075
	// Total: 0.003 + 0.0045 + 0.0015 + 0.0075 = 0.0165
	assert.InDelta(t, 0.0165, cost, 1e-12)
}

// TXE-6531/TXE-6959: OpenAI reports its cache writes as a 30-minute and a 12-hour tier. The 30m
// tier is OpenAI's base cache write (cache_creation_input_token_cost, 1.25x input); the 12h tier
// is longer than an hour and takes cache_creation_input_token_cost_above_1hr (2.5x input).
func TestComputeTextCost_OpenAI30mAnd12hCacheWriteTiers(t *testing.T) {
	p := chatPricing(0.000001, 0.000008)
	p.CacheCreationInputTokenCost = bifrost.Ptr(0.00000125)
	p.CacheCreationInputTokenCostAbove1hr = bifrost.Ptr(0.0000025)

	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     10000,
		CompletionTokens: 100,
		TotalTokens:      10100,
		PromptTokensDetails: &schemas.ChatPromptTokensDetails{
			CachedWriteTokens: 8000,
			CachedWriteTokenDetails: &schemas.ChatCachedWriteTokenDetails{
				CachedWriteTokens30m: 6000,
				CachedWriteTokens12h: 2000,
			},
		},
	}

	// Input (non-cached): 2000*1e-6 = 0.002
	// Cache write 30m (base rate): 6000*1.25e-6 = 0.0075
	// Cache write 12h (above-1hr rate): 2000*2.5e-6 = 0.005
	// Output: 100*8e-6 = 0.0008
	assert.InDelta(t, 0.0153, computeTextCostTotal(&p, usage, serviceTier{}), 1e-12)

	// Without an above-1hr rate the 12h tier falls back to the base cache-write rate, as the
	// 1h tier does.
	p.CacheCreationInputTokenCostAbove1hr = nil
	assert.InDelta(t, 0.002+8000*0.00000125+0.0008, computeTextCostTotal(&p, usage, serviceTier{}), 1e-12)
}

func TestComputeTextCost_StandardCacheCreationPricingLesserThan1hr(t *testing.T) {
	// Standard (5-min TTL) cache creation is cheaper than 1hr TTL cache creation.
	// 1hr rate ($7.50/M) is 2x the standard rate ($3.75/M).
	p := chatPricing(0.000003, 0.000015)
	p.CacheCreationInputTokenCost = bifrost.Ptr(0.00000375)
	p.CacheCreationInputTokenCostAbove1hr = bifrost.Ptr(0.0000075)

	base := &schemas.BifrostLLMUsage{
		PromptTokens:     2000,
		CompletionTokens: 500,
		TotalTokens:      2500,
	}

	usageStandard := *base
	usageStandard.PromptTokensDetails = &schemas.ChatPromptTokensDetails{
		CachedWriteTokens: 1000,
	}

	usage1hr := *base
	usage1hr.PromptTokensDetails = &schemas.ChatPromptTokensDetails{
		CachedWriteTokens: 1000,
		CachedWriteTokenDetails: &schemas.ChatCachedWriteTokenDetails{
			CachedWriteTokens1h: 1000, // all 1000 tokens at 1hr rate
		},
	}

	costStandard := computeTextCostTotal(&p, &usageStandard, serviceTier{})
	cost1hr := computeTextCostTotal(&p, &usage1hr, serviceTier{})

	assert.Less(t, costStandard, cost1hr, "standard cache creation should cost less than 1hr cache creation")
}

func TestComputeTextCost_1hrCacheCreationFallsBackToStandardWhenAbove1hrRateAbsent(t *testing.T) {
	// claude-3-5-haiku on Bedrock has no cache_creation_input_token_cost_above_1hr entry.
	// Tokens marked as 1hr cache writes must fall back to the standard cache creation rate.
	p := chatPricing(8e-7, 0.000004)
	p.CacheReadInputTokenCost = bifrost.Ptr(8e-8)
	p.CacheCreationInputTokenCost = bifrost.Ptr(0.000001)
	// CacheCreationInputTokenCostAbove1hr intentionally left nil

	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     2000,
		CompletionTokens: 500,
		TotalTokens:      2500,
		PromptTokensDetails: &schemas.ChatPromptTokensDetails{
			CachedWriteTokens: 1000,
			CachedWriteTokenDetails: &schemas.ChatCachedWriteTokenDetails{
				CachedWriteTokens1h: 1000, // all 1hr tokens, but no above_1hr rate configured
			},
		},
	}

	cost := computeTextCostTotal(&p, usage, serviceTier{})

	// Input (non-cached): (2000-1000)*8e-7 = 0.0008
	// Cache creation (1hr fallback → standard 0.000001): 1000*0.000001 = 0.001
	// Output: 500*0.000004 = 0.002
	// Total: 0.0008 + 0.001 + 0.002 = 0.0038
	assert.InDelta(t, 0.0038, cost, 1e-12)
}

func TestComputeTextCost_CacheWriteTokenDetailsNil_FallsBackToStandardCreationRate(t *testing.T) {
	// CachedWriteTokens is set but CachedWriteTokenDetails is nil.
	// All write tokens must use the standard cache creation rate even though above_1hr is configured.
	p := chatPricing(0.000003, 0.000015)
	p.CacheCreationInputTokenCost = bifrost.Ptr(0.00000375)
	p.CacheCreationInputTokenCostAbove1hr = bifrost.Ptr(0.0000075) // present but must not be used

	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     2000,
		CompletionTokens: 500,
		TotalTokens:      2500,
		PromptTokensDetails: &schemas.ChatPromptTokensDetails{
			CachedWriteTokens:       1000,
			CachedWriteTokenDetails: nil,
		},
	}

	cost := computeTextCostTotal(&p, usage, serviceTier{})

	// Input (non-cached): (2000-1000)*0.000003 = 0.003
	// Cache creation (standard, no 1hr details): 1000*0.00000375 = 0.00375
	// Output: 500*0.000015 = 0.0075
	// Total: 0.003 + 0.00375 + 0.0075 = 0.01425
	assert.InDelta(t, 0.01425, cost, 1e-12)
}

func TestComputeTextCost_CacheWriteTokenDetails1hZero_FallsBackToStandardCreationRate(t *testing.T) {
	// CachedWriteTokenDetails is present but CachedWriteTokens1h is 0 (e.g. all tokens
	// used 5-min TTL). All write tokens must use the standard cache creation rate.
	p := chatPricing(0.000003, 0.000015)
	p.CacheCreationInputTokenCost = bifrost.Ptr(0.00000375)
	p.CacheCreationInputTokenCostAbove1hr = bifrost.Ptr(0.0000075) // present but must not be used

	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     2000,
		CompletionTokens: 500,
		TotalTokens:      2500,
		PromptTokensDetails: &schemas.ChatPromptTokensDetails{
			CachedWriteTokens: 1000,
			CachedWriteTokenDetails: &schemas.ChatCachedWriteTokenDetails{
				CachedWriteTokens1h: 0,
			},
		},
	}

	cost := computeTextCostTotal(&p, usage, serviceTier{})

	// Input (non-cached): (2000-1000)*0.000003 = 0.003
	// Cache creation (standard, 1h count is 0): 1000*0.00000375 = 0.00375
	// Output: 500*0.000015 = 0.0075
	// Total: 0.003 + 0.00375 + 0.0075 = 0.01425
	assert.InDelta(t, 0.01425, cost, 1e-12)
}

func TestComputeTextCost_1hrCacheCreationAbove200k_UsesAbove1hrAbove200kRate(t *testing.T) {
	// claude-3-5-sonnet-20241022-v2:0 on Bedrock has all four cache creation tiers.
	// When totalTokens > 200k and CachedWriteTokens1h > 0, the above_1hr_above_200k rate
	// ($15/M) must be used — the most specific tier wins.
	p := chatPricing(0.000003, 0.000015)
	p.InputCostPerTokenAbove200kTokens = bifrost.Ptr(0.000006)
	p.OutputCostPerTokenAbove200kTokens = bifrost.Ptr(0.00003)
	p.CacheCreationInputTokenCost = bifrost.Ptr(0.00000375)
	p.CacheCreationInputTokenCostAbove200kTokens = bifrost.Ptr(0.0000075)
	p.CacheCreationInputTokenCostAbove1hr = bifrost.Ptr(0.0000075)
	p.CacheCreationInputTokenCostAbove1hrAbove200kTokens = bifrost.Ptr(0.000015)

	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     210000,
		CompletionTokens: 25000,
		TotalTokens:      235000, // input is above 200k threshold
		PromptTokensDetails: &schemas.ChatPromptTokensDetails{
			CachedWriteTokens: 10000,
			CachedWriteTokenDetails: &schemas.ChatCachedWriteTokenDetails{
				CachedWriteTokens1h: 10000,
			},
		},
	}

	cost := computeTextCostTotal(&p, usage, serviceTier{})

	// Input rate (>200k): 0.000006; output rate (>200k): 0.00003
	// Input (non-cached): (210000-10000)*0.000006 = 200000*0.000006 = 1.20
	// Cache creation 1hr above 200k: 10000*0.000015 = 0.15
	// Output: 25000*0.00003 = 0.75
	// Total: 1.20 + 0.15 + 0.75 = 2.10
	assert.InDelta(t, 2.10, cost, 1e-9)
}

func TestComputeTextCost_1hrCacheCreationAbove200k_FallsBackToAbove1hrWhenAbove200kRateAbsent(t *testing.T) {
	// When CacheCreationInputTokenCostAbove1hrAbove200kTokens is absent but
	// CacheCreationInputTokenCostAbove1hr is present, the 1hr rate must be used
	// even for >200k requests.
	p := chatPricing(0.000003, 0.000015)
	p.InputCostPerTokenAbove200kTokens = bifrost.Ptr(0.000006)
	p.OutputCostPerTokenAbove200kTokens = bifrost.Ptr(0.00003)
	p.CacheCreationInputTokenCost = bifrost.Ptr(0.00000375)
	p.CacheCreationInputTokenCostAbove200kTokens = bifrost.Ptr(0.0000075)
	p.CacheCreationInputTokenCostAbove1hr = bifrost.Ptr(0.000009) // distinct from above_200k to make fallback unambiguous
	// CacheCreationInputTokenCostAbove1hrAbove200kTokens intentionally left nil

	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     210000,
		CompletionTokens: 25000,
		TotalTokens:      235000,
		PromptTokensDetails: &schemas.ChatPromptTokensDetails{
			CachedWriteTokens: 10000,
			CachedWriteTokenDetails: &schemas.ChatCachedWriteTokenDetails{
				CachedWriteTokens1h: 10000,
			},
		},
	}

	cost := computeTextCostTotal(&p, usage, serviceTier{})

	// Cache creation 1hr (no above_200k_1hr rate, uses above_1hr): 10000*0.000009 = 0.09
	// Input (non-cached): 200000*0.000006 = 1.20
	// Output: 25000*0.00003 = 0.75
	// Total: 1.20 + 0.09 + 0.75 = 2.04
	assert.InDelta(t, 2.04, cost, 1e-9)
}

func TestComputeTextCost_1hrCacheCreationAbove200k_FallsBackToStandardAbove200kWhenNo1hrRates(t *testing.T) {
	// When neither above_1hr field is present, 1hr tokens on a >200k request fall back
	// to the standard above_200k cache creation rate.
	p := chatPricing(8e-7, 0.000004)
	p.InputCostPerTokenAbove200kTokens = bifrost.Ptr(0.0000016)
	p.OutputCostPerTokenAbove200kTokens = bifrost.Ptr(0.000008)
	p.CacheCreationInputTokenCost = bifrost.Ptr(0.000001)
	p.CacheCreationInputTokenCostAbove200kTokens = bifrost.Ptr(0.000002)
	// Neither CacheCreationInputTokenCostAbove1hr nor Above1hrAbove200k is set

	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     210000,
		CompletionTokens: 25000,
		TotalTokens:      235000,
		PromptTokensDetails: &schemas.ChatPromptTokensDetails{
			CachedWriteTokens: 10000,
			CachedWriteTokenDetails: &schemas.ChatCachedWriteTokenDetails{
				CachedWriteTokens1h: 10000,
			},
		},
	}

	cost := computeTextCostTotal(&p, usage, serviceTier{})

	// Cache creation (1hr → no 1hr rates → standard above_200k): 10000*0.000002 = 0.02
	// Input (non-cached): 200000*0.0000016 = 0.32
	// Output: 25000*0.000008 = 0.2
	// Total: 0.32 + 0.02 + 0.2 = 0.54
	assert.InDelta(t, 0.54, cost, 1e-9)
}

func TestComputeTextCost_Tiered200k(t *testing.T) {
	// Claude 3.5 Sonnet Bedrock 200k tier: input=$6/M, output=$30/M
	p := chatPricing(0.000003, 0.000015)
	p.InputCostPerTokenAbove200kTokens = bifrost.Ptr(0.000006)
	p.OutputCostPerTokenAbove200kTokens = bifrost.Ptr(0.00003)

	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     210000,
		CompletionTokens: 30000,
		TotalTokens:      240000, // input is above 200k threshold
	}

	cost := computeTextCostTotal(&p, usage, serviceTier{})

	// Uses tiered rate since input > 200k
	// 210000 * 0.000006 + 30000 * 0.00003 = 1.26 + 0.90 = 2.16
	assert.InDelta(t, 2.16, cost, 1e-9)
}

func TestComputeTextCost_Below200kUsesBaseRate(t *testing.T) {
	p := chatPricing(0.000003, 0.000015)
	p.InputCostPerTokenAbove200kTokens = bifrost.Ptr(0.000006)
	p.OutputCostPerTokenAbove200kTokens = bifrost.Ptr(0.00003)

	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     1000,
		CompletionTokens: 500,
		TotalTokens:      1500, // Below 200k
	}

	cost := computeTextCostTotal(&p, usage, serviceTier{})

	// Uses base rate since input < 200k
	// 1000 * 0.000003 + 500 * 0.000015 = 0.003 + 0.0075 = 0.0105
	assert.InDelta(t, 0.0105, cost, 1e-12)
}

func TestComputeTextCost_TotalAbove200kButInputBelow200kUsesBaseRate(t *testing.T) {
	p := chatPricing(0.000003, 0.000015)
	p.InputCostPerTokenAbove200kTokens = bifrost.Ptr(0.000006)
	p.OutputCostPerTokenAbove200kTokens = bifrost.Ptr(0.00003)

	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     180000,
		CompletionTokens: 30000,
		TotalTokens:      210000, // total is above 200k, input is not
	}

	cost := computeTextCostTotal(&p, usage, serviceTier{})

	// Uses base rates because long-context tiers are selected by input tokens.
	// 180000 * 0.000003 + 30000 * 0.000015 = 0.54 + 0.45 = 0.99
	assert.InDelta(t, 0.99, cost, 1e-9)
}

func TestComputeTextCost_Tiered272k(t *testing.T) {
	p := chatPricing(0.000003, 0.000015)
	p.InputCostPerTokenAbove200kTokens = new(0.000006)
	p.OutputCostPerTokenAbove200kTokens = new(0.00003)
	p.InputCostPerTokenAbove272kTokens = new(0.000009)
	p.OutputCostPerTokenAbove272kTokens = new(0.000045)

	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     280000,
		CompletionTokens: 30000,
		TotalTokens:      310000, // input is above 272k threshold
	}

	cost := computeTextCostTotal(&p, usage, serviceTier{})

	// Uses 272k tiered rate since input > 272k
	// 280000 * 0.000009 + 30000 * 0.000045 = 2.52 + 1.35 = 3.87
	assert.InDelta(t, 3.87, cost, 1e-9)
}

func TestComputeTextCost_Between200kAnd272kUses200kRate(t *testing.T) {
	p := chatPricing(0.000003, 0.000015)
	p.InputCostPerTokenAbove200kTokens = new(0.000006)
	p.OutputCostPerTokenAbove200kTokens = new(0.00003)
	p.InputCostPerTokenAbove272kTokens = new(0.000009)
	p.OutputCostPerTokenAbove272kTokens = new(0.000045)

	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     230000,
		CompletionTokens: 30000,
		TotalTokens:      260000, // input is between 200k and 272k
	}

	cost := computeTextCostTotal(&p, usage, serviceTier{})

	// Uses 200k tiered rate since input > 200k but <= 272k
	// 230000 * 0.000006 + 30000 * 0.00003 = 1.38 + 0.90 = 2.28
	assert.InDelta(t, 2.28, cost, 1e-9)
}

func TestComputeTextCost_272kTierWithCacheRead(t *testing.T) {
	p := chatPricing(0.000003, 0.000015)
	p.InputCostPerTokenAbove272kTokens = new(0.000009)
	p.OutputCostPerTokenAbove272kTokens = new(0.000045)
	p.CacheReadInputTokenCost = new(0.0000003)
	p.CacheReadInputTokenCostAbove272kTokens = new(0.0000009)

	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     280000,
		CompletionTokens: 30000,
		TotalTokens:      310000, // input is above 272k
		PromptTokensDetails: &schemas.ChatPromptTokensDetails{
			CachedReadTokens: 50000,
		},
	}

	cost := computeTextCostTotal(&p, usage, serviceTier{})

	// Non-cached input: (280000-50000) * 0.000009 = 230000 * 0.000009 = 2.07
	// Cached read: 50000 * 0.0000009 = 0.045
	// Output: 30000 * 0.000045 = 1.35
	// Total: 2.07 + 0.045 + 1.35 = 3.465
	assert.InDelta(t, 3.465, cost, 1e-9)
}

// Claude Haiku 5.5 bills every token category at the higher rate once the
// prompt exceeds 100k tokens. The row is the datasheet entry as published.
func TestComputeTextCost_Haiku55Tiered100k(t *testing.T) {
	var entry Entry
	require.NoError(t, json.Unmarshal([]byte(`{
		"provider": "anthropic",
		"mode": "chat",
		"input_cost_per_token": 1e-07,
		"output_cost_per_token": 5e-07,
		"cache_creation_input_token_cost": 1.25e-07,
		"cache_creation_input_token_cost_above_1hr": 2e-07,
		"cache_read_input_token_cost": 1e-08,
		"input_cost_per_token_batches": 5e-08,
		"output_cost_per_token_batches": 2.5e-07,
		"input_cost_per_token_above_100k_tokens": 5e-07,
		"output_cost_per_token_above_100k_tokens": 2.5e-06,
		"cache_creation_input_token_cost_above_100k_tokens": 6.25e-07,
		"cache_creation_input_token_cost_above_1hr_above_100k_tokens": 1e-06,
		"cache_read_input_token_cost_above_100k_tokens": 5e-08,
		"input_cost_per_token_batches_above_100k_tokens": 2.5e-07,
		"output_cost_per_token_batches_above_100k_tokens": 1.25e-06,
		"inference_geo_us_multiplier": 1.1
	}`), &entry))
	pricing := convertEntryToTablePricing("claude-haiku-5-5", entry)

	// 40k cache read, 20k 5m write, 10k 1h write, 2k output; the rest is uncached input.
	usageFor := func(promptTokens int) *schemas.BifrostLLMUsage {
		return &schemas.BifrostLLMUsage{
			PromptTokens:     promptTokens,
			CompletionTokens: 2_000,
			TotalTokens:      promptTokens + 2_000,
			PromptTokensDetails: &schemas.ChatPromptTokensDetails{
				CachedReadTokens:  40_000,
				CachedWriteTokens: 30_000,
				CachedWriteTokenDetails: &schemas.ChatCachedWriteTokenDetails{
					CachedWriteTokens5m: 20_000,
					CachedWriteTokens1h: 10_000,
				},
			},
		}
	}

	tests := []struct {
		name         string
		promptTokens int
		want         float64
	}{
		// 30_000*1e-7 + 40_000*1e-8 + 20_000*1.25e-7 + 10_000*2e-7 + 2_000*5e-7
		{name: "exactly 100k uses base rates", promptTokens: 100_000, want: 0.0089},
		// 30_001*5e-7 + 40_000*5e-8 + 20_000*6.25e-7 + 10_000*1e-6 + 2_000*2.5e-6
		{name: "one token over 100k uses above-100k rates", promptTokens: 100_001, want: 0.0445005},
		// 80_000*5e-7 + 40_000*5e-8 + 20_000*6.25e-7 + 10_000*1e-6 + 2_000*2.5e-6
		{name: "150k uses above-100k rates", promptTokens: 150_000, want: 0.0695},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.InDelta(t, tt.want, computeTextCostTotal(&pricing, usageFor(tt.promptTokens), serviceTier{}), 1e-12)
		})
	}

	t.Run("breakdown above 100k", func(t *testing.T) {
		cost := computeTextCost(&pricing, usageFor(150_000), serviceTier{})
		require.NotNil(t, cost)
		require.NotNil(t, cost.InputCostDetails)
		assert.InDelta(t, 0.04, cost.InputCostDetails.TextCost, 1e-12)
		assert.InDelta(t, 0.002, cost.InputCostDetails.CachedReadCost, 1e-12)
		assert.InDelta(t, 0.0225, cost.InputCostDetails.CachedWriteCost, 1e-12)
		assert.InDelta(t, 0.005, cost.OutputCost, 1e-12)
	})

	t.Run("inference_geo us stacks on above-100k rates", func(t *testing.T) {
		got := computeTextCostTotal(&pricing, usageFor(150_000), serviceTier{inferenceGeoUS: true})
		assert.InDelta(t, 0.0695*1.1, got, 1e-12)
	})

	t.Run("batch stacks with above-100k rates", func(t *testing.T) {
		s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
			makeKey(pricing.Model, pricing.Provider, pricing.Mode): pricing,
		})
		below := s.CalculateBatchCostDetailsForUsage(usageFor(100_000), schemas.Anthropic, "claude-haiku-5-5", schemas.ChatCompletionRequest, nil)
		require.True(t, below.Priced)
		assert.InDelta(t, 0.0089*0.5, below.Cost, 1e-12)

		// Batch ratio 0.5: 80_000*2.5e-7 + 40_000*2.5e-8 + 20_000*3.125e-7 + 10_000*5e-7 + 2_000*1.25e-6
		above := s.CalculateBatchCostDetailsForUsage(usageFor(150_000), schemas.Anthropic, "claude-haiku-5-5", schemas.ChatCompletionRequest, nil)
		require.True(t, above.Priced)
		assert.InDelta(t, 0.03475, above.Cost, 1e-12)
	})
}

func TestComputeTextCost_SearchQueryCost(t *testing.T) {
	p := chatPricing(0.000003, 0.000015)
	p.WebSearchCostPerRequest = bifrost.Ptr(0.01) // $0.01 per web search request

	numQueries := 3
	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     1000,
		CompletionTokens: 500,
		TotalTokens:      1500,
		ToolUsage:        &schemas.ToolUsage{WebSearch: &schemas.WebSearchToolUsage{NumRequests: numQueries}},
	}

	cost := computeTextCostTotal(&p, usage, serviceTier{})

	// 1000*0.000003 + 500*0.000015 + 3*0.01 = 0.003 + 0.0075 + 0.03 = 0.0405
	assert.InDelta(t, 0.0405, cost, 1e-12)
}

func TestComputeTextCost_LegacySearchContextRateNotPriced(t *testing.T) {
	p := chatPricing(0.000003, 0.000015)
	p.SearchContextCostPerQuery = bifrost.Ptr(0.01)

	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     1000,
		CompletionTokens: 500,
		TotalTokens:      1500,
		ToolUsage:        &schemas.ToolUsage{WebSearch: &schemas.WebSearchToolUsage{NumRequests: 3}},
	}

	// Only web_search_cost_per_request prices web search; the legacy rate is folded in at unmarshal.
	assert.InDelta(t, 0.0105, computeTextCostTotal(&p, usage, serviceTier{}), 1e-12)
}

func TestEntryUnmarshal_WebSearchCostPerRequestFallsBackToSearchContext(t *testing.T) {
	var legacy Entry
	require.NoError(t, json.Unmarshal([]byte(`{"search_context_cost_per_query":{"search_context_size_low":0.01,"search_context_size_medium":0.01,"search_context_size_high":0.01}}`), &legacy))
	require.NotNil(t, legacy.WebSearchCostPerRequest)
	assert.InDelta(t, 0.01, *legacy.WebSearchCostPerRequest, 1e-12)

	var explicit Entry
	require.NoError(t, json.Unmarshal([]byte(`{"web_search_cost_per_request":0.02,"search_context_cost_per_query":{"search_context_size_medium":0.01}}`), &explicit))
	require.NotNil(t, explicit.WebSearchCostPerRequest)
	assert.InDelta(t, 0.02, *explicit.WebSearchCostPerRequest, 1e-12)

	var none Entry
	require.NoError(t, json.Unmarshal([]byte(`{"input_cost_per_token":0.001}`), &none))
	assert.Nil(t, none.WebSearchCostPerRequest)
}

// upstreamBedrockGuardrailsEntry is bedrock/guardrails exactly as the upstream datasheet publishes it
// (getbifrost.ai/datasheet, 14 Nov 2026; source aws.amazon.com/bedrock/pricing).
const upstreamBedrockGuardrailsEntry = `{"guardrail_cost_per_unit":{"automatedReasoningPolicyUnits":0.00017,"contentPolicyImageUnits":0.00075,"contentPolicyUnits":0.00015,"contextualGroundingPolicyUnits":0.0001,"sensitiveInformationPolicyFreeUnits":0,"sensitiveInformationPolicyUnits":0.0001,"topicPolicyUnits":0.00015,"wordPolicyUnits":0},"mode":"guardrail","source":"https://aws.amazon.com/bedrock/pricing/","provider":"bedrock","base_model":"guardrails"}`

// TestEntryUnmarshal_GuardrailCostPerUnitSpreadsIntoUnitRates (TXE-7365): upstream's map of unit to
// price lands on the per-unit rates, and a flat rate already set is not overwritten.
func TestEntryUnmarshal_GuardrailCostPerUnitSpreadsIntoUnitRates(t *testing.T) {
	var entry Entry
	require.NoError(t, json.Unmarshal([]byte(upstreamBedrockGuardrailsEntry), &entry))
	for name, got := range map[string]*float64{
		"content": entry.GuardrailCostPerContentPolicyUnit, "content image": entry.GuardrailCostPerContentPolicyImageUnit,
		"topic": entry.GuardrailCostPerTopicPolicyUnit, "word": entry.GuardrailCostPerWordPolicyUnit,
		"sensitive": entry.GuardrailCostPerSensitiveInformationPolicyUnit, "sensitive free": entry.GuardrailCostPerSensitiveInformationPolicyFreeUnit,
		"grounding": entry.GuardrailCostPerContextualGroundingPolicyUnit, "reasoning": entry.GuardrailCostPerAutomatedReasoningPolicyUnit,
	} {
		require.NotNil(t, got, name)
	}
	assert.InDelta(t, 0.00015, *entry.GuardrailCostPerContentPolicyUnit, 1e-12)
	assert.InDelta(t, 0.00075, *entry.GuardrailCostPerContentPolicyImageUnit, 1e-12)
	assert.InDelta(t, 0.00017, *entry.GuardrailCostPerAutomatedReasoningPolicyUnit, 1e-12)
	assert.Zero(t, *entry.GuardrailCostPerWordPolicyUnit)

	var flat Entry
	require.NoError(t, json.Unmarshal([]byte(`{"guardrail_cost_per_content_policy_unit":0.0002,"guardrail_cost_per_unit":{"contentPolicyUnits":0.00015}}`), &flat))
	assert.InDelta(t, 0.0002, *flat.GuardrailCostPerContentPolicyUnit, 1e-12, "a flat rate wins over the map")
}

// TestCalculateCost_BedrockGuardrailUnitsAreBilled (TXE-7365): a Bedrock request whose guardrail
// reported policy units is charged each kind at its upstream rate, on the additional side as
// guardrail cost, on top of its tokens. The rates come from bedrock/guardrails unless the request's
// own model row sets them (a pricing override). Before the key was parsed the units were free.
func TestCalculateCost_BedrockGuardrailUnitsAreBilled(t *testing.T) {
	var guardrailsEntry Entry
	require.NoError(t, json.Unmarshal([]byte(upstreamBedrockGuardrailsEntry), &guardrailsEntry))
	guardrails := convertEntryToTablePricing("bedrock/guardrails", guardrailsEntry)
	model := chatPricing(0.000003, 0.000015)
	model.Model, model.Provider, model.Mode = "anthropic.claude-sonnet-4-5", "bedrock", "chat"
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey(model.Model, "bedrock", "chat"):       model,
		makeKey("guardrails", "bedrock", "guardrail"): guardrails,
	})
	units := &schemas.GuardrailUnits{
		ContentPolicyUnits: 4, ContentPolicyImageUnits: 1, TopicPolicyUnits: 1, WordPolicyUnits: 1,
		SensitiveInformationPolicyUnits: 1, ContextualGroundingPolicyUnits: 3,
	}
	chat := func(units *schemas.GuardrailUnits) *schemas.BifrostResponse {
		return &schemas.BifrostResponse{ChatResponse: &schemas.BifrostChatResponse{
			Usage: &schemas.BifrostLLMUsage{PromptTokens: 100, CompletionTokens: 10, TotalTokens: 110, GuardrailUnits: units},
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType: schemas.ChatCompletionRequest, RoutingInfo: routingInfoFor(schemas.Bedrock, "anthropic.claude-sonnet-4-5"),
			},
		}}
	}
	tokens := 100*0.000003 + 10*0.000015
	guardrail := 4*0.00015 + 1*0.00075 + 1*0.00015 + 1*0.0 + 1*0.0001 + 3*0.0001

	breakdown := s.CalculateCostBreakdown(chat(units), nil)
	require.NotNil(t, breakdown)
	assert.InDelta(t, tokens+guardrail, breakdown.TotalCost, 1e-12)
	require.NotNil(t, breakdown.AdditionalCostDetails)
	assert.InDelta(t, guardrail, breakdown.AdditionalCostDetails.GuardrailCost, 1e-12)
	assert.InDelta(t, tokens, s.CalculateCost(chat(nil), nil), 1e-12, "no guardrail units, tokens only")

	// A pricing override on the model's own row sets its own guardrail rates, which win rate by
	// rate. One that sets only the content rate changes only content units: every other kind is
	// still charged at bedrock/guardrails' rate, never at nothing (the partial patch the UI and API
	// send).
	model.GuardrailCostPerContentPolicyUnit = bifrost.Ptr(0.001)
	s = testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey(model.Model, "bedrock", "chat"):       model,
		makeKey("guardrails", "bedrock", "guardrail"): guardrails,
	})
	rest := 1*0.00075 + 1*0.00015 + 1*0.0 + 1*0.0001 + 3*0.0001
	assert.InDelta(t, tokens+4*0.001+rest, s.CalculateCost(chat(units), nil), 1e-12,
		"a content-only override keeps the provider's rates for the other unit kinds")

	// An explicit zero in the override is a rate, kept rather than replaced by the provider's.
	model.GuardrailCostPerTopicPolicyUnit = bifrost.Ptr(0.0)
	s = testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey(model.Model, "bedrock", "chat"):       model,
		makeKey("guardrails", "bedrock", "guardrail"): guardrails,
	})
	assert.InDelta(t, tokens+4*0.001+rest-1*0.00015, s.CalculateCost(chat(units), nil), 1e-12,
		"an override's explicit zero topic rate charges topic units nothing")

	// With no provider guardrails row, the model's own rates are all there is.
	s = testStoreWithPricing(map[string]configstoreTables.TableModelPricing{makeKey(model.Model, "bedrock", "chat"): model})
	assert.InDelta(t, tokens+4*0.001, s.CalculateCost(chat(units), nil), 1e-12)

	// A model with no pricing row at all (TXE-7390) prices no tokens, but its guardrail units are
	// still charged at bedrock/guardrails' rates: on a finished request, and on the billed usage
	// of a stream cut short.
	s = testStoreWithPricing(map[string]configstoreTables.TableModelPricing{makeKey("guardrails", "bedrock", "guardrail"): guardrails})
	unpriced := s.CalculateCostBreakdown(chat(units), nil)
	require.NotNil(t, unpriced, "an unpriced model's guardrail units are charged")
	assert.InDelta(t, guardrail, unpriced.TotalCost, 1e-12)
	require.NotNil(t, unpriced.AdditionalCostDetails)
	assert.InDelta(t, guardrail, unpriced.AdditionalCostDetails.GuardrailCost, 1e-12)
	assert.Zero(t, unpriced.InputCost, "no token is priced")
	interrupted := &schemas.BifrostLLMUsage{PromptTokens: 100, TotalTokens: 100, GuardrailUnits: units}
	assert.InDelta(t, guardrail, s.CalculateCostForUsage(interrupted, schemas.Bedrock, "anthropic.claude-sonnet-4-5", schemas.ChatCompletionStreamRequest, nil), 1e-12)
	assert.Nil(t, s.CalculateCostBreakdown(chat(nil), nil), "an unpriced model with no guardrail units still has no cost")

	// With neither the model nor the provider's guardrails priced, nothing is charged.
	s = testStoreWithPricing(map[string]configstoreTables.TableModelPricing{})
	assert.Nil(t, s.CalculateCostBreakdown(chat(units), nil))
}

func TestComputeTextCost_DeprecatedNumSearchQueriesNotPriced(t *testing.T) {
	p := chatPricing(0.000003, 0.000015)
	p.WebSearchCostPerRequest = bifrost.Ptr(0.01)

	numQueries := 3
	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     1000,
		CompletionTokens: 500,
		TotalTokens:      1500,
		CompletionTokensDetails: &schemas.ChatCompletionTokensDetails{
			NumSearchQueries: &numQueries,
		},
	}

	cost := computeTextCostTotal(&p, usage, serviceTier{})

	// Only tokens: 0.003 + 0.0075; search is billed from ToolUsage alone.
	assert.InDelta(t, 0.0105, cost, 1e-12)
}

func TestComputeTextCost_NoCacheRateFallsBackToBaseInputRate(t *testing.T) {
	// If cache rate fields are nil, tieredCacheReadInputTokenRate falls back to base InputCostPerToken
	p := chatPricing(0.000005, 0.000015)

	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     1000,
		CompletionTokens: 500,
		TotalTokens:      1500,
		PromptTokensDetails: &schemas.ChatPromptTokensDetails{
			CachedReadTokens: 400,
		},
	}

	cost := computeTextCostTotal(&p, usage, serviceTier{})

	// Non-cached prompt: (1000-400)*0.000005 = 600*0.000005 = 0.003
	// Cached prompt: 400 tokens at base input rate (no cache rate set) = 400*0.000005 = 0.002
	// Output: 500*0.000015 = 0.0075
	// Total: 0.003 + 0.002 + 0.0075 = 0.0125
	assert.InDelta(t, 0.0125, cost, 1e-12)
}

// =========================================================================
// 2. computeEmbeddingCost — unit tests
// =========================================================================

func TestComputeEmbeddingCost_Basic(t *testing.T) {
	// Titan Embed Text v1: $0.1/M input
	p := configstoreTables.TableModelPricing{
		InputCostPerToken:  bifrost.Ptr(0.0000001),
		OutputCostPerToken: bifrost.Ptr(0.0),
	}
	usage := &schemas.BifrostLLMUsage{
		PromptTokens: 5000,
		TotalTokens:  5000,
	}
	cost := computeEmbeddingCostTotal(&p, usage, serviceTier{})
	// 5000 * 0.0000001 = 0.0005
	assert.InDelta(t, 0.0005, cost, 1e-12)
}

func TestComputeEmbeddingCost_TotalAbove200kButInputBelow200kUsesBaseRate(t *testing.T) {
	p := configstoreTables.TableModelPricing{
		InputCostPerToken:                bifrost.Ptr(0.000003),
		InputCostPerTokenAbove200kTokens: bifrost.Ptr(0.000006),
	}
	usage := &schemas.BifrostLLMUsage{
		PromptTokens: 180000,
		TotalTokens:  210000,
	}

	cost := computeEmbeddingCostTotal(&p, usage, serviceTier{})

	assert.InDelta(t, 180000*0.000003, cost, 1e-9)
}

func TestComputeEmbeddingCost_NilUsage(t *testing.T) {
	p := configstoreTables.TableModelPricing{InputCostPerToken: new(0.0000001)}
	assert.Equal(t, 0.0, computeEmbeddingCostTotal(&p, nil, serviceTier{}))
}

// A vendor that bills embeddings by character (Vertex text models: usage.billed_characters)
// is priced by character when the model has a per-character rate, and the token rate is
// not added on top. Before TXE-7081 a character-only usage cost nothing, so it advanced no
// dollar budget.
func TestComputeEmbeddingCost_BilledCharactersUsePerCharacterRate(t *testing.T) {
	p := configstoreTables.TableModelPricing{
		InputCostPerToken:     bifrost.Ptr(0.0000001),
		InputCostPerCharacter: bifrost.Ptr(0.000000025),
	}
	chars := 4000

	onlyCharacters := &schemas.BifrostLLMUsage{BilledCharacters: &chars}
	assert.InDelta(t, 4000*0.000000025, computeEmbeddingCostTotal(&p, onlyCharacters, serviceTier{}), 1e-15)

	both := &schemas.BifrostLLMUsage{PromptTokens: 900, TotalTokens: 900, BilledCharacters: &chars}
	assert.InDelta(t, 4000*0.000000025, computeEmbeddingCostTotal(&p, both, serviceTier{}), 1e-15,
		"the vendor bills characters, so the token rate is not added on top")
}

// Without a per-character rate, or without a reported character count, embeddings cost
// what their token count costs, as before; Bifrost never turns characters into tokens.
func TestComputeEmbeddingCost_TokenPathUnchangedWithoutCharacterPricing(t *testing.T) {
	tokenOnly := configstoreTables.TableModelPricing{InputCostPerToken: bifrost.Ptr(0.0000001)}
	chars := 4000
	usage := &schemas.BifrostLLMUsage{PromptTokens: 900, TotalTokens: 900, BilledCharacters: &chars}
	assert.InDelta(t, 900*0.0000001, computeEmbeddingCostTotal(&tokenOnly, usage, serviceTier{}), 1e-15)
	assert.Equal(t, 0.0, computeEmbeddingCostTotal(&tokenOnly, &schemas.BifrostLLMUsage{BilledCharacters: &chars}, serviceTier{}),
		"characters are not priced at a token rate")

	charPriced := configstoreTables.TableModelPricing{InputCostPerToken: bifrost.Ptr(0.0000001), InputCostPerCharacter: bifrost.Ptr(0.000000025)}
	assert.InDelta(t, 900*0.0000001, computeEmbeddingCostTotal(&charPriced, &schemas.BifrostLLMUsage{PromptTokens: 900, TotalTokens: 900}, serviceTier{}), 1e-15,
		"no reported character count keeps the token path")
}

// =========================================================================
// 3. computeRerankCost — unit tests
// =========================================================================

func TestComputeRerankCost_Basic(t *testing.T) {
	p := configstoreTables.TableModelPricing{
		InputCostPerToken:  bifrost.Ptr(0.000001),
		OutputCostPerToken: bifrost.Ptr(0.000002),
	}
	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     2000,
		CompletionTokens: 100,
		TotalTokens:      2100,
	}
	cost := computeRerankCostTotal(&p, usage, serviceTier{})
	// 2000*0.000001 + 100*0.000002 = 0.002 + 0.0002 = 0.0022
	assert.InDelta(t, 0.0022, cost, 1e-12)
}

func TestComputeRerankCost_TotalAbove200kButInputBelow200kUsesBaseRate(t *testing.T) {
	p := chatPricing(0.000003, 0.000015)
	p.InputCostPerTokenAbove200kTokens = bifrost.Ptr(0.000006)
	p.OutputCostPerTokenAbove200kTokens = bifrost.Ptr(0.00003)
	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     180000,
		CompletionTokens: 30000,
		TotalTokens:      210000,
	}

	cost := computeRerankCostTotal(&p, usage, serviceTier{})

	assert.InDelta(t, 180000*0.000003+30000*0.000015, cost, 1e-9)
}

func TestComputeRerankCost_WithSearchCost(t *testing.T) {
	p := configstoreTables.TableModelPricing{
		InputCostPerToken:       bifrost.Ptr(0.0),
		OutputCostPerToken:      bifrost.Ptr(0.0),
		WebSearchCostPerRequest: bifrost.Ptr(0.001),
	}
	numQueries := 5
	usage := &schemas.BifrostLLMUsage{
		ToolUsage: &schemas.ToolUsage{WebSearch: &schemas.WebSearchToolUsage{NumRequests: numQueries}},
	}
	cost := computeRerankCostTotal(&p, usage, serviceTier{})
	assert.InDelta(t, 0.005, cost, 1e-12)
}

// TestComputeRerankCost_BreakdownDetails asserts rerank surfaces the output-side
// SearchQueriesCost (billed on output, matching computeTextCost) rather than
// folding it into a bare OutputCost total.
func TestComputeRerankCost_BreakdownDetails(t *testing.T) {
	p := configstoreTables.TableModelPricing{
		InputCostPerToken:       bifrost.Ptr(0.001),
		OutputCostPerToken:      bifrost.Ptr(0.002),
		WebSearchCostPerRequest: bifrost.Ptr(0.001),
	}
	numQueries := 3
	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     10,
		CompletionTokens: 5,
		ToolUsage:        &schemas.ToolUsage{WebSearch: &schemas.WebSearchToolUsage{NumRequests: numQueries}},
	}
	cost := computeRerankCost(&p, usage, serviceTier{})
	require.NotNil(t, cost)
	require.NotNil(t, cost.InputCostDetails)
	require.NotNil(t, cost.OutputCostDetails)
	assert.InDelta(t, 0.01, cost.InputCost, 1e-12)   // 10 * 0.001
	assert.InDelta(t, 0.013, cost.OutputCost, 1e-12) // 5*0.002 + 3*0.001
	assert.InDelta(t, 0.023, cost.TotalCost, 1e-12)  // input + output
	assert.InDelta(t, 0.01, cost.InputCostDetails.TextCost, 1e-12)
	assert.InDelta(t, 0.01, cost.OutputCostDetails.TextCost, 1e-12)
	assert.InDelta(t, 0.003, cost.OutputCostDetails.SearchQueriesCost, 1e-12)
}

// TestComputeSpeechCost_BreakdownDetails asserts TTS splits text input / audio output.
func TestComputeSpeechCost_BreakdownDetails(t *testing.T) {
	p := configstoreTables.TableModelPricing{
		InputCostPerToken:       bifrost.Ptr(0.000001),
		OutputCostPerAudioToken: bifrost.Ptr(0.00005),
	}
	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     200,
		CompletionTokens: 100,
		TotalTokens:      300,
	}
	cost := computeSpeechCost(&p, usage, nil, 0, serviceTier{})
	require.NotNil(t, cost)
	require.NotNil(t, cost.InputCostDetails)
	require.NotNil(t, cost.OutputCostDetails)
	assert.InDelta(t, 0.0002, cost.InputCostDetails.TextCost, 1e-12)  // 200 * 0.000001
	assert.InDelta(t, 0.005, cost.OutputCostDetails.AudioCost, 1e-12) // 100 * 0.00005
	assert.Zero(t, cost.OutputCostDetails.TextCost)
}

// TestComputeTranscriptionCost_BreakdownDetails asserts STT splits audio input / text
// output, and that a mixed audio+text input reconciles AudioCost + TextCost to the total.
func TestComputeTranscriptionCost_BreakdownDetails(t *testing.T) {
	p := configstoreTables.TableModelPricing{
		InputCostPerToken:      bifrost.Ptr(0.000005),
		OutputCostPerToken:     bifrost.Ptr(0.000015),
		InputCostPerAudioToken: bifrost.Ptr(0.00001),
	}
	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     2000,
		CompletionTokens: 500,
		TotalTokens:      2500,
	}
	audioDetails := &schemas.TranscriptionUsageInputTokenDetails{
		AudioTokens: 1500,
		TextTokens:  500,
	}
	cost := computeTranscriptionCost(&p, usage, nil, audioDetails, serviceTier{})
	require.NotNil(t, cost)
	require.NotNil(t, cost.InputCostDetails)
	require.NotNil(t, cost.OutputCostDetails)
	assert.InDelta(t, 0.015, cost.InputCostDetails.AudioCost, 1e-12)  // 1500 * 0.00001
	assert.InDelta(t, 0.0025, cost.InputCostDetails.TextCost, 1e-12)  // 500 * 0.000005
	assert.InDelta(t, 0.0075, cost.OutputCostDetails.TextCost, 1e-12) // 500 * 0.000015
	// Details reconcile with the authoritative side totals.
	assert.InDelta(t, cost.InputCost, cost.InputCostDetails.AudioCost+cost.InputCostDetails.TextCost, 1e-12)
}

func TestComputeRerankCost_NilUsage(t *testing.T) {
	p := configstoreTables.TableModelPricing{InputCostPerToken: new(0.001)}
	assert.Equal(t, 0.0, computeRerankCostTotal(&p, nil, serviceTier{}))
}

// TestComputeCost_MultiModalBreakdown asserts that non-text modalities also
// populate the input/output split (not just the total), so per-model quota
// aggregation attributes their cost to the right buckets. Cache stays text-only.
func TestComputeCost_MultiModalBreakdown(t *testing.T) {
	// Embedding: input-only — the whole cost lands in InputTokensCost.
	embPricing := configstoreTables.TableModelPricing{InputCostPerToken: bifrost.Ptr(0.0000001)}
	emb := computeEmbeddingCost(&embPricing, &schemas.BifrostLLMUsage{PromptTokens: 5000, TotalTokens: 5000}, serviceTier{})
	require.NotNil(t, emb)
	assert.InDelta(t, 0.0005, emb.InputCost, 1e-12)
	assert.Zero(t, emb.OutputCost)
	assert.InDelta(t, emb.InputCost, emb.TotalCost, 1e-12)

	// Rerank: input + output, and search queries billed on the output side.
	rerankPricing := chatPricing(0.000002, 0.000004)
	rr := computeRerankCost(&rerankPricing, &schemas.BifrostLLMUsage{
		PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500,
	}, serviceTier{})
	require.NotNil(t, rr)
	assert.InDelta(t, 1000*0.000002, rr.InputCost, 1e-12)
	assert.InDelta(t, 500*0.000004, rr.OutputCost, 1e-12)
	assert.InDelta(t, rr.InputCost+rr.OutputCost, rr.TotalCost, 1e-12)

	// Image: input + output tokens split.
	imgPricing := chatPricing(0.000002, 0.000008)
	img := computeImageCost(&imgPricing, &schemas.ImageUsage{
		InputTokens:  1000,
		OutputTokens: 200,
		TotalTokens:  1200,
	}, imageRequestShape{}, serviceTier{})
	require.NotNil(t, img)
	assert.InDelta(t, 1000*0.000002, img.InputCost, 1e-12)
	assert.InDelta(t, 200*0.000008, img.OutputCost, 1e-12)
	assert.InDelta(t, img.InputCost+img.OutputCost, img.TotalCost, 1e-12)
}

// =========================================================================
// 4. computeSpeechCost — unit tests
// =========================================================================

func TestComputeSpeechCost_TokensPreferredOverDuration(t *testing.T) {
	// TTS: input=text tokens, output=audio tokens (preferred over per-second)
	p := configstoreTables.TableModelPricing{
		InputCostPerToken:   bifrost.Ptr(0.0000025),
		OutputCostPerToken:  bifrost.Ptr(0.00001),
		OutputCostPerSecond: bifrost.Ptr(0.00025),
	}
	seconds := 60.0
	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     100,
		CompletionTokens: 200,
		TotalTokens:      300,
	}
	cost := computeSpeechCostTotal(&p, usage, &seconds, 0, serviceTier{})
	// Input: 100 text tokens * $0.0000025 = $0.00025
	// Output: 200 audio tokens present → uses token rate $0.00001, NOT per-second
	//         200 * $0.00001 = $0.002
	// Total: $0.00225
	assert.InDelta(t, 0.00225, cost, 1e-12)
}

func TestComputeSpeechCost_OutputFallsBackToPerSecond(t *testing.T) {
	// TTS: no output tokens → falls back to per-second output pricing
	p := configstoreTables.TableModelPricing{
		InputCostPerToken:   bifrost.Ptr(0.000001),
		OutputCostPerToken:  bifrost.Ptr(0.000002),
		OutputCostPerSecond: bifrost.Ptr(0.0001),
	}
	seconds := 120.0
	usage := &schemas.BifrostLLMUsage{PromptTokens: 500}
	cost := computeSpeechCostTotal(&p, usage, &seconds, 0, serviceTier{})
	// Input: 500 * $0.000001 = $0.0005
	// Output: no CompletionTokens → falls back to 120 * $0.0001 = $0.012
	// Total: $0.0125
	assert.InDelta(t, 0.0125, cost, 1e-12)
}

func TestComputeSpeechCost_OutputAudioTokenRate(t *testing.T) {
	// TTS: output uses OutputCostPerAudioToken when available
	p := configstoreTables.TableModelPricing{
		InputCostPerToken:       bifrost.Ptr(0.000001),
		OutputCostPerToken:      bifrost.Ptr(0.000002),
		OutputCostPerAudioToken: bifrost.Ptr(0.00005),
	}
	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     200,
		CompletionTokens: 100,
		TotalTokens:      300,
	}
	cost := computeSpeechCostTotal(&p, usage, nil, 0, serviceTier{})
	// Input: 200 * $0.000001 = $0.0002
	// Output: 100 * $0.00005 = $0.005 (OutputCostPerAudioToken preferred)
	// Total: $0.0052
	assert.InDelta(t, 0.0052, cost, 1e-12)
}

func TestComputeSpeechCost_TokenFallback(t *testing.T) {
	p := chatPricing(0.000005, 0.000015)
	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     1000,
		CompletionTokens: 500,
		TotalTokens:      1500,
	}
	cost := computeSpeechCostTotal(&p, usage, nil, 0, serviceTier{}) // No audio seconds → token fallback
	// 1000*0.000005 + 500*0.000015 = 0.005 + 0.0075 = 0.0125
	assert.InDelta(t, 0.0125, cost, 1e-12)
}

func TestComputeSpeechCost_TotalAbove200kButInputBelow200kUsesBaseRate(t *testing.T) {
	p := chatPricing(0.000003, 0.000015)
	p.InputCostPerTokenAbove200kTokens = bifrost.Ptr(0.000006)
	p.OutputCostPerTokenAbove200kTokens = bifrost.Ptr(0.00003)
	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     180000,
		CompletionTokens: 30000,
		TotalTokens:      210000,
	}

	cost := computeSpeechCostTotal(&p, usage, nil, 0, serviceTier{})

	assert.InDelta(t, 180000*0.000003+30000*0.000015, cost, 1e-9)
}

func TestComputeSpeechCost_NilUsageNilSeconds(t *testing.T) {
	p := chatPricing(0.000005, 0.000015)
	assert.Equal(t, 0.0, computeSpeechCostTotal(&p, nil, nil, 0, serviceTier{}))
}

// =========================================================================
// 5. computeTranscriptionCost — unit tests
// =========================================================================

func TestComputeTranscriptionCost_DurationBased(t *testing.T) {
	// assemblyai/nano: input_cost_per_second=0.00010278
	p := configstoreTables.TableModelPricing{
		InputCostPerToken:  bifrost.Ptr(0.0),
		OutputCostPerToken: bifrost.Ptr(0.0),
		InputCostPerSecond: bifrost.Ptr(0.00010278),
	}
	seconds := 300.0 // 5 minutes
	cost := computeTranscriptionCostTotal(&p, nil, &seconds, nil, serviceTier{})
	// 300 * 0.00010278 = 0.030834
	assert.InDelta(t, 0.030834, cost, 1e-9)
}

func TestComputeTranscriptionCost_AudioTokenDetails(t *testing.T) {
	p := configstoreTables.TableModelPricing{
		InputCostPerToken:      bifrost.Ptr(0.000005),
		OutputCostPerToken:     bifrost.Ptr(0.000015),
		InputCostPerAudioToken: bifrost.Ptr(0.00001),
	}
	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     2000,
		CompletionTokens: 500,
		TotalTokens:      2500,
	}
	audioDetails := &schemas.TranscriptionUsageInputTokenDetails{
		AudioTokens: 1500,
		TextTokens:  500,
	}
	cost := computeTranscriptionCostTotal(&p, usage, nil, audioDetails, serviceTier{})
	// Audio: 1500*0.00001 = 0.015
	// Text:  500*0.000005 = 0.0025
	// Output: 500*0.000015 = 0.0075
	// Total: 0.025
	assert.InDelta(t, 0.025, cost, 1e-12)
}

func TestComputeTranscriptionCost_TokenFallback(t *testing.T) {
	p := chatPricing(0.000005, 0.000015)
	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     1000,
		CompletionTokens: 200,
		TotalTokens:      1200,
	}
	cost := computeTranscriptionCostTotal(&p, usage, nil, nil, serviceTier{})
	// 1000*0.000005 + 200*0.000015 = 0.005 + 0.003 = 0.008
	assert.InDelta(t, 0.008, cost, 1e-12)
}

func TestComputeTranscriptionCost_TotalAbove200kButInputBelow200kUsesBaseRate(t *testing.T) {
	p := chatPricing(0.000003, 0.000015)
	p.InputCostPerTokenAbove200kTokens = bifrost.Ptr(0.000006)
	p.OutputCostPerTokenAbove200kTokens = bifrost.Ptr(0.00003)
	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     180000,
		CompletionTokens: 30000,
		TotalTokens:      210000,
	}

	cost := computeTranscriptionCostTotal(&p, usage, nil, nil, serviceTier{})

	assert.InDelta(t, 180000*0.000003+30000*0.000015, cost, 1e-9)
}

func TestComputeTranscriptionCost_TokenDetailsPreferredOverDuration(t *testing.T) {
	// STT: audio token details present → uses tokens, not per-second
	p := configstoreTables.TableModelPricing{
		InputCostPerToken:          bifrost.Ptr(0.000005),
		OutputCostPerToken:         bifrost.Ptr(0.0),
		InputCostPerAudioPerSecond: bifrost.Ptr(0.0001),
		InputCostPerAudioToken:     bifrost.Ptr(0.00001),
	}
	seconds := 60.0
	audioDetails := &schemas.TranscriptionUsageInputTokenDetails{
		AudioTokens: 5000,
		TextTokens:  1000,
	}
	cost := computeTranscriptionCostTotal(&p, nil, &seconds, audioDetails, serviceTier{})
	// Input: audio token details present → tokens preferred over per-second
	//   5000 audio * $0.00001 = $0.05
	//   1000 text  * $0.000005 = $0.005
	// Output: nil usage → $0
	// Total: $0.055
	assert.InDelta(t, 0.055, cost, 1e-12)
}

func TestComputeTranscriptionCost_DurationFallbackWhenNoTokens(t *testing.T) {
	// STT: no audio token details, no prompt tokens → falls back to per-second
	p := configstoreTables.TableModelPricing{
		InputCostPerToken:          bifrost.Ptr(0.000005),
		OutputCostPerToken:         bifrost.Ptr(0.000015),
		InputCostPerAudioPerSecond: bifrost.Ptr(0.0001),
	}
	seconds := 60.0
	usage := &schemas.BifrostLLMUsage{
		CompletionTokens: 200,
		TotalTokens:      200,
	}
	cost := computeTranscriptionCostTotal(&p, usage, &seconds, nil, serviceTier{})
	// Input: no audio details, PromptTokens=0 → falls back to 60 * $0.0001 = $0.006
	// Output: 200 * $0.000015 = $0.003
	// Total: $0.009
	assert.InDelta(t, 0.009, cost, 1e-12)
}

// =========================================================================
// 6. computeImageCost — unit tests
// =========================================================================

func TestComputeImageCost_PerImage(t *testing.T) {
	// dall-e-3 (aiml): output_cost_per_image=$0.052
	p := configstoreTables.TableModelPricing{
		InputCostPerToken:  bifrost.Ptr(0.0),
		OutputCostPerToken: bifrost.Ptr(0.0),
		OutputCostPerImage: bifrost.Ptr(0.052),
	}
	usage := &schemas.ImageUsage{
		OutputTokensDetails: &schemas.ImageTokenDetails{
			NImages: 2,
		},
	}
	cost := computeImageCostTotal(&p, usage, "", "", serviceTier{})
	// 2 * 0.052 = 0.104
	assert.InDelta(t, 0.104, cost, 1e-12)
}

// TXE-6526: output details that carry only reasoning tokens (Gemini reports thoughtsTokenCount
// with no candidatesTokensDetails) must not hide the aggregate output tokens from token pricing.
func TestComputeImageCost_ReasoningOnlyDetailsKeepAggregateOutputTokens(t *testing.T) {
	p := configstoreTables.TableModelPricing{
		InputCostPerToken:  bifrost.Ptr(0.0),
		OutputCostPerToken: bifrost.Ptr(0.00003),
	}
	withoutDetails := computeImageCostTotal(&p, &schemas.ImageUsage{OutputTokens: 1300}, "", "", serviceTier{})
	assert.InDelta(t, 0.039, withoutDetails, 1e-12)

	reasoningOnly := computeImageCostTotal(&p, &schemas.ImageUsage{
		OutputTokens:        1300,
		OutputTokensDetails: &schemas.ImageTokenDetails{ReasoningTokens: bifrost.Ptr(200)},
	}, "", "", serviceTier{})
	assert.InDelta(t, withoutDetails, reasoningOnly, 1e-12)

	// A model priced per image, with no token rate, keeps its per-image price for the same usage
	// (the cost path counts the delivered images into NImages).
	perImage := configstoreTables.TableModelPricing{
		InputCostPerToken:  bifrost.Ptr(0.0),
		OutputCostPerToken: bifrost.Ptr(0.0),
		OutputCostPerImage: bifrost.Ptr(0.04),
	}
	in := extractCostInput(&schemas.BifrostResponse{ImageGenerationResponse: &schemas.BifrostImageGenerationResponse{
		Data:  []schemas.ImageData{{B64JSON: "aW1hZ2U="}},
		Usage: &schemas.ImageUsage{OutputTokens: 1300, OutputTokensDetails: &schemas.ImageTokenDetails{ReasoningTokens: bifrost.Ptr(200)}},
	}})
	assert.InDelta(t, 0.04, computeImageCostTotal(&perImage, in.imageUsage, "", "", serviceTier{}), 1e-12)
	// And the token-priced model prices the aggregate through the same path.
	assert.InDelta(t, withoutDetails, computeImageCostTotal(&p, in.imageUsage, "", "", serviceTier{}), 1e-12)

	// A modality breakdown still wins over the aggregate.
	withBreakdown := computeImageCostTotal(&p, &schemas.ImageUsage{
		OutputTokens:        1300,
		OutputTokensDetails: &schemas.ImageTokenDetails{ImageTokens: 1000, ReasoningTokens: bifrost.Ptr(200)},
	}, "", "", serviceTier{})
	assert.InDelta(t, 0.03, withBreakdown, 1e-12)
}

// TXE-6526: a response whose every image the vendor blocked (a 200 with content_filter and no
// data) delivered nothing, so per-image and per-pixel pricing charge no output. Tokens the vendor
// reported are still priced, and a response with no data and no content_filter (large response
// mode) keeps the one-image default.
func TestComputeImageCost_BlockedResponseChargesNoOutputImage(t *testing.T) {
	perImage := configstoreTables.TableModelPricing{
		InputCostPerToken:  bifrost.Ptr(0.000001),
		OutputCostPerToken: bifrost.Ptr(0.0),
		OutputCostPerImage: bifrost.Ptr(0.04),
	}
	price := func(p *configstoreTables.TableModelPricing, resp *schemas.BifrostImageGenerationResponse, size string) float64 {
		in := extractCostInput(&schemas.BifrostResponse{ImageGenerationResponse: resp})
		return computeImageCostTotal(p, in.imageUsage, size, "", serviceTier{})
	}
	blocked := func() *schemas.BifrostImageGenerationResponse {
		return &schemas.BifrostImageGenerationResponse{
			Data:          []schemas.ImageData{},
			ContentFilter: &schemas.ContentFilterInfo{FilteredCount: 1, Reasons: []string{"IMAGE_SAFETY"}},
			Usage:         &schemas.ImageUsage{InputTokens: 9, TotalTokens: 9, OutputImages: bifrost.Ptr(0)},
		}
	}

	// Blocked: only the 9 input tokens are charged.
	resp := blocked()
	assert.InDelta(t, 0.000009, price(&perImage, resp, ""), 1e-12)
	assert.Nil(t, resp.Usage.OutputTokensDetails, "the response's own usage is not written to")

	// Same usage with no content_filter: the one-image default still applies.
	unmarked := blocked()
	unmarked.ContentFilter = nil
	assert.InDelta(t, 0.040009, price(&perImage, unmarked, ""), 1e-12)

	// Delivered: one image charged.
	delivered := &schemas.BifrostImageGenerationResponse{
		Data:  []schemas.ImageData{{B64JSON: "aW1hZ2U="}},
		Usage: &schemas.ImageUsage{InputTokens: 9, TotalTokens: 9, OutputImages: bifrost.Ptr(1)},
	}
	assert.InDelta(t, 0.040009, price(&perImage, delivered, ""), 1e-12)

	// Per-pixel pricing is not charged on a block either.
	perPixel := configstoreTables.TableModelPricing{
		InputCostPerToken:  bifrost.Ptr(0.0),
		OutputCostPerToken: bifrost.Ptr(0.0),
		OutputCostPerPixel: bifrost.Ptr(0.00000001),
	}
	assert.InDelta(t, 0.0, price(&perPixel, blocked(), "1024x1024"), 1e-12)

	// A block that answered in text keeps its reported output tokens priced.
	tokenPriced := configstoreTables.TableModelPricing{
		InputCostPerToken:  bifrost.Ptr(0.0),
		OutputCostPerToken: bifrost.Ptr(0.00003),
		OutputCostPerImage: bifrost.Ptr(0.04),
	}
	textOnly := blocked()
	textOnly.Usage = &schemas.ImageUsage{OutputTokens: 7, OutputTokensDetails: &schemas.ImageTokenDetails{TextTokens: 7}, OutputImages: bifrost.Ptr(0)}
	assert.InDelta(t, 0.00021, price(&tokenPriced, textOnly, ""), 1e-12)
	aggregateOnly := blocked()
	aggregateOnly.Usage = &schemas.ImageUsage{OutputTokens: 7, OutputImages: bifrost.Ptr(0)}
	assert.InDelta(t, 0.00021, price(&tokenPriced, aggregateOnly, ""), 1e-12)
}

func TestComputeImageCost_PerImageDefaultsToOne(t *testing.T) {
	p := configstoreTables.TableModelPricing{
		OutputCostPerImage: bifrost.Ptr(0.052),
	}
	usage := &schemas.ImageUsage{} // No token details → defaults to 1 image
	cost := computeImageCostTotal(&p, usage, "", "", serviceTier{})
	assert.InDelta(t, 0.052, cost, 1e-12)
}

func TestComputeImageCost_TokenBased(t *testing.T) {
	p := configstoreTables.TableModelPricing{
		InputCostPerToken:  bifrost.Ptr(0.000005),
		OutputCostPerToken: bifrost.Ptr(0.000015),
	}
	usage := &schemas.ImageUsage{
		InputTokens:  1000,
		OutputTokens: 500,
		TotalTokens:  1500,
	}
	cost := computeImageCostTotal(&p, usage, "", "", serviceTier{})
	// 1000*0.000005 + 500*0.000015 = 0.005 + 0.0075 = 0.0125
	assert.InDelta(t, 0.0125, cost, 1e-12)
}

func TestComputeImageCost_TotalAbove200kButInputBelow200kUsesBaseRate(t *testing.T) {
	p := chatPricing(0.000003, 0.000015)
	p.InputCostPerTokenAbove200kTokens = bifrost.Ptr(0.000006)
	p.OutputCostPerTokenAbove200kTokens = bifrost.Ptr(0.00003)
	usage := &schemas.ImageUsage{
		InputTokens:  180000,
		OutputTokens: 30000,
		TotalTokens:  210000,
	}

	cost := computeImageCostTotal(&p, usage, "", "", serviceTier{})

	assert.InDelta(t, 180000*0.000003+30000*0.000015, cost, 1e-9)
}

func TestComputeImageCost_DerivesTierTokensFromTotalMinusOutputWhenInputMissing(t *testing.T) {
	p := chatPricing(0.000003, 0.000015)
	p.OutputCostPerTokenAbove200kTokens = bifrost.Ptr(0.00003)
	usage := &schemas.ImageUsage{
		OutputTokens: 30000,
		TotalTokens:  240000, // derived input = 210000, so output uses long-context rate
	}

	cost := computeImageCostTotal(&p, usage, "", "", serviceTier{})

	assert.InDelta(t, 30000*0.00003, cost, 1e-9)
}

func TestComputeImageCost_DoesNotUseBareTotalTokensAsInputTierTokens(t *testing.T) {
	p := chatPricing(0.000003, 0.000015)
	p.OutputCostPerImage = bifrost.Ptr(0.05)
	p.OutputCostPerTokenAbove200kTokens = bifrost.Ptr(0.00003)
	usage := &schemas.ImageUsage{
		TotalTokens: 210000, // no input/output split; total includes output, so do not use it as input
	}

	cost := computeImageCostTotal(&p, usage, "", "", serviceTier{})

	assert.InDelta(t, 0.05, cost, 1e-9)
}

func TestComputeImageCost_TokenBasedWithDetails(t *testing.T) {
	p := configstoreTables.TableModelPricing{
		InputCostPerToken:  bifrost.Ptr(0.000005),
		OutputCostPerToken: bifrost.Ptr(0.000015),
	}
	usage := &schemas.ImageUsage{
		InputTokens:  2000,
		OutputTokens: 1000,
		TotalTokens:  3000,
		InputTokensDetails: &schemas.ImageTokenDetails{
			TextTokens:  500,
			ImageTokens: 1500,
		},
		OutputTokensDetails: &schemas.ImageTokenDetails{
			TextTokens:  200,
			ImageTokens: 800,
		},
	}
	cost := computeImageCostTotal(&p, usage, "", "", serviceTier{})
	// Input: (500+1500)*0.000005 = 2000*0.000005 = 0.01
	// Output: (200+800)*0.000015 = 1000*0.000015 = 0.015
	// Total: 0.025
	assert.InDelta(t, 0.025, cost, 1e-12)
}

func TestComputeImageCost_NilUsage(t *testing.T) {
	p := configstoreTables.TableModelPricing{OutputCostPerImage: new(0.05)}
	assert.Equal(t, 0.0, computeImageCostTotal(&p, nil, "", "", serviceTier{}))
}

func TestComputeImageCost_InputAndOutputPerImage(t *testing.T) {
	p := configstoreTables.TableModelPricing{
		InputCostPerImage:  bifrost.Ptr(0.01),
		OutputCostPerImage: bifrost.Ptr(0.05),
	}
	usage := &schemas.ImageUsage{
		NumInputImages:      3,
		OutputTokensDetails: &schemas.ImageTokenDetails{NImages: 2},
	}
	cost := computeImageCostTotal(&p, usage, "", "", serviceTier{})
	// 3 input * $0.01 + 2 output * $0.05 = $0.03 + $0.10 = $0.13
	assert.InDelta(t, 0.13, cost, 1e-12)
}

func TestComputeImageCost_PerPixelOutput(t *testing.T) {
	p := configstoreTables.TableModelPricing{
		OutputCostPerPixel: bifrost.Ptr(0.000000019), // ~$0.02 for 1024x1024
	}
	usage := &schemas.ImageUsage{
		OutputTokensDetails: &schemas.ImageTokenDetails{NImages: 1},
	}
	cost := computeImageCostTotal(&p, usage, "1024x1024", "", serviceTier{})
	// 1024*1024 * 1 * 0.000000019 = 1048576 * 0.000000019 ≈ 0.01992
	assert.InDelta(t, 1048576*0.000000019, cost, 1e-12)
}

func TestComputeImageCost_PerPixelInputAndOutput(t *testing.T) {
	p := configstoreTables.TableModelPricing{
		InputCostPerPixel:  bifrost.Ptr(0.00000001),
		OutputCostPerPixel: bifrost.Ptr(0.00000002),
	}
	usage := &schemas.ImageUsage{
		NumInputImages:      2,
		OutputTokensDetails: &schemas.ImageTokenDetails{NImages: 3},
	}
	cost := computeImageCostTotal(&p, usage, "512x512", "", serviceTier{})
	pixels := 512 * 512 // 262144
	// Input: 262144 * 2 * 0.00000001 = 0.00524288
	// Output: 262144 * 3 * 0.00000002 = 0.01572864
	expected := float64(pixels*2)*0.00000001 + float64(pixels*3)*0.00000002
	assert.InDelta(t, expected, cost, 1e-12)
}

func TestComputeImageCost_TokensPreferredOverPixels(t *testing.T) {
	p := configstoreTables.TableModelPricing{
		InputCostPerToken:  bifrost.Ptr(0.000005),
		OutputCostPerToken: bifrost.Ptr(0.000015),
		InputCostPerPixel:  bifrost.Ptr(0.00000001),
		OutputCostPerPixel: bifrost.Ptr(0.00000002),
	}
	usage := &schemas.ImageUsage{
		InputTokens:  1000,
		OutputTokens: 500,
		TotalTokens:  1500,
	}
	cost := computeImageCostTotal(&p, usage, "1024x1024", "", serviceTier{})
	// Tokens should win: 1000*0.000005 + 500*0.000015 = 0.0125
	assert.InDelta(t, 0.0125, cost, 1e-12)
}

func TestComputeImageCost_PixelsPreferredOverPerImage(t *testing.T) {
	p := configstoreTables.TableModelPricing{
		OutputCostPerPixel: bifrost.Ptr(0.00000002),
		OutputCostPerImage: bifrost.Ptr(999.0), // should not be used
	}
	usage := &schemas.ImageUsage{
		OutputTokensDetails: &schemas.ImageTokenDetails{NImages: 1},
	}
	cost := computeImageCostTotal(&p, usage, "256x256", "", serviceTier{})
	// Per-pixel should win: 65536 * 1 * 0.00000002 = 0.00131072
	assert.InDelta(t, 65536*0.00000002, cost, 1e-12)
}

func TestComputeImageCost_PerPixelFallsBackToPerImage_WhenNoSize(t *testing.T) {
	p := configstoreTables.TableModelPricing{
		OutputCostPerPixel: bifrost.Ptr(0.00000002),
		OutputCostPerImage: bifrost.Ptr(0.05),
	}
	usage := &schemas.ImageUsage{
		OutputTokensDetails: &schemas.ImageTokenDetails{NImages: 2},
	}
	cost := computeImageCostTotal(&p, usage, "", "", serviceTier{})
	// No size → pixels=0, falls through to per-image: 2 * $0.05 = $0.10
	assert.InDelta(t, 0.10, cost, 1e-12)
}

func TestComputeImageCost_QualityBasedRates(t *testing.T) {
	usage := &schemas.ImageUsage{
		OutputTokensDetails: &schemas.ImageTokenDetails{NImages: 1},
	}
	// Quality-specific rates take precedence over base/size-tier
	p := configstoreTables.TableModelPricing{
		OutputCostPerImage:              bifrost.Ptr(0.01),
		OutputCostPerImageLowQuality:    bifrost.Ptr(0.02),
		OutputCostPerImageMediumQuality: bifrost.Ptr(0.03),
		OutputCostPerImageHighQuality:   bifrost.Ptr(0.04),
		OutputCostPerImageAutoQuality:   bifrost.Ptr(0.05),
	}
	assert.InDelta(t, 0.02, computeImageCostTotal(&p, usage, "", "low", serviceTier{}), 1e-12)
	assert.InDelta(t, 0.03, computeImageCostTotal(&p, usage, "", "medium", serviceTier{}), 1e-12)
	assert.InDelta(t, 0.04, computeImageCostTotal(&p, usage, "", "high", serviceTier{}), 1e-12)
	assert.InDelta(t, 0.05, computeImageCostTotal(&p, usage, "", "auto", serviceTier{}), 1e-12)
	// "hd" does not match any quality case so perImageRate stays nil → size/base fallback.
	assert.InDelta(t, 0.01, computeImageCostTotal(&p, usage, "", "hd", serviceTier{}), 1e-12)
	// Empty quality is treated as auto
	assert.InDelta(t, 0.05, computeImageCostTotal(&p, usage, "", "", serviceTier{}), 1e-12)
}

func TestComputeImageCost_MegapixelTier_SelectsCorrectBand(t *testing.T) {
	// Mirrors replicate/prunaai/p-image-upscale's real tier structure.
	p := configstoreTables.TableModelPricing{
		OutputCostPerImage:                  bifrost.Ptr(0.005),
		OutputCostPerImageAbove4Megapixels:  bifrost.Ptr(0.01),
		OutputCostPerImageAbove8Megapixels:  bifrost.Ptr(0.02),
		OutputCostPerImageAbove16Megapixels: bifrost.Ptr(0.04),
		OutputCostPerImageAbove32Megapixels: bifrost.Ptr(0.06),
		OutputCostPerImageAbove64Megapixels: bifrost.Ptr(0.12),
	}
	usage := &schemas.ImageUsage{
		OutputTokensDetails: &schemas.ImageTokenDetails{NImages: 1},
	}

	// 10MP output (between the 8MP and 16MP thresholds) → $0.02 tier.
	cost := computeImageCostTotal(&p, usage, "1x10000000", "", serviceTier{})
	assert.InDelta(t, 0.02, cost, 1e-12)

	// 2MP output (below the lowest 4MP threshold) → falls back to base rate.
	cost = computeImageCostTotal(&p, usage, "1x2000000", "", serviceTier{})
	assert.InDelta(t, 0.005, cost, 1e-12)

	// 70MP output (above every threshold) → top $0.12 tier.
	cost = computeImageCostTotal(&p, usage, "1x70000000", "", serviceTier{})
	assert.InDelta(t, 0.12, cost, 1e-12)
}

func TestComputeImageCost_MegapixelAndSquaredPixelTiers_Interleave(t *testing.T) {
	// 4096x4096 = 16,777,216px sits BETWEEN the 16MP and 32MP thresholds, not
	// between 8MP and 16MP — the tier ladder must be ordered by actual pixel
	// count, not by field family, or a model using both families would pick
	// the wrong tier.
	p := configstoreTables.TableModelPricing{
		OutputCostPerImage:                     bifrost.Ptr(0.005),
		OutputCostPerImageAbove16Megapixels:    bifrost.Ptr(0.04),
		OutputCostPerImageAbove4096x4096Pixels: bifrost.Ptr(0.30),
	}
	usage := &schemas.ImageUsage{
		OutputTokensDetails: &schemas.ImageTokenDetails{NImages: 1},
	}

	// 16.5MP: above the 16MP threshold but below 4096x4096 (16,777,216px) → $0.04.
	cost := computeImageCostTotal(&p, usage, "1x16500000", "", serviceTier{})
	assert.InDelta(t, 0.04, cost, 1e-12)

	// 17MP: above both thresholds → the larger (4096x4096) threshold wins → $0.30.
	cost = computeImageCostTotal(&p, usage, "1x17000000", "", serviceTier{})
	assert.InDelta(t, 0.30, cost, 1e-12)
}

func TestParseImagePixels(t *testing.T) {
	assert.Equal(t, 1048576, parseImagePixels("1024x1024"))
	assert.Equal(t, 262144, parseImagePixels("512x512"))
	assert.Equal(t, 1835008, parseImagePixels("1792x1024"))
	assert.Equal(t, 0, parseImagePixels(""))
	assert.Equal(t, 0, parseImagePixels("invalid"))
	assert.Equal(t, 0, parseImagePixels("1024"))
	assert.Equal(t, 0, parseImagePixels("0x1024"))
	assert.Equal(t, 0, parseImagePixels("-1x1024"))
}

// =========================================================================
// 7. computeVideoCost — unit tests
// =========================================================================

func TestComputeVideoCost_DurationBased(t *testing.T) {
	p := configstoreTables.TableModelPricing{
		InputCostPerToken:           bifrost.Ptr(0.000001),
		OutputCostPerToken:          bifrost.Ptr(0.0),
		OutputCostPerVideoPerSecond: bifrost.Ptr(0.001),
	}
	seconds := 30
	usage := &schemas.BifrostLLMUsage{PromptTokens: 500, TotalTokens: 500}
	cost := computeVideoCostTotal(&p, usage, &seconds, serviceTier{})
	// Output: 30 * 0.001 = 0.03
	// Input:  500 * 0.000001 = 0.0005
	// Total:  0.0305
	assert.InDelta(t, 0.0305, cost, 1e-12)
}

func TestComputeVideoCost_TotalAbove200kButInputBelow200kUsesBaseRate(t *testing.T) {
	p := chatPricing(0.000003, 0.000015)
	p.InputCostPerTokenAbove200kTokens = bifrost.Ptr(0.000006)
	p.OutputCostPerTokenAbove200kTokens = bifrost.Ptr(0.00003)
	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     180000,
		CompletionTokens: 30000,
		TotalTokens:      210000,
	}

	cost := computeVideoCostTotal(&p, usage, nil, serviceTier{})

	assert.InDelta(t, 180000*0.000003+30000*0.000015, cost, 1e-9)
}

func TestComputeVideoCost_OutputCostPerSecondFallback(t *testing.T) {
	p := configstoreTables.TableModelPricing{
		InputCostPerToken:   bifrost.Ptr(0.0),
		OutputCostPerToken:  bifrost.Ptr(0.0),
		OutputCostPerSecond: bifrost.Ptr(0.002),
	}
	seconds := 10
	cost := computeVideoCostTotal(&p, nil, &seconds, serviceTier{})
	assert.InDelta(t, 0.02, cost, 1e-12)
}

func TestComputeVideoCost_NilSeconds(t *testing.T) {
	p := configstoreTables.TableModelPricing{
		InputCostPerToken:           bifrost.Ptr(0.000001),
		OutputCostPerVideoPerSecond: bifrost.Ptr(0.001),
	}
	usage := &schemas.BifrostLLMUsage{PromptTokens: 1000}
	cost := computeVideoCostTotal(&p, usage, nil, serviceTier{})
	// Only input tokens: 1000 * 0.000001 = 0.001
	assert.InDelta(t, 0.001, cost, 1e-12)
}

// sizedVideoPricing mirrors sora-2-pro's published rates: $0.30/s at 720p,
// $0.50/s at 1024p, $0.70/s at 1080p, with the unbanded rate as the fallback.
func sizedVideoPricing() configstoreTables.TableModelPricing {
	return configstoreTables.TableModelPricing{
		OutputCostPerVideoPerSecond:      bifrost.Ptr(0.30),
		OutputCostPerVideoPerSecond480p:  bifrost.Ptr(0.10),
		OutputCostPerVideoPerSecond720p:  bifrost.Ptr(0.30),
		OutputCostPerVideoPerSecond1024p: bifrost.Ptr(0.50),
		OutputCostPerVideoPerSecond1080p: bifrost.Ptr(0.70),
		OutputCostPerVideoPerSecond4k:    bifrost.Ptr(0.60),
	}
}

func TestComputeVideoCost_ResolutionBands(t *testing.T) {
	p := sizedVideoPricing()
	seconds := 8

	for _, tc := range []struct {
		name string
		size string
		want float64
	}{
		{"720p landscape", "1280x720", 8 * 0.30},
		{"720p portrait", "720x1280", 8 * 0.30},
		{"1024p landscape", "1792x1024", 8 * 0.50},
		{"1024p portrait", "1024x1792", 8 * 0.50},
		{"1080p landscape", "1920x1080", 8 * 0.70},
		{"1080p portrait", "1080x1920", 8 * 0.70},
		{"4k", "3840x2160", 8 * 0.60},
		// No band matches 480, so this must not round up into the 720p rate.
		{"480p landscape", "854x480", 8 * 0.10},
		{"480p portrait", "480x854", 8 * 0.10},
		// 360 has no band, so it must not round up into the 480p rate.
		{"unlisted resolution falls back", "640x360", 8 * 0.30},
		{"malformed size falls back", "not-a-size", 8 * 0.30},
		{"absent size falls back", "", 8 * 0.30},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cost := computeVideoCostTotalSized(&p, nil, &seconds, tc.size, 1, serviceTier{})
			assert.InDelta(t, tc.want, cost, 1e-12)
		})
	}
}

// A pricing row carrying no banded rates must price exactly as it did before the
// bands existed.
func TestComputeVideoCost_NoBandedRatesMatchesUnbanded(t *testing.T) {
	p := configstoreTables.TableModelPricing{OutputCostPerVideoPerSecond: bifrost.Ptr(0.001)}
	seconds := 30
	assert.InDelta(t,
		computeVideoCostTotal(&p, nil, &seconds, serviceTier{}),
		computeVideoCostTotalSized(&p, nil, &seconds, "1920x1080", 1, serviceTier{}),
		1e-12)
}

// Seconds is one clip's duration, so a job returning several clips bills for each.
func TestComputeVideoCost_MultipleOutputsBillPerClip(t *testing.T) {
	p := sizedVideoPricing()
	seconds := 8

	assert.InDelta(t, 3*8*0.70, computeVideoCostTotalSized(&p, nil, &seconds, "1920x1080", 3, serviceTier{}), 1e-12)
	// A job that has not yet produced its clips still owes one clip's worth.
	assert.InDelta(t, 8*0.70, computeVideoCostTotalSized(&p, nil, &seconds, "1920x1080", 0, serviceTier{}), 1e-12)
}

// Completion tokens win over the per-second path, and the clip multiplier belongs
// only to the per-second branch — token usage already covers every output.
func TestComputeVideoCost_CompletionTokensIgnoreClipCount(t *testing.T) {
	p := sizedVideoPricing()
	p.OutputCostPerToken = bifrost.Ptr(0.00002)
	seconds := 8
	usage := &schemas.BifrostLLMUsage{CompletionTokens: 1000, TotalTokens: 1000}

	cost := computeVideoCostTotalSized(&p, usage, &seconds, "1920x1080", 4, serviceTier{})

	assert.InDelta(t, 1000*0.00002, cost, 1e-12)
}

// End-to-end through the catalog: a 1080p sora-2-pro job must bill at the 1080p
// rate rather than the base rate its model row carries.
func TestCalculateCost_VideoResolutionBandEndToEnd(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("sora-2-pro", "openai", "video_generation"): {
			Model: "sora-2-pro", Provider: "openai", Mode: "video_generation",
			OutputCostPerVideoPerSecond:      bifrost.Ptr(0.30),
			OutputCostPerVideoPerSecond1080p: bifrost.Ptr(0.70),
		},
	})

	resp := &schemas.BifrostResponse{
		VideoGenerationResponse: &schemas.BifrostVideoGenerationResponse{
			Status:  schemas.VideoStatusCompleted,
			Seconds: bifrost.Ptr("8"),
			Size:    "1920x1080",
			Videos:  []schemas.VideoOutput{{Type: schemas.VideoOutputTypeURL, URL: bifrost.Ptr("https://example.test/v.mp4")}},
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType: schemas.VideoGenerationRequest,
				RoutingInfo: routingInfoFor(schemas.OpenAI, "sora-2-pro"),
			},
		},
	}

	assert.InDelta(t, 5.60, s.CalculateCost(resp, nil), 1e-9)
}

// A Seedance task created with return_last_frame lists its final frame (image/jpeg) after the video.
// The frame is not a second clip, so it must not multiply the per-second charge.
func TestCalculateCost_VideoLastFrameImageIsNotAClip(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("seedance", "openai", "video_generation"): {
			Model: "seedance", Provider: "openai", Mode: "video_generation",
			OutputCostPerVideoPerSecond: bifrost.Ptr(0.30),
		},
	})

	resp := &schemas.BifrostResponse{
		VideoGenerationResponse: &schemas.BifrostVideoGenerationResponse{
			Status:  schemas.VideoStatusCompleted,
			Seconds: bifrost.Ptr("5"),
			Size:    "1280x720",
			Videos: []schemas.VideoOutput{
				{Type: schemas.VideoOutputTypeURL, URL: bifrost.Ptr("https://example.test/v.mp4"), ContentType: "video/mp4"},
				{Type: schemas.VideoOutputTypeURL, URL: bifrost.Ptr("https://example.test/last.jpeg"), ContentType: "image/jpeg"},
			},
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType: schemas.VideoGenerationRequest,
				RoutingInfo: routingInfoFor(schemas.OpenAI, "seedance"),
			},
		},
	}

	assert.InDelta(t, 1.50, s.CalculateCost(resp, nil), 1e-9)
}

func TestVideoDimensionsFromResponse_IgnoresImageOutputs(t *testing.T) {
	resp := &schemas.BifrostVideoGenerationResponse{
		Videos: []schemas.VideoOutput{
			{Type: schemas.VideoOutputTypeURL, ContentType: "video/mp4"},
			{Type: schemas.VideoOutputTypeURL, ContentType: "image/jpeg"},
		},
	}
	assert.Equal(t, 1, VideoDimensionsFromResponse(resp).OutputCount)
}

// =========================================================================
// 8. tieredInputRate / tieredOutputRate
// =========================================================================

func TestTieredInputRate_BelowThreshold(t *testing.T) {
	p := configstoreTables.TableModelPricing{
		InputCostPerToken:                bifrost.Ptr(0.000003),
		InputCostPerTokenAbove200kTokens: bifrost.Ptr(0.000006),
	}
	assert.Equal(t, 0.000003, tieredInputRate(&p, 100000, serviceTier{}))
}

func TestTieredInputRate_AboveThreshold(t *testing.T) {
	p := configstoreTables.TableModelPricing{
		InputCostPerToken:                bifrost.Ptr(0.000003),
		InputCostPerTokenAbove200kTokens: bifrost.Ptr(0.000006),
	}
	assert.Equal(t, 0.000006, tieredInputRate(&p, 210000, serviceTier{}))
}

func TestTieredInputRate_AboveThresholdNoTieredRate(t *testing.T) {
	p := configstoreTables.TableModelPricing{
		InputCostPerToken: bifrost.Ptr(0.000003),
	}
	// Falls back to base rate when tiered field is nil
	assert.Equal(t, 0.000003, tieredInputRate(&p, 300000, serviceTier{}))
}

func TestTieredOutputRate_AboveThreshold(t *testing.T) {
	p := configstoreTables.TableModelPricing{
		OutputCostPerToken:                bifrost.Ptr(0.000015),
		OutputCostPerTokenAbove200kTokens: bifrost.Ptr(0.00003),
	}
	assert.Equal(t, 0.00003, tieredOutputRate(&p, 250000, serviceTier{}))
}

// =========================================================================
// 9. extractCostInput — usage extraction
// =========================================================================

func TestExtractCostInput_ChatResponse(t *testing.T) {
	usage := &schemas.BifrostLLMUsage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150}
	resp := &schemas.BifrostResponse{
		ChatResponse: &schemas.BifrostChatResponse{Usage: usage},
	}
	input := extractCostInput(resp)
	require.NotNil(t, input.usage)
	assert.Equal(t, 100, input.usage.PromptTokens)
	assert.Equal(t, 50, input.usage.CompletionTokens)
}

func TestExtractCostInput_EmbeddingResponse(t *testing.T) {
	usage := &schemas.BifrostLLMUsage{PromptTokens: 200, TotalTokens: 200}
	resp := &schemas.BifrostResponse{
		EmbeddingResponse: &schemas.BifrostEmbeddingResponse{Usage: usage},
	}
	input := extractCostInput(resp)
	require.NotNil(t, input.usage)
	assert.Equal(t, 200, input.usage.PromptTokens)
}

func TestExtractCostInput_ImageResponse(t *testing.T) {
	imgUsage := &schemas.ImageUsage{InputTokens: 100, OutputTokens: 200, TotalTokens: 300}
	resp := &schemas.BifrostResponse{
		ImageGenerationResponse: &schemas.BifrostImageGenerationResponse{Usage: imgUsage},
	}
	input := extractCostInput(resp)
	assert.Nil(t, input.usage)
	require.NotNil(t, input.imageUsage)
	assert.Equal(t, 300, input.imageUsage.TotalTokens)
}

func TestExtractCostInput_TranscriptionWithSeconds(t *testing.T) {
	sec := 60.0
	resp := &schemas.BifrostResponse{
		TranscriptionResponse: &schemas.BifrostTranscriptionResponse{
			Usage: &schemas.TranscriptionUsage{
				Seconds:      &sec,
				InputTokens:  bifrost.Ptr(1000),
				OutputTokens: bifrost.Ptr(200),
				TotalTokens:  bifrost.Ptr(1200),
			},
		},
	}
	input := extractCostInput(resp)
	require.NotNil(t, input.usage)
	require.NotNil(t, input.audioSeconds)
	assert.Equal(t, 60.0, *input.audioSeconds)
	assert.Equal(t, 1000, input.usage.PromptTokens)
}

func TestExtractCostInput_SpeechResponse(t *testing.T) {
	resp := &schemas.BifrostResponse{
		SpeechResponse: &schemas.BifrostSpeechResponse{
			Usage: &schemas.SpeechUsage{
				InputTokens:  100,
				OutputTokens: 500,
				TotalTokens:  600,
			},
		},
	}
	input := extractCostInput(resp)
	require.NotNil(t, input.usage)
	assert.Equal(t, 100, input.usage.PromptTokens)
	assert.Equal(t, 500, input.usage.CompletionTokens)
	assert.Equal(t, 600, input.usage.TotalTokens)
}

func TestExtractCostInput_VideoResponse(t *testing.T) {
	sec := "15"
	resp := &schemas.BifrostResponse{
		VideoGenerationResponse: &schemas.BifrostVideoGenerationResponse{
			Seconds: &sec,
		},
	}
	input := extractCostInput(resp)
	require.NotNil(t, input.videoSeconds)
	assert.Equal(t, 15, *input.videoSeconds)
}

func TestExtractCostInput_VideoResponseInvalidSeconds(t *testing.T) {
	sec := "not-a-number"
	resp := &schemas.BifrostResponse{
		VideoGenerationResponse: &schemas.BifrostVideoGenerationResponse{
			Seconds: &sec,
		},
	}
	input := extractCostInput(resp)
	assert.Nil(t, input.videoSeconds)
}

// =========================================================================
// 10. Semantic cache billing (calculateCostWithCache)
// =========================================================================

func TestCalculateCost_SemanticCacheDirectHit(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gpt-4o", "openai", "chat"): {
			Model: "gpt-4o", Provider: "openai", Mode: "chat",
			InputCostPerToken: bifrost.Ptr(0.000005), OutputCostPerToken: bifrost.Ptr(0.000015),
		},
	})

	hitType := "direct"
	resp := &schemas.BifrostResponse{
		ChatResponse: &schemas.BifrostChatResponse{
			Usage: &schemas.BifrostLLMUsage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150},
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType: schemas.ChatCompletionRequest,
				RoutingInfo: routingInfoFor(schemas.OpenAI, "gpt-4o"),
				CacheDebug: &schemas.BifrostCacheMetadata{
					CacheHit: true,
					HitType:  &hitType,
				},
			},
		},
	}

	cost := s.CalculateCost(resp, nil)
	assert.Equal(t, 0.0, cost)
}

func TestCalculateCost_SemanticCacheSemanticHit(t *testing.T) {
	embProvider := "openai"
	embModel := "text-embedding-3-small"
	embTokens := 500

	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gpt-4o", "openai", "chat"): {
			Model: "gpt-4o", Provider: "openai", Mode: "chat",
			InputCostPerToken: bifrost.Ptr(0.000005), OutputCostPerToken: bifrost.Ptr(0.000015),
		},
		makeKey("text-embedding-3-small", "openai", "embedding"): {
			Model: "text-embedding-3-small", Provider: "openai", Mode: "embedding",
			InputCostPerToken: bifrost.Ptr(0.00000002),
		},
	})

	hitType := "semantic"
	resp := &schemas.BifrostResponse{
		ChatResponse: &schemas.BifrostChatResponse{
			Usage: &schemas.BifrostLLMUsage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150},
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType: schemas.ChatCompletionRequest,
				RoutingInfo: routingInfoFor(schemas.OpenAI, "gpt-4o"),
				CacheDebug: &schemas.BifrostCacheMetadata{
					CacheHit:     true,
					HitType:      &hitType,
					ProviderUsed: &embProvider,
					ModelUsed:    &embModel,
					InputTokens:  &embTokens,
				},
			},
		},
	}

	cost := s.CalculateCost(resp, nil)
	// Only embedding cost: 500 * 0.00000002 = 0.00001
	assert.InDelta(t, 0.00001, cost, 1e-12)
}

func TestCalculateCost_SemanticCacheMiss(t *testing.T) {
	embProvider := "openai"
	embModel := "text-embedding-3-small"
	embTokens := 500

	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gpt-4o", "openai", "chat"): {
			Model: "gpt-4o", Provider: "openai", Mode: "chat",
			InputCostPerToken: bifrost.Ptr(0.000005), OutputCostPerToken: bifrost.Ptr(0.000015),
		},
		makeKey("text-embedding-3-small", "openai", "embedding"): {
			Model: "text-embedding-3-small", Provider: "openai", Mode: "embedding",
			InputCostPerToken: bifrost.Ptr(0.00000002),
		},
	})

	resp := &schemas.BifrostResponse{
		ChatResponse: &schemas.BifrostChatResponse{
			Usage: &schemas.BifrostLLMUsage{PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500},
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType: schemas.ChatCompletionRequest,
				RoutingInfo: routingInfoFor(schemas.OpenAI, "gpt-4o"),
				CacheDebug: &schemas.BifrostCacheMetadata{
					CacheHit:     false,
					ProviderUsed: &embProvider,
					ModelUsed:    &embModel,
					InputTokens:  &embTokens,
				},
			},
		},
	}

	cost := s.CalculateCost(resp, nil)
	// Base cost: 1000*0.000005 + 500*0.000015 = 0.005 + 0.0075 = 0.0125
	// Embedding cost: 500 * 0.00000002 = 0.00001
	// Total: 0.01251
	assert.InDelta(t, 0.01251, cost, 1e-12)
}

func TestCalculateCost_SemanticCacheHitNoEmbeddingInfo(t *testing.T) {
	s := testStoreWithPricing(nil)

	resp := &schemas.BifrostResponse{
		ChatResponse: &schemas.BifrostChatResponse{
			ExtraFields: schemas.BifrostResponseExtraFields{
				CacheDebug: &schemas.BifrostCacheMetadata{
					CacheHit: true,
					// No ProviderUsed, ModelUsed, InputTokens
				},
			},
		},
	}

	cost := s.CalculateCost(resp, nil)
	assert.Equal(t, 0.0, cost)
}

// TestCalculateCostBreakdown_SemanticCacheHitIsAdditional verifies a semantic
// cache hit's embedding-lookup cost lands on the additional side
// (SemanticCacheCost), not folded into the request's input.
func TestCalculateCostBreakdown_SemanticCacheHitIsAdditional(t *testing.T) {
	embProvider := "openai"
	embModel := "text-embedding-3-small"
	embTokens := 500

	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gpt-4o", "openai", "chat"): {
			Model: "gpt-4o", Provider: "openai", Mode: "chat",
			InputCostPerToken: bifrost.Ptr(0.000005), OutputCostPerToken: bifrost.Ptr(0.000015),
		},
		makeKey("text-embedding-3-small", "openai", "embedding"): {
			Model: "text-embedding-3-small", Provider: "openai", Mode: "embedding",
			InputCostPerToken: bifrost.Ptr(0.00000002),
		},
	})

	hitType := "semantic"
	resp := &schemas.BifrostResponse{
		ChatResponse: &schemas.BifrostChatResponse{
			Usage: &schemas.BifrostLLMUsage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150},
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType: schemas.ChatCompletionRequest,
				RoutingInfo: routingInfoFor(schemas.OpenAI, "gpt-4o"),
				CacheDebug: &schemas.BifrostCacheMetadata{
					CacheHit: true, HitType: &hitType,
					ProviderUsed: &embProvider, ModelUsed: &embModel, InputTokens: &embTokens,
				},
			},
		},
	}

	bd := s.CalculateCostBreakdown(resp, nil)
	require.NotNil(t, bd)
	require.NotNil(t, bd.AdditionalCostDetails)
	// Only the embedding lookup: 500 * 0.00000002 = 0.00001, all on the additional side.
	assert.InDelta(t, 0.00001, bd.AdditionalCost, 1e-12)
	assert.InDelta(t, 0.00001, bd.AdditionalCostDetails.SemanticCacheCost, 1e-12)
	assert.Zero(t, bd.InputCost)
	assert.InDelta(t, 0.00001, bd.TotalCost, 1e-12)
}

// TestCalculateCostBreakdown_SemanticCacheMissAddsAdditional verifies a cache
// miss prices the full LLM call plus the embedding lookup, with the lookup on
// the additional side and the three sides reconciling to the total.
func TestCalculateCostBreakdown_SemanticCacheMissAddsAdditional(t *testing.T) {
	embProvider := "openai"
	embModel := "text-embedding-3-small"
	embTokens := 500

	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gpt-4o", "openai", "chat"): {
			Model: "gpt-4o", Provider: "openai", Mode: "chat",
			InputCostPerToken: bifrost.Ptr(0.000005), OutputCostPerToken: bifrost.Ptr(0.000015),
		},
		makeKey("text-embedding-3-small", "openai", "embedding"): {
			Model: "text-embedding-3-small", Provider: "openai", Mode: "embedding",
			InputCostPerToken: bifrost.Ptr(0.00000002),
		},
	})

	resp := &schemas.BifrostResponse{
		ChatResponse: &schemas.BifrostChatResponse{
			Usage: &schemas.BifrostLLMUsage{PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500},
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType: schemas.ChatCompletionRequest,
				RoutingInfo: routingInfoFor(schemas.OpenAI, "gpt-4o"),
				CacheDebug: &schemas.BifrostCacheMetadata{
					CacheHit:     false,
					ProviderUsed: &embProvider, ModelUsed: &embModel, InputTokens: &embTokens,
				},
			},
		},
	}

	bd := s.CalculateCostBreakdown(resp, nil)
	require.NotNil(t, bd)
	require.NotNil(t, bd.AdditionalCostDetails)
	// Base: 1000*5e-6 + 500*1.5e-5 = 0.0125. Embedding: 500*2e-8 = 0.00001 (additional).
	assert.InDelta(t, 0.005, bd.InputCost, 1e-12)
	assert.InDelta(t, 0.0075, bd.OutputCost, 1e-12)
	assert.InDelta(t, 0.00001, bd.AdditionalCost, 1e-12)
	assert.InDelta(t, 0.00001, bd.AdditionalCostDetails.SemanticCacheCost, 1e-12)
	assert.InDelta(t, 0.01251, bd.TotalCost, 1e-12)
	assert.InDelta(t, bd.TotalCost, bd.InputCost+bd.OutputCost+bd.AdditionalCost, 1e-12)
}

// TestCalculateCostBreakdown_RoutingEmbeddingIsItsOwnDetail verifies the routing
// classification embed lands on the additional side under its own detail line,
// so a breakdown that also carries a guardrail or cache sidecar stays
// attributable to the call that incurred each part. The scalar CalculateCost is
// pinned alongside the breakdown: callers that only read the total must see the
// same billing decision the categories describe.
func TestCalculateCostBreakdown_RoutingEmbeddingIsItsOwnDetail(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gpt-4o", "openai", "chat"): {
			Model: "gpt-4o", Provider: "openai", Mode: "chat",
			InputCostPerToken: bifrost.Ptr(0.000005), OutputCostPerToken: bifrost.Ptr(0.000015),
		},
		makeKey("text-embedding-3-small", "openai", "embedding"): {
			Model: "text-embedding-3-small", Provider: "openai", Mode: "embedding",
			InputCostPerToken: bifrost.Ptr(0.00000002),
		},
	})

	newResponse := func(countTowardBudgets bool) *schemas.BifrostResponse {
		return &schemas.BifrostResponse{
			ChatResponse: &schemas.BifrostChatResponse{
				Usage: &schemas.BifrostLLMUsage{PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500},
				ExtraFields: schemas.BifrostResponseExtraFields{
					RequestType: schemas.ChatCompletionRequest,
					RoutingInfo: routingInfoFor(schemas.OpenAI, "gpt-4o"),
					RoutingMetadata: &schemas.BifrostRoutingMetadata{
						Calls: []schemas.BifrostRoutingCall{embedRoutingCall(500, countTowardBudgets)},
					},
				},
			},
		}
	}

	// Base: 1000*5e-6 + 500*1.5e-5 = 0.0125. Routing embed: 500*2e-8 = 0.00001.
	assert.InDelta(t, 0.01251, s.CalculateCost(newResponse(true), nil), 1e-12)

	bd := s.CalculateCostBreakdown(newResponse(true), nil)
	require.NotNil(t, bd)
	require.NotNil(t, bd.AdditionalCostDetails)
	assert.InDelta(t, 0.005, bd.InputCost, 1e-12)
	assert.InDelta(t, 0.0075, bd.OutputCost, 1e-12)
	assert.InDelta(t, 0.00001, bd.AdditionalCost, 1e-12)
	assert.InDelta(t, 0.00001, bd.AdditionalCostDetails.RoutingCost, 1e-12)
	assert.Zero(t, bd.AdditionalCostDetails.GuardrailCost, "a routing embed must not be reported as a guardrail call")
	assert.InDelta(t, 0.01251, bd.TotalCost, 1e-12)
	assert.InDelta(t, bd.TotalCost, bd.InputCost+bd.OutputCost+bd.AdditionalCost, 1e-12)

	// The detail line tracks the cost actually charged: a request that never
	// opted routing into budget attribution carries neither, and the embed drops
	// out of the total rather than being billed silently.
	assert.InDelta(t, 0.0125, s.CalculateCost(newResponse(false), nil), 1e-12)

	optedOut := s.CalculateCostBreakdown(newResponse(false), nil)
	require.NotNil(t, optedOut)
	assert.Zero(t, optedOut.AdditionalCost)
	if optedOut.AdditionalCostDetails != nil {
		assert.Zero(t, optedOut.AdditionalCostDetails.RoutingCost)
	}
}

// TestCalculateCostBreakdown_SemanticCacheHitAddsRequestSurcharge verifies the
// embedding model's flat CostPerRequest fee is included in the semantic-cache
// lookup cost on a hit, not just the per-token charge.
func TestCalculateCostBreakdown_SemanticCacheHitAddsRequestSurcharge(t *testing.T) {
	embProvider := "openai"
	embModel := "text-embedding-3-small"
	embTokens := 500

	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gpt-4o", "openai", "chat"): {
			Model: "gpt-4o", Provider: "openai", Mode: "chat",
			InputCostPerToken: bifrost.Ptr(0.000005), OutputCostPerToken: bifrost.Ptr(0.000015),
		},
		makeKey("text-embedding-3-small", "openai", "embedding"): {
			Model: "text-embedding-3-small", Provider: "openai", Mode: "embedding",
			InputCostPerToken: bifrost.Ptr(0.00000002), CostPerRequest: bifrost.Ptr(0.001),
		},
	})

	hitType := "semantic"
	resp := &schemas.BifrostResponse{
		ChatResponse: &schemas.BifrostChatResponse{
			Usage: &schemas.BifrostLLMUsage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150},
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType: schemas.ChatCompletionRequest,
				RoutingInfo: routingInfoFor(schemas.OpenAI, "gpt-4o"),
				CacheDebug: &schemas.BifrostCacheMetadata{
					CacheHit: true, HitType: &hitType,
					ProviderUsed: &embProvider, ModelUsed: &embModel, InputTokens: &embTokens,
				},
			},
		},
	}

	bd := s.CalculateCostBreakdown(resp, nil)
	require.NotNil(t, bd)
	require.NotNil(t, bd.AdditionalCostDetails)
	// Lookup: 500*2e-8 + 0.001 request fee = 0.00101, all on the additional side.
	assert.InDelta(t, 0.00101, bd.AdditionalCost, 1e-12)
	assert.InDelta(t, 0.00101, bd.AdditionalCostDetails.SemanticCacheCost, 1e-12)
	assert.Zero(t, bd.InputCost)
	assert.InDelta(t, 0.00101, bd.TotalCost, 1e-12)
}

// TestCalculateCostBreakdown_SemanticCacheMissAddsRequestSurcharge verifies the
// embedding CostPerRequest fee is included in the lookup cost on a miss, on top
// of the full LLM call, with the three sides reconciling to the total.
func TestCalculateCostBreakdown_SemanticCacheMissAddsRequestSurcharge(t *testing.T) {
	embProvider := "openai"
	embModel := "text-embedding-3-small"
	embTokens := 500

	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gpt-4o", "openai", "chat"): {
			Model: "gpt-4o", Provider: "openai", Mode: "chat",
			InputCostPerToken: bifrost.Ptr(0.000005), OutputCostPerToken: bifrost.Ptr(0.000015),
		},
		makeKey("text-embedding-3-small", "openai", "embedding"): {
			Model: "text-embedding-3-small", Provider: "openai", Mode: "embedding",
			InputCostPerToken: bifrost.Ptr(0.00000002), CostPerRequest: bifrost.Ptr(0.001),
		},
	})

	resp := &schemas.BifrostResponse{
		ChatResponse: &schemas.BifrostChatResponse{
			Usage: &schemas.BifrostLLMUsage{PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500},
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType: schemas.ChatCompletionRequest,
				RoutingInfo: routingInfoFor(schemas.OpenAI, "gpt-4o"),
				CacheDebug: &schemas.BifrostCacheMetadata{
					CacheHit:     false,
					ProviderUsed: &embProvider, ModelUsed: &embModel, InputTokens: &embTokens,
				},
			},
		},
	}

	bd := s.CalculateCostBreakdown(resp, nil)
	require.NotNil(t, bd)
	require.NotNil(t, bd.AdditionalCostDetails)
	// Base: 1000*5e-6 + 500*1.5e-5 = 0.0125. Embedding: 500*2e-8 + 0.001 = 0.00101 (additional).
	assert.InDelta(t, 0.005, bd.InputCost, 1e-12)
	assert.InDelta(t, 0.0075, bd.OutputCost, 1e-12)
	assert.InDelta(t, 0.00101, bd.AdditionalCost, 1e-12)
	assert.InDelta(t, 0.00101, bd.AdditionalCostDetails.SemanticCacheCost, 1e-12)
	assert.InDelta(t, 0.01351, bd.TotalCost, 1e-12)
	assert.InDelta(t, bd.TotalCost, bd.InputCost+bd.OutputCost+bd.AdditionalCost, 1e-12)
}

// TestCalculateCostAddsGuardrailJudgeCost verifies judge cost is additive to the main request.
func TestCalculateCostAddsGuardrailJudgeCost(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gpt-4o", "openai", "chat"): {
			Model: "gpt-4o", Provider: "openai", Mode: "chat",
			InputCostPerToken: bifrost.Ptr(0.000001), OutputCostPerToken: bifrost.Ptr(0.000002),
		},
		makeKey("claude-judge", "anthropic", "chat"): {
			Model: "claude-judge", Provider: "anthropic", Mode: "chat",
			InputCostPerToken: bifrost.Ptr(0.000003), OutputCostPerToken: bifrost.Ptr(0.000004),
		},
	})
	resp := makeChatResponse(schemas.OpenAI, "gpt-4o", &schemas.BifrostLLMUsage{
		PromptTokens:     100,
		CompletionTokens: 50,
		TotalTokens:      150,
	})
	resp.GetExtraFields().GuardrailDebug = &schemas.BifrostGuardrailMetadata{
		JudgeCalls: []schemas.BifrostGuardrailJudgeCall{{
			JudgeProvider:    schemas.Anthropic,
			JudgeModel:       "claude-judge",
			PromptTokens:     20,
			CompletionTokens: 5,
			TotalTokens:      25,
		}},
	}

	// Main: 100*0.000001 + 50*0.000002 = 0.0002.
	// Judge: 20*0.000003 + 5*0.000004 = 0.00008.
	assert.InDelta(t, 0.00028, s.CalculateCost(resp, nil), 1e-12)

	// The judge cost lands on the additional side, not input/output, and the
	// three reconcile to the total.
	bd := s.CalculateCostBreakdown(resp, nil)
	require.NotNil(t, bd)
	assert.InDelta(t, 0.0001, bd.InputCost, 1e-12)
	assert.InDelta(t, 0.0001, bd.OutputCost, 1e-12)
	assert.InDelta(t, 0.00008, bd.AdditionalCost, 1e-12)
	require.NotNil(t, bd.AdditionalCostDetails)
	assert.InDelta(t, 0.00008, bd.AdditionalCostDetails.GuardrailCost, 1e-12)
	assert.InDelta(t, bd.TotalCost, bd.InputCost+bd.OutputCost+bd.AdditionalCost, 1e-12)
}

// TestCalculateGuardrailCostPreservesUsageDetails verifies cache-aware judge pricing uses detailed usage.
func TestCalculateGuardrailCostPreservesUsageDetails(t *testing.T) {
	pricing := chatPricing(0.00001, 0)
	pricing.Model = "gpt-judge"
	pricing.Provider = "openai"
	pricing.Mode = "chat"
	pricing.CacheReadInputTokenCost = bifrost.Ptr(0.000001)
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gpt-judge", "openai", "chat"): pricing,
	})

	cost := s.CalculateGuardrailCost(&schemas.BifrostGuardrailMetadata{
		JudgeCalls: []schemas.BifrostGuardrailJudgeCall{{
			JudgeProvider: schemas.OpenAI,
			JudgeModel:    "gpt-judge",
			PromptTokens:  10,
			PromptTokensDetails: &schemas.ChatPromptTokensDetails{
				CachedReadTokens: 8,
			},
			TotalTokens: 10,
		}},
	}, nil)

	// Two uncached tokens at $10/M plus eight cached reads at $1/M.
	assert.InDelta(t, 0.000028, cost, 1e-12)
}

// TestCalculateCostDirectCacheHitStillBillsGuardrail verifies cache hits do not hide judge spend.
func TestCalculateCostDirectCacheHitStillBillsGuardrail(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gpt-4o-mini", "openai", "chat"): {
			Model: "gpt-4o-mini", Provider: "openai", Mode: "chat",
			InputCostPerToken: bifrost.Ptr(0.000001), OutputCostPerToken: bifrost.Ptr(0.000002),
		},
	})
	hitType := "direct"
	resp := makeChatResponse(schemas.OpenAI, "cached-model", nil)
	resp.GetExtraFields().CacheDebug = &schemas.BifrostCacheMetadata{CacheHit: true, HitType: &hitType}
	resp.GetExtraFields().GuardrailDebug = &schemas.BifrostGuardrailMetadata{
		JudgeCalls: []schemas.BifrostGuardrailJudgeCall{{
			JudgeProvider:    schemas.OpenAI,
			JudgeModel:       "gpt-4o-mini",
			PromptTokens:     10,
			CompletionTokens: 5,
			TotalTokens:      15,
		}},
	}

	assert.InDelta(t, 0.00002, s.CalculateCost(resp, nil), 1e-12)
}

// TestCalculateGuardrailCostUsesJudgeProviderWithoutCallerSelectedKey verifies
// judge pricing retains virtual-key attribution while excluding the caller's
// selected provider key from scoped override matching.
func TestCalculateGuardrailCostUsesJudgeProviderWithoutCallerSelectedKey(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("claude-judge", "anthropic", "chat"): {
			Model:              "claude-judge",
			Provider:           "anthropic",
			Mode:               "chat",
			InputCostPerToken:  bifrost.Ptr(1.0),
			OutputCostPerToken: bifrost.Ptr(0.0),
		},
	})

	virtualKeyID := "vk-caller"
	callerSelectedKeyID := "openai-key"
	judgeProviderID := "anthropic"
	require.NoError(t, s.SetOverrides([]configstoreTables.TablePricingOverride{
		{
			ID:               "judge-vk-provider",
			ScopeKind:        string(ScopeKindVirtualKeyProvider),
			VirtualKeyID:     &virtualKeyID,
			ProviderID:       &judgeProviderID,
			MatchType:        string(MatchTypeExact),
			Pattern:          "claude-judge",
			RequestTypes:     []schemas.RequestType{schemas.ChatCompletionRequest},
			PricingPatchJSON: `{"input_cost_per_token":3}`,
		},
		{
			ID:               "caller-vk-provider-key",
			ScopeKind:        string(ScopeKindVirtualKeyProviderKey),
			VirtualKeyID:     &virtualKeyID,
			ProviderID:       &judgeProviderID,
			ProviderKeyID:    &callerSelectedKeyID,
			MatchType:        string(MatchTypeExact),
			Pattern:          "claude-judge",
			RequestTypes:     []schemas.RequestType{schemas.ChatCompletionRequest},
			PricingPatchJSON: `{"input_cost_per_token":99}`,
		},
	}))

	cost := s.CalculateGuardrailCost(&schemas.BifrostGuardrailMetadata{
		JudgeCalls: []schemas.BifrostGuardrailJudgeCall{{
			JudgeProvider: schemas.Anthropic,
			JudgeModel:    "claude-judge",
			PromptTokens:  10,
			TotalTokens:   10,
		}},
	}, &LookupScopes{
		VirtualKeyID:  virtualKeyID,
		SelectedKeyID: callerSelectedKeyID,
		Provider:      "openai",
	})

	// The virtual-key + Anthropic override applies (10 * 3). Reusing the
	// caller's selected key would instead select the 99/token override.
	assert.InDelta(t, 30.0, cost, 1e-12)
}

// =========================================================================
// 11. CalculateCost integration — end-to-end
// =========================================================================

func TestCalculateCost_NilResponse(t *testing.T) {
	s := testStoreWithPricing(nil)
	assert.Equal(t, 0.0, s.CalculateCost(nil, nil))
}

func TestCalculateCost_ProviderComputedCostPassthrough(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gpt-4o", "openai", "chat"): chatPricing(0.000005, 0.000015),
	})

	resp := makeChatResponse(schemas.OpenAI, "gpt-4o", &schemas.BifrostLLMUsage{
		PromptTokens:     1000,
		CompletionTokens: 500,
		TotalTokens:      1500,
		Cost: &schemas.BifrostCost{
			TotalCost: 0.99, // Provider already calculated
		},
	})

	cost := s.CalculateCost(resp, nil)
	assert.Equal(t, 0.99, cost)
}

// Image usage hangs off imageUsage, never input.usage, so it needs its own
// provider-cost short circuit. xAI reports cost_in_usd_ticks, normalized into
// Cost by the provider before billing sees it.
func TestCalculateCost_ImageProviderComputedCostPassthrough(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("grok-imagine-image-quality", "xai", "image_generation"): {
			Model: "grok-imagine-image-quality", Provider: "xai", Mode: "image_generation",
			OutputCostPerImage: bifrost.Ptr(0.5), // deliberately unlike the reported cost
		},
	})

	usage := &schemas.ImageUsage{CostInUsdTicks: bifrost.Ptr(int64(200000000))}
	usage.NormalizeProviderCost()

	resp := makeImageResponse(schemas.XAI, "grok-imagine-image-quality", usage)
	resp.ImageGenerationResponse.Data = []schemas.ImageData{{URL: "https://imgen.x.ai/x.jpeg"}}

	assert.Equal(t, 0.02, s.CalculateCost(resp, nil))
}

// Transcription usage is rebuilt into a new BifrostLLMUsage before pricing, so the
// provider-reported cost has to be carried over for the short circuit to see it.
// The usage is OpenRouter's voxtral response: billed per second, with a fixed
// prompt token count that says nothing about the audio length.
func TestCalculateCost_TranscriptionProviderComputedCostPassthrough(t *testing.T) {
	pricing := configstoreTables.TableModelPricing{
		Model:                      "mistralai/voxtral-mini-transcribe-2602",
		Provider:                   "openrouter",
		Mode:                       "audio_transcription",
		InputCostPerAudioPerSecond: bifrost.Ptr(0.0001), // deliberately unlike the reported cost
	}
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey(pricing.Model, pricing.Provider, pricing.Mode): pricing,
	})

	var usage schemas.TranscriptionUsage
	require.NoError(t, json.Unmarshal([]byte(`{"seconds":31,"total_tokens":202,"input_tokens":6,"output_tokens":196,"cost":0.001705}`), &usage))
	extra := schemas.BifrostResponseExtraFields{
		RequestType: schemas.TranscriptionRequest,
		RoutingInfo: routingInfoFor(schemas.OpenRouter, pricing.Model),
	}

	resp := &schemas.BifrostResponse{
		TranscriptionResponse: &schemas.BifrostTranscriptionResponse{Usage: &usage, ExtraFields: extra},
	}
	assert.InDelta(t, 0.001705, s.CalculateCost(resp, nil), 1e-12)

	extra.RequestType = schemas.TranscriptionStreamRequest
	streamResp := &schemas.BifrostResponse{
		TranscriptionStreamResponse: &schemas.BifrostTranscriptionStreamResponse{Usage: &usage, ExtraFields: extra},
	}
	assert.InDelta(t, 0.001705, s.CalculateCost(streamResp, nil), 1e-12)
}

// Video/3D provider-reported cost hangs off VideoGenerationResponse.Usage.Cost. Runware reports an
// exact per-task price (and 3D has no datasheet rate), so the reported cost must win verbatim.
func TestCalculateCost_VideoProviderComputedCostPassthrough(t *testing.T) {
	s := testStoreWithPricing(nil) // no datasheet entry on purpose — 3D has no rate

	resp := &schemas.BifrostResponse{
		VideoGenerationResponse: &schemas.BifrostVideoGenerationResponse{
			Usage: &schemas.VideoUsage{Cost: &schemas.BifrostCost{TotalCost: 0.5}},
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType: schemas.VideoGenerationRequest,
				RoutingInfo: routingInfoFor(schemas.Runware, "tripo:v3.1@0"),
			},
		},
	}

	assert.Equal(t, 0.5, s.CalculateCost(resp, nil))
}

// The same payload polled back through a retrieve must not be billed: Runware echoes the task cost
// on every getResponse once it has succeeded, so billing the retrieve would charge the job again
// on each poll.
func TestCalculateCost_VideoRetrieveIsNotBilled(t *testing.T) {
	s := testStoreWithPricing(nil)

	resp := &schemas.BifrostResponse{
		VideoGenerationResponse: &schemas.BifrostVideoGenerationResponse{
			Usage: &schemas.VideoUsage{Cost: &schemas.BifrostCost{TotalCost: 0.5}},
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType: schemas.VideoRetrieveRequest,
				RoutingInfo: routingInfoFor(schemas.Runware, "tripo:v3.1@0"),
			},
		},
	}

	assert.Zero(t, s.CalculateCost(resp, nil))
}

// Passthrough responses can carry a provider-reported cost (e.g. Runware's per-task cost read from
// the raw body). It must win verbatim, with no datasheet entry required.
func TestCalculateCost_PassthroughProviderComputedCost(t *testing.T) {
	s := testStoreWithPricing(nil) // no datasheet entry — raw passthrough has no rate

	resp := &schemas.BifrostResponse{
		PassthroughResponse: &schemas.BifrostPassthroughResponse{
			PassthroughUsage: &schemas.BifrostPassthroughUsage{Cost: &schemas.BifrostCost{TotalCost: 0.5}},
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType:     schemas.PassthroughRequest,
				PassthroughPath: "/v1",
				RoutingInfo:     routingInfoFor(schemas.Runware, "tripo:v3.1@0"),
			},
		},
	}

	assert.Equal(t, 0.5, s.CalculateCost(resp, nil))
}

// passthroughUsageToCostInput must not mutate the caller's usage when attaching a provider-reported
// cost — the cost path is read-only (mirrors the DeepCopy invariant in extractCostInput). Without
// the defensive copy, assigning Cost would write back onto the shared response's LLMUsage.
func TestPassthroughUsageToCostInput_DoesNotMutateSourceUsage(t *testing.T) {
	su := &schemas.BifrostPassthroughUsage{
		LLMUsage: &schemas.BifrostLLMUsage{PromptTokens: 100},
		Cost:     &schemas.BifrostCost{TotalCost: 0.5},
	}

	input := passthroughUsageToCostInput(su)

	// The returned input carries the provider-reported cost...
	require.NotNil(t, input.usage)
	require.NotNil(t, input.usage.Cost)
	assert.Equal(t, 0.5, input.usage.Cost.TotalCost)
	// ...but the caller's source usage must be left untouched.
	assert.Nil(t, su.LLMUsage.Cost, "passthroughUsageToCostInput must not mutate su.LLMUsage")
}

// Without a reported cost the datasheet still prices the request.
func TestCalculateCost_ImageFallsBackToDatasheetWithoutReportedCost(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("grok-imagine-image-quality", "xai", "image_generation"): {
			Model: "grok-imagine-image-quality", Provider: "xai", Mode: "image_generation",
			OutputCostPerImage: bifrost.Ptr(0.5),
		},
	})

	usage := &schemas.ImageUsage{}
	usage.NormalizeProviderCost()

	resp := makeImageResponse(schemas.XAI, "grok-imagine-image-quality", usage)
	resp.ImageGenerationResponse.Data = []schemas.ImageData{{URL: "https://imgen.x.ai/x.jpeg"}}

	assert.Equal(t, 0.5, s.CalculateCost(resp, nil))
}

func TestCalculateCost_NoUsageData(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gpt-4o", "openai", "chat"): chatPricing(0.000005, 0.000015),
	})

	resp := makeChatResponse(schemas.OpenAI, "gpt-4o", nil)
	cost := s.CalculateCost(resp, nil)
	assert.Equal(t, 0.0, cost)
}

func TestCalculateCost_ChatCompletion_GPT4o(t *testing.T) {
	// GPT-4o: $5/M input, $15/M output, cache_read=$0.5/M
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gpt-4o", "openai", "chat"): {
			Model: "gpt-4o", Provider: "openai", Mode: "chat",
			InputCostPerToken:       bifrost.Ptr(0.000005),
			OutputCostPerToken:      bifrost.Ptr(0.000015),
			CacheReadInputTokenCost: bifrost.Ptr(0.0000005),
		},
	})

	resp := makeChatResponse(schemas.OpenAI, "gpt-4o", &schemas.BifrostLLMUsage{
		PromptTokens:     10000,
		CompletionTokens: 2000,
		TotalTokens:      12000,
	})

	cost := s.CalculateCost(resp, nil)
	// 10000*0.000005 + 2000*0.000015 = 0.05 + 0.03 = 0.08
	assert.InDelta(t, 0.08, cost, 1e-12)
}

// TestCalculateCost_ChatCompletion_CostPerRequest verifies the flat per-request
// fee is billed once, additive on top of the usual token-based cost.
func TestCalculateCost_ChatCompletion_CostPerRequest(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gpt-4o", "openai", "chat"): {
			Model: "gpt-4o", Provider: "openai", Mode: "chat",
			InputCostPerToken:  bifrost.Ptr(0.000005),
			OutputCostPerToken: bifrost.Ptr(0.000015),
			CostPerRequest:     bifrost.Ptr(0.01),
		},
	})

	resp := makeChatResponse(schemas.OpenAI, "gpt-4o", &schemas.BifrostLLMUsage{
		PromptTokens:     10000,
		CompletionTokens: 2000,
		TotalTokens:      12000,
	})

	cost := s.CalculateCost(resp, nil)
	// 10000*0.000005 + 2000*0.000015 + 0.01 (flat) = 0.08 + 0.01 = 0.09
	assert.InDelta(t, 0.09, cost, 1e-12)
}

func TestCalculateCost_ChatCompletion_Claude35Sonnet_WithCache(t *testing.T) {
	// Claude 3.5 Sonnet (Bedrock): $3/M input, $15/M output, cache_read=$0.3/M, cache_creation=$3.75/M
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("anthropic.claude-3-5-sonnet-20241022-v2:0", "bedrock", "chat"): {
			Model: "anthropic.claude-3-5-sonnet-20241022-v2:0", Provider: "bedrock", Mode: "chat",
			InputCostPerToken:                 bifrost.Ptr(0.000003),
			OutputCostPerToken:                bifrost.Ptr(0.000015),
			CacheReadInputTokenCost:           bifrost.Ptr(0.0000003),
			CacheCreationInputTokenCost:       bifrost.Ptr(0.00000375),
			InputCostPerTokenAbove200kTokens:  bifrost.Ptr(0.000006),
			OutputCostPerTokenAbove200kTokens: bifrost.Ptr(0.00003),
		},
	})

	resp := makeChatResponse(schemas.Bedrock, "anthropic.claude-3-5-sonnet-20241022-v2:0", &schemas.BifrostLLMUsage{
		PromptTokens:     5000,
		CompletionTokens: 1000,
		TotalTokens:      6000,
		PromptTokensDetails: &schemas.ChatPromptTokensDetails{
			CachedReadTokens:  3000, // 3000 cache read tokens
			CachedWriteTokens: 500,  // 500 cache creation tokens
		},
	})

	cost := s.CalculateCost(resp, nil)
	// Both cached read and write tokens are input-side deductions from promptTokens.
	// Input: (5000-3000-500)*0.000003 + 3000*0.0000003 + 500*0.00000375 = 0.0045 + 0.0009 + 0.001875 = 0.007275
	// Output: 1000*0.000015 = 0.015
	// Total: 0.007275 + 0.015 = 0.022275
	assert.InDelta(t, 0.022275, cost, 1e-12)
}

func TestCalculateCost_Embedding(t *testing.T) {
	// Titan Embed Text v1: $0.1/M input
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("amazon.titan-embed-text-v1", "bedrock", "embedding"): {
			Model: "amazon.titan-embed-text-v1", Provider: "bedrock", Mode: "embedding",
			InputCostPerToken:  bifrost.Ptr(0.0000001),
			OutputCostPerToken: bifrost.Ptr(0.0),
		},
	})

	resp := makeEmbeddingResponse(schemas.Bedrock, "amazon.titan-embed-text-v1", &schemas.BifrostLLMUsage{
		PromptTokens: 10000,
		TotalTokens:  10000,
	})

	cost := s.CalculateCost(resp, nil)
	// 10000 * 0.0000001 = 0.001
	assert.InDelta(t, 0.001, cost, 1e-12)
}

// CalculateCost is what governance charges a virtual key's dollar budget with: a Vertex
// text embedding that reports only billed characters now costs characters times the
// per-character rate instead of nothing (TXE-7081).
func TestCalculateCost_EmbeddingBilledCharacters(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("text-embedding-005", "vertex", "embedding"): {
			Model: "text-embedding-005", Provider: "vertex", Mode: "embedding",
			InputCostPerToken:     bifrost.Ptr(0.0),
			InputCostPerCharacter: bifrost.Ptr(0.000000025),
		},
	})
	chars := 12000
	resp := makeEmbeddingResponse(schemas.Vertex, "text-embedding-005", &schemas.BifrostLLMUsage{BilledCharacters: &chars})
	assert.InDelta(t, 12000*0.000000025, s.CalculateCost(resp, nil), 1e-15)
}

func TestCalculateCost_Rerank(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("amazon.rerank-v1:0", "bedrock", "rerank"): {
			Model: "amazon.rerank-v1:0", Provider: "bedrock", Mode: "rerank",
			InputCostPerToken:  bifrost.Ptr(0.0),
			OutputCostPerToken: bifrost.Ptr(0.0),
		},
	})

	resp := makeRerankResponse(schemas.Bedrock, "amazon.rerank-v1:0", &schemas.BifrostLLMUsage{
		PromptTokens: 500,
		TotalTokens:  500,
	})

	cost := s.CalculateCost(resp, nil)
	assert.Equal(t, 0.0, cost)
}

func TestCalculateCost_ImageGeneration(t *testing.T) {
	// dall-e-3 via aiml: output_cost_per_image=$0.052
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("dall-e-3", "aiml", "image_generation"): {
			Model: "dall-e-3", Provider: "aiml", Mode: "image_generation",
			OutputCostPerImage: bifrost.Ptr(0.052),
		},
	})

	resp := makeImageResponse("aiml", "dall-e-3", &schemas.ImageUsage{
		OutputTokensDetails: &schemas.ImageTokenDetails{NImages: 3},
	})

	cost := s.CalculateCost(resp, nil)
	// 3 * 0.052 = 0.156
	assert.InDelta(t, 0.156, cost, 1e-12)
}

func TestCalculateCost_StreamRequestTypeNormalized(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gpt-4o", "openai", "chat"): chatPricing(0.000005, 0.000015),
	})

	// Stream request type should be normalized to base type
	resp := &schemas.BifrostResponse{
		ChatResponse: &schemas.BifrostChatResponse{
			Usage: &schemas.BifrostLLMUsage{PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500},
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType: schemas.ChatCompletionStreamRequest,
				RoutingInfo: routingInfoFor(schemas.OpenAI, "gpt-4o"),
			},
		},
	}

	cost := s.CalculateCost(resp, nil)
	assert.InDelta(t, 0.0125, cost, 1e-12)
}

func TestCalculateCost_WebSocketResponsesFallsBackToChatPricing(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gpt-4o", "openai", "chat"): chatPricing(0.000005, 0.000015),
	})

	resp := &schemas.BifrostResponse{
		ResponsesStreamResponse: &schemas.BifrostResponsesStreamResponse{
			Response: &schemas.BifrostResponsesResponse{
				Usage: &schemas.ResponsesResponseUsage{InputTokens: 1000, OutputTokens: 500, TotalTokens: 1500},
			},
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType: schemas.WebSocketResponsesRequest,
				RoutingInfo: routingInfoFor(schemas.OpenAI, "gpt-4o"),
			},
		},
	}

	cost := s.CalculateCost(resp, nil)
	assert.InDelta(t, 0.0125, cost, 1e-12)
}

func TestCalculateCost_NoPricingData(t *testing.T) {
	s := testStoreWithPricing(nil)
	resp := makeChatResponse(schemas.OpenAI, "unknown-model", &schemas.BifrostLLMUsage{
		PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500,
	})
	cost := s.CalculateCost(resp, nil)
	assert.Equal(t, 0.0, cost)
}

// =========================================================================
// 12. Pricing resolution — getPricing fallback logic
// =========================================================================

func TestGetPricing_DirectLookup(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gpt-4o", "openai", "chat"): chatPricing(0.000005, 0.000015),
	})
	p := s.resolvePricing(schemas.RoutingInfo{Provider: "openai", Model: "gpt-4o"}, schemas.ChatCompletionRequest, LookupScopes{Provider: "openai"})
	assert.Equal(t, 0.000005, derefF(p.InputCostPerToken))
}

func TestCalculateCost_TogetherAliasUsesTogetherAIPricingWithCache(t *testing.T) {
	const model = "zai-org/GLM-5.2"
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey(model, "together_ai", "chat"): {
			Model:                   model,
			Provider:                "together_ai",
			Mode:                    "chat",
			InputCostPerToken:       bifrost.Ptr(1.40 / 1_000_000),
			CacheReadInputTokenCost: bifrost.Ptr(0.26 / 1_000_000),
			OutputCostPerToken:      bifrost.Ptr(4.40 / 1_000_000),
		},
	})
	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     36_807_862,
		CompletionTokens: 54_932,
		TotalTokens:      36_862_794,
		PromptTokensDetails: &schemas.ChatPromptTokensDetails{
			CachedReadTokens: 33_829_760,
		},
	}

	cost := s.CalculateCost(makeChatResponse("together", model, usage), nil)
	assert.InDelta(t, 13.2067812, cost, 1e-9)
}

func TestGetPricing_GeminiFallsBackToVertex(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gemini-2.0-flash", "vertex", "chat"): {
			Model: "gemini-2.0-flash", Provider: "vertex", Mode: "chat",
			InputCostPerToken: bifrost.Ptr(0.0000001), OutputCostPerToken: bifrost.Ptr(0.0000004),
		},
	})
	p := s.resolvePricing(schemas.RoutingInfo{Provider: "gemini", Model: "gemini-2.0-flash"}, schemas.ChatCompletionRequest, LookupScopes{Provider: "gemini"})
	assert.Equal(t, 0.0000001, derefF(p.InputCostPerToken))
}

func TestGetPricing_VertexStripsProviderPrefix(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gemini-2.0-flash", "vertex", "chat"): chatPricing(0.0000001, 0.0000004),
	})
	p := s.resolvePricing(schemas.RoutingInfo{Provider: "vertex", Model: "google/gemini-2.0-flash"}, schemas.ChatCompletionRequest, LookupScopes{Provider: "vertex"})
	assert.Equal(t, 0.0000001, derefF(p.InputCostPerToken))
}

func TestGetPricing_BedrockAddsAnthropicPrefix(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("anthropic.claude-3-5-sonnet-20241022-v2:0", "bedrock", "chat"): chatPricing(0.000003, 0.000015),
	})
	p := s.resolvePricing(schemas.RoutingInfo{Provider: "bedrock", Model: "claude-3-5-sonnet-20241022-v2:0"}, schemas.ChatCompletionRequest, LookupScopes{Provider: "bedrock"})
	assert.Equal(t, 0.000003, derefF(p.InputCostPerToken))
}

func TestGetPricing_BedrockAddsOpenAIPrefix(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("openai.gpt-oss-120b", "bedrock", "chat"): chatPricing(0.00000015, 0.0000006),
	})
	p := s.resolvePricing(schemas.RoutingInfo{Provider: "bedrock", Model: "gpt-oss-120b"}, schemas.ChatCompletionRequest, LookupScopes{Provider: "bedrock"})
	require.NotNil(t, p)
	assert.Equal(t, 0.00000015, derefF(p.InputCostPerToken))
}

func TestGetPricing_BedrockAddsGooglePrefix(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("google.gemma-4-31b", "bedrock", "chat"): chatPricing(0.00000014, 0.0000004),
	})
	p := s.resolvePricing(schemas.RoutingInfo{Provider: "bedrock", Model: "gemma-4-31b"}, schemas.ChatCompletionRequest, LookupScopes{Provider: "bedrock"})
	require.NotNil(t, p)
	assert.Equal(t, 0.00000014, derefF(p.InputCostPerToken))
}

func TestGetPricing_BedrockAddsXAIPrefix(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("xai.grok-4.3", "bedrock", "chat"): chatPricing(0.00000125, 0.0000025),
	})
	p := s.resolvePricing(schemas.RoutingInfo{Provider: "bedrock", Model: "grok-4.3"}, schemas.ChatCompletionRequest, LookupScopes{Provider: "bedrock"})
	require.NotNil(t, p)
	assert.Equal(t, 0.00000125, derefF(p.InputCostPerToken))
}

func TestGetPricing_BedrockMantleFallsBackToBedrock(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("openai.gpt-oss-120b", "bedrock", "chat"): chatPricing(0.00000015, 0.0000006),
	})
	p := s.resolvePricing(schemas.RoutingInfo{Provider: "bedrock_mantle", Model: "openai.gpt-oss-120b"}, schemas.ChatCompletionRequest, LookupScopes{Provider: "bedrock_mantle"})
	require.NotNil(t, p)
	assert.Equal(t, 0.00000015, derefF(p.InputCostPerToken))
}

func TestGetPricing_BedrockMantleResponsesFallsBackToBedrockChat(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("openai.gpt-oss-120b", "bedrock", "chat"): chatPricing(0.00000015, 0.0000006),
	})
	// bedrock_mantle provider + responses request → try bedrock + responses → try bedrock + chat
	p := s.resolvePricing(schemas.RoutingInfo{Provider: "bedrock_mantle", Model: "openai.gpt-oss-120b"}, schemas.ResponsesRequest, LookupScopes{Provider: "bedrock_mantle"})
	require.NotNil(t, p)
	assert.Equal(t, 0.00000015, derefF(p.InputCostPerToken))
}

func TestGetPricing_BedrockMantleAddsAnthropicPrefix(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("anthropic.claude-3-5-sonnet-20241022-v2:0", "bedrock", "chat"): chatPricing(0.000003, 0.000015),
	})
	p := s.resolvePricing(schemas.RoutingInfo{Provider: "bedrock_mantle", Model: "claude-3-5-sonnet-20241022-v2:0"}, schemas.ChatCompletionRequest, LookupScopes{Provider: "bedrock_mantle"})
	require.NotNil(t, p)
	assert.Equal(t, 0.000003, derefF(p.InputCostPerToken))
}

func TestGetPricing_BedrockMantleAddsOpenAIPrefix(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("openai.gpt-oss-120b", "bedrock", "chat"): chatPricing(0.00000015, 0.0000006),
	})
	// bedrock_mantle folds onto bedrock, then the openai. prefix retry fires
	p := s.resolvePricing(schemas.RoutingInfo{Provider: "bedrock_mantle", Model: "gpt-oss-120b"}, schemas.ChatCompletionRequest, LookupScopes{Provider: "bedrock_mantle"})
	require.NotNil(t, p)
	assert.Equal(t, 0.00000015, derefF(p.InputCostPerToken))
}

func TestGetPricing_BedrockMantleResponsesAddsOpenAIPrefix(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("openai.gpt-5.5", "bedrock", "responses"): chatPricing(0.0000055, 0.000033),
	})
	p := s.resolvePricing(schemas.RoutingInfo{Provider: "bedrock_mantle", Model: "gpt-5.5"}, schemas.ResponsesRequest, LookupScopes{Provider: "bedrock_mantle"})
	require.NotNil(t, p)
	assert.Equal(t, 0.0000055, derefF(p.InputCostPerToken))
}

func TestGetPricing_ResponsesFallsBackToChat(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gpt-4o", "openai", "chat"): chatPricing(0.000005, 0.000015),
	})
	p := s.resolvePricing(schemas.RoutingInfo{Provider: "openai", Model: "gpt-4o"}, schemas.ResponsesRequest, LookupScopes{Provider: "openai"})
	assert.Equal(t, 0.000005, derefF(p.InputCostPerToken))
}

func TestGetPricing_ResponsesStreamFallsBackToChat(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gpt-4o", "openai", "chat"): chatPricing(0.000005, 0.000015),
	})
	p := s.resolvePricing(schemas.RoutingInfo{Provider: "openai", Model: "gpt-4o"}, schemas.ResponsesStreamRequest, LookupScopes{Provider: "openai"})
	assert.Equal(t, 0.000005, derefF(p.InputCostPerToken))
}

// TestCalculateCost_RealtimeCatalogMode verifies text and audio usage are priced from realtime rows.
func TestCalculateCost_RealtimeCatalogMode(t *testing.T) {
	pricing := configstoreTables.TableModelPricing{
		Model: "gpt-realtime", Provider: "openai", Mode: "realtime",
		InputCostPerToken: bifrost.Ptr(0.000004), OutputCostPerToken: bifrost.Ptr(0.000016),
		InputCostPerAudioToken: bifrost.Ptr(0.000032), OutputCostPerAudioToken: bifrost.Ptr(0.000064),
	}
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey(pricing.Model, pricing.Provider, pricing.Mode): pricing,
	})
	resp := &schemas.BifrostResponse{ResponsesResponse: &schemas.BifrostResponsesResponse{
		Usage: &schemas.ResponsesResponseUsage{
			InputTokens: 100, OutputTokens: 50, TotalTokens: 150,
			InputTokensDetails:  &schemas.ResponsesResponseInputTokens{TextTokens: 40, AudioTokens: 60},
			OutputTokensDetails: &schemas.ResponsesResponseOutputTokens{TextTokens: 20, AudioTokens: 30},
		},
		ExtraFields: schemas.BifrostResponseExtraFields{
			RequestType: schemas.RealtimeRequest, RoutingInfo: routingInfoFor(schemas.OpenAI, pricing.Model),
		},
	}}
	breakdown := s.CalculateCostBreakdown(resp, nil)
	require.NotNil(t, breakdown)
	assert.InDelta(t, 0.00432, breakdown.TotalCost, 1e-12)
	assert.InDelta(t, 0.00192, breakdown.InputCostDetails.AudioCost, 1e-12)
	assert.InDelta(t, 0.00192, breakdown.OutputCostDetails.AudioCost, 1e-12)
}

// TestGetPricing_RealtimeModeOrder verifies realtime, responses, then chat precedence.
func TestGetPricing_RealtimeModeOrder(t *testing.T) {
	for _, modes := range [][]string{{"realtime", "responses", "chat"}, {"responses", "chat"}, {"chat"}} {
		t.Run(modes[0], func(t *testing.T) {
			entries := map[string]configstoreTables.TableModelPricing{}
			for i, mode := range modes {
				entries[makeKey("gpt-realtime", "openai", mode)] = chatPricing(float64(i+1), 0)
			}
			s := testStoreWithPricing(entries)
			pricing := s.resolvePricing(routingInfoFor(schemas.OpenAI, "gpt-realtime"), schemas.RealtimeRequest, LookupScopes{Provider: "openai"})
			require.NotNil(t, pricing)
			assert.Equal(t, 1.0, derefF(pricing.InputCostPerToken))
		})
	}
}

func TestGetPricing_RealtimeFallsBackToChat(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gpt-4o", "openai", "chat"): chatPricing(0.000005, 0.000015),
	})
	p := s.resolvePricing(schemas.RoutingInfo{Provider: "openai", Model: "gpt-4o"}, schemas.RealtimeRequest, LookupScopes{Provider: "openai"})
	assert.Equal(t, 0.000005, derefF(p.InputCostPerToken))
}

func TestGetPricing_ChatFallsBackToResponses(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gpt-4o", "openai", "responses"): chatPricing(0.000005, 0.000015),
	})
	p := s.resolvePricing(schemas.RoutingInfo{Provider: "openai", Model: "gpt-4o"}, schemas.ChatCompletionRequest, LookupScopes{Provider: "openai"})
	require.NotNil(t, p)
	assert.Equal(t, 0.000005, derefF(p.InputCostPerToken))
}

func TestGetPricing_ChatStreamFallsBackToResponses(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gpt-4o", "openai", "responses"): chatPricing(0.000005, 0.000015),
	})
	p := s.resolvePricing(schemas.RoutingInfo{Provider: "openai", Model: "gpt-4o"}, schemas.ChatCompletionStreamRequest, LookupScopes{Provider: "openai"})
	require.NotNil(t, p)
	assert.Equal(t, 0.000005, derefF(p.InputCostPerToken))
}

func TestGetPricing_BedrockMantleChatStreamFallsBackToBedrockResponses(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("openai.gpt-5.5", "bedrock", "responses"): chatPricing(0.0000055, 0.000033),
	})
	// The datasheet files openai.gpt-5.5 as responses-only, but bedrock serves it
	// over chat completions too: bedrock_mantle folds onto bedrock, chat mode
	// misses, and the responses row is used.
	p := s.resolvePricing(schemas.RoutingInfo{Provider: "bedrock_mantle", Model: "openai.gpt-5.5"}, schemas.ChatCompletionStreamRequest, LookupScopes{Provider: "bedrock_mantle"})
	require.NotNil(t, p)
	assert.Equal(t, 0.0000055, derefF(p.InputCostPerToken))
	assert.Equal(t, 0.000033, derefF(p.OutputCostPerToken))
}

func TestGetPricing_BedrockChatAddsOpenAIPrefixThenFallsBackToResponses(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("openai.gpt-5.5", "bedrock", "responses"): chatPricing(0.0000055, 0.000033),
	})
	// Bare model name + chat request: the openai. prefix retry fires, then the
	// prefixed key falls back to responses mode.
	p := s.resolvePricing(schemas.RoutingInfo{Provider: "bedrock", Model: "gpt-5.5"}, schemas.ChatCompletionRequest, LookupScopes{Provider: "bedrock"})
	require.NotNil(t, p)
	assert.Equal(t, 0.0000055, derefF(p.InputCostPerToken))
}

func TestGetPricing_ExactModeWinsOverFallback(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gpt-4o", "openai", "chat"):      chatPricing(0.000005, 0.000015),
		makeKey("gpt-4o", "openai", "responses"): chatPricing(0.000009, 0.000036),
	})
	chat := s.resolvePricing(schemas.RoutingInfo{Provider: "openai", Model: "gpt-4o"}, schemas.ChatCompletionRequest, LookupScopes{Provider: "openai"})
	require.NotNil(t, chat)
	assert.Equal(t, 0.000005, derefF(chat.InputCostPerToken))

	responses := s.resolvePricing(schemas.RoutingInfo{Provider: "openai", Model: "gpt-4o"}, schemas.ResponsesRequest, LookupScopes{Provider: "openai"})
	require.NotNil(t, responses)
	assert.Equal(t, 0.000009, derefF(responses.InputCostPerToken))
}

func TestGetPricing_GeminiChatFallsBackToVertexResponses(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gemini-2.0-flash", "vertex", "responses"): chatPricing(0.0000001, 0.0000004),
	})
	// gemini provider + chat request → try vertex + chat → try vertex + responses
	p := s.resolvePricing(schemas.RoutingInfo{Provider: "gemini", Model: "gemini-2.0-flash"}, schemas.ChatCompletionRequest, LookupScopes{Provider: "gemini"})
	require.NotNil(t, p)
	assert.Equal(t, 0.0000001, derefF(p.InputCostPerToken))
}

func TestGetPricing_GeminiResponsesFallsBackToVertexChat(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gemini-2.0-flash", "vertex", "chat"): chatPricing(0.0000001, 0.0000004),
	})
	// gemini provider + responses request → try vertex + responses → try vertex + chat
	p := s.resolvePricing(schemas.RoutingInfo{Provider: "gemini", Model: "gemini-2.0-flash"}, schemas.ResponsesRequest, LookupScopes{Provider: "gemini"})
	assert.Equal(t, 0.0000001, derefF(p.InputCostPerToken))
}

func TestGetPricing_NotFound(t *testing.T) {
	s := testStoreWithPricing(nil)
	p := s.resolvePricing(schemas.RoutingInfo{Provider: "openai", Model: "nonexistent"}, schemas.ChatCompletionRequest, LookupScopes{Provider: "openai"})
	assert.Nil(t, p)
}

// =========================================================================
// 13. resolvePricing — deployment fallback
// =========================================================================

func TestResolvePricing_DeploymentFallback(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("my-deployment", "openai", "chat"): chatPricing(0.000005, 0.000015),
	})

	// Model not found directly, but deployment matches
	p := s.resolvePricing(schemas.RoutingInfo{Provider: "openai", Model: "gpt-4o-custom", ResolvedKeyAlias: &schemas.ResolvedKeyAlias{ModelID: "my-deployment"}}, schemas.ChatCompletionRequest, LookupScopes{})
	require.NotNil(t, p)
	assert.Equal(t, 0.000005, derefF(p.InputCostPerToken))
}

func TestResolvePricing_ResolvedModelHasPriority(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gpt-4o", "openai", "chat"):        chatPricing(0.000005, 0.000015),
		makeKey("my-deployment", "openai", "chat"): chatPricing(0.000001, 0.000002),
	})

	// Resolved model ("my-deployment") is looked up first and has priority
	// over the originally requested model ("gpt-4o").
	p := s.resolvePricing(schemas.RoutingInfo{Provider: "openai", Model: "gpt-4o", ResolvedKeyAlias: &schemas.ResolvedKeyAlias{ModelID: "my-deployment"}}, schemas.ChatCompletionRequest, LookupScopes{})
	require.NotNil(t, p)
	assert.Equal(t, 0.000001, derefF(p.InputCostPerToken))
}

func TestResolvePricing_NothingFound(t *testing.T) {
	s := testStoreWithPricing(nil)
	p := s.resolvePricing(schemas.RoutingInfo{Provider: "openai", Model: "unknown"}, schemas.ChatCompletionRequest, LookupScopes{})
	assert.Nil(t, p)
}

// =========================================================================
// 14. normalizeStreamRequestType
// =========================================================================

func TestNormalizeStreamRequestType(t *testing.T) {
	tests := []struct {
		input    schemas.RequestType
		expected schemas.RequestType
	}{
		{schemas.ChatCompletionStreamRequest, schemas.ChatCompletionRequest},
		{schemas.TextCompletionStreamRequest, schemas.TextCompletionRequest},
		{schemas.ResponsesStreamRequest, schemas.ResponsesRequest},
		{schemas.SpeechStreamRequest, schemas.SpeechRequest},
		{schemas.TranscriptionStreamRequest, schemas.TranscriptionRequest},
		{schemas.ImageGenerationStreamRequest, schemas.ImageGenerationRequest},
		{schemas.ImageEditStreamRequest, schemas.ImageEditRequest},
		{schemas.RealtimeRequest, schemas.RealtimeRequest},             // realtime is its own base type
		{schemas.ChatCompletionRequest, schemas.ChatCompletionRequest}, // non-stream unchanged
		{schemas.EmbeddingRequest, schemas.EmbeddingRequest},           // non-stream unchanged
	}

	for _, tt := range tests {
		assert.Equal(t, tt.expected, normalizeStreamRequestType(tt.input), "for input %s", tt.input)
	}
}

// =========================================================================
// 15. responsesUsageToBifrostUsage
// =========================================================================

func TestResponsesUsageToBifrostUsage_Basic(t *testing.T) {
	u := &schemas.ResponsesResponseUsage{
		InputTokens:  100,
		OutputTokens: 50,
		TotalTokens:  150,
	}
	result := responsesUsageToBifrostUsage(u)
	assert.Equal(t, 100, result.PromptTokens)
	assert.Equal(t, 50, result.CompletionTokens)
	assert.Equal(t, 150, result.TotalTokens)
	assert.Nil(t, result.PromptTokensDetails)
	assert.Nil(t, result.CompletionTokensDetails)
}

func TestResponsesUsageToBifrostUsage_WithTokenDetails(t *testing.T) {
	numQueries := 2
	u := &schemas.ResponsesResponseUsage{
		InputTokens:  1000,
		OutputTokens: 500,
		TotalTokens:  1500,
		InputTokensDetails: &schemas.ResponsesResponseInputTokens{
			CachedReadTokens:  300,
			CachedWriteTokens: 50,
			TextTokens:        600,
			AudioTokens:       50,
			ImageTokens:       50,
		},
		OutputTokensDetails: &schemas.ResponsesResponseOutputTokens{
			ReasoningTokens:  100,
			NumSearchQueries: &numQueries,
		},
		ToolUsage: &schemas.ToolUsage{WebSearch: &schemas.WebSearchToolUsage{NumRequests: numQueries}},
	}
	result := responsesUsageToBifrostUsage(u)

	require.NotNil(t, result.PromptTokensDetails)
	assert.Equal(t, 300, result.PromptTokensDetails.CachedReadTokens)
	assert.Equal(t, 50, result.PromptTokensDetails.CachedWriteTokens)
	assert.Equal(t, 600, result.PromptTokensDetails.TextTokens)
	assert.Equal(t, 50, result.PromptTokensDetails.AudioTokens)
	assert.Equal(t, 50, result.PromptTokensDetails.ImageTokens)

	require.NotNil(t, result.CompletionTokensDetails)
	assert.Equal(t, 100, result.CompletionTokensDetails.ReasoningTokens)
	require.NotNil(t, result.CompletionTokensDetails.NumSearchQueries)
	assert.Equal(t, 2, *result.CompletionTokensDetails.NumSearchQueries)
	assert.Equal(t, 2, result.ToolUsage.WebSearch.NumRequests)
}

// =========================================================================
// 16. Edge cases
// =========================================================================

func TestCalculateCost_200kTier_EndToEnd(t *testing.T) {
	// Claude 3.5 Sonnet Bedrock with 200k tier pricing
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("anthropic.claude-3-5-sonnet-20240620-v1:0", "bedrock", "chat"): {
			Model: "anthropic.claude-3-5-sonnet-20240620-v1:0", Provider: "bedrock", Mode: "chat",
			InputCostPerToken:                          bifrost.Ptr(0.000003),
			OutputCostPerToken:                         bifrost.Ptr(0.000015),
			InputCostPerTokenAbove200kTokens:           bifrost.Ptr(0.000006),
			OutputCostPerTokenAbove200kTokens:          bifrost.Ptr(0.00003),
			CacheReadInputTokenCost:                    bifrost.Ptr(0.0000003),
			CacheCreationInputTokenCost:                bifrost.Ptr(0.00000375),
			CacheReadInputTokenCostAbove200kTokens:     bifrost.Ptr(0.0000006),
			CacheCreationInputTokenCostAbove200kTokens: bifrost.Ptr(0.0000075),
		},
	})

	resp := makeChatResponse(schemas.Bedrock, "anthropic.claude-3-5-sonnet-20240620-v1:0", &schemas.BifrostLLMUsage{
		PromptTokens:     210000,
		CompletionTokens: 20000,
		TotalTokens:      230000, // input is above 200k
	})

	cost := s.CalculateCost(resp, nil)
	// Tiered rate: input=0.000006, output=0.00003
	// 210000*0.000006 + 20000*0.00003 = 1.26 + 0.6 = 1.86
	assert.InDelta(t, 1.86, cost, 1e-9)
}

func TestCalculateCost_272kTier_EndToEnd(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("claude-3-7-sonnet", "anthropic", "chat"): {
			Model:                                  "claude-3-7-sonnet",
			Provider:                               "anthropic",
			Mode:                                   "chat",
			InputCostPerToken:                      new(0.000003),
			OutputCostPerToken:                     new(0.000015),
			InputCostPerTokenAbove200kTokens:       new(0.000006),
			OutputCostPerTokenAbove200kTokens:      new(0.00003),
			InputCostPerTokenAbove272kTokens:       new(0.000009),
			OutputCostPerTokenAbove272kTokens:      new(0.000045),
			CacheReadInputTokenCost:                new(0.0000003),
			CacheReadInputTokenCostAbove200kTokens: new(0.0000006),
			CacheReadInputTokenCostAbove272kTokens: new(0.0000009),
		},
	})

	resp := makeChatResponse(schemas.Anthropic, "claude-3-7-sonnet", &schemas.BifrostLLMUsage{
		PromptTokens:     280000,
		CompletionTokens: 30000,
		TotalTokens:      310000, // input is above 272k
	})

	cost := s.CalculateCost(resp, nil)
	// Tiered rate: input=0.000009, output=0.000045
	// 280000*0.000009 + 30000*0.000045 = 2.52 + 1.35 = 3.87
	assert.InDelta(t, 3.87, cost, 1e-9)
}

func TestCalculateCost_272kTier_CacheReadFallbackChain(t *testing.T) {
	// Verifies the 272k cache read rate takes precedence over 200k and base rates
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("claude-3-7-sonnet", "anthropic", "chat"): {
			Model:                                  "claude-3-7-sonnet",
			Provider:                               "anthropic",
			Mode:                                   "chat",
			InputCostPerToken:                      new(0.000003),
			OutputCostPerToken:                     new(0.000015),
			InputCostPerTokenAbove272kTokens:       new(0.000009),
			OutputCostPerTokenAbove272kTokens:      new(0.000045),
			CacheReadInputTokenCost:                new(0.0000003),
			CacheReadInputTokenCostAbove200kTokens: new(0.0000006),
			CacheReadInputTokenCostAbove272kTokens: new(0.0000009),
		},
	})

	resp := makeChatResponse(schemas.Anthropic, "claude-3-7-sonnet", &schemas.BifrostLLMUsage{
		PromptTokens:     280000,
		CompletionTokens: 30000,
		TotalTokens:      310000,
		PromptTokensDetails: &schemas.ChatPromptTokensDetails{
			CachedReadTokens: 50000,
		},
	})

	cost := s.CalculateCost(resp, nil)
	// Non-cached input: (280000-50000) * 0.000009 = 230000 * 0.000009 = 2.07
	// Cached read (272k rate): 50000 * 0.0000009 = 0.045
	// Output: 30000 * 0.000045 = 1.35
	// Total: 2.07 + 0.045 + 1.35 = 3.465
	assert.InDelta(t, 3.465, cost, 1e-9)
}

// =========================================================================
// Priority tier tests
// =========================================================================

func TestComputeTextCost_PriorityUsesInputOutputPriorityRate(t *testing.T) {
	p := chatPricing(0.000003, 0.000015)
	p.InputCostPerTokenPriority = new(0.000006)
	p.OutputCostPerTokenPriority = new(0.00003)

	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     1000,
		CompletionTokens: 500,
		TotalTokens:      1500,
	}

	cost := computeTextCostTotal(&p, usage, serviceTier{isPriority: true})

	// Uses priority rates: 1000*0.000006 + 500*0.00003 = 0.006 + 0.015 = 0.021
	assert.InDelta(t, 0.021, cost, 1e-12)
}

func TestComputeTextCost_NonPriorityIgnoresPriorityRate(t *testing.T) {
	p := chatPricing(0.000003, 0.000015)
	p.InputCostPerTokenPriority = new(0.000006)
	p.OutputCostPerTokenPriority = new(0.00003)

	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     1000,
		CompletionTokens: 500,
		TotalTokens:      1500,
	}

	cost := computeTextCostTotal(&p, usage, serviceTier{})

	// Uses base rates, ignores priority fields: 1000*0.000003 + 500*0.000015 = 0.003 + 0.0075 = 0.0105
	assert.InDelta(t, 0.0105, cost, 1e-12)
}

func TestComputeTextCost_Priority272kTier(t *testing.T) {
	p := chatPricing(0.000003, 0.000015)
	p.InputCostPerTokenPriority = new(0.000006)
	p.OutputCostPerTokenPriority = new(0.00003)
	p.InputCostPerTokenAbove272kTokens = new(0.000009)
	p.InputCostPerTokenAbove272kTokensPriority = new(0.000012)
	p.OutputCostPerTokenAbove272kTokens = new(0.000045)
	p.OutputCostPerTokenAbove272kTokensPriority = new(0.00006)

	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     280000,
		CompletionTokens: 30000,
		TotalTokens:      310000,
	}

	cost := computeTextCostTotal(&p, usage, serviceTier{isPriority: true})

	// Uses 272k priority rates: 280000*0.000012 + 30000*0.00006 = 3.36 + 1.80 = 5.16
	assert.InDelta(t, 5.16, cost, 1e-9)
}

func TestComputeTextCost_Priority272kTierFallsBackToNonPriority272k(t *testing.T) {
	// Priority flag set but no priority-specific 272k rate — fall back to non-priority 272k
	p := chatPricing(0.000003, 0.000015)
	p.InputCostPerTokenAbove272kTokens = new(0.000009)
	p.OutputCostPerTokenAbove272kTokens = new(0.000045)

	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     280000,
		CompletionTokens: 30000,
		TotalTokens:      310000,
	}

	cost := computeTextCostTotal(&p, usage, serviceTier{isPriority: true})

	// Falls back to non-priority 272k rate: 280000*0.000009 + 30000*0.000045 = 2.52 + 1.35 = 3.87
	assert.InDelta(t, 3.87, cost, 1e-9)
}

func TestComputeTextCost_PriorityCacheReadRate(t *testing.T) {
	p := chatPricing(0.000003, 0.000015)
	p.InputCostPerTokenPriority = new(0.000006)
	p.OutputCostPerTokenPriority = new(0.00003)
	p.CacheReadInputTokenCost = new(0.0000003)
	p.CacheReadInputTokenCostPriority = new(0.0000006)

	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     1000,
		CompletionTokens: 500,
		TotalTokens:      1500,
		PromptTokensDetails: &schemas.ChatPromptTokensDetails{
			CachedReadTokens: 400,
		},
	}

	cost := computeTextCostTotal(&p, usage, serviceTier{isPriority: true})

	// Non-cached input: (1000-400)*0.000006 = 600*0.000006 = 0.0036
	// Cached read (priority rate): 400*0.0000006 = 0.00024
	// Output: 500*0.00003 = 0.015
	// Total: 0.0036 + 0.00024 + 0.015 = 0.01884
	assert.InDelta(t, 0.01884, cost, 1e-12)
}

func TestCalculateCost_PriorityTier_EndToEnd(t *testing.T) {
	tier := schemas.BifrostServiceTierPriority
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gpt-4o", "openai", "chat"): {
			Model:                      "gpt-4o",
			Provider:                   "openai",
			Mode:                       "chat",
			InputCostPerToken:          new(0.000005),
			OutputCostPerToken:         new(0.000015),
			InputCostPerTokenPriority:  new(0.000010),
			OutputCostPerTokenPriority: new(0.000030),
		},
	})

	resp := &schemas.BifrostResponse{
		ChatResponse: &schemas.BifrostChatResponse{
			ServiceTier: &tier,
			Usage: &schemas.BifrostLLMUsage{
				PromptTokens:     1000,
				CompletionTokens: 500,
				TotalTokens:      1500,
			},
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType: schemas.ChatCompletionRequest,
				RoutingInfo: routingInfoFor(schemas.OpenAI, "gpt-4o"),
			},
		},
	}

	cost := s.CalculateCost(resp, nil)
	// Priority rates: 1000*0.000010 + 500*0.000030 = 0.010 + 0.015 = 0.025
	assert.InDelta(t, 0.025, cost, 1e-12)
}

func TestCalculateCost_NonPriorityServiceTier_UsesBaseRate(t *testing.T) {
	tier := schemas.BifrostServiceTierAuto
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gpt-4o", "openai", "chat"): {
			Model:                      "gpt-4o",
			Provider:                   "openai",
			Mode:                       "chat",
			InputCostPerToken:          new(0.000005),
			OutputCostPerToken:         new(0.000015),
			InputCostPerTokenPriority:  new(0.000010),
			OutputCostPerTokenPriority: new(0.000030),
		},
	})

	resp := &schemas.BifrostResponse{
		ChatResponse: &schemas.BifrostChatResponse{
			ServiceTier: &tier,
			Usage: &schemas.BifrostLLMUsage{
				PromptTokens:     1000,
				CompletionTokens: 500,
				TotalTokens:      1500,
			},
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType: schemas.ChatCompletionRequest,
				RoutingInfo: routingInfoFor(schemas.OpenAI, "gpt-4o"),
			},
		},
	}

	cost := s.CalculateCost(resp, nil)
	// Base rates (not priority): 1000*0.000005 + 500*0.000015 = 0.005 + 0.0075 = 0.0125
	assert.InDelta(t, 0.0125, cost, 1e-12)
}

func TestTieredCacheReadRate_FallbackOrder(t *testing.T) {
	// 272k rate takes precedence over 200k, 200k over base, base over input rate
	t.Run("uses_272k_when_above_272k", func(t *testing.T) {
		p := chatPricing(0.000003, 0.000015)
		p.CacheReadInputTokenCost = new(0.0000003)
		p.CacheReadInputTokenCostAbove200kTokens = new(0.0000006)
		p.CacheReadInputTokenCostAbove272kTokens = new(0.0000009)
		assert.Equal(t, 0.0000009, tieredCacheReadInputTokenRate(&p, 280000, serviceTier{}))
	})
	t.Run("uses_200k_when_between_200k_and_272k", func(t *testing.T) {
		p := chatPricing(0.000003, 0.000015)
		p.CacheReadInputTokenCost = new(0.0000003)
		p.CacheReadInputTokenCostAbove200kTokens = new(0.0000006)
		p.CacheReadInputTokenCostAbove272kTokens = new(0.0000009)
		assert.Equal(t, 0.0000006, tieredCacheReadInputTokenRate(&p, 230000, serviceTier{}))
	})
	t.Run("uses_base_cache_rate_when_below_200k", func(t *testing.T) {
		p := chatPricing(0.000003, 0.000015)
		p.CacheReadInputTokenCost = new(0.0000003)
		p.CacheReadInputTokenCostAbove200kTokens = new(0.0000006)
		p.CacheReadInputTokenCostAbove272kTokens = new(0.0000009)
		assert.Equal(t, 0.0000003, tieredCacheReadInputTokenRate(&p, 1500, serviceTier{}))
	})
	t.Run("falls_back_to_input_rate_when_no_cache_rate_set", func(t *testing.T) {
		p := chatPricing(0.000003, 0.000015)
		// No cache rates set at all
		assert.Equal(t, 0.000003, tieredCacheReadInputTokenRate(&p, 280000, serviceTier{}))
	})
	t.Run("priority_uses_272k_priority_rate", func(t *testing.T) {
		p := chatPricing(0.000003, 0.000015)
		p.CacheReadInputTokenCost = new(0.0000003)
		p.CacheReadInputTokenCostPriority = new(0.0000006)
		p.CacheReadInputTokenCostAbove272kTokens = new(0.0000009)
		p.CacheReadInputTokenCostAbove272kTokensPriority = new(0.0000012)
		assert.Equal(t, 0.0000012, tieredCacheReadInputTokenRate(&p, 280000, serviceTier{isPriority: true}))
	})
	t.Run("priority_falls_back_to_272k_non_priority_when_priority_rate_missing", func(t *testing.T) {
		p := chatPricing(0.000003, 0.000015)
		p.CacheReadInputTokenCostAbove272kTokens = new(0.0000009)
		assert.Equal(t, 0.0000009, tieredCacheReadInputTokenRate(&p, 280000, serviceTier{isPriority: true}))
	})
	t.Run("priority_uses_priority_base_cache_rate_below_tiers", func(t *testing.T) {
		p := chatPricing(0.000003, 0.000015)
		p.CacheReadInputTokenCost = new(0.0000003)
		p.CacheReadInputTokenCostPriority = new(0.0000006)
		assert.Equal(t, 0.0000006, tieredCacheReadInputTokenRate(&p, 1500, serviceTier{isPriority: true}))
	})
	t.Run("flex_uses_flex_cache_rate", func(t *testing.T) {
		p := chatPricing(0.000003, 0.000015)
		p.CacheReadInputTokenCost = new(0.0000003)
		p.CacheReadInputTokenCostFlex = new(0.0000005)
		assert.Equal(t, 0.0000005, tieredCacheReadInputTokenRate(&p, 1500, serviceTier{isFlex: true}))
	})
	t.Run("flex_uses_flex_cache_rate_regardless_of_token_count", func(t *testing.T) {
		p := chatPricing(0.000003, 0.000015)
		p.CacheReadInputTokenCost = new(0.0000003)
		p.CacheReadInputTokenCostFlex = new(0.0000005)
		p.CacheReadInputTokenCostAbove272kTokens = new(0.0000009)
		// Even above 272k, flex flat rate takes precedence
		assert.Equal(t, 0.0000005, tieredCacheReadInputTokenRate(&p, 280000, serviceTier{isFlex: true}))
	})
	t.Run("flex_falls_back_to_base_cache_rate_when_no_flex_cache_rate", func(t *testing.T) {
		p := chatPricing(0.000003, 0.000015)
		p.CacheReadInputTokenCost = new(0.0000003)
		// No flex cache rate — falls back to base cache rate
		assert.Equal(t, 0.0000003, tieredCacheReadInputTokenRate(&p, 1500, serviceTier{isFlex: true}))
	})
	t.Run("flex_wins_over_272k_priority_and_priority_base_when_all_present", func(t *testing.T) {
		p := chatPricing(0.000003, 0.000015)
		p.CacheReadInputTokenCostAbove272kTokens = new(5e-7)
		p.CacheReadInputTokenCostFlex = new(1.3e-7)
		p.CacheReadInputTokenCostPriority = new(5e-7)
		p.CacheReadInputTokenCostAbove272kTokensPriority = new(0.000001)
		// token count exceeds 272k — but flex flat rate should still win
		assert.Equal(t, 1.3e-7, tieredCacheReadInputTokenRate(&p, 280000, serviceTier{isFlex: true}))
	})
}

// =========================================================================
// tierFromResponse tests
// =========================================================================

func TestTierFromResponse_Priority(t *testing.T) {
	s := schemas.BifrostServiceTierPriority
	tier := tierFromResponse(&s, nil, nil)
	assert.True(t, tier.isPriority)
	assert.False(t, tier.isFlex)
}

func TestTierFromResponse_Flex(t *testing.T) {
	s := schemas.BifrostServiceTierFlex
	tier := tierFromResponse(&s, nil, nil)
	assert.False(t, tier.isPriority)
	assert.True(t, tier.isFlex)
}

func TestTierFromResponse_Ultrafast(t *testing.T) {
	s := schemas.BifrostServiceTierUltrafast
	tier := tierFromResponse(&s, nil, nil)
	assert.False(t, tier.isPriority)
	assert.False(t, tier.isFlex)
	assert.True(t, tier.isUltrafast)
}

// OpenAI renamed Priority processing to Fast mode on 2026-07-30 and accepts
// service_tier "priority" and "fast" interchangeably, so "fast" bills on the
// priority columns. It must not flip isFast, which is the Anthropic speed flag.
func TestTierFromResponse_Fast(t *testing.T) {
	s := schemas.BifrostServiceTierFast
	tier := tierFromResponse(&s, nil, nil)
	assert.True(t, tier.isPriority)
	assert.False(t, tier.isFlex)
	assert.False(t, tier.isUltrafast)
	assert.False(t, tier.isFast)
}

func TestUltrafastRates(t *testing.T) {
	p := configstoreTables.TableModelPricing{
		InputCostPerToken:                    new(1.0),
		OutputCostPerToken:                   new(2.0),
		CacheReadInputTokenCost:              new(0.5),
		CacheCreationInputTokenCost:          new(0.75),
		InputCostPerTokenUltrafast:           new(3.0),
		OutputCostPerTokenUltrafast:          new(6.0),
		CacheReadInputTokenCostUltrafast:     new(1.5),
		CacheCreationInputTokenCostUltrafast: new(2.25),
	}
	tier := serviceTier{isUltrafast: true}
	assert.Equal(t, 3.0, tieredInputRate(&p, 1000, tier))
	assert.Equal(t, 6.0, tieredOutputRate(&p, 1000, tier))
	assert.Equal(t, 1.5, tieredCacheReadInputTokenRate(&p, 1000, tier))
	assert.Equal(t, 2.25, tieredCacheCreationInputTokenRate(&p, 1000, tier))
}

func TestUltrafastRatesFallBackToStandardWhenUnconfigured(t *testing.T) {
	p := configstoreTables.TableModelPricing{
		InputCostPerToken:           new(1.0),
		OutputCostPerToken:          new(2.0),
		CacheReadInputTokenCost:     new(0.5),
		CacheCreationInputTokenCost: new(0.75),
	}
	tier := serviceTier{isUltrafast: true}
	assert.Equal(t, 1.0, tieredInputRate(&p, 1000, tier))
	assert.Equal(t, 2.0, tieredOutputRate(&p, 1000, tier))
	assert.Equal(t, 0.5, tieredCacheReadInputTokenRate(&p, 1000, tier))
	assert.Equal(t, 0.75, tieredCacheCreationInputTokenRate(&p, 1000, tier))
}

func TestUltrafastAbove272kRates(t *testing.T) {
	p := configstoreTables.TableModelPricing{
		InputCostPerToken:                                   new(1.0),
		OutputCostPerToken:                                  new(2.0),
		CacheReadInputTokenCost:                             new(0.5),
		CacheCreationInputTokenCost:                         new(0.75),
		InputCostPerTokenAbove272kTokens:                    new(10.0),
		OutputCostPerTokenAbove272kTokens:                   new(20.0),
		CacheReadInputTokenCostAbove272kTokens:              new(5.0),
		CacheCreationInputTokenCostAbove272kTokens:          new(7.5),
		InputCostPerTokenUltrafast:                          new(3.0),
		OutputCostPerTokenUltrafast:                         new(6.0),
		CacheReadInputTokenCostUltrafast:                    new(1.5),
		CacheCreationInputTokenCostUltrafast:                new(2.25),
		InputCostPerTokenAbove272kTokensUltrafast:           new(30.0),
		OutputCostPerTokenAbove272kTokensUltrafast:          new(60.0),
		CacheReadInputTokenCostAbove272kTokensUltrafast:     new(15.0),
		CacheCreationInputTokenCostAbove272kTokensUltrafast: new(22.5),
	}
	tier := serviceTier{isUltrafast: true}
	above := TokenTierAbove272K + 1

	assert.Equal(t, 30.0, tieredInputRate(&p, above, tier))
	assert.Equal(t, 60.0, tieredOutputRate(&p, above, tier))
	assert.Equal(t, 15.0, tieredCacheReadInputTokenRate(&p, above, tier))
	assert.Equal(t, 22.5, tieredCacheCreationInputTokenRate(&p, above, tier))

	// At or below the boundary the flat ultrafast rate still applies.
	assert.Equal(t, 3.0, tieredInputRate(&p, TokenTierAbove272K, tier))
	assert.Equal(t, 6.0, tieredOutputRate(&p, TokenTierAbove272K, tier))
	assert.Equal(t, 1.5, tieredCacheReadInputTokenRate(&p, TokenTierAbove272K, tier))
	assert.Equal(t, 2.25, tieredCacheCreationInputTokenRate(&p, TokenTierAbove272K, tier))

	// Standard tier above 272k is unaffected by the ultrafast long-context rates.
	assert.Equal(t, 10.0, tieredInputRate(&p, above, serviceTier{}))
	assert.Equal(t, 20.0, tieredOutputRate(&p, above, serviceTier{}))
	assert.Equal(t, 5.0, tieredCacheReadInputTokenRate(&p, above, serviceTier{}))
	assert.Equal(t, 7.5, tieredCacheCreationInputTokenRate(&p, above, serviceTier{}))
}

func TestUltrafastAbove272kFallsBackToFlatUltrafast(t *testing.T) {
	p := configstoreTables.TableModelPricing{
		InputCostPerToken:                          new(1.0),
		OutputCostPerToken:                         new(2.0),
		CacheReadInputTokenCost:                    new(0.5),
		CacheCreationInputTokenCost:                new(0.75),
		InputCostPerTokenAbove272kTokens:           new(10.0),
		OutputCostPerTokenAbove272kTokens:          new(20.0),
		CacheReadInputTokenCostAbove272kTokens:     new(5.0),
		CacheCreationInputTokenCostAbove272kTokens: new(7.5),
		InputCostPerTokenUltrafast:                 new(3.0),
		OutputCostPerTokenUltrafast:                new(6.0),
		CacheReadInputTokenCostUltrafast:           new(1.5),
		CacheCreationInputTokenCostUltrafast:       new(2.25),
	}
	tier := serviceTier{isUltrafast: true}
	above := TokenTierAbove272K + 1
	assert.Equal(t, 3.0, tieredInputRate(&p, above, tier))
	assert.Equal(t, 6.0, tieredOutputRate(&p, above, tier))
	assert.Equal(t, 1.5, tieredCacheReadInputTokenRate(&p, above, tier))
	assert.Equal(t, 2.25, tieredCacheCreationInputTokenRate(&p, above, tier))
}

// TestUltrafastAbove272kFromDatasheetJSON feeds the gpt-6-astra datasheet entry
// through the production JSON -> row conversion, so a missing json tag or
// conversion-map line for the long-context ultrafast rates fails here.
func TestUltrafastAbove272kFromDatasheetJSON(t *testing.T) {
	p := pricingRowFromDatasheetJSON(t, "gpt-6-astra", `{
		"provider": "openai", "mode": "responses", "base_model": "gpt-6-astra",
		"input_cost_per_token": 0.00001,
		"input_cost_per_token_above_272k_tokens": 0.00002,
		"input_cost_per_token_ultrafast": 0.00006,
		"input_cost_per_token_above_272k_tokens_ultrafast": 0.00012,
		"output_cost_per_token_ultrafast": 0.0003,
		"output_cost_per_token_above_272k_tokens_ultrafast": 0.00045,
		"cache_read_input_token_cost_ultrafast": 0.000006,
		"cache_read_input_token_cost_above_272k_tokens_ultrafast": 0.000012,
		"cache_creation_input_token_cost_ultrafast": 0.000075,
		"cache_creation_input_token_cost_above_272k_tokens_ultrafast": 0.00015
	}`)
	tier := serviceTier{isUltrafast: true}
	short, long := 100_000, 300_000

	assert.InDelta(t, 0.00006, tieredInputRate(&p, short, tier), 1e-15)
	assert.InDelta(t, 0.00012, tieredInputRate(&p, long, tier), 1e-15)
	assert.InDelta(t, 0.0003, tieredOutputRate(&p, short, tier), 1e-15)
	assert.InDelta(t, 0.00045, tieredOutputRate(&p, long, tier), 1e-15)
	assert.InDelta(t, 0.000006, tieredCacheReadInputTokenRate(&p, short, tier), 1e-15)
	assert.InDelta(t, 0.000012, tieredCacheReadInputTokenRate(&p, long, tier), 1e-15)
	assert.InDelta(t, 0.000075, tieredCacheCreationInputTokenRate(&p, short, tier), 1e-15)
	assert.InDelta(t, 0.00015, tieredCacheCreationInputTokenRate(&p, long, tier), 1e-15)

	roundTrip := convertTablePricingToEntry(&p)
	require.NotNil(t, roundTrip.InputCostPerTokenAbove272kTokensUltrafast)
	require.NotNil(t, roundTrip.OutputCostPerTokenAbove272kTokensUltrafast)
	require.NotNil(t, roundTrip.CacheReadInputTokenCostAbove272kTokensUltrafast)
	require.NotNil(t, roundTrip.CacheCreationInputTokenCostAbove272kTokensUltrafast)
}

func TestTierFromResponse_Default(t *testing.T) {
	for _, s := range []schemas.BifrostServiceTier{schemas.BifrostServiceTierAuto, schemas.BifrostServiceTierDefault, ""} {
		tier := tierFromResponse(&s, nil, nil)
		assert.False(t, tier.isPriority, "expected no priority for %q", s)
		assert.False(t, tier.isFlex, "expected no flex for %q", s)
		assert.False(t, tier.isUltrafast, "expected no ultrafast for %q", s)
	}
}

func TestTierFromResponse_Nil(t *testing.T) {
	tier := tierFromResponse(nil, nil, nil)
	assert.False(t, tier.isPriority)
	assert.False(t, tier.isFlex)
}

// =========================================================================
// Flex tier tests
// =========================================================================

func TestComputeTextCost_FlexUsesFlexRate(t *testing.T) {
	p := chatPricing(0.000003, 0.000015)
	p.InputCostPerTokenFlex = new(0.0000015)
	p.OutputCostPerTokenFlex = new(0.0000075)

	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     1000,
		CompletionTokens: 500,
		TotalTokens:      1500,
	}

	cost := computeTextCostTotal(&p, usage, serviceTier{isFlex: true})

	// Flex rates: 1000*0.0000015 + 500*0.0000075 = 0.0015 + 0.00375 = 0.00525
	assert.InDelta(t, 0.00525, cost, 1e-12)
}

func TestComputeTextCost_NonFlexIgnoresFlexRate(t *testing.T) {
	p := chatPricing(0.000003, 0.000015)
	p.InputCostPerTokenFlex = new(0.0000015)
	p.OutputCostPerTokenFlex = new(0.0000075)

	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     1000,
		CompletionTokens: 500,
		TotalTokens:      1500,
	}

	cost := computeTextCostTotal(&p, usage, serviceTier{})

	// Base rates, flex fields ignored: 1000*0.000003 + 500*0.000015 = 0.003 + 0.0075 = 0.0105
	assert.InDelta(t, 0.0105, cost, 1e-12)
}

func TestComputeTextCost_FlexIgnoresTokenTiers(t *testing.T) {
	// Flex is a flat rate — token-count tiers (272k, 200k, 128k) do not apply.
	p := chatPricing(0.000003, 0.000015)
	p.InputCostPerTokenFlex = new(0.0000015)
	p.OutputCostPerTokenFlex = new(0.0000075)
	p.InputCostPerTokenAbove272kTokens = new(0.000009)
	p.OutputCostPerTokenAbove272kTokens = new(0.000045)

	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     250000,
		CompletionTokens: 30000,
		TotalTokens:      280000,
	}

	cost := computeTextCostTotal(&p, usage, serviceTier{isFlex: true})

	// Flex flat rate overrides 272k tier: 250000*0.0000015 + 30000*0.0000075 = 0.375 + 0.225 = 0.60
	assert.InDelta(t, 0.60, cost, 1e-9)
}

func TestComputeTextCost_FlexCacheReadRate(t *testing.T) {
	p := chatPricing(0.000003, 0.000015)
	p.InputCostPerTokenFlex = new(0.0000015)
	p.OutputCostPerTokenFlex = new(0.0000075)
	p.CacheReadInputTokenCost = new(0.0000003)
	p.CacheReadInputTokenCostFlex = new(0.0000006)

	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     1000,
		CompletionTokens: 500,
		TotalTokens:      1500,
		PromptTokensDetails: &schemas.ChatPromptTokensDetails{
			CachedReadTokens: 400,
		},
	}

	cost := computeTextCostTotal(&p, usage, serviceTier{isFlex: true})

	// Non-cached input: (1000-400)*0.0000015 = 600*0.0000015 = 0.0009
	// Cached read (flex rate): 400*0.0000006 = 0.00024
	// Output: 500*0.0000075 = 0.00375
	// Total: 0.0009 + 0.00024 + 0.00375 = 0.00489
	assert.InDelta(t, 0.00489, cost, 1e-12)
}

func TestComputeTextCost_FlexFallsBackToBaseWhenNoFlexRate(t *testing.T) {
	// isFlex set but no flex fields configured — falls back to base rates.
	p := chatPricing(0.000003, 0.000015)

	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     1000,
		CompletionTokens: 500,
		TotalTokens:      1500,
	}

	cost := computeTextCostTotal(&p, usage, serviceTier{isFlex: true})

	// Base rates used as fallback: 1000*0.000003 + 500*0.000015 = 0.003 + 0.0075 = 0.0105
	assert.InDelta(t, 0.0105, cost, 1e-12)
}

func TestCalculateCost_FlexTier_EndToEnd(t *testing.T) {
	tier := schemas.BifrostServiceTierFlex
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gpt-4o", "openai", "chat"): {
			Model:                  "gpt-4o",
			Provider:               "openai",
			Mode:                   "chat",
			InputCostPerToken:      new(0.000005),
			OutputCostPerToken:     new(0.000015),
			InputCostPerTokenFlex:  new(0.0000025),
			OutputCostPerTokenFlex: new(0.0000075),
		},
	})

	resp := &schemas.BifrostResponse{
		ChatResponse: &schemas.BifrostChatResponse{
			ServiceTier: &tier,
			Usage: &schemas.BifrostLLMUsage{
				PromptTokens:     1000,
				CompletionTokens: 500,
				TotalTokens:      1500,
			},
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType: schemas.ChatCompletionRequest,
				RoutingInfo: routingInfoFor(schemas.OpenAI, "gpt-4o"),
			},
		},
	}

	cost := s.CalculateCost(resp, nil)
	// Flex rates: 1000*0.0000025 + 500*0.0000075 = 0.0025 + 0.00375 = 0.00625
	assert.InDelta(t, 0.00625, cost, 1e-12)
}

func TestCalculateCost_FlexTier_FallsBackToBaseWhenNoFlexRate(t *testing.T) {
	tier := schemas.BifrostServiceTierFlex
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gpt-4o", "openai", "chat"): chatPricing(0.000005, 0.000015),
	})

	resp := &schemas.BifrostResponse{
		ChatResponse: &schemas.BifrostChatResponse{
			ServiceTier: &tier,
			Usage: &schemas.BifrostLLMUsage{
				PromptTokens:     1000,
				CompletionTokens: 500,
				TotalTokens:      1500,
			},
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType: schemas.ChatCompletionRequest,
				RoutingInfo: routingInfoFor(schemas.OpenAI, "gpt-4o"),
			},
		},
	}

	cost := s.CalculateCost(resp, nil)
	// No flex rates configured — falls back to base: 1000*0.000005 + 500*0.000015 = 0.005 + 0.0075 = 0.0125
	assert.InDelta(t, 0.0125, cost, 1e-12)
}

func TestCalculateCost_ProviderCostZeroTotalStillCalculates(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gpt-4o", "openai", "chat"): chatPricing(0.000005, 0.000015),
	})

	// Provider cost present but TotalCost is 0 → our calculation runs
	resp := makeChatResponse(schemas.OpenAI, "gpt-4o", &schemas.BifrostLLMUsage{
		PromptTokens:     1000,
		CompletionTokens: 500,
		TotalTokens:      1500,
		Cost: &schemas.BifrostCost{
			TotalCost: 0,
		},
	})

	cost := s.CalculateCost(resp, nil)
	assert.InDelta(t, 0.0125, cost, 1e-12)
}

func TestCalculateCost_AllCachedTokens(t *testing.T) {
	// All prompt tokens are from cache
	p := chatPricing(0.000005, 0.000015)
	p.CacheReadInputTokenCost = bifrost.Ptr(0.0000005)

	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     1000,
		CompletionTokens: 0,
		TotalTokens:      1000,
		PromptTokensDetails: &schemas.ChatPromptTokensDetails{
			CachedReadTokens: 1000, // All cached
		},
	}

	cost := computeTextCostTotal(&p, usage, serviceTier{})
	// Non-cached: 0, cached: 1000*0.0000005 = 0.0005
	assert.InDelta(t, 0.0005, cost, 1e-12)
}

// =========================================================================
// Nil usage fallbacks — per-unit pricing when no token data is reported
// =========================================================================

func TestCalculateCost_ImageGeneration_NilUsage_PerImagePricing(t *testing.T) {
	// Image response exists but Usage is nil — should default to 1 image with per-image pricing
	pricing := configstoreTables.TableModelPricing{
		Model:              "dall-e-3",
		Provider:           "openai",
		Mode:               "image_generation",
		InputCostPerToken:  bifrost.Ptr(0.0),
		OutputCostPerImage: bifrost.Ptr(0.04),
	}

	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("dall-e-3", "openai", "image_generation"): pricing,
	})

	resp := makeImageResponse("openai", "dall-e-3", nil)
	cost := s.CalculateCost(resp, nil)
	// 1 image * $0.04 = $0.04
	assert.InDelta(t, 0.04, cost, 1e-12)
}

func TestCalculateCost_ImageGeneration_NilUsage_InputAndOutputPerImage(t *testing.T) {
	// Both input and output per-image pricing, but no NumInputImages set
	pricing := configstoreTables.TableModelPricing{
		Model:              "test-image-model",
		Provider:           "test",
		Mode:               "image_generation",
		InputCostPerImage:  bifrost.Ptr(0.01),
		OutputCostPerImage: bifrost.Ptr(0.04),
	}

	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("test-image-model", "test", "image_generation"): pricing,
	})

	resp := makeImageResponse("test", "test-image-model", nil)
	cost := s.CalculateCost(resp, nil)
	// NumInputImages is 0 (not populated from request), so only output pricing applies
	// 1 output image * $0.04 = $0.04
	assert.InDelta(t, 0.04, cost, 1e-12)
}

func TestCalculateCost_ImageGeneration_WithInputImages(t *testing.T) {
	// Input + output per-image pricing with NumInputImages populated from request
	pricing := configstoreTables.TableModelPricing{
		Model:              "gpt-image-1",
		Provider:           "openai",
		Mode:               "image_generation",
		InputCostPerImage:  bifrost.Ptr(0.01),
		OutputCostPerImage: bifrost.Ptr(0.04),
	}

	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gpt-image-1", "openai", "image_generation"): pricing,
	})

	resp := makeImageResponse("openai", "gpt-image-1", &schemas.ImageUsage{
		NumInputImages: 2,
	})
	cost := s.CalculateCost(resp, nil)
	// 2 input images * $0.01 + 1 output image * $0.04 = $0.06
	assert.InDelta(t, 0.06, cost, 1e-12)
}

func TestCalculateCost_ImageGeneration_OutputCountFromData(t *testing.T) {
	// Output image count derived from len(Data) via populateOutputImageCount
	pricing := configstoreTables.TableModelPricing{
		Model:              "dall-e-3",
		Provider:           "openai",
		Mode:               "image_generation",
		OutputCostPerImage: bifrost.Ptr(0.04),
	}

	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("dall-e-3", "openai", "image_generation"): pricing,
	})

	resp := &schemas.BifrostResponse{
		ImageGenerationResponse: &schemas.BifrostImageGenerationResponse{
			Data: []schemas.ImageData{
				{URL: "https://example.com/img1.png", Index: 0},
				{URL: "https://example.com/img2.png", Index: 1},
				{URL: "https://example.com/img3.png", Index: 2},
			},
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType: schemas.ImageGenerationRequest,
				RoutingInfo: routingInfoFor("openai", "dall-e-3"),
			},
		},
	}
	cost := s.CalculateCost(resp, nil)
	// 3 output images * $0.04 = $0.12
	assert.InDelta(t, 0.12, cost, 1e-12)
}

func TestCalculateCost_ImageGeneration_NilUsage_NoPerImagePricing(t *testing.T) {
	// No per-image pricing and no tokens — should return 0
	pricing := configstoreTables.TableModelPricing{
		Model:              "token-only-model",
		Provider:           "test",
		Mode:               "image_generation",
		InputCostPerToken:  bifrost.Ptr(0.000001),
		OutputCostPerToken: bifrost.Ptr(0.000002),
	}

	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("token-only-model", "test", "image_generation"): pricing,
	})

	resp := makeImageResponse("test", "token-only-model", nil)
	cost := s.CalculateCost(resp, nil)
	// No per-image pricing and all tokens are zero → 0
	assert.InDelta(t, 0.0, cost, 1e-12)
}

func TestCalculateCost_ImageGeneration_EmptyUsage_PerImagePricing(t *testing.T) {
	// Usage exists but all fields are zero — same as nil usage, should use per-image pricing
	pricing := configstoreTables.TableModelPricing{
		Model:              "dall-e-3",
		Provider:           "openai",
		Mode:               "image_generation",
		OutputCostPerImage: bifrost.Ptr(0.04),
	}

	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("dall-e-3", "openai", "image_generation"): pricing,
	})

	resp := makeImageResponse("openai", "dall-e-3", &schemas.ImageUsage{})
	cost := s.CalculateCost(resp, nil)
	assert.InDelta(t, 0.04, cost, 1e-12)
}

func TestComputeImageCost_MixedInputTokensOutputPerImage(t *testing.T) {
	// Input has tokens (text prompt), output has no tokens but per-image pricing
	p := configstoreTables.TableModelPricing{
		InputCostPerToken:  bifrost.Ptr(0.000005),
		OutputCostPerToken: bifrost.Ptr(0.000015),
		OutputCostPerImage: bifrost.Ptr(0.04),
	}
	usage := &schemas.ImageUsage{
		InputTokens:         500,
		OutputTokensDetails: &schemas.ImageTokenDetails{NImages: 2},
	}
	cost := computeImageCostTotal(&p, usage, "", "", serviceTier{})
	// Input: 500 tokens * $0.000005 = $0.0025
	// Output: no output tokens → falls back to 2 images * $0.04 = $0.08
	assert.InDelta(t, 0.0825, cost, 1e-12)
}

func TestComputeImageCost_MixedInputPerImageOutputTokens(t *testing.T) {
	// Input has no tokens but per-image count, output has tokens
	p := configstoreTables.TableModelPricing{
		InputCostPerToken:  bifrost.Ptr(0.000005),
		OutputCostPerToken: bifrost.Ptr(0.000015),
		InputCostPerImage:  bifrost.Ptr(0.01),
	}
	usage := &schemas.ImageUsage{
		NumInputImages: 3,
		OutputTokens:   1000,
	}
	cost := computeImageCostTotal(&p, usage, "", "", serviceTier{})
	// Input: no input tokens → falls back to 3 images * $0.01 = $0.03
	// Output: 1000 tokens * $0.000015 = $0.015
	assert.InDelta(t, 0.045, cost, 1e-12)
}

func TestComputeImageCost_BothHaveTokens_IgnoresPerImage(t *testing.T) {
	// Both sides have tokens — per-image pricing is ignored
	p := configstoreTables.TableModelPricing{
		InputCostPerToken:  bifrost.Ptr(0.000005),
		OutputCostPerToken: bifrost.Ptr(0.000015),
		InputCostPerImage:  bifrost.Ptr(0.01),
		OutputCostPerImage: bifrost.Ptr(0.04),
	}
	usage := &schemas.ImageUsage{
		InputTokens:    200,
		OutputTokens:   800,
		TotalTokens:    1000,
		NumInputImages: 3,
	}
	cost := computeImageCostTotal(&p, usage, "", "", serviceTier{})
	// Input: 200 * $0.000005 = $0.001 (tokens present, per-image ignored)
	// Output: 800 * $0.000015 = $0.012 (tokens present, per-image ignored)
	assert.InDelta(t, 0.013, cost, 1e-12)
}

func TestCalculateCost_ResponsesWithCodeInterpreter(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gpt-4.1", "openai", "chat"): chatPricing(0.000002, 0.000008),
	})

	ciType := schemas.ResponsesMessageTypeCodeInterpreterCall
	resp := &schemas.BifrostResponse{
		ResponsesResponse: &schemas.BifrostResponsesResponse{
			Usage: &schemas.ResponsesResponseUsage{
				InputTokens:  579,
				OutputTokens: 334,
				TotalTokens:  913,
			},
			Output: []schemas.ResponsesMessage{
				{Type: &ciType},
				{Type: &ciType},
			},
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType: schemas.ResponsesRequest,
				RoutingInfo: routingInfoFor(schemas.OpenAI, "gpt-4.1"),
			},
		},
	}

	cost := s.CalculateCost(resp, nil)
	// Token cost only: 579*0.000002 + 334*0.000008 = 0.001158 + 0.002672 = 0.003830
	// Session cost is now tracked via ContainerCreateRequest, not per-response
	assert.InDelta(t, 0.003830, cost, 1e-6)
}

// ---------------------------------------------------------------------------
// computeContainerCreationCost
// ---------------------------------------------------------------------------

func TestComputeContainerCreationCost_Basic(t *testing.T) {
	p := configstoreTables.TableModelPricing{
		Model:                         "container",
		Provider:                      "openai",
		Mode:                          "chat",
		CodeInterpreterCostPerSession: bifrost.Ptr(0.03),
	}
	assert.InDelta(t, 0.03, computeContainerCreationCostTotal(&p), 1e-12)
}

func TestComputeContainerCreationCost_NilPricing(t *testing.T) {
	assert.Equal(t, 0.0, computeContainerCreationCostTotal(nil))
}

func TestComputeContainerCreationCost_NilRate(t *testing.T) {
	p := configstoreTables.TableModelPricing{
		Model:    "container",
		Provider: "openai",
		Mode:     "chat",
	}
	assert.Equal(t, 0.0, computeContainerCreationCostTotal(&p))
}

// ---------------------------------------------------------------------------
// ContainerCreateRequest end-to-end via CalculateCost
// ---------------------------------------------------------------------------

func TestCalculateCost_ContainerCreate_NoMemoryLimit(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("container", "openai", "chat"): {
			Model:                         "container",
			Provider:                      "openai",
			Mode:                          "chat",
			CodeInterpreterCostPerSession: bifrost.Ptr(0.03),
		},
	})

	resp := &schemas.BifrostResponse{
		ContainerCreateResponse: &schemas.BifrostContainerCreateResponse{
			ID:   "cntr_abc123",
			Name: "test-container",
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType: schemas.ContainerCreateRequest,
				RoutingInfo: schemas.RoutingInfo{Provider: schemas.OpenAI},
			},
		},
	}

	cost := s.CalculateCost(resp, nil)
	assert.InDelta(t, 0.03, cost, 1e-12)
}

func TestCalculateCost_ContainerCreate_MemorySpecificEntry(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("container", "openai", "chat"): {
			Model:                         "container",
			Provider:                      "openai",
			Mode:                          "chat",
			CodeInterpreterCostPerSession: bifrost.Ptr(0.03),
		},
		makeKey("container-4g", "openai", "chat"): {
			Model:                         "container-4g",
			Provider:                      "openai",
			Mode:                          "chat",
			CodeInterpreterCostPerSession: bifrost.Ptr(0.12),
		},
	})

	resp := &schemas.BifrostResponse{
		ContainerCreateResponse: &schemas.BifrostContainerCreateResponse{
			ID:          "cntr_abc123",
			Name:        "test-container",
			MemoryLimit: "4g",
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType: schemas.ContainerCreateRequest,
				RoutingInfo: schemas.RoutingInfo{Provider: schemas.OpenAI},
			},
		},
	}

	cost := s.CalculateCost(resp, nil)
	assert.InDelta(t, 0.12, cost, 1e-12)
}

func TestCalculateCost_ContainerCreate_FallsBackToBaseEntry(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("container", "openai", "chat"): {
			Model:                         "container",
			Provider:                      "openai",
			Mode:                          "chat",
			CodeInterpreterCostPerSession: bifrost.Ptr(0.03),
		},
	})

	resp := &schemas.BifrostResponse{
		ContainerCreateResponse: &schemas.BifrostContainerCreateResponse{
			ID:          "cntr_abc123",
			Name:        "test-container",
			MemoryLimit: "4g",
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType: schemas.ContainerCreateRequest,
				RoutingInfo: schemas.RoutingInfo{Provider: schemas.OpenAI},
			},
		},
	}

	// No container-4g entry — should fall back to base "container" rate
	cost := s.CalculateCost(resp, nil)
	assert.InDelta(t, 0.03, cost, 1e-12)
}

func TestCalculateCost_ContainerCreate_NoEntry(t *testing.T) {
	s := testStoreWithPricing(nil)

	resp := &schemas.BifrostResponse{
		ContainerCreateResponse: &schemas.BifrostContainerCreateResponse{
			ID:   "cntr_abc123",
			Name: "test-container",
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType: schemas.ContainerCreateRequest,
				RoutingInfo: schemas.RoutingInfo{Provider: schemas.OpenAI},
			},
		},
	}

	cost := s.CalculateCost(resp, nil)
	assert.Equal(t, 0.0, cost)
}

// ---------------------------------------------------------------------------
// Backward-compat: RoutingInfo missing → synthesize from deprecated triplet
//
// Covers callers stuck on the legacy ExtraFields shape:
//   - LoggerPlugin.RecalculateCosts replaying logs written before RoutingInfo existed
//   - Third-party plugins / SDK users that haven't migrated to RoutingInfo
//
// The fallback only fires when RoutingInfo is fully empty (zero Provider,
// zero Model, nil ResolvedKeyAlias). Any partial population is trusted.
// ---------------------------------------------------------------------------

func TestCalculateCost_BackCompat_LegacyFieldsOnly_NoAlias(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gpt-4o", "openai", "chat"): chatPricing(0.000005, 0.000015),
	})

	// Caller populates only the deprecated triplet — no RoutingInfo.
	// Pricing should fall back to Provider + OriginalModelRequested.
	resp := &schemas.BifrostResponse{
		ChatResponse: &schemas.BifrostChatResponse{
			Usage: &schemas.BifrostLLMUsage{PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500},
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType:            schemas.ChatCompletionRequest,
				Provider:               schemas.OpenAI,
				OriginalModelRequested: "gpt-4o",
			},
		},
	}

	cost := s.CalculateCost(resp, nil)
	// 1000 * 0.000005 + 500 * 0.000015 = 0.005 + 0.0075 = 0.0125
	assert.InDelta(t, 0.0125, cost, 1e-12)
}

func TestCalculateCost_BackCompat_LegacyFieldsOnly_WithAlias(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("my-deployment", "openai", "chat"): chatPricing(0.000005, 0.000015),
	})

	// Caller populates only the deprecated triplet with a distinct
	// ResolvedModelUsed (i.e. an alias was matched at original request time
	// and the wire model differs from the caller-facing name). The fallback
	// should route ResolvedModelUsed into ResolvedKeyAlias.ModelID so the
	// catalog lookup hits the deployment-keyed entry.
	resp := &schemas.BifrostResponse{
		ChatResponse: &schemas.BifrostChatResponse{
			Usage: &schemas.BifrostLLMUsage{PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500},
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType:            schemas.ChatCompletionRequest,
				Provider:               schemas.OpenAI,
				OriginalModelRequested: "my-alias-name",
				ResolvedModelUsed:      "my-deployment",
			},
		},
	}

	cost := s.CalculateCost(resp, nil)
	// 1000 * 0.000005 + 500 * 0.000015 = 0.0125, charged via the deployment-keyed entry
	assert.InDelta(t, 0.0125, cost, 1e-12)
}

func TestCalculateCost_BackCompat_RoutingInfoWinsOverLegacyFields(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gpt-4o", "openai", "chat"): chatPricing(0.000005, 0.000015),
		makeKey("gemini-2.0-flash", "gemini", "chat"): {
			Model:              "gemini-2.0-flash",
			Provider:           "gemini",
			Mode:               "chat",
			InputCostPerToken:  bifrost.Ptr(0.0000001),
			OutputCostPerToken: bifrost.Ptr(0.0000004),
		},
	})

	// Both populated. The modern fields (RoutingInfo) must win — the
	// fallback only fires when RoutingInfo is fully unset.
	resp := &schemas.BifrostResponse{
		ChatResponse: &schemas.BifrostChatResponse{
			Usage: &schemas.BifrostLLMUsage{PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500},
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType:            schemas.ChatCompletionRequest,
				RoutingInfo:            routingInfoFor(schemas.OpenAI, "gpt-4o"),
				Provider:               schemas.Gemini,
				OriginalModelRequested: "gemini-2.0-flash",
			},
		},
	}

	cost := s.CalculateCost(resp, nil)
	// Priced via RoutingInfo → openai/gpt-4o → 0.0125 (not the gemini rate).
	assert.InDelta(t, 0.0125, cost, 1e-12)
}

func TestCalculateCost_BackCompat_BothEmpty_ReturnsZero(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gpt-4o", "openai", "chat"): chatPricing(0.000005, 0.000015),
	})

	// Neither RoutingInfo nor the deprecated triplet are populated.
	// Pricing has no way to identify the model; cost is 0.
	resp := &schemas.BifrostResponse{
		ChatResponse: &schemas.BifrostChatResponse{
			Usage: &schemas.BifrostLLMUsage{PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500},
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType: schemas.ChatCompletionRequest,
			},
		},
	}

	cost := s.CalculateCost(resp, nil)
	assert.Equal(t, 0.0, cost)
}

func TestCalculateCost_BackCompat_PartialRoutingInfo_NoFallback(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gpt-4o", "openai", "chat"): chatPricing(0.000005, 0.000015),
	})

	// RoutingInfo has Model but no Provider. The legacy Provider field is
	// also set. The fallback MUST NOT fire — partial RoutingInfo means the
	// caller intended to use RoutingInfo. With Provider unset on RoutingInfo,
	// the catalog lookup fails and cost is 0. (This guards the trigger
	// against false positives.)
	resp := &schemas.BifrostResponse{
		ChatResponse: &schemas.BifrostChatResponse{
			Usage: &schemas.BifrostLLMUsage{PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500},
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType: schemas.ChatCompletionRequest,
				RoutingInfo: schemas.RoutingInfo{Model: "gpt-4o"},
				Provider:    schemas.OpenAI,
			},
		},
	}

	cost := s.CalculateCost(resp, nil)
	assert.Equal(t, 0.0, cost)
}

// TestCalculateCostForUsage_MatchesCalculateCost verifies the cost-from-usage helper:
// billing a bare usage object (the failure/cancel path) yields exactly the same
// cost as billing a full BifrostResponse carrying that usage (the success path),
// so failed-but-token-consuming requests are charged at identical rates.
func TestCalculateCostForUsage_MatchesCalculateCost(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gpt-4o", "openai", "chat"): chatPricing(0.000005, 0.000015),
	})

	usage := &schemas.BifrostLLMUsage{PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500}

	resp := &schemas.BifrostResponse{
		ChatResponse: &schemas.BifrostChatResponse{
			Usage: usage,
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType: schemas.ChatCompletionRequest,
				RoutingInfo: routingInfoFor(schemas.OpenAI, "gpt-4o"),
			},
		},
	}

	respCost := s.CalculateCost(resp, nil)
	usageCost := s.CalculateCostForUsage(usage, schemas.OpenAI, "gpt-4o", schemas.ChatCompletionRequest, nil)

	assert.Greater(t, respCost, 0.0, "response cost should be positive")
	assert.Equal(t, respCost, usageCost, "bare-usage cost must equal full-response cost")
}

// TestCalculateCostBreakdownForUsage_CarriesSplit verifies the breakdown variant
// returns the same total as the scalar path and carries the input/output split
// so bare-usage billing can denormalize per category.
func TestCalculateCostBreakdownForUsage_CarriesSplit(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gpt-4o", "openai", "chat"): chatPricing(0.000005, 0.000015),
	})

	usage := &schemas.BifrostLLMUsage{PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500}

	bd := s.CalculateCostBreakdownForUsage(usage, schemas.OpenAI, "gpt-4o", schemas.ChatCompletionRequest, nil)
	require.NotNil(t, bd)
	// 1000*0.000005 input, 500*0.000015 output.
	assert.InDelta(t, 0.005, bd.InputCost, 1e-12)
	assert.InDelta(t, 0.0075, bd.OutputCost, 1e-12)
	assert.InDelta(t, bd.TotalCost, bd.InputCost+bd.OutputCost+bd.AdditionalCost, 1e-12)
	// Total matches the scalar path exactly.
	assert.Equal(t, s.CalculateCostForUsage(usage, schemas.OpenAI, "gpt-4o", schemas.ChatCompletionRequest, nil), bd.TotalCost)
}

// TestCalculateCostForUsage_NilUsageIsZero verifies the helper bills nothing
// when there is no usage (e.g. a failure that consumed no tokens).
func TestCalculateCostForUsage_NilUsageIsZero(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gpt-4o", "openai", "chat"): chatPricing(0.000005, 0.000015),
	})
	assert.Equal(t, 0.0, s.CalculateCostForUsage(nil, schemas.OpenAI, "gpt-4o", schemas.ChatCompletionRequest, nil))
}

// TestCalculateCostForUsage_AppliesServedTier verifies the cancel/failure billing
// path honors the served Anthropic tier carried internally on the usage (fast mode
// + data residency), so interrupted fast or US-residency streams keep their
// multiplier instead of being billed as standard/global.
func TestCalculateCostForUsage_AppliesServedTier(t *testing.T) {
	p := chatPricing(0.000005, 0.000015) // base $5/$15 per MTok
	p.InputCostPerTokenFast = bifrost.Ptr(0.00001)
	p.OutputCostPerTokenFast = bifrost.Ptr(0.00003)
	p.InferenceGeoUSMultiplier = bifrost.Ptr(1.1)
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("claude-x", "anthropic", "chat"): p,
	})
	mk := func(speed, geo *string) *schemas.BifrostLLMUsage {
		return &schemas.BifrostLLMUsage{PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500, Speed: speed, InferenceGeo: geo}
	}
	cost := func(u *schemas.BifrostLLMUsage) float64 {
		return s.CalculateCostForUsage(u, schemas.Anthropic, "claude-x", schemas.ChatCompletionRequest, nil)
	}
	fast, us := "fast", "us"

	// No served tier → base rate.
	assert.InDelta(t, 1000*0.000005+500*0.000015, cost(mk(nil, nil)), 1e-12)
	// speed:"fast" → flat fast rate.
	assert.InDelta(t, 1000*0.00001+500*0.00003, cost(mk(&fast, nil)), 1e-12)
	// inference_geo:"us" → 1.1x on base.
	assert.InDelta(t, (1000*0.000005+500*0.000015)*1.1, cost(mk(nil, &us)), 1e-12)
}

// ===========================================================================
// Golden per-model OpenAI pricing tests
//
// These feed the exact published OpenAI rates through the real datasheet
// JSON -> TableModelPricing conversion (convertEntryToTablePricing) and assert
// the invoice cost for every service tier and context window. They pin two
// things at once: the rate-selection ladders in cost.go, and the field wiring
// that carries each new rate from the datasheet JSON into the pricing row.
//
// NOTE: the numbers below are the source of truth for *expected billing*,
// transcribed from the OpenAI pricing page. The live per-model values arrive
// via the datasheet sync (not this repo), so these tests validate the compute
// engine + wiring, not the sync payload.
// ===========================================================================

// pricingRowFromDatasheetJSON parses a datasheet entry (the shape stored in the
// synced pricing catalog) and runs the production JSON -> row conversion, so a
// dropped json tag or a missing conversion-map line would fail these tests.
func pricingRowFromDatasheetJSON(t *testing.T, modelKey, blob string) configstoreTables.TableModelPricing {
	t.Helper()
	var entry Entry
	require.NoError(t, json.Unmarshal([]byte(blob), &entry))
	return convertEntryToTablePricing(modelKey, entry)
}

// Datasheet entries (pricing fields only) for the gpt-5.6 flagship family.
// Cache writes are the cache_creation_* fields. Short/long context are the
// base vs _above_272k rates.
const (
	gpt56SolDatasheet = `{
		"provider": "openai", "mode": "chat", "base_model": "gpt-5.6-sol",
		"input_cost_per_token": 0.000005,
		"input_cost_per_token_above_272k_tokens": 0.00001,
		"input_cost_per_token_flex": 0.0000025,
		"input_cost_per_token_flex_above_272k_tokens": 0.000005,
		"input_cost_per_token_priority": 0.00001,
		"output_cost_per_token": 0.00003,
		"output_cost_per_token_above_272k_tokens": 0.000045,
		"output_cost_per_token_flex": 0.000015,
		"output_cost_per_token_flex_above_272k_tokens": 0.0000225,
		"output_cost_per_token_priority": 0.00006,
		"cache_read_input_token_cost": 0.0000005,
		"cache_read_input_token_cost_above_272k_tokens": 0.000001,
		"cache_read_input_token_cost_flex": 0.00000025,
		"cache_read_input_token_cost_flex_above_272k_tokens": 0.0000005,
		"cache_read_input_token_cost_priority": 0.000001,
		"cache_creation_input_token_cost": 0.00000625,
		"cache_creation_input_token_cost_above_272k_tokens": 0.0000125,
		"cache_creation_input_token_cost_flex": 0.000003125,
		"cache_creation_input_token_cost_flex_above_272k_tokens": 0.00000625,
		"cache_creation_input_token_cost_priority": 0.0000125
	}`
	gpt56TerraDatasheet = `{
		"provider": "openai", "mode": "chat", "base_model": "gpt-5.6-terra",
		"input_cost_per_token": 0.0000025,
		"input_cost_per_token_above_272k_tokens": 0.000005,
		"input_cost_per_token_flex": 0.00000125,
		"input_cost_per_token_flex_above_272k_tokens": 0.0000025,
		"input_cost_per_token_priority": 0.000005,
		"output_cost_per_token": 0.000015,
		"output_cost_per_token_above_272k_tokens": 0.0000225,
		"output_cost_per_token_flex": 0.0000075,
		"output_cost_per_token_flex_above_272k_tokens": 0.00001125,
		"output_cost_per_token_priority": 0.00003,
		"cache_read_input_token_cost": 0.00000025,
		"cache_read_input_token_cost_above_272k_tokens": 0.0000005,
		"cache_read_input_token_cost_flex": 0.000000125,
		"cache_read_input_token_cost_flex_above_272k_tokens": 0.00000025,
		"cache_read_input_token_cost_priority": 0.0000005,
		"cache_creation_input_token_cost": 0.000003125,
		"cache_creation_input_token_cost_above_272k_tokens": 0.00000625,
		"cache_creation_input_token_cost_flex": 0.0000015625,
		"cache_creation_input_token_cost_flex_above_272k_tokens": 0.000003125,
		"cache_creation_input_token_cost_priority": 0.00000625
	}`
	gpt56LunaDatasheet = `{
		"provider": "openai", "mode": "chat", "base_model": "gpt-5.6-luna",
		"input_cost_per_token": 0.000001,
		"input_cost_per_token_above_272k_tokens": 0.000002,
		"input_cost_per_token_flex": 0.0000005,
		"input_cost_per_token_flex_above_272k_tokens": 0.000001,
		"input_cost_per_token_priority": 0.000002,
		"output_cost_per_token": 0.000006,
		"output_cost_per_token_above_272k_tokens": 0.000009,
		"output_cost_per_token_flex": 0.000003,
		"output_cost_per_token_flex_above_272k_tokens": 0.0000045,
		"output_cost_per_token_priority": 0.000012,
		"cache_read_input_token_cost": 0.0000001,
		"cache_read_input_token_cost_above_272k_tokens": 0.0000002,
		"cache_read_input_token_cost_flex": 0.00000005,
		"cache_read_input_token_cost_flex_above_272k_tokens": 0.0000001,
		"cache_read_input_token_cost_priority": 0.0000002,
		"cache_creation_input_token_cost": 0.00000125,
		"cache_creation_input_token_cost_above_272k_tokens": 0.0000025,
		"cache_creation_input_token_cost_flex": 0.000000625,
		"cache_creation_input_token_cost_flex_above_272k_tokens": 0.00000125,
		"cache_creation_input_token_cost_priority": 0.0000025
	}`
	// gpt-5.5: long-context tiering but NO published cache-write rate ("-").
	gpt55Datasheet = `{
		"provider": "openai", "mode": "chat", "base_model": "gpt-5.5",
		"input_cost_per_token": 0.000005,
		"input_cost_per_token_above_272k_tokens": 0.00001,
		"input_cost_per_token_flex": 0.0000025,
		"input_cost_per_token_flex_above_272k_tokens": 0.000005,
		"input_cost_per_token_priority": 0.0000125,
		"output_cost_per_token": 0.00003,
		"output_cost_per_token_above_272k_tokens": 0.000045,
		"output_cost_per_token_flex": 0.000015,
		"output_cost_per_token_flex_above_272k_tokens": 0.0000225,
		"output_cost_per_token_priority": 0.000075,
		"cache_read_input_token_cost": 0.0000005,
		"cache_read_input_token_cost_above_272k_tokens": 0.000001,
		"cache_read_input_token_cost_flex": 0.00000025,
		"cache_read_input_token_cost_flex_above_272k_tokens": 0.0000005,
		"cache_read_input_token_cost_priority": 0.00000125
	}`
	// gpt-5.4-mini: NO long-context tier and NO cache-write rate.
	gpt54MiniDatasheet = `{
		"provider": "openai", "mode": "chat", "base_model": "gpt-5.4-mini",
		"input_cost_per_token": 0.00000075,
		"input_cost_per_token_flex": 0.000000375,
		"input_cost_per_token_priority": 0.0000015,
		"output_cost_per_token": 0.0000045,
		"output_cost_per_token_flex": 0.00000225,
		"output_cost_per_token_priority": 0.000009,
		"cache_read_input_token_cost": 0.000000075,
		"cache_read_input_token_cost_flex": 0.0000000375,
		"cache_read_input_token_cost_priority": 0.00000015
	}`
)

// TestGoldenOpenAIPricing_GPT56Family asserts the exact invoice cost for every
// (tier x context) cell of the gpt-5.6 pricing tables, including cache read and
// cache write. Each row hardcodes the rate that *should* apply, so a mis-selected
// tier or context bucket fails the assertion.
func TestGoldenOpenAIPricing_GPT56Family(t *testing.T) {
	sol := pricingRowFromDatasheetJSON(t, "gpt-5.6-sol", gpt56SolDatasheet)
	terra := pricingRowFromDatasheetJSON(t, "gpt-5.6-terra", gpt56TerraDatasheet)
	luna := pricingRowFromDatasheetJSON(t, "gpt-5.6-luna", gpt56LunaDatasheet)

	// Short context stays under 272k; long context crosses it.
	const (
		shortPrompt, shortRead, shortWrite, shortOut = 10000, 4000, 2000, 1000
		longPrompt, longRead, longWrite, longOut     = 300000, 100000, 50000, 1000
	)

	type rates struct{ in, cacheRead, cacheWrite, out float64 }
	cases := []struct {
		name    string
		pricing configstoreTables.TableModelPricing
		tier    serviceTier
		long    bool
		r       rates
	}{
		// gpt-5.6-sol
		{"sol/standard/short", sol, serviceTier{}, false, rates{0.000005, 0.0000005, 0.00000625, 0.00003}},
		{"sol/standard/long", sol, serviceTier{}, true, rates{0.00001, 0.000001, 0.0000125, 0.000045}},
		{"sol/flex/short", sol, serviceTier{isFlex: true}, false, rates{0.0000025, 0.00000025, 0.000003125, 0.000015}},
		{"sol/flex/long", sol, serviceTier{isFlex: true}, true, rates{0.000005, 0.0000005, 0.00000625, 0.0000225}},
		{"sol/priority/short", sol, serviceTier{isPriority: true}, false, rates{0.00001, 0.000001, 0.0000125, 0.00006}},
		// gpt-5.6-terra
		{"terra/standard/short", terra, serviceTier{}, false, rates{0.0000025, 0.00000025, 0.000003125, 0.000015}},
		{"terra/standard/long", terra, serviceTier{}, true, rates{0.000005, 0.0000005, 0.00000625, 0.0000225}},
		{"terra/flex/short", terra, serviceTier{isFlex: true}, false, rates{0.00000125, 0.000000125, 0.0000015625, 0.0000075}},
		{"terra/flex/long", terra, serviceTier{isFlex: true}, true, rates{0.0000025, 0.00000025, 0.000003125, 0.00001125}},
		{"terra/priority/short", terra, serviceTier{isPriority: true}, false, rates{0.000005, 0.0000005, 0.00000625, 0.00003}},
		// gpt-5.6-luna
		{"luna/standard/short", luna, serviceTier{}, false, rates{0.000001, 0.0000001, 0.00000125, 0.000006}},
		{"luna/standard/long", luna, serviceTier{}, true, rates{0.000002, 0.0000002, 0.0000025, 0.000009}},
		{"luna/flex/short", luna, serviceTier{isFlex: true}, false, rates{0.0000005, 0.00000005, 0.000000625, 0.000003}},
		{"luna/flex/long", luna, serviceTier{isFlex: true}, true, rates{0.000001, 0.0000001, 0.00000125, 0.0000045}},
		{"luna/priority/short", luna, serviceTier{isPriority: true}, false, rates{0.000002, 0.0000002, 0.0000025, 0.000012}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prompt, read, write, out := shortPrompt, shortRead, shortWrite, shortOut
			if tc.long {
				prompt, read, write, out = longPrompt, longRead, longWrite, longOut
			}
			usage := &schemas.BifrostLLMUsage{
				PromptTokens:     prompt,
				CompletionTokens: out,
				TotalTokens:      prompt + out,
				PromptTokensDetails: &schemas.ChatPromptTokensDetails{
					CachedReadTokens:  read,
					CachedWriteTokens: write,
				},
			}
			p := tc.pricing
			cost := computeTextCostTotal(&p, usage, tc.tier)
			nonCached := prompt - read - write
			want := float64(nonCached)*tc.r.in + float64(read)*tc.r.cacheRead + float64(write)*tc.r.cacheWrite + float64(out)*tc.r.out
			assert.InDelta(t, want, cost, 1e-9)
		})
	}
}

// gpt6AstraDatasheet is the live openai/gpt-6-astra entry (pricing fields only).
// OpenAI publishes the Fast tier (ex-Priority, renamed 2026-07-30) and the
// datasheet stores it under the _priority keys: $20/$2/$25/$100 per 1M short
// context, $40/$4/$50/$150 above 272k.
const gpt6AstraDatasheet = `{
		"provider": "openai", "mode": "responses", "base_model": "gpt-6-astra",
		"input_cost_per_token": 0.00001,
		"input_cost_per_token_above_272k_tokens": 0.00002,
		"input_cost_per_token_priority": 0.00002,
		"input_cost_per_token_above_272k_tokens_priority": 0.00004,
		"output_cost_per_token": 0.00005,
		"output_cost_per_token_above_272k_tokens": 0.000075,
		"output_cost_per_token_priority": 0.0001,
		"output_cost_per_token_above_272k_tokens_priority": 0.00015,
		"cache_read_input_token_cost": 0.000001,
		"cache_read_input_token_cost_above_272k_tokens": 0.000002,
		"cache_read_input_token_cost_priority": 0.000002,
		"cache_read_input_token_cost_above_272k_tokens_priority": 0.000004,
		"cache_creation_input_token_cost": 0.0000125,
		"cache_creation_input_token_cost_above_272k_tokens": 0.000025,
		"cache_creation_input_token_cost_priority": 0.000025,
		"cache_creation_input_token_cost_above_272k_tokens_priority": 0.00005
	}`

// TestGoldenOpenAIPricing_GPT6AstraFast drives the served service_tier string
// through tierFromResponse (not a pre-built serviceTier) so the wire value
// "fast" is what selects the rates. The short-context row is the exact usage
// from a customer report that Bifrost billed at standard ($0.00886) instead of
// Fast ($0.01772). The "default" rows pin the downgrade path: OpenAI echoes
// service_tier "default" when it serves a Fast request at standard speed and
// charges standard rates.
func TestGoldenOpenAIPricing_GPT6AstraFast(t *testing.T) {
	astra := pricingRowFromDatasheetJSON(t, "gpt-6-astra", gpt6AstraDatasheet)

	type rates struct{ in, cacheRead, out float64 }
	cases := []struct {
		name              string
		served            schemas.BifrostServiceTier
		prompt, read, out int
		r                 rates
	}{
		// Reported request: 7283 input (7280 cached), 31 output, no cache writes.
		{"fast/short", schemas.BifrostServiceTierFast, 7283, 7280, 31, rates{0.00002, 0.000002, 0.0001}},
		{"priority/short", schemas.BifrostServiceTierPriority, 7283, 7280, 31, rates{0.00002, 0.000002, 0.0001}},
		{"default/short", schemas.BifrostServiceTierDefault, 7283, 7280, 31, rates{0.00001, 0.000001, 0.00005}},
		// Above 272k the long-context priority columns apply.
		{"fast/long", schemas.BifrostServiceTierFast, 300000, 100000, 1000, rates{0.00004, 0.000004, 0.00015}},
		{"default/long", schemas.BifrostServiceTierDefault, 300000, 100000, 1000, rates{0.00002, 0.000002, 0.000075}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			usage := &schemas.BifrostLLMUsage{
				PromptTokens:     tc.prompt,
				CompletionTokens: tc.out,
				TotalTokens:      tc.prompt + tc.out,
				PromptTokensDetails: &schemas.ChatPromptTokensDetails{
					CachedReadTokens: tc.read,
				},
			}
			served := tc.served
			p := astra
			cost := computeTextCostTotal(&p, usage, tierFromResponse(&served, nil, nil))
			nonCached := tc.prompt - tc.read
			want := float64(nonCached)*tc.r.in + float64(tc.read)*tc.r.cacheRead + float64(tc.out)*tc.r.out
			assert.InDelta(t, want, cost, 1e-9)
		})
	}

	// The reported invoice, in dollars: 3 x $20/M + 7280 x $2/M + 31 x $100/M.
	fast := schemas.BifrostServiceTierFast
	reported := &schemas.BifrostLLMUsage{
		PromptTokens: 7283, CompletionTokens: 31, TotalTokens: 7314,
		PromptTokensDetails: &schemas.ChatPromptTokensDetails{CachedReadTokens: 7280},
	}
	assert.InDelta(t, 0.017720, computeTextCostTotal(&astra, reported, tierFromResponse(&fast, nil, nil)), 1e-9)
}

// TestGoldenOpenAIPricing_GPT6AstraFastLongContextCacheWrite pins the Fast
// (priority-column) cache-write rate above 272k through the production JSON -> row
// conversion: OpenAI publishes $50/M for Fast cache writes on long context, twice
// the $25/M short-context Fast rate, and the datasheet carries it as
// cache_creation_input_token_cost_above_272k_tokens_priority.
func TestGoldenOpenAIPricing_GPT6AstraFastLongContextCacheWrite(t *testing.T) {
	astra := pricingRowFromDatasheetJSON(t, "gpt-6-astra", gpt6AstraDatasheet)
	fast := schemas.BifrostServiceTierFast
	tier := tierFromResponse(&fast, nil, nil)

	// 272k is the boundary: at or below it the flat $25/M Fast rate applies.
	assert.InDelta(t, 0.000025, tieredCacheCreationInputTokenRate(&astra, TokenTierAbove272K, tier), 1e-15)
	assert.InDelta(t, 0.00005, tieredCacheCreationInputTokenRate(&astra, 300000, tier), 1e-15)
	// Standard above 272k is unaffected.
	assert.InDelta(t, 0.000025, tieredCacheCreationInputTokenRate(&astra, 300000, serviceTier{}), 1e-15)

	// Long-context Fast invoice with cache writes: 200k uncached x $40/M + 50k read x $4/M
	// + 50k write x $50/M + 1k out x $150/M.
	usage := &schemas.BifrostLLMUsage{
		PromptTokens: 300000, CompletionTokens: 1000, TotalTokens: 301000,
		PromptTokensDetails: &schemas.ChatPromptTokensDetails{CachedReadTokens: 50000, CachedWriteTokens: 50000},
	}
	want := 200000*0.00004 + 50000*0.000004 + 50000*0.00005 + 1000*0.00015
	assert.InDelta(t, want, computeTextCostTotal(&astra, usage, tier), 1e-9)
}

// TestGoldenOpenAIPricing_NoCacheWriteModels covers models that OpenAI prices
// without a cache-write rate (gpt-5.5) and without a long-context tier
// (gpt-5.4-mini): cache-write tokens fall back to the input rate, and a >272k
// request on a model with no long-context rate stays on the base rate.
func TestGoldenOpenAIPricing_NoCacheWriteModels(t *testing.T) {
	gpt55 := pricingRowFromDatasheetJSON(t, "gpt-5.5", gpt55Datasheet)
	gpt54mini := pricingRowFromDatasheetJSON(t, "gpt-5.4-mini", gpt54MiniDatasheet)

	// gpt-5.5 has no cache-write rate: write tokens bill at the (long-context) input rate.
	t.Run("gpt-5.5/standard/long: cache-write falls back to input rate", func(t *testing.T) {
		usage := &schemas.BifrostLLMUsage{
			PromptTokens: 300000, CompletionTokens: 1000, TotalTokens: 301000,
			PromptTokensDetails: &schemas.ChatPromptTokensDetails{CachedReadTokens: 100000, CachedWriteTokens: 50000},
		}
		p := gpt55
		cost := computeTextCostTotal(&p, usage, serviceTier{})
		// non-cached 150000*0.00001 + read 100000*0.000001 + write 50000*0.00001 (input fallback) + out 1000*0.000045
		want := 150000*0.00001 + 100000*0.000001 + 50000*0.00001 + 1000*0.000045
		assert.InDelta(t, want, cost, 1e-9)
	})

	t.Run("gpt-5.5/flex/short", func(t *testing.T) {
		usage := &schemas.BifrostLLMUsage{
			PromptTokens: 10000, CompletionTokens: 1000, TotalTokens: 11000,
			PromptTokensDetails: &schemas.ChatPromptTokensDetails{CachedReadTokens: 4000},
		}
		p := gpt55
		cost := computeTextCostTotal(&p, usage, serviceTier{isFlex: true})
		want := 6000*0.0000025 + 4000*0.00000025 + 1000*0.000015
		assert.InDelta(t, want, cost, 1e-12)
	})

	t.Run("gpt-5.5/priority/short", func(t *testing.T) {
		usage := &schemas.BifrostLLMUsage{
			PromptTokens: 10000, CompletionTokens: 1000, TotalTokens: 11000,
			PromptTokensDetails: &schemas.ChatPromptTokensDetails{CachedReadTokens: 4000},
		}
		p := gpt55
		cost := computeTextCostTotal(&p, usage, serviceTier{isPriority: true})
		want := 6000*0.0000125 + 4000*0.00000125 + 1000*0.000075
		assert.InDelta(t, want, cost, 1e-9)
	})

	// gpt-5.4-mini has no long-context rate: a >272k request must use the base rate.
	t.Run("gpt-5.4-mini/standard/above-272k uses base rate", func(t *testing.T) {
		usage := &schemas.BifrostLLMUsage{
			PromptTokens: 300000, CompletionTokens: 1000, TotalTokens: 301000,
			PromptTokensDetails: &schemas.ChatPromptTokensDetails{CachedReadTokens: 50000},
		}
		p := gpt54mini
		cost := computeTextCostTotal(&p, usage, serviceTier{})
		want := 250000*0.00000075 + 50000*0.000000075 + 1000*0.0000045
		assert.InDelta(t, want, cost, 1e-9)
	})
}

// TestCalculateCost_GPT56_Responses_FlexLongContext_EndToEnd drives the full
// pipeline for a gpt-5.6 Responses call: service_tier=flex + a >272k prompt with
// cache writes, through tier detection, usage mapping, pricing lookup, and cost.
func TestCalculateCost_GPT56_Responses_FlexLongContext_EndToEnd(t *testing.T) {
	sol := pricingRowFromDatasheetJSON(t, "gpt-5.6-sol", gpt56SolDatasheet)
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gpt-5.6-sol", "openai", "responses"): sol,
	})
	tier := schemas.BifrostServiceTierFlex
	resp := &schemas.BifrostResponse{
		ResponsesResponse: &schemas.BifrostResponsesResponse{
			ServiceTier: &tier,
			Usage: &schemas.ResponsesResponseUsage{
				InputTokens:  300000,
				OutputTokens: 1000,
				TotalTokens:  301000,
				InputTokensDetails: &schemas.ResponsesResponseInputTokens{
					CachedReadTokens:  100000,
					CachedWriteTokens: 50000,
				},
			},
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType: schemas.ResponsesRequest,
				RoutingInfo: routingInfoFor(schemas.OpenAI, "gpt-5.6-sol"),
			},
		},
	}
	cost := s.CalculateCost(resp, nil)
	// flex long-context: 150000*0.000005 + 100000*0.0000005 + 50000*0.00000625 + 1000*0.0000225
	want := 150000*0.000005 + 100000*0.0000005 + 50000*0.00000625 + 1000*0.0000225
	assert.InDelta(t, want, cost, 1e-9)
}

// TestTieredCacheCreationRate_PriorityWinsOver200kBand verifies a priority cache-write
// request in the 200k–272k band uses the flat priority rate, not the standard >200k
// rate. The priority cache-write rate is flat, so it must take precedence over the
// standard context tiers.
func TestTieredCacheCreationRate_PriorityWinsOver200kBand(t *testing.T) {
	p := configstoreTables.TableModelPricing{
		CacheCreationInputTokenCost:                bifrost.Ptr(0.000001),
		CacheCreationInputTokenCostAbove200kTokens: bifrost.Ptr(0.000002), // standard >200k — must NOT win for priority
		CacheCreationInputTokenCostPriority:        bifrost.Ptr(0.000005), // flat priority — must win
	}
	// 250k tokens: >200k and ≤272k.
	assert.Equal(t, 0.000005, tieredCacheCreationInputTokenRate(&p, 250000, serviceTier{isPriority: true}))
	// Non-priority at the same size still uses the standard >200k rate.
	assert.Equal(t, 0.000002, tieredCacheCreationInputTokenRate(&p, 250000, serviceTier{}))
}

// TestTieredCacheCreationRate_PriorityAbove272k verifies the long-context priority
// cache-write column wins above 272k and that, when a catalog lacks it, the flat
// priority rate still covers the whole context window (never the standard >272k rate).
func TestTieredCacheCreationRate_PriorityAbove272k(t *testing.T) {
	p := configstoreTables.TableModelPricing{
		CacheCreationInputTokenCost:                        bifrost.Ptr(0.000001),
		CacheCreationInputTokenCostAbove272kTokens:         bifrost.Ptr(0.000002),
		CacheCreationInputTokenCostPriority:                bifrost.Ptr(0.000005),
		CacheCreationInputTokenCostAbove272kTokensPriority: bifrost.Ptr(0.00001),
	}
	priority := serviceTier{isPriority: true}
	assert.Equal(t, 0.00001, tieredCacheCreationInputTokenRate(&p, 300000, priority))
	assert.Equal(t, 0.000005, tieredCacheCreationInputTokenRate(&p, TokenTierAbove272K, priority))
	assert.Equal(t, 0.000002, tieredCacheCreationInputTokenRate(&p, 300000, serviceTier{}))

	p.CacheCreationInputTokenCostAbove272kTokensPriority = nil
	assert.Equal(t, 0.000005, tieredCacheCreationInputTokenRate(&p, 300000, priority), "flat priority covers >272k when the long-context column is absent")
}

// ---------------------------------------------------------------------------
// Anthropic server-side fallback: price the model that actually served
// ---------------------------------------------------------------------------

// seedFallbackPricing loads the real published Fable 5 / Opus 4.8 rates. Fable 5
// is exactly 2x Opus 4.8 on both input and output, which is what makes pricing a
// fallback-served turn against the requested model a clean doubling.
func seedFallbackPricing(s *Store) {
	s.pricingData[makeKey("claude-fable-5", "anthropic", "responses")] = configstoreTables.TableModelPricing{
		Model: "claude-fable-5", Provider: "anthropic", Mode: "responses",
		InputCostPerToken:  bifrost.Ptr(10.0 / 1_000_000),
		OutputCostPerToken: bifrost.Ptr(50.0 / 1_000_000),
	}
	s.pricingData[makeKey("claude-opus-4-8", "anthropic", "responses")] = configstoreTables.TableModelPricing{
		Model: "claude-opus-4-8", Provider: "anthropic", Mode: "responses",
		InputCostPerToken:  bifrost.Ptr(5.0 / 1_000_000),
		OutputCostPerToken: bifrost.Ptr(25.0 / 1_000_000),
	}
}

// fallbackResponse mirrors a real server-side-fallback payload: the caller asked
// for claude-fable-5, claude-opus-4-8 served, and the top-level usage mirrors the
// serving attempt.
func fallbackResponse(servingModel *string, in, out int) *schemas.BifrostResponse {
	return &schemas.BifrostResponse{
		ResponsesResponse: &schemas.BifrostResponsesResponse{
			Model: "claude-opus-4-8",
			Usage: &schemas.ResponsesResponseUsage{InputTokens: in, OutputTokens: out},
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType: schemas.ResponsesRequest,
				RoutingInfo: schemas.RoutingInfo{
					Provider:                schemas.Anthropic,
					Model:                   "claude-fable-5",
					ServerSideFallbackModel: servingModel,
				},
			},
		},
	}
}

func TestCalculateCost_ServerSideFallback_PricesServingModel(t *testing.T) {
	s := newTestStore()
	seedFallbackPricing(s)

	// Real payload: 31 in / 257 out, served by Opus after Fable declined.
	// Before the fix this returned the Fable figure, which is exactly double.
	resp := fallbackResponse(bifrost.Ptr("claude-opus-4-8"), 31, 257)
	assert.InDelta(t, 31*(5.0/1_000_000)+257*(25.0/1_000_000), s.CalculateCost(resp, nil), 1e-12,
		"expected Opus 4.8 rates (the serving model), not Fable 5")
}

// No handoff — the overwhelming majority of responses — must be untouched, even
// though the response's own model field names a different model.
func TestCalculateCost_NoServerSideFallback_UsesRoutingModel(t *testing.T) {
	s := newTestStore()
	seedFallbackPricing(s)

	resp := fallbackResponse(nil, 31, 257)
	assert.InDelta(t, 31*(10.0/1_000_000)+257*(50.0/1_000_000), s.CalculateCost(resp, nil), 1e-12,
		"without a recorded handoff, pricing must stay on the requested model")
}

// An unpriceable serving model falls through the candidate chain to the
// requested model rather than collapsing the cost to zero.
func TestCalculateCost_ServerSideFallback_UnknownServingModelFallsThrough(t *testing.T) {
	s := newTestStore()
	seedFallbackPricing(s)

	resp := fallbackResponse(bifrost.Ptr("claude-not-in-catalog"), 31, 257)
	cost := s.CalculateCost(resp, nil)
	assert.InDelta(t, 31*(10.0/1_000_000)+257*(50.0/1_000_000), cost, 1e-12)
	assert.NotZero(t, cost, "an unknown serving model must not zero the cost")
}

// A key alias would otherwise win the lookup; the serving model outranks it.
func TestCalculateCost_ServerSideFallback_OutranksKeyAlias(t *testing.T) {
	s := newTestStore()
	seedFallbackPricing(s)

	resp := fallbackResponse(bifrost.Ptr("claude-opus-4-8"), 31, 257)
	resp.ResponsesResponse.ExtraFields.RoutingInfo.Model = "my-fast-alias"
	resp.ResponsesResponse.ExtraFields.RoutingInfo.ResolvedKeyAlias = &schemas.ResolvedKeyAlias{
		ModelID:   "claude-fable-5",
		ModelName: bifrost.Ptr("claude-fable-5"),
	}

	assert.InDelta(t, 31*(5.0/1_000_000)+257*(25.0/1_000_000), s.CalculateCost(resp, nil), 1e-12,
		"the alias must not outrank the model that actually served")
}

// Overrides follow the serving model, so a negotiated rate for the model that
// actually ran is the one that applies.
func TestCalculateCost_ServerSideFallback_OverridesKeyOnServingModel(t *testing.T) {
	s := newTestStore()
	seedFallbackPricing(s)
	providerID := "anthropic"
	require.NoError(t, s.SetOverrides([]configstoreTables.TablePricingOverride{{
		ID:               "opus-negotiated-rate",
		ScopeKind:        string(ScopeKindProvider),
		ProviderID:       &providerID,
		MatchType:        string(MatchTypeExact),
		Pattern:          "claude-opus-4-8",
		RequestTypes:     []schemas.RequestType{schemas.ResponsesRequest},
		PricingPatchJSON: `{"input_cost_per_token":0.000001,"output_cost_per_token":0.000002}`,
	}}))

	resp := fallbackResponse(bifrost.Ptr("claude-opus-4-8"), 31, 257)
	assert.InDelta(t, 31*0.000001+257*0.000002, s.CalculateCost(resp, nil), 1e-12,
		"an override for the serving model must apply to a fallback-served turn")
}

// Streaming is billed through CalculateCostForUsage, which receives only
// (usage, provider, requestedModel) and never sees RoutingInfo. A live
// fallback-served stream (38 in / 345 out, served by Opus after Fable declined)
// was reported at $0.01763 — exactly the Fable figure, i.e. double.
func TestCalculateCostForUsage_ServerSideFallback_PricesServingModel(t *testing.T) {
	s := newTestStore()
	seedFallbackPricing(s)

	usage := &schemas.BifrostLLMUsage{
		PromptTokens:            38,
		CompletionTokens:        345,
		TotalTokens:             383,
		ServerSideFallbackModel: bifrost.Ptr("claude-opus-4-8"),
	}
	got := s.CalculateCostForUsage(usage, schemas.Anthropic, "claude-fable-5", schemas.ResponsesRequest, nil)
	assert.InDelta(t, 38*(5.0/1_000_000)+345*(25.0/1_000_000), got, 1e-12,
		"expected Opus 4.8 rates for a fallback-served stream")
	assert.InDelta(t, 0.008815, got, 1e-6, "expected the Opus figure, not the $0.01763 Fable one")
}

// Sticky-routed stream: a single fallback_message iteration, 38 in / 320 out,
// reported live at $0.01638 (Fable). Should be $0.00819.
func TestCalculateCostForUsage_ServerSideFallback_StickyStream(t *testing.T) {
	s := newTestStore()
	seedFallbackPricing(s)

	usage := &schemas.BifrostLLMUsage{
		PromptTokens: 38, CompletionTokens: 320, TotalTokens: 358,
		ServerSideFallbackModel: bifrost.Ptr("claude-opus-4-8"),
	}
	assert.InDelta(t, 0.00819,
		s.CalculateCostForUsage(usage, schemas.Anthropic, "claude-fable-5", schemas.ResponsesRequest, nil), 1e-6)
}

// No handoff: the bare-usage path must keep pricing the requested model.
func TestCalculateCostForUsage_NoServerSideFallback_Unchanged(t *testing.T) {
	s := newTestStore()
	seedFallbackPricing(s)

	usage := &schemas.BifrostLLMUsage{PromptTokens: 38, CompletionTokens: 345, TotalTokens: 383}
	assert.InDelta(t, 38*(10.0/1_000_000)+345*(50.0/1_000_000),
		s.CalculateCostForUsage(usage, schemas.Anthropic, "claude-fable-5", schemas.ResponsesRequest, nil), 1e-12)
}

func TestCalculateCostForUsage_BatchResultsUsesBatchRates(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gpt-4o-mini", "openai", "chat"): {
			Model:                     "gpt-4o-mini",
			Provider:                  "openai",
			Mode:                      "chat",
			InputCostPerToken:         bifrost.Ptr(0.000010),
			OutputCostPerToken:        bifrost.Ptr(0.000020),
			InputCostPerTokenBatches:  bifrost.Ptr(0.000005),
			OutputCostPerTokenBatches: bifrost.Ptr(0.000010),
		},
	})

	cost := s.CalculateCostForUsage(&schemas.BifrostLLMUsage{
		PromptTokens:     100,
		CompletionTokens: 20,
		TotalTokens:      120,
	}, schemas.OpenAI, "gpt-4o-mini", schemas.BatchResultsRequest, nil)

	assert.InDelta(t, 0.0007, cost, 1e-12)
}

func TestCalculateCostForUsage_BatchCacheRatesStackWithBatchDiscount(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("claude-batch", "anthropic", "chat"): {
			Model:                               "claude-batch",
			Provider:                            "anthropic",
			Mode:                                "chat",
			InputCostPerToken:                   bifrost.Ptr(0.000003),
			OutputCostPerToken:                  bifrost.Ptr(0.000015),
			InputCostPerTokenBatches:            bifrost.Ptr(0.0000015),
			OutputCostPerTokenBatches:           bifrost.Ptr(0.0000075),
			CacheReadInputTokenCost:             bifrost.Ptr(0.0000003),
			CacheCreationInputTokenCost:         bifrost.Ptr(0.00000375),
			CacheCreationInputTokenCostAbove1hr: bifrost.Ptr(0.000006),
		},
	})

	details := s.CalculateBatchCostDetailsForUsage(&schemas.BifrostLLMUsage{
		PromptTokens:     1000,
		CompletionTokens: 100,
		TotalTokens:      1100,
		PromptTokensDetails: &schemas.ChatPromptTokensDetails{
			CachedReadTokens:  400,
			CachedWriteTokens: 300,
			CachedWriteTokenDetails: &schemas.ChatCachedWriteTokenDetails{
				CachedWriteTokens5m: 200,
				CachedWriteTokens1h: 100,
			},
		},
	}, schemas.Anthropic, "claude-batch", schemas.ChatCompletionRequest, nil)
	cost, priced := details.Cost, details.Priced

	require.True(t, priced)
	// Batch ratio is 0.5. Input: 300*1.5e-6 + 400*0.15e-6 +
	// 200*1.875e-6 + 100*3e-6. Output: 100*7.5e-6.
	assert.InDelta(t, 0.001935, cost, 1e-12)
}

// TXE-6959: the batch path prices OpenAI's 12h cache-write tier at the above-1hr rate and the
// 30m tier at the base cache-write rate, each scaled by the batch discount.
func TestCalculateCostForUsage_BatchOpenAICacheWriteTiers(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gpt-batch", "openai", "chat"): {
			Model:                               "gpt-batch",
			Provider:                            "openai",
			Mode:                                "chat",
			InputCostPerToken:                   bifrost.Ptr(0.000001),
			OutputCostPerToken:                  bifrost.Ptr(0.000008),
			InputCostPerTokenBatches:            bifrost.Ptr(0.0000005),
			OutputCostPerTokenBatches:           bifrost.Ptr(0.000004),
			CacheCreationInputTokenCost:         bifrost.Ptr(0.00000125),
			CacheCreationInputTokenCostAbove1hr: bifrost.Ptr(0.0000025),
		},
	})

	details := s.CalculateBatchCostDetailsForUsage(&schemas.BifrostLLMUsage{
		PromptTokens:     10000,
		CompletionTokens: 100,
		TotalTokens:      10100,
		PromptTokensDetails: &schemas.ChatPromptTokensDetails{
			CachedWriteTokens: 8000,
			CachedWriteTokenDetails: &schemas.ChatCachedWriteTokenDetails{
				CachedWriteTokens30m: 6000,
				CachedWriteTokens12h: 2000,
			},
		},
	}, schemas.OpenAI, "gpt-batch", schemas.ChatCompletionRequest, nil)

	require.True(t, details.Priced)
	// Batch ratio 0.5. Input: 2000*0.5e-6 + 6000*0.625e-6 + 2000*1.25e-6. Output: 100*4e-6.
	assert.InDelta(t, 0.001+0.00375+0.0025+0.0004, details.Cost, 1e-12)
}

func TestCalculateCostForUsage_BatchAbove200kTierScalesWithBatchDiscount(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gpt-5.x-batch", "openai", "chat"): {
			Model:                                  "gpt-5.x-batch",
			Provider:                               "openai",
			Mode:                                   "chat",
			InputCostPerToken:                      bifrost.Ptr(0.000002),
			OutputCostPerToken:                     bifrost.Ptr(0.000008),
			InputCostPerTokenBatches:               bifrost.Ptr(0.000001),
			OutputCostPerTokenBatches:              bifrost.Ptr(0.000004),
			InputCostPerTokenAbove200kTokens:       bifrost.Ptr(0.000003),
			OutputCostPerTokenAbove200kTokens:      bifrost.Ptr(0.000012),
			CacheReadInputTokenCost:                bifrost.Ptr(0.0000005),
			CacheReadInputTokenCostAbove200kTokens: bifrost.Ptr(0.00000075),
		},
	})

	details := s.CalculateBatchCostDetailsForUsage(&schemas.BifrostLLMUsage{
		PromptTokens:     250_000,
		CompletionTokens: 1_000,
		TotalTokens:      251_000,
		PromptTokensDetails: &schemas.ChatPromptTokensDetails{
			CachedReadTokens: 50_000,
		},
	}, schemas.OpenAI, "gpt-5.x-batch", schemas.ChatCompletionRequest, nil)
	cost, priced := details.Cost, details.Priced

	require.True(t, priced)
	// batchRatio is 0.5. Above-200k tier applies (250k prompt tokens):
	// non-cached input: 200_000 * (3e-6 * 0.5) = 0.3
	// cached read:       50_000 * (0.75e-6 * 0.5) = 0.01875
	// output:             1_000 * (12e-6 * 0.5) = 0.006
	assert.InDelta(t, 0.32475, cost, 1e-12)
}

func TestCalculateCostForUsage_BatchEmbeddingUsesEmbeddingPricingMode(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("text-embedding-batch", "openai", "embedding"): {
			Model:                    "text-embedding-batch",
			Provider:                 "openai",
			Mode:                     "embedding",
			InputCostPerTokenBatches: bifrost.Ptr(0.00000001),
		},
	})

	details := s.CalculateBatchCostDetailsForUsage(&schemas.BifrostLLMUsage{
		PromptTokens: 1000,
		TotalTokens:  1000,
	}, schemas.OpenAI, "text-embedding-batch", schemas.EmbeddingRequest, nil)
	cost, priced := details.Cost, details.Priced
	require.True(t, priced)
	assert.InDelta(t, 0.00001, cost, 1e-12)
}

// TestCalculateBatchCostDetailsForUsage_DefaultsRateAndReportsIt exercises
// CalculateBatchCostDetailsForUsage directly (not through CalculateCostForUsage)
// to confirm the reported InputCostPerTokenBatches/OutputCostPerTokenBatches
// carry the defaulted rate — accounting.go persists these into the aggregate
// log's model_breakdown, so a nil here would silently lose the rate that was
// actually charged.
func TestCalculateBatchCostDetailsForUsage_DefaultsRateAndReportsIt(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gpt-4o-mini", "openai", "chat"): chatPricing(0.000010, 0.000020),
	})

	details := s.CalculateBatchCostDetailsForUsage(&schemas.BifrostLLMUsage{
		PromptTokens:     100,
		CompletionTokens: 20,
		TotalTokens:      120,
	}, schemas.OpenAI, "gpt-4o-mini", schemas.BatchResultsRequest, nil)

	require.True(t, details.Priced)
	assert.InDelta(t, 0.0007, details.Cost, 1e-12)
	require.NotNil(t, details.InputCostPerTokenBatches)
	assert.InDelta(t, 0.000005, *details.InputCostPerTokenBatches, 1e-12)
	require.NotNil(t, details.OutputCostPerTokenBatches)
	assert.InDelta(t, 0.000010, *details.OutputCostPerTokenBatches, 1e-12)
}

// TestCalculateCostForUsage_BatchResultsDefaultsToHalfStandardRate covers a
// model with standard chat pricing but no batch-specific rate: rather than
// refusing to price (the old behavior), it prices at defaultBatchPricingRatio
// of the standard rate. 0.000010/2=0.000005 and 0.000020/2=0.000010 are the
// exact rates TestCalculateCostForUsage_BatchResultsUsesBatchRates sets
// explicitly, so the two tests land on the identical cost by construction.
func TestCalculateCostForUsage_BatchResultsDefaultsToHalfStandardRate(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gpt-4o-mini", "openai", "chat"): chatPricing(0.000010, 0.000020),
	})

	cost := s.CalculateCostForUsage(&schemas.BifrostLLMUsage{
		PromptTokens:     100,
		CompletionTokens: 20,
		TotalTokens:      120,
	}, schemas.OpenAI, "gpt-4o-mini", schemas.BatchResultsRequest, nil)

	assert.InDelta(t, 0.0007, cost, 1e-12)
}

// TestCalculateCostForUsage_BatchResultsUnpricedWithNoStandardRateEither
// covers a model with no pricing at all — nothing to default 50% of, so it
// must still refuse rather than silently pricing at zero.
func TestCalculateCostForUsage_BatchResultsUnpricedWithNoStandardRateEither(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{})

	cost := s.CalculateCostForUsage(&schemas.BifrostLLMUsage{
		PromptTokens:     100,
		CompletionTokens: 20,
		TotalTokens:      120,
	}, schemas.OpenAI, "gpt-4o-mini", schemas.BatchResultsRequest, nil)

	assert.Equal(t, 0.0, cost)
}

// TestCalculateCostForUsage_BatchResults_CostPerRequestSurcharge covers the
// BatchResultsRequest branch in computeCostFromInput: it must fall through to
// the shared flat per-request surcharge like every other request type, not
// return early and skip it.
func TestCalculateCostForUsage_BatchResults_CostPerRequestSurcharge(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gpt-4o-mini", "openai", "chat"): {
			Model:                     "gpt-4o-mini",
			Provider:                  "openai",
			Mode:                      "chat",
			InputCostPerToken:         bifrost.Ptr(0.000010),
			OutputCostPerToken:        bifrost.Ptr(0.000020),
			InputCostPerTokenBatches:  bifrost.Ptr(0.000005),
			OutputCostPerTokenBatches: bifrost.Ptr(0.000010),
			CostPerRequest:            bifrost.Ptr(0.01),
		},
	})

	cost := s.CalculateCostForUsage(&schemas.BifrostLLMUsage{
		PromptTokens:     100,
		CompletionTokens: 20,
		TotalTokens:      120,
	}, schemas.OpenAI, "gpt-4o-mini", schemas.BatchResultsRequest, nil)

	// Token cost matches TestCalculateCostForUsage_BatchResultsUsesBatchRates
	// (0.0007) plus the flat per-request surcharge.
	assert.InDelta(t, 0.0007+0.01, cost, 1e-12)
}

// TestCalculateCostForUsage_BatchResults_InferenceGeoUSMultiplier covers the
// same branch applying the data-residency multiplier: batch pricing and US
// data residency are independent axes, so a batch result carrying
// inference_geo:"us" must still scale token/cache costs by the multiplier.
func TestCalculateCostForUsage_BatchResults_InferenceGeoUSMultiplier(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("claude-batch", "anthropic", "chat"): {
			Model:                     "claude-batch",
			Provider:                  "anthropic",
			Mode:                      "chat",
			InputCostPerToken:         bifrost.Ptr(0.000003),
			OutputCostPerToken:        bifrost.Ptr(0.000015),
			InputCostPerTokenBatches:  bifrost.Ptr(0.0000015),
			OutputCostPerTokenBatches: bifrost.Ptr(0.0000075),
			InferenceGeoUSMultiplier:  bifrost.Ptr(1.1),
		},
	})

	baseCost := 1000*0.0000015 + 100*0.0000075

	withUS := s.CalculateCostForUsage(&schemas.BifrostLLMUsage{
		PromptTokens:     1000,
		CompletionTokens: 100,
		TotalTokens:      1100,
		InferenceGeo:     bifrost.Ptr("us"),
	}, schemas.Anthropic, "claude-batch", schemas.BatchResultsRequest, nil)
	assert.InDelta(t, baseCost*1.1, withUS, 1e-12)

	without := s.CalculateCostForUsage(&schemas.BifrostLLMUsage{
		PromptTokens:     1000,
		CompletionTokens: 100,
		TotalTokens:      1100,
	}, schemas.Anthropic, "claude-batch", schemas.BatchResultsRequest, nil)
	assert.InDelta(t, baseCost, without, 1e-12)
}

// TestCalculateBatchCostDetailsForUsage_CostPerRequestSurcharge covers the
// real batch-settlement entry point (used by jobaccounting.summarizeResults
// and the recalculate-costs path, unlike CalculateCostForUsage's batch branch
// which only handles bare billed-usage on a failed retrieve). It must apply
// the same flat per-request surcharge CalculateCostForUsage does, so the two
// entry points never disagree on the cost of identical usage against the
// identical pricing row.
func TestCalculateBatchCostDetailsForUsage_CostPerRequestSurcharge(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gpt-4o-mini", "openai", "chat"): {
			Model:                     "gpt-4o-mini",
			Provider:                  "openai",
			Mode:                      "chat",
			InputCostPerToken:         bifrost.Ptr(0.000010),
			OutputCostPerToken:        bifrost.Ptr(0.000020),
			InputCostPerTokenBatches:  bifrost.Ptr(0.000005),
			OutputCostPerTokenBatches: bifrost.Ptr(0.000010),
			CostPerRequest:            bifrost.Ptr(0.01),
		},
	})

	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     100,
		CompletionTokens: 20,
		TotalTokens:      120,
	}

	details := s.CalculateBatchCostDetailsForUsage(usage, schemas.OpenAI, "gpt-4o-mini", schemas.BatchResultsRequest, nil)
	require.True(t, details.Priced)

	viaUsage := s.CalculateCostForUsage(usage, schemas.OpenAI, "gpt-4o-mini", schemas.BatchResultsRequest, nil)
	assert.InDelta(t, viaUsage, details.Cost, 1e-12)
	assert.InDelta(t, 0.0007+0.01, details.Cost, 1e-12)
}

func TestCalculateCost_RerankPerQuery(t *testing.T) {
	// Cohere and Bedrock both bill rerank per query rather than per token, so the token rates
	// on these rows are genuinely zero upstream. Before input_cost_per_query was wired, that
	// made every rerank request cost nothing.
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("rerank-v3.5", "cohere", "rerank"): {
			Model: "rerank-v3.5", Provider: "cohere", Mode: "rerank",
			InputCostPerToken:  bifrost.Ptr(0.0),
			OutputCostPerToken: bifrost.Ptr(0.0),
			InputCostPerQuery:  bifrost.Ptr(0.002),
		},
	})

	t.Run("single search unit", func(t *testing.T) {
		resp := makeRerankResponse(schemas.Cohere, "rerank-v3.5", &schemas.BifrostLLMUsage{
			SearchUnits: bifrost.Ptr(1),
		})
		assert.InDelta(t, 0.002, s.CalculateCost(resp, nil), 1e-12)
	})

	t.Run("multiple search units bill per unit", func(t *testing.T) {
		// A query covers up to 100 document chunks; 150 documents bills as 2.
		resp := makeRerankResponse(schemas.Cohere, "rerank-v3.5", &schemas.BifrostLLMUsage{
			SearchUnits: bifrost.Ptr(2),
		})
		assert.InDelta(t, 0.004, s.CalculateCost(resp, nil), 1e-12)
	})

	t.Run("usage without search units bills one query", func(t *testing.T) {
		resp := makeRerankResponse(schemas.Cohere, "rerank-v3.5", &schemas.BifrostLLMUsage{
			PromptTokens: 500,
			TotalTokens:  500,
		})
		assert.InDelta(t, 0.002, s.CalculateCost(resp, nil), 1e-12)
	})

	t.Run("nil usage still bills one query", func(t *testing.T) {
		// Vertex reports no usage on rerank; returning zero would under-report every call.
		resp := makeRerankResponse(schemas.Cohere, "rerank-v3.5", nil)
		assert.InDelta(t, 0.002, s.CalculateCost(resp, nil), 1e-12)
	})
}

// =========================================================================
// computeDecisionCost — unit tests
// =========================================================================

func TestComputeDecisionCost_InputOnlyBilling(t *testing.T) {
	// Typesafe's jev pricing shape: input tokens billed, output tokens free.
	p := configstoreTables.TableModelPricing{
		InputCostPerToken:  bifrost.Ptr(0.000000042),
		OutputCostPerToken: bifrost.Ptr(0.0),
	}
	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     50000,
		CompletionTokens: 40,
		TotalTokens:      50040,
	}
	cost := computeDecisionCostTotal(&p, usage, serviceTier{})
	assert.InDelta(t, 50000*0.000000042, cost, 1e-12)
}

func TestComputeDecisionCost_OutputRateHonored(t *testing.T) {
	p := configstoreTables.TableModelPricing{
		InputCostPerToken:  bifrost.Ptr(0.000001),
		OutputCostPerToken: bifrost.Ptr(0.000002),
	}
	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     1000,
		CompletionTokens: 500,
		TotalTokens:      1500,
	}
	cost := computeDecisionCostTotal(&p, usage, serviceTier{})
	assert.InDelta(t, 1000*0.000001+500*0.000002, cost, 1e-12)
}

func TestComputeDecisionCost_NilUsage(t *testing.T) {
	p := configstoreTables.TableModelPricing{InputCostPerToken: new(0.000000042)}
	assert.Equal(t, 0.0, computeDecisionCostTotal(&p, nil, serviceTier{}))
}

func TestCalculateCost_DecisionTokenBilling(t *testing.T) {
	// Pins the dispatch arm: an unhandled request type silently falls into
	// CalculateCost's default nil branch and every decision would cost zero.
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("jev-1.13.0", "typesafe", "decisions"): {
			Model: "jev-1.13.0", Provider: "typesafe", Mode: "decisions",
			InputCostPerToken:  bifrost.Ptr(0.000000042),
			OutputCostPerToken: bifrost.Ptr(0.0),
		},
	})

	resp := makeDecisionResponse(schemas.Typesafe, "jev-1.13.0", &schemas.BifrostLLMUsage{
		PromptTokens: 50000,
		TotalTokens:  50000,
	})

	assert.InDelta(t, 50000*0.000000042, s.CalculateCost(resp, nil), 1e-12)
}

// A model with no decisions row answers a decision by emulation: one Responses call
// to the provider, which must be billed as that text call, cache reads and writes
// included, from its chat row or, failing that, its responses row. Without the
// fallback the decision found no row and cost nothing, so every emulated decision
// was a free paid call outside every budget.
func TestCalculateCost_EmulatedDecisionBilledAsTextCall(t *testing.T) {
	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     1000,
		CompletionTokens: 100,
		TotalTokens:      1100,
		PromptTokensDetails: &schemas.ChatPromptTokensDetails{
			CachedReadTokens:  800,
			CachedWriteTokens: 100,
		},
	}
	row := configstoreTables.TableModelPricing{
		Model: "gpt-5.5", Provider: "openai",
		InputCostPerToken:           bifrost.Ptr(0.000003),
		OutputCostPerToken:          bifrost.Ptr(0.00001),
		CacheReadInputTokenCost:     bifrost.Ptr(0.0000003),
		CacheCreationInputTokenCost: bifrost.Ptr(0.00000375),
	}
	want := computeTextCostTotal(&row, usage, serviceTier{})
	if want <= 0 {
		t.Fatalf("text cost of the emulated call = %v, want > 0", want)
	}

	for _, mode := range []string{"chat", "responses"} {
		t.Run(mode, func(t *testing.T) {
			r := row
			r.Mode = mode
			s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
				makeKey("gpt-5.5", "openai", mode): r,
			})
			got := s.CalculateCost(makeDecisionResponse(schemas.OpenAI, "gpt-5.5", usage), nil)
			assert.InDelta(t, want, got, 1e-12)
		})
	}
}

// An emulated decision served at priority is billed at the priority rate, as its text
// call would be.
func TestCalculateCost_EmulatedDecisionUsesServedTier(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gpt-5.5", "openai", "chat"): {
			Model: "gpt-5.5", Provider: "openai", Mode: "chat",
			InputCostPerToken: bifrost.Ptr(0.000001), OutputCostPerToken: bifrost.Ptr(0.000002),
			InputCostPerTokenPriority: bifrost.Ptr(0.000005),
		},
	})
	resp := makeDecisionResponse(schemas.OpenAI, "gpt-5.5", &schemas.BifrostLLMUsage{PromptTokens: 1000, TotalTokens: 1000})
	priority := schemas.BifrostServiceTierPriority
	resp.DecisionResponse.ServiceTier = &priority
	assert.InDelta(t, 1000*0.000005, s.CalculateCost(resp, nil), 1e-12)
}

// A failed request billed from its bare usage keeps the served processing tier.
func TestCalculateCostForUsage_UsesServedServiceTier(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gpt-5.5", "openai", "responses"): {
			Model: "gpt-5.5", Provider: "openai", Mode: "responses",
			InputCostPerToken: bifrost.Ptr(0.000001), OutputCostPerToken: bifrost.Ptr(0.000002),
			InputCostPerTokenPriority: bifrost.Ptr(0.000005),
		},
	})
	priority := schemas.BifrostServiceTierPriority
	usage := &schemas.BifrostLLMUsage{PromptTokens: 1000, TotalTokens: 1000, ServiceTier: &priority}
	assert.InDelta(t, 1000*0.000005, s.CalculateCostForUsage(usage, schemas.OpenAI, "gpt-5.5", schemas.ResponsesRequest, nil), 1e-12)
}

// A native decisions row keeps the token-only decision pricing even when the model
// also has a chat row.
func TestCalculateCost_NativeDecisionRowWinsOverChatRow(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("jev-1.13.0", "typesafe", "decisions"): {
			Model: "jev-1.13.0", Provider: "typesafe", Mode: "decisions",
			InputCostPerToken:  bifrost.Ptr(0.000000042),
			OutputCostPerToken: bifrost.Ptr(0.0),
		},
		makeKey("jev-1.13.0", "typesafe", "chat"): {
			Model: "jev-1.13.0", Provider: "typesafe", Mode: "chat",
			InputCostPerToken:  bifrost.Ptr(0.001),
			OutputCostPerToken: bifrost.Ptr(0.001),
		},
	})
	resp := makeDecisionResponse(schemas.Typesafe, "jev-1.13.0", &schemas.BifrostLLMUsage{
		PromptTokens: 50000, CompletionTokens: 40, TotalTokens: 50040,
	})
	assert.InDelta(t, 50000*0.000000042, s.CalculateCost(resp, nil), 1e-12)
}

// =========================================================================
// Decision rates on a chat row, and their overrides
// =========================================================================

// Rates used across the decision pricing tests. The chat and decision rates
// differ on purpose so a test fails if the wrong set is applied.
const (
	lunaChatInput       = 2e-6
	lunaChatOutput      = 8e-6
	lunaChatAbove272k   = 4e-6
	lunaDecisionInput   = 1e-7
	lunaDecisionOutput  = 3e-7
	decisionTestEpsilon = 1e-12
)

// lunaChatRow builds the gpt-6-luna chat row: chat rates plus a 272k tier, so a
// tier leaking into decision pricing is caught. withDecisionRates adds the
// decision rates the datasheet carries on the same row.
func lunaChatRow(withDecisionRates bool) configstoreTables.TableModelPricing {
	row := configstoreTables.TableModelPricing{
		Model: "gpt-6-luna", Provider: "openai", Mode: "chat",
		InputCostPerToken:                bifrost.Ptr(lunaChatInput),
		OutputCostPerToken:               bifrost.Ptr(lunaChatOutput),
		InputCostPerTokenAbove272kTokens: bifrost.Ptr(lunaChatAbove272k),
	}
	if withDecisionRates {
		row.InputCostPerTokenDecisions = bifrost.Ptr(lunaDecisionInput)
		row.OutputCostPerTokenDecisions = bifrost.Ptr(lunaDecisionOutput)
	}
	return row
}

// lunaStore builds a store holding the given gpt-6-luna chat row.
func lunaStore(row configstoreTables.TableModelPricing) *Store {
	return testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey(row.Model, row.Provider, row.Mode): row,
	})
}

// lunaDecisionCost prices a decision on openai/gpt-6-luna with the given usage.
func lunaDecisionCost(s *Store, prompt, completion int) float64 {
	return s.CalculateCost(makeDecisionResponse(schemas.OpenAI, "gpt-6-luna", &schemas.BifrostLLMUsage{
		PromptTokens: prompt, CompletionTokens: completion, TotalTokens: prompt + completion,
	}), nil)
}

// TestDecisionPricing_NativeUsesDecisionRates pins that a chat row carrying
// decision rates prices a decision from them, not from its chat rates.
func TestDecisionPricing_NativeUsesDecisionRates(t *testing.T) {
	s := lunaStore(lunaChatRow(true))

	assert.InDelta(t, 1000*lunaDecisionInput+50*lunaDecisionOutput, lunaDecisionCost(s, 1000, 50), decisionTestEpsilon)
}

// TestDecisionPricing_WithoutDecisionRatesUsesChatRates pins the rule for a
// model with no decision rates, whose decision is a chat call by emulation.
func TestDecisionPricing_WithoutDecisionRatesUsesChatRates(t *testing.T) {
	s := lunaStore(lunaChatRow(false))

	assert.InDelta(t, 1000*lunaChatInput+50*lunaChatOutput, lunaDecisionCost(s, 1000, 50), decisionTestEpsilon)
}

// TestDecisionPricing_DecisionRatesAreFlat pins that a chat tier cannot override
// the flat decision rate on a prompt past the tier's threshold.
func TestDecisionPricing_DecisionRatesAreFlat(t *testing.T) {
	s := lunaStore(lunaChatRow(true))

	assert.InDelta(t, 300000*lunaDecisionInput, lunaDecisionCost(s, 300000, 0), decisionTestEpsilon)
}

// TestDecisionPricing_PartialDecisionRates pins that each side uses its decision
// rate when present and the row's own rate when not.
func TestDecisionPricing_PartialDecisionRates(t *testing.T) {
	inputOnly := lunaChatRow(false)
	inputOnly.InputCostPerTokenDecisions = bifrost.Ptr(lunaDecisionInput)
	outputOnly := lunaChatRow(false)
	outputOnly.OutputCostPerTokenDecisions = bifrost.Ptr(lunaDecisionOutput)

	assert.InDelta(t, 1000*lunaDecisionInput+50*lunaChatOutput, lunaDecisionCost(lunaStore(inputOnly), 1000, 50), decisionTestEpsilon)
	assert.InDelta(t, 1000*lunaChatInput+50*lunaDecisionOutput, lunaDecisionCost(lunaStore(outputOnly), 1000, 50), decisionTestEpsilon)
}

// TestDecisionPricing_ChatRequestIgnoresDecisionRates pins that decision rates
// never reach a chat request on the same row.
func TestDecisionPricing_ChatRequestIgnoresDecisionRates(t *testing.T) {
	s := lunaStore(lunaChatRow(true))
	resp := makeChatResponse(schemas.OpenAI, "gpt-6-luna", &schemas.BifrostLLMUsage{
		PromptTokens: 1000, CompletionTokens: 50, TotalTokens: 1050,
	})

	assert.InDelta(t, 1000*lunaChatInput+50*lunaChatOutput, s.CalculateCost(resp, nil), decisionTestEpsilon)
}

// setLunaOverride installs a provider-scoped override on openai/gpt-6-luna for
// the given request types with the given pricing patch JSON.
func setLunaOverride(t *testing.T, s *Store, patchJSON string, requestTypes ...schemas.RequestType) {
	t.Helper()
	providerID := "openai"
	require.NoError(t, s.SetOverrides([]configstoreTables.TablePricingOverride{{
		ID:               "luna-override",
		ScopeKind:        string(ScopeKindProvider),
		ProviderID:       &providerID,
		MatchType:        string(MatchTypeExact),
		Pattern:          "gpt-6-luna",
		RequestTypes:     requestTypes,
		PricingPatchJSON: patchJSON,
	}}))
}

// TestDecisionPricing_PlainOverrideIsOnlyAFallback pins that the normal input
// rate is the fallback for a decision: an override of it applies when the row
// has no decision rate, and is ignored when the row has one.
func TestDecisionPricing_PlainOverrideIsOnlyAFallback(t *testing.T) {
	withoutRates := lunaStore(lunaChatRow(false))
	setLunaOverride(t, withoutRates, `{"input_cost_per_token":0.000005}`, schemas.DecisionRequest)
	assert.InDelta(t, 1000*0.000005+50*lunaChatOutput, lunaDecisionCost(withoutRates, 1000, 50), decisionTestEpsilon)

	withRates := lunaStore(lunaChatRow(true))
	setLunaOverride(t, withRates, `{"input_cost_per_token":0.000005}`, schemas.DecisionRequest)
	assert.InDelta(t, 1000*lunaDecisionInput+50*lunaDecisionOutput, lunaDecisionCost(withRates, 1000, 50), decisionTestEpsilon)
}

// TestDecisionPricing_ChatOverrideDoesNotApply pins that an override scoped to
// chat leaves decision pricing alone.
func TestDecisionPricing_ChatOverrideDoesNotApply(t *testing.T) {
	s := lunaStore(lunaChatRow(true))
	setLunaOverride(t, s, `{"input_cost_per_token":0.000005}`, schemas.ChatCompletionRequest)

	assert.InDelta(t, 1000*lunaDecisionInput+50*lunaDecisionOutput, lunaDecisionCost(s, 1000, 50), decisionTestEpsilon)
}

// TestDecisionPricing_DecisionRateOverrideWins pins that an override setting a
// decision rate, the field mirrored from the datasheet, wins over the datasheet's
// decision rates and over a plain override in the same patch.
func TestDecisionPricing_DecisionRateOverrideWins(t *testing.T) {
	s := lunaStore(lunaChatRow(true))
	setLunaOverride(t, s, `{"input_cost_per_token":0.000009,"input_cost_per_token_decisions":0.000005,"output_cost_per_token_decisions":0.000006}`, schemas.DecisionRequest)

	assert.InDelta(t, 1000*0.000005+50*0.000006, lunaDecisionCost(s, 1000, 50), decisionTestEpsilon)
}

// TestDecisionPricing_PartialDecisionRateOverrideKeepsOtherSide pins that an
// override of one decision rate leaves the other side at what the row priced it.
func TestDecisionPricing_PartialDecisionRateOverrideKeepsOtherSide(t *testing.T) {
	withRates := lunaStore(lunaChatRow(true))
	setLunaOverride(t, withRates, `{"input_cost_per_token_decisions":0.000005}`, schemas.DecisionRequest)
	assert.InDelta(t, 1000*0.000005+50*lunaDecisionOutput, lunaDecisionCost(withRates, 1000, 50), decisionTestEpsilon)

	withoutRates := lunaStore(lunaChatRow(false))
	setLunaOverride(t, withoutRates, `{"input_cost_per_token_decisions":0.000005}`, schemas.DecisionRequest)
	assert.InDelta(t, 1000*0.000005+50*lunaChatOutput, lunaDecisionCost(withoutRates, 1000, 50), decisionTestEpsilon)
}

// TestDecisionPricing_DecisionRateOverrideIsFlat pins that decision rates an
// override sets are flat too, past a chat tier's threshold.
func TestDecisionPricing_DecisionRateOverrideIsFlat(t *testing.T) {
	s := lunaStore(lunaChatRow(false))
	setLunaOverride(t, s, `{"input_cost_per_token_decisions":0.000005}`, schemas.DecisionRequest)

	assert.InDelta(t, 300000*0.000005, lunaDecisionCost(s, 300000, 0), decisionTestEpsilon)
}

// TestDecisionPricing_DecisionRateOverrideOnChatScopeIsIgnored pins that decision
// rates in an override scoped to chat change neither chat nor decision pricing.
func TestDecisionPricing_DecisionRateOverrideOnChatScopeIsIgnored(t *testing.T) {
	s := lunaStore(lunaChatRow(true))
	setLunaOverride(t, s, `{"input_cost_per_token_decisions":0.000005}`, schemas.ChatCompletionRequest)
	chat := makeChatResponse(schemas.OpenAI, "gpt-6-luna", &schemas.BifrostLLMUsage{PromptTokens: 1000, CompletionTokens: 50, TotalTokens: 1050})

	assert.InDelta(t, 1000*lunaDecisionInput+50*lunaDecisionOutput, lunaDecisionCost(s, 1000, 50), decisionTestEpsilon)
	assert.InDelta(t, 1000*lunaChatInput+50*lunaChatOutput, s.CalculateCost(chat, nil), decisionTestEpsilon)
}

// TestDecisionPricing_OverrideOnlyProviderUsesDecisionRates pins the custom
// provider case: with no catalog row at all, an override on either the decision
// fields or the plain fields prices the decision.
func TestDecisionPricing_OverrideOnlyProviderUsesDecisionRates(t *testing.T) {
	for name, patch := range map[string]string{
		"decision fields": `{"input_cost_per_token_decisions":0.000005,"output_cost_per_token_decisions":0.000006}`,
		"plain fields":    `{"input_cost_per_token":0.000005,"output_cost_per_token":0.000006}`,
	} {
		t.Run(name, func(t *testing.T) {
			s := newTestStore()
			providerID := "my-laya"
			require.NoError(t, s.SetOverrides([]configstoreTables.TablePricingOverride{{
				ID:               "custom-provider-override",
				ScopeKind:        string(ScopeKindProvider),
				ProviderID:       &providerID,
				MatchType:        string(MatchTypeExact),
				Pattern:          "english",
				RequestTypes:     []schemas.RequestType{schemas.DecisionRequest},
				PricingPatchJSON: patch,
			}}))
			resp := makeDecisionResponse("my-laya", "english", &schemas.BifrostLLMUsage{PromptTokens: 1000, CompletionTokens: 50, TotalTokens: 1050})

			assert.InDelta(t, 1000*0.000005+50*0.000006, s.CalculateCost(resp, nil), decisionTestEpsilon)
		})
	}
}

// TestDecisionPricing_DecisionsModeRowWins pins that a decisions-mode row, as
// Typesafe's Jev models have, is priced from itself and never falls to a chat
// row of the same model.
func TestDecisionPricing_DecisionsModeRowWins(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("jev-1.13.0", "typesafe", "decisions"): {
			Model: "jev-1.13.0", Provider: "typesafe", Mode: "decisions",
			InputCostPerToken: bifrost.Ptr(0.000000042), OutputCostPerToken: bifrost.Ptr(0.0),
		},
		makeKey("jev-1.13.0", "typesafe", "chat"): {
			Model: "jev-1.13.0", Provider: "typesafe", Mode: "chat",
			InputCostPerToken: bifrost.Ptr(1.0), OutputCostPerToken: bifrost.Ptr(1.0),
		},
	})
	resp := makeDecisionResponse(schemas.Typesafe, "jev-1.13.0", &schemas.BifrostLLMUsage{PromptTokens: 50000, TotalTokens: 50000})

	assert.InDelta(t, 50000*0.000000042, s.CalculateCost(resp, nil), decisionTestEpsilon)
}

// TestDecisionPricing_NegativeRateIsUnpriced pins that a variable-price row
// (rates of -1) falling back from a decision is left unpriced, not billed as a
// negative cost.
func TestDecisionPricing_NegativeRateIsUnpriced(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("typesafe/jev-router", "openrouter", "chat"): {
			Model: "typesafe/jev-router", Provider: "openrouter", Mode: "chat",
			InputCostPerToken: bifrost.Ptr(-1.0), OutputCostPerToken: bifrost.Ptr(-1.0),
		},
	})
	resp := makeDecisionResponse(schemas.OpenRouter, "typesafe/jev-router", &schemas.BifrostLLMUsage{PromptTokens: 1000, CompletionTokens: 10, TotalTokens: 1010})

	assert.Equal(t, 0.0, s.CalculateCost(resp, nil))
}

// TestDecisionPricing_ResponsesOnlyRowIsPriced: upstream leaves a model with only a
// responses row unpriced for decisions. The txehq fork prices an emulated decision from
// the model's responses row too, since the emulation is one Responses call, and a row
// that carries decision rates uses them (TestCalculateCost_EmulatedDecisionBilledAsTextCall).
func TestDecisionPricing_ResponsesOnlyRowIsPriced(t *testing.T) {
	row := lunaChatRow(true)
	row.Mode = "responses"
	s := lunaStore(row)

	assert.InDelta(t, 1000*lunaDecisionInput+50*lunaDecisionOutput, lunaDecisionCost(s, 1000, 50), decisionTestEpsilon)
}

// TestDecisionPricing_ProviderFallbacksReachChatRow pins that the Bedrock vendor
// prefix and Vertex prefix-strip lookups retry a decision in chat mode.
func TestDecisionPricing_ProviderFallbacksReachChatRow(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("anthropic.claude-3-5-sonnet-20241022-v2:0", "bedrock", "chat"): {
			Model: "anthropic.claude-3-5-sonnet-20241022-v2:0", Provider: "bedrock", Mode: "chat",
			InputCostPerToken: bifrost.Ptr(3e-6), OutputCostPerToken: bifrost.Ptr(15e-6),
		},
		makeKey("claude-3-5-sonnet", "vertex", "chat"): {
			Model: "claude-3-5-sonnet", Provider: "vertex", Mode: "chat",
			InputCostPerToken: bifrost.Ptr(4e-6), OutputCostPerToken: bifrost.Ptr(20e-6),
		},
	})

	bedrock := s.resolvePricing(schemas.RoutingInfo{Provider: "bedrock", Model: "claude-3-5-sonnet-20241022-v2:0"}, schemas.DecisionRequest, LookupScopes{})
	require.NotNil(t, bedrock)
	assert.Equal(t, 3e-6, *bedrock.InputCostPerToken)

	vertex := s.resolvePricing(schemas.RoutingInfo{Provider: "vertex", Model: "anthropic/claude-3-5-sonnet"}, schemas.DecisionRequest, LookupScopes{})
	require.NotNil(t, vertex)
	assert.Equal(t, 4e-6, *vertex.InputCostPerToken)
}

// TestDecisionPricing_KeepsRowLevelFee pins that a flat per-request fee on the
// row is still billed on top of the decision rates.
func TestDecisionPricing_KeepsRowLevelFee(t *testing.T) {
	row := lunaChatRow(true)
	row.CostPerRequest = bifrost.Ptr(0.01)
	s := lunaStore(row)

	assert.InDelta(t, 1000*lunaDecisionInput+50*lunaDecisionOutput+0.01, lunaDecisionCost(s, 1000, 50), decisionTestEpsilon)
}

// TestComputeDecisionCost_NegativeRateIsUnpriced pins the guard directly.
func TestComputeDecisionCost_NegativeRateIsUnpriced(t *testing.T) {
	usage := &schemas.BifrostLLMUsage{PromptTokens: 1000, CompletionTokens: 10, TotalTokens: 1010}

	negativeInput := configstoreTables.TableModelPricing{InputCostPerToken: bifrost.Ptr(-1.0), OutputCostPerToken: bifrost.Ptr(0.0)}
	negativeOutput := configstoreTables.TableModelPricing{InputCostPerToken: bifrost.Ptr(1e-6), OutputCostPerToken: bifrost.Ptr(-1.0)}

	assert.Nil(t, computeDecisionCost(&negativeInput, usage, serviceTier{}))
	assert.Nil(t, computeDecisionCost(&negativeOutput, usage, serviceTier{}))
}

// TestDecisionRates_SurviveEntryAndTableConversion pins that the decision rates
// parse from the datasheet JSON and survive both conversions.
func TestDecisionRates_SurviveEntryAndTableConversion(t *testing.T) {
	var entry Entry
	require.NoError(t, json.Unmarshal([]byte(`{
		"provider": "openai", "mode": "chat", "base_model": "gpt-6-luna",
		"input_cost_per_token": 2e-6, "output_cost_per_token": 8e-6,
		"input_cost_per_token_decisions": 1e-7, "output_cost_per_token_decisions": 3e-7
	}`), &entry))
	require.NotNil(t, entry.InputCostPerTokenDecisions)
	require.NotNil(t, entry.OutputCostPerTokenDecisions)

	row := convertEntryToTablePricing("gpt-6-luna", entry)
	assert.Equal(t, 1e-7, *row.InputCostPerTokenDecisions)
	assert.Equal(t, 3e-7, *row.OutputCostPerTokenDecisions)

	back := convertTablePricingToEntry(&row)
	assert.Equal(t, 1e-7, *back.InputCostPerTokenDecisions)
	assert.Equal(t, 3e-7, *back.OutputCostPerTokenDecisions)
}

func TestCalculateCost_RerankPerTokenStillWorks(t *testing.T) {
	// Voyage and Jina style rerankers bill per token and carry no per-query rate; the two
	// pricing shapes must not interfere.
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("rerank-2", "voyage", "rerank"): {
			Model: "rerank-2", Provider: "voyage", Mode: "rerank",
			InputCostPerToken: bifrost.Ptr(0.00000005),
		},
	})

	resp := makeRerankResponse("voyage", "rerank-2", &schemas.BifrostLLMUsage{
		PromptTokens: 1000,
		TotalTokens:  1000,
	})

	assert.InDelta(t, 0.00005, s.CalculateCost(resp, nil), 1e-12)
}

// =========================================================================
// Per-image rates keyed jointly by size and quality
// =========================================================================

// makeSizedImageResponse builds an image-generation response carrying the size
// and quality parameters that drive per-image rate selection.
func makeSizedImageResponse(provider schemas.ModelProvider, model, size, quality string, nImages int) *schemas.BifrostResponse {
	return &schemas.BifrostResponse{
		ImageGenerationResponse: &schemas.BifrostImageGenerationResponse{
			Usage: &schemas.ImageUsage{
				OutputTokensDetails: &schemas.ImageTokenDetails{NImages: nImages},
			},
			ImageGenerationResponseParameters: &schemas.ImageGenerationResponseParameters{
				Size:    size,
				Quality: quality,
			},
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType: schemas.ImageGenerationRequest,
				RoutingInfo: routingInfoFor(provider, model),
			},
		},
	}
}

// sizeQualityImagePricing mirrors the gpt-image style datasheet rows: a full
// size x quality matrix plus the quality-agnostic size rates.
func sizeQualityImagePricing() configstoreTables.TableModelPricing {
	return configstoreTables.TableModelPricing{
		Model: "gpt-image-1", Provider: "openai", Mode: "image_generation",

		OutputCostPerImageAbove1024x1024PixelsLowQuality: bifrost.Ptr(0.009),
		OutputCostPerImageAbove1024x1536PixelsLowQuality: bifrost.Ptr(0.013),
		OutputCostPerImageAbove1536x1024PixelsLowQuality: bifrost.Ptr(0.013),

		OutputCostPerImageAbove1024x1024PixelsMediumQuality: bifrost.Ptr(0.034),
		OutputCostPerImageAbove1024x1536PixelsMediumQuality: bifrost.Ptr(0.05),
		OutputCostPerImageAbove1536x1024PixelsMediumQuality: bifrost.Ptr(0.05),

		OutputCostPerImageAbove1024x1024PixelsHighQuality: bifrost.Ptr(0.133),
		OutputCostPerImageAbove1024x1536PixelsHighQuality: bifrost.Ptr(0.2),
		OutputCostPerImageAbove1536x1024PixelsHighQuality: bifrost.Ptr(0.2),

		OutputCostPerImageAbove1024x1024PixelsStandardQuality: bifrost.Ptr(0.009),
		OutputCostPerImageAbove1024x1536PixelsStandardQuality: bifrost.Ptr(0.013),
		OutputCostPerImageAbove1536x1024PixelsStandardQuality: bifrost.Ptr(0.013),

		OutputCostPerImageAbove1024x1024Pixels: bifrost.Ptr(0.009),
		OutputCostPerImageAbove1024x1536Pixels: bifrost.Ptr(0.013),
		OutputCostPerImageAbove1536x1024Pixels: bifrost.Ptr(0.013),
	}
}

func TestCalculateCost_ImageGeneration_SizeAndQualityRates(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gpt-image-1", "openai", "image_generation"): sizeQualityImagePricing(),
	})

	tests := []struct {
		name     string
		size     string
		quality  string
		nImages  int
		expected float64
	}{
		{"low 1024x1024", "1024x1024", "low", 1, 0.009},
		{"low 1024x1536", "1024x1536", "low", 1, 0.013},
		{"low 1536x1024", "1536x1024", "low", 1, 0.013},
		{"medium 1024x1024", "1024x1024", "medium", 1, 0.034},
		{"medium 1024x1536", "1024x1536", "medium", 1, 0.05},
		{"medium 1536x1024", "1536x1024", "medium", 1, 0.05},
		{"high 1024x1024", "1024x1024", "high", 1, 0.133},
		{"high 1024x1536", "1024x1536", "high", 1, 0.2},
		{"high 1536x1024", "1536x1024", "high", 1, 0.2},
		{"standard 1024x1024", "1024x1024", "standard", 1, 0.009},
		{"standard 1024x1536", "1024x1536", "standard", 1, 0.013},
		{"standard 1536x1024", "1536x1024", "standard", 1, 0.013},
		// No quality given falls through to the quality-agnostic size rates.
		{"unspecified quality 1024x1536", "1024x1536", "", 1, 0.013},
		{"unspecified quality 1536x1024", "1536x1024", "", 1, 0.013},
		{"unspecified quality 1024x1024", "1024x1024", "", 1, 0.009},
		// Rates are per generated image.
		{"high 1024x1536 x3", "1024x1536", "high", 3, 0.6},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cost := s.CalculateCost(makeSizedImageResponse("openai", "gpt-image-1", tc.size, tc.quality, tc.nImages), nil)
			assert.InDelta(t, tc.expected, cost, 1e-12)
		})
	}
}

// The 1024x1536 and 1536x1024 thresholds have identical pixel counts, so
// selection must key off width and height, not the total.
func TestCalculateCost_ImageGeneration_OrientationDistinguishesEqualPixelCounts(t *testing.T) {
	p := sizeQualityImagePricing()
	p.OutputCostPerImageAbove1024x1536PixelsHighQuality = bifrost.Ptr(0.21)
	p.OutputCostPerImageAbove1536x1024PixelsHighQuality = bifrost.Ptr(0.19)
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gpt-image-1", "openai", "image_generation"): p,
	})

	portrait := s.CalculateCost(makeSizedImageResponse("openai", "gpt-image-1", "1024x1536", "high", 1), nil)
	landscape := s.CalculateCost(makeSizedImageResponse("openai", "gpt-image-1", "1536x1024", "high", 1), nil)

	assert.InDelta(t, 0.21, portrait, 1e-12)
	assert.InDelta(t, 0.19, landscape, 1e-12)
}

// An image that clears both orientation thresholds is billed at the 1536x1024
// rate, which rateForSize checks ahead of 1024x1536.
func TestCalculateCost_ImageGeneration_BothOrientationsMatchPrefersLandscape(t *testing.T) {
	p := sizeQualityImagePricing()
	p.OutputCostPerImageAbove1024x1536PixelsHighQuality = bifrost.Ptr(0.21)
	p.OutputCostPerImageAbove1536x1024PixelsHighQuality = bifrost.Ptr(0.19)
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gpt-image-1", "openai", "image_generation"): p,
	})

	cost := s.CalculateCost(makeSizedImageResponse("openai", "gpt-image-1", "1536x1536", "high", 1), nil)

	assert.InDelta(t, 0.19, cost, 1e-12)
}

// Joint size+quality rates are more specific than quality-only rates and win
// over them; quality-only still applies when no size+quality rate matches.
func TestCalculateCost_ImageGeneration_SizeQualityBeatsQualityOnly(t *testing.T) {
	p := sizeQualityImagePricing()
	p.OutputCostPerImageHighQuality = bifrost.Ptr(0.5)
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gpt-image-1", "openai", "image_generation"): p,
	})

	// 1024x1536 has a high-quality size rate, which wins over the quality-only 0.5.
	assert.InDelta(t, 0.2, s.CalculateCost(makeSizedImageResponse("openai", "gpt-image-1", "1024x1536", "high", 1), nil), 1e-12)
	// 512x512 has no high-quality size rate, so it falls back to the quality-only rate.
	assert.InDelta(t, 0.5, s.CalculateCost(makeSizedImageResponse("openai", "gpt-image-1", "512x512", "high", 1), nil), 1e-12)
}

// A quality with no configured size rates falls through quality-only, then
// size-only, then the flat per-image rate.
func TestCalculateCost_ImageGeneration_FallsBackThroughRateChain(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gpt-image-1", "openai", "image_generation"): {
			Model: "gpt-image-1", Provider: "openai", Mode: "image_generation",
			OutputCostPerImageAbove1536x1024Pixels: bifrost.Ptr(0.013),
			OutputCostPerImage:                     bifrost.Ptr(0.004),
		},
	})

	// No standard-quality rates configured, so the size-only rate applies.
	assert.InDelta(t, 0.013, s.CalculateCost(makeSizedImageResponse("openai", "gpt-image-1", "1536x1024", "standard", 1), nil), 1e-12)
	// Below every configured threshold, so the flat per-image rate applies.
	assert.InDelta(t, 0.004, s.CalculateCost(makeSizedImageResponse("openai", "gpt-image-1", "512x512", "standard", 1), nil), 1e-12)
}

// TXE-7356: image rates that the fork dropped at sync, so each of these images billed $0. The
// rows are the live upstream datasheet's (getbifrost.ai/datasheet, fetched 2026-11-14), run
// through the production JSON -> row conversion, so a missing json tag fails here too.
const (
	azureDallE3Row = `{"provider":"azure","mode":"image_generation",
		"output_cost_per_image_above_1024_and_1024_pixels_hd_quality":0.088,
		"output_cost_per_image_above_1024_and_1792_pixels_hd_quality":0.132,
		"output_cost_per_image_above_1792_and_1024_pixels_hd_quality":0.132,
		"output_cost_per_image_above_1024_and_1024_pixels_standard_quality":0.044,
		"output_cost_per_image_above_1024_and_1792_pixels_standard_quality":0.088,
		"output_cost_per_image_above_1792_and_1024_pixels_standard_quality":0.088}`
	xaiGrokImagineQualityRow = `{"provider":"xai","mode":"image_generation","input_cost_per_image":0.04,
		"output_cost_per_image_1K":0.04,"output_cost_per_image_2K":0.06}`
	bedrockSDXL1Row = `{"provider":"bedrock","mode":"image_generation",
		"output_cost_per_image_above_1024_and_1024_pixels_50_steps":0.04,
		"output_cost_per_image_above_1024_and_1024_pixels_max_steps":0.08}`
	bedrockSDXL09Row = `{"provider":"bedrock","mode":"image_generation",
		"output_cost_per_image_above_512_and_512_pixels_50_steps":0.018,
		"output_cost_per_image_above_512_and_512_pixels_max_steps":0.036,
		"output_cost_per_image_above_max_and_max_pixels_50_steps":0.036,
		"output_cost_per_image_above_max_and_max_pixels_max_steps":0.072}`
	falGPTImage25FlareRow = `{"provider":"fal_ai","mode":"image_generation",
		"output_cost_per_image_above_1024_and_768_pixels_high_quality":0.03612,
		"output_cost_per_image_above_1920_and_1080_pixels_high_quality":0.0396,
		"output_cost_per_image_above_2560_and_1440_pixels_high_quality":0.05529,
		"output_cost_per_image_above_3840_and_2160_pixels_high_quality":0.10008,
		"output_cost_per_image_above_1024_and_1024_pixels_xhigh_quality":0.09366,
		"output_cost_per_image_above_1024_and_1536_pixels_xhigh_quality":0.07377,
		"output_cost_per_image_above_1024_and_1024_pixels_max_quality":0.21072,
		"output_cost_per_image_above_3840_and_2160_pixels_max_quality":0.40026}`
	replicateNanoBanana2Row = `{"provider":"replicate","mode":"image_generation","output_cost_per_image":0.067,
		"output_cost_per_image_1K":0.067,"output_cost_per_image_2K":0.101,"output_cost_per_image_4K":0.151}`
	falTrellis2Row = `{"provider":"fal_ai","mode":"image_generation","output_cost_per_image_512":0.25,
		"output_cost_per_image_1024":0.3,"output_cost_per_image_1536":0.35,"output_cost_per_image_0.5K":0.2}`
)

// variantImageResponse is makeSizedImageResponse plus the resolution tier and steps the request named.
func variantImageResponse(provider schemas.ModelProvider, model, size, quality, resolution string, steps *int) *schemas.BifrostResponse {
	resp := makeSizedImageResponse(provider, model, size, quality, 1)
	resp.ImageGenerationResponse.ImageGenerationResponseParameters.Resolution = resolution
	resp.ImageGenerationResponse.ImageGenerationResponseParameters.NumInferenceSteps = steps
	return resp
}

func TestCalculateCost_ImageGeneration_TXE7356Variants(t *testing.T) {
	rows := map[string]string{
		"azure/dall-e-3":                                  azureDallE3Row,
		"xai/grok-imagine-image-quality":                  xaiGrokImagineQualityRow,
		"stability.stable-diffusion-xl-v1":                bedrockSDXL1Row,
		"stability.stable-diffusion-xl-v0":                bedrockSDXL09Row,
		"fal_ai/openai/gpt-image-2.5/flare/text-to-image": falGPTImage25FlareRow,
		"replicate/google/nano-banana-2":                  replicateNanoBanana2Row,
		"fal_ai/fal-ai/trellis-2":                         falTrellis2Row,
	}
	pricing := map[string]configstoreTables.TableModelPricing{}
	for key, blob := range rows {
		row := pricingRowFromDatasheetJSON(t, key, blob)
		pricing[makeKey(row.Model, row.Provider, row.Mode)] = row
	}
	s := testStoreWithPricing(pricing)

	tests := []struct {
		name                      string
		provider                  schemas.ModelProvider
		model                     string
		size, quality, resolution string
		steps                     *int
		expected                  float64
	}{
		// The ticket's example: $0.088 upstream, $0 on the fork before TXE-7356.
		{"dall-e-3 hd 1024x1024", "azure", "dall-e-3", "1024x1024", "hd", "", nil, 0.088},
		{"dall-e-3 hd 1792x1024", "azure", "dall-e-3", "1792x1024", "hd", "", nil, 0.132},
		{"dall-e-3 hd 1024x1792", "azure", "dall-e-3", "1024x1792", "hd", "", nil, 0.132},
		{"dall-e-3 standard 1024x1024", "azure", "dall-e-3", "1024x1024", "standard", "", nil, 0.044},
		{"dall-e-3 standard 1792x1024", "azure", "dall-e-3", "1792x1024", "standard", "", nil, 0.088},
		// xAI publishes only tier rates; xAI's own spelling is lower case.
		{"grok-imagine-image-quality 1k", "xai", "grok-imagine-image-quality", "", "", "1k", nil, 0.04},
		{"grok-imagine-image-quality 2k", "xai", "grok-imagine-image-quality", "", "", "2k", nil, 0.06},
		{"grok-imagine-image-quality default tier", "xai", "grok-imagine-image-quality", "", "", "", nil, 0.04},
		// A tier the row does not price rounds up to the next priced tier, never down.
		{"grok-imagine-image-quality 1.5k rounds up to 2K", "xai", "grok-imagine-image-quality", "", "", "1.5k", nil, 0.06},
		// Above every priced tier: the largest priced tier, never unpriced.
		{"grok-imagine-image-quality 4k above every tier takes 2K", "xai", "grok-imagine-image-quality", "", "", "4k", nil, 0.06},
		// A pixel tier on a row priced only by K tiers: no rate of its family, so the 1K default, never $0.
		{"grok-imagine-image-quality numeric tier is another family", "xai", "grok-imagine-image-quality", "", "", "1024", nil, 0.04},
		// Not a tier at all ("hd", "1e9K"): treated as naming none, so the default 1K rate.
		{"grok-imagine-image-quality not a tier", "xai", "grok-imagine-image-quality", "", "", "hd", nil, 0.04},
		{"grok-imagine-image-quality non-finite tier", "xai", "grok-imagine-image-quality", "", "", "1e9K", nil, 0.04},
		// Bedrock SDXL 1.0: 50 or fewer steps (the default 30 when unset), or more.
		{"sdxl 1.0 default steps", "bedrock", "stability.stable-diffusion-xl-v1", "1024x1024", "", "", nil, 0.04},
		{"sdxl 1.0 50 steps", "bedrock", "stability.stable-diffusion-xl-v1", "1024x1024", "", "", bifrost.Ptr(50), 0.04},
		{"sdxl 1.0 51 steps", "bedrock", "stability.stable-diffusion-xl-v1", "1024x1024", "", "", bifrost.Ptr(51), 0.08},
		{"sdxl 1.0 size unknown", "bedrock", "stability.stable-diffusion-xl-v1", "", "", "", bifrost.Ptr(70), 0.08},
		// Bedrock SDXL 0.9: 512x512 or smaller apart from larger.
		{"sdxl 0.9 512x512 30 steps", "bedrock", "stability.stable-diffusion-xl-v0", "512x512", "", "", bifrost.Ptr(30), 0.018},
		{"sdxl 0.9 512x512 60 steps", "bedrock", "stability.stable-diffusion-xl-v0", "512x512", "", "", bifrost.Ptr(60), 0.036},
		{"sdxl 0.9 1024x1024 30 steps", "bedrock", "stability.stable-diffusion-xl-v0", "1024x1024", "", "", bifrost.Ptr(30), 0.036},
		{"sdxl 0.9 1024x1024 60 steps", "bedrock", "stability.stable-diffusion-xl-v0", "1024x1024", "", "", bifrost.Ptr(60), 0.072},
		// New thresholds and quality grids.
		{"flare high 1024x768", "fal_ai", "openai/gpt-image-2.5/flare/text-to-image", "1024x768", "high", "", nil, 0.03612},
		{"flare high 1920x1080", "fal_ai", "openai/gpt-image-2.5/flare/text-to-image", "1920x1080", "high", "", nil, 0.0396},
		{"flare high 1080x1920 portrait", "fal_ai", "openai/gpt-image-2.5/flare/text-to-image", "1080x1920", "high", "", nil, 0.0396},
		{"flare high 2560x1440", "fal_ai", "openai/gpt-image-2.5/flare/text-to-image", "2560x1440", "high", "", nil, 0.05529},
		{"flare high 3840x2160", "fal_ai", "openai/gpt-image-2.5/flare/text-to-image", "3840x2160", "high", "", nil, 0.10008},
		{"flare xhigh 1024x1024", "fal_ai", "openai/gpt-image-2.5/flare/text-to-image", "1024x1024", "xhigh", "", nil, 0.09366},
		{"flare xhigh 1024x1536", "fal_ai", "openai/gpt-image-2.5/flare/text-to-image", "1024x1536", "xhigh", "", nil, 0.07377},
		// Only the portrait orientation is published; the landscape image is the same pixels.
		{"flare xhigh 1536x1024 takes the portrait rate", "fal_ai", "openai/gpt-image-2.5/flare/text-to-image", "1536x1024", "xhigh", "", nil, 0.07377},
		{"flare max 1024x1024", "fal_ai", "openai/gpt-image-2.5/flare/text-to-image", "1024x1024", "max", "", nil, 0.21072},
		{"flare max 3840x2160", "fal_ai", "openai/gpt-image-2.5/flare/text-to-image", "3840x2160", "max", "", nil, 0.40026},
		// Tier rates: the named tier wins over the flat rate; no tier takes the flat rate.
		{"nano-banana-2 4K", "replicate", "google/nano-banana-2", "", "", "4K", nil, 0.151},
		{"nano-banana-2 2K", "replicate", "google/nano-banana-2", "", "", "2K", nil, 0.101},
		{"nano-banana-2 no tier", "replicate", "google/nano-banana-2", "", "", "", nil, 0.067},
		// A size and no tier: the tier Replicate sends for that size (4096x4096 is 4K), not the flat rate.
		{"nano-banana-2 size 4096x4096", "replicate", "google/nano-banana-2", "4096x4096", "", "", nil, 0.151},
		{"nano-banana-2 size 2048x1152", "replicate", "google/nano-banana-2", "2048x1152", "", "", nil, 0.101},
		{"nano-banana-2 size 1024x1024", "replicate", "google/nano-banana-2", "1024x1024", "", "", nil, 0.067},
		// A named tier wins over the size's tier.
		{"nano-banana-2 size 4096x4096 tier 1K", "replicate", "google/nano-banana-2", "4096x4096", "", "1K", nil, 0.067},
		{"trellis-2 1536", "fal_ai", "fal-ai/trellis-2", "", "", "1536", nil, 0.35},
		{"trellis-2 512", "fal_ai", "fal-ai/trellis-2", "", "", "512", nil, 0.25},
		{"trellis-2 0.5K", "fal_ai", "fal-ai/trellis-2", "", "", "0.5k", nil, 0.2},
		// A row priced by named tiers other than 1K has no default: unpriced, not $0 priced.
		{"trellis-2 no tier", "fal_ai", "fal-ai/trellis-2", "", "", "", nil, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cost := s.CalculateCost(variantImageResponse(tc.provider, tc.model, tc.size, tc.quality, tc.resolution, tc.steps), nil)
			assert.InDelta(t, tc.expected, cost, 1e-12)
		})
	}
}

// The ticket's dall-e-3 example through the response fields main already had (size and quality
// only), so it fails on main for the cost and not just for compiling: $0.088, never $0.
func TestCalculateCost_ImageGeneration_AzureDallE3HDQuality(t *testing.T) {
	row := pricingRowFromDatasheetJSON(t, "azure/dall-e-3", azureDallE3Row)
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{makeKey(row.Model, row.Provider, row.Mode): row})
	assert.InDelta(t, 0.088, s.CalculateCost(makeSizedImageResponse("azure", "dall-e-3", "1024x1024", "hd", 1), nil), 1e-12)
}

func TestParseImageDimensions(t *testing.T) {
	w, h := parseImageDimensions("1024x1536")
	assert.Equal(t, 1024, w)
	assert.Equal(t, 1536, h)

	w, h = parseImageDimensions("1536x1024")
	assert.Equal(t, 1536, w)
	assert.Equal(t, 1024, h)

	for _, bad := range []string{"", "invalid", "1024", "0x1024", "-1x1024"} {
		w, h = parseImageDimensions(bad)
		assert.Equal(t, 0, w, bad)
		assert.Equal(t, 0, h, bad)
	}
}

// ---------------------------------------------------------------------------
// RoutingCallCost / CalculateCost's routing branch
//
// A request that classifies via semantic and then falls back to the llm
// classifier makes two billable calls. These tests pin that both are priced —
// individually and summed — regardless of whether one, the other, or both are
// present, and that CalculateCost attributes each call's cost independently
// of the other call's CountTowardBudgets flag.
// ---------------------------------------------------------------------------

func routingCostTestStore() *Store {
	return testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("text-embedding-3-small", "openai", "embedding"): {
			Model: "text-embedding-3-small", Provider: "openai", Mode: "embedding",
			InputCostPerToken: bifrost.Ptr(0.00000002),
		},
		makeKey("claude-haiku-4-5", "anthropic", "chat"): {
			Model: "claude-haiku-4-5", Provider: "anthropic", Mode: "chat",
			InputCostPerToken: bifrost.Ptr(0.000001), OutputCostPerToken: bifrost.Ptr(0.000005),
		},
	})
}

func embedRoutingCall(inputTokens int, countTowardBudgets bool) schemas.BifrostRoutingCall {
	provider, model, tokens := "openai", "text-embedding-3-small", inputTokens
	return schemas.BifrostRoutingCall{
		ProviderUsed: &provider, ModelUsed: &model, InputTokens: &tokens,
		CountTowardBudgets: countTowardBudgets,
	}
}

func llmRoutingCall(inputTokens, outputTokens int, countTowardBudgets bool) schemas.BifrostRoutingCall {
	provider, model, in, out := "anthropic", "claude-haiku-4-5", inputTokens, outputTokens
	return schemas.BifrostRoutingCall{
		ProviderUsed: &provider, ModelUsed: &model, InputTokens: &in, OutputTokens: &out,
		CountTowardBudgets: countTowardBudgets,
	}
}

func TestRoutingCallCost_EmbedOnly(t *testing.T) {
	s := routingCostTestStore()
	// 200 tokens * 0.00000002/token
	assert.InDelta(t, 0.000004, s.RoutingCallCost(embedRoutingCall(200, true), nil), 1e-12)
}

func TestRoutingCallCost_LLMOnly(t *testing.T) {
	s := routingCostTestStore()
	// 30 input * 0.000001 + 8 output * 0.000005
	want := 30*0.000001 + 8*0.000005
	assert.InDelta(t, want, s.RoutingCallCost(llmRoutingCall(30, 8, true), nil), 1e-12)
}

func TestRoutingCallCost_CostPerRequest(t *testing.T) {
	s := routingCostTestStore()
	embeddingPricingKey := makeKey("text-embedding-3-small", "openai", "embedding")
	embeddingPricing := s.pricingData[embeddingPricingKey]
	embeddingPricing.CostPerRequest = bifrost.Ptr(0.01)
	s.pricingData[embeddingPricingKey] = embeddingPricing
	llmPricingKey := makeKey("claude-haiku-4-5", "anthropic", "chat")
	llmPricing := s.pricingData[llmPricingKey]
	llmPricing.CostPerRequest = bifrost.Ptr(0.02)
	s.pricingData[llmPricingKey] = llmPricing

	// The flat fee is added once to each internal provider call, independently
	// of whether the call is the semantic embedding or LLM classifier.
	assert.InDelta(t, 0.01+200*0.00000002, s.RoutingCallCost(embedRoutingCall(200, true), nil), 1e-12)
	assert.InDelta(t, 0.02+30*0.000001+8*0.000005, s.RoutingCallCost(llmRoutingCall(30, 8, true), nil), 1e-12)
}

func TestRoutingCallCost_IgnoresParentSelectedKey(t *testing.T) {
	s := routingCostTestStore()
	providerID := "openai"
	selectedKeyID := "parent-key"
	model := "text-embedding-3-small"
	require.NoError(t, s.SetOverrides([]configstoreTables.TablePricingOverride{
		{
			ID:               "routing-provider",
			ScopeKind:        string(ScopeKindProvider),
			ProviderID:       &providerID,
			MatchType:        string(MatchTypeExact),
			Pattern:          model,
			RequestTypes:     []schemas.RequestType{schemas.EmbeddingRequest},
			PricingPatchJSON: `{"input_cost_per_token":3}`,
		},
		{
			ID:               "routing-parent-provider-key",
			ScopeKind:        string(ScopeKindProviderKey),
			ProviderKeyID:    &selectedKeyID,
			MatchType:        string(MatchTypeExact),
			Pattern:          model,
			RequestTypes:     []schemas.RequestType{schemas.EmbeddingRequest},
			PricingPatchJSON: `{"input_cost_per_token":99}`,
		},
	}))

	// The provider-scoped override applies, but the parent request's
	// provider-key override must not leak into the routing call.
	assert.InDelta(t, 10*3, s.RoutingCallCost(embedRoutingCall(10, true), &LookupScopes{
		Provider:      "openai",
		SelectedKeyID: selectedKeyID,
	}), 1e-12)
}

// TestCalculateCost_RoutingBranchAttributesEachCallByItsOwnBudgetFlag pins the
// regression this whole redesign fixes: CalculateCost's routing branch must
// price and sum every routing metadata call, and each call's own
// CountTowardBudgets flag — not one flag for the whole stamp — decides
// whether that call's cost folds into the request's total.
func TestCalculateCost_RoutingBranchAttributesEachCallByItsOwnBudgetFlag(t *testing.T) {
	s := routingCostTestStore()
	embedCost := 200 * 0.00000002
	llmCost := 30*0.000001 + 8*0.000005

	baseResp := func(routingMetadata *schemas.BifrostRoutingMetadata) *schemas.BifrostResponse {
		return &schemas.BifrostResponse{
			ChatResponse: &schemas.BifrostChatResponse{
				Usage: &schemas.BifrostLLMUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
				ExtraFields: schemas.BifrostResponseExtraFields{
					RequestType:     schemas.ChatCompletionRequest,
					RoutingInfo:     routingInfoFor(schemas.OpenAI, "gpt-4o-mini"),
					RoutingMetadata: routingMetadata,
				},
			},
		}
	}
	s.pricingData[makeKey("gpt-4o-mini", "openai", "chat")] = configstoreTables.TableModelPricing{
		Model: "gpt-4o-mini", Provider: "openai", Mode: "chat",
		InputCostPerToken: bifrost.Ptr(0.0), OutputCostPerToken: bifrost.Ptr(0.0),
	}

	t.Run("both calls opted in", func(t *testing.T) {
		resp := baseResp(&schemas.BifrostRoutingMetadata{Calls: []schemas.BifrostRoutingCall{
			embedRoutingCall(200, true),
			llmRoutingCall(30, 8, true),
		}})
		assert.InDelta(t, embedCost+llmCost, s.CalculateCost(resp, nil), 1e-12)
	})

	t.Run("only the embed call opted in — the fix for the overwrite bug", func(t *testing.T) {
		// Before the fix, a single-slot routing metadata record meant the llm call (written
		// second) silently replaced the embed call's usage; the embed's cost was
		// unrecoverable even though it explicitly opted into budget attribution.
		resp := baseResp(&schemas.BifrostRoutingMetadata{Calls: []schemas.BifrostRoutingCall{
			embedRoutingCall(200, true),
			llmRoutingCall(30, 8, false),
		}})
		assert.InDelta(t, embedCost, s.CalculateCost(resp, nil), 1e-12)
	})

	t.Run("only the llm call opted in", func(t *testing.T) {
		resp := baseResp(&schemas.BifrostRoutingMetadata{Calls: []schemas.BifrostRoutingCall{
			embedRoutingCall(200, false),
			llmRoutingCall(30, 8, true),
		}})
		assert.InDelta(t, llmCost, s.CalculateCost(resp, nil), 1e-12)
	})

	t.Run("neither opted in", func(t *testing.T) {
		resp := baseResp(&schemas.BifrostRoutingMetadata{Calls: []schemas.BifrostRoutingCall{
			embedRoutingCall(200, false),
			llmRoutingCall(30, 8, false),
		}})
		assert.Equal(t, 0.0, s.CalculateCost(resp, nil))
	})

	t.Run("nil routing debug", func(t *testing.T) {
		resp := baseResp(nil)
		assert.Equal(t, 0.0, s.CalculateCost(resp, nil))
	})
}

// --- Video pricing dimensions (the settlement-time pricing basis) ---

// newVideoDimensionTestStore is a catalog carrying the resolution-banded video
// rates, so dimension-driven pricing is exercised against real lookup rather than
// a hand-held pricing row.
func newVideoDimensionTestStore(t *testing.T) *Store {
	t.Helper()
	banded := sizedVideoPricing()
	banded.Model = "sora-2-pro"
	banded.Provider = "openai"
	banded.Mode = "video_generation"
	return testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("sora-2-pro", "openai", "video_generation"): banded,
	})
}

func TestVideoPricingDimensions_MergedWithPrefersObserved(t *testing.T) {
	eight, twelve := 8, 12
	audio := true
	captured := VideoPricingDimensions{
		Model:       "veo-3.1",
		RequestType: schemas.VideoGenerationRequest,
		Seconds:     &eight,
		Size:        "1920x1080",
		Audio:       &audio,
		Extra:       map[string]any{"sample_count": 2},
	}
	// The provider clamped the duration and downscaled: what actually happened
	// wins, because that is what the bill is for.
	observed := VideoPricingDimensions{Seconds: &twelve, Size: "1280x720", OutputCount: 3}

	merged := captured.MergedWith(observed)
	require.NotNil(t, merged.Seconds)
	assert.Equal(t, 12, *merged.Seconds)
	assert.Equal(t, "1280x720", merged.Size)
	assert.Equal(t, 3, merged.OutputCount)
	// Everything the response stayed silent about survives from the request —
	// which for most providers is nearly all of it.
	assert.Equal(t, "veo-3.1", merged.Model)
	assert.Equal(t, schemas.VideoGenerationRequest, merged.RequestType)
	require.NotNil(t, merged.Audio)
	assert.True(t, *merged.Audio)
	assert.Equal(t, 2, merged.Extra["sample_count"])
}

func TestVideoPricingDimensions_MergedWithKeepsCapturedWhenResponseIsSilent(t *testing.T) {
	eight := 8
	captured := VideoPricingDimensions{Model: "veo-3.1", Seconds: &eight, Size: "1920x1080"}

	// The common case: a retrieve that reports nothing but "done".
	merged := captured.MergedWith(VideoPricingDimensions{})
	require.NotNil(t, merged.Seconds)
	assert.Equal(t, 8, *merged.Seconds)
	assert.Equal(t, "1920x1080", merged.Size)
	assert.Equal(t, "veo-3.1", merged.Model)
	assert.Zero(t, merged.OutputCount, "a silent response must not claim the job produced clips")
}

func TestVideoDimensionsFromResponse(t *testing.T) {
	seconds := "8"
	resp := &schemas.BifrostVideoGenerationResponse{
		Model:   "sora-2-pro",
		Seconds: &seconds,
		Size:    "1920x1080",
		Videos:  []schemas.VideoOutput{{Type: schemas.VideoOutputTypeURL}, {Type: schemas.VideoOutputTypeURL}},
	}
	dims := VideoDimensionsFromResponse(resp)
	assert.Equal(t, "sora-2-pro", dims.Model)
	require.NotNil(t, dims.Seconds)
	assert.Equal(t, 8, *dims.Seconds)
	assert.Equal(t, "1920x1080", dims.Size)
	assert.Equal(t, 2, dims.OutputCount)
	assert.Nil(t, dims.ProviderCost)
}

func TestVideoDimensionsFromResponse_CarriesProviderCost(t *testing.T) {
	resp := &schemas.BifrostVideoGenerationResponse{
		Model: "runware-video",
		Usage: &schemas.VideoUsage{Cost: &schemas.BifrostCost{TotalCost: 1.23}},
	}
	dims := VideoDimensionsFromResponse(resp)
	require.NotNil(t, dims.ProviderCost)
	assert.InDelta(t, 1.23, *dims.ProviderCost, 1e-12)
}

func TestCalculateVideoCostDetails_ProviderCostWinsOverCatalogRate(t *testing.T) {
	store := newVideoDimensionTestStore(t)
	seconds := 8
	providerCost := 0.42
	dims := VideoPricingDimensions{
		Model:        "sora-2-pro",
		RequestType:  schemas.VideoGenerationRequest,
		Seconds:      &seconds,
		Size:         "1920x1080",
		OutputCount:  1,
		ProviderCost: &providerCost,
	}
	details := store.CalculateVideoCostDetails(dims, schemas.OpenAI, nil)
	assert.True(t, details.Priced)
	assert.True(t, details.ProviderCostUsed)
	assert.InDelta(t, 0.42, details.Cost, 1e-12,
		"a provider that reports its own figure is never overridden by an estimate")
}

func TestCalculateVideoCostDetails_UsesResolutionBandedRate(t *testing.T) {
	store := newVideoDimensionTestStore(t)
	seconds := 8
	dims := VideoPricingDimensions{
		Model:       "sora-2-pro",
		RequestType: schemas.VideoGenerationRequest,
		Seconds:     &seconds,
		Size:        "1920x1080",
		OutputCount: 1,
	}
	details := store.CalculateVideoCostDetails(dims, schemas.OpenAI, nil)
	require.True(t, details.Priced)
	assert.False(t, details.ProviderCostUsed)
	assert.InDelta(t, 8*0.70, details.Cost, 1e-12)
}

func TestCalculateVideoCostDetails_BillsEveryClip(t *testing.T) {
	store := newVideoDimensionTestStore(t)
	seconds := 8
	dims := VideoPricingDimensions{
		Model:       "sora-2-pro",
		RequestType: schemas.VideoGenerationRequest,
		Seconds:     &seconds,
		Size:        "1280x720",
		OutputCount: 3,
	}
	details := store.CalculateVideoCostDetails(dims, schemas.OpenAI, nil)
	require.True(t, details.Priced)
	assert.InDelta(t, 3*8*0.30, details.Cost, 1e-12)
}

func TestCalculateVideoCostDetails_UnknownModelIsUnpriced(t *testing.T) {
	store := newVideoDimensionTestStore(t)
	seconds := 8
	dims := VideoPricingDimensions{Model: "no-such-model", Seconds: &seconds, Size: "1920x1080"}

	details := store.CalculateVideoCostDetails(dims, schemas.OpenAI, nil)
	assert.False(t, details.Priced, "no rate must read as unpriced, never as free")
	assert.Zero(t, details.Cost)
}

func TestCalculateVideoCostDetails_NoDurationIsUnpriced(t *testing.T) {
	store := newVideoDimensionTestStore(t)
	// An upscale job: no duration basis, and no upscale rate published yet. It must
	// park as unpriced rather than be billed as a zero-length generation.
	upscale := "upscale"
	factor := 4
	dims := VideoPricingDimensions{
		Model:         "sora-2-pro",
		RequestType:   schemas.VideoEditRequest,
		Type:          &upscale,
		UpscaleFactor: &factor,
	}
	details := store.CalculateVideoCostDetails(dims, schemas.OpenAI, nil)
	assert.False(t, details.Priced)
	assert.Zero(t, details.Cost)
}

// A queued video has produced nothing and may never produce anything. Billing it
// at submission is the bug the job settler exists to fix: the charge belongs at
// settlement, where the outcome is known.
func TestCalculateCost_QueuedVideoIsNotBilledAtSubmission(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("sora-2-pro", "openai", "video_generation"): {
			Model: "sora-2-pro", Provider: "openai", Mode: "video_generation",
			OutputCostPerVideoPerSecond:      bifrost.Ptr(0.30),
			OutputCostPerVideoPerSecond1080p: bifrost.Ptr(0.70),
		},
	})

	newResp := func(status schemas.VideoStatus) *schemas.BifrostResponse {
		return &schemas.BifrostResponse{
			VideoGenerationResponse: &schemas.BifrostVideoGenerationResponse{
				Status:  status,
				Seconds: bifrost.Ptr("8"),
				Size:    "1920x1080",
				ExtraFields: schemas.BifrostResponseExtraFields{
					RequestType: schemas.VideoGenerationRequest,
					RoutingInfo: routingInfoFor(schemas.OpenAI, "sora-2-pro"),
				},
			},
		}
	}

	// Failed is included deliberately: it used to fall through to computeVideoCost,
	// which bills max(1, clips) — so a failed 8s job was charged for a full clip,
	// while VideoSettler.Settle recorded the same job at zero.
	for _, status := range []schemas.VideoStatus{schemas.VideoStatusQueued, schemas.VideoStatusInProgress, schemas.VideoStatusFailed} {
		assert.Zero(t, s.CalculateCost(newResp(status), nil), string(status))
	}

	// A provider-reported cost on an unfinished job is a quote, not a bill. This is
	// the path that bypassed the gate entirely: extractCostInput routes VideoUsage
	// into input.usage.Cost, which the provider-cost short-circuit returns before
	// any status is consulted.
	for _, status := range []schemas.VideoStatus{schemas.VideoStatusQueued, schemas.VideoStatusInProgress, schemas.VideoStatusFailed} {
		quoted := newResp(status)
		quoted.VideoGenerationResponse.Usage = &schemas.VideoUsage{Cost: &schemas.BifrostCost{TotalCost: 3.21}}
		assert.Zero(t, s.CalculateCost(quoted, nil), "provider cost on %s must not bill at submission", status)
	}

	// A completed job still honours the provider's own figure verbatim.
	priced := newResp(schemas.VideoStatusCompleted)
	priced.VideoGenerationResponse.Videos = []schemas.VideoOutput{{Type: schemas.VideoOutputTypeURL}}
	priced.VideoGenerationResponse.Usage = &schemas.VideoUsage{Cost: &schemas.BifrostCost{TotalCost: 3.21}}
	assert.InDelta(t, 3.21, s.CalculateCost(priced, nil), 1e-9)

	// A terminal response still prices inline — that is what settlement drives.
	terminal := newResp(schemas.VideoStatusCompleted)
	terminal.VideoGenerationResponse.Videos = []schemas.VideoOutput{{Type: schemas.VideoOutputTypeURL}}
	assert.InDelta(t, 5.60, s.CalculateCost(terminal, nil), 1e-9)
}

// deepSeekPeakHours mirrors DeepSeek's published schedule: peak is Mon-Fri
// 01:00-04:00 and 06:00-10:00 UTC, and every other instant is off-peak.
func deepSeekPeakHours() *configstoreTables.PeakHoursSchedule {
	return &configstoreTables.PeakHoursSchedule{
		Timezone: "UTC",
		Windows: []configstoreTables.PeakHoursWindow{
			{Days: []int{1, 2, 3, 4, 5}, Start: "01:00", End: "04:00"},
			{Days: []int{1, 2, 3, 4, 5}, Start: "06:00", End: "10:00"},
		},
	}
}

// deepSeekPricing is a deepseek-v4-flash row: base rates are the PEAK prices
// and the multiplier halves them off-peak.
func deepSeekPricing() configstoreTables.TableModelPricing {
	return configstoreTables.TableModelPricing{
		Model:                   "deepseek-v4-flash",
		Provider:                "deepseek",
		Mode:                    "chat",
		InputCostPerToken:       bifrost.Ptr(0.00000044),
		OutputCostPerToken:      bifrost.Ptr(0.00000132),
		CacheReadInputTokenCost: bifrost.Ptr(0.000000014),
		OffPeakCostMultiplier:   bifrost.Ptr(0.5),
		PeakHours:               deepSeekPeakHours(),
	}
}

func utc(t *testing.T, s string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, s)
	require.NoError(t, err)
	return parsed
}

// TestOffPeak_DeepSeekSchedule walks DeepSeek's real windows. 2026-08-17 is a
// Monday, so 08-15 is Saturday and 08-16 is Sunday.
func TestOffPeak_DeepSeekSchedule(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("deepseek-v4-flash", "deepseek", "chat"): deepSeekPricing(),
	})

	// 1000 prompt + 1000 completion at peak:
	// 1000*0.00000044 + 1000*0.00000132 = 0.00176
	const peakCost = 0.00176

	cases := []struct {
		name     string
		at       string
		expected float64
	}{
		{"monday inside first peak window", "2026-08-17T02:00:00Z", peakCost},
		{"monday inside second peak window", "2026-08-17T09:59:00Z", peakCost},
		{"monday in the gap between windows", "2026-08-17T05:00:00Z", peakCost / 2},
		{"monday before the first window", "2026-08-17T00:30:00Z", peakCost / 2},
		{"monday after the last window", "2026-08-17T23:00:00Z", peakCost / 2},
		{"saturday at a peak-hour clock time", "2026-08-15T02:00:00Z", peakCost / 2},
		{"sunday at a peak-hour clock time", "2026-08-16T02:00:00Z", peakCost / 2},
		// Half-open [start, end): the start instant is peak, the end instant is not.
		{"exact window start is peak", "2026-08-17T01:00:00Z", peakCost},
		{"exact window end is off-peak", "2026-08-17T04:00:00Z", peakCost / 2},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := makeChatResponse(schemas.DeepSeek, "deepseek-v4-flash", &schemas.BifrostLLMUsage{
				PromptTokens: 1000, CompletionTokens: 1000, TotalTokens: 2000,
			})
			cost := s.CalculateCost(resp, &LookupScopes{BilledAt: utc(t, tc.at)})
			assert.InDelta(t, tc.expected, cost, 1e-12)
		})
	}
}

// TestOffPeak_DiscountsCacheReads verifies the discount reaches the cache-hit
// rate, not just the plain input rate — DeepSeek halves all three categories.
func TestOffPeak_DiscountsCacheReads(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("deepseek-v4-flash", "deepseek", "chat"): deepSeekPricing(),
	})

	usage := &schemas.BifrostLLMUsage{
		PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500,
		PromptTokensDetails: &schemas.ChatPromptTokensDetails{CachedReadTokens: 800},
	}

	peak := s.CalculateCostBreakdown(
		makeChatResponse(schemas.DeepSeek, "deepseek-v4-flash", usage),
		&LookupScopes{BilledAt: utc(t, "2026-08-17T02:00:00Z")},
	)
	off := s.CalculateCostBreakdown(
		makeChatResponse(schemas.DeepSeek, "deepseek-v4-flash", usage),
		&LookupScopes{BilledAt: utc(t, "2026-08-17T05:00:00Z")},
	)
	require.NotNil(t, peak)
	require.NotNil(t, off)
	require.NotNil(t, peak.InputCostDetails)
	require.NotNil(t, off.InputCostDetails)

	assert.Greater(t, peak.InputCostDetails.CachedReadCost, 0.0)
	assert.InDelta(t, peak.InputCostDetails.CachedReadCost/2, off.InputCostDetails.CachedReadCost, 1e-15)
	assert.InDelta(t, peak.InputCostDetails.TextCost/2, off.InputCostDetails.TextCost, 1e-15)
	assert.InDelta(t, peak.OutputCost/2, off.OutputCost, 1e-15)
	assert.InDelta(t, peak.TotalCost/2, off.TotalCost, 1e-15)
}

// TestOffPeak_FlatFeesAreNotDiscounted pins the exclusions: a per-request
// surcharge and a per-search-query fee are not usage charges, so an off-peak
// request pays them in full.
func TestOffPeak_FlatFeesAreNotDiscounted(t *testing.T) {
	pricing := deepSeekPricing()
	pricing.CostPerRequest = bifrost.Ptr(0.01)
	pricing.WebSearchCostPerRequest = bifrost.Ptr(0.005)

	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("deepseek-v4-flash", "deepseek", "chat"): pricing,
	})

	queries := 2
	usage := &schemas.BifrostLLMUsage{
		PromptTokens: 1000, CompletionTokens: 1000, TotalTokens: 2000,
		ToolUsage: &schemas.ToolUsage{WebSearch: &schemas.WebSearchToolUsage{NumRequests: queries}},
	}

	bd := s.CalculateCostBreakdown(
		makeChatResponse(schemas.DeepSeek, "deepseek-v4-flash", usage),
		&LookupScopes{BilledAt: utc(t, "2026-08-17T05:00:00Z")},
	)
	require.NotNil(t, bd)
	require.NotNil(t, bd.InputCostDetails)
	require.NotNil(t, bd.OutputCostDetails)

	// Flat fees at full price...
	assert.InDelta(t, 0.01, bd.InputCostDetails.RequestCost, 1e-12)
	assert.InDelta(t, 0.01, bd.OutputCostDetails.SearchQueriesCost, 1e-12)
	// ...token costs halved.
	assert.InDelta(t, 1000*0.00000044/2, bd.InputCostDetails.TextCost, 1e-15)
	assert.InDelta(t, 1000*0.00000132/2, bd.OutputCostDetails.TextCost, 1e-15)
	// Sides still reconcile.
	assert.InDelta(t, bd.InputCost+bd.OutputCost+bd.AdditionalCost, bd.TotalCost, 1e-12)
}

// TestOffPeak_FailsClosed covers every way the discount can fail to resolve.
// All of them must bill at the peak (higher) rate — base rates are the peak
// prices, so failing open would hand out a discount that was never configured.
func TestOffPeak_FailsClosed(t *testing.T) {
	const peakCost = 0.00176

	mutate := map[string]func(p *configstoreTables.TableModelPricing){
		"no multiplier": func(p *configstoreTables.TableModelPricing) { p.OffPeakCostMultiplier = nil },
		"no schedule":   func(p *configstoreTables.TableModelPricing) { p.PeakHours = nil },
		"empty windows": func(p *configstoreTables.TableModelPricing) {
			p.PeakHours = &configstoreTables.PeakHoursSchedule{Timezone: "UTC"}
		},
		"unknown timezone": func(p *configstoreTables.TableModelPricing) {
			p.PeakHours.Timezone = "Mars/Olympus_Mons"
		},
		"malformed clock": func(p *configstoreTables.TableModelPricing) {
			p.PeakHours.Windows = []configstoreTables.PeakHoursWindow{{Days: []int{1}, Start: "0100", End: "04:00"}}
		},
		"out of range hour": func(p *configstoreTables.TableModelPricing) {
			p.PeakHours.Windows = []configstoreTables.PeakHoursWindow{{Days: []int{1}, Start: "25:00", End: "26:00"}}
		},
		"no days": func(p *configstoreTables.TableModelPricing) {
			p.PeakHours.Windows = []configstoreTables.PeakHoursWindow{{Days: nil, Start: "01:00", End: "04:00"}}
		},
		// A Days list that parses but names no real weekday leaves the window
		// unusable. Counting it as evaluated would report "not peak" and hand
		// out the discount on garbage input.
		"weekday above range": func(p *configstoreTables.TableModelPricing) {
			p.PeakHours.Windows = []configstoreTables.PeakHoursWindow{{Days: []int{7}, Start: "01:00", End: "04:00"}}
		},
		"weekday below range": func(p *configstoreTables.TableModelPricing) {
			p.PeakHours.Windows = []configstoreTables.PeakHoursWindow{{Days: []int{-1}, Start: "01:00", End: "04:00"}}
		},
		"every weekday out of range across windows": func(p *configstoreTables.TableModelPricing) {
			p.PeakHours.Windows = []configstoreTables.PeakHoursWindow{
				{Days: []int{9}, Start: "01:00", End: "04:00"},
				{Days: []int{-3}, Start: "06:00", End: "10:00"},
			}
		},
		"multiplier above one": func(p *configstoreTables.TableModelPricing) {
			p.OffPeakCostMultiplier = bifrost.Ptr(1.5)
		},
		"zero multiplier": func(p *configstoreTables.TableModelPricing) {
			p.OffPeakCostMultiplier = bifrost.Ptr(0.0)
		},
		"negative multiplier": func(p *configstoreTables.TableModelPricing) {
			p.OffPeakCostMultiplier = bifrost.Ptr(-0.5)
		},
	}

	for name, fn := range mutate {
		t.Run(name, func(t *testing.T) {
			pricing := deepSeekPricing()
			fn(&pricing)
			s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
				makeKey("deepseek-v4-flash", "deepseek", "chat"): pricing,
			})
			resp := makeChatResponse(schemas.DeepSeek, "deepseek-v4-flash", &schemas.BifrostLLMUsage{
				PromptTokens: 1000, CompletionTokens: 1000, TotalTokens: 2000,
			})
			// 05:00 Monday is off-peak under the real schedule, so anything
			// other than the full peak price means the guard leaked.
			cost := s.CalculateCost(resp, &LookupScopes{BilledAt: utc(t, "2026-08-17T05:00:00Z")})
			assert.InDelta(t, peakCost, cost, 1e-12)
		})
	}
}

// TestOffPeak_MidnightWrappingWindow covers a window whose end is before its
// start: 22:00-02:00 on Monday must also cover Tuesday 01:00.
func TestOffPeak_MidnightWrappingWindow(t *testing.T) {
	pricing := deepSeekPricing()
	pricing.PeakHours = &configstoreTables.PeakHoursSchedule{
		Timezone: "UTC",
		Windows: []configstoreTables.PeakHoursWindow{
			{Days: []int{1}, Start: "22:00", End: "02:00"}, // Monday 22:00 -> Tuesday 02:00
		},
	}
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("deepseek-v4-flash", "deepseek", "chat"): pricing,
	})

	const peakCost = 0.00176
	cases := []struct {
		name     string
		at       string
		expected float64
	}{
		{"monday late evening is peak", "2026-08-17T23:30:00Z", peakCost},
		{"tuesday small hours still peak", "2026-08-18T01:30:00Z", peakCost},
		{"tuesday after the wrap ends", "2026-08-18T02:30:00Z", peakCost / 2},
		{"monday before the window opens", "2026-08-17T21:00:00Z", peakCost / 2},
		{"tuesday evening is not covered", "2026-08-18T23:30:00Z", peakCost / 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := makeChatResponse(schemas.DeepSeek, "deepseek-v4-flash", &schemas.BifrostLLMUsage{
				PromptTokens: 1000, CompletionTokens: 1000, TotalTokens: 2000,
			})
			cost := s.CalculateCost(resp, &LookupScopes{BilledAt: utc(t, tc.at)})
			assert.InDelta(t, tc.expected, cost, 1e-12)
		})
	}
}

// TestOffPeak_NonUTCSchedule verifies the schedule is evaluated in its own
// timezone, not the instant's UTC clock reading.
func TestOffPeak_NonUTCSchedule(t *testing.T) {
	pricing := deepSeekPricing()
	pricing.PeakHours = &configstoreTables.PeakHoursSchedule{
		Timezone: "Asia/Kolkata", // UTC+05:30
		Windows: []configstoreTables.PeakHoursWindow{
			{Days: []int{1}, Start: "09:00", End: "17:00"},
		},
	}
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("deepseek-v4-flash", "deepseek", "chat"): pricing,
	})

	const peakCost = 0.00176
	resp := func() *schemas.BifrostResponse {
		return makeChatResponse(schemas.DeepSeek, "deepseek-v4-flash", &schemas.BifrostLLMUsage{
			PromptTokens: 1000, CompletionTokens: 1000, TotalTokens: 2000,
		})
	}

	// 2026-08-17 05:00 UTC == 10:30 Monday in Kolkata -> peak.
	assert.InDelta(t, peakCost,
		s.CalculateCost(resp(), &LookupScopes{BilledAt: utc(t, "2026-08-17T05:00:00Z")}), 1e-12)
	// 2026-08-17 02:00 UTC == 07:30 Monday in Kolkata -> off-peak.
	assert.InDelta(t, peakCost/2,
		s.CalculateCost(resp(), &LookupScopes{BilledAt: utc(t, "2026-08-17T02:00:00Z")}), 1e-12)
}

// TestOffPeak_AppliesToNonTextModalities confirms the single application site
// in computeCostFromInput reaches modalities other than chat.
func TestOffPeak_AppliesToNonTextModalities(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("embed-model", "deepseek", "embedding"): {
			Model: "embed-model", Provider: "deepseek", Mode: "embedding",
			InputCostPerToken:     bifrost.Ptr(0.000001),
			OffPeakCostMultiplier: bifrost.Ptr(0.5),
			PeakHours:             deepSeekPeakHours(),
		},
	})

	usage := &schemas.BifrostLLMUsage{PromptTokens: 1000, TotalTokens: 1000}
	peak := s.CalculateCost(makeEmbeddingResponse(schemas.DeepSeek, "embed-model", usage),
		&LookupScopes{BilledAt: utc(t, "2026-08-17T02:00:00Z")})
	off := s.CalculateCost(makeEmbeddingResponse(schemas.DeepSeek, "embed-model", usage),
		&LookupScopes{BilledAt: utc(t, "2026-08-17T05:00:00Z")})

	assert.InDelta(t, 0.001, peak, 1e-12)
	assert.InDelta(t, 0.0005, off, 1e-12)
}

// insideNeverPeakWindow reports whether an instant falls inside the only span
// the neverPeak schedule below actually covers, 00:00:00-00:02:00 UTC.
func insideNeverPeakWindow(at time.Time) bool {
	at = at.UTC()
	return at.Hour() == 0 && at.Minute() < 2
}

// bracketsNeverPeakWindow reports whether a CalculateCost call that started at
// before and returned at after could have read the wall clock inside that span.
// Sampling only one side is not enough: the production read happens between the
// two, so a call that straddles 00:02:00 UTC prices as peak while a later
// single sample reads as outside the window and declines to skip.
func bracketsNeverPeakWindow(before, after time.Time) bool {
	return insideNeverPeakWindow(before) || insideNeverPeakWindow(after)
}

// TestOffPeak_ZeroBilledAtFallsBackToWallClock verifies a caller that never
// learned the request start time still prices coherently rather than treating
// the zero time (year 1) as a real instant.
//
// Both schedules below answer the same at every instant, so the test never
// reads the clock itself and cannot straddle a window boundary between
// computing the expectation and pricing the response. Deriving the expectation
// from a time.Now() of its own would race the second time.Now() inside
// offPeakMultiplier.
func TestOffPeak_ZeroBilledAtFallsBackToWallClock(t *testing.T) {
	const peakCost = 0.00176

	alwaysPeak := &configstoreTables.PeakHoursSchedule{
		Timezone: "UTC",
		Windows: []configstoreTables.PeakHoursWindow{
			{Days: []int{0, 1, 2, 3, 4, 5, 6}, Start: "00:00", End: "24:00"},
		},
	}
	// A window listing no weekday that ever occurs cannot match, so every
	// instant is off-peak.
	neverPeak := &configstoreTables.PeakHoursSchedule{
		Timezone: "UTC",
		Windows: []configstoreTables.PeakHoursWindow{
			{Days: []int{0, 1, 2, 3, 4, 5, 6}, Start: "00:00", End: "00:01"},
			{Days: []int{0, 1, 2, 3, 4, 5, 6}, Start: "00:01", End: "00:02"},
		},
	}

	for _, tc := range []struct {
		name  string
		sched *configstoreTables.PeakHoursSchedule
		want  float64
	}{
		{"peak at every instant", alwaysPeak, peakCost},
		{"off-peak at almost every instant", neverPeak, peakCost / 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pricing := deepSeekPricing()
			pricing.PeakHours = tc.sched
			s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
				makeKey("deepseek-v4-flash", "deepseek", "chat"): pricing,
			})
			resp := makeChatResponse(schemas.DeepSeek, "deepseek-v4-flash", &schemas.BifrostLLMUsage{
				PromptTokens: 1000, CompletionTokens: 1000, TotalTokens: 2000,
			})
			// A zero BilledAt is the point: it forces the wall-clock fallback.
			// The read that decides the price happens inside CalculateCost, so
			// bracket the call and skip if either side of the bracket sits in
			// the two-minute window neverPeak does cover. Sampling only after
			// the call misses a call that straddles the window's edge.
			before := time.Now()
			got := s.CalculateCost(resp, &LookupScopes{})
			after := time.Now()
			if tc.want != peakCost && bracketsNeverPeakWindow(before, after) {
				t.Skip("wall clock is inside the neverPeak schedule's only window")
			}
			assert.InDelta(t, tc.want, got, 1e-12)
		})
	}
}

// TestNeverPeakWindowBracket pins the skip guard used by
// TestOffPeak_ZeroBilledAtFallsBackToWallClock: a call whose wall-clock read
// could have landed inside the neverPeak window must be recognised as such even
// when the reading taken after the call has already left the window.
func TestNeverPeakWindowBracket(t *testing.T) {
	for _, tc := range []struct {
		name   string
		before time.Time
		after  time.Time
		want   bool
	}{
		{
			name:   "call straddles the end of the window",
			before: time.Date(2026, 8, 17, 0, 1, 59, 900_000_000, time.UTC),
			after:  time.Date(2026, 8, 17, 0, 2, 0, 100_000_000, time.UTC),
			want:   true,
		},
		{
			name:   "call straddles the start of the window",
			before: time.Date(2026, 8, 16, 23, 59, 59, 900_000_000, time.UTC),
			after:  time.Date(2026, 8, 17, 0, 0, 0, 100_000_000, time.UTC),
			want:   true,
		},
		{
			name:   "call sits wholly inside the window",
			before: time.Date(2026, 8, 17, 0, 0, 30, 0, time.UTC),
			after:  time.Date(2026, 8, 17, 0, 0, 31, 0, time.UTC),
			want:   true,
		},
		{
			name:   "call sits wholly outside the window",
			before: time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC),
			after:  time.Date(2026, 8, 17, 12, 0, 1, 0, time.UTC),
			want:   false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, bracketsNeverPeakWindow(tc.before, tc.after))
		})
	}
}

// TestOffPeak_NilScopes exercises the nil-scopes entry point, which builds an
// empty LookupScopes and therefore a zero BilledAt.
func TestOffPeak_NilScopes(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("deepseek-v4-flash", "deepseek", "chat"): deepSeekPricing(),
	})
	resp := makeChatResponse(schemas.DeepSeek, "deepseek-v4-flash", &schemas.BifrostLLMUsage{
		PromptTokens: 1000, CompletionTokens: 1000, TotalTokens: 2000,
	})
	assert.NotPanics(t, func() { s.CalculateCost(resp, nil) })
}

// TestParseClockMinutes pins the "HH:MM" parser, including the 24:00
// end-of-day marker that lets a window span a full day.
func TestParseClockMinutes(t *testing.T) {
	cases := []struct {
		in   string
		want int
		ok   bool
	}{
		{"00:00", 0, true},
		{"01:00", 60, true},
		{"09:59", 599, true},
		{"23:59", 1439, true},
		{"24:00", 1440, true},
		{"24:01", 0, false},
		{"25:00", 0, false},
		{"01:60", 0, false},
		{"-1:00", 0, false},
		{"0100", 0, false},
		{"", 0, false},
		{"aa:bb", 0, false},
	}
	for _, tc := range cases {
		got, ok := parseClockMinutes(tc.in)
		assert.Equal(t, tc.ok, ok, "ok for %q", tc.in)
		if tc.ok {
			assert.Equal(t, tc.want, got, "value for %q", tc.in)
		}
	}
}

// TestEntryUnmarshal_TimeOfDayFields verifies the two fields survive Entry's
// custom UnmarshalJSON, which routes through an alias struct to special-case
// search_context_cost_per_query and could otherwise drop embedded fields.
func TestEntryUnmarshal_TimeOfDayFields(t *testing.T) {
	raw := []byte(`{
		"provider": "deepseek",
		"mode": "chat",
		"input_cost_per_token": 0.00000044,
		"output_cost_per_token": 0.00000132,
		"off_peak_cost_multiplier": 0.5,
		"peak_hours": {
			"timezone": "UTC",
			"windows": [
				{"days": [1,2,3,4,5], "start": "01:00", "end": "04:00"},
				{"days": [1,2,3,4,5], "start": "06:00", "end": "10:00"}
			]
		}
	}`)

	var entry Entry
	require.NoError(t, entry.UnmarshalJSON(raw))

	require.NotNil(t, entry.OffPeakCostMultiplier)
	assert.Equal(t, 0.5, *entry.OffPeakCostMultiplier)
	require.NotNil(t, entry.PeakHours)
	assert.Equal(t, "UTC", entry.PeakHours.Timezone)
	require.Len(t, entry.PeakHours.Windows, 2)
	assert.Equal(t, []int{1, 2, 3, 4, 5}, entry.PeakHours.Windows[0].Days)
	assert.Equal(t, "01:00", entry.PeakHours.Windows[0].Start)
	assert.Equal(t, "10:00", entry.PeakHours.Windows[1].End)

	// And that they survive the Entry -> Table -> Entry round trip the sync uses.
	table := convertEntryToTablePricing("deepseek-v4-flash", entry)
	require.NotNil(t, table.OffPeakCostMultiplier)
	assert.Equal(t, 0.5, *table.OffPeakCostMultiplier)
	require.NotNil(t, table.PeakHours)
	assert.Len(t, table.PeakHours.Windows, 2)

	back := convertTablePricingToEntry(&table)
	require.NotNil(t, back.OffPeakCostMultiplier)
	assert.Equal(t, 0.5, *back.OffPeakCostMultiplier)
	require.NotNil(t, back.PeakHours)
	assert.Equal(t, "UTC", back.PeakHours.Timezone)
	assert.Len(t, back.PeakHours.Windows, 2)
}

// TestOffPeak_EndToEndFromDatasheetFile drives the real sync entry point
// against a file:// datasheet, so the fields are exercised through URL fetch,
// JSON decode, Entry -> TableModelPricing conversion and the in-memory catalog
// — not just the hand-built pricing rows the other tests use.
func TestOffPeak_EndToEndFromDatasheetFile(t *testing.T) {
	datasheet := `{
		"deepseek-v4-flash": {
			"provider": "deepseek",
			"mode": "chat",
			"input_cost_per_token": 0.00000044,
			"output_cost_per_token": 0.00000132,
			"off_peak_cost_multiplier": 0.5,
			"peak_hours": {
				"timezone": "UTC",
				"windows": [
					{"days": [1,2,3,4,5], "start": "01:00", "end": "04:00"},
					{"days": [1,2,3,4,5], "start": "06:00", "end": "10:00"}
				]
			}
		}
	}`

	path := filepath.Join(t.TempDir(), "datasheet.json")
	require.NoError(t, os.WriteFile(path, []byte(datasheet), 0o600))

	s := newTestStore()
	s.url = "file://" + path
	require.NoError(t, s.LoadFromURLIntoMemory(context.Background()))

	pricing, ok := s.pricingData[makeKey("deepseek-v4-flash", "deepseek", "chat")]
	require.True(t, ok, "model missing from catalog after sync")
	require.NotNil(t, pricing.OffPeakCostMultiplier, "off_peak_cost_multiplier lost in sync")
	assert.Equal(t, 0.5, *pricing.OffPeakCostMultiplier)
	require.NotNil(t, pricing.PeakHours, "peak_hours lost in sync")
	require.Len(t, pricing.PeakHours.Windows, 2)

	resp := func() *schemas.BifrostResponse {
		return makeChatResponse(schemas.DeepSeek, "deepseek-v4-flash", &schemas.BifrostLLMUsage{
			PromptTokens: 1000, CompletionTokens: 1000, TotalTokens: 2000,
		})
	}
	const peakCost = 0.00176
	assert.InDelta(t, peakCost,
		s.CalculateCost(resp(), &LookupScopes{BilledAt: utc(t, "2026-08-17T02:00:00Z")}), 1e-12)
	assert.InDelta(t, peakCost/2,
		s.CalculateCost(resp(), &LookupScopes{BilledAt: utc(t, "2026-08-17T05:00:00Z")}), 1e-12)
}

// TestOffPeak_RoutingAndCacheCalls: the routing classification calls and the
// semantic-cache embedding lookup price outside computeCostFromInput, so they
// must apply the off-peak discount themselves. The flat per-request fee stays
// at full price.
func TestOffPeak_RoutingAndCacheCalls(t *testing.T) {
	embedPricing := configstoreTables.TableModelPricing{
		Model: "embed-model", Provider: "deepseek", Mode: "embedding",
		InputCostPerToken:     bifrost.Ptr(0.00000002),
		CostPerRequest:        bifrost.Ptr(0.01),
		OffPeakCostMultiplier: bifrost.Ptr(0.5),
		PeakHours:             deepSeekPeakHours(),
	}
	chatPricing := deepSeekPricing()
	chatPricing.CostPerRequest = bifrost.Ptr(0.02)
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("embed-model", "deepseek", "embedding"):  embedPricing,
		makeKey("deepseek-v4-flash", "deepseek", "chat"): chatPricing,
	})
	off := &LookupScopes{BilledAt: utc(t, "2026-08-17T05:00:00Z")}
	peak := &LookupScopes{BilledAt: utc(t, "2026-08-17T02:00:00Z")}

	provider, embedModel, chatModel := "deepseek", "embed-model", "deepseek-v4-flash"
	in, out := 1000, 1000
	embedCall := schemas.BifrostRoutingCall{ProviderUsed: &provider, ModelUsed: &embedModel, InputTokens: &in}
	llmCall := schemas.BifrostRoutingCall{ProviderUsed: &provider, ModelUsed: &chatModel, InputTokens: &in, OutputTokens: &out}

	assert.InDelta(t, 0.01+1000*0.00000002, s.RoutingCallCost(embedCall, peak), 1e-15, "routing embed at peak")
	assert.InDelta(t, 0.01+1000*0.00000002/2, s.RoutingCallCost(embedCall, off), 1e-15, "routing embed off-peak")
	assert.InDelta(t, 0.02+0.00176, s.RoutingCallCost(llmCall, peak), 1e-15, "routing llm at peak")
	assert.InDelta(t, 0.02+0.00176/2, s.RoutingCallCost(llmCall, off), 1e-15, "routing llm off-peak")

	cache := &schemas.BifrostCacheMetadata{ProviderUsed: &provider, ModelUsed: &embedModel, InputTokens: &in}
	assert.InDelta(t, 0.01+1000*0.00000002, s.CalculateCacheEmbeddingCost(cache, peak), 1e-15, "cache lookup at peak")
	assert.InDelta(t, 0.01+1000*0.00000002/2, s.CalculateCacheEmbeddingCost(cache, off), 1e-15, "cache lookup off-peak")
}

// TestOffPeak_BatchSettlement: batch settlement and repricing go through
// CalculateBatchCostDetailsForUsage, which must apply the same off-peak multiplier
// as every other usage charge (and report the discounted effective rates), while
// leaving the flat per-request fee undiscounted.
func TestOffPeak_BatchSettlement(t *testing.T) {
	pricing := deepSeekPricing()
	pricing.InputCostPerTokenBatches = bifrost.Ptr(0.0000002)
	pricing.OutputCostPerTokenBatches = bifrost.Ptr(0.0000006)
	pricing.CostPerRequest = bifrost.Ptr(0.03)
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("deepseek-v4-flash", "deepseek", "chat"): pricing,
	})
	usage := &schemas.BifrostLLMUsage{PromptTokens: 1000, CompletionTokens: 1000, TotalTokens: 2000}
	usageCost := 1000*0.0000002 + 1000*0.0000006

	peak := s.CalculateBatchCostDetailsForUsage(usage, schemas.DeepSeek, "deepseek-v4-flash", schemas.ChatCompletionRequest,
		&LookupScopes{BilledAt: utc(t, "2026-08-17T02:00:00Z")})
	off := s.CalculateBatchCostDetailsForUsage(usage, schemas.DeepSeek, "deepseek-v4-flash", schemas.ChatCompletionRequest,
		&LookupScopes{BilledAt: utc(t, "2026-08-17T05:00:00Z")})
	require.True(t, peak.Priced)
	require.True(t, off.Priced)
	assert.InDelta(t, 0.03+usageCost, peak.Cost, 1e-15, "batch at peak")
	assert.InDelta(t, 0.03+usageCost/2, off.Cost, 1e-15, "batch off-peak halves usage, not the per-request fee")
	require.NotNil(t, off.InputCostPerTokenBatches)
	assert.InDelta(t, 0.0000001, *off.InputCostPerTokenBatches, 1e-18, "reported off-peak input rate")
	assert.InDelta(t, 0.0000006, *peak.OutputCostPerTokenBatches, 1e-18, "peak rate is unchanged")
}

// TestOffPeak_OCRPageCharges: OCR is priced per page (plus an annotation charge per
// page), and those usage charges take the off-peak multiplier like any other, while
// the flat per-request fee does not.
func TestOffPeak_OCRPageCharges(t *testing.T) {
	pricing := configstoreTables.TableModelPricing{
		Model: "mistral-ocr-latest", Provider: "mistral", Mode: "ocr",
		OCRCostPerPage:        bifrost.Ptr(0.01),
		AnnotationCostPerPage: bifrost.Ptr(0.002),
		CostPerRequest:        bifrost.Ptr(0.05),
		OffPeakCostMultiplier: bifrost.Ptr(0.5),
		PeakHours:             deepSeekPeakHours(),
	}
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("mistral-ocr-latest", "mistral", "ocr"): pricing,
	})
	annotation := `{"title":"x"}`
	resp := &schemas.BifrostResponse{OCRResponse: &schemas.BifrostOCRResponse{
		Model:              "mistral-ocr-latest",
		UsageInfo:          &schemas.OCRUsageInfo{PagesProcessed: 10},
		DocumentAnnotation: &annotation,
		ExtraFields: schemas.BifrostResponseExtraFields{
			RequestType: schemas.OCRRequest,
			RoutingInfo: routingInfoFor(schemas.Mistral, "mistral-ocr-latest"),
		},
	}}
	pages := 10 * (0.01 + 0.002)
	assert.InDelta(t, 0.05+pages, s.CalculateCost(resp, &LookupScopes{BilledAt: utc(t, "2026-08-17T02:00:00Z")}), 1e-12, "OCR at peak")
	off := s.CalculateCostBreakdown(resp, &LookupScopes{BilledAt: utc(t, "2026-08-17T05:00:00Z")})
	require.NotNil(t, off)
	assert.InDelta(t, 0.05+pages/2, off.TotalCost, 1e-12, "OCR off-peak halves page and annotation charges, not the per-request fee")
	assert.InDelta(t, off.TotalCost, off.InputCost+off.OutputCost+off.AdditionalCost, 1e-12, "breakdown stays consistent")
}

func TestCalculateCostForUsage_LiveVoiceDuration(t *testing.T) {
	pricing := configstoreTables.TableModelPricing{
		Model: "gpt-live-1", Provider: "openai", Mode: "live",
		InputCostPerSecond: new(0.001), CostPerRequest: new(0.01),
	}
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey(pricing.Model, pricing.Provider, pricing.Mode): pricing,
	})
	for _, seconds := range []float64{0, 15, 35} {
		usage := &schemas.BifrostLLMUsage{AudioSeconds: new(seconds)}
		cost := s.CalculateCostForUsage(usage, schemas.OpenAI, pricing.Model, schemas.LiveRequest, nil)
		assert.InDelta(t, seconds*0.001, cost, 1e-12)
		assert.InDelta(t, s.CalculateCost(liveVoiceWindow(pricing.Model, seconds), nil), cost, 1e-12,
			"usage carried on an error is priced the same as successful voice usage")
	}
}

// TestCalculateCostBreakdown_BillingSegmentsKeepPerResponseBoundaries: an aggregate
// that settles several responses on one unit is priced response by response. Two
// 150k-input responses stay under the 272k threshold each and keep their own service
// tiers; priced as one 300k-input response they would cross it and take one tier.
func TestCalculateCostBreakdown_BillingSegmentsKeepPerResponseBoundaries(t *testing.T) {
	p := gpt56SolPricing()
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey(p.Model, p.Provider, p.Mode): p,
	})
	segment := func(tier *schemas.BifrostServiceTier) schemas.BifrostResponsesResponse {
		return schemas.BifrostResponsesResponse{
			ServiceTier: tier,
			Usage:       &schemas.ResponsesResponseUsage{InputTokens: 150000, OutputTokens: 1000, TotalTokens: 151000},
		}
	}
	priority := schemas.BifrostServiceTierPriority
	resp := &schemas.BifrostResponse{ResponsesResponse: &schemas.BifrostResponsesResponse{
		Usage: &schemas.ResponsesResponseUsage{InputTokens: 300000, OutputTokens: 2000, TotalTokens: 302000},
		ExtraFields: schemas.BifrostResponseExtraFields{
			RequestType: schemas.ResponsesRequest,
			RoutingInfo: routingInfoFor(schemas.ModelProvider(p.Provider), p.Model),
		},
		BillingSegments: []schemas.BifrostResponsesResponse{segment(&priority), segment(nil)},
	}}

	one := &schemas.BifrostLLMUsage{PromptTokens: 150000, CompletionTokens: 1000, TotalTokens: 151000}
	want := computeTextCostTotal(&p, one, serviceTier{isPriority: true}) + computeTextCostTotal(&p, one, serviceTier{})
	merged := computeTextCostTotal(&p, &schemas.BifrostLLMUsage{PromptTokens: 300000, CompletionTokens: 2000, TotalTokens: 302000}, serviceTier{})
	require.NotEqual(t, want, merged, "fixture must make merged pricing differ")

	breakdown := s.CalculateCostBreakdown(resp, nil)
	require.NotNil(t, breakdown)
	assert.InDelta(t, want, breakdown.TotalCost, 1e-12)
	assert.InDelta(t, breakdown.TotalCost, breakdown.InputCost+breakdown.OutputCost+breakdown.AdditionalCost, 1e-12)
	assert.InDelta(t, want, s.CalculateCost(resp, nil), 1e-12)
}

// TestCalculateCost_BillingTurnsKeepPerCallThresholds: usage that sums several
// independently billable calls (an injected-tool loop's turns) is priced call by
// call, on the success path and on the bare-usage error path. Two 150k-input
// turns stay under the 272k threshold each; summed they would cross it.
func TestCalculateCost_BillingTurnsKeepPerCallThresholds(t *testing.T) {
	p := gpt56SolPricing()
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey(p.Model, p.Provider, p.Mode): p,
	})
	turn := schemas.BifrostLLMUsage{PromptTokens: 150000, CompletionTokens: 1000, TotalTokens: 151000}
	usage := schemas.AddBillingTurn(&turn, &turn)
	require.Len(t, usage.BillingTurns, 2)
	require.Equal(t, 300000, usage.PromptTokens, "the usage still reports the totals")

	want := 2 * computeTextCostTotal(&p, &turn, serviceTier{})
	merged := computeTextCostTotal(&p, &schemas.BifrostLLMUsage{PromptTokens: 300000, CompletionTokens: 2000, TotalTokens: 302000}, serviceTier{})
	require.NotEqual(t, want, merged, "fixture must make merged pricing differ")

	resp := &schemas.BifrostResponse{ChatResponse: &schemas.BifrostChatResponse{
		Usage: usage,
		ExtraFields: schemas.BifrostResponseExtraFields{
			RequestType: schemas.ChatCompletionRequest,
			RoutingInfo: routingInfoFor(schemas.ModelProvider(p.Provider), p.Model),
		},
	}}
	assert.InDelta(t, want, s.CalculateCost(resp, nil), 1e-12)
	assert.InDelta(t, want, s.CalculateCostForUsage(usage, schemas.ModelProvider(p.Provider), p.Model, schemas.ChatCompletionRequest, nil), 1e-12)

	// The Responses conversions carry the turns, and a Responses result prices them too.
	viaResponses := usage.ToResponsesResponseUsage().ToBifrostLLMUsage()
	assert.InDelta(t, want, s.CalculateCostForUsage(viaResponses, schemas.ModelProvider(p.Provider), p.Model, schemas.ResponsesRequest, nil), 1e-12)
	responsesResult := &schemas.BifrostResponse{ResponsesResponse: &schemas.BifrostResponsesResponse{
		Usage: usage.ToResponsesResponseUsage(),
		ExtraFields: schemas.BifrostResponseExtraFields{
			RequestType: schemas.ResponsesRequest,
			RoutingInfo: routingInfoFor(schemas.ModelProvider(p.Provider), p.Model),
		},
	}}
	assert.InDelta(t, want, s.CalculateCost(responsesResult, nil), 1e-12)
}

// TestCalculateCost_BillingTurnsKeepEachTurnsServedTier: a turn is priced at the tier that served
// it, on the response path and the bare-usage path alike, whatever the last turn's was.
func TestCalculateCost_BillingTurnsKeepEachTurnsServedTier(t *testing.T) {
	p := gpt56SolPricing()
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey(p.Model, p.Provider, p.Mode): p,
	})
	priority := schemas.BifrostServiceTierPriority
	first := schemas.BifrostLLMUsage{PromptTokens: 1000, CompletionTokens: 100, TotalTokens: 1100, ServiceTier: &priority}
	second := schemas.BifrostLLMUsage{PromptTokens: 1000, CompletionTokens: 100, TotalTokens: 1100}
	usage := schemas.AddBillingTurn(&first, &second)

	want := computeTextCostTotal(&p, &first, serviceTier{isPriority: true}) + computeTextCostTotal(&p, &second, serviceTier{})
	require.NotEqual(t, want, 2*computeTextCostTotal(&p, &second, serviceTier{}), "fixture must make the tiers differ")
	require.NotEqual(t, want, 2*computeTextCostTotal(&p, &first, serviceTier{isPriority: true}), "fixture must make the tiers differ")

	resp := &schemas.BifrostResponse{ChatResponse: &schemas.BifrostChatResponse{
		ServiceTier: &priority, // the last turn's tier, which must not price the others
		Usage:       usage,
		ExtraFields: schemas.BifrostResponseExtraFields{
			RequestType: schemas.ChatCompletionRequest,
			RoutingInfo: routingInfoFor(schemas.ModelProvider(p.Provider), p.Model),
		},
	}}
	assert.InDelta(t, want, s.CalculateCost(resp, nil), 1e-12)
	assert.InDelta(t, want, s.CalculateCostForUsage(usage, schemas.ModelProvider(p.Provider), p.Model, schemas.ChatCompletionRequest, nil), 1e-12)
}

// TestCalculateCost_BillingTurnsKeepEachTurnsServedModel: a turn a server-side fallback served is
// priced at that model's rates, and the turn beside it at the requested model's, whichever the
// last turn was.
func TestCalculateCost_BillingTurnsKeepEachTurnsServedModel(t *testing.T) {
	requested := gpt56SolPricing()
	served := chatPricing(0.000001, 0.000002)
	served.Model = "served-by-fallback"
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey(requested.Model, requested.Provider, requested.Mode): requested,
		makeKey(served.Model, served.Provider, served.Mode):          served,
	})
	first := schemas.BifrostLLMUsage{PromptTokens: 1000, CompletionTokens: 100, TotalTokens: 1100, ServerSideFallbackModel: bifrost.Ptr(served.Model)}
	second := schemas.BifrostLLMUsage{PromptTokens: 1000, CompletionTokens: 100, TotalTokens: 1100}
	usage := schemas.AddBillingTurn(&first, &second)

	want := computeTextCostTotal(&served, &first, serviceTier{}) + computeTextCostTotal(&requested, &second, serviceTier{})
	assert.InDelta(t, want, s.CalculateCostForUsage(usage, schemas.ModelProvider(requested.Provider), requested.Model, schemas.ChatCompletionRequest, nil), 1e-12)

	info := routingInfoFor(schemas.ModelProvider(requested.Provider), requested.Model)
	info.ServerSideFallbackModel = bifrost.Ptr(served.Model) // the last turn's, which must not price the others
	resp := &schemas.BifrostResponse{ChatResponse: &schemas.BifrostChatResponse{
		Usage:       usage,
		ExtraFields: schemas.BifrostResponseExtraFields{RequestType: schemas.ChatCompletionRequest, RoutingInfo: info},
	}}
	assert.InDelta(t, want, s.CalculateCost(resp, nil), 1e-12)
}

// TestCalculateCost_BillingTurnsFeesAndProviderCosts: a per-request fee is one per call, and a
// provider-reported cost is that call's only, so a call without one is still priced.
func TestCalculateCost_BillingTurnsFeesAndProviderCosts(t *testing.T) {
	p := gpt56SolPricing()
	p.CostPerRequest = bifrost.Ptr(0.25)
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey(p.Model, p.Provider, p.Mode): p,
	})
	provider := schemas.ModelProvider(p.Provider)
	priced := schemas.BifrostLLMUsage{PromptTokens: 1000, CompletionTokens: 100, TotalTokens: 1100}
	reported := priced
	reported.Cost = &schemas.BifrostCost{TotalCost: 3}

	plain := schemas.AddBillingTurn(&priced, &priced)
	assert.InDelta(t, 2*(computeTextCostTotal(&p, &priced, serviceTier{})+0.25),
		s.CalculateCostForUsage(plain, provider, p.Model, schemas.ChatCompletionRequest, nil), 1e-12, "one fee per call")

	mixed := schemas.AddBillingTurn(&reported, &priced)
	require.InDelta(t, 3, mixed.Cost.TotalCost, 1e-12, "the sum only knows the reported call's cost")
	assert.InDelta(t, 3+computeTextCostTotal(&p, &priced, serviceTier{})+0.25,
		s.CalculateCostForUsage(mixed, provider, p.Model, schemas.ChatCompletionRequest, nil), 1e-12)

	resp := &schemas.BifrostResponse{ChatResponse: &schemas.BifrostChatResponse{
		Usage: mixed,
		ExtraFields: schemas.BifrostResponseExtraFields{
			RequestType: schemas.ChatCompletionRequest,
			RoutingInfo: routingInfoFor(provider, p.Model),
		},
	}}
	assert.InDelta(t, 3+computeTextCostTotal(&p, &priced, serviceTier{})+0.25, s.CalculateCost(resp, nil), 1e-12)
}

// TestCalculateCost_BillingTurnsPriceReasoningOnce: reasoning tokens are part of each call's
// output and of no one else's, so the turns cost what the same calls cost one at a time.
func TestCalculateCost_BillingTurnsPriceReasoningOnce(t *testing.T) {
	p := gpt56SolPricing()
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey(p.Model, p.Provider, p.Mode): p,
	})
	provider := schemas.ModelProvider(p.Provider)
	turn := schemas.BifrostLLMUsage{
		PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500,
		CompletionTokensDetails: &schemas.ChatCompletionTokensDetails{ReasoningTokens: 300},
	}
	one := s.CalculateCostForUsage(&turn, provider, p.Model, schemas.ChatCompletionRequest, nil)
	require.Positive(t, one)
	sum := schemas.AddBillingTurn(&turn, &turn)
	require.Equal(t, 600, sum.CompletionTokensDetails.ReasoningTokens, "the total reports both calls' reasoning")
	assert.InDelta(t, 2*one, s.CalculateCostForUsage(sum, provider, p.Model, schemas.ChatCompletionRequest, nil), 1e-12)
}

// TestCalculateCostBreakdown_BillingSegmentsSidecarsOnce: sidecar costs belong to the unit, not
// to each response it settled, on the plain path and on a semantic-cache miss.
func TestCalculateCostBreakdown_BillingSegmentsSidecarsOnce(t *testing.T) {
	p := gpt56SolPricing()
	embedding := configstoreTables.TableModelPricing{
		Model: "text-embedding-3-small", Provider: "openai", Mode: "embedding", InputCostPerToken: bifrost.Ptr(0.0000001), OutputCostPerToken: bifrost.Ptr(0.0),
	}
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey(p.Model, p.Provider, p.Mode):                         p,
		makeKey(embedding.Model, embedding.Provider, embedding.Mode): embedding,
	})
	segment := schemas.BifrostResponsesResponse{Usage: &schemas.ResponsesResponseUsage{InputTokens: 150000, OutputTokens: 1000, TotalTokens: 151000}}
	one := &schemas.BifrostLLMUsage{PromptTokens: 150000, CompletionTokens: 1000, TotalTokens: 151000}
	requestCost := 2 * computeTextCostTotal(&p, one, serviceTier{})
	aggregate := func(extra schemas.BifrostResponseExtraFields) *schemas.BifrostResponse {
		extra.RequestType = schemas.ResponsesRequest
		extra.RoutingInfo = routingInfoFor(schemas.ModelProvider(p.Provider), p.Model)
		return &schemas.BifrostResponse{ResponsesResponse: &schemas.BifrostResponsesResponse{
			Usage:           &schemas.ResponsesResponseUsage{InputTokens: 300000, OutputTokens: 2000, TotalTokens: 302000},
			ExtraFields:     extra,
			BillingSegments: []schemas.BifrostResponsesResponse{segment, segment},
		}}
	}

	providerUsed, modelUsed, tokens := p.Provider, p.Model, 1000
	routed := s.CalculateCostBreakdown(aggregate(schemas.BifrostResponseExtraFields{RoutingMetadata: &schemas.BifrostRoutingMetadata{
		Calls: []schemas.BifrostRoutingCall{{ProviderUsed: &providerUsed, ModelUsed: &modelUsed, InputTokens: &tokens, RequestType: schemas.ChatCompletionRequest, CountTowardBudgets: true}},
	}}), nil)
	require.NotNil(t, routed)
	sidecar := float64(tokens) * *p.InputCostPerToken
	assert.InDelta(t, requestCost+sidecar, routed.TotalCost, 1e-9, "the routing call is billed once, not per segment")
	assert.InDelta(t, sidecar, routed.AdditionalCost, 1e-9)

	cacheProvider, cacheModel := schemas.OpenAI, embedding.Model
	cacheTokens := 1000
	missed := s.CalculateCostBreakdown(aggregate(schemas.BifrostResponseExtraFields{CacheDebug: &schemas.BifrostCacheMetadata{
		ProviderUsed: bifrost.Ptr(string(cacheProvider)), ModelUsed: &cacheModel, InputTokens: &cacheTokens,
	}}), nil)
	require.NotNil(t, missed)
	assert.InDelta(t, requestCost+1000*0.0000001, missed.TotalCost, 1e-9, "a cache miss still prices each segment, and the lookup once")
}

// TestCalculateCostBreakdown_BillingSegmentsDoNotTouchTheAggregate: pricing reads the segments;
// the aggregate a later plugin or the log row reads keeps its totals, and a provider-reported
// cost on one segment is that segment's.
func TestCalculateCostBreakdown_BillingSegmentsDoNotTouchTheAggregate(t *testing.T) {
	p := gpt56SolPricing()
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey(p.Model, p.Provider, p.Mode): p,
	})
	one := &schemas.BifrostLLMUsage{PromptTokens: 1000, CompletionTokens: 100, TotalTokens: 1100}
	reportedCost := &schemas.BifrostCost{TotalCost: 3}
	result := &schemas.BifrostResponse{ResponsesResponse: &schemas.BifrostResponsesResponse{
		Model: "aggregate-model",
		Usage: &schemas.ResponsesResponseUsage{InputTokens: 2000, OutputTokens: 200, TotalTokens: 2200},
		ExtraFields: schemas.BifrostResponseExtraFields{
			RequestType: schemas.ResponsesRequest,
			RoutingInfo: routingInfoFor(schemas.ModelProvider(p.Provider), p.Model),
		},
		BillingSegments: []schemas.BifrostResponsesResponse{
			{Usage: &schemas.ResponsesResponseUsage{InputTokens: 1000, OutputTokens: 100, TotalTokens: 1100, Cost: reportedCost}},
			{Usage: &schemas.ResponsesResponseUsage{InputTokens: 1000, OutputTokens: 100, TotalTokens: 1100}},
		},
	}}

	got := s.CalculateCostBreakdown(result, nil)

	require.NotNil(t, got)
	assert.InDelta(t, 3+computeTextCostTotal(&p, one, serviceTier{}), got.TotalCost, 1e-12)
	assert.Equal(t, 2000, result.ResponsesResponse.Usage.InputTokens)
	assert.Len(t, result.ResponsesResponse.BillingSegments, 2)
	assert.Empty(t, result.ResponsesResponse.BillingSegments[0].Model, "a segment is priced from a copy")
	assert.InDelta(t, 3, reportedCost.TotalCost, 1e-12, "the provider's cost object is not added to")
}

// TestDecisionCost_EmulatedCacheAndNativeGuards pins the row rules on the bare-usage path a
// rejected decision bills through: an emulated decision bills its cache reads and writes as the
// text call it is, a responses-only row prices it, native decision rates stay flat over the
// same usage, and a variable-price row stays unpriced.
func TestDecisionCost_EmulatedCacheAndNativeGuards(t *testing.T) {
	usage := &schemas.BifrostLLMUsage{
		PromptTokens: 1000, CompletionTokens: 10, TotalTokens: 1010,
		PromptTokensDetails: &schemas.ChatPromptTokensDetails{CachedReadTokens: 500, CachedWriteTokens: 300},
	}
	price := func(row configstoreTables.TableModelPricing) float64 {
		return lunaStore(row).CalculateCostForUsage(usage, schemas.ModelProvider(row.Provider), row.Model, schemas.DecisionRequest, nil)
	}
	emulated := lunaChatRow(false)
	emulated.CacheReadInputTokenCost = bifrost.Ptr(lunaChatInput / 10)
	emulated.CacheCreationInputTokenCost = bifrost.Ptr(lunaChatInput * 2)
	want := computeTextCostTotal(&emulated, usage, serviceTier{})
	require.NotEqual(t, want, 1000*lunaChatInput+10*lunaChatOutput, "fixture must make cache rates differ from the input rate")
	assert.InDelta(t, want, price(emulated), decisionTestEpsilon)

	responsesOnly := emulated
	responsesOnly.Mode = "responses"
	assert.InDelta(t, want, price(responsesOnly), decisionTestEpsilon)

	native := lunaChatRow(true)
	native.CacheReadInputTokenCost = emulated.CacheReadInputTokenCost
	assert.InDelta(t, 1000*lunaDecisionInput+10*lunaDecisionOutput, price(native), decisionTestEpsilon)

	variable := emulated
	variable.InputCostPerToken, variable.OutputCostPerToken = bifrost.Ptr(-1.0), bifrost.Ptr(-1.0)
	assert.Zero(t, price(variable))
}

// TestCalculateCostBreakdown_SegmentsPastTheLiveBoundStayExact: a drain stopped at its bound can
// hold a few responses more than it, and each still prices alone: 67 mixed-tier responses of 150k
// input stay under the threshold and on their own tier, far from what their sum would cost.
func TestCalculateCostBreakdown_SegmentsPastTheLiveBoundStayExact(t *testing.T) {
	p := gpt56SolPricing()
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey(p.Model, p.Provider, p.Mode): p,
	})
	priority := schemas.BifrostServiceTierPriority
	one := &schemas.BifrostLLMUsage{PromptTokens: 150000, CompletionTokens: 1000, TotalTokens: 151000}
	segments := make([]schemas.BifrostResponsesResponse, 67)
	want := 0.0
	for i := range segments {
		segments[i].Usage = &schemas.ResponsesResponseUsage{InputTokens: 150000, OutputTokens: 1000, TotalTokens: 151000}
		if i%2 == 0 {
			segments[i].ServiceTier = &priority
			want += computeTextCostTotal(&p, one, serviceTier{isPriority: true})
		} else {
			want += computeTextCostTotal(&p, one, serviceTier{})
		}
	}
	got := s.CalculateCostBreakdown(&schemas.BifrostResponse{ResponsesResponse: &schemas.BifrostResponsesResponse{
		Usage: &schemas.ResponsesResponseUsage{InputTokens: 67 * 150000, OutputTokens: 67000, TotalTokens: 67 * 151000},
		ExtraFields: schemas.BifrostResponseExtraFields{
			RequestType: schemas.ResponsesRequest,
			RoutingInfo: routingInfoFor(schemas.ModelProvider(p.Provider), p.Model),
		},
		BillingSegments: segments,
	}}, nil)
	require.NotNil(t, got)
	assert.InDelta(t, want, got.TotalCost, 1e-9)
	assert.InDelta(t, want, got.InputCost+got.OutputCost+got.AdditionalCost, 1e-9)
}

// BenchmarkCalculateCostBillingTurns measures pricing on the request path: an ordinary call
// pays one length check for the turns, and a ten-turn loop one pricing pass per call.
func BenchmarkCalculateCostBillingTurns(b *testing.B) {
	p := gpt56SolPricing()
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey(p.Model, p.Provider, p.Mode): p,
	})
	turn := &schemas.BifrostLLMUsage{PromptTokens: 150000, CompletionTokens: 1000, TotalTokens: 151000}
	var summed *schemas.BifrostLLMUsage
	for range schemas.DefaultMaxAgentDepth {
		summed = schemas.AddBillingTurn(summed, turn)
	}
	result := func(usage *schemas.BifrostLLMUsage) *schemas.BifrostResponse {
		return &schemas.BifrostResponse{ChatResponse: &schemas.BifrostChatResponse{
			Usage: usage,
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType: schemas.ChatCompletionRequest,
				RoutingInfo: routingInfoFor(schemas.ModelProvider(p.Provider), p.Model),
			},
		}}
	}
	for name, usage := range map[string]*schemas.BifrostLLMUsage{"single": turn, "loop10": summed} {
		resp := result(usage)
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if s.CalculateCost(resp, nil) <= 0 {
					b.Fatal("unpriced")
				}
			}
		})
	}
}

// BenchmarkCalculateCostRealtimeTurns prices a realtime turn reported in tokens (the common
// case, which TXE-7367 must not slow) and one reported by duration only (xAI grok-voice, which
// TXE-7367 bills per second).
func BenchmarkCalculateCostRealtimeTurns(b *testing.B) {
	duration := realtimeRowStore(b, "xai", "grok-voice-3.0", "realtime", `{"provider":"xai","mode":"realtime","input_cost_per_second":0.001333333333333}`)
	tokens := realtimeRowStore(b, "openai", "gpt-realtime", "realtime", `{"provider":"openai","mode":"realtime","input_cost_per_token":4e-06,"output_cost_per_token":1.6e-05}`)
	seconds := 60.0
	turn := func(provider schemas.ModelProvider, model string, usage *schemas.ResponsesResponseUsage) *schemas.BifrostResponse {
		return &schemas.BifrostResponse{ResponsesResponse: &schemas.BifrostResponsesResponse{
			Usage:       usage,
			ExtraFields: schemas.BifrostResponseExtraFields{RequestType: schemas.RealtimeRequest, RoutingInfo: routingInfoFor(provider, model)},
		}}
	}
	for _, tc := range []struct {
		name string
		s    *Store
		resp *schemas.BifrostResponse
	}{
		{"tokens", tokens, turn(schemas.OpenAI, "gpt-realtime", &schemas.ResponsesResponseUsage{InputTokens: 100, OutputTokens: 10, TotalTokens: 110})},
		{"duration", duration, turn(schemas.XAI, "grok-voice-3.0", &schemas.ResponsesResponseUsage{AudioSeconds: &seconds})},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			var sink float64
			for b.Loop() {
				sink += tc.s.CalculateCost(tc.resp, nil)
			}
			_ = sink
		})
	}
}

// BenchmarkCalculateCostImageServedTier prices a token-priced image response with no served tier
// (the common case) and one served on the priority tier (TXE-7372).
func BenchmarkCalculateCostImageServedTier(b *testing.B) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gemini-2.5-flash-image", "gemini", "image_generation"): {
			Model: "gemini-2.5-flash-image", Provider: "gemini", Mode: "image_generation",
			InputCostPerToken:               bifrost.Ptr(3e-07),
			OutputCostPerImageToken:         bifrost.Ptr(3e-05),
			OutputCostPerImageTokenPriority: bifrost.Ptr(5.4e-05),
		},
	})
	priority := schemas.BifrostServiceTierPriority
	for _, tc := range []struct {
		name string
		tier *schemas.BifrostServiceTier
	}{{"no-tier", nil}, {"priority", &priority}} {
		resp := makeImageResponse(schemas.Gemini, "gemini-2.5-flash-image", &schemas.ImageUsage{
			InputTokens: 12, OutputTokens: 1290, TotalTokens: 1302,
			OutputTokensDetails: &schemas.ImageTokenDetails{ImageTokens: 1290},
		})
		resp.ImageGenerationResponse.Data = []schemas.ImageData{{B64JSON: "aW1n"}}
		setImageServedTier(resp, tc.tier)
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if s.CalculateCost(resp, nil) <= 0 {
					b.Fatal("unpriced")
				}
			}
		})
	}
}

// setImageServedTier puts the served tier on an image response.
func setImageServedTier(resp *schemas.BifrostResponse, tier *schemas.BifrostServiceTier) {
	resp.ImageGenerationResponse.ServiceTier = tier
}

// TestOffPeak_RoutingCallDiscountAtWindowEdges: a classification call takes the off-peak
// discount exactly once, on its usage and never on its flat fee, at both edges of a peak
// window, and through CalculateCost it is added once beside the request it served.
func TestOffPeak_RoutingCallDiscountAtWindowEdges(t *testing.T) {
	pricing := deepSeekPricing()
	pricing.CostPerRequest = bifrost.Ptr(0.01)
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("deepseek-v4-flash", "deepseek", "chat"): pricing,
	})
	provider, model, in, out := "deepseek", "deepseek-v4-flash", 1000, 1000
	call := schemas.BifrostRoutingCall{
		ProviderUsed: &provider, ModelUsed: &model, InputTokens: &in, OutputTokens: &out,
		CountTowardBudgets: true,
	}
	const usage, flat = 0.00176, 0.01 // 1000*0.00000044 + 1000*0.00000132
	response := func() *schemas.BifrostResponse {
		return &schemas.BifrostResponse{ChatResponse: &schemas.BifrostChatResponse{
			Usage: &schemas.BifrostLLMUsage{PromptTokens: 1000, CompletionTokens: 1000, TotalTokens: 2000},
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType:     schemas.ChatCompletionRequest,
				RoutingInfo:     routingInfoFor(schemas.DeepSeek, model),
				RoutingMetadata: &schemas.BifrostRoutingMetadata{Calls: []schemas.BifrostRoutingCall{call}},
			},
		}}
	}
	for _, tc := range []struct {
		at   string
		rate float64
	}{
		{"2026-08-17T00:59:59Z", 0.5},
		{"2026-08-17T01:00:00Z", 1},
		{"2026-08-17T03:59:59Z", 1},
		{"2026-08-17T04:00:00Z", 0.5},
	} {
		t.Run(tc.at, func(t *testing.T) {
			scopes := &LookupScopes{BilledAt: utc(t, tc.at)}
			assert.InDelta(t, usage*tc.rate+flat, s.RoutingCallCost(call, scopes), 1e-12, "the call alone")
			assert.InDelta(t, 2*(usage*tc.rate+flat), s.CalculateCost(response(), scopes), 1e-12, "the call beside its request")
		})
	}
}

// BenchmarkVideoCostInputs measures the two places a video's billable clip count is read, which
// upstream's last-frame change (20dac1fc5) turned from len() into a scan of the outputs: the
// settlement dimensions and the cost input of a video response.
func BenchmarkVideoCostInputs(b *testing.B) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("sora-2", "openai", "video_generation"): {
			Model: "sora-2", Provider: "openai", Mode: "video_generation",
			OutputCostPerVideoPerSecond: bifrost.Ptr(0.30),
		},
	})
	video := func(outputs ...schemas.VideoOutput) *schemas.BifrostVideoGenerationResponse {
		return &schemas.BifrostVideoGenerationResponse{
			Status:  schemas.VideoStatusCompleted,
			Seconds: bifrost.Ptr("5"),
			Size:    "1280x720",
			Videos:  outputs,
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType: schemas.VideoGenerationRequest,
				RoutingInfo: routingInfoFor(schemas.OpenAI, "sora-2"),
			},
		}
	}
	clip := schemas.VideoOutput{Type: schemas.VideoOutputTypeURL, URL: bifrost.Ptr("https://example.test/v.mp4"), ContentType: "video/mp4"}
	frame := schemas.VideoOutput{Type: schemas.VideoOutputTypeURL, URL: bifrost.Ptr("https://example.test/last.jpeg"), ContentType: "image/jpeg"}
	for _, tc := range []struct {
		name string
		resp *schemas.BifrostVideoGenerationResponse
	}{
		{"one_clip", video(clip)},
		{"clip_and_last_frame", video(clip, frame)},
	} {
		b.Run("dimensions/"+tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if dims := VideoDimensionsFromResponse(tc.resp); dims.OutputCount == 0 {
					b.Fatal("no clips counted")
				}
			}
		})
		result := &schemas.BifrostResponse{VideoGenerationResponse: tc.resp}
		b.Run("calculate_cost/"+tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if s.CalculateCost(result, nil) <= 0 {
					b.Fatal("unpriced")
				}
			}
		})
	}
}

// TXE-6959: text cost per cache-write tier shape.
func BenchmarkComputeTextCostCacheWriteTiers(b *testing.B) {
	p := chatPricing(0.000001, 0.000008)
	p.CacheCreationInputTokenCost = bifrost.Ptr(0.00000125)
	p.CacheCreationInputTokenCostAbove1hr = bifrost.Ptr(0.0000025)
	cases := []struct {
		name  string
		tiers *schemas.ChatCachedWriteTokenDetails
	}{
		{"no_tiers", nil},
		{"anthropic_5m_1h", &schemas.ChatCachedWriteTokenDetails{CachedWriteTokens5m: 6000, CachedWriteTokens1h: 2000}},
		{"openai_30m_12h", &schemas.ChatCachedWriteTokenDetails{CachedWriteTokens30m: 6000, CachedWriteTokens12h: 2000}},
	}
	for _, tc := range cases {
		usage := &schemas.BifrostLLMUsage{PromptTokens: 10000, CompletionTokens: 100, TotalTokens: 10100,
			PromptTokensDetails: &schemas.ChatPromptTokensDetails{CachedWriteTokens: 8000, CachedWriteTokenDetails: tc.tiers}}
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = computeTextCostTotal(&p, usage, serviceTier{})
			}
		})
	}
}

// TXE-6431: an image task is billed once, at its create, as the one image it makes; the query that
// returns the finished task and the cancel are not billed again. Before, every image task cost 0,
// so a budget-limited virtual key's paid BFL submits charged nothing.
func TestCalculateCost_ImageTaskBilledOnceAtCreate(t *testing.T) {
	perImage := configstoreTables.TableModelPricing{
		InputCostPerToken:  bifrost.Ptr(0.0),
		OutputCostPerToken: bifrost.Ptr(0.0),
		OutputCostPerImage: bifrost.Ptr(0.04),
	}
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("flux-pro-1.1", "bfl", "image_generation"): perImage,
	})
	task := func(requestType schemas.RequestType, status schemas.TaskStatus, data []schemas.TaskArtifact) *schemas.BifrostResponse {
		return &schemas.BifrostResponse{ImageTaskResponse: &schemas.BifrostImageTaskResponse{
			Status: status,
			Data:   data,
			Usage:  &schemas.ImageUsage{VendorCost: &schemas.VendorCost{Amount: 4, Unit: schemas.VendorCostUnitCredit}},
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType: requestType,
				RoutingInfo: routingInfoFor(schemas.BFL, "flux-pro-1.1"),
			},
		}}
	}
	link := []schemas.TaskArtifact{{Type: schemas.VideoOutputTypeURL, URL: bifrost.Ptr("https://delivery.example.test/s.jpeg")}}

	assert.InDelta(t, 0.04, s.CalculateCost(task(schemas.ImageGenerationTaskRequest, schemas.VideoStatusQueued, []schemas.TaskArtifact{}), nil), 1e-12,
		"the create is billed as one image")
	assert.InDelta(t, 0.04, s.CalculateCost(task(schemas.ImageEditTaskRequest, schemas.VideoStatusQueued, nil), nil), 1e-12,
		"an edit task create falls back to the model's image generation price")
	assert.Zero(t, s.CalculateCost(task(schemas.ImageTaskRetrieveRequest, schemas.VideoStatusCompleted, link), nil),
		"the query of the finished task is not billed again")
	assert.Zero(t, s.CalculateCost(task(schemas.ImageTaskCancelRequest, schemas.VideoStatusQueued, nil), nil))
}

// Cost path for embeddings (TXE-7081): the common token-priced case, and a
// character-priced Vertex text embedding.
func BenchmarkComputeEmbeddingCost(b *testing.B) {
	tokenPriced := configstoreTables.TableModelPricing{InputCostPerToken: bifrost.Ptr(0.0000001)}
	charPriced := configstoreTables.TableModelPricing{InputCostPerToken: bifrost.Ptr(0.0), InputCostPerCharacter: bifrost.Ptr(0.000000025)}
	chars := 4000
	tokens := &schemas.BifrostLLMUsage{PromptTokens: 900, TotalTokens: 900}
	characters := &schemas.BifrostLLMUsage{BilledCharacters: &chars}
	b.Run("tokens", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_ = computeEmbeddingCost(&tokenPriced, tokens, serviceTier{})
		}
	})
	b.Run("characters", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_ = computeEmbeddingCost(&charPriced, characters, serviceTier{})
		}
	})
}

// TestVideoSeconds pins the one reading of a video duration (TXE-7297): whole seconds as given,
// a vendor's fractional length rounded to the nearest second and never below one, and anything
// that is not a positive number (auto, empty, zero) read as no duration. A fraction dropped by
// strconv.Atoi left a Kling job ("5.1") with no duration, and it settled unpriced.
func TestVideoSeconds(t *testing.T) {
	cases := []struct {
		in   string
		want int
		ok   bool
	}{
		{"8", 8, true}, {" 4 ", 4, true}, {"5.1", 5, true}, {"6.5", 7, true}, {"5.0", 5, true},
		{"0.3", 1, true}, {"auto", 0, false}, {"", 0, false}, {"0", 0, false}, {"-2", 0, false}, {"NaN", 0, false},
	}
	for _, tc := range cases {
		got, ok := VideoSeconds(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("VideoSeconds(%q) = %d, %v; want %d, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
	dims := VideoDimensionsFromResponse(&schemas.BifrostVideoGenerationResponse{Seconds: schemas.Ptr("5.1")})
	if dims.Seconds == nil || *dims.Seconds != 5 {
		t.Errorf("a response reporting 5.1 seconds gives %v, want 5", dims.Seconds)
	}
}

// pixverseV6Pricing is replicate/pixverse/pixverse-v6 as the upstream datasheet publishes it
// (getbifrost.ai/datasheet, 14 Nov 2026): a silent and a with-audio rate for each band, and no
// 4K rate at all.
func pixverseV6Pricing() configstoreTables.TableModelPricing {
	return configstoreTables.TableModelPricing{
		Model: "pixverse/pixverse-v6", Provider: "replicate", Mode: "video_generation",
		OutputCostPerVideoPerSecond:           bifrost.Ptr(0.05),
		OutputCostPerVideoPerSecond360p:       bifrost.Ptr(0.05),
		OutputCostPerVideoPerSecond360pAudio:  bifrost.Ptr(0.07),
		OutputCostPerVideoPerSecond540p:       bifrost.Ptr(0.07),
		OutputCostPerVideoPerSecond540pAudio:  bifrost.Ptr(0.09),
		OutputCostPerVideoPerSecond720p:       bifrost.Ptr(0.09),
		OutputCostPerVideoPerSecond720pAudio:  bifrost.Ptr(0.12),
		OutputCostPerVideoPerSecond1080p:      bifrost.Ptr(0.18),
		OutputCostPerVideoPerSecond1080pAudio: bifrost.Ptr(0.23),
	}
}

// TestCalculateVideoCostDetails_PricesAudioAtTheBandsAudioRate (TXE-7306): a job with generated
// audio settles at its band's with-audio rate, an explicit audio false at the silent rate, and
// an unstated flag at Replicate's default, with audio. A band with no audio rate, or a size
// with no band, falls back to the silent rates. Before the audio keys were parsed, every one of
// these settled at a silent rate, and 360p and 540p at the unbanded 0.05.
func TestCalculateVideoCostDetails_PricesAudioAtTheBandsAudioRate(t *testing.T) {
	store := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("pixverse/pixverse-v6", "replicate", "video_generation"): pixverseV6Pricing(),
	})
	on, off := true, false
	cases := []struct {
		name  string
		size  string
		audio *bool
		rate  float64
	}{
		{"720p with audio", "1280x720", &on, 0.12},
		{"720p silent", "1280x720", &off, 0.09},
		{"720p audio unstated is priced with audio", "1280x720", nil, 0.12},
		{"portrait 720p with audio", "720x1280", &on, 0.12},
		{"360p with audio", "640x360", &on, 0.07},
		{"360p silent band", "640x360", &off, 0.05},
		{"540p silent band", "960x540", &off, 0.07},
		{"540p with audio", "960x540", &on, 0.09},
		{"1080p with audio", "1920x1080", &on, 0.23},
		{"4K has no rate of its own: the unbanded rate", "3840x2160", &on, 0.05},
		{"no size names no band: the unbanded rate", "", &on, 0.05},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seconds := 5
			details := store.CalculateVideoCostDetails(VideoPricingDimensions{
				Model: "pixverse/pixverse-v6", RequestType: schemas.VideoGenerationRequest,
				Seconds: &seconds, Size: tc.size, Audio: tc.audio, OutputCount: 1,
			}, schemas.Replicate, nil)
			require.True(t, details.Priced)
			assert.InDelta(t, 5*tc.rate, details.Cost, 1e-12)
		})
	}
}

// TestVideoPricedWithAudio_FollowsTheProvidersDefault (TXE-7306): a stated flag wins; an unstated
// one takes the provider's audio default, and a provider with no entry is priced with audio. A
// provider whose vendor renders silence by default settles an unstated job at the silent rate.
func TestVideoPricedWithAudio_FollowsTheProvidersDefault(t *testing.T) {
	const silentDefault schemas.ModelProvider = "test-silent-default"
	VideoAudioDefaults[silentDefault] = VideoAudioDefault{On: false, Source: "test"}
	t.Cleanup(func() { delete(VideoAudioDefaults, silentDefault) })
	on, off := true, false

	assert.True(t, VideoPricedWithAudio(schemas.Runway, nil), "Runway renders audio by default")
	assert.False(t, VideoPricedWithAudio(schemas.Runway, &off))
	assert.False(t, VideoPricedWithAudio(silentDefault, nil))
	assert.True(t, VideoPricedWithAudio(silentDefault, &on))
	assert.True(t, VideoPricedWithAudio("no-entry-provider", nil), "a provider with no entry is priced with audio")

	pricing := pixverseV6Pricing()
	pricing.Provider = string(silentDefault)
	store := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("pixverse/pixverse-v6", string(silentDefault), "video_generation"): pricing,
	})
	seconds := 5
	details := store.CalculateVideoCostDetails(VideoPricingDimensions{
		Model: "pixverse/pixverse-v6", RequestType: schemas.VideoGenerationRequest, Seconds: &seconds, Size: "1280x720", OutputCount: 1,
	}, silentDefault, nil)
	require.True(t, details.Priced)
	assert.InDelta(t, 5*0.09, details.Cost, 1e-12, "an unstated flag on a silent-default provider takes the silent rate")
}

// TestCalculateCost_VideoResponsePricesUnstatedAudioWithAudio: the per-request price of a
// finished video reads no audio flag (a response does not carry one), so it takes its provider's
// audio default: with audio for Replicate, as settlement does for an unstated flag.
func TestCalculateCost_VideoResponsePricesUnstatedAudioWithAudio(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("pixverse/pixverse-v6", "replicate", "video_generation"): pixverseV6Pricing(),
	})
	resp := &schemas.BifrostResponse{
		VideoGenerationResponse: &schemas.BifrostVideoGenerationResponse{
			Status:  schemas.VideoStatusCompleted,
			Seconds: bifrost.Ptr("8"),
			Size:    "1280x720",
			Videos:  []schemas.VideoOutput{{Type: schemas.VideoOutputTypeURL, URL: bifrost.Ptr("https://example.test/v.mp4")}},
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType: schemas.VideoGenerationRequest,
				RoutingInfo: routingInfoFor(schemas.Replicate, "pixverse/pixverse-v6"),
			},
		},
	}
	assert.InDelta(t, 8*0.12, s.CalculateCost(resp, nil), 1e-9)
}

// BenchmarkVideoOutputPerSecondRate is the request-path cost TXE-7306 adds: one more nil check
// for an audio band before the silent band lookup.
// TXE-7356: the request-path cost of pricing a per-image response, the common cases (a joint
// size and quality rate, a flat rate) and the rates TXE-7356 adds (a resolution tier, steps).
func BenchmarkCalculateCostImagePerImage(b *testing.B) {
	flat := configstoreTables.TableModelPricing{Model: "flat-image", Provider: "openai", Mode: "image_generation", OutputCostPerImage: bifrost.Ptr(0.04)}
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gpt-image-1", "openai", "image_generation"): sizeQualityImagePricing(),
		makeKey("flat-image", "openai", "image_generation"):  flat,
	})
	joint := makeSizedImageResponse("openai", "gpt-image-1", "1024x1536", "high", 1)
	flatResp := makeSizedImageResponse("openai", "flat-image", "1024x1024", "", 1)
	b.Run("joint size and quality", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_ = s.CalculateCost(joint, nil)
		}
	})
	b.Run("flat per image", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_ = s.CalculateCost(flatResp, nil)
		}
	})
}

// BenchmarkCalculateCostImageVariants is BenchmarkCalculateCostImagePerImage for the TXE-7356 rates.
func BenchmarkCalculateCostImageVariants(b *testing.B) {
	tier := configstoreTables.TableModelPricing{Model: "grok-imagine-image-quality", Provider: "xai", Mode: "image_generation",
		OutputCostPerImage1K: bifrost.Ptr(0.04), OutputCostPerImage2K: bifrost.Ptr(0.06)}
	steps := configstoreTables.TableModelPricing{Model: "stability.stable-diffusion-xl-v1", Provider: "bedrock", Mode: "image_generation",
		OutputCostPerImageAbove1024x1024Pixels50Steps: bifrost.Ptr(0.04), OutputCostPerImageAbove1024x1024PixelsMaxSteps: bifrost.Ptr(0.08)}
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey(tier.Model, tier.Provider, tier.Mode):    tier,
		makeKey(steps.Model, steps.Provider, steps.Mode): steps,
	})
	tierResp := variantImageResponse("xai", tier.Model, "", "", "2k", nil)
	stepsResp := variantImageResponse("bedrock", steps.Model, "1024x1024", "", "", bifrost.Ptr(60))
	b.Run("resolution tier", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_ = s.CalculateCost(tierResp, nil)
		}
	})
	b.Run("steps", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_ = s.CalculateCost(stepsResp, nil)
		}
	})
}

// TXE-7357: the cost of settling a video job, the common per-second case and the rates TXE-7357
// adds (a clip price, a mode, video tokens).
func BenchmarkCalculateVideoCostDetailsBands(b *testing.B) {
	p := pixverseV6Pricing()
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{makeKey(p.Model, p.Provider, p.Mode): p})
	seconds := 5
	dims := VideoPricingDimensions{Model: p.Model, RequestType: schemas.VideoGenerationRequest, Seconds: &seconds, Size: "1280x720", OutputCount: 1}
	b.ReportAllocs()
	for b.Loop() {
		_ = s.CalculateVideoCostDetails(dims, schemas.Replicate, nil)
	}
}

// BenchmarkCalculateVideoCostDetailsTXE7357 is BenchmarkCalculateVideoCostDetailsBands for the TXE-7357 rates.
func BenchmarkCalculateVideoCostDetailsTXE7357(b *testing.B) {
	s := txe7357Store(b)
	off := false
	cases := map[string]struct {
		provider schemas.ModelProvider
		dims     VideoPricingDimensions
	}{
		"clip":         {schemas.MiniMax, VideoPricingDimensions{Model: "MiniMax-Hailuo-2.3", Seconds: bifrost.Ptr(6), Size: "1366x768"}},
		"mode":         {schemas.Replicate, VideoPricingDimensions{Model: "kwaivgi/kling-v3-video", Seconds: bifrost.Ptr(5), Audio: &off, Extra: map[string]any{"mode": "pro"}}},
		"video_tokens": {schemas.BytePlus, VideoPricingDimensions{Model: "dreamina-seedance-2-0-260128", OutputTokens: bifrost.Ptr(108900)}},
	}
	for name, tc := range cases {
		tc.dims.RequestType = schemas.VideoGenerationRequest
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = s.CalculateVideoCostDetails(tc.dims, tc.provider, nil)
			}
		})
	}
}

func BenchmarkVideoOutputPerSecondRate(b *testing.B) {
	p := pixverseV6Pricing()
	b.ReportAllocs()
	for b.Loop() {
		_ = videoOutputPerSecondRate(&p, "1280x720", true)
		_ = videoOutputPerSecondRate(&p, "1280x720", false)
	}
}

// =========================================================================
// TXE-7353: service-tier and 12h cache-write rates upstream publishes. Every value below is
// the live datasheet's (getbifrost.ai/datasheet, 14 Nov 2026).
// =========================================================================

// decodeEntry decodes a datasheet row the way sync does and converts it to a pricing row.
func decodeEntry(t *testing.T, row string) configstoreTables.TableModelPricing {
	t.Helper()
	var entry Entry
	require.NoError(t, json.Unmarshal([]byte(row), &entry))
	return convertEntryToTablePricing("openai/test-model", entry)
}

// azure/gpt-6-sol publishes the 12h write at 5e-06 and no above-1hr rate. Before TXE-7353 a 12h
// write fell back to the base write rate, 2.5e-06, half its price.
func TestComputeTextCost_OpenAI12hCacheWriteUsesThe12hRate(t *testing.T) {
	p := decodeEntry(t, `{
		"provider": "azure", "mode": "chat",
		"input_cost_per_token": 2e-06, "output_cost_per_token": 1e-05,
		"cache_creation_input_token_cost": 2.5e-06,
		"cache_creation_input_token_cost_above_12hr": 5e-06,
		"cache_creation_input_token_cost_above_12hr_above_272k_tokens": 1e-05,
		"cache_creation_input_token_cost_above_12hr_flex": 2.5e-06,
		"cache_creation_input_token_cost_above_12hr_above_272k_tokens_flex": 5e-06,
		"cache_creation_input_token_cost_above_12hr_priority": 1e-05,
		"cache_creation_input_token_cost_above_12hr_above_272k_tokens_priority": 2e-05,
		"cache_creation_input_token_cost_above_12hr_ultrafast": 2e-05,
		"cache_creation_input_token_cost_above_12hr_above_272k_tokens_ultrafast": 4e-05
	}`)
	write := func(prompt int) *schemas.BifrostLLMUsage {
		return &schemas.BifrostLLMUsage{
			PromptTokens: prompt, TotalTokens: prompt,
			PromptTokensDetails: &schemas.ChatPromptTokensDetails{
				CachedWriteTokens: 8000,
				CachedWriteTokenDetails: &schemas.ChatCachedWriteTokenDetails{
					CachedWriteTokens30m: 6000,
					CachedWriteTokens12h: 2000,
				},
			},
		}
	}
	cases := []struct {
		name    string
		prompt  int
		tier    serviceTier
		rate12h float64
	}{
		{"standard", 10000, serviceTier{}, 5e-06},
		{"standard above 272k", 300000, serviceTier{}, 1e-05},
		{"flex", 10000, serviceTier{isFlex: true}, 2.5e-06},
		{"flex above 272k", 300000, serviceTier{isFlex: true}, 5e-06},
		{"priority", 10000, serviceTier{isPriority: true}, 1e-05},
		{"priority above 272k", 300000, serviceTier{isPriority: true}, 2e-05},
		{"ultrafast", 10000, serviceTier{isUltrafast: true}, 2e-05},
		{"ultrafast above 272k", 300000, serviceTier{isUltrafast: true}, 4e-05},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := computeTextCost(&p, write(tc.prompt), tc.tier)
			require.NotNil(t, got)
			want12h := 2000 * tc.rate12h
			// The 30m writes take the row's base write rate in every case (the row carries no
			// tier or 272k base write rate).
			assert.InDelta(t, want12h+6000*2.5e-06, got.InputCostDetails.CachedWriteCost, 1e-12)
		})
	}
}

// A 1h write and a 12h write in one response each take their own rate; a row without 12h rates
// keeps pricing a 12h write at the above-1hr rate, as before TXE-7353.
func TestComputeTextCost_1hAnd12hWritesTakeTheirOwnRates(t *testing.T) {
	p := chatPricing(1e-06, 8e-06)
	p.CacheCreationInputTokenCost = bifrost.Ptr(1.25e-06)
	p.CacheCreationInputTokenCostAbove1hr = bifrost.Ptr(2e-06)
	p.CacheCreationInputTokenCostAbove12hr = bifrost.Ptr(3e-06)
	usage := &schemas.BifrostLLMUsage{
		PromptTokens: 10000, TotalTokens: 10000,
		PromptTokensDetails: &schemas.ChatPromptTokensDetails{
			CachedWriteTokens: 6000,
			CachedWriteTokenDetails: &schemas.ChatCachedWriteTokenDetails{
				CachedWriteTokens5m: 1000, CachedWriteTokens1h: 2000, CachedWriteTokens12h: 3000,
			},
		},
	}
	assert.InDelta(t, 4000*1e-06+1000*1.25e-06+2000*2e-06+3000*3e-06, computeTextCostTotal(&p, usage, serviceTier{}), 1e-12)

	p.CacheCreationInputTokenCostAbove12hr = nil
	assert.InDelta(t, 4000*1e-06+1000*1.25e-06+5000*2e-06, computeTextCostTotal(&p, usage, serviceTier{}), 1e-12)
}

// A batch 12h write takes the synchronous 12h rate scaled by the batch ratio, like every other
// batch category.
func TestComputeBatchTextCost_12hCacheWriteScalesThe12hRate(t *testing.T) {
	p := chatPricing(2e-06, 1e-05)
	p.InputCostPerTokenBatches = bifrost.Ptr(1e-06)
	p.CacheCreationInputTokenCost = bifrost.Ptr(2.5e-06)
	p.CacheCreationInputTokenCostAbove12hr = bifrost.Ptr(5e-06)
	usage := &schemas.BifrostLLMUsage{
		PromptTokens: 10000, TotalTokens: 10000,
		PromptTokensDetails: &schemas.ChatPromptTokensDetails{
			CachedWriteTokens:       2000,
			CachedWriteTokenDetails: &schemas.ChatCachedWriteTokenDetails{CachedWriteTokens12h: 2000},
		},
	}
	got := computeBatchTextCost(&p, usage, serviceTier{})
	require.NotNil(t, got)
	assert.InDelta(t, 8000*1e-06+2000*5e-06*0.5, got.TotalCost, 1e-12)
}

// azure/gpt-6-astra publishes its flex 272k rate only as input_cost_per_token_above_272k_tokens_flex.
// Before TXE-7353 that spelling was dropped and a 300k-token flex request billed at the flat flex
// rate, 5e-06, half its price.
func TestEntry_UpstreamRateAliasesFoldIntoTheirFields(t *testing.T) {
	p := decodeEntry(t, `{
		"provider": "azure", "mode": "chat",
		"input_cost_per_token": 1e-05, "output_cost_per_token": 6e-05,
		"input_cost_per_token_flex": 5e-06, "output_cost_per_token_flex": 3e-05,
		"input_cost_per_token_above_272k_tokens_flex": 1e-05,
		"output_cost_per_token_above_272k_tokens_flex": 4.5e-05,
		"cache_read_input_token_cost_above_272k_tokens_flex": 1e-06,
		"cache_creation_input_token_cost_above_272k_tokens_flex": 1.25e-05
	}`)
	require.NotNil(t, p.InputCostPerTokenFlexAbove272kTokens)
	assert.Equal(t, 1e-05, *p.InputCostPerTokenFlexAbove272kTokens)
	assert.Equal(t, 4.5e-05, *p.OutputCostPerTokenFlexAbove272kTokens)
	assert.Equal(t, 1e-06, *p.CacheReadInputTokenCostFlexAbove272kTokens)
	assert.Equal(t, 1.25e-05, *p.CacheCreationInputTokenCostFlexAbove272kTokens)

	usage := &schemas.BifrostLLMUsage{PromptTokens: 300000, CompletionTokens: 1000, TotalTokens: 301000}
	assert.InDelta(t, 300000*1e-05+1000*4.5e-05, computeTextCostTotal(&p, usage, serviceTier{isFlex: true}), 1e-9)

	// OpenAI's fast tier is Priority renamed: *_fast_above_272k_tokens fills the priority 272k
	// rate when the row lacks it, and the tier-first priority spelling fills it first.
	fast := decodeEntry(t, `{
		"provider": "openai", "mode": "chat",
		"input_cost_per_token": 2e-06, "output_cost_per_token": 1e-05,
		"input_cost_per_token_fast_above_272k_tokens": 8e-06,
		"output_cost_per_token_fast_above_272k_tokens": 3e-05,
		"cache_read_input_token_cost_fast_above_272k_tokens": 8e-07,
		"cache_creation_input_token_cost_fast_above_272k_tokens": 1e-05,
		"cache_read_input_token_cost_priority_above_272k_tokens": 9e-07
	}`)
	assert.Equal(t, 8e-06, *fast.InputCostPerTokenAbove272kTokensPriority)
	assert.Equal(t, 3e-05, *fast.OutputCostPerTokenAbove272kTokensPriority)
	assert.Equal(t, 9e-07, *fast.CacheReadInputTokenCostAbove272kTokensPriority)
	assert.Equal(t, 1e-05, *fast.CacheCreationInputTokenCostAbove272kTokensPriority)

	// The fork's own spelling wins over an alias on the same row.
	both := decodeEntry(t, `{
		"provider": "openai", "mode": "chat",
		"input_cost_per_token_flex_above_272k_tokens": 2e-06,
		"input_cost_per_token_above_272k_tokens_flex": 3e-06
	}`)
	assert.Equal(t, 2e-06, *both.InputCostPerTokenFlexAbove272kTokens)
}

// gemini/gemini-3.1-pro-preview publishes flex above 200k at twice the flat flex rate.
func TestComputeTextCost_FlexAbove200k(t *testing.T) {
	p := decodeEntry(t, `{
		"provider": "gemini", "mode": "chat",
		"input_cost_per_token": 2e-06, "output_cost_per_token": 1.2e-05,
		"input_cost_per_token_above_200k_tokens": 4e-06, "output_cost_per_token_above_200k_tokens": 1.8e-05,
		"cache_read_input_token_cost": 2e-07, "cache_read_input_token_cost_above_200k_tokens": 4e-07,
		"input_cost_per_token_flex": 1e-06, "output_cost_per_token_flex": 6e-06,
		"cache_read_input_token_cost_flex": 2e-07,
		"input_cost_per_token_above_200k_tokens_flex": 2e-06,
		"output_cost_per_token_above_200k_tokens_flex": 9e-06,
		"cache_read_input_token_cost_above_200k_tokens_flex": 4e-07
	}`)
	flex := serviceTier{isFlex: true}
	long := &schemas.BifrostLLMUsage{
		PromptTokens: 250000, CompletionTokens: 1000, TotalTokens: 251000,
		PromptTokensDetails: &schemas.ChatPromptTokensDetails{CachedReadTokens: 50000},
	}
	assert.InDelta(t, 200000*2e-06+50000*4e-07+1000*9e-06, computeTextCostTotal(&p, long, flex), 1e-9)
	short := &schemas.BifrostLLMUsage{PromptTokens: 1000, CompletionTokens: 100, TotalTokens: 1100}
	assert.InDelta(t, 1000*1e-06+100*6e-06, computeTextCostTotal(&p, short, flex), 1e-12)
}

// gemini-2.5-flash publishes audio input at 1e-06, 5e-07 on flex and 1.8e-06 on priority.
func TestComputeTextCost_AudioInputTokensByTier(t *testing.T) {
	p := decodeEntry(t, `{
		"provider": "vertex_ai", "mode": "chat",
		"input_cost_per_token": 3e-07, "output_cost_per_token": 2.5e-06,
		"input_cost_per_audio_token": 1e-06,
		"input_cost_per_audio_token_flex": 5e-07,
		"input_cost_per_audio_token_priority": 1.8e-06,
		"input_cost_per_token_flex": 1.5e-07, "input_cost_per_token_priority": 5.4e-07
	}`)
	usage := &schemas.BifrostLLMUsage{
		PromptTokens: 1000, TotalTokens: 1000,
		PromptTokensDetails: &schemas.ChatPromptTokensDetails{AudioTokens: 400},
	}
	assert.InDelta(t, 600*3e-07+400*1e-06, computeTextCostTotal(&p, usage, serviceTier{}), 1e-12)
	assert.InDelta(t, 600*1.5e-07+400*5e-07, computeTextCostTotal(&p, usage, serviceTier{isFlex: true}), 1e-12)
	assert.InDelta(t, 600*5.4e-07+400*1.8e-06, computeTextCostTotal(&p, usage, serviceTier{isPriority: true}), 1e-12)
	assert.InDelta(t, 1.8e-06, tieredAudioTokenInputRate(&p, 1000, serviceTier{isPriority: true}), 1e-15)
}

// gemini/gemini-2.5-flash-image publishes image output tokens at 3e-05 and 5.4e-05 on priority.
func TestComputeImageCost_PriorityImageOutputTokens(t *testing.T) {
	p := decodeEntry(t, `{
		"provider": "gemini", "mode": "image_generation",
		"input_cost_per_token": 3e-07, "output_cost_per_token": 2.5e-06,
		"output_cost_per_image_token": 3e-05,
		"output_cost_per_image_token_priority": 5.4e-05
	}`)
	usage := &schemas.ImageUsage{
		InputTokens: 100, OutputTokens: 1290, TotalTokens: 1390,
		OutputTokensDetails: &schemas.ImageTokenDetails{ImageTokens: 1290},
	}
	assert.InDelta(t, 100*3e-07+1290*3e-05, computeImageCostTotal(&p, usage, "", "", serviceTier{}), 1e-12)
	assert.InDelta(t, 100*3e-07+1290*5.4e-05, computeImageCostTotal(&p, usage, "", "", serviceTier{isPriority: true}), 1e-12)
}

// TXE-7372: the served tier rides on the image response, so CalculateCost prices a
// priority-served image at the priority image token rate. Before, the image response carried
// no tier and a priority image billed at the standard rate. Vertex gemini-3-pro-image publishes
// 1.2e-04 and 2.16e-04.
func TestCalculateCost_PriorityServedImageTakesThePriorityImageTokenRate(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gemini-3-pro-image", "vertex", "image_generation"): {
			Model: "gemini-3-pro-image", Provider: "vertex", Mode: "image_generation",
			InputCostPerToken:               bifrost.Ptr(2e-06),
			OutputCostPerImageToken:         bifrost.Ptr(1.2e-04),
			OutputCostPerImageTokenPriority: bifrost.Ptr(2.16e-04),
		},
	})
	usage := func() *schemas.ImageUsage {
		return &schemas.ImageUsage{
			InputTokens: 100, OutputTokens: 1120, TotalTokens: 1220,
			OutputTokensDetails: &schemas.ImageTokenDetails{ImageTokens: 1120},
		}
	}
	data := []schemas.ImageData{{B64JSON: "aW1n"}}

	standard := makeImageResponse(schemas.Vertex, "gemini-3-pro-image", usage())
	standard.ImageGenerationResponse.Data = data
	assert.InDelta(t, 100*2e-06+1120*1.2e-04, s.CalculateCost(standard, nil), 1e-12)

	priority := makeImageResponse(schemas.Vertex, "gemini-3-pro-image", usage())
	priority.ImageGenerationResponse.Data = data
	priority.ImageGenerationResponse.ServiceTier = bifrost.Ptr(schemas.BifrostServiceTierPriority)
	assert.InDelta(t, 100*2e-06+1120*2.16e-04, s.CalculateCost(priority, nil), 1e-12)

	// A tier with no image rate of its own falls back to the standard image token rate.
	flex := makeImageResponse(schemas.Vertex, "gemini-3-pro-image", usage())
	flex.ImageGenerationResponse.Data = data
	flex.ImageGenerationResponse.ServiceTier = bifrost.Ptr(schemas.BifrostServiceTierFlex)
	assert.InDelta(t, 100*2e-06+1120*1.2e-04, s.CalculateCost(flex, nil), 1e-12)
}

// gpt-6-luna publishes decision rates of 1e-07 input and 2e-07 above 272k, decision cache reads
// at 1e-08 (2e-08 above 272k) and decision cache writes at 1.25e-07 (2.5e-07 above 272k).
func TestComputeDecisionCost_Above272kAndCacheRates(t *testing.T) {
	p := decodeEntry(t, `{
		"provider": "openai", "mode": "chat",
		"input_cost_per_token": 1e-07, "output_cost_per_token": 5e-07,
		"input_cost_per_token_decisions": 1e-07,
		"output_cost_per_token_decisions": 0,
		"input_cost_per_token_above_272k_tokens_decisions": 2e-07,
		"output_cost_per_token_above_272k_tokens_decisions": 0,
		"cache_read_input_token_cost_decisions": 1e-08,
		"cache_read_input_token_cost_above_272k_tokens_decisions": 2e-08,
		"cache_creation_input_token_cost_decisions": 1.25e-07,
		"cache_creation_input_token_cost_above_272k_tokens_decisions": 2.5e-07
	}`)
	usage := func(prompt int) *schemas.BifrostLLMUsage {
		return &schemas.BifrostLLMUsage{
			PromptTokens: prompt, CompletionTokens: 40, TotalTokens: prompt + 40,
			PromptTokensDetails: &schemas.ChatPromptTokensDetails{CachedReadTokens: 6000, CachedWriteTokens: 1000},
		}
	}
	assert.InDelta(t, 3000*1e-07+6000*1e-08+1000*1.25e-07, computeDecisionCostTotal(&p, usage(10000), serviceTier{}), 1e-12)
	assert.InDelta(t, 293000*2e-07+6000*2e-08+1000*2.5e-07, computeDecisionCostTotal(&p, usage(300000), serviceTier{}), 1e-12)

	// A row with flat decision rates and no decision cache rates keeps pricing every prompt
	// token at the decision input rate.
	flat := decodeEntry(t, `{"provider": "openai", "mode": "chat", "input_cost_per_token": 1e-07, "input_cost_per_token_decisions": 1e-07, "output_cost_per_token_decisions": 0}`)
	assert.InDelta(t, 300000*1e-07, computeDecisionCostTotal(&flat, usage(300000), serviceTier{}), 1e-12)
}

// resolutionBandStore loads rows copied verbatim from the live upstream datasheet
// (getbifrost.ai/datasheet, fetched 14 Nov 2026) through the same Entry parse and row
// conversion a datasheet sync uses, so a band key the fork does not parse leaves the
// row without that band exactly as it would in production (TXE-7349).
func resolutionBandStore(t *testing.T) (*Store, map[string]configstoreTables.TableModelPricing) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "resolution-bands.json"))
	require.NoError(t, err)
	var entries map[string]Entry
	require.NoError(t, json.Unmarshal(data, &entries))
	rows := make(map[string]configstoreTables.TableModelPricing, len(entries))
	byKey := make(map[string]configstoreTables.TableModelPricing, len(entries))
	for modelKey, entry := range entries {
		row := convertEntryToTablePricing(modelKey, entry)
		rows[makeKey(row.Model, row.Provider, row.Mode)] = row
		byKey[modelKey] = row
	}
	return testStoreWithPricing(rows), byKey
}

// The three mispriced rows TXE-7349 names, each billed at its resolution band's rate.
// On main these bill Veo 3.1 4K at 0.4, Wan 3.0 480p at 0.165025 and MiniMax-H3 2K at 0.08.
func TestCalculateVideoCostDetails_UpstreamResolutionBands(t *testing.T) {
	store, rows := resolutionBandStore(t)
	for _, tc := range []struct {
		name       string
		modelKey   string
		seconds    int
		size       string
		resolution string
		extra      map[string]any
		wantRate   float64
	}{
		{name: "gemini veo 3.1 4k by tier", modelKey: "gemini/veo-3.1-generate-001", seconds: 8, resolution: "4k", wantRate: 0.6},
		{name: "vertex veo 3.1 4k by tier", modelKey: "vertex_ai/veo-3.1-generate-001", seconds: 8, resolution: "4k", wantRate: 0.6},
		{name: "veo 3.1 4k by size", modelKey: "gemini/veo-3.1-generate-001", seconds: 8, size: "3840x2160", wantRate: 0.6},
		// Veo reads a requested size for the aspect ratio only and renders the requested tier.
		{name: "veo 3.1 requested 4k size with resolution 1080p renders 1080p", modelKey: "gemini/veo-3.1-generate-001", seconds: 8, size: "3840x2160", resolution: "1080p", wantRate: 0.4},
		// A job row written before Resolution existed holds the tier only in Extra.
		{name: "veo 3.1 4k from extra on an older job row", modelKey: "gemini/veo-3.1-generate-001", seconds: 8, extra: map[string]any{"resolution": "4k"}, wantRate: 0.6},
		{name: "veo 3.1 1080p has no band, unbanded rate", modelKey: "gemini/veo-3.1-generate-001", seconds: 8, resolution: "1080p", wantRate: 0.4},
		{name: "wan 3.0 480p by tier", modelKey: "dashscope/wan3.0-video", seconds: 5, resolution: "480P", wantRate: 0.041256},
		{name: "wan 3.0 720p by tier", modelKey: "dashscope/wan3.0-video", seconds: 5, resolution: "720P", wantRate: 0.082513},
		{name: "wan 480p by legacy size", modelKey: "dashscope/wan3.0-video", seconds: 5, size: "832x480", wantRate: 0.041256},
		{name: "minimax H3 2K by tier", modelKey: "minimax/MiniMax-H3", seconds: 6, resolution: "2K", wantRate: 0.13},
		// miniMaxVideoSize reports a finished 2K task as 2560x1440.
		{name: "minimax H3 2K by reported size", modelKey: "minimax/MiniMax-H3", seconds: 6, size: "2560x1440", wantRate: 0.13},
		{name: "minimax H3 768P by tier", modelKey: "minimax/MiniMax-H3", seconds: 6, resolution: "768P", wantRate: 0.08},
		// Both families publish a 480p band here and disagree; the row's own source says
		// $0.36/s at 480p, which is the fork's video-specific key.
		{name: "seedance2 480p reads the video-specific band first", modelKey: "runwayml/seedance2", seconds: 5, resolution: "480p", wantRate: 0.36},
		{name: "seedance2 4k", modelKey: "runwayml/seedance2", seconds: 5, resolution: "4k", wantRate: 1.5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row, ok := rows[tc.modelKey]
			require.True(t, ok, "row %s missing from testdata", tc.modelKey)
			seconds := tc.seconds
			details := store.CalculateVideoCostDetails(VideoPricingDimensions{
				Model:       row.Model,
				RequestType: schemas.VideoGenerationRequest,
				Seconds:     &seconds,
				Size:        tc.size,
				Resolution:  tc.resolution,
				Extra:       tc.extra,
				OutputCount: 1,
			}, schemas.ModelProvider(row.Provider), nil)
			require.True(t, details.Priced)
			require.NotNil(t, details.Breakdown)
			want := float64(tc.seconds) * tc.wantRate
			assert.InDelta(t, want, details.Breakdown.OutputCost, 1e-9)
			// The total too: MiniMax-H3's input_cost_per_video_per_second is no longer billed on
			// the output duration (TXE-7351).
			assert.InDelta(t, want, details.Cost, 1e-9)
		})
	}
}

// MiniMax-H3, -Max and -Regeneration publish input_cost_per_video_per_second = 0.08 for
// reference-video input, which MiniMax rejects. A video job's seconds are the output clip's, so
// that rate must not be charged on them: the two rows TXE-7351 names bill at the upstream price
// (on main before the fix, 1.26 and 0.96).
func TestCalculateVideoCostDetails_InputVideoRateIsNotBilledOnOutputSeconds(t *testing.T) {
	store, rows := resolutionBandStore(t)
	row := rows["minimax/MiniMax-H3"]
	require.NotNil(t, row.InputCostPerVideoPerSecond, "the testdata row must carry the input-video rate")
	require.InDelta(t, 0.08, *row.InputCostPerVideoPerSecond, 1e-9)
	for _, tc := range []struct {
		resolution string
		want       float64
	}{
		{resolution: "2K", want: 0.78},
		{resolution: "768P", want: 0.48},
	} {
		t.Run(tc.resolution, func(t *testing.T) {
			seconds := 6
			details := store.CalculateVideoCostDetails(VideoPricingDimensions{
				Model: row.Model, RequestType: schemas.VideoGenerationRequest, Seconds: &seconds,
				Resolution: tc.resolution, OutputCount: 1,
			}, schemas.MiniMax, nil)
			require.True(t, details.Priced)
			assert.InDelta(t, tc.want, details.Cost, 1e-9)
			assert.Zero(t, details.Breakdown.InputCost)
		})
	}
}

// The per-request path prices from the same computeVideoCost. With no prompt tokens the input-video
// rate is charged on the seconds unless the job is exempt; prompt tokens still price input
// (TXE-7351).
func TestComputeVideoCost_InputVideoRateIgnoresOutputSeconds(t *testing.T) {
	p := configstoreTables.TableModelPricing{
		InputCostPerVideoPerSecond:                bifrost.Ptr(0.08),
		InputCostPerVideoPerSecondAbove128kTokens: bifrost.Ptr(0.16),
		OutputCostPerSecond:                       bifrost.Ptr(0.10),
	}
	seconds := 6
	cost := computeVideoCost(&p, nil, &seconds, "", 1, false, videoJob{inputVideoRateExempt: true}, serviceTier{})
	require.NotNil(t, cost)
	assert.Zero(t, cost.InputCost, "an exempt job owes no input-video rate")
	assert.InDelta(t, 0.60, cost.TotalCost, 1e-9)
	cost = computeVideoCost(&p, nil, &seconds, "", 1, false, videoJob{}, serviceTier{})
	require.NotNil(t, cost)
	assert.InDelta(t, 0.48, cost.InputCost, 1e-9)
	assert.InDelta(t, 1.08, cost.TotalCost, 1e-9)
	p.InputCostPerToken = bifrost.Ptr(0.001)
	cost = computeVideoCost(&p, &schemas.BifrostLLMUsage{PromptTokens: 10}, &seconds, "", 1, false, videoJob{inputVideoRateExempt: true}, serviceTier{})
	assert.InDelta(t, 0.01, cost.InputCost, 1e-9)
}

// Only a generation on a provider whose vendor takes no video input is exempt. Every other video job
// keeps the input-video charge on its seconds, so a governed key's budget still sees the input
// spend of a source video sent through any field, modelled or not (TXE-7351).
func TestVideoInputRateApplies(t *testing.T) {
	assert.False(t, videoInputRateApplies(schemas.VideoGenerationRequest, schemas.MiniMax))
	assert.False(t, videoInputRateApplies("", schemas.MiniMax), "an unset request type prices as a generation")
	assert.True(t, videoInputRateApplies(schemas.VideoEditRequest, schemas.MiniMax))
	assert.True(t, videoInputRateApplies(schemas.VideoRemixRequest, schemas.MiniMax))
	for _, provider := range []schemas.ModelProvider{schemas.Runway, schemas.BytePlus, schemas.DashScope, schemas.BFL, schemas.Runware, schemas.Gemini, "my-minimax"} {
		assert.True(t, videoInputRateApplies(schemas.VideoGenerationRequest, provider), provider)
	}
}

// Every provider exempted from the input-video rate must refuse video input in its converter, so the
// exemption can never cover a job that sent a source video upstream. A provider added to
// videoGenerationTakesNoVideoInput needs its converter here, or this fails (TXE-7351).
func TestVideoGenerationTakesNoVideoInput_ConvertersRefuseVideoInput(t *testing.T) {
	converters := map[schemas.ModelProvider]func(*schemas.BifrostVideoGenerationRequest) error{
		schemas.MiniMax: func(req *schemas.BifrostVideoGenerationRequest) error {
			_, err := minimax.ToMiniMaxVideoGenerationRequest(req)
			return err
		},
	}
	for provider := range videoGenerationTakesNoVideoInput {
		convert, ok := converters[provider]
		if !ok {
			t.Errorf("%s is exempt from the input-video rate, but this test does not check that its converter refuses video input", provider)
			continue
		}
		err := convert(&schemas.BifrostVideoGenerationRequest{
			Provider: provider, Model: "MiniMax-H3",
			Input: &schemas.VideoGenerationInput{Prompt: "p", VideoURI: bifrost.Ptr("https://example.invalid/src.mp4")},
		})
		assert.Error(t, err, "%s accepted video_uri, so its generations can carry a source video", provider)
	}
}

// On the per-request path and at settlement alike, a MiniMax generation owes no input-video rate
// and a generation on another provider still does; at settlement a custom provider takes its base
// provider's rule (TXE-7351).
func TestVideoCost_InputVideoRateByProvider(t *testing.T) {
	row := func(provider string) configstoreTables.TableModelPricing {
		return configstoreTables.TableModelPricing{
			Model: "m", Provider: provider, Mode: "video_generation",
			InputCostPerVideoPerSecond: bifrost.Ptr(0.05),
			OutputCostPerSecond:        bifrost.Ptr(0.10),
		}
	}
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("m", "minimax", "video_generation"):    row("minimax"),
		makeKey("m", "runway", "video_generation"):     row("runway"),
		makeKey("m", "my-minimax", "video_generation"): row("my-minimax"),
	})
	response := func(provider schemas.ModelProvider) *schemas.BifrostResponse {
		return &schemas.BifrostResponse{VideoGenerationResponse: &schemas.BifrostVideoGenerationResponse{
			Status: schemas.VideoStatusCompleted, Seconds: bifrost.Ptr("10"),
			Videos: []schemas.VideoOutput{{Type: schemas.VideoOutputTypeURL, URL: bifrost.Ptr("https://example.test/v.mp4")}},
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType: schemas.VideoGenerationRequest,
				RoutingInfo: routingInfoFor(provider, "m"),
			},
		}}
	}
	assert.InDelta(t, 1.00, s.CalculateCost(response(schemas.MiniMax), nil), 1e-9)
	assert.InDelta(t, 1.50, s.CalculateCost(response(schemas.Runway), nil), 1e-9)

	seconds := 10
	dims := func(base schemas.ModelProvider) VideoPricingDimensions {
		return VideoPricingDimensions{Model: "m", RequestType: schemas.VideoGenerationRequest, Seconds: &seconds, OutputCount: 1, BaseProvider: base}
	}
	assert.InDelta(t, 1.00, s.CalculateVideoCostDetails(dims(""), schemas.MiniMax, nil).Cost, 1e-9)
	assert.InDelta(t, 1.50, s.CalculateVideoCostDetails(dims(""), schemas.Runway, nil).Cost, 1e-9)
	assert.InDelta(t, 1.00, s.CalculateVideoCostDetails(dims(schemas.MiniMax), "my-minimax", nil).Cost, 1e-9)
	assert.InDelta(t, 1.50, s.CalculateVideoCostDetails(dims(""), "my-minimax", nil).Cost, 1e-9,
		"a custom provider whose base is unknown keeps the charge")
	// A reprice has no base provider; the exemption its settlement recorded keeps the price at 1.00.
	reprice := dims("")
	reprice.InputVideoRateExempt = true
	assert.InDelta(t, 1.00, s.CalculateVideoCostDetails(reprice, "my-minimax", nil).Cost, 1e-9)
	assert.True(t, dims(schemas.MiniMax).InputVideoRateExemptFor("my-minimax"))
	assert.False(t, dims("").InputVideoRateExemptFor("my-minimax"))
	edit := dims(schemas.MiniMax)
	edit.RequestType = schemas.VideoEditRequest
	assert.False(t, edit.InputVideoRateExemptFor("my-minimax"), "an edit carries a source video on any provider")
}

// Settlement passes the job's request type through: an edit job on a model with an input-video
// rate settles with the input charge, so a governed key's budget advances by it (TXE-7351).
func TestCalculateVideoCostDetails_EditKeepsTheInputVideoRate(t *testing.T) {
	store, rows := resolutionBandStore(t)
	row := rows["minimax/MiniMax-H3"]
	seconds := 6
	dims := func(requestType schemas.RequestType) VideoPricingDimensions {
		return VideoPricingDimensions{Model: row.Model, RequestType: requestType, Seconds: &seconds, Resolution: "2K", OutputCount: 1}
	}
	edit := store.CalculateVideoCostDetails(dims(schemas.VideoEditRequest), schemas.MiniMax, nil)
	generation := store.CalculateVideoCostDetails(dims(schemas.VideoGenerationRequest), schemas.MiniMax, nil)
	require.True(t, edit.Priced)
	require.True(t, generation.Priced)
	assert.InDelta(t, 0.78, generation.Cost, 1e-9)
	assert.InDelta(t, generation.Cost+6*0.08, edit.Cost, 1e-9)
}

// The band is read from a reported size first. When the request named both a size and a tier,
// the tier decides for a provider that renders the requested tier, and otherwise the dearer of
// the two bands does, so a cheaper tier the provider never reads cannot lower the price
// (TXE-7349).
func TestVideoPricingDimensions_BandSizePrecedence(t *testing.T) {
	p := configstoreTables.TableModelPricing{
		OutputCostPerSecond:      bifrost.Ptr(0.40),
		OutputCostPerSecond480p:  bifrost.Ptr(0.05),
		OutputCostPerSecond1080p: bifrost.Ptr(0.20),
		OutputCostPerSecond4k:    bifrost.Ptr(0.60),
	}
	band := func(d VideoPricingDimensions, provider schemas.ModelProvider) string {
		return d.bandSize(provider, &p, false)
	}

	// A reported size is what was rendered, whoever the provider.
	assert.Equal(t, "1920x1080", band(VideoPricingDimensions{Size: "1920x1080", SizeReported: true, Resolution: "4k"}, schemas.Gemini))
	// Only one of the two named: that one.
	assert.Equal(t, "1920x1080", band(VideoPricingDimensions{Size: "1920x1080"}, schemas.OpenAI))
	assert.Equal(t, "4k", band(VideoPricingDimensions{Size: "auto", Resolution: "4k"}, schemas.OpenAI))
	// A tier alone: one the provider reads sets the band; one it never reads cannot price the
	// job below what it would cost without it (the unbanded 0.40 here).
	assert.Equal(t, "2K", band(VideoPricingDimensions{Extra: map[string]any{"resolution": "2K"}}, schemas.MiniMax))
	assert.Equal(t, "480p", band(VideoPricingDimensions{Resolution: "480p"}, schemas.DashScope))
	assert.Equal(t, "", band(VideoPricingDimensions{Resolution: "480p"}, schemas.OpenAI))
	assert.Equal(t, "", band(VideoPricingDimensions{Extra: map[string]any{"resolution": "480p"}}, schemas.DashScope),
		"DashScope reads the typed tier, not extra_params")
	assert.Equal(t, "", band(VideoPricingDimensions{Resolution: "480p"}, schemas.MiniMax),
		"MiniMax reads extra_params, not the typed tier")
	assert.Equal(t, "4k", band(VideoPricingDimensions{Resolution: "4k"}, schemas.OpenAI), "an unread tier that costs more still bills")
	// Both named, a provider that renders the requested tier: the tier, cheaper or dearer.
	assert.Equal(t, "1080p", band(VideoPricingDimensions{Size: "3840x2160", Resolution: "1080p"}, schemas.Gemini))
	assert.Equal(t, "1080p", band(VideoPricingDimensions{Size: "3840x2160", Resolution: "1080p"}, schemas.Vertex))
	assert.Equal(t, "4k", band(VideoPricingDimensions{Size: "1920x1080", Resolution: "4k"}, schemas.MiniMax))
	// Both named, a provider that may ignore the tier: the dearer band. A 480p tier added to a
	// 1080p request does not buy the 480p rate.
	assert.Equal(t, "1920x1080", band(VideoPricingDimensions{Size: "1920x1080", Resolution: "480p"}, schemas.OpenAI))
	assert.Equal(t, "1920x1080", band(VideoPricingDimensions{Size: "1920x1080", Extra: map[string]any{"resolution": "480p"}}, schemas.Runway))
	assert.Equal(t, "4k", band(VideoPricingDimensions{Size: "854x480", Resolution: "4k"}, schemas.OpenAI))
	// A custom provider takes its base provider's rule; the custom name alone is not on the list.
	assert.Equal(t, "3840x2160", band(VideoPricingDimensions{Size: "3840x2160", Resolution: "1080p"}, "my-veo"))
	assert.Equal(t, "1080p", band(VideoPricingDimensions{Size: "3840x2160", Resolution: "1080p", BaseProvider: schemas.Gemini}, "my-veo"))

	assert.Equal(t, "", VideoPricingDimensions{Extra: map[string]any{"resolution": 1080}}.BandResolution(),
		"a numeric resolution (Runware 3D) names no band")

	// A reported size survives the merge over captured request dimensions, and the settlement
	// then records no tier, so a reprice reads the size.
	captured := VideoPricingDimensions{Size: "3840x2160", Resolution: "4k"}
	merged := captured.MergedWith(VideoDimensionsFromResponse(&schemas.BifrostVideoGenerationResponse{Size: "1280x720"}))
	assert.Equal(t, "1280x720", band(merged, schemas.Gemini))
	assert.Equal(t, "", merged.TierOnlyResolution(schemas.Gemini))
	assert.Equal(t, "4k", VideoPricingDimensions{Resolution: "4k"}.TierOnlyResolution(schemas.Gemini))
	assert.Equal(t, "", VideoPricingDimensions{Resolution: "4k"}.TierOnlyResolution(schemas.OpenAI), "a tier the provider never reads is not recorded")
	assert.Equal(t, "4k", captured.TierOnlyResolution(schemas.Gemini),
		"a read tier beside a requested size is what Veo renders, so an unpriced record keeps it")
	assert.Equal(t, "4k", VideoPricingDimensions{Size: "1280x720", Resolution: "4k"}.TierOnlyResolution(schemas.Gemini))
	assert.Equal(t, "", VideoPricingDimensions{Size: "1920x1080", Resolution: "480p"}.TierOnlyResolution(schemas.OpenAI),
		"with an unread tier beside a size, an unpriced record leaves the choice to the size")

	// A tier a settlement recorded prices the reprice as recorded, even where the request-source
	// rule would not trust it: the reprice hands MiniMax's tier back in the typed field, and has
	// no base provider for a custom DashScope.
	assert.Equal(t, "480p", band(VideoPricingDimensions{Resolution: "480p", ResolutionSettled: true}, schemas.MiniMax))
	assert.Equal(t, "480p", band(VideoPricingDimensions{Resolution: "480p", ResolutionSettled: true}, "my-wan"))
	assert.Equal(t, "", band(VideoPricingDimensions{Resolution: "480p"}, "my-wan"), "unsettled, the same tier is unread")
}

// The reviewed case end to end on a real store: a MiniMax override with a 768P band at 0.04/s and
// an unbanded 0.08/s settles at the band and reprices at it too (TXE-7349).
func TestCalculateVideoCostDetails_SettledTierRepricesAtItsBand(t *testing.T) {
	p := configstoreTables.TableModelPricing{
		OutputCostPerSecond:     bifrost.Ptr(0.08),
		OutputCostPerSecond768p: bifrost.Ptr(0.04),
	}
	seconds := 6
	settled := VideoPricingDimensions{Extra: map[string]any{"resolution": "768P"}, Seconds: &seconds}
	tier := settled.bandSize(schemas.MiniMax, &p, false)
	require.Equal(t, "768P", tier)
	reprice := VideoPricingDimensions{Resolution: tier, ResolutionSettled: true, Seconds: &seconds}
	assert.InDelta(t, 0.24, computeVideoCost(&p, nil, &seconds, reprice.bandSize(schemas.MiniMax, &p, false), 1, false, videoJob{}, serviceTier{}).TotalCost, 1e-9)
	unsettled := VideoPricingDimensions{Resolution: tier, Seconds: &seconds}
	assert.InDelta(t, 0.48, computeVideoCost(&p, nil, &seconds, unsettled.bandSize(schemas.MiniMax, &p, false), 1, false, videoJob{}, serviceTier{}).TotalCost, 1e-9,
		"without the settled flag the reprice would bill the unbanded rate")
}

// The details name the tier that set the band, so the settlement records it for a reprice.
func TestCalculateVideoCostDetails_ReportsTheTierThatSetTheBand(t *testing.T) {
	store, rows := resolutionBandStore(t)
	row := rows["gemini/veo-3.1-generate-001"]
	seconds := 8
	priced := func(d VideoPricingDimensions, provider schemas.ModelProvider) VideoCostDetails {
		d.Model, d.RequestType, d.Seconds, d.OutputCount = row.Model, schemas.VideoGenerationRequest, &seconds, 1
		return store.CalculateVideoCostDetails(d, provider, nil)
	}
	assert.Equal(t, "1080p", priced(VideoPricingDimensions{Size: "3840x2160", Resolution: "1080p"}, schemas.Gemini).PricedResolution)
	assert.Equal(t, "", priced(VideoPricingDimensions{Size: "3840x2160"}, schemas.Gemini).PricedResolution)
	assert.Equal(t, "", priced(VideoPricingDimensions{Size: "3840x2160", SizeReported: true, Resolution: "1080p"}, schemas.Gemini).PricedResolution)
}

func TestVideoResolutionBand_TierLabels(t *testing.T) {
	for label, want := range map[string]int{
		"480p": 480, "480P": 480, "720p": 720, "768P": 768, "1080p": 1080,
		"2K": 1440, "2k": 1440, "4k": 2160, "4K": 2160, " 4k ": 2160,
		"540p": 540, "360P": 360, "576p": 0, "auto": 0, "": 0,
	} {
		assert.Equal(t, want, videoResolutionBand(label), label)
	}
}

func TestComputeVideoCost_UpstreamBandKeys(t *testing.T) {
	p := configstoreTables.TableModelPricing{
		OutputCostPerSecond:      bifrost.Ptr(0.40),
		OutputCostPerSecond480p:  bifrost.Ptr(0.05),
		OutputCostPerSecond720p:  bifrost.Ptr(0.10),
		OutputCostPerSecond768p:  bifrost.Ptr(0.08),
		OutputCostPerSecond1080p: bifrost.Ptr(0.20),
		OutputCostPerSecond2k:    bifrost.Ptr(0.13),
		OutputCostPerSecond4k:    bifrost.Ptr(0.60),
	}
	seconds := 10
	for size, rate := range map[string]float64{
		"854x480": 0.05, "1280x720": 0.10, "1366x768": 0.08, "1920x1080": 0.20,
		"2560x1440": 0.13, "3840x2160": 0.60, "1792x1024": 0.40, "640x360": 0.40,
	} {
		assert.InDelta(t, 10*rate, computeVideoCostTotalSized(&p, nil, &seconds, size, 1, serviceTier{}), 1e-12, size)
	}
}

// BenchmarkVideoBandRate measures the per-second rate lookup every video settlement runs
// (TXE-7349), for a model with no bands, a banded size, and a size that names no band. The
// same cases run unchanged on main, which is the before figure in the PR.
func BenchmarkVideoBandRate(b *testing.B) {
	unbanded := configstoreTables.TableModelPricing{OutputCostPerSecond: bifrost.Ptr(0.40)}
	banded := configstoreTables.TableModelPricing{
		OutputCostPerSecond:              bifrost.Ptr(0.40),
		OutputCostPerVideoPerSecond1080p: bifrost.Ptr(0.70),
		OutputCostPerVideoPerSecond4k:    bifrost.Ptr(0.60),
	}
	for _, tc := range []struct {
		name    string
		pricing *configstoreTables.TableModelPricing
		size    string
	}{
		{"unbanded_model/1920x1080", &unbanded, "1920x1080"},
		{"unbanded_model/no_size", &unbanded, ""},
		{"banded_model/3840x2160", &banded, "3840x2160"},
		{"banded_model/640x360_no_band", &banded, "640x360"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if videoOutputPerSecondRate(tc.pricing, tc.size, false) <= 0 {
					b.Fatal("no rate")
				}
			}
		})
	}
	store := newVideoDimensionTestStore(&testing.T{})
	seconds := 8
	b.Run("settlement/sora-2-pro_1920x1080", func(b *testing.B) {
		b.ReportAllocs()
		dims := VideoPricingDimensions{Model: "sora-2-pro", RequestType: schemas.VideoGenerationRequest, Seconds: &seconds, Size: "1920x1080", OutputCount: 1}
		for b.Loop() {
			if !store.CalculateVideoCostDetails(dims, schemas.OpenAI, nil).Priced {
				b.Fatal("unpriced")
			}
		}
	})
}

// BenchmarkVideoBandRateTier measures the tier-label path TXE-7349 adds: a job that names
// "4k" or "2K" instead of a size, and an older job row holding the tier only in Extra.
func BenchmarkVideoBandRateTier(b *testing.B) {
	banded := configstoreTables.TableModelPricing{OutputCostPerSecond: bifrost.Ptr(0.40), OutputCostPerSecond4k: bifrost.Ptr(0.60), OutputCostPerSecond2k: bifrost.Ptr(0.13)}
	for label, want := range map[string]float64{"4k": 0.60, "2K": 0.13} {
		b.Run("rate/"+label, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if videoOutputPerSecondRate(&banded, label, false) != want {
					b.Fatal("band not selected")
				}
			}
		})
	}
	store := newVideoDimensionTestStore(&testing.T{})
	seconds := 8
	for _, tc := range []struct {
		name string
		dims VideoPricingDimensions
	}{
		{"settlement/resolution_4k", VideoPricingDimensions{Model: "sora-2-pro", RequestType: schemas.VideoGenerationRequest, Seconds: &seconds, Resolution: "4k", OutputCount: 1}},
		{"settlement/extra_resolution_4k", VideoPricingDimensions{Model: "sora-2-pro", RequestType: schemas.VideoGenerationRequest, Seconds: &seconds, Extra: map[string]any{"resolution": "4k"}, OutputCount: 1}},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if !store.CalculateVideoCostDetails(tc.dims, schemas.OpenAI, nil).Priced {
					b.Fatal("unpriced")
				}
			}
		})
	}
}

// BenchmarkAddProviderGuardrailCost is the pricing cost TXE-7365 adds: a request that reports
// guardrail units (the guardrails row read under the store's read lock, then eight terms), and the
// common request that reports none (one nil check).
func BenchmarkAddProviderGuardrailCost(b *testing.B) {
	var guardrailsEntry Entry
	if err := json.Unmarshal([]byte(upstreamBedrockGuardrailsEntry), &guardrailsEntry); err != nil {
		b.Fatal(err)
	}
	guardrails := convertEntryToTablePricing("bedrock/guardrails", guardrailsEntry)
	model := chatPricing(0.000003, 0.000015)
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{makeKey("guardrails", "bedrock", "guardrail"): guardrails})
	withUnits := &schemas.BifrostLLMUsage{GuardrailUnits: &schemas.GuardrailUnits{ContentPolicyUnits: 4, TopicPolicyUnits: 1, SensitiveInformationPolicyUnits: 1}}
	withoutUnits := &schemas.BifrostLLMUsage{PromptTokens: 100}
	b.Run("with_units", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_ = s.addProviderGuardrailCost(nil, &model, "bedrock", withUnits)
		}
	})
	b.Run("without_units", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_ = s.addProviderGuardrailCost(nil, &model, "bedrock", withoutUnits)
		}
	})
}

// =========================================================================
// TXE-7354: explicit batch rates. Values are the live datasheet's (14 Nov 2026).
// =========================================================================

// batchStore holds one decoded datasheet row under the given provider, model and mode.
func batchStore(t *testing.T, provider, model, mode, row string) *Store {
	t.Helper()
	p := decodeEntry(t, row)
	p.Model, p.Provider, p.Mode = model, provider, mode
	return testStoreWithPricing(map[string]configstoreTables.TableModelPricing{makeKey(model, provider, mode): p})
}

// azure/gpt-5.4 publishes its batch cache read at 1.3e-07. The fork derived 1.25e-07 from the
// synchronous rate and the batch ratio.
func TestBatchCost_ExplicitCacheReadBatchRate(t *testing.T) {
	s := batchStore(t, "azure", "gpt-5.4", "chat", `{
		"provider": "azure", "mode": "chat",
		"input_cost_per_token": 2.5e-06, "output_cost_per_token": 1.5e-05,
		"input_cost_per_token_batches": 1.25e-06, "output_cost_per_token_batches": 7.5e-06,
		"cache_read_input_token_cost": 2.5e-07, "cache_read_input_token_cost_batches": 1.3e-07
	}`)
	usage := &schemas.BifrostLLMUsage{PromptTokens: 10000, CompletionTokens: 100, TotalTokens: 10100,
		PromptTokensDetails: &schemas.ChatPromptTokensDetails{CachedReadTokens: 6000}}
	got := s.CalculateBatchCostDetailsForUsage(usage, schemas.Azure, "gpt-5.4", schemas.ChatCompletionRequest, nil)
	require.True(t, got.Priced)
	assert.InDelta(t, 4000*1.25e-06+6000*1.3e-07+100*7.5e-06, got.Cost, 1e-12)
}

// gpt-6-sol publishes its 272k batch rates under both spellings; a row with only the second
// (input_cost_per_token_batches_above_272k_tokens) is read too. Batch 12h writes take the
// explicit batch 12h rate.
func TestBatchCost_ExplicitTierAnd12hBatchRates(t *testing.T) {
	s := batchStore(t, "openai", "gpt-6-sol", "chat", `{
		"provider": "openai", "mode": "chat",
		"input_cost_per_token": 2e-06, "output_cost_per_token": 1e-05,
		"input_cost_per_token_batches": 1e-06, "output_cost_per_token_batches": 5e-06,
		"input_cost_per_token_above_272k_tokens": 4e-06, "output_cost_per_token_above_272k_tokens": 1.5e-05,
		"input_cost_per_token_batches_above_272k_tokens": 1.9e-06,
		"output_cost_per_token_above_272k_tokens_batches": 7e-06,
		"cache_creation_input_token_cost": 2.5e-06,
		"cache_creation_input_token_cost_above_12hr": 5e-06,
		"cache_creation_input_token_cost_above_12hr_batches": 2.4e-06,
		"cache_creation_input_token_cost_above_12hr_above_272k_tokens_batches": 4.9e-06
	}`)
	usage := func(prompt int) *schemas.BifrostLLMUsage {
		return &schemas.BifrostLLMUsage{PromptTokens: prompt, CompletionTokens: 100, TotalTokens: prompt + 100,
			PromptTokensDetails: &schemas.ChatPromptTokensDetails{CachedWriteTokens: 2000,
				CachedWriteTokenDetails: &schemas.ChatCachedWriteTokenDetails{CachedWriteTokens12h: 2000}}}
	}
	short := s.CalculateBatchCostDetailsForUsage(usage(10000), schemas.OpenAI, "gpt-6-sol", schemas.ChatCompletionRequest, nil)
	require.True(t, short.Priced)
	assert.InDelta(t, 8000*1e-06+2000*2.4e-06+100*5e-06, short.Cost, 1e-12)
	long := s.CalculateBatchCostDetailsForUsage(usage(300000), schemas.OpenAI, "gpt-6-sol", schemas.ChatCompletionRequest, nil)
	require.True(t, long.Priced)
	assert.InDelta(t, 298000*1.9e-06+2000*4.9e-06+100*7e-06, long.Cost, 1e-9)
}

// gemini-2.5-flash publishes batch audio input at 5e-07 against batch text input 1.5e-07. Without
// an explicit audio batch rate, audio takes the synchronous audio rate at the batch ratio.
func TestBatchCost_AudioInputTokens(t *testing.T) {
	row := `{
		"provider": "vertex_ai", "mode": "chat",
		"input_cost_per_token": 3e-07, "output_cost_per_token": 2.5e-06,
		"input_cost_per_token_batches": 1.5e-07, "output_cost_per_token_batches": 1.25e-06,
		"input_cost_per_audio_token": 1e-06%s
	}`
	usage := &schemas.BifrostLLMUsage{PromptTokens: 1000, TotalTokens: 1000,
		PromptTokensDetails: &schemas.ChatPromptTokensDetails{AudioTokens: 400}}
	explicit := batchStore(t, "vertex", "gemini-2.5-flash", "chat", fmt.Sprintf(row, `, "input_cost_per_audio_token_batches": 5e-07`))
	got := explicit.CalculateBatchCostDetailsForUsage(usage, schemas.Vertex, "gemini-2.5-flash", schemas.ChatCompletionRequest, nil)
	require.True(t, got.Priced)
	assert.InDelta(t, 600*1.5e-07+400*5e-07, got.Cost, 1e-12)

	derived := batchStore(t, "vertex", "gemini-2.5-flash", "chat", fmt.Sprintf(row, ""))
	got = derived.CalculateBatchCostDetailsForUsage(usage, schemas.Vertex, "gemini-2.5-flash", schemas.ChatCompletionRequest, nil)
	assert.InDelta(t, 600*1.5e-07+400*1e-06*0.5, got.Cost, 1e-12)
}

// gemini/gemini-2.5-flash-image publishes batch image output tokens at 1.5e-05. An image line was
// left unpriced (the job parked) for want of an image batch rate; it is now priced, and a chat
// line's image tokens stay at the text rate, as the synchronous chat path prices them.
func TestBatchCost_ImageLinesTakeTheImageBatchRates(t *testing.T) {
	row := `{
		"provider": "gemini", "mode": "image_generation",
		"input_cost_per_token": 3e-07, "output_cost_per_token": 2.5e-06,
		"input_cost_per_token_batches": 1.5e-07, "output_cost_per_token_batches": 1.25e-06,
		"output_cost_per_image_token": 3e-05, "output_cost_per_image_token_batches": 1.5e-05
	}`
	s := batchStore(t, "gemini", "gemini-2.5-flash-image", "image_generation", row)
	usage := &schemas.BifrostLLMUsage{PromptTokens: 100, CompletionTokens: 1300, TotalTokens: 1400,
		CompletionTokensDetails: &schemas.ChatCompletionTokensDetails{TextTokens: 10, ImageTokens: schemas.Ptr(1290)}}
	got := s.CalculateBatchCostDetailsForUsage(usage, schemas.Gemini, "gemini-2.5-flash-image", schemas.ImageGenerationRequest, nil)
	require.True(t, got.Priced)
	assert.InDelta(t, 100*1.5e-07+10*1.25e-06+1290*1.5e-05, got.Cost, 1e-12)

	// A result with no modality breakdown (usageMetadata without candidatesTokensDetails) prices
	// its aggregate output tokens as image tokens, as the synchronous image path does; reasoning
	// tokens alone are no breakdown. A breakdown with text and no image tokens prices them as text.
	for _, tc := range []struct {
		name    string
		details *schemas.ChatCompletionTokensDetails
		want    float64
	}{
		{"no details", nil, 100*1.5e-07 + 1300*1.5e-05},
		{"reasoning only", &schemas.ChatCompletionTokensDetails{ReasoningTokens: 200}, 100*1.5e-07 + 1300*1.5e-05},
		{"text only", &schemas.ChatCompletionTokensDetails{TextTokens: 1300}, 100*1.5e-07 + 1300*1.25e-06},
	} {
		aggregate := &schemas.BifrostLLMUsage{PromptTokens: 100, CompletionTokens: 1300, TotalTokens: 1400, CompletionTokensDetails: tc.details}
		got := s.CalculateBatchCostDetailsForUsage(aggregate, schemas.Gemini, "gemini-2.5-flash-image", schemas.ImageGenerationRequest, nil)
		require.True(t, got.Priced, tc.name)
		assert.InDelta(t, tc.want, got.Cost, 1e-12, tc.name)
	}

	// A batch of mixed operations reaches pricing under jobaccounting's mixed request type,
	// which no pricing path handles: unpriced, never priced as image or chat lines.
	assert.False(t, s.CalculateBatchCostDetailsForUsage(usage, schemas.Gemini, "gemini-2.5-flash-image", schemas.RequestType("batch_results_mixed"), nil).Priced)

	// A row priced per image has no token rate for the image tokens: still unpriced.
	perImage := batchStore(t, "openai", "dall-e-3", "image_generation", `{"provider": "openai", "mode": "image_generation", "output_cost_per_image": 0.04}`)
	assert.False(t, perImage.CalculateBatchCostDetailsForUsage(usage, schemas.OpenAI, "dall-e-3", schemas.ImageGenerationRequest, nil).Priced)

	chat := computeBatchTextCost(func() *configstoreTables.TableModelPricing { p := decodeEntry(t, row); return &p }(), usage, serviceTier{})
	require.NotNil(t, chat)
	assert.InDelta(t, 100*1.5e-07+1300*1.25e-06, chat.TotalCost, 1e-12)
}

// ===========================================================================
// TXE-7357: video tokens, per-clip prices, Kling's mode, Seedance's video-input rate and
// Vertex's embedding interval tiers. The rows are the live upstream datasheet's
// (getbifrost.ai/datasheet, fetched 2026-11-14), run through the production JSON -> row
// conversion, so a missing json tag fails here too.
// ===========================================================================

const (
	minimaxHailuo23Row = `{"provider":"minimax","mode":"video_generation","output_cost_per_video_768p_6s":0.28,
		"output_cost_per_video_768p_10s":0.56,"output_cost_per_video_1080p_6s":0.49}`
	replicateHailuo23Row = `{"provider":"replicate","mode":"video_generation","output_cost_per_video":0.28,
		"output_cost_per_video_768p_6s":0.28,"output_cost_per_video_768p_10s":0.56,"output_cost_per_video_1080p_6s":0.49}`
	replicateKlingV3Row = `{"provider":"replicate","mode":"video_generation","output_cost_per_video_per_second":0.168,
		"output_cost_per_video_per_second_standard":0.168,"output_cost_per_video_per_second_standard_audio":0.252,
		"output_cost_per_video_per_second_pro":0.224,"output_cost_per_video_per_second_pro_audio":0.336,
		"output_cost_per_video_per_second_4k":0.42,"output_cost_per_video_per_second_4k_audio":0.42}`
	replicateSeedance20Row = `{"provider":"replicate","mode":"video_generation","output_cost_per_video_per_second":0.08,
		"output_cost_per_video_per_second_480p":0.08,"output_cost_per_video_per_second_480p_video_in":0.1,
		"output_cost_per_video_per_second_720p":0.18,"output_cost_per_video_per_second_720p_video_in":0.22,
		"output_cost_per_video_per_second_1080p":0.45,"output_cost_per_video_per_second_1080p_video_in":0.55,
		"output_cost_per_video_per_second_4k":1,"output_cost_per_video_per_second_4k_video_in":1.25}`
	byteplusSeedance20Row = `{"provider":"byteplus","mode":"video_generation","output_cost_per_video_token":0.000007}`
	geminiOmniFlashRow    = `{"provider":"gemini","mode":"video_generation","input_cost_per_token":0.0000015,
		"output_cost_per_token":0.000009,"output_cost_per_video_token":0.0000175,"output_cost_per_video_per_second":0.10136}`
	geminiEmbedding2Row = `{"provider":"gemini","mode":"embedding","input_cost_per_token":2E-7,"input_cost_per_token_batches":1E-7,
		"input_cost_per_video_token":0.000012,"input_cost_per_video_token_batches":0.000006,"output_cost_per_token":0}`
	geminiFlashLiveRow = `{"provider":"gemini","mode":"realtime","input_cost_per_token":7.5E-7,"input_cost_per_audio_token":0.000003,
		"input_cost_per_video_token":0.000001,"output_cost_per_token":0.0000045}`
	vertexMultimodalEmbeddingRow = `{"provider":"vertex_ai","mode":"embedding","input_cost_per_video_per_second":0.0005,
		"input_cost_per_video_per_second_above_15s_interval":0.002,"input_cost_per_video_per_second_above_8s_interval":0.001,
		"input_cost_per_token":8E-7}`
)

// txe7357Store holds the TXE-7357 rows under the model names their providers route.
func txe7357Store(t testing.TB) *Store {
	t.Helper()
	rows := map[string]string{
		"minimax/MiniMax-Hailuo-2.3":            minimaxHailuo23Row,
		"replicate/minimax/hailuo-2.3":          replicateHailuo23Row,
		"replicate/kwaivgi/kling-v3-video":      replicateKlingV3Row,
		"replicate/bytedance/seedance-2.0":      replicateSeedance20Row,
		"byteplus/dreamina-seedance-2-0-260128": byteplusSeedance20Row,
		"gemini/gemini-omni-flash-preview":      geminiOmniFlashRow,
		"vertex_ai/multimodalembedding@001":     vertexMultimodalEmbeddingRow,
	}
	pricing := map[string]configstoreTables.TableModelPricing{}
	for key, blob := range rows {
		var entry Entry
		if err := json.Unmarshal([]byte(blob), &entry); err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		row := convertEntryToTablePricing(key, entry)
		pricing[makeKey(row.Model, row.Provider, row.Mode)] = row
	}
	return testStoreWithPricing(pricing)
}

func TestCalculateVideoCostDetails_TXE7357(t *testing.T) {
	s := txe7357Store(t)
	off := false
	on := true
	tokens := 108900
	tests := []struct {
		name     string
		provider schemas.ModelProvider
		dims     VideoPricingDimensions
		expected float64 // 0: unpriced
	}{
		// MiniMax Hailuo 2.3 publishes only per-clip prices: before TXE-7357 every job was $0.
		{"hailuo 768p 6s", schemas.MiniMax, VideoPricingDimensions{Model: "MiniMax-Hailuo-2.3", Seconds: bifrost.Ptr(6), Size: "1366x768"}, 0.28},
		{"hailuo 768p 10s", schemas.MiniMax, VideoPricingDimensions{Model: "MiniMax-Hailuo-2.3", Seconds: bifrost.Ptr(10), Size: "1366x768"}, 0.56},
		{"hailuo 1080p 6s", schemas.MiniMax, VideoPricingDimensions{Model: "MiniMax-Hailuo-2.3", Seconds: bifrost.Ptr(6), Size: "1920x1080"}, 0.49},
		{"hailuo portrait 768p 6s", schemas.MiniMax, VideoPricingDimensions{Model: "MiniMax-Hailuo-2.3", Seconds: bifrost.Ptr(6), Size: "768x1366"}, 0.28},
		{"hailuo two clips", schemas.MiniMax, VideoPricingDimensions{Model: "MiniMax-Hailuo-2.3", Seconds: bifrost.Ptr(6), Size: "1366x768", OutputCount: 2}, 0.56},
		// 1080p 10s is not a clip MiniMax publishes, and there is no per-second rate: unpriced.
		{"hailuo 1080p 10s unpriced", schemas.MiniMax, VideoPricingDimensions{Model: "MiniMax-Hailuo-2.3", Seconds: bifrost.Ptr(10), Size: "1920x1080"}, 0},
		// Replicate's Hailuo has a flat per-clip price too, which needs no length.
		{"replicate hailuo no length", schemas.Replicate, VideoPricingDimensions{Model: "minimax/hailuo-2.3"}, 0.28},
		{"replicate hailuo 768p 10s", schemas.Replicate, VideoPricingDimensions{Model: "minimax/hailuo-2.3", Seconds: bifrost.Ptr(10), Size: "1366x768"}, 0.56},
		// A tier named only as resolution (Replicate forwards it as an input) with no size is
		// priced at its clip: Hailuo publishes no per-second rate to compare tiers by.
		{"replicate hailuo resolution 768p 10s", schemas.Replicate, VideoPricingDimensions{Model: "minimax/hailuo-2.3", Seconds: bifrost.Ptr(10), Extra: map[string]any{"resolution": "768p"}}, 0.56},
		{"replicate hailuo resolution 1080p 6s", schemas.Replicate, VideoPricingDimensions{Model: "minimax/hailuo-2.3", Seconds: bifrost.Ptr(6), Extra: map[string]any{"resolution": "1080p"}}, 0.49},
		{"replicate hailuo unified resolution 1080p 6s", schemas.Replicate, VideoPricingDimensions{Model: "minimax/hailuo-2.3", Seconds: bifrost.Ptr(6), Resolution: "1080p"}, 0.49},
		// Kling v3 on Replicate: pro is $0.224/s upstream; the fork billed every mode at $0.168/s.
		{"kling pro silent", schemas.Replicate, VideoPricingDimensions{Model: "kwaivgi/kling-v3-video", Seconds: bifrost.Ptr(5), Audio: &off, Extra: map[string]any{"mode": "pro"}}, 5 * 0.224},
		{"kling pro with audio", schemas.Replicate, VideoPricingDimensions{Model: "kwaivgi/kling-v3-video", Seconds: bifrost.Ptr(5), Audio: &on, Extra: map[string]any{"mode": "pro"}}, 5 * 0.336},
		{"kling standard silent", schemas.Replicate, VideoPricingDimensions{Model: "kwaivgi/kling-v3-video", Seconds: bifrost.Ptr(5), Audio: &off, Extra: map[string]any{"mode": "standard"}}, 5 * 0.168},
		{"kling std with audio", schemas.Replicate, VideoPricingDimensions{Model: "kwaivgi/kling-v3-video", Seconds: bifrost.Ptr(5), Audio: &on, Mode: "std"}, 5 * 0.252},
		// Replicate's audio default is on (VideoAudioDefaults), so an unstated flag takes pro's audio rate.
		{"kling pro audio unstated", schemas.Replicate, VideoPricingDimensions{Model: "kwaivgi/kling-v3-video", Seconds: bifrost.Ptr(5), Extra: map[string]any{"mode": "pro"}}, 5 * 0.336},
		{"kling no mode", schemas.Replicate, VideoPricingDimensions{Model: "kwaivgi/kling-v3-video", Seconds: bifrost.Ptr(5), Audio: &off}, 5 * 0.168},
		// A mode Bifrost does not recognize fails closed, at the dearer mode rate.
		{"kling unknown mode", schemas.Replicate, VideoPricingDimensions{Model: "kwaivgi/kling-v3-video", Seconds: bifrost.Ptr(5), Audio: &off, Mode: "master"}, 5 * 0.224},
		// The band is the rendered resolution and wins over the mode.
		{"kling pro 4k", schemas.Replicate, VideoPricingDimensions{Model: "kwaivgi/kling-v3-video", Seconds: bifrost.Ptr(5), Audio: &off, Size: "3840x2160", Extra: map[string]any{"mode": "pro"}}, 5 * 0.42},
		// Seedance 2.0 720p: $0.22/s with a video input upstream, $0.18/s without.
		{"seedance 720p video in", schemas.Replicate, VideoPricingDimensions{Model: "bytedance/seedance-2.0", Seconds: bifrost.Ptr(5), Size: "1280x720", VideoInput: &on}, 5 * 0.22},
		{"seedance 720p no video in", schemas.Replicate, VideoPricingDimensions{Model: "bytedance/seedance-2.0", Seconds: bifrost.Ptr(5), Size: "1280x720"}, 5 * 0.18},
		{"seedance 1080p video in", schemas.Replicate, VideoPricingDimensions{Model: "bytedance/seedance-2.0", Seconds: bifrost.Ptr(4), Size: "1920x1080", VideoInput: &on}, 4 * 0.55},
		{"seedance 4k video in", schemas.Replicate, VideoPricingDimensions{Model: "bytedance/seedance-2.0", Seconds: bifrost.Ptr(4), Size: "3840x2160", VideoInput: &on}, 4 * 1.25},
		// A create naming its tier only as resolution (Replicate forwards it as an input) and no
		// size settles at that tier's video-input rate, not the $0.08/s unbanded one.
		{"seedance 4k resolution only video in", schemas.Replicate, VideoPricingDimensions{Model: "bytedance/seedance-2.0", Seconds: bifrost.Ptr(5), Extra: map[string]any{"resolution": "4k"}, VideoInput: &on}, 5 * 1.25},
		{"seedance 4k unified resolution video in", schemas.Replicate, VideoPricingDimensions{Model: "bytedance/seedance-2.0", Seconds: bifrost.Ptr(5), Resolution: "4k", VideoInput: &on}, 5 * 1.25},
		// BytePlus Seedance bills per output video token and reports them: before TXE-7357 $0.
		{"byteplus seedance tokens", schemas.BytePlus, VideoPricingDimensions{Model: "dreamina-seedance-2-0-260128", OutputTokens: &tokens, Seconds: bifrost.Ptr(5)}, 108900 * 0.000007},
		// Gemini Omni reports its video tokens: priced by them rather than by the per-second rate.
		{"gemini omni video tokens", schemas.Gemini, VideoPricingDimensions{Model: "gemini-omni-flash-preview", OutputTokens: bifrost.Ptr(5000), Seconds: bifrost.Ptr(5)}, 5000 * 0.0000175},
		{"gemini omni no tokens", schemas.Gemini, VideoPricingDimensions{Model: "gemini-omni-flash-preview", Seconds: bifrost.Ptr(5)}, 5 * 0.10136},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.dims.RequestType = schemas.VideoGenerationRequest
			details := s.CalculateVideoCostDetails(tc.dims, tc.provider, nil)
			if tc.expected == 0 {
				assert.False(t, details.Priced, "cost %v", details.Cost)
				return
			}
			require.True(t, details.Priced)
			assert.InDelta(t, tc.expected, details.Cost, 1e-9)
		})
	}
}

// The per-request path prices a completed video response the same way, from what the response
// reports: MiniMax's rendered size and seconds, and BytePlus's output video tokens.
func TestCalculateCost_VideoResponse_TXE7357(t *testing.T) {
	s := txe7357Store(t)
	video := func(provider schemas.ModelProvider, model string, seconds, size string, usage *schemas.VideoUsage) *schemas.BifrostResponse {
		resp := &schemas.BifrostVideoGenerationResponse{
			Status: schemas.VideoStatusCompleted, Size: size, Usage: usage,
			Videos:      []schemas.VideoOutput{{ContentType: "video/mp4"}},
			ExtraFields: schemas.BifrostResponseExtraFields{RequestType: schemas.VideoRetrieveRequest, PricingRequestType: schemas.VideoGenerationRequest, RoutingInfo: routingInfoFor(provider, model)},
		}
		if seconds != "" {
			resp.Seconds = &seconds
		}
		return &schemas.BifrostResponse{VideoGenerationResponse: resp}
	}
	assert.InDelta(t, 0.28, s.CalculateCost(video(schemas.MiniMax, "MiniMax-Hailuo-2.3", "6", "1366x768", nil), nil), 1e-9)
	assert.InDelta(t, 108900*0.000007, s.CalculateCost(video(schemas.BytePlus, "dreamina-seedance-2-0-260128", "", "", &schemas.VideoUsage{OutputTokens: bifrost.Ptr(108900)}), nil), 1e-9)
	assert.InDelta(t, 0.28, s.CalculateCost(video(schemas.Replicate, "minimax/hailuo-2.3", "", "", nil), nil), 1e-9)
}

// Input video tokens are priced at the video token rate, in chat, embedding and batch pricing.
func TestInputVideoTokens_TXE7357(t *testing.T) {
	live := pricingRowFromDatasheetJSON(t, "gemini/gemini-3.1-flash-live-preview", geminiFlashLiveRow)
	usage := &schemas.BifrostLLMUsage{PromptTokens: 1000, CompletionTokens: 100, TotalTokens: 1100,
		PromptTokensDetails: &schemas.ChatPromptTokensDetails{AudioTokens: 200, VideoTokens: 500}}
	text := computeTextCost(&live, usage, serviceTier{})
	require.NotNil(t, text)
	// 300 text tokens at the text rate, 200 audio at the audio rate, 500 video at the video rate.
	assert.InDelta(t, 300*7.5e-7+200*0.000003+500*0.000001, text.InputCost, 1e-12)
	assert.InDelta(t, text.InputCost, text.InputCostDetails.TextCost+text.InputCostDetails.AudioCost, 1e-12, "the breakdown still sums to the input side")

	emb := pricingRowFromDatasheetJSON(t, "gemini/gemini-embedding-2", geminiEmbedding2Row)
	embUsage := &schemas.BifrostLLMUsage{PromptTokens: 1000, TotalTokens: 1000, PromptTokensDetails: &schemas.ChatPromptTokensDetails{VideoTokens: 600}}
	cost := computeEmbeddingCost(&emb, embUsage, serviceTier{})
	require.NotNil(t, cost)
	assert.InDelta(t, 400*2e-7+600*0.000012, cost.TotalCost, 1e-12)

	batch := computeBatchTextCost(&emb, embUsage, serviceTier{})
	require.NotNil(t, batch)
	assert.InDelta(t, 400*1e-7+600*0.000006, batch.TotalCost, 1e-12)
	// In a batch too, cached video is not moved to the video batch rate: with 400 of the 600 video
	// tokens cached, only 200 fresh video tokens are.
	cachedBatch := &schemas.BifrostLLMUsage{PromptTokens: 1000, TotalTokens: 1000,
		PromptTokensDetails: &schemas.ChatPromptTokensDetails{VideoTokens: 600, CachedVideoTokens: 400, CachedReadTokens: 400}}
	noCachedVideo := &schemas.BifrostLLMUsage{PromptTokens: 1000, TotalTokens: 1000,
		PromptTokensDetails: &schemas.ChatPromptTokensDetails{VideoTokens: 200, CachedReadTokens: 400}}
	assert.InDelta(t, computeBatchTextCost(&emb, noCachedVideo, serviceTier{}).TotalCost, computeBatchTextCost(&emb, cachedBatch, serviceTier{}).TotalCost, 1e-12)

	// A vendor reporting more video tokens than the prompt has cannot price more than the prompt.
	over := &schemas.BifrostLLMUsage{PromptTokens: 100, TotalTokens: 100, PromptTokensDetails: &schemas.ChatPromptTokensDetails{VideoTokens: 900}}
	assert.InDelta(t, 100*0.000012, computeEmbeddingCost(&emb, over, serviceTier{}).TotalCost, 1e-12)

	// Cached video is in the video count but billed at the cache-read rate: 1,500 prompt tokens,
	// 1,000 of them cached video, 500 fresh text, take no video adjustment at all. With 1,200
	// video of which 1,000 cached, only the 200 fresh video tokens take the video rate.
	cachedUsage := func(video, cachedVideo int) *schemas.BifrostLLMUsage {
		return &schemas.BifrostLLMUsage{PromptTokens: 1500, CompletionTokens: 100, TotalTokens: 1600,
			PromptTokensDetails: &schemas.ChatPromptTokensDetails{CachedReadTokens: 1000, VideoTokens: video, CachedVideoTokens: cachedVideo}}
	}
	noVideo := computeTextCost(&live, cachedUsage(0, 0), serviceTier{}).TotalCost
	assert.InDelta(t, noVideo, computeTextCost(&live, cachedUsage(1000, 1000), serviceTier{}).TotalCost, 1e-12, "fresh text was priced as video")
	assert.InDelta(t, noVideo+200*(0.000001-7.5e-7), computeTextCost(&live, cachedUsage(1200, 1000), serviceTier{}).TotalCost, 1e-12)
}

// /v1/responses reports video tokens on input_tokens_details; both the ordinary and the streamed
// response keep them through normalization, so they take the video rate as on chat.
func TestResponsesUsageKeepsVideoTokens_TXE7357(t *testing.T) {
	live := pricingRowFromDatasheetJSON(t, "gemini/gemini-3.1-flash-live-preview", geminiFlashLiveRow)
	usage := &schemas.ResponsesResponseUsage{InputTokens: 1000, OutputTokens: 100, TotalTokens: 1100,
		InputTokensDetails: &schemas.ResponsesResponseInputTokens{TextTokens: 500, VideoTokens: 500}}
	want := 500*7.5e-7 + 500*0.000001 + 100*0.0000045
	for name, resp := range map[string]*schemas.BifrostResponse{
		"responses":        {ResponsesResponse: &schemas.BifrostResponsesResponse{Usage: usage}},
		"responses stream": {ResponsesStreamResponse: &schemas.BifrostResponsesStreamResponse{Response: &schemas.BifrostResponsesResponse{Usage: usage}}},
	} {
		t.Run(name, func(t *testing.T) {
			input := extractCostInput(resp)
			require.NotNil(t, input.usage)
			require.NotNil(t, input.usage.PromptTokensDetails)
			assert.Equal(t, 500, input.usage.PromptTokensDetails.VideoTokens)
			cost := computeTextCost(&live, input.usage, serviceTier{})
			require.NotNil(t, cost)
			assert.InDelta(t, want, cost.TotalCost, 1e-12)
		})
	}
}

// Vertex multimodalembedding prices a second of embedded video by the embedding interval.
func TestCalculateCost_EmbeddingVideoIntervals_TXE7357(t *testing.T) {
	s := txe7357Store(t)
	embedding := func(segments ...[2]int) *schemas.BifrostResponse {
		data := make([]schemas.EmbeddingData, 0, len(segments))
		for i, seg := range segments {
			data = append(data, schemas.EmbeddingData{Index: i, Modality: schemas.EmbeddingModalityVideo,
				VideoSegment: &schemas.EmbeddingVideoSegment{StartOffsetSec: seg[0], EndOffsetSec: seg[1]}})
		}
		return &schemas.BifrostResponse{EmbeddingResponse: &schemas.BifrostEmbeddingResponse{Data: data,
			ExtraFields: schemas.BifrostResponseExtraFields{RequestType: schemas.EmbeddingRequest, RoutingInfo: routingInfoFor(schemas.Vertex, "multimodalembedding@001")}}}
	}
	// Vertex's default interval is 16 seconds: above 15, so $0.002/s for 40 seconds of video.
	assert.InDelta(t, 40*0.002, s.CalculateCost(embedding([2]int{0, 16}, [2]int{16, 32}, [2]int{32, 40}), nil), 1e-12)
	assert.InDelta(t, 30*0.001, s.CalculateCost(embedding([2]int{0, 10}, [2]int{10, 20}, [2]int{20, 30}), nil), 1e-12)
	assert.InDelta(t, 12*0.0005, s.CalculateCost(embedding([2]int{0, 4}, [2]int{4, 8}, [2]int{8, 12}), nil), 1e-12)
	// A video shorter than the interval returns one segment as long as the video: the interval
	// the request named prices it, else Vertex's default of 16 seconds, never the segment's length.
	assert.InDelta(t, 6*0.002, s.CalculateCost(embedding([2]int{0, 6}), nil), 1e-12, "default interval")
	short := embedding([2]int{0, 6})
	short.EmbeddingResponse.VideoIntervalSec = 8
	assert.InDelta(t, 6*0.0005, s.CalculateCost(short, nil), 1e-12, "interval 8 named by the request")
	short.EmbeddingResponse.VideoIntervalSec = 10
	assert.InDelta(t, 6*0.001, s.CalculateCost(short, nil), 1e-12, "interval 10 named by the request")
}

// TestResponsesUsageKeepsEveryInputDetail fails when responsesUsageToBifrostUsage drops an input
// detail the chat prompt details also carry (TXE-7357: video_tokens was dropped, so a video sent
// through /v1/responses was billed at the text rate). Every field the two structs share by name
// and type is set to its own value and must survive the conversion.
func TestResponsesUsageKeepsEveryInputDetail(t *testing.T) {
	in := &schemas.ResponsesResponseInputTokens{}
	src := reflect.ValueOf(in).Elem()
	dstType := reflect.TypeOf(schemas.ChatPromptTokensDetails{})
	shared := 0
	for i := 0; i < src.NumField(); i++ {
		f := src.Type().Field(i)
		if df, ok := dstType.FieldByName(f.Name); ok && df.Type == f.Type && f.Type.Kind() == reflect.Int {
			src.Field(i).SetInt(int64(100 + i))
			shared++
		}
	}
	if shared < 7 {
		t.Fatalf("only %d shared int fields found; the structs changed shape, update this check", shared)
	}
	out := responsesUsageToBifrostUsage(&schemas.ResponsesResponseUsage{InputTokensDetails: in}).PromptTokensDetails
	if out == nil {
		t.Fatal("input details dropped")
	}
	dst := reflect.ValueOf(out).Elem()
	for i := 0; i < src.NumField(); i++ {
		f := src.Type().Field(i)
		if df, ok := dstType.FieldByName(f.Name); ok && df.Type == f.Type && f.Type.Kind() == reflect.Int {
			if got, want := dst.FieldByName(f.Name).Int(), src.Field(i).Int(); got != want {
				t.Errorf("responsesUsageToBifrostUsage drops InputTokensDetails.%s: got %d, want %d", f.Name, got, want)
			}
		}
	}
}

// TestVideoBillingExtraKeysCoverEveryExtraRead fails when video pricing reads a key from
// VideoPricingDimensions.Extra that videoBillingExtraKeys does not list (TXE-7357): a job whose
// request content may not be stored keeps only the listed keys, so an unlisted one would be priced
// without the value the caller sent.
func TestVideoBillingExtraKeysCoverEveryExtraRead(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "videopricing.go", nil, 0)
	require.NoError(t, err)
	listed := map[string]bool{}
	for _, key := range videoBillingExtraKeys {
		listed[key] = true
	}
	read := 0
	ast.Inspect(file, func(n ast.Node) bool {
		idx, ok := n.(*ast.IndexExpr)
		if !ok {
			return true
		}
		sel, ok := idx.X.(*ast.SelectorExpr)
		lit, isLit := idx.Index.(*ast.BasicLit)
		if !ok || sel.Sel.Name != "Extra" || !isLit || lit.Kind != token.STRING {
			return true
		}
		key, _ := strconv.Unquote(lit.Value)
		read++
		if !listed[key] {
			t.Errorf("%s: video pricing reads Extra[%q], which videoBillingExtraKeys does not list", fset.Position(idx.Pos()), key)
		}
		return true
	})
	if read == 0 {
		t.Fatal("found no Extra reads in videopricing.go; the check no longer sees how pricing reads Extra")
	}
	assert.Equal(t, map[string]any{"mode": "pro", "resolution": "4k"},
		VideoBillingSelectors(map[string]any{"mode": "pro", "resolution": "4k", "reference_videos": []any{"https://x/v.mp4"}, "prompt_extend": true}))
	assert.Nil(t, VideoBillingSelectors(map[string]any{"seed": 3}))
}

// realtimeRowStore holds one decoded datasheet row under the given provider, model and mode.
func realtimeRowStore(tb testing.TB, provider, model, mode, row string) *Store {
	tb.Helper()
	var entry Entry
	require.NoError(tb, json.Unmarshal([]byte(row), &entry))
	p := convertEntryToTablePricing(provider+"/"+model, entry)
	p.Model, p.Provider, p.Mode = model, provider, mode
	return testStoreWithPricing(map[string]configstoreTables.TableModelPricing{makeKey(model, provider, mode): p})
}

// TXE-7367: xai/grok-voice-3.0 is priced per second ($0.001333) and its realtime response.done
// reports a duration, not tokens. The turn was priced through computeTextCost, which reads only
// tokens, and cost $0. A realtime turn that reports tokens keeps its token pricing.
func TestCalculateCost_RealtimeDurationTurnIsBilledPerSecond(t *testing.T) {
	s := realtimeRowStore(t, "xai", "grok-voice-3.0", "realtime", `{"provider":"xai","mode":"realtime","input_cost_per_second":0.001333333333333}`)
	turn := func(usage *schemas.ResponsesResponseUsage) *schemas.BifrostResponse {
		return &schemas.BifrostResponse{ResponsesResponse: &schemas.BifrostResponsesResponse{
			Usage:       usage,
			ExtraFields: schemas.BifrostResponseExtraFields{RequestType: schemas.RealtimeRequest, RoutingInfo: routingInfoFor(schemas.XAI, "grok-voice-3.0")},
		}}
	}
	seconds := 60.0
	assert.InDelta(t, 60*0.001333333333333, s.CalculateCost(turn(&schemas.ResponsesResponseUsage{AudioSeconds: &seconds}), nil), 1e-9)

	tokens := realtimeRowStore(t, "openai", "gpt-realtime", "realtime", `{"provider":"openai","mode":"realtime","input_cost_per_token":4e-06,"output_cost_per_token":1.6e-05,"input_cost_per_second":0.001}`)
	tokenTurn := &schemas.BifrostResponse{ResponsesResponse: &schemas.BifrostResponsesResponse{
		Usage:       &schemas.ResponsesResponseUsage{InputTokens: 100, OutputTokens: 10, TotalTokens: 110, AudioSeconds: &seconds},
		ExtraFields: schemas.BifrostResponseExtraFields{RequestType: schemas.RealtimeRequest, RoutingInfo: routingInfoFor(schemas.OpenAI, "gpt-realtime")},
	}}
	assert.InDelta(t, 100*4e-06+10*1.6e-05, tokens.CalculateCost(tokenTurn, nil), 1e-12, "a turn with tokens is priced on them, not its duration")
}
