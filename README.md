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
Historical Node application state is deliberately not imported or read by the
Go runtime, and no backward-compatibility path is supported.

Memory is a revisioned SQLite ledger with profile, durable, and daily records,
FTS5 plus vector retrieval, local-model query planning/reranking, and a
post-response local-model curator. The ledger cutover requires a fresh Go
database; existing Go state is also deliberately not migrated.

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
GOCACHE=/tmp/openclaw-go-cache go test -tags sqlite_fts5 ./...
GOCACHE=/tmp/openclaw-go-cache go vet -tags sqlite_fts5 ./...
GOCACHE=/tmp/openclaw-go-cache go test -tags sqlite_fts5 -race ./...
GOCACHE=/tmp/openclaw-go-cache go build -tags sqlite_fts5 -o /tmp/openclaw-go ./cmd/openclaw
```

Build the standalone runtime image with:

```bash
docker build -t openclaw-go:local golang
```

`AGENTS.md` records the retained scope, current status, and known gaps.
