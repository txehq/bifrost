package perplexity

import (
	"strings"

	"github.com/capsohq/bifrost/core/schemas"
)

// PerplexityChatRequest represents a Perplexity chat completion request
type PerplexityChatRequest struct {
	Model                   string                  `json:"model"`                                // Required: Model to use for chat completion
	Messages                []schemas.ChatMessage   `json:"messages"`                             // Required: Array of message objects
	SearchMode              *string                 `json:"search_mode"`                          // Required: Search mode
	ReasoningEffort         *string                 `json:"reasoning_effort"`                     // Required: Reasoning effort (low, medium, high)
	MaxTokens               *int                    `json:"max_tokens,omitempty"`                 // Optional: Maximum tokens to generate
	Temperature             *float64                `json:"temperature,omitempty"`                // Optional: Sampling temperature
	TopP                    *float64                `json:"top_p,omitempty"`                      // Optional: Top-p sampling
	LanguagePreference      *string                 `json:"language_preference,omitempty"`        // Optional: Language preference
	SearchDomainFilter      []string                `json:"search_domain_filter,omitempty"`       // Optional: Search domain filter
	ReturnImages            *bool                   `json:"return_images,omitempty"`              // Optional: Return images
	ReturnRelatedQuestions  *bool                   `json:"return_related_questions,omitempty"`   // Optional: Return related questions
	SearchRecencyFilter     *string                 `json:"search_recency_filter,omitempty"`      // Optional: Search recency filter
	SearchAfterDateFilter   *string                 `json:"search_after_date_filter,omitempty"`   // Optional: Search after date filter
	SearchBeforeDateFilter  *string                 `json:"search_before_date_filter,omitempty"`  // Optional: Search before date filter
	LastUpdatedAfterFilter  *string                 `json:"last_updated_after_filter,omitempty"`  // Optional: Last updated after filter
	LastUpdatedBeforeFilter *string                 `json:"last_updated_before_filter,omitempty"` // Optional: Last updated before filter
	TopK                    *int                    `json:"top_k,omitempty"`                      // Optional: Top-k sampling
	Stream                  *bool                   `json:"stream,omitempty"`                     // Optional: Enable streaming
	PresencePenalty         *float64                `json:"presence_penalty,omitempty"`           // Optional: Presence penalty
	FrequencyPenalty        *float64                `json:"frequency_penalty,omitempty"`          // Optional: Frequency penalty
	ResponseFormat          *interface{}            `json:"response_format,omitempty"`            // Format for the response
	DisableSearch           *bool                   `json:"disable_search,omitempty"`             // Optional: Disable search
	EnableSearchClassifier  *bool                   `json:"enable_search_classifier,omitempty"`   // Optional: Enable search classifier
	WebSearchOptions        []WebSearchOption       `json:"web_search_options,omitempty"`         // Optional: Web search options
	MediaResponse           *MediaResponse          `json:"media_response,omitempty"`             // Optional: Media response
	Tools                   []schemas.ChatTool      `json:"tools,omitempty"`                      // Optional: Tools available for the model
	ToolChoice              *schemas.ChatToolChoice `json:"tool_choice,omitempty"`                // Optional: Whether to call a tool
	ParallelToolCalls       *bool                   `json:"parallel_tool_calls,omitempty"`        // Optional: Enable parallel tool calls
	Stop                    []string                `json:"stop,omitempty"`                       // Optional: Stop sequences
	LogProbs                *bool                   `json:"logprobs,omitempty"`                   // Optional: Return log probabilities
	TopLogProbs             *int                    `json:"top_logprobs,omitempty"`               // Optional: Number of top log probabilities
	NumSearchResults        *int                    `json:"num_search_results,omitempty"`         // Optional: Number of search results
	NumImages               *int                    `json:"num_images,omitempty"`                 // Optional: Number of images
	SearchLanguageFilter    []string                `json:"search_language_filter,omitempty"`     // Optional: Search language filter
	ImageFormatFilter       []string                `json:"image_format_filter,omitempty"`        // Optional: Image format filter
	ImageDomainFilter       []string                `json:"image_domain_filter,omitempty"`        // Optional: Image domain filter
	SafeSearch              *bool                   `json:"safe_search,omitempty"`                // Optional: Enable safe search
	StreamMode              *string                 `json:"stream_mode,omitempty"`                // Optional: Stream mode
	ExtraParams             map[string]interface{}  `json:"-"`
}

// GetExtraParams implements the RequestBodyWithExtraParams interface
func (r *PerplexityChatRequest) GetExtraParams() map[string]interface{} {
	return r.ExtraParams
}

type WebSearchOption struct {
	SearchContextSize             *string                      `json:"search_context_size,omitempty"`              // "low" | "medium" | "high"
	UserLocation                  *WebSearchOptionUserLocation `json:"user_location,omitempty"`                    // The approximate location of the user
	ImageResultsEnhancedRelevance *bool                        `json:"image_results_enhanced_relevance,omitempty"` // Optional: Image results enhanced relevance
	SearchType                    *string                      `json:"search_type,omitempty"`                      // Optional: "general" | "news" | "academic" | "social" | "writing"
}

