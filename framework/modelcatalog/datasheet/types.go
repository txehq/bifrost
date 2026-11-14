// Package pricing owns the pricing/model-parameters catalog (compartments A + B + E):
// canonical pricing rows fetched from the upstream datasheet, per-provider
// datasheet-derived model views, supported request types and parameters,
// and scoped pricing overrides. It also computes per-response cost.
//
// The package performs no list-models I/O — that's the live store's domain.
// The hourly sync ticker lives on the composer (ModelCatalog), not here; the
// composer calls SyncFromURL / LoadFromDB / Sync*ModelParams*. Reads are
// hot-path and lock-free where possible.
package datasheet

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/bytedance/sonic"
	"github.com/capsohq/bifrost/core/schemas"
	configstoreTables "github.com/capsohq/bifrost/framework/configstore/tables"
)

// Tier boundaries for tiered token pricing. Matches the upstream datasheet
// keys (input_cost_per_token_above_<N>k_tokens).
const (
	TokenTierAbove272K = 272000
	TokenTierAbove200K = 200000
	TokenTierAbove128K = 128000
	TokenTierAbove100K = 100000
	// Fork, TXE-7355: MiniMax M3 prices above 512k, Qwen above 256k, and some hosts above 32k.
	TokenTierAbove512K = 512000
	TokenTierAbove256K = 256000
	TokenTierAbove32K  = 32000
)

// retryBackoffMin is the initial wait before the first retry; subsequent
// retries scale exponentially up to maxBackoff.
const retryBackoffMin = time.Second

// Entry represents a single model's pricing information. Field names and
// JSON tags match the datasheet schema exactly. AdditionalAttributes carries
// editorial metadata stored on the pricing row — never populated from the
// URL datasheet, only from DB reads via the management API.
type Entry struct {
	BaseModel string `json:"base_model,omitempty"`
	Provider  string `json:"provider"`
	Mode      string `json:"mode"`

	ContextLength   *int                  `json:"context_length,omitempty"`
	MaxInputTokens  *int                  `json:"max_input_tokens,omitempty"`
	MaxOutputTokens *int                  `json:"max_output_tokens,omitempty"`
	Architecture    *schemas.Architecture `json:"architecture,omitempty"`
	IsDeprecated    bool                  `json:"is_deprecated,omitempty"`

	// AdditionalAttributes carries editorial metadata stored on the pricing
	// row (e.g. description). Populated from the DB read path only; the
	// json:"-" tag prevents URL datasheet payloads from ever feeding into
	// this field via json.Unmarshal.
	AdditionalAttributes map[string]string `json:"-"`

	Options

	upstreamRateAliases
}

// upstreamRateAliases are second spellings upstream publishes for rates Options already holds
// (fork, TXE-7353). Upstream writes the tier both before and after the context window
// (input_cost_per_token_flex_above_272k_tokens and input_cost_per_token_above_272k_tokens_flex),
// and some rows (every Azure gpt-6 row) carry only the second spelling, so a 272k+ flex request
// on them was billed at the flat flex rate. OpenAI's "fast" service tier is Priority renamed
// (see tierFromResponse), and its *_fast_above_272k_tokens rates equal the priority 272k rates on
// every row that has both. Entry.UnmarshalJSON folds each alias into its Options field when that
// field is absent; the aliases are never stored or emitted on their own.
type upstreamRateAliases struct {
	AliasInputCostPerTokenAbove272kTokensFlex           *float64 `json:"input_cost_per_token_above_272k_tokens_flex,omitempty"`
	AliasOutputCostPerTokenAbove272kTokensFlex          *float64 `json:"output_cost_per_token_above_272k_tokens_flex,omitempty"`
	AliasCacheReadInputTokenCostAbove272kTokensFlex     *float64 `json:"cache_read_input_token_cost_above_272k_tokens_flex,omitempty"`
	AliasCacheCreationInputTokenCostAbove272kTokensFlex *float64 `json:"cache_creation_input_token_cost_above_272k_tokens_flex,omitempty"`

	AliasInputCostPerTokenPriorityAbove272kTokens           *float64 `json:"input_cost_per_token_priority_above_272k_tokens,omitempty"`
	AliasOutputCostPerTokenPriorityAbove272kTokens          *float64 `json:"output_cost_per_token_priority_above_272k_tokens,omitempty"`
	AliasCacheReadInputTokenCostPriorityAbove272kTokens     *float64 `json:"cache_read_input_token_cost_priority_above_272k_tokens,omitempty"`
	AliasCacheCreationInputTokenCostPriorityAbove272kTokens *float64 `json:"cache_creation_input_token_cost_priority_above_272k_tokens,omitempty"`

	AliasInputCostPerTokenFastAbove272kTokens           *float64 `json:"input_cost_per_token_fast_above_272k_tokens,omitempty"`
	AliasOutputCostPerTokenFastAbove272kTokens          *float64 `json:"output_cost_per_token_fast_above_272k_tokens,omitempty"`
	AliasCacheReadInputTokenCostFastAbove272kTokens     *float64 `json:"cache_read_input_token_cost_fast_above_272k_tokens,omitempty"`
	AliasCacheCreationInputTokenCostFastAbove272kTokens *float64 `json:"cache_creation_input_token_cost_fast_above_272k_tokens,omitempty"`

	// Batch tiers, which upstream writes both before and after the context window (fork, TXE-7354).
	AliasInputCostPerTokenBatchesAbove100kTokens           *float64 `json:"input_cost_per_token_batches_above_100k_tokens,omitempty"`
	AliasOutputCostPerTokenBatchesAbove100kTokens          *float64 `json:"output_cost_per_token_batches_above_100k_tokens,omitempty"`
	AliasInputCostPerTokenBatchesAbove272kTokens           *float64 `json:"input_cost_per_token_batches_above_272k_tokens,omitempty"`
	AliasOutputCostPerTokenBatchesAbove272kTokens          *float64 `json:"output_cost_per_token_batches_above_272k_tokens,omitempty"`
	AliasCacheReadInputTokenCostBatchesAbove272kTokens     *float64 `json:"cache_read_input_token_cost_batches_above_272k_tokens,omitempty"`
	AliasCacheCreationInputTokenCostBatchesAbove272kTokens *float64 `json:"cache_creation_input_token_cost_batches_above_272k_tokens,omitempty"`
}

// foldInto copies each alias into its Options field when the field is unset, in order of
// preference: the tier-first spelling, then the tier-last one, then (for priority) fast.
func (a upstreamRateAliases) foldInto(o *Options) {
	fill := func(dst **float64, srcs ...*float64) {
		for _, src := range srcs {
			if *dst != nil {
				return
			}
			if src != nil {
				v := *src
				*dst = &v
			}
		}
	}
	fill(&o.InputCostPerTokenFlexAbove272kTokens, a.AliasInputCostPerTokenAbove272kTokensFlex)
	fill(&o.OutputCostPerTokenFlexAbove272kTokens, a.AliasOutputCostPerTokenAbove272kTokensFlex)
	fill(&o.CacheReadInputTokenCostFlexAbove272kTokens, a.AliasCacheReadInputTokenCostAbove272kTokensFlex)
	fill(&o.CacheCreationInputTokenCostFlexAbove272kTokens, a.AliasCacheCreationInputTokenCostAbove272kTokensFlex)

	fill(&o.InputCostPerTokenAbove272kTokensPriority, a.AliasInputCostPerTokenPriorityAbove272kTokens, a.AliasInputCostPerTokenFastAbove272kTokens)
	fill(&o.OutputCostPerTokenAbove272kTokensPriority, a.AliasOutputCostPerTokenPriorityAbove272kTokens, a.AliasOutputCostPerTokenFastAbove272kTokens)
	fill(&o.CacheReadInputTokenCostAbove272kTokensPriority, a.AliasCacheReadInputTokenCostPriorityAbove272kTokens, a.AliasCacheReadInputTokenCostFastAbove272kTokens)
	fill(&o.CacheCreationInputTokenCostAbove272kTokensPriority, a.AliasCacheCreationInputTokenCostPriorityAbove272kTokens, a.AliasCacheCreationInputTokenCostFastAbove272kTokens)

	fill(&o.InputCostPerTokenAbove100kTokensBatches, a.AliasInputCostPerTokenBatchesAbove100kTokens)
	fill(&o.OutputCostPerTokenAbove100kTokensBatches, a.AliasOutputCostPerTokenBatchesAbove100kTokens)
	fill(&o.InputCostPerTokenAbove272kTokensBatches, a.AliasInputCostPerTokenBatchesAbove272kTokens)
	fill(&o.OutputCostPerTokenAbove272kTokensBatches, a.AliasOutputCostPerTokenBatchesAbove272kTokens)
	fill(&o.CacheReadInputTokenCostAbove272kTokensBatches, a.AliasCacheReadInputTokenCostBatchesAbove272kTokens)
	fill(&o.CacheCreationInputTokenCostAbove272kTokensBatches, a.AliasCacheCreationInputTokenCostBatchesAbove272kTokens)
}

// UnmarshalJSON handles the special case where search_context_cost_per_query
// may arrive as either a plain float64 or a tiered object
// {"search_context_size_low":…, "search_context_size_medium":…, "search_context_size_high":…}.
func (p *Entry) UnmarshalJSON(data []byte) error {
	type entryAlias Entry
	var raw struct {
		entryAlias
		SearchContextCostPerQuery *struct {
			Low    *float64 `json:"search_context_size_low"`
			Medium *float64 `json:"search_context_size_medium"`
			High   *float64 `json:"search_context_size_high"`
		} `json:"search_context_cost_per_query,omitempty"`
		// GuardrailCostPerUnit is upstream's map of Bedrock guardrail unit to price
		// (bedrock/guardrails, fork TXE-7365), spread into the GuardrailCostPer* fields below.
		GuardrailCostPerUnit *struct {
			ContentPolicyUnits                  *float64 `json:"contentPolicyUnits"`
			ContentPolicyImageUnits             *float64 `json:"contentPolicyImageUnits"`
			TopicPolicyUnits                    *float64 `json:"topicPolicyUnits"`
			WordPolicyUnits                     *float64 `json:"wordPolicyUnits"`
			SensitiveInformationPolicyUnits     *float64 `json:"sensitiveInformationPolicyUnits"`
			SensitiveInformationPolicyFreeUnits *float64 `json:"sensitiveInformationPolicyFreeUnits"`
			ContextualGroundingPolicyUnits      *float64 `json:"contextualGroundingPolicyUnits"`
			AutomatedReasoningPolicyUnits       *float64 `json:"automatedReasoningPolicyUnits"`
		} `json:"guardrail_cost_per_unit,omitempty"`
	}
	if err := sonic.Unmarshal(data, &raw); err != nil {
		return err
	}
	*p = Entry(raw.entryAlias)
	p.upstreamRateAliases.foldInto(&p.Options)
	p.upstreamRateAliases = upstreamRateAliases{}

	// search_context_cost_per_query arrives as a tiered object — all three values are
	// equal for non-Perplexity providers; we prefer medium, then low, then high.
	// Perplexity always returns a pre-computed total_cost so the per-query rate is
	// never consumed for that provider.
	if q := raw.SearchContextCostPerQuery; q != nil {
		switch {
		case q.Medium != nil:
			p.SearchContextCostPerQuery = q.Medium
		case q.Low != nil:
			p.SearchContextCostPerQuery = q.Low
		case q.High != nil:
			p.SearchContextCostPerQuery = q.High
		}
	}
	// The guardrail unit map fills each per-unit rate it names; a flat field already set wins.
	if g := raw.GuardrailCostPerUnit; g != nil {
		for _, pair := range []struct {
			dst **float64
			src *float64
		}{
			{&p.GuardrailCostPerContentPolicyUnit, g.ContentPolicyUnits},
			{&p.GuardrailCostPerContentPolicyImageUnit, g.ContentPolicyImageUnits},
			{&p.GuardrailCostPerTopicPolicyUnit, g.TopicPolicyUnits},
			{&p.GuardrailCostPerWordPolicyUnit, g.WordPolicyUnits},
			{&p.GuardrailCostPerSensitiveInformationPolicyUnit, g.SensitiveInformationPolicyUnits},
			{&p.GuardrailCostPerSensitiveInformationPolicyFreeUnit, g.SensitiveInformationPolicyFreeUnits},
			{&p.GuardrailCostPerContextualGroundingPolicyUnit, g.ContextualGroundingPolicyUnits},
			{&p.GuardrailCostPerAutomatedReasoningPolicyUnit, g.AutomatedReasoningPolicyUnits},
		} {
			if *pair.dst == nil {
				*pair.dst = pair.src
			}
		}
	}
	// Older datasheet rows lack web_search_cost_per_request; fall back to the per-query search rate.
	if p.WebSearchCostPerRequest == nil {
		p.WebSearchCostPerRequest = p.SearchContextCostPerQuery
	}
	return nil
}

