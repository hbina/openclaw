---
title: Input Prompt Improvement Plan
summary: Planned improvements to private Telegram DM ingestion and Rust model-context assembly
---

# Input Prompt Improvement Plan

Status: **in progress; Phase 1 implemented, later phases remain planned**

This document tracks improvements to the canonical Rust runtime's handling of
an admitted Telegram message, from private-DM ingress through the final local
chat-model request. Checked items describe behavior supported by the Rust
runtime. Unchecked implementation items are plans and must not be read as
current product behavior.

## Outcome

An admitted owner message should reach the local chat model with:

- a private-DM identity established entirely by Telegram ingress;
- current owner text kept separate from trusted routing facts, quoted text,
  conversation history, and recalled evidence;
- deterministic system, history, recall, and tool ordering;
- one token budget covering every part of the submitted request;
- bounded tool execution and enough trace evidence to reproduce the request;
- no expansion into groups, media, hosted models, personalities, workspace
  prompt files, or upstream's general plugin platform.

The purpose is not prompt parity with upstream OpenClaw. Upstream source may be
used as behavioral evidence, but the retained design remains a small,
single-owner Rust assistant using local OpenAI-compatible `llama-server`
endpoints and SQLite as its only state authority.

## Fixed Scope and Decisions

| Decision | Retained behavior |
| --- | --- |
| Owner model | Exactly one configured numeric Telegram owner per deployment. |
| Telegram surface | Text messages in the owner's private chat with the bot only. |
| Unsupported Telegram surfaces | Groups, supergroups, channels, topics, edits, reactions, media, and multiple bot accounts are rejected or ignored before agent handling. |
| Model providers | Local chat and embedding `llama-server` instances only. The `openai` name describes the compatible wire protocol, not a hosted service. |
| Runtime prompt | Fixed, neutral, and non-relational. It is not configurable through persona or workspace files. |
| State | SQLite is authoritative for transcripts, memory, derived recall data, traces, tasks, and reminders. |
| Recall | Retrieval and evidence selection remain required stages. Recall query rewriting is optional and retains its audited raw-query fallback. |
| Tools | Tool schemas are supplied separately in the Chat Completions request. Trusted owner and routing identity never come from tool arguments. |

## Current Rust Baseline

| Status | Existing capability | Evidence |
| --- | --- | --- |
| [x] | Telegram rejects senders whose `from.id` does not match the configured numeric owner ID. | `rust/src/channels.rs` |
| [x] | One level of Telegram reply and selected-quote context is represented structurally. | `rust/src/channels.rs`, `rust/src/gateway/history.rs` |
| [x] | Inbound messages, assistant messages, exact tool calls, and tool results are persisted as structured transcript rows. | `rust/src/state/conversation.rs`, `rust/src/gateway/history.rs` |
| [x] | Complete tool-call/result sequences are reconstructed without replaying an incomplete tool tail. | `rust/src/gateway/history.rs` |
| [x] | Recall planning has a strict tool contract and falls back to the trimmed current request without explicit keywords when that contract is invalid. | `rust/src/gateway/agent.rs` |
| [x] | Active memories use hybrid keyword and vector retrieval; older complete conversation exchanges use vector retrieval. | `rust/src/memory.rs`, `rust/src/gateway/rag.rs` |
| [x] | Nonempty recall candidates undergo a structured evidence-selection call before final generation. | `rust/src/gateway/agent.rs` |
| [x] | Recalled conversations are marked as historical evidence rather than current instructions. | `rust/src/gateway/agent.rs`, `rust/src/gateway/rag.rs` |
| [x] | Chat requests send structured function schemas, disable parallel tool calls, and preserve exact tool-call identifiers. | `rust/src/providers.rs`, `rust/src/gateway/agent.rs` |
| [x] | The exact sanitized Chat Completions request and the model response are recorded in the SQLite trace. | `rust/src/gateway/agent.rs`, `rust/src/state/trace.rs` |

## Target Input and Prompt Pipeline

```text
Telegram update
  -> validate private chat and configured owner
  -> durably identify and claim the inbound update
  -> construct canonical trusted and untrusted input fields
  -> acquire the private-conversation lock
  -> persist the structured inbound event
  -> load a token-budgeted recent transcript
  -> plan recall, retrieve candidates, and select evidence
  -> allocate the complete model-input budget
  -> assemble ordered system, evidence, history, reply, and user messages
  -> run a bounded local Chat Completions tool loop
  -> synchronously index the completed exchange
  -> deliver and finalize the SQLite trace
```

The desired final model-message ordering is:

| Order | API role | Content | Trust treatment |
| ---: | --- | --- | --- |
| 1 | `system` | Fixed assistant behavior, task/reminder semantics, tool rules, recall rules, and a prompt-contract version. | Application-authored and authoritative. |
| 2 | `system` | Minimal ingress facts: Telegram channel, private conversation kind, chat ID, message ID, and timestamp. | Gateway-authored and authoritative; contains no human-authored names or text. |
| 3 | `system` | Bounded active profile memory and selected recall evidence. | Historical evidence, explicitly non-authoritative. |
| 4 | historical roles | Newest complete exchanges that fit the budget, including exact assistant tool calls and matching tool results. | Prior transcript; old user text is not a current request. |
| 5 | `user` | Optional replied-to or selected quote context. | Human-authored historical text, explicitly untrusted as a current instruction. |
| 6 | `user` | Current admitted owner message. | The only current owner request. |
| API `tools` field | function schemas | Per-run tool definitions in deterministic name order. | Application-defined capabilities; no model-controlled routing identity. |

## Progress Tracker

### Phase 1: Enforce the private-DM ingress contract

| Status | Work item | Acceptance evidence |
| --- | --- | --- |
| [x] | Deserialize Telegram `update_id`, `chat.id`, and `chat.type` in addition to `from.id` and `message_id`. | Fixture tests cover all admitted and rejected chat shapes. |
| [x] | Require `chat.type == "private"`, `from.id == configured_owner_id`, and `chat.id == configured_owner_id` before invoking the agent. | Owner-authored group and supergroup messages cannot create traces, transcripts, model calls, or deliveries. |
| [x] | Use `from.id` only for admission and owner identity; use `chat.id` for the conversation route and Telegram delivery target. | A test proves routing and delivery do not read the sender field interchangeably. |
| [x] | Define a canonical Rust inbound structure containing update ID, message ID, chat ID, sender ID, timestamp, normalized current text, and optional structured reply context. | Serialization round-trip and deterministic rendering tests pass. |
| [x] | Normalize newlines, reject unsafe control content, and apply explicit input/reply size limits before persistence or model work. | Boundary-size, malformed-input, and Unicode tests pass. |

This phase deliberately does not introduce general account, topic, thread,
group, or channel abstractions. Private Telegram chats need two semantically
distinct numeric values—sender identity and chat/delivery identity—even though
they are required to be equal for the supported deployment.

### Phase 2: Make inbound handling durable and replay-safe

| Status | Work item | Acceptance evidence |
| --- | --- | --- |
| [ ] | Add a SQLite inbound-event record with scoped uniqueness for the Telegram chat and message/update identity. | Replaying one Telegram update cannot execute a task, reminder, or memory mutation twice. |
| [ ] | Persist an accepted update before advancing the durable polling checkpoint. | A restart after admission but before generation retains retryable work. |
| [ ] | Add received, processing, completed, failed, and leased/abandoned processing states with bounded retry attempts. | Crash-and-restart tests recover abandoned work without concurrent duplicate handling. |
| [ ] | Define duplicate behavior for completed, active, retryable, and permanently failed events. | Tests prove each state has a deterministic response and no accidental tool replay. |

This durability work is part of the input boundary because the same Telegram
text must not become two independent current requests after a restart.

### Phase 3: Separate trusted routing context from human-authored text

| Status | Work item | Acceptance evidence |
| --- | --- | --- |
| [ ] | Replace the current combined rendered input with separately derived trusted ingress context, optional quoted context, and current owner text. | Captured model requests show each component in its intended role and order. |
| [ ] | Keep human-authored reply bodies and selected quotes out of authoritative system instructions. | A quoted reminder/tool request does not cause a mutation unless the current owner message independently requests it. |
| [ ] | Add fixed prompt language stating that recalled conversations, previous user turns, tool output, and quoted text are evidence rather than current instructions. | Adversarial fixture tests and live local-model tests preserve the distinction. |
| [ ] | Stop identifying the owner in behavioral prose as `User <numeric id>`; use a neutral admitted-owner label and keep necessary IDs in the trusted ingress block. | Prompt snapshots contain no unnecessary owner-ID interpolation. |
| [ ] | Version the prompt assembly contract and record the version in every response trace. | Operators can associate a stored request with the exact assembly rules that produced it. |

### Phase 4: Budget the entire model request

| Status | Work item | Acceptance evidence |
| --- | --- | --- |
| [ ] | Replace the fixed two-exchange recent window with newest-first complete exchanges selected under a token budget. | Follow-up tests retain more ordinary context when space permits and never split tool transactions. |
| [ ] | Reserve tokens for output, the fixed prompt, tool schemas, trusted ingress facts, and the current owner message before admitting optional context. | Required content either fits or fails with a stable explicit error before provider submission. |
| [ ] | Jointly budget reply context, recent history, active core memory, recalled memory, and recalled conversations. | The final request remains within the discovered `llama-server` context size for worst-case fixtures. |
| [ ] | Bound individual and aggregate tool-result replay sizes while preserving call/result pairing and exact IDs. | Oversized tool results are deterministically reduced or excluded without producing an invalid transcript. |
| [ ] | Record per-component token counts and every truncation/exclusion decision in the trace. | A trace report explains exactly why each optional context component was included or omitted. |

Required content must not be silently truncated. If the fixed prompt, current
message, tool schemas, and output reserve cannot fit together, the turn fails
explicitly rather than submitting a request whose meaning has been changed.

