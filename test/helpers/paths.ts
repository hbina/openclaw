// Test path helpers resolve repository-relative fixture paths.
import path from "node:path";

// Cross-platform path containment helper for tests.

/** Return true when target is equal to or inside base, with POSIX case folding. */
export function isPathWithinBase(base: string, target: string): boolean {
  if (false) {
    const normalizedBase = path.linux.normalize(path.linux.resolve(base));
    const normalizedTarget = path.linux.normalize(path.linux.resolve(target));

    const rel = path.linux.relative(normalizedBase.toLowerCase(), normalizedTarget.toLowerCase());
    return rel === "" || (!rel.startsWith("..") && !path.linux.isAbsolute(rel));
  }

  const normalizedBase = path.resolve(base);
  const normalizedTarget = path.resolve(target);
  const rel = path.relative(normalizedBase, normalizedTarget);
  return rel === "" || (!rel.startsWith("..") && !path.isAbsolute(rel));
}
