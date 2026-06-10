---
summary: "Model providers (LLMs) supported by the slim fork"
read_when:
  - You want to choose a model provider
  - You want quick setup examples for LLM auth + model selection
title: "Model provider quickstart"
---

This slim fork supports OpenAI (and any OpenAI API-compatible endpoint) and
Anthropic. Pick one, authenticate, then set the default model as `provider/model`.

## Quick start (two steps)

1. Authenticate with the provider (usually via `openclaw onboard`).
2. Set the default model:

```json5
{
  agents: { defaults: { model: { primary: "anthropic/claude-opus-4-6" } } },
}
```

## Supported providers

- [OpenAI (and OpenAI-compatible endpoints)](/providers/openai) - set `OPENAI_BASE_URL` to point at any OpenAI-compatible server (vLLM, Ollama, LM Studio, LiteLLM, OpenRouter, etc.).
- [Anthropic (API key + Claude CLI)](/providers/anthropic)

## Related

- [Model selection](/concepts/model-providers)
- [Model failover](/concepts/model-failover)
- [Models CLI](/cli/models)
