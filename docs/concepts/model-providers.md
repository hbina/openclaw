---
summary: "Model provider overview with example configs + CLI flows"
read_when:
  - You need a provider-by-provider model setup reference
  - You want example configs or CLI onboarding commands for model providers
title: "Model providers"
sidebarTitle: "Model providers"
---

Reference for **LLM/model providers** (not chat channels like WhatsApp/Telegram). For model selection rules, see [Models](/concepts/models).

## Quick rules

<AccordionGroup>
  <Accordion title="Model refs and CLI helpers">
    - Model refs use `provider/model` (example: `anthropic/claude-opus-4-6`).
    - `agents.defaults.models` acts as an allowlist when set.
    - CLI helpers: `openclaw onboard`, `openclaw models list`, `openclaw models set <provider/model>`.
    - `models.providers.*.contextWindow` / `contextTokens` / `maxTokens` set provider-level defaults; `models.providers.*.models[].contextWindow` / `contextTokens` / `maxTokens` override them per model.
    - Fallback rules, cooldown probes, and session-override persistence: [Model failover](/concepts/model-failover).

  </Accordion>
  <Accordion title="Adding provider auth does not change your primary model">
    `openclaw configure` preserves an existing `agents.defaults.model.primary` when you add or reauth a provider. `openclaw models auth login` does the same unless you pass `--set-default`. Provider plugins may still return a recommended default model in their auth config patch, but OpenClaw treats that as "make this model available" when a primary model already exists, not "replace the current primary model."

    To intentionally switch the default model, use `openclaw models set <provider/model>` or `openclaw models auth login --provider <id> --set-default`.

  </Accordion>
  <Accordion title="OpenAI provider/runtime split">
    OpenAI-family routes are prefix-specific:

    - `openai/<model>` uses the native Codex app-server harness for agent turns by default. This is the usual ChatGPT/Codex subscription setup.
    - legacy Codex model refs are legacy config that doctor rewrites to `openai/<model>`.
    - `openai/<model>` plus provider/model `agentRuntime.id: "openclaw"` uses OpenClaw's built-in runtime for explicit API-key or compatibility routes.

    See [OpenAI](/providers/openai) and [Codex harness](/plugins/codex-harness). If the provider/runtime split is confusing, read [Agent runtimes](/concepts/agent-runtimes) first.

    Plugin auto-enable follows the same boundary: `openai/*` agent refs enable the Codex plugin for the default route, and explicit provider/model `agentRuntime.id: "codex"` or legacy `codex/<model>` refs also require it.

    GPT-5.5 is available through the native Codex app-server harness by default on `openai/gpt-5.5`, and through the OpenClaw runtime when provider/model runtime policy explicitly selects `openclaw`.

  </Accordion>
  <Accordion title="CLI runtimes">
    CLI runtimes use the same split: choose a canonical model ref such as `anthropic/claude-*`, then set provider/model runtime policy to `claude-cli` when you want the local Claude CLI backend.

    Legacy `claude-cli/*` refs migrate back to canonical provider refs with the runtime recorded separately. Legacy `codex-cli/*` refs migrate to `openai/*` and use the Codex app-server route; OpenClaw no longer keeps a bundled Codex CLI backend.

  </Accordion>
</AccordionGroup>

## Plugin-owned provider behavior

Most provider-specific logic lives in provider plugins (`registerProvider(...)`) while OpenClaw keeps the generic inference loop. Plugins own onboarding, model catalogs, auth env-var mapping, transport/config normalization, tool-schema cleanup, failover classification, OAuth refresh, usage reporting, thinking/reasoning profiles, and more.

The full list of provider-SDK hooks and bundled-plugin examples lives in [Provider plugins](/plugins/sdk-provider-plugins). A provider that needs a totally custom request executor is a separate, deeper extension surface.

