---
summary: "Generated inventory of OpenClaw plugins shipped in core, published externally, or kept source-only"
read_when:
  - You are deciding whether a plugin ships in the core npm package or installs separately
  - You are updating bundled plugin package metadata or release automation
  - You need the canonical internal vs external plugin list
title: "Plugin inventory"
---

# Plugin inventory

This page is generated from `extensions/*/package.json`, `openclaw.plugin.json`,
and the root npm package `files` exclusions. Regenerate it with:

```bash
pnpm plugins:inventory:gen
```

## Definitions

- **Core npm package:** built into the `openclaw` npm package and available without a separate plugin install.
- **Official external package:** OpenClaw-maintained plugin omitted from the core npm package, kept in this official inventory, and installed on demand through ClawHub and/or npm.
- **Source checkout only:** repo-local plugin omitted from published npm artifacts and not advertised as an installable package.

Source checkouts are different from npm installs: after `pnpm install`, bundled
plugins load from `extensions/<id>` so local edits and package-local workspace
dependencies are available.

## Install a plugin

Use the install route in each entry to decide whether install is needed. Plugins
that say `included in OpenClaw` are already present in the core package.
Official external packages need one install, then a Gateway restart.

For example, Discord is an official external package:

```bash
openclaw plugins install @openclaw/discord
openclaw gateway restart
openclaw plugins inspect discord --runtime --json
```

During the launch cutover, ordinary bare package specs still install from npm.
Use `clawhub:@openclaw/discord` or `npm:@openclaw/discord` when you need an
explicit source. After install, follow the plugin's setup doc, such as
[Discord](/channels/discord), to add credentials and channel config. See
[Manage plugins](/plugins/manage-plugins) for update, uninstall, and publishing
commands.

Each entry lists the package, distribution route, and description.

## Core npm package

2 plugins

- **[anthropic](/plugins/reference/anthropic)** (`@openclaw/anthropic-provider`) - included in OpenClaw. Adds Anthropic model provider support to OpenClaw.

- **[memory-core](/plugins/reference/memory-core)** (`@openclaw/memory-core`) - included in OpenClaw. Adds memory embedding provider support. Adds agent-callable tools.

## Official external packages

2 plugins

- **[discord](/plugins/reference/discord)** (`@openclaw/discord`) - npm; ClawHub. OpenClaw Discord channel plugin for channels, DMs, commands, and app events.

- **[whatsapp](/plugins/reference/whatsapp)** (`@openclaw/whatsapp`) - ClawHub: `clawhub:@openclaw/whatsapp`; npm. OpenClaw WhatsApp channel plugin for WhatsApp Web chats.

## Source checkout only

2 plugins

- **[openai](/plugins/reference/openai)** (`@openclaw/openai-provider`) - source checkout only. Adds OpenAI model provider support to OpenClaw.

- **[telegram](/plugins/reference/telegram)** (`@openclaw/telegram`) - source checkout only. Adds the Telegram channel surface for sending and receiving OpenClaw messages.
