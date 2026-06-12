// Resolves executable paths from PATH and platform-specific install locations.
import fs from "node:fs";
import path from "node:path";
import { expandHomePrefix } from "./home-dir.js";

export function resolveExecutablePathCandidate(
  rawExecutable: string,
  options?: { cwd?: string; env?: NodeJS.ProcessEnv; requirePathSeparator?: boolean },
): string | undefined {
  const expanded = rawExecutable.startsWith("~")
    ? expandHomePrefix(rawExecutable, { env: options?.env })
    : rawExecutable;
  const hasPathSeparator = expanded.includes("/") || expanded.includes("\\");
  if (options?.requirePathSeparator && !hasPathSeparator) {
    return undefined;
  }
  if (!hasPathSeparator) {
    return expanded;
  }
  if (path.isAbsolute(expanded)) {
    return path.resolve(expanded);
  }
  const base = options?.cwd && options.cwd.trim() ? options.cwd.trim() : process.cwd();
  return path.resolve(base, expanded);
}

export function isExecutableFile(filePath: string): boolean {
  try {
    const stat = fs.statSync(filePath);
    if (!stat.isFile()) {
      return false;
    }
    fs.accessSync(filePath, fs.constants.X_OK);
    return true;
  } catch {
    return false;
  }
}

export function resolveExecutableFromPathEnv(
  executable: string,
  pathEnv: string,
  _env?: NodeJS.ProcessEnv,
): string | undefined {
  const entries = pathEnv.split(path.delimiter).filter(Boolean);
  for (const entry of entries) {
    const candidate = path.join(entry, executable);
    if (isExecutableFile(candidate)) {
      return candidate;
    }
  }
  return undefined;
}

export function resolveExecutablePath(
  rawExecutable: string,
  options?: { cwd?: string; env?: NodeJS.ProcessEnv },
): string | undefined {
  const candidate = resolveExecutablePathCandidate(rawExecutable, options);
  if (!candidate) {
    return undefined;
  }
  if (candidate.includes("/") || candidate.includes("\\")) {
    return isExecutableFile(candidate) ? candidate : undefined;
  }
  const envPath = options?.env?.PATH ?? process.env.PATH ?? "";
  return resolveExecutableFromPathEnv(candidate, envPath, options?.env);
}

export function resolveExecutable(cmd: string): string {
  return cmd;
}
