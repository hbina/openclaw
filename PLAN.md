# Slim Fork Migration Plan

## Goal

This branch is intended to become a much slimmer OpenClaw fork. The first goal is to remove most features from `main` and keep only the surfaces needed for a focused reminder-agent style deployment.

The fork should be easier to install, easier to audit, and cheaper to operate than upstream OpenClaw. It should not carry unused mobile apps, unsupported channel plugins, broad provider catalogs, large QA harnesses, unused runtime dependencies, or packaging paths that no longer match the fork's product shape.

The final goal is to port the retained slim application from the current TypeScript/Node implementation to Go. The slim fork should define the durable product and protocol surface first, then replace runtime components with Go implementations behind the same documented Docker-first operator experience.

## Target Shape

Keep a small, explicit product surface:

- Core Gateway runtime.
- One primary agent profile.
- Minimal web/dashboard surface required to operate the agent.
- Telegram, WhatsApp, and Discord channel support only.
- Two model providers: OpenAI (including any OpenAI API-compatible endpoint) and Anthropic.
- Built-in memory support through `memory-core` for cross-session recall.
- A simple Docker image that starts SSH and the Gateway by default.
- Persistent runtime state under a Docker volume or a documented host mount.
- A trimmed dependency graph containing only packages needed by the retained runtime, tests, docs, and Docker build.
- A Go implementation of the retained runtime once the slim TypeScript surface is stable.

Remove or defer everything else unless a concrete reminder-agent requirement depends on it.

## Non-Goals

- Maintaining parity with upstream OpenClaw feature breadth.
- Preserving every bundled plugin or provider as a compatibility promise.
- Shipping mobile apps as part of this fork.
- Keeping release, QA, and packaging infrastructure that only serves upstream's full product matrix.
- Supporting model providers other than OpenAI (and OpenAI-compatible endpoints) and Anthropic.
- Keeping unused dependencies merely because upstream OpenClaw still needs them.
- Baking private credentials, bot tokens, or provider API keys into a shared image.
- Rewriting the whole upstream application in Go before the slim runtime surface is proven.

## Migration Principles

- Prefer deletion over compatibility shims for surfaces not supported by the fork.
- Keep one canonical setup path: Docker-first, with manual SSH access available for auth and debugging.
- Keep runtime state outside the image. The image is shareable; the instance state is not.
- Keep secrets out of git and image layers.
- Collapse config to the current supported shape; do not retain old upstream migration paths unless this fork has already shipped them.
- Remove dependencies when their last retained runtime, build, test, or docs use is deleted. Do not keep package graph weight for removed upstream surfaces.
- Remove tests only when the covered feature is intentionally removed. Keep or rewrite tests for retained core behavior.
- Validate each phase with the narrowest command that proves the retained product still works.
- Treat the TypeScript slim runtime as the behavioral reference for the Go port. Do not start the Go rewrite by re-creating removed upstream surfaces.
- Keep protocol, config, state, and Docker behavior explicit enough that Go components can replace TypeScript components incrementally.

## Phase 0: Safety Cleanup

Do this before any commit:

- Remove any staged or untracked secret files, especially `TELEGRAM_BOT_TOKEN`.
- Audit `.env`, Docker build context, and new Compose files to ensure tokens and auth sessions are not copied into images.
- Add or confirm ignore rules for local token/auth artifacts.
- Decide whether this branch will keep full git history or become a new fork root after the first clean slim commit.

Exit criteria:

- `git status` contains no staged secret files.
- Docker build context excludes `.env`, auth profiles, provider API keys, and channel tokens.

## Phase 1: Define The Retained Product Surface

Create an explicit keep/drop matrix before more deletion:

- Keep: Gateway, config loader, agent loop, Telegram, WhatsApp, Discord, OpenAI and Anthropic provider runtime/config, memory-core recall/dreaming, minimal dashboard, Docker manual image.
- Drop: mobile apps, unsupported channel plugins, providers other than OpenAI (and OpenAI-compatible endpoints) and Anthropic, provider-specific auth flows outside the OpenAI/Anthropic API contracts, bundled QA lab/matrix/channel fixtures, release paths for removed artifacts.
- Decide: browser/canvas tools, file-transfer, dashboard depth, updater, docs site packaging, and which OpenAI-compatible features are required.

Retained plugin/integration matrix:

