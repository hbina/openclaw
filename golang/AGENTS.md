# Go Runtime Intent

This directory is the sole production implementation of the retained OpenClaw
assistant. The repository-level `AGENTS.md` defines the product and trust
model; this file records why the Go runtime uses its present boundaries.

Source code and tests explain package mechanics. Keep this document focused on
the reasoning that should guide changes when several implementations appear
technically possible.

## Runtime Goals

The Go runtime exists to make the assistant small, locally deployable, and
operationally understandable. A single canonical path is preferred over
compatibility branches because every additional path makes persistence,
failure recovery, and local-model behavior harder to prove.

The runtime is deliberately independent of Node, hosted model providers, and
cloud fallbacks. Provider abstractions exist to isolate local wire contracts
and enable testing, not to reopen the product to hosted services.

## Architectural Rationale

- **Configuration is strict and startup-loaded.** Invalid model, persona, or
  storage configuration should fail before the assistant accepts work. Hidden
  defaults and late fallbacks turn operator mistakes into inconsistent runtime
  behavior.
- **The provider boundary is narrow.** Chat generation, embeddings, and prompt
  sizing have different contracts and failure modes. Small interfaces keep
  those contracts testable without making the rest of the runtime aware of a
  particular server implementation.
- **Tool execution is the trust boundary for model output.** Model arguments
  are untrusted proposals. Validation, trusted routing identity, state mutation,
  and transcript recording converge at the executor so no alternate caller can
  bypass the same guarantees.
- **SQLite transactions define durable truth.** Task, reminder, or memory mutation and
  the tool result that justifies the assistant’s claim belong in one commit.
  Reminder delivery transcripts and schedule completion share the same
  requirement. Partial success would make the conversation disagree with the
  state users actually own.
- **The supported schema is canonical rather than self-migrating.** Silent
  runtime compatibility branches obscure which data shape has been tested.
  Schema changes therefore require a separately reviewed, backup-first operator
  transition instead of opportunistic startup mutation.
- **Conversation history is structured.** Inbound messages, scheduled reminder
  events, tool calls, and tool results retain their meaning so replay and recall
  do not have to infer semantics from flattened prose.
- **The semantic index is derived state.** Conversation history is the source
  of truth; embeddings and chunks must be rebuildable. This allows model or
  index changes without making vectors irreplaceable owner data.
- **Recalled history is explicitly non-authoritative.** Retrieval provides
  relevant evidence, not new instructions. This distinction prevents archived
  requests and tool calls from becoming unintended current actions.
- **Conversation turns are serialized per routing key.** Ordered transcripts
  and matching tool-call sequences matter more than parallelism within one
  conversation. Independent conversations may still proceed without sharing a
  global lock.
- **Tool schemas remain simple and strict.** The deployed local Gemma model must
  use them reliably. Schema elegance or breadth is less important than
  predictable calls under the actual local model.
- **Contextual reminders are tool-free.** A due reminder may use persona,
  recent conversation, and semantic recall to improve wording, but it must not
  acquire new capabilities while firing. The stored text remains the fallback,
  and the exact delivered exchange becomes conversation history.

## Implementation Values

Prefer explicit errors, bounded contexts at I/O boundaries, strict decoding,
and small packages because failure should be visible at the boundary where it
can be understood. Avoid hidden global fallbacks: they make a local deployment
appear healthy while silently changing its behavior.

Preserve one canonical implementation. Removing a stale path is usually safer
than maintaining a shim whose behavior must be proven forever. New dependencies
need direct contract inspection and pinned versions because dependency behavior
becomes part of the local assistant’s reliability envelope.

Do not let model-controlled data substitute for trusted routing identity. Do
not partition owner state by channel or sender as though those values denoted
separate tenants. Both rules follow from the repository’s single-owner trust
model.

## Evidence Expected From Changes

Tests should prove successful behavior, validation, persistence, reopen
behavior, and meaningful failures. Concurrency-sensitive changes also need race
evidence. Build and static-analysis success establish basic integrity, but they
do not prove user-visible model behavior.

Provider, retrieval, reminder-delivery, persona, state, and channel changes
need proportional proof against the configured local servers or real delivery
boundary. Mocked HTTP is useful for deterministic failure coverage; it cannot
establish parity with the deployed local model.

State-affecting changes, including the owner-global task ledger, must show that
transcript and database truth remain aligned and survive restart where
relevant. Report unavailable live proof plainly rather than weakening the
claimed contract.