// Options holds every individual cost field. Embedded into Entry and reused
// as the patch shape for Override.
type Options struct {
	// Costs - Text
	InputCostPerToken         *float64 `json:"input_cost_per_token,omitempty"`
	OutputCostPerToken        *float64 `json:"output_cost_per_token,omitempty"`
	InputCostPerTokenBatches  *float64 `json:"input_cost_per_token_batches,omitempty"`
	OutputCostPerTokenBatches *float64 `json:"output_cost_per_token_batches,omitempty"`
	// Explicit batch rates for each token category and context tier (fork, TXE-7354). A batch
	// row prices a category at its explicit batch rate when the datasheet publishes one, and at
	// the synchronous rate scaled by the batch/input ratio when it does not.
	InputCostPerTokenAbove100kTokensBatches                    *float64 `json:"input_cost_per_token_above_100k_tokens_batches,omitempty"`
	OutputCostPerTokenAbove100kTokensBatches                   *float64 `json:"output_cost_per_token_above_100k_tokens_batches,omitempty"`
	InputCostPerTokenAbove200kTokensBatches                    *float64 `json:"input_cost_per_token_above_200k_tokens_batches,omitempty"`
	OutputCostPerTokenAbove200kTokensBatches                   *float64 `json:"output_cost_per_token_above_200k_tokens_batches,omitempty"`
	InputCostPerTokenAbove272kTokensBatches                    *float64 `json:"input_cost_per_token_above_272k_tokens_batches,omitempty"`
	OutputCostPerTokenAbove272kTokensBatches                   *float64 `json:"output_cost_per_token_above_272k_tokens_batches,omitempty"`
	CacheReadInputTokenCostBatches                             *float64 `json:"cache_read_input_token_cost_batches,omitempty"`
	CacheReadInputTokenCostAbove100kTokensBatches              *float64 `json:"cache_read_input_token_cost_above_100k_tokens_batches,omitempty"`
	CacheReadInputTokenCostAbove200kTokensBatches              *float64 `json:"cache_read_input_token_cost_above_200k_tokens_batches,omitempty"`
	CacheReadInputTokenCostAbove272kTokensBatches              *float64 `json:"cache_read_input_token_cost_above_272k_tokens_batches,omitempty"`
	CacheCreationInputTokenCostBatches                         *float64 `json:"cache_creation_input_token_cost_batches,omitempty"`
	CacheCreationInputTokenCostAbove100kTokensBatches          *float64 `json:"cache_creation_input_token_cost_above_100k_tokens_batches,omitempty"`
	CacheCreationInputTokenCostAbove272kTokensBatches          *float64 `json:"cache_creation_input_token_cost_above_272k_tokens_batches,omitempty"`
	CacheCreationInputTokenCostBatches1h                       *float64 `json:"cache_creation_input_token_cost_batches_1h,omitempty"`
	CacheCreationInputTokenCostAbove12hrBatches                *float64 `json:"cache_creation_input_token_cost_above_12hr_batches,omitempty"`
	CacheCreationInputTokenCostAbove12hrAbove272kTokensBatches *float64 `json:"cache_creation_input_token_cost_above_12hr_above_272k_tokens_batches,omitempty"`
	InputCostPerAudioTokenBatches                              *float64 `json:"input_cost_per_audio_token_batches,omitempty"`
	InputCostPerImageTokenBatches                              *float64 `json:"input_cost_per_image_token_batches,omitempty"`
	OutputCostPerImageTokenBatches                             *float64 `json:"output_cost_per_image_token_batches,omitempty"`
	InputCostPerTokenPriority                                  *float64 `json:"input_cost_per_token_priority,omitempty"`
	OutputCostPerTokenPriority                                 *float64 `json:"output_cost_per_token_priority,omitempty"`
	InputCostPerTokenUltrafast                                 *float64 `json:"input_cost_per_token_ultrafast,omitempty"`
	OutputCostPerTokenUltrafast                                *float64 `json:"output_cost_per_token_ultrafast,omitempty"`
	InputCostPerTokenFlex                                      *float64 `json:"input_cost_per_token_flex,omitempty"`
	OutputCostPerTokenFlex                                     *float64 `json:"output_cost_per_token_flex,omitempty"`
	// Decision requests on a model whose row is another mode (OpenAI's gpt-6-luna
	// is a chat row served natively on /v1/decisions). See computeDecisionCost.
	InputCostPerTokenDecisions  *float64 `json:"input_cost_per_token_decisions,omitempty"`
	OutputCostPerTokenDecisions *float64 `json:"output_cost_per_token_decisions,omitempty"`
	// Decision rates above the 272k context window, and the decision cache rates
	// (fork, TXE-7353). Upstream publishes them for gpt-6-luna.
	InputCostPerTokenAbove272kTokensDecisions           *float64 `json:"input_cost_per_token_above_272k_tokens_decisions,omitempty"`
	OutputCostPerTokenAbove272kTokensDecisions          *float64 `json:"output_cost_per_token_above_272k_tokens_decisions,omitempty"`
	CacheReadInputTokenCostDecisions                    *float64 `json:"cache_read_input_token_cost_decisions,omitempty"`
	CacheReadInputTokenCostAbove272kTokensDecisions     *float64 `json:"cache_read_input_token_cost_above_272k_tokens_decisions,omitempty"`
	CacheCreationInputTokenCostDecisions                *float64 `json:"cache_creation_input_token_cost_decisions,omitempty"`
	CacheCreationInputTokenCostAbove272kTokensDecisions *float64 `json:"cache_creation_input_token_cost_above_272k_tokens_decisions,omitempty"`
	// Fast mode (Anthropic research preview, speed:"fast" on Opus 4.6/4.7/4.8).
	// Flat rate across the full context window — no 128k/200k/272k tiering.
	InputCostPerTokenFast  *float64 `json:"input_cost_per_token_fast,omitempty"`
	OutputCostPerTokenFast *float64 `json:"output_cost_per_token_fast,omitempty"`
	InputCostPerCharacter  *float64 `json:"input_cost_per_character,omitempty"`
	// Costs - 128k Tier
	InputCostPerTokenAbove128kTokens          *float64 `json:"input_cost_per_token_above_128k_tokens,omitempty"`
	InputCostPerImageAbove128kTokens          *float64 `json:"input_cost_per_image_above_128k_tokens,omitempty"`
	InputCostPerVideoPerSecondAbove128kTokens *float64 `json:"input_cost_per_video_per_second_above_128k_tokens,omitempty"`
	InputCostPerAudioPerSecondAbove128kTokens *float64 `json:"input_cost_per_audio_per_second_above_128k_tokens,omitempty"`
	OutputCostPerTokenAbove128kTokens         *float64 `json:"output_cost_per_token_above_128k_tokens,omitempty"`
	// Costs - 32k, 256k and 512k tiers, and cache reads above 128k (fork, TXE-7355). The
	// largest threshold the prompt passes wins, as for the 100k-272k tiers.
	InputCostPerTokenAbove32kTokens        *float64 `json:"input_cost_per_token_above_32k_tokens,omitempty"`
	OutputCostPerTokenAbove32kTokens       *float64 `json:"output_cost_per_token_above_32k_tokens,omitempty"`
	CacheReadInputTokenCostAbove32kTokens  *float64 `json:"cache_read_input_token_cost_above_32k_tokens,omitempty"`
	CacheReadInputTokenCostAbove128kTokens *float64 `json:"cache_read_input_token_cost_above_128k_tokens,omitempty"`
	InputCostPerTokenAbove256kTokens       *float64 `json:"input_cost_per_token_above_256k_tokens,omitempty"`
	OutputCostPerTokenAbove256kTokens      *float64 `json:"output_cost_per_token_above_256k_tokens,omitempty"`
	CacheReadInputTokenCostAbove256kTokens *float64 `json:"cache_read_input_token_cost_above_256k_tokens,omitempty"`
	InputCostPerTokenAbove512kTokens       *float64 `json:"input_cost_per_token_above_512k_tokens,omitempty"`
	OutputCostPerTokenAbove512kTokens      *float64 `json:"output_cost_per_token_above_512k_tokens,omitempty"`
	CacheReadInputTokenCostAbove512kTokens *float64 `json:"cache_read_input_token_cost_above_512k_tokens,omitempty"`
	// Costs - 100k Tier
	InputCostPerTokenAbove100kTokens  *float64 `json:"input_cost_per_token_above_100k_tokens,omitempty"`
	OutputCostPerTokenAbove100kTokens *float64 `json:"output_cost_per_token_above_100k_tokens,omitempty"`
	// Costs - 200k Tier
	InputCostPerTokenAbove200kTokens          *float64 `json:"input_cost_per_token_above_200k_tokens,omitempty"`
	InputCostPerTokenAbove200kTokensPriority  *float64 `json:"input_cost_per_token_above_200k_tokens_priority,omitempty"`
	OutputCostPerTokenAbove200kTokens         *float64 `json:"output_cost_per_token_above_200k_tokens,omitempty"`
	OutputCostPerTokenAbove200kTokensPriority *float64 `json:"output_cost_per_token_above_200k_tokens_priority,omitempty"`
	// Flex above 200k (Gemini; fork, TXE-7353).
	InputCostPerTokenAbove200kTokensFlex       *float64 `json:"input_cost_per_token_above_200k_tokens_flex,omitempty"`
	OutputCostPerTokenAbove200kTokensFlex      *float64 `json:"output_cost_per_token_above_200k_tokens_flex,omitempty"`
	CacheReadInputTokenCostAbove200kTokensFlex *float64 `json:"cache_read_input_token_cost_above_200k_tokens_flex,omitempty"`
	// Costs - 272k Tier
	InputCostPerTokenAbove272kTokens           *float64 `json:"input_cost_per_token_above_272k_tokens,omitempty"`
	InputCostPerTokenAbove272kTokensPriority   *float64 `json:"input_cost_per_token_above_272k_tokens_priority,omitempty"`
	InputCostPerTokenFlexAbove272kTokens       *float64 `json:"input_cost_per_token_flex_above_272k_tokens,omitempty"`
	OutputCostPerTokenAbove272kTokens          *float64 `json:"output_cost_per_token_above_272k_tokens,omitempty"`
	OutputCostPerTokenAbove272kTokensPriority  *float64 `json:"output_cost_per_token_above_272k_tokens_priority,omitempty"`
	OutputCostPerTokenFlexAbove272kTokens      *float64 `json:"output_cost_per_token_flex_above_272k_tokens,omitempty"`
	InputCostPerTokenAbove272kTokensUltrafast  *float64 `json:"input_cost_per_token_above_272k_tokens_ultrafast,omitempty"`
	OutputCostPerTokenAbove272kTokensUltrafast *float64 `json:"output_cost_per_token_above_272k_tokens_ultrafast,omitempty"`

	// Costs - Cache
	CacheCreationInputTokenCost                        *float64 `json:"cache_creation_input_token_cost,omitempty"`
	CacheReadInputTokenCost                            *float64 `json:"cache_read_input_token_cost,omitempty"`
	CacheCreationInputTokenCostAbove200kTokens         *float64 `json:"cache_creation_input_token_cost_above_200k_tokens,omitempty"`
	CacheReadInputTokenCostAbove200kTokens             *float64 `json:"cache_read_input_token_cost_above_200k_tokens,omitempty"`
	CacheReadInputTokenCostAbove200kTokensPriority     *float64 `json:"cache_read_input_token_cost_above_200k_tokens_priority,omitempty"`
	CacheCreationInputTokenCostAbove1hr                *float64 `json:"cache_creation_input_token_cost_above_1hr,omitempty"`
	CacheCreationInputTokenCostAbove1hrAbove200kTokens *float64 `json:"cache_creation_input_token_cost_above_1hr_above_200k_tokens,omitempty"`
	CacheCreationInputTokenCostAbove100kTokens         *float64 `json:"cache_creation_input_token_cost_above_100k_tokens,omitempty"`
	CacheReadInputTokenCostAbove100kTokens             *float64 `json:"cache_read_input_token_cost_above_100k_tokens,omitempty"`
	CacheCreationInputTokenCostAbove1hrAbove100kTokens *float64 `json:"cache_creation_input_token_cost_above_1hr_above_100k_tokens,omitempty"`
	CacheCreationInputAudioTokenCost                   *float64 `json:"cache_creation_input_audio_token_cost,omitempty"`
	CacheReadInputTokenCostPriority                    *float64 `json:"cache_read_input_token_cost_priority,omitempty"`
	CacheReadInputTokenCostUltrafast                   *float64 `json:"cache_read_input_token_cost_ultrafast,omitempty"`
	CacheReadInputTokenCostFlex                        *float64 `json:"cache_read_input_token_cost_flex,omitempty"`
	CacheReadInputImageTokenCost                       *float64 `json:"cache_read_input_image_token_cost,omitempty"`
	CacheReadInputTokenCostAbove272kTokens             *float64 `json:"cache_read_input_token_cost_above_272k_tokens,omitempty"`
	CacheReadInputTokenCostAbove272kTokensPriority     *float64 `json:"cache_read_input_token_cost_above_272k_tokens_priority,omitempty"`
	CacheReadInputTokenCostFlexAbove272kTokens         *float64 `json:"cache_read_input_token_cost_flex_above_272k_tokens,omitempty"`
	CacheReadInputTokenCostAbove272kTokensUltrafast    *float64 `json:"cache_read_input_token_cost_above_272k_tokens_ultrafast,omitempty"`
	// OpenAI cache-write (cache-creation) tiered rates, added with gpt-5.6.
	CacheCreationInputTokenCostAbove272kTokens          *float64 `json:"cache_creation_input_token_cost_above_272k_tokens,omitempty"`
	CacheCreationInputTokenCostFlex                     *float64 `json:"cache_creation_input_token_cost_flex,omitempty"`
	CacheCreationInputTokenCostFlexAbove272kTokens      *float64 `json:"cache_creation_input_token_cost_flex_above_272k_tokens,omitempty"`
	CacheCreationInputTokenCostPriority                 *float64 `json:"cache_creation_input_token_cost_priority,omitempty"`
	CacheCreationInputTokenCostAbove272kTokensPriority  *float64 `json:"cache_creation_input_token_cost_above_272k_tokens_priority,omitempty"`
	CacheCreationInputTokenCostUltrafast                *float64 `json:"cache_creation_input_token_cost_ultrafast,omitempty"`
	CacheCreationInputTokenCostAbove272kTokensUltrafast *float64 `json:"cache_creation_input_token_cost_above_272k_tokens_ultrafast,omitempty"`
	// Fast mode (Anthropic) cache rates — flat across the full context window, no tiering.
	CacheCreationInputTokenCostFast         *float64 `json:"cache_creation_input_token_cost_fast,omitempty"`
	CacheCreationInputTokenCostAbove1hrFast *float64 `json:"cache_creation_input_token_cost_above_1hr_fast,omitempty"`
	CacheReadInputTokenCostFast             *float64 `json:"cache_read_input_token_cost_fast,omitempty"`
	// OpenAI 12-hour cache-write rates (prompt_cache_retention "24h" writes reported as
	// ttl_12h_tokens), by service tier and the 272k context window (fork, TXE-7353). A
	// 12h write is priced here; without a 12h rate it falls back to the above-1hr rate.
	CacheCreationInputTokenCostAbove12hr                         *float64 `json:"cache_creation_input_token_cost_above_12hr,omitempty"`
	CacheCreationInputTokenCostAbove12hrAbove272kTokens          *float64 `json:"cache_creation_input_token_cost_above_12hr_above_272k_tokens,omitempty"`
	CacheCreationInputTokenCostAbove12hrFlex                     *float64 `json:"cache_creation_input_token_cost_above_12hr_flex,omitempty"`
	CacheCreationInputTokenCostAbove12hrAbove272kTokensFlex      *float64 `json:"cache_creation_input_token_cost_above_12hr_above_272k_tokens_flex,omitempty"`
	CacheCreationInputTokenCostAbove12hrPriority                 *float64 `json:"cache_creation_input_token_cost_above_12hr_priority,omitempty"`
	CacheCreationInputTokenCostAbove12hrAbove272kTokensPriority  *float64 `json:"cache_creation_input_token_cost_above_12hr_above_272k_tokens_priority,omitempty"`
	CacheCreationInputTokenCostAbove12hrUltrafast                *float64 `json:"cache_creation_input_token_cost_above_12hr_ultrafast,omitempty"`
	CacheCreationInputTokenCostAbove12hrAbove272kTokensUltrafast *float64 `json:"cache_creation_input_token_cost_above_12hr_above_272k_tokens_ultrafast,omitempty"`

	// Costs - Image
	InputCostPerImage                             *float64 `json:"input_cost_per_image,omitempty"`
	InputCostPerPixel                             *float64 `json:"input_cost_per_pixel,omitempty"`
	OutputCostPerImage                            *float64 `json:"output_cost_per_image,omitempty"`
	OutputCostPerPixel                            *float64 `json:"output_cost_per_pixel,omitempty"`
	OutputCostPerImagePremiumImage                *float64 `json:"output_cost_per_image_premium_image,omitempty"`
	OutputCostPerImageAbove512x512Pixels          *float64 `json:"output_cost_per_image_above_512_and_512_pixels,omitempty"`
	OutputCostPerImageAbove512x512PixelsPremium   *float64 `json:"output_cost_per_image_above_512_and_512_pixels_and_premium_image,omitempty"`
	OutputCostPerImageAbove1024x1024Pixels        *float64 `json:"output_cost_per_image_above_1024_and_1024_pixels,omitempty"`
	OutputCostPerImageAbove1024x1024PixelsPremium *float64 `json:"output_cost_per_image_above_1024_and_1024_pixels_and_premium_image,omitempty"`
	OutputCostPerImageAbove1024x1536Pixels        *float64 `json:"output_cost_per_image_above_1024_and_1536_pixels,omitempty"`
	OutputCostPerImageAbove1536x1024Pixels        *float64 `json:"output_cost_per_image_above_1536_and_1024_pixels,omitempty"`
	OutputCostPerImageAbove2048x2048Pixels        *float64 `json:"output_cost_per_image_above_2048_and_2048_pixels,omitempty"`
	OutputCostPerImageAbove4096x4096Pixels        *float64 `json:"output_cost_per_image_above_4096_and_4096_pixels,omitempty"`
	OutputCostPerImageAbove4Megapixels            *float64 `json:"output_cost_per_image_above_4_megapixels,omitempty"`
	OutputCostPerImageAbove8Megapixels            *float64 `json:"output_cost_per_image_above_8_megapixels,omitempty"`
	OutputCostPerImageAbove16Megapixels           *float64 `json:"output_cost_per_image_above_16_megapixels,omitempty"`
	OutputCostPerImageAbove32Megapixels           *float64 `json:"output_cost_per_image_above_32_megapixels,omitempty"`
	OutputCostPerImageAbove64Megapixels           *float64 `json:"output_cost_per_image_above_64_megapixels,omitempty"`
	OutputCostPerImageLowQuality                  *float64 `json:"output_cost_per_image_low_quality,omitempty"`
	OutputCostPerImageMediumQuality               *float64 `json:"output_cost_per_image_medium_quality,omitempty"`
	OutputCostPerImageHighQuality                 *float64 `json:"output_cost_per_image_high_quality,omitempty"`
	OutputCostPerImageAutoQuality                 *float64 `json:"output_cost_per_image_auto_quality,omitempty"`
	InputCostPerImageToken                        *float64 `json:"input_cost_per_image_token,omitempty"`
	OutputCostPerImageToken                       *float64 `json:"output_cost_per_image_token,omitempty"`
	// Priority image output tokens (Gemini image models; fork, TXE-7353).
	OutputCostPerImageTokenPriority *float64 `json:"output_cost_per_image_token_priority,omitempty"`

	// Costs - Image, joint size and quality. These are the most specific
	// per-image rates: they win over the quality-only and size-only rates
	// above, since upstream prices size and quality together.
	OutputCostPerImageAbove1024x1024PixelsLowQuality      *float64 `json:"output_cost_per_image_above_1024_and_1024_pixels_low_quality,omitempty"`
	OutputCostPerImageAbove1024x1536PixelsLowQuality      *float64 `json:"output_cost_per_image_above_1024_and_1536_pixels_low_quality,omitempty"`
	OutputCostPerImageAbove1536x1024PixelsLowQuality      *float64 `json:"output_cost_per_image_above_1536_and_1024_pixels_low_quality,omitempty"`
	OutputCostPerImageAbove1024x1024PixelsMediumQuality   *float64 `json:"output_cost_per_image_above_1024_and_1024_pixels_medium_quality,omitempty"`
	OutputCostPerImageAbove1024x1536PixelsMediumQuality   *float64 `json:"output_cost_per_image_above_1024_and_1536_pixels_medium_quality,omitempty"`
	OutputCostPerImageAbove1536x1024PixelsMediumQuality   *float64 `json:"output_cost_per_image_above_1536_and_1024_pixels_medium_quality,omitempty"`
	OutputCostPerImageAbove1024x1024PixelsHighQuality     *float64 `json:"output_cost_per_image_above_1024_and_1024_pixels_high_quality,omitempty"`
	OutputCostPerImageAbove1024x1536PixelsHighQuality     *float64 `json:"output_cost_per_image_above_1024_and_1536_pixels_high_quality,omitempty"`
	OutputCostPerImageAbove1536x1024PixelsHighQuality     *float64 `json:"output_cost_per_image_above_1536_and_1024_pixels_high_quality,omitempty"`
	OutputCostPerImageAbove1024x1024PixelsStandardQuality *float64 `json:"output_cost_per_image_above_1024_and_1024_pixels_standard_quality,omitempty"`
	OutputCostPerImageAbove1024x1536PixelsStandardQuality *float64 `json:"output_cost_per_image_above_1024_and_1536_pixels_standard_quality,omitempty"`
	OutputCostPerImageAbove1536x1024PixelsStandardQuality *float64 `json:"output_cost_per_image_above_1536_and_1024_pixels_standard_quality,omitempty"`
	// Fork (TXE-7356): the hd, xhigh and max quality grids, and the 768, 1792,
	// 1080, 1440 and 2160 size thresholds, priced exactly like the joint rates above.
	OutputCostPerImageAbove1024x1024PixelsHDQuality       *float64 `json:"output_cost_per_image_above_1024_and_1024_pixels_hd_quality,omitempty"`
	OutputCostPerImageAbove1024x1792PixelsHDQuality       *float64 `json:"output_cost_per_image_above_1024_and_1792_pixels_hd_quality,omitempty"`
	OutputCostPerImageAbove1792x1024PixelsHDQuality       *float64 `json:"output_cost_per_image_above_1792_and_1024_pixels_hd_quality,omitempty"`
	OutputCostPerImageAbove1024x1792PixelsStandardQuality *float64 `json:"output_cost_per_image_above_1024_and_1792_pixels_standard_quality,omitempty"`
	OutputCostPerImageAbove1792x1024PixelsStandardQuality *float64 `json:"output_cost_per_image_above_1792_and_1024_pixels_standard_quality,omitempty"`
	OutputCostPerImageAbove1024x1024PixelsXHighQuality    *float64 `json:"output_cost_per_image_above_1024_and_1024_pixels_xhigh_quality,omitempty"`
	OutputCostPerImageAbove1024x1536PixelsXHighQuality    *float64 `json:"output_cost_per_image_above_1024_and_1536_pixels_xhigh_quality,omitempty"`
	OutputCostPerImageAbove1024x768PixelsXHighQuality     *float64 `json:"output_cost_per_image_above_1024_and_768_pixels_xhigh_quality,omitempty"`
	OutputCostPerImageAbove1920x1080PixelsXHighQuality    *float64 `json:"output_cost_per_image_above_1920_and_1080_pixels_xhigh_quality,omitempty"`
	OutputCostPerImageAbove2560x1440PixelsXHighQuality    *float64 `json:"output_cost_per_image_above_2560_and_1440_pixels_xhigh_quality,omitempty"`
	OutputCostPerImageAbove3840x2160PixelsXHighQuality    *float64 `json:"output_cost_per_image_above_3840_and_2160_pixels_xhigh_quality,omitempty"`
	OutputCostPerImageAbove1024x1024PixelsMaxQuality      *float64 `json:"output_cost_per_image_above_1024_and_1024_pixels_max_quality,omitempty"`
	OutputCostPerImageAbove1024x1536PixelsMaxQuality      *float64 `json:"output_cost_per_image_above_1024_and_1536_pixels_max_quality,omitempty"`
	OutputCostPerImageAbove1024x768PixelsMaxQuality       *float64 `json:"output_cost_per_image_above_1024_and_768_pixels_max_quality,omitempty"`
	OutputCostPerImageAbove1920x1080PixelsMaxQuality      *float64 `json:"output_cost_per_image_above_1920_and_1080_pixels_max_quality,omitempty"`
	OutputCostPerImageAbove2560x1440PixelsMaxQuality      *float64 `json:"output_cost_per_image_above_2560_and_1440_pixels_max_quality,omitempty"`
	OutputCostPerImageAbove3840x2160PixelsMaxQuality      *float64 `json:"output_cost_per_image_above_3840_and_2160_pixels_max_quality,omitempty"`
	OutputCostPerImageAbove1024x768PixelsLowQuality       *float64 `json:"output_cost_per_image_above_1024_and_768_pixels_low_quality,omitempty"`
	OutputCostPerImageAbove1920x1080PixelsLowQuality      *float64 `json:"output_cost_per_image_above_1920_and_1080_pixels_low_quality,omitempty"`
	OutputCostPerImageAbove2560x1440PixelsLowQuality      *float64 `json:"output_cost_per_image_above_2560_and_1440_pixels_low_quality,omitempty"`
	OutputCostPerImageAbove3840x2160PixelsLowQuality      *float64 `json:"output_cost_per_image_above_3840_and_2160_pixels_low_quality,omitempty"`
	OutputCostPerImageAbove1024x768PixelsMediumQuality    *float64 `json:"output_cost_per_image_above_1024_and_768_pixels_medium_quality,omitempty"`
	OutputCostPerImageAbove1920x1080PixelsMediumQuality   *float64 `json:"output_cost_per_image_above_1920_and_1080_pixels_medium_quality,omitempty"`
	OutputCostPerImageAbove2560x1440PixelsMediumQuality   *float64 `json:"output_cost_per_image_above_2560_and_1440_pixels_medium_quality,omitempty"`
	OutputCostPerImageAbove3840x2160PixelsMediumQuality   *float64 `json:"output_cost_per_image_above_3840_and_2160_pixels_medium_quality,omitempty"`
	OutputCostPerImageAbove1024x768PixelsHighQuality      *float64 `json:"output_cost_per_image_above_1024_and_768_pixels_high_quality,omitempty"`
	OutputCostPerImageAbove1920x1080PixelsHighQuality     *float64 `json:"output_cost_per_image_above_1920_and_1080_pixels_high_quality,omitempty"`
	OutputCostPerImageAbove2560x1440PixelsHighQuality     *float64 `json:"output_cost_per_image_above_2560_and_1440_pixels_high_quality,omitempty"`
	OutputCostPerImageAbove3840x2160PixelsHighQuality     *float64 `json:"output_cost_per_image_above_3840_and_2160_pixels_high_quality,omitempty"`

	// Costs - Image, resolution tier (fork, TXE-7356). Some vendors price an image by the
	// resolution tier the request names (resolution: "1K", "2K", ...) rather than by its pixel
	// size. Selected from the request's resolution, ahead of every size rate.
	OutputCostPerImageHalfK   *float64 `json:"output_cost_per_image_0.5K,omitempty"`
	OutputCostPerImage1K      *float64 `json:"output_cost_per_image_1K,omitempty"`
	OutputCostPerImage2K      *float64 `json:"output_cost_per_image_2K,omitempty"`
	OutputCostPerImage4K      *float64 `json:"output_cost_per_image_4K,omitempty"`
	OutputCostPerImageRes512  *float64 `json:"output_cost_per_image_512,omitempty"`
	OutputCostPerImageRes1024 *float64 `json:"output_cost_per_image_1024,omitempty"`
	OutputCostPerImageRes1536 *float64 `json:"output_cost_per_image_1536,omitempty"`

	// Costs - Image, inference steps (fork, TXE-7356). Bedrock's Stable Diffusion XL prices
	// an image by size and by whether it ran 50 or fewer steps (_50_steps) or more
	// (_max_steps). "max" is every size larger than the model's smaller step-priced size.
	OutputCostPerImageAbove512x512Pixels50Steps    *float64 `json:"output_cost_per_image_above_512_and_512_pixels_50_steps,omitempty"`
	OutputCostPerImageAbove512x512PixelsMaxSteps   *float64 `json:"output_cost_per_image_above_512_and_512_pixels_max_steps,omitempty"`
	OutputCostPerImageAbove1024x1024Pixels50Steps  *float64 `json:"output_cost_per_image_above_1024_and_1024_pixels_50_steps,omitempty"`
	OutputCostPerImageAbove1024x1024PixelsMaxSteps *float64 `json:"output_cost_per_image_above_1024_and_1024_pixels_max_steps,omitempty"`
	OutputCostPerImageAboveMaxPixels50Steps        *float64 `json:"output_cost_per_image_above_max_and_max_pixels_50_steps,omitempty"`
	OutputCostPerImageAboveMaxPixelsMaxSteps       *float64 `json:"output_cost_per_image_above_max_and_max_pixels_max_steps,omitempty"`

	// Costs - Audio/Video
	InputCostPerAudioToken *float64 `json:"input_cost_per_audio_token,omitempty"`
	// Audio input tokens by service tier (Gemini and Vertex; fork, TXE-7353).
	InputCostPerAudioTokenFlex     *float64 `json:"input_cost_per_audio_token_flex,omitempty"`
	InputCostPerAudioTokenPriority *float64 `json:"input_cost_per_audio_token_priority,omitempty"`
	InputCostPerAudioPerSecond     *float64 `json:"input_cost_per_audio_per_second,omitempty"`
	InputCostPerSecond             *float64 `json:"input_cost_per_second,omitempty"`
	// InputCostPerTextMessage prices each text message a client sends into a realtime voice
	// session (usage.text_messages; fork, TXE-7363): xAI voice, $0.004 per text input.
	InputCostPerTextMessage     *float64 `json:"input_cost_per_text_message,omitempty"`
	InputCostPerVideoPerSecond  *float64 `json:"input_cost_per_video_per_second,omitempty"`
	OutputCostPerAudioToken     *float64 `json:"output_cost_per_audio_token,omitempty"`
	OutputCostPerVideoPerSecond *float64 `json:"output_cost_per_video_per_second,omitempty"`
	OutputCostPerSecond         *float64 `json:"output_cost_per_second,omitempty"`

	// Resolution-banded video output rates, matched on the short edge of the response's
	// size. Absent band falls back to OutputCostPerVideoPerSecond, then OutputCostPerSecond.
	OutputCostPerVideoPerSecond360p  *float64 `json:"output_cost_per_video_per_second_360p,omitempty"`
	OutputCostPerVideoPerSecond480p  *float64 `json:"output_cost_per_video_per_second_480p,omitempty"`
	OutputCostPerVideoPerSecond540p  *float64 `json:"output_cost_per_video_per_second_540p,omitempty"`
	OutputCostPerVideoPerSecond720p  *float64 `json:"output_cost_per_video_per_second_720p,omitempty"`
	OutputCostPerVideoPerSecond1024p *float64 `json:"output_cost_per_video_per_second_1024p,omitempty"`
	OutputCostPerVideoPerSecond1080p *float64 `json:"output_cost_per_video_per_second_1080p,omitempty"`
	OutputCostPerVideoPerSecond4k    *float64 `json:"output_cost_per_video_per_second_4k,omitempty"`

	// Video output rates with generated audio, per resolution band (fork, TXE-7306). Upstream
	// publishes them where a vendor bills a video with audio above a silent one (Replicate's
	// pixverse-v6 720p: 0.09 silent, 0.12 with audio). A job with audio that its model has no
	// audio rate for is priced at the silent rate of its band.
	OutputCostPerVideoPerSecond360pAudio  *float64 `json:"output_cost_per_video_per_second_360p_audio,omitempty"`
	OutputCostPerVideoPerSecond540pAudio  *float64 `json:"output_cost_per_video_per_second_540p_audio,omitempty"`
	OutputCostPerVideoPerSecond720pAudio  *float64 `json:"output_cost_per_video_per_second_720p_audio,omitempty"`
	OutputCostPerVideoPerSecond1080pAudio *float64 `json:"output_cost_per_video_per_second_1080p_audio,omitempty"`
	OutputCostPerVideoPerSecond4kAudio    *float64 `json:"output_cost_per_video_per_second_4k_audio,omitempty"`
	UnmeasuredVideoSeconds                *float64 `json:"unmeasured_video_seconds,omitempty"` // fork: seconds an unmeasurable video is charged up to (TXE-7401)
	// Upstream's resolution-banded output rates (TXE-7349), published for Veo, Wan, MiniMax,
	// Runway, Replicate and fal. A band is read after the video-specific band of the same
	// resolution above and before either unbanded rate.
	OutputCostPerSecond480p  *float64 `json:"output_cost_per_second_480p,omitempty"`
	OutputCostPerSecond720p  *float64 `json:"output_cost_per_second_720p,omitempty"`
	OutputCostPerSecond768p  *float64 `json:"output_cost_per_second_768p,omitempty"`
	OutputCostPerSecond1080p *float64 `json:"output_cost_per_second_1080p,omitempty"`
	OutputCostPerSecond2k    *float64 `json:"output_cost_per_second_2k,omitempty"`
	OutputCostPerSecond4k    *float64 `json:"output_cost_per_second_4k,omitempty"`

	// Video tokens, per-clip rates, Kling's mode, Seedance's video-input surcharge and Vertex's
	// embedding interval tiers (fork, TXE-7357).
	//   - Video tokens: input video tokens a vendor reports apart (Gemini) and the output video
	//     tokens a video job reports (Seedance's completion tokens, Gemini Omni's video tokens).
	//   - Per clip: one price per clip of a resolution and length (MiniMax Hailuo), and a flat
	//     price per clip whatever its length.
	//   - Mode: Kling's pro and standard modes, with and without audio, in place of the unbanded rate.
	//   - Video in: the band's rate when the job takes a video as input.
	//   - Interval: Vertex multimodalembedding prices a second of video by its embedding interval.
	InputCostPerVideoToken                     *float64 `json:"input_cost_per_video_token,omitempty"`
	InputCostPerVideoTokenBatches              *float64 `json:"input_cost_per_video_token_batches,omitempty"`
	InputCostPerVideoPerSecondAbove8sInterval  *float64 `json:"input_cost_per_video_per_second_above_8s_interval,omitempty"`
	InputCostPerVideoPerSecondAbove15sInterval *float64 `json:"input_cost_per_video_per_second_above_15s_interval,omitempty"`
	OutputCostPerVideoToken                    *float64 `json:"output_cost_per_video_token,omitempty"`
	OutputCostPerVideo                         *float64 `json:"output_cost_per_video,omitempty"`
	OutputCostPerVideo768p6s                   *float64 `json:"output_cost_per_video_768p_6s,omitempty"`
	OutputCostPerVideo768p10s                  *float64 `json:"output_cost_per_video_768p_10s,omitempty"`
	OutputCostPerVideo1080p6s                  *float64 `json:"output_cost_per_video_1080p_6s,omitempty"`
	OutputCostPerVideoPerSecond480pVideoIn     *float64 `json:"output_cost_per_video_per_second_480p_video_in,omitempty"`
	OutputCostPerVideoPerSecond720pVideoIn     *float64 `json:"output_cost_per_video_per_second_720p_video_in,omitempty"`
	OutputCostPerVideoPerSecond1080pVideoIn    *float64 `json:"output_cost_per_video_per_second_1080p_video_in,omitempty"`
	OutputCostPerVideoPerSecond4kVideoIn       *float64 `json:"output_cost_per_video_per_second_4k_video_in,omitempty"`
	OutputCostPerVideoPerSecondPro             *float64 `json:"output_cost_per_video_per_second_pro,omitempty"`
	OutputCostPerVideoPerSecondProAudio        *float64 `json:"output_cost_per_video_per_second_pro_audio,omitempty"`
	OutputCostPerVideoPerSecondStandard        *float64 `json:"output_cost_per_video_per_second_standard,omitempty"`
	OutputCostPerVideoPerSecondStandardAudio   *float64 `json:"output_cost_per_video_per_second_standard_audio,omitempty"`

	// Costs - Other.
	//
	// SearchContextCostPerQuery is stored as a single float64, but the upstream datasheet
	// represents it as a tiered object. See Entry.UnmarshalJSON.
	SearchContextCostPerQuery *float64 `json:"search_context_cost_per_query,omitempty"`
	// WebSearchCostPerRequest prices each server-side web search call (usage.tool_usage.web_search.num_requests).
	WebSearchCostPerRequest       *float64 `json:"web_search_cost_per_request,omitempty"`
	CodeInterpreterCostPerSession *float64 `json:"code_interpreter_cost_per_session,omitempty"`
	// Bedrock Guardrails rates, per policy unit (fork, TXE-7365). Upstream publishes them as one
	// map, guardrail_cost_per_unit, keyed by Bedrock's unit names on bedrock/guardrails;
	// Entry.UnmarshalJSON spreads it into these fields so each rate syncs, overrides and prices
	// like any other.
	GuardrailCostPerContentPolicyUnit                  *float64 `json:"guardrail_cost_per_content_policy_unit,omitempty"`
	GuardrailCostPerContentPolicyImageUnit             *float64 `json:"guardrail_cost_per_content_policy_image_unit,omitempty"`
	GuardrailCostPerTopicPolicyUnit                    *float64 `json:"guardrail_cost_per_topic_policy_unit,omitempty"`
	GuardrailCostPerWordPolicyUnit                     *float64 `json:"guardrail_cost_per_word_policy_unit,omitempty"`
	GuardrailCostPerSensitiveInformationPolicyUnit     *float64 `json:"guardrail_cost_per_sensitive_information_policy_unit,omitempty"`
	GuardrailCostPerSensitiveInformationPolicyFreeUnit *float64 `json:"guardrail_cost_per_sensitive_information_policy_free_unit,omitempty"`
	GuardrailCostPerContextualGroundingPolicyUnit      *float64 `json:"guardrail_cost_per_contextual_grounding_policy_unit,omitempty"`
	GuardrailCostPerAutomatedReasoningPolicyUnit       *float64 `json:"guardrail_cost_per_automated_reasoning_policy_unit,omitempty"`
	// InputCostPerQuery is the per-query rate rerank models bill on. Cohere and Bedrock both
	// define a query (a "search unit") as one query against up to 100 document chunks, so a
	// request over that many chunks bills as several queries. It is unrelated to
	// SearchContextCostPerQuery, which prices web-search context on chat models.
	//
	// Not applicable to Vertex: its Ranking API bills "ranking units" derived from record count
	// and title/content size, so a flat per-query rate would misprice every call.
	InputCostPerQuery        *float64 `json:"input_cost_per_query,omitempty"`
	InferenceGeoUSMultiplier *float64 `json:"inference_geo_us_multiplier,omitempty"`
	// CostPerRequest is a flat fee added once per billed request, on top of
	// whatever usage-based cost the request otherwise computes to.
	CostPerRequest *float64 `json:"cost_per_request,omitempty"`

	// Costs - OCR
	OCRCostPerPage        *float64 `json:"ocr_cost_per_page,omitempty"`
	AnnotationCostPerPage *float64 `json:"annotation_cost_per_page,omitempty"`

	// Costs - Time of day
	//
	// OffPeakCostMultiplier scales every usage-based charge when the request
	// falls outside the PeakHours windows. Every other rate on this struct is
	// the PEAK price, so the multiplier is expected to be in (0, 1] — 0.5 for
	// DeepSeek's 50% off-peak discount. Both fields must be present for a
	// discount to apply; either one alone bills at peak.
	OffPeakCostMultiplier *float64           `json:"off_peak_cost_multiplier,omitempty"`
	PeakHours             *PeakHoursSchedule `json:"peak_hours,omitempty"`
}

