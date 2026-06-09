// Defines core provider schema fragments for config parsing.
import { normalizeOptionalString } from "@openclaw/normalization-core/string-coerce";
import { z } from "zod";
import {
  normalizeCommandDescription,
  normalizeSlashCommandName,
  resolveCustomCommands,
} from "../shared/custom-command-config.js";
import { ToolPolicySchema } from "./zod-schema.agent-runtime.js";
import { NativeExecApprovalEnableModeSchema } from "./zod-schema.approvals.js";
import {
  ChannelHealthMonitorSchema,
  ChannelHeartbeatVisibilitySchema,
} from "./zod-schema.channels.js";
import {
  BlockStreamingChunkSchema,
  BlockStreamingCoalesceSchema,
  ContextVisibilityModeSchema,
  DmConfigSchema,
  DmPolicySchema,
  GroupPolicySchema,
  HexColorSchema,
  MarkdownConfigSchema,
  MentionPatternsPolicySchema,
  ProviderCommandsSchema,
  SecretInputSchema,
  ReplyToModeSchema,
  RetryConfigSchema,
  TtsConfigSchema,
  requireAllowlistAllowFrom,
  requireOpenAllowFrom,
} from "./zod-schema.core.js";
import { validateTelegramWebhookSecretRequirements } from "./zod-schema.secret-input-validation.js";
import { sensitive } from "./zod-schema.sensitive.js";

const ToolPolicyBySenderSchema = z.record(z.string(), ToolPolicySchema).optional();

const DiscordIdSchema = z
  .union([z.string(), z.number()])
  .transform((value, ctx) => {
    if (typeof value === "number") {
      if (!Number.isSafeInteger(value) || value < 0) {
        ctx.addIssue({
          code: z.ZodIssueCode.custom,
          message:
            `Discord ID "${String(value)}" is not a valid non-negative safe integer. ` +
            `Wrap it in quotes in your config file.`,
        });
        return z.NEVER;
      }
      return String(value);
    }
    return value;
  })
  .pipe(z.string());
const DiscordIdListSchema = z.array(DiscordIdSchema);
const DiscordSnowflakeStringSchema = z.string().regex(/^\d+$/, "Discord user ID must be numeric");

const TelegramInlineButtonsScopeSchema = z.enum(["off", "dm", "group", "all", "allowlist"]);
const TelegramIdListSchema = z.array(z.union([z.string(), z.number()]));

const TelegramCapabilitiesSchema = z.union([
  z.array(z.string()),
  z
    .object({
      inlineButtons: TelegramInlineButtonsScopeSchema.optional(),
    })
    .strict(),
]);
const TextChunkModeSchema = z.enum(["length", "newline"]);
const UnifiedStreamingModeSchema = z.enum(["off", "partial", "block", "progress"]);
const ChannelStreamingBlockSchema = z
  .object({
    enabled: z.boolean().optional(),
    coalesce: BlockStreamingCoalesceSchema.optional(),
  })
  .strict();
const ChannelStreamingPreviewSchema = z
  .object({
    chunk: BlockStreamingChunkSchema.optional(),
    toolProgress: z.boolean().optional(),
    commandText: z.enum(["raw", "status"]).optional(),
  })
  .strict();
const TelegramStreamingPreviewSchema = ChannelStreamingPreviewSchema.extend({
  nativeToolProgress: z.boolean().optional(),
  nativeToolProgressAllowFrom: z.array(z.union([z.string(), z.number()])).optional(),
}).strict();
const ChannelStreamingProgressSchema = z
  .object({
    label: z.union([z.string(), z.literal(false)]).optional(),
    labels: z.array(z.string()).optional(),
    maxLines: z.number().int().positive().optional(),
    maxLineChars: z.number().int().positive().optional(),
    render: z.enum(["text", "rich"]).optional(),
    toolProgress: z.boolean().optional(),
    commandText: z.enum(["raw", "status"]).optional(),
    commentary: z.boolean().optional(),
  })
  .strict();
const ChannelPreviewStreamingConfigSchema = z
  .object({
    mode: UnifiedStreamingModeSchema.optional(),
    chunkMode: TextChunkModeSchema.optional(),
    preview: ChannelStreamingPreviewSchema.optional(),
    progress: ChannelStreamingProgressSchema.optional(),
    block: ChannelStreamingBlockSchema.optional(),
  })
  .strict();
