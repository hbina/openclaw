---
title: OpenClaw Go
summary: The supported surface of the slim personal reminder assistant
---

OpenClaw Go is a self-hosted assistant for exactly one trusted owner per
deployment. It supports Telegram text conversations, an HTTP chat endpoint,
one-shot and recurring reminders, durable semantic memory, and automatic
semantic recall from SQLite conversation history.

The runtime connects only to two operator-managed local `llama-server`
processes: one for chat and tool generation and one for EmbeddingGemma
embeddings. Configuration and secrets are mounted separately; state lives in
one mounted SQLite database.

Start with [installation](/install), then review [security](/security) before
exposing the Gateway or Telegram bot.
