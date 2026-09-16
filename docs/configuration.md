---
title: Configuration
summary: Canonical public and secret configuration
---

Configuration is strict JSON. Unknown keys, including keys from the removed
Node runtime, cause startup to fail.

`openclaw.json`:

```json
{
  "agents": {
    "defaults": {
      "historySearch": {
        "minScore": 0.35
      },
      "memoryMaintenance": {
        "enabled": true,
        "schedule": "0 3 * * *",
        "timezone": "Asia/Kuala_Lumpur",
        "batchSize": 24
      }
    }
  },
  "channels": {
    "telegram": {
      "enabled": true,
      "ownerUserId": "123456789"
    }
  },
  "models": {
    "providers": {
      "openai": {
        "baseUrl": "http://host.docker.internal:8080/v1"
      }
    },
    "embeddings": {
      "baseUrl": "http://host.docker.internal:8081/v1",
      "model": "default",
      "indexId": "embeddinggemma-q8-v1",
      "dimensions": 768
    }
  }
}
```

`channels.telegram.ownerUserId` is the single admitted Telegram owner. It is
stored as a string to preserve the numeric identifier exactly. Updates from any
other Telegram sender, any non-private chat, or a private chat whose chat id
does not equal this value are discarded before the agent, transcript, tools,
or memory pipeline is invoked. The sender id establishes owner identity while
the chat id supplies conversation and delivery routing. When Telegram is
enabled, both this ID and the bot token in `secrets.json` are mandatory startup
inputs; the runtime does not silently start without its admitted channel.
Inbound leases, five-attempt retry policy, and bounded backoff are fixed runtime
behavior and intentionally add no public configuration surface.

`agents.defaults.memoryMaintenance` controls the native Rust consolidation
worker. `schedule` is a five-field cron expression interpreted in the named
IANA `timezone`; `batchSize` bounds the number of complete owner exchanges in
one run. The worker is disabled unless `enabled` is true.

The `openai` name identifies the OpenAI-compatible wire protocol. It does not
enable the hosted OpenAI service, provider fallback, or a Responses API path.
Both configured endpoints must resolve to operator-controlled local services;
startup rejects non-local provider behavior. The runtime deliberately sends
chat model id `default` and uses non-streaming `/chat/completions` responses.

`secrets.json`:

```json
{
  "models": {
    "providers": {
      "openai": {
        "apiKey": "local-server-key-if-required"
      }
    },
    "embeddings": {
      "apiKey": "local-embedding-key-if-required"
    }
  },
  "channels": {
    "telegram": {
      "botToken": "telegram-bot-token"
    }
  }
}
```

Assistant behavior is fixed, neutral, direct, and non-relational. Personality
and identity customization are unsupported: `agents.defaults.soul` and
`agents.defaults.identity` are rejected as unknown keys. Remove both keys from
existing configuration before starting this release.

Configuration does not select a prompt size. The Rust runtime reads the chat
server's active context window and uses its template/tokenizer endpoints before
every submitted generation. Round, tool, tool-result, and retry bounds are
fixed safety behavior rather than operator-tunable settings.

Changing schema-era configuration does not migrate state. For every Rust test
cutover, stop the service, retain an optional operator backup, remove the active
test database, and allow the new binary to create a fresh canonical database.