const TelegramPreviewStreamingConfigSchema = ChannelPreviewStreamingConfigSchema.extend({
  preview: TelegramStreamingPreviewSchema.optional(),
}).strict();
const DiscordPreviewStreamingConfigSchema = ChannelPreviewStreamingConfigSchema;
const BotLoopProtectionSchema = z
  .object({
    enabled: z.boolean().optional(),
    maxEventsPerWindow: z.number().int().positive().optional(),
    windowSeconds: z.number().int().positive().optional(),
    cooldownSeconds: z.number().int().positive().optional(),
  })
  .strict();

const TelegramErrorPolicySchema = z.enum(["always", "once", "silent"]).optional();
const TelegramCommandNamePattern = /^[a-z0-9_]{1,32}$/;
const TelegramCustomCommandConfig = {
  label: "Telegram",
  pattern: TelegramCommandNamePattern,
  patternDescription: "use a-z, 0-9, underscore; max 32 chars",
} as const;
export const TelegramTopicSchema = z
  .object({
    requireMention: z.boolean().optional(),
    ingest: z.boolean().optional(),
    disableAudioPreflight: z.boolean().optional(),
    groupPolicy: GroupPolicySchema.optional(),
    skills: z.array(z.string()).optional(),
    enabled: z.boolean().optional(),
    allowFrom: z.array(z.union([z.string(), z.number()])).optional(),
    systemPrompt: z.string().optional(),
    agentId: z.string().optional(),
    errorPolicy: TelegramErrorPolicySchema,
    errorCooldownMs: z.number().int().nonnegative().optional(),
  })
  .strict();

export const TelegramGroupSchema = z
  .object({
    requireMention: z.boolean().optional(),
    ingest: z.boolean().optional(),
    disableAudioPreflight: z.boolean().optional(),
    groupPolicy: GroupPolicySchema.optional(),
    tools: ToolPolicySchema,
    toolsBySender: ToolPolicyBySenderSchema,
    skills: z.array(z.string()).optional(),
    enabled: z.boolean().optional(),
    allowFrom: z.array(z.union([z.string(), z.number()])).optional(),
    systemPrompt: z.string().optional(),
    topics: z.record(z.string(), TelegramTopicSchema.optional()).optional(),
    errorPolicy: TelegramErrorPolicySchema,
    errorCooldownMs: z.number().int().nonnegative().optional(),
  })
  .strict();

const AutoTopicLabelSchema = z
  .union([
    z.boolean(),
    z
      .object({
        enabled: z.boolean().optional(),
        prompt: z.string().optional(),
      })
      .strict(),
  ])
  .optional();

export const TelegramDirectSchema = z
  .object({
    dmPolicy: DmPolicySchema.optional(),
    tools: ToolPolicySchema,
    toolsBySender: ToolPolicyBySenderSchema,
    skills: z.array(z.string()).optional(),
    enabled: z.boolean().optional(),
    allowFrom: z.array(z.union([z.string(), z.number()])).optional(),
    systemPrompt: z.string().optional(),
    topics: z.record(z.string(), TelegramTopicSchema.optional()).optional(),
    errorPolicy: TelegramErrorPolicySchema,
    errorCooldownMs: z.number().int().nonnegative().optional(),
    requireTopic: z.boolean().optional(),
    autoTopicLabel: AutoTopicLabelSchema,
  })
  .strict();

const TelegramCustomCommandSchema = z
  .object({
    command: z.string().overwrite(normalizeSlashCommandName),
    description: z.string().overwrite(normalizeCommandDescription),
  })
  .strict();

const validateTelegramCustomCommands = (
  value: { customCommands?: Array<{ command?: string; description?: string }> },
  ctx: z.RefinementCtx,
) => {
  if (!value.customCommands || value.customCommands.length === 0) {
    return;
  }
  const { issues } = resolveCustomCommands({
    commands: value.customCommands,
    checkReserved: false,
    checkDuplicates: false,
    config: TelegramCustomCommandConfig,
  });
  for (const issue of issues) {
    ctx.addIssue({
      code: z.ZodIssueCode.custom,
      path: ["customCommands", issue.index, issue.field],
      message: issue.message,
    });
  }
};

