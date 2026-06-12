// Provider stream shared helpers implement reusable stream wrappers and payload policies.
import { randomUUID } from "node:crypto";
import {
  extractStandalonePlainTextToolCallText,
  normalizePlainTextToolCallStreamEvents,
  promoteStandalonePlainTextToolCallMessage,
  scrubOverCapPlainTextToolCallMessage,
  type PlainTextToolCallNameMatcher,
  type PlainTextToolCallMessageNormalization,
} from "../../packages/tool-call-repair/src/index.js";
import type { StreamFn } from "../agents/runtime/index.js";
import { streamWithPayloadPatch } from "../llm/providers/stream-wrappers/stream-payload-utils.js";
import { streamSimple } from "../llm/stream.js";
import { createAssistantMessageEventStream } from "../llm/utils/event-stream.js";
import type { ProviderWrapStreamFnContext } from "./plugin-entry.js";

/** Optional provider stream decorator factory used by shared provider wrappers. */
export type ProviderStreamWrapperFactory =
  /** Wrapper factory that can decorate, replace, or omit a provider stream function. */
  ((streamFn: StreamFn | undefined) => StreamFn | undefined) | null | undefined | false;

/** Compose stream wrapper factories from left to right around a base stream function. */
export function composeProviderStreamWrappers(
  /** Base provider stream function to pass through the wrapper chain. */
  baseStreamFn: StreamFn | undefined,
  /** Ordered wrapper factories; falsey entries are skipped. */
  ...wrappers: ProviderStreamWrapperFactory[]
): StreamFn | undefined {
  return wrappers.reduce(
    (streamFn, wrapper) => (wrapper ? wrapper(streamFn) : streamFn),
    baseStreamFn,
  );
}

function toRecord(value: unknown): Record<string, unknown> | undefined {
  return value && typeof value === "object" ? (value as Record<string, unknown>) : undefined;
}

function resolveContextToolNames(context: Parameters<StreamFn>[1]): Set<string> {
  const tools = (context as { tools?: unknown }).tools;
  if (!Array.isArray(tools)) {
    return new Set();
  }
  const names = tools
    .map((tool) => {
      const record = toRecord(tool);
      return typeof record?.name === "string" && record.name.trim() ? record.name : undefined;
    })
    .filter((name): name is string => Boolean(name));
  return new Set(names);
}

function createSyntheticToolCallId(): string {
  return `call_${randomUUID().replace(/-/g, "").slice(0, 24)}`;
}

function createPlainTextToolCallBlock(parsed: {
  arguments: Record<string, unknown>;
  name: string;
}): Record<string, unknown> {
  return {
    type: "toolCall",
    id: createSyntheticToolCallId(),
    name: parsed.name,
    arguments: parsed.arguments,
    partialArgs: JSON.stringify(parsed.arguments),
  };
}

function promotePlainTextToolCalls(
  message: unknown,
  toolNames: Set<string>,
): Record<string, unknown> | undefined {
  const messageRecord = toRecord(message);
  if (
    Array.isArray(messageRecord?.content) &&
    messageRecord.content.some((block) => toRecord(block)?.type === "toolCall")
  ) {
    return undefined;
  }
  return promoteStandalonePlainTextToolCallMessage({
    allowedToolNames: toolNames,
    createToolCallBlock: (block, name) => createPlainTextToolCallBlock({ ...block, name }),
    isRetainableNonTextBlock: () => true,
    message,
  });
}

function emitPromotedToolCallEvents(
  stream: { push(event: unknown): void },
  message: Record<string, unknown>,
): void {
  const content = Array.isArray(message.content) ? message.content : [];
  content.forEach((block, contentIndex) => {
    const record = toRecord(block);
    if (record?.type !== "toolCall") {
      return;
    }
    stream.push({ type: "toolcall_start", contentIndex, partial: message });
    stream.push({
      type: "toolcall_delta",
      contentIndex,
      delta: typeof record.partialArgs === "string" ? record.partialArgs : "{}",
      partial: message,
    });
  });
}

function extractPlainTextToolCallCandidate(message: unknown): string | undefined {
  return extractStandalonePlainTextToolCallText({
    allowOtherNonTextBlocks: true,
    message,
  });
}