// PeakHoursSchedule and PeakHoursWindow are defined in the configstore tables
// package (TableModelPricing has to reference them and datasheet already
// imports tables, so the dependency can only run that way). Aliased here so
// the datasheet JSON shape reads as one self-contained type set.
type (
	PeakHoursSchedule = configstoreTables.PeakHoursSchedule
	PeakHoursWindow   = configstoreTables.PeakHoursWindow
)

// LookupScopes carries the runtime identifiers used to resolve scoped pricing
// overrides during cost calculation.
type LookupScopes struct {
	UserID        string
	VirtualKeyID  string
	SelectedKeyID string
	Provider      string
	// BilledAt is the instant used to decide peak vs off-peak for models that
	// carry a PeakHours schedule. It is the request's START time, not the
	// completion time: that keeps pricing deterministic and reproducible, makes
	// streaming and non-streaming agree, and matches the timestamp users see in
	// logs. A long stream that crosses a window boundary bills entirely at its
	// start-time rate. The zero value means "unknown" and falls back to the
	// wall clock at evaluation time.
	BilledAt time.Time
}

// LookupScopesFromContext builds a LookupScopes from a BifrostContext. Reads
// the governance virtual key ID (not the raw VK token), the selected key ID,
// and the resolved calling user ID. provider should be the provider name
// string (e.g. "openai"); pass "" if unavailable. Returns nil only when ctx
// is nil. An empty scopes value is still returned when all fields are empty
// so global-scope overrides remain evaluable.
//
// NOT SAFE in a goroutine — reads from ctx which is cancelled when the
// request ends. Call synchronously in PostHooks and pass the result by value
// to anything that may outlive the request.
func LookupScopesFromContext(ctx *schemas.BifrostContext, provider string) *LookupScopes {
	if ctx == nil {
		return nil
	}
	userID := grantUserID(ctx)
	if userID == "" {
		userID, _ = ctx.Value(schemas.BifrostContextKeyUserID).(string)
	}
	virtualKeyID, _ := ctx.Value(schemas.BifrostContextKeyGovernanceVirtualKeyID).(string)
	selectedKeyID, _ := ctx.Value(schemas.BifrostContextKeySelectedKeyID).(string)
	billedAt, _ := ctx.Value(schemas.BifrostContextKeyRequestStartTime).(time.Time)
	return &LookupScopes{
		UserID:        userID,
		VirtualKeyID:  virtualKeyID,
		SelectedKeyID: selectedKeyID,
		Provider:      provider,
		BilledAt:      billedAt,
	}
}