export const TelegramAccountSchemaBase = z
  .object({
    name: z.string().optional(),
    capabilities: TelegramCapabilitiesSchema.optional(),
    execApprovals: z
      .object({
        enabled: NativeExecApprovalEnableModeSchema.optional(),
        approvers: TelegramIdListSchema.optional(),
        agentFilter: z.array(z.string()).optional(),
        sessionFilter: z.array(z.string()).optional(),
        target: z.enum(["dm", "channel", "both"]).optional(),
      })
      .strict()
      .optional(),
    markdown: MarkdownConfigSchema,
    enabled: z.boolean().optional(),
    commands: ProviderCommandsSchema,
    customCommands: z.array(TelegramCustomCommandSchema).optional(),
    configWrites: z.boolean().optional(),
    dmPolicy: DmPolicySchema.optional().default("pairing"),
    botToken: SecretInputSchema.optional().register(sensitive),
    tokenFile: z.string().optional(),
    replyToMode: ReplyToModeSchema.optional(),
    groups: z.record(z.string(), TelegramGroupSchema.optional()).optional(),
    allowFrom: z.array(z.union([z.string(), z.number()])).optional(),
    defaultTo: z.union([z.string(), z.number()]).optional(),
    groupAllowFrom: z.array(z.union([z.string(), z.number()])).optional(),
    groupPolicy: GroupPolicySchema.optional().default("allowlist"),
    mentionPatterns: MentionPatternsPolicySchema.optional(),
    contextVisibility: ContextVisibilityModeSchema.optional(),
    historyLimit: z.number().int().min(0).optional(),
    dmHistoryLimit: z.number().int().min(0).optional(),
    dms: z.record(z.string(), DmConfigSchema.optional()).optional(),
    direct: z.record(z.string(), TelegramDirectSchema.optional()).optional(),
    textChunkLimit: z.number().int().positive().optional(),
    streaming: TelegramPreviewStreamingConfigSchema.optional(),
    mediaMaxMb: z.number().positive().optional(),
    timeoutSeconds: z.number().int().positive().optional(),
    mediaGroupFlushMs: z
      .number()
      .int()
      .min(10)
      .max(60_000)
      .optional()
      .describe(
        "Buffer window in milliseconds for Telegram media groups/albums before dispatching them as one inbound message. Default: 500.",
      ),
    pollingStallThresholdMs: z.number().int().min(30_000).max(600_000).optional(),
    retry: RetryConfigSchema,
    network: z
      .object({
        autoSelectFamily: z.boolean().optional(),
        dnsResultOrder: z.enum(["ipv4first", "verbatim"]).optional(),
        dangerouslyAllowPrivateNetwork: z
          .boolean()
          .optional()
          .describe(
            "Dangerous opt-in for trusted Telegram fake-IP or transparent-proxy environments where api.telegram.org resolves to private/internal/special-use addresses during media downloads.",
          ),
      })
      .strict()
      .optional(),
    proxy: z.string().optional(),
    webhookUrl: z
      .string()
      .optional()
      .describe(
        "Public HTTPS webhook URL registered with Telegram for inbound updates. This must be internet-reachable and requires channels.telegram.webhookSecret.",
      ),
    webhookSecret: SecretInputSchema.optional()
      .describe(
        "Secret token sent to Telegram during webhook registration and verified on inbound webhook requests. Telegram returns this value for verification; this is not the gateway auth token and not the bot token.",
      )
      .register(sensitive),
    webhookPath: z
      .string()
      .optional()
      .describe(
        "Local webhook route path served by the gateway listener. Defaults to /telegram-webhook.",
      ),
    webhookHost: z
      .string()
      .optional()
      .describe(
        "Local bind host for the webhook listener. Defaults to 127.0.0.1; keep loopback unless you intentionally expose direct ingress.",
      ),
    webhookPort: z
      .number()
      .int()
      .nonnegative()
      .optional()
      .describe(
        "Local bind port for the webhook listener. Defaults to 8787; set to 0 to let the OS assign an ephemeral port.",
      ),
    webhookCertPath: z
      .string()
      .optional()
      .describe(
        "Path to the self-signed certificate (PEM) to upload to Telegram during webhook registration. Required for self-signed certs (direct IP or no domain).",
      ),
    actions: z
      .object({
        reactions: z.boolean().optional(),
        sendMessage: z.boolean().optional(),
        poll: z.boolean().optional(),
        deleteMessage: z.boolean().optional(),
        editMessage: z.boolean().optional(),
        sticker: z.boolean().optional(),
        createForumTopic: z.boolean().optional(),
        editForumTopic: z.boolean().optional(),
      })
      .strict()
      .optional(),
    threadBindings: z
      .object({
        enabled: z.boolean().optional(),
        idleHours: z.number().nonnegative().optional(),
        maxAgeHours: z.number().nonnegative().optional(),
        spawnSessions: z.boolean().optional(),
        defaultSpawnContext: z.enum(["isolated", "fork"]).optional(),
        spawnSubagentSessions: z.boolean().optional(),
        spawnAcpSessions: z.boolean().optional(),
      })
      .strict()
      .optional(),
    reactionNotifications: z.enum(["off", "own", "all"]).optional(),
    reactionLevel: z.enum(["off", "ack", "minimal", "extensive"]).optional(),
    heartbeat: ChannelHeartbeatVisibilitySchema,
    healthMonitor: ChannelHealthMonitorSchema,
    linkPreview: z.boolean().optional(),
    silentErrorReplies: z.boolean().optional(),
    responsePrefix: z.string().optional(),
    ackReaction: z.string().optional(),
    errorPolicy: TelegramErrorPolicySchema,
    errorCooldownMs: z.number().int().nonnegative().optional(),
    apiRoot: z.string().url().optional(),
    trustedLocalFileRoots: z
      .array(z.string())
      .optional()
      .describe(
        "Trusted local filesystem roots for self-hosted Telegram Bot API absolute file_path values. Only absolute paths under these roots are read directly; all other absolute paths are rejected.",
      ),
    autoTopicLabel: AutoTopicLabelSchema,
  })
  .strict();

