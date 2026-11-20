package datasheet

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/capsohq/bifrost/core/schemas"
	configstoreTables "github.com/capsohq/bifrost/framework/configstore/tables"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// PriceStatus tells a model a dollar budget cannot be charged for (no row, or a row that sets no
// price) from a free one (prices set, all $0) and a priced one (TXE-7957).
// A batch line is priced only by the rates batch settlement applies (CalculateBatchCostDetailsForUsage):
// a row priced per image or per second has a synchronous price and no batch one.
func TestBatchPriceStatus(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gpt-4o", "openai", "chat"):                {Model: "gpt-4o", Provider: "openai", Mode: "chat", InputCostPerToken: schemas.Ptr(2.5e-06), OutputCostPerToken: schemas.Ptr(1e-05)},
		makeKey("input-only", "openai", "chat"):            {Model: "input-only", Provider: "openai", Mode: "chat", InputCostPerToken: schemas.Ptr(2.5e-06)},
		makeKey("free-model", "openai", "chat"):            {Model: "free-model", Provider: "openai", Mode: "chat", InputCostPerToken: schemas.Ptr(0.0), OutputCostPerToken: schemas.Ptr(0.0)},
		makeKey("per-second", "openai", "chat"):            {Model: "per-second", Provider: "openai", Mode: "chat", InputCostPerSecond: schemas.Ptr(0.001)},
		makeKey("embed", "openai", "embedding"):            {Model: "embed", Provider: "openai", Mode: "embedding", InputCostPerToken: schemas.Ptr(1e-07)},
		makeKey("per-image", "openai", "image_generation"): {Model: "per-image", Provider: "openai", Mode: "image_generation", OutputCostPerImage: schemas.Ptr(0.04)},
		makeKey("token-image", "openai", "image_generation"): {Model: "token-image", Provider: "openai", Mode: "image_generation",
			InputCostPerToken: schemas.Ptr(5e-06), OutputCostPerImageToken: schemas.Ptr(4e-05)},
	})
	cases := []struct {
		model       string
		requestType schemas.RequestType
		want        PriceStatus
	}{
		{"gpt-4o", schemas.ChatCompletionRequest, Priced},
		{"gpt-4o", schemas.BatchResultsRequest, Priced}, // an endpoint-less batch settles as chat
		{"input-only", schemas.ChatCompletionRequest, Unpriced},
		{"free-model", schemas.ChatCompletionRequest, Free},
		{"per-second", schemas.ChatCompletionRequest, Unpriced},
		{"embed", schemas.EmbeddingRequest, Priced},
		{"per-image", schemas.ImageGenerationRequest, Unpriced},
		{"token-image", schemas.ImageGenerationRequest, Priced},
		{"gpt-4o", schemas.SpeechRequest, Unpriced}, // no batch settlement for the mode
		{"no-row", schemas.ChatCompletionRequest, Unpriced},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, s.BatchPriceStatus(schemas.OpenAI, tc.model, tc.requestType, nil), "%s %s", tc.model, tc.requestType)
	}
}