function createProviderToolNameMatcher(toolNames: Set<string>): PlainTextToolCallNameMatcher {
  return {
    hasExactName: (name) => toolNames.has(name),
    hasNamePrefix: (prefix) => {
      for (const toolName of toolNames) {
        if (toolName.startsWith(prefix)) {
          return true;
        }
      }
      return false;
    },
  };
}

function normalizeProviderDoneMessage(
  message: unknown,
  toolNames: Set<string>,
  matcher: PlainTextToolCallNameMatcher,
): PlainTextToolCallMessageNormalization {
  const scrubbedMessage = scrubOverCapPlainTextToolCallMessage({
    candidateText: extractPlainTextToolCallCandidate(message),
    matcher,
    message,
  });
  if (scrubbedMessage) {
    return { kind: "scrubbed", message: scrubbedMessage };
  }
  const promotedMessage = promotePlainTextToolCalls(message, toolNames);
  return promotedMessage ? { kind: "promoted", message: promotedMessage } : undefined;
}

function wrapPlainTextToolCallStream(
  source: ReturnType<StreamFn>,
  context: Parameters<StreamFn>[1],
): ReturnType<StreamFn> {
  const toolNames = resolveContextToolNames(context);
  if (toolNames.size === 0) {
    return source;
  }
  const matcher = createProviderToolNameMatcher(toolNames);
  const output = createAssistantMessageEventStream();
  const stream = output as unknown as { push(event: unknown): void; end(): void };

  void (async () => {
    let ended = false;
    const endStream = () => {
      if (!ended) {
        ended = true;
        stream.end();
      }
    };

    try {
      const normalizedEvents = normalizePlainTextToolCallStreamEvents(
        source as AsyncIterable<unknown>,
        {
          createPromotedToolCallEvents: (message) => {
            const events: unknown[] = [];
            emitPromotedToolCallEvents({ push: (event: unknown) => events.push(event) }, message);
            return events;
          },
          matcher,
          normalizeDoneMessage: ({ message }) =>
            normalizeProviderDoneMessage(message, toolNames, matcher),
          stopAfterDone: true,
        },
      );
      for await (const event of normalizedEvents) {
        stream.push(event);
      }
    } catch (error) {
      stream.push({
        type: "error",
        reason: "error",
        error: {
          role: "assistant",
          content: [],
          stopReason: "error",
          errorMessage: error instanceof Error ? error.message : String(error),
        },
      });
    } finally {
      endStream();
    }
  })();

  return output as ReturnType<StreamFn>;
}

/**
 * Provider stream wrapper for local/proxy providers that sometimes emit a
 * standalone textual tool-call block even when native tool calling is enabled.
 */
export function createPlainTextToolCallCompatWrapper(
  /** Provider stream function to wrap; defaults to the simple stream implementation. */
  baseStreamFn: StreamFn | undefined,
): StreamFn {
  const underlying = baseStreamFn ?? streamSimple;
  return (model, context, options) => {
    const maybeStream = underlying(model, context, options);
    if (maybeStream && typeof maybeStream === "object" && "then" in maybeStream) {
      return Promise.resolve(maybeStream).then((stream) =>
        wrapPlainTextToolCallStream(stream, context),
      ) as ReturnType<StreamFn>;
    }
    return wrapPlainTextToolCallStream(maybeStream, context);
  };
}

/** @deprecated Bundled provider stream helper; do not use from third-party plugins. */
export function defaultToolStreamExtraParams(
  /** Existing provider extra params; explicit tool_stream values are preserved. */
  extraParams?: Record<string, unknown>,
): Record<string, unknown> {
  if (extraParams?.tool_stream !== undefined) {
    return extraParams;
  }
  return {
    ...extraParams,
    tool_stream: true,
  };
}

