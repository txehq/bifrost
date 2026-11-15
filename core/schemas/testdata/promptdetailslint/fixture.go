// Fixture for TestPromptDetailsLintFixtures (TXE-7503). Never compiled: the lint reads it as
// syntax. Every line ending in `// want` must be reported, and no other line.
package fixture

import "github.com/capsohq/bifrost/core/schemas"

// buildOAIResponsesUsage as it was on main before TXE-7361 (PR #303): the hand copy that
// dropped the cached modality split on the OpenAI, Azure and Vertex Responses passthrough.
func buildOAIResponsesUsage(ru *schemas.ResponsesResponseUsage, serviceTier *string) *schemas.BifrostPassthroughUsage {
	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     ru.InputTokens,
		CompletionTokens: ru.OutputTokens,
		TotalTokens:      ru.TotalTokens,
	}
	if ru.InputTokensDetails != nil {
		usage.PromptTokensDetails = &schemas.ChatPromptTokensDetails{ // want
			CachedReadTokens:  ru.InputTokensDetails.CachedReadTokens,
			CachedWriteTokens: ru.InputTokensDetails.CachedWriteTokens,
		}
	}
	return &schemas.BifrostPassthroughUsage{LLMUsage: usage}
}

// vertexNormalizeBatchUsage as it was on main before TXE-7361: the same hand copy in JSON
// form, which kept only cached_tokens on a Vertex batch result line.
func vertexNormalizeBatchUsage(body map[string]any, meta gemini.GenerateContentResponseUsageMetadata) map[string]any {
	usage := map[string]any{
		"prompt_tokens":     meta.PromptTokenCount,
		"completion_tokens": meta.CandidatesTokenCount + meta.ThoughtsTokenCount,
		"total_tokens":      meta.TotalTokenCount,
	}
	if meta.ThoughtsTokenCount > 0 {
		usage["completion_tokens_details"] = map[string]any{
			"reasoning_tokens": meta.ThoughtsTokenCount,
		}
	}
	if meta.CachedContentTokenCount > 0 {
		usage["prompt_tokens_details"] = map[string]any{ // want
			"cached_tokens": meta.CachedContentTokenCount,
		}
	}
	body["usage"] = usage
	return body
}

// The same JSON copy written as a nested literal.
func nestedJSONCopy(cached int) map[string]any {
	return map[string]any{
		"usage": map[string]any{
			"input_tokens_details": map[string]any{"cached_tokens": cached}, // want
		},
	}
}

// Chat to Responses by hand, through a local alias.
func aliasCopy(usage *schemas.BifrostLLMUsage) *schemas.ResponsesResponseUsage {
	out := &schemas.ResponsesResponseUsage{}
	if d := usage.PromptTokensDetails; d != nil {
		out.InputTokensDetails = &schemas.ResponsesResponseInputTokens{ // want
			TextTokens:       d.TextTokens,
			CachedReadTokens: d.CachedReadTokens,
		}
	}
	return out
}

// A parameter of a prompt-details type is a prompt-details value too, and an unqualified
// type name (inside package schemas) is the same type.
func paramCopy(d *ResponsesResponseInputTokens) ChatPromptTokensDetails {
	return ChatPromptTokensDetails{AudioTokens: d.AudioTokens} // want
}

// Field by field into an empty struct is the same copy.
func fieldByField(in *schemas.ResponsesResponseUsage, out *schemas.BifrostLLMUsage) {
	out.PromptTokensDetails = &schemas.ChatPromptTokensDetails{}
	out.PromptTokensDetails.CachedReadTokens = in.InputTokensDetails.CachedReadTokens    // want
	out.PromptTokensDetails.CachedWriteTokens += in.InputTokensDetails.CachedWriteTokens // want
}

// Not reported below.

// The shared conversions.
func shared(ru *schemas.ResponsesResponseUsage, usage *schemas.BifrostLLMUsage) {
	usage.PromptTokensDetails = ru.InputTokensDetails.ToChatPromptTokensDetails()
	ru.InputTokensDetails = usage.PromptTokensDetails.ToResponsesInputTokens()
	usage.PromptTokensDetails.Add(ru.InputTokensDetails.ToChatPromptTokensDetails())
}

// Built from the vendor's own fields (Anthropic cache read and write), not copied.
func vendorNative(au anthropicUsage) *schemas.ChatPromptTokensDetails {
	return &schemas.ChatPromptTokensDetails{
		CachedReadTokens:  au.CacheReadInputTokens,
		CachedWriteTokens: au.CacheCreationInputTokens,
	}
}

// An empty literal, and one built from locals.
func emptyAndLocals(usage *schemas.BifrostLLMUsage, read, write int) {
	usage.PromptTokensDetails = &schemas.ChatPromptTokensDetails{}
	usage.PromptTokensDetails = &schemas.ChatPromptTokensDetails{CachedReadTokens: read, CachedWriteTokens: write}
}

// Adjusting a value in place is not a copy from another one.
func inPlace(usage *schemas.BifrostLLMUsage) {
	usage.PromptTokensDetails.TextTokens = usage.PromptTokensDetails.TextTokens - usage.PromptTokensDetails.CachedReadTokens
}

// Reading the wire key is not building it.
func readsJSON(body map[string]any) any {
	usage, _ := body["usage"].(map[string]any)
	return usage["prompt_tokens_details"]
}
