# 2026-08-10 — LFXV2-3053: the query bound's mirror image, an opaque URI, and a 3xx read as failure

**Fix** — dealako's CHANGES_REQUESTED on PR #102, plus the two bot findings under it. Three
defects, two of them created by the previous round's fix.

## 1. Bounding the fallback at the `?` opened the shape it was closing

`2026-08-10-lfxv2-3053-multi-url-fallback-query-at.md` bounded the multi-URL fallback's `@`
search at the first `?`, because an `@` past the query is not userinfo. Cursor flagged the
mirror image (High): with no `@` **before** the bound, the value falls through to
`trimQueryAndFragment`, which cuts at the `?`. For

```
nats://u:p?x@a:4222,nats://b:4222
```

the `?` is inside the PASSWORD, so the cut prints `nats://u:p` — the credential, in the log. The
single-URL path already refuses this exact shape; a comma in the password is not a reason to
stop refusing it.

The fallback now refuses it too, on **narrower** terms. Refusing in the single-URL path costs
one host; refusing here costs every host in a list, so this branch additionally requires a `:`
in the segment owning the `?`. A password is what makes truncation dangerous, and userinfo
carrying one must have a `:` before the delimiter, so
`nats://b?access_token=x@live-secret,nats://c` — no `:`, `b` could only ever be a bare
username — keeps its hosts, and the pinned expectation from the previous round survives
unchanged.

**The `/` test does not transfer between the two branches, and that is where this nearly went
wrong a second time.** The single-URL path decides whether a `?` is genuine by looking for a `/`
that closed the authority. Applied to a list by scanning from the start of the value, it finds
the `/` in an EARLIER segment's path — `https://a/b,nats://u:p?x@host` — and calls the query
genuine, which is precisely the fallthrough being fixed. Only the LAST segment can own the
delimiter, since every comma from the query onward belongs to it, so `owningAuthority` starts
its scan at the comma before the delimiter.

## 2. `URL(*url.URL)` rendered an opaque URI verbatim

Copilot's finding. `URL` clears `User`, `RawQuery` and the fragment — and for an opaque URI
(no `//` after the scheme) `net/url` puts EVERYTHING after the colon in `Opaque` and never
populates `User`, `Host` or `Path`. So `ftp:svc:s3cret@idp.example/jwks` passed through untouched.

Copilot's stated output was stale for the current code, but its prescription was right, and the
path is reachable: `auth.New` refuses a JWKS URL whose scheme is not http(s) and formats the
refused value with this function — the value being rejected is the one logged. Such a URI now
collapses to `scheme:***`. Nothing is preserved because there is nothing to preserve: host and
path survive elsewhere because they are a URL's diagnostic value, and an opaque URI has neither
as far as `net/url` is concerned. For the case that motivates this, the scheme is the diagnosis.

## 3. A JWKS redirect was a permanent outage

Found while reading the auth side for #1 and #2, not reported by anyone.

`jwksStatusGuard.RoundTrip` errored on everything that was not 2xx. **An `http.RoundTripper`
sits BELOW `http.Client`'s redirect handling** — the Client only follows a 3xx it is handed, so
an error means it never sees one. An ordinary http→https upgrade or a CDN hop in front of the
IdP turned every token verification into `ErrKeyUnavailable`, forever, because every refresh
takes the same path.

3xx is now passed back. Following is only safe because the credential does not travel with it,
so `credentialed` was gated to dress the **first hop only**, matching `req.URL` against the
sanitized URL handed to the provider. Both halves are load-bearing: without the pass-through the
redirect is a 302 error; without the gate every hop's URL is rewritten back to the configured
endpoint and the Client loops to its 10-hop limit re-sending the credential each time. The
target also must not receive the operator's QUERY — `net/http` drops `Authorization` across
hosts on its own, but nothing drops a query, and nothing drops either across a same-host
redirect to a different path.

## Regression guards

All four were verified by reverting the fix and watching them fail:

| Test | Reverted | Failure |
|---|---|---|
| `multi-URL fallback refuses a password containing a ?` | the refusal | `nats://u:p` |
| `an earlier segment's slash does not make a later query genuine` | the comma scan | `https://a/b,nats://u:p` |
| `a list whose last segment has a path keeps its hosts` | — | pins that the fix is not blanket refusal |
| `TestURL_OpaqueURIIsCollapsedToItsScheme` | the `Opaque` branch | renders `svc:s3cret@…` |
| `TestVerifyActor_FollowsAJWKSRedirectWithoutForwardingCredentials` | the 3xx pass-through | `returned HTTP 302` |
| ″ | the first-hop gate | `stopped after 10 redirects` |
