/** Command for setting the default text model. */
import { logConfigUpdated } from "../../config/logging.js";
import { resolveAgentModelPrimaryValue } from "../../config/model-input.js";
import type { RuntimeEnv } from "../../runtime.js";
import { repairCodexRuntimePluginInstallForModelSelection } from "../codex-runtime-plugin-install.js";
import { applyDefaultModelPrimaryUpdate, updateConfig } from "./shared.js";

/** Sets agents.defaults.model.primary and repairs provider runtime plugin installs when needed. */
export async function modelsSetCommand(modelRaw: string, runtime: RuntimeEnv) {
  const updated = await updateConfig((cfg, context) => {
    return applyDefaultModelPrimaryUpdate({
      cfg,
      resolveCfg: context.runtimeConfig,
      modelRaw,
      field: "model",
    });
  });
  const selectedModel = resolveAgentModelPrimaryValue(updated.agents?.defaults?.model) ?? modelRaw;
  const repaired = await repairCodexRuntimePluginInstallForModelSelection({
    cfg: updated,
    model: selectedModel,
  });
  for (const warning of repaired.warnings) {
    runtime.error?.(warning);
  }

  logConfigUpdated(runtime);
  runtime.log(`Default model: ${selectedModel}`);
}
