---
title: HTTP API
summary: Current Gateway endpoints
---

The Gateway exposes two endpoints.

`GET /healthz` returns `200 OK` with body `OK`.

`POST /chat` accepts:

```json
{
  "sender_id": "operator-chosen-routing-key",
  "message": "List my reminders"
}
```

It returns:

```json
{
  "reply": "..."
}
```

`sender_id` defaults to `cli-user` when omitted. It is a conversation routing
key for the one trusted owner, not an account or authorization boundary.

The `openclaw chat` command is a one-shot terminal client for this endpoint. It
accepts a message as command arguments or on stdin and surfaces the
`X-OpenClaw-Trace-ID` response header for local debugging. It requires the
Gateway to already be running and does not create a second agent runtime.

The Rust Gateway binds only to loopback and rejects non-loopback peers. Requests
have fixed limits: 16 KiB of headers, 1 MiB total body, 64 KiB current message,
256-byte sender key, 32 concurrent connections, and a ten-minute request
deadline. Errors use `{"error":{"code":"...","message":"..."}}` and a
stable HTTP status. The endpoint has no authentication or per-client rate
limiter, so keep it local; a reverse proxy is an operator-supplied trust
boundary rather than part of OpenClaw admission.
