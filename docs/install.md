---
title: Install
summary: Build and run the standalone Go image
---

Prerequisites:

- Docker;
- an OpenAI-compatible local chat `llama-server`;
- a dedicated local EmbeddingGemma `llama-server`;
- a non-secret `openclaw.json` and separate `secrets.json`;
- a persistent host directory for SQLite.

For the memory-ledger release, the SQLite directory must be empty. Existing
Node and pre-ledger Go databases are intentionally not migrated or read.

Build:

```bash
docker build -t openclaw-go:local golang
```

The container requires:

- `OPENCLAW_CONFIG_DIR`, containing read-only `openclaw.json` and
  `secrets.json`;
- `OPENCLAW_DATA_DIR`, pointing to a writable persistent directory;
- optionally `TZ`, which controls local prompt time and omitted cron
  timezones;
- optionally `PORT`; the default is `18789`.

Example:

```bash
docker run -d \
  --name openclaw-go \
  --restart unless-stopped \
  -p 127.0.0.1:18789:18789 \
  -e OPENCLAW_CONFIG_DIR=/config \
  -e OPENCLAW_DATA_DIR=/data \
  -e TZ=Asia/Kuala_Lumpur \
  -v /absolute/path/openclaw.json:/config/openclaw.json:ro \
  -v /absolute/path/secrets.json:/config/secrets.json:ro \
  -v /absolute/path/state:/data \
  openclaw-go:local
```

Keep the Gateway on loopback or another trusted network until HTTP
authentication and request limits are implemented.

For a guarded transition from an existing container, use the repository's
fresh-state helper rather than pointing Go at an older database:

```bash
scripts/deploy-go-docker.py cutover --yes \
  --previous-container PREVIOUS_CONTAINER
```

The helper retains the stopped previous container and prints the corresponding
rollback command after all automated proof passes. Telegram owner and rejected
non-owner delivery still require manual acceptance before declaring the
cutover complete.
