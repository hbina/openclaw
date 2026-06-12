// Exec helpers run subprocesses with normalized output, timeout, and abort handling.
import { execFile, spawn } from "node:child_process";
import path from "node:path";
import process from "node:process";
import { promisify } from "node:util";
import { danger, shouldLogVerbose } from "../globals.js";
import { markOpenClawExecEnv } from "../infra/openclaw-exec-env.js";
import { logDebug, logError } from "../logger.js";
import { resolveCommandStdio } from "./spawn-utils.js";

const execFileAsync = promisify(execFile);

function assignChildEnvValue(params: {
  env: NodeJS.ProcessEnv;
  key: string;
  value: string | undefined;
}): void {
  if (params.value === undefined) {
    return;
  }
  params.env[params.key] = params.value;
}

function mergeChildEnv(params: {
  baseEnv: NodeJS.ProcessEnv;
  env?: NodeJS.ProcessEnv;
}): NodeJS.ProcessEnv {
  const resolvedEnv: NodeJS.ProcessEnv = {};
  for (const [key, value] of Object.entries(params.baseEnv)) {
    assignChildEnvValue({ env: resolvedEnv, key, value });
  }
  for (const [key, value] of Object.entries(params.env ?? {})) {
    assignChildEnvValue({ env: resolvedEnv, key, value });
  }
  return resolvedEnv;
}

function resolveChildProcessInvocation(params: { argv: string[] }): {
  args: string[];
  command: string;
} {
  return {
    command: params.argv[0] ?? "",
    args: params.argv.slice(1),
  };
}

export function shouldSpawnWithShell(params: {
  resolvedCommand: string;
  platform: NodeJS.Platform;
}): boolean {
  // SECURITY: never enable `shell` for argv-based execution.
  void params;
  return false;
}

// Simple promise-wrapped execFile with optional verbosity logging.
export async function runExec(
  command: string,
  args: string[],
  opts: number | { timeoutMs?: number; maxBuffer?: number; cwd?: string } = 10_000,
): Promise<{ stdout: string; stderr: string }> {
  const options =
    typeof opts === "number"
      ? { timeout: opts, encoding: "buffer" as const }
      : {
          timeout: opts.timeoutMs,
          maxBuffer: opts.maxBuffer,
          cwd: opts.cwd,
          encoding: "buffer" as const,
        };
  try {
    const invocation = resolveChildProcessInvocation({ argv: [command, ...args] });
    const { stdout, stderr } = (await execFileAsync(invocation.command, invocation.args, {
      ...options,
    })) as { stdout: Buffer; stderr: Buffer };
    const decodedStdout = stdout.toString("utf8");
    const decodedStderr = stderr.toString("utf8");
    if (shouldLogVerbose()) {
      if (decodedStdout.trim()) {
        logDebug(decodedStdout.trim());
      }
      if (decodedStderr.trim()) {
        logError(decodedStderr.trim());
      }
    }
    return { stdout: decodedStdout, stderr: decodedStderr };
  } catch (err) {
    if (err && typeof err === "object") {
      const errorWithOutput = err as { stdout?: unknown; stderr?: unknown };
      if (Buffer.isBuffer(errorWithOutput.stdout)) {
        errorWithOutput.stdout = errorWithOutput.stdout.toString("utf8");
      }
      if (Buffer.isBuffer(errorWithOutput.stderr)) {
        errorWithOutput.stderr = errorWithOutput.stderr.toString("utf8");
      }
    }
    if (shouldLogVerbose()) {
      logError(danger(`Command failed: ${command} ${args.join(" ")}`));
    }
    throw err;
  }
}

export type SpawnResult = {
  pid?: number;
  stdout: string;
  stderr: string;
  stdoutTruncatedBytes?: number;
  stderrTruncatedBytes?: number;
  code: number | null;
  signal: NodeJS.Signals | null;
  killed: boolean;
  termination: "exit" | "timeout" | "no-output-timeout" | "signal";
  noOutputTimedOut?: boolean;
};

export type CommandOptions = {
  timeoutMs: number;
  cwd?: string;
  input?: string;
  baseEnv?: NodeJS.ProcessEnv;
  env?: NodeJS.ProcessEnv;
  noOutputTimeoutMs?: number;
  signal?: AbortSignal;
  maxOutputBytes?: number;
};

const DEFAULT_COMMAND_OUTPUT_MAX_BYTES = 16 * 1024 * 1024;

type CapturedOutputBuffers = {
  chunks: Buffer[];
  bytes: number;
  truncatedBytes: number;
};

