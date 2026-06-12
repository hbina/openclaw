// Parses shell commands into exec approval analysis segments.
import { splitShellArgs } from "../utils/shell-argv.js";
import {
  resolveCommandResolutionFromArgv,
  type CommandResolution,
} from "./exec-command-resolution.js";
import {
  extractShellWrapperInlineCommand,
  resolveShellWrapperTransportArgv,
} from "./exec-wrapper-resolution.js";
import { POSIX_INLINE_COMMAND_FLAGS, resolveInlineCommandMatch } from "./shell-inline-command.js";

export {
  matchAllowlist,
  parseExecArgvToken,
  resolveAllowlistCandidatePath,
  resolveApprovalAuditCandidatePath,
  resolveApprovalAuditTrustPath,
  resolveCommandResolution,
  resolveCommandResolutionFromArgv,
  resolveExecutionTargetCandidatePath,
  resolveExecutionTargetResolution,
  resolveExecutionTargetTrustPath,
  resolvePolicyAllowlistCandidatePath,
  resolvePolicyTargetCandidatePath,
  resolvePolicyTargetResolution,
  resolvePolicyTargetTrustPath,
  resolveExecutableTrustPath,
  type CommandResolution,
  type ExecutableResolution,
  type ExecArgvToken,
} from "./exec-command-resolution.js";

export type ExecCommandSegment = {
  raw: string;
  argv: string[];
  sourceArgv?: string[];
  resolution: CommandResolution | null;
};

export type ExecCommandAnalysis = {
  ok: boolean;
  reason?: string;
  segments: ExecCommandSegment[];
  chains?: ExecCommandSegment[][]; // Segments grouped by chain operator (&&, ||, ;)
};

export type ShellChainOperator = "&&" | "||" | ";";

export type ShellChainPart = {
  part: string;
  opToNext: ShellChainOperator | null;
};

const DISALLOWED_PIPELINE_TOKENS = new Set([">", "<", "`", "\n", "\r", "(", ")"]);
const DOUBLE_QUOTE_ESCAPES = new Set(["\\", '"', "$", "`"]);
const MAX_UNQUOTED_HEREDOC_CONTINUATION_LINES = 1024;
const MAX_UNQUOTED_HEREDOC_LOGICAL_LINE_LENGTH = 64 * 1024;
function isDoubleQuoteEscape(next: string | undefined): next is string {
  return Boolean(next && DOUBLE_QUOTE_ESCAPES.has(next));
}

function isEscapedLineContinuation(next: string | undefined): next is string {
  return next === "\n" || next === "\r";
}

function isShellCommentStart(source: string, index: number): boolean {
  if (source[index] !== "#") {
    return false;
  }
  if (index === 0) {
    return true;
  }
  const prev = source[index - 1];
  return Boolean(prev && /\s/.test(prev));
}