<Note>
Provider-owned runner behavior lives on explicit provider hooks such as replay policy, tool-schema normalization, stream wrapping, and transport/request helpers. The legacy `ProviderPlugin.capabilities` static bag is compatibility-only and is no longer read by shared runner logic.
</Note>

## API key rotation

<AccordionGroup>
  <Accordion title="Key sources and priority">
    Configure multiple keys via:

    - `OPENCLAW_LIVE_<PROVIDER>_KEY` (single live override, highest priority)
    - `<PROVIDER>_API_KEYS` (comma or semicolon list)
    - `<PROVIDER>_API_KEY` (primary key)
    - `<PROVIDER>_API_KEY_*` (numbered list, e.g. `<PROVIDER>_API_KEY_1`)

    For Google providers, `GOOGLE_API_KEY` is also included as fallback. Key selection order preserves priority and deduplicates values.

  </Accordion>
  <Accordion title="When rotation kicks in">
    - Requests are retried with the next key only on rate-limit responses (for example `429`, `rate_limit`, `quota`, `resource exhausted`, `Too many concurrent requests`, `ThrottlingException`, `concurrency limit reached`, `workers_ai ... quota limit exceeded`, or periodic usage-limit messages).
    - Non-rate-limit failures fail immediately; no key rotation is attempted.
    - When all candidate keys fail, the final error is returned from the last attempt.

  </Accordion>
</AccordionGroup>

## Official provider plugins

Official provider plugins publish their own model catalog rows. These providers require **no** `models.providers` model entries; enable the provider plugin, set auth, and pick a model. Use `models.providers` only for explicit custom providers or narrow request settings such as timeouts.

### OpenAI

- Provider: `openai`
- Auth: `OPENAI_API_KEY`
- Optional rotation: `OPENAI_API_KEYS`, `OPENAI_API_KEY_1`, `OPENAI_API_KEY_2`, plus `OPENCLAW_LIVE_OPENAI_KEY` (single override)
- Example models: `openai/gpt-5.5`, `openai/gpt-5.4-mini`
- Verify account/model availability with `openclaw models list --provider openai` if a specific install or API key behaves differently.
- CLI: `openclaw onboard --auth-choice openai-api-key`
- Default transport is `auto`; OpenClaw passes the transport choice to the shared model runtime.
- Override per model via `agents.defaults.models["openai/<model>"].params.transport` (`"sse"`, `"websocket"`, or `"auto"`)
- OpenAI priority processing can be enabled via `agents.defaults.models["openai/<model>"].params.serviceTier`
- `/fast` and `params.fastMode` map direct `openai/*` Responses requests to `service_tier=priority` on `api.openai.com`
- Use `params.serviceTier` when you want an explicit tier instead of the shared `/fast` toggle
- Hidden OpenClaw attribution headers (`originator`, `version`, `User-Agent`) apply only on native OpenAI traffic to `api.openai.com`, not generic OpenAI-compatible proxies
- Native OpenAI routes also keep Responses `store`, prompt-cache hints, and OpenAI reasoning-compat payload shaping; proxy routes do not
- `openai/gpt-5.3-codex-spark` is intentionally suppressed in OpenClaw because live OpenAI API requests reject it and the current Codex catalog does not expose it

```json5
{
  agents: { defaults: { model: { primary: "openai/gpt-5.5" } } },
}
```

### Anthropic

- Provider: `anthropic`
- Auth: `ANTHROPIC_API_KEY`
- Optional rotation: `ANTHROPIC_API_KEYS`, `ANTHROPIC_API_KEY_1`, `ANTHROPIC_API_KEY_2`, plus `OPENCLAW_LIVE_ANTHROPIC_KEY` (single override)
- Example model: `anthropic/claude-opus-4-6`
- CLI: `openclaw onboard --auth-choice apiKey`
- Direct public Anthropic requests support the shared `/fast` toggle and `params.fastMode`, including API-key and OAuth-authenticated traffic sent to `api.anthropic.com`; OpenClaw maps that to Anthropic `service_tier` (`auto` vs `standard_only`)
- Preferred Claude CLI config keeps the model ref canonical and selects the CLI
  backend separately: `anthropic/claude-opus-4-8` with
  model-scoped `agentRuntime.id: "claude-cli"`. Legacy
  `claude-cli/claude-opus-4-7` refs still work for compatibility.

