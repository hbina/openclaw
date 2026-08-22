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

The endpoint currently has no authentication, body-size limit, rate limit, or
stable JSON error envelope. Bind it only to a trusted interface or protect it
with an operator-managed reverse proxy.