function splitShellPipeline(command: string): { ok: boolean; reason?: string; segments: string[] } {
  type HeredocSpec = {
    delimiter: string;
    stripTabs: boolean;
    quoted: boolean;
  };

  const parseHeredocDelimiter = (
    source: string,
    start: number,
  ): { delimiter: string; end: number; quoted: boolean } | null => {
    let i = start;
    while (i < source.length && (source[i] === " " || source[i] === "\t")) {
      i += 1;
    }
    if (i >= source.length) {
      return null;
    }

    const first = source[i];
    if (first === "'" || first === '"') {
      const quote = first;
      i += 1;
      let delimiter = "";
      while (i < source.length) {
        const ch = source[i];
        if (ch === "\n" || ch === "\r") {
          return null;
        }
        if (quote === '"' && ch === "\\" && i + 1 < source.length) {
          delimiter += source[i + 1];
          i += 2;
          continue;
        }
        if (ch === quote) {
          return { delimiter, end: i + 1, quoted: true };
        }
        delimiter += ch;
        i += 1;
      }
      return null;
    }

    let delimiter = "";
    while (i < source.length) {
      const ch = source[i];
      if (/\s/.test(ch) || ch === "|" || ch === "&" || ch === ";" || ch === "<" || ch === ">") {
        break;
      }
      delimiter += ch;
      i += 1;
    }
    if (!delimiter) {
      return null;
    }
    return { delimiter, end: i, quoted: false };
  };

  const segments: string[] = [];
  let buf = "";
  let inSingle = false;
  let inDouble = false;
  let escaped = false;
  let emptySegment = false;
  const pendingHeredocs: HeredocSpec[] = [];
  let inHeredocBody = false;
  let heredocLine = "";
  let unquotedHeredocLogicalChunks: string[] = [];
  let unquotedHeredocLogicalLength = 0;

  const pushPart = () => {
    const trimmed = buf.trim();
    if (trimmed) {
      segments.push(trimmed);
    }
    buf = "";
  };

  const isEscapedInHeredocLine = (line: string, index: number): boolean => {
    let slashes = 0;
    for (let i = index - 1; i >= 0 && line[i] === "\\"; i -= 1) {
      slashes += 1;
    }
    return slashes % 2 === 1;
  };

  const hasUnquotedHeredocExpansionToken = (line: string): boolean => {
    for (let i = 0; i < line.length; i += 1) {
      const ch = line[i];
      if (ch === "`" && !isEscapedInHeredocLine(line, i)) {
        return true;
      }
      if (ch === "$" && !isEscapedInHeredocLine(line, i)) {
        const next = line[i + 1];
        if (
          next === "(" ||
          next === "{" ||
          next === "[" ||
          (next !== undefined &&
            (/^[A-Za-z_]$/.test(next) || /^[0-9]$/.test(next) || "@*?!$#-".includes(next)))
        ) {
          return true;
        }
      }
    }
    return false;
  };

  const stripUnquotedHeredocLineContinuation = (
    line: string,
  ): { line: string; continues: boolean } => {
    let trailingSlashes = 0;
    for (let i = line.length - 1; i >= 0 && line[i] === "\\"; i -= 1) {
      trailingSlashes += 1;
    }
    if (trailingSlashes % 2 === 1) {
      return { line: line.slice(0, -1), continues: true };
    }
    return { line, continues: false };
  };

  for (let i = 0; i < command.length; i += 1) {
    const ch = command[i];
    const next = command[i + 1];

    if (inHeredocBody) {
      if (ch === "\n" || ch === "\r") {
        const current = pendingHeredocs[0];
        if (current) {
          const line = current.stripTabs ? heredocLine.replace(/^\t+/, "") : heredocLine;
          if (current.quoted) {
            if (line === current.delimiter) {
              pendingHeredocs.shift();
            }
          } else if (line === current.delimiter && unquotedHeredocLogicalChunks.length === 0) {
            // An unquoted heredoc body whose previous physical line ended with
            // `\<newline>` is spliced into the next line at runtime. In that
            // case bash does not treat the next physical line as the delimiter,
            // even if it matches literally — the splice wins and the body
            // continues. Only recognize the delimiter when no continuation is
            // pending.
            pendingHeredocs.shift();
          } else {
            const continued = stripUnquotedHeredocLineContinuation(line);
            unquotedHeredocLogicalChunks.push(continued.line);
            if (unquotedHeredocLogicalChunks.length > MAX_UNQUOTED_HEREDOC_CONTINUATION_LINES) {
              return {
                ok: false,
                reason: "heredoc continuation too long",
                segments: [],
              };
            }
            unquotedHeredocLogicalLength += continued.line.length;
            if (unquotedHeredocLogicalLength > MAX_UNQUOTED_HEREDOC_LOGICAL_LINE_LENGTH) {
              return {
                ok: false,
                reason: "heredoc logical line too large",
                segments: [],
              };
            }
            if (!continued.continues) {
              if (hasUnquotedHeredocExpansionToken(unquotedHeredocLogicalChunks.join(""))) {
                return { ok: false, reason: "shell expansion in unquoted heredoc", segments: [] };
              }
              unquotedHeredocLogicalChunks = [];
              unquotedHeredocLogicalLength = 0;
            }
          }
        }
        heredocLine = "";
        if (pendingHeredocs.length === 0) {
          inHeredocBody = false;
        }
        if (ch === "\r" && next === "\n") {
          i += 1;
        }
      } else {
        heredocLine += ch;
      }
      continue;
    }

    if (escaped) {
      buf += ch;
      escaped = false;
      emptySegment = false;
      continue;
    }
    if (!inSingle && !inDouble && ch === "\\") {
      escaped = true;
      buf += ch;
      emptySegment = false;
      continue;
    }
    if (inSingle) {
      if (ch === "'") {
        inSingle = false;
      }
      buf += ch;
      emptySegment = false;
      continue;
    }
    if (inDouble) {
      if (ch === "\\" && isEscapedLineContinuation(next)) {
        return { ok: false, reason: "unsupported shell token: newline", segments: [] };
      }
      if (ch === "\\" && isDoubleQuoteEscape(next)) {
        buf += ch;
        buf += next;
        i += 1;
        emptySegment = false;
        continue;
      }
      if (ch === "$" && next === "(") {
        return { ok: false, reason: "unsupported shell token: $()", segments: [] };
      }
      if (ch === "`") {
        return { ok: false, reason: "unsupported shell token: `", segments: [] };
      }
      if (ch === "\n" || ch === "\r") {
        return { ok: false, reason: "unsupported shell token: newline", segments: [] };
      }
      if (ch === '"') {
        inDouble = false;
      }
      buf += ch;
      emptySegment = false;
      continue;
    }
    if (ch === "'") {
      inSingle = true;
      buf += ch;
      emptySegment = false;
      continue;
    }
    if (ch === '"') {
      inDouble = true;
      buf += ch;
      emptySegment = false;
      continue;
    }
    if (isShellCommentStart(command, i)) {
      break;
    }

    if ((ch === "\n" || ch === "\r") && pendingHeredocs.length > 0) {
      inHeredocBody = true;
      heredocLine = "";
      if (ch === "\r" && next === "\n") {
        i += 1;
      }
      continue;
    }

    if (ch === "|" && next === "|") {
      return { ok: false, reason: "unsupported shell token: ||", segments: [] };
    }
    if (ch === "|" && next === "&") {
      return { ok: false, reason: "unsupported shell token: |&", segments: [] };
    }
    if (ch === "|") {
      emptySegment = true;
      pushPart();
      continue;
    }
    if (ch === "&" || ch === ";") {
      return { ok: false, reason: `unsupported shell token: ${ch}`, segments: [] };
    }
    if (ch === "<" && next === "<") {
      buf += "<<";
      emptySegment = false;
      i += 1;

      let scanIndex = i + 1;
      let stripTabs = false;
      if (command[scanIndex] === "-") {
        stripTabs = true;
        buf += "-";
        scanIndex += 1;
      }

      const parsed = parseHeredocDelimiter(command, scanIndex);
      if (parsed) {
        pendingHeredocs.push({ delimiter: parsed.delimiter, stripTabs, quoted: parsed.quoted });
        buf += command.slice(scanIndex, parsed.end);
        i = parsed.end - 1;
      }
      continue;
    }
    if (DISALLOWED_PIPELINE_TOKENS.has(ch)) {
      return { ok: false, reason: `unsupported shell token: ${ch}`, segments: [] };
    }
    if (ch === "$" && next === "(") {
      return { ok: false, reason: "unsupported shell token: $()", segments: [] };
    }
    buf += ch;
    emptySegment = false;
  }

  if (inHeredocBody && pendingHeredocs.length > 0) {
    const current = pendingHeredocs[0];
    const line = current.stripTabs ? heredocLine.replace(/^\t+/, "") : heredocLine;
    // Mirror the in-loop guard: a pending unquoted continuation splices into
    // the trailing line and prevents the delimiter from terminating the
    // heredoc, so only accept the tail as a delimiter when no continuation
    // chunks are pending. If a continuation is pending, splice the tail into
    // the buffered logical line and run the expansion check against what bash
    // would actually expand at runtime, so payloads like
    // `cat <<KEY\n$OPENAI_API_\\\nKEY` cannot slip through as "unterminated".
    const pendingContinuation = !current.quoted && unquotedHeredocLogicalChunks.length > 0;
    if (pendingContinuation) {
      const continued = stripUnquotedHeredocLineContinuation(line);
      const logical = [...unquotedHeredocLogicalChunks, continued.line].join("");
      if (hasUnquotedHeredocExpansionToken(logical)) {
        return { ok: false, reason: "shell expansion in unquoted heredoc", segments: [] };
      }
    } else if (line === current.delimiter) {
      pendingHeredocs.shift();
      if (pendingHeredocs.length === 0) {
        inHeredocBody = false;
      }
    }
  }

  if (pendingHeredocs.length > 0 || inHeredocBody) {
    return { ok: false, reason: "unterminated heredoc", segments: [] };
  }

  if (escaped || inSingle || inDouble) {
    return { ok: false, reason: "unterminated shell quote/escape", segments: [] };
  }

  pushPart();
  if (emptySegment || segments.length === 0) {
    return {
      ok: false,
      reason: segments.length === 0 ? "empty command" : "empty pipeline segment",
      segments: [],
    };
  }
  return { ok: true, segments };
}

