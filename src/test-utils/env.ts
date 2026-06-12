// Test helpers for environment variable setup and restoration.
import path from "node:path";

/** Captures selected process.env keys so tests can restore exact prior state. */
export function captureEnv(keys: string[]) {
  const snapshot = new Map<string, string | undefined>();
  for (const key of keys) {
    snapshot.set(key, process.env[key]);
  }

  return {
    restore() {
      for (const [key, value] of snapshot) {
        if (value === undefined) {
          delete process.env[key];
        } else {
          process.env[key] = value;
        }
      }
    },
  };
}

function applyEnvValues(env: Record<string, string | undefined>): void {
  for (const [key, value] of Object.entries(env)) {
    if (value === undefined) {
      delete process.env[key];
    } else {
      process.env[key] = value;
    }
  }
}

const PATH_RESOLUTION_ENV_KEYS = [
  "HOME",
  "OPENCLAW_HOME",
  "OPENCLAW_STATE_DIR",
  "OPENCLAW_BUNDLED_PLUGINS_DIR",
  "OPENCLAW_DISABLE_BUNDLED_PLUGINS",
] as const;

export function createPathResolutionEnv(
  homeDir: string,
  env: Record<string, string | undefined> = {},
): NodeJS.ProcessEnv {
  const resolvedHome = path.resolve(homeDir);
  const nextEnv: NodeJS.ProcessEnv = {
    ...process.env,
    HOME: resolvedHome,
    OPENCLAW_HOME: undefined,
    OPENCLAW_STATE_DIR: undefined,
    OPENCLAW_BUNDLED_PLUGINS_DIR: undefined,
    OPENCLAW_DISABLE_BUNDLED_PLUGINS: undefined,
  };

  for (const [key, value] of Object.entries(env)) {
    nextEnv[key] = value;
  }

  return nextEnv;
}

export function withPathResolutionEnv<T>(
  homeDir: string,
  env: Record<string, string | undefined>,
  fn: (resolvedEnv: NodeJS.ProcessEnv) => T,
): T {
  const resolvedEnv = createPathResolutionEnv(homeDir, env);
  const scopedEnv: Record<string, string | undefined> = {};
  for (const key of new Set([...PATH_RESOLUTION_ENV_KEYS, ...Object.keys(env)])) {
    scopedEnv[key] = resolvedEnv[key];
  }
  return withEnv(scopedEnv, () => fn(resolvedEnv));
}

export function captureFullEnv() {
  const snapshot: Record<string, string | undefined> = { ...process.env };

  return {
    restore() {
      for (const key of Object.keys(process.env)) {
        if (!(key in snapshot)) {
          delete process.env[key];
        }
      }
      for (const [key, value] of Object.entries(snapshot)) {
        if (value === undefined) {
          delete process.env[key];
        } else {
          process.env[key] = value;
        }
      }
    },
  };
}

export function withEnv<T>(env: Record<string, string | undefined>, fn: () => T): T {
  const snapshot = captureEnv(Object.keys(env));
  try {
    applyEnvValues(env);
    return fn();
  } finally {
    snapshot.restore();
  }
}

export async function withEnvAsync<T>(
  env: Record<string, string | undefined>,
  fn: () => Promise<T>,
): Promise<T> {
  const snapshot = captureEnv(Object.keys(env));
  try {
    applyEnvValues(env);
    return await fn();
  } finally {
    snapshot.restore();
  }
}
