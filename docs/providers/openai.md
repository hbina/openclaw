---
summary: "Use an OpenAI-compatible Chat Completions provider in OpenClaw"
read_when:
  - You want to use OpenAI-compatible chat models in the slim fork
  - You need OPENAI_BASE_URL or OPENAI_API_KEY setup
title: "OpenAI"
---

The slim fork supports one model provider:

- provider id: `openai`
- API family: OpenAI-compatible Chat Completions
- default base URL: `https://api.openai.com/v1`
- required credential: API key

Responses API, ChatGPT/Codex OAuth, embeddings, image generation, speech,
realtime, web search, and provider-specific setup flows are not part of the
slim fork surface.

## Configuration

Set the API key and base URL in the Gateway environment or config. Use a model
id that exists on your endpoint.

```json5
{
  env: {
    OPENAI_BASE_URL: "https://api.openai.com/v1",
    OPENAI_API_KEY: "example-openai-key-not-real",
  },
  agents: {
    defaults: {
      model: { primary: "openai/gpt-5.5" },
    },
  },
}
```

For a self-hosted or proxy endpoint:

```json5
{
  env: {
    OPENAI_BASE_URL: "https://llm.example.com/v1",
    OPENAI_API_KEY: "example-compatible-key-not-real",
  },
  agents: {
    defaults: {
      model: { primary: "openai/llama-3.3-70b" },
    },
  },
}
```

## Docker first run

In the slim Docker image, state is persisted under `/home/node`. SSH into the
container, write `/home/node/.openclaw/openclaw.json`, then restart the
container:

```bash
ssh node@127.0.0.1 -p 2222
mkdir -p /home/node/.openclaw
cat >/home/node/.openclaw/openclaw.json <<'JSON5'
{
  env: {
    OPENAI_BASE_URL: "https://api.openai.com/v1",
    OPENAI_API_KEY: "replace-with-your-api-key",
  },
  agents: {
    defaults: {
      model: { primary: "openai/gpt-5.5" },
    },
  },
}
JSON5
exit
docker compose -f docker-compose.manual-ssh.yml restart openclaw-manual
```

See [Docker](/install/docker#slim-manual-ssh-image) for the complete container
flow.

## Related

- [Docker](/install/docker#slim-manual-ssh-image)
- [Telegram](/channels/telegram)
- [Discord](/channels/discord)
- [WhatsApp](/channels/whatsapp)
