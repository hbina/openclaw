// Provider runtime contract helpers define reusable runtime tests for provider plugins.
import { afterAll, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import type { ProviderRuntimeModel } from "../plugin-entry.js";
import { registerProviderPlugin, requireRegisteredProvider } from "../plugin-test-runtime.js";
import type { ProviderPlugin } from "../provider-model-shared.js";
import { createProviderUsageFetch, makeResponse } from "../test-env.js";

const CONTRACT_SETUP_TIMEOUT_MS = 300_000;

const OPENAI_CODEX_PROVIDER_RUNTIME_MODULE_ID =
  "../../../extensions/openai/openai-chatgpt-provider.runtime.js";
const refreshOpenAICodexTokenMock = vi.fn();

function installProviderRuntimeContractMocks() {
  vi.doMock(OPENAI_CODEX_PROVIDER_RUNTIME_MODULE_ID, () => ({
    refreshOpenAICodexToken: refreshOpenAICodexTokenMock,
  }));
}

function removeProviderRuntimeContractMocks() {
  vi.doUnmock(OPENAI_CODEX_PROVIDER_RUNTIME_MODULE_ID);
}

function createModel(overrides: Partial<ProviderRuntimeModel> & Pick<ProviderRuntimeModel, "id">) {
  return {
    id: overrides.id,
    name: overrides.name ?? overrides.id,
    api: overrides.api ?? "openai-responses",
    provider: overrides.provider ?? "demo",
    baseUrl: overrides.baseUrl ?? "https://api.example.com/v1",
    reasoning: overrides.reasoning ?? true,
    input: overrides.input ?? ["text"],
    cost: overrides.cost ?? { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
    contextWindow: overrides.contextWindow ?? 200_000,
    maxTokens: overrides.maxTokens ?? 8_192,
  } satisfies ProviderRuntimeModel;
}

function requireRecord(value: unknown, label: string): Record<string, unknown> {
  expect(value, label).toBeTypeOf("object");
  expect(value, label).not.toBeNull();
  return value as Record<string, unknown>;
}

function expectFields(value: unknown, fields: Record<string, unknown>) {
  const record = requireRecord(value, "record");
  for (const [key, expected] of Object.entries(fields)) {
    expect(record[key]).toEqual(expected);
  }
}

type ProviderRuntimeContractFixture = {
  providerIds: string[];
  pluginId: string;
  name: string;
  load: ProviderRuntimeContractPluginLoader;
};

export type ProviderRuntimeContractPluginLoader = () => Promise<{
  default: Parameters<typeof registerProviderPlugin>[0]["plugin"];
}>;

function installRuntimeHooks(fixtures: readonly ProviderRuntimeContractFixture[]) {
  const providers = new Map<string, ProviderPlugin>();
  let loadPromise: Promise<void> | null = null;

  function requireProviderContractProvider(providerId: string): ProviderPlugin {
    const provider = providers.get(providerId);
    if (!provider) {
      throw new Error(`provider runtime contract fixture missing for ${providerId}`);
    }
    return provider;
  }

  async function ensureProvidersLoaded() {
    if (!loadPromise) {
      loadPromise = (async () => {
        providers.clear();
        const registeredFixtures = await Promise.all(
          fixtures.map(async (fixture) => {
            const plugin = await fixture.load();
            return {
              fixture,
              providers: (
                await registerProviderPlugin({
                  plugin: plugin.default,
                  id: fixture.pluginId,
                  name: fixture.name,
                })
              ).providers,
            };
          }),
        );
        for (const { fixture, providers: registeredProviders } of registeredFixtures) {
          for (const providerId of fixture.providerIds) {
            providers.set(
              providerId,
              requireRegisteredProvider(registeredProviders, providerId, "provider"),
            );
          }
        }
      })();
    }

    await loadPromise;
  }

  beforeAll(async () => {
    installProviderRuntimeContractMocks();
    await ensureProvidersLoaded();
  }, CONTRACT_SETUP_TIMEOUT_MS);

  afterAll(() => {
    removeProviderRuntimeContractMocks();
  });

  beforeEach(() => {
    refreshOpenAICodexTokenMock.mockReset();
  }, CONTRACT_SETUP_TIMEOUT_MS);

  return requireProviderContractProvider;
}

export function describeAnthropicProviderRuntimeContract(
  load: ProviderRuntimeContractPluginLoader,
) {
  describe("anthropic provider runtime contract", { timeout: CONTRACT_SETUP_TIMEOUT_MS }, () => {
    const requireProviderContractProvider = installRuntimeHooks([
      { providerIds: ["anthropic"], pluginId: "anthropic", name: "Anthropic", load },
    ]);

    it("owns anthropic 4.6 forward-compat resolution", () => {
      const provider = requireProviderContractProvider("anthropic");
      const model = provider.resolveDynamicModel?.({
        provider: "anthropic",
        modelId: "claude-sonnet-4.6-20260219",
        modelRegistry: {
          find: (_provider: string, id: string) =>
            id === "claude-sonnet-4-6-20260219"
              ? createModel({
                  id,
                  api: "anthropic-messages",
                  provider: "anthropic",
                  baseUrl: "https://api.anthropic.com",
                })
              : null,
        } as never,
      });

      expectFields(model, {
        id: "claude-sonnet-4.6-20260219",
        provider: "anthropic",
        api: "anthropic-messages",
        baseUrl: "https://api.anthropic.com",
      });
    });

    it("owns usage auth resolution", async () => {
      const provider = requireProviderContractProvider("anthropic");
      await expect(
        provider.resolveUsageAuth?.({
          config: {} as never,
          env: {} as NodeJS.ProcessEnv,
          provider: "anthropic",
          resolveApiKeyFromConfigAndStore: () => undefined,
          resolveOAuthToken: async () => ({
            token: "anthropic-oauth-token",
          }),
        }),
      ).resolves.toEqual({
        token: "anthropic-oauth-token",
      });
    });

    it("owns auth doctor hint generation", () => {
      const provider = requireProviderContractProvider("anthropic");
      const hint = provider.buildAuthDoctorHint?.({
        provider: "anthropic",
        profileId: "anthropic:default",
        config: {
          auth: {
            profiles: {
              "anthropic:default": {
                provider: "anthropic",
                mode: "oauth",
              },
            },
          },
        } as never,
        store: {
          version: 1,
          profiles: {
            "anthropic:oauth-user@example.com": {
              type: "oauth",
              provider: "anthropic",
              access: "oauth-access",
              refresh: "oauth-refresh",
              expires: Date.now() + 60_000,
            },
          },
        },
      });

      expect(hint).toContain("suggested profile: anthropic:oauth-user@example.com");
      expect(hint).toContain("openclaw doctor --yes");
    });

    it("owns usage snapshot fetching", async () => {
      const provider = requireProviderContractProvider("anthropic");
      const mockFetch = createProviderUsageFetch(async (url) => {
        if (url.includes("api.anthropic.com/api/oauth/usage")) {
          return makeResponse(200, {
            five_hour: { utilization: 20, resets_at: "2026-01-07T01:00:00Z" },
            seven_day: { utilization: 35, resets_at: "2026-01-09T01:00:00Z" },
          });
        }
        return makeResponse(404, "not found");
      });

      await expect(
        provider.fetchUsageSnapshot?.({
          config: {} as never,
          env: {} as NodeJS.ProcessEnv,
          provider: "anthropic",
          token: "anthropic-oauth-token",
          timeoutMs: 5_000,
          fetchFn: mockFetch as unknown as typeof fetch,
        }),
      ).resolves.toEqual({
        provider: "anthropic",
        displayName: "Claude",
        windows: [
          { label: "5h", usedPercent: 20, resetAt: Date.parse("2026-01-07T01:00:00Z") },
          { label: "Week", usedPercent: 35, resetAt: Date.parse("2026-01-09T01:00:00Z") },
        ],
      });
    });
  });
}

export function describeOpenAIProviderRuntimeContract(load: ProviderRuntimeContractPluginLoader) {
  describe("openai provider runtime contract", { timeout: CONTRACT_SETUP_TIMEOUT_MS }, () => {
    const requireProviderContractProvider = installRuntimeHooks([
      { providerIds: ["openai", "openai"], pluginId: "openai", name: "OpenAI", load },
    ]);

    it("owns openai gpt-5.4 forward-compat resolution", () => {
      const provider = requireProviderContractProvider("openai");
      const model = provider.resolveDynamicModel?.({
        provider: "openai",
        modelId: "gpt-5.4-pro",
        modelRegistry: {
          find: (_provider: string, id: string) =>
            id === "gpt-5.2-pro"
              ? createModel({
                  id,
                  provider: "openai",
                  baseUrl: "https://api.openai.com/v1",
                  input: ["text", "image"],
                })
              : null,
        } as never,
      });

      expectFields(model, {
        id: "gpt-5.4-pro",
        provider: "openai",
        api: "openai-responses",
        baseUrl: "https://api.openai.com/v1",
        contextWindow: 1_050_000,
        maxTokens: 128_000,
      });
    });

    it("owns openai gpt-5.5 forward-compat resolution", () => {
      const provider = requireProviderContractProvider("openai");
      const model = provider.resolveDynamicModel?.({
        provider: "openai",
        modelId: "gpt-5.5",
        modelRegistry: {
          find: (_provider: string, id: string) =>
            id === "gpt-5.4"
              ? createModel({
                  id,
                  provider: "openai",
                  baseUrl: "https://api.openai.com/v1",
                  input: ["text", "image"],
                })
              : null,
        } as never,
      });

      expectFields(model, {
        id: "gpt-5.5",
        provider: "openai",
        api: "openai-responses",
        baseUrl: "https://api.openai.com/v1",
        contextWindow: 1_000_000,
        contextTokens: 272_000,
        maxTokens: 128_000,
        mediaInput: {
          image: { maxSidePx: 6000, preferredSidePx: 2048, tokenMode: "detail" },
        },
      });
    });

    it("owns openai gpt-5.4 mini forward-compat resolution", () => {
      const provider = requireProviderContractProvider("openai");
      const model = provider.resolveDynamicModel?.({
        provider: "openai",
        modelId: "gpt-5.4-mini",
        modelRegistry: {
          find: (_provider: string, id: string) =>
            id === "gpt-5-mini"
              ? createModel({
                  id,
                  provider: "openai",
                  api: "openai-responses",
                  baseUrl: "https://api.openai.com/v1",
                  input: ["text", "image"],
                  reasoning: true,
                  contextWindow: 400_000,
                  maxTokens: 128_000,
                })
              : null,
        } as never,
      });

      expectFields(model, {
        id: "gpt-5.4-mini",
        provider: "openai",
        api: "openai-responses",
        baseUrl: "https://api.openai.com/v1",
        contextWindow: 400_000,
        maxTokens: 128_000,
      });
    });

    it("owns direct openai transport normalization", () => {
      const provider = requireProviderContractProvider("openai");
      expectFields(
        provider.normalizeResolvedModel?.({
          provider: "openai",
          modelId: "gpt-5.4",
          model: createModel({
            id: "gpt-5.4",
            provider: "openai",
            api: "openai-completions",
            baseUrl: "https://api.openai.com/v1",
            input: ["text", "image"],
            contextWindow: 1_050_000,
            maxTokens: 128_000,
          }),
        }),
        {
          api: "openai-responses",
        },
      );
    });

    it("owns refresh fallback for accountId extraction failures", async () => {
      const provider = requireProviderContractProvider("openai");
      const credential = {
        type: "oauth" as const,
        provider: "openai",
        access: "cached-access-token",
        refresh: "refresh-token",
        expires: Date.now() - 60_000,
      };

      refreshOpenAICodexTokenMock.mockRejectedValueOnce(
        new Error("Failed to extract accountId from token"),
      );

      await expect(provider.refreshOAuth?.(credential)).resolves.toEqual(credential);
    });

    it("owns forward-compat codex models", () => {
      const provider = requireProviderContractProvider("openai");
      const model = provider.resolveDynamicModel?.({
        provider: "openai",
        modelId: "gpt-5.4",
        authProfileMode: "oauth",
        modelRegistry: {
          find: (_provider: string, id: string) =>
            id === "gpt-5.2-codex"
              ? createModel({
                  id,
                  api: "openai-chatgpt-responses",
                  provider: "openai",
                  baseUrl: "https://chatgpt.com/backend-api",
                })
              : null,
        } as never,
      });

      expectFields(model, {
        id: "gpt-5.4",
        provider: "openai",
        api: "openai-chatgpt-responses",
        contextWindow: 1_050_000,
        maxTokens: 128_000,
      });
    });

    it("keeps OpenClaw cost metadata but applies Codex context metadata for gpt-5.5 models", () => {
      const provider = requireProviderContractProvider("openai");
      const model = provider.resolveDynamicModel?.({
        provider: "openai",
        modelId: "gpt-5.5",
        authProfileMode: "oauth",
        modelRegistry: {
          find: (_provider: string, id: string) =>
            id === "gpt-5.5"
              ? createModel({
                  id,
                  api: "openai-chatgpt-responses",
                  provider: "openai",
                  baseUrl: "https://chatgpt.com/backend-api",
                  input: ["text", "image"],
                  cost: { input: 5, output: 30, cacheRead: 0.5, cacheWrite: 0 },
                  contextWindow: 272_000,
                  maxTokens: 128_000,
                })
              : null,
        } as never,
      });

      expectFields(model, {
        id: "gpt-5.5",
        provider: "openai",
        api: "openai-chatgpt-responses",
        contextWindow: 400_000,
        contextTokens: 272_000,
        maxTokens: 128_000,
      });
    });

    it("claims codex mini models through the Codex OAuth route", () => {
      const provider = requireProviderContractProvider("openai");
      const model = provider.resolveDynamicModel?.({
        provider: "openai",
        modelId: "gpt-5.4-mini",
        authProfileMode: "oauth",
        modelRegistry: {
          find: (_provider: string, id: string) =>
            id === "gpt-5.4"
              ? createModel({
                  id,
                  api: "openai-chatgpt-responses",
                  provider: "openai",
                  baseUrl: "https://chatgpt.com/backend-api",
                  cost: { input: 5, output: 30, cacheRead: 0.5, cacheWrite: 0 },
                  contextWindow: 272_000,
                  maxTokens: 128_000,
                })
              : null,
        } as never,
      });

      expectFields(model, {
        id: "gpt-5.4-mini",
        provider: "openai",
        api: "openai-chatgpt-responses",
        contextWindow: 400_000,
        contextTokens: 272_000,
        maxTokens: 128_000,
        cost: { input: 0.75, output: 4.5, cacheRead: 0.075, cacheWrite: 0 },
      });
    });

    it("owns codex transport defaults", () => {
      const provider = requireProviderContractProvider("openai");
      expect(
        provider.prepareExtraParams?.({
          provider: "openai",
          modelId: "gpt-5.4",
          model: createModel({
            id: "gpt-5.4",
            provider: "openai",
            api: "openai-chatgpt-responses",
            baseUrl: "https://chatgpt.com/backend-api/codex",
          }),
          extraParams: { temperature: 0.2 },
        }),
      ).toEqual({
        temperature: 0.2,
        transport: "auto",
      });
    });

    it("owns usage snapshot fetching", async () => {
      const provider = requireProviderContractProvider("openai");
      const mockFetch = createProviderUsageFetch(async (url) => {
        if (url.includes("chatgpt.com/backend-api/wham/usage")) {
          return makeResponse(200, {
            rate_limit: {
              primary_window: {
                used_percent: 12,
                limit_window_seconds: 10800,
                reset_at: 1_705_000,
              },
            },
            plan_type: "Plus",
          });
        }
        return makeResponse(404, "not found");
      });

      await expect(
        provider.fetchUsageSnapshot?.({
          config: {} as never,
          env: {} as NodeJS.ProcessEnv,
          provider: "openai",
          token: "codex-token",
          accountId: "acc-1",
          timeoutMs: 5_000,
          fetchFn: mockFetch as unknown as typeof fetch,
        }),
      ).resolves.toEqual({
        provider: "openai",
        displayName: "OpenAI",
        windows: [{ label: "3h", usedPercent: 12, resetAt: 1_705_000_000 }],
        plan: "Plus",
      });
    });
  });
}
