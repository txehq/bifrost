package modelcatalog

import (
	"slices"

	"github.com/capsohq/bifrost/core/schemas"
)

var defaultProviderModels = map[schemas.ModelProvider][]string{
	// Fallback seed models for providers that may not yet exist in the remote datasheet.
	// These are used only when pricing-backed model discovery has no entries.
	schemas.GLM: {
		"glm-5",
		"glm-4.7",
		"glm-4.7x",
		"glm-4.7-flash",
		"glm-4.7-flashx",
		"glm-4.6",
		"glm-4.6v",
		"glm-4.6v-flash",
		"glm-4.5",
		"glm-4.5-air",
		"glm-4.5-airx",
		"glm-z1-air",
		"glm-z1-airx",
		"glm-z1-flash",
		"glm-z1-flashx",
		"glm-z1-thinking",
		"glm-z1-rumination",
	},
	schemas.MiniMax: {
		"MiniMax-M2.5",
		"MiniMax-M2",
		"MiniMax-M2.1",
		"MiniMax-M2.5-lightning",
		"MiniMax-M2.1-lightning",
		"speech-2.6-hd",
		"speech-2.6-turbo",
		"speech-02-hd",
		"speech-02-turbo",
	},
	schemas.DeepSeek: {
		"deepseek-chat",
		"deepseek-reasoner",
	},
	schemas.Moonshot: {
		"kimi-k2.5",
		"kimi-k2-thinking",
		"kimi-k2-thinking-turbo",
		"kimi-latest",
		"kimi-latest-8k",
		"kimi-latest-32k",
		"kimi-latest-128k",
	},
	schemas.Volcengine: {
		"doubao-embedding",
		"doubao-embedding-text-240715",
		"doubao-embedding-large",
		"doubao-embedding-large-text-240915",
		"doubao-embedding-large-text-250515",
		"deepseek-v3-2-251201",
		"glm-4-7-251222",
		"kimi-k2-thinking-251104",
	},
	schemas.ModelArk: {
		"doubao-embedding",
		"doubao-embedding-text-240715",
		"doubao-embedding-large",
		"doubao-embedding-large-text-240915",
		"doubao-embedding-large-text-250515",
		"deepseek-v3-2-251201",
		"glm-4-7-251222",
		"kimi-k2-thinking-251104",
	},
	schemas.Qwen: {
		"qwen-plus-latest",
		"qwen-plus",
		"qwen-turbo-latest",
		"qwen-turbo",
		"qwen-max-latest",
		"qwen3-max-preview",
		"qwen3-coder-plus",
		"qwen3-coder-480b-a35b-instruct",
	},
}

func getDefaultModelsForProvider(provider schemas.ModelProvider) []string {
	models, exists := defaultProviderModels[provider]
	if !exists {
		return nil
	}
	return slices.Clone(models)
}

func appendUniqueModels(target []string, candidates []string) []string {
	if len(candidates) == 0 {
		return target
	}

	seenModels := make(map[string]struct{}, len(target)+len(candidates))
	for _, model := range target {
		seenModels[model] = struct{}{}
	}

	for _, model := range candidates {
		if _, exists := seenModels[model]; exists {
			continue
		}
		seenModels[model] = struct{}{}
		target = append(target, model)
	}

	return target
}