| Surface                          | Retain | Notes                                                                                                    |
| -------------------------------- | ------ | -------------------------------------------------------------------------------------------------------- |
| `openai` provider                | Yes    | OpenAI + any OpenAI-compatible endpoint. API key + base URL + model id.                                  |
| `anthropic` provider             | Yes    | Native Anthropic Messages API. API key + model id.                                                       |
| `telegram` channel               | Yes    | Bot-token setup, pairing, inbound/outbound messaging.                                                    |
| `whatsapp` channel               | Yes    | Existing WhatsApp Web QR/session flow.                                                                   |
| `discord` channel                | Yes    | Bot-token setup, conservative DM/allowlisted behavior first.                                             |
| `memory-core` plugin             | Yes    | Cross-session recall through `memory_get`/`memory_search`, local memory provider, and optional dreaming. |
| Other providers/channels/plugins | No     | Remove from runtime, setup, docs, tests, and package surfaces unless later explicitly re-added.          |

For every kept surface, name:

- Entry point.
- Config keys.
- Runtime dependencies.
- Tests to preserve.
- Setup and verification command.

Exit criteria:

- A keep/drop matrix exists in this plan or a linked migration document.
- Package metadata and workspace package list match the intended keep set.

## Phase 2: Package And Workspace Pruning

Prune the package graph deliberately:

- Remove deleted apps/plugins from `pnpm-workspace.yaml`.
- Remove package exports, files entries, package excludes, scripts, and generated metadata for removed surfaces.
- Keep `memory-core` source, build entries, package exports, and plugin-sdk memory host exports because memory is a retained product feature.
- Regenerate lock/shrinkwrap files using the repo's supported dependency commands.
- Remove dependency patches only when the patched package is no longer in the retained graph.
- Audit direct and transitive dependency weight after each pruning phase; remove root dependencies that are no longer imported by retained code or needed by retained scripts.
- Keep Node and pnpm versions aligned with upstream until the slim fork has its own release policy.

Exit criteria:

- `pnpm install` succeeds from a clean checkout.
- The lockfile contains only retained workspace and runtime dependencies.
- Root `dependencies`, `devDependencies`, package patches, and Docker image installs are justified by retained surfaces.
- Removed package scripts no longer appear in `package.json`.

## Phase 3: Runtime Pruning

Remove runtime code in dependency order:

- Delete unused channel/provider/plugin registries after callers are removed.
- Delete config schemas and defaults for removed surfaces.
- Delete setup/onboarding branches for removed providers and channels.
- Delete runtime discovery for removed bundled plugins.
- Keep runtime discovery/build output for retained bundled plugins: `openai`, `telegram`, `whatsapp`, `discord`, and `memory-core`.
- Keep memory tool registration and memory runtime hooks, but trim any docs/UI/setup language that implies non-retained memory backends are supported.
- Simplify startup metadata so it reports only retained capabilities.
- Keep errors explicit when a removed feature is requested.

Exit criteria:

- Gateway starts with the slim config.
- Removed plugins/providers cannot be loaded accidentally.
- `dist/extensions` contains only retained bundled plugins plus shared runtime dependencies.
- Startup logs and health output describe the slim runtime accurately.

## Phase 4: Docker-First Setup

Make the manual Docker path the primary setup path:

- Build a shareable image that includes source, dependencies, SSH, and the Gateway entrypoint.
- Start SSH and Gateway by default.
- Keep `/home/node` as persistent volume state.
- Persist `memory-core` state under the same `/home/node` volume; never bake memory stores or recall indexes into the image.
- Expose only required ports by default: SSH, Gateway, bridge, and any retained callback port.
- Document first-run configuration: SSH into the container, configure an OpenAI-compatible provider, configure Telegram/WhatsApp/Discord credentials, restart.
- Add a healthcheck once the Gateway can start reliably before first auth.

Exit criteria:

- `docker compose -f docker-compose.manual-ssh.yml up -d --build` starts a stable container.
- SSH works on the documented port.
- Gateway health responds on the documented port after required auth/config is present.
- Restarting the container restarts the Gateway automatically.

## Phase 5: Supported Channel Paths

Make Telegram, WhatsApp, and Discord the only supported channels:

- Keep Telegram config schema, runtime, pairing, and troubleshooting paths.
- Keep WhatsApp config schema, runtime, pairing, and troubleshooting paths.
- Keep Discord config schema, runtime, pairing, intents guidance, and troubleshooting paths.
- Remove generic channel setup language that implies unsupported channels work.
- Provide one setup path per channel:
  - Telegram: BotFather token, Gateway start, pairing approval.
  - WhatsApp: documented login/session flow, Gateway start, pairing approval.
  - Discord: bot token, required intents, Gateway start, pairing approval.