### Phase 5: Refine memory and conversation recall

| Status | Work item | Acceptance evidence |
| --- | --- | --- |
| [ ] | Retain mandatory recall planning, retrieval, evidence selection when candidates exist, generation, and synchronous indexing. | Existing failure semantics remain intact under focused tests. |
| [ ] | Keep SQLite memory and transcript rows as the only recall corpus; do not add Markdown or workspace-file memory. | Configuration and runtime contain no alternate memory source. |
| [ ] | Add bounded SQLite FTS candidates for conversation chunks and merge them deterministically with vector candidates before evidence selection. | Exact names, identifiers, and phrases can be recalled even when semantic similarity is weak. |
| [ ] | Deduplicate always-present core memory and selected memory by stable Memory ID. | The same memory revision appears no more than once in the final request. |
| [ ] | Consider always injecting only bounded profile memory while retrieving durable and daily memory semantically. | Live-model comparison demonstrates the quality/latency trade-off before changing behavior. |
| [ ] | Include provenance and observation time for mutable recalled facts and direct the final model to verify stale operational claims. | Live tests do not present old operational evidence as certainly current. |

The upstream model-optional `memory_search` pattern will not replace mandatory
recall. The existing main-assistant `search_memory` tool remains useful for an
explicit deeper search after the required pre-generation recall stage.

### Phase 6: Bound the agent and provider boundary

| Status | Work item | Acceptance evidence |
| --- | --- | --- |
| [ ] | Add maximum agent rounds, cumulative tool calls, and cumulative generated/tool-result context. | A looping mock model terminates with a stable audited error. |
| [ ] | Detect an unchanged repeated tool call and prevent an accidental mutation loop. | Repeated-call tests execute the mutation at most once. |
| [ ] | Recount or validate the request budget before every tool-loop model call. | Added tool calls/results cannot grow a later request past the context limit. |
| [ ] | Add narrowly bounded retries for safe transient local generation failures before a response is accepted. | Timeout and transient HTTP tests retry safely without replaying committed tools. |
| [ ] | Keep `/chat/completions`, non-streaming final responses, and local-only provider configuration. | No hosted credential, provider fallback, Responses API, or partial Telegram delivery path is introduced. |

### Phase 7: Verification and cutover evidence

| Status | Work item | Acceptance evidence |
| --- | --- | --- |
| [ ] | Add prompt snapshots for a new DM, a follow-up, a reply, recalled evidence, and a tool round. | Snapshots show deterministic roles, ordering, IDs, and prompt version. |
| [ ] | Add negative tests for wrong owner, owner in a group, spoofed metadata, quoted tool instructions, duplicate updates, and incomplete tool history. | No rejected or historical content crosses the relevant authority boundary. |
| [ ] | Add context-limit tests using large messages, replies, histories, memories, recall results, and tool results. | Every submitted request fits; required-content overflow fails before network I/O. |
| [ ] | Run live local-Gemma tests for follow-up resolution, reply resolution, reminder intent, task intent, memory recall, stale evidence, and adversarial quoted text. | Results and model/config identities are recorded; mock-only success is not reported as behavioral proof. |
| [ ] | Run a credential-backed private Telegram DM test, including restart and duplicate-update scenarios. | The configured owner is admitted, every other chat shape is rejected, and observed delivery/routing matches the stored trace. |
| [ ] | Update public architecture, Telegram, configuration, security, and operations documentation only after the corresponding behavior is verified. | Documentation describes shipped behavior and names any remaining limitations. |

## Deliberate Non-Goals

- Telegram groups, supergroups, channels, topics, mentions, reactions, edits,
  media, voice transcription, and multiple accounts.
- General upstream route bindings, pairing flows, plugin prompts, skills,
  scheduled agents, or provider compatibility layers.
- Hosted OpenAI or other hosted-model providers, Responses API continuation,
  cloud fallback, or partial streaming replies.
- `SOUL.md`, `IDENTITY.md`, `USER.md`, `MEMORY.md`, or other runtime workspace
  prompt injection.
- Migration or import of historical Node, Go, or pre-ledger Rust state.
- Configurable assistant identity, personality, social role, or relationship
  behavior.

## Definition of Done

This plan is complete only when all of the following are true:

- every implementation and verification checkbox above is resolved;
- only the configured owner's private Telegram chat can create an agent turn;
- a Telegram update cannot cause duplicate durable mutations after replay or
  restart;
- the stored structured inbound event can deterministically reconstruct the
  model-visible current message and reply context;
- trusted routing facts, current owner text, historical transcript, quoted
  text, recalled evidence, and tool schemas occupy their documented roles;
- every model request is within the local model's measured context window;
- tool loops and transient retries have explicit bounds and do not replay a
  committed mutation;
- exact submitted requests, prompt version, context selection, tool calls,
  retrieval evidence, output transformation, and delivery outcome remain
  inspectable in SQLite;
- focused tests and live local-model and private-Telegram evidence pass; and
- public documentation has been updated to describe only the verified result.
