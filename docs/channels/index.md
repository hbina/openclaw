---
summary: "Messaging platforms OpenClaw can connect to"
read_when:
  - You want to choose a chat channel for OpenClaw
  - You need a quick overview of supported messaging platforms
title: "Chat channels"
---

OpenClaw can talk to you on any chat app you already use. Each channel connects via the Gateway.
Text is supported everywhere; media and reactions vary by channel.

## Delivery notes

- Telegram replies that contain markdown image syntax, such as `![alt](url)`,
  are converted into media replies on the final outbound path when possible.
- Channels that accept bot-authored inbound messages can use shared
  [bot loop protection](/channels/bot-loop-protection) to prevent bot pairs from
  replying to each other indefinitely.
- Supported always-on rooms can use [ambient room events](/channels/ambient-room-events)
  so unmentioned room chatter becomes quiet context unless the agent sends with
  the `message` tool.

## Supported channels

- [Telegram](/channels/telegram) - Bot API via grammY; supports groups. Fastest setup (simple bot token).
- [WhatsApp](/channels/whatsapp) - uses Baileys and requires QR pairing.
- [Discord](/channels/discord) - Discord Bot API + Gateway; supports servers, channels, and DMs.
- [WebChat](/web/webchat) - Gateway WebChat UI over WebSocket.

## Notes

- Channels can run simultaneously; configure multiple and OpenClaw will route per chat.
- Fastest setup is usually **Telegram** (simple bot token). WhatsApp requires QR pairing and
  stores more state on disk.
- Group behavior varies by channel; see [Groups](/channels/groups).
- DM pairing and allowlists are enforced for safety; see [Security](/gateway/security).
- Troubleshooting: [Channel troubleshooting](/channels/troubleshooting).
- Model providers are documented separately; see [Model Providers](/providers/models).
