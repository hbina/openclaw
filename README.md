# OpenClaw Rust

OpenClaw is a locally operated personal assistant for exactly one trusted owner
per deployment. The canonical production runtime is one standalone Rust binary
with Telegram text delivery, an HTTP chat endpoint, owner-global tasks,
one-shot and recurring reminders, revisioned memory, conversation recall, and
SQLite persistence.

Assistant behavior is fixed, neutral, and non-relational. Chat and embeddings
use operator-controlled local `llama-server` instances through their
OpenAI-compatible protocols. There are no hosted-model fallbacks, plugins,
multi-agent routing, browser tools, or Node dependencies.

Every response keeps an inspectable SQLite trace of admitted input, prompt
projection metadata, context-budget decisions, recall evidence, model and tool
rounds, output transformations, indexing, and delivery. Current owner text is
sent separately from the application-produced routing/reply carrier, and
historical text is never promoted to current instruction authority.

SQLite is the sole state authority for tasks, reminders, memories, transcripts,
derived FTS/vector indexes, inbound Telegram work, and traces. Historical Node,
Go, and pre-ledger Rust state is deliberately not migrated or read; use a fresh
database for every Rust test cutover.

## Documentation

- [Product and architecture](docs/architecture.md)
- [Installation](docs/install.md)
- [HTTP API](docs/http-api.md)
- [Telegram behavior](docs/telegram.md)
- [Tasks, reminders, and memory](docs/reminders-memory.md)
- [SQLite schema](docs/database.md)
- [Operations and recovery](docs/operations.md)
- [Security and limitations](docs/security.md)

## Development

Run Rust commands from `rust/`:

```bash
cargo fmt --all -- --check
cargo clippy --all-targets --all-features --locked -- -D warnings
cargo test --locked
cargo build --locked --release
```

The final standalone Rust container and Compose definition are not yet shipped.
Use the release binary for current testing; the retained Go image is not an
alternate production runtime.

`AGENTS.md` records the retained product intent, safety boundaries, current
risks, and production cutover criteria.
