# 2026-08-10 — Two credential leaks: the redirect target, and a password ending in `]`

**Fix** — `internal/infrastructure/auth/jwt.go`, `pkg/redact/url.go` (LFXV2-3053). Two real
findings from the review of PR #102, both of which put a credential in a log line. Four other
findings in the same round were verified STALE against the current code and deliberately not
acted on; the closing section says which and why.

## Leak 1 — the redirect target reached the error completely un-redacted

The previous commit made `RoundTrip` pass a followable 3xx back to `http.Client` so an ordinary
http→https upgrade would not become a permanent `ErrKeyUnavailable`. That is the right outcome
and the wrong place to get it.

Once the Client is handed the 3xx it builds the next request from the upstream `Location`
**itself**, and names that URL in every error it constructs. A `*url.Error` renders through
`net/http`'s `stripPassword`, which masks the password and keeps the **username** and the
**entire query**. Verified by probe:

```
Get "https://u:***@127.0.0.1:1/jwks?access_token=s3cret": dial tcp 127.0.0.1:1: connect: connection refused
```

Under this repo's `pkg/redact` contract that is a leak twice over: a username issued alongside a
password is half a credential, and `?access_token=` is a whole one. And nothing in this service
chose that URL — it came from whatever answered the JWKS host.

`CheckRedirect` was evaluated as the fix and is strictly **worse**. Returning an error from it
produces a `*url.Error` whose `URL` field is the target with **no** redaction at all — no
`stripPassword`, nothing:

```
Get "https://u:p@127.0.0.1:1/jwks?access_token=s3cret": JWKS redirect target carries credentials; refusing to follow
```

The only complete fix is that `http.Client` must never learn the target. `RoundTrip` now follows
redirects in its own loop (`redirectTarget`), so the Client sees one request and one final
response. `redirectTarget` follows exactly the statuses `net/http` does (301/302/303/307/308) and
only with a parseable `Location`; it resolves against the **current hop** via `cur.URL.Parse`
rather than `resp.Location()`, which needs `resp.Request` — a field only `http.Transport`
populates, so the latter would break under a fake transport in a test. `Authorization` is deleted
on every hop, the method is forced to GET (a JWKS fetch has no body to replay, so there is no
307/308 method preservation to get wrong), and non-`http(s)` schemes are refused.

Two things had to move with the following:

- **The hop bound.** `jwksMaxRedirects` = 10, `http.Client`'s default in all but name. Without it
  a self-redirecting endpoint is an unbounded loop inside one `RoundTrip`, held only by
  `jwksFetchTimeout` rather than refused — and every token verification in the process waits it
  out. `TestVerifyActor_RefusesARedirectLoop` counts hops and fails if the loop runs past it.
- **Draining.** `drainAndClose` reads a bounded prefix of each abandoned body before closing, so
  the connection returns to the idle pool instead of re-doing TCP and TLS on the next hop.

`scrubURLError` is the default-deny backstop: any `*url.Error` anywhere in the chain is rebuilt
around `redact.URLUserinfo(ue.URL)`. It is not a fix for a known path — it exists because
`RoundTrip` now issues requests to URLs it did not choose, so the transport below it can name one
in an error. It replaces the whole chain rather than the matched layer, since a layer wrapping
the `*url.Error` can carry the same text.

`TestVerifyActor_RedirectTargetCredentialsNeverReachTheError` points a `Location` at a dead host
with userinfo and an `access_token` query, then walks **every** `errors.Unwrap` layer asserting
none contains the username, password, token value or the parameter name — and separately asserts
the CONFIGURED host IS still named, because an error identifying nothing is not an improvement.
Revert-verified: against the pass-through it fails with

```
error layer *fmt.wrapError leaks "svc" from the REDIRECT TARGET
```

## Leak 2 — a password is allowed to end in `]`

`pathClosesAuthority` decides whether the `/` in a value like `nats://u:p/path?x@host` closes an
authority or sits inside a password. Its IPv6 exemption was `strings.HasSuffix(host, "]")`, and a
password can end in a bracket. `nats://u:secret]/path?x@host:4222` has `u:secret]` before the
`/`; the suffix test calls that an IPv6 host and therefore a closed authority, so the `?` reads
as a genuine query, the value is cut there, and **`nats://u:secret]` goes to the log**.

Requiring the opening bracket as well is not enough on its own — `[secret]` has both.
`bracketedHostCloses` validates the whole form instead: the address is hex digits, `:` and `.`,
with **at least one `:`**, since every IPv6 literal has one and nothing else reaching this
function does; any trailing `:port` is decimal and non-empty (an empty port is refused, since
`u:` is a likelier empty password than a real authority).

The zone id after a `%25` (RFC 6874) needed a **looser** rule than the address, and the first
version of this fix got it wrong: a zone id is an **interface name**, so `eth0` and `en0` are the
ordinary cases and a hex-only rule refuses every real one — `[fe80::1%25eth0]` came out as
`nats://***`, discarding a legitimate host to fix a leak. Over-rejection is a defect too. The
address and the zone are split at the first `%` and the zone gets the unreserved set, which is
safe precisely because the address in front of it has already had to pass.

Both leak cases are in `TestURLUserinfo_NeverEmitsACredential` and were verified failing on the
old code (`got "nats://u:secret]/path"`, `which still carries "secret]"`). `[::1]`,
`[::ffff:127.0.0.1]` and `[fe80::1%25eth0]` were added to `TestURLUserinfo_Shapes` so the
tightening cannot start eating real hosts unnoticed.

## Four findings in the same round were stale, and were not acted on

Editing code to satisfy an unverified claim is a real new defect, so each was probed first:

- **Multi-URL credential suffix after a query `@`** — probe:
  `nats://a,nats://b?access_token=prefix@live-secret,nats://c` → `nats://a,nats://b`. Already
  correct, already covered by a named regression test.
- **A slash inside userinfo hiding the credential** — probe: `nats://u:p/x?y@host:4222` →
  `nats://***`. This is the very hole `pathClosesAuthority` was written to close; the review was
  describing the code that preceded it.
- **"Redirects are converted into an error"** — false as of the previous commit; followable
  redirects were already passed through. They are now followed outright.
- **The opaque-URI finding** — outdated, fixed by an earlier commit on this branch.

Each was answered on its thread by quoting the passing test or the probe output rather than by
changing code.