export const TelegramAccountSchema = TelegramAccountSchemaBase.superRefine((value, ctx) => {
  // Account-level schemas skip allowFrom validation because accounts inherit
  // allowFrom from the parent channel config at runtime (resolveTelegramAccount
  // shallow-merges top-level and account values in src/telegram/accounts.ts).
  // Validation is enforced at the top-level TelegramConfigSchema instead.
  validateTelegramCustomCommands(value, ctx);
});

export const TelegramConfigSchema = TelegramAccountSchemaBase.extend({
  accounts: z.record(z.string(), TelegramAccountSchema.optional()).optional(),
  defaultAccount: z.string().optional(),
}).superRefine((value, ctx) => {
  requireOpenAllowFrom({
    policy: value.dmPolicy,
    allowFrom: value.allowFrom,
    ctx,
    path: ["allowFrom"],
    message:
      'channels.telegram.dmPolicy="open" requires channels.telegram.allowFrom to include "*"',
  });
  requireAllowlistAllowFrom({
    policy: value.dmPolicy,
    allowFrom: value.allowFrom,
    ctx,
    path: ["allowFrom"],
    message:
      'channels.telegram.dmPolicy="allowlist" requires channels.telegram.allowFrom to contain at least one sender ID',
  });
  validateTelegramCustomCommands(value, ctx);

  if (value.accounts) {
    for (const [accountId, account] of Object.entries(value.accounts)) {
      if (!account) {
        continue;
      }
      const effectivePolicy = account.dmPolicy ?? value.dmPolicy;
      const effectiveAllowFrom = account.allowFrom ?? value.allowFrom;
      requireOpenAllowFrom({
        policy: effectivePolicy,
        allowFrom: effectiveAllowFrom,
        ctx,
        path: ["accounts", accountId, "allowFrom"],
        message:
          'channels.telegram.accounts.*.dmPolicy="open" requires channels.telegram.accounts.*.allowFrom (or channels.telegram.allowFrom) to include "*"',
      });
      requireAllowlistAllowFrom({
        policy: effectivePolicy,
        allowFrom: effectiveAllowFrom,
        ctx,
        path: ["accounts", accountId, "allowFrom"],
        message:
          'channels.telegram.accounts.*.dmPolicy="allowlist" requires channels.telegram.accounts.*.allowFrom (or channels.telegram.allowFrom) to contain at least one sender ID',
      });
    }
  }

  if (!value.accounts) {
    validateTelegramWebhookSecretRequirements(value, ctx);
    return;
  }
  for (const [accountId, account] of Object.entries(value.accounts)) {
    if (!account) {
      continue;
    }
    if (account.enabled === false) {
      continue;
    }
    const effectiveDmPolicy = account.dmPolicy ?? value.dmPolicy;
    const effectiveAllowFrom = Array.isArray(account.allowFrom)
      ? account.allowFrom
      : value.allowFrom;
    requireOpenAllowFrom({
      policy: effectiveDmPolicy,
      allowFrom: effectiveAllowFrom,
      ctx,
      path: ["accounts", accountId, "allowFrom"],
      message:
        'channels.telegram.accounts.*.dmPolicy="open" requires channels.telegram.allowFrom or channels.telegram.accounts.*.allowFrom to include "*"',
    });
    requireAllowlistAllowFrom({
      policy: effectiveDmPolicy,
      allowFrom: effectiveAllowFrom,
      ctx,
      path: ["accounts", accountId, "allowFrom"],
      message:
        'channels.telegram.accounts.*.dmPolicy="allowlist" requires channels.telegram.allowFrom or channels.telegram.accounts.*.allowFrom to contain at least one sender ID',
    });
  }
  validateTelegramWebhookSecretRequirements(value, ctx);
});

