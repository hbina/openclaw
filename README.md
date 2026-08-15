# OpenClaw Go

This branch is a slim, Docker-first personal assistant. The retained
runtime is Go, serves one trusted owner per deployment, uses Telegram for text
messages, and keeps tasks, reminders, durable memory, structured transcripts,
and a derived retrieval index in one SQLite database.

Every generated reply and contextual reminder has an always-on local
provenance trace covering accepted input, selected recall, model/tool rounds,
application transformations, and delivery outcome. Operators inspect these
records with `openclaw trace list` and `openclaw trace show`.

The model stack is local:

- one OpenAI-compatible `llama-server` for chat and tool generation;
- one dedicated EmbeddingGemma `llama-server` for memory and conversation
  retrieval.

There are no hosted-model fallbacks, plugins, multi-agent routing, WhatsApp,
Discord, browser tools, skills, or Node dependencies. The upstream Node runtime
has been removed from this branch; it remains available only in Git history.

## Documentation

- [Product and architecture](docs/architecture.md)
- [Docker setup and configuration](docs/install.md)
- [HTTP API](docs/http-api.md)
- [Telegram behavior](docs/telegram.md)
- [Tasks, reminders, and memory](docs/reminders-memory.md)
- [SQLite schema](docs/database.md)
- [Operations and backups](docs/operations.md)
- [Security and current limitations](docs/security.md)

## Development

Run Go commands from `golang/`:

```bash
GOCACHE=/tmp/openclaw-go-cache go test ./...
GOCACHE=/tmp/openclaw-go-cache go vet ./...
GOCACHE=/tmp/openclaw-go-cache go test -race ./...
GOCACHE=/tmp/openclaw-go-cache go build -o /tmp/openclaw-go ./cmd/openclaw
```

Build the standalone runtime image with:

```bash
docker build -t openclaw-go:local golang
```

`AGENTS.md` records the retained scope, current status, and known gaps.