// grantUserID returns the user the request's grant attributes it to, or "" when
// the context carries no grant, no identity, or no user. Callers fall back to the
// deprecated BifrostContextKeyUserID for contexts built without a grant.
func grantUserID(ctx *schemas.BifrostContext) string {
	g := ctx.Grant()
	if g == nil {
		return ""
	}
	identity := g.Identity()
	if identity == nil {
		return ""
	}
	if user := identity.User(); user != nil {
		return user.ID
	}
	return ""
}

// ScopeKind identifies which governance scope an override applies to.
type ScopeKind string

const (
	ScopeKindGlobal                ScopeKind = "global"
	ScopeKindProvider              ScopeKind = "provider"
	ScopeKindProviderKey           ScopeKind = "provider_key"
	ScopeKindVirtualKey            ScopeKind = "virtual_key"
	ScopeKindVirtualKeyProvider    ScopeKind = "virtual_key_provider"
	ScopeKindVirtualKeyProviderKey ScopeKind = "virtual_key_provider_key"
	// The user scope family targets the resolved calling user. It ranks
	// below the virtual-key family and above the provider scopes during
	// resolution: virtual-key pricing is checked first, then the user's.
	// Within the family, more identifiers = more specific.
	ScopeKindUser            ScopeKind = "user"
	ScopeKindUserProvider    ScopeKind = "user_provider"
	ScopeKindUserProviderKey ScopeKind = "user_provider_key"
)

// MatchType controls how an override pattern is matched against model names.
type MatchType string

const (
	MatchTypeExact    MatchType = "exact"
	MatchTypeWildcard MatchType = "wildcard"
)

// Override describes a scoped pricing override shared across config storage,
// model catalog compilation, and governance APIs.
type Override struct {
	ID            string                `json:"id"`
	Name          string                `json:"name"`
	ScopeKind     ScopeKind             `json:"scope_kind"`
	UserID        *string               `json:"user_id,omitempty"`
	VirtualKeyID  *string               `json:"virtual_key_id,omitempty"`
	ProviderID    *string               `json:"provider_id,omitempty"`
	ProviderKeyID *string               `json:"provider_key_id,omitempty"`
	MatchType     MatchType             `json:"match_type"`
	Pattern       string                `json:"pattern"`
	RequestTypes  []schemas.RequestType `json:"request_types,omitempty"`
	Options       Options               `json:"options"`
}

// serviceTier captures the OpenAI service_tier value from a response.
// Add new tier flags here as OpenAI introduces them.
type serviceTier struct {
	isPriority  bool // true when service_tier == "priority" or "fast" (OpenAI renamed Priority to Fast on 2026-07-30)
	isFlex      bool // true when service_tier == "flex"
	isUltrafast bool // true when service_tier == "ultrafast"
	isFast      bool // true when usage.speed == "fast" (Anthropic fast mode)
	// true when usage.inference_geo == "us" (Anthropic data residency 1.1x multiplier)
	inferenceGeoUS bool
}

// costInput holds the extracted usage data from a BifrostResponse,
// normalized for the pricing engine.
type costInput struct {
	usage               *schemas.BifrostLLMUsage
	audioTextInputChars int
	audioSeconds        *float64
	audioTokenDetails   *schemas.TranscriptionUsageInputTokenDetails
	imageUsage          *schemas.ImageUsage
	imageSize           string // e.g. "1024x1024", used for per-pixel pricing
	imageQuality        string // "low", "medium", "high", "auto" (gpt-image-1.5); empty = use base rate
	imageResolution     string // resolution tier the request named, e.g. "1K" (fork, TXE-7356)
	imageSteps          *int   // inference steps the request named (fork, TXE-7356)
	videoSeconds        *int
	videoSize           string // e.g. "1920x1080", used for resolution-banded video pricing
	videoCount          int    // generated clips on the response; 0 until the job returns them
	videoOutputTokens   *int   // output video tokens the vendor reported (fork, TXE-7357)
	videoInputSeconds   bool   // the vendor billed input video seconds: the job had a video input (TXE-7357)
	// embeddingVideoSeconds and embeddingVideoInterval are the seconds of video an embedding
	// response embedded and the longest segment it embedded them in (fork, TXE-7357).
	embeddingVideoSeconds  int
	embeddingVideoInterval int
	// videoStatus is the job's lifecycle status. A video is billed at settlement,
	// not at submission, so a non-terminal status prices to nothing.
	videoStatus       schemas.VideoStatus
	ocrProcessedPages *int
	ocrIsAnnotated    *bool
	// containerIdentifierString, when non-empty, replaces the actual requested/resolved
	// model names during pricing lookup. Used for request types whose cost is not
	// tied to a specific model. Currently only used for container creates.
	containerIdentifierString string
	tier                      serviceTier
}

// customPricingEntry is one flattened override ready for lookup.
type customPricingEntry struct {
	id            string
	scopeKind     ScopeKind
	userID        string
	virtualKeyID  string
	providerID    string
	providerKeyID string
	pattern       string // exact model name, or wildcard prefix (trailing * stripped)
	wildcard      bool
	requestModes  map[string]struct{} // always non-nil for valid overrides
	options       Options
}

// customPricingData is the in-memory lookup structure for pricing overrides.
// Exact matches are indexed by model name; wildcards are a flat slice.
type customPricingData struct {
	exact    map[string][]customPricingEntry
	wildcard []customPricingEntry
}

// --- private helpers (shared across pricing/*.go files) ---

// makeKey is the composite map key used by pricingData: model|provider|mode.
func makeKey(model, provider, mode string) string {
	return model + "|" + provider + "|" + mode
}

// normalizeProvider folds equivalent provider identifiers onto the canonical
// provider name used by the pricing catalog.
func normalizeProvider(p string) string {
	switch {
	case strings.Contains(p, "together"):
		return "together_ai"
	case strings.Contains(p, "vertex_ai") || p == "google-vertex":
		return string(schemas.Vertex)
	case strings.Contains(p, "bedrock"):
		return string(schemas.Bedrock)
	case strings.Contains(p, "cohere"):
		return string(schemas.Cohere)
	case strings.Contains(p, "runwayml"):
		return string(schemas.Runway)
	case strings.Contains(p, "fireworks_ai"):
		return string(schemas.Fireworks)
	default:
		return p
	}
}

// normalizeRequestType collapses streaming and non-streaming variants of a
// request type to a single pricing mode string.
func normalizeRequestType(reqType schemas.RequestType) string {
	switch reqType {
	case schemas.TextCompletionRequest, schemas.TextCompletionStreamRequest:
		return "completion"
	case schemas.ChatCompletionRequest, schemas.ChatCompletionStreamRequest, schemas.BatchResultsRequest:
		return "chat"
	case schemas.ResponsesRequest, schemas.ResponsesStreamRequest, schemas.WebSocketResponsesRequest, schemas.RealtimeRequest, schemas.CompactionRequest:
		return "responses"
	case schemas.EmbeddingRequest:
		return "embedding"
	case schemas.RerankRequest:
		return "rerank"
	case schemas.DecisionRequest:
		return "decisions"
	case schemas.SpeechRequest, schemas.SpeechStreamRequest:
		return "audio_speech"
	case schemas.TranscriptionRequest, schemas.TranscriptionStreamRequest:
		return "audio_transcription"
	// Fork (TXE-6431): an image task create is priced as the image it makes.
	case schemas.ImageGenerationRequest, schemas.ImageGenerationStreamRequest, schemas.ImageVariationRequest, schemas.ImageGenerationTaskRequest:
		return "image_generation"
	case schemas.ImageEditRequest, schemas.ImageEditStreamRequest, schemas.ImageEditTaskRequest:
		return "image_edit"
	case schemas.VideoGenerationRequest, schemas.VideoRemixRequest, schemas.VideoEditRequest:
		return "video_generation"
	case schemas.OCRRequest:
		return "ocr"
	case schemas.ContainerCreateRequest:
		return "container_create"
	case schemas.LiveRequest:
		return "live"
	}
	return "unknown"
}

// chatResponsesFallbackMode returns the counterpart pricing mode for request
// types that providers serve over both the chat-completions and the responses
// API. Many models carry a datasheet row under only one of the two modes (e.g.
// bedrock's openai.gpt-5.5 ships as responses-only), so a lookup that misses in
// its own mode retries under the counterpart rather than pricing at zero.
// Decision requests retry under chat: a model served natively on a decisions
// endpoint carries its decision rates on its chat row (see computeDecisionCost), and
// one answered by emulation is billed as the text call it is. A model with
// only a responses row is found by getBasePricing's further responses retry.
// Returns false for request types with no such counterpart.
func chatResponsesFallbackMode(reqType schemas.RequestType) (string, bool) {
	switch reqType {
	case schemas.ResponsesRequest, schemas.ResponsesStreamRequest, schemas.WebSocketResponsesRequest, schemas.RealtimeRequest, schemas.CompactionRequest:
		return normalizeRequestType(schemas.ChatCompletionRequest), true
	case schemas.ChatCompletionRequest, schemas.ChatCompletionStreamRequest:
		return normalizeRequestType(schemas.ResponsesRequest), true
	case schemas.DecisionRequest:
		return normalizeRequestType(schemas.ChatCompletionRequest), true
	}
	return "", false
}

// normalizeStreamRequestType maps a stream variant to its non-stream base type.
// Idempotent — passing a non-stream type returns it unchanged.
func normalizeStreamRequestType(rt schemas.RequestType) schemas.RequestType {
	switch rt {
	case schemas.TextCompletionStreamRequest:
		return schemas.TextCompletionRequest
	case schemas.ChatCompletionStreamRequest:
		return schemas.ChatCompletionRequest
	case schemas.ResponsesStreamRequest, schemas.WebSocketResponsesRequest:
		return schemas.ResponsesRequest
	case schemas.RealtimeRequest:
		return schemas.RealtimeRequest
	case schemas.SpeechStreamRequest:
		return schemas.SpeechRequest
	case schemas.TranscriptionStreamRequest:
		return schemas.TranscriptionRequest
	case schemas.ImageGenerationStreamRequest:
		return schemas.ImageGenerationRequest
	case schemas.ImageEditStreamRequest:
		return schemas.ImageEditRequest
	default:
		return rt
	}
}

// extractModelName strips a leading "provider/" prefix from a model key.
func extractModelName(modelKey string) string {
	if idx := strings.Index(modelKey, "/"); idx >= 0 {
		return modelKey[idx+1:]
	}
	return modelKey
}

// normalizeEndpointToOutputType converts a supported_endpoints URL path to a
// normalized output type. Empty string for unrecognized endpoints.
func normalizeEndpointToOutputType(endpoint string) string {
	switch {
	case strings.Contains(endpoint, "/chat/completions"):
		return "chat_completion"
	case strings.Contains(endpoint, "/responses"):
		return "responses"
	case strings.Contains(endpoint, "/completions"):
		return "text_completion"
	default:
		return ""
	}
}

