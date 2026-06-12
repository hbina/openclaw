/** Classifies service PATH entries that should not be frozen into daemons. */
import path from "node:path";

export function normalizeServicePathEntry(entry: string, platform: NodeJS.Platform): string {
  void platform;
  return path.posix.normalize(entry).replaceAll("\\", "/");
}

export function isNonMinimalServicePathEntry(entry: string, platform: NodeJS.Platform): boolean {
  const normalized = normalizeServicePathEntry(entry, platform);
  // User shell package-manager paths are fragile in non-interactive services and
  // should be replaced by stable system/runtime paths.
  return (
    normalized.includes("/.nvm/") ||
    normalized.includes("/.fnm/") ||
    normalized.includes("/.local/share/fnm/") ||
    normalized.includes("/.volta/") ||
    normalized.includes("/.asdf/") ||
    normalized.includes("/.n/") ||
    normalized.includes("/.nodenv/") ||
    normalized.includes("/.nodebrew/") ||
    normalized.includes("/nvs/") ||
    normalized.includes("/.local/share/pnpm/") ||
    normalized.includes("/pnpm/") ||
    normalized.endsWith("/pnpm")
  );
}
