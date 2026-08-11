# 2026-08-10 — LFXV2-3053 three shapes `redact.URLUserinfo` still leaked

**Update** — the comma cut is bounded at the first `?`/`#`, list segments must
be scheme-**anchored**, and the userinfo `@` is searched across the whole
pre-query region rather than the first `/`-bounded authority. Three values that
this helper returned with a live credential in them no longer do.

Each was found by asking the only question that matters of a redactor — not
"does this look right" but "name a value whose output still contains the
secret" — and each has a case in `TestURLUserinfo_NeverEmitsACredential` that
asserts absence of the secret substring, so a future rewrite that reintroduces
the shape fails on the credential rather than on formatting.

## The comma cut

`nats://a:4222,nats://b:4222?access_token=s3cret,secret://b64tail` was split
into three segments. Each looked like a URL, the middle one lost its query to
the per-segment trim, and the third — the tail half of the token — was joined
straight back into the output.

The earlier guard, "no `?` before the first comma", inspected the value and
then cut it anyway; anchoring the scheme does not help either, because
`secret://tail` is itself scheme-anchored. Neither test is applied where the
mistake is. By the time segments exist the value has already been cut in the
wrong place, so the fix bounds the **cut**: only commas preceding any `?` or
`#` in the whole value delimit, and the remainder rides along on the final
segment where the query trim reaches it.

Anchoring is kept regardless — it is the narrowing that was asked for, and it
closes a different shape (`https://idp/jwks?access_token=a,b64tail`, where the
tail merely *contains* a `://`) — and is bound by its own case,
`nats://u:p@a:4222,x/y://b@c`, which reverting the anchor turns into two
"redacted" segments instead of one.

## The pre-query `@`

`redactOne` looked for userinfo between the scheme and the first `/`. Two
values escaped it.

`nats://u:p/x@host:4222` is malformed — a `/` inside userinfo, which is exactly
the sort of value that reaches a redactor — so the bound cut the search region
down to `u:p`, found no `@`, and returned the string verbatim, password
included.

`https://idp.example?contact=a@b&access_token=s3cret` has no `/` at all, so the
region ran to the end of the string and the query's own `@` was read as
userinfo. Rebuilding around it (`u[:authStart] + "***@" + u[at+1:]`) deletes the
`?` along with the prefix, so the query trim that would have removed
`access_token` finds no `?` to cut at.

Searching the whole pre-query region fixes the first and makes the second
impossible: an `@` past the `?` is the query's own, per RFC 3986 §3.2 and §3.4.

## The one shape that stays undecidable

No path, and the only `@` after a `?`: either a harmless query `@` or a
password containing a `?` (`nats://u:p?x@host`). The two want opposite handling
and the value cannot say which it is. A `/` before the query settles it — a
path has already closed the authority — and without one, nothing after the
scheme is printed. So `nats://u:p?x@host:4222` now redacts to `nats://***`
where it previously kept `@host:4222`. That is a deliberate loss of the host in
a rare shape, taken because the alternative is emitting half a password.