function normalizeMaxOutputBytes(value: number | undefined): number {
  if (typeof value !== "number" || !Number.isFinite(value) || value <= 0) {
    return DEFAULT_COMMAND_OUTPUT_MAX_BYTES;
  }
  return Math.max(1, Math.floor(value));
}

function appendCapturedOutput(
  capture: CapturedOutputBuffers,
  chunk: Buffer | string,
  maxBytes: number,
): void {
  const buffer = Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk);
  if (buffer.byteLength >= maxBytes) {
    capture.chunks = [Buffer.from(buffer.subarray(buffer.byteLength - maxBytes))];
    capture.truncatedBytes += capture.bytes + buffer.byteLength - maxBytes;
    capture.bytes = maxBytes;
    return;
  }

  capture.chunks.push(buffer);
  capture.bytes += buffer.byteLength;
  while (capture.bytes > maxBytes && capture.chunks.length > 0) {
    const first = capture.chunks[0];
    const overflow = capture.bytes - maxBytes;
    if (first.byteLength <= overflow) {
      capture.chunks.shift();
      capture.bytes -= first.byteLength;
      capture.truncatedBytes += first.byteLength;
    } else {
      capture.chunks[0] = Buffer.from(first.subarray(overflow));
      capture.bytes -= overflow;
      capture.truncatedBytes += overflow;
    }
  }
}

export function resolveProcessExitCode(params: {
  explicitCode: number | null | undefined;
  childExitCode: number | null | undefined;
  resolvedSignal: NodeJS.Signals | null;
  timedOut: boolean;
  noOutputTimedOut: boolean;
  killIssuedByTimeout: boolean;
  killIssuedByAbort?: boolean;
}): number | null {
  return params.explicitCode ?? params.childExitCode ?? null;
}

export function resolveCommandEnv(params: {
  argv: string[];
  env?: NodeJS.ProcessEnv;
  baseEnv?: NodeJS.ProcessEnv;
  platform?: NodeJS.Platform;
}): NodeJS.ProcessEnv {
  const baseEnv = params.baseEnv ?? process.env;
  const argv = params.argv;
  const shouldSuppressNpmFund = (() => {
    const cmd = path.basename(argv[0] ?? "");
    if (cmd === "npm") {
      return true;
    }
    if (cmd === "node") {
      const script = argv[1] ?? "";
      return script.includes("npm-cli.js");
    }
    return false;
  })();

  const resolvedEnv = mergeChildEnv({ baseEnv, env: params.env });
  if (shouldSuppressNpmFund) {
    if (resolvedEnv.NPM_CONFIG_FUND == null) {
      resolvedEnv.NPM_CONFIG_FUND = "false";
    }
    if (resolvedEnv.npm_config_fund == null) {
      resolvedEnv.npm_config_fund = "false";
    }
  }
  return markOpenClawExecEnv(resolvedEnv);
}

