---
title: Telegram
summary: Supported Telegram behavior and limitations
---

Telegram is the only messaging channel. The adapter uses long polling and
handles text updates. It records the numeric sender id as routing metadata and
preserves one level of reply context, including quoted text when Telegram
provides it.

Reminder delivery sends a text message to the stored numeric Telegram sender
id. The notification body is rendered by the local chat model using the
configured persona, recent conversation, and semantic recall, without tools.
If contextual rendering is unavailable, the stored reminder text is sent
under the same fixed heading. Successful deliveries are stored as structured
conversation exchanges. One-shot reminders are deleted after successful
delivery, and recurring reminders advance only after successful delivery.

Not yet implemented:

- pairing or owner allowlists;
- correct group and topic identity;
- media, reactions, edits, or general thread handling;
- multiple Telegram accounts;
- durable delivery claims, leases, or idempotency.

Until admission controls exist, possession of the bot username/token path may
allow an unknown Telegram sender to interact with the assistant. Treat the
current adapter as test-only unless network- and bot-level controls make that
acceptable.
