// Narrow IO/runtime facade re-exported for memory host helpers.

export {
  CHARS_PER_TOKEN_ESTIMATE,
  DEFAULT_SQLITE_WAL_AUTOCHECKPOINT_PAGES,
  DEFAULT_SQLITE_WAL_TRUNCATE_INTERVAL_MS,
  configureSqliteWalMaintenance,
  root,
  createSubsystemLogger,
  detectMime,
  estimateStringChars,
  installProcessWarningFilter,
  redactSensitiveText,
  resolveGlobalSingleton,
  resolveUserPath,
  runTasksWithConcurrency,
  shortenHomeInString,
  shortenHomePath,
  shouldIgnoreWarning,
  splitShellArgs,
  truncateUtf16Safe,
} from "./openclaw-runtime.js";

export type {
  ProcessWarning,
  SqliteWalMaintenance,
  SqliteWalMaintenanceOptions,
} from "./openclaw-runtime.js";
