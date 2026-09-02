---
title: Telegram
summary: Supported Telegram behavior and limitations
---

Telegram is the only messaging channel. The adapter uses long polling and
handles text updates from the one configured numeric owner user id. Other
senders are discarded before agent handling or persistence. It records the
admitted numeric sender id as routing metadata and
preserves one level of reply context, including quoted text when Telegram
provides it.

Reminder delivery sends a text message to the stored numeric Telegram sender
id. The notification body is rendered by the local chat model using the
fixed neutral behavior, recent conversation, and semantic recall, without tools.
If contextual rendering is unavailable, the stored reminder text is sent
under the same fixed heading. Successful deliveries are stored as structured
conversation exchanges. One-shot reminders are deleted after successful
delivery, and recurring reminders advance only after successful delivery.

Not yet implemented:

- pairing beyond the one configured owner allowlist;
- correct group and topic identity;
- media, reactions, edits, or general thread handling;
- multiple Telegram accounts;
- durable delivery claims, leases, or idempotency.

The configured owner id is derived from Telegram ingress and is never accepted
from model-controlled arguments or message text.
