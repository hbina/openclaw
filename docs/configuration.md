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
      "soul": "Required operator-owned behavior instructions.",
      "identity": "Required operator-owned identity.",
      "historySearch": {
        "minScore": 0.35
      }
    }
  },
  "channels": {
    "telegram": {
      "enabled": true
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

Persona strings are global and loaded only at startup. Edit
`agents.defaults.soul` or `identity` and restart; there is no chat mutation
tool or persona table.