type WebSearchOptionUserLocation struct {
	Latitude  *float64 `json:"latitude,omitempty"`
	Longitude *float64 `json:"longitude,omitempty"`
	City      *string  `json:"city,omitempty"`    // Free text input for the city
	Country   *string  `json:"country,omitempty"` // Two-letter ISO country code
	Region    *string  `json:"region,omitempty"`  // Free text input for the region
}

type MediaResponse struct {
	Overrides MediaResponseOverrides `json:"overrides,omitempty"` // Optional: Overrides for the media response
}

type MediaResponseOverrides struct {
	ReturnVideos *bool `json:"return_videos,omitempty"` // Optional: Return videos
	ReturnImages *bool `json:"return_images,omitempty"` // Optional: Return images
}

type PerplexityChatResponse struct {
	ID            string                          `json:"id"`
	Choices       []schemas.BifrostResponseChoice `json:"choices"`
	Created       int                             `json:"created"` // The Unix timestamp (in seconds).
	Model         string                          `json:"model"`
	Object        string                          `json:"object"` // "chat.completion" or "chat.completion.chunk"
	Citations     []string                        `json:"citations,omitempty"`
	SearchResults []schemas.SearchResult          `json:"search_results,omitempty"`
	Videos        []schemas.VideoResult           `json:"videos,omitempty"`
	Usage         *Usage                          `json:"usage,omitempty"`
}

type Usage struct {
	PromptTokens      int                                  `json:"prompt_tokens"`
	CompletionTokens  int                                  `json:"completion_tokens"`
	TotalTokens       int                                  `json:"total_tokens"`
	SearchContextSize *string                              `json:"search_context_size,omitempty"`
	CitationTokens    *int                                 `json:"citation_tokens,omitempty"`
	NumSearchQueries  *int                                 `json:"num_search_queries,omitempty"`
	ReasoningTokens   *int                                 `json:"reasoning_tokens,omitempty"`
	Cost              *perplexityCost                      `json:"cost,omitempty"`
	ToolCallsDetails  map[string]PerplexityToolCallDetails `json:"tool_calls_details,omitempty"` // Per-tool call report, keyed by Perplexity's tool name
}

// PerplexityToolCallDetails is one entry of usage.tool_calls_details. Only the call count is read.
type PerplexityToolCallDetails struct {
	Invocation int `json:"invocation"`
}

// toBifrostUsage maps Perplexity's usage onto the unified usage. The non-streamed response and
// every streamed frame go through it, so a stream's final usage equals the non-streamed one
// (contract C10). A field Perplexity did not report stays unset (contract C5).
// SearchContextSize is the tier Perplexity says it billed the request at; its per-request search
// fee is priced by it, so it is copied from the vendor's usage, never from the request (TXE-6549).
func (u *Usage) toBifrostUsage() *schemas.BifrostLLMUsage {
	if u == nil {
		return nil
	}
	usage := &schemas.BifrostLLMUsage{
		PromptTokens:      u.PromptTokens,
		CompletionTokens:  u.CompletionTokens,
		TotalTokens:       u.TotalTokens,
		SearchContextSize: u.SearchContextSize,
		Cost:              u.Cost.toBifrostCost(),
		VendorCost:        u.Cost.toVendorCost(),
	}

	if u.CitationTokens != nil || u.NumSearchQueries != nil || u.ReasoningTokens != nil {
		details := &schemas.ChatCompletionTokensDetails{
			CitationTokens:   u.CitationTokens,
			NumSearchQueries: u.NumSearchQueries,
		}
		if u.ReasoningTokens != nil {
			details.ReasoningTokens = *u.ReasoningTokens
		}
		usage.CompletionTokensDetails = details
	}

	if u.NumSearchQueries != nil || len(u.ToolCallsDetails) > 0 {
		toolUsage := &schemas.ToolUsage{}
		if u.NumSearchQueries != nil {
			toolUsage.WebSearch = &schemas.WebSearchToolUsage{NumRequests: *u.NumSearchQueries}
		}
		if len(u.ToolCallsDetails) > 0 {
			toolUsage.PerTool = make(map[string]schemas.ToolRequestUsage, len(u.ToolCallsDetails))
			for tool, details := range u.ToolCallsDetails {
				toolUsage.PerTool[tool] = schemas.ToolRequestUsage{NumRequests: details.Invocation}
			}
		}
		usage.ToolUsage = toolUsage
	}

	return usage
}

// PerplexityStreamFrame decodes one streamed chat.completion.chunk. Everything but usage decodes straight
// into the embedded unified response, as the shared OpenAI-compatible stream does; usage is
// shadowed so Perplexity's own usage shape is read instead of the unified one, which has no
// place for its top-level citation_tokens and num_search_queries or its flat cost names.
type PerplexityStreamFrame struct {
	*schemas.BifrostChatResponse
	Usage *Usage `json:"usage,omitempty"`
}

