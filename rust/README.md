# OpenClaw Rust

This directory contains a standalone Rust reimplementation of the retained Go
runtime. It keeps the same one-owner product boundary, canonical SQLite schema,
local OpenAI-compatible chat and embedding protocols, task/reminder tools,
semantic memory and conversation recall, contextual reminder delivery,
Telegram text channel, provenance traces, and native memory maintenance.

The Rust runtime intentionally does not read or migrate historical Node or
pre-ledger state. Start it with a fresh database, or with a canonical database
created by the retained Go runtime.

## Develop

```bash
cd rust
cargo fmt --all -- --check
cargo clippy --all-targets --locked -- -D warnings
cargo test --locked
cargo build --locked --release
```

Runtime startup uses the same `openclaw.json` and `secrets.json` configuration
contract as the Go implementation:

```bash
OPENCLAW_CONFIG_DIR=/path/to/config \
OPENCLAW_DATA_DIR=/path/to/data \
cargo run --locked
```

The HTTP gateway defaults to `127.0.0.1:18789` and rejects non-loopback bind
addresses. This keeps the unauthenticated CLI endpoint inside the local-machine
trust boundary. `PORT` changes the loopback port; `OPENCLAW_HTTP_ADDR` may set a
different loopback IP socket address.

Useful operator commands include:

```bash
cargo run --locked -- health
cargo run --locked -- chat "List my open tasks"
cargo run --locked -- trace list --database /path/to/openclaw-agent.sqlite
cargo run --locked -- memory status --database /path/to/openclaw-agent.sqlite
cargo run --locked -- memory maintenance status --database /path/to/openclaw-agent.sqlite
```

## Container

Build the standalone, non-root image from the repository root:

```bash
docker build -t openclaw-rust:local rust
```

Mount configuration read-only and state read-write. On Linux, host networking
lets the container reach llama-server processes bound to host loopback while
keeping the gateway on that same loopback boundary. Otherwise, point the model
URLs at an explicitly reachable trusted host address. Telegram delivery does
not need an inbound published port.

```bash
docker run -d --name openclaw-rust \
  --network host \
  -v /path/to/config:/config:ro \
  -v /path/to/data:/data \
  openclaw-rust:local

docker exec openclaw-rust /app/openclaw chat "List my open tasks"
```

Local-model and live Telegram evidence still depends on the operator's actual
llama-server processes and bot credentials; unit tests do not substitute for
those delivery checks.