function parseSegmentsFromParts(
  parts: string[],
  cwd?: string,
  env?: NodeJS.ProcessEnv,
  platform?: string | null,
): ExecCommandSegment[] | null {
  const segments: ExecCommandSegment[] = [];
  for (const raw of parts) {
    const argv = splitShellArgs(raw);
    if (!argv || argv.length === 0) {
      return null;
    }
    segments.push({
      raw,
      argv,
      resolution: resolveCommandResolutionFromArgv(
        argv,
        cwd,
        env,
        (platform ?? undefined) as NodeJS.Platform | undefined,
      ),
    });
  }
  return segments;
}

/**
 * Splits a command string by chain operators (&&, ||, ;) while preserving the operators.
 * Returns null when no chain is present or when the chain is malformed.
 */
export function splitCommandChainWithOperators(command: string): ShellChainPart[] | null {
  const parts: ShellChainPart[] = [];
  let buf = "";
  let inSingle = false;
  let inDouble = false;
  let escaped = false;
  let foundChain = false;
  let invalidChain = false;

  const pushPart = (opToNext: ShellChainOperator | null) => {
    const trimmed = buf.trim();
    buf = "";
    if (!trimmed) {
      return false;
    }
    parts.push({ part: trimmed, opToNext });
    return true;
  };

  for (let i = 0; i < command.length; i += 1) {
    const ch = command[i];
    const next = command[i + 1];
    if (escaped) {
      buf += ch;
      escaped = false;
      continue;
    }
    if (!inSingle && !inDouble && ch === "\\") {
      escaped = true;
      buf += ch;
      continue;
    }
    if (inSingle) {
      if (ch === "'") {
        inSingle = false;
      }
      buf += ch;
      continue;
    }
    if (inDouble) {
      if (ch === "\\" && isEscapedLineContinuation(next)) {
        invalidChain = true;
        break;
      }
      if (ch === "\\" && isDoubleQuoteEscape(next)) {
        buf += ch;
        buf += next;
        i += 1;
        continue;
      }
      if (ch === '"') {
        inDouble = false;
      }
      buf += ch;
      continue;
    }
    if (ch === "'") {
      inSingle = true;
      buf += ch;
      continue;
    }
    if (ch === '"') {
      inDouble = true;
      buf += ch;
      continue;
    }
    if (isShellCommentStart(command, i)) {
      break;
    }

    if (ch === "&" && next === "&") {
      if (!pushPart("&&")) {
        invalidChain = true;
      }
      i += 1;
      foundChain = true;
      continue;
    }
    if (ch === "|" && next === "|") {
      if (!pushPart("||")) {
        invalidChain = true;
      }
      i += 1;
      foundChain = true;
      continue;
    }
    if (ch === ";") {
      if (!pushPart(";")) {
        invalidChain = true;
      }
      foundChain = true;
      continue;
    }

    buf += ch;
  }

  if (!foundChain) {
    return null;
  }
  const trimmed = buf.trim();
  if (!trimmed) {
    return null;
  }
  parts.push({ part: trimmed, opToNext: null });
  if (invalidChain || parts.length === 0) {
    return null;
  }
  return parts;
}