export const DiscordDmSchema = z
  .object({
    enabled: z.boolean().optional(),
    policy: DmPolicySchema.optional(),
    allowFrom: DiscordIdListSchema.optional(),
    groupEnabled: z.boolean().optional(),
    groupChannels: DiscordIdListSchema.optional(),
  })
  .strict();

export const DiscordThreadSchema = z
  .object({
    inheritParent: z.boolean().optional(),
  })
  .strict();

export const DiscordGuildChannelSchema = z
  .object({
    requireMention: z.boolean().optional(),
    ignoreOtherMentions: z.boolean().optional(),
    tools: ToolPolicySchema,
    toolsBySender: ToolPolicyBySenderSchema,
    skills: z.array(z.string()).optional(),
    enabled: z.boolean().optional(),
    users: DiscordIdListSchema.optional(),
    roles: DiscordIdListSchema.optional(),
    systemPrompt: z.string().optional(),
    includeThreadStarter: z.boolean().optional(),
    autoThread: z.boolean().optional(),
    /** Naming strategy for auto-created threads. "message" uses message text; "generated" creates an LLM title after thread creation. */
    autoThreadName: z.enum(["message", "generated"]).optional(),
    /** Archive duration for auto-created threads in minutes. Discord supports 60, 1440 (1 day), 4320 (3 days), 10080 (1 week). Default: 60. */
    autoArchiveDuration: z
      .union([
        z.enum(["60", "1440", "4320", "10080"]),
        z.literal(60),
        z.literal(1440),
        z.literal(4320),
        z.literal(10080),
      ])
      .optional(),
  })
  .strict();

export const DiscordGuildSchema = z
  .object({
    slug: z.string().optional(),
    requireMention: z.boolean().optional(),
    ignoreOtherMentions: z.boolean().optional(),
    tools: ToolPolicySchema,
    toolsBySender: ToolPolicyBySenderSchema,
    reactionNotifications: z.enum(["off", "own", "all", "allowlist"]).optional(),
    users: DiscordIdListSchema.optional(),
    roles: DiscordIdListSchema.optional(),
    channels: z.record(z.string(), DiscordGuildChannelSchema.optional()).optional(),
  })
  .strict();

const DiscordUiSchema = z
  .object({
    components: z
      .object({
        accentColor: HexColorSchema.optional(),
      })
      .strict()
      .optional(),
  })
  .strict()
  .optional();

const DiscordVoiceAutoJoinSchema = z
  .object({
    guildId: z.string().min(1),
    channelId: z.string().min(1),
  })
  .strict();

const DiscordVoiceAllowedChannelSchema = z
  .object({
    guildId: z.string().min(1),
    channelId: z.string().min(1),
  })
  .strict();

const DiscordVoiceRealtimeToolPolicySchema = z.enum(["safe-read-only", "owner", "none"]);
const DiscordVoiceRealtimeConsultPolicySchema = z.enum(["auto", "always"]);
const DiscordVoiceRealtimeBootstrapContextFileSchema = z.enum([
  "IDENTITY.md",
  "USER.md",
  "SOUL.md",
]);
const DiscordVoiceRealtimeWakeNameSchema = z
  .string()
  .min(1)
  .regex(/^\s*[^a-z0-9]*[a-z0-9]+(?:[^a-z0-9]+[a-z0-9]+)?[^a-z0-9]*\s*$/i, {
    message: "Discord realtime wake names must be one or two words.",
  });