/** Wrap a provider stream so callers can patch the outbound provider payload once. */
export function createPayloadPatchStreamWrapper(
  /** Provider stream function whose outbound payload should be patched. */
  baseStreamFn: StreamFn | undefined,
  patchPayload: (params: {
    /** Mutable provider payload immediately before the underlying stream dispatches it. */
    payload: Record<string, unknown>;
    /** Model selected for the stream call. */
    model: Parameters<StreamFn>[0];
    /** Stream context passed by the runtime. */
    context: Parameters<StreamFn>[1];
    /** Stream options passed by the runtime. */
    options: Parameters<StreamFn>[2];
  }) => void,
  wrapperOptions?: {
    shouldPatch?: (params: {
      /** Model selected for the stream call. */
      model: Parameters<StreamFn>[0];
      /** Stream context passed by the runtime. */
      context: Parameters<StreamFn>[1];
      /** Stream options passed by the runtime. */
      options: Parameters<StreamFn>[2];
    }) => boolean;
  },
): StreamFn {
  const underlying = baseStreamFn ?? streamSimple;
  return (model, context, options) => {
    if (wrapperOptions?.shouldPatch && !wrapperOptions.shouldPatch({ model, context, options })) {
      return underlying(model, context, options);
    }
    return streamWithPayloadPatch(underlying, model, context, options, (payload) =>
      patchPayload({ payload, model, context, options }),
    );
  };
}

function isAnthropicThinkingEnabled(payload: Record<string, unknown>): boolean {
  const thinking = payload.thinking;
  if (!thinking || typeof thinking !== "object") {
    return false;
  }
  return (thinking as { type?: unknown }).type !== "disabled";
}

function assistantMessageHasAnthropicToolUse(message: Record<string, unknown>): boolean {
  if (Array.isArray(message.tool_calls) && message.tool_calls.length > 0) {
    return true;
  }
  const content = message.content;
  if (!Array.isArray(content)) {
    return false;
  }
  return content.some(
    (block) =>
      block &&
      typeof block === "object" &&
      ((block as { type?: unknown }).type === "tool_use" ||
        (block as { type?: unknown }).type === "toolCall"),
  );
}

function stripTrailingAssistantPrefillMessages(payload: Record<string, unknown>): number {
  if (!Array.isArray(payload.messages)) {
    return 0;
  }

  let stripped = 0;
  while (payload.messages.length > 0) {
    const finalMessage = payload.messages[payload.messages.length - 1];
    if (!finalMessage || typeof finalMessage !== "object") {
      break;
    }

    const message = finalMessage as Record<string, unknown>;
    if (message.role !== "assistant" || assistantMessageHasAnthropicToolUse(message)) {
      break;
    }

    payload.messages.pop();
    stripped += 1;
  }
  return stripped;
}

/** @deprecated Anthropic-family provider stream helper; do not use from third-party plugins. */
export function stripTrailingAnthropicAssistantPrefillWhenThinking(
  payload: Record<string, unknown>,
): number {
  if (!isAnthropicThinkingEnabled(payload)) {
    return 0;
  }
  return stripTrailingAssistantPrefillMessages(payload);
}

/** @deprecated Anthropic-family provider stream helper; do not use from third-party plugins. */
export function createAnthropicThinkingPrefillPayloadWrapper(
  baseStreamFn: StreamFn | undefined,
  onStripped?: (stripped: number) => void,
  wrapperOptions?: Parameters<typeof createPayloadPatchStreamWrapper>[2],
): StreamFn {
  return createPayloadPatchStreamWrapper(
    baseStreamFn,
    ({ payload }) => {
      const stripped = stripTrailingAnthropicAssistantPrefillWhenThinking(payload);
      if (stripped > 0) {
        onStripped?.(stripped);
      }
    },
    wrapperOptions,
  );
}

/** @deprecated OpenAI-compatible provider stream helper; do not use from third-party plugins. */
export type OpenAICompatibleThinkingLevel = ProviderWrapStreamFnContext["thinkingLevel"];

/** @deprecated OpenAI-compatible provider stream helper; do not use from third-party plugins. */
export function isOpenAICompatibleThinkingEnabled(params: {
  thinkingLevel: OpenAICompatibleThinkingLevel;
  options: Parameters<StreamFn>[2];
}): boolean {
  const options = (params.options ?? {}) as { reasoningEffort?: unknown; reasoning?: unknown };
  const raw = options.reasoningEffort ?? options.reasoning ?? params.thinkingLevel ?? "high";
  if (typeof raw !== "string") {
    return true;
  }
  const normalized = raw.trim().toLowerCase();
  return normalized !== "off" && normalized !== "none";
}

