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
other Telegram sender are discarded before the agent, transcript, tools, or
memory pipeline is invoked. When Telegram is enabled, both this ID and the bot
token in `secrets.json` are mandatory startup inputs; the runtime does not
silently start without its admitted channel.

`agents.defaults.memoryMaintenance` controls the native Go consolidation
worker. `schedule` is a five-field cron expression interpreted in the named
IANA `timezone`; `batchSize` bounds the number of complete owner exchanges in
one run. The worker is disabled unless `enabled` is true.

The `openai` name identifies the OpenAI-compatible wire protocol. It does not
enable the hosted OpenAI service. The runtime deliberately sends chat model id
`default`.

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
