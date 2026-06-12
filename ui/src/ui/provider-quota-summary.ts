// Control UI module implements provider quota summary behavior.
import { asDateTimestampMs } from "@openclaw/normalization-core/number-coercion";
import type { ModelAuthStatusProvider, ModelAuthStatusResult } from "./types.ts";

export type QuotaPeriodSummary = {
  displayName: string;
  label: string;
  remaining: number;
  resetAt?: number;
};

export function formatQuotaReset(resetAt?: number): string | null {
  const timestampMs = asDateTimestampMs(resetAt);
  if (timestampMs === undefined) {
    return null;
  }
  const diffMs = timestampMs - Date.now();
  if (diffMs <= 0) {
    return "now";
  }
  const minutes = Math.floor(diffMs / 60_000);
  if (minutes < 60) {
    return `${minutes}m`;
  }
  const hours = Math.floor(minutes / 60);
  const remainingMinutes = minutes % 60;
  if (hours < 24) {
    return remainingMinutes > 0 ? `${hours}h ${remainingMinutes}m` : `${hours}h`;
  }
  const days = Math.floor(hours / 24);
  if (days < 7) {
    const remainingHours = hours % 24;
    return remainingHours > 0 ? `${days}d ${remainingHours}h` : `${days}d`;
  }
  return new Date(timestampMs).toLocaleDateString(undefined, { month: "short", day: "numeric" });
}

export function collectQuotaPeriods(
  providers: ReadonlyArray<ModelAuthStatusProvider>,
): QuotaPeriodSummary[] {
  return providers
    .flatMap((provider) =>
      (provider.usage?.windows ?? []).map((window) => ({
        displayName: provider.displayName,
        label: (window.label || "").trim(),
        remaining: Math.max(0, Math.min(100, Math.round(100 - window.usedPercent))),
        resetAt: window.resetAt,
      })),
    )
    .toSorted((a, b) => a.remaining - b.remaining || a.displayName.localeCompare(b.displayName));
}

export function collectQuotaPeriodsFromAuthStatus(
  status: ModelAuthStatusResult | null,
  filter: (provider: ModelAuthStatusProvider) => boolean,
): QuotaPeriodSummary[] {
  return collectQuotaPeriods((status?.providers ?? []).filter(filter));
}