func TestPriceStatus(t *testing.T) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{
		makeKey("gpt-4o", "openai", "chat"):                  {Model: "gpt-4o", Provider: "openai", Mode: "chat", InputCostPerToken: schemas.Ptr(2.5e-06)},
		makeKey("free-model", "openai", "chat"):              {Model: "free-model", Provider: "openai", Mode: "chat", InputCostPerToken: schemas.Ptr(0.0), OutputCostPerToken: schemas.Ptr(0.0)},
		makeKey("no-prices", "openai", "chat"):               {Model: "no-prices", Provider: "openai", Mode: "chat", MaxInputTokens: schemas.Ptr(1000)},
		makeKey("gpt-image-1", "openai", "image_generation"): {Model: "gpt-image-1", Provider: "openai", Mode: "image_generation", OutputCostPerImage: schemas.Ptr(0.04)},
	})
	cases := []struct {
		model       string
		requestType schemas.RequestType
		want        PriceStatus
	}{
		{"gpt-4o", schemas.ChatCompletionRequest, Priced},
		{"gpt-4o", schemas.ChatCompletionStreamRequest, Priced},
		{"gpt-4o", schemas.ResponsesRequest, Priced}, // the chat row prices a Responses request
		{"free-model", schemas.ChatCompletionRequest, Free},
		{"no-prices", schemas.ChatCompletionRequest, Unpriced},
		{"no-row", schemas.ChatCompletionRequest, Unpriced},
		{"", schemas.ChatCompletionRequest, Unpriced},
		{"gpt-4o", schemas.EmbeddingRequest, Unpriced}, // no embedding row
		{"gpt-image-1", schemas.ImageGenerationRequest, Priced},
		{"gpt-image-1", schemas.ImageEditRequest, Priced}, // an edit is priced from the generation row
		// A request with no pricing mode of its own is priced in whichever mode a line or body is.
		{"gpt-image-1", schemas.BatchCreateRequest, Priced},
		{"gpt-4o", schemas.PassthroughRequest, Priced},
		{"no-row", schemas.BatchCreateRequest, Unpriced},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, s.PriceStatus(schemas.OpenAI, tc.model, tc.requestType, nil), "%s %s", tc.model, tc.requestType)
	}

	// An override-only price makes a model with no row priced.
	require.NoError(t, s.SetOverrides([]configstoreTables.TablePricingOverride{{
		ID: "override-no-row", ScopeKind: string(ScopeKindGlobal), MatchType: string(MatchTypeExact),
		Pattern: "no-row", RequestTypes: []schemas.RequestType{schemas.ChatCompletionRequest}, PricingPatchJSON: `{"input_cost_per_token":0.000001}`,
	}, {
		ID: "override-wild", ScopeKind: string(ScopeKindGlobal), MatchType: string(MatchTypeWildcard),
		Pattern: "wild-*", RequestTypes: []schemas.RequestType{schemas.ChatCompletionRequest}, PricingPatchJSON: `{"input_cost_per_token":0.000001}`,
	}}))
	assert.Equal(t, Priced, s.PriceStatus(schemas.OpenAI, "no-row", schemas.ChatCompletionRequest, nil))

	// Every spelling a lookup folds onto one row has that row's identity, so governance cannot be
	// handed a new key by respelling a model.
	identity, status := s.PriceIdentity(schemas.OpenAI, "gpt-4o", schemas.ChatCompletionRequest, nil)
	assert.Equal(t, Priced, status)
	assert.Equal(t, "openai|chat|gpt-4o", identity)
	for _, spelling := range []string{"GPT-4O", "Gpt-4o"} {
		respelled, _ := s.PriceIdentity(schemas.OpenAI, spelling, schemas.ChatCompletionStreamRequest, nil)
		assert.Equal(t, identity, respelled, spelling)
	}
	overrideOnly, _ := s.PriceIdentity(schemas.OpenAI, "no-row", schemas.ChatCompletionRequest, nil)
	assert.Equal(t, "override|override-no-row|chat", overrideOnly)
	// Every model one wildcard override prices shares the override's identity, whatever the caller names.
	for _, model := range []string{"wild-a", "wild-b", "wild-anything"} {
		wild, status := s.PriceIdentity(schemas.OpenAI, model, schemas.ChatCompletionRequest, nil)
		assert.Equal(t, Priced, status, model)
		assert.Equal(t, "override|override-wild|chat", wild, model)
	}
	// An exact override matches its own spelling only: another spelling is unpriced and refused
	// before the call, so it never reaches the after-the-fact set.
	_, status = s.PriceIdentity(schemas.OpenAI, "No-Row", schemas.ChatCompletionRequest, nil)
	assert.Equal(t, Unpriced, status)
	none, status := s.PriceIdentity(schemas.OpenAI, "still-no-row", schemas.ChatCompletionRequest, nil)
	assert.Equal(t, Unpriced, status)
	assert.Empty(t, none)
}