/** @deprecated DeepSeek provider stream helper; do not use from third-party plugins. */
export type DeepSeekV4ThinkingLevel = ProviderWrapStreamFnContext["thinkingLevel"];
/** @deprecated DeepSeek provider stream helper; do not use from third-party plugins. */
export type DeepSeekV4ReasoningEffort = "minimal" | "low" | "medium" | "high" | "xhigh" | "max";

function isDisabledDeepSeekV4ThinkingLevel(thinkingLevel: DeepSeekV4ThinkingLevel): boolean {
  const normalized = typeof thinkingLevel === "string" ? thinkingLevel.toLowerCase() : "";
  return normalized === "off" || normalized === "none";
}

function resolveDeepSeekV4ReasoningEffort(
  thinkingLevel: DeepSeekV4ThinkingLevel,
): DeepSeekV4ReasoningEffort {
  return thinkingLevel === "xhigh" || thinkingLevel === "max" ? "max" : "high";
}

function stripDeepSeekV4ReasoningContent(payload: Record<string, unknown>): void {
  if (!Array.isArray(payload.messages)) {
    return;
  }
  for (const message of payload.messages) {
    if (!message || typeof message !== "object") {
      continue;
    }
    delete (message as Record<string, unknown>).reasoning_content;
  }
}

function ensureDeepSeekV4AssistantReasoningContent(
  payload: Record<string, unknown>,
  params?: {
    shouldBackfillAssistantMessage?: (message: Record<string, unknown>) => boolean;
  },
): void {
  if (!Array.isArray(payload.messages)) {
    return;
  }
  for (const message of payload.messages) {
    if (!message || typeof message !== "object") {
      continue;
    }
    const record = message as Record<string, unknown>;
    if (record.role !== "assistant") {
      continue;
    }
    if (params?.shouldBackfillAssistantMessage && !params.shouldBackfillAssistantMessage(record)) {
      continue;
    }
    if (!("reasoning_content" in record)) {
      record.reasoning_content = "";
    }
  }
}

/** @deprecated DeepSeek provider stream helper; do not use from third-party plugins. */
export function createDeepSeekV4OpenAICompatibleThinkingWrapper(params: {
  baseStreamFn: StreamFn | undefined;
  thinkingLevel: DeepSeekV4ThinkingLevel;
  shouldPatchModel: (model: Parameters<StreamFn>[0]) => boolean;
  resolveReasoningEffort?: (thinkingLevel: DeepSeekV4ThinkingLevel) => DeepSeekV4ReasoningEffort;
  shouldBackfillAssistantReasoningContent?: (message: Record<string, unknown>) => boolean;
}): StreamFn | undefined {
  if (!params.baseStreamFn) {
    return undefined;
  }
  const underlying = params.baseStreamFn;
  const resolveReasoningEffort = params.resolveReasoningEffort ?? resolveDeepSeekV4ReasoningEffort;
  return (model, context, options) => {
    if (!params.shouldPatchModel(model)) {
      return underlying(model, context, options);
    }

    return streamWithPayloadPatch(underlying, model, context, options, (payload) => {
      if (isDisabledDeepSeekV4ThinkingLevel(params.thinkingLevel)) {
        payload.thinking = { type: "disabled" };
        delete payload.reasoning_effort;
        delete payload.reasoning;
        stripDeepSeekV4ReasoningContent(payload);
        return;
      }

      payload.thinking = { type: "enabled" };
      payload.reasoning_effort = resolveReasoningEffort(params.thinkingLevel);
      ensureDeepSeekV4AssistantReasoningContent(payload, {
        shouldBackfillAssistantMessage: params.shouldBackfillAssistantReasoningContent,
      });
    });
  };
}

type ThinkingOnlyFinalTextStream = Awaited<ReturnType<StreamFn>>;

