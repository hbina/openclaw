---
title: OpenClaw Rust
summary: Local single-owner tasks, reminders, memory, and conversation recall
---

OpenClaw is a self-hosted assistant for exactly one trusted owner per
deployment. The canonical Rust runtime supports Telegram private text chats,
a loopback HTTP chat endpoint, owner-global tasks, reminders, revisioned
memory, hybrid conversation recall, and detailed SQLite traces.

Chat and embedding inference remain local through operator-controlled
`llama-server` instances. SQLite contains all authoritative application state;
the runtime has no hosted-model fallback, plugin platform, browser automation,
or alternate filesystem memory.

Start with [Installation](/install), then review [Configuration](/configuration)
and [Security and limitations](/security) before enabling Telegram or using the
HTTP Gateway. Operators should also understand the disposable fresh-database
cutover and recovery procedures in [Operations](/operations).
