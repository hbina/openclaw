/** Config preflight for doctor: state migration, recovery, and snapshot loading. */
import { note } from "../../packages/terminal-core/src/note.js";
import {
  readConfigFileSnapshot,
  recoverConfigFromJsonRootSuffix,
  recoverConfigFromLastKnownGood,
} from "../config/io.js";
import { formatConfigIssueLines } from "../config/issue-format.js";
import type { OpenClawConfig } from "../config/types.openclaw.js";
import { isTruthyEnvValue } from "../infra/env.js";
import { noteIncludeConfinementWarning } from "./doctor-config-analysis.js";

type DoctorStateMigrationsModule = typeof import("./doctor-state-migrations.js");
type DoctorCronModule = typeof import("./doctor/cron/index.js");

let doctorStateMigrationsPromise: Promise<DoctorStateMigrationsModule> | null = null;
let doctorCronPromise: Promise<DoctorCronModule> | null = null;

function loadDoctorStateMigrations(): Promise<DoctorStateMigrationsModule> {
  doctorStateMigrationsPromise ??= import("./doctor-state-migrations.js");
  return doctorStateMigrationsPromise;
}

function loadDoctorCron(): Promise<DoctorCronModule> {
  doctorCronPromise ??= import("./doctor/cron/index.js");
  return doctorCronPromise;
}

export type DoctorConfigPreflightResult = {
  snapshot: Awaited<ReturnType<typeof readConfigFileSnapshot>>;
  baseConfig: OpenClawConfig;
};

/** Returns true during updater-managed config rewrites where plugin validation may be stale. */
export function shouldSkipPluginValidationForDoctorConfigPreflight(
  env: NodeJS.ProcessEnv = process.env,
): boolean {
  return isTruthyEnvValue(env.OPENCLAW_UPDATE_IN_PROGRESS);
}

function noteStateMigrationResult(result: { changes: string[]; warnings: string[] }): void {
  if (result.changes.length > 0) {
    note(result.changes.map((entry) => `- ${entry}`).join("\n"), "Doctor changes");
  }
  if (result.warnings.length > 0) {
    note(result.warnings.map((entry) => `- ${entry}`).join("\n"), "Doctor warnings");
  }
}

/**
 * Runs early doctor config checks before the main config repair flow.
 *
 * It may migrate legacy state paths, recover corrupt target config when requested, and
 * returns the best-effort config snapshot used by later doctor checks.
 */
export async function runDoctorConfigPreflight(
  options: {
    migrateState?: boolean;
    repairPrefixedConfig?: boolean;
    recoverCorruptTargetStore?: boolean;
    invalidConfigNote?: string | false;
  } = {},
): Promise<DoctorConfigPreflightResult> {
  if (options.migrateState !== false) {
    const { autoMigrateLegacyStateDir } = await loadDoctorStateMigrations();
    const stateDirResult = await autoMigrateLegacyStateDir({ env: process.env });
    noteStateMigrationResult(stateDirResult);
  }

  const readOptions = {
    skipPluginValidation: shouldSkipPluginValidationForDoctorConfigPreflight(),
  };
  let snapshot = await readConfigFileSnapshot(readOptions);
  if (options.repairPrefixedConfig === true && snapshot.exists && !snapshot.valid) {
    if (await recoverConfigFromJsonRootSuffix(snapshot)) {
      note("Removed non-JSON prefix from openclaw.json; original saved as .clobbered.*.", "Config");
      snapshot = await readConfigFileSnapshot(readOptions);
    } else if (
      await recoverConfigFromLastKnownGood({ snapshot, reason: "doctor-invalid-config" })
    ) {
      note(
        "Restored openclaw.json from last-known-good; original saved as .clobbered.*.",
        "Config",
      );
      snapshot = await readConfigFileSnapshot(readOptions);
    }
  }
  const invalidConfigNote =
    options.invalidConfigNote ?? "Config invalid; doctor will run with best-effort config.";
  if (
    invalidConfigNote &&
    snapshot.exists &&
    !snapshot.valid &&
    snapshot.legacyIssues.length === 0
  ) {
    note(invalidConfigNote, "Config");
    noteIncludeConfinementWarning(snapshot);
  }

  const warnings = snapshot.warnings ?? [];
  if (warnings.length > 0) {
    note(formatConfigIssueLines(warnings, "-").join("\n"), "Config warnings");
  }

  const baseConfig = snapshot.sourceConfig ?? snapshot.config ?? {};
  if (options.migrateState !== false) {
    if (snapshot.valid) {
      const { repairLegacyCronStoreWithoutPrompt } = await loadDoctorCron();
      const cronResult = await repairLegacyCronStoreWithoutPrompt({ cfg: baseConfig });
      noteStateMigrationResult(cronResult);
    }
    const { autoMigrateLegacyState, autoMigrateLegacyTaskStateSidecars } =
      await loadDoctorStateMigrations();
    const stateResult = snapshot.valid
      ? await autoMigrateLegacyState({
          cfg: baseConfig,
          env: process.env,
          recoverCorruptTargetStore: options.recoverCorruptTargetStore,
        })
      : await autoMigrateLegacyTaskStateSidecars({ env: process.env });
    noteStateMigrationResult(stateResult);
  }

  return {
    snapshot,
    baseConfig,
  };
}