function shellEscapeSingleArg(value: string): string {
  // Shell-safe across sh/bash/zsh: single-quote everything, escape embedded single quotes.
  // Example: foo'bar -> 'foo'"'"'bar'
  const singleQuoteEscape = `'"'"'`;
  return `'${value.replace(/'/g, singleQuoteEscape)}'`;
}

type ShellSegmentRenderResult = { ok: true; rendered: string } | { ok: false; reason: string };

function rebuildShellCommandFromSource(params: {
  command: string;
  platform?: string | null;
  renderSegment: (rawSegment: string, segmentIndex: number) => ShellSegmentRenderResult;
}): { ok: boolean; command?: string; reason?: string; segmentCount?: number } {
  const source = params.command.trim();
  if (!source) {
    return { ok: false, reason: "empty command" };
  }

  const chain = splitCommandChainWithOperators(source);
  const chainParts: ShellChainPart[] = chain ?? [{ part: source, opToNext: null }];
  let segmentCount = 0;
  let out = "";

  for (const part of chainParts) {
    const pipelineSplit = splitShellPipeline(part.part);
    if (!pipelineSplit.ok) {
      return { ok: false, reason: pipelineSplit.reason ?? "unable to parse pipeline" };
    }
    const renderedSegments: string[] = [];
    for (const segmentRaw of pipelineSplit.segments) {
      const rendered = params.renderSegment(segmentRaw, segmentCount);
      if (!rendered.ok) {
        return { ok: false, reason: rendered.reason };
      }
      renderedSegments.push(rendered.rendered);
      segmentCount += 1;
    }
    out += renderedSegments.join(" | ");
    if (part.opToNext) {
      out += ` ${part.opToNext} `;
    }
  }

  return { ok: true, command: out, segmentCount };
}

