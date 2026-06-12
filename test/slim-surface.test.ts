import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";

const repoRoot = process.cwd();

function readText(path: string): string {
  return readFileSync(join(repoRoot, path), "utf8");
}

describe("slim fork surface", () => {
  it("keeps only the retained bundled plugins", () => {
    const retained = readdirSync(join(repoRoot, "extensions"), { withFileTypes: true })
      .filter((entry) => entry.isDirectory())
      .map((entry) => entry.name)
      .toSorted();

    expect(retained).toEqual([
      "anthropic",
      "discord",
      "memory-core",
      "openai",
      "telegram",
      "whatsapp",
    ]);
  });

  it("does not publish removed package entry points or scripts", () => {
    const pkg = JSON.parse(readText("package.json")) as {
      exports: Record<string, unknown>;
      scripts: Record<string, string>;
    };
    const exportText = Object.keys(pkg.exports).join("\n");
    const scriptText = Object.entries(pkg.scripts)
      .map(([name, command]) => `${name}: ${command}`)
      .join("\n");

    expect(exportText).not.toMatch(/matrix|provider-onboard|posix-spawn/);
    expect(scriptText).not.toMatch(
      /(^|[^a-z])(android|ios|gemini|google-gemini|matrix|npm-onboard|release-typed-onboarding|swift|posix)([^a-z]|$)/i,
    );
  });

  it("keeps the Docker image focused on gateway, UI, and retained plugins", () => {
    const dockerfile = readText("Dockerfile");

    expect(dockerfile).not.toContain("matrix-sdk-crypto");
    expect(dockerfile).not.toContain("canvas:a2ui:bundle");
    expect(dockerfile).not.toContain("qa:lab:build");
    expect(dockerfile).not.toContain("/app/qa ./qa");
  });

  it("documents Docker and file-based setup as the primary operator flow", () => {
    const readme = readText("README.md");
    const dockerDoc = readText("docs/install/docker.md");

    expect(readme).toContain("Preferred setup: run the Docker image");
    expect(readme).not.toContain("Preferred setup: run `openclaw onboard`");
    expect(dockerDoc).toContain("The slim fork is Docker-first");
    expect(dockerDoc).toContain("`openclaw.json`");
    expect(dockerDoc).toContain("`/home/node/.openclaw/.env`");
  });
});
