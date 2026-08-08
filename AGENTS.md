# Repository Intent

## Why This File Exists

`AGENTS.md` preserves the intent, constraints, and trade-offs that cannot be
reliably reconstructed from source code. The codebase explains how the product
works; this file explains why it has its present shape and which user outcomes
must survive future changes.

Keep this file focused on durable product direction, architectural boundaries,
known risks, and the meaning of “done.” Do not turn it into a file inventory,
command reference, implementation walkthrough, or record of ephemeral
deployment details. When an operational rule is important enough to retain
here, explain the failure or user harm that the rule prevents.

Scoped `AGENTS.md` files may add rationale for their subtree, but they must not
override the product intent recorded here. Update this file when goals, scope,
accepted trade-offs, priorities, or cutover criteria change—not merely because
an implementation detail moved.

## Product Purpose

OpenClaw is one locally operated personal assistant for exactly one trusted
owner per deployment. Its purpose is to provide useful reminders, durable
memory, conversation recall, and a consistent operator-defined persona while
keeping the owner’s conversations and state under local control.

Each owner runs a separate instance. This keeps the trust model understandable
and avoids importing account, tenancy, and cross-user data-isolation complexity
into a personal tool. Channel and sender identifiers route the owner’s
conversations and topics; they are not internal ownership boundaries.

The retained product is intentionally narrow: a standalone Go gateway and
agent loop, Telegram text delivery, one-shot and recurring reminders, semantic
memory and conversation recall, operator-owned persona configuration, local
chat and embedding models, and persistent SQLite state. Upstream OpenClaw’s
broader platform surface is not the goal of this fork.

## Deliberate Product Boundaries

- **Local inference is a privacy and availability boundary.** Chat and
  embeddings run through local `llama-server` instances so private assistant
  data does not depend on a hosted model provider. The retained `openai`
  provider name denotes an OpenAI-compatible wire protocol only. Hosted
  OpenAI, Anthropic, ChatGPT, Claude, CLI-agent, MCP-subprocess, and cloud
  fallback paths do not belong in the production Go runtime.
- **Go is the only production runtime.** The slim fork removed the Node runtime
  to reduce the deployable surface and eliminate a second behavioral path.
  Deleted Node code in Git history may clarify an ambiguous retained behavior,
  but it is not a reason to restore unsupported upstream features.
- **SQLite is the single state authority.** Reminders, memory, transcripts, and
  derived recall metadata belong together so mutation, backup, recovery, and
  inspection have one coherent boundary. JSON, JSONL, or text sidecars would
  create split-brain and partial-recovery risks.
- **Persona is operator-owned configuration.** `soul` and `identity` define the
  one assistant across every conversation. Requiring explicit startup
  configuration keeps persona changes deliberate and reviewable; chat-driven
  persona mutation, implicit defaults, compatibility fallbacks, and separate
  persona state would undermine that ownership.
- **Ingress admission and internal routing are different concerns.** Pairing or
  allowlists protect the one-owner boundary at channel entry. Once admitted,
  all non-secret state belongs to that owner and may be useful across the
  owner’s channels; do not turn routing keys into tenant partitions.
- **Reminder tools express reminder state, not arbitrary scheduled agents.** A
  reminder may use persona and conversation context to phrase a notification,
  but it cannot silently browse, watch for changes, suppress unchanged results,
  or contact another person. Pretending otherwise would promise work the
  current scheduler cannot perform. If Node-style scheduled agent behavior is
  retained, it needs an explicit design rather than being smuggled into reminder
  wording.

## Behavioral Invariants and Their Rationale

- Trusted channel and sender identity must come from ingress routing, never
  model-controlled tool arguments, because prompt content is not an
  authorization source.
- A state mutation and its matching tool-result transcript must commit
  atomically. The assistant must never claim durable work that the database did
  not accept.
- Exact tool-call identifiers and deterministic prompt/tool ordering must be
  preserved so stored conversations can be replayed without changing meaning.
- Semantic reminder selection uses normal model tool choice rather than
  keyword forcing. User intent is contextual, and lexical triggers create
  false reminder mutations.
