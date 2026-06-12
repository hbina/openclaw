// Provides fixture helpers for config backup rotation tests.
import path from "node:path";
import { expect } from "vitest";

export function resolveConfigPathFromTempState(fileName = "openclaw.json"): string {
  const stateDir = process.env.OPENCLAW_STATE_DIR?.trim();
  if (!stateDir) {
    throw new Error("Expected OPENCLAW_STATE_DIR to be set by withTempHome");
  }
  return path.join(stateDir, fileName);
}

export function expectPosixMode(statMode: number, expectedMode: number): void {
  expect(statMode & 0o777).toBe(expectedMode);
}
