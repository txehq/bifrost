import { ProviderKeyConfig, CustomProviderConfig } from '../../core/fixtures/test-data.fixture'

/**
 * Factory function to create provider key test data
 */
export function createProviderKeyData(overrides: Partial<ProviderKeyConfig> = {}): ProviderKeyConfig {
  const timestamp = Date.now()
  return {
    name: `Test Key ${timestamp}`,
    value: `sk-test-${timestamp}-${Math.random().toString(36).substring(7)}`,
    models: ['*'],
    weight: 1.0,
    ...overrides,
  }
}

/**
 * Factory function to create custom provider test data
 */
export function createCustomProviderData(overrides: Partial<CustomProviderConfig> = {}): CustomProviderConfig {
  const timestamp = Date.now()
  return {
    name: `test-provider-${timestamp}`,
    baseProviderType: 'openai',
    baseUrl: 'https://api.example.com',
    isKeyless: false,
    ...overrides,
  }
}

/**
 * Known provider names for testing
 */
export const KNOWN_PROVIDERS = [
  'openai',
  'anthropic',
  'gemini',
  'cohere',
  'bedrock',
  'azure',
  'vertex',
  'groq',
  'mistral',
  'deepseek',
  'qwen',
  'cerebras',
  'nebius',
  'sambanova',
] as const

/**
 * Sample API keys for testing (fake values)
 */
export const SAMPLE_API_KEYS = {
  openai: 'sk-test-openai-key-12345678901234567890',
  anthropic: 'sk-ant-test-key-12345678901234567890',
  gemini: 'test-gemini-api-key-1234567890',
}

/**
 * Sample models for each provider
 */
export const SAMPLE_MODELS = {
  openai: ['gpt-4', 'gpt-4-turbo', 'gpt-3.5-turbo'],
  anthropic: ['claude-3-opus', 'claude-3-sonnet', 'claude-3-haiku'],
  gemini: ['gemini-pro', 'gemini-pro-vision'],
}

/**
 * Providers added in this fork that must remain available in UI flows.
 */
export const NEWLY_ADDED_PROVIDERS = ['glm', 'minimax', 'moonshot', 'volcengine', 'deepseek', 'qwen'] as const

/**
 * Fork providers registered before their operations exist, with the label the UI shows.
 * Each must be addable from the "Add New Provider" dropdown and take a plain API key.
 */
export const REGISTRATION_ONLY_PROVIDERS = [
  { name: 'dashscope', label: 'Alibaba DashScope' },
  { name: 'bfl', label: 'Black Forest Labs' },
] as const

/**
 * Seed model canaries used to verify provider model catalog population.
 */
export const NEW_PROVIDER_MODEL_CANARIES: Partial<Record<(typeof NEWLY_ADDED_PROVIDERS)[number], string>> = {
  minimax: 'MiniMax-M2.5',
  moonshot: 'kimi-latest',
  volcengine: 'doubao-embedding',
  deepseek: 'deepseek/deepseek-chat',
}