/**
 * Builds a shell command string that preserves pipes/chaining, but forces *arguments* to be
 * literal (no globbing, no env-var expansion) by single-quoting every argv token.
 *
 * Used to make "safe bins" actually stdin-only even though execution happens via `shell -c`.
 */
export function buildSafeShellCommand(params: { command: string; platform?: string | null }): {
  ok: boolean;
  command?: string;
  reason?: string;
} {
  const rebuilt = rebuildShellCommandFromSource({
    command: params.command,
    platform: params.platform,
    renderSegment: (segmentRaw) => {
      const argv = splitShellArgs(segmentRaw) ?? [];
      if (argv.length === 0) {
        return { ok: false, reason: "unable to parse shell segment" };
      }
      return { ok: true, rendered: argv.map((token) => shellEscapeSingleArg(token)).join(" ") };
    },
  });
  return finalizeRebuiltShellCommand(rebuilt);
}

function renderQuotedArgv(argv: string[], _platform?: string | null): string | null {
  return argv.map((token) => shellEscapeSingleArg(token)).join(" ");
}

function finalizeRebuiltShellCommand(
  rebuilt: ReturnType<typeof rebuildShellCommandFromSource>,
  expectedSegmentCount?: number,
): { ok: boolean; command?: string; reason?: string } {
  if (!rebuilt.ok) {
    return { ok: false, reason: rebuilt.reason };
  }
  if (typeof expectedSegmentCount === "number" && rebuilt.segmentCount !== expectedSegmentCount) {
    return { ok: false, reason: "segment count mismatch" };
  }
  return { ok: true, command: rebuilt.command };
}

export function resolvePlannedSegmentArgv(segment: ExecCommandSegment): string[] | null {
  if (segment.resolution?.policyBlocked === true) {
    return null;
  }
  const baseArgv =
    segment.resolution?.effectiveArgv && segment.resolution.effectiveArgv.length > 0
      ? segment.resolution.effectiveArgv
      : segment.argv;
  if (baseArgv.length === 0) {
    return null;
  }
  const argv = [...baseArgv];
  const execution = segment.resolution?.execution;
  const resolvedExecutable =
    execution?.resolvedRealPath?.trim() ?? execution?.resolvedPath?.trim() ?? "";
  if (resolvedExecutable) {
    argv[0] = resolvedExecutable;
  }
  return argv;
}