// normalizeModeToOutputType converts mode to a normalized output type.
func normalizeModeToOutputType(mode string) string {
	switch mode {
	case "chat":
		return "chat_completion"
	case "completion":
		return "text_completion"
	case "responses":
		return "responses"
	default:
		return ""
	}
}

// extractSupportedParams builds a list of supported OpenAI-compatible parameter
// names from model_parameters[].id values and supports_* boolean flags.
func extractSupportedParams(parsed *schemas.ModelCapabilities) []string {
	var supported []string
	addParam := func(name string) {
		if !slices.Contains(supported, name) {
			supported = append(supported, name)
		}
	}

	for _, mp := range parsed.ModelParameters {
		switch mp.ID {
		case "reasoning_effort", "reasoning_summary":
			addParam("reasoning")
		case "web_search":
			addParam("web_search_options") // chat-path param
			addParam("web_search")         // responses-path server tool
		// Anthropic rows spell it stop_sequences; Bedrock's Nova/Titan rows carry the
		// Converse camelCase stopSequences. Both mean the neutral "stop" parameter that
		// compat's dropUnsupportedParams gates on — without the camelCase spelling those
		// 81 models silently lose stop, and the provider runs to end_turn instead.
		case "stop_sequences", "stopSequences":
			addParam("stop")
		case "promptTools", "image_detail", "stream":
			// skip — not top-level request parameters
		default:
			addParam(mp.ID)
		}
	}

	if parsed.SupportsAssistantPrefill != nil && *parsed.SupportsAssistantPrefill {
		// Not an actual request parameter; if present, trailing assistant messages are
		// left in place instead of being trimmed. Read by anthropic and by bedrock's
		// anthropic models, and by gemini/vertex -- where the default is the opposite
		// (no model supports prefill, so the trim is on unless a record turns it off),
		// because Gemini rejects any conversation ending on a role:"model" turn.
		addParam("assistant_prefill")
	}
	if parsed.SupportsFunctionCalling != nil && *parsed.SupportsFunctionCalling {
		addParam("tools")
	}
	if parsed.SupportsParallelFunctionCalling != nil && *parsed.SupportsParallelFunctionCalling {
		addParam("parallel_tool_calls")
	}
	if parsed.SupportsToolChoice != nil && *parsed.SupportsToolChoice {
		addParam("tool_choice")
	}
	if parsed.SupportsReasoning != nil && *parsed.SupportsReasoning {
		addParam("reasoning")
	}
	if parsed.SupportsReasoningWithToolCalls == nil || *parsed.SupportsReasoningWithToolCalls {
		addParam("reasoning_with_tool_calls")
	}
	if parsed.SupportsNoneReasoningEffort != nil && *parsed.SupportsNoneReasoningEffort {
		addParam("supports_none_reasoning_effort")
	}
	if parsed.SupportsReasoningSummaryDetailed == nil || *parsed.SupportsReasoningSummaryDetailed {
		addParam("reasoning_summary_detailed")
	}
	if parsed.SupportsReasoningSummaryConcise == nil || *parsed.SupportsReasoningSummaryConcise {
		addParam("reasoning_summary_concise")
	}
	if parsed.SupportsReasoningSummaryAuto == nil || *parsed.SupportsReasoningSummaryAuto {
		addParam("reasoning_summary_auto")
	}
	if parsed.SupportsResponseSchema != nil && *parsed.SupportsResponseSchema {
		addParam("response_format")
		addParam("text")
	}
	if parsed.SupportsServiceTier != nil && *parsed.SupportsServiceTier {
		addParam("service_tier")
	}
	if parsed.SupportsPromptCaching != nil && *parsed.SupportsPromptCaching {
		addParam("cachePoint")
		addParam("cache_control")
		addParam("prompt_cache_key")
		addParam("prompt_cache_retention")
	}
	if parsed.SupportsWebSearch != nil && *parsed.SupportsWebSearch {
		addParam("web_search")
		addParam("web_search_options")
	}

	return supported
}

// withRetries runs op until it succeeds or maxRetries retries are exhausted
// (1 initial attempt + maxRetries retries). After each failure it waits with
// exponential backoff starting at 1 second (retryBackoffMin), capped at
// maxBackoff when > 0. If maxBackoff is zero, the delay grows unbounded.
func withRetries[T any](ctx context.Context, maxRetries int, maxBackoff time.Duration, op func() (T, error)) (T, error) {
	var zero T
	if maxRetries < 0 {
		maxRetries = 0
	}
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		select {
		case <-ctx.Done():
			return zero, retryAbortErr(ctx, lastErr)
		default:
		}

		if attempt > 0 {
			backoff := retryBackoffMin * time.Duration(1<<uint(attempt-1))
			if maxBackoff > 0 && backoff > maxBackoff {
				backoff = maxBackoff
			}
			select {
			case <-ctx.Done():
				return zero, retryAbortErr(ctx, lastErr)
			case <-time.After(backoff):
			}
		}
		v, err := op()
		if err == nil {
			return v, nil
		}
		lastErr = err
	}
	return zero, lastErr
}

// retryAbortErr keeps the last attempt's error when ctx ends mid-retry, since
// a bare "context deadline exceeded" hides which URL failed and why.
func retryAbortErr(ctx context.Context, lastErr error) error {
	if lastErr == nil {
		return ctx.Err()
	}
	return fmt.Errorf("%w (last attempt: %v)", ctx.Err(), lastErr)
}