- Decide whether channel credentials are config, env, or file-backed secrets for this fork.
- Keep group/server behavior disabled or tightly allowlisted until needed.

Exit criteria:

- A Telegram DM can create a pairing request.
- `openclaw pairing list telegram` and `openclaw pairing approve telegram <CODE>` work inside the container.
- WhatsApp can complete its documented login/session flow and receive a paired reply.
- Discord can connect with the documented intents and receive a paired reply.
- Each retained channel receives a model-backed reply.

## Phase 6: Model Provider Path (OpenAI + Anthropic)

Make OpenAI (including OpenAI-compatible endpoints) and Anthropic the only model provider surfaces:

- Keep OpenAI provider config: base URL, API key or secret reference, model id, and optional compatibility flags.
- Keep Anthropic provider config: API key or secret reference and model id (native Messages API).
- Remove all other providers (Amazon Bedrock, Anthropic-via-Vertex, Google/Gemini, and any other vendor providers) plus their vendor-specific auth and setup paths.
- Decide whether subscription-based Claude CLI auth is retained or dropped in favor of Anthropic API keys.
- Keep provider behavior focused on the OpenAI and Anthropic request/response contracts used by the agent.
- Add a startup or doctor check that reports a missing key or model id clearly for the configured provider.
- Decide which features are supported per provider: streaming, tool calls, images, embeddings, structured output, and reasoning/thinking fields.

Exit criteria:

- An OpenAI (or OpenAI-compatible) endpoint and an Anthropic API key can each produce a Gateway-backed agent reply.
- Missing provider config fails with an actionable message.
- Removed providers cannot be selected from setup, config defaults, runtime catalogs, or docs.

## Phase 7: UI And Docs Slimming

Keep only docs and UI that match the fork:

- Update README and setup docs to describe the slim fork, not upstream OpenClaw.
- Remove docs for unsupported channels, providers, apps, QA tooling, and release workflows.
- Keep memory docs only for the retained `memory-core` path. Remove or rewrite docs that present removed memory backends as supported in the fork.
- Keep a small operator runbook: build image, run container, SSH login, OpenAI-compatible provider setup, Telegram/WhatsApp/Discord setup, memory/backup expectations, health checks, logs, backup/restore.
- Ensure dashboard/control UI does not advertise removed features.

Exit criteria:

- No visible docs claim removed features are supported.
- The first-run instructions work from a fresh Docker volume.
- `git diff --check` passes for docs and scripts.

## Phase 8: Test Strategy

Replace broad upstream coverage with focused slim coverage:

- Keep unit tests for config, Gateway startup, agent loop, OpenAI-compatible provider requests, `memory-core` recall/dreaming behavior, Telegram pairing/inbound/outbound, WhatsApp pairing/inbound/outbound, Discord pairing/inbound/outbound, and Docker entrypoint behavior.
- Delete tests for removed apps/plugins/providers.
- Delete or rewrite tests for removed memory backends, but keep tests for `memory-core` and its retained plugin-sdk memory host contracts.
- Add smoke tests for the slim container startup path.
- Keep one local fast lane and one Docker smoke lane.

Exit criteria:

- Focused test command is documented and passes.
- Docker smoke proves SSH and Gateway startup.
- Memory smoke proves retained `memory-core` tools register and can read/search persisted runtime state after restart.
- No test references deleted packages or unsupported features.

## Phase 9: Release And Distribution

Define a release model for the fork:

- Pick image name and tag policy.
- Decide whether npm packaging remains supported or Docker is the only supported distribution.
- Remove upstream release scripts that publish removed artifacts.
- Add a minimal release checklist: build image, scan for secrets, run Docker smoke, tag, push image.
- Document backup/restore for the persistent volume.
- Include memory state in backup/restore expectations because `memory-core` is retained and user-visible.

Exit criteria:

- A clean clone can build the image.
- A user can run the published image with documented commands.
- No private instance state is included in the image.

## Phase 10: Go Port

Port the retained slim application to Go after the TypeScript slim runtime has a stable, verified surface.

Retained Go target:

- Gateway HTTP/WebSocket runtime.
- Agent loop for one primary profile.
- OpenAI Chat Completions client (incl. OpenAI-compatible endpoints) and Anthropic Messages client.
- Telegram, WhatsApp, and Discord channel adapters.
- `memory-core` equivalent: persisted recall/search and the retained dreaming behavior.
- Config loader for the slim config shape only.
- SQLite/runtime state under `/home/node`.
- Docker image that keeps the same first-run operator flow: SSH, Gateway, volume state, health checks, and channel/provider setup.

Porting sequence:

1. Freeze the slim TypeScript behavior with focused tests and Docker smoke proof.
2. Write protocol/config/state fixtures from the TypeScript runtime for Gateway, channels, provider calls, pairing, and memory.
3. Implement Go packages behind the retained boundaries: config, state, provider, channel adapters, memory, Gateway, and Docker entrypoint.
4. Run Go and TypeScript implementations side by side against the same fixture corpus until behavior matches for retained surfaces.
5. Switch the Docker image to the Go binary once provider reply, memory, channel pairing/reply, restart, and health checks pass.
6. Remove TypeScript runtime-only code after the Go image fully owns the retained product surface.

Exit criteria:

- A clean clone builds a Go binary and Docker image without Node runtime dependencies for production use.
- The Go image supports the same documented slim first-run path.
- OpenAI-compatible provider reply works through the Go Gateway.
- Telegram, WhatsApp, and Discord pairing/reply work through the Go Gateway.
- Memory recall/search persists across container restart.
- Existing slim config and state either load directly or have a documented one-time migration.
- TypeScript runtime code is no longer required in the production image.

## Immediate Next Actions

1. Remove `TELEGRAM_BOT_TOKEN` from git and local disk if it contains a real token.
2. Decide the exact retained feature set and record it in the keep/drop matrix.
3. Rebuild the manual Docker image with the fixed entrypoint environment.
4. Verify container restart brings SSH and Gateway back automatically.
5. Verify `memory-core` is present in `dist/extensions` and memory tools register in the slim Gateway.
6. Complete one OpenAI-compatible provider reply test.
7. Complete Telegram, WhatsApp, and Discord pairing and reply tests.
8. Capture protocol/config/state fixtures that will become the Go-port compatibility corpus.
9. Reduce staged deletions into reviewable commits by phase.

## Open Questions

- Is the dashboard retained as-is, trimmed, or replaced with CLI-only operation?
- Which OpenAI/Anthropic features are required for the first cut: streaming, tool calls, images, embeddings, structured output, or reasoning/thinking fields?
- Which Anthropic access path is retained: native API key only, or also subscription-based Claude CLI auth? (Anthropic-via-Bedrock/Vertex is dropped.)
- Which `memory-core` behaviors are enabled by default for the first cut: search only, `memory_get`, dreaming, or explicit opt-in memory?
- Which WhatsApp integration path is retained: QR/device login, bot/business API, or the existing upstream provider only?
- Which Discord intents are required, and should server/channel behavior be disabled by default?
- Should browser/canvas/tools remain available for the reminder agent?
- Should this fork keep upstream package names or rename package/image/docs surfaces?
- Should Docker expose ports only on loopback by default, or support LAN by default for local network access?
- Should the Go port preserve OpenClaw's existing Gateway API shape exactly, or define a smaller v1 protocol for the fork?
- Which Go WhatsApp library/runtime should replace the current WhatsApp Web implementation, and what session migration is acceptable?
- Should the Go port keep a small web dashboard, or make CLI/API operation the only supported operator surface?

## Validation Checklist

Before the first slim-fork commit:

- `git status -sb` reviewed for accidental broad deletes and secrets.
- `git diff --check`.
- Clean Docker build of the manual image.
- Container starts without crash-looping.
- SSH login works.
- Gateway health responds.
- `memory-core` is bundled and its retained tools register.
- Memory state survives a container restart through the `/home/node` volume.
- OpenAI-compatible provider reply works.
- Telegram pairing and reply work.
- WhatsApp pairing and reply work.
- Discord pairing and reply work.

Before publishing an image:

- Rebuild from a clean clone.
- Confirm no `.env`, channel token, provider API key, or OpenClaw credentials are present in image layers.
- Run the documented first-run path with a fresh Docker volume.
- Record exact image tag, commit SHA, and verification commands.

Before replacing TypeScript with Go:

- Go binary and Docker image build from a clean clone.
- Go implementation passes the retained fixture corpus.
- Go Docker smoke proves SSH, Gateway health, restart, provider reply, memory persistence, and retained channel pairing/reply.
- Migration behavior for existing `/home/node` state is documented and tested.
