/**
 * Resolves provider/model prompt-cache retention behavior.
 */
import { resolveAnthropicCacheRetentionFamily } from "../../llm/providers/stream-wrappers/anthropic-family-cache-semantics.js";

type CacheRetention = "none" | "short" | "long";

export function resolveCacheRetention(
  extraParams: Record<string, unknown> | undefined,
  provider: string,
  modelApi?: string,
  modelId?: string,
  supportsPromptCacheKey?: boolean,
): CacheRetention | undefined {
  const hasExplicitCacheConfig =
    extraParams?.cacheRetention !== undefined || extraParams?.cacheControlTtl !== undefined;
  const family = resolveAnthropicCacheRetentionFamily({
    provider,
    modelApi,
    modelId,
    hasExplicitCacheConfig,
  });
  // OpenAI-compatible completions backends (oMLX, llama.cpp, etc.) opt into
  // prompt caching via `compat.supportsPromptCacheKey: true`. Without that
  // flag they sit outside the Anthropic family gate, so issue #81281
  // dropped the user's explicit `cacheRetention` before the transport layer
  // could emit it. Proxies that route non-cacheable models via the same
  // openai-completions wire leave the flag unset, so the existing family gate
  // still applies to them.
  const cacheKeyEligible = supportsPromptCacheKey === true;

  if (!family && !cacheKeyEligible) {
    return undefined;
  }

  const newVal = extraParams?.cacheRetention;
  if (newVal === "none" || newVal === "short" || newVal === "long") {
    return newVal;
  }

  const legacy = extraParams?.cacheControlTtl;
  if (legacy === "5m" && family) {
    return "short";
  }
  if (legacy === "1h" && family) {
    return "long";
  }

  return family === "anthropic-direct" ? "short" : undefined;
}