// convertEntryToTablePricing converts a parsed Entry from the upstream
// datasheet into the row shape persisted in the config store.
func convertEntryToTablePricing(modelKey string, entry Entry) configstoreTables.TableModelPricing {
	provider := normalizeProvider(entry.Provider)
	modelName := extractModelName(modelKey)
	return configstoreTables.TableModelPricing{
		Model:           modelName,
		BaseModel:       entry.BaseModel,
		Provider:        provider,
		Mode:            entry.Mode,
		ContextLength:   entry.ContextLength,
		MaxInputTokens:  entry.MaxInputTokens,
		MaxOutputTokens: entry.MaxOutputTokens,
		Architecture:    entry.Architecture,
		IsDeprecated:    entry.IsDeprecated,

		InputCostPerToken:           entry.InputCostPerToken,
		OutputCostPerToken:          entry.OutputCostPerToken,
		InputCostPerTokenBatches:    entry.InputCostPerTokenBatches,
		OutputCostPerTokenBatches:   entry.OutputCostPerTokenBatches,
		InputCostPerTokenPriority:   entry.InputCostPerTokenPriority,
		OutputCostPerTokenPriority:  entry.OutputCostPerTokenPriority,
		InputCostPerTokenUltrafast:  entry.InputCostPerTokenUltrafast,
		OutputCostPerTokenUltrafast: entry.OutputCostPerTokenUltrafast,
		InputCostPerTokenFlex:       entry.InputCostPerTokenFlex,
		OutputCostPerTokenFlex:      entry.OutputCostPerTokenFlex,
		InputCostPerTokenDecisions:  entry.InputCostPerTokenDecisions,
		OutputCostPerTokenDecisions: entry.OutputCostPerTokenDecisions,
		// Fork, TXE-7353: service-tier and 12h cache-write rates.
		InputCostPerTokenAbove272kTokensDecisions:                    entry.InputCostPerTokenAbove272kTokensDecisions,
		OutputCostPerTokenAbove272kTokensDecisions:                   entry.OutputCostPerTokenAbove272kTokensDecisions,
		CacheReadInputTokenCostDecisions:                             entry.CacheReadInputTokenCostDecisions,
		CacheReadInputTokenCostAbove272kTokensDecisions:              entry.CacheReadInputTokenCostAbove272kTokensDecisions,
		CacheCreationInputTokenCostDecisions:                         entry.CacheCreationInputTokenCostDecisions,
		CacheCreationInputTokenCostAbove272kTokensDecisions:          entry.CacheCreationInputTokenCostAbove272kTokensDecisions,
		InputCostPerTokenAbove200kTokensFlex:                         entry.InputCostPerTokenAbove200kTokensFlex,
		OutputCostPerTokenAbove200kTokensFlex:                        entry.OutputCostPerTokenAbove200kTokensFlex,
		CacheReadInputTokenCostAbove200kTokensFlex:                   entry.CacheReadInputTokenCostAbove200kTokensFlex,
		CacheCreationInputTokenCostAbove12hr:                         entry.CacheCreationInputTokenCostAbove12hr,
		CacheCreationInputTokenCostAbove12hrAbove272kTokens:          entry.CacheCreationInputTokenCostAbove12hrAbove272kTokens,
		CacheCreationInputTokenCostAbove12hrFlex:                     entry.CacheCreationInputTokenCostAbove12hrFlex,
		CacheCreationInputTokenCostAbove12hrAbove272kTokensFlex:      entry.CacheCreationInputTokenCostAbove12hrAbove272kTokensFlex,
		CacheCreationInputTokenCostAbove12hrPriority:                 entry.CacheCreationInputTokenCostAbove12hrPriority,
		CacheCreationInputTokenCostAbove12hrAbove272kTokensPriority:  entry.CacheCreationInputTokenCostAbove12hrAbove272kTokensPriority,
		CacheCreationInputTokenCostAbove12hrUltrafast:                entry.CacheCreationInputTokenCostAbove12hrUltrafast,
		CacheCreationInputTokenCostAbove12hrAbove272kTokensUltrafast: entry.CacheCreationInputTokenCostAbove12hrAbove272kTokensUltrafast,
		OutputCostPerImageTokenPriority:                              entry.OutputCostPerImageTokenPriority,
		InputCostPerAudioTokenFlex:                                   entry.InputCostPerAudioTokenFlex,
		InputCostPerAudioTokenPriority:                               entry.InputCostPerAudioTokenPriority,
		// Fork, TXE-7354: explicit batch rates.
		InputCostPerTokenAbove100kTokensBatches:                    entry.InputCostPerTokenAbove100kTokensBatches,
		OutputCostPerTokenAbove100kTokensBatches:                   entry.OutputCostPerTokenAbove100kTokensBatches,
		InputCostPerTokenAbove200kTokensBatches:                    entry.InputCostPerTokenAbove200kTokensBatches,
		OutputCostPerTokenAbove200kTokensBatches:                   entry.OutputCostPerTokenAbove200kTokensBatches,
		InputCostPerTokenAbove272kTokensBatches:                    entry.InputCostPerTokenAbove272kTokensBatches,
		OutputCostPerTokenAbove272kTokensBatches:                   entry.OutputCostPerTokenAbove272kTokensBatches,
		CacheReadInputTokenCostBatches:                             entry.CacheReadInputTokenCostBatches,
		CacheReadInputTokenCostAbove100kTokensBatches:              entry.CacheReadInputTokenCostAbove100kTokensBatches,
		CacheReadInputTokenCostAbove200kTokensBatches:              entry.CacheReadInputTokenCostAbove200kTokensBatches,
		CacheReadInputTokenCostAbove272kTokensBatches:              entry.CacheReadInputTokenCostAbove272kTokensBatches,
		CacheCreationInputTokenCostBatches:                         entry.CacheCreationInputTokenCostBatches,
		CacheCreationInputTokenCostAbove100kTokensBatches:          entry.CacheCreationInputTokenCostAbove100kTokensBatches,
		CacheCreationInputTokenCostAbove272kTokensBatches:          entry.CacheCreationInputTokenCostAbove272kTokensBatches,
		CacheCreationInputTokenCostBatches1h:                       entry.CacheCreationInputTokenCostBatches1h,
		CacheCreationInputTokenCostAbove12hrBatches:                entry.CacheCreationInputTokenCostAbove12hrBatches,
		CacheCreationInputTokenCostAbove12hrAbove272kTokensBatches: entry.CacheCreationInputTokenCostAbove12hrAbove272kTokensBatches,
		InputCostPerAudioTokenBatches:                              entry.InputCostPerAudioTokenBatches,
		InputCostPerImageTokenBatches:                              entry.InputCostPerImageTokenBatches,
		OutputCostPerImageTokenBatches:                             entry.OutputCostPerImageTokenBatches,
		// Fork, TXE-7355: long-context tiers.
		InputCostPerTokenAbove32kTokens:            entry.InputCostPerTokenAbove32kTokens,
		OutputCostPerTokenAbove32kTokens:           entry.OutputCostPerTokenAbove32kTokens,
		CacheReadInputTokenCostAbove32kTokens:      entry.CacheReadInputTokenCostAbove32kTokens,
		CacheReadInputTokenCostAbove128kTokens:     entry.CacheReadInputTokenCostAbove128kTokens,
		InputCostPerTokenAbove256kTokens:           entry.InputCostPerTokenAbove256kTokens,
		OutputCostPerTokenAbove256kTokens:          entry.OutputCostPerTokenAbove256kTokens,
		CacheReadInputTokenCostAbove256kTokens:     entry.CacheReadInputTokenCostAbove256kTokens,
		InputCostPerTokenAbove512kTokens:           entry.InputCostPerTokenAbove512kTokens,
		OutputCostPerTokenAbove512kTokens:          entry.OutputCostPerTokenAbove512kTokens,
		CacheReadInputTokenCostAbove512kTokens:     entry.CacheReadInputTokenCostAbove512kTokens,
		InputCostPerTokenFast:                      entry.InputCostPerTokenFast,
		OutputCostPerTokenFast:                     entry.OutputCostPerTokenFast,
		InputCostPerTokenAbove200kTokens:           entry.InputCostPerTokenAbove200kTokens,
		InputCostPerTokenAbove200kTokensPriority:   entry.InputCostPerTokenAbove200kTokensPriority,
		OutputCostPerTokenAbove200kTokens:          entry.OutputCostPerTokenAbove200kTokens,
		OutputCostPerTokenAbove200kTokensPriority:  entry.OutputCostPerTokenAbove200kTokensPriority,
		InputCostPerTokenAbove272kTokens:           entry.InputCostPerTokenAbove272kTokens,
		InputCostPerTokenAbove272kTokensPriority:   entry.InputCostPerTokenAbove272kTokensPriority,
		InputCostPerTokenFlexAbove272kTokens:       entry.InputCostPerTokenFlexAbove272kTokens,
		OutputCostPerTokenAbove272kTokens:          entry.OutputCostPerTokenAbove272kTokens,
		OutputCostPerTokenAbove272kTokensPriority:  entry.OutputCostPerTokenAbove272kTokensPriority,
		OutputCostPerTokenFlexAbove272kTokens:      entry.OutputCostPerTokenFlexAbove272kTokens,
		InputCostPerTokenAbove272kTokensUltrafast:  entry.InputCostPerTokenAbove272kTokensUltrafast,
		OutputCostPerTokenAbove272kTokensUltrafast: entry.OutputCostPerTokenAbove272kTokensUltrafast,
		InputCostPerCharacter:                      entry.InputCostPerCharacter,
		InputCostPerTokenAbove128kTokens:           entry.InputCostPerTokenAbove128kTokens,
		InputCostPerImageAbove128kTokens:           entry.InputCostPerImageAbove128kTokens,
		InputCostPerVideoPerSecondAbove128kTokens:  entry.InputCostPerVideoPerSecondAbove128kTokens,
		InputCostPerAudioPerSecondAbove128kTokens:  entry.InputCostPerAudioPerSecondAbove128kTokens,
		OutputCostPerTokenAbove128kTokens:          entry.OutputCostPerTokenAbove128kTokens,
		InputCostPerTokenAbove100kTokens:           entry.InputCostPerTokenAbove100kTokens,
		OutputCostPerTokenAbove100kTokens:          entry.OutputCostPerTokenAbove100kTokens,

		CacheCreationInputTokenCost:                         entry.CacheCreationInputTokenCost,
		CacheReadInputTokenCost:                             entry.CacheReadInputTokenCost,
		CacheCreationInputTokenCostAbove200kTokens:          entry.CacheCreationInputTokenCostAbove200kTokens,
		CacheReadInputTokenCostAbove200kTokens:              entry.CacheReadInputTokenCostAbove200kTokens,
		CacheReadInputTokenCostAbove200kTokensPriority:      entry.CacheReadInputTokenCostAbove200kTokensPriority,
		CacheCreationInputTokenCostAbove1hr:                 entry.CacheCreationInputTokenCostAbove1hr,
		CacheCreationInputTokenCostAbove1hrAbove200kTokens:  entry.CacheCreationInputTokenCostAbove1hrAbove200kTokens,
		CacheCreationInputTokenCostAbove100kTokens:          entry.CacheCreationInputTokenCostAbove100kTokens,
		CacheReadInputTokenCostAbove100kTokens:              entry.CacheReadInputTokenCostAbove100kTokens,
		CacheCreationInputTokenCostAbove1hrAbove100kTokens:  entry.CacheCreationInputTokenCostAbove1hrAbove100kTokens,
		CacheCreationInputAudioTokenCost:                    entry.CacheCreationInputAudioTokenCost,
		CacheReadInputTokenCostPriority:                     entry.CacheReadInputTokenCostPriority,
		CacheReadInputTokenCostUltrafast:                    entry.CacheReadInputTokenCostUltrafast,
		CacheReadInputTokenCostFlex:                         entry.CacheReadInputTokenCostFlex,
		CacheReadInputImageTokenCost:                        entry.CacheReadInputImageTokenCost,
		CacheReadInputTokenCostAbove272kTokens:              entry.CacheReadInputTokenCostAbove272kTokens,
		CacheReadInputTokenCostAbove272kTokensPriority:      entry.CacheReadInputTokenCostAbove272kTokensPriority,
		CacheReadInputTokenCostFlexAbove272kTokens:          entry.CacheReadInputTokenCostFlexAbove272kTokens,
		CacheReadInputTokenCostAbove272kTokensUltrafast:     entry.CacheReadInputTokenCostAbove272kTokensUltrafast,
		CacheCreationInputTokenCostAbove272kTokens:          entry.CacheCreationInputTokenCostAbove272kTokens,
		CacheCreationInputTokenCostFlex:                     entry.CacheCreationInputTokenCostFlex,
		CacheCreationInputTokenCostFlexAbove272kTokens:      entry.CacheCreationInputTokenCostFlexAbove272kTokens,
		CacheCreationInputTokenCostPriority:                 entry.CacheCreationInputTokenCostPriority,
		CacheCreationInputTokenCostAbove272kTokensPriority:  entry.CacheCreationInputTokenCostAbove272kTokensPriority,
		CacheCreationInputTokenCostUltrafast:                entry.CacheCreationInputTokenCostUltrafast,
		CacheCreationInputTokenCostAbove272kTokensUltrafast: entry.CacheCreationInputTokenCostAbove272kTokensUltrafast,
		CacheCreationInputTokenCostFast:                     entry.CacheCreationInputTokenCostFast,
		CacheCreationInputTokenCostAbove1hrFast:             entry.CacheCreationInputTokenCostAbove1hrFast,
		CacheReadInputTokenCostFast:                         entry.CacheReadInputTokenCostFast,

		InputCostPerImage:                             entry.InputCostPerImage,
		InputCostPerPixel:                             entry.InputCostPerPixel,
		OutputCostPerImage:                            entry.OutputCostPerImage,
		OutputCostPerPixel:                            entry.OutputCostPerPixel,
		OutputCostPerImagePremiumImage:                entry.OutputCostPerImagePremiumImage,
		OutputCostPerImageAbove512x512Pixels:          entry.OutputCostPerImageAbove512x512Pixels,
		OutputCostPerImageAbove512x512PixelsPremium:   entry.OutputCostPerImageAbove512x512PixelsPremium,
		OutputCostPerImageAbove1024x1024Pixels:        entry.OutputCostPerImageAbove1024x1024Pixels,
		OutputCostPerImageAbove1024x1024PixelsPremium: entry.OutputCostPerImageAbove1024x1024PixelsPremium,
		OutputCostPerImageAbove2048x2048Pixels:        entry.OutputCostPerImageAbove2048x2048Pixels,
		OutputCostPerImageAbove4096x4096Pixels:        entry.OutputCostPerImageAbove4096x4096Pixels,
		OutputCostPerImageAbove4Megapixels:            entry.OutputCostPerImageAbove4Megapixels,
		OutputCostPerImageAbove8Megapixels:            entry.OutputCostPerImageAbove8Megapixels,
		OutputCostPerImageAbove16Megapixels:           entry.OutputCostPerImageAbove16Megapixels,
		OutputCostPerImageAbove32Megapixels:           entry.OutputCostPerImageAbove32Megapixels,
		OutputCostPerImageAbove64Megapixels:           entry.OutputCostPerImageAbove64Megapixels,
		OutputCostPerImageLowQuality:                  entry.OutputCostPerImageLowQuality,
		OutputCostPerImageMediumQuality:               entry.OutputCostPerImageMediumQuality,
		OutputCostPerImageHighQuality:                 entry.OutputCostPerImageHighQuality,
		OutputCostPerImageAutoQuality:                 entry.OutputCostPerImageAutoQuality,
		InputCostPerImageToken:                        entry.InputCostPerImageToken,
		OutputCostPerImageToken:                       entry.OutputCostPerImageToken,

		OutputCostPerImageAbove1024x1536Pixels:                entry.OutputCostPerImageAbove1024x1536Pixels,
		OutputCostPerImageAbove1536x1024Pixels:                entry.OutputCostPerImageAbove1536x1024Pixels,
		OutputCostPerImageAbove1024x1024PixelsLowQuality:      entry.OutputCostPerImageAbove1024x1024PixelsLowQuality,
		OutputCostPerImageAbove1024x1536PixelsLowQuality:      entry.OutputCostPerImageAbove1024x1536PixelsLowQuality,
		OutputCostPerImageAbove1536x1024PixelsLowQuality:      entry.OutputCostPerImageAbove1536x1024PixelsLowQuality,
		OutputCostPerImageAbove1024x1024PixelsMediumQuality:   entry.OutputCostPerImageAbove1024x1024PixelsMediumQuality,
		OutputCostPerImageAbove1024x1536PixelsMediumQuality:   entry.OutputCostPerImageAbove1024x1536PixelsMediumQuality,
		OutputCostPerImageAbove1536x1024PixelsMediumQuality:   entry.OutputCostPerImageAbove1536x1024PixelsMediumQuality,
		OutputCostPerImageAbove1024x1024PixelsHighQuality:     entry.OutputCostPerImageAbove1024x1024PixelsHighQuality,
		OutputCostPerImageAbove1024x1536PixelsHighQuality:     entry.OutputCostPerImageAbove1024x1536PixelsHighQuality,
		OutputCostPerImageAbove1536x1024PixelsHighQuality:     entry.OutputCostPerImageAbove1536x1024PixelsHighQuality,
		OutputCostPerImageAbove1024x1024PixelsStandardQuality: entry.OutputCostPerImageAbove1024x1024PixelsStandardQuality,
		OutputCostPerImageAbove1024x1536PixelsStandardQuality: entry.OutputCostPerImageAbove1024x1536PixelsStandardQuality,
		OutputCostPerImageAbove1536x1024PixelsStandardQuality: entry.OutputCostPerImageAbove1536x1024PixelsStandardQuality,
		OutputCostPerImageAbove1024x1024PixelsHDQuality:       entry.OutputCostPerImageAbove1024x1024PixelsHDQuality,
		OutputCostPerImageAbove1024x1792PixelsHDQuality:       entry.OutputCostPerImageAbove1024x1792PixelsHDQuality,
		OutputCostPerImageAbove1792x1024PixelsHDQuality:       entry.OutputCostPerImageAbove1792x1024PixelsHDQuality,
		OutputCostPerImageAbove1024x1792PixelsStandardQuality: entry.OutputCostPerImageAbove1024x1792PixelsStandardQuality,
		OutputCostPerImageAbove1792x1024PixelsStandardQuality: entry.OutputCostPerImageAbove1792x1024PixelsStandardQuality,
		OutputCostPerImageAbove1024x1024PixelsXHighQuality:    entry.OutputCostPerImageAbove1024x1024PixelsXHighQuality,
		OutputCostPerImageAbove1024x1536PixelsXHighQuality:    entry.OutputCostPerImageAbove1024x1536PixelsXHighQuality,
		OutputCostPerImageAbove1024x768PixelsXHighQuality:     entry.OutputCostPerImageAbove1024x768PixelsXHighQuality,
		OutputCostPerImageAbove1920x1080PixelsXHighQuality:    entry.OutputCostPerImageAbove1920x1080PixelsXHighQuality,
		OutputCostPerImageAbove2560x1440PixelsXHighQuality:    entry.OutputCostPerImageAbove2560x1440PixelsXHighQuality,
		OutputCostPerImageAbove3840x2160PixelsXHighQuality:    entry.OutputCostPerImageAbove3840x2160PixelsXHighQuality,
		OutputCostPerImageAbove1024x1024PixelsMaxQuality:      entry.OutputCostPerImageAbove1024x1024PixelsMaxQuality,
		OutputCostPerImageAbove1024x1536PixelsMaxQuality:      entry.OutputCostPerImageAbove1024x1536PixelsMaxQuality,
		OutputCostPerImageAbove1024x768PixelsMaxQuality:       entry.OutputCostPerImageAbove1024x768PixelsMaxQuality,
		OutputCostPerImageAbove1920x1080PixelsMaxQuality:      entry.OutputCostPerImageAbove1920x1080PixelsMaxQuality,
		OutputCostPerImageAbove2560x1440PixelsMaxQuality:      entry.OutputCostPerImageAbove2560x1440PixelsMaxQuality,
		OutputCostPerImageAbove3840x2160PixelsMaxQuality:      entry.OutputCostPerImageAbove3840x2160PixelsMaxQuality,
		OutputCostPerImageAbove1024x768PixelsLowQuality:       entry.OutputCostPerImageAbove1024x768PixelsLowQuality,
		OutputCostPerImageAbove1920x1080PixelsLowQuality:      entry.OutputCostPerImageAbove1920x1080PixelsLowQuality,
		OutputCostPerImageAbove2560x1440PixelsLowQuality:      entry.OutputCostPerImageAbove2560x1440PixelsLowQuality,
		OutputCostPerImageAbove3840x2160PixelsLowQuality:      entry.OutputCostPerImageAbove3840x2160PixelsLowQuality,
		OutputCostPerImageAbove1024x768PixelsMediumQuality:    entry.OutputCostPerImageAbove1024x768PixelsMediumQuality,
		OutputCostPerImageAbove1920x1080PixelsMediumQuality:   entry.OutputCostPerImageAbove1920x1080PixelsMediumQuality,
		OutputCostPerImageAbove2560x1440PixelsMediumQuality:   entry.OutputCostPerImageAbove2560x1440PixelsMediumQuality,
		OutputCostPerImageAbove3840x2160PixelsMediumQuality:   entry.OutputCostPerImageAbove3840x2160PixelsMediumQuality,
		OutputCostPerImageAbove1024x768PixelsHighQuality:      entry.OutputCostPerImageAbove1024x768PixelsHighQuality,
		OutputCostPerImageAbove1920x1080PixelsHighQuality:     entry.OutputCostPerImageAbove1920x1080PixelsHighQuality,
		OutputCostPerImageAbove2560x1440PixelsHighQuality:     entry.OutputCostPerImageAbove2560x1440PixelsHighQuality,
		OutputCostPerImageAbove3840x2160PixelsHighQuality:     entry.OutputCostPerImageAbove3840x2160PixelsHighQuality,
		OutputCostPerImageHalfK:                               entry.OutputCostPerImageHalfK,
		OutputCostPerImage1K:                                  entry.OutputCostPerImage1K,
		OutputCostPerImage2K:                                  entry.OutputCostPerImage2K,
		OutputCostPerImage4K:                                  entry.OutputCostPerImage4K,
		OutputCostPerImageRes512:                              entry.OutputCostPerImageRes512,
		OutputCostPerImageRes1024:                             entry.OutputCostPerImageRes1024,
		OutputCostPerImageRes1536:                             entry.OutputCostPerImageRes1536,
		OutputCostPerImageAbove512x512Pixels50Steps:           entry.OutputCostPerImageAbove512x512Pixels50Steps,
		OutputCostPerImageAbove512x512PixelsMaxSteps:          entry.OutputCostPerImageAbove512x512PixelsMaxSteps,
		OutputCostPerImageAbove1024x1024Pixels50Steps:         entry.OutputCostPerImageAbove1024x1024Pixels50Steps,
		OutputCostPerImageAbove1024x1024PixelsMaxSteps:        entry.OutputCostPerImageAbove1024x1024PixelsMaxSteps,
		OutputCostPerImageAboveMaxPixels50Steps:               entry.OutputCostPerImageAboveMaxPixels50Steps,
		OutputCostPerImageAboveMaxPixelsMaxSteps:              entry.OutputCostPerImageAboveMaxPixelsMaxSteps,

		InputCostPerAudioToken:      entry.InputCostPerAudioToken,
		InputCostPerAudioPerSecond:  entry.InputCostPerAudioPerSecond,
		InputCostPerSecond:          entry.InputCostPerSecond,
		InputCostPerTextMessage:     entry.InputCostPerTextMessage,
		InputCostPerVideoPerSecond:  entry.InputCostPerVideoPerSecond,
		OutputCostPerAudioToken:     entry.OutputCostPerAudioToken,
		OutputCostPerVideoPerSecond: entry.OutputCostPerVideoPerSecond,
		OutputCostPerSecond:         entry.OutputCostPerSecond,

		OutputCostPerVideoPerSecond360p:  entry.OutputCostPerVideoPerSecond360p,
		OutputCostPerVideoPerSecond480p:  entry.OutputCostPerVideoPerSecond480p,
		OutputCostPerVideoPerSecond540p:  entry.OutputCostPerVideoPerSecond540p,
		OutputCostPerVideoPerSecond720p:  entry.OutputCostPerVideoPerSecond720p,
		OutputCostPerVideoPerSecond1024p: entry.OutputCostPerVideoPerSecond1024p,
		OutputCostPerVideoPerSecond1080p: entry.OutputCostPerVideoPerSecond1080p,
		OutputCostPerVideoPerSecond4k:    entry.OutputCostPerVideoPerSecond4k,

		OutputCostPerVideoPerSecond360pAudio:  entry.OutputCostPerVideoPerSecond360pAudio,
		OutputCostPerVideoPerSecond540pAudio:  entry.OutputCostPerVideoPerSecond540pAudio,
		OutputCostPerVideoPerSecond720pAudio:  entry.OutputCostPerVideoPerSecond720pAudio,
		OutputCostPerVideoPerSecond1080pAudio: entry.OutputCostPerVideoPerSecond1080pAudio,
		OutputCostPerVideoPerSecond4kAudio:    entry.OutputCostPerVideoPerSecond4kAudio,
		UnmeasuredVideoSeconds:                entry.UnmeasuredVideoSeconds,
		OutputCostPerSecond480p:               entry.OutputCostPerSecond480p,
		OutputCostPerSecond720p:               entry.OutputCostPerSecond720p,
		OutputCostPerSecond768p:               entry.OutputCostPerSecond768p,
		OutputCostPerSecond1080p:              entry.OutputCostPerSecond1080p,
		OutputCostPerSecond2k:                 entry.OutputCostPerSecond2k,
		OutputCostPerSecond4k:                 entry.OutputCostPerSecond4k,

		InputCostPerVideoToken:                     entry.InputCostPerVideoToken,
		InputCostPerVideoTokenBatches:              entry.InputCostPerVideoTokenBatches,
		InputCostPerVideoPerSecondAbove8sInterval:  entry.InputCostPerVideoPerSecondAbove8sInterval,
		InputCostPerVideoPerSecondAbove15sInterval: entry.InputCostPerVideoPerSecondAbove15sInterval,
		OutputCostPerVideoToken:                    entry.OutputCostPerVideoToken,
		OutputCostPerVideo:                         entry.OutputCostPerVideo,
		OutputCostPerVideo768p6s:                   entry.OutputCostPerVideo768p6s,
		OutputCostPerVideo768p10s:                  entry.OutputCostPerVideo768p10s,
		OutputCostPerVideo1080p6s:                  entry.OutputCostPerVideo1080p6s,
		OutputCostPerVideoPerSecond480pVideoIn:     entry.OutputCostPerVideoPerSecond480pVideoIn,
		OutputCostPerVideoPerSecond720pVideoIn:     entry.OutputCostPerVideoPerSecond720pVideoIn,
		OutputCostPerVideoPerSecond1080pVideoIn:    entry.OutputCostPerVideoPerSecond1080pVideoIn,
		OutputCostPerVideoPerSecond4kVideoIn:       entry.OutputCostPerVideoPerSecond4kVideoIn,
		OutputCostPerVideoPerSecondPro:             entry.OutputCostPerVideoPerSecondPro,
		OutputCostPerVideoPerSecondProAudio:        entry.OutputCostPerVideoPerSecondProAudio,
		OutputCostPerVideoPerSecondStandard:        entry.OutputCostPerVideoPerSecondStandard,
		OutputCostPerVideoPerSecondStandardAudio:   entry.OutputCostPerVideoPerSecondStandardAudio,

		SearchContextCostPerQuery:     entry.SearchContextCostPerQuery,
		WebSearchCostPerRequest:       entry.WebSearchCostPerRequest,
		CodeInterpreterCostPerSession: entry.CodeInterpreterCostPerSession,

		GuardrailCostPerContentPolicyUnit:                  entry.GuardrailCostPerContentPolicyUnit,
		GuardrailCostPerContentPolicyImageUnit:             entry.GuardrailCostPerContentPolicyImageUnit,
		GuardrailCostPerTopicPolicyUnit:                    entry.GuardrailCostPerTopicPolicyUnit,
		GuardrailCostPerWordPolicyUnit:                     entry.GuardrailCostPerWordPolicyUnit,
		GuardrailCostPerSensitiveInformationPolicyUnit:     entry.GuardrailCostPerSensitiveInformationPolicyUnit,
		GuardrailCostPerSensitiveInformationPolicyFreeUnit: entry.GuardrailCostPerSensitiveInformationPolicyFreeUnit,
		GuardrailCostPerContextualGroundingPolicyUnit:      entry.GuardrailCostPerContextualGroundingPolicyUnit,
		GuardrailCostPerAutomatedReasoningPolicyUnit:       entry.GuardrailCostPerAutomatedReasoningPolicyUnit,
		InputCostPerQuery:                                  entry.InputCostPerQuery,
		InferenceGeoUSMultiplier:                           entry.InferenceGeoUSMultiplier,
		CostPerRequest:                                     entry.CostPerRequest,

		OCRCostPerPage:        entry.OCRCostPerPage,
		AnnotationCostPerPage: entry.AnnotationCostPerPage,

		OffPeakCostMultiplier: entry.OffPeakCostMultiplier,
		PeakHours:             entry.PeakHours,
	}
}

