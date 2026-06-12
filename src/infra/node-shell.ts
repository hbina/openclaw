// Node shell command construction keeps platform shell flags centralized for
// system.run and related command execution paths.
/** Build argv for running a command through the platform default shell. */
export function buildNodeShellCommand(command: string, _platform?: string | null) {
  return ["/bin/sh", "-lc", command];
}