// Every TableModelPricing *float64 column that is not a price must be named here. A price column
// whose name says neither cost nor price would be skipped by PriceStatus and read as unpriced.
func TestPriceFieldIndexesCoverEveryPriceColumn(t *testing.T) {
	notPrices := map[string]bool{
		"unmeasured_video_seconds":    true, // seconds an unmeasurable video is charged up to
		"free_input_images":           true, // a count of images the input price covers
		"inference_geo_us_multiplier": true, // a multiplier on the prices
	}
	typ := reflect.TypeFor[configstoreTables.TableModelPricing]()
	floatPtr := reflect.TypeFor[*float64]()
	priced := map[int]bool{}
	for _, i := range priceFieldIndexes() {
		priced[i] = true
	}
	require.Greater(t, len(priced), 100, "the price columns were not found")
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if field.Type != floatPtr {
			continue
		}
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		switch {
		case priced[i] && notPrices[name]:
			t.Errorf("%s is listed as not a price but its name reads as one", name)
		case !priced[i] && !notPrices[name]:
			t.Errorf("%s is a *float64 column whose name says neither cost nor price: name it as a price, or list it in notPrices with what it is", name)
		}
	}
}

// upstreamPriceSignaturesSnapshot is testdata/upstream_price_signatures.json: one row per
// (mode, set of prices above $0) the upstream datasheet publishes in a mode Bifrost serves, taken
// verbatim (mode, provider and price fields only) from the first row with that signature.
type upstreamPriceSignaturesSnapshot struct {
	Source     string                     `json:"source"`
	Fetched    string                     `json:"fetched"`
	Signatures map[string]json.RawMessage `json:"signatures"`
}

const upstreamPriceSignaturesSnapshotPath = "testdata/upstream_price_signatures.json"

// servedPricingModes are the datasheet modes a Bifrost request type is priced in.
var servedPricingModes = map[string]schemas.RequestType{
	"chat":                schemas.ChatCompletionRequest,
	"completion":          schemas.TextCompletionRequest,
	"responses":           schemas.ResponsesRequest,
	"realtime":            schemas.RealtimeRequest,
	"decisions":           schemas.DecisionRequest,
	"embedding":           schemas.EmbeddingRequest,
	"rerank":              schemas.RerankRequest,
	"audio_speech":        schemas.SpeechRequest,
	"audio_transcription": schemas.TranscriptionRequest,
	"image_generation":    schemas.ImageGenerationRequest,
	"image_edit":          schemas.ImageEditRequest,
	"video_generation":    schemas.VideoGenerationRequest,
	"ocr":                 schemas.OCRRequest,
}

// unpricedAnswerExemptions are the upstream signatures whose rows PriceStatus calls priced while
// the answer every request of the mode carries prices at $0, each with its reason. Governance still
// fails such an answer closed after the fact (plugins/governance unpricedbudget.go).
// The reasons an upstream signature may read priced while the answer every request of its mode
// carries prices at $0. Each says what the row prices that the answer does not carry. Under a
// dollar budget, governance charges such an answer as unpriced after the fact and refuses the model
// from then on (TXE-7957), so these are bounded to one answer per model and replica.
const (
	exemptVendorReportedTokens     = "priced only per token (text, image, audio or video tokens), which the answer carries only when the vendor reports usage; the units every answer of the mode carries are not priced"
	exemptMAIImageReportsNoUsage   = "Microsoft publishes no output tokens or price per image for MAI-Image-2.6 and its answer reports no usage (estimatedImageTokenExemptions); TXE-6543 serves the MAI route only with per-image-priced models"
	exemptPricedOutsideItsMode     = "priced only in units the row's mode does not carry (images on an audio or text row, input pixels or images on a generation row, media seconds on an embedding row): the datasheet files it under a mode its prices do not describe"
	exemptToolRowNotAModel         = "a tool's price row (code interpreter sessions, web search), filed under chat; no request names it as its model"
	exemptProvisionedThroughput    = "a Bedrock provisioned-throughput commitment priced per second of reservation, not per request; a request is not billed by it"
	exemptPriorityTierOnly         = "priced only at the priority tier (and cache reads); a standard-tier answer has no price on this row"
	exemptTierTheAnswerDoesNotName = "priced per size and quality, or per resolution and duration, combination; an answer that names no listed combination has no price on this row"
	exemptPricedPerSecondOfAudio   = "priced per second of audio, which the answer carries only when the vendor reports it"
	exemptPricedPerCharacter       = "priced per character; chat pricing reads the tokens a chat answer carries, not characters"
)