function promoteThinkingOnlyFinalOutputToText(message: unknown): void {
  if (!message || typeof message !== "object") {
    return;
  }
  const record = message as { content?: unknown; stopReason?: unknown };
  if (record.stopReason !== "stop" && record.stopReason !== "length") {
    return;
  }
  if (!Array.isArray(record.content) || record.content.length === 0) {
    return;
  }

  let hasVisibleText = false;
  let hasToolCall = false;
  let hasVisibleThinking = false;
  for (const block of record.content) {
    if (!block || typeof block !== "object") {
      continue;
    }
    const typedBlock = block as { type?: unknown; text?: unknown; thinking?: unknown };
    if (
      typedBlock.type === "text" &&
      typeof typedBlock.text === "string" &&
      typedBlock.text.trim()
    ) {
      hasVisibleText = true;
    }
    if (typedBlock.type === "toolCall" || typedBlock.type === "tool_use") {
      hasToolCall = true;
    }
    if (
      typedBlock.type === "thinking" &&
      typeof typedBlock.thinking === "string" &&
      typedBlock.thinking.trim()
    ) {
      hasVisibleThinking = true;
    }
  }
  if (hasVisibleText || hasToolCall || !hasVisibleThinking) {
    return;
  }

  record.content = record.content.map((block) => {
    if (!block || typeof block !== "object") {
      return block;
    }
    const typedBlock = block as { type?: unknown; thinking?: unknown };
    if (
      typedBlock.type !== "thinking" ||
      typeof typedBlock.thinking !== "string" ||
      !typedBlock.thinking.trim()
    ) {
      return block;
    }
    return { type: "text", text: typedBlock.thinking };
  });
}

function wrapThinkingOnlyFinalTextStream(
  stream: ThinkingOnlyFinalTextStream,
): ThinkingOnlyFinalTextStream {
  const originalResult = stream.result.bind(stream);
  stream.result = async () => {
    const message = await originalResult();
    promoteThinkingOnlyFinalOutputToText(message);
    return message;
  };

  const originalAsyncIterator = stream[Symbol.asyncIterator].bind(stream);
  (stream as { [Symbol.asyncIterator]: typeof originalAsyncIterator })[Symbol.asyncIterator] =
    function () {
      const iterator = originalAsyncIterator();
      return {
        async next() {
          const result = await iterator.next();
          if (!result.done && result.value && typeof result.value === "object") {
            const event = result.value as { partial?: unknown; message?: unknown };
            promoteThinkingOnlyFinalOutputToText(event.partial);
            promoteThinkingOnlyFinalOutputToText(event.message);
          }
          return result;
        },
        async return(value?: unknown) {
          return iterator.return?.(value) ?? { done: true as const, value: undefined };
        },
        async throw(error?: unknown) {
          return iterator.throw?.(error) ?? { done: true as const, value: undefined };
        },
        [Symbol.asyncIterator]() {
          return this;
        },
      };
    };
  return stream;
}

/** @deprecated OpenAI-compatible provider stream helper; do not use from third-party plugins. */
export function createThinkingOnlyFinalTextWrapper(params: {
  baseStreamFn: StreamFn | undefined;
  shouldPatchModel: (model: Parameters<StreamFn>[0]) => boolean;
}): StreamFn | undefined {
  if (!params.baseStreamFn) {
    return undefined;
  }
  const underlying = params.baseStreamFn;
  return (model, context, options) => {
    const maybeStream = underlying(model, context, options);
    if (!params.shouldPatchModel(model)) {
      return maybeStream;
    }
    if (maybeStream && typeof maybeStream === "object" && "then" in maybeStream) {
      return Promise.resolve(maybeStream).then((stream) => wrapThinkingOnlyFinalTextStream(stream));
    }
    return wrapThinkingOnlyFinalTextStream(maybeStream);
  };
}

export {
  applyAnthropicPayloadPolicyToParams,
  resolveAnthropicPayloadPolicy,
} from "../agents/anthropic-payload-policy.js";
export { applyAnthropicEphemeralCacheControlMarkers } from "../llm/providers/stream-wrappers/anthropic-cache-control-payload.js";
export {
  createMoonshotThinkingWrapper,
  resolveMoonshotThinkingType,
} from "../llm/providers/stream-wrappers/moonshot-thinking.js";
export { streamWithPayloadPatch };
export {
  createToolStreamWrapper,
  createZaiToolStreamWrapper,
} from "../llm/providers/stream-wrappers/zai.js";
