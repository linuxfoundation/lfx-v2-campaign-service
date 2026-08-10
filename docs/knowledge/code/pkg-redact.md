---
type: "Go Package"
title: "pkg/redact"
description: "Renders credential-bearing values into log-safe forms; strips URL userinfo entirely, unlike url.URL.Redacted()."
resource: "pkg/redact"
---

# pkg/redact

Renders credential-bearing values into log-safe forms; strips URL userinfo entirely, unlike
`url.URL.Redacted()`.

## Why this is not `url.URL.Redacted()`

The standard library masks the PASSWORD and keeps the username, on the general view that a
username identifies rather than authenticates. Every credential-bearing URL this service
accepts — a JWKS endpoint behind a basic-auth gateway, a NATS URL with inline credentials —
carries a username issued alongside the password as half of one credential, so printing it
narrows an attacker's search rather than telling them nothing. The contract here is stricter:
userinfo goes entirely.

The host and path always survive. They are the whole diagnostic value of printing the URL,
and a redactor that eats them gets replaced by a raw print at the next outage.

## The two entry points

* `URL(*url.URL) string` — for a parsed URL. Clears `User` on a **copy**; mutating the
  caller's URL would be a formatter with a side effect on the credential it is hiding (the
  next request would go out unauthenticated). `nil` renders as the empty string.
* `URLUserinfo(string) string` — for a URL that may not parse. `url.Parse`'s own error embeds
  the whole raw URL, so the raw string is exactly what must not be printed and there is no
  `*url.URL` to work with. Splits on the **last** `@`, because a password may contain a
  percent-encoded one. A comma-separated value is redacted entry by entry: NATS accepts a
  server list in one URL, and the last-`@` rule would otherwise take everything before the
  final credential as userinfo and drop the earlier hosts.

## Callers

`internal/infrastructure/config` (`Config.String`/`GoString`, for `JWKS_URL` and `NATS_URL`)
and `internal/infrastructure/auth` (the startup URL validation and every `jwksStatusGuard`
error and debug line). One implementation in one place is the point: the defect that produced
this package was two formatting sites in different packages disagreeing about what "redacted"
meant.

See [pkg/redact](../../../pkg/redact).
