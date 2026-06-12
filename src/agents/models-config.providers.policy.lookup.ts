/**
 * Resolves provider plugin lookup keys from provider config aliases.
 */
import { MODEL_APIS } from "../config/types.models.js";
import type { ProviderConfig } from "./models-config.providers.secrets.js";

const GENERIC_PROVIDER_APIS = new Set<string>([
  "openai-completions",
  "openai-responses",
  "anthropic-messages",
]);

export function resolveProviderPluginLookupKey(
  providerKey: string,
  provider?: ProviderConfig,
): string {
  const api = provider?.api?.trim() ?? "";
  if (
    api &&
    MODEL_APIS.includes(api as (typeof MODEL_APIS)[number]) &&
    !GENERIC_PROVIDER_APIS.has(api)
  ) {
    return api;
  }
  return providerKey;
}
