---
title: Security and limitations
summary: Current trust boundary and known gaps
---

This is a single-owner assistant. Each owner runs a separate deployment.
Channel and sender ids route conversations and reminder delivery; they are not
tenant ids and do not isolate data inside the runtime. Tasks, durable memory,
and semantic conversation recall are intentionally owner-global.

Keep these boundaries:

- mount public config read-only;
- mount `secrets.json` separately and never commit or log it;
- persist SQLite outside the image;
- connect only to operator-controlled local model endpoints;
- expose the Gateway only to a trusted network or authenticated reverse proxy.

Known high-priority gaps:

- `/chat` has no authentication, request-size limit, rate limit, safe-bind
  policy, or stable error envelope;
- Telegram has no pairing or owner allowlist;
- reminder delivery has no durable claim/lease or delivery-idempotency token;
- backup/restore is an operator procedure, not an in-product command;
- live Telegram delivery still needs credential-backed acceptance proof.

Unsupported surfaces include hosted model providers, cloud fallbacks, plugins,
skills, arbitrary command execution, browser automation, webhooks, WhatsApp,
Discord, media, multi-account operation, multi-user accounts, and multi-agent
routing.
