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
  percent-encoded one.

Both drop the **query and fragment** as well as userinfo. A JWKS endpoint is
operator-supplied, and `https://idp/jwks?access_token=…` is a shape real identity providers
accept — so clearing `User` and printing the query would honour the letter of the contract
and miss the credential actually present. In the string form the strip runs strictly **after**
userinfo removal: a password may contain a `?`, so cutting first would truncate
`nats://u:p?x@host` to `nats://u:p` and log half a secret. Once userinfo is gone, every
remaining `?` and `#` is at or after the host.

### The comma rule, and why it is conditional

NATS accepts a server list in one URL value, and the last-`@` rule flattens such a value to
its final server — safe, but it hides the hosts this package exists to keep visible. So a
comma-separated value **is** redacted entry by entry, but only when splitting is provably
safe.

The catch is that `,` is an RFC 3986 sub-delim and legal unescaped inside userinfo. Split
`nats://u:p,x@host` blindly and the first piece is `nats://u:p` — no `@`, nothing to redact,
half the password in the log. That is strictly worse than the lossy behaviour the split was
meant to improve on, and it shipped once: the entry-by-entry split landed unconditional and
Cursor caught it on review.

Which a comma is cannot be decided from the value. `nats://a:4222` is either a host and port
or a user and password. So the split happens only where the answer does not matter — when
**every** segment carries an `@` (each is a credential-bearing server, so all get redacted)
or **none** does (there is no credential to leak). Any mix falls back to the whole-value
rule: lossy, but incapable of leaking.

That test alone still splits a token, because a comma is legal in a **query** too and a query
is where the other credential shape lives: neither piece of
`https://idp/jwks?access_token=a,b64tail` has an `@`, so the count rule calls it a list and
`b64tail` is joined straight back into the output. Two further conditions close it. Every
segment must begin its own `scheme://` — a list of servers is a list of URLs, and a comma that
does not start one is a character inside a value. And no `?` or `#` may appear **before** the
first comma: everything past the start of a query or fragment belongs to it (RFC 3986 §3.4,
§3.5), so a comma there is never a delimiter. The second condition exists because the first is
satisfiable by a token whose tail happens to contain `://`.

### When the whole-value fallback runs

`redactOne` bounds userinfo to the authority, and that bound is only trustworthy while the
value holds one URL. A comma **and** a second `://` means it does not, and the conservative
whole-string rule applies instead: redact from the last `@` anywhere, losing the earlier hosts
rather than printing a password.

Both halves of that condition are load-bearing, and each was learned by leaking:

- The check runs **before** the first authority's `@` is handled, not only when that authority
  has none. A mixed list is exactly what the split refuses, so it is the likeliest value to
  arrive whole — and in `nats://u:p@a,nats://u2:p2@b` the first authority *does* have an `@`,
  so a later-placed check never runs and the second credential prints verbatim.
- A comma is **required**, not just a second `://`. Without a comma there is one URL, and a
  later `://` sits in its query or path — a `?redirect=https://…` parameter, most obviously.
  Treating that as a second URL rebuilds the output from an `@` inside the query, discarding
  the `?` along with the prefix, so the query trim finds nothing to cut and every parameter
  after that `@` survives. One JWKS URL of that shape printed an `access_token` in full.

## Callers

`internal/infrastructure/config` (`Config.String`/`GoString`, for `JWKS_URL` and `NATS_URL`)
and `internal/infrastructure/auth` (the startup URL validation and every `jwksStatusGuard`
error and debug line). One implementation in one place is the point: the defect that produced
this package was two formatting sites in different packages disagreeing about what "redacted"
meant.

See [pkg/redact](../../../pkg/redact).
