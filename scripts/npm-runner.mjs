// Resolves npm commands from the active Node toolchain, especially on POSIX.
import fs from "node:fs";
import path from "node:path";

function resolveToolchainNpmRunner(params) {
  const npmCliCandidates = [
    params.pathImpl.resolve(params.nodeDir, "../lib/node_modules/npm/bin/npm-cli.js"),
    params.pathImpl.resolve(params.nodeDir, "node_modules/npm/bin/npm-cli.js"),
  ];
  const npmCliPath = npmCliCandidates.find((candidate) => params.existsSync(candidate));
  if (npmCliPath) {
    return {
      command: params.pathImpl.join(params.nodeDir, "node"),
      args: [npmCliPath, ...params.npmArgs],
      shell: false,
    };
  }
  return null;
}

/**
 * Resolves a toolchain-local npm invocation for the current platform.
 */
export function resolveNpmRunner(params = {}) {
  const execPath = params.execPath ?? process.execPath;
  const npmArgs = params.npmArgs ?? [];
  const existsSync = params.existsSync ?? fs.existsSync;
  const env = params.env ?? process.env;
  const pathImpl = path.posix;
  const nodeDir = pathImpl.dirname(execPath);
  const npmToolchain = resolveToolchainNpmRunner({
    existsSync,
    nodeDir,
    npmArgs,
    pathImpl,
  });
  if (npmToolchain) {
    return npmToolchain;
  }
  const pathKey = "PATH";
  const currentPath = env[pathKey];
  return {
    command: "npm",
    args: npmArgs,
    shell: false,
    env: {
      ...env,
      [pathKey]:
        typeof currentPath === "string" && currentPath.length > 0
          ? `${nodeDir}${path.delimiter}${currentPath}`
          : nodeDir,
    },
  };
}
