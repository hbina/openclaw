// Openai tests cover the slim Chat Completions provider.
import { describe, expect, it } from "vitest";
import { buildOpenAICodexProviderPlugin, buildOpenAIProvider } from "./openai-provider.js";

describe("buildOpenAIProvider", () => {
  it("exposes only API-key auth for the OpenAI provider", () => {
    const provider = buildOpenAIProvider();

    expect(provider.id).toBe("openai");
    expect(provider.envVars).toEqual(["OPENAI_API_KEY"]);
    expect(provider.auth.map((method) => method.id)).toEqual(["api-key"]);
    expect(provider.hookAliases).toBeUndefined();
    expect(provider.fetchUsageSnapshot).toBeUndefined();
    expect(provider.refreshOAuth).toBeUndefined();
  });

  it("resolves custom base URL models through Chat Completions", () => {
    const provider = buildOpenAIProvider();

    expect(
      provider.resolveDynamicModel?.({
        provider: "openai",
        modelId: "llama-3.3-70b",
        providerConfig: {
          api: "openai-completions",
          baseUrl: "https://llm.example.test/v1",
        },
      } as never),
    ).toMatchObject({
      id: "llama-3.3-70b",
      name: "llama-3.3-70b",
      provider: "openai",
      api: "openai-completions",
      baseUrl: "https://llm.example.test/v1",
      input: ["text"],
    });
  });

  it("normalizes retained OpenAI models to Chat Completions", () => {
    const provider = buildOpenAIProvider();

    expect(
      provider.normalizeTransport?.({
        provider: "openai",
        api: "openai-responses",
        baseUrl: "https://api.openai.com/v1",
      } as never),
    ).toEqual({
      api: "openai-completions",
      baseUrl: "https://api.openai.com/v1",
    });
  });

  it("keeps the deprecated Codex builder as an OpenAI provider alias only", () => {
    const provider = buildOpenAICodexProviderPlugin();

    expect(provider.id).toBe("openai");
    expect(provider.auth.map((method) => method.id)).toEqual(["api-key"]);
    expect(provider.refreshOAuth).toBeUndefined();
  });
});
