// Openai plugin module implements default models behavior.
import { ensureModelAllowlistEntry } from "openclaw/plugin-sdk/provider-onboard";
import {
  applyAgentDefaultModelPrimary,
  type OpenClawConfig,
} from "openclaw/plugin-sdk/provider-onboard";

export const OPENAI_DEFAULT_MODEL = "openai/gpt-5.5";

export function applyOpenAIProviderConfig(cfg: OpenClawConfig): OpenClawConfig {
  const next = ensureModelAllowlistEntry({
    cfg,
    modelRef: OPENAI_DEFAULT_MODEL,
  });
  const models = { ...next.agents?.defaults?.models };
  models[OPENAI_DEFAULT_MODEL] = {
    ...models[OPENAI_DEFAULT_MODEL],
    alias: models[OPENAI_DEFAULT_MODEL]?.alias ?? "GPT",
  };

  return {
    ...next,
    agents: {
      ...next.agents,
      defaults: {
        ...next.agents?.defaults,
        models,
      },
    },
  };
}

export function applyOpenAIConfig(cfg: OpenClawConfig): OpenClawConfig {
  return applyAgentDefaultModelPrimary(applyOpenAIProviderConfig(cfg), OPENAI_DEFAULT_MODEL);
}
