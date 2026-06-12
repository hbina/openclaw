// Normalizes executable tokens used by wrapper and policy analysis.
import path from "node:path";
import { normalizeLowercaseStringOrEmpty } from "@openclaw/normalization-core/string-coerce";

/** Return a lowercase POSIX basename. */
export function basenameLower(token: string): string {
  return normalizeLowercaseStringOrEmpty(path.posix.basename(token));
}

/** Normalize an executable token for wrapper and policy matching. */
export function normalizeExecutableToken(token: string): string {
  return basenameLower(token);
}