const DiscordVoiceRealtimeSchema = z
  .object({
    provider: z.string().min(1).optional(),
    model: z.string().min(1).optional(),
    speakerVoice: z.string().min(1).optional(),
    speakerVoiceId: z.string().min(1).optional(),
    voice: z.string().min(1).optional(),
    instructions: z.string().min(1).optional(),
    toolPolicy: DiscordVoiceRealtimeToolPolicySchema.optional(),
    consultPolicy: DiscordVoiceRealtimeConsultPolicySchema.optional(),
    requireWakeName: z.boolean().optional(),
    wakeNames: z.array(DiscordVoiceRealtimeWakeNameSchema).min(1).optional(),
    bootstrapContextFiles: z.array(DiscordVoiceRealtimeBootstrapContextFileSchema).optional(),
    bargeIn: z.boolean().optional(),
    minBargeInAudioEndMs: z.number().int().min(0).max(10_000).optional(),
    debounceMs: z.number().int().positive().max(10_000).optional(),
    providers: z.record(z.string(), z.record(z.string(), z.unknown()).optional()).optional(),
  })
  .strict();

const DiscordVoiceAgentSessionSchema = z
  .object({
    mode: z.enum(["voice", "target"]).optional(),
    target: z.string().min(1).optional(),
  })
  .strict()
  .superRefine((value, ctx) => {
    if (value.mode === "target" && !value.target) {
      ctx.addIssue({
        code: z.ZodIssueCode.custom,
        path: ["target"],
        message: 'voice.agentSession.target is required when mode is "target"',
      });
    }
  });

const DiscordVoiceSchema = z
  .object({
    enabled: z.boolean().optional(),
    mode: z.enum(["stt-tts", "agent-proxy", "bidi"]).optional(),
    agentSession: DiscordVoiceAgentSessionSchema.optional(),
    model: z.string().min(1).optional(),
    realtime: DiscordVoiceRealtimeSchema.optional(),
    autoJoin: z.array(DiscordVoiceAutoJoinSchema).optional(),
    followUsersEnabled: z.boolean().optional(),
    followUsers: z.array(z.string().min(1)).optional(),
    allowedChannels: z.array(DiscordVoiceAllowedChannelSchema).optional(),
    daveEncryption: z.boolean().optional(),
    decryptionFailureTolerance: z.number().int().min(0).optional(),
    connectTimeoutMs: z.number().int().positive().max(120_000).optional(),
    reconnectGraceMs: z.number().int().positive().max(120_000).optional(),
    captureSilenceGraceMs: z.number().int().positive().max(30_000).optional(),
    tts: TtsConfigSchema.optional(),
  })
  .strict()
  .optional();

