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
- durable delivery claims, leases, or idempotency.

The configured owner id is derived from Telegram ingress and is never accepted
from model-controlled arguments or message text.
