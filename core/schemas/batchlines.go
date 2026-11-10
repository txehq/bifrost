package schemas

import (
	"fmt"
	"strings"
)

// An inline batch line's body is the OpenAI shape of the line's url, the body the sync
// route takes for the same operation. The functions here build, from a line, the request
// the sync route builds from that body, so a provider converts a batch line with the sync
// operation's converter and the two routes send the vendor the same parameters.
//
// The known-field tables say which top-level fields of a body the unified request models;
// every other field is an extra param. The HTTP transport's sync routes read the same
// tables, so the two routes cannot disagree about what is an extra param.

// ChatRequestKnownFields are the top-level fields of an OpenAI-shape chat completion body
// that are not extra params.
var ChatRequestKnownFields = map[string]bool{
	"model":                  true,
	"messages":               true,
	"fallbacks":              true,
	"stream":                 true,
	"frequency_penalty":      true,
	"logit_bias":             true,
	"logprobs":               true,
	"max_completion_tokens":  true,
	"metadata":               true,
	"modalities":             true,
	"parallel_tool_calls":    true,
	"presence_penalty":       true,
	"prompt_cache_key":       true,
	"prompt_cache_retention": true,
	"reasoning":              true,
	"reasoning_effort":       true,
	"reasoning_max_tokens":   true,
	"response_format":        true,
	"safety_identifier":      true,
	"service_tier":           true,
	"stream_options":         true,
	"store":                  true,
	"temperature":            true,
	"tool_choice":            true,
	"tools":                  true,
	"truncation":             true,
	"user":                   true,
	"verbosity":              true,

	"include_server_side_tool_invocations": true,
}

// ImageGenerationRequestKnownFields are the top-level fields of an OpenAI-shape image
// generation body that are not extra params (ImageGenerationInput and
// ImageGenerationParameters).
var ImageGenerationRequestKnownFields = map[string]bool{
	"model":               true,
	"prompt":              true,
	"fallbacks":           true,
	"stream":              true,
	"n":                   true,
	"background":          true,
	"moderation":          true,
	"partial_images":      true,
	"size":                true,
	"quality":             true,
	"output_compression":  true,
	"output_format":       true,
	"style":               true,
	"response_format":     true,
	"seed":                true,
	"negative_prompt":     true,
	"num_inference_steps": true,
	"user":                true,
	"aspect_ratio":        true,
	"input_images":        true,
	"type":                true,
}

// BatchLineError is a batch line the caller got wrong: a missing body, a missing required
// field, a url the provider cannot run in a batch. Providers answer it with a 400 naming
// the line.
type BatchLineError struct {
	Index    int    // Position of the line in requests
	CustomID string // The line's custom_id
	Param    string // The request field at fault, as a path under requests[i]
	Message  string
}

func (e *BatchLineError) Error() string {
	return fmt.Sprintf("batch request item %d (custom_id %q): %s", e.Index, e.CustomID, e.Message)
}

// RequestBody returns the line's body: Body, or Params for callers that use Anthropic's
// name for it.
func (item BatchRequestItem) RequestBody() map[string]any {
	if item.Body != nil {
		return item.Body
	}
	return item.Params
}

// Endpoint returns the operation the line runs: its own url, or the batch's endpoint when
// the line names none.
func (item BatchRequestItem) Endpoint(batchEndpoint BatchEndpoint) BatchEndpoint {
	if url := strings.TrimSpace(item.URL); url != "" {
		return BatchEndpoint(url)
	}
	return batchEndpoint
}

// batchLineExtraParams returns the fields of body the unified request does not model.
func batchLineExtraParams(body map[string]any, known map[string]bool) map[string]any {
	extraParams := make(map[string]any)
	for key, value := range body {
		if !known[key] {
			extraParams[key] = value
		}
	}
	return extraParams
}

// ToBifrostChatRequest builds the chat request the sync /v1/chat/completions route builds
// from the line's body: messages, the chat parameters (with the legacy max_tokens read as
// max_completion_tokens) and every unmodelled field as an extra param. provider and model
// are the batch's: a batch runs one model.
func (item BatchRequestItem) ToBifrostChatRequest(provider ModelProvider, model string) (*BifrostChatRequest, error) {
	body := item.RequestBody()
	if body == nil {
		return nil, fmt.Errorf("body is required")
	}
	encoded, err := Marshal(body)
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Messages []ChatMessage `json:"messages"`
	}
	if err := Unmarshal(encoded, &parsed); err != nil {
		return nil, fmt.Errorf("invalid messages: %w", err)
	}
	if len(parsed.Messages) == 0 {
		return nil, fmt.Errorf("messages is required for chat completion")
	}
	params := &ChatParameters{}
	if err := Unmarshal(encoded, params); err != nil {
		return nil, fmt.Errorf("invalid chat parameters: %w", err)
	}
	extraParams := batchLineExtraParams(body, ChatRequestKnownFields)
	if maxTokens, exists := extraParams["max_tokens"]; exists {
		delete(extraParams, "max_tokens")
		if params.MaxCompletionTokens == nil {
			switch value := maxTokens.(type) {
			case float64:
				params.MaxCompletionTokens = Ptr(int(value))
			case int:
				params.MaxCompletionTokens = Ptr(value)
			case int64:
				params.MaxCompletionTokens = Ptr(int(value))
			}
		}
	}
	params.ExtraParams = extraParams
	return &BifrostChatRequest{
		Provider: provider,
		Model:    model,
		Input:    parsed.Messages,
		Params:   params,
	}, nil
}

// ToBifrostImageGenerationRequest builds the image generation request the sync
// /v1/images/generations route builds from the line's body.
func (item BatchRequestItem) ToBifrostImageGenerationRequest(provider ModelProvider, model string) (*BifrostImageGenerationRequest, error) {
	body := item.RequestBody()
	if body == nil {
		return nil, fmt.Errorf("body is required")
	}
	encoded, err := Marshal(body)
	if err != nil {
		return nil, err
	}
	input := &ImageGenerationInput{}
	if err := Unmarshal(encoded, input); err != nil {
		return nil, fmt.Errorf("invalid image generation input: %w", err)
	}
	if input.Prompt == "" {
		return nil, fmt.Errorf("prompt cannot be empty")
	}
	params := &ImageGenerationParameters{}
	if err := Unmarshal(encoded, params); err != nil {
		return nil, fmt.Errorf("invalid image generation parameters: %w", err)
	}
	params.ExtraParams = batchLineExtraParams(body, ImageGenerationRequestKnownFields)
	return &BifrostImageGenerationRequest{
		Provider: provider,
		Model:    model,
		Input:    input,
		Params:   params,
	}, nil
}
