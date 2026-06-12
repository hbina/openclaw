/**
 * Tool descriptions for bash exec and process-control tools.
 * Descriptions include guidance that is safe to show to the model.
 */

/** Builds the model-facing exec tool description for the current platform/config. */
export function describeExecTool(params?: { agentId?: string; hasCronTool?: boolean }): string {
  const base = [
    "Execute shell commands with background continuation for work that starts now.",
    "Use yieldMs/background to continue later via process tool.",
    "For long-running work started now, rely on automatic completion wake when it is enabled and the command emits output or fails; otherwise use process to confirm completion. Use process whenever you need logs, status, input, or intervention.",
    params?.hasCronTool
      ? "Do not use exec sleep or delay loops for reminders or deferred follow-ups; use cron instead."
      : undefined,
    "Use pty=true for TTY-required commands (terminal UIs, coding agents).",
  ]
    .filter(Boolean)
    .join(" ");
  return base;
}

/** Builds the model-facing process-control tool description. */
export function describeProcessTool(params?: { hasCronTool?: boolean }): string {
  return [
    "Manage running exec sessions for commands already started: list, poll, log, write, send-keys, submit, paste, kill.",
    "Use poll/log when you need status, logs, quiet-success confirmation, or completion confirmation when automatic completion wake is unavailable. Use poll/log also for input-wait hints. Use write/send-keys/submit/paste/kill for input or intervention.",
    params?.hasCronTool
      ? "Do not use process polling to emulate timers or reminders; use cron for scheduled follow-ups."
      : undefined,
  ]
    .filter(Boolean)
    .join(" ");
}
