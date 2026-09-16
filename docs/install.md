---
title: Install
summary: Build and run the standalone Rust gateway
---

Prerequisites:

- a Rust toolchain compatible with `rust/Cargo.lock`;
- an OpenAI-compatible local chat `llama-server`;
- a dedicated local EmbeddingGemma `llama-server`;
- a non-secret `openclaw.json` and separate `secrets.json`;
- a writable directory for SQLite.

Every Rust test deployment starts with an empty data directory. Historical
Node, Go, and older Rust databases are intentionally not migrated or read.

Build the release binary:

```bash
cd rust
cargo build --locked --release
```

The process requires:

- `OPENCLAW_CONFIG_DIR`, containing `openclaw.json` and `secrets.json`;
- `OPENCLAW_DATA_DIR`, pointing to the writable state directory;
- optionally `TZ`, which controls local prompt time and omitted cron
  timezones;
- optionally `PORT`; the default is `18789`;
- optionally `OPENCLAW_HTTP_ADDR`, which must remain a loopback address.

Start it directly:

```bash
OPENCLAW_CONFIG_DIR=/absolute/path/config \
OPENCLAW_DATA_DIR=/absolute/path/empty-state \
TZ=Asia/Kuala_Lumpur \
./rust/target/release/openclaw-rust
```

For a service deployment, put those variables in a root-readable environment
file and configure the service to execute the release binary. Keep configuration
and state outside the build tree, run as an unprivileged account, and grant
write access only to the data directory.

The repository does not yet ship the final standalone Rust container or
Compose definition. Do not use the retained Go image or its deployment helper
as an alternate production runtime. Root-image and Compose cutover remains a
separate production-readiness item.