var unpricedAnswerExemptions = map[string]string{
	"anthropic/container":          exemptToolRowNotAModel,
	"azure/cohere-rerank-v5-fast":  exemptVendorReportedTokens,
	"azure/dall-e-3":               exemptTierTheAnswerDoesNotName,
	"azure/FLUX.2-flex":            exemptPricedOutsideItsMode,
	"azure/gpt-4o-mini-transcribe": exemptVendorReportedTokens,
	"azure/gpt-image-1":            exemptVendorReportedTokens,
	"azure/gpt-image-1.5":          exemptVendorReportedTokens,
	"azure/gpt-image-2":            exemptVendorReportedTokens,
	"azure/gpt-live-1":             exemptPricedPerSecondOfAudio,
	"azure/MAI-Image-2.6":          exemptMAIImageReportsNoUsage,
	"bedrock/*/1-month-commitment/cohere.command-light-text-v14":            exemptProvisionedThroughput,
	"bedrock/ap-northeast-1/1-month-commitment/anthropic.claude-instant-v1": exemptProvisionedThroughput,
	"byteplus/dreamina-seedance-2-0-260128":                                 exemptVendorReportedTokens,
	"chatgpt-image-latest":                                                  exemptVendorReportedTokens,
	"databricks/databricks-gpt-image-1":                                     exemptVendorReportedTokens,
	"databricks/databricks-gpt-image-1-5":                                   exemptVendorReportedTokens,
	"databricks/system.ai.gemini-3-1-flash-image":                           exemptVendorReportedTokens,
	"eu.twelvelabs.marengo-embed-2-7-v1:0":                                  exemptPricedOutsideItsMode,
	"fireworks_ai/accounts/fireworks/models/minimax-m2p7":                   exemptPriorityTierOnly,
	"fireworks_ai/glm-5p3":                                                  exemptPriorityTierOnly,
	"gemini/lyria-3-clip-preview":                                           exemptPricedOutsideItsMode,
	"gpt-image-1":                                                           exemptVendorReportedTokens,
	"gpt-image-1-mini":                                                      exemptVendorReportedTokens,
	"gpt-image-2":                                                           exemptVendorReportedTokens,
	"gpt-image-2-2026-04-21":                                                exemptVendorReportedTokens,
	"gpt-image-2.5-flare":                                                   exemptVendorReportedTokens,
	"gpt-image-2.5-flare-2026-09-08":                                        exemptVendorReportedTokens,
	"gpt-live-1":                                                            exemptPricedPerSecondOfAudio,
	"medlm-large":                                                           exemptPricedPerCharacter,
	"meta/sam-3.1":                                                          exemptPricedOutsideItsMode,
	"minimax/MiniMax-Hailuo-02":                                             exemptTierTheAnswerDoesNotName,
	"novita/baai/bge-reranker-v2-m3":                                        exemptVendorReportedTokens,
	"openai/container":                                                      exemptToolRowNotAModel,
	"openrouter/microsoft/mai-image-2.5":                                    exemptVendorReportedTokens,
	"openrouter/openai/gpt-image-1":                                         exemptVendorReportedTokens,
	"openrouter/openai/gpt-image-2.6-flare":                                 exemptVendorReportedTokens,
	"replicate/elevenlabs/dubbing":                                          exemptPricedPerSecondOfAudio,
	"replicate/ibm-granite/granite-speech-3.3-8b":                           exemptVendorReportedTokens,
	"replicate/ibm-granite/granite-speech-4.1-2b":                           exemptVendorReportedTokens,
	"vertex_ai/lyria-002":                                                   exemptPricedOutsideItsMode,
	"xai/grok-voice-2.0":                                                    exemptPricedPerSecondOfAudio,
}