export const DiscordAccountSchema = z
  .object({
    name: z.string().optional(),
    capabilities: z.array(z.string()).optional(),
    markdown: MarkdownConfigSchema,
    enabled: z.boolean().optional(),
    commands: ProviderCommandsSchema,
    configWrites: z.boolean().optional(),
    token: SecretInputSchema.optional().register(sensitive),
    applicationId: DiscordIdSchema.optional(),
    proxy: z.string().optional(),
    gatewayInfoTimeoutMs: z.number().int().positive().max(120_000).optional(),
    gatewayReadyTimeoutMs: z.number().int().positive().max(120_000).optional(),
    gatewayRuntimeReadyTimeoutMs: z.number().int().positive().max(120_000).optional(),
    allowBots: z.union([z.boolean(), z.literal("mentions")]).optional(),
    botLoopProtection: BotLoopProtectionSchema.optional(),
    dangerouslyAllowNameMatching: z.boolean().optional(),
    mentionAliases: z.record(z.string(), DiscordSnowflakeStringSchema).optional(),
    groupPolicy: GroupPolicySchema.optional().default("allowlist"),
    mentionPatterns: MentionPatternsPolicySchema.optional(),
    contextVisibility: ContextVisibilityModeSchema.optional(),
    historyLimit: z.number().int().min(0).optional(),
    dmHistoryLimit: z.number().int().min(0).optional(),
    dms: z.record(z.string(), DmConfigSchema.optional()).optional(),
    textChunkLimit: z.number().int().positive().optional(),
    suppressEmbeds: z.boolean().optional(),
    streaming: DiscordPreviewStreamingConfigSchema.optional(),
    maxLinesPerMessage: z.number().int().positive().optional(),
    mediaMaxMb: z.number().positive().optional(),
    retry: RetryConfigSchema,
    actions: z
      .object({
        reactions: z.boolean().optional(),
        stickers: z.boolean().optional(),
        emojiUploads: z.boolean().optional(),
        stickerUploads: z.boolean().optional(),
        polls: z.boolean().optional(),
        permissions: z.boolean().optional(),
        messages: z.boolean().optional(),
        threads: z.boolean().optional(),
        pins: z.boolean().optional(),
        search: z.boolean().optional(),
        memberInfo: z.boolean().optional(),
        roleInfo: z.boolean().optional(),
        roles: z.boolean().optional(),
        channelInfo: z.boolean().optional(),
        voiceStatus: z.boolean().optional(),
        events: z.boolean().optional(),
        moderation: z.boolean().optional(),
        channels: z.boolean().optional(),
        presence: z.boolean().optional(),
      })
      .strict()
      .optional(),
    replyToMode: ReplyToModeSchema.optional(),
    thread: DiscordThreadSchema.optional(),
    // Aliases for channels.discord.dm.policy / channels.discord.dm.allowFrom. Prefer these for
    // inheritance in multi-account setups (shallow merge works; nested dm object doesn't).
    dmPolicy: DmPolicySchema.optional(),
    allowFrom: DiscordIdListSchema.optional(),
    defaultTo: z.string().optional(),
    dm: DiscordDmSchema.optional(),
    guilds: z.record(z.string(), DiscordGuildSchema.optional()).optional(),
    heartbeat: ChannelHeartbeatVisibilitySchema,
    healthMonitor: ChannelHealthMonitorSchema,
    execApprovals: z
      .object({
        enabled: NativeExecApprovalEnableModeSchema.optional(),
        approvers: DiscordIdListSchema.optional(),
        agentFilter: z.array(z.string()).optional(),
        sessionFilter: z.array(z.string()).optional(),
        cleanupAfterResolve: z.boolean().optional(),
        target: z.enum(["dm", "channel", "both"]).optional(),
      })
      .strict()
      .optional(),
    agentComponents: z
      .object({
        enabled: z.boolean().optional(),
        ttlMs: z
          .number()
          .int()
          .positive()
          .max(24 * 60 * 60 * 1000)
          .optional(),
      })
      .strict()
      .optional(),
    ui: DiscordUiSchema,
    slashCommand: z
      .object({
        ephemeral: z.boolean().optional(),
      })
      .strict()
      .optional(),
    threadBindings: z
      .object({
        enabled: z.boolean().optional(),
        idleHours: z.number().nonnegative().optional(),
        maxAgeHours: z.number().nonnegative().optional(),
        spawnSessions: z.boolean().optional(),
        defaultSpawnContext: z.enum(["isolated", "fork"]).optional(),
        spawnSubagentSessions: z.boolean().optional(),
        spawnAcpSessions: z.boolean().optional(),
      })
      .strict()
      .optional(),
    intents: z
      .object({
        presence: z.boolean().optional(),
        guildMembers: z.boolean().optional(),
        voiceStates: z.boolean().optional(),
      })
      .strict()
      .optional(),
    voice: DiscordVoiceSchema,
    pluralkit: z
      .object({
        enabled: z.boolean().optional(),
        token: SecretInputSchema.optional().register(sensitive),
      })
      .strict()
      .optional(),
    responsePrefix: z.string().optional(),
    ackReaction: z.string().optional(),
    ackReactionScope: z
      .enum(["group-mentions", "group-all", "direct", "all", "off", "none"])
      .optional(),
    activity: z.string().optional(),
    status: z.enum(["online", "dnd", "idle", "invisible"]).optional(),
    autoPresence: z
      .object({
        enabled: z.boolean().optional(),
        intervalMs: z.number().int().positive().optional(),
        minUpdateIntervalMs: z.number().int().positive().optional(),
        healthyText: z.string().optional(),
        degradedText: z.string().optional(),
        exhaustedText: z.string().optional(),
      })
      .strict()
      .optional(),
    activityType: z
      .union([z.literal(0), z.literal(1), z.literal(2), z.literal(3), z.literal(4), z.literal(5)])
      .optional(),
    activityUrl: z.string().url().optional(),
    inboundWorker: z
      .object({
        runTimeoutMs: z.number().int().nonnegative().optional(),
      })
      .strict()
      .optional(),
    eventQueue: z
      .object({
        listenerTimeout: z.number().int().positive().optional(),
        maxQueueSize: z.number().int().positive().optional(),
        maxConcurrency: z.number().int().positive().optional(),
      })
      .strict()
      .optional(),
  })
  .strict()
  .superRefine((value, ctx) => {
    const activityText = normalizeOptionalString(value.activity) ?? "";
    const hasActivity = Boolean(activityText);
    const hasActivityType = value.activityType !== undefined;
    const activityUrl = normalizeOptionalString(value.activityUrl) ?? "";
    const hasActivityUrl = Boolean(activityUrl);

    if ((hasActivityType || hasActivityUrl) && !hasActivity) {
      ctx.addIssue({
        code: z.ZodIssueCode.custom,
        message: "channels.discord.activity is required when activityType or activityUrl is set",
        path: ["activity"],
      });
    }

    if (value.activityType === 1 && !hasActivityUrl) {
      ctx.addIssue({
        code: z.ZodIssueCode.custom,
        message: "channels.discord.activityUrl is required when activityType is 1 (Streaming)",
        path: ["activityUrl"],
      });
    }

    if (hasActivityUrl && value.activityType !== 1) {
      ctx.addIssue({
        code: z.ZodIssueCode.custom,
        message: "channels.discord.activityType must be 1 (Streaming) when activityUrl is set",
        path: ["activityType"],
      });
    }

    const autoPresenceInterval = value.autoPresence?.intervalMs;
    const autoPresenceMinUpdate = value.autoPresence?.minUpdateIntervalMs;
    if (
      typeof autoPresenceInterval === "number" &&
      typeof autoPresenceMinUpdate === "number" &&
      autoPresenceMinUpdate > autoPresenceInterval
    ) {
      ctx.addIssue({
        code: z.ZodIssueCode.custom,
        message:
          "channels.discord.autoPresence.minUpdateIntervalMs must be less than or equal to channels.discord.autoPresence.intervalMs",
        path: ["autoPresence", "minUpdateIntervalMs"],
      });
    }

    // DM allowlist validation is enforced at DiscordConfigSchema so account entries
    // can inherit top-level allowFrom via runtime shallow merge.
  });

