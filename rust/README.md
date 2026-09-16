# OpenClaw Rust

This directory contains the canonical standalone OpenClaw runtime. It provides
the one-owner SQLite state model, local OpenAI-compatible chat and embedding
clients, task/reminder/memory tools, hybrid recall, contextual reminder
delivery, Telegram private-text admission, HTTP chat, provenance traces, and
native background memory maintenance.

Historical Node, Go, and pre-ledger Rust state is deliberately unsupported.
Start every Rust test cutover with a fresh database created by this runtime.

## Develop

```bash
cargo fmt --all -- --check
cargo clippy --all-targets --all-features --locked -- -D warnings
cargo test --locked
cargo build --locked --release
```

Runtime startup reads the strict `openclaw.json` and separately mounted
`secrets.json` contract:

```bash
OPENCLAW_CONFIG_DIR=/path/to/config \
OPENCLAW_DATA_DIR=/path/to/data \
cargo run --locked
```

The Gateway defaults to loopback port `18789` and rejects non-loopback bind
addresses. `PORT` changes the port; `OPENCLAW_HTTP_ADDR` may select another
loopback socket address.

Useful commands include:

```bash
cargo run --locked -- health
cargo run --locked -- chat "List my open tasks"
cargo run --locked -- trace list --database /path/to/openclaw-agent.sqlite
cargo run --locked -- memory status --database /path/to/openclaw-agent.sqlite
```

## Container status

The final standalone Rust image and Compose definition are not yet present.
Run the release binary directly for current testing. The retained Go image and
deployment scripts are historical and are not a supported Rust path.

Credential-backed Telegram and live-model proof depends on the operator's
actual local services; unit tests do not substitute for those checks.