// perplexityCost is Perplexity's raw cost object, a flat per-category shape. It
// is mapped into the neutral schemas.BifrostCost via toBifrostCost rather than
// deserialized into it directly, keeping the provider wire format out of the
// core schema.
type perplexityCost struct {
	InputTokensCost     float64  `json:"input_tokens_cost"`
	OutputTokensCost    float64  `json:"output_tokens_cost"`
	ReasoningTokensCost float64  `json:"reasoning_tokens_cost"`
	CitationTokensCost  float64  `json:"citation_tokens_cost"`
	SearchQueriesCost   float64  `json:"search_queries_cost"`
	RequestCost         float64  `json:"request_cost"`
	TotalCost           *float64 `json:"total_cost"` // Pointer so a reported zero is told apart from no total
}

// toVendorCost is the total Perplexity says it charged, in US dollars, copied and never summed
// from the categories (fork, TXE-6987, contract C5): nil when Perplexity sent no total_cost.
func (c *perplexityCost) toVendorCost() *schemas.VendorCost {
	if c == nil || c.TotalCost == nil {
		return nil
	}
	return &schemas.VendorCost{Amount: *c.TotalCost, Unit: schemas.VendorCostUnitUSD}
}

// toBifrostCost maps Perplexity's flat cost onto the nested BifrostCost: the flat
// request surcharge folds into the input side; reasoning/citation/search are
// output-side categories.
func (c *perplexityCost) toBifrostCost() *schemas.BifrostCost {
	if c == nil {
		return nil
	}
	inputCost := c.InputTokensCost + c.RequestCost
	outputCost := c.OutputTokensCost + c.ReasoningTokensCost + c.CitationTokensCost + c.SearchQueriesCost
	total := inputCost + outputCost
	if c.TotalCost != nil && *c.TotalCost != 0 {
		total = *c.TotalCost
	}
	bc := &schemas.BifrostCost{InputCost: inputCost, OutputCost: outputCost, TotalCost: total}
	if inputCost != 0 {
		bc.InputCostDetails = &schemas.InputCostDetails{
			TextCost:    c.InputTokensCost,
			RequestCost: c.RequestCost,
		}
	}
	if outputCost != 0 {
		bc.OutputCostDetails = &schemas.OutputCostDetails{
			TextCost:          c.OutputTokensCost,
			ReasoningCost:     c.ReasoningTokensCost,
			CitationCost:      c.CitationTokensCost,
			SearchQueriesCost: c.SearchQueriesCost,
		}
	}
	return bc
}

// PerplexityResponsesUsage holds the parts of the Agent API's ResponsesUsage the shared Responses decode has no field for.
type PerplexityResponsesUsage struct {
	ToolCallsDetails map[string]PerplexityToolCallDetails `json:"tool_calls_details,omitempty"` // Per-tool call report, keyed by Perplexity's tool name
	Cost             *PerplexityResponsesCost             `json:"cost,omitempty"`
}

// PerplexityResponsesCost holds the parts of the Agent API's ResponsesCost read for vendor_cost.
type PerplexityResponsesCost struct {
	Currency  string   `json:"currency"`
	TotalCost *float64 `json:"total_cost"`
}

// perplexityWebSearchTool is the key Perplexity reports its web search tool under in tool_calls_details.
const perplexityWebSearchTool = "web_search"

// applyTo adds what Perplexity reported to the usage the shared decode produced (fork, TXE-6987, contract
// C5 and C10): the per-tool call counts, the web search count, and the vendor's own total as vendor_cost.
// The shared decode already reads tokens and cost.input_cost, output_cost and total_cost. A field
// Perplexity did not report stays unset, and a total in a currency other than US dollars is not copied,
// because vendor_cost has no unit for it.
func (u *PerplexityResponsesUsage) applyTo(usage *schemas.ResponsesResponseUsage) {
	if u == nil || usage == nil {
		return
	}
	if len(u.ToolCallsDetails) > 0 {
		if usage.ToolUsage == nil {
			usage.ToolUsage = &schemas.ToolUsage{}
		}
		usage.ToolUsage.PerTool = make(map[string]schemas.ToolRequestUsage, len(u.ToolCallsDetails))
		for tool, details := range u.ToolCallsDetails {
			usage.ToolUsage.PerTool[tool] = schemas.ToolRequestUsage{NumRequests: details.Invocation}
		}
		if webSearch, ok := u.ToolCallsDetails[perplexityWebSearchTool]; ok {
			usage.ToolUsage.WebSearch = &schemas.WebSearchToolUsage{NumRequests: webSearch.Invocation}
		}
	}
	if c := u.Cost; c != nil && c.TotalCost != nil && (c.Currency == "" || strings.EqualFold(c.Currency, "USD")) {
		usage.VendorCost = &schemas.VendorCost{Amount: *c.TotalCost, Unit: schemas.VendorCostUnitUSD}
	}
}
