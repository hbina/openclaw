---
title: Telegram
summary: Supported Telegram behavior and limitations
---

Telegram is the only messaging channel. The adapter uses long polling and
handles text updates only when the sender id and private-chat id both equal the
one configured numeric owner id. Other senders and group, supergroup, channel,
or mismatched private-chat shapes are discarded before agent handling or
persistence. The sender id remains the owner identity; the chat id is stored
separately as the conversation route and delivery target. One level of reply
context is preserved, including quoted text when Telegram provides it.
Current text and reply context normalize CRLF and CR newlines to LF before
persistence. C0/C1 controls other than LF and TAB are rejected, as are current
text or reply bodies above 16 KiB and selected quotes above 4 KiB.

Accepted updates are written to SQLite before the long-poll checkpoint moves
past them. A separate gateway worker claims the oldest unfinished event with a
renewable lease. Processing survives restarts, uses at most five attempts with
bounded backoff, and resumes the original response trace. Stored successful
model rounds and exact tool results are reused, so replaying an update cannot
repeat a committed task, reminder, or memory mutation. Completed, actively
processing, retryable, and permanently failed duplicates have deterministic
no-reexecution behavior. An identity collision whose stored canonical payload
differs fails closed without advancing the checkpoint.

An inbound event completes only when the Telegram reply and its SQLite trace,
assistant transcript, and derived index changes have committed. Delivery is
at-least-once: a process failure after Telegram accepts `sendMessage` but
before the receipt commits locally can produce a duplicate reply. Recovery
still reuses the stored output and never repeats generation or tools for that
delivery ambiguity.

Reminder delivery sends a text message to the stored numeric Telegram chat id.
The notification body is rendered by the local chat model using the
fixed neutral behavior, recent conversation, and semantic recall, without tools.
Provider, retrieval, generation, or indexing failures leave the reminder due
and retryable; stored text is not used as a generation fallback. Successful
deliveries are stored as structured conversation exchanges. One-shot reminders
are deleted after successful delivery, and recurring reminders advance only
after successful delivery.

Not yet implemented:

- pairing beyond the one configured owner allowlist;
- group, supergroup, channel, topic, or thread handling;
- media, reactions, or edits;
- multiple Telegram accounts;
- idempotent external Telegram delivery receipts;
- durable reminder-delivery claims and leases.

The configured owner id is derived from Telegram ingress and is never accepted
from model-controlled arguments or message text.
