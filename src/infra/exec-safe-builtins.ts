// Classifies closed POSIX shell builtins for exec allowlist checks.
import type { ExecCommandSegment } from "./exec-approvals-analysis.js";

// POSIX shell builtins that cannot execute external code or mutate environment state on their
// own. Shell allowlist evaluation handles them as a closed internal set instead of path-based
// safeBins matching.
const DEFAULT_SAFE_BUILTINS: ReadonlySet<string> = new Set([
  ":",
  "cd",
  "false",
  "pwd",
  "test",
  "true",
]);

/** Returns true when a parsed POSIX shell segment is one of the closed safe builtin forms. */
export function isSafeBuiltinSegment(params: {
  segment: ExecCommandSegment;
  platform?: string | null;
}): boolean {
  const head = params.segment.argv[0]?.trim().toLowerCase();
  if (!head) {
    return false;
  }
  if (head === "[") {
    return params.segment.argv.at(-1) === "]";
  }
  return DEFAULT_SAFE_BUILTINS.has(head);
}
