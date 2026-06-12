// Provides shared replay-policy helpers for provider plugins.
import { normalizeLowercaseStringOrEmpty } from "@openclaw/normalization-core/string-coerce";
import type {
  ProviderReasoningOutputMode,
  ProviderReplayPolicy,
  ProviderReplayPolicyContext,
} from "./types.js";

/** @deprecated Provider replay helper; prefer provider-local replay hooks. */
export function buildOpenAICompatibleReplayPolicy(
  modelApi: string | null | undefined,
  options: {
    sanitizeToolCallIds?: boolean;
    modelId?: string | null;
    dropReasoningFromHistory?: boolean;
  } = {},
): ProviderReplayPolicy | undefined {
  if (
    modelApi !== "openai-completions" &&
    modelApi !== "openai-responses" &&
    modelApi !== "openai-chatgpt-responses" &&
    modelApi !== "azure-openai-responses"
  ) {
    return undefined;
  }

  const sanitizeToolCallIds = options.sanitizeToolCallIds ?? true;
  const dropReasoningFromHistory = options.dropReasoningFromHistory ?? true;
  const isResponsesFamily =
    modelApi === "openai-responses" ||
    modelApi === "openai-chatgpt-responses" ||
    modelApi === "azure-openai-responses";

  return {
    ...(sanitizeToolCallIds
      ? { sanitizeToolCallIds: true, toolCallIdMode: "strict" as const }
      : {}),
    ...(isResponsesFamily ? { allowSyntheticToolResults: true } : {}),
    ...(modelApi === "openai-completions"
      ? {
          applyAssistantFirstOrderingFix: true,
          validateGeminiTurns: true,
          validateAnthropicTurns: true,
        }
      : {
          applyAssistantFirstOrderingFix: false,
          validateGeminiTurns: false,
          validateAnthropicTurns: false,
        }),
    ...(modelApi === "openai-completions" && dropReasoningFromHistory
      ? { dropReasoningFromHistory: true }
      : {}),
  };
}

/** @deprecated Anthropic-family provider replay helper; prefer provider-local replay hooks. */
export function buildStrictAnthropicReplayPolicy(
  options: {
    dropThinkingBlocks?: boolean;
    sanitizeToolCallIds?: boolean;
    preserveNativeAnthropicToolUseIds?: boolean;
  } = {},
): ProviderReplayPolicy {
  const sanitizeToolCallIds = options.sanitizeToolCallIds ?? true;
  return {
    sanitizeMode: "full",
    ...(sanitizeToolCallIds
      ? {
          sanitizeToolCallIds: true,
          toolCallIdMode: "strict" as const,
          ...(options.preserveNativeAnthropicToolUseIds
            ? { preserveNativeAnthropicToolUseIds: true }
            : {}),
        }
      : {}),
    preserveSignatures: true,
    repairToolUseResultPairing: true,
    validateAnthropicTurns: true,
    allowSyntheticToolResults: true,
    ...(options.dropThinkingBlocks ? { dropThinkingBlocks: true } : {}),
  };
}

/**
 * Returns true for Claude models that preserve thinking blocks in context
 * natively (Opus 4.5+, Sonnet 4.5+, Haiku 4.5+). For these models, dropping
 * thinking blocks from prior turns breaks prompt cache prefix matching.
 *
 * See: https://platform.claude.com/docs/en/build-with-claude/extended-thinking#differences-in-thinking-across-model-versions
 *
 * @deprecated Anthropic-family provider replay helper; prefer provider-local replay hooks.
 */
export function shouldPreserveThinkingBlocks(modelId?: string): boolean {
  const id = normalizeLowercaseStringOrEmpty(modelId);
  if (!id.includes("claude")) {
    return false;
  }

  // Models that preserve thinking blocks natively (Claude 4.5+):
  // - claude-opus-4-x (opus-4-5, opus-4-6, ...)
  // - claude-sonnet-4-x (sonnet-4-5, sonnet-4-6, ...)
  //   Note: "sonnet-4" is safe — legacy "claude-3-5-sonnet" does not contain "sonnet-4"
  // - claude-haiku-4-x (haiku-4-5, ...)
  // Models that require dropping thinking blocks:
  // - claude-3-7-sonnet, claude-3-5-sonnet, and earlier
  if (id.includes("opus-4") || id.includes("sonnet-4") || id.includes("haiku-4")) {
    return true;
  }

  // Future-proofing: claude-5-x, claude-6-x etc. should also preserve
  if (/claude-[5-9]/.test(id) || /claude-\d{2,}/.test(id)) {
    return true;
  }

  return false;
}

/** @deprecated Anthropic-family provider replay helper; prefer provider-local replay hooks. */
export function buildAnthropicReplayPolicyForModel(modelId?: string): ProviderReplayPolicy {
  const isClaude = normalizeLowercaseStringOrEmpty(modelId).includes("claude");
  return buildStrictAnthropicReplayPolicy({
    dropThinkingBlocks: isClaude && !shouldPreserveThinkingBlocks(modelId),
  });
}

/** @deprecated Anthropic-family provider replay helper; prefer provider-local replay hooks. */
export function buildNativeAnthropicReplayPolicyForModel(modelId?: string): ProviderReplayPolicy {
  const isClaude = normalizeLowercaseStringOrEmpty(modelId).includes("claude");
  return buildStrictAnthropicReplayPolicy({
    dropThinkingBlocks: isClaude && !shouldPreserveThinkingBlocks(modelId),
    sanitizeToolCallIds: true,
    preserveNativeAnthropicToolUseIds: true,
  });
}

/** @deprecated Provider replay helper; prefer provider-local replay hooks. */
export function buildHybridAnthropicOrOpenAIReplayPolicy(
  ctx: ProviderReplayPolicyContext,
  options: { anthropicModelDropThinkingBlocks?: boolean } = {},
): ProviderReplayPolicy | undefined {
  if (ctx.modelApi === "anthropic-messages") {
    const isClaude = normalizeLowercaseStringOrEmpty(ctx.modelId).includes("claude");
    return buildStrictAnthropicReplayPolicy({
      dropThinkingBlocks:
        options.anthropicModelDropThinkingBlocks &&
        isClaude &&
        !shouldPreserveThinkingBlocks(ctx.modelId),
    });
  }

  return buildOpenAICompatibleReplayPolicy(ctx.modelApi, { modelId: ctx.modelId });
}

/** @deprecated Provider replay helper; prefer provider-local replay hooks. */
export function resolveTaggedReasoningOutputMode(): ProviderReasoningOutputMode {
  return "tagged";
}