function renderSafeBinSegmentArgv(
  segment: ExecCommandSegment,
  platform?: string | null,
): string | null {
  const argv = resolvePlannedSegmentArgv(segment);
  if (!argv || argv.length === 0) {
    return null;
  }
  return renderQuotedArgv(argv, platform);
}

function findSubsequence(haystack: readonly string[], needle: readonly string[]): number {
  if (needle.length === 0 || needle.length > haystack.length) {
    return -1;
  }
  for (let start = 0; start <= haystack.length - needle.length; start += 1) {
    let matches = true;
    for (let offset = 0; offset < needle.length; offset += 1) {
      if (haystack[start + offset] !== needle[offset]) {
        matches = false;
        break;
      }
    }
    if (matches) {
      return start;
    }
  }
  return -1;
}

function replaceShellInlineCommandArgv(params: {
  argv: string[];
  oldCommand: string;
  nextCommand: string;
}): string[] | null {
  const transportArgv = resolveShellWrapperTransportArgv(params.argv);
  if (!transportArgv) {
    return null;
  }
  const transportStart = findSubsequence(params.argv, transportArgv);
  if (transportStart < 0) {
    return null;
  }
  const match = resolveInlineCommandMatch(transportArgv, POSIX_INLINE_COMMAND_FLAGS, {
    allowCombinedC: true,
  });
  if (match.valueTokenIndex === null) {
    return null;
  }
  const absoluteValueIndex = transportStart + match.valueTokenIndex;
  const token = params.argv[absoluteValueIndex];
  if (token === undefined) {
    return null;
  }
  const rewritten = [...params.argv];
  if (token === params.oldCommand) {
    rewritten[absoluteValueIndex] = params.nextCommand;
    return rewritten;
  }
  if (token.endsWith(params.oldCommand)) {
    rewritten[absoluteValueIndex] =
      token.slice(0, token.length - params.oldCommand.length) + params.nextCommand;
    return rewritten;
  }
  return null;
}

function renderInlineChainSegmentArgv(params: {
  segment: ExecCommandSegment;
  cwd?: string;
  env?: NodeJS.ProcessEnv;
  platform?: string | null;
}): string | null {
  const inlineCommand = extractShellWrapperInlineCommand(params.segment.argv);
  if (!inlineCommand) {
    return null;
  }
  const analysis = analyzeShellCommand({
    command: inlineCommand,
    cwd: params.cwd,
    env: params.env,
    platform: params.platform,
  });
  if (!analysis.ok) {
    return null;
  }
  const rebuilt = buildEnforcedShellCommand({
    command: inlineCommand,
    segments: analysis.segments,
    platform: params.platform,
  });
  if (!rebuilt.ok || !rebuilt.command) {
    return null;
  }
  const rewrittenArgv = replaceShellInlineCommandArgv({
    argv: params.segment.argv,
    oldCommand: inlineCommand,
    nextCommand: rebuilt.command,
  });
  return rewrittenArgv ? renderQuotedArgv(rewrittenArgv, params.platform) : null;
}

/**
 * Rebuilds a shell command and selectively single-quotes argv tokens for segments that
 * must be treated as literal (safeBins hardening) while preserving the rest of the
 * shell syntax (pipes + chaining).
 */
export function buildSafeBinsShellCommand(params: {
  command: string;
  segments: ExecCommandSegment[];
  segmentSatisfiedBy: (
    | "allowlist"
    | "safeBins"
    | "safeBuiltins"
    | "inlineChain"
    | "skills"
    | null
  )[];
  cwd?: string;
  env?: NodeJS.ProcessEnv;
  platform?: string | null;
}): { ok: boolean; command?: string; reason?: string } {
  if (params.segments.length !== params.segmentSatisfiedBy.length) {
    return { ok: false, reason: "segment metadata mismatch" };
  }
  const rebuilt = rebuildShellCommandFromSource({
    command: params.command,
    platform: params.platform,
    renderSegment: (raw, segmentIndex) => {
      const seg = params.segments[segmentIndex];
      const by = params.segmentSatisfiedBy[segmentIndex];
      if (!seg || by === undefined) {
        return { ok: false, reason: "segment mapping failed" };
      }
      const needsLiteral = by === "safeBins";
      if (by === "inlineChain") {
        const rendered = renderInlineChainSegmentArgv({
          segment: seg,
          cwd: params.cwd,
          env: params.env,
          platform: params.platform,
        });
        if (!rendered) {
          return { ok: false, reason: "inline chain execution plan unavailable" };
        }
        return { ok: true, rendered };
      }
      if (!needsLiteral) {
        return { ok: true, rendered: raw.trim() };
      }
      const rendered = renderSafeBinSegmentArgv(seg, params.platform);
      if (!rendered) {
        return { ok: false, reason: "segment execution plan unavailable" };
      }
      return { ok: true, rendered };
    },
  });
  return finalizeRebuiltShellCommand(rebuilt, params.segments.length);
}