<Note>
Anthropic staff told us OpenClaw-style Claude CLI usage is allowed again, so OpenClaw treats Claude CLI reuse and `claude -p` usage as sanctioned for this integration unless Anthropic publishes a new policy. Anthropic setup-token remains available as a supported OpenClaw token path, but OpenClaw now prefers Claude CLI reuse and `claude -p` when available.
</Note>

```json5
{
  agents: { defaults: { model: { primary: "anthropic/claude-opus-4-6" } } },
}
```

### OpenAI ChatGPT/Codex OAuth

- Provider: `openai`
- Auth: OAuth (ChatGPT)
- Legacy OpenAI Codex model ref: `openai/gpt-5.5`
- Native Codex app-server harness ref: `openai/gpt-5.5`
- Native Codex app-server harness docs: [Codex harness](/plugins/codex-harness)
- Legacy model refs: `codex/gpt-*`
- Plugin boundary: `openai/*` loads the OpenAI plugin; the native Codex app-server plugin is selected by the Codex harness runtime.
- CLI: `openclaw onboard --auth-choice openai` or `openclaw models auth login --provider openai`
- Default transport is `auto` (WebSocket-first, SSE fallback)
- Override per OpenAI Codex model via `agents.defaults.models["openai/<model>"].params.transport` (`"sse"`, `"websocket"`, or `"auto"`)
- `params.serviceTier` is also forwarded on native Codex Responses requests (`chatgpt.com/backend-api`)
- Hidden OpenClaw attribution headers (`originator`, `version`, `User-Agent`) are only attached on native Codex traffic to `chatgpt.com/backend-api`, not generic OpenAI-compatible proxies
- Shares the same `/fast` toggle and `params.fastMode` config as direct `openai/*`; OpenClaw maps that to `service_tier=priority`
- `openai/gpt-5.5` uses the Codex catalog native `contextWindow = 400000` and default runtime `contextTokens = 272000`; override the runtime cap with `models.providers.openai.models[].contextTokens`
- Policy note: OpenAI Codex OAuth is explicitly supported for external tools/workflows like OpenClaw.
- For the common subscription plus native Codex runtime route, sign in with `openai` auth and configure `openai/gpt-5.5`; OpenAI agent turns select Codex by default.
- Use provider/model `agentRuntime.id: "openclaw"` only when you want the built-in OpenClaw route; otherwise keep `openai/gpt-5.5` on the default Codex harness.
- legacy Codex GPT refs are legacy state, not a live provider route. Use `openai/gpt-5.5` on the native Codex runtime for new agent config, and run `openclaw doctor --fix` to migrate old legacy Codex model refs to canonical `openai/*` refs.

```json5
{
  plugins: { entries: { codex: { enabled: true } } },
  agents: {
    defaults: {
      model: { primary: "openai/gpt-5.5" },
    },
  },
}
```

```json5
{
  models: {
    providers: {
      openai: {
        models: [{ id: "gpt-5.5", contextTokens: 160000 }],
      },
    },
  },
}
```

## Providers via `models.providers` (custom/base URL)

Use `models.providers` (or `models.json`) to add **custom** providers or OpenAI/Anthropic-compatible proxies.

Many of the bundled provider plugins below already publish a default catalog. Use explicit `models.providers.<id>` entries only when you want to override the default base URL, headers, or model list.

Gateway model capability checks also read explicit `models.providers.<id>.models[]` metadata. If a custom or proxy model accepts images, set `input: ["text", "image"]` on that model so WebChat and node-origin attachment paths pass images as native model inputs instead of text-only media refs.

