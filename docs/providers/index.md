---
summary: "Model providers (LLMs) supported by the slim fork"
read_when:
  - You want to choose a model provider
  - You need a quick overview of supported LLM backends
title: "Provider directory"
---

This slim fork supports two model providers: **OpenAI** (and any OpenAI
API-compatible endpoint) and **Anthropic**. Pick a provider, authenticate, then
set the default model as `provider/model`.

Looking for chat channel docs (Telegram/WhatsApp/Discord)? See [Channels](/channels).

## Quick start

1. Authenticate with the provider (usually via `openclaw onboard`).
2. Set the default model:

```json5
{
  agents: { defaults: { model: { primary: "anthropic/claude-opus-4-6" } } },
}
```

## Provider docs

- [OpenAI (and OpenAI-compatible endpoints)](/providers/openai) - set `OPENAI_BASE_URL` to use any OpenAI-compatible server (vLLM, Ollama, LM Studio, LiteLLM, OpenRouter, etc.).
- [Anthropic (API key + Claude CLI)](/providers/anthropic)

## Community tools

- [Claude Max API Proxy](/providers/claude-max-api-proxy) - community proxy that exposes Claude subscription credentials as an OpenAI-compatible endpoint (verify Anthropic policy/terms before use).