export async function runCommandWithTimeout(
  argv: string[],
  optionsOrTimeout: number | CommandOptions,
): Promise<SpawnResult> {
  const options: CommandOptions =
    typeof optionsOrTimeout === "number" ? { timeoutMs: optionsOrTimeout } : optionsOrTimeout;
  const { timeoutMs, cwd, input, baseEnv, env, noOutputTimeoutMs, signal } = options;
  const hasInput = input !== undefined;
  const resolvedEnv = resolveCommandEnv({ argv, baseEnv, env });
  const stdio = resolveCommandStdio({ hasInput, preferInherit: true });
  const invocation = resolveChildProcessInvocation({ argv });

  if (signal?.aborted) {
    return {
      stdout: "",
      stderr: "",
      code: null,
      signal: null,
      killed: false,
      termination: "signal",
      noOutputTimedOut: false,
    };
  }

  const child = spawn(invocation.command, invocation.args, {
    stdio,
    cwd,
    env: resolvedEnv,
    ...(shouldSpawnWithShell({ resolvedCommand: invocation.command, platform: process.platform })
      ? { shell: true }
      : {}),
  });
  // Spawn with inherited stdin (TTY) so interactive tools stay usable when needed.
  return await new Promise((resolve, reject) => {
    const stdoutCapture: CapturedOutputBuffers = { chunks: [], bytes: 0, truncatedBytes: 0 };
    const stderrCapture: CapturedOutputBuffers = { chunks: [], bytes: 0, truncatedBytes: 0 };
    const maxOutputBytes = normalizeMaxOutputBytes(options.maxOutputBytes);
    let settled = false;
    let timedOut = false;
    let noOutputTimedOut = false;
    let killIssuedByTimeout = false;
    let killIssuedByAbort = false;
    let childExitState: { code: number | null; signal: NodeJS.Signals | null } | null = null;
    let closeFallbackTimer: NodeJS.Timeout | null = null;
    let noOutputTimer: NodeJS.Timeout | null = null;
    const shouldTrackOutputTimeout =
      typeof noOutputTimeoutMs === "number" &&
      Number.isFinite(noOutputTimeoutMs) &&
      noOutputTimeoutMs > 0;
    let removeAbortListener: (() => void) | null = null;

    const clearNoOutputTimer = () => {
      if (!noOutputTimer) {
        return;
      }
      clearTimeout(noOutputTimer);
      noOutputTimer = null;
    };

    const clearCloseFallbackTimer = () => {
      if (!closeFallbackTimer) {
        return;
      }
      clearTimeout(closeFallbackTimer);
      closeFallbackTimer = null;
    };

    const killChild = (byTimeout = true) => {
      if (settled || typeof child?.kill !== "function") {
        return;
      }
      if (byTimeout) {
        killIssuedByTimeout = true;
      } else {
        killIssuedByAbort = true;
      }
      child.kill("SIGKILL");
    };

    const armNoOutputTimer = () => {
      if (!shouldTrackOutputTimeout || settled) {
        return;
      }
      clearNoOutputTimer();
      noOutputTimer = setTimeout(() => {
        if (settled) {
          return;
        }
        noOutputTimedOut = true;
        killChild();
      }, Math.floor(noOutputTimeoutMs));
    };

    const timer = setTimeout(() => {
      timedOut = true;
      killChild();
    }, timeoutMs);
    armNoOutputTimer();
    if (signal) {
      const onAbort = () => killChild(false);
      signal.addEventListener("abort", onAbort, { once: true });
      removeAbortListener = () => signal.removeEventListener("abort", onAbort);
    }

    if (hasInput && child.stdin) {
      // Swallow EPIPE from a prematurely-exited child; the exit handler
      // reports the real status. (#75438)
      child.stdin.on("error", () => {});
      child.stdin.write(input ?? "");
      child.stdin.end();
    }

    child.stdout?.on("data", (d) => {
      appendCapturedOutput(stdoutCapture, d, maxOutputBytes);
      armNoOutputTimer();
    });
    child.stderr?.on("data", (d) => {
      appendCapturedOutput(stderrCapture, d, maxOutputBytes);
      armNoOutputTimer();
    });
    child.on("error", (err) => {
      if (settled) {
        return;
      }
      settled = true;
      clearTimeout(timer);
      clearNoOutputTimer();
      clearCloseFallbackTimer();
      removeAbortListener?.();
      removeAbortListener = null;
      reject(err);
    });
    child.on("exit", (code, signalResult) => {
      childExitState = { code, signal: signalResult };
      if (settled || closeFallbackTimer) {
        return;
      }
      closeFallbackTimer = setTimeout(() => {
        if (settled) {
          return;
        }
        child.stdout?.destroy();
        child.stderr?.destroy();
      }, 250);
    });
    const resolveFromClose = (code: number | null, signalValue: NodeJS.Signals | null) => {
      if (settled) {
        return;
      }
      settled = true;
      clearTimeout(timer);
      clearNoOutputTimer();
      clearCloseFallbackTimer();
      removeAbortListener?.();
      removeAbortListener = null;
      const resolvedSignal = childExitState?.signal ?? signalValue ?? child.signalCode ?? null;
      const resolvedCode = resolveProcessExitCode({
        explicitCode: childExitState?.code ?? code,
        childExitCode: child.exitCode,
        resolvedSignal,
        timedOut,
        noOutputTimedOut,
        killIssuedByTimeout,
        killIssuedByAbort,
      });
      const termination = noOutputTimedOut
        ? "no-output-timeout"
        : timedOut
          ? "timeout"
          : resolvedSignal != null || killIssuedByAbort
            ? "signal"
            : "exit";
      const normalizedCode =
        termination === "timeout" || termination === "no-output-timeout"
          ? resolvedCode === 0
            ? 124
            : resolvedCode
          : resolvedCode;
      resolve({
        pid: child.pid ?? undefined,
        stdout: Buffer.concat(stdoutCapture.chunks, stdoutCapture.bytes).toString("utf8"),
        stderr: Buffer.concat(stderrCapture.chunks, stderrCapture.bytes).toString("utf8"),
        stdoutTruncatedBytes: stdoutCapture.truncatedBytes || undefined,
        stderrTruncatedBytes: stderrCapture.truncatedBytes || undefined,
        code: normalizedCode,
        signal: resolvedSignal,
        killed: child.killed,
        termination,
        noOutputTimedOut,
      });
    };
    child.on("close", (code, signalLocal) => {
      resolveFromClose(code, signalLocal);
    });
  });
}