`agents.defaults.models["provider/model"]` only controls model visibility, aliases, and per-model metadata for agents. It does not register a new runtime model by itself. For custom provider models, also add `models.providers.<provider>.models[]` with at least the matching `id`.

### LM Studio

LM Studio ships as a bundled provider plugin which uses the native API:

- Provider: `lmstudio`
- Auth: `LM_API_TOKEN`
- Default inference base URL: `http://localhost:1234/v1`

Then set a model (replace with one of the IDs returned by `http://localhost:1234/api/v1/models`):

```json5
{
  agents: {
    defaults: { model: { primary: "lmstudio/openai/gpt-oss-20b" } },
  },
}
```

OpenClaw uses LM Studio's native `/api/v1/models` and `/api/v1/models/load` for discovery + auto-load, with `/v1/chat/completions` for inference by default. If you want LM Studio JIT loading, TTL, and auto-evict to own model lifecycle, set `models.providers.lmstudio.params.preload: false`. See [/providers/lmstudio](/providers/lmstudio) for setup and troubleshooting.

### Ollama

Ollama ships as a bundled provider plugin and uses Ollama's native API:

- Provider: `ollama`
- Auth: None required (local server)
- Example model: `ollama/llama3.3`
- Installation: [https://ollama.com/download](https://ollama.com/download)

```bash
# Install Ollama, then pull a model:
ollama pull llama3.3
```

```json5
{
  agents: {
    defaults: { model: { primary: "ollama/llama3.3" } },
  },
}
```

Ollama is detected locally at `http://127.0.0.1:11434` when you opt in with `OLLAMA_API_KEY`, and the bundled provider plugin adds Ollama directly to `openclaw onboard` and the model picker. See [/providers/ollama](/providers/ollama) for onboarding, cloud/local mode, and custom configuration.

### vLLM

vLLM ships as a bundled provider plugin for local/self-hosted OpenAI-compatible servers:

- Provider: `vllm`
- Auth: Optional (depends on your server)
- Default base URL: `http://127.0.0.1:8000/v1`

To opt in to auto-discovery locally (any value works if your server doesn't enforce auth):

```bash
export VLLM_API_KEY="vllm-local"
```

Then set a model (replace with one of the IDs returned by `/v1/models`):

```json5
{
  agents: {
    defaults: { model: { primary: "vllm/your-model-id" } },
  },
}
```

See [/providers/vllm](/providers/vllm) for details.

### SGLang

SGLang ships as a bundled provider plugin for fast self-hosted OpenAI-compatible servers:

- Provider: `sglang`
- Auth: Optional (depends on your server)
- Default base URL: `http://127.0.0.1:30000/v1`

To opt in to auto-discovery locally (any value works if your server does not enforce auth):

```bash
export SGLANG_API_KEY="sglang-local"
```

Then set a model (replace with one of the IDs returned by `/v1/models`):

```json5
{
  agents: {
    defaults: { model: { primary: "sglang/your-model-id" } },
  },
}
```

See [/providers/sglang](/providers/sglang) for details.

### Local proxies (LM Studio, vLLM, LiteLLM, etc.)

Example (OpenAI-compatible):

```json5
{
  agents: {
    defaults: {
      model: { primary: "lmstudio/my-local-model" },
      models: { "lmstudio/my-local-model": { alias: "Local" } },
    },
  },
  models: {
    providers: {
      lmstudio: {
        baseUrl: "http://localhost:1234/v1",
        apiKey: "${LM_API_TOKEN}",
        api: "openai-completions",
        timeoutSeconds: 300,
        models: [
          {
            id: "my-local-model",
            name: "Local Model",
            reasoning: false,
            input: ["text"],
            cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
            contextWindow: 200000,
            maxTokens: 8192,
          },
        ],
      },
    },
  },
}
```

<AccordionGroup>
  <Accordion title="Default optional fields">
    For custom providers, `reasoning`, `input`, `cost`, `contextWindow`, and `maxTokens` are optional. When omitted, OpenClaw defaults to:

    - `reasoning: false`
    - `input: ["text"]`
    - `cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 }`
    - `contextWindow: 200000`
    - `maxTokens: 8192`

    Recommended: set explicit values that match your proxy/model limits.

  </Accordion>
  <Accordion title="Proxy-route shaping rules">
    - For `api: "openai-completions"` on non-native endpoints (any non-empty `baseUrl` whose host is not `api.openai.com`), OpenClaw forces `compat.supportsDeveloperRole: false` to avoid provider 400 errors for unsupported `developer` roles.
    - Proxy-style OpenAI-compatible routes also skip native OpenAI-only request shaping: no `service_tier`, no Responses `store`, no Completions `store`, no prompt-cache hints, no OpenAI reasoning-compat payload shaping, and no hidden OpenClaw attribution headers.
    - For OpenAI-compatible Completions proxies that need vendor-specific fields, set `agents.defaults.models["provider/model"].params.extra_body` (or `extraBody`) to merge extra JSON into the outbound request body.
    - For vLLM chat-template controls, set `agents.defaults.models["provider/model"].params.chat_template_kwargs`. The bundled vLLM plugin automatically sends `enable_thinking: false` and `force_nonempty_content: true` for `vllm/nemotron-3-*` when the session thinking level is off.
    - For slow local models or remote LAN/tailnet hosts, set `models.providers.<id>.timeoutSeconds`. This extends provider model HTTP request handling, including connect, headers, body streaming, and the total guarded-fetch abort, without increasing the whole agent runtime timeout. If `agents.defaults.timeoutSeconds` or a run-specific timeout is lower, raise that ceiling too; provider timeouts cannot extend the whole run.
    - Model provider HTTP calls allow Surge, Clash, and sing-box fake-IP DNS answers in `198.18.0.0/15` and `fc00::/7` only for the configured provider `baseUrl` hostname. Custom/local provider endpoints also trust that exact configured `scheme://host:port` origin for guarded model requests, including loopback, LAN, and tailnet hosts. This is not a new config option; the `baseUrl` you configure extends the request policy only for that origin. Fake-IP hostname allowance and exact-origin trust are independent mechanisms. Other private, loopback, link-local, metadata destinations, and different ports still require an explicit `models.providers.<id>.request.allowPrivateNetwork: true` opt-in. Set `models.providers.<id>.request.allowPrivateNetwork: false` to opt out of the exact-origin trust.
    - If `baseUrl` is empty/omitted, OpenClaw keeps the default OpenAI behavior (which resolves to `api.openai.com`).
    - For safety, an explicit `compat.supportsDeveloperRole: true` is still overridden on non-native `openai-completions` endpoints.
    - For `api: "anthropic-messages"` on non-direct endpoints (any provider other than canonical `anthropic`, or a custom `models.providers.anthropic.baseUrl` whose host is not a public `api.anthropic.com` endpoint), OpenClaw suppresses implicit Anthropic beta headers such as `claude-code-20250219`, `interleaved-thinking-2025-05-14`, and OAuth markers, so custom Anthropic-compatible proxies do not reject unsupported beta flags. Set `models.providers.<id>.headers["anthropic-beta"]` explicitly if your proxy needs specific beta features.

  </Accordion>
</AccordionGroup>

## CLI examples

```bash
openclaw onboard
openclaw models set anthropic/claude-opus-4-6
openclaw models list
```

See also: [Configuration](/gateway/configuration) for full configuration examples.

## Related

- [Configuration reference](/gateway/config-agents#agent-defaults) - model config keys
- [Model failover](/concepts/model-failover) - fallback chains and retry behavior
- [Models](/concepts/models) - model configuration and aliases
- [Providers](/providers) - per-provider setup guides
