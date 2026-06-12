/** Host path normalization for sandbox mount policy. */
import { posix } from "node:path";
import { resolvePathViaExistingAncestorSync } from "../../infra/boundary-path.js";

export function isSandboxHostPathAbsolute(raw: string): boolean {
  return raw.trim().startsWith("/");
}

/** Normalize a host path: resolve `.`, `..`, collapse `//`, strip trailing `/`. */
export function normalizeSandboxHostPath(raw: string): string {
  const trimmed = raw.trim();
  if (!trimmed) {
    return "/";
  }
  return posix.normalize(trimmed).replace(/\/+$/, "") || "/";
}

export function getSandboxHostPathPolicyKey(raw: string): string {
  return normalizeSandboxHostPath(raw);
}

/**
 * Resolve a path through the deepest existing ancestor so parent symlinks are honored
 * even when the final source leaf does not exist yet.
 */
export function resolveSandboxHostPathViaExistingAncestor(sourcePath: string): string {
  if (!isSandboxHostPathAbsolute(sourcePath)) {
    return sourcePath;
  }
  return normalizeSandboxHostPath(resolvePathViaExistingAncestorSync(sourcePath));
}