- Recalled conversation is historical evidence, not current instruction. It
  must be framed as non-authoritative so old tool requests or adversarial text
  are not replayed as new commands.
- Reminder completion follows successful channel delivery. Failed sends must
  remain retryable; recurring advancement or one-shot deletion before delivery
  would silently lose reminders.
- Contextual reminder wording is an enhancement, not a delivery dependency.
  If recall or generation is unavailable, the stored reminder text remains the
  truthful fallback.
- Secrets are operational inputs, not application state or documentation.
  Credentials must never be printed, committed, copied into images, or exposed
  in reports.

## Migration Truth and Current Risk

The retained Go runtime already supports local chat and structured tool calls,
atomic reminder operations, one-shot and recurring schedules, semantic memory
and conversation recall, required persona configuration, contextual reminder
delivery with structured transcripts, basic Telegram text delivery, health and
chat HTTP endpoints, and a standalone image without Node or hosted-model
dependencies.

That working feature set is not equivalent to production readiness. Remaining
work is prioritized by the user harm it prevents:

1. **HTTP admission, limits, bind safety, and stable errors** prevent unintended
   access and unbounded resource use at the exposed gateway.
2. **Telegram owner admission and correct conversation behavior** prevent
   strangers or ambiguous routing from entering the trusted-owner context.
   Pairing and allowlists, DM/group identity, media, threads, reactions,
   multi-account expectations, and live credential-backed proof remain
   unresolved parts of that boundary.
3. **Durable reminder claims and delivery idempotency** prevent duplicate or
   lost notifications across crashes and concurrent delivery attempts.
4. **A deliberate scheduled-agent decision** prevents contextual reminder
   wording from being mistaken for site watching, conditional suppression, or
   third-party contact. Those Node-style jobs remain a separate unresolved
   capability.
5. **A deliberate Node-state decision** prevents historical owner data from
   being silently abandoned. The Node runtime was deleted before parity
   fixtures or an importer were captured. Recover evidence from Git history at
   `53284074db`, or record an explicit accepted-discard decision.
6. **Backup, restore, corruption, restart, and rollback drills** establish that
   local ownership is meaningful during failure, not only during normal use.
7. **Compose and root-image cutover** remove the final ambiguity about which
   runtime operators are expected to deploy.

Close these risks in order unless the user chooses a different priority. A
complete, proven slice is more valuable than several partially implemented
ones because operational confidence depends on end-to-end behavior.

## Evidence and Change Discipline

Behavioral claims must come from source, callers, tests, dependency contracts,
and observed behavior—not from a diff alone. This matters especially for local
model behavior, where a mock cannot establish that the deployed Gemma model
understands a prompt or strict tool schema.

Verification should be proportional to the user-facing risk. State changes
need persistence and failure-path proof; provider, retrieval, reminder,
persona, and channel changes need live local-model or delivery evidence in
addition to focused tests. Missing proof must be reported as a gap rather than
converted into a claim of parity.

The working tree may contain intentional migration work owned by the user.
Inspect it before editing and preserve unrelated staged, unstaged, and untracked
changes. Do not reset, restore, stash, delete, or absorb work merely to obtain a
clean diff. Commit only when asked and include only the intended files. These
rules protect work whose context may not be visible from the current task.

Public documentation must describe verified behavior and explicit limitations,
not aspirations as shipped features. Reports should use repository-relative
source references so they remain useful outside one machine.

`AGENTS.md` is the source of this guidance. `CLAUDE.md` remains a symlink so
agents receive one consistent set of intentions rather than divergent copies.

## Production Cutover Meaning

Production cutover is complete only when the retained behaviors have focused
tests and live local-model proof, HTTP and Telegram enforce the one-owner
admission boundary, reminder delivery is crash-safe and idempotent, historical
Node state has a tested import or explicit discard decision, recovery and image
rollback drills pass, and the documented deployment uses the standalone Go
image.

The removal of Node and unsupported cloud/platform surfaces is already
complete. It narrows the system; it does not waive the remaining safety and
recovery obligations.
