# Slim Fork Migration Plan

## Goal

This branch is intended to become a much slimmer OpenClaw fork. The first goal is to remove most features from `main` and keep only the surfaces needed for a focused reminder-agent style deployment.

Setup should be simple and portable: one canonical JSON config file for non-secret behavior, plus one separate secret file for credentials. Both live outside the image and can be moved between machines with the persisted runtime volume.

The fork should be easier to install, easier to audit, and cheaper to operate than upstream OpenClaw. It should not carry unused mobile apps, unsupported channel plugins, broad provider catalogs, large QA harnesses, unused runtime dependencies, or packaging paths that no longer match the fork's product shape.

Native Windows support is not part of this fork's product surface. The supported operator path is the Docker-first runtime on Unix-like hosts.

Go porting is out of scope. The retained TypeScript/Node runtime is the final
implementation target for this slim fork.

## Target Shape

Keep a small, explicit product surface:

- Core Gateway runtime.
- One primary agent profile.
- Minimal web/dashboard surface required to operate the agent.
- Telegram, WhatsApp, and Discord channel support only.
- Two model providers: OpenAI (including any OpenAI API-compatible endpoint) and Anthropic.
- Built-in memory support through `memory-core` for cross-session recall.
- A simple Docker image that starts SSH and the Gateway by default.
- A single `openclaw.json` setup file plus a companion secret file for credentials, both stored outside the image.
- Persistent runtime state under a Docker volume or a documented host mount.
- A trimmed dependency graph containing only packages needed by the retained runtime, tests, docs, and Docker build.
- No native Windows distribution, installer, or runtime support.

Remove or defer everything else unless a concrete reminder-agent requirement depends on it.

## Non-Goals

- Maintaining parity with upstream OpenClaw feature breadth.
- Preserving every bundled plugin or provider as a compatibility promise.
- Shipping mobile apps as part of this fork.
- Keeping release, QA, and packaging infrastructure that only serves upstream's full product matrix.
- Supporting model providers other than OpenAI (and OpenAI-compatible endpoints) and Anthropic.
- Keeping unused dependencies merely because upstream OpenClaw still needs them.
- Baking private credentials, bot tokens, or provider API keys into a shared image.
- Supporting native Windows, WSL2-specific packaging, or Windows-only installers in this fork.
- Rewriting the slim application in Go.

## Migration Principles

- Prefer deletion over compatibility shims for surfaces not supported by the fork.
- Keep one canonical setup path: Docker-first, with manual SSH access available for auth and debugging.
- Keep runtime state outside the image. The image is shareable; the instance state is not.
- Keep non-secret setup in `openclaw.json` and keep secrets in a separate file. Do not mix credentials into the canonical config file.
- Keep secrets out of git and image layers.
- Collapse config to the current supported shape; do not retain old upstream migration paths unless this fork has already shipped them.
- Remove dependencies when their last retained runtime, build, test, or docs use is deleted. Do not keep package graph weight for removed upstream surfaces.
- Remove tests only when the covered feature is intentionally removed. Keep or rewrite tests for retained core behavior.
- Validate each phase with the narrowest command that proves the retained product still works.
- Treat Windows support as removed scope unless a retained Docker/runtime requirement explicitly depends on it.
- Treat the TypeScript slim runtime as the product runtime. Do not start or
  prepare a Go rewrite in this fork.

## Status (as of branch `slim/reminder-agent`)

Progress so far, by phase:

- **Phase 0 (Safety):** Done. `.dockerignore` excludes `.env`, secrets, auth profiles; no secret files tracked.
- **Phase 1 (Keep/drop):** Done. Matrix recorded above; `extensions/` pruned to `openai`, `anthropic`, `telegram`, `whatsapp`, `discord`, `memory-core`.
- **Phase 2 (Package pruning):** Partial. Mobile apps and unsupported plugins removed; lockfile re-integrated. Remaining: drop now-unused vendor SDKs (Bedrock/Google/etc.) — blocked until the dead provider/Windows runtime code is removed.
- **Phase 3 (Runtime pruning):** Largely done for channels and providers (see Phases 5/6). Some dead provider quirk code in `src/agents` still pending (folds into Phase 2 dep-trim).
- **Phase 4 (Docker-first):** Done and validated. Manual-SSH image builds, healthcheck added, SSH + Gateway `/healthz` verified, restart recovery confirmed.
- **Phase 5 (Channels):** Code complete. Telegram/WhatsApp/Discord are the only channels (catalog, config types, zod schemas, SDK, metadata, docs, tests all trimmed). Live pairing/reply proofs still need real channel credentials.
- **Phase 6 (Providers):** Code complete. OpenAI (+ OpenAI-compatible) and Anthropic only; the Anthropic provider plugin (incl. Claude CLI auth) was restored, external provider catalog trimmed. Live reply proofs need an endpoint/key.
- **Phase 7 (UI/Docs):** Largely done. README, provider docs, `model-providers` concept doc, platforms/channels indexes, and channel troubleshooting rewritten to the slim surface; `docs.json` nav cleaned. Control UI channels, quick settings, and session labels are now pruned to the retained channel set. Remaining: audit the rest of the dashboard/control UI for removed-feature references.
- **Phase 8 (Tests):** Reset. The entire inherited `*.test.ts` suite (4,610 files) was removed for a clean-slate rebuild; vitest config + test helpers kept so focused tests can be re-added. The Phase 8 suite has not been written yet.
- **Phase 9 (Release):** Not started. Go porting is intentionally out of scope.

Cross-cutting decisions made during this work:

- **Windows is not supported.** Windows/macOS/iOS/Android docs, Windows CI workflows, and app docs are removed. Removing the inert win32 code woven through core runtime (`src/**/windows-*.ts` + ~150 `process.platform === "win32"` branches) is in progress in small `tsgo`-verified batches: `windows-task-restart`, `windows-argv`, and `windows-port-pids` are done; `schtasks` (1.4k-line subsystem), `windows-acl`, `windows-command`/`windows-encoding`/`windows-spawn` (hot process-spawn path), and `windows-install-roots` remain.
- **Verification constraint:** with the test suite removed, runtime changes are verified by `tsgo:prod`/`build` only (no behavioral net) until Phase 8 re-adds tests.

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
- Keep the Docker runtime supported on Unix-like hosts only; do not add native Windows packaging or setup paths.
- Document first-run configuration: SSH into the container, configure an OpenAI-compatible provider, configure Telegram/WhatsApp/Discord credentials, restart.
- Document first-run configuration as editing or mounting `openclaw.json` plus the separate secret file, then restarting.
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
- Keep Anthropic auth via both native API key/secret ref and subscription-based Claude CLI; drop Anthropic-via-Bedrock/Vertex.
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
- No docs claim native Windows support for the fork.
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
- Do not produce Windows installers, Windows Hub packages, or Windows release lanes for this fork.

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
- Which OpenAI/Anthropic features are required for the first cut: streaming, tool calls, images, embeddings, structured output, or reasoning/thinking fields?
- Anthropic access path decided: native API key plus subscription-based Claude CLI auth; Anthropic-via-Bedrock/Vertex dropped.
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
