// Anthropic-family cache wrapper preserves cache-control semantics in tool payloads.
import {
  normalizeLowercaseStringOrEmpty,
  normalizeOptionalLowercaseString,
} from "@openclaw/normalization-core/string-coerce";

type AnthropicCacheRetentionFamily = "anthropic-direct" | "custom-anthropic-api";

export function isAnthropicModelRef(modelId: string): boolean {
  return normalizeLowercaseStringOrEmpty(modelId).startsWith("anthropic/");
}

export function isOpenRouterAnthropicModelRef(provider: string, modelId: string): boolean {
  return (
    normalizeOptionalLowercaseString(provider) === "openrouter" && isAnthropicModelRef(modelId)
  );
}

export function isAnthropicFamilyCacheTtlEligible(params: {
  provider: string;
  modelApi?: string;
  modelId: string;
}): boolean {
  const normalizedProvider = normalizeOptionalLowercaseString(params.provider);
  if (normalizedProvider === "anthropic") {
    return true;
  }
  return params.modelApi === "anthropic-messages";
}

export function resolveAnthropicCacheRetentionFamily(params: {
  provider: string;
  modelApi?: string;
  modelId?: string;
  hasExplicitCacheConfig: boolean;
}): AnthropicCacheRetentionFamily | undefined {
  const normalizedProvider = normalizeOptionalLowercaseString(params.provider);
  if (normalizedProvider === "anthropic") {
    return "anthropic-direct";
  }
  if (params.hasExplicitCacheConfig && params.modelApi === "anthropic-messages") {
    return "custom-anthropic-api";
  }
  return undefined;
}