// TestEveryUpstreamPriceSignatureIsGatedOrPricesAnAnswer is the class check for TXE-7957. For
// every (mode, set of prices) the upstream datasheet publishes in a mode Bifrost serves, it loads
// the row as the gateway does and asks two things:
//
//   - a row that sets no price above $0 must read unpriced or free, so governance refuses it under
//     a dollar budget before the upstream call (or charges the $0 it costs);
//   - a row that reads priced must price the answer every request of its mode carries (tokens,
//     images, input characters, seconds, pages, one query) above $0, or be exempt with a reason.
//
// Regenerate the snapshot from a datasheet downloaded by hand (tests run offline):
//
//	curl -sSL -o /tmp/datasheet.json https://getbifrost.ai/datasheet
//	UPSTREAM_DATASHEET_FILE=/tmp/datasheet.json UPSTREAM_DATASHEET_FETCHED=YYYY-MM-DD go test ./modelcatalog/datasheet/ -run TestEveryUpstreamPriceSignatureIsGatedOrPricesAnAnswer
//
// It runs on every pull request in the Go unit tests (offline) job.
func TestEveryUpstreamPriceSignatureIsGatedOrPricesAnAnswer(t *testing.T) {
	if source := os.Getenv("UPSTREAM_DATASHEET_FILE"); source != "" {
		writeUpstreamPriceSignaturesSnapshot(t, source)
	}
	raw, err := os.ReadFile(upstreamPriceSignaturesSnapshotPath)
	require.NoError(t, err)
	var snapshot upstreamPriceSignaturesSnapshot
	require.NoError(t, json.Unmarshal(raw, &snapshot))
	require.Greater(t, len(snapshot.Signatures), 100, "%s lists too few signatures", upstreamPriceSignaturesSnapshotPath)

	entries := make(map[string]Entry, len(snapshot.Signatures))
	for key, row := range snapshot.Signatures {
		var entry Entry
		require.NoError(t, json.Unmarshal(row, &entry), key)
		entries[key] = entry
	}
	s := testStoreWithPricing(nil)
	s.applyPricingData(entries)

	flagged := map[string]bool{}
	for _, key := range sortedKeys(entries) {
		row := convertEntryToTablePricing(key, entries[key])
		requestType, served := servedPricingModes[row.Mode]
		require.True(t, served, "%s: mode %q is not served; the generator keeps only served modes", key, row.Mode)
		provider := schemas.ModelProvider(row.Provider)
		status := s.PriceStatus(provider, row.Model, requestType, nil)
		hasPositivePrice := rowPriceStatus(&row) == Priced
		if !hasPositivePrice {
			assert.NotEqual(t, Priced, status, "%s sets no price above $0 but reads priced, so a budget would admit it and charge $0", key)
			continue
		}
		require.Equal(t, Priced, status, "%s sets prices but does not resolve as priced in its own mode", key)
		cost := s.CalculateCost(representativeAnswer(provider, row.Model, row.Mode, requestType), nil)
		if cost > 0 {
			continue
		}
		flagged[key] = true
		if reason, exempt := unpricedAnswerExemptions[key]; !exempt || strings.TrimSpace(reason) == "" {
			t.Errorf("%s (mode %s) reads priced, so a dollar budget admits it, but the answer every %s request carries prices at $0. Price what that answer carries, or exempt it in unpricedAnswerExemptions with the reason", key, row.Mode, row.Mode)
		}
	}
	for _, key := range sortedKeys(unpricedAnswerExemptions) {
		if !flagged[key] {
			t.Errorf("%s is in unpricedAnswerExemptions but no longer prices its answer at $0, or upstream no longer publishes its signature: remove the exemption", key)
		}
	}
}

