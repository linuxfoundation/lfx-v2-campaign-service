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
`b64tail` is joined straight back into the output. Every segment must therefore begin its own
`scheme://` — a list of servers is a list of URLs, and a comma that does not start one is a
character inside a value. The scheme has to be at the **start**: merely containing a `://` was
the weaker form, and it admitted the tail of a value as though it were the head of a URL.

Even anchored, no test applied to a **segment** is enough, because a token tail can be
scheme-shaped: `nats://a,nats://b?access_token=s3cret,secret://tail` yields three well-formed-
looking entries, the middle one trims at its `?`, and the third — half a token — is joined
straight back in. By the time segments exist the value has already been cut in the wrong place.
So the **cut** is bounded instead: a comma delimits only where it precedes any `?` or `#` in the
whole value. Everything from the start of a query or fragment belongs to it (RFC 3986 §3.4,
§3.5), so no comma past that point separates entries, and the remainder stays attached to the
last segment where the query trim reaches it.

### Where userinfo is looked for

`redactOne` searches for the `@` in everything before the first `?` or `#` — the authority
**and** the path. Bounding at the first `/` instead, as it first did, leaked twice:

- `nats://u:p/x@host:4222` is malformed (a `/` inside userinfo — which is the kind of input
  this helper exists for), so the bound cut the region down to `u:p`, found no `@`, and
  returned the value untouched, password included.
- `https://idp.example?contact=a@b&access_token=s3cret` has no `/` at all, so the bound was the
  end of the string and the last `@` — inside the query — was taken for userinfo. Rebuilding
  around it deletes the `?` with the prefix, so the query trim finds nothing to cut and every
  parameter after that `@` survives.

Stopping at the query fixes the first and is what makes the second detectable, while keeping
the case the bound exists for: an `@` in a query is not userinfo, and treating it as one throws
away the host and path that are the only reason to log a URL.

One shape stays undecidable — no path, and the only `@` past a `?`. That is either a harmless
query `@` or a malformed password containing a `?` (`nats://u:p?x@host`), and the two want
opposite handling. A path before the query settles it, since a path has already closed the
authority; without one, nothing after the scheme is printed. `nats://u:p?x@host:4222` therefore
renders as `nats://***`, losing the host. Lossy beats leaky, and the shapes it costs are rare.

A bare `/` is **not** that path, and taking it for one leaked. `pathClosesAuthority` asks what
the `/` closes, not whether one exists: `nats://u:p/path?x@host` has a `/`, but `u:p` is no
authority for it to close (`p` is not a port), so the only legal reading is userinfo — the `/`
is inside the password, and trimming at the `?` printed `nats://u:p`. The tell is a `:` in the
pre-slash host whose right side is not a decimal port; a bare host (`idp.example.com`), a real
port (`:8443`) and a bracketed IPv6 literal all keep their host and path. The multi-URL branch's
`passwordCouldSpanQuery` asks the same question for the same reason — it had the identical hole.

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

The fallback's own `@` search is bounded at the first `?` for the same reason the single-URL
path bounds it: an `@` inside a query is not userinfo, and rebuilding around it deletes the `?`
and everything before, leaving the query trim nothing to cut. Bounding it re-opened the mirror
image, though — with no `@` before the bound the value falls through to the query trim, which
cuts at the `?`, and for `nats://u:p?x@a,nats://b` that prints `nats://u:p`. So the fallback
refuses that shape too, on narrower terms than the single-URL path: refusing there costs one
host, refusing here costs every host in the list, so it additionally requires a `:` in the
segment that owns the `?` — a password is what makes truncation dangerous, and userinfo
carrying one must have a `:` before the delimiter. `nats://b?access_token=x@secret,nats://c`
has none and keeps its hosts.

**Which segment owns the `?` is not the first one.** In a list only the LAST segment can, since
every comma from the query onward belongs to it, so the scan for the deciding `/` starts at the
comma before the delimiter. Scanning from the start of the value finds the `/` in an earlier
segment's path — `https://a/b,nats://u:p?x@host` — and calls the query genuine, which is exactly
the fallthrough that leaked.

### Opaque URIs collapse to their scheme

`URL(*url.URL)` clears `User` and the query, which does nothing for an opaque URI: with no `//`
after the scheme, `net/url` puts EVERYTHING after the colon in `Opaque` and never populates
`User`, `Host` or `Path`, so `ftp:svc:s3cret@idp/jwks` rendered verbatim. It is reachable —
`auth.New` refuses a JWKS URL whose scheme is not http(s) and formats the refused value with
this function, so the value being rejected is the one logged. Such a URI now renders as
`scheme:***`. Nothing is preserved because there is nothing to preserve: host and path survive
elsewhere because they are a URL's diagnostic value, and an opaque URI has neither as far as
`net/url` is concerned. For the case that motivates it, the scheme *is* the diagnosis.

## Callers

`internal/infrastructure/config` (`Config.String`/`GoString`, for `JWKS_URL` and `NATS_URL`)
and `internal/infrastructure/auth` (the startup URL validation and every `jwksStatusGuard`
error and debug line). One implementation in one place is the point: the defect that produced
this package was two formatting sites in different packages disagreeing about what "redacted"
meant.

See [pkg/redact](../../../pkg/redact).
