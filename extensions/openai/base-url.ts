// Openai plugin module implements base url behavior.
import { normalizeOptionalString } from "openclaw/plugin-sdk/string-coerce-runtime";

export const OPENAI_API_BASE_URL = "https://api.openai.com/v1";

export function resolveOpenAIDefaultBaseUrl(
  env: Record<string, string | undefined> = process.env,
): string {
  return normalizeOptionalString(env.OPENAI_BASE_URL) ?? OPENAI_API_BASE_URL;
}

export function isOpenAIApiBaseUrl(baseUrl?: string): boolean {
  const trimmed = normalizeOptionalString(baseUrl);
  if (!trimmed) {
    return false;
  }
  return /^https?:\/\/api\.openai\.com(?:\/v1)?\/?$/i.test(trimmed);
}
