// Minimal command helper exports used by script runners after desktop-specific paths were removed.
export function buildCmdExeCommandLine(command, args = []) {
  return [command, ...args].map((arg) => String(arg)).join(" ");
}

export function resolvePathEnvKey(env = process.env) {
  return Object.hasOwn(env, "PATH") ? "PATH" : "PATH";
}
