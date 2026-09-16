---
title: Security and limitations
summary: Current trust boundary and deliberately unsupported surfaces
---

OpenClaw is a single-owner local assistant. Each owner runs a separate
deployment. Telegram sender identity establishes admission, while channel and
conversation identifiers route delivery; they are not tenant boundaries.
Tasks, memories, and semantic conversation recall are intentionally
owner-global after admission.

Keep these operational boundaries:

- mount public configuration read-only;
- mount `secrets.json` separately and never commit, print, or image it;
- persist or deliberately discard the one SQLite database as a unit;
- connect only to operator-controlled local model endpoints; and
- leave the unauthenticated HTTP Gateway loopback-bound.

The Rust Gateway rejects non-loopback bind addresses and non-loopback peers. It
uses fixed header, body, current-message, sender-key, connection, and request
time limits and returns stable JSON error codes. It has no HTTP authentication
or per-owner rate limiter, so a reverse proxy does not broaden the supported
trust model unless the operator supplies equivalent local admission controls.

Telegram admits only the configured numeric owner's private chat. Routing
identity comes from the parsed Bot API update and cannot be supplied by prompt
text or tool arguments. Accepted updates are durable lease-fenced work and
committed tools are replay-safe across retries and restarts. Pairing beyond the
single allowlist is unsupported.

Telegram delivery is at-least-once. A crash after Bot API acceptance but before
the local receipt commit can duplicate the already-stored reply, although it
does not rerun generation or tools. Reminder delivery also has no external
idempotency token; a failed send remains due and retryable.

Prompt trust is structural rather than role-name magic. The application owns
the current-turn carrier, escapes its delimiters, labels quoted text as data,
and keeps current owner text separate. Historical messages, recalled evidence,
tool output, and quotes remain non-authoritative. Ingress admission and tool
execution—not model interpretation—enforce identity and permissions.

Unsupported surfaces include hosted model providers, cloud fallback, plugins,
skills, arbitrary commands, browser automation, webhooks, WhatsApp, Discord,
Telegram groups/media, multi-account operation, multi-user accounts, and
multi-agent routing.

Historical Node, Go, and older Rust databases are intentionally incompatible.
Fresh-state cutover is the supported test policy; retained backups are forensic
operator artifacts, not migration inputs.