export const DiscordConfigSchema = DiscordAccountSchema.extend({
  accounts: z.record(z.string(), DiscordAccountSchema.optional()).optional(),
  defaultAccount: z.string().optional(),
}).superRefine((value, ctx) => {
  const dmPolicy = value.dmPolicy ?? value.dm?.policy ?? "pairing";
  const allowFrom = value.allowFrom ?? value.dm?.allowFrom;
  const allowFromPath =
    value.allowFrom !== undefined ? (["allowFrom"] as const) : (["dm", "allowFrom"] as const);
  requireOpenAllowFrom({
    policy: dmPolicy,
    allowFrom,
    ctx,
    path: [...allowFromPath],
    message:
      'channels.discord.dmPolicy="open" requires channels.discord.allowFrom (or channels.discord.dm.allowFrom) to include "*"',
  });
  requireAllowlistAllowFrom({
    policy: dmPolicy,
    allowFrom,
    ctx,
    path: [...allowFromPath],
    message:
      'channels.discord.dmPolicy="allowlist" requires channels.discord.allowFrom (or channels.discord.dm.allowFrom) to contain at least one sender ID',
  });

  if (!value.accounts) {
    return;
  }
  for (const [accountId, account] of Object.entries(value.accounts)) {
    if (!account) {
      continue;
    }
    const effectivePolicy =
      account.dmPolicy ?? account.dm?.policy ?? value.dmPolicy ?? value.dm?.policy ?? "pairing";
    const effectiveAllowFrom =
      account.allowFrom ?? account.dm?.allowFrom ?? value.allowFrom ?? value.dm?.allowFrom;
    requireOpenAllowFrom({
      policy: effectivePolicy,
      allowFrom: effectiveAllowFrom,
      ctx,
      path: ["accounts", accountId, "allowFrom"],
      message:
        'channels.discord.accounts.*.dmPolicy="open" requires channels.discord.accounts.*.allowFrom (or channels.discord.allowFrom) to include "*"',
    });
    requireAllowlistAllowFrom({
      policy: effectivePolicy,
      allowFrom: effectiveAllowFrom,
      ctx,
      path: ["accounts", accountId, "allowFrom"],
      message:
        'channels.discord.accounts.*.dmPolicy="allowlist" requires channels.discord.accounts.*.allowFrom (or channels.discord.allowFrom) to contain at least one sender ID',
    });
  }
});