// convertTablePricingToEntry converts a TableModelPricing row from the DB back
// into the Entry shape callers consume.
func convertTablePricingToEntry(pricing *configstoreTables.TableModelPricing) *Entry {
	options := Options{
		InputCostPerToken:           pricing.InputCostPerToken,
		OutputCostPerToken:          pricing.OutputCostPerToken,
		InputCostPerTokenBatches:    pricing.InputCostPerTokenBatches,
		OutputCostPerTokenBatches:   pricing.OutputCostPerTokenBatches,
		InputCostPerTokenPriority:   pricing.InputCostPerTokenPriority,
		OutputCostPerTokenPriority:  pricing.OutputCostPerTokenPriority,
		InputCostPerTokenUltrafast:  pricing.InputCostPerTokenUltrafast,
		OutputCostPerTokenUltrafast: pricing.OutputCostPerTokenUltrafast,
		InputCostPerTokenFlex:       pricing.InputCostPerTokenFlex,
		OutputCostPerTokenFlex:      pricing.OutputCostPerTokenFlex,
		InputCostPerTokenDecisions:  pricing.InputCostPerTokenDecisions,
		OutputCostPerTokenDecisions: pricing.OutputCostPerTokenDecisions,
		// Fork, TXE-7353: service-tier and 12h cache-write rates.
		InputCostPerTokenAbove272kTokensDecisions:                    pricing.InputCostPerTokenAbove272kTokensDecisions,
		OutputCostPerTokenAbove272kTokensDecisions:                   pricing.OutputCostPerTokenAbove272kTokensDecisions,
		CacheReadInputTokenCostDecisions:                             pricing.CacheReadInputTokenCostDecisions,
		CacheReadInputTokenCostAbove272kTokensDecisions:              pricing.CacheReadInputTokenCostAbove272kTokensDecisions,
		CacheCreationInputTokenCostDecisions:                         pricing.CacheCreationInputTokenCostDecisions,
		CacheCreationInputTokenCostAbove272kTokensDecisions:          pricing.CacheCreationInputTokenCostAbove272kTokensDecisions,
		InputCostPerTokenAbove200kTokensFlex:                         pricing.InputCostPerTokenAbove200kTokensFlex,
		OutputCostPerTokenAbove200kTokensFlex:                        pricing.OutputCostPerTokenAbove200kTokensFlex,
		CacheReadInputTokenCostAbove200kTokensFlex:                   pricing.CacheReadInputTokenCostAbove200kTokensFlex,
		CacheCreationInputTokenCostAbove12hr:                         pricing.CacheCreationInputTokenCostAbove12hr,
		CacheCreationInputTokenCostAbove12hrAbove272kTokens:          pricing.CacheCreationInputTokenCostAbove12hrAbove272kTokens,
		CacheCreationInputTokenCostAbove12hrFlex:                     pricing.CacheCreationInputTokenCostAbove12hrFlex,
		CacheCreationInputTokenCostAbove12hrAbove272kTokensFlex:      pricing.CacheCreationInputTokenCostAbove12hrAbove272kTokensFlex,
		CacheCreationInputTokenCostAbove12hrPriority:                 pricing.CacheCreationInputTokenCostAbove12hrPriority,
		CacheCreationInputTokenCostAbove12hrAbove272kTokensPriority:  pricing.CacheCreationInputTokenCostAbove12hrAbove272kTokensPriority,
		CacheCreationInputTokenCostAbove12hrUltrafast:                pricing.CacheCreationInputTokenCostAbove12hrUltrafast,
		CacheCreationInputTokenCostAbove12hrAbove272kTokensUltrafast: pricing.CacheCreationInputTokenCostAbove12hrAbove272kTokensUltrafast,
		OutputCostPerImageTokenPriority:                              pricing.OutputCostPerImageTokenPriority,
		InputCostPerAudioTokenFlex:                                   pricing.InputCostPerAudioTokenFlex,
		InputCostPerAudioTokenPriority:                               pricing.InputCostPerAudioTokenPriority,
		// Fork, TXE-7354: explicit batch rates.
		InputCostPerTokenAbove100kTokensBatches:                    pricing.InputCostPerTokenAbove100kTokensBatches,
		OutputCostPerTokenAbove100kTokensBatches:                   pricing.OutputCostPerTokenAbove100kTokensBatches,
		InputCostPerTokenAbove200kTokensBatches:                    pricing.InputCostPerTokenAbove200kTokensBatches,
		OutputCostPerTokenAbove200kTokensBatches:                   pricing.OutputCostPerTokenAbove200kTokensBatches,
		InputCostPerTokenAbove272kTokensBatches:                    pricing.InputCostPerTokenAbove272kTokensBatches,
		OutputCostPerTokenAbove272kTokensBatches:                   pricing.OutputCostPerTokenAbove272kTokensBatches,
		CacheReadInputTokenCostBatches:                             pricing.CacheReadInputTokenCostBatches,
		CacheReadInputTokenCostAbove100kTokensBatches:              pricing.CacheReadInputTokenCostAbove100kTokensBatches,
		CacheReadInputTokenCostAbove200kTokensBatches:              pricing.CacheReadInputTokenCostAbove200kTokensBatches,
		CacheReadInputTokenCostAbove272kTokensBatches:              pricing.CacheReadInputTokenCostAbove272kTokensBatches,
		CacheCreationInputTokenCostBatches:                         pricing.CacheCreationInputTokenCostBatches,
		CacheCreationInputTokenCostAbove100kTokensBatches:          pricing.CacheCreationInputTokenCostAbove100kTokensBatches,
		CacheCreationInputTokenCostAbove272kTokensBatches:          pricing.CacheCreationInputTokenCostAbove272kTokensBatches,
		CacheCreationInputTokenCostBatches1h:                       pricing.CacheCreationInputTokenCostBatches1h,
		CacheCreationInputTokenCostAbove12hrBatches:                pricing.CacheCreationInputTokenCostAbove12hrBatches,
		CacheCreationInputTokenCostAbove12hrAbove272kTokensBatches: pricing.CacheCreationInputTokenCostAbove12hrAbove272kTokensBatches,
		InputCostPerAudioTokenBatches:                              pricing.InputCostPerAudioTokenBatches,
		InputCostPerImageTokenBatches:                              pricing.InputCostPerImageTokenBatches,
		OutputCostPerImageTokenBatches:                             pricing.OutputCostPerImageTokenBatches,
		// Fork, TXE-7355: long-context tiers.
		InputCostPerTokenAbove32kTokens:            pricing.InputCostPerTokenAbove32kTokens,
		OutputCostPerTokenAbove32kTokens:           pricing.OutputCostPerTokenAbove32kTokens,
		CacheReadInputTokenCostAbove32kTokens:      pricing.CacheReadInputTokenCostAbove32kTokens,
		CacheReadInputTokenCostAbove128kTokens:     pricing.CacheReadInputTokenCostAbove128kTokens,
		InputCostPerTokenAbove256kTokens:           pricing.InputCostPerTokenAbove256kTokens,
		OutputCostPerTokenAbove256kTokens:          pricing.OutputCostPerTokenAbove256kTokens,
		CacheReadInputTokenCostAbove256kTokens:     pricing.CacheReadInputTokenCostAbove256kTokens,
		InputCostPerTokenAbove512kTokens:           pricing.InputCostPerTokenAbove512kTokens,
		OutputCostPerTokenAbove512kTokens:          pricing.OutputCostPerTokenAbove512kTokens,
		CacheReadInputTokenCostAbove512kTokens:     pricing.CacheReadInputTokenCostAbove512kTokens,
		InputCostPerTokenFast:                      pricing.InputCostPerTokenFast,
		OutputCostPerTokenFast:                     pricing.OutputCostPerTokenFast,
		InputCostPerTokenAbove200kTokens:           pricing.InputCostPerTokenAbove200kTokens,
		InputCostPerTokenAbove200kTokensPriority:   pricing.InputCostPerTokenAbove200kTokensPriority,
		OutputCostPerTokenAbove200kTokens:          pricing.OutputCostPerTokenAbove200kTokens,
		OutputCostPerTokenAbove200kTokensPriority:  pricing.OutputCostPerTokenAbove200kTokensPriority,
		InputCostPerTokenAbove272kTokens:           pricing.InputCostPerTokenAbove272kTokens,
		InputCostPerTokenAbove272kTokensPriority:   pricing.InputCostPerTokenAbove272kTokensPriority,
		InputCostPerTokenFlexAbove272kTokens:       pricing.InputCostPerTokenFlexAbove272kTokens,
		OutputCostPerTokenAbove272kTokens:          pricing.OutputCostPerTokenAbove272kTokens,
		OutputCostPerTokenAbove272kTokensPriority:  pricing.OutputCostPerTokenAbove272kTokensPriority,
		OutputCostPerTokenFlexAbove272kTokens:      pricing.OutputCostPerTokenFlexAbove272kTokens,
		InputCostPerTokenAbove272kTokensUltrafast:  pricing.InputCostPerTokenAbove272kTokensUltrafast,
		OutputCostPerTokenAbove272kTokensUltrafast: pricing.OutputCostPerTokenAbove272kTokensUltrafast,
		InputCostPerCharacter:                      pricing.InputCostPerCharacter,
		InputCostPerTokenAbove128kTokens:           pricing.InputCostPerTokenAbove128kTokens,
		InputCostPerImageAbove128kTokens:           pricing.InputCostPerImageAbove128kTokens,
		InputCostPerVideoPerSecondAbove128kTokens:  pricing.InputCostPerVideoPerSecondAbove128kTokens,
		InputCostPerAudioPerSecondAbove128kTokens:  pricing.InputCostPerAudioPerSecondAbove128kTokens,
		OutputCostPerTokenAbove128kTokens:          pricing.OutputCostPerTokenAbove128kTokens,
		InputCostPerTokenAbove100kTokens:           pricing.InputCostPerTokenAbove100kTokens,
		OutputCostPerTokenAbove100kTokens:          pricing.OutputCostPerTokenAbove100kTokens,

		CacheCreationInputTokenCost:                         pricing.CacheCreationInputTokenCost,
		CacheReadInputTokenCost:                             pricing.CacheReadInputTokenCost,
		CacheCreationInputTokenCostAbove200kTokens:          pricing.CacheCreationInputTokenCostAbove200kTokens,
		CacheReadInputTokenCostAbove200kTokens:              pricing.CacheReadInputTokenCostAbove200kTokens,
		CacheReadInputTokenCostAbove200kTokensPriority:      pricing.CacheReadInputTokenCostAbove200kTokensPriority,
		CacheCreationInputTokenCostAbove1hr:                 pricing.CacheCreationInputTokenCostAbove1hr,
		CacheCreationInputTokenCostAbove1hrAbove200kTokens:  pricing.CacheCreationInputTokenCostAbove1hrAbove200kTokens,
		CacheCreationInputTokenCostAbove100kTokens:          pricing.CacheCreationInputTokenCostAbove100kTokens,
		CacheReadInputTokenCostAbove100kTokens:              pricing.CacheReadInputTokenCostAbove100kTokens,
		CacheCreationInputTokenCostAbove1hrAbove100kTokens:  pricing.CacheCreationInputTokenCostAbove1hrAbove100kTokens,
		CacheCreationInputAudioTokenCost:                    pricing.CacheCreationInputAudioTokenCost,
		CacheReadInputTokenCostPriority:                     pricing.CacheReadInputTokenCostPriority,
		CacheReadInputTokenCostUltrafast:                    pricing.CacheReadInputTokenCostUltrafast,
		CacheReadInputTokenCostFlex:                         pricing.CacheReadInputTokenCostFlex,
		CacheReadInputImageTokenCost:                        pricing.CacheReadInputImageTokenCost,
		CacheReadInputTokenCostAbove272kTokens:              pricing.CacheReadInputTokenCostAbove272kTokens,
		CacheReadInputTokenCostAbove272kTokensPriority:      pricing.CacheReadInputTokenCostAbove272kTokensPriority,
		CacheReadInputTokenCostFlexAbove272kTokens:          pricing.CacheReadInputTokenCostFlexAbove272kTokens,
		CacheReadInputTokenCostAbove272kTokensUltrafast:     pricing.CacheReadInputTokenCostAbove272kTokensUltrafast,
		CacheCreationInputTokenCostAbove272kTokens:          pricing.CacheCreationInputTokenCostAbove272kTokens,
		CacheCreationInputTokenCostFlex:                     pricing.CacheCreationInputTokenCostFlex,
		CacheCreationInputTokenCostFlexAbove272kTokens:      pricing.CacheCreationInputTokenCostFlexAbove272kTokens,
		CacheCreationInputTokenCostPriority:                 pricing.CacheCreationInputTokenCostPriority,
		CacheCreationInputTokenCostAbove272kTokensPriority:  pricing.CacheCreationInputTokenCostAbove272kTokensPriority,
		CacheCreationInputTokenCostUltrafast:                pricing.CacheCreationInputTokenCostUltrafast,
		CacheCreationInputTokenCostAbove272kTokensUltrafast: pricing.CacheCreationInputTokenCostAbove272kTokensUltrafast,
		CacheCreationInputTokenCostFast:                     pricing.CacheCreationInputTokenCostFast,
		CacheCreationInputTokenCostAbove1hrFast:             pricing.CacheCreationInputTokenCostAbove1hrFast,
		CacheReadInputTokenCostFast:                         pricing.CacheReadInputTokenCostFast,

		InputCostPerImage:                             pricing.InputCostPerImage,
		InputCostPerPixel:                             pricing.InputCostPerPixel,
		OutputCostPerImage:                            pricing.OutputCostPerImage,
		OutputCostPerPixel:                            pricing.OutputCostPerPixel,
		OutputCostPerImagePremiumImage:                pricing.OutputCostPerImagePremiumImage,
		OutputCostPerImageAbove512x512Pixels:          pricing.OutputCostPerImageAbove512x512Pixels,
		OutputCostPerImageAbove512x512PixelsPremium:   pricing.OutputCostPerImageAbove512x512PixelsPremium,
		OutputCostPerImageAbove1024x1024Pixels:        pricing.OutputCostPerImageAbove1024x1024Pixels,
		OutputCostPerImageAbove1024x1024PixelsPremium: pricing.OutputCostPerImageAbove1024x1024PixelsPremium,
		OutputCostPerImageAbove2048x2048Pixels:        pricing.OutputCostPerImageAbove2048x2048Pixels,
		OutputCostPerImageAbove4096x4096Pixels:        pricing.OutputCostPerImageAbove4096x4096Pixels,
		OutputCostPerImageAbove4Megapixels:            pricing.OutputCostPerImageAbove4Megapixels,
		OutputCostPerImageAbove8Megapixels:            pricing.OutputCostPerImageAbove8Megapixels,
		OutputCostPerImageAbove16Megapixels:           pricing.OutputCostPerImageAbove16Megapixels,
		OutputCostPerImageAbove32Megapixels:           pricing.OutputCostPerImageAbove32Megapixels,
		OutputCostPerImageAbove64Megapixels:           pricing.OutputCostPerImageAbove64Megapixels,
		OutputCostPerImageLowQuality:                  pricing.OutputCostPerImageLowQuality,
		OutputCostPerImageMediumQuality:               pricing.OutputCostPerImageMediumQuality,
		OutputCostPerImageHighQuality:                 pricing.OutputCostPerImageHighQuality,
		OutputCostPerImageAutoQuality:                 pricing.OutputCostPerImageAutoQuality,
		InputCostPerImageToken:                        pricing.InputCostPerImageToken,
		OutputCostPerImageToken:                       pricing.OutputCostPerImageToken,

		OutputCostPerImageAbove1024x1536Pixels:                pricing.OutputCostPerImageAbove1024x1536Pixels,
		OutputCostPerImageAbove1536x1024Pixels:                pricing.OutputCostPerImageAbove1536x1024Pixels,
		OutputCostPerImageAbove1024x1024PixelsLowQuality:      pricing.OutputCostPerImageAbove1024x1024PixelsLowQuality,
		OutputCostPerImageAbove1024x1536PixelsLowQuality:      pricing.OutputCostPerImageAbove1024x1536PixelsLowQuality,
		OutputCostPerImageAbove1536x1024PixelsLowQuality:      pricing.OutputCostPerImageAbove1536x1024PixelsLowQuality,
		OutputCostPerImageAbove1024x1024PixelsMediumQuality:   pricing.OutputCostPerImageAbove1024x1024PixelsMediumQuality,
		OutputCostPerImageAbove1024x1536PixelsMediumQuality:   pricing.OutputCostPerImageAbove1024x1536PixelsMediumQuality,
		OutputCostPerImageAbove1536x1024PixelsMediumQuality:   pricing.OutputCostPerImageAbove1536x1024PixelsMediumQuality,
		OutputCostPerImageAbove1024x1024PixelsHighQuality:     pricing.OutputCostPerImageAbove1024x1024PixelsHighQuality,
		OutputCostPerImageAbove1024x1536PixelsHighQuality:     pricing.OutputCostPerImageAbove1024x1536PixelsHighQuality,
		OutputCostPerImageAbove1536x1024PixelsHighQuality:     pricing.OutputCostPerImageAbove1536x1024PixelsHighQuality,
		OutputCostPerImageAbove1024x1024PixelsStandardQuality: pricing.OutputCostPerImageAbove1024x1024PixelsStandardQuality,
		OutputCostPerImageAbove1024x1536PixelsStandardQuality: pricing.OutputCostPerImageAbove1024x1536PixelsStandardQuality,
		OutputCostPerImageAbove1536x1024PixelsStandardQuality: pricing.OutputCostPerImageAbove1536x1024PixelsStandardQuality,
		OutputCostPerImageAbove1024x1024PixelsHDQuality:       pricing.OutputCostPerImageAbove1024x1024PixelsHDQuality,
		OutputCostPerImageAbove1024x1792PixelsHDQuality:       pricing.OutputCostPerImageAbove1024x1792PixelsHDQuality,
		OutputCostPerImageAbove1792x1024PixelsHDQuality:       pricing.OutputCostPerImageAbove1792x1024PixelsHDQuality,
		OutputCostPerImageAbove1024x1792PixelsStandardQuality: pricing.OutputCostPerImageAbove1024x1792PixelsStandardQuality,
		OutputCostPerImageAbove1792x1024PixelsStandardQuality: pricing.OutputCostPerImageAbove1792x1024PixelsStandardQuality,
		OutputCostPerImageAbove1024x1024PixelsXHighQuality:    pricing.OutputCostPerImageAbove1024x1024PixelsXHighQuality,
		OutputCostPerImageAbove1024x1536PixelsXHighQuality:    pricing.OutputCostPerImageAbove1024x1536PixelsXHighQuality,
		OutputCostPerImageAbove1024x768PixelsXHighQuality:     pricing.OutputCostPerImageAbove1024x768PixelsXHighQuality,
		OutputCostPerImageAbove1920x1080PixelsXHighQuality:    pricing.OutputCostPerImageAbove1920x1080PixelsXHighQuality,
		OutputCostPerImageAbove2560x1440PixelsXHighQuality:    pricing.OutputCostPerImageAbove2560x1440PixelsXHighQuality,
		OutputCostPerImageAbove3840x2160PixelsXHighQuality:    pricing.OutputCostPerImageAbove3840x2160PixelsXHighQuality,
		OutputCostPerImageAbove1024x1024PixelsMaxQuality:      pricing.OutputCostPerImageAbove1024x1024PixelsMaxQuality,
		OutputCostPerImageAbove1024x1536PixelsMaxQuality:      pricing.OutputCostPerImageAbove1024x1536PixelsMaxQuality,
		OutputCostPerImageAbove1024x768PixelsMaxQuality:       pricing.OutputCostPerImageAbove1024x768PixelsMaxQuality,
		OutputCostPerImageAbove1920x1080PixelsMaxQuality:      pricing.OutputCostPerImageAbove1920x1080PixelsMaxQuality,
		OutputCostPerImageAbove2560x1440PixelsMaxQuality:      pricing.OutputCostPerImageAbove2560x1440PixelsMaxQuality,
		OutputCostPerImageAbove3840x2160PixelsMaxQuality:      pricing.OutputCostPerImageAbove3840x2160PixelsMaxQuality,
		OutputCostPerImageAbove1024x768PixelsLowQuality:       pricing.OutputCostPerImageAbove1024x768PixelsLowQuality,
		OutputCostPerImageAbove1920x1080PixelsLowQuality:      pricing.OutputCostPerImageAbove1920x1080PixelsLowQuality,
		OutputCostPerImageAbove2560x1440PixelsLowQuality:      pricing.OutputCostPerImageAbove2560x1440PixelsLowQuality,
		OutputCostPerImageAbove3840x2160PixelsLowQuality:      pricing.OutputCostPerImageAbove3840x2160PixelsLowQuality,
		OutputCostPerImageAbove1024x768PixelsMediumQuality:    pricing.OutputCostPerImageAbove1024x768PixelsMediumQuality,
		OutputCostPerImageAbove1920x1080PixelsMediumQuality:   pricing.OutputCostPerImageAbove1920x1080PixelsMediumQuality,
		OutputCostPerImageAbove2560x1440PixelsMediumQuality:   pricing.OutputCostPerImageAbove2560x1440PixelsMediumQuality,
		OutputCostPerImageAbove3840x2160PixelsMediumQuality:   pricing.OutputCostPerImageAbove3840x2160PixelsMediumQuality,
		OutputCostPerImageAbove1024x768PixelsHighQuality:      pricing.OutputCostPerImageAbove1024x768PixelsHighQuality,
		OutputCostPerImageAbove1920x1080PixelsHighQuality:     pricing.OutputCostPerImageAbove1920x1080PixelsHighQuality,
		OutputCostPerImageAbove2560x1440PixelsHighQuality:     pricing.OutputCostPerImageAbove2560x1440PixelsHighQuality,
		OutputCostPerImageAbove3840x2160PixelsHighQuality:     pricing.OutputCostPerImageAbove3840x2160PixelsHighQuality,
		OutputCostPerImageHalfK:                               pricing.OutputCostPerImageHalfK,
		OutputCostPerImage1K:                                  pricing.OutputCostPerImage1K,
		OutputCostPerImage2K:                                  pricing.OutputCostPerImage2K,
		OutputCostPerImage4K:                                  pricing.OutputCostPerImage4K,
		OutputCostPerImageRes512:                              pricing.OutputCostPerImageRes512,
		OutputCostPerImageRes1024:                             pricing.OutputCostPerImageRes1024,
		OutputCostPerImageRes1536:                             pricing.OutputCostPerImageRes1536,
		OutputCostPerImageAbove512x512Pixels50Steps:           pricing.OutputCostPerImageAbove512x512Pixels50Steps,
		OutputCostPerImageAbove512x512PixelsMaxSteps:          pricing.OutputCostPerImageAbove512x512PixelsMaxSteps,
		OutputCostPerImageAbove1024x1024Pixels50Steps:         pricing.OutputCostPerImageAbove1024x1024Pixels50Steps,
		OutputCostPerImageAbove1024x1024PixelsMaxSteps:        pricing.OutputCostPerImageAbove1024x1024PixelsMaxSteps,
		OutputCostPerImageAboveMaxPixels50Steps:               pricing.OutputCostPerImageAboveMaxPixels50Steps,
		OutputCostPerImageAboveMaxPixelsMaxSteps:              pricing.OutputCostPerImageAboveMaxPixelsMaxSteps,

		InputCostPerAudioToken:      pricing.InputCostPerAudioToken,
		InputCostPerAudioPerSecond:  pricing.InputCostPerAudioPerSecond,
		InputCostPerSecond:          pricing.InputCostPerSecond,
		InputCostPerTextMessage:     pricing.InputCostPerTextMessage,
		InputCostPerVideoPerSecond:  pricing.InputCostPerVideoPerSecond,
		OutputCostPerAudioToken:     pricing.OutputCostPerAudioToken,
		OutputCostPerVideoPerSecond: pricing.OutputCostPerVideoPerSecond,
		OutputCostPerSecond:         pricing.OutputCostPerSecond,

		OutputCostPerVideoPerSecond360p:  pricing.OutputCostPerVideoPerSecond360p,
		OutputCostPerVideoPerSecond480p:  pricing.OutputCostPerVideoPerSecond480p,
		OutputCostPerVideoPerSecond540p:  pricing.OutputCostPerVideoPerSecond540p,
		OutputCostPerVideoPerSecond720p:  pricing.OutputCostPerVideoPerSecond720p,
		OutputCostPerVideoPerSecond1024p: pricing.OutputCostPerVideoPerSecond1024p,
		OutputCostPerVideoPerSecond1080p: pricing.OutputCostPerVideoPerSecond1080p,
		OutputCostPerVideoPerSecond4k:    pricing.OutputCostPerVideoPerSecond4k,

		OutputCostPerVideoPerSecond360pAudio:  pricing.OutputCostPerVideoPerSecond360pAudio,
		OutputCostPerVideoPerSecond540pAudio:  pricing.OutputCostPerVideoPerSecond540pAudio,
		OutputCostPerVideoPerSecond720pAudio:  pricing.OutputCostPerVideoPerSecond720pAudio,
		OutputCostPerVideoPerSecond1080pAudio: pricing.OutputCostPerVideoPerSecond1080pAudio,
		OutputCostPerVideoPerSecond4kAudio:    pricing.OutputCostPerVideoPerSecond4kAudio,
		UnmeasuredVideoSeconds:                pricing.UnmeasuredVideoSeconds,
		OutputCostPerSecond480p:               pricing.OutputCostPerSecond480p,
		OutputCostPerSecond720p:               pricing.OutputCostPerSecond720p,
		OutputCostPerSecond768p:               pricing.OutputCostPerSecond768p,
		OutputCostPerSecond1080p:              pricing.OutputCostPerSecond1080p,
		OutputCostPerSecond2k:                 pricing.OutputCostPerSecond2k,
		OutputCostPerSecond4k:                 pricing.OutputCostPerSecond4k,

		InputCostPerVideoToken:                     pricing.InputCostPerVideoToken,
		InputCostPerVideoTokenBatches:              pricing.InputCostPerVideoTokenBatches,
		InputCostPerVideoPerSecondAbove8sInterval:  pricing.InputCostPerVideoPerSecondAbove8sInterval,
		InputCostPerVideoPerSecondAbove15sInterval: pricing.InputCostPerVideoPerSecondAbove15sInterval,
		OutputCostPerVideoToken:                    pricing.OutputCostPerVideoToken,
		OutputCostPerVideo:                         pricing.OutputCostPerVideo,
		OutputCostPerVideo768p6s:                   pricing.OutputCostPerVideo768p6s,
		OutputCostPerVideo768p10s:                  pricing.OutputCostPerVideo768p10s,
		OutputCostPerVideo1080p6s:                  pricing.OutputCostPerVideo1080p6s,
		OutputCostPerVideoPerSecond480pVideoIn:     pricing.OutputCostPerVideoPerSecond480pVideoIn,
		OutputCostPerVideoPerSecond720pVideoIn:     pricing.OutputCostPerVideoPerSecond720pVideoIn,
		OutputCostPerVideoPerSecond1080pVideoIn:    pricing.OutputCostPerVideoPerSecond1080pVideoIn,
		OutputCostPerVideoPerSecond4kVideoIn:       pricing.OutputCostPerVideoPerSecond4kVideoIn,
		OutputCostPerVideoPerSecondPro:             pricing.OutputCostPerVideoPerSecondPro,
		OutputCostPerVideoPerSecondProAudio:        pricing.OutputCostPerVideoPerSecondProAudio,
		OutputCostPerVideoPerSecondStandard:        pricing.OutputCostPerVideoPerSecondStandard,
		OutputCostPerVideoPerSecondStandardAudio:   pricing.OutputCostPerVideoPerSecondStandardAudio,

		SearchContextCostPerQuery:     pricing.SearchContextCostPerQuery,
		WebSearchCostPerRequest:       pricing.WebSearchCostPerRequest,
		InputCostPerQuery:             pricing.InputCostPerQuery,
		CodeInterpreterCostPerSession: pricing.CodeInterpreterCostPerSession,

		GuardrailCostPerContentPolicyUnit:                  pricing.GuardrailCostPerContentPolicyUnit,
		GuardrailCostPerContentPolicyImageUnit:             pricing.GuardrailCostPerContentPolicyImageUnit,
		GuardrailCostPerTopicPolicyUnit:                    pricing.GuardrailCostPerTopicPolicyUnit,
		GuardrailCostPerWordPolicyUnit:                     pricing.GuardrailCostPerWordPolicyUnit,
		GuardrailCostPerSensitiveInformationPolicyUnit:     pricing.GuardrailCostPerSensitiveInformationPolicyUnit,
		GuardrailCostPerSensitiveInformationPolicyFreeUnit: pricing.GuardrailCostPerSensitiveInformationPolicyFreeUnit,
		GuardrailCostPerContextualGroundingPolicyUnit:      pricing.GuardrailCostPerContextualGroundingPolicyUnit,
		GuardrailCostPerAutomatedReasoningPolicyUnit:       pricing.GuardrailCostPerAutomatedReasoningPolicyUnit,
		InferenceGeoUSMultiplier:                           pricing.InferenceGeoUSMultiplier,
		CostPerRequest:                                     pricing.CostPerRequest,

		OCRCostPerPage:        pricing.OCRCostPerPage,
		AnnotationCostPerPage: pricing.AnnotationCostPerPage,

		OffPeakCostMultiplier: pricing.OffPeakCostMultiplier,
		PeakHours:             pricing.PeakHours,
	}
	entry := &Entry{
		BaseModel:            pricing.BaseModel,
		Provider:             pricing.Provider,
		Mode:                 pricing.Mode,
		ContextLength:        pricing.ContextLength,
		MaxInputTokens:       pricing.MaxInputTokens,
		MaxOutputTokens:      pricing.MaxOutputTokens,
		Architecture:         pricing.Architecture,
		IsDeprecated:         pricing.IsDeprecated,
		AdditionalAttributes: pricing.AdditionalAttributes,
		Options:              options,
	}
	return entry
}

// IsEmptyModelCapabilities reports whether no field on the override struct is
// set. Compared against the zero value so new fields need no maintenance here.
func IsEmptyModelCapabilities(ov *schemas.ModelCapabilities) bool {
	if ov == nil {
		return true
	}
	return reflect.DeepEqual(*ov, schemas.ModelCapabilities{})
}

// convertTableOverride converts a TablePricingOverride to an Override.
func convertTableOverride(override *configstoreTables.TablePricingOverride) (Override, error) {
	var options Options
	if err := sonic.Unmarshal([]byte(override.PricingPatchJSON), &options); err != nil {
		return Override{}, err
	}
	return Override{
		ID:            override.ID,
		Name:          override.Name,
		ScopeKind:     ScopeKind(override.ScopeKind),
		UserID:        override.UserID,
		VirtualKeyID:  override.VirtualKeyID,
		ProviderID:    override.ProviderID,
		ProviderKeyID: override.ProviderKeyID,
		MatchType:     MatchType(override.MatchType),
		Pattern:       override.Pattern,
		RequestTypes:  override.RequestTypes,
		Options:       options,
	}, nil
}
