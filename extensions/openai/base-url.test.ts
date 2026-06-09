// Openai tests cover slim base URL behavior.
import { describe, expect, it } from "vitest";
import {
  isOpenAIApiBaseUrl,
  OPENAI_API_BASE_URL,
  resolveOpenAIDefaultBaseUrl,
} from "./base-url.js";

describe("openai base URL helpers", () => {
  it("recognizes direct OpenAI API routes", () => {
    expect(isOpenAIApiBaseUrl("https://api.openai.com")).toBe(true);
    expect(isOpenAIApiBaseUrl("https://api.openai.com/v1")).toBe(true);
    expect(isOpenAIApiBaseUrl("https://api.openai.com/v1/")).toBe(true);
  });

  it("rejects proxy or unrelated API routes", () => {
    expect(isOpenAIApiBaseUrl("https://proxy.example.com/v1")).toBe(false);
    expect(isOpenAIApiBaseUrl("https://chatgpt.com/backend-api")).toBe(false);
    expect(isOpenAIApiBaseUrl(undefined)).toBe(false);
  });

  it("resolves default API base URL from OPENAI_BASE_URL", () => {
    expect(resolveOpenAIDefaultBaseUrl({})).toBe(OPENAI_API_BASE_URL);
    expect(resolveOpenAIDefaultBaseUrl({ OPENAI_BASE_URL: "   " })).toBe(OPENAI_API_BASE_URL);
    expect(resolveOpenAIDefaultBaseUrl({ OPENAI_BASE_URL: " https://proxy.example/v1 " })).toBe(
      "https://proxy.example/v1",
    );
  });
});
