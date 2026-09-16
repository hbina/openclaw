---
title: Telegram
summary: Supported private-owner text behavior and limitations
---

Telegram is the only messaging channel. The Rust adapter uses Bot API long
polling and admits text updates only when all three facts agree:

- `chat.type` is `private`;
- `from.id` equals the one configured numeric owner ID; and
- `chat.id` equals that same owner ID.

Other senders, mismatched private chats, groups, supergroups, and channels are
rejected before agent handling, traces, transcripts, model calls, or tools.
Sender identity is used for admission; chat identity remains a separate
conversation and delivery route. Neither value is accepted from prompt text or
model-controlled tool arguments.

One replied-to message and Telegram's selected quote are preserved as bounded
structured data. Current text and reply bodies normalize CRLF and CR to LF.
C0/C1 controls other than LF and TAB are rejected. Current text and reply
bodies are limited to 16 KiB and selected quotes to 4 KiB.

At the model boundary, minimal route facts and optional reply data occupy an
application-produced `user` carrier immediately before a separate `user`
message containing only current owner text. Human text that resembles a carrier
delimiter is escaped. Old carriers are not stored or replayed as owner text.

## Durable processing

An accepted update is inserted in SQLite before the polling checkpoint
advances. A gateway worker claims the oldest unfinished event with a renewable,
generation-fenced lease. Work survives restarts, uses at most five attempts
with bounded backoff, and resumes the same response trace. Successful model
rounds and exact tool results are reused, so a duplicate or retry cannot repeat
a committed task, reminder, or memory mutation.

Completed, active, retryable, and permanently failed duplicates have explicit
no-reexecution behavior. Reusing a Telegram identity with different canonical
content fails closed without moving the polling checkpoint.

An inbound event completes only after Telegram accepts the response and the
delivery receipt, assistant transcript, derived conversation index, and trace
commit locally. Delivery is at-least-once: a crash after Telegram accepts
`sendMessage` but before the local receipt commits can send the stored response
again. That ambiguity does not rerun generation or tools.

Reminder notifications use the stored numeric chat route and the same required
local recall stages, without exposing tools. A failure leaves the reminder due.
One-shot reminders are removed, and recurring reminders advance, only after a
successful Telegram delivery commit.

## Unsupported Telegram surfaces

- pairing beyond the configured numeric owner allowlist;
- groups, supergroups, channels, topics, and threads;
- media, reactions, edits, and voice transcription;
- multiple bot accounts; and
- an external idempotency key for Bot API delivery.

The deployment verification procedure includes a real private-owner message,
restart persistence, duplicate-state inspection, route matching, and a Bot API
credential check. Unsupported chat shapes remain covered by deterministic
ingress fixtures because the Bot API cannot synthesize messages from another
user account.