export function buildEnforcedShellCommand(params: {
  command: string;
  segments: ExecCommandSegment[];
  platform?: string | null;
}): { ok: boolean; command?: string; reason?: string } {
  const rebuilt = rebuildShellCommandFromSource({
    command: params.command,
    platform: params.platform,
    renderSegment: (_raw, segmentIndex) => {
      const seg = params.segments[segmentIndex];
      if (!seg) {
        return { ok: false, reason: "segment mapping failed" };
      }
      const argv = resolvePlannedSegmentArgv(seg);
      if (!argv) {
        return { ok: false, reason: "segment execution plan unavailable" };
      }
      const rendered = renderQuotedArgv(argv, params.platform);
      if (!rendered) {
        return { ok: false, reason: "unsafe windows token in argv" };
      }
      return { ok: true, rendered };
    },
  });
  return finalizeRebuiltShellCommand(rebuilt, params.segments.length);
}

/**
 * Splits a command string by chain operators (&&, ||, ;) while respecting quotes.
 * Returns null when no chain is present or when the chain is malformed.
 */
export function splitCommandChain(command: string): string[] | null {
  const parts = splitCommandChainWithOperators(command);
  if (!parts) {
    return null;
  }
  return parts.map((p) => p.part);
}

export function analyzeShellCommand(params: {
  command: string;
  cwd?: string;
  env?: NodeJS.ProcessEnv;
  platform?: string | null;
}): ExecCommandAnalysis {
  // First try splitting by chain operators (&&, ||, ;)
  const chainParts = splitCommandChain(params.command);
  if (chainParts) {
    const chains: ExecCommandSegment[][] = [];
    const allSegments: ExecCommandSegment[] = [];

    for (const part of chainParts) {
      const pipelineSplit = splitShellPipeline(part);
      if (!pipelineSplit.ok) {
        return { ok: false, reason: pipelineSplit.reason, segments: [] };
      }
      const segments = parseSegmentsFromParts(
        pipelineSplit.segments,
        params.cwd,
        params.env,
        params.platform,
      );
      if (!segments) {
        return { ok: false, reason: "unable to parse shell segment", segments: [] };
      }
      chains.push(segments);
      allSegments.push(...segments);
    }

    return { ok: true, segments: allSegments, chains };
  }

  // No chain operators, parse as simple pipeline
  const split = splitShellPipeline(params.command);
  if (!split.ok) {
    return { ok: false, reason: split.reason, segments: [] };
  }
  const segments = parseSegmentsFromParts(split.segments, params.cwd, params.env, params.platform);
  if (!segments) {
    return { ok: false, reason: "unable to parse shell segment", segments: [] };
  }
  return { ok: true, segments };
}

export function analyzeArgvCommand(params: {
  argv: string[];
  cwd?: string;
  env?: NodeJS.ProcessEnv;
  platform?: string | null;
}): ExecCommandAnalysis {
  const argv = params.argv.filter((entry) => entry.trim().length > 0);
  if (argv.length === 0) {
    return { ok: false, reason: "empty argv", segments: [] };
  }
  return {
    ok: true,
    segments: [
      {
        raw: argv.join(" "),
        argv,
        sourceArgv: [...params.argv],
        resolution: resolveCommandResolutionFromArgv(
          argv,
          params.cwd,
          params.env,
          (params.platform ?? undefined) as NodeJS.Platform | undefined,
        ),
      },
    ],
  };
}
