// Slim OpenAI-compatible provider: API key auth plus Chat Completions only.
import type {
  ProviderResolveDynamicModelContext,
  ProviderRuntimeModel,
} from "openclaw/plugin-sdk/plugin-entry";
import { createProviderApiKeyAuthMethod } from "openclaw/plugin-sdk/provider-auth-api-key";
import {
  DEFAULT_CONTEXT_TOKENS,
  normalizeModelCompat,
  normalizeProviderId,
  type ProviderPlugin,
} from "openclaw/plugin-sdk/provider-model-shared";
import { normalizeOptionalString } from "openclaw/plugin-sdk/string-coerce-runtime";
import { resolveOpenAIDefaultBaseUrl } from "./base-url.js";
import { applyOpenAIConfig, OPENAI_DEFAULT_MODEL } from "./default-models.js";

const PROVIDER_ID = "openai";
const OPENAI_API_KEY_LABEL = "OpenAI API Key";

function resolveModelId(ctx: ProviderResolveDynamicModelContext): string {
  const modelId = ctx.modelId.trim();
  if (modelId) {
    return modelId;
  }
  return OPENAI_DEFAULT_MODEL.replace(/^openai\//u, "");
}

function resolveBaseUrl(ctx?: ProviderResolveDynamicModelContext): string {
  return (
    normalizeOptionalString(ctx?.providerConfig?.baseUrl) ??
    normalizeOptionalString(ctx?.config?.models?.providers?.[PROVIDER_ID]?.baseUrl) ??
    resolveOpenAIDefaultBaseUrl()
  );
}

function buildChatCompletionsModel(ctx: ProviderResolveDynamicModelContext): ProviderRuntimeModel {
  const id = resolveModelId(ctx);
  return normalizeModelCompat({
    id,
    name: id,
    provider: PROVIDER_ID,
    api: "openai-completions",
    baseUrl: resolveBaseUrl(ctx),
    input: ["text"],
    contextWindow: DEFAULT_CONTEXT_TOKENS,
    maxTokens: DEFAULT_CONTEXT_TOKENS,
    cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
  } satisfies ProviderRuntimeModel);
}

export function buildOpenAIProvider(): ProviderPlugin {
  return {
    id: PROVIDER_ID,
    label: "OpenAI",
    docsPath: "/providers/openai",
    envVars: ["OPENAI_API_KEY"],
    auth: [
      createProviderApiKeyAuthMethod({
        providerId: PROVIDER_ID,
        methodId: "api-key",
        label: OPENAI_API_KEY_LABEL,
        hint: "Use an OpenAI-compatible API key",
        optionKey: "openaiApiKey",
        flagName: "--openai-api-key",
        envVar: "OPENAI_API_KEY",
        promptMessage: "Enter OpenAI-compatible API key",
        profileId: "openai:api-key",
        defaultModel: OPENAI_DEFAULT_MODEL,
        expectedProviders: [PROVIDER_ID],
        applyConfig: (cfg) => applyOpenAIConfig(cfg),
        wizard: {
          choiceId: "openai-api-key",
          choiceLabel: OPENAI_API_KEY_LABEL,
          choiceHint: "Use an OpenAI-compatible API key",
          assistantPriority: 5,
          groupId: PROVIDER_ID,
          groupLabel: "OpenAI",
          groupHint: "Chat Completions-compatible API key",
        },
      }),
    ],
    resolveDynamicModel: (ctx) =>
      normalizeProviderId(ctx.provider) === PROVIDER_ID
        ? buildChatCompletionsModel(ctx)
        : undefined,
    normalizeResolvedModel: (ctx) =>
      normalizeProviderId(ctx.provider) === PROVIDER_ID
        ? {
            ...ctx.model,
            provider: PROVIDER_ID,
            api: "openai-completions",
            baseUrl: normalizeOptionalString(ctx.model.baseUrl) ?? resolveBaseUrl(),
          }
        : undefined,
    normalizeTransport: (ctx) =>
      normalizeProviderId(ctx.provider) === PROVIDER_ID
        ? {
            api: "openai-completions",
            baseUrl: normalizeOptionalString(ctx.baseUrl) ?? resolveBaseUrl(),
          }
        : undefined,
    buildMissingAuthMessage: (ctx) =>
      normalizeProviderId(ctx.provider) === PROVIDER_ID
        ? 'No API key found for provider "openai". Set OPENAI_API_KEY or configure an OpenAI API-key auth profile.'
        : undefined,
    resolveReasoningOutputMode: () => "text",
  };
}

/** @deprecated The slim fork keeps this export as a source-compatible alias only. */
export function buildOpenAICodexProviderPlugin(): ProviderPlugin {
  return buildOpenAIProvider();
}
