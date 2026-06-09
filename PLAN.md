# Slim Fork Migration Plan

## Goal

This branch is intended to become a much slimmer OpenClaw fork. The goal is to remove most features from `main` and keep only the surfaces needed for a focused reminder-agent style deployment.

The fork should be easier to install, easier to audit, and cheaper to operate than upstream OpenClaw. It should not carry unused mobile apps, unsupported channel plugins, broad provider catalogs, large QA harnesses, or packaging paths that no longer match the fork's product shape.

## Target Shape

Keep a small, explicit product surface:

- Core Gateway runtime.
- One primary agent profile.
- Minimal web/dashboard surface required to operate the agent.
- Telegram, WhatsApp, and Discord channel support only.
- One model provider contract: any OpenAI API-compatible endpoint.
- Built-in memory support through `memory-core` for cross-session recall.
- A simple Docker image that starts SSH and the Gateway by default.
- Persistent runtime state under a Docker volume or a documented host mount.

Remove or defer everything else unless a concrete reminder-agent requirement depends on it.

## Non-Goals

- Maintaining parity with upstream OpenClaw feature breadth.
- Preserving every bundled plugin or provider as a compatibility promise.
- Shipping mobile apps as part of this fork.
- Keeping release, QA, and packaging infrastructure that only serves upstream's full product matrix.
- Supporting non-OpenAI-compatible model providers.
- Baking private credentials, bot tokens, or provider API keys into a shared image.

## Migration Principles

- Prefer deletion over compatibility shims for surfaces not supported by the fork.
- Keep one canonical setup path: Docker-first, with manual SSH access available for auth and debugging.
- Keep runtime state outside the image. The image is shareable; the instance state is not.
- Keep secrets out of git and image layers.
- Collapse config to the current supported shape; do not retain old upstream migration paths unless this fork has already shipped them.
- Remove tests only when the covered feature is intentionally removed. Keep or rewrite tests for retained core behavior.
- Validate each phase with the narrowest command that proves the retained product still works.

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

- Keep: Gateway, config loader, agent loop, Telegram, WhatsApp, Discord, OpenAI-compatible provider runtime/config, memory-core recall/dreaming, minimal dashboard, Docker manual image.
- Drop: mobile apps, unsupported channel plugins, non-OpenAI-compatible providers, provider-specific auth flows outside the OpenAI-compatible API contract, bundled QA lab/matrix/channel fixtures, release paths for removed artifacts.
- Decide: browser/canvas tools, file-transfer, dashboard depth, updater, docs site packaging, and which OpenAI-compatible features are required.

Retained plugin/integration matrix:

| Surface                          | Retain | Notes                                                                                                    |
| -------------------------------- | ------ | -------------------------------------------------------------------------------------------------------- |
| `openai` provider                | Yes    | OpenAI-compatible Chat Completions only. API key + base URL + model id.                                  |
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
- Keep Node and pnpm versions aligned with upstream until the slim fork has its own release policy.

Exit criteria:

- `pnpm install` succeeds from a clean checkout.
- The lockfile contains only retained workspace and runtime dependencies.
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

## Phase 6: OpenAI-Compatible Provider Path

Make OpenAI-compatible APIs the only model provider surface:

- Keep provider config for base URL, API key or secret reference, model id, and optional compatibility flags.
- Remove Anthropic, Claude CLI, vendor-specific auth, and non-OpenAI-compatible provider setup paths.
- Keep provider behavior focused on the OpenAI-compatible request/response contract used by the agent.
- Add a startup or doctor check that reports missing provider endpoint, key, or model id clearly.
- Decide which compatibility features are supported: streaming, tool calls, images, embeddings, structured output, and reasoning fields.

Exit criteria:

- A local or hosted OpenAI-compatible endpoint can produce a Gateway-backed agent reply.
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

## Immediate Next Actions

1. Remove `TELEGRAM_BOT_TOKEN` from git and local disk if it contains a real token.
2. Decide the exact retained feature set and record it in the keep/drop matrix.
3. Rebuild the manual Docker image with the fixed entrypoint environment.
4. Verify container restart brings SSH and Gateway back automatically.
5. Verify `memory-core` is present in `dist/extensions` and memory tools register in the slim Gateway.
6. Complete one OpenAI-compatible provider reply test.
7. Complete Telegram, WhatsApp, and Discord pairing and reply tests.
8. Reduce staged deletions into reviewable commits by phase.

## Open Questions

- Is the dashboard retained as-is, trimmed, or replaced with CLI-only operation?
- Which OpenAI-compatible features are required for the first cut: streaming, tool calls, images, embeddings, structured output, or reasoning fields?
- Which `memory-core` behaviors are enabled by default for the first cut: search only, `memory_get`, dreaming, or explicit opt-in memory?
- Which WhatsApp integration path is retained: QR/device login, bot/business API, or the existing upstream provider only?
- Which Discord intents are required, and should server/channel behavior be disabled by default?
- Should browser/canvas/tools remain available for the reminder agent?
- Should this fork keep upstream package names or rename package/image/docs surfaces?
- Should Docker expose ports only on loopback by default, or support LAN by default for local network access?

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
