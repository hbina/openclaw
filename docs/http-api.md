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

The endpoint currently has no authentication, body-size limit, rate limit, or
stable JSON error envelope. Bind it only to a trusted interface or protect it
with an operator-managed reverse proxy.
