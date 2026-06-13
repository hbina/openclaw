# Go Migration Plan

This document details the step-by-step strategy for porting the OpenClaw Node.js/TypeScript reference implementation to a clean, dependency-free Golang implementation. This migration corresponds to **Phase 10** of the broader OpenClaw Slim Fork plan.

## 1. Goal & Architecture

The ultimate objective is to replace the current Node.js runtime with a single compiled Go binary. The Go architecture aims to be simpler, leaner, and faster, while maintaining strict behavioral parity with the slimmed-down TypeScript fork.

### 1.1 Target Go Architecture

The Go project (housed in the `go/` directory) uses a standard idiomatic structure:

- `cmd/openclaw/main.go`: The unified entry point.
- `internal/config`: Loads and parses `openclaw.json` and a separate secrets JSON file.
- `internal/state`: Manages the SQLite database (`agent_state` and `memory_entries`) using `go-sqlite3`.
- `internal/providers`: Defines a universal `Provider` interface, with implementations for **OpenAI** (compatible with OpenAI-like endpoints) and **Anthropic**.
- `internal/channels`: Defines channel adapters for **Telegram**, **WhatsApp**, and **Discord**.
- `internal/memory`: Contains the `memory-core` port for cross-session recall and dreaming logic.
- `internal/gateway`: Houses the HTTP/WebSocket server and the core agent decision loop.

## 2. Implementation Phases

We are adopting a clean-room reimplementation approach rather than a line-by-line translation. We implement domain boundaries iteratively.

### Phase 2.1: Foundation (Completed)

- [x] Initialize the Go module (`github.com/openclaw/openclaw/go`).
- [x] Create the foundational directory structure.
- [x] Implement configuration loading and structures mapping to the slim JSON config schemas.
- [x] Implement the `Provider` interfaces and HTTP clients for OpenAI and Anthropic.
- [x] Set up the `internal/state` SQLite database connection and auto-migration routine.

### Phase 2.2: Channels & External Integrations (In Progress)

- [ ] **Telegram Adapter**: Port the bot token initialization, webhook/polling setup, and message translation.
- [ ] **Discord Adapter**: Port the bot token initialization, intent definitions, and message translation.
- [ ] **WhatsApp Adapter**: Implement the WhatsApp Web QR/session flow using a suitable Go library (e.g., `whatsmeow`).
- [ ] **Pairing Flow**: Implement the authorization and pairing approval logic for devices.

### Phase 2.3: Agent Loop & Memory Core

- [ ] **Memory Core**: Port the embedding, search, and recall features. Wire them directly to the `internal/state` SQLite wrapper.
- [ ] **Agent Loop**: Re-implement the primary agent execution loop. This includes prompt compilation, tool-use execution, and feeding responses back to channels.

### Phase 2.4: Gateway & Transport

- [ ] Implement the Gateway HTTP and WebSocket server.
- [ ] Wire the API endpoints to the Agent loop and Channel webhooks.
- [ ] Establish the `/healthz` endpoint for Docker container orchestration.

## 3. Testing & Parity Validation Strategy

Since the Go port is a complete rewrite, ensuring exact behavioral parity with the Node.js implementation is critical.

### 3.1 Unit Testing

- Write native Go tests (`go test ./...`) for all `internal/` packages.
- Mock external dependencies (e.g., HTTP requests to OpenAI/Anthropic APIs) using Go's `httptest` package.

### 3.2 Fixture-Based Contract Testing

- Capture request/response JSON fixtures from the TypeScript implementation.
- Build Go integration tests that assert the Go Gateway and Agent Loop produce identical JSON tool-calls, memory state mutations, and API responses when fed the same inputs.

## 4. Release & Cutover

Once the Go implementation passes all fixture parity tests and unit tests:

1. **Multi-Stage Docker Build**: Update the root `Dockerfile` to use a Go builder image, outputting a scratch/alpine container that only contains the compiled Go binary and CA certificates.
2. **Parallel Deployment Proof**: Run the Go Docker image locally side-by-side with the old Node.js image to ensure state (`/home/node/openclaw-agent.sqlite`) is perfectly compatible.
3. **Deprecation**: Delete the TypeScript runtime (`src/`, `packages/`, `extensions/`, `scripts/`) from the repository, leaving only the Go source and the operational Docker and markdown files.