// representativeAnswer is the answer every request of a mode carries, whichever vendor serves it:
// token usage for text and embeddings, one query for rerank, input characters for speech (Bifrost
// counts them from the request), seconds for transcription and video, one image at its size for
// images, pages for OCR. Units only some vendors report (image tokens, audio tokens) are left out on
// purpose: a row priced only in them prices this answer at $0.
func representativeAnswer(provider schemas.ModelProvider, model, mode string, requestType schemas.RequestType) *schemas.BifrostResponse {
	extra := schemas.BifrostResponseExtraFields{RequestType: requestType, RoutingInfo: routingInfoFor(provider, model)}
	tokens := &schemas.BifrostLLMUsage{PromptTokens: 1000, CompletionTokens: 100, TotalTokens: 1100}
	switch mode {
	case "chat":
		return &schemas.BifrostResponse{ChatResponse: &schemas.BifrostChatResponse{Usage: tokens, ExtraFields: extra}}
	case "completion":
		return &schemas.BifrostResponse{TextCompletionResponse: &schemas.BifrostTextCompletionResponse{Usage: tokens, ExtraFields: extra}}
	case "responses", "realtime":
		return &schemas.BifrostResponse{ResponsesResponse: &schemas.BifrostResponsesResponse{
			Usage: &schemas.ResponsesResponseUsage{InputTokens: 1000, OutputTokens: 100, TotalTokens: 1100}, ExtraFields: extra,
		}}
	case "decisions":
		return &schemas.BifrostResponse{DecisionResponse: &schemas.BifrostDecisionResponse{Usage: tokens, ExtraFields: extra}}
	case "embedding":
		return &schemas.BifrostResponse{EmbeddingResponse: &schemas.BifrostEmbeddingResponse{
			Usage: &schemas.BifrostLLMUsage{PromptTokens: 1000, TotalTokens: 1000}, ExtraFields: extra,
		}}
	case "rerank":
		return &schemas.BifrostResponse{RerankResponse: &schemas.BifrostRerankResponse{ExtraFields: extra}}
	case "audio_speech":
		return &schemas.BifrostResponse{SpeechResponse: &schemas.BifrostSpeechResponse{Usage: &schemas.SpeechUsage{InputChars: 1000}, ExtraFields: extra}}
	case "audio_transcription":
		return &schemas.BifrostResponse{TranscriptionResponse: &schemas.BifrostTranscriptionResponse{
			Usage: &schemas.TranscriptionUsage{Type: "duration", Seconds: schemas.Ptr(60.0)}, ExtraFields: extra,
		}}
	case "image_generation", "image_edit":
		return &schemas.BifrostResponse{ImageGenerationResponse: &schemas.BifrostImageGenerationResponse{
			Data:                              []schemas.ImageData{{URL: "https://example.invalid/1.png"}},
			ImageGenerationResponseParameters: &schemas.ImageGenerationResponseParameters{Size: "1024x1024"},
			ExtraFields:                       extra,
		}}
	case "video_generation":
		return &schemas.BifrostResponse{VideoGenerationResponse: &schemas.BifrostVideoGenerationResponse{
			Status: schemas.VideoStatusCompleted, Seconds: schemas.Ptr("5"), Size: "1280x720", ExtraFields: extra,
		}}
	case "ocr":
		return &schemas.BifrostResponse{OCRResponse: &schemas.BifrostOCRResponse{
			Model: model, UsageInfo: &schemas.OCRUsageInfo{PagesProcessed: 10}, ExtraFields: extra,
		}}
	}
	return nil
}

func writeUpstreamPriceSignaturesSnapshot(t *testing.T, source string) {
	t.Helper()
	raw, err := os.ReadFile(source)
	require.NoError(t, err)
	var datasheet map[string]map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &datasheet))
	seen := map[string]bool{}
	signatures := map[string]json.RawMessage{}
	for _, key := range sortedKeys(datasheet) {
		row := datasheet[key]
		var mode string
		if json.Unmarshal(row["mode"], &mode) != nil {
			continue
		}
		if _, served := servedPricingModes[mode]; !served {
			continue
		}
		kept := map[string]json.RawMessage{"mode": row["mode"]}
		if provider, ok := row["provider"]; ok {
			kept["provider"] = provider
		}
		var positive []string
		for field, value := range row {
			if !isPriceJSONName(field) {
				continue
			}
			kept[field] = value
			var number float64
			if json.Unmarshal(value, &number) == nil && number > 0 {
				positive = append(positive, field)
			}
		}
		sort.Strings(positive)
		signature := mode + "|" + strings.Join(positive, ",")
		if seen[signature] {
			continue
		}
		seen[signature] = true
		out, err := json.Marshal(kept)
		require.NoError(t, err)
		signatures[key] = out
	}
	out, err := json.MarshalIndent(upstreamPriceSignaturesSnapshot{
		Source:     "https://getbifrost.ai/datasheet",
		Fetched:    os.Getenv("UPSTREAM_DATASHEET_FETCHED"),
		Signatures: signatures,
	}, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Clean(upstreamPriceSignaturesSnapshotPath), append(out, '\n'), 0o644))
	t.Logf("wrote %d upstream price signatures to %s", len(signatures), upstreamPriceSignaturesSnapshotPath)
}
